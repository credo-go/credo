# ADR-020: Reload Signal, Partial Config Reload, and TLS Certificate Rotation

**Status:** Accepted **Date:** 2026-08-23 **Depends on:** ADR-005, ADR-006 **Related:** ADR-004, ADR-014, ADR-024 **Specification:** [Lifecycle spec](../specs/lifecycle.md#appreloadctx-contextcontext-error), [Config spec](../specs/config.md#reload--partial-reload-adr-020) **Guide:** [Configuration guide](../guides/configuration.md#reloading-configuration), [Deployment guide](../guides/deployment.md)

## Context

Credo applications run as long-lived services under systemd, container orchestrators, and process supervisors. All of these offer a "reload" verb (`systemctl reload`, `kill -HUP`) that operators expect to mean "pick up changed configuration and rotated certificates without dropping connections". A Go process that does not handle SIGHUP terminates on it, so a unit file with `ExecReload=kill -HUP $MAINPID` kills an unprepared service — the opposite of what the operator asked for.

Two constraints shape the design:

1. **ADR-005 makes config a startup snapshot.** `config.Load()` produces a `*config.Config`; modules call `Unmarshal("section", &typed)` once at the boundary and inject the typed struct through DI. A service that received `DBConfig` by value at construction cannot observe a later change to the underlying snapshot. A "reload everything" promise is structurally unfulfillable: server port, DI graph, middleware order and route tables are restart-only. A reload that silently applies some values and not others is worse than no reload at all.

2. **TLS is server config (ADR-006).** A key pair loaded once at startup keeps serving a stale certificate after on-disk rotation, and short-lived certificates (ACME, internal PKI with 24h–90d validity) rotate on disk while the process keeps running. The owner of a `*tls.Config` (`WithTLSConfig`) can rotate through its own `GetCertificate`; a file-based source (`WithTLSFiles`, `server.tls.*`) needs the framework to do it.

## Decision

### A reload is an explicit, opt-in, partial operation

> **Current until v0.26.0 (R15):** the next two paragraphs name the two hook kinds, `app.OnConfigChange[T]` and `app.OnReload`, which go; [live sections](#a-section-is-a-snapshot-unless-it-is-declared-live) replace them.

A reload is a lifecycle event with one trigger, `app.Reload(ctx)` (SIGHUP under `Run`), and two hook kinds: typed per-section subscribers (`app.OnConfigChange[T]`) and generic hooks (`app.OnReload`, FIFO). `credo.WithReloadTimeout` bounds signal-triggered reloads and `credo.WithoutReloadSignals` turns the signal trigger off. Signatures and contracts: [lifecycle spec](../specs/lifecycle.md#appreloadctx-contextcontext-error).

Only what is _subscribed_ is live. Every other part of the configuration is restart-only, and the framework says so out loud (see "Unsubscribed changes are reported, never applied").

### A section is a snapshot unless it is declared live

> **Pending v0.26.0 (R15):** not implemented yet; a section follows a reload only through an `app.OnConfigChange[T]` subscriber, `app.OnReload` hooks run at every reload, and file-based TLS is a framework reload participant.

A reload has one trigger, `app.Reload(ctx)` (SIGHUP under `Run`), and what it changes is declared per section:

```go
var Limits = config.Section[LimitsConfig]("limits")

app.Provide(Limits.Live) // decoded and validated when built, replaced whole by each reload that changes it

func NewOrderService(limits *config.Live[LimitsConfig]) *OrderService // the signature says the value is live

l := s.limits.Load() // once per operation: one atomic load, a value no reload changes
```

- **Static by default, live by declaration.** A section bound through its descriptor's `Decode` ([ADR-005](005-configuration-architecture.md#a-section-is-named-once-configsectiont)) is a snapshot, and a change to it takes a restart. `Section[T].Live(src config.RawConfig) (*config.Live[T], error)` decodes and validates the section as `Decode` does and registers the handle with the configuration it read; each reload that changes a key under the section replaces the handle's value whole. No other binding changes at a reload, and no singleton is rebuilt ([ADR-004](004-dependency-injection-and-infra.md)).
- **`config.Live[T]`.** `Load() T` returns the current value with one atomic load, and a consumer calls it once per operation. `OnChange(fn func(ctx context.Context, prev, next T) error)` registers a callback for state derived from the section — a limiter, a compiled pattern, a `slog.LevelVar` — run after the new value is published. `config.Fixed(v)` returns a handle whose value is `v` and never changes, for a consumer's unit test. What the type does not enforce is documented with it: `Load` in each operation, derived state in `OnChange`, and a value's slices and maps read, never written, since every `Load` of one reload shares them.
- **One nature per section.** A section is static or live within one configuration. Every read through its descriptor records the section's nature in the `*config.Config` it reads — `Decode`, `GetConfig` and `MustGetConfig` as static, `Live` as live — and the framework's read of `server` counts as static. A read of the other nature fails, naming the section and both reads: a constructor at its construction, the composition root at its call. A section nested in another, or one that contains it, counts as the same section, since the keys it reads are the other's. Read both ways, one section would run in two generations: the live handle would take a reload's value while the snapshot kept the old one, and the report would count the section's keys as applied. Keys that need a restart and keys a reload applies are therefore separate sections. A raw `Unmarshal` with a string key is outside the check.

Kept from the reload as it was designed: the operator's trigger, validation before publish, no rollback, the report of the changes that need a restart, and the rejected alternatives below, whole-configuration reload, restart-on-reload and watching configuration files among them. Replaced: per-key subscribers, generic hooks and framework participants.

Rejected: removing runtime reload, which would leave an application whose operational values — a log level, a rate limit, a timeout, a maintenance flag — must change without a restart to build the trigger, the staging, the validation and the drain's step itself, the toolkit answer the all-in-one framework exists to spare it, while ConfigMap and Secret volume files change in place under Kubernetes too; `OnConfigChange` taking the descriptor, which keeps a callback that applies the fields it chooses while its whole section counts as covered, and the holder each subscriber builds; `OnReload`, or a component capability run at every reload, since derived state follows its section through `OnChange` and a refresh unrelated to configuration installs its own SIGHUP handler, which `signal.Notify` serves beside Credo's; every section live, which makes every read an atomic load and hides from the constructor which values can change; liveness per field, since the value a consumer holds is a copy and applying part of a section is the defect live sections remove; the live sections held by the App, with `Stager` kept for a custom configuration, which needs the configuration bound through a wrapper that `Resolve[RawConfig]` would return in place of the caller's value, for an extension only a test implements; and `Get` for the read, a name Credo gives lookups by key.

**Problem.** A subscriber receives a decoded `T` and applies it in its own domain, so every live value needs a holder its subscriber builds — an `atomic.Pointer[T]`, a swapped limiter — and the subscriber may apply only the fields it chooses while its whole section counts as covered, so a key it ignores is neither applied nor reported. Generic hooks run at every reload whatever changed, and the framework's own participant rotates certificates only when something triggers a reload. A constructor's signature does not say whether the value it receives can change. The report of what needs a restart is a log line computed against the previous reload's snapshot, so a second reload that changes nothing stops reporting a key that still needs a restart, and the caller of `App.Reload` — an admin endpoint, an orchestrator's hook — receives only an error and cannot answer the operator with it. The staged candidate, `Stager`, is an extension point that only a test implements.

**Removes.** `app.OnConfigChange[T]`, `app.OnReload` and the framework's reload participants; `config.Reloader`, `config.Stager`, `config.Staged` and `Config.Stage` from the public API; `Config.Reload()` without a context, which becomes `Config.Reload(ctx)` returning its result; `App.Reload(ctx) error`, which returns the reload's result beside its error.

### SIGHUP triggers `Reload` — on Unix, under `Run()` only

`Run()` registers SIGHUP alongside SIGINT/SIGTERM. A SIGHUP calls `app.Reload` with a context bounded by `WithReloadTimeout` (default 30s; also settable through the `server.reload_timeout` config key, mirroring `server.shutdown_timeout`). Signal-triggered reloads run off the signal loop's goroutine, so the loop stays responsive: a SIGINT/SIGTERM that lands during a long reload starts the drain immediately and resets signal delivery at once (a second signal force-kills as usual), rather than waiting for the reload to return. At most one reload runs at a time; signals that arrive while one is in flight are coalesced into at most one follow-up reload. SIGHUP never terminates the process, and a failed reload never terminates the process either: a service that keeps running on its previous configuration is strictly better than one that dies because an operator fat-fingered a YAML file.

`RunContext` and `ServeContext` install no signal handler; their callers use `app.Reload(ctx)` directly (for example from an admin endpoint or an orchestrator hook). On Windows there is no SIGHUP; registration of hooks and `app.Reload` work identically, only the signal path is absent.

**Opting out — `credo.WithoutReloadSignals()`**: disables the reload trigger under `Run` with subscribe-and-ignore semantics — SIGHUP is still captured, but the handler logs an Info line instead of calling `Reload`. Still-subscribed is deliberate and follows the rule that keeps the signal channel subscribed through the drain: under `Run`, SIGHUP must never fall through to its default action and terminate the process non-gracefully (a stray logrotate postrotate or a forgotten `ExecReload` would otherwise kill production). The three signal policies are each one API away: reload (`Run`, default), logged no-op (`Run` + `WithoutReloadSignals`), raw Unix disposition (`RunContext`/`ServeContext`). Rejected alternative: not installing the handler at all — "honest" default disposition, but it makes the middle policy unreachable and turns a stray HUP into an immediate, drain-less death; anyone who wants that semantics already has the signal-free entry points. Programmatic `app.Reload` is unaffected; signal policy is a code-level decision, so there is deliberately no `server.*` config key for it.

### `Reload` is running-only, serialized, and never partially visible

`app.Reload` succeeds only in the `running` state: before it there is nothing to reload, and a stopping App must not start new work against components that are being torn down. Concurrent calls are serialized by a capacity-one slot rather than a mutex, so waiting is context-aware: a queued caller gives up on its own context, or returns the not-running error without reloading when shutdown begins, and the drain can take the slot and keep it. A caller that waits performs its own full reload, which gives the one guarantee callers need: after `Reload` returns, the snapshot is at least as new as when it was called. The snapshot swap is atomic, so readers observe the old or the new snapshot, never a mix. States, errors and their signal-path treatment: [lifecycle spec](../specs/lifecycle.md#appreloadctx-contextcontext-error).

### Reload and shutdown are coordinated; cancellation is cooperative

> **Current until v0.26.0 (R15):** the next paragraph's "every participant, subscriber, and hook" names the hooks that go; with R15 the callbacks a reload in flight runs are the live sections' `OnChange` callbacks, which receive the reload's context, and the drain waits for them before the internal tier as the paragraph states.

Reload callbacks may be using internal components (re-opening a log sink, swapping a client), so a reload in flight when shutdown begins is coordinated with the drain in tiers ([ADR-006](006-application-lifecycle.md), [ADR-024](024-lifecycle-components.md)). Every participant, subscriber, and hook receives a context that ends with either the caller's context or the App's session, and the drain cancels the session first, which tells the reload to stop. After the HTTP drain and the ingress tier, and before the internal tier, the drain waits for the in-flight reload and keeps the slot so no later reload can start. If the shutdown deadline expires first, the reload is logged and reported as still in flight (`credo: shutdown: reload still in flight`) and teardown proceeds, the same contract as an abandoned stop hook. Sequence: [lifecycle spec](../specs/lifecycle.md#shutdown-in-tiers).

Cancellation is cooperative: a Go callback cannot be forcibly stopped, so a hook that ignores its context can still overlap the internal tier's teardown, and the reported error is the framework's honest account of that. Rejected: making the reload wait a hard barrier that is never abandoned — a stuck reload hook would then hold the whole process open past the shutdown budget, which is worse for an operator than a logged overlap. Work with a genuine live-dependency requirement belongs to a component that depends on what it uses: its `Shutdown` stops admitting new reload work and waits for the in-flight callback, and it stops before its dependencies because the drain follows the dependency order — not the dependency's teardown, which would close the resource before the reload is even told to stop.

### Validate before publish, never roll back after

> **Current until v0.26.0 (R15):** the next two paragraphs describe the staged candidate, the `OnConfigChange[T]` subscribers, the reload participants and the `OnReload` hooks; [all or nothing, one generation](#all-or-nothing-for-validation-one-generation-for-reads) replaces them.

A reload stages a candidate snapshot first, decodes every affected `OnConfigChange[T]` subscriber from it, and validates the value when `T` implements `validation.Validatable`. Any failure aborts the whole reload before anything is published: the previous snapshot stays current, no subscriber is invoked, and `Reload` returns the joined decode/validation errors. Typed decode thus doubles as the schema check that the untyped config store lacks, and a bad edit can never half-apply.

After publication, the framework's reload participants (file-based TLS rotation), the affected subscribers in registration order, and the `OnReload` hooks all run; an error or recovered panic is collected, the sequence continues, and `Reload` returns the joined errors. There is no rollback, because earlier subscribers may already have applied their new values and a half-rolled-back state is harder to reason about than a logged partial failure. The step-by-step sequence: [lifecycle spec](../specs/lifecycle.md#appreloadctx-contextcontext-error).

### All or nothing for validation, one generation for reads

> **Pending v0.26.0 (R15):** not implemented yet; the paragraphs above describe the reload's validation and its callbacks. This section's Problem and Removes are [the live sections'](#a-section-is-a-snapshot-unless-it-is-declared-live).

Every live section the reload changes is decoded and validated against the candidate before anything is published, and one failure aborts the reload with every old value current. The snapshot and the changed sections are then published as one generation: once a `Load` returns a reload's value for one section, every later `Load` of that configuration returns that reload's values or newer ones. Two `Load` calls can still straddle a reload, so values that must agree within one operation belong to one section.

The `OnChange` callbacks run in registration order, for the sections the reload changed, with the reload's context, which the caller's deadline or the drain cancels. An error or a recovered panic is joined into the reload's result, the later callbacks still run, and nothing rolls back ([Rejected Alternatives](#rejected-alternatives)). No callback is skipped when the caller's deadline passes, since the next reload's diff would not show its change again. The drain waits for a reload in flight, its callbacks included, before the internal tier.

### Unsubscribed changes are reported, never applied

> **Current until v0.26.0 (R15):** the next paragraph describes the restart-required Warn for keys no subscription covers, computed against the previous snapshot; [the report](#the-report-names-what-was-not-applied) replaces it.

After publishing, every leaf key in `Changes` that no subscription covers is logged at Warn: `config changed but no reloadable consumer is registered; restart required` with the key paths (never the values — they may be secrets). This is the central promise of the design: an operator who changes `server.port` and reloads learns immediately that a restart is needed instead of discovering it at the next incident.

### The report names what was not applied

> **Pending v0.26.0 (R15):** not implemented yet; the paragraph above holds, and `App.Reload` returns only an error. This section's Problem and Removes are [the live sections'](#a-section-is-a-snapshot-unless-it-is-declared-live).

A live section covers every key under it, since its whole value is replaced; every other key whose value differs from the one the start read, a key of a section bound through `Decode` included, is reported as needing a restart. A key whose file value changed while a later source, `.env` or the process environment, keeps it is reported with that source. Key paths are reported, never values, since they may be secrets.

`App.Reload` returns the reload's result, the one `Config.Reload` returns, beside its error, and logs it, so an endpoint behind `ExecReload` or an orchestrator's hook answers the operator with what needs a restart. The keys that need a restart are computed against the snapshot the start read: a reload that changes nothing keeps reporting them, and one that reverts a key to its start value clears it, as PostgreSQL's `pg_settings.pending_restart` does. A reload that a live section's validation aborts returns a result that says nothing was applied and why, beside its error. No accessor reads that state between reloads, and a metric of it waits for the observability release.

### `config.Reloader` and `config.Changes`

> **Current until v0.26.0 (R15):** the next paragraph describes `Stager`, `Reloader` and a custom store's participation, which leave the public API; [the configuration holds its live sections](#the-configuration-holds-its-live-sections) replaces it.

The config store takes part through optional interfaces in the `config` package, so `RawConfig` stays a two-method interface (ADR-005): a two-phase `Stager` (stage a candidate with its `Changes`, commit later) and a one-shot `Reloader`. `*config.Config` implements both. The App prefers `Stager` because only a staged candidate makes validate-before-publish possible; `Reloader` stays for simple custom stores, which are published first and validated afterwards, so a bad value reaches readers before it is reported. A store that implements neither runs only the participants and `OnReload` hooks, and every `OnConfigChange` subscription is a registration-time panic for that app (a subscription that can never fire is a misconfiguration, not a silent no-op). `Changes` records key paths only, never values, so it is safe to log. Interfaces and semantics: [config spec](../specs/config.md#reload--partial-reload-adr-020).

### The configuration holds its live sections

> **Pending v0.26.0 (R15):** not implemented yet; the App orchestrates the reload through the configuration's `Stager` or `Reloader`. This section's Problem and Removes are [the live sections'](#a-section-is-a-snapshot-unless-it-is-declared-live).

`Config.Reload(ctx)` re-reads the sources, validates the changed live sections, publishes, runs the callbacks and returns what changed, the sections it replaced, the keys that need a restart and the keys a later source keeps. `App.Reload` keeps the trigger, the slot and the drain's step, logs that result and returns it. `RawConfig` stays a two-method interface ([ADR-005](005-configuration-architecture.md)): over a configuration that cannot reload — `testutil`'s, or a custom `RawConfig` — `Section.Live` fails at construction and `App.Reload` returns an error, each naming the configuration's type and `config.Load`. `config.Reloader`, `Stager`, `Staged` and `Config.Stage` leave the public API. The triggers stay: `App.Reload(ctx)`, SIGHUP under `Run` bounded by `WithReloadTimeout` or `server.reload_timeout`, and `WithoutReloadSignals`; `RunContext` and `ServeContext` stay signal-free, and configuration files are not watched.

### `OnConfigChange[T]` is a generic method on `*App`

> **Current until v0.26.0 (R15):** this section describes `app.OnConfigChange[T]`, which goes; a live section ([above](#a-section-is-a-snapshot-unless-it-is-declared-live)) replaces it.

`OnConfigChange[T](key, fn)` follows the Go 1.27 concrete-type generic-method convention of `ProvideValue[T]`/`Resolve[T]`/`GetConfig[T]`. It is subject to the same registration guards as `OnStart` (rejected once the App is frozen, that is prepared or shut down). The same key may have several subscribers; nested keys are independent subscriptions (`"databases"` and `"databases.primary"` can both exist; both fire when `databases.primary.dsn` changes).

The subscriber receives the decoded `T` and is responsible for applying it atomically in its own domain (`atomic.Pointer[T]`, `slog.LevelVar.Set`, swapping a rate-limit bucket, re-opening a file). The framework does not mutate DI singletons, re-run constructors, or touch handlers: the DI graph is immutable after `Finalize` (ADR-004), and reload does not change that.

### File-based TLS rotates on reload

> **Current until v0.26.0 (R15):** the next three paragraphs describe the reload participant that rotates file-based certificates and `OnReload` driving a `WithTLSConfig` rotation; [certificate files rotate when they change](#certificate-files-rotate-when-they-change) replaces them.

`WithTLSFiles` and `server.tls.*` serve the key pair through a `GetCertificate` callback backed by an `atomic.Pointer[tls.Certificate]` (one atomic load per handshake; no measurable cost). Preflight loads and validates the initial pair, so a missing or mismatched pair still fails the start.

The framework registers an internal reload participant for this source that runs on **every** reload, not only when `server.tls.*` keys change: certificate files rotate in place without any config-key change, which is invisible to `Changes`. The participant reads the paths (fixed for `WithTLSFiles`; from the new snapshot for `server.tls.*`), calls `tls.LoadX509KeyPair`, and on success swaps the pointer. On failure the previous certificate keeps serving and the error is reported through the normal reload error path. Existing connections are unaffected; new handshakes see the new certificate immediately. `WithHTTPRedirect` is unaffected (it serves plaintext).

`WithTLSConfig` is left entirely alone: the caller owns the `*tls.Config` and its `GetCertificate`, and may use `OnReload` to drive their own rotation.

### Certificate files rotate when they change

> **Pending v0.26.0 (R15):** not implemented yet; file-based certificates rotate on every reload, through the participant above.

With `WithTLSFiles` or `server.tls.*`, `GetCertificate` checks the two files at most once a second, on the handshake that finds the last check older than that, while concurrent handshakes serve the current pair without waiting: `os.Stat` on each file, compared with the last load by `os.SameFile`, modification time and size. A change loads the pair with `tls.LoadX509KeyPair` and swaps it atomically; a pair that fails to load keeps the old one and is logged once per change, with both files and the error, so a half-written pair is retried when its other file changes. `os.Stat` follows symbolic links, so the link a Kubernetes Secret volume swaps on an update is a change, and no signal is needed. Preflight still loads and validates the initial pair. The paths are read at start, so a reload that changes `server.tls.*` reports a restart. `WithTLSConfig` stays the caller's, untouched: its owner rotates through its own `GetCertificate`.

The rejection of polling ([below](#rejected-alternatives)) is reopened for certificate files only, with the evidence that a Kubernetes Secret update sends the process no signal: the check at the handshake adds no dependency and no goroutine, a partial write fails to load and keeps the old pair, and `os.Stat` follows the link swap of a Secret volume. Configuration files stay unwatched.

**Problem.** File-based certificates rotate only when a reload runs, so a certificate renewed on disk keeps the old one serving until an operator or a hook triggers a reload, and under Kubernetes a Secret update sends the process no signal at all.

**Removes.** Certificate rotation on reload and the reload participant behind it; a reload no longer moves the pair to new `server.tls.*` paths, which take a restart.

### A trigger under Kubernetes is the application's

> **Pending v0.26.0 (R15):** not implemented yet; the deployment guide shows `ExecReload` and an admin endpoint whose handler calls `App.Reload`, which returns only an error. This section's Problem and Removes are [the live sections'](#a-section-is-a-snapshot-unless-it-is-declared-live).

A ConfigMap or Secret volume file changes in place and no signal reaches the process, so a live section follows it only if something calls `App.Reload`. Credo provides no endpoint, whose authentication is the application's, and no watcher, which would apply a change at each kubelet's sync rather than when the operator decides, on every replica within that window and with no readiness gate. The [deployment guide](../guides/deployment.md) shows three triggers: a handler behind the application's authentication that calls `App.Reload` and returns its result; a job that calls that handler on each pod in turn; and a sidecar that sends SIGHUP when the volume changes, which `Run` maps to a reload and which needs no endpoint, at the cost of sharing the pod's process namespace (`shareProcessNamespace`).

### Observability

Reload is a lifecycle event, not a request: it writes framework records to the App logger — an Info summary per reload, an Error per failed step, the restart-required Warn ([lifecycle spec](../specs/lifecycle.md#appreloadctx-contextcontext-error)). The observability release ([ADR-014](014-observability.md)) is to add a `credo_reload_total{result}` counter and a reload-duration histogram through the same `Infra` carrier; nothing in this decision depends on them.

## Rejected Alternatives

> **Current until v0.26.0 (R15):** the first item's "opt-in subscription model" becomes the sections declared live; the third item rejects watching for certificates too, which [certificate files rotate when they change](#certificate-files-rotate-when-they-change) reopens for certificate files only; the fifth item, `OnConfigChange` on `RawConfig` or an option, goes with `OnConfigChange`; the sixth item's "subscribers" become the `OnChange` callbacks. The rejections of the live-section design are in [its section](#a-section-is-a-snapshot-unless-it-is-declared-live).

- **Whole-config hot reload (mutate the snapshot in place).** Incompatible with ADR-005: typed structs already injected cannot see the change, so "reload" would apply an undefined subset. The opt-in subscription model makes the applied subset explicit and the unapplied remainder loud.
- **Restart-on-reload (re-exec with socket inheritance, à la nginx binary upgrade).** Correct in principle, but it is a deployment strategy, not a framework hook; containers and systemd already provide zero-downtime restarts. Out of scope.
- **Filesystem watching (fsnotify / polling) for config and certificates.** Adds a dependency and a background goroutine, fires on partial writes, behaves differently on bind mounts and Kubernetes ConfigMap symlink swaps, and removes operator control over _when_ a change applies. Reload stays trigger-driven; an application that wants watching can call `app.Reload` from its own watcher.
- **SIGUSR1/SIGUSR2 for reload.** SIGHUP is the convention every supervisor and operator already knows; USR signals stay free for application use.
- **`OnConfigChange` on `RawConfig` / an `Option`.** A generic method cannot live on an interface, and subscriptions are post-construction registrations like other `On*` hooks (API verb convention).
- **Rollback of subscribers on partial failure.** Requires every subscriber to expose an inverse and still cannot undo side effects (re-opened files, already-served handshakes). Validate-before-publish prevents the common failure (bad input); the remaining failures are logged and returned.
- **Re-deriving `CREDO_ENV` on reload.** Changing environment swaps entire file sets and almost always implies restart-only sections; the environment name is fixed at first load and switching it is a restart.

## Consequences

> **Current until v0.26.0 (R15):** the second item's "remains true for DI-injected values" becomes "remains true for every section not declared live", and the last item's admin endpoint returns the reload's result.

- `systemctl reload` / `kill -HUP` is safe and useful for Credo services: SIGHUP never kills the process under `Run()`.
- ADR-005's "config is a startup snapshot" remains true for DI-injected values.
- The process environment is re-read, but a supervisor's `EnvironmentFile=` is applied only at process start, so env-sourced changes require a restart ([deployment guide](../guides/deployment.md#systemd)).
- `systemctl reload` cannot report reload failure (signal delivery is the only thing systemd observes). Teams that need synchronous failure expose `app.Reload` behind an authenticated admin endpoint and use `ExecReload=curl …` ([deployment guide](../guides/deployment.md#reload-without-a-signal)).
