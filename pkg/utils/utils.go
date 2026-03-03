// utils.go — Shared Utility Functions
//
// This file provides helpers used across the plugin backend:
//   - Time parsing: converting many timestamp formats to Go time.Time
//   - Template variable substitution: replacing $__interval, $__range, etc.
//   - Step/interval calculation: computing appropriate query resolution
//   - LogsQL expression manipulation: adding _time filters to stats queries
//
// TIME FORMAT SUPPORT:
// VictoriaLogs accepts timestamps in many formats (see ParseTimeAt). The plugin
// must handle all of them because:
//   - Grafana sends start/end as Unix milliseconds or RFC3339 strings
//   - Users can type literal times like "2024-01-01" or "now-1h" into query fields
//   - VictoriaLogs' own _time field uses RFC3339Nano
//
// STEP / INTERVAL CALCULATION:
// For time-series queries, we must decide how many data points to return.
// Grafana tells us MaxDataPoints (panel width in pixels) and IntervalMs.
// CalculateStep divides the time range by MaxDataPoints and then snaps the
// result to a "nice" human-readable value (see roundInterval).

package utils

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/VictoriaMetrics/metricsql"
	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/gtime"
)

// Grafana template variable placeholders that appear in LogsQL expressions.
// These are replaced with computed values at query time (see ReplaceTemplateVariable).
const (
	varInterval   = "$__interval"    // Human-readable interval, e.g. "15s"
	varIntervalMs = "$__interval_ms" // Interval in milliseconds as an integer string
	varRange      = "$__range"       // Full time range as a LogsQL range literal
)

// timeField is the VictoriaLogs reserved field name for the log timestamp.
// It is used as a filter prefix in stats queries: _time:[start, end].
const timeField = "_time"

// Nanosecond constants for timezone offset arithmetic.
// Using explicit nanosecond values avoids repeated multiplication in hot paths.
const (
	nsecsPerHour   = 3600 * 1e9
	nsecsPerMinute = 60 * 1e9
)

var (
	// defaultResolution is the fallback number of data points when Grafana
	// does not specify MaxDataPoints. 1500 matches Grafana's default panel width,
	// giving roughly one data point per pixel on a typical display.
	defaultResolution int64 = 1500

	// year and day are used in formatDuration to produce human-readable interval strings.
	year = time.Hour * 24 * 365
	day  = time.Hour * 24
)

const (
	// Nanosecond bounds for safe int64 time storage.
	// minTimeNsecs is 0 (not the mathematical minimum of int64) because VictoriaLogs'
	// storage engine does not support negative timestamps (pre-1970 times).
	// maxTimeNsecs is int64 max, which corresponds to the year ~2262.
	minTimeNsecs = 0
	maxTimeNsecs = int64(1<<63 - 1)
	// maxTimeMsecs is used when converting millisecond-precision timestamps that
	// might overflow if naively multiplied to nanoseconds.
	maxTimeMsecs = maxTimeNsecs / 1e6
)

// GetTime parses a timestamp string into a Go time.Time.
//
// It tries RFC3339Nano first (the native VictoriaLogs _time format) for performance.
// If that fails, it falls back to ParseTime which handles a wide range of formats
// (see ParseTimeAt). The result is clamped to [0, maxTimeNsecs] to prevent overflow.
//
// All times are returned in UTC for consistency across timezones.
func GetTime(s string) (time.Time, error) {
	if nsecs, ok := TryParseTimestampRFC3339Nano(s); ok {
		if nsecs < minTimeNsecs {
			nsecs = 0
		}
		if nsecs > maxTimeNsecs {
			nsecs = maxTimeNsecs
		}
		return time.Unix(0, nsecs).UTC(), nil
	}

	// Fall back to the more general parser which handles Unix timestamps,
	// truncated ISO dates (YYYY, YYYY-MM, etc.), and relative expressions like "now-1h".
	secs, err := ParseTime(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("cannot parse %s: %w", s, err)
	}
	// Convert seconds to milliseconds, then clamp before multiplying to nanoseconds
	// to avoid int64 overflow with very large timestamps.
	msecs := int64(secs * 1e3)
	if msecs < minTimeNsecs {
		msecs = 0
	}
	if msecs > maxTimeMsecs {
		msecs = maxTimeMsecs
	}

	return time.Unix(0, msecs*int64(time.Millisecond)).UTC(), nil
}

