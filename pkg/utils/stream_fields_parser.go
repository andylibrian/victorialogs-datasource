// stream_fields_parser.go — VictoriaLogs _stream Field Parser
//
// VictoriaLogs attaches structured metadata to log entries via the _stream field.
// The _stream field uses a Prometheus-like label set syntax:
//
//   {app="nginx", namespace="prod", instance="10.0.0.1:9090"}
//
// This syntax looks similar to Prometheus label selectors, but VictoriaLogs can have
// values that contain commas (e.g. JSON blobs or multi-value strings). Standard
// string.Split(",") would break on such values, so we use a custom parser that
// respects quoted boundaries.
//
// The parser produces a []StreamField slice where each element is one label=value pair.
// The result is used in response.go to populate the Grafana labels map, which enables
// filtering and grouping in the Logs panel.

package utils

import (
	"fmt"
	"strconv"
	"strings"
)

// StreamField is a single label=value pair extracted from a VictoriaLogs _stream field.
// Label is the key (e.g. "app") and Value is the unquoted string (e.g. "nginx").
type StreamField struct {
	Label string
	Value string
}

// ParseStreamFields parses the VictoriaLogs _stream field into a slice of StreamField pairs.
//
// The _stream field format is: {label1="value1", label2="value2", ...}
// where:
//   - The entire string is wrapped in { }
//   - Each label name is an unquoted identifier
//   - Each value is a double-quoted string (Go strconv.Unquote syntax)
//   - Commas separate label=value pairs
//   - Values may contain commas, so we cannot split naively
//
// Returns nil (not an error) for an empty _stream field, which means
// the log entry has no structured stream labels.
func ParseStreamFields(streamFields string) ([]StreamField, error) {
	if streamFields == "" {
		return nil, nil
	}

	if !strings.HasPrefix(streamFields, "{") {
		return nil, fmt.Errorf("_stream field must start with '{'")
	}
	if !strings.HasSuffix(streamFields, "}") {
		return nil, fmt.Errorf("_stream field must end with '}'")
	}

	streams := streamFields[1 : len(streamFields)-1]
	if len(streams) == 0 {
		return []StreamField(nil), nil
	}

	labelValuesPairs := splitStreamsToFields(streams)
	stf := make([]StreamField, 0, len(labelValuesPairs))
	for _, labelValuePair := range labelValuesPairs {
		labelValuePair = strings.TrimSpace(labelValuePair)
		if labelValuePair[0] == '"' || labelValuePair[0] == '`' {
			return nil, fmt.Errorf("_stream label can not start with quote: %q", labelValuePair)
		}
		fields := strings.SplitN(labelValuePair, "=", 2)
		if len(fields) != 2 {
			return nil, fmt.Errorf("_stream field %q must have `label=\"value\"` format", labelValuePair)
		}

		label := strings.TrimSpace(fields[0])
		if len(label) == 0 {
			return nil, fmt.Errorf("_stream field %q must have non-empty key", labelValuePair)
		}

		value := strings.TrimSpace(fields[1])
		if !strings.HasSuffix(value, `"`) && !strings.HasPrefix(value, `"`) {
			return nil, fmt.Errorf("_stream field %q must have quoted value", labelValuePair)
		}

		// Remove only the enclosing quotes, preserving any internal quotes
		unqValue, err := strconv.Unquote(value)
		if err != nil {
			return nil, fmt.Errorf("_stream field %q has invalid quoted value: %v", labelValuePair, err)
		}
		if len(unqValue) == 0 {
			return nil, fmt.Errorf("_stream field %q must have non-empty value", labelValuePair)
		}
		stf = append(stf, StreamField{
			Label: label,
			Value: unqValue,
		})
	}

	return stf, nil
}

// splitStreamsToFields splits a raw stream label string (the inner content between { })
// into individual "label=\"value\"" tokens, respecting quoted values.
//
// The challenge: values may contain commas, so a naive strings.Split(",") would break
// on entries like: app="service,backend", env="prod"
// We need to split only on commas that appear AFTER a closing quote.
//
// State machine overview:
//   - inQuotes: we are inside a "..." value; commas here are part of the value
//   - expectingComma: we just closed a quote; next non-space char should be ','
//   - escaping: previous char was '\'; the next '"' should not toggle inQuotes
//
// Example walkthrough for: app="nginx",env="prod"
//   - a,p,p,=   → written to currentField (not in quotes, not after a quote)
//   - "         → inQuotes=true
//   - n,g,i,n,x → written to currentField (inQuotes, comma would be skipped)
//   - "         → inQuotes=false, expectingComma=true
//   - ,         → char==comma && expectingComma → flush field, reset
//   - e,n,v,=   → written to next currentField
//   - ...
func splitStreamsToFields(streamFields string) []string {
	var fields []string
	var currentField strings.Builder
	var inQuotes bool
	var escaping bool
	// expectingComma is set after a closing quote to enforce that only commas
	// immediately following a quoted value act as field delimiters.
	var expectingComma bool

	for i := 0; i < len(streamFields); i++ {
		char := streamFields[i]

		if char == '"' {
			if !escaping {
				inQuotes = !inQuotes
			}
			currentField.WriteByte(char)

			// After closing a quote we expect the next meaningful character to be
			// a comma (field separator). This prevents accidentally splitting on
			// commas that appear inside a quoted value.
			if !inQuotes {
				expectingComma = true
			}
			escaping = false
		} else if char == ',' && expectingComma {
			// Comma immediately after a closing quote → field boundary.
			field := strings.TrimSpace(currentField.String())
			fields = append(fields, field)
			currentField.Reset()
			expectingComma = false
			escaping = false
		} else {
			currentField.WriteByte(char)

			// Any non-space character after a closing quote that is not a comma
			// means we're still inside the same field (e.g. trailing whitespace
			// or malformed input). Reset the expectation so we don't lose data.
			if expectingComma && char != ' ' {
				expectingComma = false
			}
			// Track escape sequences so that \" inside a value doesn't close the quote.
			escaping = char == '\\' && !escaping
		}
	}

	// Flush the last field (there is no trailing comma after the final entry).
	if currentField.Len() > 0 {
		field := strings.TrimSpace(currentField.String())
		fields = append(fields, field)
	}

	return fields
}
