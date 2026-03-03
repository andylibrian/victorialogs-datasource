// response.go — VictoriaLogs Response Parsing
//
// This file handles parsing HTTP responses from VictoriaLogs into Grafana data frames.
//
// VictoriaLogs uses two different response formats depending on the query type:
//
//   NDJSON (Newline-Delimited JSON):
//     Used for instant queries (/select/logsql/query) and streaming (/select/logsql/tail).
//     Each line is a complete JSON object representing one log entry.
//     Example:
//       {"_time":"2024-01-01T12:00:00Z","_msg":"error connecting","_stream":"{app=\"nginx\"}","level":"error"}
//       {"_time":"2024-01-01T12:00:01Z","_msg":"connection reset","_stream":"{app=\"nginx\"}","level":"warn"}
//     Parsed by parseInstantResponse (all at once) or parseStreamResponse (line by line for live tail).
//
//   JSON (Prometheus-compatible):
//     Used for stats queries (/select/logsql/stats_query and /select/logsql/stats_query_range).
//     Returns a Prometheus-style object: {status, data: {resultType, result}}.
//     resultType is either "vector" (instant/stats) or "matrix" (range).
//     Parsed by parseStatsResponse.
//
//   JSON (Hits format):
//     Used for hits queries (/select/logsql/hits).
//     Returns a histogram of log counts bucketed by field values and time intervals.
//     Parsed by parseHitsResponse.
//
// Grafana Data Frames:
//   All parsers return backend.DataResponse containing data.Frames.
//   Each frame has typed fields (time, string, float64, JSON) that Grafana renders
//   using its various visualization panels (Logs, Time series, Table, etc.).
//
// Parser selection is done in query.go (DatasourceInstance.query) based on QueryType.

package plugin

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	"github.com/valyala/fastjson"

	"github.com/VictoriaMetrics/victorialogs-datasource/pkg/utils"
)

const (
	// VictoriaLogs reserved field names.
	// These are special fields that every log entry contains.
	// They carry the log message, stream labels, and timestamp.
	messageField = "_msg"    // The human-readable log message
	streamField  = "_stream" // Structured stream labels, e.g. {app="nginx",env="prod"}
	timeField    = "_time"   // RFC3339 or Unix timestamp of the log entry

	// Grafana logs panel expects these exact field names in the data frame.
	// Deviating from these names prevents correct log visualization.
	gLabelsField = "labels" // JSON object of all log labels (used for filtering)
	gTimeField   = "Time"   // Timestamp field (capital T is Grafana's convention)
	gLineField   = "Line"   // Log line / message text
	gValueField  = "Value"  // Numeric value (used in hits/stats frames)

	// logsVisualisation is the Grafana visualization hint for log panels.
	// Setting PreferredVisualization to this value makes Grafana automatically
	// choose the Logs panel instead of a generic table or time series.
	logsVisualisation = "logs"
)

// nowFunc is the current-time provider, exposed as a variable so tests can
// inject a fixed timestamp. In production this is always time.Now.
// We need this because Grafana requires a non-empty Time field in every log frame,
// but some VictoriaLogs responses may omit _time (e.g. error cases).
var nowFunc = time.Now

