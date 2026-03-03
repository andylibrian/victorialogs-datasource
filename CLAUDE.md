# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

VictoriaLogs datasource plugin for Grafana. A hybrid TypeScript (frontend) + Go (backend) Grafana datasource plugin that queries VictoriaLogs using LogsQL.

## Build & Development Commands

### Frontend (TypeScript/React)
- `yarn install` — install dependencies
- `yarn dev` — webpack dev mode with watch
- `yarn build` — production build
- `yarn test` — run Jest tests
- `yarn test -- --testPathPattern=<pattern>` — run a single test file
- `yarn lint` — ESLint
- `yarn lint:fix` — ESLint with auto-fix
- `yarn typecheck` — TypeScript type checking (`tsc --noEmit`)

### Backend (Go)
- `make vl-backend-plugin-build` — build backend binaries via Mage
- `make golang-test` — run Go tests (`go test ./pkg/...`)
- `make golang-test-race` — Go tests with race detector
- `make check-all` — gofmt + go vet + golangci-lint
- `make vl-plugin-build` — build both frontend and backend

### Local Grafana
- `docker compose up` — run Grafana with the plugin (provisioning in `provisioning/`)

## Architecture

### Frontend (`src/`)
- **Entry point**: `module.ts` registers the plugin with Grafana's `DataSourcePlugin`, wiring up `VictoriaLogsDatasource`, `QueryEditorByApp`, and `ConfigEditor`.
- **Datasource**: `datasource.ts` — `VictoriaLogsDatasource` extends `DataSourceWithBackend`. Handles query execution, template variable interpolation, log context, supplementary queries (log volume/sample), ad-hoc filters, and multi-tenancy headers. Queries are sent to the backend plugin which proxies to VictoriaLogs.
- **Query Editor**: `components/QueryEditor/` — supports two modes via `QueryEditorMode`: `Code` (Monaco-based LogsQL editor) and `Builder` (visual query builder in `QueryBuilder/`). `QueryEditorByApp.tsx` selects the appropriate editor variant based on Grafana app context (Explore vs Dashboard vs Alerting).
- **LogsQL parsing**: `LogsQL/`, `modifyQuery.ts`, `parsing.ts`, `parsingUtils.ts` — utilities for programmatically modifying LogsQL expressions (adding/removing filters, operators, sort pipes). Uses `@grafana/lezer-logql` for syntax tree parsing.
- **Configuration**: `configuration/` — datasource settings UI: derived fields, log level rules, query limits, tenant settings, alerting settings.
- **Transformers**: `transformers/` — post-processes backend responses: frame processing, derived field extraction, log level detection.
- **Variable support**: `variableSupport/` — implements Grafana template variable queries (field names, field values).
- **Types**: `types.ts` — core type definitions including `Query`, `Options`, `QueryType` (Instant/Stats/StatsRange/Hits), `QueryEditorMode`.

### Backend (`pkg/`)
- **Plugin entry**: `main.go` creates the Grafana plugin instance.
- **Datasource**: `pkg/plugin/datasource.go` — implements `backend.QueryDataHandler`. Routes queries to VictoriaLogs HTTP endpoints based on `QueryType`.
- **Query handling**: `pkg/plugin/query.go` — builds HTTP requests to VictoriaLogs API endpoints (`/select/logsql/query`, `/select/logsql/stats_query`, `/select/logsql/stats_query_range`, `/select/logsql/hits`).
- **Response parsing**: `pkg/plugin/response.go` — parses VictoriaLogs JSON responses into Grafana data frames.

### Query Flow
1. Grafana UI → `QueryEditor` builds a `Query` with `expr` (LogsQL), `queryType`, `step`, etc.
2. `VictoriaLogsDatasource.query()` applies template variables, sort pipes, and timezone offsets, then sends to the backend via `DataSourceWithBackend`.
3. Backend `datasource.go` routes to the appropriate VictoriaLogs HTTP endpoint based on `queryType`.
4. Response flows back through `response.go` → frontend `transformers/` for frame processing and derived field extraction.

## Code Style
- Frontend: ESLint + Prettier — 2-space indent, semicolons, single quotes, no unused imports.
- Backend: `gofmt` formatted, `golangci-lint` enforced.
- Tests live next to source files as `*.test.ts` (frontend) and `*_test.go` (backend).

## Commit Style
Short imperative subjects, often lowercase, optionally scoped (`fix`, `docs:`, `ci:`), with issue refs like `(#597)`.
