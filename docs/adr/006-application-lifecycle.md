# ADR-006: Application Lifecycle

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** ADR-001

## Context

An enterprise framework (ADR-001) must provide a well-defined application lifecycle: startup, runtime, and graceful shutdown. Background services (workers, pub/sub subscribers, gRPC servers) need a signal to stop accepting work. In-flight HTTP requests need time to complete. Shutdown hooks must release resources (DB connections, caches, file handles) in a deterministic order.

Go's stdlib `*http.Server` provides `Shutdown(ctx)` for HTTP drain but has no concept of application-level context, lifecycle state, or shutdown hooks. Credo fills this gap.

## Decision

### Lifecycle State Machine

```
building → starting → running → stopping → stopped

Failure transitions:
  preflight / listen error      → building   (pre-session: nothing started, retryable)
  OnStart hook error            → stopped    (session: full teardown, terminal)
  serve error (after running)   → stopped    (session: full teardown, terminal)
```

A failure is classed by how far startup progressed. **Pre-session** failures (TLS preflight, listener bind) happen before any OnStart hook runs and before startup resolves any singleton, so they roll back to `building` and the App may run again. **Session** failures — any error once the OnStart phase has begun (a hook returning an error, regardless of position) or a non-`ErrServerClosed` error from `Serve` after the app reached `running` — run the full teardown chain (the same one a graceful shutdown runs) and the App becomes terminally `stopped`; retry means a new App.

| State | Meaning |
| --- | --- |
| `building` | Initial. Route/middleware registration allowed |
| `starting` | Transient. CAS claimed, ctx/server being written. Shutdown blocked |
| `running` | Port bound, server accepting. Registration frozen. Shutdown allowed |
| `stopping` | Draining HTTP and pre-infrastructure subsystems, then DI/hooks |
| `stopped` | Fully stopped |

State is stored as `atomic.Uint32` with `CompareAndSwap` transitions — no mutex on the hot path.

### API

```go
app.Run()                                  // Serve HTTP/HTTPS; block on SIGINT/SIGTERM, then drain
app.RunContext(ctx)                        // Serve HTTP/HTTPS; caller-driven cancellation, no signals
app.ServeContext(ctx, l)                   // Serve on a caller-provided net.Listener (TLS-exempt)
app.Shutdown(ctx)                          // Graceful shutdown with deadline
app.State() string                         // Current state name
app.IsRunning() bool                       // Convenience check
app.Addr() net.Addr                        // Actual bound address (nil before Run)
app.OnStart(fn func(ctx context.Context) error)     // FIFO startup hook
app.OnPreDrain(fn func(ctx context.Context) error)  // Parallel pre-cancellation drain
app.OnDrain(fn func(ctx context.Context) error)     // Parallel pre-DI subsystem drain
app.OnShutdown(fn func(ctx context.Context) error)  // LIFO shutdown hook
app.Reload(ctx)                            // Trigger-driven partial reload (ADR-020)
app.OnReload(fn func(ctx context.Context) error)    // FIFO reload hook
app.OnConfigChange[T](key, fn)             // Typed per-section config subscriber

// Construction options:
credo.WithShutdownTimeout(d)               // Drain deadline for signal/cancel shutdown (default 30s)
credo.WithReloadTimeout(d)                 // Budget for SIGHUP-triggered reloads (default 30s)
credo.WithTLSFiles(certFile, keyFile)      // Serve HTTPS from a PEM cert/key pair
credo.WithTLSConfig(cfg)                    // Serve HTTPS from a *tls.Config (mTLS, SNI, reload)
credo.WithHTTPRedirect(addr)               // Second listener: redirect HTTP→HTTPS (requires TLS)
```

