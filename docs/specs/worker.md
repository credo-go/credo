# Worker Spec

**Status**: Draft (API sketch — under discussion) **Package**: `worker/` **Sources**: robfig/cron v3 (MIT, cron expression parser only) **ADR**: TBD

---

## Canonical Source

This file is the single source of truth for Credo's worker system design.

---

## Overview

The `worker/` package provides a unified background task system for Credo applications. It replaces the originally planned separate `cron/` package by supporting both **continuous** and **scheduled** workers through a single interface.

Key design properties:

- **Single interface** — `Worker` with `Name()` + `Run(ctx)`. Continuous vs scheduled is a registration option, not a type distinction.
- **Three-layer model** — `Definition` (immutable registration record) → `runner` (runtime executor) → `Info` (read-only snapshot). Config and live state never share a struct.
- **Fail-fast** — cron expressions are parsed at `Register` time. Invalid schedules are rejected immediately, not at startup.
- **Lifecycle integrated** — workers start during `app.Run()` via OnStart hook, stop via context cancellation on `app.Shutdown()`.
- **Panic-safe** — automatic panic recovery per worker execution.
- **Shutdown-aware** — context cancellation from app shutdown is recognized as a graceful stop, never counted as a failure or restart.
- **Distinct error policies** — continuous workers have a `restartPolicy` (restart count + backoff), scheduled workers have a `failurePolicy` (consecutive error limit). Same `Worker` interface, different operational semantics.
- **Overlap-safe** — scheduled workers unconditionally skip-if-still-running. No silent parallel execution. Allow/Queue deferred to v2.
- **Testable by design** — all timing tests run under `testing/synctest` bubbles (virtual clock), so scheduler and backoff tests are deterministic and instant without a clock-injection seam.
- **DI-friendly** — workers are constructed by user (possibly via DI), registered with `worker.Register(app, w, opts...)` or fail-fast `worker.MustRegister(app, w, opts...)`.
- **Observable** — structured logging per execution, future metrics/tracing hooks (depends on Phase 3.5).

---

## Goals

1. Provide a single, clean abstraction for background tasks (continuous loops, queue consumers, scheduled cleanups, periodic reports).
2. Eliminate the need for a separate `cron/` package — cron is a scheduling strategy, not a separate system.
3. Follow established Credo patterns: `store.Register`-style registration, `ensurePool` singleton pattern, `OnStart`/context cancellation lifecycle.
4. Keep the package self-contained with zero root-package changes beyond the integration point.
5. Ensure every time-dependent code path is deterministically testable (via `testing/synctest` virtual time).

---

## Non-Goals

- Job queue / task distribution (use dedicated systems: Asynq, Temporal).
- Persistent job state / job storage.
- Cluster-aware scheduling (single-instance only).
- Worker-specific DI scope (workers use app-level DI like everything else).
- Health check integration in v1 (snapshot API is sufficient; opt-in readiness binding deferred to v2).

---

## Three-Layer Model

The worker system cleanly separates **immutable config**, **mutable runtime state**, and **read-only snapshots**:

```
Register(app, w, opts...) error
       │
       ▼
  ┌────────────┐   pool.Start()   ┌────────────┐   pool.Workers()   ┌────────────┐
  │ Definition │ ────────────────► │   runner    │ ──────────────────► │    Info    │
  │ (immutable)│                   │  (mutable)  │                    │ (snapshot) │
  └────────────┘                   └────────────┘                    └────────────┘
  • name                           • definition (ref)                 • copied fields
  • worker                         • status (mu-guarded)              • frozen at read
  • schedule (compiled)            • attempts, lastRun, lastError     • safe to expose
  • restartPolicy / failurePolicy  •   (mu-guarded, string error)
  • startImmediately               • (serial run loop → overlap skip)
```

**Why**: Mixing config with live state causes the struct to accumulate temporal fields (attempt counters, last-error, running-flags). Over time the boundary between "what was configured" and "what is happening now" blurs. Three layers keep each concern in its own scope.

---

## Core Types

### Worker Interface

```go
package worker

import "context"

// Worker defines a background task managed by the framework.
//
// Name returns a unique identifier for logging and metrics.
// Run executes the worker's logic. The ctx is derived from the app
// context and carries execution metadata (worker name, attempt number,
// scheduled time, run ID). It is cancelled on app shutdown.
//
// Run semantics depend on how the worker is registered:
//   - Continuous (no WithSchedule): Run is called once. The worker should
//     loop internally and return when ctx is done. Returning an error
//     triggers a restart (governed by restartPolicy). Returning nil means
//     the worker finished normally — no restart.
//   - Scheduled (with WithSchedule): Run is called on each cron tick.
//     Returning an error increments the consecutive failure counter
//     (governed by failurePolicy). Returning nil resets the counter.
type Worker interface {
	Name() string
	Run(ctx context.Context) error
}
```

### WorkerFunc — Inline Worker

```go
// Func adapts a plain function into a Worker.
// The name is required for logging/metrics.
//
// Usage:
//
//	if err := worker.Register(app, worker.Func("cache-warm", func(ctx context.Context) error {
//	    return warmCache(ctx)
//	}), worker.WithSchedule("@every 5m")); err != nil {
//	    return err
//	}
func Func(name string, fn func(ctx context.Context) error) Worker
```

---

## Execution Context

The `ctx` passed to `Worker.Run()` carries lightweight execution metadata. Services and dependencies come from DI constructors — context is NOT a service locator.