// parseInstantResponse reads an NDJSON response body and returns a DataResponse
// containing a single Grafana logs frame with Time, Line, and labels fields.
//
// The response is an NDJSON stream where each line is a JSON object representing
// one log entry. We accumulate all lines into a single frame (unlike parseStreamResponse,
// which sends one frame per line for live streaming).
//
// WHY fastjson instead of encoding/json?
// fastjson is significantly faster for this use case because:
//   - We parse each line independently (no complex struct unmarshaling)
//   - We only extract a few fields by name (not all fields)
//   - fastjson avoids allocations by reusing internal buffers across Parse calls
//
// WHY a 64KB read buffer?
// A 64KB buffer lets us read most log lines in a single syscall. Lines longer than
// this are skipped (with a debug log) rather than causing a fatal error, because
// truncated data would produce corrupt output.
func parseInstantResponse(reader io.Reader) backend.DataResponse {
	// Initialize three parallel arrays that will form the frame's columns.
	// Grafana's Logs panel looks for these exact field names.
	labelsField := data.NewFieldFromFieldType(data.FieldTypeJSON, 0)
	labelsField.Name = gLabelsField

	timeFd := data.NewFieldFromFieldType(data.FieldTypeTime, 0)
	timeFd.Name = gTimeField

	lineField := data.NewFieldFromFieldType(data.FieldTypeString, 0)
	lineField.Name = gLineField

	// 64KB buffer covers most real-world log lines without excessive memory use.
	br := bufio.NewReaderSize(reader, 64*1024)
	// fastjson.Parser is stateful and not goroutine-safe, but here we're in a
	// single goroutine, so it's safe to reuse across loop iterations.
	var parser fastjson.Parser
	var finishedReading bool
	for n := 0; !finishedReading; n++ {
		b, err := br.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				// Line exceeds 64KB buffer. Skip it to avoid corrupt output.
				// This is unusual but can happen with base64-encoded payloads or verbose logs.
				backend.Logger.Debug("skipping line number: line too long", "lineNumber", n)
				continue
			}
			if errors.Is(err, io.EOF) {
				// b can be != nil when EOF is returned, so we need to process it
				finishedReading = true
			} else {
				return newResponseError(fmt.Errorf("cannot read line in response: %s", err), backend.StatusInternal)
			}
		}

		if len(b) == 0 {
			continue
		}

		b = bytes.Trim(b, "\n")
		value, err := parser.ParseBytes(b)
		if err != nil {
			return newResponseError(fmt.Errorf("error decode response: %s", err), backend.StatusInternal)
		}

		if value.Exists(messageField) {
			message := value.GetStringBytes(messageField)
			lineField.Append(string(message))
		}
		if value.Exists(timeField) {
			t := value.GetStringBytes(timeField)
			getTime, err := utils.GetTime(string(t))
			if err != nil {
				return newResponseError(fmt.Errorf("error parse time from _time field: %s", err), backend.StatusInternal)
			}
			timeFd.Append(getTime)
		}

		// Build the labels map from _stream fields and any other extra fields.
		// _stream fields represent the structured metadata (app, namespace, etc.).
		// Any other field in the JSON object is also promoted to a label so users
		// can filter and group by arbitrary extracted fields.
		labels := data.Labels{}
		if value.Exists(streamField) {
			stream := value.GetStringBytes(streamField)
			stf, err := utils.ParseStreamFields(string(stream))
			if err != nil {
				return newResponseError(fmt.Errorf("%s", err), backend.StatusInternal)
			}
			for _, field := range stf {
				labels[field.Label] = field.Value
			}
			// If there is no _msg field but there are stream fields, we still need
			// a Line value so the row count stays aligned across all three arrays.
			if !value.Exists(messageField) && len(stf) >= 0 {
				lineField.Append("")
			}
		}

		// Iterate over all JSON fields and add any non-reserved ones to labels.
		// Reserved fields (_time, _stream, _msg) are handled above; skip them here.
		obj, err := value.Object()
		if err != nil {
			return newResponseError(fmt.Errorf("error get object from decoded response: %s", err), backend.StatusInternal)
		}
		obj.Visit(func(key []byte, v *fastjson.Value) {
			if bytes.Equal(key, []byte(timeField)) ||
				bytes.Equal(key, []byte(streamField)) ||
				bytes.Equal(key, []byte(messageField)) {
				return
			}
			fieldName := string(key)
			value := string(v.GetStringBytes())
			labels[fieldName] = value
		})

		d, err := labelsToJSON(labels)
		if err != nil {
			return newResponseError(err, backend.StatusInternal)
		}
		labelsField.Append(d)
	}

	// Grafana's Logs panel requires the Line field to be non-empty.
	// If no _msg field was present, fall back to displaying the raw labels JSON.
	if lineField.Len() == 0 {
		for i := 0; i < labelsField.Len(); i++ {
			label := labelsField.At(i)
			lineField.Append(fmt.Sprintf("%s", label))
		}
	}

	// Grafana's Logs panel requires the Time field to be non-empty.
	// If VictoriaLogs omitted _time (which shouldn't happen normally),
	// we use the current time as a fallback to avoid a broken panel.
	if timeFd.Len() == 0 {
		now := nowFunc()
		for i := 0; i < lineField.Len(); i++ {
			timeFd.Append(now)
		}
	}

	frame := data.NewFrame("", timeFd, lineField, labelsField)

	rsp := backend.DataResponse{}
	// FrameMeta must be set (even as empty) for Grafana to recognize the frame structure.
	frame.Meta = &data.FrameMeta{}
	rsp.Frames = append(rsp.Frames, frame)

	return rsp
}