`Run` and `RunContext` serve plaintext or TLS from the same call: there is no separate TLS serve method. Whether a request is served over HTTPS is decided by configuration (see [TLS](#tls)), which is orthogonal to the control-flow choice (signal-aware vs caller-driven) those methods actually encode.

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

### Lifecycle Context

The lifecycle context is created at `Run()`/`RunContext()` and cancelled during `Shutdown()`, after readiness is withdrawn and `OnPreDrain` completes. Credo deliberately exposes **no** public `Context()` accessor: a nullable accessor would have to return `context.Background()` before `Run`, a silent dead zone for any goroutine that captured it too early. Background services receive the context through their `OnStart` hook's `lifecycleCtx` parameter and select on `lifecycleCtx.Done()` to detect shutdown:

```go
app.OnStart(func(lifecycleCtx context.Context) error {
    go func() {
        for {
            select {
            case <-lifecycleCtx.Done():
                return
            case msg := <-subscriber.Messages():
                process(msg)
            }
        }
    }()
    return nil
})
```

Without an accessor the dead zone is structurally unreachable: there is no pre-`Run` context to capture. (A dedicated background-service abstraction that receives this context directly is planned.)

### Startup Sequence

```
1. DI Finalize (idempotent)
2. CAS building → starting
3. Create lifecycle context
4. Bind port (net.Listen)
5. Store bound address (app.Addr() now returns the real address)
6. OnStart hooks in FIFO order (first registered, first called)
7. Store state = running
```

If any OnStart hook returns an error, startup aborts and the App runs the full teardown chain (mark unready → OnPreDrain → cancel lifecycle context → parallel HTTP + OnDrain subsystem drain → DI container shutdown → OnShutdown hooks), then the bound listener is closed and `Run` returns the hook error (joined with any teardown error). The App ends in the terminal `stopped` state — an earlier hook may already have produced externally visible side effects (started workers, acquired a migration lock), so a session that began must tear down rather than roll back. State is `stateStarting` during the hooks, where a concurrent `Shutdown` (which requires `stateRunning`) cannot race the drain.

### Shutdown Sequence

```
1. CAS running → stopping
2. Mark unready — /ready returns 503 so load balancers stop routing (liveness stays up)
3. Run every OnPreDrain hook concurrently (lifecycle workers and DI remain live)
4. Cancel lifecycle context (signals background services)
5. In parallel: HTTP server drain and every OnDrain subsystem hook
6. DI container shutdown (reverse-order singleton cleanup)
7. OnShutdown hooks in LIFO order (last registered, first called)
8. Clear bound address
9. Store state = stopped
```

OnPreDrain, HTTP drain, and OnDrain receive one absolute deadline. Hook
execution order within each hook phase is intentionally unspecified. An
OnPreDrain hook returns successfully only after work requiring live lifecycle
workers or DI has finished; an OnDrain hook must guarantee its subsystem can no
longer execute handlers that depend on DI infrastructure. At deadline expiry,
Credo immediately logs that OnPreDrain work is still pending but does not
abandon it: the lifecycle context and DI stay live until every OnPreDrain hook
actually returns. The hook's completion timestamp then determines its final
identified incomplete error. Later phases continue with the same, possibly
ended context. HTTP or OnDrain work still follows the ordinary
deadline-incomplete contract and is not a hard barrier. All errors are
collected via `errors.Join` — no false graceful-success result.

### Lifecycle Hooks

**OnStart** — called after the port is bound, before the server accepts connections:

```go
app.OnStart(func(lifecycleCtx context.Context) error {
    log.Println("server ready on", app.Addr())
    return consul.Register(lifecycleCtx, app.Addr())
})
```

- FIFO order (first registered, first called)
- Receives the lifecycle context (created at `Run` time)
- Fail-fast: the first error aborts startup, remaining hooks are skipped, the full teardown chain runs, and the App ends terminally `stopped` (a session that began tears down rather than rolling back)
- Must be called before `Run()`; panics after compile (frozen guard)

**OnPreDrain** — called after state enters stopping and readiness is withdrawn,
but before lifecycle cancellation:

```go
app.OnPreDrain(func(ctx context.Context) error {
    return coordinator.FlushWhileWorkersAreLive(ctx)
})
```

- No ordering guarantee between hooks; registration index/source identify errors only
- Receives the shared absolute shutdown deadline
- Must finish work that specifically requires live lifecycle workers or DI before returning nil
- Runs on every teardown, including an OnStart failure; must be idempotent and tolerate partial startup
- Panic is isolated and joined as an identified hook error; other hooks and later teardown phases continue
- Deadline-ignoring work emits a waiting diagnostic at the deadline; after it returns, its completion timestamp produces the final identified incomplete error, while lifecycle cancellation and teardown wait throughout
- Must be registered before compile; nil or late registration panics

Most subsystems should use OnDrain. OnPreDrain is deliberately narrower: it is
only for cases where lifecycle cancellation itself would tear down a dependency
before the subsystem can finish its drain.

**OnDrain** — called after lifecycle cancellation, concurrently with HTTP drain
and with every other OnDrain hook, before DI teardown:

```go
app.OnDrain(func(ctx context.Context) error {
    return subsystem.Shutdown(ctx)
})
```

- No ordering guarantee between hooks; registration index/source identify errors only
- Receives the shared absolute shutdown deadline
- Must stop admission and wait for DI-dependent handlers/cleanup before returning nil
- Runs on every teardown, including an OnStart failure; must be idempotent and tolerate partial startup
- Panic is isolated and joined as an identified hook error; other drain work continues
- Deadline-ignoring work is reported as incomplete and may return later, while teardown proceeds
- Must be registered before compile; nil or late registration panics

WebSocket is the first in-tree consumer: `websocket.Use` registers its
connection registry drain here. A completed drain proves that handlers finish
before their DI repositories are closed; an incomplete drain is identified and
teardown continues. Future gRPC or pubsub servers may use the same narrow seam,
but OnDrain is not a general startup/restartable Service abstraction.

**OnShutdown** — called after DI teardown during every teardown:

```go
app.OnShutdown(func(ctx context.Context) error {
    return externalClient.Shutdown(ctx)
})
```

- Hooks receive the shutdown deadline context from `Shutdown(ctx)`
- LIFO order (reverse registration order) — resources opened last are closed first
- Run on **every** teardown, including a failed startup — OnShutdown is the session teardown point, not an OnStart mirror, so hooks must be idempotent and must not assume any particular OnStart hook completed
- Must be called before `Run()`; panics after compile (frozen guard)
- Sequential execution — for parallel shutdown, wrap in a single hook with `errgroup`

**OnReload** — called by a trigger-driven reload (`SIGHUP` under `Run`, or `app.Reload`) after the config snapshot has been re-published and typed `OnConfigChange[T]` subscribers have run. FIFO; errors are joined and logged but never terminate the process; registration is frozen with the other hooks. The full model (partial config reload, validate-before-publish, file-based TLS rotation) is [ADR-020](020-reload-and-partial-config-reload.md).

### Frozen Guard

After `compile()` (triggered by first `ServeHTTP` or `Run`), the app is frozen. Late registration of routes, middleware, meta, status handlers, or shutdown hooks panics with a clear message. This prevents subtle race conditions from concurrent registration during serving.

### Design Decisions

| Decision | Rationale |
| --- | --- |
| Signal-aware `Run` default | `Run` handles SIGINT/SIGTERM and drains gracefully — the common case needs no boilerplate. `RunContext`/`ServeContext` give callers full control with no signal handler (tests, embedding, custom signal sets) |
| SIGHUP reloads, never terminates | `Run` also handles SIGHUP (Unix) by calling `app.Reload` under `WithReloadTimeout`; a failed reload keeps the previous configuration and the process stays up. `RunContext`/`ServeContext` stay signal-free and use `app.Reload` directly. Rejected: letting SIGHUP fall through to Go's default handler (kills the process — the opposite of `systemctl reload`), SIGUSR1/2 (non-conventional), filesystem watching (ADR-020) |
| `Run` not a naive signal wrapper | `stop()` runs the instant the first signal arrives, _before_ the drain — so a second signal force-kills (standard two-stage Ctrl+C). A `defer stop(); RunContext(ctx)` wrapper would swallow it |
| One drain mechanism, CAS-idempotent | Signal, context-cancel, and explicit `Shutdown` share one `initiateShutdown`; the `running`→`stopping` CAS (not a parallel `sync.Once`) makes concurrent triggers safe |
| Single-use App | Terminal `stopped` state; re-run returns an error. Re-run was already broken (latched component flags); `New()` is the restart path |
| TLS as server config | TLS is configured (`WithTLSFiles`/`WithTLSConfig`/`server.tls.*`), not selected by a serve method. Transport (plain vs TLS) is orthogonal to control flow (signal vs context) — folding it into `Run`/`RunContext` removes the `RunTLS`/`RunTLSContext` combinatorial pair. Rejected: separate `RunTLS*` methods (mirror stdlib `ListenAndServeTLS`, but TLS belongs to the same category as host/port — configuration) |
| TLS source precedence, not conflict | `WithTLSConfig` > `WithTLSFiles` > `server.tls.*` > plaintext, whole-source override. Rejected: erroring when two sources are set — precedence lets an option cleanly override a config-file default, the common case, and avoids a brittle "set exactly one" rule |
| TLS cert preflight | The resolved config is built and validated before `stateRunning`, so a bad cert (missing/mismatched files, partial cert-without-key, a `WithTLSConfig` with no certificate source, or an explicit-but-empty/nil source — `WithTLSConfig(nil)`, `WithTLSFiles("", "")`) fails fast with the same pre-session rollback as a listen error. An explicit option recording that it was set lets an empty/nil value fail loud rather than silently downgrade to a lower-precedence source or plaintext |
| HTTP→HTTPS via redirect listener, not dual-serve | `WithHTTPRedirect` adds a redirect-only second listener (301/308 to HTTPS), requiring TLS; its runtime failure tears the app down like the main listener (a requested redirect must not silently die while the app reports healthy), and on drain it closes before the main server. Rejected: a second listener serving the _app_ over plaintext (HTTP-without-TLS invites accidental cleartext traffic — not a supported mode) and auto-HSTS (a near-permanent client-side commitment — opt-in via `middleware.Secure` only, never automatic) |
| Readiness unready on shutdown | Drain step 0 flips `/ready` to 503 so load balancers stop routing before the HTTP drain; liveness stays up so orchestrators don't kill the draining process |
| Narrow `OnPreDrain` before lifecycle cancellation | A small class of coordination work must finish while lifecycle-bound workers and DI are still live. Unordered hooks provide a hard teardown barrier after readiness withdrawal: deadline expiry is reported, but cancellation and infrastructure teardown cannot race an unfinished hook. Rejected: abandoning a hook at the deadline, which breaks the live-dependency guarantee; moving lifecycle cancellation after all HTTP/OnDrain work, which would keep unrelated workers live for the whole drain; and using `OnDrain`, whose established contract begins after cancellation. |
| Narrow `OnDrain`, lifecycle-`Service` still deferred | On completed drains, `OnDrain` provides the handlers-before-DI proof; incomplete drains are identified and teardown proceeds. The unordered pre-infrastructure hook is concrete and public for WebSocket. Background startup/restartability is different: `worker.Pool` still uses lifecycle cancellation + DI `Shutdowner`, while a general `Service` taxonomy waits for multiple concrete consumers. Future gRPC/pubsub may use OnDrain without implying restart support. |
| No post-compile hook registration | Frozen guard prevents race conditions |
| Sequential LIFO shutdown hooks | Deterministic, debuggable. Parallel via user `errgroup` in single hook |
| FIFO for OnStart | Natural execution order — symmetric with LIFO shutdown |
| OnStart fail-fast | Startup hooks are sequential and dependent — the first error aborts the rest and triggers full teardown |
| OnStart before stateRunning | Hooks run in `stateStarting` — avoids race with `Shutdown()` which requires `stateRunning` |
| `stateStarting` transient state | Closes race window between CAS and server field writes — Shutdown cannot read nil fields |
| `http.ErrServerClosed` → nil | Graceful shutdown is not an error condition |
| Pre-session failure → building | Preflight/listen errors start nothing — rolling back to `building` keeps the App retryable |
| Session failure → terminal stopped | An OnStart-hook error or a post-running serve error runs the full teardown (OnPreDrain → lifecycle cancellation → HTTP/OnDrain → DI shutdown → OnShutdown) and ends `stopped`; a session that began may hold side effects, and `building` is unsound once `container.Shutdown` has run. Rejected: uniform rollback to `building`, which skipped teardown and leaked started resources |

## Consequences

**Positive:**

- Zero-boilerplate graceful shutdown: `Run` handles signals and applies the `WithShutdownTimeout` deadline (except that a cancellation-ignoring `OnPreDrain` remains a hard safety barrier until it returns)
- Readiness flips to 503 at shutdown start, so load balancers drain the instance before it stops accepting
- Deterministic startup/shutdown sequence
- Background services get clean shutdown signal via lifecycle context (delivered through `OnStart`)
- Narrow coordination work can finish while lifecycle workers and DI are still live through `OnPreDrain`
- Successfully drained subsystems can prove their active handlers stop before DI infrastructure through `OnDrain`
- LIFO hooks ensure correct resource cleanup order
- Frozen guard catches registration bugs at development time
- State machine prevents double-run and double-shutdown
- Startup and runtime serve failures enter the same teardown chain as graceful shutdown, so cleanup of started resources is attempted instead of skipped; shared deadline semantics still apply

**Negative:**

- Advanced signal needs (custom signal sets, multi-server coordination) use `RunContext` with the caller's own `signal.NotifyContext` — the default `Run` covers SIGINT/SIGTERM
- A cancellation-ignoring `OnPreDrain` hook can delay shutdown beyond its deadline; Credo reports the breach but preserves the teardown barrier because cancelling workers or DI early would violate the hook's purpose
- Sequential hooks may slow shutdown if a hook is slow (mitigate: deadline ctx)
- No restart capability — must create new App after shutdown