```go
// Context value accessors (worker package)

// RunID returns a unique identifier for this execution.
// Useful for correlating logs across a single Run invocation.
func RunID(ctx context.Context) string

// Attempt returns the current attempt number (1-based).
// For continuous workers: restart count. For scheduled: always 1
// (consecutive errors tracked separately).
func Attempt(ctx context.Context) int

// WorkerName returns the worker's name from context.
func WorkerName(ctx context.Context) string

// ScheduledAt returns the intended fire time for scheduled workers.
// Returns zero time for continuous workers.
func ScheduledAt(ctx context.Context) time.Time
```

**What goes in context**: execution metadata only (run_id, attempt, worker_name, scheduled_at). **What does NOT go in context**: logger, tracer, services, config — these are injected via DI constructor.

---

## Schedule (Compiled)

Cron expressions are parsed at `Register` time and stored as a compiled `Schedule` object. No raw string survives into runtime.

```go
// Schedule represents a compiled cron schedule.
// Immutable after creation.
type Schedule struct {
	expr   string    // original expression (for display/logging only)
	next   func(now time.Time) time.Time // compiled next-fire calculator
}

// Next returns the next fire time after the given time.
func (s *Schedule) Next(now time.Time) time.Time

// String returns the original cron expression.
func (s *Schedule) String() string

// ParseSchedule parses a cron expression into a compiled Schedule.
// Returns an error if the expression is invalid.
// Supports: standard 5-field Vixie cron (lists, ranges, steps, month and
// weekday names, "?", 7=Sunday), predefined descriptors (@hourly,
// @daily/@midnight, @weekly, @monthly), intervals (@every 5m).
// Schedules fire at second 0 of the matching minute in the server's local
// time; for sub-minute periods use @every.
//
// Called at Register time — fail-fast, no runtime parsing.
func ParseSchedule(expr string) (*Schedule, error)
```

**Why compile at registration**: Invalid cron expressions are caught immediately with a clear error message and stack trace pointing to the `Register` call site, not a cryptic runtime failure minutes later when the scheduler first tries to compute the next tick.

---

## Options

Options are **mode-specific**. Using a continuous-only option with a scheduled worker (or vice versa) makes `Register` panic. This makes misconfiguration a compile-time-adjacent problem, not a silent default.

```go
import "time"

// Option configures a worker registration.
type Option func(*options)

// --- Continuous worker options (restartPolicy) ---

// WithMaxRestarts sets the maximum restart count for continuous workers.
// 0 = unlimited restarts (default). After exceeding the limit, the
// worker transitions to StatusFailed.
// Register panics if combined with WithSchedule — use
// WithMaxConsecutiveFailures instead.
func WithMaxRestarts(n int) Option

// WithRestartDelay sets the delay between restarts after an error or panic.
// Default: 3s.
// Register panics if combined with WithSchedule (timing governed by the
// cron schedule).
func WithRestartDelay(d time.Duration) Option

// --- Scheduled worker options (failurePolicy) ---

// WithSchedule sets a cron expression, making this a scheduled worker.
// The expression is parsed immediately; Register panics if it is
// invalid. See [ParseSchedule] for supported formats.
//
// Without this option, the worker runs continuously.
func WithSchedule(expr string) Option

// WithMaxConsecutiveFailures sets the consecutive error limit for
// scheduled workers. 0 = unlimited (default). After exceeding the limit,
// the worker transitions to StatusFailed permanently (requires app
// restart to recover).
// Error if used without WithSchedule — use WithMaxRestarts instead.
func WithMaxConsecutiveFailures(n int) Option

// WithStartImmediately causes a scheduled worker to run once immediately
// at startup, before waiting for the first cron tick.
// Error if used without WithSchedule (continuous workers always start
// immediately).
func WithStartImmediately() Option
```

### Overlap Handling

v1 unconditionally skips an activation if the previous execution is still running. No configuration needed — this is the only safe default.

Each scheduled worker is a single goroutine: sleep until the next activation, run it synchronously, recompute. Because runs are serial, overlap is impossible by construction. Activations whose time passes while a run is still in flight are **skipped with a log message** ("worker tick skipped") when the loop resumes — real, observable skip semantics, never a queue of stale runs.

Short-interval scheduled jobs (e.g., `@every 30s`) can easily overlap if a single execution takes longer than expected. Allow-by-default would give users silent parallel workers competing for the same resources. The serial loop makes the implementation trivial and the semantics unambiguous.

`OverlapAllow` and `OverlapQueue` are deferred to v2. Allow would require per-execution state tracking (multiple concurrent `running` flags, multiple `lastError` values, completion-order-dependent failure counting) — the single-runner model breaks down. Queue adds internal buffering and staleness edge cases. Neither is justified for v1.

### Error Policy Separation

```go
// restartPolicy governs continuous worker restart behavior (unexported).
type restartPolicy struct {
	maxRestarts  int           // 0 = unlimited
	restartDelay time.Duration // default: 3s
}

// failurePolicy governs scheduled worker failure handling (unexported).
type failurePolicy struct {
	maxConsecutiveFailures int // 0 = unlimited
}
```

**Why separate**: `WithMaxAttempts(3)` on a continuous worker means "restart up to 3 times after crashes". On a scheduled worker it would mean "stop after 3 consecutive errors". Same option name, two different semantics — confusing API. Explicit `WithMaxRestarts` / `WithMaxConsecutiveFailures` make the intent unambiguous at every call site. Cross-mode usage returns an error at registration rather than being silently ignored.

