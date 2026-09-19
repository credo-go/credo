# Worker Spec

**Status**: Implemented (v0.20.0 contract) **Package**: `worker/` **Sources**: robfig/cron v3 (MIT, cron expression parser only) **ADR**: [ADR-023](../adr/023-worker-system.md) **Guide**: [Worker Guide](../guides/worker.md)

This file is the contract of Credo's worker system: what registration accepts, how a run is admitted and classified, what the pool reports, and which log lines it writes. The rationale and the rejected alternatives live in ADR-023.

---

## Overview

The `worker/` package runs background tasks — queue consumers, watchers, periodic cleanups, reports — inside a Credo application. Continuous and scheduled work share one interface; the kind is chosen at registration.

- **One interface** — `Worker` has a single method, `Run(ctx) error`. The name is given at registration and is the worker's identity.
- **Two registration forms** — `Register(app, name, w, opts...)` for a constructed value, `RegisterProvided[T](app, name, opts...)` for a worker the DI container provides; T is resolved when the pool starts.
- **Three layers** — an internal definition (immutable registration record) → a runner (mutable runtime state) → `Info` (read-only snapshot carrying the effective `Config` and the live state).
- **Fail-fast registration** — names, options, cron expressions and readiness policies are validated when `Register` is called; nothing is parsed at run time.
- **Lifecycle integrated** — the pool starts in an `OnStart` hook (after the port is bound, before traffic) and drains in the `OnDrain` phase, before any DI singleton is torn down.
- **Run outcomes are classified in one place** — panics, timeouts, graceful stops, errors, successes and early continuous exits follow one ordered table, and the loop exit is decided only after the outcome is recorded.
- **Continuous workers are permanent** — `Run` must stay active until shutdown; an early nil return is a failure and restarts.
- **Scheduled workers never overlap** — one goroutine per worker runs activations serially; activations that pass during a run are skipped and logged.
- **Cooperative timeouts** — `WithRunTimeout` cancels a scheduled run's context; a timed-out run is a failure whatever `Run` returns.
- **Observable** — exactly one start and one stop line per worker, run lines correlated by `run_id`, `Pool.Workers()` snapshots with snake_case JSON, opt-in readiness.

---

## Goals

1. One abstraction for continuous and scheduled background work; cron is a scheduling strategy, not a separate system.
2. Constructor-injected workers work with the framework's bootstrap order (register → `Finalize` → resolve).
3. Registration is verifiable: a test can read the effective policy of every registered worker without running it.
4. The lifecycle is legible from default-level logs and from readiness; no worker can end without reaching a reported state.
5. Every timing path is deterministically testable with `testing/synctest`, without a clock seam.

## Non-Goals

- Job queues, task distribution, persistent job state (use a dedicated system).
- Cluster-aware scheduling; the scheduler is per process.
- On-demand triggering of a scheduled worker, or a synchronous first run as a startup gate (readiness `RequireFirstSuccess` covers the rotation side).
- Overlap policies other than skip; preemptive timeouts that abandon a goroutine; per-worker cancellation.
- Restart strategies other than permanent for continuous workers.
- Automatic readiness binding: participation is opt-in per worker through `WithReadiness`.
- A worker test package: a worker is tested by calling `w.Run(t.Context())`.
- Metrics and tracing hooks (Phase 3.5; they will reuse the `run_id` and `duration` log attributes).

---

## Three-Layer Model

```
Register / RegisterProvided            Pool.Start                    Pool.Workers
          │                                 │                              │
          ▼                                 ▼                              ▼
  ┌──────────────────┐   resolve+publish  ┌──────────────────┐   snapshot   ┌──────────────────────┐
  │ definition       │ ─────────────────► │ runner           │ ───────────► │ Info                 │
  │ (internal,       │                    │ (internal,       │              │ Name, Kind           │
  │  immutable)      │                    │  mutable, mu)    │              │ Config  ← definition │
  └──────────────────┘                    └──────────────────┘              │ Status, Restarts,    │
  • name                                  • resolved Worker                 │ ConsecutiveFailures, │
  • resolver + source label               • status, restarts,               │ LastRun, LastSuccess,│
  • compiled schedule                     •   consecutiveFailures,          │ LastError ← runner   │
  • restart / failure policy              •   lastRun, lastSuccess,         └──────────────────────┘
  • startImmediately, runTimeout          •   lastError
  • readiness policy (copy)
```

