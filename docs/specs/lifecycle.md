# Lifecycle Spec

> Status: **Implemented** (Phase 2.5, updated Phase 3+)
> **ADRs**: [005-configuration-architecture](../adr/005-configuration-architecture.md),
> [006-application-lifecycle](../adr/006-application-lifecycle.md)

## Overview

Credo uses a state machine to govern the application lifecycle. This prevents
undefined behavior from late route/middleware registration and enables
graceful shutdown with in-flight request draining.

## State Machine

```
             compile()             Run()                                          Shutdown()
  building ------------ [frozen] ---> starting ---> [OnStart] ---> running ---> stopping ---> stopped
      |                                  |              |             |                          |
      | ServeHTTP()                      | listen       | hook        | 2nd Run()                | 2nd Shutdown()
      | (compile only,                   | error        | error       | -> error                  | -> error
      |  state unchanged)                v              v             v                          v
      v                               building       building      [error]                    [error]
  [frozen=true]
```

### States

| State | Value | Description |
|-------|-------|-------------|
| `building` | 0 | Initial state. Route/MW/hook registration allowed. |
| `starting` | 1 | Transient startup state. Run claimed; server/ctx being written; OnStart hooks executing. |
| `running` | 2 | Server is listening. Registration frozen. |
| `stopping` | 3 | Draining in-flight requests + running hooks. |
| `stopped` | 4 | Fully stopped. Terminal state. |

## `frozen` vs `state`

Two separate flags exist because `ServeHTTP` and `Run` serve different purposes:

- **`frozen` (atomic.Bool)**: Set by `compile()`. Prevents route/middleware
  registration after the handler chain is built. Triggered by both `ServeHTTP`
  (for standalone `httptest` usage) and `Run`.

- **`state` (atomic.Uint32)**: Tracks server lifecycle. Only `Run()` transitions
  to `running`. A user who calls `ServeHTTP` directly (with their own
  `*http.Server`) stays in `building` state — they manage their own lifecycle.

This separation allows:
1. `httptest.NewServer(app)` — compiles (freezes) but doesn't change state.
2. `app.Run()` — compiles, freezes, AND enters running state.

## API

### `app.State() string`

Returns the current lifecycle state as a human-readable string.

### `app.IsRunning() bool`

Reports whether the server is in the `running` state.

### `app.Run() error`

Compiles the handler chain, transitions to `running`, and starts an HTTP server.
Server address is derived from framework-internal server config (host + port).
Returns `nil` on graceful shutdown (via `Shutdown`), or an error if the server
fails to start or is already running.

### `app.RunTLS(certFile, keyFile string) error`

Same as `Run` but starts an HTTPS server. Server address from framework-internal server config.

### `app.Context() context.Context`

Returns the app-level context. Created at `Run()` time via
`context.WithCancel(context.Background())`. Cancelled at the beginning of
`Shutdown()`. Background services (workers, pub/sub, gRPC) should select on
`ctx.Done()` to detect graceful shutdown.

Returns `context.Background()` if `Run()` has not been called yet.

For manual signal handling, use `signal.NotifyContext` with `app.Shutdown`.
For the common case, use the convenience wrappers below.

### `app.Addr() net.Addr`

Returns the actual network address the server is listening on. Particularly
useful when the server was started with port 0 (OS-assigned ephemeral port).
Returns `nil` before `Run()` or after `Shutdown()`.

### `app.Shutdown(ctx context.Context) error`

Gracefully shuts down the server:

1. Transitions from `running` → `stopping` (CAS; error if not running).
2. Cancels app context — signals background services to shut down.
3. Drains in-flight HTTP requests via `http.Server.Shutdown(ctx)`.
4. Shuts down DI container singletons via `container.Shutdown(ctx)`.
5. Calls `OnShutdown` hooks in **LIFO** order, passing `ctx` for deadline awareness.
6. Collects all errors via `errors.Join`.
7. Clears bound address (`Addr()` returns nil).
8. Transitions to `stopped`.

The app context is cancelled **before** HTTP drain to give background services
maximum lead time for shutdown.

### `app.OnStart(fn func(ctx context.Context) error)`

