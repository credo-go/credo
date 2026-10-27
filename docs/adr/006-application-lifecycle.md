# ADR-006: Application Lifecycle

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** ADR-001 **Related:** ADR-020, ADR-022, ADR-024 **Specification:** [Lifecycle spec](../specs/lifecycle.md) **Guide:** [Deployment guide](../guides/deployment.md)

## Context

An enterprise framework (ADR-001) must provide a well-defined application lifecycle: startup, runtime, and graceful shutdown. Background services (workers, pub/sub subscribers, gRPC servers) need a signal to stop accepting work. In-flight HTTP requests need time to complete. Resources (DB connections, caches, file handles) must be released in a deterministic order, after the work that uses them.

Go's stdlib `*http.Server` provides `Shutdown(ctx)` for the HTTP drain but has no lifecycle state, no start phase and no ordered teardown of the resources requests use. Credo fills this gap.

## Decision

### Lifecycle State Machine

```
building → starting → running → stopping → stopped      (Run, RunContext, ServeContext, App.Start)
building → stopping → stopped                           (bootstrap Shutdown of a never-run App)

Failure and interruption:
  prepare / preflight / listen error   → building   (nothing started; a failed preparation is
                                                     stored and never retried)
  start failure (Start, start hook,
    constructor in the start walk)     → stopped    (rollback of what was built, terminal)
  shutdown requested while starting    → stopped    (start interrupted, rollback, terminal)
  serve error (after running)          → stopped    (full drain, terminal)
```

