# ADR-006: Application Lifecycle

**Status:** Accepted **Date:** 2026-03-01 **Amended:** 2026-09-05 by [ADR-022](022-bootstrap-and-di-ownership.md) (shared preparation, bootstrap Shutdown, lifecycle 503, dependency-ordered DI teardown); v0.24.0 by [ADR-024](024-lifecycle-components.md) (lifecycle components: an interruptible start phase, `App.Start`, `OnStart`/`OnStop`, the drain in tiers, call-scoped contexts) **Depends on:** ADR-001 **Related:** [ADR-024](024-lifecycle-components.md)

## Context

An enterprise framework (ADR-001) must provide a well-defined application lifecycle: startup, runtime, and graceful shutdown. Background services (workers, pub/sub subscribers, gRPC servers) need a signal to stop accepting work. In-flight HTTP requests need time to complete. Resources (DB connections, caches, file handles) must be released in a deterministic order, after the work that uses them.

Go's stdlib `*http.Server` provides `Shutdown(ctx)` for HTTP drain but has no concept of application-level context, lifecycle state, or shutdown hooks. Credo fills this gap.

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

`starting` is the **start phase**: preparation, listener bind and the start walk of [ADR-024](024-lifecycle-components.md) — the internal tier's components in dependency order and its start hooks, then the ingress tier's — before the listener accepts. A failure is classed by how far the phase progressed. **Pre-session** failures (preparation, TLS preflight, listener bind) happen before the start walk, so they roll back to `building` and the App may run again — with one exception: a preparation failure (DI finalize error, compile panic) is a stored terminal developer error, so `building` then permits bootstrap cleanup through `Shutdown`, not a retry. A **start failure** — a component's `Start`, a start hook or a constructor in the start walk that returns an error or panics — and a non-`ErrServerClosed` error from `Serve` after the App reached `running` leave the App terminally `stopped`; retry means a new App.

- `App.Start(ctx)` enters the same phase without a listener: it claims `building → starting`, prepares, runs the start walk and enters `running`; the App is then served through `ServeHTTP` by a server its owner runs and drains. It is accepted only in `building`, so a second `App.Start`, or a serve entry point after it, returns the state error.
- `Shutdown` is accepted in `starting`. A shutdown requested during the start phase — `Shutdown`, the first signal in `Run`, or the context of `RunContext` or `ServeContext` — interrupts it: the running `Start` sees its context cancelled, nothing further starts, and what was built is rolled back under the drain deadline counted from the request. `Shutdown` waits for that rollback within its own context. The App ends `stopped`.
- A start failure rolls back rather than draining a session that never served: every built component is stopped in reverse dependency order except the one whose `Start` failed, which has released what it opened, and the stop hooks run. The App ends `stopped`, so the single-use rule holds.

| State | Meaning |
| --- | --- |
| `building` | Initial. Route/middleware registration allowed until preparation. `Shutdown` accepted (bootstrap teardown) |
| `starting` | The start phase: preparation, listener bind, the start walk. A shutdown request interrupts it and rolls back |
| `running` | Start phase complete; a managed listener accepts. Registration frozen. Shutdown allowed |
| `stopping` | Draining: HTTP beside the ingress tier, then the internal tier |
| `stopped` | Fully stopped |

State is stored as `atomic.Uint32` with `CompareAndSwap` transitions — no mutex on the hot path.

### API