Registers a startup hook. Hooks are called in **FIFO** order after the port
is bound but before the server starts accepting connections (state is still
`starting`). The `ctx` parameter is the app context (created at `Run` time).

If any hook returns an error, startup aborts: state rolls back to `building`,
the listener is closed, and `Run` returns the error. Remaining hooks are
skipped (fail-fast).

`app.Addr()` is available inside hooks — critical for port-0 scenarios.

Typical uses: cache warm-up, database migrations. The `store/sqldb`
migration wrapper's `Migrate` method matches this hook signature, so
opt-in auto-migration is `app.OnStart(db.Migrate)` (see the
[Store Spec](store.md)).

Must be called before `compile()` (panics if frozen).

### `app.OnShutdown(fn func(ctx context.Context) error)`

Registers a shutdown hook. Hooks are called in LIFO order during `Shutdown`.
The `ctx` parameter carries the shutdown deadline from `Shutdown(ctx)`.
Must be called before `compile()` (panics if frozen).

### `credo.RunWithSignals(app *App, timeout time.Duration, signals ...os.Signal) error`

Convenience wrapper: starts the HTTP server, blocks until a signal is received,
then calls `app.Shutdown` with a deadline context derived from `timeout`.

- Default signals (when none provided): `os.Interrupt` + `syscall.SIGTERM`
- If `app.Run()` fails before a signal, the error is returned immediately.
- A second signal terminates the process (signal handling is reset after the
  first signal).

### `credo.RunTLSWithSignals(app *App, certFile, keyFile string, timeout time.Duration, signals ...os.Signal) error`

TLS variant of `RunWithSignals`. Same behavior, uses `app.RunTLS` internally.

## Registration Guards

The following methods panic if called after `compile()`:

| Method | Guard |
|--------|-------|
| `app.GlobalMiddleware()` | `checkFrozen("GlobalMiddleware")` |
| `app.GET/POST/PUT/...()` (and `group.*`) | `checkFrozen("route registration")` (via `addRoute`) |
| `app.Host()` | `checkFrozen("host registration")` |
| `app.Mount()` | `checkFrozen("Mount")` |
| `app.Static()` / `app.File()` (and `group.*`) | `checkFrozen("Static")` / `checkFrozen("File")` |
| `app.StatusHandler()` | `checkFrozen("StatusHandler")` |
| `app.SetErrorRenderer()` | `checkFrozen("SetErrorRenderer")` |
| `app.SetMeta()` / `app.RemoveMeta()` | `checkFrozen("SetMeta")` / `checkFrozen("RemoveMeta")` |
| `app.UseHealth()` | `checkFrozen("UseHealth")` |
| `app.UseI18n()` | `checkFrozen("UseI18n")` |
| `app.OnStart()` | `checkFrozen("OnStart")` |
| `app.OnShutdown()` | `checkFrozen("OnShutdown")` |
| `group.Middleware()` | `checkFrozen("Group.Middleware")` |
| `group.SetMeta()` / `group.RemoveMeta()` | `checkFrozen("Group.SetMeta")` / `checkFrozen("Group.RemoveMeta")` |
| `route.Name()` / `route.SetMeta()` / `route.Middleware()` | `checkFrozen("Route.Name")` / `checkFrozen("Route.SetMeta")` / `checkFrozen("Route.Middleware")` |

The same fail-fast policy governs all registration APIs: misconfiguration
(nil handlers, malformed patterns, duplicates) panics at startup, while
operations that touch the outside world (request handling, file I/O such as
`UseI18n` locale loading) return errors. See the package documentation's
"Panics and Errors" section.

## Thread Safety

- `state` and `frozen` use `sync/atomic` — safe for concurrent reads.
- `server`, `ctx`, `cancel`, and `boundAddr` fields protected by `serverMu` mutex.
- `compile()` guarded by `sync.Once`.
- State transitions use `CompareAndSwap` — exactly one goroutine wins.

## Container Integration

Shutdown hooks bridge the container's `Shutdowner` interface with the
app lifecycle. The shutdown context is propagated for deadline-aware cleanup:

```go
app.OnShutdown(func(ctx context.Context) error {
    return c.Shutdown(ctx)
})
```

This pattern ensures DI-managed resources (DB connections, caches) are
cleaned up during graceful shutdown with deadline awareness.