---

## Definition (Immutable Registration Record)

```go
// Definition is the immutable configuration of a registered worker.
// Created at Register time, never modified after. Held by the Pool
// and referenced by runners.
type Definition struct {
	name             string
	worker           Worker
	schedule         *Schedule       // nil → continuous; non-nil → scheduled
	restartPolicy    restartPolicy   // meaningful for continuous only
	failurePolicy    failurePolicy   // meaningful for scheduled only
	startImmediately bool            // meaningful for scheduled only
}

// Kind returns "continuous" or "scheduled".
func (d *Definition) Kind() string
```

---

## Runner (Runtime Executor, internal)

```go
// runner is the mutable runtime state of a worker. Created by
// Pool.Start() from a Definition. Not exported.
//
// Fields are protected by mu except for `running` (atomic for lock-free
// overlap check on the hot path). The mutex is held briefly by the
// execution loops and by Pool.Workers() snapshot reads.
type runner struct {
	def     *Definition // immutable reference

	mu        sync.Mutex
	status    Status     // current lifecycle state
	attempts  int64      // restart count (continuous) or consecutive failures (scheduled)
	lastRun   time.Time  // last execution start (zero if never)
	lastError string     // last error message (empty if healthy)

	running atomic.Bool  // true while Run() is in-flight (overlap guard)
}

// Setter methods acquire mu. Pseudocode uses these for clarity:
//   runner.setStatus(s)    runner.setAttempts(n)
//   runner.setLastRun(t)   runner.setLastError(msg)

// snapshot returns an Info copy under mu. Called by Pool.Workers().
func (r *runner) snapshot() Info {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Info{
		Name:      r.def.name,
		Kind:      r.def.Kind(),
		Schedule:  r.def.scheduleExpr(),
		Status:    r.status,
		Attempts:  r.attempts,
		LastRun:   r.lastRun,
		LastError: r.lastError,
	}
}
```

**Why mutex, not atomic.Value**: `atomic.Value` requires every `Store` to use the same concrete type. Storing `nil` and then an `*errors.errorString` panics at runtime. Storing `error` as interface is equally fragile — different concrete error types across stores. A mutex-guarded `string` field is simple, correct, and makes the snapshot read (`Pool.Workers()`) trivially safe.

**No per-runner cancel**: The pool context governs all runners. Individual runner termination (e.g., worker transitions to `StatusFailed`) is handled by the goroutine returning, not by cancellation. A `cancel` field would add unused API surface; if per-worker cancellation is needed in v2 (e.g., admin stop-single-worker), it can be added then.

The runner is an internal type. Users interact with `Definition` at registration time and `Info` at query time — never with the runner directly.

---

## Info (Read-Only Snapshot)

```go
// Info is a point-in-time snapshot of a worker's state.
// Returned by Pool.Workers(). All fields are copied — safe to hold,
// log, serialize, or compare across calls.
type Info struct {
	Name       string    // from Definition
	Kind       string    // "continuous" or "scheduled"
	Schedule   string    // original cron expression (display only), empty for continuous
	Status     Status    // snapshot of current status
	Attempts   int64     // restarts (continuous) or consecutive failures (scheduled)
	LastRun    time.Time // last execution start (zero if never)
	LastError  string    // last error message (empty if healthy) — string, not error
}
```

**Why `LastError string` not `error`**: Info is a snapshot meant for display, logging, and serialization. Carrying a live `error` value leaks implementation details and prevents clean JSON marshaling. The string is the error's message at snapshot time.

### Status

```go
// Status represents a worker's current lifecycle state.
type Status string

const (
	StatusIdle    Status = "idle"    // registered, pool not yet started
	StatusRunning Status = "running" // actively executing Run()
	StatusWaiting Status = "waiting" // continuous: backoff delay; scheduled: waiting for next tick
	StatusStopped Status = "stopped" // finished normally (Run returned nil), no restart
	StatusFailed  Status = "failed"  // exceeded max restarts/failures, permanently stopped
)
```

**Removed `StatusDisabled`**: The original draft had "disabled until next successful tick" which is self-contradictory — a disabled worker cannot have a successful tick. Instead, `StatusFailed` is permanent: the worker is done until app restart. There is no intermediate "paused/suppressed" state. If auto-recovery is needed, users set `maxRestarts: 0` (unlimited) or `maxConsecutiveFailures: 0` (unlimited) and handle degradation in their own logic.

---

## Time and Testing (synctest)

The pool and runners call the `time` package directly (`time.Now`, `time.NewTimer`); there is no clock-injection seam. Determinism comes from `testing/synctest` (Go 1.26+): timing tests run inside a bubble whose virtual clock advances only when every goroutine is durably blocked.

```go
func TestRunContinuous_MaxRestartsMarksFailed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pool := newTestPool()
		// ... register a failing worker with restartDelay: time.Minute
		pool.Start(t.Context())

		synctest.Wait()             // first run failed, runner sleeps
		time.Sleep(time.Minute)     // virtual: restart timer fires instantly
		synctest.Wait()             // second run settled
		// assert StatusFailed ...
	})
}
```

**Why synctest over a Clock interface**: the earlier design carried a `Clock` abstraction (`Now()` / `After()`) plus a hand-rolled `fakeClock` with its own timer bookkeeping — production indirection whose only purpose was testability, and `time.After` in the runner leaked timers until they fired. `testing/synctest` gives deterministic, instant timing tests against the real `time` package, so the seam (and `clock.go`) was deleted outright. Two rules of thumb inside bubbles: `synctest.Wait` does **not** advance the clock (sleep past a deadline to fire timers), and goroutines must block on synctest-aware primitives (channels, timers, mutexes) for time to advance.

