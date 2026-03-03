# Glossary

Terms used across the onboarding docs. Entries link to the onboarding doc section where the concept is explained in detail.

---

### Ad-hoc Filter

A Grafana feature that lets users add key-value filters to queries on the fly from the UI, without editing the query expression. In this plugin, ad-hoc filters are injected into the LogsQL expression via `addLabelToQuery()` or passed as `extra_filters` query parameters. See [Frontend Architecture — Ad-hoc Filters](./onboarding-frontend.md#ad-hoc-filters).

### DataSourceWithBackend

A Grafana SDK base class for datasource plugins that have a Go backend. The frontend class extends it to send queries through the plugin protocol instead of making direct HTTP calls. See [System Overview — Plugin Registration](./onboarding-system-overview.md#plugin-registration-and-lifecycle).

### Datasource Plugin

A Grafana plugin type that provides a connection to an external data source. This plugin connects Grafana to VictoriaLogs. Registered in [src/module.ts](../src/module.ts).

### Derived Field

A synthetic field extracted from log lines using regex patterns or label lookups. Used to create clickable links (e.g., trace ID → Tempo). Configured in datasource settings. See [Frontend Architecture — Transformers](./onboarding-frontend.md#response-transformers).

### Extra Filters

Additional LogsQL filter expressions passed as a query parameter (`extra_filters`) to VictoriaLogs, separate from the main query expression. Used for ad-hoc filters when not applied to the root query.

### FilterVisualQuery

The recursive data structure representing a parsed LogsQL filter tree in the visual query builder. Contains `values` (strings or nested `FilterVisualQuery` objects) and `operators` (`AND`/`OR`). See [LogsQL Handling — Visual Query Builder](./onboarding-logsql-handling.md#visual-query-builder).

### Grafana Data Frame

The standard data structure Grafana uses to represent query results. A frame contains typed fields (columns) with values. The backend creates frames from VictoriaLogs responses; the frontend enriches them. See [Query Flow](./onboarding-query-flow.md).

### Hits Query

A query that returns hit counts grouped by field values over time buckets. Uses the `/select/logsql/hits` endpoint. QueryType: `hits`. See [Query Flow — Query Types](./onboarding-query-flow.md#query-types-and-endpoints).

### Instant Query

A query that returns raw log records matching a LogsQL expression. Uses the `/select/logsql/query` endpoint. QueryType: `instant`. This is the default query type. See [Query Flow — Query Types](./onboarding-query-flow.md#query-types-and-endpoints).

### Log Level Rules

User-configured rules that map log labels or message patterns to severity levels (e.g., `error`, `warn`, `info`). Applied during response transformation to add a `level` field to log frames. See [Frontend Architecture — Transformers](./onboarding-frontend.md#response-transformers).

### LogsQL

The query language used by VictoriaLogs for searching and analyzing logs. Supports filters, pipes, and aggregations. This plugin provides a Code editor (raw LogsQL) and a Builder (visual construction). See [LogsQL Handling](./onboarding-logsql-handling.md).

### Multi-tenancy Headers

HTTP headers (`AccountID`, `ProjectID`) sent with every request to VictoriaLogs to scope queries and ingestion to a specific tenant. Configured in datasource settings. See [Backend Architecture — Multi-tenancy](./onboarding-backend.md#multi-tenancy-header-handling).

### Pipe

A LogsQL processing stage appended to a query expression with `|`. Examples: `| stats count() as total`, `| sort by (_time)`, `| fields _msg`. Pipes transform or aggregate log records. See [LogsQL Handling](./onboarding-logsql-handling.md).

### Query Builder / Visual Query

The visual mode of the query editor that lets users construct LogsQL expressions by selecting filters and operators from dropdowns rather than typing raw LogsQL. Uses `FilterVisualQuery` + `pipes` as its data model. See [LogsQL Handling — Visual Query Builder](./onboarding-logsql-handling.md#visual-query-builder).

### QueryEditorMode

Enum (`builder` | `code`) that determines which query editor variant is shown. `code` shows the Monaco-based LogsQL editor; `builder` shows the visual query builder. Defined in [src/types.ts](../src/types.ts#L42).

### QueryType

Enum that determines which VictoriaLogs API endpoint a query targets. Values: `instant`, `stats`, `statsRange`, `hits`. Defined in [src/types.ts](../src/types.ts#L35) (frontend) and [pkg/plugin/query.go](../pkg/plugin/query.go#L34) (backend).

### Resource API

HTTP endpoints exposed by the backend plugin (via `CallResourceHandler`) for non-query operations: fetching field names, field values, tenant IDs, and VMUI URLs. Called by the frontend for autocomplete and configuration. See [Backend Architecture — Resource API](./onboarding-backend.md#http-route-registration).

### Sort Pipe

A `| sort by (_time)` pipe automatically appended to instant queries to control log ordering. Direction depends on context (Explore sort order vs Dashboard panel direction). See [LogsQL Handling — Sort Pipes](./onboarding-logsql-handling.md#sort-pipes).

### Stats Query

A query that returns aggregated statistics at a single point in time. Uses the `/select/logsql/stats_query` endpoint. QueryType: `stats`. See [Query Flow — Query Types](./onboarding-query-flow.md#query-types-and-endpoints).

### Stats Range Query

A query that returns aggregated statistics over time buckets (time series). Uses the `/select/logsql/stats_query_range` endpoint. QueryType: `statsRange`. See [Query Flow — Query Types](./onboarding-query-flow.md#query-types-and-endpoints).

### Step

The time interval between data points in a stats range query or hits query. Calculated from `MaxDataPoints` and the time range, or specified explicitly by the user. See [Query Flow — Step Calculation](./onboarding-query-flow.md#step-calculation).

### Supplementary Query

An automatic background query created by Grafana to show additional visualizations alongside the main query results. This plugin supports `LogsVolume` (bar chart of log counts over time) and `LogsSample` (sample log lines). See [Frontend Architecture — Supplementary Queries](./onboarding-frontend.md#supplementary-queries).

### Tail / Live Streaming

Real-time log streaming using the `/select/logsql/tail` endpoint. Logs are pushed to the browser as they arrive via Grafana's server-side streaming infrastructure. See [Backend Architecture — Streaming](./onboarding-backend.md#streaming-and-tail-support).

### Template Variable

A Grafana feature that lets users create dashboard variables (dropdowns) backed by query results. This plugin supports `fieldName` and `fieldValue` variable types. See [Frontend Architecture — Variable Support](./onboarding-frontend.md#variable-support).

### VictoriaLogs

A log management system by VictoriaMetrics. This plugin connects Grafana to VictoriaLogs for log querying and visualization.

### VisualQuery

The data structure for the visual query builder: `{ filters: FilterVisualQuery, pipes: string[] }`. Represents a parsed LogsQL expression that can be edited visually. Defined in [src/types.ts](../src/types.ts#L105).
