# Lifecycle Spec

> Status: **Implemented** (Phase 2.5, updated Phase 3+) **ADRs**: [005-configuration-architecture](../adr/005-configuration-architecture.md), [006-application-lifecycle](../adr/006-application-lifecycle.md)

## Overview

Credo uses a state machine to govern the application lifecycle. This prevents undefined behavior from late route/middleware registration and enables graceful shutdown with in-flight request draining.

## State Machine

```
             compile()          Run()                                  Shutdown()
  building ----[frozen]----> starting ---[OnStart]---> running ---> stopping ---> stopped
      |                         |             |           |                         ^
      | ServeHTTP()             |             |           | serve error             |
      | (compile only,          |             |           |  (non-graceful)         |
      |  state unchanged)       |             |           └────────── drain ────────┤
      v                         |             |                                     |
  [frozen=true]                 |             └─────────── OnStart error ── drain ──┘
                                |                                            → stopped
                                └─ preflight / listen error → building (retryable)
```

Failures split by how far startup got. A **pre-session** failure (TLS preflight or listener bind) starts nothing, so it rolls back to `building` and the App may run again. A **session** failure — an OnStart hook returning an error, or a non-`ErrServerClosed` error from `Serve` after `running` — runs the full teardown chain (the drain shared with graceful shutdown) and ends in the terminal `stopped` state. A second `Run` after shutdown and a second `Shutdown` both return an error (state unchanged).

### States

| State | Value | Description |
| --- | --- | --- |
| `building` | 0 | Initial state. Route/MW/hook registration allowed. |
| `starting` | 1 | Transient startup state. Run claimed; server/ctx being written; OnStart hooks executing. |
| `running` | 2 | Server is listening. Registration frozen. |
| `stopping` | 3 | Draining in-flight requests + running hooks. |
| `stopped` | 4 | Fully stopped. Terminal state — reached by graceful shutdown or by a session-failure teardown (OnStart hook error / post-running serve error). |

## `frozen` vs `state`

Two separate flags exist because `ServeHTTP` and `Run` serve different purposes:

- **`frozen` (atomic.Bool)**: Set by `compile()`. Prevents route/middleware registration after the handler chain is built. Triggered by both `ServeHTTP` (for standalone `httptest` usage) and `Run`.

- **`state` (atomic.Uint32)**: Tracks server lifecycle. Only `Run()` transitions to `running`. A user who calls `ServeHTTP` directly (with their own `*http.Server`) stays in `building` state — they manage their own lifecycle.

This separation allows:

1. `httptest.NewServer(app)` — compiles (freezes) but doesn't change state.
2. `app.Run()` — compiles, freezes, AND enters running state.

## API

### `app.State() string`

Returns the current lifecycle state as a human-readable string.

### `app.IsRunning() bool`

Reports whether the server is in the `running` state.

### `app.Run() error`

Compiles the handler chain, transitions to `running`, and serves HTTP until an interrupt (Ctrl+C) or `SIGTERM` arrives, then performs graceful shutdown bounded by `WithShutdownTimeout`. A second signal during shutdown force-kills the process — signal handling is reset the moment the first signal arrives. Server address is derived from framework-internal server config (host + port). Returns `nil` on graceful shutdown, or an error if the server fails to start or the app has already run.

`Run` is the safe default for a process whose lifetime is the server's. For explicit lifecycle control — tests, embedding, caller-driven cancellation — use `RunContext`.

### `app.RunTLS(certFile, keyFile string) error`

TLS sibling of `Run`: serves HTTPS under the same signal handling. The certificate and key are validated **before** the server starts accepting connections, so a bad key pair fails fast (state rolls back to `building`).

### `app.RunContext(ctx context.Context) error`

Like `Run` but installs **no** signal handler — cancellation is entirely the caller's. Serves until `ctx` is cancelled, the server stops, or a programmatic `Shutdown`. On `ctx` cancellation the drain keeps `ctx`'s values but drops its cancellation (so an already-cancelled `ctx` still drains), bounded by `WithShutdownTimeout`. This is the entry point for tests, embedding, and tracing contexts. Cancelling `ctx` **during** startup does not abort an in-progress `OnStart` hook (hooks receive the app context, not `ctx`) — the cancellation takes effect only after all hooks complete; see the `app.OnStart` notes below.

