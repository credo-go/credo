# ADR-023: Worker System

**Status:** Accepted, implemented in v0.20.0; [restart backoff](#restart-backoff) accepted 2026-09-19, pending implementation (v0.21.0) **Date:** 2026-09-19 **Depends on:** ADR-004, ADR-006, ADR-016, ADR-022 **Related:** ADR-021 **Specification:** [Worker spec](../specs/worker.md) **Guide:** [Worker guide](../guides/worker.md) **Plan:** [Restart backoff and startup features](../plans/restart-backoff-and-startup-features.md)

**2026-09-19 amendment:** continuous workers will restart with a capped, jittered exponential delay by default. The decision is recorded under [Restart backoff](#restart-backoff) and in the alternatives below. Until it ships in v0.21.0 the restart delay is fixed, and every other section describes the released behavior.

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

### Restart backoff

**Accepted 2026-09-19, pending implementation (v0.21.0).**

Permanence made the restart delay load-bearing. The delay is fixed (3 s by default), `WithMaxRestarts` is unlimited by default, and every failed run writes one Error line. A continuous worker whose dependency stays unreachable, and whose `Run` therefore fails immediately every time, restarts about 28,800 times a day, writes at least as many Error lines and reconnects to the dependency at the same rate. Backoff changes how long the runner waits between restarts, not whether it restarts: the permanent-restart contract stands.

After a failed run for which a restart is planned, the delay is drawn from a window that doubles and is capped:

```text
ceiling = min(base × 2^(k−1), cap)
lower   = max(base, ceiling/2)
delay   = uniform in [lower, ceiling]; exactly lower when lower == ceiling
```

`k` is the failure's position in the current sequence, starting at 1, and the multiplier is fixed at 2. `base` is the existing effective restart delay (`WithRestartDelay` → `worker.restart_delay` → `DefaultRestartDelay`); it becomes the first and the minimum delay. The cap is new: `WithMaxRestartDelay(d)` per worker, `worker.max_restart_delay` per pool, `DefaultMaxRestartDelay` = 1 minute otherwise.

The jitter keeps `base` as a floor. Full jitter — a uniform pick in `[0, ceiling]` — spreads load best and is the right choice for `httpclient` retries, but it can pick a near-zero wait and reintroduce the tight loop the restart delay exists to prevent. With the floor, the first delay is exactly `base`, no delay is shorter than `base` or longer than the cap, and a cap equal to `base` is a fixed delay without a separate mode. Individual delays need not grow monotonically; their window does.

A run that lasted at least the effective cap resets the sequence: its failure counts as the first, so the next restart waits `base`. Without a reset, a worker that runs for hours and fails once a day would wait at the cap for ever. Runtime is a heuristic, not proof of health, so a long run resets only the backoff — never `Restarts` or the `WithMaxRestarts` budget.

The cap resolves the way `WithRestartDelay` resolves today. An omitted option takes the pool configuration, then the default, and is raised to `base` when `base` is larger: a pool-level cap below one worker's base is a default that does not fit that worker, not an error. An explicit `WithMaxRestartDelay(0)` skips the pool configuration and selects `max(DefaultMaxRestartDelay, base)`. An explicit positive cap is used as given, and one below the effective `base` is a registration error. An existing `WithRestartDelay(10 * time.Minute)` therefore stays valid, and stays a fixed delay unless a pool cap above ten minutes is configured. The only fixed delay that no configuration can change is the same positive value in both options.

Backoff is the default rather than an option, because the flood appears precisely in applications that registered a continuous worker without restart options. With the default base and cap, the capped window is 30–60 s, about 45 s on average: roughly 1,920 runs a day once the cap is reached (2,880 at the floor), fifteen times fewer than today. A 5-minute cap would give about 384. The cap also bounds the wait before the next recovery attempt, so a larger default would delay recovery for every application after its dependency returns; an application that expects long outages raises the cap per pool or per worker. Log volume shrinks; failures are neither hidden nor downgraded.

The snapshot and the log make the policy visible. `Config` reports the effective cap as `MaxRestartDelay` (`max_restart_delay`; zero for scheduled workers). The continuous failure line carries `next_restart_in`, the selected delay, only when a restart is planned — not when the limit was just exhausted and not when shutdown was already observed — so the restart decision is made before the line is written. The streak and the current delay are not exposed in `Info`: no consumer needs them, and either can be added later. Scheduled workers are unchanged: their cadence is the schedule, and `WithMaxRestartDelay` on a scheduled worker is a registration error like the other continuous-only options.

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
- **Opt-in restart backoff.** Rejected: the failure it prevents occurs in applications that rely on the defaults.
- **Full jitter for restart delays.** Not chosen: it can pick a near-zero wait, and the restart delay is a minimum-wait guarantee. It remains the right choice for `httpclient` retries.
- **A circuit breaker or an elapsed-time budget ("give up after T").** Not offered: neither is the same as `WithMaxRestarts`, but the restart-count budget covers every known need, and nothing demonstrates a need for a second limit.
- **Per-error-class restart delays** (retry a timeout quickly, an authentication failure slowly). Not offered: the worker's `Run` knows which failures deserve a quick retry and can retry them inside the run.
- **Backoff for scheduled workers.** Not offered: a schedule is already a cadence, and skipping activations after failures would be a different feature (pause on failure).
- **Pluggable backoff strategies, or a configurable multiplier, jitter or reset threshold.** Not offered: one built-in policy with two settings, base and cap, each available per worker and per pool.
- **The backoff streak or the current delay in `Info`.** Deferred until a consumer needs them; the planned delay is on the failure line.
- **Time zone selection for cron schedules** — a per-worker zone, or `TZ=`/`CRON_TZ=` prefixes. Not offered: schedules use the process's local time zone by design, and selection can be added when a concrete consumer needs it.
- **Reserve the `credo.` prefix for worker names.** Not reserved: nothing needs it now, and restricting names later must account for the names the contract already allows.
- **Overlap policies "allow" and "queue".** Deferred: allowing overlap needs per-execution state (several running flags and last errors, completion-order failure counting) that breaks the one-runner model; queueing adds buffering and staleness rules.
- **A scheduler goroutine feeding an executor goroutine per worker.** Replaced by the serial loop, which has the same observable semantics without the coordination channels.
- **Preemptive timeouts or a pool-level default budget.** Rejected: abandoning a goroutine breaks the one-run-at-a-time guarantee, and budgets differ per job.
- **A worker test package.** Not provided: a `Run`-only worker is tested by calling `Run`.

## Consequences

Constructor-injected workers are the default path, and registration is testable: a test reads `Pool.Workers()` and asserts the effective policy of every worker without running it. A continuous worker can no longer disappear silently, a timeout cannot report a success, and a panic cannot pass as a shutdown. Operators get one start and one stop line per worker, correlated run lines and a stable JSON snapshot.

Upgrading from earlier releases is a breaking change: registration calls take a name, `Func` loses its name argument, snapshot fields and counters change, the attempt accessor is removed, `WithMaxRestarts(N)` allows one more run, several log messages change, `@every` inputs that were rewritten are rejected, and — without a compile error — a continuous `Run` that returns nil on purpose is restarted. The release notes and the [pre-v1 migration guide](../guides/pre-v1-migration.md#workers) carry the full table. Metrics and tracing hooks are left to the observability release; they will reuse the `run_id` and `duration` attributes.

The v0.21.0 restart backoff is a behavior change without a compile error. Restarts move further apart after repeated failures, so a worker with `WithMaxRestarts(N)` reaches `failed` — and a `FailWhenFailed` readiness check drops — later than before: with the defaults, `WithMaxRestarts(5)` waits roughly 48–93 s in total instead of 15 s. Setting both delay options to the same value restores the fixed delay. Those release notes and the migration guide will carry both rows.
