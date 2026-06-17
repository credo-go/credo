# ADR-006: Application Lifecycle

**Status:** Accepted
**Date:** 2026-03-01
**Revised:** 2026-06-18 — context-aware run APIs (`RunContext`, `RunTLSContext`,
`ServeContext`), signal-aware `Run`/`RunTLS` default, `WithShutdownTimeout`,
`App.Context()` accessor removed, single-use App; `RunWithSignals` /
`RunTLSWithSignals` removed.
**Depends on:** ADR-001

## Context

An enterprise framework (ADR-001) must provide a well-defined application
lifecycle: startup, runtime, and graceful shutdown. Background services
(workers, pub/sub subscribers, gRPC servers) need a signal to stop
accepting work. In-flight HTTP requests need time to complete. Shutdown
hooks must release resources (DB connections, caches, file handles) in a
deterministic order.

Go's stdlib `*http.Server` provides `Shutdown(ctx)` for HTTP drain but
has no concept of application-level context, lifecycle state, or shutdown
hooks. Credo fills this gap.

## Decision

### Lifecycle State Machine

```
building → starting → running → stopping → stopped
              ↓ (listen error)
           building
```

| State | Meaning |
|-------|---------|
| `building` | Initial. Route/middleware registration allowed |
| `starting` | Transient. CAS claimed, ctx/server being written. Shutdown blocked |
| `running` | Port bound, server accepting. Registration frozen. Shutdown allowed |
| `stopping` | Draining HTTP, running hooks |
| `stopped` | Fully stopped |

State is stored as `atomic.Uint32` with `CompareAndSwap` transitions —
no mutex on the hot path.

### API

```go
app.Run()                                  // Serve HTTP; block on SIGINT/SIGTERM, then drain
app.RunTLS(certFile, keyFile)              // Serve HTTPS; same signal handling
app.RunContext(ctx)                        // Serve HTTP; caller-driven cancellation, no signals
app.RunTLSContext(ctx, certFile, keyFile)  // Serve HTTPS; caller-driven cancellation
app.ServeContext(ctx, l)                   // Serve on a caller-provided net.Listener
app.Shutdown(ctx)                          // Graceful shutdown with deadline
app.State() string                         // Current state name
app.IsRunning() bool                       // Convenience check
app.Addr() net.Addr                        // Actual bound address (nil before Run)
app.OnStart(fn func(ctx context.Context) error)     // FIFO startup hook
app.OnShutdown(fn func(ctx context.Context) error)  // LIFO shutdown hook

// Construction option:
credo.WithShutdownTimeout(d)               // Drain budget for signal/cancel shutdown (default 30s)
```

### App Context

The app-level context is created at `Run()`/`RunContext()` and cancelled at the
**beginning** of `Shutdown()`. There is **no** public `Context()` accessor: the
previous nullable accessor returned `context.Background()` before `Run`, a silent
dead zone for any goroutine that captured it too early. Background services
receive the context through their `OnStart` hook's `ctx` parameter and select on
`ctx.Done()` to detect shutdown:

```go
app.OnStart(func(ctx context.Context) error {
    go func() {
        for {
            select {
            case <-ctx.Done():
                return
            case msg := <-subscriber.Messages():
                process(msg)
            }
        }
    }()
    return nil
})
```

Removing the accessor makes the dead zone structurally unreachable: there is no
pre-`Run` context to capture. (A dedicated background-service abstraction that
receives this context directly is planned.)

### Startup Sequence

```
1. DI Finalize (idempotent)
2. CAS building → starting
3. Create app context
4. Bind port (net.Listen)
5. Store bound address (app.Addr() now returns the real address)
6. OnStart hooks in FIFO order (first registered, first called)
7. Store state = running
```

If any OnStart hook returns an error, startup aborts: state rolls back
to `building`, the listener is closed, and `Run` returns the error.

### Shutdown Sequence

