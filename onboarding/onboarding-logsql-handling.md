# LogsQL Handling — Developer Onboarding Guide

This document covers how the plugin manipulates LogsQL expressions: adding/removing filters, injecting sort pipes, interpolating template variables, and converting between text and visual query representations.

**Prerequisites:** [System Overview](./onboarding-system-overview.md), [Query Flow](./onboarding-query-flow.md)

If you encounter unfamiliar terms, see the [Glossary](./glossary.md).

## Table of Contents

- [Overview](#overview)
- [Query Modification](#query-modification)
- [Sort Pipes](#sort-pipes)
- [Format Detection](#format-detection)
- [Variable Interpolation](#variable-interpolation)
- [Multi-Value Variable Handling](#multi-value-variable-handling)
- [LogsQL Utilities](#logsql-utilities)
- [Visual Query Builder](#visual-query-builder)
- [Ad-hoc Filter Integration](#ad-hoc-filter-integration)
- [Backend Template Variables](#backend-template-variables)
- [Key Design Patterns](#key-design-patterns)
- [See Also](#see-also)

## Overview

LogsQL manipulation happens in two places:

| Location | What | Files |
|----------|------|-------|
| **Frontend** | Filter manipulation, sort pipes, variable interpolation, visual query building | `modifyQuery.ts`, `parsingUtils.ts`, `LogsQL/`, `QueryBuilder/utils/` |
| **Backend** | Infrastructure variable replacement (`$__interval`, `$__range`), time field injection | `pkg/utils/utils.go` |

The frontend handles all user-facing query manipulation. The backend only replaces infrastructure-level variables that depend on runtime query parameters.

## Query Modification

**File:** [src/modifyQuery.ts](../src/modifyQuery.ts)

### Adding Filters

`addLabelToQuery()` ([line 62](../src/modifyQuery.ts#L62)) adds a filter to a query expression:

```typescript
addLabelToQuery(query: string, filter: AdHocVariableFilter): string
```

1. Splits the expression into filters and pipes using `splitExpression()`
2. Formats the filter value based on operator type:
   - Standard operators (`=`, `!=`, `=~`, `!~`, `<`, `>`): `key:"value"` or `key:~"value"`
   - Multi-value operators (`=|`, `!=|`): `key:in("val1","val2")` or `!key:in(...)`
   - Stream keys (`_stream`, `_stream_id`): special format `_stream: value`
3. Appends with `AND` connector
4. Re-attaches pipes

### Removing Filters

`removeLabelFromQuery()` ([line 75](../src/modifyQuery.ts#L75)):

1. Parses the expression into a `VisualQuery` via `buildVisualQueryFromString()`
2. Recursively removes matching filter values from the `FilterVisualQuery` tree via `recursiveRemove()` ([line 92](../src/modifyQuery.ts#L92))
3. Stringifies back to LogsQL via `parseVisualQueryToString()`

### Checking Filters

`queryHasFilter()` ([line 17](../src/modifyQuery.ts#L17)):

```typescript
queryHasFilter(query: string, key: string, value: string, operator?: string): boolean
```

Checks if the query contains a specific filter by testing all applicable operators.

### Key Normalization

`normalizeKey()` ([line 23](../src/modifyQuery.ts#L23)): Keys containing `:` are wrapped in double quotes to avoid parsing ambiguity (e.g., `"kubernetes.labels:app"` instead of `kubernetes.labels:app`).

### Operators

| Category | Operators | Example |
|----------|-----------|---------|
| Standard | `=`, `!=`, `=~`, `!~`, `<`, `>` | `level:="error"` |
| Multi-value | `=\|`, `!=\|` | `level:in("error","warn")` |
| Stream | (same as above) | `_stream: {host="server1"}` |

## Sort Pipes

**File:** [src/modifyQuery.ts](../src/modifyQuery.ts#L121)

`addSortPipeToQuery()` appends `| sort by (_time) <direction>` to instant queries:

```typescript
addSortPipeToQuery({ expr, queryType, direction }: Query, app: CoreApp, isLiveStreaming): string
```

**Rules:**
1. **Only for instant queries** — stats/hits queries don't need sorting
2. **Not for live streaming** — streaming results arrive in real time
3. **Skipped if sort already present** — regex checks for existing `| sort by (_time)` or `| order by (_time)`
4. **Direction depends on context:**
   - Dashboard/PanelEditor: uses panel `direction` setting (defaults to `desc`)
   - Explore: reads from local storage (`LOGS_SORT_ORDER`)
   - Other contexts: no sort added

## Format Detection

**File:** [src/modifyQuery.ts](../src/modifyQuery.ts#L149)

`getQueryFormat()` detects if a query produces histogram data:

```typescript
getQueryFormat(expr: string): Format | undefined
```

Checks for `histogram()` pipe function via `isExprHasStatsPipeFunc()`. Returns `'histogram'` format which tells the frontend to render as a heatmap.

## Variable Interpolation

**File:** [src/parsingUtils.ts](../src/parsingUtils.ts)

The interpolation pipeline is complex because LogsQL has different syntax requirements depending on the operator context. Here's the full chain:

### Step 1: Operator Conversion for Multi-Value

`replaceOperatorWithIn()` ([parsingUtils.ts](../src/parsingUtils.ts)) — converts standard operators to `in()` operators when the variable is multi-value:

```
fieldName:$variable   → fieldName:in($variable)
fieldName:!=$variable → !fieldName:in($variable)
```

**Detection logic** (`detectOperator()` [line ~129](../src/parsingUtils.ts)):
- Scans backwards from the variable position to find the operator
- Classifies as `NEGATED_EQUALS` (`:!`, `:!=`, `!=`) or `EQUALS` (`:`, `:=`, `=`)
- Finds the field name before the operator

### Step 2: Regex Variable Quoting

`doubleQuoteRegExp()` ([regExpOperator.ts](../src/LogsQL/regExpOperator.ts#L117)) ensures regex filters with variables are quoted before interpolation:

```
field:~$var  → field:~"$var"
```

### Step 3: Grafana Interpolation

`templateSrv.replace()` — Grafana's built-in interpolation, using `interpolateQueryExpr()` ([datasource.ts:241](../src/datasource.ts#L241)) as the custom formatter.

For multi-value variables, the formatter creates a marker:
```
$_StartMultiVariable_val1_separator_val2_EndMultiVariable
```

### Step 4: Post-Processing

After Grafana interpolation:
- `correctRegExpValueAll()` — fixes `$__all` for regex operators
- `correctMultiExactOperatorValueAll()` — fixes `$__all` for multi-exact operators
- `replaceMultiVariables()` — expands the `$_StartMultiVariable_..._EndMultiVariable` markers

`replaceVariables()` / `returnVariables()` exist in `parsingUtils.ts`, but they are not part of this runtime interpolation chain in `datasource.interpolateString()`.

## Multi-Value Variable Handling

When a template variable has multiple selected values, the expansion depends on the operator context:

| Context | Input | Output |
|---------|-------|--------|
| Regex operator (`~`) | `field:~$var` | `field:~"(val1\|val2)"` |
| `in()` operator | `field:in($var)` | `field:in(val1,val2)` |
| Default | `$var` | `val1 OR val2` |

The special `$__all` value (meaning "all values selected"):
- Regex: expands to `.*`
- Multi-exact: handled by `correctMultiExactOperatorValueAll()`

## LogsQL Utilities

### Regex Operator Detection

**File:** [src/LogsQL/regExpOperator.ts](../src/LogsQL/regExpOperator.ts)

- `getQueryExprVariableRegExp()` — detects regex variables (`field:~"$Variable"` or `field:~$Variable`)
- `replaceRegExpOperatorToOperator()` — converts `:~` to `:` when switching from regex to exact match
- `isRegExpOperatorInLastFilter()` — checks if the last filter uses a regex operator
- `doubleQuoteRegExp()` — wraps regex variable values in double quotes

### Multi-Exact Operator

**File:** [src/LogsQL/multiExactOperator.ts](../src/LogsQL/multiExactOperator.ts)

- `correctMultiExactOperatorValueAll()` — handles `$__all` value for multi-value exact match operators

### Stats Pipe Detection

**File:** [src/LogsQL/statsPipeFunctions.ts](../src/LogsQL/statsPipeFunctions.ts)

- `isExprHasStatsPipeFunctions()` — detects stats pipe functions in expression (for warnings)
- `isExprHasStatsPipeFunc(expr, funcName)` — checks for a specific stats function (e.g., `histogram`)

## Visual Query Builder

The visual query builder converts between a `VisualQuery` data structure and LogsQL text.

### Data Model

```typescript
interface VisualQuery {
  filters: FilterVisualQuery;  // Recursive filter tree
  pipes: string[];              // Pipe expressions (as strings)
}

interface FilterVisualQuery {
  values: (string | FilterVisualQuery)[];  // Leaf filters or nested groups
  operators: string[];                      // 'AND', 'OR' between values
}
```

### Parse: LogsQL → VisualQuery

**File:** [src/components/QueryEditor/QueryBuilder/utils/parseFromString.ts](../src/components/QueryEditor/QueryBuilder/utils/parseFromString.ts)

`buildVisualQueryFromString(expr)`:

1. **Split by pipes** — `splitExpression()` splits on top-level `|`, separating filters from pipes
2. **Parse filters** — `parseStringToFilterVisualQuery()` recursively parses the filter portion:
   - `splitByTopLevelParentheses()` handles nested parenthesized groups
   - Groups are split by `AND`/`OR` operators
   - Leaf strings become filter values; nested groups become child `FilterVisualQuery` objects
3. **Return** — `{ query: VisualQuery, errors: string[] }`

### Stringify: VisualQuery → LogsQL

**File:** [src/components/QueryEditor/QueryBuilder/utils/parseToString.ts](../src/components/QueryEditor/QueryBuilder/utils/parseToString.ts)

`parseVisualQueryToString(query)`:

1. Recursively stringify the `FilterVisualQuery` tree, joining values with operators
2. Append pipes with `|` separators

### Example

```
LogsQL: level:="error" AND host:="server1" | stats count() as total
         ↕
VisualQuery: {
  filters: {
    values: ['level:="error"', 'host:="server1"'],
    operators: ['AND']
  },
  pipes: ['stats count() as total']
}
```

### Mode Switching Safety

When switching from Code to Builder mode, the expression is parsed via `buildVisualQueryFromString()`. If parsing produces errors (the expression uses syntax the builder can't represent), a confirmation modal is shown instead of silently losing query parts.

## Ad-hoc Filter Integration

Ad-hoc filters from Grafana's variable system are integrated at two points:

1. **`toggleQueryFilter()`** ([datasource.ts:164](../src/datasource.ts#L164)) — called when user clicks filter icons in log results. Uses `addLabelToQuery()` / `removeLabelFromQuery()`.

2. **`applyTemplateVariables()`** ([datasource.ts:201](../src/datasource.ts#L201)) — applies ad-hoc filter variable values. Either:
   - Prepends to the expression: `<filters> | <user-query>` (if `isApplyExtraFiltersToRootQuery`)
   - Passes as `extra_filters` query parameter (default)

## Backend Template Variables

**File:** [pkg/utils/utils.go](../pkg/utils/utils.go)

The backend replaces infrastructure variables that depend on runtime query parameters:

| Variable | Replacement | Example |
|----------|-------------|---------|
| `$__interval` | Duration string from `IntervalMs` | `15s`, `1m`, `5m` |
| `$__interval_ms` | Raw milliseconds | `15000` |
| `$__range` | Duration string from `TimeRange` | `1h`, `6h`, `24h` |

`AddTimeFieldWithRange()` — for stats queries, injects `_time:[from,to]` into the expression if no time field is already present. This scopes the stats aggregation to the selected time range.

## Key Design Patterns

1. **Two-phase variable interpolation.** Frontend handles Grafana template variables (user-defined values); backend handles infrastructure variables (`$__interval`, `$__range`) that depend on runtime parameters. This separation keeps each layer focused.

2. **Marker-based multi-value expansion.** Multi-value variables use a placeholder format (`$_StartMultiVariable_..._EndMultiVariable`) that is expanded context-sensitively in a second pass. This avoids the complexity of trying to determine the correct expansion format during initial interpolation.

3. **Visual query as AST.** The `FilterVisualQuery` tree is essentially a simple AST for LogsQL filter expressions. Modifications (add/remove filters) operate on this tree, then the tree is stringified back. This is more robust than regex-based manipulation.

4. **Conservative mode switching.** When switching from Code to Builder, the plugin validates the expression is fully parseable. If not, it shows a warning rather than silently dropping unparseable parts.

5. **Variable protection during manipulation.** Before modifying a query expression, variables are replaced with internal markers (`__V_...__V__`). After modification, markers are restored. This prevents variable names from being accidentally modified or broken during filter operations.

## See Also

- [Query Flow](./onboarding-query-flow.md) — where interpolation fits in the query pipeline
- [Frontend Architecture](./onboarding-frontend.md) — datasource class and query editor
- [System Overview](./onboarding-system-overview.md) — high-level architecture
