// fields_query.go — Metadata Query Parameter Handling
//
// This file defines FieldsQuery, the query parameters used for metadata
// API calls: field names, field values, streams, stream field names/values.
//
// These endpoints are called by the frontend's query editor for autocomplete:
//   - When the user types in the query box, the editor requests field names
//     to provide LogsQL syntax completion
//   - When building a visual filter, the editor requests field values for dropdowns
//   - The variable support layer uses field_values for template variable queries
//
// Unlike main queries (query.go), metadata queries are sent from the frontend via
// VLAPIQuery → datasource resource calls, not through the standard query data path.
// The parameters are POSTed as JSON and converted to URL query parameters here.

package plugin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
)

// FieldsQuery holds the parameters for VictoriaLogs metadata API requests.
// These are received from the frontend as a JSON body and forwarded to VictoriaLogs
// as URL query parameters. Only non-empty fields are included in the URL.
type FieldsQuery struct {
	// Query is the LogsQL expression to filter which log entries are considered
	// when enumerating fields or values. An empty query means "all logs".
	Query string `json:"query"`
	// Limit caps the number of returned values. Prevents huge dropdowns.
	Limit string `json:"limit"`
	// Start and End define the time range to search within (Unix timestamps or RFC3339).
	Start string `json:"start"`
	End   string `json:"end"`
	// Field is the specific field name to enumerate values for (used in field_values requests).
	// Example: "level" → returns ["error", "warn", "info"]
	Field string `json:"field"`
	// ExtraFilters is an additional LogsQL expression ANDed with Query.
	// This is how dashboard-level ad-hoc filters and template variables
	// propagate into metadata queries.
	ExtraFilters string `json:"extra_filters"`
	// ExtraStreamFilters is an additional stream selector ANDed with the stream part of Query.
	// Example: '{namespace="prod"}' to limit field enumeration to a specific namespace.
	ExtraStreamFilters string `json:"extra_stream_filters"`
}

// getFieldsQueryFromRaw parses the JSON body of a metadata resource request into FieldsQuery.
func getFieldsQueryFromRaw(data io.ReadCloser) (*FieldsQuery, error) {
	var q FieldsQuery
	if err := json.NewDecoder(data).Decode(&q); err != nil {
		return nil, fmt.Errorf("failed to parse query json: %s", err)
	}
	return &q, nil
}

// queryParams converts the FieldsQuery into URL query parameters for the VictoriaLogs API.
// Only non-empty fields are included to avoid overriding VictoriaLogs server defaults.
func (fv *FieldsQuery) queryParams() url.Values {
	params := url.Values{}
	if fv.Query != "" {
		params.Set("query", fv.Query)
	}
	if fv.Limit != "" {
		params.Set("limit", fv.Limit)
	}
	if fv.Start != "" {
		params.Set("start", fv.Start)
	}
	if fv.End != "" {
		params.Set("end", fv.End)
	}
	if fv.Field != "" {
		params.Set("field", fv.Field)
	}
	if fv.ExtraFilters != "" {
		params.Set("extra_filters", fv.ExtraFilters)
	}
	if fv.ExtraStreamFilters != "" {
		params.Set("extra_stream_filters", fv.ExtraStreamFilters)
	}
	return params
}