---

## Registration

### worker.Register

```go
// Register adds a Worker to the application's worker pool. The pool is
// created on the first call (singleton pattern via DI). Workers are
// started during app.Run() and stopped on app.Shutdown().
//
// Register must be called before app.Run(). Registration is startup
// configuration, so invalid registration is returned as an error when:
//   - app or w is nil
//   - w.Name() is empty or duplicate
//   - WithSchedule expression is invalid (parsed immediately)
//   - Continuous-only options used with scheduled, or vice versa
//
// Pattern: sub-package function taking *credo.App, like store.Register.
// Use MustRegister for fail-fast bootstrap code that should panic on error.
func Register(app *credo.App, w Worker, opts ...Option) error

// MustRegister is like Register but panics on error.
func MustRegister(app *credo.App, w Worker, opts ...Option)
```

Internal flow:

```
1. Validate: nil checks, empty name
2. Apply raw options → intermediate options struct
3. If schedule option present → ParseSchedule(expr) — return error on invalid
4. Classify mode: continuous (no schedule) or scheduled
5. Reject cross-mode options → error:
   • scheduled + WithMaxRestarts      → "WithMaxRestarts is for continuous workers; use WithMaxConsecutiveFailures"
   • scheduled + WithRestartDelay     → "WithRestartDelay is for continuous workers"
   • continuous + WithMaxConsecFail   → "WithMaxConsecutiveFailures is for scheduled workers; use WithMaxRestarts"
   • continuous + WithStartImmediately → "WithStartImmediately is for scheduled workers"
6. Build restartPolicy / failurePolicy from validated options
7. Build Definition (immutable)
8. ensurePool(app) → resolve-or-create *Pool singleton in DI
9. pool.addDefinition(def) — check name uniqueness, error on duplicate
```

### ensurePool (internal)

```go
// ensurePool resolves or creates the worker Pool in the DI container.
// On first call, a new Pool is created, registered in DI, and lifecycle
// hooks are installed.
//
// The pool receives the app-level logger via app.Logger(). This requires
// a minimal root package addition: App.Logger() *slog.Logger (returns
// the same logger used by the framework's error handler, server lifecycle,
// and i18n setup). Worker execution logs are tagged with "module"="worker"
// to distinguish them from framework-internal logs.
func ensurePool(app *credo.App) (*Pool, error) {
	p, err := credo.Resolve[*Pool](app)
	if err == nil {
		return p, nil
	}

	// First worker registration — create pool and wire lifecycle.
	logger := app.Logger().With("module", "worker")
	p = newPool(logger, cfg.RestartDelay)
	if err := credo.ProvideValue[*Pool](app, p); err != nil {
		// Race: another goroutine may have registered between Resolve and ProvideValue.
		resolved, resolveErr := credo.Resolve[*Pool](app)
		if resolveErr == nil {
			return resolved, nil
		}
		return nil, fmt.Errorf("worker: register pool: %w", errors.Join(err, resolveErr))
	}

	// Start workers after port is bound (after store connections, before accepting).
	app.OnStart(func(lifecycleCtx context.Context) error {
		return p.Start(lifecycleCtx)
	})

	return p, nil
}
```

> **Root package change required**: `App.Logger() *slog.Logger` — a trivial accessor for the existing `app.logger` field. This is consistent with how `store.Register` wires store health through the module-internal `internal/health.StoreFunc` DI seam (sub-packages need limited access to app internals without expanding the public API).

---

## Pool

```go
// Pool manages registered workers: starts them during app startup,
// monitors their lifecycle, and stops them during shutdown.
//
// Pool is not exported for direct construction. Use Register to add
// workers — the pool is created automatically on first registration.
// Pool implements credo.Shutdowner for automatic container shutdown.
type Pool struct {
	mu          sync.Mutex
	definitions []*Definition  // immutable after Start
	runners     []*runner      // created by Start
	logger      *slog.Logger
	cancel      context.CancelFunc
	wg          sync.WaitGroup // tracks one goroutine per worker (continuous loop or scheduled loop)
	started     bool
}

// Start creates a runner for each Definition and launches goroutines.
// Called via OnStart hook. The ctx is the lifecycle context — cancelled on Shutdown.
func (p *Pool) Start(ctx context.Context) error

// Shutdown stops all workers gracefully. Cancels the pool context and
// waits for all scheduler and in-flight execution goroutines to finish
// (respects ctx deadline).
// Implements credo.Shutdowner for automatic container shutdown.
func (p *Pool) Shutdown(ctx context.Context) error

// Workers returns a snapshot of all worker states.
// Safe to call concurrently. The returned slice is a copy.
func (p *Pool) Workers() []Info
```

`Pool.Start` creates a `runner` for each `Definition`:

```go
func (p *Pool) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started {
		return errors.New("worker: pool already started")
	}

	poolCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.started = true

	for _, def := range p.definitions {
		r := &runner{def: def}
		p.runners = append(p.runners, r)

		if def.schedule != nil {
			// Scheduled: first status is Waiting (for the first activation).
			// startImmediately case sets Running inside runScheduled.
			r.setStatus(StatusWaiting)
			p.wg.Go(func() { p.runScheduled(poolCtx, r) })
		} else {
			// Continuous: Run() is called immediately.
			r.setStatus(StatusRunning)
			p.wg.Go(func() { p.runContinuous(poolCtx, r) })
		}
	}
	return nil
}
```

