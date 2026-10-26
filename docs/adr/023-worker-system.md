# ADR-023: Worker System

**Status:** Accepted **Date:** 2026-09-19 **Depends on:** ADR-004, ADR-006, ADR-016, ADR-022, ADR-024 **Related:** ADR-014, ADR-021 **Specification:** [Worker spec](../specs/worker.md) **Guide:** [Worker guide](../guides/worker.md)

## Context

Applications built on Credo run background work next to their HTTP handlers: queue consumers and watchers that live for the whole process, and periodic jobs — cleanups, reports, reconciliation — that run on a schedule. Without framework support each application hand-rolls goroutines, panic recovery, restart loops, cron parsing and shutdown coordination, and gets the interaction with the application lifecycle and DI teardown subtly wrong.

A worker system has to settle a set of contract questions before v1 freezes it:

- **Two opposite contracts behind one method.** A continuous `Run` must block until its context is cancelled, so a nil return is a failure; a scheduled `Run` does one unit of work, so a nil return is the success. Which contract a worker signed must be visible where it is registered, not decided by an option far from the call.
- **Settings that belong to one kind.** Restart settings mean nothing to a scheduled worker, and run timeouts and failure streaks nothing to a continuous one. A kind-specific setting on the wrong kind must not be writable, rather than rejected at run time by a validation matrix that grows with every setting.
- **DI-built workers.** Constructor injection is the framework's headline pattern, `Resolve` is admitted only after `Finalize` (ADR-022), and a worker built by the container must still be registered before `Finalize`.
- **Identity.** Registration needs the name before the instance exists — for duplicate detection, logging and the readiness name — so the name cannot live on the instance.
- **Stop order.** A worker that drains a queue the HTTP handlers fill must stop after the handlers have finished and before the database it writes to; a cron job must not start a run while the App drains.
- **Honest state.** The snapshot must report the configuration the runner executes, not the arguments as passed, and status names must carry their operational meaning: a continuous worker waiting to restart after a failure is unhealthy, a scheduled worker waiting for its next activation is not.
- **Failure semantics.** A continuous worker must not stop silently while the App runs; a restart loop must not flood logs and dependencies while a dependency is down; a run cut short by its timeout must not count as a success; a panic's stack must not leak into readiness output; a cron shorthand must not be rewritten silently; a run must not start with an already cancelled context.
- **Observability.** Lifecycle logging must not depend on the exit path, run lines must correlate with the worker's own lines, and one alerting rule must cover "a worker gave up" whatever the kind.

## Decision

### One package, two kinds, chosen by the registration method

Continuous and scheduled work share one package, one interface, one supervisor and one loop; a schedule is a scheduling strategy, not a second system, which is why no separate `cron` package exists. The kind is chosen by the registration method:

```go
workers := worker.Use(app)

workers.Continuous("order-consumer", consumer)
workers.Scheduled("session-cleanup", "0 */6 * * *", cleanup)
```

The call site states the contract aloud: a reader who sees a finite function passed to `Continuous` has a reason to stop. The schedule is a required positional argument, so a scheduled worker without a schedule cannot be written. Each method takes its own configuration type, so a setting of the other kind is a compile error, and no cross-kind validation exists. A third kind later is one configuration type and its two registration methods, not another column in a matrix. The method names follow Credo's registration vocabulary, which names the thing being registered (`app.GET`, `app.Static`, `app.Host`).

### The name is registration identity

`Worker` has one method, `Run(ctx) error`. The name is an argument of the registration method, the same position Credo takes for routes and stores: a name identifies a registration, not an instance. `Func` is a function type in the `http.HandlerFunc` style. Names are validated strictly and uniformly — non-empty, no surrounding whitespace, no control characters, never normalized — because a name is a log attribute, a future metric label and the suffix of the component name `worker:<name>`, under which readiness and the lifecycle report the worker. No prefix is reserved: no framework-owned worker needs a namespace, and health's `credo.` reservation applies to the full name `worker:<name>`.

### Each worker is a component; the supervisor is a registry

