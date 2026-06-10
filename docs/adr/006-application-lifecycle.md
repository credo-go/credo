# ADR-006: Application Lifecycle

**Status:** Accepted
**Date:** 2026-03-01
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
app.Run()                                  // Start HTTP (addr from server config)
app.RunTLS(certFile, keyFile)              // Start HTTPS
app.Shutdown(ctx)                          // Graceful shutdown with deadline
app.State() string                         // Current state name
app.IsRunning() bool                       // Convenience check
app.Context() context.Context              // App-level context
app.Addr() net.Addr                        // Actual bound address (nil before Run)
app.OnStart(fn func(ctx context.Context) error)    // FIFO startup hook
app.OnShutdown(fn func(ctx context.Context) error)  // LIFO shutdown hook

// Convenience (package-level):
credo.RunWithSignals(app, timeout, signals...)                    // Run + signal + Shutdown
credo.RunTLSWithSignals(app, cert, key, timeout, signals...)     // RunTLS + signal + Shutdown
```

### App Context

`app.Context()` returns an app-level context created at `Run()` and
cancelled at the **beginning** of `Shutdown()`. Background services select
on `ctx.Done()` to detect shutdown:

```go
go func() {
    ctx := app.Context()
    for {
        select {
        case <-ctx.Done():
            return
        case msg := <-subscriber.Messages():
            process(msg)
        }
    }
}()
```

Before `Run()` returns, `Context()` returns `context.Background()`
(never cancelled) to avoid nil panics.

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
| Optional signal handling | Credo is a library — signal handling defaults to the caller. `RunWithSignals` / `RunTLSWithSignals` provide an opt-in convenience wrapper |
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
- Deterministic startup/shutdown sequence
- Background services get clean shutdown signal via app context
- LIFO hooks ensure correct resource cleanup order
- Frozen guard catches registration bugs at development time
- State machine prevents double-run and double-shutdown

**Negative:**
- Manual signal handling still needed for advanced cases (custom signal sets, multi-server coordination)
- Sequential hooks may slow shutdown if a hook is slow (mitigate: deadline ctx)
- No restart capability — must create new App after shutdown