---

## Execution Model

### Continuous Worker

```
runContinuous(ctx, runner):
  defer wg.Done()
  restarts := 0
  policy := runner.def.restartPolicy

  loop:
    if ctx.Err() != nil → runner.setStatus(Stopped); return

    runCtx := enrichContext(ctx, runner.def.name, restarts+1, zeroTime, newRunID())
    runner.setLastRun(time.Now())

    err := safeRun(runCtx, runner.def.worker)

    if err == nil:
      runner.setStatus(Stopped)   // worker finished normally
      return

    // Shutdown cancellation is not a failure.
    if isGracefulStop(err, ctx):
      runner.setStatus(Stopped)
      log("worker %s: graceful stop", name)
      return

    runner.setLastError(err.Error())
    restarts++
    runner.setAttempts(restarts)
    log("worker %s error (restart %d): %v", name, restarts, err)

    if policy.maxRestarts > 0 && restarts >= policy.maxRestarts:
      runner.setStatus(Failed)
      log("worker %s exceeded max restarts (%d), stopping", name, policy.maxRestarts)
      return

    runner.setStatus(Waiting)
    timer := time.NewTimer(policy.restartDelay)
    select:
      case <-ctx.Done():
        timer.Stop()   // no timer leak on shutdown
        runner.setStatus(Stopped)
        return
      case <-timer.C:
        goto loop
```

### Scheduled Worker

Each scheduled worker is a **single goroutine** running a serial loop: sleep until the next activation, run it synchronously, recompute. (An earlier design used two goroutines per worker — a scheduler handing fire times to an executor over an unbuffered channel — plus three coordination channels for startup, failure, and tick handoff. The serial loop provides the same observable semantics with none of that choreography.)

```
runScheduled(ctx, runner):
  consecutiveFailures := 0

  // runOnce executes one activation synchronously and reports whether the
  // loop should stop (graceful stop or permanent failure). It owns all
  // runner state updates: lastRun, attempts, status, lastError.
  runOnce(intendedTime) -> stop:
    runner.startRun(time.Now())
    runCtx := enrichContext(ctx, runner.def.name, 1, intendedTime, newRunID())
    err := safeRun(runCtx, runner.def.worker)

    if isGracefulStop(err, ctx):           // shutdown is not a failure
      status = Stopped; return true
    if err == nil:
      consecutiveFailures = 0
      status = Waiting (Stopped if ctx done); return false
    consecutiveFailures++
    log("scheduled worker run failed", ...)
    if policy.maxConsecutiveFailures exceeded:
      status = Failed                      // permanent — requires app restart
      return true
    if ctx done: status = Stopped; return true
    status = Waiting; return false

  // --- startImmediately ---
  // Synthetic startup run with ScheduledAt = time.Time{} (zero), executed
  // before the first computed activation — serial by construction, so it
  // can never overlap the first real activation.
  if runner.def.startImmediately:
    if ctx done → Stopped; return
    if runOnce(zeroTime) → return

  // --- main loop ---
  // anchor is the last intended activation. Computing the next activation
  // from it (not from time.Now()) keeps @every periods anchored: a long
  // run delays the next fire but does not shift the grid.
  anchor := time.Now()
  timer := stopped time.Timer (reused across iterations, Stop on exit)

  loop:
    next := schedule.Next(anchor)
    while next is not zero and !next.After(time.Now()):
      // The previous run outlasted this activation — skip it, never queue.
      log("worker tick skipped", "scheduled_at", next)
      next = schedule.Next(next)
    if next is zero:
      status = Failed, lastError = "schedule has no future activation"
      return

    timer.Reset(time.Until(next))
    select:
      case <-ctx.Done():
        runner.stopIfNotFailed()   // preserve last execution snapshot
        return
      case <-timer.C:

    if runOnce(next) → return
    anchor = next
```

**Key properties of this model:**

- **Overlap is impossible by construction.** Runs are serial; there is no concurrent executor to race against.
- **Skips are real and observable.** Activations that pass while a run is in flight are logged ("worker tick skipped") and dropped — never queued as stale back-to-back runs.
- **@every stays anchored.** The next activation is computed from the last intended time, so a 10s-long run under `@every 1m` still fires on the original minute grid instead of drifting by run duration. (Cron expressions are wall-clock anchored either way.)
- **Shutdown waits for real work.** The pool waitgroup tracks the single goroutine, which is either sleeping (exits on `ctx.Done()`) or inside `Run()` (returns via the same parent context).
- **No timer leak.** One reusable `time.Timer` per worker, stopped on exit — no `time.After` channels left behind.
- **startImmediately is just a first run** with `ScheduledAt` = zero time, executed before the loop starts.

### Graceful Stop Detection

```go
// isGracefulStop reports whether err is a context cancellation or deadline
// caused by the parent (pool/app) context shutting down — as opposed to
// a real application error. Graceful stops are NOT counted as failures and
// do NOT increment restart/consecutive-failure counters.
//
// The check requires BOTH:
//   1. err wraps context.Canceled or context.DeadlineExceeded
//   2. the parent ctx (pool ctx, not the enriched per-run ctx) is done
//
// This prevents false positives: a worker that creates its own
// sub-context with a deadline and lets it expire produces
// context.DeadlineExceeded, but parent ctx is still alive → real error.
func isGracefulStop(err error, parentCtx context.Context) bool {
	if parentCtx.Err() == nil {
		return false // parent is still alive — this is a real error
	}
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}
```

