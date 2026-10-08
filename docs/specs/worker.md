# Worker Spec

**Status**: Implemented (worker contract v0.20.0, restart backoff v0.21.0, workers as lifecycle components v0.24.0) **Package**: `worker/` **Sources**: robfig/cron v3 (MIT, cron expression parser only) **ADR**: [ADR-023](../adr/023-worker-system.md) **Guide**: [Worker Guide](../guides/worker.md)

This file is the contract of Credo's worker system: what registration accepts, how each worker attaches to the App as a lifecycle component, how a run is admitted and classified, what the supervisor reports, and which log lines it writes. The rationale and the rejected alternatives live in ADR-023; the component model lives in [ADR-024](../adr/024-lifecycle-components.md) and the [lifecycle spec](lifecycle.md).

---

## Overview

The `worker/` package runs background tasks — queue consumers, watchers, periodic cleanups, reports — inside a Credo application. Continuous and scheduled work share one interface; the kind is chosen by the registration method.

- **One interface** — `Worker` has a single method, `Run(ctx) error`. The name is given at registration and is the worker's identity.
- **One supervisor** — `worker.Use(app)` returns a `*Supervisor`, a registry and reporting object with no lifecycle of its own.
- **Four registration methods** — `Continuous` and `Scheduled` for a constructed value, `ContinuousProvided[T]` and `ScheduledProvided[T]` for a worker the DI container provides; `T` is built in the start walk.
- **One component per worker** — each registration adds a component named `worker:<name>` to the App: its `Start` launches the loop, its `Shutdown` cancels the worker's context and waits for `Run`, and its `Ready`, present only when the registration sets a readiness condition, reports those conditions.
- **A tier per registration** — a scheduled worker defaults to the ingress tier and stops with the HTTP drain; a continuous worker defaults to the internal tier and stops after it, in reverse dependency order.
- **Per-kind configuration in code** — `ContinuousConfig` and `ScheduledConfig`, zero meaning the default; the package reads no configuration section.
- **Fail-fast registration** — names, configurations and cron expressions are validated when the registration method is called, and misuse panics; nothing is parsed at run time.
- **Run outcomes are classified in one place** — panics, timeouts, graceful stops, errors, successes and early continuous exits follow one ordered table, and the loop exit is decided only after the outcome is recorded.
- **Continuous workers are permanent** — `Run` must stay active until shutdown; an early nil return is a failure and follows the restart policy.
- **Scheduled workers never overlap** — one goroutine per worker runs activations serially; activations that pass during a run are skipped and logged.
- **Cooperative timeouts** — `ScheduledConfig.RunTimeout` cancels a scheduled run's context; a timed-out run is a failure whatever `Run` returns.
- **Observable** — exactly one start and one stop line per started worker, run lines correlated by `run_id`, one terminal line, `Snapshot()` and `Lookup()` with snake_case JSON, opt-in readiness.

---

## Goals

1. One abstraction for continuous and scheduled background work; cron is a scheduling strategy, not a separate system.
2. The kind's contract is visible at the call that registers the worker, and a setting of the other kind cannot be written.
3. Constructor-injected workers work with the framework's bootstrap order (register → `Finalize` → start) and are ordered against the components they use, in both directions.
4. Registration is verifiable: a test can read the effective policy of every registered worker without running it.
5. The lifecycle is legible from default-level logs and from readiness; no worker can end without reaching a reported state.
6. Every timing path is deterministically testable with `testing/synctest`, without a clock seam.

## Non-Goals

