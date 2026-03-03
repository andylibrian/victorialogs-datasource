package plugin

/**
 * Query URL Building and Request Construction
 *
 * This file handles the construction of HTTP request URLs for different query types.
 * It translates query parameters into the URL format that VictoriaLogs expects.
 *
 * ARCHITECTURE:
 * The query type determines which VictoriaLogs endpoint to use:
 * - instant → /select/logsql/query (log lines)
 * - stats → /select/logsql/stats_query (point-in-time stats)
 * - statsRange → /select/logsql/stats_query_range (stats over time)
 * - hits → /select/logsql/hits (histogram for log volumes)
 * - tail → /select/logsql/tail (live streaming)
 *
 * WHY DIFFERENT QUERY TYPES?
 * VictoriaLogs provides different endpoints optimized for different use cases:
 * - instant: Returns individual log lines (fast for large result sets)
 * - stats: Returns aggregated metrics at a point in time (for dashboards)
 * - statsRange: Returns time series data for charts
 * - hits: Returns bucketed counts for log volume visualization
 * - tail: Returns streaming logs in real-time
 *
 * Each query type has different URL parameter requirements:
 * - Time range (start, end)
 * - Step (for range queries)
 * - Limit (for log queries)
 * - Field names (for hits queries)
 *
 * LEGEND FORMAT:
 * Users can customize how metrics are labeled in charts using legendFormat:
 * - __auto: Use the raw query expression as the legend
 * - Custom pattern: Use {{label}} to substitute label values
 * - Empty: Use default label formatting
 *
 * For more details, see onboarding/onboarding-query-flow.md
 */

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"

	"github.com/VictoriaMetrics/victorialogs-datasource/pkg/utils"
)

// VictoriaLogs API endpoint paths
// These paths are appended to the datasource URL to form complete endpoints.
// See: https://docs.victoriametrics.com/victorialogs/
const (
	// instantQueryPath returns individual log lines matching a LogsQL query.
	// This is the most common query type for viewing logs.
	instantQueryPath = "/select/logsql/query"

	// tailQueryPath returns a streaming response of log lines in real-time.
	// The connection stays open and sends new logs as they arrive.
	tailQueryPath = "/select/logsql/tail"

	// statsQueryPath returns aggregated statistics at a single point in time.
	// Used for dashboards showing current state.
	statsQueryPath = "/select/logsql/stats_query"

	// statsQueryRangePath returns aggregated statistics over a time range.
	// Used for time-series charts showing trends.
	statsQueryRangePath = "/select/logsql/stats_query_range"

	// hitsQueryPath returns a histogram of log counts grouped by field values.
	// Used for log volume visualization and analysis.
	hitsQueryPath = "/select/logsql/hits"

	// defaultMaxLines is the default limit for log queries when not specified.
	// This prevents overwhelming the browser with millions of log lines.
	defaultMaxLines = 1000

	// legendFormatAuto is a special value that means "use the query expression as legend".
	legendFormatAuto = "__auto"

	// metricsName is the label key for the metric name in VictoriaMetrics/VictoriaLogs.
	metricsName = "__name__"

	// defaultInterval is the default step interval when none is specified.
	defaultInterval = 15 * time.Second
)

// QueryType represents the type of query to execute.
// This determines which VictoriaLogs endpoint to use and how to format the response.
type QueryType string

const (
	// QueryTypeInstant retrieves individual log lines.
	// This is the default and most common query type.
	QueryTypeInstant QueryType = "instant"

	// QueryTypeStats retrieves point-in-time statistics.
	// Returns a single aggregated value (e.g., count, sum).
	QueryTypeStats QueryType = "stats"

	// QueryTypeStatsRange retrieves statistics over a time range.
	// Returns a time series of aggregated values.
	QueryTypeStatsRange QueryType = "statsRange"

	// QueryTypeHits retrieves a histogram of log counts.
	// Used for analyzing log volume by field values.
	QueryTypeHits QueryType = "hits"
)

