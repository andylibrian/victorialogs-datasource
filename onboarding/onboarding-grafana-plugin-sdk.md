# Grafana Plugin SDK Deep Dive — Developer Onboarding Guide

This guide maps the Grafana Plugin SDK internals that matter most for this datasource:

- `DataSourceWithBackend` (frontend runtime wrapper)
- plugin protocol (Grafana core <-> backend plugin process)
- data frames (frontend model, backend model, Arrow/JSON transport)

**Prerequisites:** [System Overview](./onboarding-system-overview.md), [Frontend](./onboarding-frontend.md), [Backend](./onboarding-backend.md)

## Table of Contents

- [Frontend SDK Surface](#frontend-sdk-surface)
- [Plugin Protocol End-to-End](#plugin-protocol-end-to-end)
- [Data Frames End-to-End](#data-frames-end-to-end)
- [VictoriaLogs Mapping](#victorialogs-mapping)
- [Reading Path](#reading-path)

## Frontend SDK Surface

### Plugin registration (`DataSourcePlugin`)

`DataSourcePlugin` is the frontend registration point:

- `grafana/packages/grafana-data/src/types/datasource.ts`
  - `class DataSourcePlugin`
  - `.setQueryEditor()`, `.setConfigEditor()`, etc.

This is what your plugin does in [`src/module.ts`](../src/module.ts):

```ts
export const plugin = new DataSourcePlugin(VictoriaLogsDatasource)
  .setQueryEditor(QueryEditorByApp)
  .setConfigEditor(ConfigEditor);
```

### Runtime wrapper (`DataSourceWithBackend`)

Most backend datasource plugins extend `DataSourceWithBackend`:

- `grafana/packages/grafana-runtime/src/utils/DataSourceWithBackend.ts`
  - `query()` builds `POST /api/ds/query?ds_type=...`
  - `getResource()` and `postResource()` map to `/api/datasources/uid/:uid/resources/...`
  - `callHealthCheck()` maps to `/api/datasources/uid/:uid/health`
  - streaming bridge: `toStreamingDataResponse()` + `standardStreamOptionsProvider`

Response decoding path:

- `grafana/packages/grafana-runtime/src/utils/queryResponse.ts`
  - `toDataQueryResponse()` converts backend payloads into frontend `DataFrame[]`

## Plugin Protocol End-to-End

### Protocol contract

Primary protocol definitions live in:

- `grafana-plugin-sdk-go/proto/backend.proto`
  - `service Data` (`QueryData`, `QueryChunkedData`)
  - `service Resource` (`CallResource`)
  - `service Diagnostics` (`CheckHealth`)
  - `service Stream` (`SubscribeStream`, `RunStream`, `PublishStream`)
  - `message PluginContext` / `DataSourceInstanceSettings`

### Grafana core (host) side

Core request path for `DataSourceWithBackend.query()`:

1. `grafana/pkg/api/ds_query.go` -> `QueryMetricsV2`
2. `grafana/pkg/services/query/query.go` -> `ServiceImpl.QueryData`
3. `grafana/pkg/plugins/manager/client/client.go` -> `Service.QueryData`
4. `grafana/pkg/plugins/backendplugin/grpcplugin/client_v2.go` -> gRPC call to plugin process

Plugin process client bootstrapping:

- `grafana/pkg/plugins/backendplugin/grpcplugin/client.go`
  - handshake config (protocol version + magic cookie)
  - plugin set (`diagnostics`, `resource`, `data`, `stream`, ...)
- `grafana/pkg/plugins/backendplugin/grpcplugin/grpc_plugin.go`
  - process lifecycle (`Start`, `Stop`) and dispatch to `ClientV2`

### Plugin SDK (plugin process) side

SDK server setup:

- `grafana-plugin-sdk-go/backend/serve.go`
- `grafana-plugin-sdk-go/backend/grpcplugin/serve.go`

Datasource auto-instance management:

- `grafana-plugin-sdk-go/backend/datasource/manage.go`

In this plugin:

- [`pkg/main.go`](../pkg/main.go) -> `backend.Manage(..., backend.ServeOpts{...})`

## Data Frames End-to-End

### Frontend model

Frontend frame contracts:

- `grafana/packages/grafana-data/src/types/dataFrame.ts`
  - `FieldType`, `Field`, `DataFrame`
- `grafana/packages/grafana-data/src/dataframe/DataFrameJSON.ts`
  - `dataFrameFromJSON()`, `dataFrameToJSON()`

### Backend model

Backend frame contracts:

- `grafana-plugin-sdk-go/backend/data.go`
  - `QueryDataResponse`, `DataResponse`, `DataFrameFormat`
- `grafana-plugin-sdk-go/data/frame.go`
  - `type Frame` and field vectors

Transport/encoding:

- `grafana-plugin-sdk-go/backend/convert_to_protobuf.go`
  - `QueryDataResponse()` -> Arrow or JSON bytes
- `grafana-plugin-sdk-go/backend/convert_from_protobuf.go`
  - `QueryDataResponse()` -> decode Arrow/JSON back to frames
- `grafana-plugin-sdk-go/data/arrow.go`
  - `Frame.MarshalArrow()`, `UnmarshalArrowFrame()`

Important detail: `DataSourceWithBackend.query()` can switch to live stream mode when a returned frame has `meta.channel` (`toStreamingDataResponse()`).

## VictoriaLogs Mapping

Use these files side-by-side with the Grafana files above:

- Frontend wrapper usage: [`src/datasource.ts`](../src/datasource.ts)
  - `class VictoriaLogsDatasource extends DataSourceWithBackend`
  - `runQuery()` uses `super.query()` + `transformBackendResult()`
- Backend entrypoint: [`pkg/main.go`](../pkg/main.go)
  - `backend.Manage(...)` with query/resource/health/stream handlers
- Backend query/stream handlers: [`pkg/plugin/datasource.go`](../pkg/plugin/datasource.go)

## Reading Path

If you only have 60-90 minutes:

1. `grafana/packages/grafana-runtime/src/utils/DataSourceWithBackend.ts`
2. `grafana/pkg/api/ds_query.go`
3. `grafana/pkg/services/query/query.go`
4. `grafana/pkg/plugins/backendplugin/grpcplugin/client.go`
5. `grafana/pkg/plugins/backendplugin/grpcplugin/client_v2.go`
6. `grafana-plugin-sdk-go/proto/backend.proto`
7. `grafana-plugin-sdk-go/backend/serve.go`
8. `grafana-plugin-sdk-go/backend/convert_to_protobuf.go`
9. `grafana-plugin-sdk-go/data/frame.go`
10. [`src/datasource.ts`](../src/datasource.ts) and [`pkg/main.go`](../pkg/main.go)
