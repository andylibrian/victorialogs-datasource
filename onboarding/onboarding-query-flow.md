# Query Flow — Developer Onboarding Guide

This document traces the end-to-end path of a query from the Grafana UI through the plugin to VictoriaLogs and back. Understanding this flow is essential for debugging query issues and adding new query types.

**Prerequisites:** [System Overview](./onboarding-system-overview.md)

If you encounter unfamiliar terms, see the [Glossary](./glossary.md).

## Table of Contents

- [Query Types and Endpoints](#query-types-and-endpoints)
- [Complete Data Flow](#complete-data-flow)
- [Step 1: Frontend Query Preparation](#step-1-frontend-query-preparation)
- [Step 2: Template Variable Interpolation](#step-2-template-variable-interpolation)
- [Step 3: Backend Query Routing](#step-3-backend-query-routing)
- [Step 4: URL Construction](#step-4-url-construction)
- [Step 5: HTTP Request to VictoriaLogs](#step-5-http-request-to-victorialogs)
- [Step 6: Response Parsing](#step-6-response-parsing)
- [Step 7: Frontend Response Transformation](#step-7-frontend-response-transformation)
- [Step Calculation](#step-calculation)
- [Supplementary Queries](#supplementary-queries)
- [Log Context Queries](#log-context-queries)
- [Live Streaming](#live-streaming)
- [Key Design Patterns](#key-design-patterns)
- [See Also](#see-also)

## Query Types and Endpoints

| QueryType | Frontend Enum | Backend Const | VictoriaLogs Endpoint | Response Format | Use Case |
|-----------|---------------|---------------|----------------------|-----------------|----------|
| `instant` | `QueryType.Instant` | `QueryTypeInstant` | `/select/logsql/query` | NDJSON (line-by-line) | Raw log records |
| `stats` | `QueryType.Stats` | `QueryTypeStats` | `/select/logsql/stats_query` | JSON (vector/matrix) | Point-in-time aggregation |
| `statsRange` | `QueryType.StatsRange` | `QueryTypeStatsRange` | `/select/logsql/stats_query_range` | JSON (matrix) | Time series aggregation |
| `hits` | `QueryType.Hits` | `QueryTypeHits` | `/select/logsql/hits` | JSON (hits array) | Hit counts by field over time |
| (streaming) | — | — | `/select/logsql/tail` | NDJSON (streaming) | Live log tail |

**Defined in:**
- Frontend: [src/types.ts:35](../src/types.ts#L35)
- Backend: [pkg/plugin/query.go:34](../pkg/plugin/query.go#L34)

## Complete Data Flow

```
┌─────────────────────────────────────────────────────────────────────────┐
│ FRONTEND                                                                │
│                                                                         │
│  QueryEditor                                                            │
│  User writes LogsQL expr, selects QueryType                             │
│       │                                                                 │
│       ▼                                                                 │
│  datasource.ts:query() [line 124]                                       │
│  ├── addSortPipeToQuery()      — append | sort by (_time) for instant   │
│  ├── formatOffsetDuration()    — calculate timezone offset              │
│  ├── getQueryFormat()          — detect histogram format                │
│  └── templateSrv.replace()     — replace $step variable                 │
│       │                                                                 │
│       ▼                                                                 │
│  datasource.ts:applyTemplateVariables() [line 201]                      │
│  ├── interpolateString()       — variable interpolation chain           │
│  └── getExtraFilters()         — ad-hoc filters → extra_filters         │
│       │                                                                 │
│       ▼                                                                 │
│  DataSourceWithBackend.query() — sends to Go backend via plugin protocol│
└────────┬────────────────────────────────────────────────────────────────┘
         │
┌────────▼────────────────────────────────────────────────────────────────┐
│ BACKEND                                                                 │
│                                                                         │
│  datasource.go:QueryData() [line 289]                                   │
│  ├── For each query (parallel via WaitGroup):                           │
│  │   ├── getQueryFromRaw()     — JSON → Query struct                    │
│  │   └── query() [line 438]                                             │
│  │       ├── getQueryURL() [line 66]                                    │
│  │       │   ├── instant  → queryInstantURL()  [line 138]               │
│  │       │   ├── stats    → statsQueryURL()    [line 171]               │
│  │       │   ├── statsRange → statsQueryRangeURL() [line 197]           │
│  │       │   └── hits     → hitsQueryURL()     [line 239]               │
│  │       │                                                              │
│  │       ├── datasourceQuery() [line 369]                               │
│  │       │   └── HTTP GET/POST → VictoriaLogs                           │
│  │       │                                                              │
│  │       └── Parse response:                                            │
│  │           ├── instant  → parseInstantResponse()  [response.go:39]    │
│  │           ├── stats    → parseStatsResponse()    [response.go:268]   │
│  │           └── hits     → parseHitsResponse()     [response.go:290]   │
│  │                                                                      │
│  └── Returns backend.QueryDataResponse (map of RefID → DataFrames)      │
└────────┬────────────────────────────────────────────────────────────────┘
         │
┌────────▼────────────────────────────────────────────────────────────────┐
│ FRONTEND (response)                                                     │
│                                                                         │
│  datasource.ts:runQuery() [line 151]                                    │
│  └── transformBackendResult() [transformBackendResult.ts:15]            │
│      ├── Group frames by type (streams, metric, histogram)              │
│      ├── processStreamsFrames()                                          │
│      │   ├── addLevelField()     — log level detection                  │
│      │   ├── getDerivedFields()  — regex/label extraction               │
│      │   └── getStreamFields()   — label formatting                     │
│      ├── processMetricRangeFrames()                                     │
│      ├── processMetricInstantFrames()                                   │
│      └── processHistogramFrames()                                       │
│       │                                                                 │
│       ▼                                                                 │
│  Grafana renders logs panel / chart / table                             │
└─────────────────────────────────────────────────────────────────────────┘
```

## Step 1: Frontend Query Preparation

**File:** [src/datasource.ts](../src/datasource.ts#L124)

When Grafana triggers a query, `VictoriaLogsDatasource.query()` prepares each target:

```typescript
query(request: DataQueryRequest<Query>): Observable<DataQueryResponse> {
  const timezoneOffset = formatOffsetDuration(request.timezone, request.range.from.utcOffset());
  const queries = request.targets
    .filter((q) => q.expr || config.publicDashboardAccessToken !== '')
    .map((q) => ({
      ...q,
      expr: addSortPipeToQuery(q, request.app, request.liveStreaming),
      maxLines: q.maxLines ?? this.maxLines,
      timezoneOffset,
      format: getQueryFormat(q.expr),
      step: this.templateSrv.replace(q.step, request.scopedVars),
    }));
```

**What happens here:**

1. **Filter empty queries** — skips queries with no expression (unless public dashboard)
2. **Add sort pipe** — appends `| sort by (_time) desc` for instant queries in Explore, respects panel direction in Dashboards. Skipped for stats/streaming queries. See [modifyQuery.ts:addSortPipeToQuery()](../src/modifyQuery.ts#L121).
3. **Set maxLines** — falls back to datasource default (1000)
4. **Calculate timezone offset** — for correct bucket alignment in stats_query_range and hits
5. **Detect format** — checks if query produces histogram data via `getQueryFormat()`
6. **Replace step variable** — interpolates `$step` if user specified a custom step

## Step 2: Template Variable Interpolation

**File:** [src/datasource.ts](../src/datasource.ts#L201)

Grafana calls `applyTemplateVariables()` before sending the query to the backend:

```typescript
applyTemplateVariables(target: Query, scopedVars: ScopedVars, adhocFilters?: AdHocVariableFilter[]): Query {
  let expr = this.interpolateString(target.expr, variables);
  // ...
  return { ...target, expr, extraFilters };
}
```

The interpolation chain in `interpolateString()` ([datasource.ts](../src/datasource.ts)):

1. **`replaceOperatorsToInForMultiQueryVariables()`** — converts `field:$var` to `field:in($var)` for multi-value variables
2. **`doubleQuoteRegExp()`** — wraps regex variable values in double quotes
3. **`templateSrv.replace()`** — Grafana's built-in variable replacement, using `interpolateQueryExpr()` as custom formatter
4. **`correctRegExpValueAll()`** — fixes `$__all` values for regex operators
5. **`correctMultiExactOperatorValueAll()`** — fixes `$__all` for multi-exact operators
6. **`replaceMultiVariables()`** — expands `$_StartMultiVariable_..._EndMultiVariable` markers into proper LogsQL syntax

**Multi-value variable handling** ([datasource.ts:241](../src/datasource.ts#L241)):

The custom formatter `interpolateQueryExpr()` wraps multi-value arrays in a special marker format: `$_StartMultiVariable_val1_separator_val2_EndMultiVariable`. This is later expanded by `replaceMultiVariables()` into context-appropriate syntax:
- Regex operator (`~`): `(val1|val2)`
- `in()` operator: `val1,val2`
- Otherwise: `val1 OR val2`

## Step 3: Backend Query Routing

**File:** [pkg/plugin/datasource.go](../pkg/plugin/datasource.go#L289)

`QueryData()` processes all queries in parallel:

```go
func (d *Datasource) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
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

Each query is:
1. Deserialized from JSON into a `Query` struct ([query.go:46](../pkg/plugin/query.go#L46))
2. Routed by `QueryType` to the appropriate URL builder and response parser

## Step 4: URL Construction

**File:** [pkg/plugin/query.go](../pkg/plugin/query.go#L66)

`getQueryURL()` switches on `QueryType`:

| QueryType | Method | Endpoint | Key Parameters |
|-----------|--------|----------|----------------|
| `instant` | `queryInstantURL()` | `/select/logsql/query` | `query`, `limit`, `start`, `end` |
| `stats` | `statsQueryURL()` | `/select/logsql/stats_query` | `query`, `time` |
| `statsRange` | `statsQueryRangeURL()` | `/select/logsql/stats_query_range` | `query`, `start`, `end`, `step`, `offset` |
| `hits` | `hitsQueryURL()` | `/select/logsql/hits` | `query`, `start`, `end`, `step`, `offset`, `field` (repeated) |

**Common processing in each builder:**
1. Merge custom query parameters from datasource settings
2. Apply `extra_filters` if present
3. Replace template variables (`$__interval`, `$__interval_ms`, `$__range`) via `utils.ReplaceTemplateVariable()`
4. Set time range from `TimeRange.From` / `TimeRange.To` (defaults to last 5 minutes if unset)

## Step 5: HTTP Request to VictoriaLogs

**File:** [pkg/plugin/datasource.go](../pkg/plugin/datasource.go#L369)

`datasourceQuery()` makes the HTTP request:

1. Builds the URL via `getQueryURL()` or `queryTailURL()` (for streaming)
2. Creates an HTTP request with the configured method (GET or POST)
3. Adds custom headers and multi-tenancy headers (`AccountID`, `ProjectID`)
4. Sends the request using the instance's `httpClient`
5. **Retry logic:** retries once on transient errors (EOF, broken pipe, connection reset)
6. Returns the response body as `io.ReadCloser`

## Step 6: Response Parsing

**File:** [pkg/plugin/response.go](../pkg/plugin/response.go)

### Instant Response (NDJSON)

`parseInstantResponse()` ([response.go:39](../pkg/plugin/response.go#L39)) reads NDJSON line by line:

- Each line is a JSON object with `_msg`, `_time`, `_stream`, and arbitrary fields
- `_stream` is parsed as a JSON object of labels
- Creates a Grafana frame with three fields: `Time`, `Line` (message), `labels` (JSON-encoded labels)
- Skips lines exceeding 64KB

### Stats Response (JSON)

`parseStatsResponse()` ([response.go:268](../pkg/plugin/response.go#L268)):

- Decodes JSON with `ResultType` (vector or matrix) and `Result` array
- **Vector:** single value per series → one frame per series
- **Matrix:** array of `[timestamp, value]` pairs → time series frame per series
- Applies legend formatting and interval metadata

### Hits Response (JSON)

`parseHitsResponse()` ([response.go:290](../pkg/plugin/response.go#L290)):

- Decodes JSON array of hits with timestamps, values, and field labels
- Creates time series frames grouped by field values

## Step 7: Frontend Response Transformation

**File:** [src/transformers/transformBackendResult.ts](../src/transformers/transformBackendResult.ts#L15)

`transformBackendResult()` enriches the backend's raw frames:

1. **Group frames** by type: streams (logs), metric instant, metric range, histogram
2. **Process stream frames** (`processStreamsFrames()`):
   - Add `level` field based on configured [log level rules](./glossary.md#log-level-rules)
   - Extract [derived fields](./glossary.md#derived-field) via regex or label matching
   - Format label fields for dashboard vs explore views
3. **Process metric frames** — pass through with metadata
4. **Process histogram frames** — add histogram-specific metadata

## Step Calculation

For `statsRange` and `hits` queries, the step (bucket size) is calculated in the backend:

**File:** [pkg/utils/utils.go](../pkg/utils/utils.go)

```
step = CalculateStep(minInterval, timeRange, maxDataPoints)
```

1. If the user specifies a `step` explicitly, it is used directly
2. Otherwise, `CalculateStep()` divides the time range by `MaxDataPoints` to get a raw step
3. The raw step is rounded up to a "nice" interval (1s, 5s, 10s, 15s, 30s, 1m, 5m, ..., 1y)
4. The step is clamped to at least `minInterval` (derived from the query's `interval` or `intervalMs`)

## Supplementary Queries

Grafana requests supplementary queries for the Explore view. This plugin supports two types:

**File:** [src/datasource.ts](../src/datasource.ts#L405)

| Type | Purpose | Implementation |
|------|---------|----------------|
| `LogsVolume` | Bar chart showing log counts over time | Uses `queryLogsVolume()` — re-runs the query as a hits query |
| `LogsSample` | Sample log lines | Re-runs the same instant query with limited results |

`getSupplementaryRequest()` creates a new query request based on the original targets, transforming them into the appropriate supplementary query format.

## Log Context Queries

When a user clicks "Show context" on a log line, the plugin fetches surrounding logs:

**File:** [src/datasource.ts](../src/datasource.ts#L493)

1. Extracts `_stream_id` from the log row's labels or dataframe fields
2. Builds a context query from `_stream_id` only (no explicit sort pipe is added in this path)
3. Builds a direction-aware time window (backward/forward) around the selected row timestamp
4. Sends it through the regular query path; with no explicit `queryType`, backend routing treats it as an instant query

## Live Streaming

Live log tailing uses Grafana's streaming infrastructure:

1. Frontend detects `liveStreaming` flag and calls `runLiveQueryThroughBackend()`
2. Backend `streamQuery()` ([datasource.go:327](../pkg/plugin/datasource.go#L327)) sends request to `/select/logsql/tail`
3. Response is parsed line-by-line via `parseStreamResponse()` ([response.go:157](../pkg/plugin/response.go#L157))
4. Frames are pushed to a channel and forwarded to the browser via Grafana Live

## Key Design Patterns

1. **Parallel query execution.** The backend processes all queries concurrently using `sync.WaitGroup` and protects the response map with a `sync.Mutex`.

2. **QueryType-driven polymorphism.** Both URL construction and response parsing are dispatched based on `QueryType`, making it straightforward to add new query types.

3. **Two-phase variable interpolation.** Variables are interpolated on the frontend (Grafana template variables, multi-value expansion) before the backend handles infrastructure-level variables (`$__interval`, `$__range`).

4. **NDJSON streaming for logs.** Instant log queries use line-by-line NDJSON parsing rather than loading the entire response into memory, supporting large result sets efficiently.

5. **Retry on transient failures.** The backend retries once on network errors (EOF, broken pipe, reset by peer) to handle temporary connection issues.

## See Also

- [System Overview](./onboarding-system-overview.md) — architecture context
- [Frontend Architecture](./onboarding-frontend.md) — datasource class and transformers in detail
- [Backend Architecture](./onboarding-backend.md) — Go plugin internals
- [LogsQL Handling](./onboarding-logsql-handling.md) — query manipulation and variable interpolation details