```go
app.Run()                                  // Serve HTTP/HTTPS; block on SIGINT/SIGTERM, then drain
app.RunContext(ctx)                        // Serve HTTP/HTTPS; caller-driven cancellation, no signals
app.ServeContext(ctx, l)                   // Serve on a caller-provided net.Listener (TLS-exempt)
app.Start(ctx)                             // Run the start phase without a listener
app.Shutdown(ctx)                          // Graceful shutdown with deadline
app.State() string                         // Current state name
app.IsRunning() bool                       // Convenience check
app.Addr() net.Addr                        // Actual bound address (nil before Run)
app.Manage(v, opts...)                     // Add a component that is not a DI binding (ADR-024)
app.OnStart(fn func(ctx context.Context) error, opts...) // Start hook: FIFO, after its tier's components start
app.OnStop(fn func(ctx context.Context) error, opts...)  // Stop hook: LIFO, before its tier's components stop
app.Reload(ctx)                            // Trigger-driven partial reload (ADR-020)
app.OnReload(fn func(ctx context.Context) error)    // FIFO reload hook
app.OnConfigChange[T](key, fn)             // Typed per-section config subscriber

// Construction options:
credo.WithShutdownTimeout(d)               // Drain deadline for signal/cancel shutdown (default 30s)
credo.WithReloadTimeout(d)                 // Budget for SIGHUP-triggered reloads (default 30s)
credo.WithoutReloadSignals()               // SIGHUP under Run: logged no-op instead of Reload
credo.WithTLSFiles(certFile, keyFile)      // Serve HTTPS from a PEM cert/key pair
credo.WithTLSConfig(cfg)                    // Serve HTTPS from a *tls.Config (mTLS, SNI, reload)
credo.WithHTTPRedirect(addr)               // Second listener: redirect HTTP→HTTPS (requires TLS)
```