```
1. CAS running → stopping
2. Cancel app context (signals background services)
3. HTTP server drain (srv.Shutdown(ctx))
4. DI container shutdown (reverse-order singleton cleanup)
5. OnShutdown hooks in LIFO order (last registered, first called)
6. Clear bound address
7. Store state = stopped
```

All errors are collected via `errors.Join` — no early return.

### Lifecycle Hooks

**OnStart** — called after the port is bound, before the server accepts connections:

```go
app.OnStart(func(ctx context.Context) error {
    log.Println("server ready on", app.Addr())
    return consul.Register(ctx, app.Addr())
})
```

- FIFO order (first registered, first called)
- Receives the app context (created at `Run` time)
- Fail-fast: first error aborts startup, remaining hooks are skipped
- Must be called before `Run()`; panics after compile (frozen guard)

**OnShutdown** — called during graceful shutdown:

```go
app.OnShutdown(func(ctx context.Context) error {
    return db.Close()
})
```

- Hooks receive the shutdown deadline context from `Shutdown(ctx)`
- LIFO order (reverse registration order) — resources opened last are
  closed first
- Must be called before `Run()`; panics after compile (frozen guard)
- Sequential execution — for parallel shutdown, wrap in a single hook
  with `errgroup`

### Frozen Guard

After `compile()` (triggered by first `ServeHTTP` or `Run`), the app
is frozen. Late registration of routes, middleware, meta, status handlers,
or shutdown hooks panics with a clear message. This prevents subtle race
conditions from concurrent registration during serving.

### Design Decisions

| Decision | Rationale |
|----------|-----------|
| Signal-aware `Run` default | `Run`/`RunTLS` handle SIGINT/SIGTERM and drain gracefully — the common case needs no boilerplate. `RunContext`/`RunTLSContext`/`ServeContext` give callers full control with no signal handler (tests, embedding, custom signal sets) |
| `Run` not a naive signal wrapper | `stop()` runs the instant the first signal arrives, *before* the drain — so a second signal force-kills (standard two-stage Ctrl+C). A `defer stop(); RunContext(ctx)` wrapper would swallow it |
| One drain mechanism, CAS-idempotent | Signal, context-cancel, and explicit `Shutdown` share one `initiateShutdown`; the `running`→`stopping` CAS (not a parallel `sync.Once`) makes concurrent triggers safe |
| Single-use App | Terminal `stopped` state; re-run returns an error. Re-run was already broken (latched component flags); `New()` is the restart path |
| TLS cert preflight | `RunTLS`/`RunTLSContext` load the key pair before `stateRunning`, so a bad cert fails fast with listen-error rollback discipline |
| No post-compile hook registration | Frozen guard prevents race conditions |
| Sequential LIFO shutdown hooks | Deterministic, debuggable. Parallel via user `errgroup` in single hook |
| FIFO for OnStart | Natural execution order — symmetric with LIFO shutdown |
| OnStart fail-fast | Startup hooks are sequential and dependent — first error aborts |
| OnStart before stateRunning | Hooks run in `stateStarting` — avoids race with `Shutdown()` which requires `stateRunning` |
| `stateStarting` transient state | Closes race window between CAS and server field writes — Shutdown cannot read nil fields |
| `http.ErrServerClosed` → nil | Graceful shutdown is not an error condition |
| Listen error → rollback to building | Failed run allows retry |

## Consequences

**Positive:**
- Zero-boilerplate graceful shutdown: `Run` handles signals and drains within `WithShutdownTimeout`
- Deterministic startup/shutdown sequence
- Background services get clean shutdown signal via app context (delivered through `OnStart`)
- LIFO hooks ensure correct resource cleanup order
- Frozen guard catches registration bugs at development time
- State machine prevents double-run and double-shutdown

**Negative:**
- Advanced signal needs (custom signal sets, multi-server coordination) use `RunContext` with the caller's own `signal.NotifyContext` — the default `Run` covers SIGINT/SIGTERM
- Sequential hooks may slow shutdown if a hook is slow (mitigate: deadline ctx)
- No restart capability — must create new App after shutdown
