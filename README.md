# Credo

**An all-in-one Go web framework for modern, enterprise-grade applications.**

[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8?style=flat&logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/credo-go/credo)](https://goreportcard.com/report/github.com/credo-go/credo)
[![GoDoc](https://pkg.go.dev/badge/github.com/credo-go/credo)](https://pkg.go.dev/github.com/credo-go/credo)

> **Status: Beta** -- Core APIs are stable enough for real application development.
> Breaking changes may still happen before v1, but should be documented with migration notes.
> Feedback from active pilot projects continues to shape the framework.

## What Credo Optimizes For

- **All-in-one**: router, DI, config, validation, observability, data access -- one cohesive framework.
- **Enterprise defaults**: Clean Architecture is the recommended path (CLI scaffolding + docs), but not enforced.
- **Errors are values**: handlers return `error`; centralized error handling renders RFC 7807 Problem Details.
- **Typed config snapshot**: config is loaded once at startup and injected as typed structs via DI.
- **Integrated-first, explicit boundaries**: framework infrastructure is wired by convention; app deps and typed config stay visible and override-friendly.
- **stdlib boundary compatibility**: `*credo.App` is an `http.Handler`; stdlib middleware can be adapted.

## Maturity by Area

Credo is **Beta** overall. Shipped packages are usable for real development; the
table below is explicit about what is solid, what is still experimental, and what
is on the roadmap.

| Area | Status |
|------|--------|
| Routing, Context, Handlers, Middleware | Beta |
| Dependency Injection (`Provide`/`Resolve`, `Infra`) | Beta |
| Configuration (`config`, typed snapshot) | Beta |
| Validation | Beta |
| Authentication (JWT / Basic / API key) | Beta |
| Internationalization (`i18n`) | Beta |
| Health checks | Beta |
| Data access — contracts (`store`) | Beta |
| Data access — Bun SQL wrapper + migrations (`store/sqldb`) | Beta |
| Background workers + cron (`worker`) | Beta |
| Pagination | Beta |
| Outbound HTTP client (`httpclient`) | Beta |
| Observability — structured logging (slog via `Infra`) | Beta |
| Observability — tracing (OpenTelemetry) | Experimental |
| Observability — metrics (Prometheus) | Experimental |
| pubsub (incl. in-process events) · grpc · websocket · openapi · admin server · CLI | Planned |

## Installation

```bash
go get github.com/credo-go/credo@latest
```

> **Requires Go 1.26+.** Credo tracks the current Go release to build on the modern
> standard library (e.g. `os.Root`, structured `log/slog`). It targets new and
> actively-maintained services rather than legacy codebases pinned to older
> toolchains — enterprise-grade in capability, modern in its baseline.

## Quick Start (Target API)

```go
package main

import (
    "log"

    "github.com/credo-go/credo"
)

func main() {
    // Create the app (auto-loads config from files, .env, and env vars).
    app, err := credo.New()
    if err != nil {
        log.Fatal(err)
    }

    app.GET("/", func(ctx *credo.Context) error {
        return ctx.Response().JSON(200, map[string]string{"message": "Hello, Credo!"})
    })

    // Server settings come from framework-internal server config.
    // Example: set `CREDO_SERVER__PORT=8080`.
    if err := app.Run(); err != nil {
        log.Fatal(err)
    }
}
```

For explicit config control, load it first with `config.Load(...)` and pass it
to `credo.New(credo.WithRawConfig(raw))`; this bypasses the default auto-load.

## Config: Typed Snapshot (Anti-Pattern-Free)

Credo positions config as a startup-time snapshot, not a runtime service.
String keys should appear only at module boundaries; everything beyond that boundary is typed.

```go
type DatabaseConfig struct {
    DSN string `credo:"dsn"`
}

func SetupDatabase(app *credo.App, store credo.RawConfig) error {
    var cfg DatabaseConfig
    if err := store.Unmarshal("databases.default", &cfg); err != nil {
        return err
    }
    return credo.ProvideValue(app, &cfg)
}
```

## Dependency Injection

Credo uses generics-based DI. Cross-cutting infrastructure is carried explicitly via
`credo.Infra`; business dependencies (including typed config) are normal constructor parameters.
Single-interface wiring uses `Alias[I, T]`, and ordered interface collections use
`BindMany[I, T]` + `ResolveAll[I]` or `[]I` constructor injection.

```go
func NewOrderService(infra credo.Infra, cfg *OrderConfig, repo OrderRepo) *OrderService {
    infra.Logger.Info("order service initialized")
    return &OrderService{cfg: cfg, repo: repo}
}
```

## Documentation

- User guide: `docs/guides/getting-started.md`
- User guide: `docs/guides/routing.md`
- User guide: `docs/guides/middleware.md`
- User guide: `docs/guides/proxy-trust.md`
- User guide: `docs/guides/dependency-injection.md`
- User guide: `docs/guides/data-access.md`
- User guide: `docs/guides/configuration.md`
- User guide: `docs/guides/localization.md`
- Architecture decisions: `docs/adr/`
- Detailed specs: `docs/specs/`

## Repository Layout (High Level)

- Root package (`github.com/credo-go/credo`): `App`, `Context`, routing, handler/middleware types
- `config/`: config loading; returns `credo.RawConfig`
- `middleware/`, `validation/`, `auth/`, `store/`, ...: feature packages
- `internal/`: private implementations (router radix tree, DI internals, etc.)
- `docs/`: ADRs (`docs/adr/`) and detailed specs (`docs/specs/`)

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines on how to contribute.

## Security

See [SECURITY.md](SECURITY.md) for vulnerability reporting instructions, and
[SECURITY-UPSTREAMS.md](SECURITY-UPSTREAMS.md) for how Credo tracks the upstream
projects its adapted code derives from. Reports are triaged by severity on a
best-effort basis — there is no fixed response-time guarantee.

## License

MIT -- see [LICENSE](LICENSE) for details.