`Run` and `RunContext` serve plaintext or TLS from the same call: there is no separate TLS serve method. Whether a request is served over HTTPS is decided by configuration (see [TLS](#tls)), which is orthogonal to the control-flow choice (signal-aware vs caller-driven) those methods actually encode.

#### The hook set and `App.Start`

The lifecycle has two hooks, `OnStart` and `OnStop`, and both are anonymous components of a tier — internal unless `credo.Ingress()` says otherwise ([ADR-024](024-lifecycle-components.md)). What the former drain hooks (`OnPreDrain`, `OnDrain`) drained is a component, ordered by its edges and its tier. `OnStop` did not keep the name of the former `OnShutdown` hook, which ran after DI teardown: its slot is before its tier's components, and a moved slot under an unchanged name would have been a silent change rather than a compile error. `App.Start` exists so that an App served through `ServeHTTP` — by an external `http.Server` or in a test — starts its components too; its godoc opens with "Start runs the start phase without a listener; Run, RunContext and ServeContext serve.", because `Start(addr)` listens in other frameworks.

### TLS

TLS is **server configuration, not a serve-method variant**. `Run`/`RunContext` serve HTTPS when a certificate source is configured and plaintext otherwise. Three sources populate it, resolved by **precedence** — highest wins, whole-source override (never field-merged), never a conflict error:

```
WithTLSConfig(*tls.Config)   →  highest: full crypto/tls surface (mTLS, SNI, GetCertificate reload, ALPN)
WithTLSFiles(cert, key)      →  PEM file paths via option (rotated on reload, ADR-020)
server.tls.cert_file/key_file →  the same paths via config (rotated on reload, ADR-020)
(none)                       →  plaintext
```

`WithTLSFiles` overrides the `server.tls.*` keys at construction (the option is resolved after config unmarshal so it wins); `WithTLSConfig` outranks both and is resolved later, so when it is set the file sources are never examined. All TLS validation happens once, at **preflight** (a missing/mismatched key pair, a partial cert-without-key, a `WithTLSConfig` with no certificate source, or an explicitly-set-but-empty source — `WithTLSConfig(nil)` or `WithTLSFiles` with an empty path), making a bad cert a pre-session failure that rolls back to `building`. Because each explicit option records that it was set, an empty or nil explicit source fails loud here rather than silently falling through to a lower-precedence source or to plaintext — the security-sensitive failure mode (accidentally serving plaintext) is never reached silently. The resolved `*tls.Config` is loaded once and, for `WithTLSConfig`, cloned — the caller's live pointer is never bound to the running server, and later caller mutations do not affect serving. The certificate-source check mirrors `net/http`'s own (`Certificates`, `GetCertificate`, or `GetConfigForClient`).

`ServeContext` is TLS-exempt: it serves the listener it is handed exactly as given. For HTTPS on a custom listener, wrap it yourself with `tls.NewListener`.

**HTTP→HTTPS redirect.** `WithHTTPRedirect(addr)` runs a second, plaintext listener whose only job is to permanently redirect every request to its HTTPS equivalent (301 for GET/HEAD, 308 for other methods — matching the trailing-slash redirect convention; the target reuses the request host with the TLS server's port, omitted when 443). It requires TLS (else preflight fails fast, like a missing cert) and starts and drains with the main server; on drain the redirect listener is closed _before_ the main server so no client is redirected to an HTTPS server that has just stopped accepting. A runtime failure of the redirect listener tears the app down — the same terminal teardown as a main-listener failure — so a requested redirect can never silently die while the app reports healthy. It is a deliberately narrow redirect-only listener, not a second application listener serving plaintext traffic — HTTP-without-TLS is not a supported app mode. `ServeContext` ignores it (the caller owns its listener). HSTS — making clients _prefer_ HTTPS on their own — is a separate, orthogonal concern handled by `middleware.Secure` (opt-in, sent only over HTTPS), never auto-enabled.

### Server Construction and the Escape Hatch

The `*http.Server` is built once per session, from configuration. The framework maps the fields it has an opinion about — address, timeouts, `MaxHeaderBytes`, `MaxHeaderValueCount`, the `ErrorLog` bridge — and `WithHTTPServer(func(*http.Server))` exposes the rest of the standard-library surface without Credo growing an option per field. Construction order:

```
serverConfig  →  buildServer fields  →  ErrorLog bridge  →  WithHTTPServer callback  →  (preflight) TLS chain  →  listen/serve
```

The callback runs **last** among the construction steps, so it has the final word on everything set before it, config keys included. Three fields are re-imposed afterwards because the lifecycle depends on them:

| Field | Who wins | Why |
| --- | --- | --- |
| `Handler` | framework | The `App` is the router; a replaced handler would bypass the middleware tiers, the error pipeline, and route introspection while the App still reported itself as serving |
| `Addr` | framework | The listener is bound from it; a callback-set address would bind somewhere `app.Addr()` and readiness checks do not describe |
| `TLSConfig` | framework | TLS has a documented precedence chain resolved later, at preflight. A Credo TLS source overwrites the callback's value; with no Credo TLS source the server is served by `Serve`, which ignores `TLSConfig` — the callback can never quietly upgrade a listener the operator configured as plaintext |
| everything else | callback | `Protocols` (H2C, HTTP/2 tuning), `ConnState`, `BaseContext`, `ConnContext`, `TLSNextProto`, `DisableClientPriority`, and the framework-mapped timeouts and limits |

The server's lifecycle methods (`Serve`, `ServeTLS`, `Shutdown`, `Close`, `RegisterOnShutdown`) stay framework-owned: the callback must not call them or retain the pointer past its return. This is a documented contract, not an enforced one — retention is undetectable at runtime, and a debug-mode guess would report false positives on legitimate reads.

The `WithHTTPRedirect` listener is deliberately excluded. It is a fixed-function 301/308 responder, not an application server; applying protocol knobs or connection hooks meant for the app to it would be surprising in both directions. It keeps its own mirrored `ErrorLog` and `ReadHeaderTimeout`.

`server.max_header_value_count` is the one Go 1.27 field that also gets a config key, for parity with `max_header_bytes`: it is an operational limit an operator tunes per environment, and both are ordinary integers. Zero means "apply net/http's own default" (500) rather than a number Credo freezes into itself; a negative value is rejected at `New`, because net/http reads every value below 1 as the default and a typo would otherwise silently do nothing. `Protocols` and `DisableClientPriority` get no keys — they are Go values, and a stringly-typed subset would be a second, weaker API. Everything the callback sets is restart-only: the server is constructed once per session, so a reload cannot change it.

### Call-Scoped Contexts

Every context the lifecycle hands out lives as long as the call it is handed to: an `OnStart` hook's and a component's `Start` context is cancelled when the call returns, or earlier when a shutdown is requested during the start phase; a `Shutdown` or `OnStop` context carries the drain deadline; a `Ready` context belongs to the probe request. A background loop is therefore a component ([ADR-024](024-lifecycle-components.md)): its `Start` launches the loop on a goroutine whose context derives from `context.WithoutCancel(ctx)` with its own cancel, and its `Shutdown` calls that cancel and waits for the loop:

```go
func (s *Subscriber) Start(ctx context.Context) error {
    loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
    s.cancel, s.done = cancel, make(chan struct{})
    go func() {
        defer close(s.done)
        for {
            select {
            case <-loopCtx.Done():
                return
            case msg := <-s.messages:
                process(msg)
            }
        }
    }()
    return nil
}

func (s *Subscriber) Shutdown(ctx context.Context) error {
    s.cancel()
    select {
    case <-s.done:
        return nil
    case <-ctx.Done():
        return ctx.Err()
    }
}
```

`Shutdown` is the one stop signal, and it arrives in the component's turn of the drain rather than before the HTTP drain begins — which is why a component handed work by the last requests does not stop before they finish. A loop that keeps `Start`'s context stops as soon as `Start` returns, so the mistake shows in the first test rather than as a broken drain order in production.

The session context the App keeps internally still exists: it is created when the start phase opens a session and cancelled as the drain begins, and reload observes it ([ADR-020](020-reload-and-partial-config-reload.md)). It is not delivered to application code, and Credo exposes **no** public `Context()` accessor: a nullable accessor would have to return `context.Background()` before `Run`, a silent dead zone for any goroutine that captured it too early.

Rejected: an application-lifetime context handed to `OnStart` hooks and cancelled when the drain begins, which the lifecycle offered before the component model — every goroutine that watched it stopped at once, before the HTTP drain, so a worker lost the work the last requests handed it; handing that context to `Start`, which would stop every component that watched it at once, out of order; and a per-component lifetime context, which gives one resource two stop signals and cannot bound the start work. ADR-024 records the last two.

### Start Phase and Its Interruption

```
1. CAS building → starting (claims the start slot; App.Start claims the same slot)
2. Prepare: DI Finalize (idempotent) → compile → publish; one stored result shared with ServeHTTP
   (failure: state back to building, error returned, result stored and never retried)
3. TLS preflight, then bind the port and store the bound address — app.Addr() now returns
   the real address (managed serving only; failure: state back to building)
4. Internal tier: build what has Start or Ready and is still unbuilt, and every constructor
   handed to Manage; Start in dependency order; then internal OnStart hooks FIFO
5. Ingress tier: the same, then ingress OnStart hooks FIFO
6. Store state = running; log "server started" (managed serving only); accept
```

Managed serving, `App.Start`, direct `ServeHTTP` and an external `http.Server` all reach the same validated runtime model through step 2. A direct `ServeHTTP` call on an App with nothing to start prepares on its first request without claiming the start slot and panics with the stored preparation error while lifecycle admission is open; an App with anything to start is started first ([Every way of serving starts the components](024-lifecycle-components.md#every-way-of-serving-starts-the-components)). Preparation admission — not DI `Finalize` — freezes HTTP registration, so a composition root can `Finalize`, resolve controllers and bind their routes before serving. The listener is bound before the start walk, so `Start` and start hooks can read `app.Addr()`, but it accepts nothing until the walk has succeeded.

Every step from 2 onward can be interrupted. A shutdown requested during the phase — `Shutdown`, the first signal in `Run`, the context of `RunContext` or `ServeContext` — cancels the running `Start`'s context, starts nothing further and rolls back under the drain deadline counted from the request; a `Start` that ignores the cancellation is abandoned and reported like a `Shutdown` that misses the deadline. A requested shutdown is not a start failure: `Run` returns nil after a clean rollback and logs the interrupted `Start`'s error, "server started" is never logged, and the listener never accepts. An interrupted `App.Start` returns the interruption's cause — its context's error, or an error saying that `Shutdown` interrupted it — joined with the lifecycle error when the rollback was not clean, because its caller goes on to serve the App and must not mistake an interrupted start for a started one.

A start failure — a `Start` or start hook that returns an error or panics, or a constructor that fails in the start walk — closes the listener, stops every built component in reverse dependency order except the one whose `Start` failed, runs the stop hooks under the shutdown timeout, and returns the failure as the one lifecycle error ([ADR-024](024-lifecycle-components.md#one-error)). The App ends terminally `stopped`: a component that started may already have produced externally visible side effects, so a session that began never returns to `building`.

Rejected: a start that no shutdown can interrupt. An orchestrator kills the process after its grace period anyway, so the guarantee never held, and a cancelled start rolls back where a killed one cannot; a step that must finish says so with `context.WithoutCancel`, and migrations already require transactional or idempotent work.

### Drain in Tiers

```
1. CAS running → stopping (or building → stopping: bootstrap teardown, which freezes HTTP
   and DI writes without validating DI and runs the chain with no servers; or, while
   starting, interrupt the start phase and roll back)
2. Mark unready — /ready returns 503 so load balancers stop routing (liveness stays up) —
   and cancel the session context (an in-flight reload stops)
3. Concurrently: the HTTP drain (redirect listener before the main server), and the ingress
   tier — ingress OnStop hooks LIFO, then ingress components, those that no edge orders
   concurrently with each other
4. Wait for an in-flight reload (ADR-020)
5. Internal tier: internal OnStop hooks LIFO, then DI enters closing and the internal
   components stop in reverse dependency order, values with no edges in reverse
   registration order
6. Clear bound address
7. Store state = stopped
```

The tiers are [ADR-024](024-lifecycle-components.md)'s: ingress is where work enters the process, internal is everything else, and no dependency crosses them backwards, so the dependency order holds for every component and the tiers order only components that no dependency path connects. One deadline — `WithShutdownTimeout` (default 30s) for a signal- or cancellation-triggered drain, the caller's context for an explicit `Shutdown(ctx)` — serves the whole drain and is spent in order: a long HTTP drain leaves less for the internal tier. Every phase follows the same deadline rule — there is no hard barrier: a `Shutdown` or stop hook that misses the deadline is abandoned and reported, the dependencies of an abandoned component stay open, and a `Shutdown` that returns an error promptly is reported while its dependencies are still stopped. A start or shutdown failure is reported as one `*LifecycleError` naming each component, its phase and its outcome; the HTTP drain's and an overrunning reload's errors are joined with it — no false graceful-success result.

After `stopped`, `ServeHTTP` answers every request with the framework's default 503 envelope through a callback-free branch (no preparation, resolution, dispatch, custom renderer or locale detection), so a rejected request can never touch a component that has already been shut down. An App that was prepared before the drain keeps serving during `stopping` — that is what the HTTP drain and the readiness/liveness probes need.

Rejected: a hard barrier that is never abandoned, as the former `OnPreDrain` hook provided. It existed because the lifecycle context's cancellation tore down lifecycle-bound workers before coordination work finished; with call-scoped contexts nothing is cancelled ahead of its turn, and a component that needs live dependencies while it stops has them, because they stop after it. A hook is a component and is abandoned at the deadline like one; a component that truly needs a barrier documents that as its own property.

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
- A start hook's context ends with the call, or earlier when a shutdown is requested during the start phase. A start hook that returns an error or panics is a start failure: nothing further starts and the start rolls back ([Start Phase and Its Interruption](#start-phase-and-its-interruption)).
- A stop hook receives the drain deadline, runs on every teardown — the drain, bootstrap teardown, and the rollback of a failed or interrupted start — and must tolerate a start that never reached its counterpart. A stop hook that misses the deadline is abandoned and reported like a component's `Shutdown`; an error or a panic is reported without stopping the drain.
- A panicking hook is that hook's error: the record `credo: OnStart hook panic` or `credo: OnStop hook panic` carries the hook's index and the stack, and the lifecycle error names the hook `OnStart[i]` or `OnStop[i]`.
- A hook is a leaf action. Anything with its own teardown is a component ([ADR-024](024-lifecycle-components.md)) — a value built outside DI with `app.Manage`, a resource whose teardown is `Close` with `credo.Closer()` on its binding — and process-level cleanup that must outlive every component, such as flushing a log sink, belongs after `Run` returns. A stop hook that closed a resource would run before that resource's consumers, because its slot precedes its tier's components.
- Registration follows the bootstrap order ([ADR-022](022-bootstrap-and-di-ownership.md)) and happens before the App is prepared; a nil hook, an option other than `credo.Ingress()` or a late registration panics.

**OnReload** — called by a trigger-driven reload (`SIGHUP` under `Run`, or `app.Reload`) after the config snapshot has been re-published and typed `OnConfigChange[T]` subscribers have run. FIFO; errors are joined and logged but never terminate the process; registration is frozen with the other hooks. The full model (partial config reload, validate-before-publish, file-based TLS rotation) is [ADR-020](020-reload-and-partial-config-reload.md).

### Frozen Guard

At preparation admission (the first `ServeHTTP` request or a managed serve entry point) and at bootstrap-shutdown admission, the app is frozen. Late registration of routes, middleware, meta, status handlers, or lifecycle hooks panics with `credo: <what> called after app was compiled or shut down`. This prevents subtle race conditions from concurrent registration during serving or teardown. DI `Finalize` alone does not freeze HTTP registration.

### Design Decisions

| Decision | Rationale |
| --- | --- |
| Signal-aware `Run` default | `Run` handles SIGINT/SIGTERM and drains gracefully — the common case needs no boilerplate. `RunContext`/`ServeContext` give callers full control with no signal handler (tests, embedding, custom signal sets) |
| SIGHUP reloads, never terminates | `Run` also handles SIGHUP (Unix) by calling `app.Reload` under `WithReloadTimeout`; a failed reload keeps the previous configuration and the process stays up. `WithoutReloadSignals()` opts out with subscribe-and-ignore semantics: the signal is still captured (never falls through to terminate) but is logged and ignored instead of reloading. `RunContext`/`ServeContext` stay signal-free and use `app.Reload` directly. Rejected: letting SIGHUP fall through to Go's default handler (kills the process — the opposite of `systemctl reload`), SIGUSR1/2 (non-conventional), filesystem watching (ADR-020) |
| `Run` not a naive signal wrapper | `stop()` runs the instant the first signal arrives, _before_ the drain — so a second signal force-kills (standard two-stage Ctrl+C). A `defer stop(); RunContext(ctx)` wrapper would swallow it |
| One drain mechanism, CAS-idempotent | Signal, context-cancel, and explicit `Shutdown` share one `initiateShutdown`; the `running`→`stopping` CAS (not a parallel `sync.Once`) makes concurrent triggers safe |
| One stored preparation | Managed serving and direct `ServeHTTP` share a single Finalize → compile → publish result. A failed preparation is stored and repeated (managed: same error; `ServeHTTP`: panic), never retried against a frozen plan. Rejected: a `sync.Once` around compile (a panicking call counts as done and a later request would reach a nil handler) and per-entry-point preparation |
| Bootstrap `Shutdown` in `building` | A `building`→`stopping` CAS mirrors the start claim so a concurrent start and shutdown pick one owner; writes are frozen without requiring successful `Finalize`, and the shared drain runs with no servers. Gives composition roots and `ServeHTTP`-only tests a cleanup path. External servers remain owner-drained |
| Callback-free 503 after stopped | A rejected request gets the default `service_unavailable` envelope from a branch that cannot reach user middleware, custom renderers, locale detection or DI. Rejected: panicking (an availability outcome, not developer misuse) and routing through `handleError` (its renderer may capture a singleton already shut down) |
| Single-use App | Terminal `stopped` state; re-run returns an error. Re-run was already broken (latched component flags); `New()` is the restart path |
| TLS as server config | TLS is configured (`WithTLSFiles`/`WithTLSConfig`/`server.tls.*`), not selected by a serve method. Transport (plain vs TLS) is orthogonal to control flow (signal vs context) — folding it into `Run`/`RunContext` removes the `RunTLS`/`RunTLSContext` combinatorial pair. Rejected: separate `RunTLS*` methods (mirror stdlib `ListenAndServeTLS`, but TLS belongs to the same category as host/port — configuration) |
| TLS source precedence, not conflict | `WithTLSConfig` > `WithTLSFiles` > `server.tls.*` > plaintext, whole-source override. Rejected: erroring when two sources are set — precedence lets an option cleanly override a config-file default, the common case, and avoids a brittle "set exactly one" rule |
| TLS cert preflight | The resolved config is built and validated before `stateRunning`, so a bad cert (missing/mismatched files, partial cert-without-key, a `WithTLSConfig` with no certificate source, or an explicit-but-empty/nil source — `WithTLSConfig(nil)`, `WithTLSFiles("", "")`) fails fast with the same pre-session rollback as a listen error. An explicit option recording that it was set lets an empty/nil value fail loud rather than silently downgrade to a lower-precedence source or plaintext |
| HTTP→HTTPS via redirect listener, not dual-serve | `WithHTTPRedirect` adds a redirect-only second listener (301/308 to HTTPS), requiring TLS; its runtime failure tears the app down like the main listener (a requested redirect must not silently die while the app reports healthy), and on drain it closes before the main server. Rejected: a second listener serving the _app_ over plaintext (HTTP-without-TLS invites accidental cleartext traffic — not a supported mode) and auto-HSTS (a near-permanent client-side commitment — opt-in via `middleware.Secure` only, never automatic) |
| `http.Server` escape hatch as a callback | `WithHTTPServer(fn func(*http.Server))` sees the fully-built server and adjusts it, ending the "the standard library added a field, Credo needs a release" treadmill. Rejected: a `WithHTTPServer(*http.Server)` value form, which would force a per-field merge policy against everything Credo populates from config; and one `With…` option per stdlib field, which is the treadmill itself |
| Callback last, three fields re-imposed | Running the callback after every framework-set field makes it the last word for the fields Credo merely maps, which is what an escape hatch is for. `Handler`, `Addr`, and `TLSConfig` are re-imposed because the lifecycle, the bound address, and the TLS precedence chain depend on them — silently honouring a callback-set `TLSConfig` would let a plaintext-configured listener serve TLS with no trace in the configuration |
| Readiness unready on shutdown | The drain's first step flips `/ready` to 503 so load balancers stop routing before the HTTP drain; liveness stays up so orchestrators don't kill the draining process |
| One lifecycle abstraction | The five former teardown mechanisms — the `OnPreDrain` and `OnDrain` hooks, DI `Shutdowner`s, `OnShutdown` hooks after DI teardown, and the lifecycle context's cancellation — are graph-ordered components ([ADR-024](024-lifecycle-components.md)). Rejected: the deferred lifecycle `Service`, whose `Run(ctx)` blocks for the lifetime and whose restart taxonomy is a worker concern; components are start-once |
| Two hooks, each a component | `OnStart` (FIFO) and `OnStop` (LIFO) run within their tier, after its components start and before they stop. `OnStop` has its own name because its slot precedes the tier's components, where the former `OnShutdown` ran after DI teardown. Rejected: moving `OnShutdown` before DI teardown in a patch — the order would have changed twice |
| No hard barrier | With call-scoped contexts nothing stops before its turn. Every hook and component is abandoned at the deadline, and what an abandoned component kept open is reported instead of closed under it. Rejected: a pre-cancellation hook that is never abandoned, which existed only because the lifecycle context's cancellation stopped workers out of order |
| No post-compile hook registration | Frozen guard prevents race conditions |
| Start hooks fail fast | Start hooks are sequential and may depend on each other — the first error, like a component's failed `Start`, aborts the rest and rolls back |
| Interruptible start, accepted while starting | `Shutdown` is accepted in `starting`, and every shutdown request cancels the running `Start` and rolls back. Rejected: a start no shutdown can interrupt (see [Start Phase and Its Interruption](#start-phase-and-its-interruption)) |
| `stateStarting` as the start phase | Claiming the slot before preparation and the server field writes keeps a concurrent claim out; a `Shutdown` arriving then interrupts the start phase and waits for its rollback instead of draining fields that may still be nil |
| `http.ErrServerClosed` → nil | Graceful shutdown is not an error condition |
| Pre-session failure → building | Preflight/listen errors start nothing — rolling back to `building` keeps the App retryable |
| Start failure → rollback → terminal stopped | Every built component stops in reverse dependency order except the one whose `Start` failed, which has released what it opened; a component that started may hold side effects, so the App ends `stopped` and stays single-use. Rejected: uniform rollback to `building`, which would skip teardown and leak started resources |
| Serve failure → terminal stopped | A post-running serve error, or a runtime failure of the redirect listener, runs the full drain and ends `stopped` |
| Every way of serving starts the components | `App.Start` runs the start phase for an App served through `ServeHTTP`; an App with anything to start refuses `ServeHTTP` until the phase has succeeded, and an external server's owner drains it before `App.Shutdown` |

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
- The costs [ADR-024](024-lifecycle-components.md#consequences) records: a double start for a value an `OnStart` hook starts when its binding's type shows `Start` and `Shutdown`, a readiness probe that restarts nothing, and behavior changes that compile unchanged — `OnStart`'s context ends with the call, and a shutdown during startup interrupts the start
- No restart capability — must create new App after shutdown
- A failed preparation cannot be repaired in place; the App's remaining use is cleanup