### Panic Recovery

```go
// safeRun calls w.Run(ctx) and recovers panics, converting them to errors.
func safeRun(ctx context.Context, w Worker) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("worker %q panicked: %v\n%s", w.Name(), r, debug.Stack())
		}
	}()
	return w.Run(ctx)
}
```

### Context Enrichment

```go
// enrichContext adds execution metadata to the context.
// No services — only data that identifies this specific execution.
func enrichContext(
	parent context.Context,
	name string,
	attempt int,
	scheduledAt time.Time,
	runID string,
) context.Context {
	ctx := context.WithValue(parent, workerNameKey{}, name)
	ctx = context.WithValue(ctx, attemptKey{}, attempt)
	ctx = context.WithValue(ctx, scheduledAtKey{}, scheduledAt)
	ctx = context.WithValue(ctx, runIDKey{}, runID)
	return ctx
}
```

---

## Lifecycle Integration

### Startup Sequence

```
app.Run()
  ├─ compile()
  ├─ Finalize(app) — DI freeze
  ├─ net.Listen() — bind port
  ├─ OnStart hooks (FIFO):
  │   ├─ ... (store ping, other hooks)
  │   └─ pool.Start(ctx)
  │       ├─ for each Definition → create runner
  │       ├─ continuous: go runContinuous(ctx, runner)
  │       └─ scheduled:  go runScheduled(ctx, runner)
  ├─ state → running
  └─ srv.Serve() — accept connections
```

### Shutdown Sequence

```
app.Shutdown(ctx)
  ├─ state → stopping
  ├─ cancel app ctx              ← workers receive ctx.Done()
  ├─ srv.Shutdown() — HTTP drain
  ├─ container.Shutdown()        ← pool.Shutdown(ctx) called here
  │   └─ cancel pool ctx
  │   └─ wg.Wait() — wait for the per-worker goroutines (continuous and scheduled loops)
  └─ OnShutdown hooks (LIFO)
```

Workers receive shutdown signal via context cancellation. The pool's `Shutdown` is called automatically because `Pool` implements `credo.Shutdowner` and is registered in the DI container.

---

## Health Integration (deferred)

v1 exposes only `Pool.Workers() []Info` — a snapshot API sufficient for logging, debugging, and admin endpoints.

Health check binding is **not** wired automatically. Not every failed worker should degrade readiness — a failed metrics-reporter should not make the entire app unready. If needed in the future:

```go
// Future (v2): opt-in readiness binding per worker
worker.MustRegister(app, critical,
	worker.WithSchedule("@every 30s"),
	worker.WithCritical(), // failed → readiness probe fails
)
```

`WithCritical()` is reserved for a future version. Until then, users who need worker health in readiness can add a custom `ReadinessCheck` that inspects `Pool.Workers()`.

---

## Usage Examples

### Example 1: Queue Consumer (Continuous)

```go
package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/worker"
)

type OrderConsumer struct {
	queue  *Queue
	orders *OrderService
}

func NewOrderConsumer(infra credo.Infra, q *Queue, svc *OrderService) *OrderConsumer {
	return &OrderConsumer{queue: q, orders: svc}
}

func (w *OrderConsumer) Name() string { return "order-consumer" }

func (w *OrderConsumer) Run(ctx context.Context) error {
	for {
		msg, err := w.queue.Receive(ctx) // blocks until message or ctx done
		if err != nil {
			return err // framework restarts worker
		}
		if err := w.orders.Process(ctx, msg); err != nil {
			// Log and continue — don't kill the worker for a single message
			slog.ErrorContext(ctx, "process order failed",
				"err", err,
				"msg_id", msg.ID,
				"run_id", worker.RunID(ctx),
			)
		}
	}
}

func main() {
	app, _ := credo.New()

	credo.MustProvide[*Queue](app, NewQueue)
	credo.MustProvide[*OrderService](app, NewOrderService)
	credo.MustProvide[*OrderConsumer](app, NewOrderConsumer)

	consumer := credo.MustResolve[*OrderConsumer](app)

	// Continuous: unlimited restarts, 5s backoff between restarts
	worker.MustRegister(app, consumer,
		worker.WithRestartDelay(5*time.Second),
	)

	app.GET("/orders/{id}", getOrder)
	app.Run()
}
```

### Example 2: Scheduled Cleanup (Cron Replacement)

```go
type SessionCleanup struct {
	db *sqldb.DB
}

func NewSessionCleanup(infra credo.Infra, db *sqldb.DB) *SessionCleanup {
	return &SessionCleanup{db: db}
}

func (w *SessionCleanup) Name() string { return "session-cleanup" }

func (w *SessionCleanup) Run(ctx context.Context) error {
	result, err := w.db.NewDelete().
		Model((*Session)(nil)).
		Where("expired_at < ?", time.Now()).
		Exec(ctx)
	if err != nil {
		return err
	}
	slog.InfoContext(ctx, "expired sessions cleaned",
		"count", result.RowsAffected(),
		"run_id", worker.RunID(ctx),
		"scheduled_at", worker.ScheduledAt(ctx),
	)
	return nil
}

func main() {
	app, _ := credo.New()

	credo.MustProvide[*SessionCleanup](app, NewSessionCleanup)
	cleanup := credo.MustResolve[*SessionCleanup](app)

	// Every 6 hours, run once at startup, fail permanently after 3 consecutive errors
	worker.MustRegister(app, cleanup,
		worker.WithSchedule("0 */6 * * *"),
		worker.WithStartImmediately(),
		worker.WithMaxConsecutiveFailures(3),
		// Overlap: if cleanup takes >6h, next tick is skipped automatically
	)

	app.Run()
}
```

