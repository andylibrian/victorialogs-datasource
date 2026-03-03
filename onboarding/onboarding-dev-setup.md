# Developer Setup — Developer Onboarding Guide

This document covers how to build, run, test, and release the VictoriaLogs Grafana datasource plugin.

**Prerequisites:** [System Overview](./onboarding-system-overview.md)

If you encounter unfamiliar terms, see the [Glossary](./glossary.md).

## Table of Contents

- [Prerequisites](#prerequisites)
- [Local Development Workflow](#local-development-workflow)
- [Build Commands](#build-commands)
- [Testing](#testing)
- [Linting and Type Checking](#linting-and-type-checking)
- [CI/CD Pipeline](#cicd-pipeline)
- [Release Process](#release-process)
- [Provisioning Configuration](#provisioning-configuration)
- [Project Configuration Files](#project-configuration-files)
- [Key Dependencies](#key-dependencies)
- [See Also](#see-also)

## Prerequisites

| Tool | Version | Purpose |
|------|---------|---------|
| Node.js | 20 (`.nvmrc`) for local dev, 24.11.1 in frontend CI checks | Frontend build |
| Yarn | 1.x | Package manager |
| Go | >= 1.25.7 (from go.mod) | Backend build |
| Docker & Docker Compose | Latest | Local Grafana environment |
| Make | Any | Build orchestration |

## Local Development Workflow

The typical dev loop:

```bash
# 1. Install frontend dependencies
yarn install

# 2. Start frontend in watch mode (rebuilds on file changes)
yarn dev

# 3. In another terminal, start local Grafana + VictoriaLogs
docker compose up
```

This gives you:
- **Grafana** at `http://localhost:3000` with the plugin pre-provisioned
- **VictoriaLogs** at `http://localhost:9428`
- **Live reload** — frontend changes are picked up automatically via webpack watch + LiveReload

### Docker Compose Services

**File:** [compose.yaml](../compose.yaml)

| Service | Image | Port | Purpose |
|---------|-------|------|---------|
| `victorialogs` | `victoriametrics/victoria-logs:latest` | 9428 | Log storage backend |
| `grafana` | Built from `.config/Dockerfile` | 3000 | Grafana with plugin mounted |

The Grafana container runs in development mode:
- `GF_DEFAULT_APP_MODE=development` (allows unsigned plugins)
- `GF_PLUGINS_ALLOW_LOADING_UNSIGNED_PLUGINS=victoriametrics-logs-datasource`
- Plugin source is mounted at `/var/lib/grafana/plugins/victoriametrics-logs-datasource`
- Provisioning files are mounted at `/etc/grafana/provisioning`

### Backend Changes

When you modify Go code, you need to rebuild the backend:

```bash
make vl-backend-plugin-build
```

Then restart the Grafana container to pick up the new binary.

## Build Commands

### Frontend

| Command | Purpose |
|---------|---------|
| `yarn install` | Install dependencies |
| `yarn dev` | Webpack watch mode (development) |
| `yarn build` | Webpack production build |

### Backend

| Command | Purpose |
|---------|---------|
| `make vl-backend-plugin-build` | Build backend via Mage (multi-platform) |

### Full Plugin

| Command | Purpose |
|---------|---------|
| `make vl-plugin-build` | Build both frontend and backend |
| `make vl-plugin-build-local` | Build locally without Docker |
| `make vl-plugin-pack` | Build and package as .tar.gz and .zip |

### Makefile Details

**File:** [Makefile](../Makefile)

The Makefile manages tools automatically:
- **Mage** (Go build system) — installed to `./bin/mage` if missing
- **golangci-lint** — installed to `./bin/golangci-lint` if missing
- Build info (git tag, branch, commit) is embedded into Go binaries via linker flags

The backend build uses **Mage** ([Magefile.go](../Magefile.go)) which compiles for multiple platforms:
- Linux (amd64, arm, arm64, s390x)
- Windows (amd64)
- macOS (amd64)
- FreeBSD (amd64)

Output goes to `plugins/victoriametrics-logs-datasource/`.

## Testing

### Frontend Tests (Jest)

```bash
yarn test                                  # Run all tests
yarn test -- --testPathPattern=<pattern>   # Run matching tests
yarn test:watch                            # Watch mode
yarn test:ci                               # CI mode (--maxWorkers 4)
yarn test:coverage                         # With coverage report
```

**Configuration:** [jest.config.js](../jest.config.js) extends Grafana's base config
- Transpiler: `@swc/jest` (fast compiled transpilation)
- Environment: `jest-environment-jsdom`
- Timezone: UTC (for consistent snapshots)
- CSS/SCSS mocked via `identity-obj-proxy`

Test files are co-located with source files as `*.test.ts` / `*.test.tsx`.

### Backend Tests (Go)

```bash
make golang-test        # go test ./pkg/...
make golang-test-race   # go test -race ./pkg/...
```

Test files are co-located with source files as `*_test.go`.

## Linting and Type Checking

### Frontend

```bash
yarn lint           # ESLint (cached)
yarn lint:fix       # ESLint with auto-fix
yarn typecheck      # TypeScript (tsc --noEmit)
```

**ESLint config:** [eslint.config.mjs](../eslint.config.mjs)
- Extends `@grafana/eslint-config`
- Key rules: no unused imports, 2-space indent, no console (except warn/error), emotion JSX import
- Plugins: jest, lodash, unused-imports, @emotion, import, @stylistic

**Prettier:** [.prettierrc.js](../.prettierrc.js) — extends Grafana base (2-space indent, semicolons, single quotes)

### Backend

```bash
make fmt            # gofmt
make vet            # go vet
make lint           # golangci-lint
make check-all      # All three above
```

**golangci-lint config:** [.golangci.yml](../.golangci.yml) — enables `revive` linter

## CI/CD Pipeline

**Directory:** [.github/workflows/](../.github/workflows/)

### Pull Request Checks

| Workflow | Trigger | Steps |
|----------|---------|-------|
| `pr-checks-frontend.yml` | TS/TSX, yarn.lock, package.json changes | `yarn test`, `yarn typecheck`, `yarn lint` |
| `pr-checks-backend.yml` | Go, vendor, pkg/ changes | golangci-lint |
| `pr-checks-plugin.yml` | Plugin structure changes | plugincheck2 validation |
| `pr-codeql-frontend.yml` | Frontend changes | CodeQL security scan |
| `pr-codeql-backend.yml` | Backend changes | CodeQL security scan |

### Post-Merge

| Workflow | Purpose |
|----------|---------|
| `pr-merge.yml` | Updates CHANGELOG, tags release version |
| `wait-for-publish.yaml` | Monitors plugin publishing |

### Release

| Workflow | Purpose |
|----------|---------|
| `release.yaml` | Manual dispatch: security scan (OSV), build, package, create GitHub release |

## Release Process

1. **Trigger**: Manual dispatch of `release.yaml` on main branch
2. **Security scan** (optional): OSV Scanner checks for HIGH/CRITICAL vulnerabilities
3. **Build**: Frontend (yarn build + plugin signing) and backend (Mage multi-platform)
4. **Package**: Creates `.tar.gz` and `.zip` with checksums
5. **Release**: Creates GitHub release with artifacts

Plugin signing requires `GRAFANA_ACCESS_POLICY_TOKEN` (only available in CI).

## Provisioning Configuration

**Directory:** [provisioning/](../provisioning/)

### Default Datasource

**File:** [provisioning/datasources/datasources.yml](../provisioning/datasources/datasources.yml)

```yaml
datasources:
  - name: VictoriaLogs
    type: victoriametrics-logs-datasource
    access: proxy
    url: http://victorialogs:9428
```

The `proxy` access mode means requests go through Grafana's backend (not directly from the browser).

### Playground Datasource

**File:** [provisioning/datasources/playground.datasources.yml](../provisioning/datasources/playground.datasources.yml)

Pre-configured to use the public VictoriaLogs playground at `https://play-vmlogs.victoriametrics.com`.

## Project Configuration Files

| File | Purpose |
|------|---------|
| `package.json` | Frontend dependencies and scripts |
| `tsconfig.json` | TypeScript config (strict, incremental, extends Grafana base) |
| `webpack.config.ts` | Webpack config (extends Grafana base, preserves Go binaries) |
| `jest.config.js` | Jest config (extends Grafana base, UTC timezone) |
| `eslint.config.mjs` | ESLint flat config |
| `.prettierrc.js` | Prettier config |
| `go.mod` / `go.sum` | Go dependencies |
| `Magefile.go` | Go build targets |
| `.golangci.yml` | Go linter config |
| `plugincheck.yaml` | Plugin validation config |

## Key Dependencies

### Frontend

| Package | Purpose |
|---------|---------|
| `@grafana/data`, `@grafana/runtime`, `@grafana/ui` | Grafana plugin SDK |
| `@grafana/lezer-logql` | LogsQL syntax tree parsing |
| `@grafana/plugin-ui` | Shared plugin UI components |
| `react` / `react-dom` | UI framework |
| `rxjs` | Observable-based query pipeline |
| `lodash` | Utility functions |

### Backend

| Package | Purpose |
|---------|---------|
| `grafana-plugin-sdk-go` | Grafana plugin framework |
| `valyala/fastjson` | Fast JSON parsing (NDJSON responses) |
| `VictoriaMetrics/metricsql` | Duration parsing |
| `klauspost/compress` | Compression (zstd, gzip) |
| `magefile/mage` | Build system |

## See Also

- [System Overview](./onboarding-system-overview.md) — architecture context
- [Frontend Architecture](./onboarding-frontend.md) — frontend deep dive
- [Backend Architecture](./onboarding-backend.md) — backend deep dive
