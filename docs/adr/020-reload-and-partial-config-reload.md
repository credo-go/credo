# ADR-020: Reload Signal, Partial Config Reload, and TLS Certificate Rotation

**Status:** Accepted (implementation tracked in [TODO.md](../../TODO.md), Phase 3.8) **Date:** 2026-08-23 **Depends on:** ADR-005, ADR-006 **Related:** ADR-004, ADR-014

## Context

Credo applications run as long-lived services under systemd, container orchestrators, and process supervisors. All of these offer a "reload" verb (`systemctl reload`, `kill -HUP`) that operators expect to mean "pick up changed configuration and rotated certificates without dropping connections". Today Credo only handles SIGINT/SIGTERM (`lifecycleManager.runSignal`); SIGHUP falls through to Go's default handler and terminates the process. A unit file with `ExecReload=kill -HUP $MAINPID` therefore kills the service — the opposite of what the operator asked for.

Two constraints shape the design:

1. **ADR-005 makes config a startup snapshot.** `config.Load()` produces a `*config.Config`; modules call `Unmarshal("section", &typed)` once at the boundary and inject the typed struct through DI. A service that received `DBConfig` by value at construction cannot observe a later change to the underlying snapshot. A "reload everything" promise is structurally unfulfillable: server port, DI graph, middleware order and route tables are restart-only. A reload that silently applies some values and not others is worse than no reload at all.

2. **TLS is server config (ADR-006).** `WithTLSFiles` / `server.tls.*` load the key pair once at preflight via `tls.LoadX509KeyPair` into `tls.Config.Certificates`. Short-lived certificates (ACME, internal PKI with 24h–90d validity) rotate on disk while the process keeps serving the stale pair. Users of `WithTLSConfig` can already solve this with `GetCertificate`; users of the file-based sources cannot.

## Decision

### A reload is an explicit, opt-in, partial operation

Credo adds a reload lifecycle event with three public surfaces:

```go
app.Reload(ctx context.Context) error                       // programmatic trigger
app.OnReload(fn func(ctx context.Context) error)             // generic hook, FIFO
app.OnConfigChange[T](key string, fn func(ctx context.Context, next T) error)  // typed, per-section subscriber
credo.WithReloadTimeout(d time.Duration) Option             // budget for signal-triggered reloads (default 30s; also server.reload_timeout)
```

Only what is _subscribed_ is live. Every other part of the configuration is restart-only, and the framework says so out loud (see "Unsubscribed changes are reported, never applied").

### SIGHUP triggers `Reload` — on Unix, under `Run()` only

`Run()` registers SIGHUP alongside SIGINT/SIGTERM. A SIGHUP calls `app.Reload` with a context bounded by `WithReloadTimeout` (default 30s; also settable through the `server.reload_timeout` config key, mirroring `server.shutdown_timeout`). Signal-triggered reloads run off the signal loop's goroutine, so the loop stays responsive: a SIGINT/SIGTERM that lands during a long reload starts the drain immediately and resets signal delivery at once (a second signal force-kills as usual), rather than waiting for the reload to return. At most one reload runs at a time; signals that arrive while one is in flight are coalesced into at most one follow-up reload. SIGHUP never terminates the process, and a failed reload never terminates the process either: a service that keeps running on its previous configuration is strictly better than one that dies because an operator fat-fingered a YAML file.

`RunContext` and `ServeContext` stay signal-free, as they are today; their callers use `app.Reload(ctx)` directly (for example from an admin endpoint or an orchestrator hook). On Windows there is no SIGHUP; registration of hooks and `app.Reload` work identically, only the signal path is absent. The platform split lives in `signal_unix.go` / `signal_other.go` build-tagged files.

**Opting out — `credo.WithoutReloadSignals()`**: disables the reload trigger under `Run` with subscribe-and-ignore semantics — SIGHUP is still captured, but the handler logs an Info line (`credo: reload signal ignored (reload signals disabled)`) instead of calling `Reload`. Still-subscribed is deliberate and extends the existing doctrine that keeps the channel subscribed through the drain: under `Run`, SIGHUP must never fall through to its default action and terminate the process non-gracefully (a stray logrotate postrotate or a forgotten `ExecReload` would otherwise kill production). The three signal policies are each one API away: reload (`Run`, default), logged no-op (`Run` + `WithoutReloadSignals`), raw Unix disposition (`RunContext`/`ServeContext`). Rejected alternative: not installing the handler at all — "honest" default disposition, but it makes the middle policy unreachable and turns a stray HUP into an immediate, drain-less death; anyone who wants that semantics already has the signal-free entry points. Programmatic `app.Reload` is unaffected; signal policy is a code-level decision, so there is deliberately no `server.*` config key for it.

### `Reload` is running-only, serialized, and never partially visible

`app.Reload` succeeds only in the `running` state. Before `running` it returns an error (nothing to reload); during `stopping`/`stopped` it returns the not-running error (mapped to a no-op for the signal path). Concurrent calls are serialized by a capacity-one slot rather than a mutex, so waiting is context-aware: a queued caller performs its own full reload once the slot frees (the observable guarantee is "after my `Reload` returns, the snapshot is at least as new as when I called"), gives up with its own `ctx.Err()` if its context ends first, and returns the not-running error — without reloading — if the lifecycle ends while it waits. The running check is repeated after the slot is acquired for the same reason.