### `app.RunTLSContext(ctx context.Context, certFile, keyFile string) error`

TLS sibling of `RunContext`: caller-driven cancellation, no signal handler. Same fail-fast certificate preflight as `RunTLS`.

### `app.ServeContext(ctx context.Context, l net.Listener) error`

Serves on a caller-provided listener, sharing `RunContext`'s lifecycle. The escape hatch for listeners the framework does not create itself — Unix sockets, a preconfigured test listener, H2C, or an externally managed listener. `ServeContext` takes ownership of `l` and closes it when the server stops (matching `net/http.Server.Serve` semantics). A nil listener returns an error.

The app context (created at `Run`/`RunContext` time, cancelled at the start of shutdown) is no longer exposed by a public accessor. Background services receive it through their `OnStart` hook's `ctx` parameter and select on `ctx.Done()` to detect graceful shutdown.

### `app.Addr() net.Addr`

Returns the actual network address the server is listening on. Particularly useful when the server was started with port 0 (OS-assigned ephemeral port). Returns `nil` before `Run()` or after `Shutdown()`.

### `app.Shutdown(ctx context.Context) error`

Gracefully shuts down the server:

1. Transitions from `running` → `stopping` (CAS; error if not running).
2. Marks the instance **unready** — `/ready` returns 503 (`shutting_down`) so load balancers stop routing here before the drain. Liveness stays up.
3. Cancels app context — signals background services to shut down.
4. Drains in-flight HTTP requests via `http.Server.Shutdown(ctx)`.
5. Shuts down DI container singletons via `container.Shutdown(ctx)`.
6. Calls `OnShutdown` hooks in **LIFO** order, passing `ctx` for deadline awareness.
7. Collects all errors via `errors.Join`.
8. Clears bound address (`Addr()` returns nil).
9. Transitions to `stopped`.

The app context is cancelled **before** HTTP drain to give background services maximum lead time for shutdown.

`Shutdown` is the single drain mechanism shared by every entry point. The signal-triggered drain of `Run`/`RunTLS` and the cancellation-triggered drain of `RunContext`/`RunTLSContext`/`ServeContext` run this exact sequence, made idempotent by the `running` → `stopping` CAS — a cancelled context racing a programmatic `Shutdown` cannot run the sequence twice (the loser is a no-op). Idempotency comes from that one CAS, not a parallel `sync.Once`.

#### Drain context derivation

An explicit `Shutdown(ctx)` honours the caller's `ctx` deadline as-is. Signal- and cancellation-triggered drains instead derive a bounded context from `WithShutdownTimeout` (default 30s):

| Trigger | Drain context |
| --- | --- |
| Signal (`Run`, `RunTLS`) | `context.Background()` + `WithShutdownTimeout` |
| Context cancel (`RunContext`, `RunTLSContext`, `ServeContext`) | `context.WithoutCancel(ctx)` + `WithShutdownTimeout` — keeps caller values, drops cancellation |
| Session failure (OnStart hook error / post-running serve error) | `context.Background()` + `WithShutdownTimeout` |
| Explicit `Shutdown(ctx)` | the caller's `ctx`, unchanged |

#### Single-use App

An App is single-use: `New → Run → Shutdown → discard`. Once it reaches `stopping`/`stopped`, any further `Run`/`RunContext`/`RunTLS`/`RunTLSContext`/ `ServeContext` call returns an error (`app cannot be run after shutdown; create a new App`). Tests that need a fresh server create a new `App` with `New()`. Re-run is intentionally unsupported: background components (e.g. `worker.Pool`) latch a started flag and would not reset cleanly on a second run.

#### Background services and shutdown ordering

Background work is wired through the existing primitives: a component starts in an `OnStart` hook (receiving the app context) and stops by implementing `Shutdowner`, so the DI container drains it during the container-shutdown step. The `worker.Pool` follows exactly this pattern.

A dedicated lifecycle-`Service` abstraction — a `Run(ctx)`/`Name()` seam with a guaranteed _services-drain-before-infrastructure_ phase (so a worker can still reach the database while it winds down) and a restartable/start-once taxonomy — is deliberately **deferred** until there are in-tree consumers (gRPC server, WebSocket hub, pub/sub subscriber). Introducing that public surface now, for packages that are still placeholders, would be the kind of speculative carrier the framework avoids pre-v1. Until then, services-before-infra ordering within the container step follows reverse registration order.