`worker.Use(app)` returns a `*Supervisor`: the registry and reporting object. It keeps the definitions and the live state for `Snapshot` and `Lookup`, enforces unique names and assigns the logger. It has no `Start`, no `Shutdown`, no hooks and no DI binding; `Use` registers nothing, so the supervisor has no registration window of its own.

Each registration adds one component to the App's component registry ([ADR-024](024-lifecycle-components.md)), named `worker:<name>`: its `Start` launches the worker's loop, its `Shutdown` cancels the worker's context and waits for `Run`, and its `Ready` reports the readiness conditions the registration sets ([worker spec](../specs/worker.md#components)). `Shutdown` returns when `Run` has returned — and not before. It does not give up at the deadline itself: a `Run` that ignores cancellation is abandoned by the lifecycle under its rule for every component, reported, and the components the worker depends on are kept open. Returning at the deadline would tell the lifecycle the worker is done and let it close a database under a goroutine that still uses it.

**The tier is declared per registration**, and zero is the kind's default ([worker spec](../specs/worker.md#tiers)). A scheduled worker originates its runs and defaults to ingress: it stops first, concurrently with the HTTP drain, so no run starts during the drain. A continuous worker defaults to internal: it stops after the HTTP drain, in reverse dependency order, so a worker that drains a queue the handlers fill sees every job they enqueued before the database it writes to is shut down. The defaults follow the asymmetry of the two mistakes: an external consumer wrongly left internal merely stops later, while an in-process consumer wrongly placed in ingress loses the work handlers enqueue during the drain. A consumer of an external queue declares `Tier: credo.TierIngress`. The tier belongs to each worker, not to the supervisor: one supervisor holds a queue consumer that must outlive the handlers and a cron job that must not start a run during the drain.

**Edges.** A DI-provided worker is added to the registry as a constructor over `T`, so the start walk builds it after `T`'s component dependencies have started, and the drain stops it before them — the ordering a worker that writes to a database needs, in both directions. An internal provided worker whose `T` depends on an ingress component fails `Finalize` like every internal component (ADR-024). A worker registered by value has no edges the graph can see — a closure's captures are invisible — and takes its place in its tier by registration order; a worker that uses infrastructure is registered in its provided form.

DI access to the supervisor is the application's choice. The framework binds nothing into the application's container: a module that registers workers takes the supervisor as a parameter, and an application that wants it injected into an admin controller binds it with `app.ProvideValue(workers)`.

### DI-provided workers are built in the start walk

`ContinuousProvided[T Worker](name, cfg...)` and `ScheduledProvided[T Worker](name, expr, cfg...)` are generic methods on `*Supervisor`, the form `app.Provide[T]` uses. `T` is resolved when the start walk builds the worker's component, not at registration, so registration order relative to `Provide` is free and `T` may be an interface bound through `Alias`. Each mistake surfaces in the phase that reveals it — a type without `Run` at compile time, a `T` with no binding at `Finalize`, a failed construction at start ([worker spec](../specs/worker.md#provided-workers)). Registering one `T` twice panics, under two names or in two supervisors of one App, because the container would hand both loops one instance.

### Configuration is per kind, in code

