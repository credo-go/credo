# ADR-023: Worker System

**Status:** Accepted **Date:** 2026-09-19 **Depends on:** ADR-004, ADR-006, ADR-016, ADR-022, ADR-024 **Related:** ADR-002, ADR-014, ADR-021 **Specification:** [Worker spec](../specs/worker.md) **Guide:** [Worker guide](../guides/worker.md)

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

> **Current until v0.25.0 (W4):** the next paragraph and its example place the workers in the `worker` package, registered through the `*Supervisor` that `worker.Use(app)` returns.

Continuous and scheduled work share one package, one interface, one supervisor and one loop; a schedule is a scheduling strategy, not a second system, which is why no separate `cron` package exists. The kind is chosen by the registration method:

```go
workers := worker.Use(app)

workers.Continuous("order-consumer", consumer)
workers.Scheduled("session-cleanup", "0 */6 * * *", cleanup)
```

> **Pending v0.25.0 (W4):** the next paragraph and its example state the registration in the root package; until W4 ships, the workers are registered as the paragraph above shows.

Continuous and scheduled work share one registry, one interface and one loop; a schedule is a scheduling strategy, not a second system, which is why no separate `cron` package exists. The kind is chosen by the registration method:

```go
app.Workers().Continuous("order-consumer", consumer)
app.Workers().Scheduled("session-cleanup", "0 */6 * * *", cleanup)
```

The call site states the contract aloud: a reader who sees a finite function passed to `Continuous` has a reason to stop. The schedule is a required positional argument, so a scheduled worker without a schedule cannot be written. Each method takes its own configuration type, so a setting of the other kind is a compile error, and no cross-kind validation exists. A third kind later is one configuration type and its two registration methods, not another column in a matrix. The method names follow Credo's registration vocabulary, which names the thing being registered (`app.GET`, `app.Static`, `app.Host`).

### Workers are a subsystem of the App, in the root package

> **Pending v0.25.0 (W4):** not implemented yet; the `worker` package holds the registry, its types and the engine, and `internal/kernel` adds each worker's component to the App.

`app.Workers()` returns the App's one registry, a `*credo.Workers` held in a field of the App, created on first use and the same value on every call. The placement follows the rule [ADR-002](002-code-acquisition-strategy.md#what-lives-in-the-root-package) records: a subsystem the App holds one of and reaches through `app.` lives in the root package. The root's module allowlist, recorded beside that rule, admits it, since the worker system links no third-party module: its cron parser is adapted from robfig/cron, not imported.