// Query represents a parsed backend query with all parameters needed to build a request.
// It embeds backend.DataQuery (from Grafana) and adds VictoriaLogs-specific fields.
type Query struct {
	// backend.DataQuery contains standard Grafana query fields:
	// - RefID: Unique identifier for this query
	// - TimeRange: Start and end time for the query
	// - MaxDataPoints: Suggested number of data points for charts
	backend.DataQuery `json:"inline"`

	// Expr is the LogsQL query expression.
	// Example: "error" or "app:nginx AND status:500"
	Expr string `json:"expr"`

	// LegendFormat controls how the metric is labeled in charts.
	// Supports patterns like "{{app}}" to substitute label values.
	LegendFormat string `json:"legendFormat"`

	// TimeInterval is the minimum interval between data points.
	// This is a datasource-level setting.
	TimeInterval string `json:"timeInterval"`

	// Interval is the query-level interval override.
	// Users can set this in the query editor.
	Interval string `json:"interval"`

	// IntervalMs is the interval in milliseconds.
	// Set by Grafana based on the panel's time range and width.
	IntervalMs int64 `json:"intervalMs"`

	// MaxLines limits the number of log lines returned.
	// Only used for instant queries (log lines).
	MaxLines int `json:"maxLines"`

	// Step is the step interval for range queries.
	// If empty, it's calculated from the time range and MaxDataPoints.
	Step string `json:"step"`

	// Fields specifies which fields to include in hits query results.
	// Used to group log counts by specific field values.
	Fields []string `json:"fields"`

	// QueryType determines which VictoriaLogs endpoint to use.
	QueryType QueryType `json:"queryType"`

	// ExtraFilters is additional LogsQL to AND with the main expression.
	// Used by dashboard variables to filter all queries.
	ExtraFilters string `json:"extraFilters"`

	// TimezoneOffset is the timezone offset for range queries.
	// Format: "+HH:MM" or "-HH:MM"
	TimezoneOffset string `json:"timezoneOffset"`

	// url is the cached parsed URL (internal use only).
	url *url.URL

	// ForAlerting indicates if this query is from Grafana's alerting system.
	// Alert queries may need different frame formats.
	ForAlerting bool `json:"-"`
}

// getQueryURL builds the complete URL for a query based on its type.
// It handles:
//   - Parsing the base URL and query parameters
//   - Adding extra filters if specified
//   - Routing to the appropriate URL builder based on QueryType
//
// This is the main entry point for building query URLs. The returned URL
// is ready to be used in an HTTP request to VictoriaLogs.
func (q *Query) getQueryURL(rawURL string, queryParams string) (string, error) {
	if rawURL == "" {
		return "", fmt.Errorf("url can't be blank")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse datasource url: %s", err)
	}
	params, err := url.ParseQuery(queryParams)
	if err != nil {
		return "", fmt.Errorf("failed to parse query params: %s", err.Error())
	}

	// Extra filters allow dashboard variables to filter all queries.
	// They are ANDed with the main expression.
	if q.ExtraFilters != "" {
		params.Set("extra_filters", q.ExtraFilters)
	}

	q.url = u

	// Route to appropriate URL builder based on query type
	switch q.QueryType {
	case QueryTypeStats:
		return q.statsQueryURL(params), nil
	case QueryTypeStatsRange:
		minInterval, err := q.calculateMinInterval()
		if err != nil {
			return "", fmt.Errorf("failed to calculate minimal interval: %w", err)
		}
		return q.statsQueryRangeURL(params, minInterval), nil
	case QueryTypeHits:
		minInterval, err := q.calculateMinInterval()
		if err != nil {
			return "", fmt.Errorf("failed to calculate minimal interval: %w", err)
		}
		return q.hitsQueryURL(params, minInterval), nil
	default:
		return q.queryInstantURL(params), nil
	}
}

// queryInstantURL prepare query url for instant query
func (q *Query) queryTailURL(rawURL string, queryParams string) (string, error) {
	if rawURL == "" {
		return "", fmt.Errorf("url can't be blank")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse datasource url: %s", err)
	}
	params, err := url.ParseQuery(queryParams)
	if err != nil {
		return "", fmt.Errorf("failed to parse query params: %s", err.Error())
	}

	q.url = u

	q.url.Path = path.Join(q.url.Path, tailQueryPath)
	values := q.url.Query()

	for k, vl := range params {
		for _, v := range vl {
			values.Add(k, v)
		}
	}

	q.Expr = utils.ReplaceTemplateVariable(q.Expr, q.IntervalMs, q.TimeRange)
	values.Set("query", q.Expr)

	q.url.RawQuery = values.Encode()
	return q.url.String(), nil
}

