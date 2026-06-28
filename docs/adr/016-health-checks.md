# ADR-016: Health Checks

**Status:** Accepted **Date:** 2026-03-07 **Depends on:** ADR-006, ADR-015

## Context

Enterprise applications deployed to Kubernetes (and similar orchestrators) require liveness and readiness probes. Liveness probes detect deadlocked or unrecoverable processes; readiness probes gate traffic until all dependencies are available.

Credo's store package already tracks connection health via `Registry.HealthAll`. The health check system must integrate with this existing infrastructure while remaining usable for applications that have no stores.

Options considered:

1. **Adapt alexliesenfeld/health** — the original plan (CLAUDE.md Adapt table). However, the library's API surface is much larger than needed (interceptors, middleware chains, async checkers with caching). Credo needs only about a hundred lines of engine code.
2. **Write from scratch** — small scope, well-understood requirements, no attribution overhead.

## Decision

Write the health check engine from scratch. The engine is unexported in the root package (`healthEngine`); its public surface is a small set of methods on `App`, the same pattern as i18n (`internal/i18n/` + `app.UseI18n`). `internal/health/` holds only the module-internal seam through which the store integration contributes connection health — see [Store Integration](#store-integration).

### Engine (root package, unexported)

- `healthEngine` manages named liveness and readiness checks.
- Checks run concurrently via `sync.WaitGroup.Go` (Go 1.27+).
- Each check gets `context.WithTimeout(ctx, timeout)`.
- Pre-allocated result slice — each goroutine writes at its own index.
- No checks registered = "up" for liveness (server responding proves alive).
- Store health flows in through a module-internal DI seam (`internal/health.StoreFunc`), resolved lazily on each readiness check so the store package never imports the engine and store/`UseHealth` registration order does not matter.

### Public API (root package)

```go
// Configuration (zero-config when no args).
app.UseHealth(cfg ...HealthConfig)

// Register checks (UseHealth must be called first).
app.AddLivenessCheck(name string, checker HealthChecker)
app.AddReadinessCheck(name string, checker HealthChecker)
```

There is no public store-bridge method — store health is wired through the module-internal seam below, not by user code. See godoc for the authoritative signatures.

### HealthConfig

- `Enabled *bool` — nil defaults to true.
- `Liveness *bool` — nil defaults to true.
- `Readiness *bool` — nil defaults to true.
- `LivenessPath string` — default "/health".
- `ReadinessPath string` — default "/ready".
- `CheckTimeout time.Duration` — default 5s.
- `ExposeErrors bool` — default false. Failing readiness checks report only `"status": "down"`; the underlying error is written to the application log. Check errors often carry internal details (hostnames, connection targets) that must not reach unauthenticated probe endpoints. Opt in only when the endpoint is network-restricted.
- `Group *Group` — nil = app root. Routes registered on this group, inheriting its prefix and middleware.
- `LogRequests bool` — default false. Probe requests are excluded from the access log: `UseHealth` sets the `MetaAccessLog` route meta on `/health` and `/ready` to this value, so probe traffic (Kubernetes liveness/readiness polling, often once per second per replica) does not flood the log. The endpoints stay registered and responsive regardless, and the meta propagates to each route's auto-generated HEAD twin. Because the meta is set at the route level, `LogRequests: true` re-enables logging even when the probes sit under a `Group` that silenced access logging — a route-level meta value overrides its group's (see [ADR-010](010-middleware-architecture.md)).

`*bool` toggles allow distinguishing "not set" (use default) from explicit false.

### Response Format

```json
// GET /health — minimal liveness
{"status": "up"}

// GET /ready — detailed readiness
{"status": "up", "checks": {"postgres": {"status": "up", "latency": "1.234ms"}}}
```

Status codes: 200 for "up", 503 for "down".

### Graceful Shutdown

When the application begins graceful shutdown, `/ready` immediately returns 503 with `{"status": "shutting_down"}` — before in-flight requests are drained — so load balancers stop routing to the instance. Liveness (`/health`) stays 200: the process is alive and draining, and must not be killed mid-drain. See [ADR-006](006-application-lifecycle.md) for the full shutdown sequence.

### Store Integration

`store.Register[R]()` wires `Registry.HealthAll` into the readiness endpoint through a module-internal DI seam, with no user-facing API:

- The seam type `StoreFunc func(ctx) []StoreResult` lives in `internal/health/`, importable by both `store/` and the root package but reachable by neither user code nor (directly) the other side. A root-package callback type would instead force `store/` to import root-package health types; routing the seam through `internal/health/` breaks that coupling.
- On the first `store.Register`, the store package provides a `StoreFunc` (closing over the `Registry`) into the DI container via `ProvideValue`.
- The readiness handler resolves the `StoreFunc` lazily on each check, so a store registered after `UseHealth` is reflected automatically and a missing seam (no stores) simply yields no store entries.

## Consequences

**Positive:**

- Zero-config K8s probes: `app.UseHealth()` is all that's needed.
- Automatic store health: registering a store automatically appears in `/ready` without additional user code.
- Small implementation, no public store-bridge surface to keep stable.
- No external dependencies or attribution obligations.

**Negative:**

- No async/cached checks — all checks run on each request. Acceptable for the typical probe interval (10-30s). Can be added later if needed.
- No detailed liveness response body (only status). Keeps it minimal per K8s best practices.

**Risks:**

- Slow user checks without proper timeouts could delay probe responses. Mitigated by per-check `context.WithTimeout`.
