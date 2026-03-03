# Grafana Explore Internals — Developer Onboarding Guide

This guide maps the Explore internals that power:

- supplementary queries (logs volume, logs sample)
- log context (`Show context`)
- live streaming / live tail mode

It focuses on `@grafana/` code paths and how to map them back to this plugin.

**Prerequisites:** [Query Flow](./onboarding-query-flow.md), [Frontend](./onboarding-frontend.md)

## Table of Contents

- [Core Interfaces](#core-interfaces)
- [Supplementary Queries Flow](#supplementary-queries-flow)
- [Log Context Flow](#log-context-flow)
- [Live Streaming Flow](#live-streaming-flow)
- [Reference Implementation in Loki](#reference-implementation-in-loki)
- [VictoriaLogs Mapping](#victorialogs-mapping)
- [Reading Path](#reading-path)

## Core Interfaces

Explore depends on datasource capability interfaces in:

- `grafana/packages/grafana-data/src/types/logs.ts`
  - `DataSourceWithSupplementaryQueriesSupport`
  - `DataSourceWithLogsContextSupport`
  - `SupplementaryQueryType` (`LogsVolume`, `LogsSample`)
  - guards: `hasSupplementaryQuerySupport()`, `hasLogsContextSupport()`, `hasLogsContextUiSupport()`

Live mode flag is part of request shape:

- `grafana/packages/grafana-data/src/types/datasource.ts`
  - `DataQueryRequest.liveStreaming?: boolean`

## Supplementary Queries Flow

Primary orchestration lives in:

- `grafana/public/app/features/explore/state/query.ts`

Key flow:

1. `runQueries()` runs normal datasource queries.
2. If not in live mode, it dispatches `handleSupplementaryQueries(...)`.
3. `handleSupplementaryQueries(...)` builds providers with:
   - `getSupplementaryQueryProvider(...)` from
   - `grafana/public/app/features/explore/utils/supplementaryQueries.ts`
4. Provider is stored with `storeSupplementaryQueryDataProviderAction`.
5. If enabled, `loadSupplementaryQueryData(...)` subscribes and stores emitted data.

UI consumption points:

- `grafana/public/app/features/explore/Explore.tsx` (logs sample panel visibility + toggle)
- `grafana/public/app/features/explore/Logs/LogsContainer.tsx` (logs volume toggle + load)
- `grafana/public/app/features/explore/Logs/LogsSamplePanel.tsx`

## Log Context Flow

Explore logs panel delegates to datasource context APIs in:

- `grafana/public/app/features/explore/Logs/LogsContainer.tsx`
  - checks capability via `hasLogsContextSupport()`
  - calls `getLogRowContext()`
  - optionally calls `getLogRowContextQuery()` and `getLogRowContextUi()`

This means the datasource contract determines whether context actions appear.

## Live Streaming Flow

Live mode controls:

- `grafana/public/app/features/explore/LiveTailButton.tsx`
- `grafana/public/app/features/explore/useLiveTailControls.ts`

State transitions:

- `grafana/public/app/features/explore/state/time.ts`
  - `changeRefreshInterval` sets `isLive`, `isPaused`, and panel state

Execution path:

- `grafana/public/app/features/explore/state/query.ts`
  - `runQueries()` sets `QueryOptions.liveStreaming = live`
  - request is executed through `runRequest(...)` from
  - `grafana/public/app/features/query/state/runRequest.ts`

In live mode, supplementary providers are reset and not loaded.

## Reference Implementation in Loki

Loki datasource is the best concrete reference:

- `grafana/public/app/plugins/datasource/loki/datasource.ts`
  - `getSupplementaryRequest()`
  - `getSupportedSupplementaryQueryTypes()`
  - `getSupplementaryQuery()`
  - `getLogRowContext()`, `getLogRowContextQuery()`, `getLogRowContextUi()`
  - `query()` live branching (`liveStreaming`)

Supporting files:

- `grafana/public/app/plugins/datasource/loki/LogContextProvider.ts`
- `grafana/public/app/plugins/datasource/loki/LiveStreams.ts`
- `grafana/public/app/plugins/datasource/loki/liveStreamsResultTransformer.ts`

## VictoriaLogs Mapping

Current mapping in this plugin:

- Supplementary support: [`src/datasource.ts`](../src/datasource.ts)
  - `getSupplementaryRequest()`
  - `getSupportedSupplementaryQueryTypes()`
  - `getSupplementaryQuery()`
- Log context support: [`src/datasource.ts`](../src/datasource.ts)
  - `getLogRowContext()`
- Live streaming: [`src/datasource.ts`](../src/datasource.ts)
  - `runLiveQueryThroughBackend()` via `getGrafanaLiveSrv().getDataStream(...)`

If you want parity with Loki context UX, add:

1. `getLogRowContextQuery(...)`
2. `getLogRowContextUi(...)`

## Reading Path

If you only have ~60 minutes:

1. `grafana/packages/grafana-data/src/types/logs.ts`
2. `grafana/public/app/features/explore/state/query.ts`
3. `grafana/public/app/features/explore/utils/supplementaryQueries.ts`
4. `grafana/public/app/features/explore/Logs/LogsContainer.tsx`
5. `grafana/public/app/features/explore/state/time.ts`
6. `grafana/public/app/plugins/datasource/loki/datasource.ts`
7. `grafana/public/app/plugins/datasource/loki/LogContextProvider.ts`
8. [`src/datasource.ts`](../src/datasource.ts)