// queryInstantURL prepare query url for instant query
func (q *Query) queryInstantURL(queryParams url.Values) string {
	q.url.Path = path.Join(q.url.Path, instantQueryPath)
	values := q.url.Query()

	for k, vl := range queryParams {
		for _, v := range vl {
			values.Add(k, v)
		}
	}

	if q.MaxLines <= 0 {
		q.MaxLines = defaultMaxLines
	}

	now := time.Now()
	if q.TimeRange.From.IsZero() {
		q.TimeRange.From = now.Add(-time.Minute * 5)
	}
	if q.TimeRange.To.IsZero() {
		q.TimeRange.To = now
	}

	q.Expr = utils.ReplaceTemplateVariable(q.Expr, q.IntervalMs, q.TimeRange)
	values.Set("query", q.Expr)
	values.Set("limit", strconv.Itoa(q.MaxLines))
	values.Set("start", strconv.FormatInt(q.TimeRange.From.Unix(), 10))
	values.Set("end", strconv.FormatInt(q.TimeRange.To.Unix(), 10))

	q.url.RawQuery = values.Encode()
	return q.url.String()
}

// statsQueryURL prepare query url for querying log stats
func (q *Query) statsQueryURL(queryParams url.Values) string {
	q.url.Path = path.Join(q.url.Path, statsQueryPath)
	values := q.url.Query()

	for k, vl := range queryParams {
		for _, v := range vl {
			values.Add(k, v)
		}
	}

	now := time.Now()
	if q.TimeRange.From.IsZero() {
		q.TimeRange.From = now.Add(-time.Minute * 5)
	}

	q.Expr = utils.ReplaceTemplateVariable(q.Expr, q.IntervalMs, q.TimeRange)
	q.Expr = utils.AddTimeFieldWithRange(q.Expr, q.TimeRange)

	values.Set("query", q.Expr)
	values.Set("time", strconv.FormatInt(q.TimeRange.To.Unix(), 10))

	q.url.RawQuery = values.Encode()
	return q.url.String()
}

// statsQueryRangeURL prepare query url for querying log range stats
func (q *Query) statsQueryRangeURL(queryParams url.Values, minInterval time.Duration) string {
	q.url.Path = path.Join(q.url.Path, statsQueryRangePath)
	values := q.url.Query()

	for k, vl := range queryParams {
		for _, v := range vl {
			values.Add(k, v)
		}
	}

	if q.MaxLines <= 0 {
		q.MaxLines = defaultMaxLines
	}

	now := time.Now()
	if q.TimeRange.From.IsZero() {
		q.TimeRange.From = now.Add(-time.Minute * 5)
	}
	if q.TimeRange.To.IsZero() {
		q.TimeRange.To = now
	}

	q.Expr = utils.ReplaceTemplateVariable(q.Expr, q.IntervalMs, q.TimeRange)

	step := q.Step
	if step == "" {
		step = utils.CalculateStep(minInterval, q.TimeRange, q.MaxDataPoints).String()
	}

	values.Set("query", q.Expr)
	values.Set("start", strconv.FormatInt(q.TimeRange.From.Unix(), 10))
	values.Set("end", strconv.FormatInt(q.TimeRange.To.Unix(), 10))
	values.Set("step", step)
	if q.TimezoneOffset != "" {
		values.Set("offset", q.TimezoneOffset)
	}

	q.url.RawQuery = values.Encode()
	return q.url.String()
}

// hitsQueryURL prepare query url for querying log hits
func (q *Query) hitsQueryURL(queryParams url.Values, minInterval time.Duration) string {
	q.url.Path = path.Join(q.url.Path, hitsQueryPath)
	values := q.url.Query()

	for k, vl := range queryParams {
		for _, v := range vl {
			values.Add(k, v)
		}
	}

	now := time.Now()
	if q.TimeRange.From.IsZero() {
		q.TimeRange.From = now.Add(-time.Minute * 5)
	}
	if q.TimeRange.To.IsZero() {
		q.TimeRange.To = now
	}

	q.Expr = utils.ReplaceTemplateVariable(q.Expr, q.IntervalMs, q.TimeRange)

	step := q.Step
	if step == "" {
		step = utils.CalculateStep(minInterval, q.TimeRange, q.MaxDataPoints).String()
	}

	values.Set("query", q.Expr)
	values.Set("start", strconv.FormatInt(q.TimeRange.From.Unix(), 10))
	values.Set("end", strconv.FormatInt(q.TimeRange.To.Unix(), 10))
	values.Set("step", step)
	if q.TimezoneOffset != "" {
		values.Set("offset", q.TimezoneOffset)
	}
	for _, f := range q.Fields {
		values.Add("field", f)
	}

	q.url.RawQuery = values.Encode()
	return q.url.String()
}