Each kind has its own configuration struct, `ContinuousConfig` and `ScheduledConfig` ([worker spec](../specs/worker.md#configuration)), and each registration method takes it variadically, following the framework's config-struct convention: zero arguments for the defaults, one for custom, more than one panics. Zero always means the default; nothing distinguishes an omitted field from an explicit zero. The configuration comes from the registration call and nowhere else: the package owns no configuration section, and an application that wants environment-driven settings unmarshals its own typed section and passes the values — the pattern every other feature follows (`AccessLogConfig`, `HealthConfig`, `websocket.Config`). Resolution has two levels: the given value, then the package default.

### Registration misuse panics

The registration methods panic on misuse, under the framework's three-phase rule (ADR-022): registration performs no I/O and panics on misuse known at the call site, a registration after `Finalize` or after the App is prepared or shut down included ([worker spec](../specs/worker.md#validation)); `Finalize` returns what only the whole graph reveals, such as a provided worker's missing `T`; the start phase returns I/O errors, among them a provided worker's failed construction. No `Must*` twins exist. `ParseSchedule(expr) (*Schedule, error)` serves applications that validate a configuration-supplied expression and want an error.

### The snapshot carries the effective configuration

`Info` is identity, the resolved configuration of the worker's kind and flat live state ([worker spec](../specs/worker.md#info)). The configuration is the policy the runner executes after defaults are applied, not the arguments as passed, so registration is testable without running anything; its type is the type the registration took, so the input and the report never drift apart. The live-state fields stay flat, and the status and counters are always present, so dashboards keep one schema. The configuration types hold only scalars, so every snapshot's copy of the configuration is a deep copy, and a future field that is not a scalar is excluded from the echo. One builder produces every `Info`. Because applications serve `Snapshot()` from admin endpoints, the JSON shape is a wire contract ([worker spec](../specs/worker.md#json-shape)), with durations as integer nanoseconds under the response profile of ADR-021.

The statuses carry their operational meaning ([worker spec](../specs/worker.md#status)): `backoff` is a continuous worker waiting to restart after a failure — unhealthy, recovering — while `waiting` is a scheduled worker waiting for its next activation, whatever its last run did, so a scheduled worker's "unhealthy but recovering" signal is `consecutive_failures > 0`. `LastStartedAt` is when the most recent run was admitted and `LastSucceededAt` when the last successful run completed; the names say which end of a run each records.

### Run outcome and loop exit are separate

After every run the loop first classifies the outcome, records it, and only then decides whether the loop ends. The classification order is fixed ([worker spec](../specs/worker.md#run-outcome)): a panic and a run cut short by its timeout are failures before anything can count as a graceful stop, and a context error joined with another error is a failure, since `errors.Join(ctx.Err(), flushErr)` must not hide a final write that failed. A timeout followed by a shutdown is therefore a timed-out run and a shutdown exit — neither erases the other — and a graceful stop records only the status, so `LastError` keeps one meaning: the most recent failed run.

### Continuous workers are permanent

A continuous worker's `Run` must remain active until its context is cancelled. A nil return while the context is alive is a failure with an actionable message and follows the restart policy. A restart calls `Run` again on the same value, so a continuous `Run` must be re-enterable — it builds its per-run resources inside `Run` — or be registered with `Restart{Disabled: true}`. Finite background work belongs in a component's `Start` or an `OnStart` hook when startup should wait for it, or in a continuous worker that waits for `<-ctx.Done()` after the work. This is the only restart strategy offered.

`Restart{Disabled, Limit, MinDelay, MaxDelay}` states the policy. `Limit` N, N > 0, allows the first run plus at most N restarts; zero is unlimited. `Disabled` makes the first failed run terminal: no in-process retry, and nothing more — the worker remains permanent, an early nil return is still a failure, and it ends in `failed`. It fills a gap a count cannot express, since `Limit` keeps zero for "unlimited", and with it the first failure is the last, so `LastError` holds the real cause rather than the error of a forced restart of a `Run` that is not re-enterable. `Disabled` beside any other field is a contradiction and panics.

What follows a terminal failure is the application's. The framework reports the fact — status `failed`, the `worker failed` line, and an unready instance under `UnreadyWhenFailed` — and no component ends the App (ADR-024). Readiness only takes the instance out of rotation: a `Disabled` worker bound to readiness alone leaves a process that is alive, unready and never restarted. An application that wants a restart ties a liveness check to the worker through `Lookup`, knowing that a failure caused by a shared dependency restarts every replica; the [worker guide](../guides/worker.md#escalating-a-terminal-failure) carries the recipe.

The cost of permanence is accepted and stated: a worker that returns nil on purpose keeps compiling and becomes a restart loop. It is loud at run time — an Error line naming the contract on the first occurrence.

### Timeouts are cooperative and cannot be laundered

`ScheduledConfig.RunTimeout` bounds every run of a scheduled worker by deriving the run context with `context.WithTimeoutCause(…, ErrRunTimeout)`. Classification reads the cause, not the return value: a run cut short by its budget is a failure even when it returns nil, because Credo's own idiom `case <-ctx.Done(): return nil` would otherwise turn half-done work into a success that stamps `LastSucceededAt`, resets the failure streak and opens a readiness barrier. The accepted cost is conservative: a run that completes just after its deadline counts as failed. The timeout never abandons the goroutine, so at most one run per worker is ever active. It is scheduled-only, since a continuous `Run` lives for the process.

### Panics are recorded without their stack

A recovered panic becomes an unexported error `worker: run panicked: <value>` with the stack kept alongside and logged as a separate attribute. It has no `Unwrap`, and the panic row is evaluated before the graceful row, so a panic whose value is a context error during shutdown can never pass as a graceful stop. `LastError`, and anything readiness exposes, never contains a stack trace. The type is unexported because a worker's error never returns to user code.

### Counters mean one thing

`Restarts` counts restarts that actually started: it advances in the run-admission commit, not when the previous run failed, so a shutdown during the restart wait does not count one. `ConsecutiveFailures` counts a scheduled worker's failed runs since its last success. No run-context accessor reports an attempt number: the next cron activation is not a retry of the previous one, and omitting an accessor is the reversible choice.

### Restart backoff

Permanence makes the restart delay load-bearing. Restarts are unlimited by default and every failed run writes one Error line, so with a fixed 3 s delay a continuous worker whose dependency stays unreachable, and whose `Run` therefore fails immediately every time, restarts about 28,800 times a day, writes at least as many Error lines and reconnects to the dependency at the same rate. Backoff changes how long the runner waits between restarts, not whether it restarts: the permanent-restart contract stands.

After a failed run for which a restart is planned, the delay is drawn from a window that starts at the floor, `Restart.MinDelay`, doubles as failures repeat and is capped at `Restart.MaxDelay`; the worker spec gives the [formula](../specs/worker.md#restart-backoff) and the [defaults](../specs/worker.md#configuration).

The jitter keeps the floor. Full jitter — a uniform pick in `[0, ceiling]` — spreads load best and is the right choice for `httpclient` retries, but it can pick a near-zero wait and reintroduce the tight loop the restart delay exists to prevent. With the floor, the first delay is exactly `MinDelay`, no delay is shorter than `MinDelay` or longer than `MaxDelay`, and equal values are a fixed delay without a separate mode. Individual delays need not grow monotonically; their window does.

A run that lasted at least the cap resets the sequence: its failure counts as the first, so the next restart waits the floor. Without a reset, a worker that runs for hours and fails once a day would wait at the cap for ever. Runtime is a heuristic, not proof of health, so a long run resets only the backoff — never `Restarts` or the `Limit` budget.

The cap resolves against the resolved floor, asymmetrically: a zero `MaxDelay` resolves to the default cap or the floor, whichever is larger, while a positive `MaxDelay` below the resolved `MinDelay` panics at registration, even when that floor is the default ([worker spec](../specs/worker.md#configuration)). Raising an omitted ceiling to fit an explicit floor is the safe direction (longer waits); lowering an omitted floor to fit an explicit ceiling would tighten the retry loop, so the application writes that floor itself.

Backoff is the default rather than an option, because the flood appears precisely in applications that registered a continuous worker without restart settings. With the default floor (3 s) and cap (one minute), the capped window is 30–60 s, about 45 s on average: roughly 1,920 runs a day once the cap is reached (2,880 at the floor), fifteen times fewer than with a fixed 3 s delay. A 5-minute cap would give about 384. The cap also bounds the wait before the next recovery attempt, so a larger default would delay recovery for every application after its dependency returns; an application that expects long outages raises `MaxDelay`. Log volume shrinks; failures are neither hidden nor downgraded.

The snapshot and the log make the policy visible. The snapshot echoes the resolved `Restart`. The `worker run failed` line of a continuous worker carries `next_restart_in`, the selected delay, only when a restart is planned, so the restart decision is made before the line is written ([worker spec](../specs/worker.md#logging-contract)).

### Run admission is one ordered step

The activation time is computed without side effects, the worker's context is checked, one commit records the run, and `Run` is called ([worker spec](../specs/worker.md#run-admission)). The commit is the only writer of `StatusRunning`. If the check observes cancellation, no new run starts and nothing is recorded. The order is what lets an in-package test policy prove the property without a production test seam.

### Scheduling is serial and skip-only

Each scheduled worker is one goroutine that sleeps until the next activation, runs it synchronously and recomputes. Overlap is impossible by construction; activations that pass during a run are skipped, never queued, and reported in one line per resumption. The next activation is computed from the last intended one, so `@every` grids do not drift by run duration. `@every` accepts only positive whole seconds and rejects every input it would otherwise have to rewrite, so the registered expression is the effective schedule. The scheduler is per process: every replica runs every schedule, so a job is idempotent or guarded by an application-level lock (for SQL, an advisory lock or a claim row).

### Readiness is opt-in per condition

Each configuration carries only the readiness conditions its kind can meet, named as the sentence they implement: `UnreadyWhenFailed` for both kinds, `UnreadyUntilFirstSuccess` and `UnreadyAfterSuccessAge` for scheduled workers (a continuous worker never succeeds). A worker with no condition does not contribute to readiness, because not every failed worker should take an instance out of rotation. The conditions are reported by the worker component's `Ready` under the name `worker:<name>`, evaluated in memory from the runner's last state without I/O and without resolving anything per request ([worker spec](../specs/worker.md#health-integration)).

### One run-context value

`worker.CurrentRun(ctx) (RunInfo, bool)` returns the worker's name, the run's identifier and the intended activation time. One context key carries them, the boolean says whether the context is a run context at all, and a field can be added without adding a function. The run context carries execution metadata only: services, loggers and configuration come from constructors.

### Logging has one vocabulary and a fixed shape

Every started worker writes exactly one `worker started` and one `worker stopped` line, both owned by the loop so no exit path can miss them. One message names each event whatever the kind, and run lines carry the `run_id` that `CurrentRun` returns inside the run. A successful scheduled run is logged at Debug, since liveness is answered by the snapshot and readiness, not by an Info line per run. A single terminal line, `worker failed`, with a `reason`, gives one alerting rule for "a worker gave up" whatever the kind. The stop line stays at Info even for a failed worker: the preceding Error line is the alerting signal. The [worker spec](../specs/worker.md#logging-contract) lists every message and attribute.

### State

The shutdown deadline is the application's; there is no worker-specific shutdown timeout, which would only create ambiguity about which deadline wins. `StatusFailed` is permanent until the application restarts; there is no paused state and no per-worker cancellation. Runner state is guarded by a mutex rather than an `atomic.Value`, whose stores must share one concrete type. The supervisor logs through `App.Logger()` with `module=worker` rather than through a logger registered in DI, which would let services bypass the per-service logger of `credo.Infra`. Timing tests use `testing/synctest` instead of a clock abstraction that production code would carry only for tests.

## Rejected Alternatives

- **Registering workers after `Finalize`, or a builder/App type split** that would make a late registration a compile error — both rejected in [ADR-022](022-bootstrap-and-di-ownership.md#rejected-alternatives). A worker set that depends on data is one worker whose `Run` reads the data and fans out, or a component whose `Start` does that work; a DI-built worker is the provided form, registered before `Finalize` and built in the start walk.
- **One interface with the kind chosen by an option** (`WithSchedule`). Rejected: the contract of a nil return would be decided far from the call that registers the worker, and every kind-specific option becomes a cell of a runtime validation matrix.
- **Two interfaces** (`Run` for continuous work, `Do` for scheduled work). Rejected: the registration method already names the contract, and closures — the common form — would need two adapters.
- **Functional options with presence flags**, distinguishing an omitted setting from an explicit zero. Rejected: configuration structs are the framework's convention, and an omitted-versus-zero distinction exists only to give meaning to a three-level resolution no application needs.
- **A `worker` configuration section** read by the package. Rejected: it would be the only section a feature package owns beside the root's `server`, it moves the source of a worker's delays away from the call that states everything else about the worker, and typed application configuration passed in code covers the need.
- **A placeholder value for provided workers** (`workers.Continuous(name, worker.Provided[T]())`). Rejected: it halves the method count, but it puts into the `Worker` type a value that cannot do what the interface promises; the supervisor would have to recognize it through an unexported interface, and the recognition is lost the moment an application wraps it, so a valid-looking `Worker` fails at run time — for a continuous worker, in a restart loop. Four literal methods add no rule to learn.
- **`worker.Provide[T](app, constructor, opts...)`**, combining provider and registration. Rejected: it duplicates `app.Provide`, hides the provider from ordinary DI reading, and still needs the name outside the interface.
- **A `Name()` method on the worker as its identity**, with a name parameter only for the provided form, checked at start. Rejected: two sources of truth for one identity.
- **A provided worker as a value that resolves `T` in its `Start`.** Rejected: a value has no edges, so the order a worker that writes to a database needs would become a registration-order convention.
- **The supervisor as a component, or attached through its own start and drain hooks and a DI binding.** Rejected: one stop moment for every worker cannot serve both a queue consumer that must outlive the handlers and a cron job that must not start a run during the drain, and several mechanisms for one resource reach its shutdown more than once. Each worker is a component instead.
- **A tier for the whole supervisor.** Rejected for the same reason: the tier belongs to each registration.
- **A root `RegistrationOpen()` predicate, or registration guards of the supervisor's own.** Rejected: the window is the component registry's, bootstrap is sequential by contract (ADR-022), and a late registration panics where the registry refuses it.
- **A "one supervisor per App" slot in the kernel.** Rejected: each `Use` is an independent registry, and the one duplicate that matters — a worker name — is caught by the supervisor within one registry and by the component registry's unique names across registries. A process-wide guard is rejected because one test process runs many Apps.
- **The verbs `Supervise` and `Schedule` as method names.** Rejected: `Schedule` would name both a method and the parsed-cron type, and both kinds are supervised.
- **`Pool` as the supervisor's name.** Rejected: a worker pool is N interchangeable goroutines draining one queue; this type supervises named, distinct, long-lived workers.
- **Echo the configuration exactly as given in the snapshot.** Rejected in favor of the effective policy: an echoed zero delay would misreport what the runner does.
- **Flat state and configuration on `Info`, or one union configuration for both kinds.** Rejected: the first mixes configuration with state; the second leaves half its fields "zero because not applicable" for any worker.
- **Repurposing a zero restart limit as "no restart".** Rejected: `Limit` keeps zero for "unlimited", and "no restart" is a separate word, `Disabled`.
- **A first-class liveness twin of `UnreadyWhenFailed`.** Not offered: a liveness probe should report only a failure a restart can fix, and a worker usually fails terminally because a dependency is down, so a restart repairs nothing and every replica restarts in a loop. Tying liveness to a worker is a decision the application writes down.
- **A `worker.ErrGracefulStop` sentinel.** Rejected: a nil return once the context is done is already the graceful stop; a sentinel honored only while stopping is a second spelling of it, and honored while alive it would restore the silent stop permanence removes. A client that reports a cancelled call as its own error is translated at that call, while the context is done, and the worker guide shows how without masking a real flush error.
- **Treat a nil return after the deadline as a success.** Rejected for the laundering reason above.
- **An exported panic error that unwraps its value.** Rejected: it would let a context-error panic during shutdown pass as a graceful stop, and no caller would receive the error.
- **An attempt number unified as `consecutiveFailures + 1` for scheduled workers.** Rejected: activations are not retries of each other.
- **Transient or temporary restart strategies, or options such as "restart on success".** Not offered; one permanent strategy covers continuous work, finite work has documented homes, and `Restart.Disabled` removes retries without making a worker finite.
- **A fixed restart delay.** Rejected: at 3 s, a worker whose dependency stays unreachable restarts, and writes an Error line, about 28,800 times a day ([Restart backoff](#restart-backoff)).
- **Opt-in restart backoff.** Rejected: the failure it prevents occurs in applications that rely on the defaults.
- **Full jitter for restart delays.** Rejected: it can pick a near-zero wait, and the restart delay is a minimum-wait guarantee. It remains the right choice for `httpclient` retries.
- **A circuit breaker or an elapsed-time budget ("give up after T").** Not offered: neither is the same as `Restart.Limit`, but the restart-count budget covers every known need, and nothing demonstrates a need for a second limit.
- **Per-error-class restart delays** (retry a timeout quickly, an authentication failure slowly). Not offered: the worker's `Run` knows which failures deserve a quick retry and can retry them inside the run.
- **Backoff for scheduled workers.** Not offered: a schedule is already a cadence, and skipping activations after failures would be a different feature (pause on failure).
- **Pluggable backoff strategies, or a configurable multiplier, jitter or reset threshold.** Not offered: one built-in policy with two settings, floor and cap, per worker.
- **The backoff streak or the current delay in `Info`.** Deferred until a consumer needs them; the planned delay is on the failure line.
- **Time zone selection for cron schedules** — a per-worker zone, or `TZ=`/`CRON_TZ=` prefixes. Not offered: schedules use the process's local time zone by design, and selection can be added when a concrete consumer needs it.
- **Reserve the `credo.` prefix for worker names.** Rejected: nothing needs it now, and restricting names later must account for the names the contract already allows.
- **Overlap policies "allow" and "queue".** Deferred: allowing overlap needs per-execution state (several running flags and last errors, completion-order failure counting) that breaks the one-runner model; queueing adds buffering and staleness rules.
- **A scheduler goroutine feeding an executor goroutine per worker.** Rejected: the serial loop has the same observable semantics without the coordination channels.
- **Preemptive timeouts or a supervisor-level default budget.** Rejected: abandoning a goroutine breaks the one-run-at-a-time guarantee, and budgets differ per job.
- **A worker test package.** Not offered: a `Run`-only worker is tested by calling `Run`.
- **Supervisor trees and in-process leader election.** Rejected: lifecycle components are start-once (ADR-024), and restart stays a worker concern.

Deferred, each until something needs it:

- **A worker kind that runs once at start** — background work at start whose success is terminal `stopped` and whose failure follows a restart policy, for a warm-up that must not block startup but should gate readiness. The kind-per-method registration makes adding it non-breaking.
- **A policy by which a terminal worker failure stops the App** — the process exits non-zero and whatever supervises it restarts it, the one escalation that works outside Kubernetes. It needs a lifecycle decision of its own: no component ends the App (ADR-024), and exit semantics, the interaction with the drain and the error `Run` returns belong to that decision, not to the worker system. Until then the liveness recipe is the documented route.
- **A lock hook for scheduled runs** (`ScheduledConfig.Lock`) that `store/sqldb` could implement; the documented workaround is an application-level lock. A hook field is excluded from the snapshot's echo.
- **A per-run logger** (`worker.Logger(ctx)`) pre-tagged with `worker` and `run_id`. It raises which base logger it derives from, and belongs to the observability release ([ADR-014](014-observability.md)).

## Consequences

Constructor-injected workers are the default path, ordered against the components they use in both directions, and registration is testable: a test reads `Snapshot()` and asserts the effective policy of every worker without running it. A continuous worker cannot disappear silently, a timeout cannot report a success, and a panic cannot pass as a shutdown. An in-process consumer sees every job the handlers enqueued before the drain, and its database closes after it. Operators get one start and one stop line per worker, correlated run lines, one terminal line to alert on and a stable JSON snapshot.

Metrics and tracing hooks are left to the observability release ([ADR-014](014-observability.md)); they will reuse the `run_id` and `duration` attributes.

Restart backoff spaces restarts without changing whether they happen: a worker with `Restart.Limit` N reaches `failed` — and an `UnreadyWhenFailed` readiness check drops — later than a fixed 3 s delay would make it: with the defaults, `Limit: 5` waits roughly 48–93 s in total instead of 15 s. Equal `MinDelay` and `MaxDelay` give a fixed delay.
