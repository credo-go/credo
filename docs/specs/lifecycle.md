# Lifecycle Spec

> Status: **Implemented** (Phase 2.5, updated Phase 3+); reload surface **Implemented** (Phase 3.8); v0.24.0 decisions accepted, pending implementation ([plan](../plans/components-and-sequential-bootstrap.md)) **ADRs**: [005-configuration-architecture](../adr/005-configuration-architecture.md), [006-application-lifecycle](../adr/006-application-lifecycle.md), [020-reload-and-partial-config-reload](../adr/020-reload-and-partial-config-reload.md), [024-lifecycle-components](../adr/024-lifecycle-components.md)

## Overview

Credo uses a state machine to govern the application lifecycle. This prevents undefined behavior from late route/middleware registration and enables graceful shutdown with in-flight request draining.

## State Machine

```
            Run() / RunContext() / ServeContext() / App.Start()
  building ---[claim]---> starting ---[prepare, bind*, start walk]---> running ---> stopping ---> stopped
      |                      |    |                                       |                         ^
      | ServeHTTP()          |    | start failure ──── rollback ──────────┼─────────────────────────┤
      | (prepare; refused    |    | shutdown requested ─ interrupt,       | serve error / Shutdown  |
      |  while the App has   |    |                      rollback ────────┼──── drain in tiers ─────┤
      |  anything to start)  |    |                                       └─────────────────────────┘
      |                      └─ prepare / preflight / listen error → building (retryable*)
      |
      └── Shutdown() in building ── drain with no servers (bootstrap teardown) ──────────────────────┘

  * bind: managed serving only; App.Start runs the start walk without a listener
```