Configuration and live state never share a mutable struct. The definition has one projection, `Config`, and one `Info` builder; the pre-start answer of `Pool.Workers()` and every runner snapshot go through that builder, so a field added to `Info` cannot be missing from one of them.

---

## Public API

Signatures only; the godoc in `worker/` is authoritative for wording.

```go
// Worker and adapter
type Worker interface{ Run(ctx context.Context) error }
type Func func(ctx context.Context) error // Run calls f(ctx)

// Registration
func Register(app *credo.App, name string, w Worker, opts ...Option) error
func MustRegister(app *credo.App, name string, w Worker, opts ...Option)
func RegisterProvided[T Worker](app *credo.App, name string, opts ...Option) error
func MustRegisterProvided[T Worker](app *credo.App, name string, opts ...Option)

// Options
func WithSchedule(expr string) Option
func WithStartImmediately() Option                 // scheduled
func WithMaxConsecutiveFailures(n int) Option      // scheduled
func WithRunTimeout(d time.Duration) Option        // scheduled
func WithMaxRestarts(n int) Option                 // continuous
func WithRestartDelay(d time.Duration) Option      // continuous
func WithReadiness(policy ReadinessPolicy) Option  // both kinds (conditions are kind-checked)

const DefaultRestartDelay = 3 * time.Second
var ErrRunTimeout = errors.New("worker: run timed out")

// Run context
func RunID(ctx context.Context) string
func WorkerName(ctx context.Context) string
func ScheduledAt(ctx context.Context) time.Time

// Pool (published in DI as *worker.Pool)
func (p *Pool) Start(ctx context.Context) error
func (p *Pool) Shutdown(ctx context.Context) error
func (p *Pool) Workers() []Info

// Snapshot types
type Kind string     // KindContinuous, KindScheduled
type Status string   // StatusIdle, StatusRunning, StatusWaiting, StatusStopped, StatusFailed
type Config struct{ ... }
type Info struct{ ... }
type ReadinessPolicy struct{ RequireFirstSuccess, FailWhenFailed bool; MaxSuccessAge time.Duration }

// Schedules
func ParseSchedule(expr string) (*Schedule, error)
func (s *Schedule) Next(now time.Time) time.Time
func (s *Schedule) String() string
```

`Pool.Start` and `Pool.Shutdown` are called by the lifecycle hooks the pool installs; applications call `Workers()`.

---

## Registration

### Names

The name is the registration identity. It is the `worker` attribute of every log line, the `Name` of the snapshot, the suffix of the readiness check `worker:<name>` and the value `WorkerName(ctx)` returns inside `Run`. Rules, applied to both forms and independent of `WithReadiness`:

- non-empty;
- no leading or trailing whitespace (names are never trimmed or otherwise normalized);
- no control characters;
- unique within the pool (`worker: duplicate worker name "x"`), across `Register` and `RegisterProvided`.

No prefix is reserved. The health engine's reserved `credo.` prefix applies to the full readiness check name, which always starts with `worker:`.

Registering the same instance under two names runs it on two independent loops; the worker must then be safe for concurrent `Run` calls.

### Validation

`Register` and `RegisterProvided` share one path. In order, registration fails with an error when:

