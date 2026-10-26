# ADR-016: Health Checks

**Status:** Accepted; optional/critical store readiness deferred **Date:** 2026-03-07 **Depends on:** ADR-006, ADR-015, ADR-024 **Related:** ADR-004, ADR-010, ADR-023 **Specification:** [Health spec](../specs/health.md) **Guide:** [Getting started: Health Checks](../guides/getting-started.md#health-checks), [Deployment guide](../guides/deployment.md#shutdown-and-readiness)

## Context

Enterprise applications deployed to Kubernetes (and similar orchestrators) require liveness and readiness probes. Liveness probes detect deadlocked or unrecoverable processes; readiness probes gate traffic until all dependencies are available.

Stores report typed connection health; the health system must include the stores an application registers, without resolving anything from the DI container per request, while remaining usable for applications that have none. Probes are polled often, by several callers at once, on endpoints that are usually unauthenticated: a hung dependency must not hold a probe past its deadline or accumulate a goroutine per request, diagnostic text must not leak connection targets, and probe traffic must not flood the access log. A draining instance must leave rotation before its drain without being killed for it.

## Decision

### Engine

Credo writes the health engine itself. The engine lives in `internal/health` (`Engine`, `Probe`, name validation, store-result normalization); the root package owns the endpoints, the public registration API and the response and logging policy — the same split as i18n (`internal/i18n` + `app.UseI18n`).

Each check owns a stable `Probe`: overlapping requests join one in-flight run instead of starting a callback per request, the per-check deadline is enforced even for a callback that ignores cancellation (at most one blocked callback is retained per check), and panics are isolated per check, so one store cannot abort its siblings or the readiness handler. Application, store and component checks run concurrently through this one runner. With no checks registered liveness is up: a responding server proves the process alive. The mechanics are in the [health spec](../specs/health.md#check-execution).

Rejected: adapting alexliesenfeld/health. Its API surface — interceptors, middleware chains, asynchronous checkers with caching — is much larger than Credo needs; Credo needs a small bounded runner, and writing it from scratch carries no attribution overhead.

### Public API (root package)

`app.UseHealth(cfg...)` mounts both probes with zero configuration, and `AddLivenessCheck`/`AddReadinessCheck` add the application's named checks. There is deliberately no public store-bridge method: store health comes from `store.Register`, not from user code, so the store integration has no public surface to keep stable. The signatures are in the [health spec](../specs/health.md#usehealth-and-healthconfig).

### HealthConfig

- Errors are masked by default: a failing entry reports only its status, and its cause goes to the application log. Causes often contain hostnames and connection targets that must not reach an unauthenticated probe endpoint; `ExposeErrors` opts in, for a network-restricted endpoint only.
- Probe traffic is silent in the access log by default: orchestrators poll the probes, often once per second per replica. `LogRequests` re-enables the records, overriding a group that silenced access logging ([ADR-010](010-middleware-architecture.md)).
- The probes can be mounted on a `Group`, so the group's prefix and middleware, an IP restriction for example, apply to them.
- `*bool` toggles distinguish "not set" (the default) from an explicit false.

The fields and their defaults are in the [health spec](../specs/health.md#usehealth-and-healthconfig).

### Response Format

Liveness answers its status only; readiness reports each entry. 200 means up and 503 down. Every store is critical: a `degraded` store is reported as such in its entry but blocks readiness like `down`. A per-store optional/critical policy is deferred to a separate API decision. The bodies are in the [health spec](../specs/health.md#response-format).

### Graceful Shutdown

When shutdown begins, `/ready` answers 503 `shutting_down` at once — before in-flight requests drain — so load balancers stop routing to the instance. Liveness (`/health`) keeps answering: the process is alive and draining, and must not be killed mid-drain. The sequence is in the [lifecycle spec](../specs/lifecycle.md#shutdown-in-tiers).

### Readiness from the Kernel's Registries

`/ready` reads what the App itself holds — the components the start walk built, the stores the start phase pinged, and the application's checks — and resolves nothing from the DI container per request. Each probe is built once: a component's when the App enters running, a store's when the start phase has pinged it (after every override, so an override is the value reported), an application check's at its registration. Registration order between `UseHealth`, stores and components therefore does not matter. A worker reports through its component's `Ready` like any other component ([ADR-023](023-worker-system.md)). The sources and their entry names are in the [health spec](../specs/health.md#readiness-aggregation).

Across the sources readiness fails closed:

- A store status outside `up`, `down` and `degraded` is reported down; the raw value is logged, never shown by default.
- A name that two sources report produces a down entry and 503 instead of one result silently overwriting the other in the JSON map.
- Only the typed `store.Health.Cause` is a cause; free-form `Details["error"]` values are never promoted to causes.
- Cause text is captured inside the probe's bounded run, so a cause whose `Error()` blocks or panics is subject to the same deadline and recovery; rendering and logging use only that captured text.

A store registration reaches the root through an internal Go seam between the root and `store`, not through a container binding.

Rejected: resolving a readiness seam from the container on each request — the service-locator access [ADR-004](004-dependency-injection-and-infra.md) rejects for applications; it would also have to treat every resolution error as "no entries", so a broken seam would make stores silently vanish from readiness instead of failing.

## Consequences

**Positive:**

- Zero-config K8s probes: `app.UseHealth()` is all that's needed.
- Automatic store health: a registered store appears in `/ready` without additional user code.
- Small implementation, no public store-bridge surface to keep stable.
- No external dependencies or attribution obligations.

**Negative:**

- No result cache — a completed probe is run again by the next request. Overlapping requests share only the current in-flight execution.
- No detailed liveness response body (only status). Keeps it minimal per K8s best practices.

**Risks:**

- Go cannot forcibly stop a non-cooperative callback. Credo returns at the configured deadline and prevents overlapping executions, but one callback goroutine can remain blocked indefinitely until application code releases it.
- Probe execution is detached from an individual HTTP waiter's cancellation so concurrent waiters can share it. A callback can therefore outlive that request (and, if non-cooperative, application drain); adapters must honor the finite probe context. The default five-second probe budget is shorter than the default thirty-second shutdown drain budget.
