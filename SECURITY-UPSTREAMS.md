# Upstream Security Provenance

Credo **adapts** (forks-and-owns) source code from several upstream projects rather than importing them as dependencies. Standard dependency scanners (Dependabot, `govulncheck`) only see the modules in `go.mod` / `go.sum` — they do **not** see adapted code. This file records the provenance of adapted code so that upstream security advisories can be triaged against Credo's copies.

> **Wrapped (imported) dependencies** — bun, golang-jwt, go-limiter, yaml, mapstructure, x/text, and the SQL drivers — are covered by `go.mod` and standard tooling, so they are not repeated here.

## Adapted sources

Exact upstream commits are **not** pinned: this code was adapted and has since diverged, so it is maintained as Credo's own. The "Adapted from" column records the upstream version/era referenced (per [NOTICES](NOTICES)) to help scope an advisory.

| Upstream | License | Adapted into | Adapted from |
| --- | --- | --- | --- |
| [Chi](https://github.com/go-chi/chi) | MIT | `internal/radix/`, root router (`mux.go`, `walk.go`, `routectx.go`, `credo.go`), parts of `middleware/` | tree.go / mux.go era 2024 |
| [httprouter](https://github.com/julienschmidt/httprouter) | BSD-3 | `internal/radix/` — algorithmic **reference only, no code copied** | — |
| [Echo](https://github.com/labstack/echo) | MIT | `context.go`, `request.go`, `response.go`, parts of `middleware/` | v4, 2024 |
| [Goyave](https://github.com/go-goyave/goyave) | MIT | `route.go`, `group.go`, parts of `validation/`, `store/` | v5/v6, 2024 |
| [samber/do](https://github.com/samber/do) | MIT | `internal/di/` | 2022 |
| [koanf](https://github.com/knadh/koanf) | MIT | `config/` | v2 |
| [go-i18n](https://github.com/nicksnyder/go-i18n) | MIT | `internal/i18n/` | v2 |
| [ozzo-validation](https://github.com/go-ozzo/ozzo-validation) | MIT | `validation/rule.go` | 2016 |
| [GoFr](https://github.com/gofr-dev/gofr) | Apache-2.0 | `store/health.go`, `store/lifecycle.go` | 2021 |
| [robfig/cron](https://github.com/robfig/cron) | MIT | `worker/schedule.go` | v3 |

See [NOTICES](NOTICES) for the full per-file breakdown and copyright notices.

## How adapted code is monitored

1. The scheduled [`upstream-watch`](.github/workflows/upstream-watch.yml) workflow runs `govulncheck` (covering the wrapped dependencies and the standard library) and prints the upstream list above as a monthly manual-review reminder.
2. When an upstream advisory is published, the maintainer checks — using the NOTICES per-file map — whether the affected logic was adapted into Credo, and patches Credo's copy if so.

Advisories are triaged by severity on a best-effort basis. There is **no fixed response-time guarantee** (see [SECURITY.md](SECURITY.md)).