// parseStreamResponse reads an NDJSON stream from VictoriaLogs' tail endpoint and
// sends each parsed log line as a separate data.Frame through the provided channel.
//
// WHY one frame per line (instead of batching like parseInstantResponse)?
// Live tail connections are long-lived. Batching would delay delivery until the
// batch is full, breaking the "real-time" UX. Sending one frame per line ensures
// each log entry appears in Grafana as soon as it arrives from VictoriaLogs.
//
// The caller (RunStream) reads from ch and forwards frames to Grafana's streaming
// infrastructure. The channel is closed when this function returns (in Dispose).
func parseStreamResponse(reader io.Reader, ch chan *data.Frame) error {

	br := bufio.NewReaderSize(reader, 64*1024)
	var parser fastjson.Parser
	var finishedReading bool
	for n := 0; !finishedReading; n++ {
		labelsField := data.NewFieldFromFieldType(data.FieldTypeJSON, 0)
		labelsField.Name = gLabelsField

		timeFd := data.NewFieldFromFieldType(data.FieldTypeTime, 0)
		timeFd.Name = gTimeField

		lineField := data.NewFieldFromFieldType(data.FieldTypeString, 0)
		lineField.Name = gLineField

		b, err := br.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				backend.Logger.Debug("skipping line number: line too long", "lineNumber", n)
				continue
			}
			if errors.Is(err, io.EOF) {
				// b can be != nil when EOF is returned, so we need to process it
				finishedReading = true
			} else {
				return fmt.Errorf("cannot read line in response: %s", err)
			}
		}

		if len(b) == 0 {
			continue
		}

		b = bytes.Trim(b, "\n")
		value, err := parser.ParseBytes(b)
		if err != nil {
			return fmt.Errorf("error decode response: %s", err)
		}

		if value.Exists(messageField) {
			message := value.GetStringBytes(messageField)
			lineField.Append(string(message))
		}
		if value.Exists(timeField) {
			t := value.GetStringBytes(timeField)
			getTime, err := utils.GetTime(string(t))
			if err != nil {
				return fmt.Errorf("error parse time from _time field: %s", err)
			}
			timeFd.Append(getTime)
		}

		labels := data.Labels{}
		if value.Exists(streamField) {
			stream := value.GetStringBytes(streamField)
			stf, err := utils.ParseStreamFields(string(stream))
			if err != nil {
				return err
			}
			for _, field := range stf {
				labels[field.Label] = field.Value
			}
		}

		obj, err := value.Object()
		if err != nil {
			return fmt.Errorf("error get object from decoded response: %s", err)
		}
		obj.Visit(func(key []byte, v *fastjson.Value) {
			if bytes.Equal(key, []byte(timeField)) ||
				bytes.Equal(key, []byte(streamField)) ||
				bytes.Equal(key, []byte(messageField)) {
				return
			}
			fieldName := string(key)
			value := string(v.GetStringBytes())
			labels[fieldName] = value
		})

		d, err := labelsToJSON(labels)
		if err != nil {
			return err
		}
		labelsField.Append(d)

		// Grafana expects lineFields to be always non-empty.
		if lineField.Len() == 0 {
			for i := 0; i < labelsField.Len(); i++ {
				label := labelsField.At(i)
				lineField.Append(fmt.Sprintf("%s", label))
			}
		}

		// Grafana expects time field to be always non-empty.
		if timeFd.Len() == 0 {
			now := nowFunc()
			for i := 0; i < lineField.Len(); i++ {
				timeFd.Append(now)
			}
		}

		frame := data.NewFrame("", timeFd, lineField, labelsField)
		// this is necessary information because the logs visualization is preferred
		frame.Meta = &data.FrameMeta{PreferredVisualization: logsVisualisation}

		ch <- frame
	}

	return nil
}

func parseStatsResponse(reader io.Reader, q *Query) backend.DataResponse {
	var rs Response
	if err := json.NewDecoder(reader).Decode(&rs); err != nil {
		err = fmt.Errorf("failed to decode body response: %w", err)
		return newResponseError(err, backend.StatusInternal)
	}
	rs.ForAlerting = q.ForAlerting

	frames, err := rs.getDataFrames()
	if err != nil {
		err = fmt.Errorf("failed to prepare data from response: %w", err)
		return newResponseError(err, backend.StatusInternal)
	}

	for i := range frames {
		q.addMetadataToMultiFrame(frames[i])
		q.addIntervalToFrame(frames[i])
	}

	return backend.DataResponse{Frames: frames}
}

