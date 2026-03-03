# Backend Architecture — Developer Onboarding Guide

This document covers the Go backend of the VictoriaLogs Grafana datasource plugin. The backend handles HTTP communication with VictoriaLogs, query routing, response parsing, and the resource API.

**Prerequisites:** [System Overview](./onboarding-system-overview.md)

If you encounter unfamiliar terms, see the [Glossary](./glossary.md).

## Table of Contents

- [Overview](#overview)
- [Plugin Entry Point](#plugin-entry-point)
- [Grafana Plugin / Datasource Internals Context](#grafana-plugin--datasource-internals-context)
- [Datasource and Instance Management](#datasource-and-instance-management)
- [HTTP Route Registration](#http-route-registration)
- [Query Execution — QueryData](#query-execution--querydata)
- [Query URL Building](#query-url-building)
- [HTTP Request to VictoriaLogs](#http-request-to-victorialogs)
- [Response Parsing](#response-parsing)
- [Field Queries](#field-queries)
- [Multi-tenancy Header Handling](#multi-tenancy-header-handling)
- [Health Check](#health-check)
- [Streaming and Tail Support](#streaming-and-tail-support)
- [Utility Functions](#utility-functions)
- [Test Organization](#test-organization)
- [Key Design Patterns](#key-design-patterns)
- [See Also](#see-also)

## Overview

The backend is a Go binary that runs as a separate process managed by Grafana. It implements four Grafana plugin SDK interfaces:

| Interface | Handler | Purpose |
|-----------|---------|---------|
| `QueryDataHandler` | `Datasource` | Execute LogsQL queries |
| `CallResourceHandler` | HTTP mux (via `httpadapter`) | Resource API (field names, tenants, VMUI) |
| `CheckHealthHandler` | `Datasource` | Test connectivity to VictoriaLogs |
| `StreamHandler` | `Datasource` | Live log tail streaming |

**Key files:**

| File | Lines | Purpose |
|------|-------|---------|
| [pkg/main.go](../pkg/main.go) | 31 | Plugin entry point |
| [pkg/plugin/datasource.go](../pkg/plugin/datasource.go) | ~900 | Datasource, routing, HTTP, health, streaming |
| [pkg/plugin/query.go](../pkg/plugin/query.go) | ~370 | Query URL construction |
| [pkg/plugin/response.go](../pkg/plugin/response.go) | ~570 | Response parsing |
| [pkg/plugin/fields_query.go](../pkg/plugin/fields_query.go) | ~54 | Field name/value query handling |
| [pkg/utils/utils.go](../pkg/utils/utils.go) | ~660 | Time parsing, step calculation, template vars |
| [pkg/utils/stream_fields_parser.go](../pkg/utils/stream_fields_parser.go) | ~126 | Stream label parsing |

## Plugin Entry Point

**File:** [pkg/main.go](../pkg/main.go)

```go
const VL_PLUGIN_ID = "victoriametrics-logs-datasource"

func main() {
    backend.SetupPluginEnvironment(VL_PLUGIN_ID)
    ds := plugin.NewDatasource()
    err := backend.Manage(VL_PLUGIN_ID, backend.ServeOpts{
        CallResourceHandler: ds,
        QueryDataHandler:    ds,
        CheckHealthHandler:  ds,
        StreamHandler:       ds,
    })
}
```

`backend.Manage()` starts the gRPC server that Grafana communicates with. The single `Datasource` struct implements all four handler interfaces.

## Grafana Plugin / Datasource Internals Context

This section explains how Grafana core reaches this backend and how SDK protocol pieces fit together.

### End-to-end query path (frontend -> Grafana core -> plugin process)

For backend datasource plugins, the frontend usually extends `DataSourceWithBackend` (`grafana/packages/grafana-runtime/src/utils/DataSourceWithBackend.ts`):

1. Frontend `query()` sends `POST /api/ds/query?ds_type=<type>`.
2. Grafana API receives it in `grafana/pkg/api/ds_query.go` (`QueryMetricsV2`).
3. Query service parses/groups queries in `grafana/pkg/services/query/query.go`.
4. Plugin client dispatches through `grafana/pkg/plugins/manager/client/client.go`.
5. gRPC bridge calls plugin process via `grafana/pkg/plugins/backendplugin/grpcplugin/client_v2.go`.
6. SDK adapter invokes this plugin's `QueryData` handler.

For resource calls (`getResource`/`postResource` in frontend), Grafana routes through `/api/datasources/uid/:uid/resources/...` and reaches `CallResource`.

For health checks (`Save & Test`), Grafana calls `/api/datasources/uid/:uid/health` and reaches `CheckHealth`.

### Protocol services and handler mapping

The protocol contract lives in `grafana-plugin-sdk-go/proto/backend.proto`:

- `service Data` -> `QueryData`, `QueryChunkedData`
- `service Resource` -> `CallResource`
- `service Diagnostics` -> `CheckHealth`
- `service Stream` -> `SubscribeStream`, `RunStream`, `PublishStream`

`backend.Manage(..., backend.ServeOpts{...})` wires your Go handlers into those gRPC services through SDK adapters (`grafana-plugin-sdk-go/backend/serve.go`, `.../backend/grpcplugin/serve.go`).

### PluginContext and datasource settings propagation

Every gRPC request carries `PluginContext` (`backend.proto`) including:

- org/user info
- plugin ID/version
- datasource instance settings (`jsonData`, decrypted secure fields, UID, etc.)

The SDK converts protocol payloads to Go structs in `grafana-plugin-sdk-go/backend/convert_from_protobuf.go`, then your handler receives `backend.PluginContext`/`backend.DataSourceInstanceSettings`.

This is why `getInstance(ctx, req.PluginContext)` in this plugin can create/cache per-datasource instances correctly.

### Data frame transport (Go <-> frontend)

Backend responses are `backend.QueryDataResponse` (`grafana-plugin-sdk-go/backend/data.go`) with `map[refId]DataResponse`.

Frame encoding across process boundary:

- Go -> protocol: `convert_to_protobuf.go` (`QueryDataResponse`)
- protocol -> Go: `convert_from_protobuf.go`
- wire formats: Arrow or JSON (`DataFrameFormat`)
- Arrow encode/decode: `grafana-plugin-sdk-go/data/arrow.go`

Frontend decoding path:

- `toDataQueryResponse()` in `grafana/packages/grafana-runtime/src/utils/queryResponse.ts`
- frame model in `grafana/packages/grafana-data/src/types/dataFrame.ts`

### Streaming in core vs plugin

Streaming has two separate mechanisms in Grafana:

1. `DataSourceWithBackend` streaming via returned frames with `meta.channel`, then frontend switches to live subscription (`toStreamingDataResponse()`).
2. Explore live-tail flows that set `DataQueryRequest.liveStreaming` and let datasource/plugin-specific code manage stream behavior.

This plugin's backend stream handlers (`SubscribeStream`, `RunStream`, `PublishStream`) implement the protocol expected by Grafana Live and are backed by VictoriaLogs `/select/logsql/tail`.

## Datasource and Instance Management

**File:** [pkg/plugin/datasource.go](../pkg/plugin/datasource.go#L45)

### Datasource (singleton)

```go
type Datasource struct {
    im     instancemgmt.InstanceManager
    logger log.Logger
    backend.CallResourceHandler
}
```

Created once via `NewDatasource()` ([line 52](../pkg/plugin/datasource.go#L52)). Manages the `InstanceManager` which creates/caches per-datasource-configuration instances.

### DatasourceInstance (per configuration)

```go
type DatasourceInstance struct {
    settings            DataSourceInstanceSettings
    httpClient          *http.Client      // Standard requests
    httpStreamingClient *http.Client      // Streaming (no timeout)
    grafanaSettings     *GrafanaSettings  // HTTP method, query params, headers
    liveModeResponses   sync.Map          // Channel map for live streaming
}
```

Created by `newDatasourceInstance()` ([line 71](../pkg/plugin/datasource.go#L71)) when a datasource configuration is loaded. Key initialization:

1. Creates two HTTP clients — one standard, one with zero timeout for streaming
2. Parses datasource URL and VMUI URL
3. Parses custom headers and multi-tenancy headers
4. Merges tenant headers into the common header set

When settings change, Grafana calls `Dispose()` ([line 271](../pkg/plugin/datasource.go#L271)) on the old instance, which closes HTTP connections and streaming channels.

### GrafanaSettings

```go
type GrafanaSettings struct {
    HTTPMethod          string              // GET or POST
    QueryParams         string              // Custom URL parameters
    CustomHeaders       http.Header         // All custom headers (including tenant)
    MultitenancyHeaders MultitenancyHeaders // AccountID, ProjectID
}
```

Parsed from `DataSourceInstanceSettings.JSONData` in `NewGrafanaSettings()` ([line 137](../pkg/plugin/datasource.go#L137)).

## HTTP Route Registration

**File:** [pkg/plugin/datasource.go](../pkg/plugin/datasource.go#L57)

The resource API uses a standard `http.ServeMux`:

```go
mux := http.NewServeMux()
mux.HandleFunc("/", ds.RootHandler)
mux.HandleFunc("/select/logsql/field_values", ds.VLAPIQuery)
mux.HandleFunc("/select/logsql/field_names", ds.VLAPIQuery)
mux.HandleFunc("/select/logsql/streams", ds.VLAPIQuery)
mux.HandleFunc("/select/logsql/stream_field_names", ds.VLAPIQuery)
mux.HandleFunc("/select/logsql/stream_field_values", ds.VLAPIQuery)
mux.HandleFunc("/select/tenant_ids", ds.VLAPITenantIDs)
mux.HandleFunc("/vmui", ds.VMUIQuery)
```

These endpoints are called by the frontend for autocomplete, variable queries, tenant selection, and VMUI links. They are **not** used for main query execution (that goes through `QueryData`).

| Route | Handler | Purpose |
|-------|---------|---------|
| `/select/logsql/field_values` | `VLAPIQuery` | Fetch values for a specific field |
| `/select/logsql/field_names` | `VLAPIQuery` | Fetch all field names |
| `/select/logsql/streams` | `VLAPIQuery` | Fetch stream labels |
| `/select/logsql/stream_field_names` | `VLAPIQuery` | Fetch field names within streams |
| `/select/logsql/stream_field_values` | `VLAPIQuery` | Fetch field values within streams |
| `/select/tenant_ids` | `VLAPITenantIDs` | List available tenants |
| `/vmui` | `VMUIQuery` | Return VMUI URL and tenant info |

## Query Execution — QueryData

**File:** [pkg/plugin/datasource.go](../pkg/plugin/datasource.go#L289)

`QueryData()` is the main query handler. It processes all queries in parallel:

```go
func (d *Datasource) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
    // 1. Get datasource instance
    di, err := d.getInstance(ctx, req.PluginContext)

    // 2. Check if request is from alerting
    forAlerting, err := checkAlertingRequest(headers)

    // 3. Process queries in parallel
    var wg sync.WaitGroup
    var mu sync.Mutex
    for _, q := range req.Queries {
        rawQuery, err := getQueryFromRaw(q.JSON, forAlerting)
        wg.Add(1)
        go func(rawQuery *Query) {
            defer wg.Done()
            resp := di.query(ctx, rawQuery)
            mu.Lock()
            response.Responses[rawQuery.RefID] = resp
            mu.Unlock()
        }(rawQuery)
    }
    wg.Wait()
    return response, nil
}
```

Each query is routed by `query()` ([line 438](../pkg/plugin/datasource.go#L438)):

```go
func (di *DatasourceInstance) query(ctx context.Context, q *Query) backend.DataResponse {
    r, err := di.datasourceQuery(ctx, q, false)
    switch q.QueryType {
    case QueryTypeStats, QueryTypeStatsRange:
        return parseStatsResponse(r, q)
    case QueryTypeHits:
        return parseHitsResponse(r)
    default:
        return parseInstantResponse(r)
    }
}
```

## Query URL Building

**File:** [pkg/plugin/query.go](../pkg/plugin/query.go)

### Query Struct

```go
type Query struct {
    backend.DataQuery
    Expr           string    `json:"expr"`           // LogsQL expression
    LegendFormat   string    `json:"legendFormat"`    // Display name template
    MaxLines       int       `json:"maxLines"`        // Result limit (default 1000)
    Step           string    `json:"step"`            // User-defined step
    Fields         []string  `json:"fields"`          // Fields for hits query
    QueryType      QueryType `json:"queryType"`       // instant/stats/statsRange/hits
    ExtraFilters   string    `json:"extraFilters"`    // Ad-hoc filters
    TimezoneOffset string    `json:"timezoneOffset"`  // Bucket alignment offset
    ForAlerting    bool      `json:"-"`               // Set from request headers
}
```

### URL Building by QueryType

`getQueryURL()` ([line 66](../pkg/plugin/query.go#L66)) switches on `QueryType`:

#### Instant — `queryInstantURL()` ([line 138](../pkg/plugin/query.go#L138))
- Endpoint: `/select/logsql/query`
- Parameters: `query`, `limit` (maxLines), `start`, `end`
- Default time range: last 5 minutes if unset

#### Stats — `statsQueryURL()` ([line 171](../pkg/plugin/query.go#L171))
- Endpoint: `/select/logsql/stats_query`
- Parameters: `query`, `time` (point-in-time timestamp)
- Adds `_time` field with range to expression via `utils.AddTimeFieldWithRange()`

#### Stats Range — `statsQueryRangeURL()` ([line 197](../pkg/plugin/query.go#L197))
- Endpoint: `/select/logsql/stats_query_range`
- Parameters: `query`, `start`, `end`, `step`, `offset` (timezone)
- Step: user-defined or calculated via `utils.CalculateStep()`

#### Hits — `hitsQueryURL()` ([line 239](../pkg/plugin/query.go#L239))
- Endpoint: `/select/logsql/hits`
- Parameters: `query`, `start`, `end`, `step`, `offset`, `field` (repeated for each)
- Similar to stats range but adds `field` parameters

### Common Processing

All URL builders:
1. Merge custom query parameters from datasource settings
2. Add `extra_filters` parameter if ad-hoc filters are present
3. Replace template variables (`$__interval`, `$__interval_ms`, `$__range`) via `utils.ReplaceTemplateVariable()`
4. Default to 5-minute time range if not specified

## HTTP Request to VictoriaLogs

**File:** [pkg/plugin/datasource.go](../pkg/plugin/datasource.go#L369)

`datasourceQuery()` sends the request:

1. Builds URL via `getQueryURL()` (or `queryTailURL()` for streaming)
2. Creates HTTP request using configured method (GET or POST)
3. Attaches custom headers (including multi-tenancy)
4. Sends via `httpClient` (or `httpStreamingClient` for tail)
5. **Retry on transient errors**: if the error is "trivial" (EOF, broken pipe, connection reset), retries once
6. Checks response status code — returns error details from VictoriaLogs on non-2xx

```go
// Trivial error detection for retry logic
func isTrivialError(err error) bool {
    // Checks for: io.EOF, "broken pipe", "connection reset by peer"
}
```

## Response Parsing

**File:** [pkg/plugin/response.go](../pkg/plugin/response.go)

### Instant Response — NDJSON

`parseInstantResponse()` ([line 39](../pkg/plugin/response.go#L39)):

- Reads response body line by line using `bufio.ReaderSize` (64KB buffer)
- Parses each line with `fastjson.Parser`
- Extracts VictoriaLogs fields: `_msg` → Line, `_time` → Time, `_stream` → labels
- Other fields are added as labels (JSON object)
- Creates a single frame with three fields: `Time`, `Line`, `labels`
- **Lines exceeding 64KB are skipped** (logged as debug)

### Stats Response — JSON

`parseStatsResponse()` ([line 268](../pkg/plugin/response.go#L268)):

- Decodes full JSON response
- Routes by `ResultType`:
  - **vector**: point-in-time values → one frame per series (`Time + Value`)
  - **matrix**: time series → one frame per series (`Time[] + Value[]`)
- Applies legend formatting via `parseLegend()`
- For alerting requests, uses special `FrameTypeNumericMulti` format

### Hits Response — JSON

`parseHitsResponse()` ([line 290](../pkg/plugin/response.go#L290)):

- Decodes JSON array of hits
- Creates time series frames with timestamps and float64 values
- Labels derived from hit field names

### Legend Formatting

`parseLegend()` ([query.go:279](../pkg/plugin/query.go#L279)):
- `__auto` → uses the expression as the display name
- `{{label}}` template syntax → replaces with label values
- Falls back to labels string representation

## Field Queries

**File:** [pkg/plugin/fields_query.go](../pkg/plugin/fields_query.go)

Handles frontend requests for field names and values (used for autocomplete and variable queries):

1. Parses request body into `FieldsQuery` struct
2. Converts to URL query parameters: `query`, `limit`, `start`, `end`, `field`, `extra_filters`, `extra_stream_filters`
3. Proxies to the corresponding VictoriaLogs endpoint
4. Returns raw response (the frontend handles the JSON)

## Multi-tenancy Header Handling

**File:** [pkg/plugin/datasource.go](../pkg/plugin/datasource.go#L123)

Multi-tenancy is implemented via HTTP headers:

```go
type MultitenancyHeaders struct {
    AccountID string `json:"AccountID"`
    ProjectID string `json:"ProjectID"`
}
```

- Parsed from datasource JSON settings during `NewGrafanaSettings()` ([line 137](../pkg/plugin/datasource.go#L137))
- Merged into the common `CustomHeaders` set, so every request to VictoriaLogs includes them
- For tenant ID lookups (`VLAPITenantIDs`), tenant headers are **removed** from the request to avoid scoping the tenant list query

## Health Check

**File:** [pkg/plugin/datasource.go](../pkg/plugin/datasource.go#L488)

`CheckHealth()` sends a GET request to `<datasource-url>/health`:
- Returns `HealthStatusOk` if VictoriaLogs responds with HTTP 200
- Returns `HealthStatusError` with details otherwise
- Used by the "Save & Test" button in datasource settings

## Streaming and Tail Support

Live log streaming uses Grafana's streaming infrastructure:

### Flow

1. **`SubscribeStream()`** ([line 190](../pkg/plugin/datasource.go#L190)) — called when a user subscribes
   - Creates a channel (`chan *data.Frame`) and stores it in `liveModeResponses` sync.Map
   - Returns `SubscribeStreamStatusOK`

2. **`RunStream()`** ([line 224](../pkg/plugin/datasource.go#L224)) — called for the first subscriber
   - Starts `streamQuery()` which sends request to `/select/logsql/tail`
   - Reads channel and sends frames to the browser via `StreamSender`
   - Optimizes repeated schema transmissions using `FrameJSONCache`

3. **`streamQuery()`** ([line 327](../pkg/plugin/datasource.go#L327))
   - Uses `httpStreamingClient` (no timeout) for long-lived connection
   - Calls `parseStreamResponse()` which pushes frames to the channel

4. **`Dispose()`** ([line 271](../pkg/plugin/datasource.go#L271)) — cleanup
   - Closes all streaming channels
   - Closes HTTP connections

## Utility Functions

**File:** [pkg/utils/utils.go](../pkg/utils/utils.go)

| Function | Purpose |
|----------|---------|
| `ReplaceTemplateVariable()` | Replace `$__interval`, `$__interval_ms`, `$__range` in expressions |
| `CalculateStep()` | Calculate step from MaxDataPoints and TimeRange |
| `AddTimeFieldWithRange()` | Add `_time:[from,to]` to query if missing |
| `GetTime()` / `ParseTime()` | Parse timestamps in multiple formats |
| `ParseDuration()` | Parse Prometheus-style durations |

**File:** [pkg/utils/stream_fields_parser.go](../pkg/utils/stream_fields_parser.go)

`ParseStreamFields()` — parses `_stream` JSON labels (e.g., `{"host":"server1","app":"web"}`) into `[]StreamField{Label, Value}`. Handles quoted values with escape sequences.

## Test Organization

Tests live next to source files as `*_test.go`:

| Test File | Key Tests |
|-----------|-----------|
| `pkg/plugin/query_test.go` (~555 lines) | 37+ test cases for URL building across all query types |
| `pkg/plugin/datasource_test.go` | QueryData execution, HTTP mock testing |
| `pkg/plugin/response_test.go` | Response parsing for all formats |
| `pkg/utils/utils_test.go` | Time parsing, step calculation, variable replacement |
| `pkg/utils/stream_fields_parser_test.go` | Stream label parsing |
| `pkg/utils/pointer_test.go` | Generic pointer helper |

Run tests:
```bash
make golang-test        # go test ./pkg/...
make golang-test-race   # go test -race ./pkg/...
```

## Key Design Patterns

1. **Instance manager pattern.** The `Datasource` singleton manages `DatasourceInstance` objects via Grafana's `InstanceManager`. Each datasource configuration gets its own instance with separate HTTP clients and settings. When settings change, the old instance is disposed and a new one created.

2. **Parallel query execution with WaitGroup.** Multiple queries in a single request are processed concurrently. A `sync.Mutex` protects the response map.

3. **Two HTTP clients.** Standard requests use an HTTP client with default timeouts. Streaming (tail) requests use a client with zero timeout for long-lived connections.

4. **Retry on transient errors.** Network errors (EOF, broken pipe, connection reset) trigger a single retry. This handles transient connection issues from proxies or load balancers without masking persistent failures.

5. **NDJSON streaming parser.** Instant query responses are parsed line-by-line using `bufio.Reader` and `fastjson`, avoiding loading the entire response into memory. Lines exceeding 64KB are skipped gracefully.

6. **Header-based multi-tenancy.** Tenant isolation is achieved by attaching `AccountID`/`ProjectID` headers to every request. Headers are merged into the common set at instance creation time so they don't need to be added per-request.

## See Also

- [System Overview](./onboarding-system-overview.md) — high-level architecture
- [Query Flow](./onboarding-query-flow.md) — end-to-end query walkthrough
- [Frontend Architecture](./onboarding-frontend.md) — TypeScript components
- [Developer Setup](./onboarding-dev-setup.md) — building and testing
