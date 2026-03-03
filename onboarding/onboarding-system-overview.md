# System Overview — Developer Onboarding Guide

This document provides a high-level map of the VictoriaLogs Grafana datasource plugin. Read this first to understand the architecture before diving into specific subsystems.

If you encounter unfamiliar terms, see the [Glossary](./glossary.md).

## Table of Contents

- [What This Plugin Does](#what-this-plugin-does)
- [Hybrid Architecture](#hybrid-architecture)
- [Directory Structure](#directory-structure)
- [Component Table](#component-table)
- [High-Level Data Flow](#high-level-data-flow)
- [Plugin Registration and Lifecycle](#plugin-registration-and-lifecycle)
- [Key Design Patterns](#key-design-patterns)
- [See Also](#see-also)

## What This Plugin Does

This is a Grafana [datasource plugin](./glossary.md#datasource-plugin) that connects Grafana to [VictoriaLogs](./glossary.md#victorialogs). It allows users to:

- Write and execute [LogsQL](./glossary.md#logsql) queries in Grafana's Explore and Dashboard views
- Browse logs with full-text search, filtering, and log context
- Visualize log statistics over time (counts, histograms, aggregations)
- Use Grafana [template variables](./glossary.md#template-variable) backed by VictoriaLogs field data
- Extract [derived fields](./glossary.md#derived-field) (links, trace IDs) from log lines
- Stream live log tails

## Hybrid Architecture

The plugin has two halves that communicate through Grafana's plugin protocol:

```
┌──────────────────────────────────────────────────────────────────┐
│  Grafana Browser                                                 │
│                                                                  │
│  ┌──────────────────────────────────────────────────────┐       │
│  │  Frontend (TypeScript/React)                         │       │
│  │  - Query Editor UI (Code + Builder modes)            │       │
│  │  - Datasource class (variable interpolation, etc.)   │       │
│  │  - Response transformers (derived fields, levels)    │       │
│  └───────────────────────┬──────────────────────────────┘       │
│                          │ Grafana Plugin Protocol               │
│                          │ (DataSourceWithBackend)               │
└──────────────────────────┼───────────────────────────────────────┘
                           │
┌──────────────────────────┼───────────────────────────────────────┐
│  Grafana Server          │                                       │
│  ┌───────────────────────▼──────────────────────────────┐       │
│  │  Backend (Go)                                        │       │
│  │  - Query routing by QueryType                        │       │
│  │  - HTTP request building                             │       │
│  │  - Response parsing (NDJSON, JSON)                   │       │
│  │  - Resource API (field names/values, tenants)        │       │
│  └───────────────────────┬──────────────────────────────┘       │
│                          │ HTTP                                   │
└──────────────────────────┼───────────────────────────────────────┘
                           │
┌──────────────────────────▼───────────────────────────────────────┐
│  VictoriaLogs Server                                             │
│  - /select/logsql/query         (instant log queries)            │
│  - /select/logsql/stats_query   (point-in-time stats)            │
│  - /select/logsql/stats_query_range (stats over time)            │
│  - /select/logsql/hits          (hit counts by field)            │
│  - /select/logsql/tail          (live streaming)                 │
│  - /select/logsql/field_names   (autocomplete)                   │
│  - /select/logsql/field_values  (autocomplete)                   │
│  - /select/tenant_ids           (multi-tenancy)                  │
└──────────────────────────────────────────────────────────────────┘
```

**Why a backend plugin?** Grafana datasource plugins can be frontend-only (browser → VictoriaLogs directly) or have a backend component. This plugin uses a Go backend so that:

1. VictoriaLogs credentials stay on the server (never exposed to the browser)
2. The server can proxy and transform responses (NDJSON parsing, alerting support)
3. Live streaming works through Grafana's server-side streaming infrastructure
4. Alerting rules can execute queries without a browser session

## Directory Structure

```
victorialogs-datasource/
├── src/                         # Frontend (TypeScript/React)
│   ├── module.ts                # Plugin entry point
│   ├── datasource.ts            # VictoriaLogsDatasource class
│   ├── types.ts                 # Shared type definitions
│   ├── modifyQuery.ts           # LogsQL query manipulation
│   ├── parsingUtils.ts          # Variable interpolation
│   ├── language_provider.ts     # Autocomplete support
│   ├── components/
│   │   ├── QueryEditor/         # Query editor (Code + Builder modes)
│   │   └── monaco-query-field/  # Monaco editor integration
│   ├── configuration/           # Datasource settings UI
│   ├── transformers/            # Response post-processing
│   ├── variableSupport/         # Template variable queries
│   ├── LogsQL/                  # LogsQL-specific utilities
│   └── utils/                   # Shared utilities
├── pkg/                         # Backend (Go)
│   ├── main.go                  # Plugin entry point
│   ├── plugin/
│   │   ├── datasource.go        # Datasource, routing, health check
│   │   ├── query.go             # Query URL building
│   │   ├── response.go          # Response parsing
│   │   └── fields_query.go      # Field name/value proxy
│   └── utils/
│       ├── utils.go             # Time parsing, step calculation
│       └── stream_fields_parser.go
├── provisioning/                # Local Grafana datasource config
├── compose.yaml                 # Local dev environment
├── Makefile                     # Build targets
├── Magefile.go                  # Go build system
└── .github/workflows/           # CI/CD
```

## Component Table

| Component | Language | Key File | Purpose |
|-----------|----------|----------|---------|
| Plugin registration | TS | [src/module.ts](../src/module.ts#L7) | Wires datasource, editor, config into Grafana |
| Datasource class | TS | [src/datasource.ts](../src/datasource.ts#L80) | Query execution, variable interpolation, filters |
| Query Editor | TS | [src/components/QueryEditor/QueryEditor.tsx](../src/components/QueryEditor/QueryEditor.tsx#L30) | UI for writing LogsQL queries |
| Query Builder | TS | [src/components/QueryEditor/QueryBuilder/](../src/components/QueryEditor/QueryBuilder/) | Visual query building mode |
| Config Editor | TS | [src/configuration/ConfigEditor.tsx](../src/configuration/ConfigEditor.tsx) | Datasource settings UI |
| Response transformers | TS | [src/transformers/transformBackendResult.ts](../src/transformers/transformBackendResult.ts#L15) | Frame processing, derived fields, levels |
| Variable support | TS | [src/variableSupport/VariableSupport.ts](../src/variableSupport/VariableSupport.ts) | Template variable queries |
| LogsQL utilities | TS | [src/modifyQuery.ts](../src/modifyQuery.ts), [src/parsingUtils.ts](../src/parsingUtils.ts) | Query manipulation, variable handling |
| Backend entry | Go | [pkg/main.go](../pkg/main.go#L13) | Starts plugin process |
| Backend datasource | Go | [pkg/plugin/datasource.go](../pkg/plugin/datasource.go#L45) | Query routing, HTTP proxying, health |
| Query builder | Go | [pkg/plugin/query.go](../pkg/plugin/query.go#L46) | URL construction per query type |
| Response parser | Go | [pkg/plugin/response.go](../pkg/plugin/response.go) | NDJSON/JSON → Grafana data frames |
| Field proxy | Go | [pkg/plugin/fields_query.go](../pkg/plugin/fields_query.go) | Proxies field_names/field_values requests |

## High-Level Data Flow

The primary query flow (non-streaming) is:

```
1. User writes LogsQL in QueryEditor
           │
2. Frontend datasource.ts:query() [line 124]
   - Applies template variables
   - Adds sort pipes
   - Sets timezone offset
   - Detects format (histogram)
           │
3. DataSourceWithBackend sends to Go backend
           │
4. Backend datasource.go:QueryData() [line 289]
   - Processes queries in parallel (WaitGroup)
   - For each query:
     a. query.go:getQueryURL() [line 66] → builds HTTP URL by QueryType
     b. datasource.go:datasourceQuery() [line 369] → HTTP request to VictoriaLogs
     c. datasource.go:query() [line 438] → routes to response parser
           │
5. Response flows back:
   - response.go:parseInstantResponse() [line 39] (NDJSON)
     or parseStatsResponse() [line 268] (JSON)
     or parseHitsResponse() [line 290] (JSON)
           │
6. Frontend transformBackendResult() [line 15]
   - Groups frames by type
   - Adds derived fields, log levels
   - Formats labels
           │
7. Grafana renders logs/charts
```

## Plugin Registration and Lifecycle

**Frontend registration** ([src/module.ts](../src/module.ts)):

```typescript
export const plugin = new DataSourcePlugin(VictoriaLogsDatasource)
  .setQueryEditor(QueryEditorByApp)
  .setConfigEditor(ConfigEditor);
```

This tells Grafana:
- Use `VictoriaLogsDatasource` as the datasource class
- Use `QueryEditorByApp` for the query editor (routes to appropriate editor by context)
- Use `ConfigEditor` for the datasource settings page

**Backend registration** ([pkg/main.go](../pkg/main.go#L21)):

```go
err := backend.Manage(VL_PLUGIN_ID, backend.ServeOpts{
    CallResourceHandler: ds,    // Resource API (field names, tenants, etc.)
    QueryDataHandler:    ds,    // Main query execution
    CheckHealthHandler:  ds,    // Health check endpoint
    StreamHandler:       ds,    // Live log streaming
})
```

The Go binary runs as a separate process managed by Grafana. It implements four handler interfaces:
- **QueryDataHandler**: Executes LogsQL queries and returns data frames
- **CallResourceHandler**: HTTP mux for resource API (field names, field values, tenant IDs, VMUI URL)
- **CheckHealthHandler**: Verifies VictoriaLogs is reachable via `/health`
- **StreamHandler**: Manages live tail connections via `/select/logsql/tail`

## Key Design Patterns

1. **Frontend does query manipulation, backend does HTTP proxying.** The frontend handles all LogsQL expression manipulation (adding filters, sort pipes, variable interpolation). The backend receives the final expression and builds HTTP requests to VictoriaLogs.

2. **QueryType-based routing.** Both frontend and backend use the `QueryType` enum (`instant`, `stats`, `statsRange`, `hits`) to determine which VictoriaLogs endpoint to call and how to parse the response.

3. **DataSourceWithBackend inheritance.** The frontend `VictoriaLogsDatasource` extends Grafana's `DataSourceWithBackend`, which handles the plugin protocol communication. The class adds VictoriaLogs-specific behavior on top.

4. **Response transformation pipeline.** Backend returns raw data frames; the frontend `transformBackendResult()` enriches them with derived fields, log levels, and label formatting before Grafana renders them.

5. **Multi-tenancy via headers.** Tenant isolation is handled by injecting `AccountID`/`ProjectID` HTTP headers into every request, configured once in datasource settings.

## See Also

- [Query Flow](./onboarding-query-flow.md) — detailed end-to-end query walkthrough
- [Frontend Architecture](./onboarding-frontend.md) — TypeScript components deep dive
- [Backend Architecture](./onboarding-backend.md) — Go plugin deep dive
- [Developer Setup](./onboarding-dev-setup.md) — building and running locally