// ParseTime parses time s in different formats.
//
// See https://docs.victoriametrics.com/victoriametrics/single-server-victoriametrics/#timestamp-formats
//
// It returns unix timestamp in seconds.
func ParseTime(s string) (float64, error) {
	currentTimestamp := float64(time.Now().UnixNano()) / 1e9
	return ParseTimeAt(s, currentTimestamp)
}

const (
	// time.UnixNano can only store maxInt64, which is 2262
	maxValidYear = 2262
	minValidYear = 1970
)

// ParseTimeAt parses time s in different formats, assuming the given currentTimestamp.
//
// See https://docs.victoriametrics.com/victoriametrics/single-server-victoriametrics/#timestamp-formats
//
// WHY so many format checks?
// VictoriaLogs is flexible about what timestamps users can type. Supporting truncated
// ISO dates (just YYYY, or YYYY-MM) lets users write readable queries like
//   _time:[2024-01-01, 2024-02-01]
// without specifying the full RFC3339 timestamp.
//
// The format selection is done by string length because it is unambiguous:
// date strings of the same format always have the same length. This is faster
// than trying multiple formats with time.Parse and discarding parse errors.
//
// It returns unix timestamp in seconds.
func ParseTimeAt(s string, currentTimestamp float64) (float64, error) {
	if s == "now" {
		return currentTimestamp, nil
	}
	sOrig := s
	tzOffset := float64(0)
	if len(sOrig) > 6 {
		// Try parsing timezone offset
		tz := sOrig[len(sOrig)-6:]
		if (tz[0] == '-' || tz[0] == '+') && tz[3] == ':' {
			isPlus := tz[0] == '+'
			hour, err := strconv.ParseUint(tz[1:3], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("cannot parse hour from timezone offset %q: %w", tz, err)
			}
			minute, err := strconv.ParseUint(tz[4:], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("cannot parse minute from timezone offset %q: %w", tz, err)
			}
			tzOffset = float64(hour*3600 + minute*60)
			if isPlus {
				tzOffset = -tzOffset
			}
			s = sOrig[:len(sOrig)-6]
		}
	}
	s = strings.TrimSuffix(s, "Z")
	if len(s) > 0 && (s[len(s)-1] > '9' || s[0] == '-') || strings.HasPrefix(s, "now") {
		// Parse duration relative to the current time
		s = strings.TrimPrefix(s, "now")
		d, err := ParseDuration(s)
		if err != nil {
			return 0, err
		}
		if d > 0 {
			d = -d
		}
		return currentTimestamp + float64(d)/1e9, nil
	}
	if len(s) == 4 {
		// Parse YYYY
		t, err := time.Parse("2006", s)
		if err != nil {
			return 0, err
		}
		y := t.Year()
		if y > maxValidYear || y < minValidYear {
			return 0, fmt.Errorf("cannot parse year from %q: year must in range [%d, %d]", s, minValidYear, maxValidYear)
		}
		return tzOffset + float64(t.UnixNano())/1e9, nil
	}
	if !strings.Contains(sOrig, "-") {
		// No dashes → this is a raw Unix timestamp (seconds or milliseconds).
		ts, err := strconv.ParseFloat(sOrig, 64)
		if err != nil {
			return 0, err
		}
		// Heuristic: values >= 2^32 (~4.3 billion) can't be Unix seconds in the
		// year 2024 (which is ~1.7 billion), so they must be milliseconds.
		if ts >= (1 << 32) {
			ts /= 1000
		}
		return ts, nil
	}
	if len(s) == 7 {
		// Parse YYYY-MM
		t, err := time.Parse("2006-01", s)
		if err != nil {
			return 0, err
		}
		return tzOffset + float64(t.UnixNano())/1e9, nil
	}
	if len(s) == 10 {
		// Parse YYYY-MM-DD
		t, err := time.Parse("2006-01-02", s)
		if err != nil {
			return 0, err
		}
		return tzOffset + float64(t.UnixNano())/1e9, nil
	}
	if len(s) == 13 {
		// Parse YYYY-MM-DDTHH
		t, err := time.Parse("2006-01-02T15", s)
		if err != nil {
			return 0, err
		}
		return tzOffset + float64(t.UnixNano())/1e9, nil
	}
	if len(s) == 16 {
		// Parse YYYY-MM-DDTHH:MM
		t, err := time.Parse("2006-01-02T15:04", s)
		if err != nil {
			return 0, err
		}
		return tzOffset + float64(t.UnixNano())/1e9, nil
	}
	if len(s) == 19 {
		// Parse YYYY-MM-DDTHH:MM:SS
		t, err := time.Parse("2006-01-02T15:04:05", s)
		if err != nil {
			return 0, err
		}
		return tzOffset + float64(t.UnixNano())/1e9, nil
	}
	// Parse RFC3339
	t, err := time.Parse(time.RFC3339, sOrig)
	if err != nil {
		return 0, err
	}
	return float64(t.UnixNano()) / 1e9, nil
}

