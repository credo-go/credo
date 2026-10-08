# ADR-016: Health Checks

**Status:** Accepted; readiness from the kernel's registries implemented in v0.24.0 **Date:** 2026-03-07 **Depends on:** ADR-006, ADR-015, ADR-024

## Context

Enterprise applications deployed to Kubernetes (and similar orchestrators) require liveness and readiness probes. Liveness probes detect deadlocked or unrecoverable processes; readiness probes gate traffic until all dependencies are available.

Credo's store package already tracked connection health, then through a `Registry` with a `HealthAll` view. The health check system must integrate with the stores an application registers while remaining usable for applications that have none.

Options considered:

1. **Adapt alexliesenfeld/health** — the original plan. However, the library's API surface is much larger than needed (interceptors, middleware chains, async checkers with caching). Credo needs only about a hundred lines of engine code.
2. **Write from scratch** — small scope, well-understood requirements, no attribution overhead.

## Decision

Write the health check engine from scratch. The engine is unexported in the root package (`healthEngine`); its public surface is a small set of methods on `App`, the same pattern as i18n (`internal/i18n/` + `app.UseI18n`). `internal/health/` holds the stable bounded `Probe` primitive and the check types through which stores and components contribute per-entry checks — see [Readiness from the kernel's registries](#readiness-from-the-kernels-registries).

### Engine (root package, unexported)

- `healthEngine` manages named liveness and readiness checks.
- Named and store checks run together through one concurrent runner.
- Every registered check owns a stable module-internal `Probe`. Concurrent
  readiness requests join the same in-flight execution instead of launching a
  callback per request.
- `Probe.Run` selects between an immutable flight result and the caller's
  cancellation. The callback gets a finite timeout context. A callback that
  ignores it may remain blocked, but the timeout result is published on time
  and the flight stays attached until the callback exits, bounding the leak to
  one callback (plus its coordinator) per check.
- Callback workers never write response slices. Collectors write one dedicated
  result index after receiving the immutable result, so late completion cannot
  mutate an already-published timeout response or race with rendering.
- Panics are recovered per check; one store cannot abort sibling checks or the
  readiness handler.
- No checks registered = "up" for liveness (server responding proves alive).
- Store, component and application checks are the three readiness sources described under [Readiness from the kernel's registries](#readiness-from-the-kernels-registries).

### Public API (root package)

```go
// Configuration (zero-config when no args).
app.UseHealth(cfg ...HealthConfig)

// Register checks (UseHealth must be called first).
app.AddLivenessCheck(name string, checker HealthChecker)
app.AddReadinessCheck(name string, checker HealthChecker)
```

There is no public store-bridge method — store health comes from `store.Register`, not from user code. See godoc for the authoritative signatures.

### HealthConfig

- `Enabled *bool` — nil defaults to true.
- `Liveness *bool` — nil defaults to true.
- `Readiness *bool` — nil defaults to true.
- `LivenessPath string` — default "/health".
- `ReadinessPath string` — default "/ready".
- `CheckTimeout time.Duration` — default 5s.
- `ExposeErrors bool` — default false. Failing named and store checks report only
  their bounded status; the typed cause is written to the application log.
  Causes often contain hostnames and connection targets that must not reach an
  unauthenticated probe endpoint. Opt in only when the endpoint is
  network-restricted.
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

Status codes: 200 for "up", 503 for "down". All stores are critical in the
current API. `StatusDegraded` is preserved in the per-store entry but is
readiness-blocking, so the top-level status is `"down"` and the response is 503.
Optional/critical configuration is deferred to a separate API decision.

### Graceful Shutdown

When the application begins graceful shutdown, `/ready` immediately returns 503 with `{"status": "shutting_down"}` — before in-flight requests are drained — so load balancers stop routing to the instance. Liveness (`/health`) stays 200: the process is alive and draining, and must not be killed mid-drain. See [ADR-006](006-application-lifecycle.md) for the full shutdown sequence.

### Readiness from the Kernel's Registries

`/ready` reads what the kernel already holds, and resolves nothing from the DI container per request. Its sources are:

- **components' `Ready`** — each component whose binding's type shows the `Ready` capability (`credo.Readier`), under the component's name (`credo.Named`, the type name by default). A nil error is up and an error is down. The probes are built once by the start walk from the values it built. A borrowed value keeps its readiness contribution though the App neither starts nor shuts it down ([ADR-024](024-lifecycle-components.md)). A worker whose registration sets a readiness condition reports here, as its component `worker:<name>` ([ADR-023](023-worker-system.md)); a worker without one has no `Ready` and contributes nothing.
- **the store registry** — each registered store's typed `Health`, under the store's name ([ADR-015](015-data-access.md#registration)). Its probe is built once, when the start phase has built the store's binding and pinged it, so an override of the binding is the value reported. Before the App has started there are no store entries.
- **the application's checks** — those added with `AddReadinessCheck`.

The three sources share the engine described above: each entry owns a stable `Probe`, runs under the per-check deadline with panics isolated, and overlapping requests join one in-flight execution. The registries are filled at registration and by the start phase, so registration order between `UseHealth`, stores and components does not matter. The rules that apply across the sources:

- Store status is allowlisted to `up`, `down`, or `degraded`. Unknown adapter values fail closed as `down`; the raw value is logged but remains masked from the default HTTP response.
- `store.Health.Cause error` is the typed diagnostic source. It and the module-internal result cause are marked `json:"-"`; arbitrary `Health.Details["error"]` values are never promoted to causes. Causes are masked unless `ExposeErrors`.
- Cause text is captured once inside the Probe worker. A custom `Error()` that blocks or panics is therefore subject to the same timeout/recovery boundary; HTTP rendering and slog use only the immutable captured string, while the typed cause remains available internally for `errors.Is/As`.
- A name that two sources report — a custom readiness check and a store, for example — produces an explicit synthetic down result and 503 instead of silently overwriting one result in the JSON map.

The store registry reaches the root through an internal Go seam between the root and `store`, not through a container binding. The former `internal/health.StoreFunc` DI seam — resolved on every readiness request and installed with `App.Replace` by each `store.Register` — is gone, and so is the worker readiness seam that the worker pool installed the same way: a worker reports through its component's `Ready`.

During the drain `/ready` returns 503 `shutting_down` as described under Graceful Shutdown, before any component stops; `/health` stays 200.

Rejected: resolving a readiness seam from the container on each request — the service-locator access [ADR-004](004-dependency-injection-and-infra.md) rejects for applications; it also had to treat every resolution error as "no entries", so a broken seam made stores silently vanish from readiness instead of failing.

## Consequences

**Positive:**

- Zero-config K8s probes: `app.UseHealth()` is all that's needed.
- Automatic store health: registering a store automatically appears in `/ready` without additional user code.
- Small implementation, no public store-bridge surface to keep stable.
- No external dependencies or attribution obligations.

**Negative:**

- No result cache — a completed probe is run again by the next request.
  Overlapping requests share only the current in-flight execution.
- No detailed liveness response body (only status). Keeps it minimal per K8s best practices.

**Risks:**

- Go cannot forcibly stop a non-cooperative callback. Credo returns at the
  configured deadline and prevents overlapping executions, but one callback
  goroutine can remain blocked indefinitely until application code releases it.
- Probe execution is detached from an individual HTTP waiter's cancellation so
  concurrent waiters can share it. A callback can therefore outlive that
  request (and, if non-cooperative, application drain); adapters must honor the
  finite probe context. The default five-second probe budget is shorter than
  the default thirty-second shutdown drain budget.
