# VictoriaLogs Datasource Plugin — Onboarding Docs

This folder contains in-repo onboarding guides for engineers who need to understand and change the VictoriaLogs Grafana datasource plugin.

Use this file as the entry point and reading plan.

If you encounter unfamiliar terms (LogsQL, QueryType, derived fields, etc.), see the [Glossary](./glossary.md).

## What This Plugin Is

A hybrid TypeScript (frontend) + Go (backend) Grafana datasource plugin that connects Grafana to VictoriaLogs. The frontend provides query editing (code and visual modes), template variable support, and response enrichment. The backend proxies HTTP requests to VictoriaLogs, handles authentication, and parses responses into Grafana data frames.

## Recommended Reading Order

1. [`onboarding-system-overview.md`](./onboarding-system-overview.md) — Architecture, directory structure, component map
2. [`onboarding-query-flow.md`](./onboarding-query-flow.md) — End-to-end query path from UI to VictoriaLogs and back
3. [`onboarding-frontend.md`](./onboarding-frontend.md) — TypeScript: datasource class, query editor, transformers
4. [`onboarding-backend.md`](./onboarding-backend.md) — Go: query routing, URL building, response parsing
5. [`onboarding-logsql-handling.md`](./onboarding-logsql-handling.md) — LogsQL manipulation, variable interpolation, visual builder
6. [`onboarding-dev-setup.md`](./onboarding-dev-setup.md) — Build, test, run, release

**Why this order:**

- Start with the big picture: what this plugin does and how the pieces fit together.
- Then trace a query end-to-end to see the full data path.
- Then dive into the frontend and backend independently.
- Then learn the LogsQL handling details (parsing, variables, filters).
- Finish with practical dev setup so you can build and run locally.

## Platform Deep Dives (@grafana/)

After the core plugin docs, continue with these codebase deep dives:

1. [`onboarding-grafana-plugin-sdk.md`](./onboarding-grafana-plugin-sdk.md) — `DataSourceWithBackend`, plugin protocol, data frame transport
2. [`onboarding-grafana-explore-internals.md`](./onboarding-grafana-explore-internals.md) — supplementary queries, log context, and live streaming internals

## Role-Based Shortcuts

### Frontend-focused (TypeScript/React)

1. [`onboarding-system-overview.md`](./onboarding-system-overview.md)
2. [`onboarding-query-flow.md`](./onboarding-query-flow.md)
3. [`onboarding-frontend.md`](./onboarding-frontend.md)
4. [`onboarding-logsql-handling.md`](./onboarding-logsql-handling.md)
5. Key files:
   - `src/datasource.ts` — datasource class
   - `src/components/QueryEditor/QueryEditor.tsx` — query editor
   - `src/modifyQuery.ts` — query manipulation
   - `src/transformers/transformBackendResult.ts` — response enrichment

### Backend-focused (Go)

1. [`onboarding-system-overview.md`](./onboarding-system-overview.md)
2. [`onboarding-query-flow.md`](./onboarding-query-flow.md)
3. [`onboarding-backend.md`](./onboarding-backend.md)
4. Key files:
   - `pkg/plugin/datasource.go` — main datasource, routing, HTTP
   - `pkg/plugin/query.go` — URL construction
   - `pkg/plugin/response.go` — response parsing
   - `pkg/utils/utils.go` — time parsing, step calculation

### Full-stack / New Feature

1. [`onboarding-system-overview.md`](./onboarding-system-overview.md)
2. [`onboarding-query-flow.md`](./onboarding-query-flow.md)
3. [`onboarding-frontend.md`](./onboarding-frontend.md)
4. [`onboarding-backend.md`](./onboarding-backend.md)
5. [`onboarding-logsql-handling.md`](./onboarding-logsql-handling.md)
6. [`onboarding-dev-setup.md`](./onboarding-dev-setup.md)

## Local Dev Basics

```bash
# Frontend
yarn install            # install dependencies
yarn dev                # webpack watch mode
yarn test               # run Jest tests
yarn lint               # ESLint
yarn typecheck          # TypeScript type check

# Backend
make vl-backend-plugin-build   # build Go backend
make golang-test               # run Go tests
make check-all                 # fmt + vet + golangci-lint

# Full plugin
make vl-plugin-build           # build frontend + backend

# Local Grafana
docker compose up              # Grafana + VictoriaLogs at localhost:3000
```

## First-Week Learning Checklist

1. **Trace a query from memory.** Draw the path of an instant query from QueryEditor through datasource.ts → Go backend → VictoriaLogs → response parsing → transformers → Grafana UI.
2. **Identify QueryType routing.** Find where QueryType (`instant`/`stats`/`statsRange`/`hits`) controls URL building in the backend and response parsing dispatch.
3. **Explain variable interpolation.** Describe what happens when a multi-value template variable like `$host` appears in a LogsQL expression.
4. **Find the sort pipe logic.** Explain when `| sort by (_time)` is added to a query and when it is not.
5. **Run locally.** Execute `yarn dev` + `docker compose up`, open Grafana, and run a LogsQL query.
6. **Run tests.** Execute `yarn test` and `make golang-test` at least once.
7. **Read a response transformation.** Trace how an instant query response flows through `parseInstantResponse()` in Go and `processStreamsFrames()` in TypeScript.

## What To Learn Next

After finishing the onboarding docs, explore these areas:

1. **Grafana Plugin SDK** — understand `DataSourceWithBackend`, plugin protocol, data frames
2. **VictoriaLogs query API** — learn the full set of LogsQL pipes and functions
3. **Grafana Explore internals** — how supplementary queries, log context, and live streaming work
4. **Multi-tenancy patterns** — how AccountID/ProjectID headers scope data access
5. **Testing patterns** — how mocks and test utilities are structured in both frontend and backend

If you want these topics in guided form, use:
- [`onboarding-grafana-plugin-sdk.md`](./onboarding-grafana-plugin-sdk.md)
- [`onboarding-grafana-explore-internals.md`](./onboarding-grafana-explore-internals.md)