// addMetadataToMultiFrame applies legend formatting to a stats/hits frame.
// It sets both the field's DisplayNameFromDS (used by the Grafana legend) and
// the frame's Name, which some panel types use as the series title.
//
// Only frames with at least two fields (a time column and a value column) are
// modified; single-field frames don't have labels to format.
func (q *Query) addMetadataToMultiFrame(frame *data.Frame) {
	if len(frame.Fields) < 2 {
		return
	}

	// Field index 1 is the value field (index 0 is the time field).
	// Its Labels contain the metric identifiers from the VictoriaLogs response.
	customName := q.parseLegend(frame.Fields[1].Labels)
	if customName != "" {
		// DisplayNameFromDS overrides Grafana's automatic name generation,
		// which would otherwise show the raw field name ("Value").
		frame.Fields[1].Config = &data.FieldConfig{DisplayNameFromDS: customName}
	}

	frame.Name = customName
}

// addIntervalToFrame sets the Interval hint on the time field of a frame.
// Grafana uses this hint to correctly space data points in time series panels
// when the step is not constant (e.g., after daylight saving time transitions).
func (q *Query) addIntervalToFrame(frame *data.Frame) {
	if len(frame.Fields) > 0 && q.IntervalMs > 0 {
		if frame.Fields[0].Config == nil {
			frame.Fields[0].Config = &data.FieldConfig{}
		}
		frame.Fields[0].Config.Interval = float64(q.IntervalMs)
	}
}

// legendReplacer matches {{label}} and {{ label }} patterns in a legendFormat string.
// Surrounding whitespace inside the braces is trimmed before lookup.
var legendReplacer = regexp.MustCompile(`\{\{\s*(.+?)\s*\}\}`)

// parseLegend resolves the final display name for a time series based on LegendFormat.
//
// Three modes (in priority order):
//  1. "__auto"  → use the raw LogsQL expression (expr) as the legend.
//     Useful when users want to see exactly what query produced each series.
//  2. Custom pattern like "{{app}} - {{level}}" → substitute label values.
//     Falls back to expr if all substitutions result in an empty string
//     (e.g. the labels don't contain the referenced keys).
//  3. Empty string → format the labels as a Prometheus-style label set "{key=value,...}".
//     Falls back to expr if the result would be the empty set "{}".
func (q *Query) parseLegend(labels data.Labels) string {

	switch {
	case q.LegendFormat == legendFormatAuto:
		return q.Expr
	case q.LegendFormat != "":
		result := legendReplacer.ReplaceAllStringFunc(q.LegendFormat, func(in string) string {
			labelName := strings.Replace(in, "{{", "", 1)
			labelName = strings.Replace(labelName, "}}", "", 1)
			labelName = strings.TrimSpace(labelName)
			if val, ok := labels[labelName]; ok {
				return val
			}
			return ""
		})
		if result == "" {
			return q.Expr
		}
		return result
	default:
		// If legend is empty brackets, use query expression
		legend := labelsToString(labels)
		if legend == "{}" {
			return q.Expr
		}
		return legend
	}
}

// labelsToString formats a labels map as a Prometheus-style label set string.
//
// The __name__ label (metricsName) is treated specially: it forms the metric name
// prefix before the { } block, mirroring how Prometheus displays series like:
//   http_requests_total{method="GET",status="200"}
//
// Label pairs are sorted alphabetically for deterministic output. This matters
// because different map iteration orders would produce different legend strings,
// making it impossible to correlate series across panel refreshes.
func labelsToString(labels data.Labels) string {
	if labels == nil {
		return "{}"
	}

	var labelStrings []string
	for label, value := range labels {
		if label == metricsName {
			continue // __name__ is handled separately as the metric name prefix
		}
		labelStrings = append(labelStrings, fmt.Sprintf("%s=%q", label, value))
	}

	var metricName string
	mn, ok := labels[metricsName]
	if ok {
		metricName = mn
	}

	if len(labelStrings) < 1 {
		// No additional labels; return just the metric name (or empty string).
		return metricName
	}

	sort.Strings(labelStrings)
	lbs := strings.Join(labelStrings, ",")

	return fmt.Sprintf("%s{%s}", metricName, lbs)
}

// calculateMinInterval tries to calculate interval from requested params
// in duration representation or return error if
func (q *Query) calculateMinInterval() (time.Duration, error) {
	if utils.WithIntervalVariable(q.Interval) {
		q.Interval = ""
	}
	return utils.GetIntervalFrom(q.TimeInterval, q.Interval, q.IntervalMs, defaultInterval)
}
