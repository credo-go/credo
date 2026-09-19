# ADR-023: Worker System

**Status:** Accepted, implemented in v0.20.0 **Date:** 2026-09-19 **Depends on:** ADR-004, ADR-006, ADR-016, ADR-022 **Related:** ADR-021 **Specification:** [Worker spec](../specs/worker.md) **Guide:** [Worker guide](../guides/worker.md)

## Context

Applications built on Credo run background work next to their HTTP handlers: queue consumers and watchers that live for the whole process, and periodic jobs — cleanups, reports, reconciliation — that run on a schedule. Without framework support each application hand-rolls goroutines, panic recovery, restart loops, cron parsing and shutdown coordination, and gets the interaction with the application lifecycle and DI teardown subtly wrong.

The `worker` package shipped as a beta that unified both kinds of work. Using it in real applications and reviewing its code exposed a set of contract problems that had to be settled before v1:

- A worker built by the DI container could not be registered. Registration is closed after `Finalize`, and `Resolve` is admitted only after `Finalize` (ADR-022), so the framework's headline constructor-injection pattern did not work for workers.
- The worker's name lived on the instance (`Name()`), but registration needs it before the instance exists (for duplicate detection and the readiness check name), and the registered name could diverge from what the instance reported.
- The snapshot did not report the configuration the runner actually executed, was assembled in two places, used one counter with two meanings and an untyped kind.
- `WithMaxRestarts(N)` allowed N−1 restarts.
- A continuous worker whose `Run` returned nil while the application was alive stopped permanently, silently, and invisibly to readiness.
- Lifecycle logging depended on the exit path, successful runs and starts were not logged, framework lines could not be correlated with the worker's own, and skipped activations were logged one line each.
- A panic's stack was embedded in the error text that snapshots and readiness expose.
- `@every` silently rewrote zero, negative and sub-second durations to one second.
- A scheduled run could be started with an already cancelled context.

Credo is pre-1.0 and a breaking change takes its own minor (see the [CHANGELOG](../../CHANGELOG.md) preamble and the v1 Gate in [TODO](../../TODO.md)), so the fixes ship together as one contract in one minor rather than as an additive minor followed by a breaking one.

## Decision

### One package, two kinds

Continuous and scheduled work share one package, one interface and one lifecycle; a schedule is a registration option, not a separate system, which is why no separate `cron` package exists. Options are kind-specific and a wrong-kind option is a registration error, never silently ignored: a single `WithMaxAttempts` would have meant "restarts after crashes" for one kind and "consecutive failed activations" for the other.

### The name is registration identity

`Worker` has one method, `Run(ctx) error`. The name is an argument of `Register(app, name, w, opts...)`, the same position Credo takes for routes and stores: a name identifies a registration, not an instance. `Func` is a function type in the `http.HandlerFunc` style. Names are validated strictly and uniformly — non-empty, no surrounding whitespace, no control characters, never normalized — because a name is a log attribute, a future metric label and the suffix of a readiness check name, and adding readiness later must never force a rename. No prefix is reserved: no framework-owned worker needs a namespace, and health's `credo.` reservation applies to the full check name `worker:<name>`.

### DI-provided workers resolve at pool start

`RegisterProvided[T Worker](app, name, opts...)` records a resolver instead of an instance; `T` is resolved with `app.Resolve[T]()` when the pool starts, in the `OnStart` phase — after the implicit `Finalize`, before the server accepts traffic. Registration order relative to `Provide` is free, `T` may be an interface bound through `Alias`, and a type without `Run` is a compile error. The DI rule forbids resolution only in the three shutdown hooks, so resolving in `OnStart` is within it.

Start is all-or-nothing and runs user constructors outside the pool lock: claim under the lock, resolve outside it, publish under it. A `Shutdown` that arrives during resolution wins, and the existing guarantee that no goroutine joins a wait a concurrent `Shutdown` has begun is preserved. A failed resolution fails the application's startup with an error naming the worker and the type.

### The snapshot carries the effective configuration

