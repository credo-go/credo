# Health Spec

**Status:** Implemented (beta) **Package:** root (`health.go`), `internal/health/` **ADR:** [ADR-016](../adr/016-health-checks.md) **Guide:** [Getting started: Health Checks](../guides/getting-started.md#health-checks), [Deployment guide](../guides/deployment.md#shutdown-and-readiness)

> **Normative:** every section of this spec.

This spec is the contract for the liveness and readiness endpoints, their configuration, check registration and execution, what `/ready` aggregates, and the response format. What a component's `Ready` is and when the start walk builds it is in the [lifecycle spec](lifecycle.md#readiness); a bound database's readiness is in the [store spec](store.md#lifecycle-of-a-bound-database); a worker's readiness conditions are in the [worker spec](worker.md#health-integration). The engine lives in `internal/health` (`Engine`, `Probe`, name validation); the root package owns the endpoints, the registration API and the response and logging policy.

## Endpoints

| Endpoint | Default path | Route name | Answers |
| --- | --- | --- | --- |
| Liveness | `/health` | `credo.health` | 200 when every liveness check is up, or none is registered; 503 otherwise |
| Readiness | `/ready` | `credo.ready` | 200 when every gating [readiness entry](#readiness-aggregation) is up; 503 otherwise, and at once [during shutdown](#shutdown) |

Both are GET routes with the automatic HEAD twin, registered on the app root or on `HealthConfig.Group`. The bodies are under [Response Format](#response-format).

Readiness also answers 200 with the status `degraded` when every gating entry is up and an [optional](#optional-readiness) one is not; a down gating entry answers 503.

## UseHealth and HealthConfig

```go
func (app *App) UseHealth(cfgs ...HealthConfig)

type HealthConfig struct {
    Disabled         bool
    DisableLiveness  bool
    DisableReadiness bool
    LivenessPath     string
    ReadinessPath    string
    CheckTimeout     time.Duration
    ExposeErrors     bool
    Group            *Group
    LogRequests      bool
}
```

| Field | Default | Effect |
| --- | --- | --- |
| `Disabled` | false | `true` mounts no route. The call still takes the registration: a second `UseHealth` panics, and the checks, validated as usual, are never run |
| `DisableLiveness` | false | `true` leaves the liveness route unmounted; liveness checks are still accepted and never run |
| `DisableReadiness` | false | `true` leaves the readiness route unmounted; readiness checks are still accepted and never run |
| `LivenessPath` | `/health` | Path of the liveness route, relative to `Group` when set |
| `ReadinessPath` | `/ready` | Path of the readiness route, relative to `Group` when set |
| `CheckTimeout` | 5s (zero or negative selects it) | The per-check deadline ([Check execution](#check-execution)) |
| `ExposeErrors` | false | A failing readiness entry carries an `error` member with its cause text; by default the response carries only the status and the cause goes to the log |
| `Group` | nil = app root | The routes are registered on this group and inherit its prefix and middleware; a group of another App panics, whatever the three switches say |
| `LogRequests` | false | The value of the `MetaAccessLog` route meta on both routes and their HEAD twins: probe traffic is silent in the access log by default, and `true` re-enables it even under a group that silenced access logging, because a route-level value overrides its group's ([HTTP features](http-features.md#observability-contract)) |

The zero value mounts both endpoints at their default paths. Each switch is a plain `bool` named for what it turns off, so the zero value is the default and an application maps a configuration key to a switch directly.

`UseHealth` mounts the endpoints and collects nothing: the checks are registered with `AddLivenessCheck` and `AddReadinessCheck`, before the call or after it ([Checks](#checks)). It takes zero configs for the defaults or one config, performs no I/O and returns nothing. It panics when called a second time (`credo: UseHealth already called`), whether or not either call set `Disabled`; after preparation or shutdown (`credo: App.UseHealth called after app was compiled or shut down`); or with a `Group` of another App. The `Group` is checked before the switches are read, so the mistake shows in the deployment that turns health off as well:

```text
credo: UseHealth: HealthConfig.Group belongs to a different App; pass a group created on this App, or leave Group nil to mount the endpoints on the app root
```

Its order relative to checks, components and workers does not matter: an endpoint runs the checks that are registered when a request arrives, and readiness reads what the start phase built. The [startup feature list](http-features.md#startup-visibility) shows `health` when at least one probe route is registered.

## Checks

```go
func (app *App) AddLivenessCheck(name string, check func(context.Context) error)
func (app *App) AddReadinessCheck(name string, check func(context.Context) error, opts ...RegistrationOption)
```

- A check is a function, the contract of a component's `Ready(ctx) error`. A value with a `Check` method is registered as its method value: `app.AddReadinessCheck("payments", client.Check)`.
- Checks are collected whenever they are registered: before `UseHealth`, after it, or on an App that never calls it. `UseHealth` mounts the endpoints that run them, so whether a check is valid never depends on call order.
- A check whose endpoint is not mounted is accepted and never run: any check under `Disabled`, a liveness check under `DisableLiveness`, a readiness check under `DisableReadiness`. An application that maps a configuration key to one of the switches registers its checks unconditionally.
- An App that has checks and never called `UseHealth` logs one record at Warn when it enters running: `credo: health checks are registered but UseHealth was never called, so no endpoint runs them; call app.UseHealth() to mount /health and /ready`, with the counts in the attributes `checks`, `liveness` and `readiness`. A `UseHealth` call with `Disabled` set is a decision and is not warned about.
- Both panic when `check` is nil (`credo: AddReadinessCheck: the check function of "payments" is nil; pass a func(context.Context) error`), or when the name is invalid or duplicates another check of the same kind.
- A name must be non-empty, without leading or trailing whitespace, without control characters, and must not start with the reserved `credo.` prefix. Names are never normalized, so duplicate detection is an exact comparison. A `credo.ComponentName` name follows the same rules, and a bound database's entry carries its component's name ([store spec](store.md#lifecycle-of-a-bound-database)).
- A nil error is up; any error is down.
- With no liveness checks, liveness is up: a responding server proves the process alive.
- `AddReadinessCheck` accepts `credo.OptionalReadiness()`, its one option: the check is reported in `/ready` without gating readiness ([Optional readiness](#optional-readiness)). Any other option panics, naming it and the one option accepted: `credo: App.AddReadinessCheck does not accept credo.Ingress(); it accepts credo.OptionalReadiness()`. The variadic parameter keeps every existing call compiling.
- `AddLivenessCheck` takes no option: liveness decides a restart, and a check that cannot fail it decides nothing.

## Check execution

- Every check owns a stable probe, built once: an application check's at its registration, a component's — a bound database's among them — when the App enters running.
- A request runs every entry of its endpoint concurrently, so the response takes about as long as the slowest check, bounded by `CheckTimeout`.
- A request that arrives while a check's run is in flight joins that run instead of starting another. A completed result is never reused: the next request runs the check again.
- The check receives a context that carries the request's values but not its cancellation, with `CheckTimeout` as its deadline, so one cancelled request cannot invalidate a run that another request shares. Each waiting request also stops at its own `CheckTimeout` or when it is cancelled.
- At the deadline the entry is reported down with the context's error, on time, even when the callback ignores cancellation. Such a callback keeps its run attached until it returns: meanwhile later requests receive that timeout result at once and no second callback starts, so at most one blocked callback is retained per check.
- A panic in a check is recovered and reported as that entry's down result (`panic: …`); sibling checks and the handler continue.
- The cause text is captured once, inside the run, so a cause whose `Error()` blocks or panics is bounded the same way. The response and the log use only that captured text; the typed cause stays internal.
- Every check's run measures its `latency`, from the start of the run, which the probe already records, to its result. A check that outlives its deadline reports the time until the deadline. Every entry renders it with `Duration.String()`, and the failure log carries it.

## Readiness Aggregation

`/ready` aggregates two sources and resolves nothing from the DI container per request:

| Source | Entry name | Probe built | Up when |
| --- | --- | --- | --- |
| Components' `Ready` | The component's name: its `credo.ComponentName` name, else its binding's type (`*sqldb.DB`); `worker:<name>` for a worker | Once, when the App enters running, from the values the start walk built — after `Finalize`, so after every override | `Ready(ctx)` returns nil |
| Application checks | The `AddReadinessCheck` name | At the call | The check function returns nil |

- A component contributes when its registration plans a readiness entry: its type — the binding's type, or the type handed to `app.Manage` — shows `Ready` (`credo.Readier`), and it is a component or borrowed. A bound database is such a component ([store spec](store.md#lifecycle-of-a-bound-database)); a borrowed value's `Ready` contributes although the App neither starts nor stops it.
- **One resource, one entry.** The checks are built one per resource, not one per unit that answers `Ready`: a raw handle and a wrapper that embeds it give one entry, the resource's holder of record's ([lifecycle spec](lifecycle.md)), whose `Ready` is asked. The registrations that hold the resource agree on `credo.OptionalReadiness()` and on a `credo.ComponentName` name; the entry carries that name, else the holder of record's type name.
- A worker contributes only when its registration sets a readiness condition ([worker spec](worker.md#health-integration)).
- Before the App has started there are no component entries.
- The two sources share one name space. A component whose name equals an application check's or another component's is not run: a down entry named `credo.contributed_name_conflict.<i>` takes its place, where `<i>` is the entry's position in its source, and readiness answers 503. The application check keeps its name. An entry without a valid name or probe is reported as `credo.contributed_descriptor.<i>`, and a source whose snapshot panics as `credo.readiness_contributors`.
- An entry is `up` or `down`: a component's `Ready` and an application check return an error or nil, and no status is normalized.

### Optional readiness

`credo.OptionalReadiness()` reports an entry in `/ready` without letting it gate readiness ([ADR-016](../adr/016-health-checks.md#optional-readiness)). It is given to the registration that plans the entry — `Provide`, `ProvideValue` or `Manage` — or to `AddReadinessCheck`.

- On a registration it applies wherever the registration plans a readiness entry, and panics elsewhere at registration: on a type without `Ready`, or on one that is neither a component nor borrowed. A borrowed value whose type shows `Ready` and no `Shutdown` takes it. A constructor handed to `Manage` with it whose value turns out to defer to another holder fails the start ([lifecycle spec](lifecycle.md)).
- An optional entry's check runs like any other, under the same deadline. The entry shows its own status — `down` when its check fails — and its latency, and a failure is logged like any failing check.
- The top-level status is computed from the gating entries, as without the option, then reported as `degraded` in place of `up` when an optional entry is down. A down gating entry still answers 503 `down`, and during shutdown `/ready` answers 503 `shutting_down` whatever the optional entries show.
- `Start` and `Shutdown` are unchanged: an unreachable database whose readiness is optional still fails the start unless it is opened with `sqldb.WithOptionalStart()` ([store spec](store.md#lifecycle-of-a-bound-database)).
- Gating is the default. Its cost, and why it stays the default, are in [ADR-016](../adr/016-health-checks.md#optional-readiness).

## Response Format

Both endpoints answer JSON through `Response.JSON`.

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
