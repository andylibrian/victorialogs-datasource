# Frontend Architecture — Developer Onboarding Guide

This document covers the TypeScript/React frontend of the VictoriaLogs Grafana datasource plugin. It explains the datasource class, query editor components, response transformers, variable support, and language provider.

**Prerequisites:** [System Overview](./onboarding-system-overview.md)

If you encounter unfamiliar terms, see the [Glossary](./glossary.md).

## Table of Contents

- [Plugin Entry Point](#plugin-entry-point)
- [Datasource Class](#datasource-class)
- [Query Editor](#query-editor)
- [Configuration Editor](#configuration-editor)
- [Response Transformers](#response-transformers)
- [Variable Support](#variable-support)
- [Language Provider](#language-provider)
- [Ad-hoc Filters](#ad-hoc-filters)
- [Supplementary Queries](#supplementary-queries)
- [Test Organization](#test-organization)
- [Key Design Patterns](#key-design-patterns)
- [See Also](#see-also)

## Plugin Entry Point

**File:** [src/module.ts](../src/module.ts)

```typescript
export const plugin = new DataSourcePlugin(VictoriaLogsDatasource)
  .setQueryEditor(QueryEditorByApp)
  .setConfigEditor(ConfigEditor);
```

This registers three components with Grafana:
- **VictoriaLogsDatasource** — the datasource class that handles query execution
- **QueryEditorByApp** — the query editor, which routes to different editors based on Grafana app context
- **ConfigEditor** — the datasource settings page

## Datasource Class

**File:** [src/datasource.ts](../src/datasource.ts#L80)

`VictoriaLogsDatasource` extends Grafana's `DataSourceWithBackend` and implements `DataSourceWithLogsContextSupport`.

### Key Properties

| Property | Type | Source | Purpose |
|----------|------|--------|---------|
| `maxLines` | `number` | Settings (default 1000) | Maximum log lines per query |
| `derivedFields` | `DerivedFieldConfig[]` | Settings | Regex/label extractors for links |
| `httpMethod` | `string` | Settings (default POST) | HTTP method for VictoriaLogs |
| `customQueryParameters` | `URLSearchParams` | Settings | Extra URL params for every query |
| `languageProvider` | `LogsQlLanguageProvider` | Created in constructor | Autocomplete support |
| `queryBuilderLimits` | `QueryBuilderLimits` | Settings | Max field names/values in builder |
| `logLevelRules` | `LogLevelRule[]` | Settings | Rules for log level detection |
| `multitenancyHeaders` | `MultitenancyHeaders` | Settings | AccountID/ProjectID headers |

### Key Methods

| Method | Line | Purpose |
|--------|------|---------|
| `query()` | [124](../src/datasource.ts#L124) | Main entry: prepares queries, adds sort pipes, sends to backend |
| `runQuery()` | [151](../src/datasource.ts#L151) | Sends to backend and pipes through `transformBackendResult()` |
| `applyTemplateVariables()` | [201](../src/datasource.ts#L201) | Interpolates Grafana template variables and ad-hoc filters |
| `interpolateString()` | [331](../src/datasource.ts#L331) | Full interpolation chain (multi-value, regex, etc.) |
| `interpolateQueryExpr()` | [241](../src/datasource.ts#L241) | Custom formatter for multi-value variables |
| `toggleQueryFilter()` | [164](../src/datasource.ts#L164) | Add/remove ad-hoc filters via UI clicks |
| `getSupplementaryRequest()` | [405](../src/datasource.ts#L405) | Create LogsVolume/LogsSample requests |
| `getLogRowContext()` | [493](../src/datasource.ts#L493) | Fetch surrounding logs for context view |
| `metricFindQuery()` | [267](../src/datasource.ts#L267) | Execute template variable queries |
| `getTagKeys()` / `getTagValues()` | [284](../src/datasource.ts#L284) | Provide field names/values for ad-hoc filter dropdowns |
| `fetchTenantIds()` | [568](../src/datasource.ts#L568) | Fetch available tenant IDs for settings |

### Constructor Initialization

The constructor ([line 97](../src/datasource.ts#L97)) reads settings from `DataSourceInstanceSettings<Options>`:
- Parses `maxLines`, `httpMethod`, `customQueryParameters` from JSON data
- Creates a `LogsQlLanguageProvider` for autocomplete
- Creates a `VariableSupport` instance for template variables
- Parses multi-tenancy headers
- Wires `QueryEditor` as the annotation editor

## Query Editor

### Component Hierarchy

```
QueryEditorByApp [src/components/QueryEditor/QueryEditorByApp.tsx]
├── QueryEditorForAlerting   (if app === CoreApp.CloudAlerting)
└── QueryEditor              (default)
    ├── EditorHeader
    │   ├── QueryEditorModeToggle  (Code ↔ Builder switch)
    │   └── VmuiLink               (link to VictoriaMetrics UI)
    ├── QueryCodeEditor            (if mode === Code)
    │   └── MonacoQueryField       (Monaco editor with LogsQL syntax)
    └── QueryBuilderContainer      (if mode === Builder)
        └── QueryBuilder
            └── QueryBuilderFilter (recursive filter tree)
    ├── QueryEditorOptions         (maxLines, queryType, step, etc.)
    ├── QueryEditorStatsWarn       (warning if stats query has no pipe)
    ├── QueryEditorVariableRegexpError
    ├── LevelQueryFilter           (level filter UI in Explore)
    └── QueryHintsExample          (query suggestions)
```

### QueryEditorByApp

**File:** [src/components/QueryEditor/QueryEditorByApp.tsx](../src/components/QueryEditor/QueryEditorByApp.tsx)

Routes to the appropriate editor variant:
- `CoreApp.CloudAlerting` → `QueryEditorForAlerting` (simplified editor for alert rules)
- Everything else → `QueryEditor` (full-featured editor)

### QueryEditor

**File:** [src/components/QueryEditor/QueryEditor.tsx](../src/components/QueryEditor/QueryEditor.tsx#L30)

The main query editor component. Key behaviors:

- **Mode switching** ([line 47](../src/components/QueryEditor/QueryEditor.tsx#L47)): When switching to Builder mode, validates the expression can be parsed via `buildVisualQueryFromString()`. If parsing fails, shows a confirmation modal.
- **Default query** ([constants.ts](../src/components/QueryEditor/constants.ts)): In Explore, injects a default expression if the query is empty.
- **Sort management**: The `useLogsSort()` hook manages sort direction based on panel type.
- **Stale data tracking**: Tracks when the query has changed but not been re-run.

### Visual Query Builder

**Directory:** [src/components/QueryEditor/QueryBuilder/](../src/components/QueryEditor/QueryBuilder/)

The builder converts between a `VisualQuery` data structure and LogsQL text:

```
LogsQL text ←→ VisualQuery { filters: FilterVisualQuery, pipes: string[] }
```

**Parse:** `buildVisualQueryFromString()` ([parseFromString.ts](../src/components/QueryEditor/QueryBuilder/utils/parseFromString.ts))
- Splits expression by top-level `|` to separate filters from pipes
- Recursively parses filter groups respecting parentheses and `AND`/`OR` operators
- Returns `{ query: VisualQuery, errors: string[] }`

**Stringify:** `parseVisualQueryToString()` ([parseToString.ts](../src/components/QueryEditor/QueryBuilder/utils/parseToString.ts))
- Reconstructs filter text from the `FilterVisualQuery` tree
- Appends pipes with `|` separators

**Filter tree:** `FilterVisualQuery` is recursive — values can be strings (leaf filters) or nested `FilterVisualQuery` objects (grouped sub-expressions).

## Configuration Editor

**File:** [src/configuration/ConfigEditor.tsx](../src/configuration/ConfigEditor.tsx)

The datasource settings page contains these sections:

| Section | Component | Purpose |
|---------|-----------|---------|
| Helpful Links | `HelpfulLinks` | Documentation links |
| HTTP Settings | `DataSourceHttpSettings` | URL, auth, TLS |
| Alerting | `AlertingSettings` | Alerting-specific config |
| Multi-tenancy | `TenantSettings` | AccountID/ProjectID headers |
| Limits | `LimitsSettings` → `QuerySettings` | Max lines per query |
| Log Settings | `LogsSettings` | Log-specific options |
| Derived Fields | `DerivedFields` | Regex extractors for links |
| Log Level Rules | `LogLevelRulesEditor` | Level detection rules |

Settings are stored in `DataSourceInstanceSettings.jsonData` as the `Options` type ([src/types.ts:8](../src/types.ts#L8)).

### Derived Fields Configuration

Each derived field has:
- `matcherRegex` / `matcherType` — regex pattern or label key to extract from
- `name` — field name in the result
- `url` / `datasourceUid` — link target (external URL or internal datasource)

### Log Level Rules Configuration

Each rule maps a condition to a severity level:
- `field` — the label or message field to evaluate
- `condition` — the matching expression
- `level` — the severity (error, warn, info, debug, etc.)

## Response Transformers

**File:** [src/transformers/transformBackendResult.ts](../src/transformers/transformBackendResult.ts#L15)

After the backend returns data frames, `transformBackendResult()` enriches them:

1. **Group frames** by type via `groupFrames()`:
   - `streamsFrames` — log data (from instant queries)
   - `metricInstantFrames` — single-value stats
   - `metricRangeFrames` — time series stats
   - `histogramFrames` — histogram data

2. **Process each group** through dedicated processors in [src/transformers/frameProcessors/](../src/transformers/frameProcessors/):

### Stream Frame Processor

**File:** [src/transformers/frameProcessors/streamFrameProcessor.ts](../src/transformers/frameProcessors/streamFrameProcessor.ts)

For each log frame:
1. Sets `preferredVisualisationType: 'logs'` in frame metadata
2. **Adds level field** — evaluates log level rules against labels and message via `addLevelField()` ([src/transformers/fields/levelField.ts](../src/transformers/fields/levelField.ts))
3. **Extracts derived fields** — applies regex patterns or label lookups via `getDerivedFields()` ([src/transformers/fields/derivedField.ts](../src/transformers/fields/derivedField.ts))
4. **Formats labels** — transforms label objects for dashboard views via `getStreamFields()` ([src/transformers/fields/labelField.ts](../src/transformers/fields/labelField.ts))
5. **Extracts stream IDs** — stores `_stream_id` values for log context queries

## Variable Support

### VariableSupport Class

**File:** [src/variableSupport/VariableSupport.ts](../src/variableSupport/VariableSupport.ts)

Extends `CustomVariableSupport` to provide template variable queries. Delegates to `datasource.metricFindQuery()`.

### VariableQueryEditor

**File:** [src/components/VariableQueryEditor/VariableQueryEditor.tsx](../src/components/VariableQueryEditor/VariableQueryEditor.tsx)

UI for configuring template variables:
- **Type selector**: `FieldName` (lists unique field names) or `FieldValue` (lists values for a specific field)
- **Field selector**: auto-populated dropdown of field names (for FieldValue type)
- **Query filter**: optional LogsQL filter to scope results
- **Limit**: maximum values to return (default 100)

### Query Execution

`metricFindQuery()` ([datasource.ts:267](../src/datasource.ts#L267)):
1. Interpolates the variable query's field and filter expression
2. Calls `languageProvider.getFieldList()` with the configured type, field, query, and time range
3. Returns results as `MetricFindValue[]` for Grafana's variable system

## Language Provider

**File:** [src/language_provider.ts](../src/language_provider.ts)

`LogsQlLanguageProvider` provides autocomplete data for the query editor:

- **`getFieldList()`** — fetches field names or field values from VictoriaLogs
  - Calls `/select/logsql/field_names` or `/select/logsql/field_values` via the backend resource API
  - Caches results with LRU eviction (default 100 entries)
  - Supports server-side filtering via `fieldValueFilter` parameter

## Ad-hoc Filters

Ad-hoc filters let users add key-value filters from the Grafana UI without editing the query expression.

**Implementation:**
- `toggleQueryFilter()` ([datasource.ts:164](../src/datasource.ts#L164)) — called when user clicks a filter icon on a log field
- `getTagKeys()` / `getTagValues()` ([datasource.ts:284](../src/datasource.ts#L284)) — provide dropdown options for ad-hoc filter variable
- `getExtraFilters()` ([datasource.ts:229](../src/datasource.ts#L229)) — converts ad-hoc filter array to a filter expression

Ad-hoc filters can be applied in two ways (controlled by `isApplyExtraFiltersToRootQuery`):
1. **Root query** — prepended to the expression: `<ad-hoc-filters> | <user-query>`
2. **Extra filters** — passed as `extra_filters` query parameter (default)

## Supplementary Queries

**File:** [src/datasource.ts](../src/datasource.ts#L405)

Grafana requests supplementary data for Explore view:

| Type | QueryType Used | Purpose |
|------|---------------|---------|
| `LogsVolume` | `hits` | Bar chart of log counts over time, grouped by level fields |
| `LogsSample` | `instant` | Sample log lines for preview |

`getSupplementaryQuery()` ([line 427](../src/datasource.ts#L427)):
- **LogsVolume**: Re-creates the query as a `hits` query with a calculated step, grouped by level-related fields
- **LogsSample**: Re-creates the query as an `instant` query with maxLines limit

## Test Organization

Tests live next to their source files as `*.test.ts` / `*.test.tsx`:

| Test File | Tests |
|-----------|-------|
| `src/datasource.test.ts` | Datasource class behavior |
| `src/modifyQuery.test.ts` | Query manipulation functions |
| `src/parsing.test.ts` | Quote handling utilities |
| `src/parsingUtils.test.ts` | Operator replacement, variable handling |
| `src/LogsQL/regExpOperator.test.ts` | Regex variable detection |
| `src/LogsQL/multiExactOperator.test.ts` | Multi-value exact match |
| `src/LogsQL/statsPipeFunctions.test.ts` | Stats pipe detection |
| `src/components/QueryEditor/QueryBuilder/utils/parsing.test.ts` | Visual query parse/stringify |
| `src/transformers/transformBackendResult.test.ts` | Frame transformation |
| `src/transformers/frameProcessors/histogramFrameProcessor.test.ts` | Histogram processing |
| `src/utils/timeUtils.test.ts` | Time utilities |

Run tests with:
```bash
yarn test                              # all tests
yarn test -- --testPathPattern=parsing # specific pattern
```

## Key Design Patterns

1. **DataSourceWithBackend delegation.** The datasource class extends `DataSourceWithBackend`, inheriting plugin protocol communication. It adds VictoriaLogs-specific behavior (variable interpolation, sort pipes, derived fields) as pre/post-processing around the base class's `query()` method.

2. **Two-mode query editor.** The Code editor (Monaco) is for power users who write raw LogsQL. The Builder (visual) is for users who prefer dropdowns. Both share the same `Query` type and the same `expr` field — the Builder just parses/stringifies the expression automatically.

3. **Recursive filter tree.** The visual query builder represents filters as a recursive `FilterVisualQuery` tree, supporting arbitrary nesting of AND/OR groups. This maps naturally to LogsQL's parenthesized filter expressions.

4. **Transformation pipeline.** Backend returns raw frames → `transformBackendResult()` enriches them with derived fields, levels, and label formatting → Grafana renders. This keeps the backend simple (just HTTP proxying and parsing) and the frontend handles display concerns.

5. **Multi-value variable markers.** Rather than trying to handle multi-value variables inline during interpolation, the plugin uses a marker format (`$_StartMultiVariable_..._EndMultiVariable`) that is expanded in a second pass based on the operator context (regex, in(), OR).

## See Also

- [System Overview](./onboarding-system-overview.md) — high-level architecture
- [Query Flow](./onboarding-query-flow.md) — end-to-end query walkthrough
- [Backend Architecture](./onboarding-backend.md) — Go plugin internals
- [LogsQL Handling](./onboarding-logsql-handling.md) — query manipulation details