`Info` is identity, a nested `Config` and live state — configuration and state never share a mutable struct. `Config` reports the policy the runner executes after defaults and configuration are applied, not the options as passed. `Kind` is a typed string, and the overloaded counter is split into `Restarts` (continuous) and `ConsecutiveFailures` (scheduled). One builder produces every `Info`. Because applications serve `Pool.Workers()` from admin endpoints, the JSON shape is a wire contract: snake_case names, zero timestamps, errors and the readiness policy omitted, durations as integer nanoseconds under the response profile of ADR-021.

### Run outcome and loop exit are separate

After every run the loop first classifies the outcome, records it, and only then decides whether the loop ends. The classification order is fixed: a panic is a failure; a run whose context was cancelled by its timeout is a failure; a nil return, or an error that is nothing but a context error, while the pool is stopping is a graceful stop; any other error is a failure — including a context error joined with another error, since `errors.Join(ctx.Err(), flushErr)` must not hide a final write that failed; a scheduled nil return is a success; a continuous nil return while the pool is alive is a failure. The loop exits as failed when a limit was just exhausted, otherwise as a shutdown when the pool is stopping. A timeout followed by a shutdown is therefore a timed-out run and a shutdown exit — neither erases the other — and a graceful stop records only the status, so `LastError` keeps one meaning: the most recent failed run.

### Continuous workers are permanent

A continuous worker's `Run` must remain active until the pool context is cancelled. A nil return while the context is alive is a failure with an actionable message, restarts after the restart delay, and reaches `StatusFailed` once a positive `WithMaxRestarts` limit is exhausted — which is what makes the readiness condition `FailWhenFailed` see a dead consumer. Finite background work belongs in `OnStart`, or in a continuous worker that waits for `<-ctx.Done()` after the work. This is the only restart strategy offered.

The cost is accepted and stated: a worker that returned nil on purpose keeps compiling and becomes a restart loop. It is loud at run time — an Error line naming the contract on the first occurrence — and it leads the release notes.

### Timeouts are cooperative and cannot be laundered

`WithRunTimeout(d)` bounds every run of a scheduled worker by deriving the run context with `context.WithTimeoutCause(…, ErrRunTimeout)`. Classification reads the cause, not the return value: a run cut short by its budget is a failure even when it returns nil, because Credo's own idiom `case <-ctx.Done(): return nil` would otherwise turn half-done work into a success that stamps `LastSuccess`, resets the failure streak and opens a readiness barrier. The accepted cost is conservative: a run that completes just after its deadline counts as failed. The timeout never abandons the goroutine, so at most one run per worker is ever active. It is scheduled-only (a continuous `Run` lives for the process) and has no pool-level default (budgets are per job).

### Panics are recorded without their stack

A recovered panic becomes an unexported error `worker: run panicked: <value>` with the stack kept alongside and logged as a separate attribute. It has no `Unwrap`, and the panic row is evaluated before the graceful row, so a panic whose value is a context error during shutdown can never pass as a graceful stop. `LastError`, and anything readiness exposes, never contains a stack trace. The type is unexported because a worker's error never returns to user code.

### Counters mean one thing

`WithMaxRestarts(N)` means the first run plus at most N restarts. `Restarts` counts restarts that actually started: it advances in the run-admission commit, not when the previous run failed, so a shutdown during the restart delay does not count one. The run-context accessor for an attempt number is removed: the next cron activation is not a retry of the previous one, and removing an accessor is the reversible choice.

### Run admission is one ordered step

The activation time is computed without side effects, the pool context is checked, one commit records the run (`StatusRunning`, `LastRun`, `Restarts`), and `Run` is called. The commit is the only writer of `StatusRunning`. If the check observes cancellation, no new run starts and nothing is recorded. The order is what lets an in-package test policy prove the property without a production test seam.

### Scheduling is serial and skip-only

Each scheduled worker is one goroutine that sleeps until the next activation, runs it synchronously and recomputes. Overlap is impossible by construction; activations that pass during a run are skipped, never queued, and reported in one line per resumption. The next activation is computed from the last intended one, so `@every` grids do not drift by run duration. `@every` accepts only positive whole seconds and rejects every input it previously rewrote, so the registered expression is the effective schedule.

### Logging has a fixed shape