### Example 3: WorkerFunc (Inline, Simple Tasks)

```go
func main() {
	app, _ := credo.New()

	// Simple heartbeat logger — no struct needed
	worker.MustRegister(app,
		worker.Func("heartbeat", func(ctx context.Context) error {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-ticker.C:
					slog.InfoContext(ctx, "heartbeat",
						"worker", worker.WorkerName(ctx),
					)
				}
			}
		}),
	)

	// Scheduled metrics reporter — skips tick if previous report still running
	worker.MustRegister(app,
		worker.Func("metrics-report", func(ctx context.Context) error {
			return reportMetrics(ctx)
		}),
		worker.WithSchedule("@every 1m"),
	)

	app.Run()
}
```

### Example 4: File Watcher (Continuous, Limited Retries)

```go
type ConfigWatcher struct {
	path string
	bus  *event.Bus
}

func (w *ConfigWatcher) Name() string { return "config-watcher" }

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
				w.bus.Emit("config.changed", ev.Name)
			}
		case err := <-watcher.Errors:
			return err // restart on watcher error
		}
	}
}

func main() {
	app, _ := credo.New()

	worker.MustRegister(app, &ConfigWatcher{path: "./configs", bus: myBus},
		worker.WithMaxRestarts(5),
		worker.WithRestartDelay(10*time.Second),
	)

	app.Run()
}
```

---

## Configuration (Optional)

Workers can optionally read defaults from RawConfig under the `worker` key. Per-worker options always take precedence.

```yaml
worker:
  restart_delay: "5s"           # default restart delay (continuous)
```

Config is read once at pool creation (inside `ensurePool`). Individual `WithRestartDelay` etc. options override config defaults.

**No `shutdown_timeout` config**: The pool's `Shutdown(ctx)` receives the global shutdown context from `app.Shutdown(ctx)`, which already carries a deadline. Adding a worker-specific timeout would create ambiguity about which deadline wins. The pool respects `ctx.Done()` and that is the single source of truth for shutdown deadlines.

---

## Package Structure

```
worker/
├── worker.go        # Worker interface, Func adapter, context accessors
├── definition.go    # Definition struct, options, restartPolicy, failurePolicy
├── pool.go          # Pool struct, Register, Start, Shutdown, addDefinition
├── runner.go        # runner struct, runContinuous, runScheduled, safeRun
├── schedule.go      # Schedule, ParseSchedule (adapted from robfig/cron v3)
├── info.go          # Info, Status
├── doc.go           # Package doc + robfig/cron attribution
├── register_test.go
├── runner_test.go   # testing/synctest bubbles for all timing tests
├── schedule_test.go # pinned parse/Next behavior + rejected syntax
└── testhelpers_test.go
```

Root package addition: `App.Logger() *slog.Logger` — one-line accessor.

### Dependency Direction

```
worker/ ──imports──→ credo (root)   ✓  (same as store/)
credo (root) ──does NOT import──→ worker/  ✓  (no cycle)
```

Worker package calls `credo.Resolve`, `credo.ProvideValue`, `app.OnStart`. Root package has zero awareness of worker package.

---

## Cron Expression Parser

Adapted from robfig/cron v3 (MIT) and trimmed to the expected default Unix cron surface. Only expression parsing and next-fire calculation are taken, not the scheduler/runner. Compiled into `Schedule` at registration time.

| Format             | Example                | Description             |
| ------------------ | ---------------------- | ----------------------- |
| Standard (5-field) | `0 */6 * * *`          | min hour dom month dow  |
| Predefined         | `@hourly`              | Every hour at minute 0  |
| Predefined         | `@daily` / `@midnight` | Every day at 00:00      |
| Predefined         | `@weekly`              | Every Sunday at 00:00   |
| Predefined         | `@monthly`             | First of month at 00:00 |
| Interval           | `@every 5m`            | Every 5 minutes         |
| Interval           | `@every 1h30m`         | Every 1.5 hours         |

Field syntax: lists (`1,15`), ranges (`1-5`), steps (`*/10`, `8-18/2`), month/weekday names (`jan`, `sat`), `?` as an alias for `*`, and `7` accepted as Sunday. Schedules are evaluated in the server's local time zone and fire at second 0 of the matching minute.

As in crontab(5), when both day-of-month and day-of-week are restricted (neither is `*`), the schedule fires when **either** matches: `0 0 13 * fri` runs on the 13th AND on every Friday. A step applied to `*` (e.g. `*/2` in day-of-month) counts as restricted for this rule.

Deliberately not supported (trimmed from the robfig heritage): the 6-field seconds form (sub-minute periods → `@every`), `@yearly`/`@annually` (→ `0 0 1 1 *`), and `TZ=`/`CRON_TZ=` prefixes (schedules always run in server local time). Each is rejected with a targeted error message.

---

## Comparison: Before and After

### Before (separate cron/ package from TODO)

```go
// Two separate systems needed:

// 1. Cron (Phase 4.2 — not yet implemented)
app.Cron("*/5 * * * *", cleanupHandler)

// 2. Background tasks — no framework support
go func() {
    for {
        msg, err := queue.Receive(ctx)
        // ... manual goroutine, panic recovery, restart, shutdown
    }
}()
```