Failures split by how far startup got. A **pre-session** failure (preparation, TLS preflight or listener bind) starts nothing, so it rolls back to `building` and the App may run again — except that a failed preparation is terminal: it is stored, every later serve attempt returns the same error, and the frozen DI plan cannot be repaired. Returning to `building` permits bootstrap cleanup through `Shutdown`, not a retry of a failed plan. A [failed start](#a-failed-start) — a `Start`, a start hook or a start-walk constructor that fails — and an [interrupted start](#a-shutdown-during-the-start-phase) roll back what was built and end in the terminal `stopped` state, and so does a non-`ErrServerClosed` error from `Serve` after `running`, which runs the [drain in tiers](#shutdown-in-tiers). A second `Run` after shutdown, and a `Shutdown` in `stopping` or `stopped`, return an error (state unchanged).

### States

| State | Value | Description |
| --- | --- | --- |
| `building` | 0 | Initial state. Registration follows the bootstrap order ([bootstrap spec](bootstrap-and-di-lifecycle.md)) until preparation. `Shutdown` is bootstrap teardown. |
| `starting` | 1 | The start phase: preparation, listener bind (managed serving), then the [start walk](#the-start-walk). `Shutdown` is accepted and [interrupts it](#a-shutdown-during-the-start-phase). |
| `running` | 2 | The start walk succeeded. Managed serving accepts; after `App.Start` the App is served through `ServeHTTP` by its owner's server and `Addr()` stays nil. |
| `stopping` | 3 | Readiness withdrawn; the [drain in tiers](#shutdown-in-tiers), or the rollback of a failed or interrupted start. |
| `stopped` | 4 | Terminal — reached by graceful shutdown, bootstrap teardown, a failed or interrupted start, or a post-running serve error. New requests receive the 503 envelope. |

`App.Start` claims the same start slot as the serve entry points, so the single-use rule covers it: a second `App.Start`, or `Run`, `RunContext` or `ServeContext` after it, returns the state error.

## `frozen` vs `state`

Two separate flags exist because `ServeHTTP` and `Run` serve different purposes:

- **`frozen` (atomic.Bool)**: The HTTP write gate. Set at preparation admission — the first direct `ServeHTTP` request or a managed serve entry point — and at bootstrap-shutdown admission. Prevents route/middleware/hook/feature registration after the handler chain is (about to be) built or the App is being torn down. An explicit DI-only `Finalize` does **not** set it.

- **`state` (atomic.Uint32)**: Tracks the lifecycle. Only the managed serve entry points and `App.Start` transition to `running`. A user who calls `ServeHTTP` directly without `App.Start` (with their own `*http.Server` or `httptest`) stays in `building` — possible only for an App with nothing to start ([the start gate](#the-start-gate-on-servehttp)) — manages their own server's lifecycle, and uses `Shutdown` in `building` to release what bootstrap built.

This separation allows:

1. `httptest.NewServer(app)` — prepares (freezes HTTP writes) on the first request but doesn't change state; an App with something to start calls `app.Start` first.
2. `app.Run()` — claims the start slot, prepares, freezes, runs the start walk AND enters running state.
3. `app.Start(ctx)` — the same without a listener, for an App served through `ServeHTTP`.
4. `app.Finalize()` followed by controller resolution and route binding — DI frozen, HTTP writes still open.

## Preparation

Every serve path reaches the same validated runtime model through one shared preparation step — `Finalize` → `compile` → publish — whose result (handler or error) is stored exactly once in `app.prep` (an atomic pointer with a fast path; `prepMu` serializes the slow path):

- **Admission.** Preparation is admitted only while the state is below `stopping`; admission sets `frozen`. A preparation that loses to shutdown before publishing discards its result. Once stored, the result is final: a DI finalize error or a compile panic (middleware construction included) is recorded as a terminal preparation failure, logged with its stack (`credo: preparation failed`), and never retried.
- **Managed serving** (`Run`, `RunContext`, `ServeContext`) and `App.Start` claim `building → starting` first, then prepare. A preparation failure rolls the state back to `building` and returns the error (`credo: Run: prepare: …`); a later serve attempt returns the same stored error without executing a partly compiled handler.
- **Direct `ServeHTTP`** prepares on the first request without claiming the start slot. While lifecycle admission is open it panics with the stored preparation error on every request — a graph or compile error is developer misuse under the package's panic-vs-error policy, and the stored result is what makes the panic repeatable rather than a `sync.Once` that would count a panicking call as done. An App with something to start additionally refuses to serve until its start phase has succeeded ([below](#the-start-gate-on-servehttp)).
- **Lifecycle rejection.** `ServeHTTP` checks the state on every call, before the cached result: in `stopped`, and in `stopping` when no handler was ever prepared, the request receives the callback-free 503 below without preparing, resolving or dispatching, and the stored result is untouched. A handler prepared before the drain keeps serving during `stopping`, which is what the managed HTTP drain, readiness (`/ready` → 503 `shutting_down`) and liveness rely on.

### The start gate on `ServeHTTP`

An App **has something to start** when it has a component with `Start` or `Ready` (planned from its binding's type, or handed to `app.Manage`), a constructor handed to `app.Manage`, or a start hook. Such an App refuses to serve until the start phase has completed successfully:

| When `ServeHTTP` is called | App with something to start | App with nothing to start |
| --- | --- | --- |
| Before `App.Start`, or while it runs | panics with `credo: ServeHTTP: the App has components or start hooks to start and has not been started; …`, naming `App.Start`, `testutil.Start`, `Run`/`RunContext`/`ServeContext` and `parent.Manage(child)` | prepares and serves, as above |
| After `App.Start` succeeded (`running`, `stopping` with a prepared handler) | serves | serves |
| `stopped` — after a failed or interrupted `App.Start`, or after shutdown — or `stopping` without a successful start | the callback-free 503 envelope, without touching DI | the 503 envelope in `stopped`, and in `stopping` without a prepared handler ([Lifecycle rejection](#lifecycle-rejection-503)) |
| Preparation failed | panics with the stored preparation error on every request | panics with the stored preparation error on every request |

The panic is the treatment a stored preparation error already gets: serving a handler against dependencies that were not started is developer misuse, not an availability outcome. After a failed `App.Start` the caller already has the error, so the stopped App answers with the 503 envelope rather than panicking. Under managed serving the gate never fires — the listener accepts only after the start walk — except for a child App mounted into a parent without [`parent.Manage(child)`](#mounted-apps), whose first request panics with the named message; the parent's recovery turns it into a 500 and logs it.

**Accepted, pending implementation (v0.24.0, W5):** a store registration ([store spec](store.md)) and i18n catalogs ([i18n spec](i18n.md)) are start work too.

## API

### `app.State() string`

Returns the current lifecycle state as a human-readable string.

### `app.IsRunning() bool`

Reports whether the server is in the `running` state.

### `app.Run() error`

Prepares the App, binds the listener, runs the [start walk](#the-start-walk), transitions to `running`, and serves HTTP — or HTTPS when TLS is configured (see [TLS](#tls)) — until an interrupt (Ctrl+C) or `SIGTERM` arrives, then drains [in tiers](#shutdown-in-tiers) with the deadline set by `WithShutdownTimeout`. A signal during the start phase [interrupts it](#a-shutdown-during-the-start-phase). A second signal force-kills the process — signal handling is reset the moment the first signal arrives. Server address is derived from framework-internal server config (host + port). Returns `nil` on graceful shutdown and after a clean rollback of an interrupted start, a `*LifecycleError` when the start fails or a drain or rollback does not complete, or an error if the server fails to start or the app has already run.

On Unix, `Run` also handles `SIGHUP`: each signal triggers [`app.Reload`](#appreloadctx-contextcontext-error) with the `WithReloadTimeout` budget on its own goroutine, signals arriving during a reload coalesce into at most one follow-up, and a reload failure never stops the server. Because reloads run off the signal loop, a `SIGINT`/`SIGTERM` during a long reload is serviced immediately: the drain starts (cancelling the reload's context and waiting for it before the internal tier stops) and signal delivery is reset, so a second signal force-kills as usual. There is no SIGHUP on Windows; the programmatic `Reload` is the only trigger there.

`credo.WithoutReloadSignals()` disables the reload trigger without changing the rest of `Run`'s signal policy: SIGHUP is still captured — so a stray signal can never fall through to its default action and terminate the process — but is ignored with an Info log line (`credo: reload signal ignored (reload signals disabled)`). SIGINT/SIGTERM handling and programmatic `Reload` are unaffected; the option is a no-op on Windows. Raw Unix signal disposition (an unhandled SIGHUP terminates) remains available via `RunContext`/`ServeContext`, which install no signal handlers.

`Run` is the safe default for a process whose lifetime is the server's. For explicit lifecycle control — tests, embedding, caller-driven cancellation — use `RunContext`.

### `app.RunContext(ctx context.Context) error`

Like `Run` but installs **no** signal handler — cancellation is entirely the caller's, and so is reload (call `app.Reload` directly). Serves until `ctx` is cancelled, the server stops, or a programmatic `Shutdown`. On `ctx` cancellation the drain keeps `ctx`'s values, drops its cancellation (so an already-cancelled `ctx` still drains), and applies the `WithShutdownTimeout` deadline. This is the entry point for tests, embedding, and tracing contexts. Cancelling `ctx` **during** the start phase [interrupts it](#a-shutdown-during-the-start-phase): the running `Start`'s or start hook's context is cancelled, nothing further starts, what was built is rolled back, the listener never accepts, and `RunContext` returns nil after a clean rollback.

### `app.ServeContext(ctx context.Context, l net.Listener) error`

Serves on a caller-provided listener, sharing `RunContext`'s lifecycle. The escape hatch for listeners the framework does not create itself — Unix sockets, a preconfigured test listener, or an externally managed listener. It supplies the _listener_ only; the server is still the one the framework builds, so protocol-level settings such as H2C come from [`WithHTTPServer`](#credowithhttpserverfn-funchttpserver-option). `ServeContext` takes ownership of `l` and closes it when the server stops (matching `net/http.Server.Serve` semantics). A nil listener returns an error. It serves `l` exactly as given and is **TLS-exempt** — TLS configured via `WithTLSFiles`/`WithTLSConfig` does not apply; wrap `l` with `tls.NewListener` for HTTPS.

### TLS

TLS is server configuration, not a serve method. When a certificate source is configured, `Run` and `RunContext` serve HTTPS; otherwise they serve plaintext. `ServeContext` is exempt (see above). Three sources resolve by precedence — highest wins, whole-source override, never a conflict error:

| Source | Precedence | Notes |
| --- | --- | --- |
| `WithTLSConfig(*tls.Config)` | highest | Full `crypto/tls` surface: mTLS, SNI, `GetCertificate` reload, ALPN. Cloned before use |
| `WithTLSFiles(cert, key)` | middle | PEM file paths; overrides the config keys (resolved after unmarshal). Rotated on every reload |
| `server.tls.cert_file` / `server.tls.key_file` | lowest | The same paths via config. Rotated on every reload (paths re-read from the new snapshot) |
| _(none)_ | — | Plaintext |

All TLS validation runs once at **preflight**: a missing or mismatched key pair, a partial cert-without-key, a `WithTLSConfig` with no certificate source (the check mirrors `net/http`: `Certificates`, `GetCertificate`, or `GetConfigForClient`), or an explicitly-set-but-empty source — `WithTLSConfig(nil)` or `WithTLSFiles` with an empty path — is a pre-session failure that rolls the state back to `building`. An explicit option that is empty or nil fails loud rather than silently falling through to a lower-precedence source or to plaintext. The resolved `*tls.Config` is built once and reused by the serve goroutine — no double load. For the two file-based sources the key pair is served through `GetCertificate` backed by an atomic pointer: every [reload](#appreloadctx-contextcontext-error) re-reads the files and swaps the pair on success (new handshakes see the new certificate, open connections are untouched), while a failed re-read keeps the previous certificate and surfaces through the reload error. `WithTLSConfig` is never touched by reload; its owner drives rotation through their own `GetCertificate` (optionally from an `OnReload` hook).

`WithHTTPRedirect(addr)` adds a second, plaintext listener that permanently redirects every request to HTTPS (301 for GET/HEAD, 308 otherwise). It requires TLS — without it, preflight fails fast — and binds, serves, and drains alongside the main server: a bind failure rolls back to `building` like the main listener, and a runtime failure of the redirect listener tears the app down, just like the main listener, so a requested redirect never silently dies while the app reports healthy. `ServeContext` ignores it. To make clients _prefer_ HTTPS without a redirect, enable HSTS via `middleware.Secure` (opt-in, sent only over HTTPS).

### Server construction and `WithHTTPServer`

The `*http.Server` is built once per session. Credo maps the fields it has an opinion about and hands the rest to the caller, so the standard library gaining a field does not require a Credo release:

```
serverConfig  →  buildServer fields  →  ErrorLog bridge  →  WithHTTPServer callback  →  (preflight) TLS chain  →  listen/serve
```

The callback runs last among the construction steps, so it is the final word on every field before it — the config keys included. Three fields are re-imposed afterwards:

| Field | Who wins | Note |
| --- | --- | --- |
| `Handler` | framework | Always the `App`; a replacement would bypass middleware, the error pipeline, and route introspection |
| `Addr` | framework | The listener is bound from it, and `app.Addr()` reports it |
| `TLSConfig` | framework | Assigned later by the TLS precedence chain. With no Credo TLS source the server runs under `Serve`, which ignores `TLSConfig` — a callback cannot upgrade a plaintext listener |
| timeouts, `MaxHeaderBytes`, `MaxHeaderValueCount`, `ErrorLog` | callback | Framework-mapped from config, then overridable |
| `Protocols`, `ConnState`, `BaseContext`, `ConnContext`, `TLSNextProto`, `DisableClientPriority` | callback | Reachable only this way |

`Serve`, `ServeTLS`, `Shutdown`, `Close`, and `RegisterOnShutdown` belong to the lifecycle: the callback must not call them or retain the pointer past its return. The `WithHTTPRedirect` listener is a separate, fixed-function server and is not passed to the callback; it keeps its own mirrored `ErrorLog` and `ReadHeaderTimeout`. Everything the callback sets is restart-only — the server is constructed once per session, so a reload cannot change it.

### Startup record

Managed serving (`Run`, `RunContext`, `ServeContext`) writes one Info line after the listener is bound and the start walk has succeeded, as the App enters `running` and just before the server starts accepting connections: `credo: server started` with `label` (the entry point: `Run`, `RunContext` or `ServeContext`), `addr` (the bound address, as `app.Addr()` reports it) and `features` (the built-in HTTP features in effect; the [HTTP features spec](http-features.md#startup-visibility) defines its names, order and derivation). A failed or interrupted start writes no such line. An application that only calls `ServeHTTP` from its own `http.Server` — after `App.Start` included — gets no startup record.

### Server diagnostics (`http.Server.ErrorLog`)

`net/http` reports its own problems — TLS handshake failures, listener accept errors, panics that escape the framework recovery, superfluous `WriteHeader` calls, hijacked-connection writes — through `http.Server.ErrorLog`. Credo wires that to the application logger, so those records arrive as structured entries at `ERROR` with `component=net/http` instead of going to the standard `log` package's stderr output. The stdlib message text is preserved verbatim (`http: TLS handshake error from …`), so existing greps and alerts keep matching. The redirect listener from `WithHTTPRedirect` shares the same bridge.

Two rejections are **not** observable this way: a request that exceeds the header limits (`max_header_bytes` or `max_header_value_count`) is answered with `431 Request Header Fields Too Large`, and an unsupported transfer encoding with `501`, both written straight to the connection by `net/http` without ever reaching `ErrorLog`.

### `app.Addr() net.Addr`

Returns the actual network address the server is listening on. Particularly useful when the server was started with port 0 (OS-assigned ephemeral port). It is set when the listener binds, before the start walk, so `Start` and start hooks can read it. Returns `nil` before `Run()`, under `App.Start`, and after `Shutdown()`.

### `app.Shutdown(ctx context.Context) error`

Gracefully shuts down the App with the [drain in tiers](#shutdown-in-tiers): readiness is withdrawn, the HTTP drain runs concurrently with the ingress tier, an in-flight `Reload` is awaited, then the internal tier stops. Which state it finds decides what it does:

| State | Effect |
| --- | --- |
| `running` | Claims `running → stopping` (CAS) and drains |
| `building` | [Bootstrap teardown](#bootstrap-teardown): claims `building → stopping` and drains with no servers |
| `starting` | [Interrupts the start phase](#a-shutdown-during-the-start-phase) and waits, within `ctx`, for its rollback; returns the rollback's lifecycle error, nil when it was clean. A start that has already failed but not yet begun its rollback is waited for the same way |
| `stopping`, `stopped` | Returns `credo: Shutdown: server in state "…", expected "building", "starting" or "running"` — also while the rollback of a failed start is under way, since that rollback has moved the App to `stopping` |

The refusal is decided on the state read after both claims failed: when that read shows `running` or `building` (a start completed or rolled back between the claims), the call claims again instead of refusing with a stale state. Concurrent `Shutdown` calls pick one owner of the drain; the others receive the state error. The caller's `ctx` carries the one deadline the drain shares; `WithShutdownTimeout` does not replace it.

`Shutdown` is the single drain mechanism shared by every entry point. The signal-triggered drain of `Run`, the cancellation-triggered drain of `RunContext`/`ServeContext` and the drain after a post-running serve error run this exact sequence, made idempotent by the `running` → `stopping` CAS — a cancelled context racing a programmatic `Shutdown` cannot run the sequence twice (the loser is a no-op). Idempotency comes from that one CAS, not a parallel `sync.Once`.

#### Drain context derivation

An explicit `Shutdown(ctx)` uses the caller's `ctx` deadline as-is. Signal- and cancellation-triggered drains, and the rollback of a failed start, instead derive a deadline context from `WithShutdownTimeout` (default 30s). No step of the drain outlives the deadline: a call that has not returned by then is abandoned ([Shutdown in tiers](#shutdown-in-tiers)).

| Trigger | Drain context |
| --- | --- |
| Signal (`Run`), after `running` or during the start phase | `context.Background()` + `WithShutdownTimeout`, counted from the signal |
| Context cancel (`RunContext`, `ServeContext`, and `App.Start` during the start phase) | `context.WithoutCancel(ctx)` + `WithShutdownTimeout` — keeps caller values, drops cancellation; counted from the cancellation |
| Post-running serve error (main or redirect listener) | `context.Background()` + `WithShutdownTimeout` |
| Start failure (a `Start`, start hook or start-walk constructor fails) | `context.Background()` + `WithShutdownTimeout`, counted from the failure |
| Explicit `Shutdown(ctx)` — after `running`, in `building`, or during the start phase | the caller's `ctx`, unchanged |

#### Single-use App

An App is single-use: `New → Run → Shutdown → discard`. Once it reaches `stopping`/`stopped`, any further `Run`/`RunContext`/`ServeContext`/`App.Start` call returns an error (`app cannot be run after shutdown; create a new App`). Tests that need a fresh server create a new `App` with `New()`. Re-run is intentionally unsupported: components are start-once, and background components (e.g. `worker.Pool`) latch a started flag and would not reset cleanly on a second run.

#### Bootstrap teardown

`Shutdown` on an App that was never run cleans up whatever bootstrap registered. It claims `building → stopping` — the counterpart of the start phase's `building → starting` claim, so a concurrent start and `Shutdown` pick exactly one owner — then, under the preparation lock so no in-flight direct preparation can publish afterwards, sets `frozen` and freezes the DI container without validating it (no `Finalize`, no closing yet), and runs the [drain in tiers](#shutdown-in-tiers) with no managed servers: the ingress stop hooks and components, the reload barrier, the internal stop hooks and components, `stopped`. Teardown works even when `Finalize` was never called or failed: the cleanup graph is derived from the frozen registrations and the instances actually built, so an invalid unused registration cannot prevent cleanup of independent live resources. This is the cleanup path for a composition root that fails after registering resources and for tests that only used `ServeHTTP`. An external `http.Server` or `httptest.Server` is still its owner's responsibility: stop its admission and drain its active requests before calling `Shutdown`; the 503 below is not a substitute for that drain.

#### Lifecycle rejection (503)

A request that `ServeHTTP` rejects for lifecycle reasons — the App is `stopped`, or `stopping` without a prepared handler (including a preparation that lost publication to shutdown) or without a successful start of an App that has something to start — receives HTTP 503 with the framework's default error envelope, `{"success":false,"error":{"code":"service_unavailable","message":"Service Unavailable"}}` (`Content-Type: application/json`, precomputed body, status and headers only for HEAD), and a Debug log `credo: request rejected: app is not serving`. The branch is shutdown-safe and callback-free by construction: it does not prepare, resolve, dispatch, run user middleware or status handlers, or invoke a custom error renderer, message-key resolver, locale detector, access-log filter or JSON options — any of which may capture a component that has already been shut down. This is a runtime availability outcome, independent of `WithoutRecover`, and it does not reopen the single-use App; preparation and configuration failures keep the stored developer-error contract above.

### `app.Reload(ctx context.Context) error`

Triggers a partial reload. Succeeds only in the `running` state: before `running` it returns an error (there is nothing to reload), and in `stopping`/`stopped` it returns an error that the signal path treats as a no-op. Concurrent calls are serialized through a context-aware slot: a caller that waits then performs its own full reload, so after `Reload` returns the snapshot is at least as new as when it was called; a waiting caller returns its own `ctx.Err()` if its context ends first, and returns the not-running error without reloading if shutdown begins while it waits. Every participant, subscriber, and hook receives a context cancelled by either the caller's context or the application lifecycle, and `Shutdown` waits for an in-flight reload before the internal tier stops (see [Shutdown in tiers](#shutdown-in-tiers)).

The sequence is: (1) if the registered `RawConfig` implements `config.Stager`, stage a candidate snapshot (`Stage()`) and take its `config.Changes` — a load error aborts with the old snapshot untouched; (2) for every `OnConfigChange[T]` subscription affected by the diff, decode `T` from the candidate and, when `T` has a `Validate() error` method, validate it — any failure aborts before anything is published (logged as `reload aborted before publish`); (3) `Commit()` the snapshot atomically, run framework reload participants (file-based TLS rotation), then affected `OnConfigChange` subscribers in registration order with the values decoded in step 2, then all `OnReload` hooks FIFO — errors and recovered panics are collected and the sequence continues (no rollback); (4) return `errors.Join` of the step-3 errors, log one Info summary (duration, whether config was reloaded, changed-key count, subscribers notified, error count) and one Warn naming every changed key that no subscription or participant covers (`restart required`; key paths only, never values). A reload never stops the process.

A `RawConfig` that implements only `config.Reloader` has no candidate stage: its `Reload()` publishes first and affected subscribers are decoded from the live snapshot, so a decode failure is a step-3 error rather than an abort. A `RawConfig` that implements neither leaves the configuration untouched; only participants and `OnReload` hooks run, and `OnConfigChange` is a registration-time panic. A nil `ctx` is an error.

### `app.OnReload(fn func(ctx context.Context) error)`

Registers a reload hook. Hooks run in **FIFO** order at the end of every reload, after the new snapshot is visible and typed subscribers have applied their sections, with the reload context (`WithReloadTimeout` for the signal path, the caller's for `Reload`). An error or recovered panic is joined into the `Reload` result and does not skip later hooks. Typical uses: re-open a log file after rotation, refresh an allowlist, drive rotation for a `WithTLSConfig` certificate. Must be called before `compile()` (panics if frozen); a nil hook panics.

### `app.OnConfigChange[T](key string, fn func(ctx context.Context, next T) error)`

Generic method (Go 1.27 concrete-type generic methods, as `Provide[T]`/`GetConfig[T]`) registering a typed subscriber for one config section. When a reload changes any leaf under `key` (or `key` itself), `T` is decoded from the new snapshot — validated first if it implements `Validatable` — and `fn` receives it. Subscribers for unaffected sections are not invoked. Several subscriptions may share a key, and nested keys are independent (`"databases"` and `"databases.primary"` both fire when `databases.primary.dsn` changes). The subscriber owns atomic application in its domain (`atomic.Pointer[T]`, `slog.LevelVar.Set`, swapping a limiter); the framework never rebuilds DI singletons. Must be called before `compile()`; a nil hook panics; registering one when the app's `RawConfig` implements neither `config.Stager` nor `config.Reloader` panics at registration (a subscription that can never fire is startup misuse).

### `credo.WithReloadTimeout(d time.Duration) Option`

Construction option setting the context budget for SIGHUP-triggered reloads under `Run`. Zero (the default) applies 30s. A programmatic `Reload(ctx)` ignores it and uses the caller's context. Also settable via the `server.reload_timeout` config key.

### `credo.WithShutdownTimeout(d time.Duration) Option`

Construction option (passed to `New`) setting the graceful-shutdown deadline for the signal-aware `Run` and the cancellation-triggered `RunContext`/`ServeContext`. Zero (the default) applies 30s. It also bounds the rollback of a failed or interrupted start. An explicit `Shutdown(ctx)` ignores it and uses the caller's deadline instead. Also settable via the `server.shutdown_timeout` config key.

### `credo.WithTLSFiles(certFile, keyFile string) Option`

Construction option configuring HTTPS from a PEM certificate/key file pair. When set, `Run`/`RunContext` serve TLS. Performs no I/O at construction — the pair is loaded and validated at preflight. Overrides the `server.tls.cert_file` / `server.tls.key_file` config keys; shadowed by `WithTLSConfig`. An empty cert or key path is a preflight error, not a silent fall-back to the config keys or plaintext. See [TLS](#tls).

### `credo.WithTLSConfig(cfg *tls.Config) Option`

Construction option configuring HTTPS from a fully-formed `*tls.Config` — the full `crypto/tls` surface (mTLS, SNI, custom versions/cipher suites, ALPN, `GetCertificate` reload). Highest TLS precedence; when set, `WithTLSFiles` and `server.tls.*` are ignored. The config must carry a certificate source (validated at preflight) and is cloned before use. A nil config is a preflight error, not a silent fall-back to the lower-precedence sources. See [TLS](#tls).

### `credo.WithHTTPRedirect(addr string) Option`

Construction option running a second, plaintext listener on `addr` (e.g. `":80"`) that permanently redirects every request to its HTTPS equivalent — 301 for GET/HEAD, 308 for other methods. Requires TLS (preflight fails fast otherwise); binds, serves, and drains with the main server, and a runtime failure of the listener tears the app down like a main-listener failure. Does not apply to `ServeContext`. See [TLS](#tls).

### `credo.WithHTTPServer(fn func(*http.Server)) Option`

Construction option registering a callback that receives the built `*http.Server`, keeping the whole `net/http` surface reachable — `Protocols` (including H2C), `HTTP2`, `ConnState`, `BaseContext`, `ConnContext`, `DisableClientPriority` — without an option per field. It runs once, after every framework-set field, and is the last word on all of them; `Handler`, `Addr`, and `TLSConfig` are re-imposed afterwards. The lifecycle methods (`Serve`, `ServeTLS`, `Shutdown`, `Close`, `RegisterOnShutdown`) are framework-owned and the pointer must not be retained past the call. The `WithHTTPRedirect` listener is excluded. A nil callback is a no-op. See [Server construction and `WithHTTPServer`](#server-construction-and-withhttpserver).

## Components and the Start Phase

The component contract — the type and its capabilities, discovery, resource identity, the registration options, the tiers — is [ADR-024](../adr/024-lifecycle-components.md); the options' DI surface is the [container spec](container.md). This section states what the lifecycle does with components.

### `app.Start(ctx context.Context) error`

Start runs the start phase without a listener; `Run`, `RunContext` and `ServeContext` serve. It claims `building → starting`, prepares the App (`Finalize` and compile), runs the [start walk](#the-start-walk) and enters `running`, for an App served through `ServeHTTP` by an external `http.Server` or in a test. `app.Shutdown` stops what it started. It is accepted only in `building`: a second call, or a serve entry point after it, returns the state error. A preparation failure returns `credo: Start: …` and gives the slot back to `building`, as for the serve entry points. A failed `App.Start` rolls back what was built and leaves the App `stopped`, returning the [lifecycle error](#the-lifecycle-error); `ctx` interrupts the start phase like any [shutdown during the start phase](#a-shutdown-during-the-start-phase).

Whoever serves the App through `ServeHTTP` owns that server's admission and drain and completes them before `app.Shutdown`. The rule carries the drain order: the internal tier stops after the HTTP drain only if that drain has happened, and nothing in the App can perform it for the owner.

`testutil.Start(tb, app)` starts the App and leaves its shutdown to the end of the test, failing the test when the start fails. An App built by `testutil.NewApp` already registers its shutdown cleanup first, so that cleanup runs after every cleanup the test adds later — a test server closed through `t.Cleanup` is drained before the components stop — and `testutil.Start` adds none. Any other App gets a shutdown cleanup when `testutil.Start` starts it, so its test server is created after `testutil.Start` or closed with `defer`.

### `app.Manage(v, opts...)`

Adds a component that is not a DI binding: a value with `Shutdown`, or a constructor over DI parameters — `func(deps...) C` or `func(deps...) (C, error)` — that the start walk builds and that never becomes a binding. A function whose own type has `Shutdown` (a named func type) is a value, not a constructor. `credo.Named("…")` names it (default: its type name; a duplicate panics), and `credo.Ingress()` places it in the ingress tier. A value has no edges and stops in reverse registration order within its tier; a constructor has its parameters' edges. A value without `Shutdown`, an App handed to its own `Manage`, a call after `Finalize` or shutdown, a value whose resource a `ProvideValue` binding holds, and a resource handed to `Manage` twice panic ([ADR-024](../adr/024-lifecycle-components.md#resource-identity-one-resource-one-teardown)).

### Contexts

Every context the lifecycle hands out lives as long as its call:

| Call | Context |
| --- | --- |
| `Start`, `OnStart` hook | Cancelled when the call returns, or earlier when a shutdown is requested during the start phase |
| `Shutdown`, `OnStop` hook, `Close(ctx)` of a `credo.Closer()` binding | The drain context: its deadline is the [drain deadline](#drain-context-derivation) |
| `Ready` | The probe request's context |

Work that outlives `Start` runs on a goroutine whose context derives from `context.WithoutCancel(ctx)` with its own cancel, which `Shutdown` calls; `Shutdown` is the one stop signal. A loop that keeps `Start`'s context stops as soon as `Start` returns. The session context the App keeps internally is observed only by reload ([`app.Reload`](#appreloadctx-contextcontext-error)).

### The start walk

```
1. Internal tier, in dependency order: build each component with Start or Ready that is
   still unbuilt, and every constructor handed to app.Manage, then call its Start (when
   its binding's type shows one)
2. Internal OnStart hooks, FIFO
3. Ingress tier, in dependency order: the same
4. Ingress OnStart hooks, FIFO
5. state = running; managed serving logs "credo: server started" and accepts
```

- A component's `Start` runs after the `Start` of every component it depends on. No dependency crosses the tiers backwards: an internal component that depends on an ingress one fails `Finalize` (or, for a component found only on its built value, its construction) with the path and both remedies, so the order holds across the tiers. An ingress component that depends on an internal one starts after it.
- `Start`, `Ready` and the tier are planned from the binding's type. A `Start` that only the built value has is never called, and a type with only `Start` is not a component and is never started. A value whose binding's type shows both `Start` and `Shutdown` is started exactly once, by the walk — an `OnStart` hook that also starts it starts it a second time.
- A component with neither `Start` nor `Ready` stays lazy: it is built when first resolved — during a request included — and shut down by the drain only if built.
- A constructor error in step 1 or 3 is a start failure. `Start` returns once the component is usable; it never blocks for the component's lifetime.
- Under managed serving the listener is bound before the walk, so `app.Addr()` is available in `Start` and in start hooks; it is nil under `App.Start`.

### A failed start

A `Start` or start hook that returns an error or panics, or a constructor that fails in the walk, fails the start. Nothing further starts, and the rollback stops, [in tiers](#shutdown-in-tiers) and in reverse dependency order, every component that was built — one built before `Run` and never started included — except the one whose `Start` failed: a `Start` that returns an error has released what it opened. Stop hooks run as in every teardown. A `Start` that panics is recovered, logged (`credo: component Start panic`) and reported as `panicked` with a `*credo.DIPanicError` of phase `DIPanicStart`, and the rollback continues. The listener is closed, no startup record is written, the App ends `stopped`, and the entry point returns the [lifecycle error](#the-lifecycle-error) naming each component with its phase and outcome. The rollback runs under the `WithShutdownTimeout` deadline counted from the failure.

### A shutdown during the start phase

`Shutdown`, the first signal in `Run`, the context of `RunContext` or `ServeContext`, and the context passed to `App.Start` each interrupt the start phase, at whatever point it has reached:

| Position of the request | Effect |
| --- | --- |
| Preparation or listener bind | No component starts; the rollback stops what bootstrap built |
| During a component's `Start` or a start hook | Its context is cancelled. If it returns an error, it has released what it opened and is not shut down; if it returns nil, it has started and is stopped in the rollback; if it ignores the cancellation, it is abandoned at the deadline — its dependencies stay open — and reported |
| Between two components | The next component is not started |

In every case nothing further starts, the rollback stops what was built in reverse dependency order, `credo: server started` is never logged, the listener never accepts and is closed, so a dial to it is refused, and the App ends `stopped`. The rollback runs under the drain deadline counted from the request ([drain context derivation](#drain-context-derivation)). A requested shutdown is not a start failure: `Run`, `RunContext` and `ServeContext` return nil after a clean rollback and log the interrupted `Start`'s error; an abandoned `Start` or a failed rollback makes them return the lifecycle error. `App.Start` returns an error instead, because its caller goes on to serve the App: an interrupted `App.Start` returns its context's error or, when `Shutdown` interrupted it, an error saying the App was shut down while starting, joined with the lifecycle error when the rollback was not clean — so a caller never mistakes an interrupted start for a started App. `Shutdown` called while starting waits for the rollback within its own context. A second signal still kills the process.

### Shutdown in tiers

`app.Shutdown` from `running` — and the signal- and cancellation-triggered drains and the drain after a post-running serve error, which run the same sequence:

```
1. CAS running → stopping (building → stopping is bootstrap teardown; starting → interrupt, above)
2. Mark unready — /ready returns 503 shutting_down; liveness stays up — and cancel the
   session context, which cancels an in-flight Reload
3. Concurrently:
   a. the HTTP drain (the redirect listener closes before the main server)
   b. the ingress tier: ingress OnStop hooks LIFO, then ingress components; components that
      no edge orders stop concurrently with each other, edges are respected
4. Wait for an in-flight Reload, keeping the reload slot (reported at the deadline)
5. The internal tier: internal OnStop hooks LIFO, then internal components one at a time in
   reverse dependency order, values with no edges in reverse registration order
6. Clear bound address; state = stopped
7. Return errors.Join of the HTTP drain's error, the reload's and the lifecycle error
```

Stop hooks run before their tier's components, so a hook can still use them. The DI container enters closing when the internal components begin to stop — after the internal stop hooks, which still see a live container; a resolution after it returns an error wrapping `ErrDIClosed` ([container spec](container.md#shutdown)). Bootstrap teardown runs steps 2–7 with no servers, stopping what bootstrap built, and the rollback of a failed or interrupted start runs the same tiers ([A failed start](#a-failed-start)).

**One deadline, spent in order.** Steps 3–5 share the drain deadline. The tiers run one after another, so a long HTTP drain or ingress tier leaves less of the budget for the internal tier, and a budget spent before step 5 stops nothing of it: once the deadline has ended, no further stop hook or teardown starts, and each one left is reported abandoned, its resources left to the process exit. There is no hard barrier:

- A `Shutdown`, `Close` or stop hook that has not returned at the deadline is abandoned. The dependencies of an abandoned component are not stopped — nothing closes a database under a component that may still use it — and the abandoned component and every component it kept open (`kept_open`, with `KeptOpenBy`) are reported. A call that returns later is logged, never reported.
- A `Shutdown` that returns an error promptly is reported, and its dependencies are still stopped.
- Every `Start`, `Shutdown` and hook is panic-isolated; a panic is reported as that component's outcome.
- A construction still running when the drain reaches it blocks its dependencies and is shut down in order when it completes. One that completes after the deadline gets the single fixed five-second late attempt, logged and not reported — for a component its binding's type shows and for one found only on its built value alike.

**One teardown per resource.** Values that share a resource identity are shut down once, when the last holder retires, through the holder registered first among those that have a teardown; a borrowed resource is shut down by none of them ([ADR-024](../adr/024-lifecycle-components.md#resource-identity-one-resource-one-teardown)).

### Background work

Background work is a component: its `Start` launches the work on a goroutine whose context derives from `context.WithoutCancel(ctx)` with its own cancel, and its `Shutdown` cancels that work and waits for it ([Contexts](#contexts)). Its tier places its stop: ingress for work that enters the process — a consumer of an external queue, a scheduled job — which stops beside the HTTP drain, and internal for in-process work that handlers feed, which stops after the HTTP drain and before the components it depends on. A producer that hands work to a consumer component takes the consumer as a constructor dependency, so the producer stops first. A lifecycle `Service` abstraction with a blocking `Run(ctx)` and a restartable/start-once taxonomy is rejected: a `Start` that blocks for the component's lifetime cannot tell a started component from a running one, components are start-once, and restart remains a worker concern ([ADR-024](../adr/024-lifecycle-components.md#rejected-alternatives)).

**Accepted, pending implementation (v0.24.0, W6).** Workers become components of their own. Until then the `worker` pool is one ingress component, bound with `ProvideProtectedValue[*worker.Pool](p, credo.Ingress())` and started by the start walk; its workers run on `context.WithoutCancel` of its `Start` context and stop in its `Shutdown` ([worker spec](worker.md)).

### The lifecycle error

A start or shutdown failure is one `*credo.LifecycleError{Entries []LifecycleEntry, Cause error}`, obtained with `errors.AsType[*credo.LifecycleError]` from `Run`, `RunContext`, `ServeContext`, `App.Start` and `Shutdown`. Each `LifecycleEntry` names a component — by its `credo.Named` name or its type name, a hook as `OnStart[i]` or `OnStop[i]` — with its `Tier`, its `Phase` (`PhaseStart`, or `PhaseShutdown` for the drain and for the rollback of a start), its `Outcome` (`failed`, `panicked`, `abandoned` at the deadline, or `kept_open` by an abandoned consumer, named in `KeptOpenBy`), its `Err` and its `Duration`. Only those outcomes appear: a component that started and stopped cleanly has no entry. `Cause` is the context error that ended a phase before it completed. The text reads `credo: lifecycle (cause): name (tier) phase outcome[ after d][ by …]: err; …`. It is an immutable snapshot taken at the boundary: a call returning later, or a late-construction attempt, is logged and never written back. `Unwrap() []error` exposes each failure and the cause, so `errors.Is` and `errors.As` traverse it; a panicking `Start` is a `*credo.DIPanicError` with phase `DIPanicStart`. **Accepted, pending implementation (v0.24.0, W6):** a worker is named `worker:<name>`.

### Readiness

`/ready` aggregates three sources and resolves nothing from the DI container per request: the `Ready` of every component whose binding's type shows it (or that was handed to `app.Manage` with it), under the components' names, from the values built when the App entered `running`; the kernel's store registry — each registered store's typed health, read from the value the start phase resolved once, after `Finalize` and every override ([store spec](store.md)); and the application's `AddReadinessCheck` checks. A borrowed value's `Ready` is aggregated although the App neither starts nor stops it. During the drain `/ready` returns 503 `shutting_down`.

A failure after `Start` has returned is reported through `Ready`. No component ends the App — the App's own listeners excepted — so a component that has stopped for good leaves the process alive and unready, and a failing readiness probe restarts nothing. An application that wants a restart ties a liveness check to the component itself, knowing that a liveness check failing because of a shared dependency restarts every replica.

**Accepted, pending implementation (v0.24.0, W5, W6)** for the store registry and worker readiness: until W5 store health, and until W6 worker readiness, still come from the two internal seams the health engine resolves from the container on every `/ready` request.

### `app.OnStart(fn, opts...)` and `app.OnStop(fn, opts...)`

Both register an anonymous component of a tier, internal unless `credo.Ingress()` is passed.

| | `OnStart` | `OnStop` |
| --- | --- | --- |
| Slot | After its tier's components have started | Before its tier's components stop |
| Order within the tier | FIFO | LIFO |
| Context | Ends with the call; cancelled by a shutdown during the start phase | The drain context |
| Error or panic | A start failure: the start rolls back | Reported in the lifecycle error; the drain continues |
| Missed deadline | Ignoring the cancellation of an interrupted start: abandoned at the rollback deadline and reported, like a `Start` | Abandoned and reported, like a `Shutdown` |
| Runs on | The start walk | Every teardown: the drain, bootstrap teardown, and the rollback of a failed or interrupted start |

A stop hook must tolerate a start that never reached its counterpart. A hook is a leaf action: a resource with its own teardown is a component — handed to `app.Manage`, or bound with `credo.Closer()` when its teardown is `Close` — and process-level cleanup that must outlive every component belongs after `Run` returns. A nil hook, an option other than `credo.Ingress()`, or a registration after preparation or shutdown panics.

A hook that panics fails like one that returns an error: the panic is recovered, logged once (`credo: OnStart hook panic` or `credo: OnStop hook panic`, Error, with `hook_index`, the panic value and the stack) and reported as that hook's error (`panic: …`) — a start failure for a start hook, a lifecycle-error entry for a stop hook — so the rollback or the drain continues instead of the panic ending the process. Under managed serving `app.Addr()` is available inside start hooks — critical for port-0 scenarios.

Typical start-hook uses include cache warm-up. The `store/sqldb` migration wrapper's `Migrate` method matches the hook signature, so `app.OnStart(db.Migrate)` is convenient for development and deliberate single-replica deployments. Its context is cancelled by a shutdown during the start phase and carries no deadline of its own. Multi-replica production should instead run the same method once in a deadline-bounded pre-deploy job (see the [Store Spec](store.md)).

### Mounted Apps

`*App` has `Start(ctx) error` and `Shutdown(ctx) error`, so it is a startable component. A child App mounted with `app.Mount` is handed to `parent.Manage(child)`: its start phase runs in the parent's internal tier, before the parent's listener accepts, and it stops after the parent's HTTP drain, its own components in their own order. The parent's server is the child's owner, so the parent's HTTP drain is the drain the child's `Shutdown` relies on. Without the `Manage`, a child with anything to start panics on its first request with the gate's message, which the parent's recovery turns into a logged 500; a child with nothing to start serves as before. `parent.Manage(child)` composes independent Apps; an application's own modules share one App and its DI graph.

### Scenarios the lifecycle closes

The component model is specified to close four scenarios end to end, each pinned by a test:

1. **A handler hands work to a worker that writes to the database.** A handler enqueues to an in-process continuous worker (internal tier) that writes through a database component. A request accepted just before shutdown completes during the HTTP drain, its job is written, and the database shuts down after the worker ([worker spec](worker.md)).
2. **A WebSocket drain.** The WebSocket server is an ingress component; it drains concurrently with the HTTP drain and before the internal components its handlers use ([WebSocket spec](websocket.md)).
3. **A scheduled worker.** It originates its runs, so it is ingress by default and stops with the ingress tier, before the internal components its runs use ([worker spec](worker.md)).
4. **A consumer of an external queue.** Registered with `credo.Ingress()`, it stops in the ingress tier, before the database it writes to.

**Accepted, pending implementation (v0.24.0, W6)** for scenarios 1 and 3, which need workers as components; the first scenario's acceptance test waits for W6.

## Registration Guards

The following methods panic with `credo: <what> called after app was compiled or shut down` once `frozen` is set — at preparation admission (first `ServeHTTP` request or managed serve) or at bootstrap-shutdown admission. An explicit `Finalize` does not set it, so DI-backed controllers can still be resolved and bound afterwards:

| Method | Guard |
| --- | --- |
| `app.GlobalMiddleware()` | `checkFrozen("App.GlobalMiddleware")` |
| `app.GET/POST/PUT/...()` (and `group.*`) | `checkFrozen("App.GET")` etc., one label per method (via `addRoute`) |
| `app.Host()` | `checkFrozen("App.Host")` |
| `app.Mount()` | `checkFrozen("App.Mount")` |
| `app.Static()` / `app.File()` (and `group.*`) | `checkFrozen("App.Static")` / `checkFrozen("App.File")` |
| `app.StatusHandler()` | `checkFrozen("App.StatusHandler")` |
| `app.UseErrorRenderer()` / `app.UseSuccessRenderer()` | `checkFrozen("App.UseErrorRenderer")` / `checkFrozen("App.UseSuccessRenderer")`; nil renderer and a second call also panic |
| `app.UseRequestID()` / `app.UseAccessLog()` / `app.UseCompress()` / `app.UseDecompress()` | `checkFrozen("App.UseX")`; invalid config and a second call also panic |
| `app.SetMeta()` / `app.RemoveMeta()` | through the root group: `checkFrozen("Group.SetMeta")` / `checkFrozen("Group.RemoveMeta")` |
| `app.UseHealth()` | `checkFrozen("App.UseHealth")` |
| `app.UseI18n()` | `checkFrozen("App.UseI18n")` |
| `app.OnStart()` / `app.OnStop()` | `checkFrozen("App.OnStart")` / `checkFrozen("App.OnStop")`; a nil hook and an option other than `credo.Ingress()` also panic |
| `app.OnReload()` | `checkFrozen("App.OnReload")`; nil hook also panics |
| `app.OnConfigChange[T]()` | `checkFrozen("App.OnConfigChange")`; nil hook panics; a `RawConfig` that is neither `Stager` nor `Reloader` panics |
| `group.Middleware()` | `checkFrozen("Group.Middleware")` |
| `group.SetMeta()` / `group.RemoveMeta()` | `checkFrozen("Group.SetMeta")` / `checkFrozen("Group.RemoveMeta")` |
| `route.Name()` / `route.SetMeta()` / `route.Middleware()` | `checkFrozen("Route.Name")` / `checkFrozen("Route.SetMeta")` / `checkFrozen("Route.Middleware")` |

`app.Manage()` and the DI registrations are guarded by the container's phase instead: after `Finalize`, or once shutdown began, they panic at the call ([bootstrap spec](bootstrap-and-di-lifecycle.md)). `app.Manage()` also panics on a value without `Shutdown`, a duplicate name, a resource DI already holds, or a resource handed over twice.

The same fail-fast policy governs all registration APIs: misconfiguration (nil handlers, malformed patterns, duplicates) panics at startup, while operations that touch the outside world (request handling, file I/O such as `UseI18n` locale loading) return errors. **Accepted, pending implementation (v0.24.0, W5):** `UseI18n` returns nothing and panics on misuse; its catalogs are read in the start phase, where a read failure is a start failure ([i18n spec](i18n.md)). See the package documentation's "Panics and Errors" section.

## Thread Safety

- `state` and `frozen` use `sync/atomic` — safe for concurrent reads.
- `server`, `redirectServer`, `ctx`, `cancel`, `boundAddr` and the start phase in progress (`run`) are protected by the `serverMu` mutex. The start phase is published with the `building → starting` claim, so a `Shutdown` that reads `starting` always finds it to interrupt.
- Preparation is published once through the `prep` atomic pointer; the slow path and bootstrap-shutdown admission serialize on `prepMu`, so shutdown either sees a stored result or prevents an unfinished preparation from publishing.
- State transitions use `CompareAndSwap` — exactly one goroutine wins.
- `Reload` is serialized by a capacity-one slot channel that the drain also takes (and keeps) before the internal tier stops; signal-triggered reloads run on their own goroutine and signals during a reload coalesce into one follow-up. The config snapshot swap is atomic (see the [Config Spec](config.md)).

## Container Integration

A DI singleton whose binding's type shows `Shutdown`, or whose built value has it, is a component and stops automatically in its tier; do not register a second `OnStop` bridge for the same resource. DI enters closing only when the internal components begin to stop — after the HTTP drain, the ingress tier, the reload barrier and the internal stop hooks — so everything before still sees a live container (stop hooks and components must nonetheless capture their dependencies rather than resolve; a resolve while `stopping` is logged at Debug). The container contributes the static graph that orders the teardown — consumers before the singletons they were constructed from, reverse registration order as the tie-break — and bounds every attempt by the shared deadline: a reached component gets at most one attempt, a hung one is abandoned and keeps its dependencies open, and the failures are entries of the `*credo.LifecycleError` joined into the `Shutdown` result. Only construction that completes after the deadline ended gets a separate fixed five-second best-effort cleanup attempt. The [container spec](container.md#shutdown) has the full rules.
