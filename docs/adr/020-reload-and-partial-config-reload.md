# ADR-020: Reload Signal, Partial Config Reload, and TLS Certificate Rotation

**Status:** Accepted **Date:** 2026-08-23 **Depends on:** ADR-005, ADR-006 **Related:** ADR-004, ADR-014, ADR-024 **Specification:** [Lifecycle spec](../specs/lifecycle.md#appreloadctx-contextcontext-error), [Config spec](../specs/config.md#reload--partial-reload-adr-020) **Guide:** [Configuration guide](../guides/configuration.md#reloading-configuration), [Deployment guide](../guides/deployment.md)

## Context

Credo applications run as long-lived services under systemd, container orchestrators, and process supervisors. All of these offer a "reload" verb (`systemctl reload`, `kill -HUP`) that operators expect to mean "pick up changed configuration and rotated certificates without dropping connections". A Go process that does not handle SIGHUP terminates on it, so a unit file with `ExecReload=kill -HUP $MAINPID` kills an unprepared service — the opposite of what the operator asked for.

Two constraints shape the design:

1. **ADR-005 makes config a startup snapshot.** `config.Load()` produces a `*config.Config`; modules call `Unmarshal("section", &typed)` once at the boundary and inject the typed struct through DI. A service that received `DBConfig` by value at construction cannot observe a later change to the underlying snapshot. A "reload everything" promise is structurally unfulfillable: server port, DI graph, middleware order and route tables are restart-only. A reload that silently applies some values and not others is worse than no reload at all.

2. **TLS is server config (ADR-006).** A key pair loaded once at startup keeps serving a stale certificate after on-disk rotation, and short-lived certificates (ACME, internal PKI with 24h–90d validity) rotate on disk while the process keeps running. The owner of a `*tls.Config` (`WithTLSConfig`) can rotate through its own `GetCertificate`; a file-based source (`WithTLSFiles`, `server.tls.*`) needs the framework to do it.

## Decision

### A reload is an explicit, opt-in, partial operation

A reload is a lifecycle event with one trigger, `app.Reload(ctx)` (SIGHUP under `Run`), and two hook kinds: typed per-section subscribers (`app.OnConfigChange[T]`) and generic hooks (`app.OnReload`, FIFO). `credo.WithReloadTimeout` bounds signal-triggered reloads and `credo.WithoutReloadSignals` turns the signal trigger off. Signatures and contracts: [lifecycle spec](../specs/lifecycle.md#appreloadctx-contextcontext-error).

Only what is _subscribed_ is live. Every other part of the configuration is restart-only, and the framework says so out loud (see "Unsubscribed changes are reported, never applied").

### SIGHUP triggers `Reload` — on Unix, under `Run()` only

`Run()` registers SIGHUP alongside SIGINT/SIGTERM. A SIGHUP calls `app.Reload` with a context bounded by `WithReloadTimeout` (default 30s; also settable through the `server.reload_timeout` config key, mirroring `server.shutdown_timeout`). Signal-triggered reloads run off the signal loop's goroutine, so the loop stays responsive: a SIGINT/SIGTERM that lands during a long reload starts the drain immediately and resets signal delivery at once (a second signal force-kills as usual), rather than waiting for the reload to return. At most one reload runs at a time; signals that arrive while one is in flight are coalesced into at most one follow-up reload. SIGHUP never terminates the process, and a failed reload never terminates the process either: a service that keeps running on its previous configuration is strictly better than one that dies because an operator fat-fingered a YAML file.

`RunContext` and `ServeContext` install no signal handler; their callers use `app.Reload(ctx)` directly (for example from an admin endpoint or an orchestrator hook). On Windows there is no SIGHUP; registration of hooks and `app.Reload` work identically, only the signal path is absent.

**Opting out — `credo.WithoutReloadSignals()`**: disables the reload trigger under `Run` with subscribe-and-ignore semantics — SIGHUP is still captured, but the handler logs an Info line instead of calling `Reload`. Still-subscribed is deliberate and follows the rule that keeps the signal channel subscribed through the drain: under `Run`, SIGHUP must never fall through to its default action and terminate the process non-gracefully (a stray logrotate postrotate or a forgotten `ExecReload` would otherwise kill production). The three signal policies are each one API away: reload (`Run`, default), logged no-op (`Run` + `WithoutReloadSignals`), raw Unix disposition (`RunContext`/`ServeContext`). Rejected alternative: not installing the handler at all — "honest" default disposition, but it makes the middle policy unreachable and turns a stray HUP into an immediate, drain-less death; anyone who wants that semantics already has the signal-free entry points. Programmatic `app.Reload` is unaffected; signal policy is a code-level decision, so there is deliberately no `server.*` config key for it.

### `Reload` is running-only, serialized, and never partially visible

`app.Reload` succeeds only in the `running` state: before it there is nothing to reload, and a stopping App must not start new work against components that are being torn down. Concurrent calls are serialized by a capacity-one slot rather than a mutex, so waiting is context-aware: a queued caller gives up on its own context, or returns the not-running error without reloading when shutdown begins, and the drain can take the slot and keep it. A caller that waits performs its own full reload, which gives the one guarantee callers need: after `Reload` returns, the snapshot is at least as new as when it was called. The snapshot swap is atomic, so readers observe the old or the new snapshot, never a mix. States, errors and their signal-path treatment: [lifecycle spec](../specs/lifecycle.md#appreloadctx-contextcontext-error).

### Reload and shutdown are coordinated; cancellation is cooperative

Reload callbacks may be using internal components (re-opening a log sink, swapping a client), so a reload in flight when shutdown begins is coordinated with the drain in tiers ([ADR-006](006-application-lifecycle.md), [ADR-024](024-lifecycle-components.md)). Every participant, subscriber, and hook receives a context that ends with either the caller's context or the App's session, and the drain cancels the session first, which tells the reload to stop. After the HTTP drain and the ingress tier, and before the internal tier, the drain waits for the in-flight reload and keeps the slot so no later reload can start. If the shutdown deadline expires first, the reload is logged and reported as still in flight (`credo: shutdown: reload still in flight`) and teardown proceeds, the same contract as an abandoned stop hook. Sequence: [lifecycle spec](../specs/lifecycle.md#shutdown-in-tiers).

Cancellation is cooperative: a Go callback cannot be forcibly stopped, so a hook that ignores its context can still overlap the internal tier's teardown, and the reported error is the framework's honest account of that. Rejected: making the reload wait a hard barrier that is never abandoned — a stuck reload hook would then hold the whole process open past the shutdown budget, which is worse for an operator than a logged overlap. Work with a genuine live-dependency requirement belongs to a component that depends on what it uses: its `Shutdown` stops admitting new reload work and waits for the in-flight callback, and it stops before its dependencies because the drain follows the dependency order — not the dependency's teardown, which would close the resource before the reload is even told to stop.

### Validate before publish, never roll back after

A reload stages a candidate snapshot first, decodes every affected `OnConfigChange[T]` subscriber from it, and validates the value when `T` implements `validation.Validatable`. Any failure aborts the whole reload before anything is published: the previous snapshot stays current, no subscriber is invoked, and `Reload` returns the joined decode/validation errors. Typed decode thus doubles as the schema check that the untyped config store lacks, and a bad edit can never half-apply.

After publication, the framework's reload participants (file-based TLS rotation), the affected subscribers in registration order, and the `OnReload` hooks all run; an error or recovered panic is collected, the sequence continues, and `Reload` returns the joined errors. There is no rollback, because earlier subscribers may already have applied their new values and a half-rolled-back state is harder to reason about than a logged partial failure. The step-by-step sequence: [lifecycle spec](../specs/lifecycle.md#appreloadctx-contextcontext-error).

### Unsubscribed changes are reported, never applied

After publishing, every leaf key in `Changes` that no subscription covers is logged at Warn: `config changed but no reloadable consumer is registered; restart required` with the key paths (never the values — they may be secrets). This is the central promise of the design: an operator who changes `server.port` and reloads learns immediately that a restart is needed instead of discovering it at the next incident.

### `config.Reloader` and `config.Changes`

The config store takes part through optional interfaces in the `config` package, so `RawConfig` stays a two-method interface (ADR-005): a two-phase `Stager` (stage a candidate with its `Changes`, commit later) and a one-shot `Reloader`. `*config.Config` implements both. The App prefers `Stager` because only a staged candidate makes validate-before-publish possible; `Reloader` stays for simple custom stores, which are published first and validated afterwards, so a bad value reaches readers before it is reported. A store that implements neither runs only the participants and `OnReload` hooks, and every `OnConfigChange` subscription is a registration-time panic for that app (a subscription that can never fire is a misconfiguration, not a silent no-op). `Changes` records key paths only, never values, so it is safe to log. Interfaces and semantics: [config spec](../specs/config.md#reload--partial-reload-adr-020).

### `OnConfigChange[T]` is a generic method on `*App`

`OnConfigChange[T](key, fn)` follows the Go 1.27 concrete-type generic-method convention of `Provide[T]`/`Resolve[T]`/`GetConfig[T]`. It is subject to the same registration guards as `OnStart` (rejected once the App is frozen, that is prepared or shut down). The same key may have several subscribers; nested keys are independent subscriptions (`"databases"` and `"databases.primary"` can both exist; both fire when `databases.primary.dsn` changes).

The subscriber receives the decoded `T` and is responsible for applying it atomically in its own domain (`atomic.Pointer[T]`, `slog.LevelVar.Set`, swapping a rate-limit bucket, re-opening a file). The framework does not mutate DI singletons, re-run constructors, or touch handlers: the DI graph is immutable after `Finalize` (ADR-004), and reload does not change that.

### File-based TLS rotates on reload

`WithTLSFiles` and `server.tls.*` serve the key pair through a `GetCertificate` callback backed by an `atomic.Pointer[tls.Certificate]` (one atomic load per handshake; no measurable cost). Preflight loads and validates the initial pair, so a missing or mismatched pair still fails the start.

The framework registers an internal reload participant for this source that runs on **every** reload, not only when `server.tls.*` keys change: certificate files rotate in place without any config-key change, which is invisible to `Changes`. The participant reads the paths (fixed for `WithTLSFiles`; from the new snapshot for `server.tls.*`), calls `tls.LoadX509KeyPair`, and on success swaps the pointer. On failure the previous certificate keeps serving and the error is reported through the normal reload error path. Existing connections are unaffected; new handshakes see the new certificate immediately. `WithHTTPRedirect` is unaffected (it serves plaintext).

`WithTLSConfig` is left entirely alone: the caller owns the `*tls.Config` and its `GetCertificate`, and may use `OnReload` to drive their own rotation.

### Observability

Reload is a lifecycle event, not a request: it writes framework records to the App logger — an Info summary per reload, an Error per failed step, the restart-required Warn ([lifecycle spec](../specs/lifecycle.md#appreloadctx-contextcontext-error)). The observability release ([ADR-014](014-observability.md)) is to add a `credo_reload_total{result}` counter and a reload-duration histogram through the same `Infra` carrier; nothing in this decision depends on them.

## Rejected Alternatives

- **Whole-config hot reload (mutate the snapshot in place).** Incompatible with ADR-005: typed structs already injected cannot see the change, so "reload" would apply an undefined subset. The opt-in subscription model makes the applied subset explicit and the unapplied remainder loud.
- **Restart-on-reload (re-exec with socket inheritance, à la nginx binary upgrade).** Correct in principle, but it is a deployment strategy, not a framework hook; containers and systemd already provide zero-downtime restarts. Out of scope.
- **Filesystem watching (fsnotify / polling) for config and certificates.** Adds a dependency and a background goroutine, fires on partial writes, behaves differently on bind mounts and Kubernetes ConfigMap symlink swaps, and removes operator control over _when_ a change applies. Reload stays trigger-driven; an application that wants watching can call `app.Reload` from its own watcher.
- **SIGUSR1/SIGUSR2 for reload.** SIGHUP is the convention every supervisor and operator already knows; USR signals stay free for application use.
- **`OnConfigChange` on `RawConfig` / an `Option`.** A generic method cannot live on an interface, and subscriptions are post-construction registrations like other `On*` hooks (API verb convention).
- **Rollback of subscribers on partial failure.** Requires every subscriber to expose an inverse and still cannot undo side effects (re-opened files, already-served handshakes). Validate-before-publish prevents the common failure (bad input); the remaining failures are logged and returned.
- **Re-deriving `CREDO_ENV` on reload.** Changing environment swaps entire file sets and almost always implies restart-only sections; the environment name is fixed at first load and switching it is a restart.

## Consequences

- `systemctl reload` / `kill -HUP` is safe and useful for Credo services: SIGHUP never kills the process under `Run()`.
- ADR-005's "config is a startup snapshot" remains true for DI-injected values.
- The process environment is re-read, but a supervisor's `EnvironmentFile=` is applied only at process start, so env-sourced changes require a restart ([deployment guide](../guides/deployment.md#systemd)).
- `systemctl reload` cannot report reload failure (signal delivery is the only thing systemd observes). Teams that need synchronous failure expose `app.Reload` behind an authenticated admin endpoint and use `ExecReload=curl …` ([deployment guide](../guides/deployment.md#reload-without-a-signal)).
