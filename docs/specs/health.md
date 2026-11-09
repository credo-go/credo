# Health Spec

**Status:** Implemented (beta); optional/critical store readiness deferred **Package:** root (`health.go`), `internal/health/` **ADR:** [ADR-016](../adr/016-health-checks.md) **Guide:** [Getting started: Health Checks](../guides/getting-started.md#health-checks), [Deployment guide](../guides/deployment.md#shutdown-and-readiness)

This spec is the contract for the liveness and readiness endpoints, their configuration, check registration and execution, what `/ready` aggregates, and the response format. What a component's `Ready` is and when the start walk builds it is in the [lifecycle spec](lifecycle.md#readiness); store registration is in the [store spec](store.md#registration); a worker's readiness conditions are in the [worker spec](worker.md#health-integration). The engine lives in `internal/health` (`Engine`, `Probe`, name validation, store-result normalization); the root package owns the endpoints, the registration API and the response and logging policy.

## Endpoints

| Endpoint | Default path | Route name | Answers |
| --- | --- | --- | --- |
| Liveness | `/health` | `credo.health` | 200 when every liveness check is up, or none is registered; 503 otherwise |
| Readiness | `/ready` | `credo.ready` | 200 when every [readiness entry](#readiness-aggregation) is up; 503 otherwise, and at once [during shutdown](#shutdown) |

Both are GET routes with the automatic HEAD twin, registered on the app root or on `HealthConfig.Group`. The bodies are under [Response Format](#response-format).

> **Pending v0.25.0 (W3):** not implemented yet; until W3 ships readiness answers 200 only when every entry is up.

Readiness also answers 200 with the status `degraded` when every gating entry is up and an [optional](#optional-readiness) one is not; a down gating entry answers 503.

## UseHealth and HealthConfig

```go
func (app *App) UseHealth(cfgs ...HealthConfig)

type HealthConfig struct {
    Enabled       *bool
    Liveness      *bool
    Readiness     *bool
    LivenessPath  string
    ReadinessPath string
    CheckTimeout  time.Duration
    ExposeErrors  bool
    Group         *Group
    LogRequests   bool
}
```

| Field | Default | Effect |
| --- | --- | --- |
| `Enabled` | nil = true | `false` registers nothing — no routes and no engine — and does not take the registration: a later `UseHealth` call is accepted, and `AddLivenessCheck`/`AddReadinessCheck` panic as if `UseHealth` had not been called |
| `Liveness` | nil = true | `false` leaves the liveness route unregistered; liveness checks can still be added |
| `Readiness` | nil = true | `false` leaves the readiness route unregistered; readiness checks can still be added |
| `LivenessPath` | `/health` | Path of the liveness route, relative to `Group` when set |
| `ReadinessPath` | `/ready` | Path of the readiness route, relative to `Group` when set |
| `CheckTimeout` | 5s (zero or negative selects it) | The per-check deadline ([Check execution](#check-execution)) |
| `ExposeErrors` | false | A failing readiness entry carries an `error` member with its cause text; by default the response carries only the status and the cause goes to the log |
| `Group` | nil = app root | The routes are registered on this group and inherit its prefix and middleware; a group of another App panics |
| `LogRequests` | false | The value of the `MetaAccessLog` route meta on both routes and their HEAD twins: probe traffic is silent in the access log by default, and `true` re-enables it even under a group that silenced access logging, because a route-level value overrides its group's ([HTTP features](http-features.md#observability-contract)) |

`*bool` toggles distinguish "not set" (the default) from an explicit false.

`UseHealth` takes zero configs for the defaults or one config, performs no I/O and returns nothing. It panics when called a second time (`credo: UseHealth already called`), after preparation or shutdown (`credo: App.UseHealth called after app was compiled or shut down`), or with a `Group` of another App. Its order relative to stores, components and workers does not matter: readiness reads what the start phase built. The [startup feature list](http-features.md#startup-visibility) shows `health` when at least one probe route is registered.

## Checks

```go
type HealthChecker interface {
    Check(ctx context.Context) error // nil = healthy
}

type HealthCheckFunc func(ctx context.Context) error

func (app *App) AddLivenessCheck(name string, checker HealthChecker)
func (app *App) AddReadinessCheck(name string, checker HealthChecker)
```

- Both panic when `UseHealth` has not registered the engine, when `checker` is nil, or when the name is invalid or duplicates another check of the same kind.
- A name must be non-empty, without leading or trailing whitespace, without control characters, and must not start with the reserved `credo.` prefix. Names are never normalized, so duplicate detection is an exact comparison. Store names use the same rules ([store spec](store.md#registration)).
- A nil error is up; any error is down.
- With no liveness checks, liveness is up: a responding server proves the process alive.

> **Pending v0.25.0 (W3):** not implemented yet; until W3 ships `AddReadinessCheck` takes no option, and the second bullet's store names come from `store.WithName`.

```go
func (app *App) AddLivenessCheck(name string, checker HealthChecker)
func (app *App) AddReadinessCheck(name string, checker HealthChecker, opts ...RegistrationOption)
```

- `AddReadinessCheck` accepts `credo.OptionalReadiness()`, its one option: the check is reported in `/ready` without gating readiness ([Optional readiness](#optional-readiness)). Any other option panics, naming it and the one option accepted. The variadic parameter keeps every existing call compiling.
- `AddLivenessCheck` takes no option: liveness decides a restart, and a check that cannot fail it decides nothing.
- There are no store names: a bound database's entry carries its component's name ([store spec](store.md#lifecycle-of-a-bound-database)).

## Check execution

- Every check owns a stable probe, built once: an application check's at its registration, a store's when the start phase has pinged it, a component's when the App enters running.
- A request runs every entry of its endpoint concurrently, so the response takes about as long as the slowest check, bounded by `CheckTimeout`.
- A request that arrives while a check's run is in flight joins that run instead of starting another. A completed result is never reused: the next request runs the check again.
- The check receives a context that carries the request's values but not its cancellation, with `CheckTimeout` as its deadline, so one cancelled request cannot invalidate a run that another request shares. Each waiting request also stops at its own `CheckTimeout` or when it is cancelled.
- At the deadline the entry is reported down with the context's error, on time, even when the callback ignores cancellation. Such a callback keeps its run attached until it returns: meanwhile later requests receive that timeout result at once and no second callback starts, so at most one blocked callback is retained per check.
- A panic in a check is recovered and reported as that entry's down result (`panic: …`); sibling checks and the handler continue.
- The cause text is captured once, inside the run, so a cause whose `Error()` blocks or panics is bounded the same way. The response and the log use only that captured text; the typed cause stays internal.

> **Pending v0.25.0 (W3):** not implemented yet; until W3 ships only store entries carry a latency, the first bullet's store probe exists, and a component's or application check's run measures none.

- Every check's run measures its `latency`, from the start of the run, which the probe already records, to its result. A check that outlives its deadline reports the time until the deadline. Every entry renders it with `Duration.String()`, and the failure log carries it.
- Probes are built for two sources only: an application check's at its registration, a component's — a bound database's among them — when the App enters running.

## Readiness Aggregation

> **Current until v0.25.0 (W3):** from here to the Pending marker, the section describes three sources, a store source among them, and every store as critical.

`/ready` aggregates three sources and resolves nothing from the DI container per request:

| Source | Entry name | Probe built | Up when |
| --- | --- | --- | --- |
| Components' `Ready` | The component's name: its `credo.ComponentName` name, else its type name; `worker:<name>` for a worker | Once, when the App enters running, from the values the start walk built | `Ready(ctx)` returns nil |
| Store registrations | The store's name (`store.WithName`, else the type name) | Once per store, when the start phase has built and pinged its binding — after `Finalize`, so after every override | Its `Health` status is up |
| Application checks | The `AddReadinessCheck` name | At the call | `Check(ctx)` returns nil |

- A component contributes when its type — the binding's type, or the type handed to `app.Manage` — shows `Ready` (`credo.Readier`); a borrowed value's `Ready` contributes too, although the App neither starts nor stops it. A component whose binding's type hides its value's `Ready` is refused, unless another registration of the same resource plans it ([lifecycle spec](lifecycle.md#component-types)).
- A worker contributes only when its registration sets a readiness condition ([worker spec](worker.md#health-integration)).
- Before the App has started there are no component or store entries.
- The three sources share one name space. A store whose name equals an application check's, and a component whose name equals an application check's, a store's or another component's, is not run: a down entry named `credo.store_name_conflict.<i>` or `credo.contributed_name_conflict.<i>` takes its place, where `<i>` is the entry's position in its source, and readiness answers 503. The application check keeps its name.
- An entry without a valid name or probe is reported the same way, as `credo.store_descriptor.<i>` or `credo.contributed_descriptor.<i>`, and a source whose snapshot panics as `credo.store_registry` or `credo.readiness_contributors`. The reserved `credo.` prefix keeps these names apart from application names.
- A store's `store.Health.Status` is matched case-insensitively and allowlisted to `up`, `down` and `degraded`; any other value is reported `down`, with the raw value in the logged cause, and so is an `up` that carries a cause. Only the typed `store.Health.Cause` is a cause: adapter `Details`, `Details["error"]` included, never reach the response or the cause.
- Readiness is up only when every application check, component and configuration entry is up and every store is `up`. Every store is critical: `degraded` is kept in the store's entry but blocks readiness like `down`. A per-store optional/critical policy is deferred ([ADR-016](../adr/016-health-checks.md#response-format)).

> **Pending v0.25.0 (W3):** not implemented yet; until W3 ships the aggregation above holds.

`/ready` aggregates two sources and resolves nothing from the DI container per request:

| Source | Entry name | Probe built | Up when |
| --- | --- | --- | --- |
| Components' `Ready` | The component's name: its `credo.ComponentName` name, else its binding's type (`*sqldb.DB`); `worker:<name>` for a worker | Once, when the App enters running, from the values the start walk built — after `Finalize`, so after every override | `Ready(ctx)` returns nil |
| Application checks | The `AddReadinessCheck` name | At the call | `Check(ctx)` returns nil |

- A component contributes when its registration plans a readiness entry: its type — the binding's type, or the type handed to `app.Manage` — shows `Ready` (`credo.Readier`), and it is a component or borrowed. A bound database is such a component ([store spec](store.md#lifecycle-of-a-bound-database)); a borrowed value's `Ready` contributes although the App neither starts nor stops it.
- **One resource, one entry.** The checks are built one per resource, not one per unit that answers `Ready`: a raw handle and a wrapper that embeds it give one entry, the resource's holder of record's ([lifecycle spec](lifecycle.md)), whose `Ready` is asked. The registrations that hold the resource agree on `credo.OptionalReadiness()` and on a `credo.ComponentName` name; the entry carries that name, else the holder of record's type name.
- A worker contributes only when its registration sets a readiness condition ([worker spec](worker.md#health-integration)).
- Before the App has started there are no component entries.
- The two sources share one name space. A component whose name equals an application check's or another component's is not run: a down entry named `credo.contributed_name_conflict.<i>` takes its place, where `<i>` is the entry's position in its source, and readiness answers 503. The application check keeps its name. An entry without a valid name or probe is reported as `credo.contributed_descriptor.<i>`, and a source whose snapshot panics as `credo.readiness_contributors`.
- An entry is `up` or `down`; there is no store-status allowlist and no store-result normalization, since a component's `Ready` returns an error or nil.

### Optional readiness

`credo.OptionalReadiness()` reports an entry in `/ready` without letting it gate readiness ([ADR-016](../adr/016-health-checks.md#optional-readiness)). It is given to the registration that plans the entry — `Provide`, `ProvideValue` or `Manage` — or to `AddReadinessCheck`.

- On a registration it applies wherever the registration plans a readiness entry, and panics elsewhere at registration: on a type without `Ready`, or on one that is neither a component nor borrowed. A borrowed value whose type shows `Ready` and no `Shutdown` takes it. A constructor handed to `Manage` with it whose value turns out to defer to another holder fails the start ([lifecycle spec](lifecycle.md)).
- An optional entry's check runs like any other, under the same deadline. The entry shows its own status — `down` when its check fails — and its latency, and a failure is logged like any failing check.
- The top-level status is computed from the gating entries, as without the option, then reported as `degraded` in place of `up` when an optional entry is down. A down gating entry still answers 503 `down`, and during shutdown `/ready` answers 503 `shutting_down` whatever the optional entries show.
- `Start` and `Shutdown` are unchanged: an unreachable database whose readiness is optional still fails the start unless it is opened with `sqldb.WithOptionalStart()` ([store spec](store.md#lifecycle-of-a-bound-database)).
- Gating is the default. Its cost, and why it stays the default, are in [ADR-016](../adr/016-health-checks.md#optional-readiness).

## Response Format

> **Current until v0.25.0 (W3):** from here to the Pending marker, the section describes the current bodies: a store entry's `degraded` status, latency on store entries only, and the store failure log.

Both endpoints answer JSON through `Response.JSON`.

| Endpoint and state | Status | Body |
| --- | --- | --- |
| Liveness up | 200 | `{"status":"up"}` |
| Liveness down | 503 | `{"status":"down"}` |
| Readiness | 200 when `status` is `up`, 503 when `down` | `{"status":…,"checks":{…}}`; `checks` is omitted when there are no entries |
| Readiness during shutdown | 503 | `{"status":"shutting_down"}` |

```json
{
  "status": "down",
  "checks": {
    "cache": {"status": "up"},
    "postgres": {"status": "degraded", "latency": "1.234ms"},
    "worker:reindex": {"status": "down"}
  }
}
```

- Application check and component entries carry `status`, `up` or `down`.
- Store entries carry `status` (`up`, `down` or `degraded`) and `latency`, in Go duration text: the latency the store reported, or the time waited for a probe that timed out.
- With `ExposeErrors`, a failing entry also carries `error`, its captured cause text. Liveness never carries per-check entries.

When the endpoint answers down, each failing entry is logged at Warn as `credo: health check failed` with `kind` (`liveness` or `readiness`), `check` and `error`; a store entry adds `source=store`, `status` and `latency`. The log carries the cause text whether or not `ExposeErrors` is set.

> **Pending v0.25.0 (W3):** not implemented yet; until W3 ships the bodies and the log above hold.

| Endpoint and state | Status | Body |
| --- | --- | --- |
| Liveness up | 200 | `{"status":"up"}` |
| Liveness down | 503 | `{"status":"down"}` |
| Readiness | 200 when `status` is `up` or `degraded`, 503 when `down` | `{"status":…,"checks":{…}}`; `checks` is omitted when there are no entries |
| Readiness during shutdown | 503 | `{"status":"shutting_down"}` |

```json
{
  "status": "degraded",
  "checks": {
    "cache": {"status": "down", "latency": "5s"},
    "*sqldb.DB": {"status": "up", "latency": "1.234ms"},
    "worker:reindex": {"status": "up", "latency": "12µs"}
  }
}
```

Here `cache` was added with `credo.OptionalReadiness()`; without the option its failure would answer 503 `down`.

- Every readiness entry carries `status`, `up` or `down`, and `latency`, in Go duration text: the time its run took, or the time until the deadline for a check that timed out.
- With `ExposeErrors`, a failing entry also carries `error`, its captured cause text. Liveness never carries per-check entries.

Each failing entry is logged at Warn as `credo: health check failed` with `kind` (`liveness` or `readiness`), `check`, `error` and `latency`, whenever it fails — a failing optional entry while `/ready` answers 200 `degraded` included. The log carries the cause text whether or not `ExposeErrors` is set.

## Shutdown

Once shutdown begins — the drain, or the rollback of a failed or interrupted start — `/ready` answers 503 `{"status":"shutting_down"}` at once, before the HTTP drain, and runs no check, so load balancers stop routing to the instance. `/health` keeps answering from its checks, so an orchestrator does not kill a process that is draining. The sequence is in the [lifecycle spec](lifecycle.md#shutdown-in-tiers).