func parseHitsResponse(reader io.Reader) backend.DataResponse {
	var hr HitsResponse
	if err := json.NewDecoder(reader).Decode(&hr); err != nil {
		err = fmt.Errorf("failed to decode body response: %w", err)
		return newResponseError(err, backend.StatusInternal)
	}

	frames, err := hr.getDataFrames()
	if err != nil {
		err = fmt.Errorf("failed to prepare data from response: %w", err)
		return newResponseError(err, backend.StatusInternal)
	}

	return backend.DataResponse{Frames: frames}
}

// parseErrorResponse reads data from the reader and returns error
func parseErrorResponse(reader io.Reader) error {
	var rs Response
	if err := json.NewDecoder(reader).Decode(&rs); err != nil {
		err = fmt.Errorf("failed to decode body response: %w", err)
		return err
	}

	if rs.Status == "error" {
		return fmt.Errorf("error: %s", rs.Error)
	}

	if rs.Error == "" {
		return fmt.Errorf("got unexpected error from the datasource")
	}

	return nil
}

// parseStringResponseError reads data from the reader and returns error
func parseStringResponseError(reader io.Reader) error {
	d, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("failed to read body response: %w", err)
	}
	if len(d) == 0 {
		return fmt.Errorf("got empty response from the datasource")
	}
	return fmt.Errorf("error from datasource: %s", string(d))
}

// labelsToJSON serializes Grafana labels to a JSON RawMessage.
// data.Labels (a map[string]string) produces deterministically sorted JSON keys,
// which is important for Grafana's label matching and deduplication logic.
func labelsToJSON(labels data.Labels) (json.RawMessage, error) {
	b, err := json.Marshal(labels)
	if err != nil {
		return nil, err
	}

	return b, nil
}

// ResultType values returned by VictoriaLogs stats endpoints.
// These mirror the Prometheus query API result types, allowing the same
// response parsing code to serve both instant and range queries.
const (
	// vector is returned by /stats_query (instant): one value per metric series.
	// Each Result has a single Value (timestamp + float64 string).
	vector = "vector"
	// matrix is returned by /stats_query_range: multiple values per metric series.
	// Each Result has a Values slice (array of [timestamp, float64 string] pairs).
	matrix = "matrix"
)

// Result represents a single time-series result from a stats query.
// It carries the labels that identify the series and the actual data points.
type Result struct {
	// Labels (called "metric" in JSON for Prometheus compatibility) contains
	// all the label key-value pairs that identify this time series.
	Labels Labels `json:"metric"`
	// Values contains multiple data points for matrix (range) results.
	// Each Value is a [timestamp, floatString] pair.
	Values []Value `json:"values"`
	// Value contains a single data point for vector (instant) results.
	Value Value `json:"value"`
}

// Value is a [timestamp, floatString] pair from the Prometheus-compatible response.
// Index 0 is the Unix timestamp as a float64 (seconds with nanosecond precision).
// Index 1 is the metric value as a string (e.g. "42.5" or "NaN").
// WHY string for the value? Prometheus uses strings to allow "NaN", "+Inf", "-Inf".
type Value [2]interface{}

// Labels is a map of label name → value for a single time series.
// These come from VictoriaLogs' stats query "metric" field.
type Labels map[string]string

// Data is the inner payload of a Prometheus-compatible query response.
// ResultType determines how to interpret the Result field.
type Data struct {
	// ResultType is either "vector" (instant) or "matrix" (range).
	ResultType string `json:"resultType"`
	// Result is kept as raw JSON so we can unmarshal it into the correct
	// Go type after we know the ResultType (see logStats.vectorDataFrames / matrixDataFrames).
	Result json.RawMessage `json:"result"`
}

// Response is the top-level Prometheus-compatible response from VictoriaLogs
// stats query endpoints (/stats_query and /stats_query_range).
type Response struct {
	Status string `json:"status"`
	Data   Data   `json:"data"`
	Error  string `json:"error"`
	// ForAlerting is not part of the JSON response; it's set internally to
	// switch between the regular frame format (time series) and the alerting
	// frame format (numeric multi) when building data frames.
	ForAlerting bool `json:"-"`
}

// logStats is an intermediate struct used during response parsing.
// It wraps the Result slice from the parsed JSON so we can call
// type-specific methods (vectorDataFrames / matrixDataFrames / alertingDataFrames).
type logStats struct {
	Result []Result `json:"result"`
}