// ParseDuration parses duration string in Prometheus format
func ParseDuration(s string) (time.Duration, error) {
	ms, err := metricsql.DurationValue(s, 0)
	if err != nil {
		return 0, err
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// TryParseTimestampRFC3339Nano parses s as RFC3339 with optional nanoseconds part and timezone offset and returns unix timestamp in nanoseconds.
//
// If s doesn't contain timezone offset, then the local timezone is used.
//
// The returned timestamp can be negative if s is smaller than 1970 year.x
func TryParseTimestampRFC3339Nano(s string) (int64, bool) {
	if len(s) < len("2006-01-02T15:04:05") {
		return 0, false
	}

	secs, ok, tail := tryParseTimestampSecs(s)
	if !ok {
		return 0, false
	}
	s = tail
	nsecs := secs * 1e9

	// Parse timezone offset
	offsetNsecs, prefix, ok := parseTimezoneOffset(s)
	if !ok {
		return 0, false
	}
	nsecs -= offsetNsecs
	s = prefix

	// Parse optional fractional part of seconds.
	if len(s) == 0 {
		return nsecs, true
	}
	if s[0] == '.' {
		s = s[1:]
	}
	digits := len(s)
	if digits > 9 {
		return 0, false
	}
	n64, ok := tryParseDateUint64(s)
	if !ok {
		return 0, false
	}

	if digits < 9 {
		n64 *= uint64(math.Pow10(9 - digits))
	}
	nsecs += int64(n64)
	return nsecs, true
}

func parseTimezoneOffset(s string) (int64, string, bool) {
	if strings.HasSuffix(s, "Z") {
		return 0, s[:len(s)-1], true
	}

	n := strings.LastIndexAny(s, "+-")
	if n < 0 {
		offsetNsecs := GetLocalTimezoneOffsetNsecs()
		return offsetNsecs, s, true
	}
	offsetStr := s[n+1:]
	isMinus := s[n] == '-'
	if len(offsetStr) == 0 {
		return 0, s, false
	}
	offsetNsecs, ok := tryParseHHMM(offsetStr)
	if !ok {
		return 0, s, false
	}
	if isMinus {
		offsetNsecs = -offsetNsecs
	}
	return offsetNsecs, s[:n], true
}

func tryParseHHMM(s string) (int64, bool) {
	if len(s) != len("hh:mm") || s[2] != ':' {
		return 0, false
	}
	hourStr := s[:2]
	minuteStr := s[3:]
	hours, ok := tryParseDateUint64(hourStr)
	if !ok || hours > 24 {
		return 0, false
	}
	minutes, ok := tryParseDateUint64(minuteStr)
	if !ok || minutes > 60 {
		return 0, false
	}
	return int64(hours)*nsecsPerHour + int64(minutes)*nsecsPerMinute, true
}

// tryParseTimestampSecs parses YYYY-MM-DDTHH:mm:ss into unix timestamp in seconds.
func tryParseTimestampSecs(s string) (int64, bool, string) {
	// Parse year
	if s[len("YYYY")] != '-' {
		return 0, false, s
	}
	yearStr := s[:len("YYYY")]
	n, ok := tryParseDateUint64(yearStr)
	if !ok || n < 1677 || n > 2262 {
		return 0, false, s
	}
	year := int(n)
	s = s[len("YYYY")+1:]

	// Parse month
	if s[len("MM")] != '-' {
		return 0, false, s
	}
	monthStr := s[:len("MM")]
	n, ok = tryParseDateUint64(monthStr)
	if !ok {
		return 0, false, s
	}
	month := time.Month(n)
	s = s[len("MM")+1:]

	// Parse day.
	//
	// Allow whitespace additionally to T as the delimiter after DD,
	// so SQL datetime format can be parsed additionally to RFC3339.
	// See https://github.com/VictoriaMetrics/VictoriaMetrics/issues/6721
	delim := s[len("DD")]
	if delim != 'T' && delim != ' ' {
		return 0, false, s
	}
	dayStr := s[:len("DD")]
	n, ok = tryParseDateUint64(dayStr)
	if !ok {
		return 0, false, s
	}
	day := int(n)
	s = s[len("DD")+1:]

	// Parse hour
	if s[len("HH")] != ':' {
		return 0, false, s
	}
	hourStr := s[:len("HH")]
	n, ok = tryParseDateUint64(hourStr)
	if !ok {
		return 0, false, s
	}
	hour := int(n)
	s = s[len("HH")+1:]

	// Parse minute
	if s[len("MM")] != ':' {
		return 0, false, s
	}
	minuteStr := s[:len("MM")]
	n, ok = tryParseDateUint64(minuteStr)
	if !ok {
		return 0, false, s
	}
	minute := int(n)
	s = s[len("MM")+1:]

	// Parse second
	secondStr := s[:len("SS")]
	n, ok = tryParseDateUint64(secondStr)
	if !ok {
		return 0, false, s
	}
	second := int(n)
	s = s[len("SS"):]

	secs := time.Date(year, month, day, hour, minute, second, 0, time.UTC).Unix()
	if secs < int64(-1<<63)/1e9 || secs >= int64((1<<63)-1)/1e9 {
		// Too big or too small timestamp
		return 0, false, s
	}
	return secs, true, s
}

// tryParseDateUint64 parses s (which is a part of some timestamp) as uint64 value.
func tryParseDateUint64(s string) (uint64, bool) {
	if len(s) == 0 || len(s) > 9 {
		return 0, false
	}

	if len(s) == 2 {
		// fast path for two-digit number, which is used in hours, minutes and seconds
		if s[0] < '0' || s[0] > '9' {
			return 0, false
		}
		n := 10*uint64(s[0]-'0') + uint64(s[1]-'0')
		return n, true
	}

	n := uint64(0)
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch < '0' || ch > '9' {
			return 0, false
		}
		if n > ((1<<64)-1)/10 {
			return 0, false
		}
		n *= 10
		d := uint64(ch - '0')
		if n > (1<<64)-1-d {
			return 0, false
		}
		n += d
	}
	return n, true
}

// GetLocalTimezoneOffsetNsecs returns local timezone offset in nanoseconds.
// It accounts for DST automatically.
func GetLocalTimezoneOffsetNsecs() int64 {
	_, offset := time.Now().Zone()
	return int64(offset) * 1e9
}

// ReplaceTemplateVariable substitutes Grafana's built-in template variables in
// a LogsQL expression with computed values.
//
// Grafana defines several special variables that users can include in queries:
//   - $__interval    → human-readable step duration (e.g. "15s", "5m")
//   - $__interval_ms → same interval as milliseconds integer string (e.g. "15000")
//   - $__range       → the full query time range as a LogsQL range literal (e.g. "[1704067200, 1704070800]")
//
// These variables are replaced at query time so that:
//   - $__interval aligns the query step with the panel's visual resolution
//   - $__range allows users to write stats queries that cover exactly the selected time window
func ReplaceTemplateVariable(expr string, interval int64, timeRange backend.TimeRange) string {
	expr = strings.ReplaceAll(expr, varRange, timeRangeToString(timeRange))
	expr = strings.ReplaceAll(expr, varIntervalMs, strconv.FormatInt(interval, 10))
	expr = strings.ReplaceAll(expr, varInterval, formatDuration(time.Duration(interval)*time.Millisecond))
	return expr
}

// formatDuration converts a duration to a VictoriaLogs/Prometheus-style string like "15s", "5m", "1h".
// The output is used as the $__interval replacement in LogsQL expressions.
// We use the largest unit that fits exactly to produce the most readable result.
func formatDuration(inter time.Duration) string {
	switch {
	case inter >= year:
		return fmt.Sprintf("%dy", inter/year)
	case inter >= day:
		return fmt.Sprintf("%dd", inter/day)
	case inter >= time.Hour:
		return fmt.Sprintf("%dh", inter/time.Hour)
	case inter >= time.Minute:
		return fmt.Sprintf("%dm", inter/time.Minute)
	case inter >= time.Second:
		return fmt.Sprintf("%ds", inter/time.Second)
	case inter >= time.Millisecond:
		return fmt.Sprintf("%dms", inter/time.Millisecond)
	default:
		return "1ms"
	}
}

// GetIntervalFrom returns the minimum interval to use for step calculations.
//
// Priority (highest to lowest):
//  1. queryInterval string (user-set per-query minimum interval), if not "0s"
//  2. queryIntervalMS (Grafana-calculated based on panel width and time range)
//  3. dsInterval string (datasource-level minimum interval setting)
//  4. defaultInterval (hardcoded fallback, currently 15s)
//
// The "0s" check is needed because Grafana historically set queryInterval to "0s"
// when the user leaves the field empty, rather than omitting it entirely.
//
// dsInterval is the string representation of data source min interval, if configured.
// queryInterval is the string representation of query interval (min interval), e.g. "10ms" or "10s".
// queryIntervalMS is a pre-calculated numeric representation of the query interval in milliseconds.
func GetIntervalFrom(dsInterval, queryInterval string, queryIntervalMS int64, defaultInterval time.Duration) (time.Duration, error) {
	// "0s" is treated as unset — Grafana sends this as the default empty value.
	interval := queryInterval
	if interval == "0s" {
		interval = ""
	}
	if interval == "" {
		if queryIntervalMS != 0 {
			return time.Duration(queryIntervalMS) * time.Millisecond, nil
		}
	}
	if interval == "" && dsInterval != "" {
		interval = dsInterval
	}
	if interval == "" {
		return defaultInterval, nil
	}

	parsedInterval, err := parseIntervalStringToTimeDuration(interval)
	if err != nil {
		return time.Duration(0), err
	}

	return parsedInterval, nil
}

// CalculateStep computes the optimal query step (data point interval) for a time range.
//
// The step is calculated so that the response contains approximately MaxDataPoints
// data points. This matches the panel's pixel width, giving one data point per pixel.
//
// The raw computed interval is then rounded to a "nice" human-readable value via
// roundInterval so users see steps like "1m" instead of "73s".
//
// The result is never smaller than minInterval, which prevents over-sampling
// (requesting more data points than the datasource or network can handle).
func CalculateStep(minInterval time.Duration, timeRange backend.TimeRange, maxDataPoints int64) time.Duration {
	resolution := maxDataPoints
	if resolution == 0 {
		resolution = defaultResolution
	}

	rangeValue := timeRange.To.UnixNano() - timeRange.From.UnixNano()

	calculatedInterval := time.Duration(rangeValue / resolution)

	if calculatedInterval < minInterval {
		return minInterval
	}

	return roundInterval(calculatedInterval)
}

// WithIntervalVariable returns true if expr is exactly the $__interval placeholder.
// When this is the case, the caller replaces it with an empty string so that
// GetIntervalFrom falls back to the computed intervalMs instead of trying to
// parse the literal "$__interval" string as a duration.
func WithIntervalVariable(expr string) bool {
	return expr == varInterval
}

// parseIntervalStringToTimeDuration converts an interval string to time.Duration.
//
// WHY strip < and >?
// Grafana's interval picker produces strings like "<5m" (meaning "at most 5m").
// We strip the angle brackets because gtime.ParseDuration doesn't understand them,
// but we still want to use the numeric value.
//
// WHY append "s" for pure numbers?
// A bare number like "15" is interpreted as 15 seconds. This matches Grafana's
// convention where a numeric-only interval string means seconds.
func parseIntervalStringToTimeDuration(interval string) (time.Duration, error) {
	// Strip leading "<" or ">" characters from Grafana's interval picker format.
	formattedInterval := strings.Replace(strings.Replace(interval, "<", "", 1), ">", "", 1)
	isPureNum, err := regexp.MatchString(`^\d+$`, formattedInterval)
	if err != nil {
		return time.Duration(0), err
	}
	if isPureNum {
		// Treat a bare number as seconds (e.g. "15" → "15s").
		formattedInterval += "s"
	}
	parsedInterval, err := gtime.ParseDuration(formattedInterval)
	if err != nil {
		return time.Duration(0), err
	}
	return parsedInterval, nil
}

// roundInterval snaps a raw calculated interval to a human-friendly "nice" value.
//
// WHY round intervals?
// CalculateStep divides the time range by MaxDataPoints, which yields arbitrary values
// like 73 seconds or 412 milliseconds. Displaying a graph with "73s" steps is confusing;
// users expect to see round numbers like "1m" or "30s".
//
// The breakpoints are chosen so that each bucket covers values up to the midpoint
// between two adjacent steps. For example:
//   - < 1.5s → snap to 1s
//   - 1.5s–3.5s → snap to 2s
//   - 3.5s–7.5s → snap to 5s
// This ensures consistent, predictable rounding regardless of input.
func roundInterval(interval time.Duration) time.Duration {
	switch {
	case interval <= 10*time.Millisecond:
		return time.Millisecond * 1 // 0.001s
	// 0.015s
	case interval < 15*time.Millisecond:
		return time.Millisecond * 10 // 0.01s
	// 0.035s
	case interval < 35*time.Millisecond:
		return time.Millisecond * 20 // 0.02s
	// 0.075s
	case interval < 75*time.Millisecond:
		return time.Millisecond * 50 // 0.05s
	// 0.15s
	case interval < 150*time.Millisecond:
		return time.Millisecond * 100 // 0.1s
	// 0.35s
	case interval < 350*time.Millisecond:
		return time.Millisecond * 200 // 0.2s
	// 0.75s
	case interval < 750*time.Millisecond:
		return time.Millisecond * 500 // 0.5s
	// 1.5s
	case interval < 1500*time.Millisecond:
		return time.Millisecond * 1000 // 1s
	// 3.5s
	case interval < 3500*time.Millisecond:
		return time.Millisecond * 2000 // 2s
	// 7.5s
	case interval < 7500*time.Millisecond:
		return time.Millisecond * 5000 // 5s
	// 12.5s
	case interval < 12500*time.Millisecond:
		return time.Millisecond * 10000 // 10s
	// 17.5s
	case interval < 17500*time.Millisecond:
		return time.Millisecond * 15000 // 15s
	// 25s
	case interval < 25000*time.Millisecond:
		return time.Millisecond * 20000 // 20s
	// 45s
	case interval < 45000*time.Millisecond:
		return time.Millisecond * 30000 // 30s
	// 1.5m
	case interval < 90000*time.Millisecond:
		return time.Millisecond * 60000 // 1m
	// 3.5m
	case interval < 210000*time.Millisecond:
		return time.Millisecond * 120000 // 2m
	// 7.5m
	case interval < 450000*time.Millisecond:
		return time.Millisecond * 300000 // 5m
	// 12.5m
	case interval < 750000*time.Millisecond:
		return time.Millisecond * 600000 // 10m
	// 17.5m
	case interval < 1050000*time.Millisecond:
		return time.Millisecond * 900000 // 15m
	// 25m
	case interval < 1500000*time.Millisecond:
		return time.Millisecond * 1200000 // 20m
	// 45m
	case interval < 2700000*time.Millisecond:
		return time.Millisecond * 1800000 // 30m
	// 1.5h
	case interval < 5400000*time.Millisecond:
		return time.Millisecond * 3600000 // 1h
	// 2.5h
	case interval < 9000000*time.Millisecond:
		return time.Millisecond * 7200000 // 2h
	// 4.5h
	case interval < 16200000*time.Millisecond:
		return time.Millisecond * 10800000 // 3h
	// 9h
	case interval < 32400000*time.Millisecond:
		return time.Millisecond * 21600000 // 6h
	// 24h
	case interval < 86400000*time.Millisecond:
		return time.Millisecond * 43200000 // 12h
	// 48h
	case interval < 172800000*time.Millisecond:
		return time.Millisecond * 86400000 // 24h
	// 1w
	case interval < 604800000*time.Millisecond:
		return time.Millisecond * 86400000 // 24h
	// 3w
	case interval < 1814400000*time.Millisecond:
		return time.Millisecond * 604800000 // 1w
	// 2y
	case interval < 3628800000*time.Millisecond:
		return time.Millisecond * 2592000000 // 30d
	default:
		return time.Millisecond * 31536000000 // 1y
	}
}

// optionRe matches a VictoriaLogs LogsQL options(...) clause so we can insert
// the time range filter in the correct position (after options, not at the start).
// Example expression: `count() by (app) options(skip_empty_values=true)`
var (
	optionRe = regexp.MustCompile(`(options\s*\(.*?\))(\s*|$)`)
)

// AddTimeFieldWithRange injects a _time:[start, end] filter into a LogsQL stats expression
// when one is not already present.
//
// WHY do we need this?
// The /stats_query endpoint uses a single "time" parameter (point-in-time) rather than
// "start"/"end". To restrict which logs are included in the aggregation, we embed the
// time range directly inside the LogsQL expression using the _time filter.
// Without this, a query like `count()` would aggregate ALL logs ever stored.
//
// WHY check for options() block?
// The LogsQL options(...) clause must appear at the end of the filter part (before pipes).
// We insert _time AFTER options() to preserve valid syntax:
//   BEFORE: count() by (app) options(skip_empty_values=true)
//   AFTER:  count() by (app) options(skip_empty_values=true) _time:[start, end]
// Without this handling, we'd prepend _time before options(), which is also valid but
// placing it after options() is more readable and follows VictoriaLogs conventions.
//
// If no options() block is present, we prepend _time at the beginning:
//   BEFORE: count() by (app)
//   AFTER:  _time:[start, end] count() by (app)
func AddTimeFieldWithRange(expr string, timeRange backend.TimeRange) string {
	if expr == "" {
		return expr
	}

	// Skip if the user already included _time in their expression.
	// We only look at the filter part (before any | pipe) to avoid false matches
	// in pipe arguments.
	if hasTimeField(expr) {
		return expr
	}

	timeRangeStr := timeRangeToString(timeRange)
	timeFieldWithRange := fmt.Sprintf("%s:%s", timeField, timeRangeStr)

	expr = strings.TrimSpace(expr)

	if optionRe.MatchString(expr) {
		// Insert _time after the options() clause to preserve valid syntax.
		return optionRe.ReplaceAllString(expr, fmt.Sprintf("$1 %s$2", timeFieldWithRange))
	}

	// No options block; prepend _time to the expression.
	return fmt.Sprintf("%s %s", timeFieldWithRange, expr)
}

// timeRangeToString formats a Grafana time range as a LogsQL range literal.
// The format is [unixSecStart, unixSecEnd], which VictoriaLogs uses in _time filters.
func timeRangeToString(timeRange backend.TimeRange) string {
	return fmt.Sprintf("[%s, %s]", strconv.FormatInt(timeRange.From.Unix(), 10), strconv.FormatInt(timeRange.To.Unix(), 10))
}

// hasTimeField reports whether a LogsQL expression already contains a _time filter.
// We only inspect the filter part of the expression (before the first | pipe character)
// because pipe arguments may legitimately contain the string "_time:" without being
// the time filter (e.g. in field rename operations or format strings).
func hasTimeField(expr string) bool {
	parts := strings.Split(expr, "|")
	if len(parts) > 1 {
		expr = parts[0]
	}

	return strings.Contains(expr, timeField+":")
}