`starting` is the **start phase**: preparation, listener bind and the start walk, before the listener accepts; `App.Start(ctx)` enters the same phase without a listener, for an App its owner serves through `ServeHTTP`. A **pre-session** failure (preparation, TLS preflight, listener bind) starts nothing, so it rolls back to `building` and the App may run again; a stored preparation failure then permits only bootstrap cleanup, not a retry of the frozen plan. A start failure, an interrupted start and a serve failure after `running` are terminal: a component that started may already have produced externally visible side effects, so a session that began never returns to `building`, and retry means a new App. The [lifecycle spec](../specs/lifecycle.md#state-machine) states every transition.

| State | Meaning |
| --- | --- |
| `building` | Initial. Route/middleware registration allowed until preparation. `Shutdown` accepted (bootstrap teardown) |
| `starting` | The start phase: preparation, listener bind, the start walk. A shutdown request interrupts it and rolls back |
| `running` | Start phase complete; a managed listener accepts. Registration frozen. Shutdown allowed |
| `stopping` | Draining: HTTP beside the ingress tier, then the internal tier |
| `stopped` | Fully stopped |

State is stored as `atomic.Uint32` with `CompareAndSwap` transitions — no mutex on the hot path.

### API

The entry points are `Run` (signal-aware), `RunContext` (caller-driven cancellation), `ServeContext` (a caller-provided `net.Listener`), `App.Start` (the start phase without a listener, for an App served through `ServeHTTP`) and `Shutdown`; the [lifecycle spec](../specs/lifecycle.md#api) lists them with the hooks and the construction options.

`Run` and `RunContext` serve plaintext or TLS from the same call: there is no separate TLS serve method. Whether a request is served over HTTPS is decided by configuration (see [TLS](#tls)), which is orthogonal to the control-flow choice (signal-aware vs caller-driven) those methods actually encode.

### TLS

TLS is **server configuration, not a serve-method variant**: `Run` and `RunContext` serve HTTPS when a certificate source is configured and plaintext otherwise. Transport (plain or TLS) is orthogonal to control flow (signal or context), and TLS belongs with host and port in configuration. Rejected: separate `RunTLS`/`RunTLSContext` methods, which mirror the stdlib's `ListenAndServeTLS` but double every entry point.

- **Precedence, not a conflict error.** `WithTLSConfig` > `WithTLSFiles` > `server.tls.*` > plaintext, each a whole-source override, never field-merged. Rejected: erroring when two sources are set — precedence lets an option cleanly override a config-file default, the common case, and avoids a brittle "set exactly one" rule.
- **Validated once, at preflight.** A bad certificate fails before anything starts, with the same rollback to `building` as a listen error. Each explicit option records that it was set, so an empty or nil explicit source fails loud rather than falling through to a lower-precedence source or to plaintext: the security-sensitive failure — accidentally serving plaintext — is never reached silently.
- **`ServeContext` is TLS-exempt**: it serves the listener it is handed exactly as given; HTTPS on a custom listener wraps it with `tls.NewListener`.
- **A redirect-only listener, not dual-serve.** `WithHTTPRedirect(addr)` adds a second, plaintext listener whose only job is the permanent redirect to HTTPS (301 for GET and HEAD, 308 otherwise, as the trailing-slash redirect does). Its runtime failure tears the App down like the main listener's, so a requested redirect never silently dies while the App reports healthy, and on drain it closes before the main server, so no client is redirected to an HTTPS server that has just stopped accepting. Rejected: a second listener serving the application over plaintext — HTTP-without-TLS invites accidental cleartext traffic and is not a supported mode.
- **HSTS is opt-in, through `middleware.Secure` only.** Making clients prefer HTTPS on their own is a near-permanent client-side commitment, sent only over HTTPS. Rejected: enabling it automatically, and a separate `WithHSTS` option, which would duplicate `middleware.Secure`.

The [lifecycle spec](../specs/lifecycle.md#tls) states the sources, the preflight checks and the redirect target.

### Server Construction and the Escape Hatch

The `*http.Server` is built once per session, from configuration. The framework maps the fields it has an opinion about — address, timeouts, `MaxHeaderBytes`, `MaxHeaderValueCount`, the `ErrorLog` bridge — and `WithHTTPServer(func(*http.Server))` hands the built server to the application for the rest of the standard-library surface, ending the "the standard library added a field, Credo needs a release" treadmill. Rejected: a `WithHTTPServer(*http.Server)` value form, which would force a per-field merge policy against everything Credo populates from config; and one `With…` option per stdlib field, which is the treadmill itself.

The callback runs after every framework-set field, so it is the last word on the fields Credo merely maps, config keys included — which is what an escape hatch is for. `Handler`, `Addr` and `TLSConfig` are re-imposed after it, because the lifecycle depends on them: a replaced handler would bypass the middleware tiers, the error pipeline and route introspection while the App still reported itself as serving; a callback-set address would bind somewhere `app.Addr()` and readiness checks do not describe; and honouring a callback-set `TLSConfig` would let a listener the operator configured as plaintext serve TLS with no trace in the configuration. The [lifecycle spec](../specs/lifecycle.md#server-construction-and-withhttpserver) states the construction order and which fields the callback owns.

The server's lifecycle methods (`Serve`, `ServeTLS`, `Shutdown`, `Close`, `RegisterOnShutdown`) stay framework-owned: the callback must not call them or retain the pointer past its return. This is a documented contract, not an enforced one — retention is undetectable at runtime, and a debug-mode guess would report false positives on legitimate reads.

The `WithHTTPRedirect` listener is deliberately excluded. It is a fixed-function 301/308 responder, not an application server; applying protocol knobs or connection hooks meant for the app to it would be surprising in both directions. It keeps its own mirrored `ErrorLog` and `ReadHeaderTimeout`.

`server.max_header_value_count` is the one Go 1.27 field that also gets a config key, for parity with `max_header_bytes`: it is an operational limit an operator tunes per environment, and both are ordinary integers. Zero means "apply net/http's own default" (500) rather than a number Credo freezes into itself; a negative value is rejected at `New`, because net/http reads every value below 1 as the default and a typo would otherwise silently do nothing. `Protocols` and `DisableClientPriority` get no keys — they are Go values, and a stringly-typed subset would be a second, weaker API. Everything the callback sets is restart-only: the server is constructed once per session, so a reload cannot change it.

### Call-Scoped Contexts

Every context the lifecycle hands out lives as long as the call it is handed to ([lifecycle spec](../specs/lifecycle.md#contexts)). A background loop is therefore a component ([ADR-024](024-lifecycle-components.md)): its `Start` launches the loop on a goroutine whose context derives from `context.WithoutCancel(ctx)` with its own cancel, and its `Shutdown` calls that cancel and waits for the loop (the [dependency injection guide](../guides/dependency-injection.md#shutdown-and-lifecycle) shows one). `Shutdown` is the one stop signal, and it arrives in the component's turn of the drain rather than before the HTTP drain begins — which is why a component handed work by the last requests does not stop before they finish. A loop that keeps `Start`'s context stops as soon as `Start` returns, so the mistake shows in the first test rather than as a broken drain order in production.

The session context the App keeps internally is created when the start phase opens a session and cancelled as the drain begins, and reload observes it ([ADR-020](020-reload-and-partial-config-reload.md)). It is not delivered to application code, and Credo exposes **no** public `Context()` accessor: a nullable accessor would have to return `context.Background()` before `Run`, a silent dead zone for any goroutine that captured it too early.

Rejected: an application-lifetime context handed to start hooks or to `Start` and cancelled when the drain begins — every goroutine and component watching it would stop at once, before the HTTP drain and out of order, losing the work the last requests hand over. The per-component lifetime context is rejected in [ADR-024](024-lifecycle-components.md#rejected-alternatives).

### Start Phase and Its Interruption

The start phase prepares the App, binds the listener and runs the start walk of [ADR-024](024-lifecycle-components.md#start) — the internal tier, then the ingress tier — before the listener accepts. The listener is bound before the walk, so `Start` and start hooks can read `app.Addr()` (to register a port-0 address with service discovery, say), but it accepts nothing until the walk has succeeded. Every step can be interrupted by a shutdown request, and a start failure rolls back what was built; [ADR-024](024-lifecycle-components.md#a-shutdown-during-the-start-phase) records what an interruption does and [why a start that no shutdown can interrupt was rejected](024-lifecycle-components.md#rejected-alternatives), and the [lifecycle spec](../specs/lifecycle.md#the-start-walk) states the steps, the interruption outcomes and what each entry point returns.

### Drain in Tiers

The drain first withdraws readiness — `/ready` answers 503 so load balancers stop routing, while liveness stays up so the orchestrator does not kill a draining process ([health spec](../specs/health.md#shutdown)) — then runs the HTTP drain beside the ingress tier, waits for an in-flight reload, and stops the internal tier in reverse dependency order. The tiers are [ADR-024](024-lifecycle-components.md#two-tiers)'s: ingress is where work enters the process, so it stops while the last requests finish, and internal is what those requests and the ingress components use, so it stops after them. One deadline — `WithShutdownTimeout` (default 30s) for a signal- or cancellation-triggered drain, the caller's context for an explicit `Shutdown(ctx)` — serves the whole drain and is spent in order. A step that misses it is abandoned and reported, and what it depends on stays open: nothing is closed under a consumer that may still use it. A start or shutdown failure is reported as one `*LifecycleError`, joined with the HTTP drain's and an overrunning reload's errors — no false graceful-success result. The [lifecycle spec](../specs/lifecycle.md#shutdown-in-tiers) states the sequence and the deadline rules.

A prepared App keeps serving during `stopping`, because the HTTP drain and the readiness and liveness probes need it; after `stopped`, every request gets the callback-free 503 (Design Decisions).

Rejected: a hard barrier that is never abandoned. With call-scoped contexts nothing stops before its turn, and a component that needs live dependencies while it stops has them, because they stop after it. A hook is a component and is abandoned at the deadline like one; a component that truly needs a barrier documents that as its own property.

### Lifecycle Hooks

**OnStart** and **OnStop**:

```go
app.OnStart(func(ctx context.Context) error {
    return consul.Register(ctx, app.Addr())
})
app.OnStop(func(ctx context.Context) error {
    return consul.Deregister(ctx)
})
```

- Both are anonymous components of a tier, internal unless `credo.Ingress()` says otherwise. Within a tier, start hooks run FIFO after the tier's components have started, and stop hooks run LIFO before they stop, so a hook can use what the components provide and never outlives it. Under managed serving the listener is already bound when start hooks run, so `app.Addr()` is available.
- A hook is a leaf action. Anything with its own teardown is a component ([ADR-024](024-lifecycle-components.md)) — a value built outside DI with `app.Manage`, a resource whose teardown is `Close` with `credo.Closer()` on its binding — and process-level cleanup that must outlive every component, such as flushing a log sink, belongs after `Run` returns. A stop hook that closed a resource would run before that resource's consumers, because its slot precedes its tier's components.

The [lifecycle spec](../specs/lifecycle.md#apponstartfn-opts-and-apponstopfn-opts) states the hooks' contexts, error and panic handling, and registration rules.

**OnReload** runs after a trigger-driven reload has re-published the config snapshot and the typed `OnConfigChange[T]` subscribers have run; the reload model is [ADR-020](020-reload-and-partial-config-reload.md).

### Frozen Guard

At preparation admission (the first `ServeHTTP` request or a managed serve entry point) and at bootstrap-shutdown admission, the app is frozen. Late registration of routes, middleware, meta, status handlers, or lifecycle hooks panics with `credo: <what> called after app was compiled or shut down`. This prevents subtle race conditions from concurrent registration during serving or teardown. DI `Finalize` alone does not freeze HTTP registration, so a composition root can `Finalize`, resolve controllers and bind their routes before serving ([lifecycle spec](../specs/lifecycle.md#registration-guards)).

### Design Decisions

| Decision | Rationale |
| --- | --- |
| Signal-aware `Run` default | `Run` handles SIGINT/SIGTERM and drains gracefully — the common case needs no boilerplate. `RunContext`/`ServeContext` give callers full control with no signal handler (tests, embedding, custom signal sets) |
| SIGHUP reloads, never terminates | `Run` also maps SIGHUP (Unix) to `app.Reload`; a failed reload keeps the previous configuration and the process stays up. The signal policy, its opt-out and the rejected alternatives are [ADR-020](020-reload-and-partial-config-reload.md)'s |
| `Run` not a naive signal wrapper | `stop()` runs the instant the first signal arrives, _before_ the drain — so a second signal force-kills (standard two-stage Ctrl+C). A `defer stop(); RunContext(ctx)` wrapper would swallow it |
| One drain mechanism, CAS-idempotent | Signal, context-cancel, and explicit `Shutdown` share one `initiateShutdown`; the `running`→`stopping` CAS (not a parallel `sync.Once`) makes concurrent triggers safe |
| One stored preparation | Managed serving and direct `ServeHTTP` share a single Finalize → compile → publish result. A failed preparation is stored and repeated (managed: same error; `ServeHTTP`: panic), never retried against a frozen plan. Rejected: a `sync.Once` around compile (a panicking call counts as done and a later request would reach a nil handler) and per-entry-point preparation |
| Bootstrap `Shutdown` in `building` | A `building`→`stopping` CAS mirrors the start claim so a concurrent start and shutdown pick one owner; writes are frozen without requiring successful `Finalize`, and the shared drain runs with no servers. Gives composition roots and `ServeHTTP`-only tests a cleanup path. External servers remain owner-drained |
| Callback-free 503 after stopped | A rejected request gets the default `service_unavailable` envelope from a branch that cannot reach user middleware, custom renderers, locale detection or DI, so it never touches a component that has already been shut down. Rejected: panicking (an availability outcome, not developer misuse) and routing through `handleError` (its renderer may capture a singleton already shut down) |
| Single-use App | Terminal `stopped` state; re-run returns an error. Components are start-once and a worker refuses a second `Start`, so a re-run could not reset them; `New()` is the restart path |
| One lifecycle abstraction | Components are the one lifecycle abstraction, ordered by the dependency graph and the tiers ([ADR-024](024-lifecycle-components.md)) |
| Start hooks fail fast | Start hooks are sequential and may depend on each other — the first error, like a component's failed `Start`, aborts the rest and rolls back |
| `stateStarting` as the start phase | Claiming the slot before preparation and the server field writes keeps a concurrent claim out; a `Shutdown` arriving then interrupts the start phase and waits for its rollback instead of draining fields that may still be nil |
| `http.ErrServerClosed` → nil | Graceful shutdown is not an error condition |
| Start failure → rollback → terminal stopped | Every built component except the one whose `Start` failed is stopped, and the App stays `stopped`. Rejected: uniform rollback to `building`, which would skip teardown and leak started resources |

## Consequences

**Positive:**

- Zero-boilerplate graceful shutdown: `Run` handles signals and applies the `WithShutdownTimeout` deadline, which bounds every phase, since no hook is a hard barrier
- Readiness flips to 503 at shutdown start, so load balancers drain the instance before it stops accepting
- Deterministic start and drain order: components start in dependency order and stop in reverse, the ingress tier beside the HTTP drain and the internal tier after it
- A component fed by HTTP handlers stops after the HTTP drain and before the database it writes to, and a stop hook can use every component of its tier
- Background work has one stop signal, its component's `Shutdown`, delivered in the component's turn
- A shutdown during startup interrupts the start and rolls back what was built
- Frozen guard catches registration bugs at development time
- State machine prevents double-run and double-shutdown
- A start failure rolls back what was built and a runtime serve failure enters the same drain as graceful shutdown, so cleanup of started resources is attempted instead of skipped; shared deadline semantics still apply
- A never-run App has the same cleanup path, so bootstrap failures after resource registration do not leak
- Teardown follows the dependency graph and reports what did not complete instead of closing a dependency underneath a live consumer

**Negative:**

- Advanced signal needs (custom signal sets, multi-server coordination) use `RunContext` with the caller's own `signal.NotifyContext` — the default `Run` covers SIGINT/SIGTERM
- One deadline is spent in order across the tiers: a long HTTP drain leaves less for the internal tier
- The costs [ADR-024](024-lifecycle-components.md#consequences) records: a double start for a value an `OnStart` hook starts when its binding's type shows `Start` and `Shutdown`, and a readiness probe that restarts nothing
- No restart capability — must create new App after shutdown
- A failed preparation cannot be repaired in place; the App's remaining use is cleanup