// vectorDataFrames converts instant stats results (one value per series) into
// Grafana time series frames. Each result produces a two-column frame:
// [Time (single timestamp), Value (single float64, nullable)].
func (ls logStats) vectorDataFrames() (data.Frames, error) {
	frames := make(data.Frames, len(ls.Result))
	for i, res := range ls.Result {
		value := res.Value

		ts, err := getTimestamp(value[0])
		if err != nil {
			return nil, fmt.Errorf("failed to parse timestamp for metric %v: %w", res, err)
		}

		// valuePtr is *float64 (nullable) to represent "NaN" or missing values.
		// Grafana treats nil pointers as "no data" instead of 0, which is correct
		// for metrics that have gaps.
		valuePtr, err := getFloatPtr(value[1])
		if err != nil {
			return nil, fmt.Errorf("failed to parse float value for metric %v: %w", res, err)
		}

		frames[i] = data.NewFrame("",
			data.NewField(data.TimeSeriesTimeFieldName, nil, []time.Time{ts}),
			data.NewField(data.TimeSeriesValueFieldName, data.Labels(res.Labels), []*float64{valuePtr}))
	}

	return frames, nil
}

// alertingDataFrames produces frames in the format required by Grafana's alerting engine.
// WHY different from vectorDataFrames?
// Grafana's alerting evaluation expects FrameTypeNumericMulti with TypeVersion [0,1]
// and non-nullable float64 values. Regular time-series frames are not evaluated
// by the alerting engine. This is a Grafana internals requirement.
func (ls logStats) alertingDataFrames() (data.Frames, error) {
	frames := make(data.Frames, len(ls.Result))
	for i, res := range ls.Result {
		f, err := strconv.ParseFloat(res.Value[1].(string), 64)
		if err != nil {
			return nil, fmt.Errorf("metric %v, unable to parse timestamp to float64 from %s: %w", res, res.Value[1], err)
		}

		frames[i] = data.NewFrame("",
			data.NewField(data.TimeSeriesValueFieldName, data.Labels(res.Labels), []float64{f})).
			// FrameTypeNumericMulti with version [0,1] is the contract that Grafana's
			// alerting engine uses to identify evaluable frames. Without this metadata,
			// the alert rule would silently produce no results.
			SetMeta(&data.FrameMeta{
				Type:        data.FrameTypeNumericMulti,
				TypeVersion: data.FrameTypeVersion{0, 1},
			})
	}

	return frames, nil
}

// matrixDataFrames converts range stats results (multiple values per series) into
// Grafana time series frames. Each result produces a two-column frame:
// [Time (array of timestamps), Value (array of float64, nullable)].
func (ls logStats) matrixDataFrames() (data.Frames, error) {
	frames := make(data.Frames, len(ls.Result))
	for i, res := range ls.Result {
		timestamps := make([]time.Time, len(res.Values))
		values := make([]*float64, len(res.Values))

		for j, value := range res.Values {
			t, err := getTimestamp(value[0])
			if err != nil {
				return nil, fmt.Errorf("failed to parse timestamp response for metric %v: %w", res, err)
			}
			timestamps[j] = t

			fPtr, err := getFloatPtr(value[1])
			if err != nil {
				return nil, fmt.Errorf("failed to parse float value response for metric %v: %w", res, err)
			}
			values[j] = fPtr
		}

		if len(values) < 1 || len(timestamps) < 1 {
			return nil, fmt.Errorf("log %v contains no values", res)
		}

		frames[i] = data.NewFrame("",
			data.NewField(data.TimeSeriesTimeFieldName, nil, timestamps),
			data.NewField(data.TimeSeriesValueFieldName, data.Labels(res.Labels), values))
	}

	return frames, nil
}

// getDataFrames converts a stats Response into Grafana data frames.
// It selects the correct frame builder based on ResultType and whether the
// query is for alerting or regular display.
func (r *Response) getDataFrames() (data.Frames, error) {
	var ls logStats
	if err := json.Unmarshal(r.Data.Result, &ls.Result); err != nil {
		return nil, fmt.Errorf("unmarshal err %s; \n %#v", err, string(r.Data.Result))
	}

	switch r.Data.ResultType {
	case vector:
		if r.ForAlerting {
			// Alerting requires a special frame format; see alertingDataFrames for details.
			return ls.alertingDataFrames()
		}
		return ls.vectorDataFrames()
	case matrix:
		return ls.matrixDataFrames()
	default:
		return nil, fmt.Errorf("unknown result type %q", r.Data.ResultType)
	}
}