### `app.OnStart(fn func(ctx context.Context) error)`

Registers a startup hook. Hooks are called in **FIFO** order after the port is bound but before the server starts accepting connections (state is still `starting`). The `ctx` parameter is the app context (created at `Run` time).

The hook `ctx` is the **app context** — created from `context.Background()` and cancelled at shutdown start — not the `ctx` passed to `RunContext`/`RunTLSContext`. Cancelling the `RunContext` context **during** startup therefore does not cancel a running `OnStart` hook: a long hook (e.g. a migration) runs to completion, and the caller's cancellation is observed only **after** all hooks finish, at which point the app starts and then immediately begins graceful shutdown. This is deliberate — a background service spawned in a hook should bind to the app-lifecycle context (uniform across `Run`/`RunContext`/`ServeContext`), not the caller's startup-scoped context. If you need a hook the caller can abort mid-flight, capture that context in the hook closure and select on it yourself.

If any hook returns an error, startup aborts: remaining hooks are skipped (fail-fast), the App runs the full teardown chain (cancel app context → HTTP drain → DI container shutdown → OnShutdown hooks), the listener is closed, and `Run` returns the hook error (joined with any teardown error). The App ends in the terminal `stopped` state, not `building` — an earlier hook may already have started workers, acquired a migration lock, or opened a subscription, so a session that began tears down rather than rolling back (ADR-006). The drain runs directly (state is `starting`, where `Shutdown` cannot race it), bounded by `WithShutdownTimeout`.

`app.Addr()` is available inside hooks — critical for port-0 scenarios.

Typical uses: cache warm-up, database migrations. The `store/sqldb` migration wrapper's `Migrate` method matches this hook signature, so opt-in auto-migration is `app.OnStart(db.Migrate)` (see the [Store Spec](store.md)).

Must be called before `compile()` (panics if frozen).

### `app.OnShutdown(fn func(ctx context.Context) error)`

Registers a shutdown hook. Hooks are called in LIFO order during `Shutdown`. The `ctx` parameter carries the shutdown deadline from `Shutdown(ctx)`. Must be called before `compile()` (panics if frozen).

OnShutdown hooks run on **every** teardown, including a failed startup (an OnStart hook erroring after an earlier one ran). OnShutdown is therefore the session teardown point, not an OnStart mirror: hooks must be idempotent and must not assume any particular OnStart hook completed. (Because `onStart` and `onShutdown` are independent lists — not paired by index — a hook running without its conceptual counterpart was always possible; session-failure teardown only makes it routine.)

### `credo.WithShutdownTimeout(d time.Duration) Option`

Construction option (passed to `New`) setting the graceful-shutdown drain budget for the signal-aware `Run`/`RunTLS` and the cancellation-triggered `RunContext`/`RunTLSContext`/`ServeContext`. Zero (the default) applies 30s. An explicit `Shutdown(ctx)` ignores it and honours the caller's deadline instead. Also settable via the `server.shutdown_timeout` config key.

## Registration Guards

The following methods panic if called after `compile()`:

| Method | Guard |
| --- | --- |
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

The same fail-fast policy governs all registration APIs: misconfiguration (nil handlers, malformed patterns, duplicates) panics at startup, while operations that touch the outside world (request handling, file I/O such as `UseI18n` locale loading) return errors. See the package documentation's "Panics and Errors" section.

## Thread Safety

- `state` and `frozen` use `sync/atomic` — safe for concurrent reads.
- `server`, `ctx`, `cancel`, and `boundAddr` fields protected by `serverMu` mutex.
- `compile()` guarded by `sync.Once`.
- State transitions use `CompareAndSwap` — exactly one goroutine wins.

## Container Integration

Shutdown hooks bridge the container's `Shutdowner` interface with the app lifecycle. The shutdown context is propagated for deadline-aware cleanup:

```go
app.OnShutdown(func(ctx context.Context) error {
    return c.Shutdown(ctx)
})
```

This pattern ensures DI-managed resources (DB connections, caches) are cleaned up during graceful shutdown with deadline awareness.