### After (unified worker/ package)

```go
// Single system, clear semantics:

// Scheduled task (replaces cron/)
worker.MustRegister(app, cleanup,
	worker.WithSchedule("*/5 * * * *"),
	worker.WithMaxConsecutiveFailures(5),
)

// Continuous background task
worker.MustRegister(app, consumer,
	worker.WithMaxRestarts(10),
	worker.WithRestartDelay(5*time.Second),
)

// Both get: panic recovery, policy-based error handling, structured
// logging, graceful shutdown, overlap protection — automatically.
```

---

## Test Strategy

- **synctest-based tests**: All timing tests run in `testing/synctest` bubbles. Virtual time, no `time.Sleep` flakiness, deterministic interleaving via `synctest.Wait`.
- **Unit tests**: Worker interface mock, option parsing, schedule parsing, option conflict rejection.
- **Definition tests**: Immutability, Kind(), policy defaults.
- **Runner tests**: restart count, consecutive failures, overlap skip (activations elapsing during a long run → skipped + logged, next future activation proceeds normally), panic recovery, status transitions (Idle→Running→Waiting→Failed, Idle→Waiting→Running→Stopped), graceful stop detection (context.Canceled from parent → Stopped not Failed, context.DeadlineExceeded from worker sub-ctx → counted as failure), startImmediately with ScheduledAt=zero.
- **Pool tests**: Start/Shutdown lifecycle, concurrent workers, `Workers()` snapshot correctness, duplicate name rejection.
- **Schedule tests**: cron expression parsing, `Next()` calculation, edge cases (DST, leap seconds, end-of-month).
- **Integration**: `testutil.NewApp` + `worker.Register` + verify execution via channels/atomics.
- **Benchmark**: Pool startup/shutdown overhead, schedule calculation.

Testing helpers for users:

```go
// workertest sub-package (or in testutil/)
package workertest

// RunOnce runs a worker exactly once with a test context and returns
// the error. Useful for testing worker logic without the pool.
func RunOnce(t *testing.T, w worker.Worker) error

// Spy wraps a Worker and records execution history.
type Spy struct {
	Worker worker.Worker
	Calls  atomic.Int64
	Errors []error
	mu     sync.Mutex
}

func (s *Spy) Name() string             { return s.Worker.Name() }
func (s *Spy) Run(ctx context.Context) error { ... }
func (s *Spy) CallCount() int64         { return s.Calls.Load() }
```

---

## Resolved Decisions

1. **Overlap policy**: v1 is skip-only. `OverlapAllow` requires per-execution state tracking (multiple running flags, completion-order failure counting) that breaks the single-runner model. `OverlapQueue` adds buffering and staleness. Both deferred to v2 when real usage patterns emerge. The serial loop makes overlap impossible by construction, and skipped activations are still observable: each one is logged when the loop resumes ("worker tick skipped").

2. **Option cross-mode usage**: Returns error, not silently ignored. `WithRestartDelay` on a scheduled worker → error at `Register`. Makes misconfiguration visible immediately.

3. **StatusFailed is permanent**: No intermediate "paused/suppressed" state. A failed worker requires app restart. Users who need auto-recovery use `maxRestarts: 0` (unlimited).

4. **scheduledAt = intended fire time**: The `next` value from `Schedule.Next()` is passed to context, not `time.Now()` at execution. `lastRun` on the runner stores actual execution time for monitoring. For `WithStartImmediately`, `ScheduledAt` returns `time.Time{}` (zero) — this is a synthetic startup tick with no cron-computed fire time. Callers distinguish startup ticks via `worker.ScheduledAt(ctx).IsZero()`.

5. **No shutdown_timeout config**: Pool respects the global shutdown context deadline. Worker-specific timeout would conflict.

6. **Shutdown/cancel ≠ failure**: `isGracefulStop(err, parentCtx)` checks both `errors.Is(err, context.Canceled || DeadlineExceeded)` AND parent ctx is done. Prevents false positives from worker-internal sub-contexts. Graceful stops transition to `StatusStopped`, not `StatusFailed`.

7. **Runner uses mutex, not atomic.Value**: `atomic.Value` panics on mixed-type Store (nil vs concrete error). Mutex-guarded fields with string error are simple and correct. `running` stays atomic (lock-free overlap check).

8. **No per-runner cancel**: Pool context governs all runners. Failed workers return from their goroutine. Per-worker cancel deferred to v2.

9. **App.Logger() accessor**: `App.Logger() *slog.Logger` is the correct mechanism. Registering `*slog.Logger` in DI would let users bypass `credo.Infra`'s service-scoped logger pattern — services would inject the raw logger instead of getting a properly tagged one via Infra. `App.Logger()` keeps the logger accessible to framework internals (worker pool, error handler) without polluting the DI container.

---

## Design Questions

1. **Worker.Run() loop semantics**: Should continuous workers manage their own loop, or should `Run()` represent a single execution that the framework loops? Current proposal: worker manages own loop (more flexible, simpler framework code).

2. **Registration via DI**: Should there be a `worker.Provide[T]` shortcut that combines `credo.Provide + credo.Resolve + worker.Register`? Or is the 3-step explicit approach more aligned with Credo's philosophy?

3. **Pool start timing**: OnStart (after port bind) or separate phase? Current proposal: OnStart — workers start alongside HTTP server.

4. **Config key reads**: Should pool read `worker.*` config from RawConfig automatically, or require explicit config? Current proposal: auto-read with option override (consistent with middleware config pattern).