1. `app` is nil, or (for `Register`) the worker is nil — including a nil `Func` and a typed-nil pointer;
2. the name breaks a rule above;
3. an option value is out of range: negative `WithMaxRestarts`, `WithRestartDelay`, `WithRunTimeout` or `WithMaxConsecutiveFailures`;
4. the readiness policy is invalid (see [Health Integration](#health-integration));
5. the schedule does not parse (see [Schedules](#schedules));
6. an option belongs to the other kind:

| Kind | Rejected option | Error |
| --- | --- | --- |
| scheduled | `WithMaxRestarts` | `worker: WithMaxRestarts is for continuous workers; use WithMaxConsecutiveFailures` |
| scheduled | `WithRestartDelay` | `worker: WithRestartDelay is for continuous workers` |
| continuous | `WithMaxConsecutiveFailures` | `worker: WithMaxConsecutiveFailures is for scheduled workers; use WithMaxRestarts` |
| continuous | `WithStartImmediately` | `worker: WithStartImmediately is for scheduled workers` |
| continuous | `WithRunTimeout` | `worker: WithRunTimeout is for scheduled workers` |

7. with `WithReadiness`, the check name `worker:<name>` is not a valid health check name;
8. the registration window is closed, the pool cannot be adopted, or — for the registration that creates the pool — the `worker` config section is invalid (below);
9. the name is a duplicate, or the pool has already started.

`MustRegister` and `MustRegisterProvided` panic with the same error.

### Registration window and the pool

Every registration call is rejected after `app.Finalize()` (`worker: Register after app.Finalize: …`), including calls made after the pool already exists. The first successful registration creates the pool:

- the optional `worker` config section is read through `App.ConfigExists` / `App.GetConfig` (registration runs before `Finalize`, when `Resolve` is unavailable);
- the pool is published as a protected DI binding (`ProvideProtectedValue[*Pool]`), so `Replace[*Pool]` is rejected; later registrations adopt it through `AdoptValue`, and a `*Pool` registered by any other means is refused without running a constructor;
- the readiness seam (`internal/health.ReadinessFunc`) is installed, so registration and `UseHealth` may happen in either order;
- `app.OnStart(pool.Start)` and `app.OnDrain(pool.Shutdown)` are installed. `Pool` also implements `credo.Shutdowner`; the container's later pass finds the stop sequence already complete.

The pool logs through `app.Logger()` with `module=worker`.

### Provided workers

`RegisterProvided[T]` stores a resolver instead of an instance; `T` is resolved with `app.Resolve[T]()` when the pool starts — after the implicit `Finalize`, before the server accepts traffic. Consequences:

- `RegisterProvided[T]` and `app.Provide[T]` may be called in either order.
- The constraint `T Worker` makes a type without `Run` a compile error. `T` may be an interface bound with `app.Alias`.
- "T is not provided", a constructor error or panic, and a nil result are reported at start, not at registration; T's own constructor graph is validated by `Finalize` like any provider.
- If T implements `credo.Shutdowner`, the container closes it after the pool has drained; no extra graph edge is needed.

Resolving in `OnStart` is within the DI rule, which forbids resolution only in the three shutdown hooks (`OnPreDrain`, `OnDrain`, `OnShutdown`).

---

## Start and Shutdown

### Start protocol

`Pool.Start` runs user constructors outside the pool lock and is all-or-nothing:

1. **Claim, under the lock.** Refuse when shutdown has begun (`worker: pool already shut down`) or the pool was already claimed (`worker: pool already started`). Mark the pool claimed — further registrations are refused — and snapshot the definitions.
2. **Resolve, outside the lock.** Resolve every definition. A nil result is an error (`resolved to nil`). Each failure is wrapped as `worker: "<name>": resolve <type>: <cause>`, and all failures are joined.
3. **Publish, under the lock.** If any resolution failed, return the joined error and launch nothing. If a `Shutdown` arrived during step 2, return `worker: pool already shut down` and launch nothing. Otherwise create the runners — scheduled runners are published as `waiting`, continuous runners stay `idle` until their first run is admitted — and launch one goroutine per worker under the lock.

A failed `Start` fails the application's `OnStart` phase, so startup fails before the server accepts traffic and the App runs its normal teardown. Until runners are published — before `Start`, and after a failed or pre-empted `Start` — `Workers()` reports every definition with status `idle`.

### Shutdown

The first `Shutdown` marks the pool stopping, cancels the pool context and starts waiting for every worker goroutine. Every call — concurrent, repeated, from the `OnDrain` hook or from the container's `Shutdowner` pass — returns nil once all goroutines have returned (completion takes precedence over an already-ended `ctx`) and `ctx.Err()` only while workers are still running when `ctx` ends. Because stopping is set under the same lock `Start` publishes under, a goroutine can never join a wait that has already begun. A pool that never started shuts down immediately.

---

## Execution

Each worker runs on one goroutine driven by a single loop. A policy supplies the kind-specific parts (the first wait, the activation time, what to record after a run and how long to wait next); admission, timeouts, panic recovery, classification and the lifecycle log lines are shared.

### Run admission

Admitting a run is one step, in this order:

1. the policy yields the activation time (side-effect free);
2. the pool context is checked — if it is done, the loop exits (`stopped`, unless already `failed`) and **no new `Run` is invoked**;
3. one commit records "a run started": status `running`, `LastRun` = now, and for a continuous restart `Restarts++`;
4. `Run` is called with the run context.

This commit is the only writer of `StatusRunning`. When step 2 observes cancellation, neither `LastRun` nor `Restarts` changes: before the first run `LastRun` stays zero; before a restart it keeps the previous run's time. Cancellation after the check can still occur before `Run` starts; the worker handles it through its context. The check and the call are not atomic with cancellation, and no further admission protocol exists.

### Run outcome

After `Run` returns, the outcome is classified in this order; the first matching row wins:

| # | Condition | Outcome |
| --- | --- | --- |
| 1 | `Run` panicked | failure — never a graceful stop, whatever the panic value wraps |
| 2 | the run context's cause is `ErrRunTimeout` | failure, timed out — whatever `Run` returned, nil included |
| 3 | the pool context is done and `Run` returned nil or nothing but a context error | graceful stop — no counter or timestamp changes |
| 4 | `Run` returned an error | failure |
| 5 | `Run` returned nil, scheduled worker | success |
| 6 | `Run` returned nil, continuous worker, pool context alive | failure, unexpected exit |

`context.Cause` keeps the first cancellation reason: a timeout that fired before shutdown stays a timeout; a shutdown that came first is never turned into a timeout by a later deadline. Rows 1 and 2 are evaluated before row 3, so a panic during shutdown, or a timeout that fired before it, is still recorded as a failure. A non-context error returned during shutdown (a final batch that could not be written) is a failure too, with its own text preserved.

Row 3 accepts an error only when it is a context error and nothing else: every branch of its unwrap/join tree must end in `context.Canceled` or `context.DeadlineExceeded` (on an error that wraps nothing, an `Is` method is honored as `errors.Is` honors it; an error that wraps others is judged by what it wraps, never by its own `Is`). A single wrap chain qualifies — `fmt.Errorf("query: %w", ctx.Err())`, a `*url.Error` around a cancelled dial. A joined error qualifies only when each branch does: `errors.Join(ctx.Err(), flushErr)` and `fmt.Errorf("%w: %w", ErrFlush, ctx.Err())` are failures under row 4, recorded with their full text, because `flushErr` alone would have been one. Joining an error with the cancellation never hides it.

What each outcome records:

- **Failure**: `LastError` = the error text (no stack trace); the counter of the kind advances (continuous: see [Continuous workers](#continuous-workers); scheduled: `ConsecutiveFailures++`); an Error line is logged.
- **Success** (scheduled only): status `waiting`, `ConsecutiveFailures` = 0, `LastSuccess` = completion time, `LastError` cleared.
- **Graceful stop**: only the status changes (`stopped`, unless already `failed`). `LastError` keeps the most recent failure, and `LastSuccess` and the counters keep their values.

### Loop exit

The exit is decided after the outcome is recorded: **failed** when this failure exhausted a positive limit (or the schedule has no future activation); otherwise **shutdown** when the pool context is done; otherwise the loop continues. A failure recorded during shutdown keeps its `LastError` and counter and ends in `stopped` — unless it was the one that exhausted the limit, in which case the worker is `failed`.

Cancellation while waiting (restart delay or next activation) ends the loop the same way: `stopped`, unless already `failed`, with the last execution's snapshot preserved.

### Continuous workers

- `Run` is called once at start and must stay active until the pool context is cancelled. The idiomatic ending is `case <-ctx.Done(): return nil` (or return `ctx.Err()`).
- A nil return while the pool context is alive is outcome 6: the recorded error is `worker: Run returned nil before shutdown; a continuous worker must run until its context is cancelled`, the failure line carries `unexpected_exit=true`, and the restart policy applies. A non-nil error or a panic keeps its own diagnostics and never carries that marker.
- After a failure the worker waits `RestartDelay` (status `waiting`) and runs again. The delay is resolved at registration: `WithRestartDelay` → the pool's `worker.restart_delay` config → `DefaultRestartDelay`; zero at either level means the default, so an immediately failing worker is throttled rather than busy-looping.
- `WithMaxRestarts(N)`, N > 0, allows the first run plus at most N restarts. `Restarts` counts restarts that actually started (it advances in the admission commit, so a shutdown during the restart delay does not count one). The worker becomes `failed` when a run fails and `Restarts == N`: with N = 1 it runs twice. N = 0 (the default) means unlimited restarts.
- A continuous worker never records a success: `LastSuccess` is always zero.

Finite background work does not fit a continuous worker's contract on its own. Run it in `app.OnStart` when startup should wait for it, or end the continuous `Run` with `<-ctx.Done()` after the work is done.

### Scheduled workers

Each scheduled worker is one goroutine running a serial loop: wait for the next activation, run it synchronously, compute the following one.

- **No overlap by construction.** Activations whose time passes while a run is in flight are skipped when the loop resumes and reported by one `worker ticks skipped` line per resumption; they are never queued.
- **Anchored grid.** The next activation is computed from the last intended activation, not from the completion time, so a long run delays the next fire without shifting an `@every` grid.
- **Start immediately.** `WithStartImmediately` adds one synthetic run before the first computed activation; `ScheduledAt(ctx)` is the zero time for it. The first computed activation is then based on the time that run finished.
- **Failure limit.** `WithMaxConsecutiveFailures(N)`, N > 0: the worker becomes `failed` after N consecutive failed runs. N = 0 (the default) means unlimited. A success resets the streak.
- **No future activation.** If the schedule yields no activation, the worker becomes `failed` with `LastError` = `schedule has no future activation`.
- A scheduled `Run` that returns nil while the pool is shutting down is a graceful stop (row 3), not a success: `LastSuccess` and `ConsecutiveFailures` keep their previous values.

### Run timeout

`WithRunTimeout(d)`, scheduled workers only, bounds every run, including the synthetic startup run. `d = 0` (the default) means no timeout.

- The run context is derived with `context.WithTimeoutCause(ctx, d, ErrRunTimeout)`. Inside `Run`, `errors.Is(context.Cause(ctx), worker.ErrRunTimeout)` tells the budget running out from the application shutting down.
- A run whose context was cancelled by the timeout is a failure (row 2) whatever `Run` returned. The recorded error wraps `ErrRunTimeout`: `worker: run timed out after 15s` when `Run` returned nil, `worker: run timed out after 15s: <returned error>` otherwise. The failure line carries `timed_out=true`, and the failure counts toward `WithMaxConsecutiveFailures`.
- "Timed out" states that the run exceeded its budget; it does not claim that side effects were rolled back. A run that completes just after the deadline is recorded as a failure — the conservative side of the boundary.
- The timeout is cooperative. It cancels the context and never abandons or kills the goroutine; a `Run` that ignores its context keeps the loop occupied, and activations that pass meanwhile are skipped. At most one run per worker is ever active.
- There is no pool-level default; budgets are per worker.

### Panics

Each run is protected by panic recovery. A recovered panic becomes an internal error whose text is `worker: run panicked: <value>`; the stack is kept separately and logged as the `stack` attribute of the failure line. The panic error does not unwrap to its value, so a panic with a context error as its value is never mistaken for a graceful stop. `Info.LastError` — and therefore the readiness failure text — never contains a stack trace; like any error text it is recorded verbatim and may contain newlines if the value's text does.

### Run context

The context passed to `Run` is derived from the pool context (cancelled on shutdown) and, with `WithRunTimeout`, bounded by the run timeout. It carries execution metadata only — services, loggers and configuration come from constructors:

| Accessor | Value |
| --- | --- |
| `WorkerName(ctx)` | the registration name |
| `RunID(ctx)` | a fresh identifier per run (`crypto/rand.Text`), equal to the `run_id` of the framework's lines for that run |
| `ScheduledAt(ctx)` | the intended activation time; zero for continuous workers and for the synthetic startup run |

Each accessor returns the zero value for a context that is not a run context.

---

## Logging Contract

For every worker, one pool session writes exactly one `worker started` and exactly one `worker stopped` line at Info, whatever path ends the loop. A `Start` that fails during resolution writes neither. All lines carry `worker=<name>`, and the pool logger adds `module=worker`.

| Message | Level | Attributes |
| --- | --- | --- |
| `worker started` | Info | `worker`, `kind`; scheduled adds `schedule` and either `start_immediately=true` or `next_run` |
| `worker stopped` | Info | `worker`, `kind`, `status`, `reason` (`shutdown` or `failed`) |
| `worker run failed` | Error | `worker`, `kind`, `restarts`, `error`, `run_id`, `duration`; `unexpected_exit=true` for outcome 6; `stack` for a panic |
| `scheduled worker run failed` | Error | `worker`, `scheduled_at`, `consecutive_failures`, `error`, `run_id`, `duration`; `timed_out=true` for outcome 2; `stack` for a panic |
| `scheduled worker run completed` | Debug | `worker`, `run_id`, `scheduled_at`, `duration` |
| `worker exceeded max restarts` | Error | `worker`, `kind`, `max_restarts` |
| `worker exceeded max consecutive failures` | Error | `worker`, `max_consecutive_failures` |
| `worker schedule has no future activation` | Error | `worker`, `schedule` |
| `worker ticks skipped` | Warn | `worker`, `skipped`, `first_scheduled_at`, `last_scheduled_at` |

- `reason=failed` stays at Info: the preceding Error line (`worker exceeded …` or `worker schedule has no future activation`) is the alerting signal; the stop line is lifecycle bookkeeping.
- A graceful stop writes no line of its own besides `worker stopped`.
- Successful scheduled runs are logged at Debug only. Liveness is answered by `Pool.Workers()` and `WithReadiness`; logs are the audit trail.

---

## Snapshot

### Info and Config

```go
type Config struct {
	Schedule               string           `json:"schedule"`
	StartImmediately       bool             `json:"start_immediately"`
	RunTimeout             time.Duration    `json:"run_timeout"`
	MaxConsecutiveFailures int              `json:"max_consecutive_failures"`
	MaxRestarts            int              `json:"max_restarts"`
	RestartDelay           time.Duration    `json:"restart_delay"`
	Readiness              *ReadinessPolicy `json:"readiness,omitzero"`
}

type Info struct {
	Name                string    `json:"name"`
	Kind                Kind      `json:"kind"`
	Config              Config    `json:"config"`
	Status              Status    `json:"status"`
	Restarts            int64     `json:"restarts"`
	ConsecutiveFailures int64     `json:"consecutive_failures"`
	LastRun             time.Time `json:"last_run,omitzero"`
	LastSuccess         time.Time `json:"last_success,omitzero"`
	LastError           string    `json:"last_error,omitzero"`
}
```

- `Config` is the **effective** policy the runner executes, not an echo of the options: `RestartDelay` is the resolved delay (option → config → default). `Schedule` is the expression as registered, which is also the effective schedule because `ParseSchedule` rejects every input it would otherwise have to rewrite. Fields that do not apply to the worker's kind are zero; zero limits mean unlimited.
- `Config.Readiness` is a fresh copy on every snapshot; mutating it never reaches the worker's definition.
- `Restarts` (continuous) counts restarts that started; `ConsecutiveFailures` (scheduled) counts failed runs since the last success.
- `LastRun` is the start time of the most recently admitted run. `LastSuccess` is the completion time of the last successful scheduled run. `LastError` is the error of the most recent failed run; a successful scheduled run clears it, a graceful stop does not.
- `LastError` is a string, not an `error`: a snapshot is for display and serialization.

### JSON shape

`Info` is shaped for direct encoding from an admin endpoint. Field names are snake_case (including the nested readiness policy); `last_run`, `last_success`, `last_error` and `config.readiness` are omitted while zero; every other field is always present, including the kind-inapplicable zeros in `config`; durations encode as integer nanoseconds under the framework's JSON response profile (ADR-021). An idle continuous worker and a scheduled worker after one failed run:

```json
[
  {
    "name": "consumer",
    "kind": "continuous",
    "config": {
      "schedule": "",
      "start_immediately": false,
      "run_timeout": 0,
      "max_consecutive_failures": 0,
      "max_restarts": 5,
      "restart_delay": 3000000000,
      "readiness": { "require_first_success": false, "fail_when_failed": true, "max_success_age": 0 }
    },
    "status": "idle",
    "restarts": 0,
    "consecutive_failures": 0
  },
  {
    "name": "report",
    "kind": "scheduled",
    "config": {
      "schedule": "@every 1m",
      "start_immediately": false,
      "run_timeout": 15000000000,
      "max_consecutive_failures": 3,
      "max_restarts": 0,
      "restart_delay": 0
    },
    "status": "waiting",
    "restarts": 0,
    "consecutive_failures": 1,
    "last_run": "2000-01-01T00:01:00Z",
    "last_error": "boom"
  }
]
```

### Status

| Status | Meaning |
| --- | --- |
| `idle` | registered; the pool has not published its runners, or a continuous runner has not yet been admitted to its first run |
| `running` | a run has been admitted and is executing |
| `waiting` | continuous: restart delay after a failure; scheduled: waiting for the next activation |
| `stopped` | the loop ended because the pool was shut down; the worker will not run again |
| `failed` | a positive failure limit was exhausted, or the schedule has no future activation; permanent until the application restarts |

`failed` is permanent: there is no paused or suppressed state. Applications that want indefinite recovery leave the limit at zero and handle degradation in their own logic.

---

## Lifecycle Integration

### Startup

```
app.Run()
  ├─ prepare: Finalize (DI freeze) → compile → publish
  ├─ net.Listen() — bind port
  ├─ OnStart hooks (FIFO)
  │   └─ pool.Start(ctx): claim → resolve (RegisterProvided types) → publish → one goroutine per worker
  ├─ state → running
  └─ srv.Serve() — accept connections
```

### Shutdown

```
app.Shutdown(ctx)
  ├─ state → stopping, readiness → unready
  ├─ OnPreDrain hooks            ← workers and DI remain live
  ├─ cancel app ctx              ← worker run contexts are cancelled
  ├─ parallel drain
  │   ├─ HTTP server drain
  │   └─ OnDrain hooks           ← pool.Shutdown(ctx): cancel pool ctx, wait for every worker goroutine
  ├─ DI teardown                 ← Pool's Shutdowner pass: already complete
  └─ OnShutdown hooks (LIFO)
```

Workers finish in the `OnDrain` phase, concurrently with the HTTP drain and before any DI singleton is torn down, so a worker's bounded cleanup (flushing a last batch) never observes a closed resource, whatever the registration order of the worker and the resource. All phases share the shutdown deadline; a worker that outlives it is reported as an incomplete `OnDrain` task and teardown proceeds with the expired context. There is no worker-specific shutdown timeout.

---

## Health Integration

Readiness participation is opt-in per worker; a failed metrics reporter must not take an instance out of rotation.

```go
worker.MustRegister(app, "recovery", recovery,
	worker.WithSchedule("@every 5m"),
	worker.WithStartImmediately(),
	worker.WithReadiness(worker.ReadinessPolicy{
		RequireFirstSuccess: true,             // unready until the first run succeeds
		FailWhenFailed:      true,             // unready once the worker is failed
		MaxSuccessAge:       15 * time.Minute, // unready when the last success is older
	}),
)
```

- The zero policy is rejected; `MaxSuccessAge` must be `>= 0`. `RequireFirstSuccess` and `MaxSuccessAge` are scheduled-only (a continuous worker never succeeds); `FailWhenFailed` applies to both kinds.
- `RequireFirstSuccess` stays satisfied once met. `MaxSuccessAge` is not applied before the first success; combine it with `RequireFirstSuccess` to close that window.
- `FailWhenFailed` sees every way a continuous worker can die: an early nil return is a failure like an error or a panic, so under a positive `WithMaxRestarts` limit a worker that keeps exiting reaches `failed`.
- The contribution is a readiness check named `worker:<name>` next to `AddReadinessCheck` entries (a name collision fails closed as a configuration error). It is evaluated in memory from the runner's last snapshot and never performs I/O; failure text is masked unless `HealthConfig.ExposeErrors` is set. Before `Pool.Start` a `RequireFirstSuccess` worker reports "has not started".

---

## Configuration

```yaml
worker:
  restart_delay: "5s" # default restart delay for continuous workers
```

The section is optional and read once, when the first registration creates the pool. A negative value is a registration error; zero means `DefaultRestartDelay`. `WithRestartDelay` overrides it per worker. Schedules, limits and timeouts are per-worker options only; a config reload does not re-register workers, and a changed cron expression is restart-only.

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
- Cron schedules are evaluated in the server's local time zone and fire at second 0 of the matching minute.
- As in crontab(5), when both day-of-month and day-of-week are restricted (neither is `*`), the schedule fires when **either** matches; a step on `*` counts as restricted.
- `@every` takes a Go duration that must be positive and a whole number of seconds: `@every 0s`, `@every -1h` and `@every 1500ms` are registration errors (`@every duration must be positive, got …`, `@every duration must be a whole number of seconds, got 1.5s`). No input is rounded or clamped.
- Not supported, each with a targeted error: the 6-field seconds form (use `@every` for sub-minute periods), `@yearly`/`@annually` (use `0 0 1 1 *`), and `TZ=`/`CRON_TZ=` prefixes.

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
			if ctx.Err() != nil {
				return nil // shutdown
			}
			return err // restarted after the restart delay
		}
		if err := w.orders.Process(ctx, msg); err != nil {
			// One bad message must not stop the consumer.
			w.log.ErrorContext(ctx, "process order failed",
				"error", err, "msg_id", msg.ID, "run_id", worker.RunID(ctx))
		}
	}
}

func main() {
	app, err := credo.New()
	if err != nil {
		log.Fatal(err)
	}
	app.MustProvide[*Queue](NewQueue)
	app.MustProvide[*OrderService](NewOrderService)
	app.MustProvide[*OrderConsumer](NewOrderConsumer)

	worker.MustRegisterProvided[*OrderConsumer](app, "order-consumer",
		worker.WithRestartDelay(5*time.Second),
	)

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
		Exec(ctx)
	if err != nil {
		return err
	}
	w.log.InfoContext(ctx, "expired sessions cleaned",
		"count", result.RowsAffected(), "run_id", worker.RunID(ctx))
	return nil
}

func register(app *credo.App) error {
	app.MustProvide[*SessionCleanup](NewSessionCleanup)

	// Every 6 hours plus once at startup; each run gets 10 minutes;
	// failed after 3 consecutive failures.
	return worker.RegisterProvided[*SessionCleanup](app, "session-cleanup",
		worker.WithSchedule("0 */6 * * *"),
		worker.WithStartImmediately(),
		worker.WithRunTimeout(10*time.Minute),
		worker.WithMaxConsecutiveFailures(3),
	)
}
```

### Example 3: Inline functions

```go
worker.MustRegister(app, "heartbeat", worker.Func(func(ctx context.Context) error {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			slog.InfoContext(ctx, "heartbeat", "worker", worker.WorkerName(ctx))
		}
	}
}))

// Skips an activation if the previous report is still running.
worker.MustRegister(app, "metrics-report", worker.Func(reportMetrics),
	worker.WithSchedule("@every 1m"),
)
```

### Example 4: File watcher (continuous, limited restarts)

```go
type ConfigWatcher struct {
	path     string
	onChange func(name string)
}

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

worker.MustRegister(app, "config-watcher", &ConfigWatcher{path: "./configs", onChange: reloadFile},
	worker.WithMaxRestarts(5),
	worker.WithRestartDelay(10*time.Second),
	worker.WithReadiness(worker.ReadinessPolicy{FailWhenFailed: true}),
)
```

---

## Package Structure

```
worker/
├── doc.go          # package doc + robfig/cron attribution
├── worker.go       # Worker, Func, run-context accessors
├── definition.go   # internal definition, options, ErrRunTimeout, Config/Info builders
├── info.go         # Kind, Status, Config, Info
├── pool.go         # Pool, Register/RegisterProvided, name and option validation, Start/Shutdown/Workers
├── runner.go       # runner state, run admission, loop, continuous and scheduled policies, log lines
├── outcome.go      # panic recovery, run-outcome classification
├── readiness.go    # ReadinessPolicy, WithReadiness, readiness probes
├── schedule.go     # Schedule, ParseSchedule (adapted from robfig/cron v3)
└── *_test.go       # synctest timing tests, capturing slog handler, examples
```

`worker/` imports the root package (like `store/`); the root package does not import `worker/`. The pool uses only public App methods — `CanProvideValue`, `Has`, `AdoptValue`, `ProvideProtectedValue`, `Replace`, `ConfigExists`/`GetConfig`, `Resolve`, `Logger`, `OnStart`, `OnDrain` — plus the module-internal readiness seam.

---

## Test Strategy

- **Virtual time.** Every timing test runs in a `testing/synctest` bubble; the runner calls the `time` package directly and there is no clock seam. `synctest.Wait` does not advance the clock — sleep past a deadline to fire timers.
- **Classification as a table.** The outcome classifier is a pure function; each row of the outcome table and each ordering edge (timeout then shutdown, shutdown then deadline, panic with a context-error value during shutdown) is a table entry.
- **Admission without a production seam.** An in-package test policy blocks in its activation step, the test cancels and releases it, and the admission check must observe the cancellation: zero `Run` calls and a zero `LastRun` before the first run; unchanged invocation count, `LastRun` and `Restarts` before a restart.
- **Logs through a capturing handler.** One `slog.Handler` records level, message and attributes; the lifecycle invariant (one `started`, one `stopped`, per exit path), run correlation (`run_id` equals `RunID(ctx)`) and the skip collapse are asserted through it.
- **Start/Shutdown races** use real goroutines: a `Shutdown` during resolution wins and nothing launches; concurrent `Start`/`Shutdown` never adds to a wait that has begun.
- **Wire shape.** A JSON golden encodes `Workers()` through the framework's response profile.
- **Schedules.** Pinned parse and `Next` behavior, rejected syntax, the `@every` rejection matrix, DST and end-of-month edges.