Every worker writes exactly one start line and one stop line per pool session, both owned by the loop so no exit path can miss them. Failure lines carry `run_id` (the value `RunID(ctx)` returns inside the run) and `duration`, plus `timed_out`, `unexpected_exit` or `stack` when they apply. The stop line stays at Info even for a failed worker — the preceding Error line is the alerting signal. Successful scheduled runs log at Debug: liveness is answered by the snapshot and readiness, not by an Info line per run.

### Lifecycle and state

Workers start in `OnStart`, after the port is bound, and drain in `OnDrain`, before any DI singleton is torn down, so a worker's final flush never meets a closed resource regardless of registration order. The shutdown deadline is the application's; there is no worker-specific shutdown timeout, which would only create ambiguity about which deadline wins. `StatusFailed` is permanent until the application restarts; there is no paused state and no per-worker cancellation. Runner state is guarded by a mutex rather than `atomic.Value`, whose stores must share one concrete type. The pool logs through `App.Logger()` rather than a logger registered in DI, which would let services bypass the per-service logger of `credo.Infra`. Timing tests use `testing/synctest` instead of a clock abstraction that production code would carry only for tests.

## Alternatives considered

- **Re-open registration after `Finalize`** for DI-built workers. Rejected: the uniform registration window was a deliberate fix, and publishing the pool and its readiness seam are DI writes that cannot happen after the freeze. A "declare the pool first" call would add ceremony to every application.
- **`worker.Provide[T](app, constructor, opts...)`**, combining provider and registration. Rejected: it duplicates `app.Provide`, hides the provider from ordinary DI reading, and still needs the name outside the interface.
- **Keep `Name()` and add a name parameter only to the provided form**, checked at start. Rejected: two sources of truth for one identity.
- **A root validation hook at `Finalize`** to report a missing provider earlier. Rejected: new root surface for one consumer; the start-time failure is already before traffic and names the worker and type.
- **Echo the options exactly as given in the snapshot.** Rejected in favor of the effective policy: an echoed zero delay or an absent option would misreport what the runner does.
- **Flat state and configuration on `Info`, or per-kind configuration sub-structs.** Rejected: the first mixes configuration with state; the second adds nesting without a consumer, since `Kind` already disambiguates a zero.
- **Treat a nil return after the deadline as a success.** Rejected for the laundering reason above.
- **An exported panic error that unwraps its value.** Rejected: it would let a context-error panic during shutdown pass as a graceful stop, and no caller would receive the error.
- **An attempt number unified as `consecutiveFailures + 1` for scheduled workers.** Rejected: activations are not retries of each other.
- **Transient or temporary restart strategies, or options such as "restart on success".** Not offered; one permanent strategy covers continuous work, and finite work has documented homes.
- **Reserve the `credo.` prefix for worker names.** Not reserved: nothing needs it now, and restricting names later must account for the names the contract already allows.
- **Overlap policies "allow" and "queue".** Deferred: allowing overlap needs per-execution state (several running flags and last errors, completion-order failure counting) that breaks the one-runner model; queueing adds buffering and staleness rules.
- **A scheduler goroutine feeding an executor goroutine per worker.** Replaced by the serial loop, which has the same observable semantics without the coordination channels.
- **Preemptive timeouts or a pool-level default budget.** Rejected: abandoning a goroutine breaks the one-run-at-a-time guarantee, and budgets differ per job.
- **A worker test package.** Not provided: a `Run`-only worker is tested by calling `Run`.

## Consequences

Constructor-injected workers are the default path, and registration is testable: a test reads `Pool.Workers()` and asserts the effective policy of every worker without running it. A continuous worker can no longer disappear silently, a timeout cannot report a success, and a panic cannot pass as a shutdown. Operators get one start and one stop line per worker, correlated run lines and a stable JSON snapshot.

Upgrading from earlier releases is a breaking change: registration calls take a name, `Func` loses its name argument, snapshot fields and counters change, the attempt accessor is removed, `WithMaxRestarts(N)` allows one more run, several log messages change, `@every` inputs that were rewritten are rejected, and — without a compile error — a continuous `Run` that returns nil on purpose is restarted. The release notes and the [pre-v1 migration guide](../guides/pre-v1-migration.md#workers) carry the full table. Metrics and tracing hooks are left to the observability release; they will reuse the `run_id` and `duration` attributes.