### Reload and shutdown are coordinated; cancellation is cooperative

A reload that is in flight when shutdown begins is treated like an `OnDrain` subsystem. Every participant, subscriber, and hook receives a context that is cancelled when either the caller's context or the application lifecycle ends, so drain step 2 (lifecycle cancellation) tells the reload to stop. After the HTTP/`OnDrain` phase and before DI teardown, the drain takes the reload slot — waiting for the in-flight reload to return, then holding the slot so no later reload can start — because reload callbacks may be using DI infrastructure (re-opening a log sink, swapping a client). If the shutdown deadline expires first, the reload is logged and reported as still in flight (`credo: shutdown: reload still in flight`) and teardown proceeds with the expired context, the same contract as an over-deadline `OnDrain` hook.

Cancellation is cooperative: a Go callback cannot be forcibly stopped, so a hook that ignores its context can still overlap infrastructure teardown, and the reported error is the framework's honest account of that. Rejected: making the reload wait a hard barrier like `OnPreDrain` — a stuck reload hook would then hold the whole process open past the shutdown budget, which is worse for an operator than a logged overlap; a hook with a genuine live-dependency requirement registers its own stop-and-await barrier in `OnPreDrain` — stop admitting new reload work and wait for the in-flight callback there, before lifecycle cancellation — not the dependency's teardown, which would close the resource before the reload is even told to stop.

Reload runs in four steps:

1. **Load candidate.** If the registered `RawConfig` implements `config.Reloader`, call `Reload()` to produce a candidate snapshot plus a `Changes` diff. Sources are re-read with the options captured at the original `Load` (files, prefix, `.env` path). The environment _name_ (`CREDO_ENV`) is fixed at first load: switching environments is a restart.
2. **Validate candidate (no side effects).** For every `OnConfigChange[T]` subscription whose key prefix is affected by `Changes`, decode `T` from the candidate and, if `T` implements `validation.Validatable`, validate it. Any failure aborts the whole reload before anything is published: the previous snapshot stays current, no subscriber is invoked, and `Reload` returns the joined decode/validation errors. Typed decode thus doubles as the schema check that the untyped config store lacks.
3. **Publish and notify.** Swap the snapshot atomically (`Unmarshal`/`Exists`/`Get` observe old-or-new, never a mix). Invoke affected `OnConfigChange` subscribers in registration order with the decoded `T`, then all `OnReload` hooks FIFO. A subscriber or hook error is recorded and the sequence continues; there is no rollback, because earlier subscribers may already have applied their new values and a half-rolled-back state is harder to reason about than a logged partial failure. Panics are recovered into errors.
4. **Report.** `Reload` returns `errors.Join` of step-3 errors (nil on full success). The framework logs one Info line on completion (duration, number of changed keys, number of subscribers notified, error count) and one Error line per failed hook, using the request-less framework logger.

### Unsubscribed changes are reported, never applied

After publishing, every leaf key in `Changes` that no subscription covers is logged at Warn: `config changed but no reloadable consumer is registered; restart required` with the key paths (never the values — they may be secrets). This is the central promise of the design: an operator who changes `server.port` and reloads learns immediately that a restart is needed instead of discovering it at the next incident.

### `config.Reloader` and `config.Changes`

```go
// config package
type Reloader interface {
    Reload() (Changes, error)          // one-shot: re-read and publish
}

type Stager interface {
    Stage() (Staged, error)            // two-phase: candidate first, Commit later
}

type Staged interface {
    RawConfig                          // reads the candidate
    Changes() Changes
    Commit()                           // publish atomically
}

type Changes struct { /* sorted leaf key paths that differ between old and new */ }
func (c Changes) Affects(prefix string) bool   // true if any changed key equals prefix or is under "prefix."
func (c Changes) Keys() []string               // copy of the changed leaf keys
func (c Changes) Empty() bool
```

`*config.Config` implements both. `Stage` re-runs the pipeline that `Load` ran into a candidate tree, computes the flattened symmetric difference against the current snapshot, and returns a `Staged` handle; `Commit` swaps the tree under the config's RWMutex so readers never observe a partially merged state; `Reload` is `Stage` + `Commit`. On any load error the current snapshot is untouched. `RawConfig` itself stays a two-method interface (ADR-005). The App prefers `Stager` because only a candidate stage makes step 2 (validate before publish) possible; a custom store that implements only `Reloader` is published first and validated afterwards, and a store that implements neither skips step 1: `Reload` then only runs participants and `OnReload` hooks, and every `OnConfigChange` subscription is a registration-time panic for that app (a subscription that can never fire is a misconfiguration, not a silent no-op).

### `OnConfigChange[T]` is a generic method on `*App`