The public types — `Worker`, `WorkerInfo`, `WorkerKind`, `WorkerStatus`, `RestartPolicy`, `WorkerRun`, `ErrWorkerRunTimeout`, `ContinuousConfig`, `ScheduledConfig` and `Schedule` — are defined in the root package, not aliased from an internal one, so `go doc` and pkg.go.dev show them where users look; the root namespace forces the `Worker` prefixes ([worker spec](../specs/worker.md#workers-in-the-root-package)). The engine — the runner, the restart backoff, the schedule loop and the cron parser — lives in `internal/worker`, which does not import the root package; the root converts at the boundary. The registry calls `internal/di` directly, with no seam between them, so a registration the container refuses panics with the container's reason under the worker's own call.

One registry per App makes the report whole: `Snapshot` and `Lookup` cover every worker of the App, and a duplicate name panics at the second registration whoever made the first. The worker's records name it with the key every framework record uses for an application part ([ADR-004](004-dependency-injection-and-infra.md)): `component=worker:<name>`, its component's name, the key and value of the lifecycle's records about it, beside `module=worker`.

### The name is registration identity

> **Current until v0.25.0 (W5):** `worker.Func` is a function type in the `http.HandlerFunc` style that adapts a `func(context.Context) error` to `Worker`.

`Worker` has one method, `Run(ctx) error`. The name is an argument of the registration method, the same position Credo takes for routes and health checks: a name identifies a registration, not an instance. Names are validated strictly and uniformly — non-empty, no surrounding whitespace, no control characters, never normalized — because a name is a log attribute, a future metric label and the suffix of the component name `worker:<name>`, under which readiness and the lifecycle report the worker. No prefix is reserved: no framework-owned worker needs a namespace, and health's `credo.` reservation applies to the full name `worker:<name>`.

> **Pending v0.25.0 (W5):** until W5 ships, a function is registered through `worker.Func`.

A function needs no adapter: a `func(context.Context) error` is a worker as it is, and the registry makes the conversion itself ([Three forms](#three-forms-a-worker-a-run-function-a-constructor)).

### Each worker is a component; the supervisor is a registry

> **Current until v0.25.0 (W4):** the next paragraph describes the `*Supervisor` of the `worker` package.

`worker.Use(app)` returns a `*Supervisor`: the registry and reporting object. It keeps the definitions and the live state for `Snapshot` and `Lookup`, enforces unique names and assigns the logger. It has no `Start`, no `Shutdown`, no hooks and no DI binding; `Use` registers nothing, so the supervisor has no registration window of its own.

> **Pending v0.25.0 (W4):** the next paragraph describes the registry `app.Workers()` returns; until W4 ships, the paragraph above holds.

`app.Workers()` returns the registry and reporting object. It keeps the definitions and the live state for `Snapshot` and `Lookup`, enforces unique names across the App and assigns the logger. It has no `Start`, no `Shutdown`, no hooks and no DI binding, and creating it registers nothing, so an App that calls it and registers no worker has no start work, and the registry has no registration window of its own.

Each registration adds one component to the App's component registry ([ADR-024](024-lifecycle-components.md)), named `worker:<name>`: its `Start` launches the worker's loop, its `Shutdown` cancels the worker's context and waits for `Run`, and its `Ready` reports the readiness conditions the registration sets ([worker spec](../specs/worker.md#components)). `Shutdown` returns when `Run` has returned — and not before. It does not give up at the deadline itself: a `Run` that ignores cancellation is abandoned by the lifecycle under its rule for every component, reported, and the components the worker depends on are kept open. Returning at the deadline would tell the lifecycle the worker is done and let it close a database under a goroutine that still uses it.

**The tier is declared per registration**, and zero is the kind's default ([worker spec](../specs/worker.md#tiers)). A scheduled worker originates its runs and defaults to ingress: it stops first, concurrently with the HTTP drain, so no run starts during the drain. A continuous worker defaults to internal: it stops after the HTTP drain, in reverse dependency order, so a worker that drains a queue the handlers fill sees every job they enqueued before the database it writes to is shut down. The defaults follow the asymmetry of the two mistakes: an external consumer wrongly left internal merely stops later, while an in-process consumer wrongly placed in ingress loses the work handlers enqueue during the drain. A consumer of an external queue declares `Tier: credo.TierIngress`. The tier belongs to each worker, not to the supervisor: one supervisor holds a queue consumer that must outlive the handlers and a cron job that must not start a run during the drain.

> **Current until v0.25.0 (W5):** the next paragraph states the edges of the provided form.

**Edges.** A DI-provided worker is added to the registry as a constructor over `T`, so the start walk builds it after `T`'s component dependencies have started, and the drain stops it before them — the ordering a worker that writes to a database needs, in both directions. An internal provided worker whose `T` depends on an ingress component fails `Finalize` like every internal component (ADR-024). A worker registered by value has no edges the graph can see — a closure's captures are invisible — and takes its place in its tier by registration order; a worker that uses infrastructure is registered in its provided form.

> **Pending v0.25.0 (W5):** the next paragraph states the edges of the run function and the constructor; until W5 ships, the paragraph above holds.

**Edges.** The parameters of a run function and of a constructor are the worker's edges: the start walk resolves or builds them after their components have started, and the drain stops the worker before them — the ordering a worker that writes to a database needs, in both directions. An internal worker whose parameters depend on an ingress component fails `Finalize` like every internal component (ADR-024). A worker registered by value has no edges the graph can see — a closure's captures are invisible — and takes its place in its tier by registration order; a worker that uses infrastructure is registered as a run function or a constructor.

> **Current until v0.25.0 (W4):** the next paragraph names the supervisor as what a module takes.

DI access to the supervisor is the application's choice. The framework binds nothing into the application's container: a module that registers workers takes the supervisor as a parameter, and an application that wants it injected into an admin controller binds it with `app.ProvideValue(workers)`.

> **Pending v0.25.0 (W4):** the next paragraph replaces the paragraph above when W4 ships.

DI access to the registry is the application's choice. The framework binds nothing into the application's container: a module that registers workers takes the `*credo.App` it already receives, and an application that wants the registry injected into an admin controller binds it with `app.ProvideValue(app.Workers())`.

### DI-provided workers are built in the start walk

> **Current until v0.25.0 (W5):** this section describes the provided form, which the [three forms](#three-forms-a-worker-a-run-function-a-constructor) replace.

`ContinuousProvided[T Worker](name, cfg...)` and `ScheduledProvided[T Worker](name, expr, cfg...)` are generic methods on `*Supervisor`, the form `app.Resolve[T]` uses. `T` is resolved when the start walk builds the worker's component, not at registration, so registration order relative to `Provide` is free and `T` may be an interface bound through `Alias`. Each mistake surfaces in the phase that reveals it — a type without `Run` at compile time, a `T` with no binding at `Finalize`, a failed construction at start ([worker spec](../specs/worker.md#provided-workers)). Registering one `T` twice panics, under two names or in two supervisors of one App, because the container would hand both loops one instance.

### Three forms: a worker, a run function, a constructor

> **Pending v0.25.0 (W5):** not implemented yet; the registration methods take a `Worker`, and a DI-built worker is registered through `ContinuousProvided[T]` or `ScheduledProvided[T]`.

`Continuous(name, w, cfg...)` and `Scheduled(name, expr, w, cfg...)` take `w any`, and the registration decides its form when it is called ([worker spec](../specs/worker.md#registration-forms)):

- **A worker** is a value whose type implements `Worker`, or a `func(context.Context) error`, named function types of that signature included.
- **A run function** takes exactly one `context.Context`, in any position, and returns only an `error`; its other parameters come from the container. `func(context.Context) error` is its case without them. The start walk resolves the parameters once, after their components have started, and every run calls the function with that run's context and those values. A scheduled job over a database needs no type of its own: `func(ctx context.Context, db *sqldb.DB) error`.
- **A constructor** is any other function, `func(deps...) W` or `func(deps...) (W, error)`, whose `W` is a worker in either form. The start walk builds it after its dependencies have started, as it builds a constructor handed to `Manage`, and the drain stops the worker before them.

The forms cannot be mistaken for one another: `Finalize` refuses a constructor that takes a `context.Context`, and a constructor returns `W` or `(W, error)`, never a lone `error`. Anything else panics at registration, naming the forms accepted, and so do a function that takes two contexts and one that takes a context and returns anything but a lone `error`. Each mistake surfaces in the phase that reveals it: a shape that is none of the forms at registration, a missing parameter at `Finalize` with its path, a constructor's error, panic or nil worker at start. A run function is called through `reflect` on each run and a constructor once, a cold path, as for every constructor.

A method expression is a run function whose first parameter is the receiver: `(*OutboxRelay).Run` is a `func(*OutboxRelay, context.Context) error`, and it is the form of a worker that other code uses ([below](#a-worker-that-other-code-uses-is-bound)). A constructor that returns a method value such as `relay.Run` hands over the function alone, which has no teardown; a worker with a teardown of its own is returned as itself.

**A constructor never becomes a binding.** Nothing resolves the value it builds. Each registration calls its constructor separately, so two registrations of one constructor get two values and do not collide; two loops share a value only when the constructor returns one it did not create, and the registry then tears that value down once. A constructor registration adds two units that are not bindings — the built value, `worker-value:<name>`, and the loop, `worker:<name>` — with an edge from the loop to the value. Worker names admit any printable character, so a suffix could collide with another worker's name; a distinct prefix cannot. A run function builds no value: it adds the loop alone, whose edges are its parameters, and nothing that holds or defers.

### The value a constructor builds claims nothing

> **Pending v0.25.0 (W5):** not implemented yet; a provided worker's `T` is a binding, torn down as its own component after the worker.

The built value is a holder like any constructor result, under the rules a `Provide` result follows ([ADR-024](024-lifecycle-components.md)). `Start` and `Ready` are planned from the constructor's result type: a type that shows `Shutdown` is started before its loop if it also shows `Start`, and asked for readiness if it shows `Ready`, while a `Start` or `Ready` only the built value has fails the start. Its teardown follows the built value, a `Shutdown` the result type hides included, and runs after its loop has stopped, through the component registry, never by a direct call from the worker, so the identity, kind and owner rules apply to it.

It claims no ownership. A binding or a value handed to `Manage` that holds a resource is that resource's holder of record; a value a constructor builds that holds it too defers to it: it adds no readiness entry of its own and never runs the teardown, which carries the registration's name and tier even when the worker was registered first. Without such a registration the first registered holder is the holder of record, so two worker constructors that return one value they did not create give it one entry and one teardown. A worker that embeds a bound `*sqldb.DB` therefore shares the database's one teardown, after the consumers of all its holders, and a borrowed database is torn down by none of them.

The built value is internal, as a constructor result bound without `credo.Ingress()` is, and the loop keeps its worker's tier. A shared resource takes the tier of its holder of record, so a value that took a scheduled worker's tier would move that resource's one teardown beside the HTTP drain, under handlers that still use it. The loop's tier decides when the worker stops taking work; the value is then torn down in the internal tier, after the HTTP drain. A built value with a teardown of its own that depends on an ingress component fails `Finalize`, or fails its build when the teardown is found only on the value, with the path and the remedy of splitting the worker; when that ingress component is the binding whose type the constructor returns, there is no part to split, and the remedy is to bind it without `credo.Ingress()`.

### A worker that other code uses is bound

> **Pending v0.25.0 (W5):** not implemented yet; until W5 ships, such a worker is bound with `Provide` and registered through its provided form.

The value a constructor builds is not a binding, so nothing else reaches it. A worker that handlers also use — an outbox relay whose `Enqueue` they call — is bound with `Provide` and registered through the method expression of its `Run`:

```go
app.Provide(NewOutboxRelay)
app.Workers().Continuous("outbox-relay", (*OutboxRelay).Run)
```

Handlers and the loop share one singleton, which starts once, before the loop, and is torn down once, after it, and `testutil.WithOverride` replaces it for both. The registration builds no value, so the binding is the singleton's only holder: `/ready` shows its one entry, with its options, and the teardown carries its name, whichever of the two calls comes first. The compiler checks that `Run` exists, and the registration checks its shape. The binding takes no `credo.Ingress()`, so its teardown runs after the HTTP drain, which the handlers that use it need; the loop's tier comes from the worker's configuration. A constructor that returns the binding, `func(r *OutboxRelay) *OutboxRelay { return r }`, still registers and defers to the binding, its holder of record.

### Configuration is per kind, in code

Each kind has its own configuration struct, `ContinuousConfig` and `ScheduledConfig` ([worker spec](../specs/worker.md#configuration)), and each registration method takes it variadically, following the framework's config-struct convention: zero arguments for the defaults, one for custom, more than one panics. Zero always means the default; nothing distinguishes an omitted field from an explicit zero. The configuration comes from the registration call and nowhere else: the package owns no configuration section, and an application that wants environment-driven settings unmarshals its own typed section and passes the values — the pattern every other feature follows (`AccessLogConfig`, `HealthConfig`, `websocket.Config`). Resolution has two levels: the given value, then the package default.

### Registration misuse panics

> **Current until v0.25.0 (W5):** the next paragraph names the provided worker's `T` as what `Finalize` and the start phase report.

The registration methods panic on misuse, under the framework's three-phase rule (ADR-022): registration performs no I/O and panics on misuse known at the call site, a registration after `Finalize` or after the App is prepared or shut down included ([worker spec](../specs/worker.md#validation)); `Finalize` returns what only the whole graph reveals, such as a provided worker's missing `T`; the start phase returns I/O errors, among them a provided worker's failed construction. No `Must*` twins exist. `ParseSchedule(expr) (*Schedule, error)` serves applications that validate a configuration-supplied expression and want an error.

> **Pending v0.25.0 (W5):** the next paragraph replaces the paragraph above when W5 ships.

The registration methods panic on misuse, under the framework's three-phase rule (ADR-022): registration performs no I/O and panics on misuse known at the call site — a `w` of none of the three forms, and a registration after the container was finalized or after the App shut down, included ([worker spec](../specs/worker.md#validation)); `Finalize` returns what only the whole graph reveals, such as a missing parameter of a run function or a constructor; the start phase returns I/O errors, among them a constructor's failure. No `Must*` twins exist. `ParseSchedule(expr) (*Schedule, error)` serves applications that validate a configuration-supplied expression and want an error.

> **Pending v0.25.0 (W6):** until W6 ships, a registration after `Finalize` panics with `worker: Scheduled("report") after app.Finalize; register workers before Finalize`, which names no trigger.

A registration after the container was finalized names, under the worker's own call, what finalized it — `Finalize`, the first resolving call with its type, or the App's preparation with its entry point — where, and the calls that led there, with the remedy: `credo: Workers.Continuous("outbox-relay"): called after the container was finalized by Finalize at main.go:40; register before that call`. One template serves every registration of the App ([ADR-022](022-bootstrap-and-di-ownership.md)).

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

> **Pending v0.25.0 (W4):** until W4 ships, the accessor is `worker.CurrentRun(ctx) (worker.RunInfo, bool)`, with the same fields and rules; the root names below replace it.

`credo.CurrentWorkerRun(ctx) (WorkerRun, bool)` returns the worker's name, the run's identifier and the intended activation time. One context key carries them, the boolean says whether the context is a run context at all, and a field can be added without adding a function. The run context carries execution metadata only: services, loggers and configuration come from constructors.

### Logging has one vocabulary and a fixed shape

Every started worker writes exactly one `worker started` and one `worker stopped` line, both owned by the loop so no exit path can miss them. One message names each event whatever the kind, and run lines carry the `run_id` that `CurrentRun` returns inside the run. A successful scheduled run is logged at Debug, since liveness is answered by the snapshot and readiness, not by an Info line per run. A single terminal line, `worker failed`, with a `reason`, gives one alerting rule for "a worker gave up" whatever the kind. The stop line stays at Info even for a failed worker: the preceding Error line is the alerting signal. The [worker spec](../specs/worker.md#logging-contract) lists every message and attribute.

### State

The shutdown deadline is the application's; there is no worker-specific shutdown timeout, which would only create ambiguity about which deadline wins. `StatusFailed` is permanent until the application restarts; there is no paused state and no per-worker cancellation. Runner state is guarded by a mutex rather than an `atomic.Value`, whose stores must share one concrete type. The supervisor logs through `App.Logger()` with `module=worker` rather than through a logger registered in DI, which would let services bypass the per-service logger of `credo.Infra`. Timing tests use `testing/synctest` instead of a clock abstraction that production code would carry only for tests.

## Rejected Alternatives

> **Current until v0.25.0 (W5):** the next item names the provided form as the DI-built worker.

- **Registering workers after `Finalize`, or a builder/App type split** that would make a late registration a compile error — both rejected in [ADR-022](022-bootstrap-and-di-ownership.md#rejected-alternatives). A worker set that depends on data is one worker whose `Run` reads the data and fans out, or a component whose `Start` does that work; a DI-built worker is the provided form, registered before `Finalize` and built in the start walk.

> **Pending v0.25.0 (W5):** the next item replaces the item above when W5 ships.

- **Registering workers after `Finalize`, or a builder/App type split** that would make a late registration a compile error — both rejected in [ADR-022](022-bootstrap-and-di-ownership.md#rejected-alternatives). A worker set that depends on data is one worker whose `Run` reads the data and fans out, or a component whose `Start` does that work; a DI-built worker is a run function or a constructor, registered before the container is finalized and built in the start walk.

- **One interface with the kind chosen by an option** (`WithSchedule`). Rejected: the contract of a nil return would be decided far from the call that registers the worker, and every kind-specific option becomes a cell of a runtime validation matrix.
- **Two interfaces** (`Run` for continuous work, `Do` for scheduled work). Rejected: the registration method already names the contract, and closures — the common form — would need two adapters.
- **Functional options with presence flags**, distinguishing an omitted setting from an explicit zero. Rejected: configuration structs are the framework's convention, and an omitted-versus-zero distinction exists only to give meaning to a three-level resolution no application needs.
- **A `worker` configuration section** read by the package. Rejected: it would be the only section a feature package owns beside the root's `server`, it moves the source of a worker's delays away from the call that states everything else about the worker, and typed application configuration passed in code covers the need.
> **Current until v0.25.0 (W5):** the next two items give the reasons of the provided form's era.

- **A placeholder value for provided workers** (`workers.Continuous(name, worker.Provided[T]())`). Rejected: it halves the method count, but it puts into the `Worker` type a value that cannot do what the interface promises; the supervisor would have to recognize it through an unexported interface, and the recognition is lost the moment an application wraps it, so a valid-looking `Worker` fails at run time — for a continuous worker, in a restart loop. Four literal methods add no rule to learn.
- **`worker.Provide[T](app, constructor, opts...)`**, combining provider and registration. Rejected: it duplicates `app.Provide`, hides the provider from ordinary DI reading, and still needs the name outside the interface.

> **Pending v0.25.0 (W5):** the next two items replace the two above when W5 ships.

- **A placeholder value for a worker the container builds** (`worker.Provided[T]()` passed as the worker). Rejected: it puts into the `Worker` type a value that cannot do what the interface promises; the registry would have to recognize it through an unexported interface, and the recognition is lost the moment an application wraps it, so a valid-looking `Worker` fails at run time — for a continuous worker, in a restart loop. A constructor, and the method expression of a binding's `Run`, name the same workers in forms the registration recognizes by their shape.
- **`worker.Provide[T](app, constructor, opts...)`**, combining provider and registration. Rejected: it makes the worker a binding, which duplicates `app.Provide` and hides the provider from ordinary DI reading. The rejection belonged to a worker that is a binding: a constructor handed to the registration creates no binding and duplicates nothing, which is why the constructor form is accepted, and a worker that is a binding is bound with `app.Provide` and registered through its `Run`.

- **A `Name()` method on the worker as its identity**, with a name parameter only for the provided form, checked at start. Rejected: two sources of truth for one identity.
- **A provided worker as a value that resolves `T` in its `Start`.** Rejected: a value has no edges, so the order a worker that writes to a database needs would become a registration-order convention.
- **The supervisor as a component, or attached through its own start and drain hooks and a DI binding.** Rejected: one stop moment for every worker cannot serve both a queue consumer that must outlive the handlers and a cron job that must not start a run during the drain, and several mechanisms for one resource reach its shutdown more than once. Each worker is a component instead.
- **A tier for the whole supervisor.** Rejected for the same reason: the tier belongs to each registration.
- **A root `RegistrationOpen()` predicate, or registration guards of the supervisor's own.** Rejected: the window is the component registry's, bootstrap is sequential by contract (ADR-022), and a late registration panics where the registry refuses it.
> **Current until v0.25.0 (W4):** the next item gives the reason of the `worker` package's era.

- **A "one supervisor per App" slot in the kernel.** Rejected: each `Use` is an independent registry, and the one duplicate that matters — a worker name — is caught by the supervisor within one registry and by the component registry's unique names across registries. A process-wide guard is rejected because one test process runs many Apps.

> **Pending v0.25.0 (W4):** the next item replaces the item above when W4 ships.

- **A "one supervisor per App" slot in the kernel.** Rejected for the `worker` package, whose registries were independent: the one duplicate that matters — a worker name — was caught across them by the component registry's unique names. That reason never addressed a snapshot that covers every worker. In the root package the registry is a field of the App, so one registry per App needs no slot in a kernel, and `Snapshot` and `Lookup` see every worker. A process-wide guard stays rejected because one test process runs many Apps.

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

> **Pending v0.25.0 (W4):** the next items record the alternatives to the root-package placement; until W4 ships, the workers live in the `worker` package.

- **Keeping the `worker` package.** Rejected: its registries are separate by construction, so no snapshot covers every worker of an App, and the package's placement contradicts the rule that a subsystem the App holds one of lives in the root ([ADR-002](002-code-acquisition-strategy.md#what-lives-in-the-root-package)).
- **Registration methods flat on the App** (`app.Continuous`, `app.Snapshot`). Rejected: the names would not say what they register or report, and the worker's methods would sit among the App's routing and DI methods; one noun accessor names the subsystem whose many registrations it holds.
- **One `Add` method with the kind as an argument, and its variants that infer or default the kind.** Rejected: the kind would no longer be stated by the call that registers the worker, which is where the contract of a nil return must be visible, and one method cannot take each kind's own configuration type, so a setting of the other kind would be writable again.
- **The registry in the root package with its types in `worker`.** Rejected: the types name `credo.Tier` and the registry names the types, an import cycle through `credo.Tier`.
- **Root aliases of internal types.** Rejected: an alias's documentation lives at the internal type, so `go doc` and pkg.go.dev would not show the types where users look.

> **Pending v0.25.0 (W5):** the next items record the alternatives to the three registration forms; until W5 ships, the provided form is the DI-built worker.

- **The provided form kept beside the constructor form.** Rejected: two paths to one result, each with its own rules for edges, duplicates and teardown.
- **A typed constructor method**, a generic registration method over the constructor's result type. Rejected: a generic method fixes one signature, while constructors take any parameters, so it would check nothing that registration does not check and add a method per kind.
- **Refusing a bare `func(context.Context) error` with the remedy of wrapping it.** Rejected: the wrap is a conversion the registry can make itself.
- **A `WorkerFunc` type beside the bare function.** Rejected: a second spelling of one worker.
- **Registering the constructor as a binding inside the registration.** Rejected: it would be a binding the application did not write, colliding with `credo.Override()` and with a second instance of one type, which is why the framework binds nothing automatically ([ADR-004](004-dependency-injection-and-infra.md)); a worker that is a binding is bound by the application.
- **A handle that names a binding as the worker**, such as `credo.Bound[T]()`. Rejected: with `w any` it could be a descriptor the registration recognizes rather than a `Worker` that cannot run, but it is the provided form under another spelling, a second path to one result and a root name, and it removes no rule — a constructor that returns a binding, whether a worker's or one handed to `Manage`, needs the holder of record anyway — while the method expression of the binding's `Run`, `(*OutboxRelay).Run`, already names the binding, in a form the compiler checks.
- **A run function whose `context.Context` must come first.** Rejected: a method expression puts its receiver first, so the bound worker's form would not register.
- **The worker's component calling the built value's `Shutdown` directly.** Rejected: a direct call bypasses the component registry, so the identity, kind and owner rules — one resource, one teardown — would not apply to the value.
- **Never tearing the built value down.** Rejected: a value the App built is the App's to release, as every constructor result is, and a worker with state that outlives a single `Run` would never flush it.

Deferred, each until something needs it:

- **A worker kind that runs once at start** — background work at start whose success is terminal `stopped` and whose failure follows a restart policy, for a warm-up that must not block startup but should gate readiness. The kind-per-method registration makes adding it non-breaking.
- **A policy by which a terminal worker failure stops the App** — the process exits non-zero and whatever supervises it restarts it, the one escalation that works outside Kubernetes. It needs a lifecycle decision of its own: no component ends the App (ADR-024), and exit semantics, the interaction with the drain and the error `Run` returns belong to that decision, not to the worker system. Until then the liveness recipe is the documented route.
- **A lock hook for scheduled runs** (`ScheduledConfig.Lock`) that `store/sqldb` could implement; the documented workaround is an application-level lock. A hook field is excluded from the snapshot's echo.
- **A per-run logger** (`worker.Logger(ctx)`) pre-tagged with `worker` and `run_id`. It raises which base logger it derives from, and belongs to the observability release ([ADR-014](014-observability.md)).

## Consequences

Constructor-injected workers are the default path, ordered against the components they use in both directions, and registration is testable: a test reads `Snapshot()` and asserts the effective policy of every worker without running it. A continuous worker cannot disappear silently, a timeout cannot report a success, and a panic cannot pass as a shutdown. An in-process consumer sees every job the handlers enqueued before the drain, and its database closes after it. Operators get one start and one stop line per worker, correlated run lines, one terminal line to alert on and a stable JSON snapshot.

Metrics and tracing hooks are left to the observability release ([ADR-014](014-observability.md)); they will reuse the `run_id` and `duration` attributes.

Restart backoff spaces restarts without changing whether they happen: a worker with `Restart.Limit` N reaches `failed` — and an `UnreadyWhenFailed` readiness check drops — later than a fixed 3 s delay would make it: with the defaults, `Limit: 5` waits roughly 48–93 s in total instead of 15 s. Equal `MinDelay` and `MaxDelay` give a fixed delay.