- Job queues, task distribution, persistent job state (use a dedicated system).
- Cluster-aware scheduling: the scheduler is per process, and every replica runs every schedule ([Scheduled workers](#scheduled-workers)).
- On-demand triggering of a scheduled worker, or a synchronous first run as a startup gate (`UnreadyUntilFirstSuccess` covers the rotation side).
- Overlap policies other than skip; preemptive timeouts that abandon a goroutine; per-worker cancellation.
- Restart strategies other than permanent for continuous workers.
- A worker kind that runs once at start, a policy by which a terminal worker failure stops the App, a lock hook for scheduled runs and a per-run logger — deferred (ADR-023).
- Automatic readiness binding: participation is opt-in per condition.
- A worker test package: a worker is tested by calling `w.Run(t.Context())`.
- Metrics and tracing hooks (Phase 3.5; they will reuse the `run_id` and `duration` log attributes).

---

## Three-Layer Model

```
Continuous / Scheduled / …Provided       component Start                   Snapshot / Lookup
          │                                     │                                  │
          ▼                                     ▼                                  ▼
  ┌──────────────────┐   build (+ T) + launch  ┌──────────────────┐  snapshot  ┌───────────────────────────┐
  │ definition       │ ──────────────────────► │ runner           │ ─────────► │ Info                      │
  │ (internal,       │                         │ (internal,       │            │ Name, Kind, Schedule      │
  │  immutable)      │                         │  mutable, mu)    │            │ Continuous | Scheduled    │
  └──────────────────┘                         └──────────────────┘            │            ← definition   │
  • name, kind                                 • Worker value                  │ Status, Restarts,         │
  • worker value, or T                         • status, restarts,             │ ConsecutiveFailures,      │
  • compiled schedule                          •   consecutiveFailures,        │ LastStartedAt,            │
  • resolved ContinuousConfig                  •   lastStartedAt,              │ LastSucceededAt,          │
  •   or ScheduledConfig                       •   lastSucceededAt,            │ LastError    ← runner     │
                                               •   lastError                   └───────────────────────────┘
```

Configuration and live state never share a mutable struct. The definition holds the resolved configuration of its kind, and one `Info` builder produces every snapshot — before the worker's component has started and from a running runner alike — so a field added to `Info` cannot be missing from one of them.

---

## Public API

Signatures only; the godoc in `worker/` is authoritative for wording.

```go
// Worker and adapter
type Worker interface{ Run(ctx context.Context) error }
type Func func(ctx context.Context) error // Run calls f(ctx)

// Supervisor
func Use(app *credo.App) *Supervisor
func (s *Supervisor) Continuous(name string, w Worker, cfg ...ContinuousConfig)
func (s *Supervisor) Scheduled(name, expr string, w Worker, cfg ...ScheduledConfig)
func (s *Supervisor) ContinuousProvided[T Worker](name string, cfg ...ContinuousConfig)
func (s *Supervisor) ScheduledProvided[T Worker](name, expr string, cfg ...ScheduledConfig)
func (s *Supervisor) Snapshot() []Info
func (s *Supervisor) Lookup(name string) (Info, bool)

// Configuration
type ContinuousConfig struct{ ... }
type ScheduledConfig struct{ ... }
type Restart struct{ ... }

const DefaultMinRestartDelay = 3 * time.Second
const DefaultMaxRestartDelay = time.Minute
var ErrRunTimeout = errors.New("worker: run timed out")

// Run context
type RunInfo struct {
	Worker      string    // the registration name
	ID          string    // the run's identifier, equal to the run_id of the framework's lines
	ScheduledAt time.Time // the intended activation; zero for continuous runs and the RunOnStart run
}
func CurrentRun(ctx context.Context) (RunInfo, bool)

// Snapshot types
type Kind string   // KindContinuous, KindScheduled
type Status string // StatusPending, StatusRunning, StatusBackoff, StatusWaiting, StatusStopped, StatusFailed
type Info struct{ ... }

// Schedules
func ParseSchedule(expr string) (*Schedule, error)
func (s *Schedule) Next(now time.Time) time.Time
func (s *Schedule) String() string
```

The registration methods return nothing and panic on misuse ([Validation](#validation)). The worker components they add are the App's: no method of the supervisor starts or stops a worker.

**Removed.** The `worker` configuration section; `Option` and every `With*` option (`WithSchedule`, `WithStartImmediately`, `WithMaxConsecutiveFailures`, `WithRunTimeout`, `WithMaxRestarts`, `WithRestartDelay`, `WithMaxRestartDelay`, `WithReadiness`), with the presence flags that told an omitted option from an explicit zero and the cross-kind validation; `ReadinessPolicy`; `Config`; `Pool` with `Start`, `Shutdown` and `Workers`, and its DI binding; `Register`, `RegisterProvided` and their `Must*` twins; `DefaultRestartDelay`; `RunID`, `WorkerName` and `ScheduledAt`; `StatusIdle`. The [migration guide](../guides/pre-v1-migration.md#workers) carries the naming table.

---

## Registration

### The supervisor

`worker.Use(app)` creates a supervisor for `app` and registers nothing: it binds nothing into the container, installs no hook and adds no component. The supervisor logs through `app.Logger()` with `module=worker`. `Use` may be called more than once; each supervisor is an independent registry.

The framework publishes nothing into the application's container. A module that registers workers takes the supervisor as a parameter (`func RegisterBilling(app *credo.App, workers *worker.Supervisor)`), and an application that wants it injected — into an admin controller, say — binds it itself with `app.ProvideValue(workers)`. The supervisor is not a component.

### Names

The name is the registration identity. It is the `worker` attribute of every log line, the `Name` of the snapshot, the suffix of the component name `worker:<name>` — under which readiness and the lifecycle's reports name the worker — and `RunInfo.Worker` inside `Run`. Rules, the same for all four methods:

- non-empty;
- no leading or trailing whitespace (names are never trimmed or otherwise normalized);
- no control characters;
- unique within the supervisor (`worker: Continuous("x"): duplicate worker name "x"; give each worker its own name`), across all four methods; across supervisors, the component registry's unique-name rule refuses a second `worker:<name>`, and so does a component of that name added with `credo.Named` (`worker: Continuous("x"): the App already has a component named "worker:x", registered by another supervisor or with credo.Named; give each worker its own name`).

No prefix is reserved. The health engine's reserved `credo.` prefix applies to the full name, which always starts with `worker:`.

Registering the same `Worker` value under two names runs it on two independent loops; the worker must then be safe for concurrent `Run` calls. Registering one `T` twice through the provided methods panics — under two names in one supervisor, or in two supervisors of one App, which share that check — because the container would hand both loops one instance (`worker: ContinuousProvided[*app.Relay]("b"): *app.Relay is already registered as worker "a"; the container hands every registration the same instance, so register it once`). The worker package keeps that record per App, holding the App weakly; a registration the App refuses releases its entry.

### Configuration

```go
type ContinuousConfig struct {
	Tier              credo.Tier `json:"tier"`                // 0 = credo.TierInternal
	Restart           Restart    `json:"restart"`
	UnreadyWhenFailed bool       `json:"unready_when_failed"`
}

type ScheduledConfig struct {
	Tier                     credo.Tier    `json:"tier"`                        // 0 = credo.TierIngress
	RunOnStart               bool          `json:"run_on_start"`                // one extra run when the worker starts
	RunTimeout               time.Duration `json:"run_timeout"`                 // 0 = none
	MaxConsecutiveFailures   int           `json:"max_consecutive_failures"`    // 0 = unlimited; reaching it is terminal
	UnreadyWhenFailed        bool          `json:"unready_when_failed"`
	UnreadyUntilFirstSuccess bool          `json:"unready_until_first_success"`
	UnreadyAfterSuccessAge   time.Duration `json:"unready_after_success_age"`   // 0 = off
}

type Restart struct {
	Disabled bool          `json:"disabled"`  // true: the first failed run is terminal
	Limit    int           `json:"limit"`     // 0 = unlimited; N = the first run plus at most N restarts
	MinDelay time.Duration `json:"min_delay"` // first and shortest wait; 0 = DefaultMinRestartDelay
	MaxDelay time.Duration `json:"max_delay"` // longest wait; 0 = max(DefaultMaxRestartDelay, MinDelay)
}
```

Each registration method takes its kind's configuration variadically: no argument for the defaults, one for custom, more than one panics. Zero always means the default; nothing distinguishes an omitted field from an explicit zero. The configuration comes from the registration call only: resolution has two levels, the given value and then the package default.

| Field | Zero resolves to | Rejected (panics) |
| --- | --- | --- |
| `Tier` | `credo.TierInternal` for a continuous worker, `credo.TierIngress` for a scheduled one | a value that is neither zero nor one of the two tiers |
| `Restart.Disabled` | restarts enabled | `true` beside a non-zero `Limit`, `MinDelay` or `MaxDelay` |
| `Restart.Limit` | unlimited | negative |
| `Restart.MinDelay` | `DefaultMinRestartDelay` (3 s) | negative |
| `Restart.MaxDelay` | `max(DefaultMaxRestartDelay, resolved MinDelay)` | negative; positive and below the resolved `MinDelay` |
| `RunTimeout` | no timeout | negative |
| `MaxConsecutiveFailures` | unlimited | negative |
| `UnreadyAfterSuccessAge` | off | negative |

Validation runs on the **resolved** values. `MinDelay` resolves first. A zero `MaxDelay` is raised to fit it, so `Restart{MinDelay: 10 * time.Minute}` alone is valid and a fixed ten-minute delay. A positive `MaxDelay` below the resolved `MinDelay` panics — `Restart{MaxDelay: time.Second}` alone is rejected against the default floor, with a message that names both values, says the floor is the default and tells the caller to set `MinDelay` as well: lowering an omitted floor to fit an explicit ceiling would tighten the retry loop, so the application writes that floor itself. A resolved `Restart` with `Disabled` set keeps its delays zero: they do not apply.

The package reads no configuration section. An application that wants environment-driven settings unmarshals a typed section of its own and passes the values:

```go
type workerSettings struct {
	RestartMinDelay time.Duration
	RestartMaxDelay time.Duration
}

settings := app.MustGetConfig[workerSettings]("workers")
workers.ContinuousProvided[*OrderConsumer]("order-consumer", worker.ContinuousConfig{
	Restart: worker.Restart{MinDelay: settings.RestartMinDelay, MaxDelay: settings.RestartMaxDelay},
})
```

A leftover `worker` section is no longer read; like any section nothing decodes, it is ignored, unless the application decodes the whole configuration tree into a struct under `WithStrictDecoding`, where it is an unknown key. A changed schedule, limit or timeout takes effect at the next start: a config reload does not re-register workers.

### Validation

The four methods share one path. In order, a registration panics when:

1. `app` given to `Use` is nil, or (for `Continuous` and `Scheduled`) the worker is nil — including a nil `Func` and a typed-nil pointer;
2. the name breaks a rule above;
3. more than one configuration is given;
4. a configuration value is rejected by the table above;
5. the schedule does not parse (see [Schedules](#schedules));
6. the name is a duplicate in the supervisor, or a provided `T` is already registered in the App, by this supervisor or another;
7. the component registry refuses the worker's component: the registration comes after `Finalize` (an App that is prepared has been finalized) or after the App shut down, or another supervisor or a `credo.Named` component already holds `worker:<name>`.

Every panic follows one format, `worker: <Call>: <problem>; <remedy>`, where `<Call>` is `Continuous("name")`, `Scheduled("name")`, `ContinuousProvided[*T]("name")` or `ScheduledProvided[*T]("name")`, with `T` spelled as Go prints the type (package-qualified). For example:

- `worker: Continuous("order-consumer"): Restart.MaxDelay 1s is below MinDelay 3s (the default); set MinDelay to at most 1s, or raise MaxDelay`;
- `worker: Scheduled("x"): 2 configurations given; pass at most one worker.ScheduledConfig`.

A registration the phase refuses reads without the colon after the call: `worker: Scheduled("report") after app.Finalize; register workers before Finalize`, and `worker: Scheduled("report") after the App shut down; register workers before Finalize`. `Use(nil)` panics with `worker: Use: app must not be nil; pass the *credo.App the workers belong to`. Registration performs no I/O. Errors that only the whole graph reveals are `Finalize`'s, and I/O errors are the start phase's ([Provided workers](#provided-workers)). For a schedule that comes from configuration, `ParseSchedule` returns the parse error instead of panicking.

### Provided workers

`ContinuousProvided[T]` and `ScheduledProvided[T]` add the worker's component to the registry as a constructor over `T` — `app.Manage` receives a `func(T) (component, error)` — so `T` is an edge: the start walk builds the component, resolving `T` from the container, after `T`'s component dependencies have started. Consequences:

- `Provide[T]` and the provided registration may be called in either order, both before `Finalize`.
- The constraint `T Worker` makes a type without `Run` a compile error. `T` may be an interface bound with `app.Alias`.
- A `T` with no binding is a missing dependency: `Finalize` returns it with its path. `T`'s own constructor graph is validated by `Finalize` like any provider's.
- A constructor error or panic while building `T`, or a nil value (`worker: "name": *app.Relay resolved to nil`), is a start failure of the worker's component: the start phase rolls back what was built and `Run` returns the error naming `worker:<name>`.
- The worker stops before `T`'s component dependencies: if `T` itself is a component — it has `Shutdown` — it is shut down after the worker, in dependency order.
- A provided worker of the internal tier whose `T` depends on an ingress component fails `Finalize` with the path and both remedies: declare the worker ingress with `Tier: credo.TierIngress` in its configuration — the message names the worker's own setting, since a worker registration takes no `RegistrationOption` — or split the ingress component so that what the worker uses is an internal part.

A worker registered by value has no edges the dependency graph can see — a closure's captures are invisible — and takes its place in its tier by registration order. A worker that uses infrastructure is registered in its provided form, so the graph orders it.

---

## Components

### Start, Shutdown, Ready

Each registration adds one component, named `worker:<name>`, in the tier its configuration resolves to, exactly as `app.Manage(component, credo.Named("worker:<name>"))` would add it, with `credo.Ingress()` when that tier is ingress. The component follows the lifecycle's rules for every component ([ADR-024](../adr/024-lifecycle-components.md), [lifecycle spec](lifecycle.md)); what it does in each method:

- **`Start(ctx)`** derives the worker's own context from `context.WithoutCancel(ctx)` with a cancel that only `Shutdown` calls, launches the loop on one goroutine and returns nil. The call-scoped `ctx` ends when `Start` returns; the worker's context ends only when its component is shut down. A provided worker's construction precedes `Start`, so its failure is the component's start failure. A second `Start`, or a `Start` after `Shutdown`, returns an error; the lifecycle never makes either call.
- **`Shutdown(ctx)`** cancels the worker's context and returns when `Run` and the loop have returned — and not before. It does not return at the deadline itself: a `Run` that ignores cancellation past the drain deadline is abandoned by the lifecycle, reported in the error `Run` returns, and the components the worker depends on stay open, so nothing closes a database under a run that may still use it. A worker whose component was built but never started shuts down at once.
- **`Ready(ctx)`** exists only when the registration sets a readiness condition ([Health Integration](#health-integration)): the component has one of two types, and the App plans `Ready` from the type. It reads the runner's last state in memory and never performs I/O.

The supervisor has no `Start` and no `Shutdown`: the lifecycle starts and stops each worker's component once, in order, and owns the race between a start and a shutdown requested during it.

### Tiers

| Kind | Default tier | Starts | Stops |
| --- | --- | --- | --- |
| scheduled | `credo.TierIngress` | after the internal tier, before the listener accepts | first, concurrently with the HTTP drain and the other ingress components no edge orders |
| continuous | `credo.TierInternal` | in dependency order, before the ingress tier | after the HTTP drain and the ingress tier, in reverse dependency order; components without edges in reverse registration order |

A scheduled worker originates its runs, so it stops with the listener and no run starts during the drain. A continuous worker that consumes what the HTTP handlers enqueue stops after the handlers have finished and before the database it writes to. A continuous worker that consumes an external queue declares `Tier: credo.TierIngress`, so it stops taking work while the listener does. The two defaults follow the asymmetry of the mistakes: an external consumer left internal merely stops later; an in-process consumer placed in ingress loses the work handlers enqueue during the drain.

### Startup

```
app.Run()
  ├─ prepare: Finalize (graph errors) → compile → publish
  ├─ start walk, internal tier, dependency order
  │   ├─ the components a provided worker's T depends on start first
  │   └─ worker:<name> (continuous default): build (provided: resolve T) → Start launches the loop
  ├─ start walk, ingress tier
  │   └─ worker:<name> (scheduled default): Start launches the loop
  └─ the listener accepts
```

An App served through `ServeHTTP` runs the same walk through `App.Start` ([lifecycle spec](lifecycle.md)).

### Shutdown

```
app.Shutdown(ctx)
  ├─ readiness → unready
  ├─ ingress tier, concurrently with the HTTP drain
  │   └─ worker:<name> (scheduled default): cancel the worker's context, wait for Run
  └─ internal tier, reverse dependency order
      ├─ worker:<name> (continuous default): cancel the worker's context, wait for Run
      └─ then the components the worker depends on
```

One drain deadline serves both tiers and is spent in order, so a long HTTP drain leaves less for an internal worker's final flush. There is no worker-specific shutdown timeout.

---

## Execution

Each worker runs on one goroutine driven by a single loop. A policy supplies the kind-specific parts (the first wait, the activation time, what to record after a run and how long to wait next); admission, timeouts, panic recovery, classification and the lifecycle log lines are shared.

### Run admission

Admitting a run is one step, in this order:

1. the policy yields the activation time (side-effect free);
2. the worker's context is checked — if it is done, the loop exits (`stopped`, unless already `failed`) and **no new `Run` is invoked**;
3. one commit records "a run started": status `running`, `LastStartedAt` = now, and for a continuous restart `Restarts++`;
4. `Run` is called with the run context.

This commit is the only writer of `StatusRunning`. When step 2 observes cancellation, neither `LastStartedAt` nor `Restarts` changes: before the first run `LastStartedAt` stays zero; before a restart it keeps the previous run's time. Cancellation after the check can still occur before `Run` starts; the worker handles it through its context. The check and the call are not atomic with cancellation, and no further admission protocol exists.

### Run outcome

After `Run` returns, the outcome is classified in this order; the first matching row wins:

| # | Condition | Outcome |
| --- | --- | --- |
| 1 | `Run` panicked | failure — never a graceful stop, whatever the panic value wraps |
| 2 | the run context's cause is `ErrRunTimeout` | failure, timed out — whatever `Run` returned, nil included |
| 3 | the worker's context is done and `Run` returned nil or nothing but a context error | graceful stop — no counter or timestamp changes |
| 4 | `Run` returned an error | failure |
| 5 | `Run` returned nil, scheduled worker | success |
| 6 | `Run` returned nil, continuous worker, the worker's context alive | failure, unexpected exit |

`context.Cause` keeps the first cancellation reason: a timeout that fired before shutdown stays a timeout; a shutdown that came first is never turned into a timeout by a later deadline. Rows 1 and 2 are evaluated before row 3, so a panic during shutdown, or a timeout that fired before it, is still recorded as a failure. A non-context error returned during shutdown (a final batch that could not be written) is a failure too, with its own text preserved.

Row 3 accepts an error only when it is a context error and nothing else: every branch of its unwrap/join tree must end in `context.Canceled` or `context.DeadlineExceeded` (on an error that wraps nothing, an `Is` method is honored as `errors.Is` honors it; an error that wraps others is judged by what it wraps, never by its own `Is`). A single wrap chain qualifies — `fmt.Errorf("query: %w", ctx.Err())`, a `*url.Error` around a cancelled dial. A joined error qualifies only when each branch does: `errors.Join(ctx.Err(), flushErr)` and `fmt.Errorf("%w: %w", ErrFlush, ctx.Err())` are failures under row 4, recorded with their full text, because `flushErr` alone would have been one. Joining an error with the cancellation never hides it.

A client that reports a cancelled call as an error of its own, which does not wrap `ctx.Err()`, makes the stop a failure under row 4. The application translates that one error at the call, and only while its context is done — never with a blanket `if ctx.Err() != nil { return nil }`, which would also swallow a real flush error; the worker guide shows the recipe.

What each outcome records:

- **Failure**: `LastError` = the error text (no stack trace); the counter of the kind advances (continuous: see [Continuous workers](#continuous-workers); scheduled: `ConsecutiveFailures++`); a `worker run failed` line is logged.
- **Success** (scheduled only): status `waiting`, `ConsecutiveFailures` = 0, `LastSucceededAt` = completion time, `LastError` cleared.
- **Graceful stop**: only the status changes (`stopped`, unless already `failed`). `LastError` keeps the most recent failure, and `LastSucceededAt` and the counters keep their values.

### Loop exit

The exit is decided after the outcome is recorded: **failed** when this failure ended the worker — it exhausted a positive limit, or restarts are disabled — or when the schedule has no future activation; otherwise **shutdown** when the worker's context is done; otherwise the loop continues. A failure recorded during shutdown keeps its `LastError` and counter and ends in `stopped` — unless it was the one that ended the worker, in which case the worker is `failed`.

Cancellation while waiting (restart backoff or next activation) ends the loop the same way: `stopped`, unless already `failed`, with the last execution's snapshot preserved.

### Continuous workers

- `Run` is called once at start and must stay active until the worker's context is cancelled. The idiomatic ending is `case <-ctx.Done(): return nil` (or return `ctx.Err()`).
- A nil return while the worker's context is alive is outcome 6: the recorded error is `worker: Run returned nil before shutdown; a continuous worker must run until its context is cancelled`, the failure line carries `unexpected_exit=true`, and the restart policy applies. A non-nil error or a panic keeps its own diagnostics and never carries that marker.
- **A restart calls `Run` again on the same value.** A continuous `Run` must therefore be re-enterable — it builds its per-run resources (a subscription, a watcher, a connection) inside `Run` and releases them before returning — or be registered with `Restart{Disabled: true}`.
- After a failure the worker waits (status `backoff`) and runs again. The wait starts at `Restart.MinDelay` and backs off, with jitter, up to `Restart.MaxDelay` as failures repeat ([Restart backoff](#restart-backoff)).
- `Restart.Limit` N, N > 0, allows the first run plus at most N restarts. `Restarts` counts restarts that actually started (it advances in the admission commit, so a shutdown during the backoff does not count one). The worker becomes `failed` when a run fails and `Restarts == N`: with N = 1 it runs twice. N = 0 (the default) means unlimited restarts.
- `Restart{Disabled: true}`: the first failed run — an error, a panic or an early nil return — is terminal, and the worker becomes `failed` without a restart, during shutdown too. The worker is still permanent: `Disabled` removes in-process retries and nothing else. What follows a terminal failure is the application's ([Health Integration](#health-integration)).
- A continuous worker never records a success: `LastSucceededAt` is always zero, and `ConsecutiveFailures` stays zero.

Finite background work does not fit a continuous worker's contract on its own. Run it in a component's `Start` or an `OnStart` hook when startup should wait for it, or end the continuous `Run` with `<-ctx.Done()` after the work is done.

### Restart backoff

A continuous worker restarts after a capped, jittered exponential delay. The rationale is in [ADR-023](../adr/023-worker-system.md#restart-backoff).

**Floor and cap.** `base` is the resolved `Restart.MinDelay`: `DefaultMinRestartDelay` (3 s) when zero, so an immediately failing worker is throttled rather than busy-looping. It is the first and the minimum delay. The cap is the resolved `Restart.MaxDelay`: `max(DefaultMaxRestartDelay, base)` when zero; a positive value is used as given and must not be below `base` ([Configuration](#configuration)).

**Delay.** After a failed run for which a restart is planned:

```text
ceiling = min(base × 2^(k−1), cap)
lower   = max(base, ceiling/2)
delay   = uniform in [lower, ceiling]; exactly lower when lower == ceiling
```

`k` is the failure's position in the current sequence, starting at 1. The first delay is exactly `base`; every delay lies in `[base, cap]`; a cap equal to `base` is a fixed delay; the window saturates at the cap without overflow, however long the sequence. Individual delays need not grow monotonically.

**Reset.** When the run that just failed lasted at least the cap (`duration >= cap`), the sequence restarts: that failure counts as `k = 1` and the next restart waits `base`. A reset never changes `Restarts` or the `Limit` budget.

**Limits.** The backoff spaces restarts; it does not count them. `Limit` N allows the first run plus N restarts, and `Restarts` counts restarts that started, so the time to `failed` is the sum of the delays: with the default floor and cap, `Limit: 5` reaches `failed` after roughly 48–93 s, and each further restart adds 30–60 s. Cancellation during the wait ends the loop without counting a restart. Scheduled workers do not back off: their schedule is the cadence.

**Fixed delay.** `MinDelay` and `MaxDelay` set to the same positive value give a fixed delay; so does a `MinDelay` of a minute or more alone, since a zero `MaxDelay` resolves to it.

**Announcement.** The `worker run failed` line carries `next_restart_in` only when a restart is planned ([Logging Contract](#logging-contract)).

### Scheduled workers

Each scheduled worker is one goroutine running a serial loop: wait for the next activation, run it synchronously, compute the following one.

- **No overlap by construction.** Activations whose time passes while a run is in flight are skipped when the loop resumes and reported by one `worker activations skipped` line per resumption; they are never queued.
- **Anchored grid.** The next activation is computed from the last intended activation, not from the completion time, so a long run delays the next fire without shifting an `@every` grid.
- **Run on start.** `RunOnStart` adds one run as soon as the worker starts, before the first computed activation; `RunInfo.ScheduledAt` is the zero time for it. The first computed activation is then based on the time that run finished.
- **Failure limit.** `MaxConsecutiveFailures` N, N > 0: the worker becomes `failed` after N consecutive failed runs. N = 0 (the default) means unlimited. A success resets the streak.
- **No future activation.** If the schedule yields no activation, the worker becomes `failed` with `LastError` = `schedule has no future activation`.
- A scheduled `Run` that returns nil while the worker is stopping is a graceful stop (row 3), not a success: `LastSucceededAt` and `ConsecutiveFailures` keep their previous values.
- **Every replica runs every schedule.** The scheduler is per process: three replicas run a cleanup three times. A scheduled job is idempotent or guarded by an application-level lock (for SQL, an advisory lock or a claim row).

### Run timeout

`ScheduledConfig.RunTimeout` bounds every run, including the `RunOnStart` run. Zero (the default) means no timeout.

- The run context is derived with `context.WithTimeoutCause(ctx, d, ErrRunTimeout)`. Inside `Run`, `errors.Is(context.Cause(ctx), worker.ErrRunTimeout)` tells the budget running out from the worker being stopped.
- A run whose context was cancelled by the timeout is a failure (row 2) whatever `Run` returned. The recorded error wraps `ErrRunTimeout`: `worker: run timed out after 15s` when `Run` returned nil, `worker: run timed out after 15s: <returned error>` otherwise. The failure line carries `timed_out=true`, and the failure counts toward `MaxConsecutiveFailures`.
- "Timed out" states that the run exceeded its budget; it does not claim that side effects were rolled back. A run that completes just after the deadline is recorded as a failure — the conservative side of the boundary.
- The timeout is cooperative. It cancels the context and never abandons or kills the goroutine; a `Run` that ignores its context keeps the loop occupied, and activations that pass meanwhile are skipped. At most one run per worker is ever active.
- There is no supervisor-level default; budgets are per worker.

### Panics

Each run is protected by panic recovery. A recovered panic becomes an internal error whose text is `worker: run panicked: <value>`; the stack is kept separately and logged as the `stack` attribute of the failure line. The panic error does not unwrap to its value, so a panic with a context error as its value is never mistaken for a graceful stop. `Info.LastError` — and therefore the readiness failure text — never contains a stack trace; like any error text it is recorded verbatim and may contain newlines if the value's text does.

### Run context

The context passed to `Run` is derived from the worker's context (cancelled when its component is shut down) and, with `RunTimeout`, bounded by the run timeout. It carries execution metadata only — services, loggers and configuration come from constructors. `CurrentRun(ctx)` returns it as one value:

| `RunInfo` field | Value |
| --- | --- |
| `Worker` | the registration name |
| `ID` | a fresh identifier per run (`crypto/rand.Text`), equal to the `run_id` of the framework's lines for that run |
| `ScheduledAt` | the intended activation time; zero for continuous workers and for the `RunOnStart` run |

`CurrentRun` returns `false` and a zero `RunInfo` for a context that is not a run context.

---

## Logging Contract

For every worker whose component started, the loop writes exactly one `worker started` and exactly one `worker stopped` line at Info, whatever path ends it; `worker started` is the worker's first line and `worker stopped` its last, so a schedule with no activation at start writes its `worker failed` between them. A worker whose component never started — the start phase failed or was interrupted before it — writes neither. All lines carry `worker=<name>`, and the supervisor's logger adds `module=worker`.

| Message | Level | Attributes |
| --- | --- | --- |
| `worker started` | Info | `worker`, `kind`; scheduled adds `schedule` and either `run_on_start=true` or `next_run` |
| `worker run failed` | Error | `worker`, `kind`, `error`, `run_id`, `duration`; continuous adds `restarts`, `next_restart_in` when a restart is planned and `unexpected_exit=true` for outcome 6; scheduled adds `scheduled_at`, `consecutive_failures` and `timed_out=true` for outcome 2; `stack` for a panic |
| `worker run completed` | Debug | `worker`, `kind`, `run_id`, `scheduled_at`, `duration` (scheduled only) |
| `worker activations skipped` | Warn | `worker`, `skipped`, `first_scheduled_at`, `last_scheduled_at` |
| `worker failed` | Error | `worker`, `kind`, `reason`; `limit` for `restart_limit` and `failure_limit`; `schedule` for `schedule_exhausted` |
| `worker stopped` | Info | `worker`, `kind`, `status`, `reason` (`shutdown` or `failed`) |

`worker failed` is the single terminal line, written once when the worker becomes `failed`:

| `reason` | When |
| --- | --- |
| `restart_limit` | a continuous run failed with `Restarts == Restart.Limit` |
| `restart_disabled` | a continuous run failed under `Restart{Disabled: true}` |
| `failure_limit` | a scheduled worker reached `MaxConsecutiveFailures` |
| `schedule_exhausted` | the schedule has no future activation |

- `next_restart_in` is the delay the [backoff](#restart-backoff) selected for the next restart. The restart decision is made before the line is written, so the attribute is omitted when the failure ended the worker and when shutdown was already observed; it is never written as zero to mean "no restart". Each failed run writes exactly one `worker run failed` line, followed by `worker failed` when it ended the worker. A cancellation that arrives after the line was written still ends the loop, so the attribute is a plan, not a promise.
- `reason=failed` on the stop line stays at Info: the preceding `worker failed` line is the alerting signal, one rule for every kind; the stop line is lifecycle bookkeeping.
- A graceful stop writes no line of its own besides `worker stopped`.
- Successful scheduled runs are logged at Debug only. Liveness is answered by `Snapshot()`, `Lookup()` and readiness; logs are the audit trail.

---

## Snapshot

### Info

```go
type Info struct {
	Name                string            `json:"name"`
	Kind                Kind              `json:"kind"`
	Schedule            string            `json:"schedule,omitzero"`
	Continuous          *ContinuousConfig `json:"continuous,omitzero"`
	Scheduled           *ScheduledConfig  `json:"scheduled,omitzero"`
	Status              Status            `json:"status"`
	Restarts            int64             `json:"restarts"`
	ConsecutiveFailures int64             `json:"consecutive_failures"`
	LastStartedAt       time.Time         `json:"last_started_at,omitzero"`
	LastSucceededAt     time.Time         `json:"last_succeeded_at,omitzero"`
	LastError           string            `json:"last_error,omitzero"`
}
```

`Snapshot()` returns one `Info` per registered worker, in registration order. `Lookup(name)` returns the worker registered under exactly that name, or `false`; a name it does not know is the caller's error to report, never a pass.

- Exactly one of `Continuous` and `Scheduled` is set, holding the **resolved** configuration the runner executes, not the arguments as passed: `Tier` is the resolved tier, `Restart.MinDelay` and `Restart.MaxDelay` the resolved floor and cap of the [restart backoff](#restart-backoff) (zero when `Disabled`). `Schedule` is the expression as registered, which is also the effective schedule because `ParseSchedule` rejects every input it would otherwise have to rewrite; it is empty for a continuous worker.
- Every snapshot carries its own copy of the configuration: mutating `info.Continuous` changes neither the supervisor nor the next snapshot. The configuration types hold only scalars, so the copy is deep.
- `Restarts` (continuous) counts restarts that started; `ConsecutiveFailures` (scheduled) counts failed runs since the last success. The other kind's counter is zero.
- `LastStartedAt` is the start time of the most recently admitted run. `LastSucceededAt` is the completion time of the last successful scheduled run. `LastError` is the error of the most recent failed run; a successful scheduled run clears it, a graceful stop does not.
- `LastError` is a string, not an `error`: a snapshot is for display and serialization.

### JSON shape

`Info` is shaped for direct encoding from an admin endpoint. Field names are snake_case, the nested configuration included; `schedule`, `continuous`, `scheduled`, `last_started_at`, `last_succeeded_at` and `last_error` are omitted while zero; every other field is always present, every configuration field included, so dashboards keep one schema; durations encode as integer nanoseconds under the framework's JSON response profile (ADR-021); `tier` echoes the resolved `credo.Tier` as the root encodes it (`ingress` or `internal`). A pending continuous worker and a scheduled worker after one failed run:

```json
[
  {
    "name": "consumer",
    "kind": "continuous",
    "continuous": {
      "tier": "internal",
      "restart": { "disabled": false, "limit": 5, "min_delay": 3000000000, "max_delay": 60000000000 },
      "unready_when_failed": true
    },
    "status": "pending",
    "restarts": 0,
    "consecutive_failures": 0
  },
  {
    "name": "report",
    "kind": "scheduled",
    "schedule": "@every 1m",
    "scheduled": {
      "tier": "ingress",
      "run_on_start": false,
      "run_timeout": 15000000000,
      "max_consecutive_failures": 3,
      "unready_when_failed": false,
      "unready_until_first_success": false,
      "unready_after_success_age": 0
    },
    "status": "waiting",
    "restarts": 0,
    "consecutive_failures": 1,
    "last_started_at": "2000-01-01T00:01:00Z",
    "last_error": "boom"
  }
]
```

### Status

| Status | Meaning |
| --- | --- |
| `pending` | registered; its component has not started, or a continuous worker has not yet been admitted to its first run |
| `running` | a run has been admitted and is executing |
| `backoff` | continuous only: waiting to restart after a failure — unhealthy, recovering |
| `waiting` | scheduled only: waiting for the next activation, whatever the last run did |
| `stopped` | the loop ended because the worker's component was shut down; the worker will not run again |
| `failed` | a positive failure limit was exhausted, restarts are disabled and a run failed, or the schedule has no future activation; permanent until the application restarts |

`backoff` is the one status that means "currently unhealthy but recovering". A scheduled worker has no such status: between activations it is `waiting`, and its signal is `consecutive_failures > 0`. A worker whose component was never started — before the start phase reaches it, or after a start that failed or was interrupted before it — stays `pending`. `failed` is permanent: there is no paused or suppressed state. Applications that want indefinite recovery leave the limits at zero and handle degradation in their own logic.

---

## Health Integration

Readiness participation is opt-in per condition; a failed metrics reporter must not take an instance out of rotation.

```go
workers.Scheduled("recovery", "@every 5m", recovery, worker.ScheduledConfig{
	RunOnStart:               true,
	UnreadyUntilFirstSuccess: true,             // unready until the first run succeeds
	UnreadyWhenFailed:        true,             // unready once the worker is failed
	UnreadyAfterSuccessAge:   15 * time.Minute, // unready when the last success is older
})
```

- `UnreadyWhenFailed` applies to both kinds; `UnreadyUntilFirstSuccess` and `UnreadyAfterSuccessAge` exist only on `ScheduledConfig`, because a continuous worker never succeeds. A worker with no condition set has no `Ready` and contributes nothing to readiness.
- `UnreadyUntilFirstSuccess` stays satisfied once met; pair it with `RunOnStart` unless waiting for the first activation is intended. `UnreadyAfterSuccessAge` is not applied before the first success; combine it with `UnreadyUntilFirstSuccess` to close that window.
- `UnreadyWhenFailed` sees every way a continuous worker can die: an early nil return is a failure like an error or a panic, so under a positive `Restart.Limit`, or with `Restart.Disabled`, a worker that keeps exiting reaches `failed`. With unlimited restarts it never does.
- The contribution is the worker component's `Ready`, reported by `/ready` under the component name `worker:<name>` next to `AddReadinessCheck` entries (a name collision fails closed as a configuration error). It is evaluated in memory from the runner's last snapshot, resolves nothing per request and never performs I/O; failure text is masked unless `HealthConfig.ExposeErrors` is set. `/ready` asks components only once the App has entered running, when every worker's component has started; called earlier, `Ready` of a worker with `UnreadyUntilFirstSuccess` reports "has not started", and any other condition passes.

Readiness takes an instance out of rotation and restarts nothing: a worker that reached `failed` under `UnreadyWhenFailed` leaves a process that is alive, unready and never restarted. An application that wants the orchestrator to restart the process ties a liveness check to the worker through `Lookup` — `AddLivenessCheck` with a check that fails when `Lookup` reports `StatusFailed` or does not know the name — knowing that a failure caused by a shared dependency then restarts every replica. The worker guide carries the recipe; no liveness setting exists on the configuration.

---

## Schedules

Cron expressions are compiled into a `Schedule` at registration; no raw string is parsed at run time. The parser is adapted from robfig/cron v3 (MIT) and trimmed to the default Unix cron surface; only parsing and next-fire calculation are taken, not the scheduler.

| Format | Example | Description |
| --- | --- | --- |
| Standard (5-field) | `0 */6 * * *` | minute hour day-of-month month day-of-week |
| Predefined | `@hourly` | every hour at minute 0 |
| Predefined | `@daily` / `@midnight` | every day at 00:00 |
| Predefined | `@weekly` | every Sunday at 00:00 |
| Predefined | `@monthly` | the first of the month at 00:00 |
| Interval | `@every 5m`, `@every 1h30m` | fixed period |

- Field syntax: lists (`1,15`), ranges (`1-5`), steps (`*/10`, `8-18/2`), month and weekday names (`jan`, `sat`), `?` as an alias for `*`, `7` as Sunday.
- Cron schedules are evaluated in the process's local time zone (`time.Local`) and fire at second 0 of the matching minute. There is no per-worker time zone selection; `TZ=`/`CRON_TZ=` prefixes are rejected (below).
- As in crontab(5), when both day-of-month and day-of-week are restricted (neither is `*`), the schedule fires when **either** matches; a step on `*` counts as restricted.
- `@every` takes a Go duration that must be positive and a whole number of seconds: `@every 0s`, `@every -1h` and `@every 1500ms` are rejected (`@every duration must be positive, got …`, `@every duration must be a whole number of seconds, got 1.5s`). No input is rounded or clamped.
- Not supported, each with a targeted error: the 6-field seconds form (use `@every` for sub-minute periods), `@yearly`/`@annually` (use `0 0 1 1 *`), and `TZ=`/`CRON_TZ=` prefixes.
- `Scheduled` and `ScheduledProvided` panic with the parse error; `ParseSchedule` returns it, for an expression that comes from configuration and should be validated before registration.

---

## Usage Examples

### Example 1: Queue consumer (continuous, provided by DI)

```go
type OrderConsumer struct {
	log    *slog.Logger
	queue  *Queue
	orders *OrderService
}

func NewOrderConsumer(infra credo.Infra, q *Queue, svc *OrderService) *OrderConsumer {
	return &OrderConsumer{log: infra.Logger, queue: q, orders: svc}
}

func (w *OrderConsumer) Run(ctx context.Context) error {
	for {
		msg, err := w.queue.Receive(ctx) // blocks until a message arrives or ctx is done
		if err != nil {
			return err // a cancelled Receive wraps ctx.Err(): a graceful stop at shutdown, otherwise a restart
		}
		if err := w.orders.Process(ctx, msg); err != nil {
			// One bad message must not stop the consumer.
			run, _ := worker.CurrentRun(ctx)
			w.log.ErrorContext(ctx, "process order failed",
				"error", err, "msg_id", msg.ID, "run_id", run.ID)
		}
	}
}

func main() {
	app, err := credo.New()
	if err != nil {
		log.Fatal(err)
	}
	app.Provide[*Queue](NewQueue)
	app.Provide[*OrderService](NewOrderService)
	app.Provide[*OrderConsumer](NewOrderConsumer)

	// Internal tier: stops after the HTTP drain, before the queue and the service.
	// Restarts after 5s, backing off up to the cap (1m by default) while failures repeat.
	workers := worker.Use(app)
	workers.ContinuousProvided[*OrderConsumer]("order-consumer", worker.ContinuousConfig{
		Restart: worker.Restart{MinDelay: 5 * time.Second},
	})

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
```

### Example 2: Scheduled cleanup (provided by DI)

```go
type SessionCleanup struct {
	log *slog.Logger
	db  *sqldb.DB
}

func NewSessionCleanup(infra credo.Infra, db *sqldb.DB) *SessionCleanup {
	return &SessionCleanup{log: infra.Logger, db: db}
}

func (w *SessionCleanup) Run(ctx context.Context) error {
	result, err := w.db.NewDelete().
		Model((*Session)(nil)).
		Where("expired_at < ?", time.Now()).
		Exec(ctx) // idempotent: every replica runs it
	if err != nil {
		return err
	}
	run, _ := worker.CurrentRun(ctx)
	w.log.InfoContext(ctx, "expired sessions cleaned",
		"count", result.RowsAffected(), "run_id", run.ID)
	return nil
}

func registerCleanup(app *credo.App, workers *worker.Supervisor) {
	app.Provide[*SessionCleanup](NewSessionCleanup)

	// Every 6 hours plus once at startup; each run gets 10 minutes;
	// failed after 3 consecutive failures. Ingress tier: no run starts during the drain.
	workers.ScheduledProvided[*SessionCleanup]("session-cleanup", "0 */6 * * *", worker.ScheduledConfig{
		RunOnStart:             true,
		RunTimeout:             10 * time.Minute,
		MaxConsecutiveFailures: 3,
	})
}
```

### Example 3: Inline functions

```go
workers.Continuous("heartbeat", worker.Func(func(ctx context.Context) error {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			run, _ := worker.CurrentRun(ctx)
			slog.InfoContext(ctx, "heartbeat", "worker", run.Worker)
		}
	}
}))

// Skips an activation if the previous report is still running.
workers.Scheduled("metrics-report", "@every 1m", worker.Func(reportMetrics))
```

Both are registered by value: they have no edges, and each takes its place in its tier by registration order.

### Example 4: File watcher (ingress, limited restarts)

```go
type ConfigWatcher struct {
	path     string
	onChange func(name string)
}

// Run is re-enterable: the watcher is built inside Run and closed before it returns.
func (w *ConfigWatcher) Run(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()
	if err := watcher.Add(w.path); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-watcher.Events:
			if ev.Op&fsnotify.Write != 0 {
				w.onChange(ev.Name)
			}
		case err := <-watcher.Errors:
			return err // restarted, at most 5 times
		}
	}
}

workers.Continuous("config-watcher", &ConfigWatcher{path: "./configs", onChange: reloadFile},
	worker.ContinuousConfig{
		Tier:              credo.TierIngress, // work enters the process here: stop with the listener
		Restart:           worker.Restart{Limit: 5, MinDelay: 10 * time.Second},
		UnreadyWhenFailed: true,
	})
```

---

## Package Structure

`worker/` holds `worker.go` (`Worker`, `Func`, the run context), `supervisor.go` (`Supervisor`, `Use`, the four registration methods, `Snapshot`, `Lookup`, name validation and the per-App record of provided types), `config.go` (`ContinuousConfig`, `ScheduledConfig`, `Restart`, the defaults, `ErrRunTimeout`, resolution and validation), `definition.go` (the immutable registration and the single `Info` builder), `component.go` (the worker component and the readiness conditions its `Ready` evaluates), `runner.go` (runner state, run admission, the loop, the continuous and scheduled policies, the log lines), `outcome.go` (panic recovery, run-outcome classification), `schedule.go` (`Schedule`, `ParseSchedule`, adapted from robfig/cron v3), `info.go` (`Kind`, `Status`, `Info`) and `doc.go` (package doc and robfig/cron attribution). Tests use synctest timing and a capturing slog handler.

`worker/` imports the root package (like `store/`); the root package does not import `worker/`. The supervisor adds each worker's component — a value, or a constructor over `T` for the provided forms — through the module-internal kernel seam (`internal/kernel`), which does what `App.Manage` with `credo.Named` and `credo.Ingress()` does and also carries the worker's own remedy into `Finalize`'s tier finding; it logs through `App.Logger` and binds nothing into the container.

---

## Test Strategy

- **Virtual time.** Every timing test runs in a `testing/synctest` bubble; the runner calls the `time` package directly and there is no clock seam. `synctest.Wait` does not advance the clock — sleep past a deadline to fire timers.
- **Configuration tables.** Each field's resolution and each rejected value is a table entry for both configuration types: zero → default per kind (the tier included), the `Restart` resolution (`MinDelay` first, a zero `MaxDelay` raised to it, a positive `MaxDelay` below it panicking with both values named, `Disabled` beside any other field panicking), more than one configuration panicking. The snapshot is asserted for each, without running a worker.
- **Registration misuse.** Every row of [Validation](#validation) panics with a message naming the worker, the call and the remedy, registration after `Finalize` and a duplicate `worker:<name>` across two supervisors included; a provided worker whose `T` has no binding fails `Finalize` with the path.
- **Classification as a table.** The outcome classifier is a pure function; each row of the outcome table and each ordering edge (timeout then shutdown, shutdown then deadline, panic with a context-error value during shutdown) is a table entry.
- **Admission without a production seam.** An in-package test policy blocks in its activation step, the test cancels and releases it, and the admission check must observe the cancellation: zero `Run` calls and a zero `LastStartedAt` before the first run; unchanged invocation count, `LastStartedAt` and `Restarts` before a restart.
- **Logs through a capturing handler.** One `slog.Handler` records level, message and attributes; each event of the [vocabulary](#logging-contract) is asserted — one `started` and one `stopped` per exit path, `worker failed` once with each `reason`, `run_id` equal to `CurrentRun(ctx).ID`, the skip collapse.
- **Timing against the lifecycle.** A continuous worker's context is cancelled in its tier's turn: after the HTTP drain for the internal default — a handler enqueues to an in-process continuous worker that writes through a database component, a request accepted just before shutdown completes, its job is written, and the database shuts down after the worker. A scheduled worker stops concurrently with the HTTP drain. A `Run` that ignores cancellation past the deadline keeps its dependencies open, and `Run` returns an error naming `worker:<name>` as abandoned.
- **Provided workers in the start walk.** `T` is built after its component dependencies have started and the worker stops before them; a constructor error fails the start and rolls back what was built; a provided worker of the internal tier whose `T` depends on an ingress component fails `Finalize` with the path.
- **Wire shape.** A JSON golden encodes `Snapshot()` through the framework's response profile, for each kind and status.
- **Schedules.** Pinned parse and `Next` behavior, rejected syntax, the `@every` rejection matrix, DST and end-of-month edges.
- Worker commits run `go test ./worker/... -race -count=20`.