// Hit represents a single bucket from a /select/logsql/hits response.
// Each Hit describes how many log entries matched the query within each time interval,
// optionally broken down by field values (e.g. by app, or by log level).
type Hit struct {
	// Fields contains the field values that identify this bucket.
	// Example: {"app": "nginx", "level": "error"}
	// Empty when no field grouping was requested.
	Fields map[string]string `json:"fields"`
	// Timestamps is a parallel array to Values: each entry is the start of
	// one time bucket (RFC3339 string).
	Timestamps []string `json:"timestamps"`
	// Values is a parallel array to Timestamps: each entry is the log count
	// for that time bucket.
	Values []float64 `json:"values"`
	// Total is the overall count across all buckets in this Hit.
	Total int `json:"total"`
}

// HitsResponse is the top-level response from /select/logsql/hits.
// It contains one Hit per unique combination of requested field values.
type HitsResponse struct {
	Hits []Hit `json:"hits"`
}

// getDataFrames converts a HitsResponse into one Grafana frame per Hit.
// Each frame has two columns: Time (array) and Value (array with field labels).
// The field labels enable Grafana to display one line per field-value combination
// in a time series chart (e.g. one line per log level).
func (hr *HitsResponse) getDataFrames() (data.Frames, error) {
	frames := make(data.Frames, len(hr.Hits))
	for i, hit := range hr.Hits {
		if len(hit.Timestamps) != len(hit.Values) {
			return nil, fmt.Errorf("timestamps and values length mismatch: %d != %d", len(hit.Timestamps), len(hit.Values))
		}

		timeFd := data.NewFieldFromFieldType(data.FieldTypeTime, len(hit.Timestamps))
		timeFd.Name = gTimeField

		valueFd := data.NewFieldFromFieldType(data.FieldTypeFloat64, len(hit.Values))
		valueFd.Name = gValueField
		valueFd.Labels = make(data.Labels)

		for j, ts := range hit.Timestamps {
			getTime, err := utils.GetTime(ts)
			if err != nil {
				return nil, fmt.Errorf("error parse time from _time field: %s", err)
			}
			timeFd.Set(j, getTime)
		}

		for k, v := range hit.Values {
			valueFd.Set(k, v)
		}

		for key, value := range hit.Fields {
			valueFd.Labels[key] = value
			d, err := labelsToJSON(valueFd.Labels)
			if err != nil {
				return nil, fmt.Errorf("error convert labels to json: %s", err)
			}
			valueFd.Config = &data.FieldConfig{DisplayNameFromDS: string(d)}
		}

		frames[i] = data.NewFrame("", timeFd, valueFd)
	}

	return frames, nil
}

// getTimestamp converts a Prometheus-style timestamp (float64 Unix seconds)
// to a Go time.Time with nanosecond precision.
//
// WHY float64? JSON doesn't have a distinct integer type; encoding/json decodes
// all JSON numbers into interface{} as float64. The fractional part encodes
// sub-second precision: e.g. 1704067200.123456789 → seconds=1704067200, ns=123456789.
func getTimestamp(value interface{}) (time.Time, error) {
	v, ok := value.(float64)
	if !ok {
		return time.Time{}, fmt.Errorf("failed to convert timestamp to float64 for value %v", value)
	}

	seconds := int64(v)                                // integer part = Unix seconds
	nanoseconds := int64((v - float64(seconds)) * 1e9) // fractional part → nanoseconds
	t := time.Unix(seconds, nanoseconds)
	return t, nil
}

// getFloatPtr converts a Prometheus-style metric value (JSON string) to *float64.
//
// WHY string in JSON? Prometheus encodes numeric values as strings to support
// special IEEE 754 values: "NaN", "+Inf", "-Inf" which are not valid JSON numbers.
//
// WHY *float64 (pointer)? A nil pointer represents "no data" for a time bucket,
// which Grafana renders as a gap in the chart. Using 0.0 would be misleading
// because it implies a real measurement of zero was recorded.
func getFloatPtr(value interface{}) (*float64, error) {
	f, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("unable to convert log value to string from %v", value)
	}

	if f == "" {
		// Empty string means no value for this bucket; return nil to signal "no data".
		return nil, nil
	}

	flVal, err := strconv.ParseFloat(f, 64)
	if err != nil {
		return nil, fmt.Errorf("unable to parse log value to float64 from %v: %w", value, err)
	}

	floatPtr := utils.Ptr(flVal)
	return floatPtr, nil
}