`OnConfigChange[T](key, fn)` follows the Go 1.27 concrete-type generic-method convention already used by `Provide[T]`/`Resolve[T]`/`GetConfig[T]`. It is subject to the same registration guards as `OnStart` (rejected once the app has started). The same key may have several subscribers; nested keys are independent subscriptions (`"databases"` and `"databases.primary"` can both exist; both fire when `databases.primary.dsn` changes).

The subscriber receives the decoded `T` and is responsible for applying it atomically in its own domain (`atomic.Pointer[T]`, `slog.LevelVar.Set`, swapping a rate-limit bucket, re-opening a file). The framework does not mutate DI singletons, re-run constructors, or touch handlers: the DI graph is immutable after `Finalize` (ADR-004), and reload does not change that.

### File-based TLS rotates on reload

`WithTLSFiles` and `server.tls.*` no longer load the key pair into `tls.Config.Certificates`. They install a `GetCertificate` callback backed by an `atomic.Pointer[tls.Certificate]` (one atomic load per handshake; no measurable cost). Preflight still loads and validates the initial pair exactly as today, so startup failure modes are unchanged.

The framework registers an internal reload participant for this source that runs on **every** reload, not only when `server.tls.*` keys change: certificate files rotate in place without any config-key change, which is invisible to `Changes`. The participant reads the paths (fixed for `WithTLSFiles`; from the new snapshot for `server.tls.*`), calls `tls.LoadX509KeyPair`, and on success swaps the pointer. On failure the previous certificate keeps serving and the error is reported through the normal reload error path. Existing connections are unaffected; new handshakes see the new certificate immediately. `WithHTTPRedirect` needs no change (it serves plaintext).

`WithTLSConfig` is left entirely alone: the caller owns the `*tls.Config` and its `GetCertificate`, and may use `OnReload` to drive their own rotation. The preflight certificate-source check (`Certificates || GetCertificate || GetConfigForClient`) already accepts the new shape.

### Observability

Reload is a lifecycle event, not a request: it logs through the framework default logger with `"event"="reload"`. Phase 3.5 adds a `credo_reload_total{result}` counter and a reload-duration histogram through the same `Infra` carrier; nothing in this ADR depends on that.

## Rejected Alternatives

- **Whole-config hot reload (mutate the snapshot in place).** Incompatible with ADR-005: typed structs already injected cannot see the change, so "reload" would apply an undefined subset. The opt-in subscription model makes the applied subset explicit and the unapplied remainder loud.
- **Restart-on-reload (re-exec with socket inheritance, à la nginx binary upgrade).** Correct in principle, but it is a deployment strategy, not a framework hook; containers and systemd already provide zero-downtime restarts. Out of scope.
- **Filesystem watching (fsnotify / polling) for config and certificates.** Adds a dependency and a background goroutine, fires on partial writes, behaves differently on bind mounts and Kubernetes ConfigMap symlink swaps, and removes operator control over _when_ a change applies. Reload stays trigger-driven; an application that wants watching can call `app.Reload` from its own watcher.
- **SIGUSR1/SIGUSR2 for reload.** SIGHUP is the convention every supervisor and operator already knows; USR signals stay free for application use.
- **`OnConfigChange` on `RawConfig` / an `Option`.** A generic method cannot live on an interface, and subscriptions are post-construction registrations like other `On*` hooks (API verb convention).
- **Rollback of subscribers on partial failure.** Requires every subscriber to expose an inverse and still cannot undo side effects (re-opened files, already-served handshakes). Validate-before-publish prevents the common failure (bad input); the remaining failures are logged and returned.
- **Re-deriving `CREDO_ENV` on reload.** Changing environment swaps entire file sets and almost always implies restart-only sections; treated as restart.

## Consequences

- `systemctl reload` / `kill -HUP` becomes safe and useful for Credo services; SIGHUP no longer kills the process under `Run()`.
- Partial reload is explicit: the set of live sections is exactly the set of `OnConfigChange` subscriptions plus file-based TLS, and everything else produces a Warn naming the restart-only keys.
- ADR-005's "config is a startup snapshot" remains true for DI-injected values; the ADR gains a paragraph pointing here for the reloadable exception.
- `config.Config` gains internal synchronization; `Unmarshal` cost is unchanged (one atomic load).
- File-based TLS gains rotation at zero handshake cost; the `Certificates`-slice shape is no longer observable, which only matters to tests that inspected it.
- Process environment is re-read but a supervisor's `EnvironmentFile=` is applied only at process start, so env-sourced changes require restart; documented prominently in the configuration guide.
- `systemctl reload` cannot report reload failure (signal delivery is the only thing systemd observes). Teams that need synchronous failure should expose `app.Reload` behind an authenticated admin endpoint and use `ExecReload=curl …`; documented in the deployment guide.
- New public API (additive, minor release): `App.Reload`, `App.OnReload`, `App.OnConfigChange[T]`, `WithReloadTimeout`, `config.Reloader`, `config.Changes`, `(*config.Config).Reload`.
- Reload hooks must treat their context as the lifecycle signal it is: a hook that blocks without watching `ctx.Done()` delays (and is reported by) shutdown; it never blocks termination signals.
