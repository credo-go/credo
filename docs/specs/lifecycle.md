# Lifecycle Spec

**Status:** Implemented (beta) **Package:** root (`lifecycle*.go`, `component.go`, `prepare.go`, `server.go`, `reload.go`, `tls_rotation.go`), `internal/di/` **ADR:** [ADR-006](../adr/006-application-lifecycle.md), [ADR-024](../adr/024-lifecycle-components.md), [ADR-022](../adr/022-bootstrap-and-di-ownership.md), [ADR-020](../adr/020-reload-and-partial-config-reload.md), [ADR-016](../adr/016-health-checks.md) **Guide:** [Deployment guide](../guides/deployment.md), [Dependency injection guide](../guides/dependency-injection.md#shutdown-and-lifecycle)

> **Contract.** Every section of this spec is normative — what application code can observe: the states, the start and shutdown order, the errors, the log records and the 503 — except [Current Behaviour](#current-behaviour), which describes the mechanisms behind that contract as they are now: not guaranteed, they may change in any release. The examples run as `Example*` tests in [`example_lifecycle_test.go`](../../example_lifecycle_test.go) and [`example_bootstrap_test.go`](../../example_bootstrap_test.go), so a drift fails the build.

## Overview

Credo uses a state machine to govern the application lifecycle. This prevents undefined behavior from late route/middleware registration and enables graceful shutdown with in-flight request draining. This spec owns the states, preparation, the serve entry points, TLS and server construction, the start phase, the drain, the lifecycle hooks, reload's mechanics and the registration guards. The DI surface, the registration options and resource identity are in the [container spec](container.md), what `/ready` aggregates is in the [health spec](health.md#readiness-aggregation), and the configuration side of reload — live sections and the reload's result — is in the [config spec](config.md#reload-and-its-result).

## State Machine

```
            Run() / RunContext() / ServeContext() / App.Start()
  building ---[claim]---> starting ---[prepare, bind*, start walk]---> running ---> stopping ---> stopped
      |                      |    |                                       |                         ^
      | ServeHTTP()          |    | start failure ──── rollback ──────────┼─────────────────────────┤
      | (prepare; refused    |    | shutdown requested ─ interrupt,       | serve error / Shutdown  |
      |  while the App has   |    |                      rollback ────────┼──── drain in tiers ─────┤
      |  anything to start)  |    |                                       └─────────────────────────┘
      |                      └─ prepare / preflight / listen error → building (retryable, except a stored preparation failure)
      |
      └── Shutdown() in building ── drain with no servers (bootstrap teardown) ──────────────────────┘

  * bind: managed serving only; App.Start runs the start walk without a listener
```

Failures split by how far startup got. A **pre-session** failure (preparation, TLS preflight or listener bind) starts nothing, so it rolls back to `building` and the App may run again — except that a failed preparation is terminal: it is stored, every later serve attempt returns the same error, and the frozen DI plan cannot be repaired. Returning to `building` permits bootstrap cleanup through `Shutdown`, not a retry of a failed plan. A [failed start](#a-failed-start) — a `Start`, a start hook or a start-walk constructor that fails — and an [interrupted start](#a-shutdown-during-the-start-phase) roll back what was built and end in the terminal `stopped` state, and so does a non-`ErrServerClosed` error from `Serve` after `running`, which runs the [drain in tiers](#shutdown-in-tiers). A second `Run` after shutdown, and a `Shutdown` in `stopping` or `stopped`, return an error (state unchanged).

### States

| State | Description |
| --- | --- |
| `building` | Initial state. Registration follows the bootstrap order ([container spec](container.md#documented-order)) until preparation. `Shutdown` is bootstrap teardown. |
| `starting` | The start phase: preparation, listener bind (managed serving), then the [start walk](#the-start-walk). `Shutdown` is accepted and [interrupts it](#a-shutdown-during-the-start-phase). |
| `running` | The start walk succeeded. Managed serving accepts; after `App.Start` the App is served through `ServeHTTP` by its owner's server and `Addr()` stays nil. |
| `stopping` | Readiness withdrawn; the [drain in tiers](#shutdown-in-tiers), or the rollback of a failed or interrupted start. |
| `stopped` | Terminal — reached by graceful shutdown, bootstrap teardown, a failed or interrupted start, or a post-running serve error. New requests receive the 503 envelope. |

`App.Start` claims the same start slot as the serve entry points, so the single-use rule covers it: a second `App.Start`, or `Run`, `RunContext` or `ServeContext` after it, returns the state error.

## `frozen` vs `state`

Two separate flags exist because `ServeHTTP` and `Run` serve different purposes:

- **`frozen`**: The HTTP write gate. Set at preparation admission — the first direct `ServeHTTP` request or a managed serve entry point — and at bootstrap-shutdown admission. Prevents route/middleware/hook/feature registration after the handler chain is (about to be) built or the App is being torn down. An explicit DI-only `Finalize` does **not** set it.

- **`state`**: Tracks the lifecycle. Only the managed serve entry points and `App.Start` transition to `running`. A user who calls `ServeHTTP` directly without `App.Start` (with their own `*http.Server` or `httptest`) stays in `building` — possible only for an App with nothing to start ([the start gate](#the-start-gate-on-servehttp)) — manages their own server's lifecycle, and uses `Shutdown` in `building` to release what bootstrap built.

This separation allows:

1. `httptest.NewServer(app)` — prepares (freezes HTTP writes) on the first request but doesn't change state; an App with something to start calls `app.Start` first.
2. `app.Run()` — claims the start slot, prepares, freezes, runs the start walk AND enters running state.
3. `app.Start(ctx)` — the same without a listener, for an App served through `ServeHTTP`.
4. Controller resolution and route binding — the first `Resolve`, or an explicit `app.Finalize()` before it, freezes DI, and HTTP writes stay open.

## Preparation

Every serve path reaches the same validated runtime model through one shared preparation step — `Finalize` → `compile` → publish — whose result (handler or error) is stored exactly once:

- **Admission.** Preparation is admitted only while the state is below `stopping`; admission sets `frozen`. A preparation that loses to shutdown before publishing discards its result. Once stored, the result is final: a DI finalize error or a compile panic (middleware construction included) is recorded as a terminal preparation failure and never retried. A compile panic is also logged once with its stack (`credo: preparation failed`); a DI finalize error is reported only through the stored error.
- **Managed serving** (`Run`, `RunContext`, `ServeContext`) and `App.Start` claim `building → starting` first, then prepare. A preparation failure rolls the state back to `building` and returns the error (`credo: Run: credo: prepare: DI finalize: …`, or `… compile panicked: …`); a later serve attempt returns the same stored error without executing a partly compiled handler.
- **Direct `ServeHTTP`** prepares on the first request without claiming the start slot. While lifecycle admission is open it panics with the stored preparation error on every request — a graph or compile error is developer misuse under the package's panic-vs-error policy, and the stored result makes the panic repeatable. An App with something to start additionally refuses to serve until its start phase has succeeded ([below](#the-start-gate-on-servehttp)).
- **Lifecycle rejection.** `ServeHTTP` checks the state on every call, before the cached result; a request it rejects receives the [lifecycle 503](#lifecycle-rejection-503) without preparing, resolving or dispatching, and the stored result is untouched. A handler prepared before the drain keeps serving during `stopping`, which is what the managed HTTP drain, readiness (`/ready` → 503 `shutting_down`) and liveness rely on.

### The start gate on `ServeHTTP`

An App **has something to start** when it has a component with `Start` or `Ready` (planned from its binding's type, or handed to `app.Manage`), a constructor handed to `app.Manage`, a start hook, or `UseI18n`, whose catalogs the start phase reads ([i18n spec](i18n.md)). A WebSocket server is a component with `Start`, so an App that registers one has something to start as well ([WebSocket spec](websocket.md)), and so is a bound database, whose binding's type shows `Start` and `Ready` ([store spec](store.md#lifecycle-of-a-bound-database)). Such an App refuses to serve until the start phase has completed successfully:

| When `ServeHTTP` is called | App with something to start | App with nothing to start |
| --- | --- | --- |
| Before `App.Start`, or while it runs | panics with `credo: ServeHTTP: the App has start work (components, start hooks or UseI18n) and has not been started; …`, naming `App.Start`, `testutil.Start`, `Run`/`RunContext`/`ServeContext` and `parent.Manage(child)` | prepares and serves, as above |
| After `App.Start` succeeded (`running`, `stopping` with a prepared handler) | serves (`ExampleApp_Start`) | serves |
| `stopped` — after a failed or interrupted `App.Start`, or after shutdown — or `stopping` without a successful start | the callback-free 503 envelope, without touching DI | the 503 envelope in `stopped`, and in `stopping` without a prepared handler ([Lifecycle rejection](#lifecycle-rejection-503)) |
| Preparation failed | panics with the stored preparation error on every request | panics with the stored preparation error on every request |

The panic is the treatment a stored preparation error already gets: serving a handler against dependencies that were not started is developer misuse, not an availability outcome. After a failed `App.Start` the caller already has the error, so the stopped App answers with the 503 envelope rather than panicking. Under managed serving the gate never fires — the listener accepts only after the start walk — except for a child App mounted into a parent without [`parent.Manage(child)`](#mounted-apps), whose first request panics with the named message; the parent's recovery turns it into a 500 and logs it.

## API

### `app.State() State`

Returns the current lifecycle state as a `credo.State`, a string type with one constant per state:

| Constant | Value |
| --- | --- |
| `StateBuilding` | `building` |
| `StateStarting` | `starting` |
| `StateRunning` | `running` |
| `StateStopping` | `stopping` |
| `StateStopped` | `stopped` |

The values are the state names used throughout this spec, in error text and in logs, so a `State` compares with a string literal (`app.State() == "stopped"`) and prints as one; code that stores the result in a `string` converts it. The result is a snapshot: another goroutine may move the App on before the caller acts on it.

### `app.IsRunning() bool`

Reports whether the server is in the `running` state.

### `app.Run() error`

Prepares the App, binds the listener, runs the [start walk](#the-start-walk), transitions to `running`, and serves HTTP — or HTTPS when TLS is configured (see [TLS](#tls)) — until an interrupt (Ctrl+C) or `SIGTERM` arrives, then drains [in tiers](#shutdown-in-tiers) with the deadline set by `WithShutdownTimeout`. A signal during the start phase [interrupts it](#a-shutdown-during-the-start-phase). A second signal force-kills the process — signal handling is reset the moment the first signal arrives. Server address is derived from framework-internal server config (host + port). Returns `nil` on graceful shutdown and after a clean rollback of an interrupted start, a `*LifecycleError` when the start fails or a drain or rollback does not complete, or an error if the server fails to start or the app has already run.

On Unix, `Run` also handles `SIGHUP`: each signal triggers [`app.Reload`](#appreloadctx-contextcontext-configreloadresult-error) with the `WithReloadTimeout` budget on its own goroutine, signals arriving during a reload coalesce into at most one follow-up, and a reload failure never stops the server. Because reloads run off the signal loop, a `SIGINT`/`SIGTERM` during a long reload is serviced immediately: the drain starts (cancelling the reload's context and waiting for it before the internal tier stops) and signal delivery is reset, so a second signal force-kills as usual. There is no SIGHUP on Windows; the programmatic `Reload` is the only trigger there.

`credo.WithoutReloadSignals()` disables the reload trigger without changing the rest of `Run`'s signal policy: SIGHUP is still captured — so a stray signal can never fall through to its default action and terminate the process — but is ignored with an Info log line (`credo: reload signal ignored (reload signals disabled)`). SIGINT/SIGTERM handling and programmatic `Reload` are unaffected; the option is a no-op on Windows. Raw Unix signal disposition (an unhandled SIGHUP terminates) remains available via `RunContext`/`ServeContext`, which install no signal handlers.

Over a configuration that cannot reload (a custom `RawConfig`, not a `*config.Config`), SIGHUP is captured and ignored the same way, with the Info line `credo: reload signal ignored: the configuration cannot reload` naming the signal and the configuration's type, so a routine SIGHUP such as a log rotation's stays harmless.

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
| `WithTLSFiles(cert, key)` | middle | PEM file paths; overrides the config keys (resolved after unmarshal). Rotated when the files change ([below](#certificate-files-rotate-when-they-change)) |
| `server.tls.cert_file` / `server.tls.key_file` | lowest | The same paths via config, read at start. Rotated when the files change |
| _(none)_ | — | Plaintext |

All TLS validation runs once at **preflight**: a missing or mismatched key pair, a partial cert-without-key, a `WithTLSConfig` with no certificate source (the check mirrors `net/http`: `Certificates`, `GetCertificate`, or `GetConfigForClient`), or an explicitly-set-but-empty source — `WithTLSConfig(nil)` or `WithTLSFiles` with an empty path — is a pre-session failure that rolls the state back to `building`. An explicit option that is empty or nil fails loud rather than silently falling through to a lower-precedence source or to plaintext. The resolved `*tls.Config` is built once and reused by the serve goroutine — no double load; a `WithTLSConfig` value is cloned, so later mutations of the caller's config do not affect serving.

#### Certificate files rotate when they change

For the two file-based sources the key pair is served through `GetCertificate`, and the files are checked at the handshake, not at a reload:

- `GetCertificate` checks the two files at most once a second, on the handshake that finds the last check older than that; concurrent handshakes serve the current pair without waiting, so one check runs per interval.
- A check is `os.Stat` on each file, compared with the last load by `os.SameFile`, modification time and size. `os.Stat` follows symbolic links, so the link a Kubernetes Secret volume swaps on an update is a change, and no signal is needed.
- A change loads the pair with `tls.LoadX509KeyPair` and swaps it atomically: new handshakes see the new certificate, open connections are untouched. The swap is logged at Info (`credo: TLS certificate reloaded`, with `cert_file` and `key_file`).
- A pair that fails to load — a half-written pair, a key that does not match its certificate — keeps the old pair and is logged at Error once per change (`credo: TLS certificate pair failed to load after a change on disk; keeping the current pair`, with `cert_file`, `key_file` and `error`); it is retried when its other file changes. A file that cannot be read by `os.Stat` is logged the same way, once (`credo: TLS certificate files changed but cannot be read; keeping the current pair`).
- The paths are read at start, so a reload that changes `server.tls.*` does not move the pair: it reports the keys as needing a restart ([config spec](config.md#reload-and-its-result)).

`WithTLSConfig` stays the caller's, untouched: its owner rotates through their own `GetCertificate`.

`WithHTTPRedirect(addr)` adds a second, plaintext listener that permanently redirects every request to HTTPS (301 for GET/HEAD, 308 otherwise). The target reuses the request host and request URI with the TLS server's port, omitted when it is 443. It requires TLS — without it, preflight fails fast — and binds, serves, and drains alongside the main server: a bind failure rolls back to `building` like the main listener, and a runtime failure of the redirect listener tears the app down, just like the main listener, so a requested redirect never silently dies while the app reports healthy. `ServeContext` ignores it. To make clients _prefer_ HTTPS without a redirect, enable HSTS via `middleware.Secure` (opt-in, sent only over HTTPS).

### Server construction and `WithHTTPServer`

The `*http.Server` is built once per session. Credo maps the fields it has an opinion about and hands the rest to the caller, so the standard library gaining a field does not require a Credo release:

```
preflight (TLS resolved and validated)  →  buildServer fields  →  ErrorLog bridge  →  WithHTTPServer callback
  →  listen  →  start walk  →  TLSConfig assigned from the resolved chain  →  serve
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

`net/http` reports its own problems — TLS handshake failures, listener accept errors, panics that escape the framework recovery, superfluous `WriteHeader` calls, hijacked-connection writes — through `http.Server.ErrorLog`. Credo wires that to the application logger, so those records arrive as structured entries at `ERROR` with `module=net/http` — `module` names the framework package that wrote a record, as it does for every framework module ([The scoped `Infra` carrier](#the-scoped-infra-carrier)) — instead of going to the standard `log` package's stderr output. The stdlib message text is preserved verbatim (`http: TLS handshake error from …`), so existing greps and alerts keep matching. The redirect listener from `WithHTTPRedirect` shares the same bridge.

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

`Shutdown` is the single drain mechanism shared by every entry point. The signal-triggered drain of `Run`, the cancellation-triggered drain of `RunContext`/`ServeContext` and the drain after a post-running serve error run this exact sequence once — a cancelled context racing a programmatic `Shutdown` cannot run the sequence twice (the loser is a no-op).

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

An App is single-use: `New → Run → Shutdown → discard`. Once it reaches `stopping`/`stopped`, any further `Run`/`RunContext`/`ServeContext`/`App.Start` call returns an error (`app cannot be run after shutdown; create a new App`; `ExampleApp_ServeHTTP_stopped`). Tests that need a fresh server create a new `App` with `New()`. Re-run is intentionally unsupported: components are start-once, and background components (a worker's component, for example) refuse a second `Start` and would not reset cleanly on a second run.

#### Bootstrap teardown

`Shutdown` on an App that was never run cleans up whatever bootstrap registered. It claims `building → stopping` — the counterpart of the start phase's `building → starting` claim, so a concurrent start and `Shutdown` pick exactly one owner — then, so that no in-flight direct preparation can publish afterwards, sets `frozen` and freezes the DI container without validating it (no `Finalize`, no closing yet), and runs the [drain in tiers](#shutdown-in-tiers) with no managed servers: the ingress stop hooks and components, the reload barrier, the internal stop hooks and components, `stopped`. Teardown works even when `Finalize` was never called or failed: the cleanup graph is derived from the frozen registrations and the instances actually built, so an invalid unused registration cannot prevent cleanup of independent live resources. The App ends `stopped` even when the teardown is incomplete. This is the cleanup path for a composition root that fails after registering resources and for tests that only used `ServeHTTP`. An external `http.Server` or `httptest.Server` is still its owner's responsibility: stop its admission and drain its active requests before calling `Shutdown`; the 503 below is not a substitute for that drain.

#### Lifecycle rejection (503)

A request that `ServeHTTP` rejects for lifecycle reasons — the App is `stopped`, or `stopping` without a prepared handler (including a preparation that lost publication to shutdown) or without a successful start of an App that has something to start — receives HTTP 503 with the framework's default error envelope, `{"success":false,"error":{"code":"service_unavailable","message":"Service Unavailable"}}` (`Content-Type: application/json; charset=utf-8`, status and headers only for HEAD; `ExampleApp_ServeHTTP_stopped`), and a Debug log `credo: request rejected: app is not serving`. The branch is shutdown-safe and callback-free by construction: it does not prepare, resolve, dispatch, run user middleware or status handlers, or invoke a custom error renderer, message-key resolver, locale detector, access-log filter or JSON options — any of which may capture a component that has already been shut down. It is written before the feature executor runs, so no request ID, compression or access-log record applies to it. This is a runtime availability outcome, independent of `WithoutRecover`, and it does not reopen the single-use App; preparation and configuration failures keep the stored developer-error contract above.

### `app.Reload(ctx context.Context) (config.ReloadResult, error)`

Triggers a partial reload. Succeeds only in the `running` state: before `running` it returns an error (there is nothing to reload), and in `stopping`/`stopped` it returns an error that the signal path treats as a no-op. Concurrent calls are serialized through a context-aware slot: a caller that waits then performs its own full reload, so after `Reload` returns the snapshot is at least as new as when it was called; a waiting caller returns its own `ctx.Err()` if its context ends first, and returns the not-running error without reloading if shutdown begins while it waits. Every `OnChange` callback receives a context cancelled by either the caller's context or the application lifecycle, and `Shutdown` waits for an in-flight reload before the internal tier stops (see [Shutdown in tiers](#shutdown-in-tiers)).

`app.Reload(ctx)` returns the reload's result beside its error — the result `(*config.Config).Reload(ctx)` returns ([config spec](config.md#reload-and-its-result)) — and keeps the trigger, the slot and the drain's step. In the `running` state the steps are:

1. `(*config.Config).Reload(ctx)` re-reads the sources and decodes and validates every [live section](config.md#live-sections) the reload changes against the candidate. One failure aborts the reload with every old value current; the result says nothing was applied and why, and the error names the section's key.
2. It publishes the snapshot and the changed sections as one generation.
3. It runs the changed sections' `OnChange` callbacks in registration order with the reload's context, which the caller's context or the drain cancels; an error or a recovered panic is joined into the returned error, the later callbacks still run, no callback is skipped when the context has ended, and nothing rolls back.
4. `app.Reload` logs the result and returns it with the joined errors. An aborted reload logs `credo: reload aborted; nothing was published` at Error, with `reason` and `changed_keys`. Otherwise it logs one Warn naming the keys that need a restart, computed against the snapshot the start read (`credo: configuration differs from the start outside every live section; restart to apply it`, with `keys`); one Warn per key a later source keeps (`credo: configuration file value changed, but a later source keeps the key`, with `key` and `source`); one Error per failed callback (`credo: reload callback failed`); and one Info summary (`credo: reload complete`, with `duration`, `changed_keys`, `replaced` and `restart_required`). Key paths only, never values. A reload never stops the process.

No other binding changes, a reload runs no hook of its own, and certificate files [rotate on their own](#certificate-files-rotate-when-they-change). Over a configuration that cannot reload — `testutil`'s, a custom `RawConfig` — `app.Reload` returns an error naming the configuration's type and `config.Load` (`credo: Reload: the configuration is a …, which cannot reload; …`). A nil `ctx` is an error, and a caller whose context has ended by the time it holds the slot returns `ctx.Err()` without re-reading anything. The triggers are unchanged: `app.Reload(ctx)`, SIGHUP under `Run` bounded by `WithReloadTimeout` or `server.reload_timeout`, and `WithoutReloadSignals`; `RunContext` and `ServeContext` stay signal-free. The drain waits for a reload in flight, its callbacks included, before the internal tier ([Shutdown in tiers](#shutdown-in-tiers)).

### `credo.WithReloadTimeout(d time.Duration) Option`

Construction option setting the context budget for SIGHUP-triggered reloads under `Run`. Zero (the default) applies 30s. A programmatic `Reload(ctx)` ignores it and uses the caller's context. Also settable via the `server.reload_timeout` config key.

### `credo.WithShutdownTimeout(d time.Duration) Option`

Construction option (passed to `New`) setting the graceful-shutdown deadline for the signal-aware `Run` and the cancellation-triggered `RunContext`/`ServeContext`. Zero (the default) applies 30s. It also bounds the rollback of a failed or interrupted start. An explicit `Shutdown(ctx)` ignores it and uses the caller's deadline instead. Also settable via the `server.shutdown_timeout` config key.

### `credo.WithTLSFiles(certFile, keyFile string) Option`

Construction option configuring HTTPS from a PEM certificate/key file pair. Its config-key equivalent is `server.tls.cert_file` / `server.tls.key_file`, which it overrides. Performs no I/O at construction: the pair is loaded and validated at preflight. Precedence and the empty-path rule: [TLS](#tls).

### `credo.WithTLSConfig(cfg *tls.Config) Option`

Construction option configuring HTTPS from a fully-formed `*tls.Config` — the full `crypto/tls` surface (mTLS, SNI, custom versions/cipher suites, ALPN, `GetCertificate` reload). It has the highest TLS precedence and no config-key equivalent. The certificate-source check, the clone and the nil rule: [TLS](#tls).

### `credo.WithHTTPRedirect(addr string) Option`

Construction option running the redirect-only plaintext listener on `addr` (e.g. `":80"`). It requires TLS and does not apply to `ServeContext`; the redirect and its listener's lifecycle: [TLS](#tls).

### `credo.WithHTTPServer(fn func(*http.Server)) Option`

Construction option registering a callback that receives the built `*http.Server`, keeping the whole `net/http` surface reachable — `Protocols` (including H2C), `HTTP2`, `ConnState`, `BaseContext`, `ConnContext`, `DisableClientPriority` — without an option per field. A nil callback is a no-op. When it runs, the fields re-imposed after it and what it must not do: [Server construction and `WithHTTPServer`](#server-construction-and-withhttpserver).

## Components and the Start Phase

This section states what the lifecycle does with components. Which bindings are components, the registration options and resource identity are in the [container spec](container.md#registration-options); [ADR-024](../adr/024-lifecycle-components.md) records why.

### Component types

```go
// Component is a value whose teardown the App owns.
type Component interface {
    Shutdown(ctx context.Context) error
}

// Optional capabilities, planned from the binding's type.
type Starter interface {
    Start(ctx context.Context) error
}

type Readier interface {
    Ready(ctx context.Context) error
}

// ResourceIdentifier names the resource a value holds; read from the value,
// not the binding's type.
type ResourceIdentifier interface {
    ResourceIdentity() any
}

// Tier is a component's drain tier: TierIngress stops first, concurrently
// with the HTTP drain; TierInternal, the default, stops after it.
type Tier uint8
```

A value the App owns is a component when it has `Shutdown` (a DI singleton whose binding's type shows the method or whose built value has it, or a value or constructor handed to [`app.Manage`](#appmanagev-opts)), or when its binding carries `credo.CloseOnShutdown()`. `Start`, `Ready` and the tier are planned from the binding's type at registration ([The start walk](#the-start-walk)); a capability absent from it has its documented default — no start step, no readiness entry. `Component` never gains a method; it grows through optional capabilities. The zero `Tier` is unset and means the registration's default. The zero `Tier` encodes as the empty text, `TierIngress` and `TierInternal` as `"ingress"` and `"internal"`; any other value or text is an encoding error.

```go
// IngressComponent is a component whose type places it in the ingress tier.
// IngressTier is never called.
type IngressComponent interface {
    Component
    IngressTier()
}
```

A unit is placed in the ingress tier when its registration carries `credo.Ingress()` or the type its plan reads implements `IngressComponent`: the binding's type for `Provide` and `ProvideValue`, the value's type for `Manage` of a value, and the constructor's result type for `Manage` of a constructor. The check needs no value, so `Finalize` validates the tiers before anything is built. On a declaring type `credo.Ingress()` is redundant and accepted. There is no `credo.Internal()`: a type declares the tier only when no registration could rightly place it internal. A worker takes its tier from its configuration. `websocket.Server` implements the interface ([WebSocket spec](websocket.md)).

**A capability the binding's type hides is refused.** A component whose binding's type hides its value's `Start` or `Ready` — or the `IngressTier` its type declares — is refused instead of being run without it: `ProvideValue` panics at the call, and a value a constructor builds fails the start once it is built, where the container finds a `Shutdown` the type hides. The panic or the start failure names the binding, the methods its type hides and the remedy: bind the concrete type and bind the interface with `Alias`, or give `credo.View()` when another registration owns the value. A view is exempt, since the walk calls its resource's capabilities through the owner ([container spec](container.md#one-owner-per-resource)), and so is `credo.Borrowed()`, since the App neither starts nor stops the value. A hidden `Shutdown` is not refused: it still makes the value a component whose teardown follows it, so a binding whose type shows `Start` but hides the `Shutdown` its value has is a component, and the walk starts it. The values a worker constructor and a constructor handed to `Manage` build follow the same rule, checked against the constructor's result type.

The option that makes a binding's `Close` method its teardown is `credo.CloseOnShutdown()`, and the one that names a component is `credo.ComponentName(name)`. A component's name — its `credo.ComponentName` name, else its type as the lifecycle prints it (`*users.Service`) — labels it in logs, `/ready` and a `LifecycleError`; it is not a DI key ([container spec](container.md#registration-options)).

### `app.Start(ctx context.Context) error`

Start runs the start phase without a listener; `Run`, `RunContext` and `ServeContext` serve. It claims `building → starting`, prepares the App (`Finalize` and compile), runs the [start walk](#the-start-walk) and enters `running`, for an App served through `ServeHTTP` by an external `http.Server` or in a test. `app.Shutdown` stops what it started. It is accepted only in `building`: a second call, or a serve entry point after it, returns the state error. A preparation failure returns `credo: Start: …` and gives the slot back to `building`, as for the serve entry points. A failed `App.Start` rolls back what was built and leaves the App `stopped`, returning the [lifecycle error](#the-lifecycle-error); `ctx` interrupts the start phase like any [shutdown during the start phase](#a-shutdown-during-the-start-phase).

Whoever serves the App through `ServeHTTP` owns that server's admission and drain and completes them before `app.Shutdown`. The rule carries the drain order: the internal tier stops after the HTTP drain only if that drain has happened, and nothing in the App can perform it for the owner.

`testutil.Start(tb, app)` starts the App and leaves its shutdown to the end of the test, failing the test when the start fails. An App built by `testutil.NewApp` already registers its shutdown cleanup first, so that cleanup runs after every cleanup the test adds later — a test server closed through `t.Cleanup` is drained before the components stop — and `testutil.Start` adds none. Any other App gets a shutdown cleanup when `testutil.Start` starts it, so its test server is created after `testutil.Start` or closed with `defer`.

### `app.Manage(v, opts...)`

Adds a component that is not a DI binding: a value with `Shutdown`, or a constructor over DI parameters — `func(deps...) C` or `func(deps...) (C, error)` — that the start walk builds and that never becomes a binding. A function whose own type has `Shutdown` (a named func type) is a value, not a constructor. `credo.ComponentName("…")` names it (default: its type name; a duplicate panics), and `credo.Ingress()` places it in the ingress tier. A value has no edges and stops in reverse registration order within its tier; a constructor has its parameters' edges. A value without `Shutdown`, an App handed to its own `Manage`, a call after the container was finalized or after shutdown began, a value whose resource another registration owns — a binding, or another value handed to `Manage` — panic ([container spec](container.md#one-owner-per-resource)).

A value handed to `Manage` owns what it holds, and so does the value a constructor handed to `Manage` builds, which is not a binding and cannot be a view: when that value holds a resource another registration owns, the start fails once the value is built, naming the call, the built type, the owner and the remedy — drop the `Manage` call when the constructor returns the binding's own value, or, when it returns a wrapper that embeds the handle, hold the handle in a named field, so that the wrapper is a resource of its own ([container spec](container.md#one-owner-per-resource)):

```text
the value the constructor handed to Manage(relay) builds, a *Relay, holds the resource that the ProvideValue binding of *sqldb.DB owns, and a resource has one owner; hold the *sqldb.DB in a named field of *Relay, so that it is a resource of its own
```

A `Manage` call's options are its own resource's: `credo.ComponentName`, `credo.Ingress()` and `credo.OptionalReadiness()` shape the entry and the teardown of the value it owns.

### Contexts

Every context the lifecycle hands out lives as long as its call:

| Call | Context |
| --- | --- |
| `Start`, `OnStart` hook | Cancelled when the call returns, or earlier when a shutdown is requested during the start phase |
| `Shutdown`, `OnStop` hook, `Close(ctx)` of a `credo.CloseOnShutdown()` binding | The drain context: its deadline is the [drain deadline](#drain-context-derivation) |
| `Ready` | The probe request's values without its cancellation, with `CheckTimeout` as its deadline ([health spec](health.md#check-execution)) |

Work that outlives `Start` runs on a goroutine whose context derives from `context.WithoutCancel(ctx)` with its own cancel, which `Shutdown` calls; `Shutdown` is the one stop signal. A loop that keeps `Start`'s context stops as soon as `Start` returns. The session context the App keeps internally is observed only by reload ([`app.Reload`](#appreloadctx-contextcontext-configreloadresult-error)).

### The scoped `Infra` carrier

```go
// InfraFrom returns the scoped Infra of the component whose call ctx belongs
// to, or the zero Infra outside such a call.
func InfraFrom(ctx context.Context) credo.Infra
```

Every call the App makes on a component — `Start`, `Ready`, `Shutdown` and `Close(ctx)` — and every `OnStart` and `OnStop` hook receives a context carrying the component's scoped `Infra`, read with `credo.InfraFrom(ctx)`, a readiness check included. The scoped `Infra` carries the App's logger with `component=<name>`, the component's unit name: the name its readiness entry, a `LifecycleError` and the lifecycle's own records show, the one `credo.ComponentName` sets, else its type as the lifecycle prints it (`*users.Service`), `OnStart[i]`/`OnStop[i]` for a hook and `worker:<name>` for a worker. `context.WithoutCancel` keeps values, so work derived from the `Start` context sees the same `Infra`. Outside such a call `InfraFrom` returns the zero `Infra`, whose logger falls back to the framework default — a call in a handler compiles and logs there, so `InfraFrom`'s godoc names the other two paths: request code logs through `c.Logger()`, the request's logger or else the App's, and a constructor takes `credo.Infra`.

One key names an application part in the framework's records. The logger a constructor receives through an `Infra` parameter writes `component=<name>` too, with the name of the binding it builds — its type as the lifecycle prints it (`*users.Service`), or its `credo.ComponentName` name — and `app.NewInfra(name)`, a scope for code outside a lifecycle call, writes `component=<name>`. `*users.Service` and `*orders.Service` therefore stay apart. `module=<name>` names the framework package that wrote a record — the `http.Server.ErrorLog` bridge's `module=net/http` included — beside `request_id` and `connection_id`. Only framework infrastructure travels in the context, never application services.

A framework module whose value is built outside the App and started by it takes an optional logger in its own configuration and otherwise uses the App's. Precedence: the logger given to the module, unscoped; then `InfraFrom(ctx).Logger`; then the framework default. Every record a module writes carries `module=<name>`, added with `Logger.With`, whichever logger it goes to. The App's own subsystems — the `Use*` features and the worker registry — already log to the App's logger and are outside the rule; `httpclient` stays credo-free ([ADR-004](../adr/004-dependency-injection-and-infra.md)).

### The start walk

```
0. Framework start steps: UseI18n reads its catalogs (reported in the internal tier)
1. Internal tier, in dependency order: build each component with Start or Ready that is
   still unbuilt and every constructor handed to app.Manage, then call its Start (when
   its binding's type shows one)
2. Internal OnStart hooks, FIFO
3. Ingress tier, in dependency order: the same
4. Ingress OnStart hooks, FIFO
5. state = running; managed serving logs "credo: server started" and accepts
```

`ExampleApp_Shutdown` runs the walk and the drain over two internal components, an ingress one and hooks of both tiers.

- A component's `Start` runs after the `Start` of every component it depends on. No dependency crosses the tiers backwards: an internal component that depends on an ingress one fails `Finalize` (or, for a component found only on its built value, its construction) with the path and both remedies, so the order holds across the tiers. An ingress component that depends on an internal one starts after it.
- `Start`, `Ready` and the tier are planned from the binding's type; a binding whose type hides its value's `Start` is refused ([Component types](#component-types)), and a type with only `Start` is not a component and is never started. A component whose binding's type shows `Start` is started exactly once, by the walk — an `OnStart` hook that also starts it starts it a second time. A resource is started once, through its owner, and never through a view; a borrowed resource is not started.
- A component with neither `Start` nor `Ready` stays lazy: it is built when first resolved — during a request included — and shut down by the drain only if built.
- A framework start step that fails and a constructor error in step 1 or 3 are start failures. `Start` returns once the component is usable; it never blocks for the component's lifetime.
- Under managed serving the listener is bound before the walk, so `app.Addr()` is available in `Start` and in start hooks; it is nil under `App.Start`.

**A bound store is an ordinary component.** Step 1 builds and starts a database like any component with `Start`: `app.ProvideValue(db)` alone pings it in `(*sqldb.DB).Start` before its dependants start, reports it through its `Ready` and shuts it down after its consumers. There is no store registration and no start step of its own. A raw handle and a wrapper that embeds it, declared a view with `credo.View()`, give the database one ping and one readiness entry, the handle's ([Readiness](#readiness)). A borrowed database is not pinged, since its owner starts it. A failed ping is a failed `Start`: `sqldb` closes the database before `Start` returns the error, since the pool was opened by `Open`, not by `Start`, and [the failed-start rule](#a-failed-start) does not shut down a component whose `Start` failed. A database opened with `sqldb.WithOptionalStart()` logs a failed ping at Warn, with the component's name, the deadline and the error, and its `Start` returns nil with the pool open, since `database/sql` opens connections on demand once the server is back; the failed-start rule is unchanged, and the database still gates readiness unless it is bound with `credo.OptionalReadiness()` ([store spec](store.md)).

### A failed start

A `Start` or start hook that returns an error or panics, or a constructor that fails in the walk, fails the start. Nothing further starts, and the rollback stops, [in tiers](#shutdown-in-tiers) and in reverse dependency order, every component that was built — one built before `Run` and never started included — except the one whose `Start` failed: a `Start` that returns an error has released what it opened. Stop hooks run as in every teardown. A `Start` that panics is recovered, logged (`credo: component Start panic`) and reported as `panicked` with a `*credo.PanicError` of phase `PanicStart`, and the rollback continues. The listener is closed, no startup record is written, the App ends `stopped`, and the entry point returns the [lifecycle error](#the-lifecycle-error) naming each component with its phase and outcome. The rollback runs under the `WithShutdownTimeout` deadline counted from the failure.

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
4. Wait for an in-flight Reload, keeping the reload slot. At the deadline: log
   "credo: reload still in flight at the shutdown deadline; proceeding with
   teardown" at Error, join "credo: shutdown: reload still in flight" (wrapping
   the context error) into the result, and proceed with teardown
5. The internal tier: internal OnStop hooks LIFO, then internal components one at a time in
   reverse dependency order, values with no edges in reverse registration order
6. Clear bound address; state = stopped
7. Return errors.Join of the HTTP drain's error, the reload's and the lifecycle error
```

Stop hooks run before their tier's components, so a hook can still use them; a resource that is already a component stops in its tier on its own, so no `OnStop` hook bridges its teardown. The DI container enters closing when the internal components begin to stop — after the internal stop hooks, which still see a live container; a resolution after it returns an error wrapping `ErrDIClosed` ([container spec](container.md#shutdown)). Stop hooks and components capture their dependencies rather than resolve them; a resolution while the App is `stopping` is logged at Debug (`credo: Resolve during drain`). Bootstrap teardown runs steps 2–7 with no servers, stopping what bootstrap built, and the rollback of a failed or interrupted start runs the same tiers ([A failed start](#a-failed-start)).

**One deadline, spent in order.** Steps 3–5 share the drain deadline. The tiers run one after another, so a long HTTP drain or ingress tier leaves less of the budget for the internal tier, and a budget spent before step 5 stops nothing of it: once the deadline has ended, no further stop hook or teardown starts, and each one left is reported abandoned, its resources left to the process exit. There is no hard barrier:

- A `Shutdown`, `Close` or stop hook that has not returned at the deadline is abandoned, and never retried. The dependencies of an abandoned component are not stopped — nothing closes a database under a component that may still use it — and the abandoned component and every component it kept open (`kept_open`, with `KeptOpenBy`) are reported. A result already in when the deadline ends wins the boundary: that call has completed. A call that returns later is logged, never reported.
- A `Shutdown` that returns an error promptly is reported, and its dependencies are still stopped.
- Every `Start`, `Shutdown` and hook is panic-isolated; a panic is reported as that component's outcome, with the phases [the lifecycle error](#the-lifecycle-error) names.
- A construction still running when the drain reaches it blocks its dependencies and is shut down in order when it completes. One that completes after the deadline gets the single fixed five-second late attempt, logged and not reported — for a component its binding's type shows and for one found only on its built value alike ([container spec](container.md#shutdown) states when a shared or borrowed resource gets none).

**One teardown per resource.** Values that share a resource identity are one resource, shut down once, through its owner, after the consumers of every registration that holds it, in the owner's tier and under the owner's name; a borrowed resource is shut down by none of them ([container spec](container.md#one-owner-per-resource)). A view that obtained the resource through one of its consumers does not keep that consumer open: the consumer stops first. Every other dependency holds, including one between two such resources. A value whose construction the registry failed after its constructor returned it is released in order when no other registration owns its resource ([container spec](container.md#resource-identity-and-teardown)).

**One owner.** The first registration that claims a resource — its value has `Shutdown`, or it carries `credo.CloseOnShutdown()` or `credo.Borrowed()` — owns it, and a second claim is refused unless it is a view, declared with `credo.View()` or as an `Alias`: a panic at registration for a value handed in (`ProvideValue`, `Manage`), a failed start for a value a constructor builds. A view takes the owner's tier and none of the options that shape a resource, and depends on its owner, so a consumer of a view starts after the owner and stops before it. A view's owner exists before the view's value: an owner handed in, in either registration order, or an owner a constructor builds with the view built from its binding — an `Alias`, or a constructor that takes the owner's binding. `Finalize` refuses a view bound with `ProvideValue` whose value has `Shutdown` and whose resource no value handed in owns; a view a constructor builds whose value has `Shutdown` fails its build when no registration owns its resource, at the start or at a later `Resolve`; and a value a constructor builds that claims a resource a view already holds fails its build ([container spec](container.md#one-owner-per-resource)).

### Background work

Background work is a component: its `Start` launches the work and its `Shutdown` cancels it and waits for it ([Contexts](#contexts)). Its tier places its stop: ingress for work that enters the process — a consumer of an external queue, registered with `credo.Ingress()`, or a scheduled job — which stops beside the HTTP drain and before the internal components it uses, and internal for in-process work that handlers feed, which stops after the HTTP drain and before the components it depends on, so a job a request enqueues during the drain is still processed. A producer that hands work to a consumer component takes the consumer as a constructor dependency, so the producer stops first.

Each worker is such a component, named `worker:<name>`, which the App's worker registry adds as `app.Manage` adds one. A scheduled worker defaults to the ingress tier and a continuous worker to the internal tier; the registration's configuration may declare the other ([worker spec](worker.md#components)).

A worker is registered with `app.Workers()` as a value, a run function or a constructor ([worker spec](worker.md#registration-forms)), and none of them becomes a binding. A value adds one unit, the loop `worker:<name>`, with no edges. A run function — a function that takes exactly one `context.Context`, in any position, and returns only an `error`, such as the method expression `(*OutboxRelay).Run` of a bound worker — adds the loop alone, whose edges are its other parameters: the start walk resolves them once, after their components have started, and the drain stops the loop before them; under an internal loop a parameter bound in the ingress tier fails `Finalize` with the message and the two remedies of every internal component that depends on an ingress one, the first naming the worker's `Tier` field, since a worker registration takes no `credo.Ingress()`. A constructor adds two units that are not bindings, the built value `worker-value:<name>` and the loop `worker:<name>`, with an edge from the loop to the value: the start walk builds the value after its parameters' components have started and starts it before its loop when the constructor's result type shows `Start` and `Shutdown`, and the drain stops the loop, then tears the value down through the component registry, after the loop and before its dependencies. The built value is internal, as a constructor result bound without `credo.Ingress()` is, and the loop keeps its worker's tier: a scheduled worker's loop stops beside the HTTP drain while its value is torn down in the internal tier, after the handlers that use it. A built value with a teardown of its own that depends on an ingress component fails `Finalize` — or its build, when the teardown is found only on the value — with the path and the remedy of splitting the worker; when that ingress component is the binding whose value the constructor returns, the remedy is to register the worker's run function in place of the constructor. A built value that holds a resource another registration owns fails the start, naming the method expression `(*T).Run` when the constructor returns the binding's own value and a named field when it returns a wrapper ([One teardown per resource](#shutdown-in-tiers)).

### The lifecycle error

A start or shutdown failure is one `*credo.LifecycleError{Entries []LifecycleEntry, Cause error}`, obtained with `errors.AsType[*credo.LifecycleError]` from `Run`, `RunContext`, `ServeContext`, `App.Start` and `Shutdown`. Each `LifecycleEntry` names a component — by its `credo.ComponentName` name or its type name, a hook as `OnStart[i]` or `OnStop[i]` — with its `Tier`, its `Phase` (`PhaseStart`, or `PhaseShutdown` for the drain and for the rollback of a start), its `Outcome` (`failed`, `panicked`, `abandoned` at the deadline, or `kept_open` by an abandoned consumer, named in `KeptOpenBy`), its `Err` and its `Duration` (how long an abandoned call had run at the boundary, or how long a completed one took). Only those outcomes appear: a component that started and stopped cleanly has no entry, and a start or drain without such an outcome produces no lifecycle error — a successful teardown is logged at Debug with its duration, and there is no success report. `Cause` is the context error that ended a phase before it completed. The text reads `credo: lifecycle (cause): name (tier) phase outcome[ after d][ by …]: err; …`. It is an immutable snapshot taken at the boundary: a call returning later, or a late-construction attempt, is logged and never written back. `Unwrap() []error` exposes each failure and the cause, so `errors.Is` and `errors.As` traverse it. A component's panic is a `*credo.PanicError` ([container spec](container.md#the-panic-error)), found with `errors.AsType[*credo.PanicError]` from the entry's `Err` or from the lifecycle error: a constructor that panics in the start walk has phase `PanicConstruction`, a panicking `Start` phase `PanicStart`, and a panicking `Shutdown` or `Close` phase `PanicShutdown`; a panic in the late attempt (phase `PanicLateCleanup`) is only logged. `PanicPhase` is finer than the entry's `Phase`, which says only start or shutdown. A hook's panic is its error (`panic: …`, [hooks](#apponstartfn-opts-and-apponstopfn-opts)). A worker is named `worker:<name>`.

### Readiness

The lifecycle contributes to `/ready` the `Ready` of every component whose binding's type shows it (or that was handed to `app.Manage` with it), asked of the value the start walk built and never resolved per request; a worker contributes through its component's `Ready` when its registration sets a readiness condition ([worker spec](worker.md#health-integration)). The [health spec](health.md#readiness-aggregation) states the entry names, when the probes are built and how the sources combine; once the drain begins, `/ready` answers 503 `shutting_down` without asking ([health spec](health.md#shutdown)).

**One resource, one readiness entry.** `/ready` has one check per resource instead of one per unit that answers `Ready`: the owner's ([One teardown per resource](#shutdown-in-tiers)), whose `Ready` it asks, named by the owner's `credo.ComponentName` name, else by the owner's type (`*sqldb.DB`). An owner plans an entry when its type shows `Ready` and it is a component or borrowed; a view plans none, and takes neither `credo.OptionalReadiness()` nor `credo.ComponentName`. A wrapper with readiness of its own, one that overrides the `Ready` it embeds, holds the handle in a named field, so that it is a resource of its own with an entry of its own.

**Optional entries.** `credo.OptionalReadiness()` on `Provide`, `ProvideValue` or `Manage` reports the entry without letting it gate. `Ready` is still asked under the same deadline, the entry shows its own status — `down` when its check fails — and its latency, and a failure is logged like any failing check. The aggregation computes the top-level status from the gating checks, then reports `degraded`, answered with 200, in place of `up` when an optional check fails; `shutting_down` and a down gating entry still answer 503 ([health spec](health.md#readiness-aggregation)). The option panics at registration where the registration plans no readiness entry — a type without `Ready`, or one that is neither a component nor borrowed — and `Start` and `Shutdown` are unchanged by it.

A failure after `Start` has returned is reported through `Ready`. No component ends the App — the App's own listeners excepted — so a component that has stopped for good leaves the process alive and unready, and a failing readiness probe restarts nothing ([ADR-024](../adr/024-lifecycle-components.md#start) records the policy and the liveness alternative).

### `app.OnStart(fn, opts...)` and `app.OnStop(fn, opts...)`

Both register an anonymous component of a tier, internal unless `credo.Ingress()` is passed.

|  | `OnStart` | `OnStop` |
| --- | --- | --- |
| Slot | After its tier's components have started | Before its tier's components stop |
| Order within the tier | FIFO | LIFO |
| Context | Ends with the call; cancelled by a shutdown during the start phase | The drain context |
| Error or panic | A start failure: the start rolls back | Reported in the lifecycle error; the drain continues |
| Missed deadline | Ignoring the cancellation of an interrupted start: abandoned at the rollback deadline and reported, like a `Start` | Abandoned and reported, like a `Shutdown` |
| Runs on | The start walk | Every teardown: the drain, bootstrap teardown, and the rollback of a failed or interrupted start |

A stop hook must tolerate a start that never reached its counterpart. A hook is a leaf action: a resource with its own teardown is a component — handed to `app.Manage`, or bound with `credo.CloseOnShutdown()` when its teardown is `Close` — and process-level cleanup that must outlive every component belongs after `Run` returns. A nil hook, an option other than `credo.Ingress()`, or a registration after preparation or shutdown panics.

A hook that panics fails like one that returns an error: the panic is recovered, logged once (`credo: OnStart hook panic` or `credo: OnStop hook panic`, Error, with `hook_index`, the panic value and the stack) and reported as that hook's error (`panic: …`) — a start failure for a start hook, a lifecycle-error entry for a stop hook — so the rollback or the drain continues instead of the panic ending the process. Under managed serving `app.Addr()` is available inside start hooks — critical for port-0 scenarios.

Typical start-hook uses include cache warm-up. The `store/sqldb` migration wrapper's `Migrate` method matches the hook signature, so `app.OnStart(db.Migrate)` is convenient for development and deliberate single-replica deployments. Its context is cancelled by a shutdown during the start phase and carries no deadline of its own. Multi-replica production should instead run the same method once in a deadline-bounded pre-deploy job (see the [Store Spec](store.md)).

### Mounted Apps

`*App` has `Start(ctx) error` and `Shutdown(ctx) error`, so it is a startable component. A child App mounted with `app.Mount` is handed to `parent.Manage(child)`: its start phase runs in the parent's internal tier, before the parent's listener accepts, and it stops after the parent's HTTP drain, its own components in their own order. The parent's server is the child's owner, so the parent's HTTP drain is the drain the child's `Shutdown` relies on. Without the `Manage`, a child with anything to start panics on its first request with the gate's message, which the parent's recovery turns into a logged 500; a child with nothing to start serves without it. `parent.Manage(child)` composes independent Apps; an application's own modules share one App and its DI graph.

## Registration Guards

The following methods panic with `credo: <what> called after app was compiled or shut down` once `frozen` is set — at preparation admission (first `ServeHTTP` request or managed serve) or at bootstrap-shutdown admission. An explicit `Finalize` does not set it, so DI-backed controllers can still be resolved and bound afterwards:

| Method | Guard |
| --- | --- |
| `app.PreRouting()` | `checkFrozen("App.PreRouting")` |
| `app.Middleware()` | `checkFrozen("App.Middleware")` |
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
| `group.Middleware()` | `checkFrozen("Group.Middleware")` |
| `group.SetMeta()` / `group.RemoveMeta()` | `checkFrozen("Group.SetMeta")` / `checkFrozen("Group.RemoveMeta")` |
| `route.Name()` / `route.SetMeta()` / `route.Middleware()` | `checkFrozen("Route.Name")` / `checkFrozen("Route.SetMeta")` / `checkFrozen("Route.Middleware")` |

`app.Manage()` and the DI registrations are guarded by the container's phase instead: once the container is finalized — by `Finalize`, the first resolving call or preparation — or once shutdown began, they panic at the call ([container spec](container.md#registration-misuse)). The worker registrations add their components as `app.Manage` does and panic in the same phases, with the worker's own message ([worker spec](worker.md#validation)).

Registration is sequential and unsynchronized ([container spec](container.md#sequential-bootstrap)): each method checks once and then publishes, from the goroutine that builds the App. The guard catches a call made after preparation or shutdown admission, not one that races them, so registering while the App may already be served is unsupported.

The same fail-fast policy governs all registration APIs: misconfiguration panics at the call, while a registration that needs I/O — `UseI18n`'s catalogs, a database's ping — defers it to the start phase, where a failure is a start failure ([container spec](container.md#three-error-phases); the package documentation's "Panics and Errors" section).

## Thread Safety

- `State`, `IsRunning` and `Addr` are safe for concurrent use.
- Concurrent `Shutdown` calls pick one owner of the drain; a `Shutdown` that finds the start phase in progress always interrupts it.
- Shutdown either sees a stored preparation result or prevents an unfinished preparation from publishing.
- Concurrent `Reload` calls are serialized, and the drain waits for an in-flight one before the internal tier stops; signal-triggered reloads run on their own goroutine and signals during a reload coalesce into one follow-up. A reload publishes its snapshot as one generation ([config spec](config.md#reload-and-its-result)).

How each guarantee is implemented is [current behaviour](#synchronization).

## Current Behaviour

Not guaranteed: the mechanisms behind the contract above, as they are now. A release may change them without notice, provided the contract holds.

### Synchronization

- The state and the `frozen` flag are atomic. State transitions use `CompareAndSwap`, so exactly one goroutine wins; the drain's idempotency comes from the one `running` → `stopping` CAS, not a parallel `sync.Once`.
- The server, the redirect server, the session context, the bound address and the start phase in progress are guarded by one mutex. The start phase is published with the `building → starting` claim, so a `Shutdown` that reads `starting` always finds it.
- The stored preparation has an atomic fast path; a lock serializes its slow path and bootstrap-shutdown admission, which runs under that lock. The stored result, not a `sync.Once` that would count a panicking call as done, is what makes a preparation panic repeatable.
- `Reload` is serialized by a capacity-one slot channel that the drain also takes, and keeps, before the internal tier stops. The config snapshot swap is atomic.

### Components and readiness

- The root installs the scoped `Infra` carrier around each call `internal/di` makes, through an internal hook, and around each readiness check its aggregation runs.
- A resource is started once, through its owner: `Container.CallsStart` is false for a view, a plain holder and a borrowed owner.
- The container records the owner of each resource identity when the owner's value becomes known; the teardown snapshot and the readiness entries read it. `Finalize` refuses a `ProvideValue` view no value handed in owns, and the build of a constructor's value checks that a view's owner came first (`viewOrderErrorLocked`).
- `componentReadiness` builds the one check per resource; `planUnit` records `credo.OptionalReadiness()` on the unit, and `componentReadiness` carries it on the resource's check.

### Certificates and the 503

- The file-based key pair is held in an atomic pointer that `GetCertificate` reads.
- The lifecycle 503's body is precomputed.
