# Worker Guide

This guide explains how to run background tasks with Credo's `worker/` package. For the exact contracts — outcome classification, admission order, log attributes, the snapshot's JSON shape — see the [Worker Spec](../specs/worker.md); for the reasoning behind them, [ADR-023](../adr/023-worker-system.md). Workers are lifecycle components, so the [lifecycle spec](../specs/lifecycle.md) and the [dependency-injection guide](dependency-injection.md#shutdown-and-lifecycle) describe the start and the drain they take part in.

---

## What Workers Are For

Credo workers are for application work that runs outside the HTTP request path:

- queue consumers
- event processors
- periodic cleanup jobs
- cache warmers
- reconciliation / sync loops

Workers are optional. If your app only serves HTTP requests, you do not need the package.

---

## Mental Model

A worker is anything with a `Run(ctx context.Context) error` method. You register it with a **supervisor** — `workers := worker.Use(app)` — under a name, and the registration method decides its kind:

| Method | Kind | The worker comes from |
| --- | --- | --- |
| `workers.Continuous(name, w, cfg...)` | continuous | the value you pass |
| `workers.Scheduled(name, expr, w, cfg...)` | scheduled | the value you pass |
| `workers.ContinuousProvided[T](name, cfg...)` | continuous | the DI container, as `T` |
| `workers.ScheduledProvided[T](name, expr, cfg...)` | scheduled | the DI container, as `T` |

Each registration adds one lifecycle component to the App, named `worker:<name>`. The App starts it in the start phase and stops it in the drain like every other component: its `Start` launches the worker's loop, its `Shutdown` cancels the worker's context and waits for `Run` to return. The supervisor itself has no lifecycle — it is the registry you register through and the object you ask for each worker's state.

### Continuous worker

- Credo calls `Run(ctx)` once when the worker starts.
- The worker owns its loop and **must keep running until `ctx` is cancelled**.
- If `Run` returns an error, panics, or returns nil while its context is still alive, Credo records a failure and calls `Run` again after a delay that grows while failures repeat ([Restart Backoff](#restart-backoff)) — so `Run` must be [re-enterable](#a-continuous-run-is-re-entered).
- At shutdown, the App cancels `ctx` and waits for `Run` to return.

Use this for long-lived background processes such as consumers and watchers.

### Scheduled worker

- Credo calls `Run(ctx)` once per activation of a cron expression; each call does one execution and returns.
- Returning nil is a success; returning an error or panicking is a failure.
- If an activation comes due while the previous run is still in progress, it is skipped (and logged), never queued or run in parallel.
- `RunOnStart` adds one run as soon as the worker starts, before the schedule begins.

Use this for cleanup, reporting, backfills, and other periodic jobs.

---

## Quick Start: Continuous Worker

```go
package main

import (
    "context"
    "log"
    "time"

    "github.com/credo-go/credo"
    "github.com/credo-go/credo/worker"
)

type EmailConsumer struct {
    queue  *Queue
    sender *Sender
}

func NewEmailConsumer(queue *Queue, sender *Sender) *EmailConsumer {
    return &EmailConsumer{queue: queue, sender: sender}
}

func (c *EmailConsumer) Run(ctx context.Context) error {
    for {
        msg, err := c.queue.Receive(ctx) // returns when a message arrives or ctx is done
        if err != nil {
            return err // wraps ctx.Err() at shutdown: a graceful stop; otherwise a restart
        }
        if err := c.sender.Send(ctx, msg); err != nil {
            return err
        }
    }
}

func main() {
    app, err := credo.New()
    if err != nil {
        log.Fatal(err)
    }

    app.Provide[*Queue](NewQueue)
    app.Provide[*Sender](NewSender)
    app.Provide[*EmailConsumer](NewEmailConsumer)

    workers := worker.Use(app)
    workers.ContinuousProvided[*EmailConsumer]("email-consumer", worker.ContinuousConfig{
        Restart: worker.Restart{MinDelay: 5 * time.Second},
    })

    if err := app.Run(); err != nil {
        log.Fatal(err)
    }
}
```

Important points:

- The container builds the consumer, so the App knows it depends on the queue and the sender: it starts the worker after them and stops it before them.
- `Run` blocks until shutdown or a real failure. Returning nil or `ctx.Err()` — wrapped or not — after `ctx` is cancelled is a graceful stop. An error that carries anything else is a failure even then: `errors.Join(ctx.Err(), flushErr)` is recorded and logged, so a final write that failed during shutdown is never lost. A client that reports a cancelled call in words of its own needs [one translation](#reporting-a-cancelled-call-at-shutdown).
- Restarts are unlimited unless you set `Restart.Limit`. `MinDelay: 5 * time.Second` sets the first wait; repeated failures back off from there up to a one-minute cap.
- `Queue`, `Sender` and their constructors stand for your application's dependencies.

---

## Quick Start: Scheduled Worker

```go
type CleanupWorker struct {
    repo *CleanupRepository
}

func NewCleanupWorker(repo *CleanupRepository) *CleanupWorker {
    return &CleanupWorker{repo: repo}
}

func (w *CleanupWorker) Run(ctx context.Context) error {
    return w.repo.DeleteExpired(ctx) // one execution, then return
}

func main() {
    app, err := credo.New()
    if err != nil {
        log.Fatal(err)
    }

    app.Provide[*CleanupRepository](NewCleanupRepository)
    app.Provide[*CleanupWorker](NewCleanupWorker)

    workers := worker.Use(app)
    workers.ScheduledProvided[*CleanupWorker]("cleanup", "0 */6 * * *", worker.ScheduledConfig{
        RunOnStart:             true,
        RunTimeout:             10 * time.Minute,
        MaxConsecutiveFailures: 5,
    })

    if err := app.Run(); err != nil {
        log.Fatal(err)
    }
}
```

Important points:

- `Run` does one cleanup execution and returns.
- `RunOnStart` runs once as soon as the worker starts, before waiting for the first cron activation.
- `RunTimeout` gives every run a budget; see [Timeouts](#timeouts).
- After five consecutive failures the worker is failed until the application restarts.
- Every replica of the application runs this schedule; see [Every replica runs every schedule](#every-replica-runs-every-schedule).

---

## `worker.Func`

For small jobs you do not need a struct type. `worker.Func` adapts a function, like `http.HandlerFunc`:

```go
logger := app.Logger()
workers.Continuous("heartbeat", worker.Func(func(ctx context.Context) error {
    ticker := time.NewTicker(30 * time.Second)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return nil
        case <-ticker.C:
            logger.InfoContext(ctx, "still alive")
        }
    }
}))
```

A method value works too: `workers.Scheduled("daily-report", "0 6 * * *", worker.Func(reports.Send))`. A function is registered by value, so read [Registering by value](#registering-by-value) before you give it a database to use.

---

## Registration

### The supervisor

`worker.Use(app)` returns a new `*worker.Supervisor` and registers nothing by itself: it binds nothing into the container, installs no hook and adds no component. It logs through the App's logger with `module=worker`. Create it once in the composition root and hand it to the modules that register workers:

```go
// RegisterBilling is the billing module's registration: it takes the
// supervisor the composition root created.
func RegisterBilling(app *credo.App, workers *worker.Supervisor) {
    app.Provide[*InvoiceWorker](NewInvoiceWorker)
    workers.ScheduledProvided[*InvoiceWorker]("invoice-worker", "@every 1m", worker.ScheduledConfig{
        RunTimeout: 15 * time.Second,
    })
}
```

The supervisor is not in the container. A controller that wants it as a constructor parameter — an admin endpoint, say — gets it from a binding you write: `app.ProvideValue(workers)`. Calling `worker.Use` more than once gives independent supervisors; worker names stay unique across all of them.

### Names

The name identifies the registration: it is the `worker` attribute of every log line, `Name` in the snapshot, the component name `worker:<name>` under which `/ready` and the lifecycle's error reports name the worker, and `Worker` in [`CurrentRun`](#run-context). It must be non-empty, unique within the App, and free of surrounding whitespace and control characters; names are never trimmed for you. Keep names stable — dashboards and alerts will key on them.

### Registration window and misuse

Register workers before `app.Finalize()` (or before `Run`, which finalizes implicitly). Registration does no I/O and checks everything it can at the call: a misuse panics there, with a message naming the call, the problem and the remedy:

```text
worker: Continuous("order-consumer"): Restart.MaxDelay 1s is below MinDelay 3s (the default); set MinDelay to at most 1s, or raise MaxDelay
worker: Continuous("x"): duplicate worker name "x"; give each worker its own name
worker: Scheduled("report") after app.Finalize; register workers before Finalize
```

A nil worker, a malformed name, more than one configuration, a negative value and a cron expression that does not parse panic the same way. A schedule that comes from configuration is checked first with `worker.ParseSchedule`, which returns the error instead of panicking ([Configuration](#configuration)). What only the whole graph reveals — a provided worker whose `T` has no binding — is an error `Finalize` returns, and a constructor that fails in the start phase fails the start.

### Provided workers

`ContinuousProvided[T]` and `ScheduledProvided[T]` register a worker that the container builds. They record the type, not an instance: the worker's component is a constructor over `T`, which the start phase builds after `T`'s dependencies have started.

- `app.Provide[T]` and the registration may come in either order, both before `Finalize`.
- `T` must have a `Run` method — the constraint is `T worker.Worker`, so anything else does not compile. `T` may be an interface bound with `app.Alias`.
- A `T` without a binding fails `Finalize` with the dependency path. A constructor error, a panic or a nil `T` fails the start: what was built is rolled back, and `Run` returns a `*credo.LifecycleError` naming `worker:<name>`.
- The worker stops **before** the components `T` depends on: the database a consumer writes to is still open while the consumer flushes its last batch. If `T` itself has `Shutdown`, it is a component too and is shut down after the worker.
- One `T` registered under two names panics, in one supervisor or in two: the container hands every registration the same instance, and two loops would share it.

### Registering by value

`Continuous` and `Scheduled` take a value you built. The App cannot see what that value uses — a closure's captures and a struct's fields are invisible to the dependency graph — so the worker has **no edges**: it takes its place in its tier by registration order alone, and nothing orders it after the database it writes to at start or before it at shutdown. A worker that uses infrastructure — a database, a client, a queue — is therefore registered in its provided form, so the graph orders it in both directions. Register by value only what is self-contained: a heartbeat, a ticker that logs, a function over values that are not components.

---

## Tiers

Each worker's component belongs to one of the App's two drain tiers ([dependency-injection guide](dependency-injection.md#two-tiers)):

| Kind | Default tier | Stops |
| --- | --- | --- |
| scheduled | `credo.TierIngress` | first, concurrently with the HTTP drain, so no run starts while requests drain |
| continuous | `credo.TierInternal` | after the HTTP drain, before the components it depends on |

The defaults fit the common case. A continuous worker that consumes what your HTTP handlers enqueue must still be running while the last requests finish, and the internal tier keeps it running until the HTTP drain is done — then it stops, and only then the database it writes to. A scheduled worker originates its own runs, so it stops with the listener.

Choose the ingress tier for a continuous worker when work enters the process through it — a consumer of an external queue or broker, a watcher of an outside source — so it stops taking new work while the listener does:

```go
workers.ContinuousProvided[*BrokerConsumer]("payments-events", worker.ContinuousConfig{
    Tier: credo.TierIngress, // work enters the process here: stop taking it with the listener
})
```

The two mistakes are not equal: an external consumer left internal merely stops a little later, while an in-process consumer moved to ingress stops before the handlers that feed it have finished and loses what they enqueue during the drain. A provided worker of the internal tier whose `T` depends on an ingress component fails `Finalize` with the path; declare the worker `Tier: credo.TierIngress`, or split the ingress component so that what the worker uses is an internal part.

One drain deadline serves both tiers and is spent in order, so a long HTTP drain leaves less time for an internal worker's final flush. A `Run` that ignores cancellation past the deadline is abandoned: the App keeps the components the worker depends on open — nothing closes a database under a run that may still use it — and `Run` returns a `*credo.LifecycleError` naming `worker:<name>`.

An App with workers has something to start. Served through `ServeHTTP` by a server you own, or by `httptest`, it is started first with `app.Start(ctx)` (`testutil.Start(t, app)` in tests); until then `ServeHTTP` panics. See [Testing Workers](#testing-workers) and the [deployment guide](deployment.md#an-httpserver-you-own).

---

## Configuration

Each registration method takes its kind's configuration — no argument for the defaults, one for custom, more than one panics. Zero means the default in every field:

| Field | Kind | Zero means |
| --- | --- | --- |
| `Tier` | both | the kind's default tier ([Tiers](#tiers)) |
| `Restart.Disabled` | continuous | restarts enabled; `true` makes the first failed run terminal and cannot be combined with the other `Restart` fields |
| `Restart.Limit` | continuous | unlimited restarts; N allows the first run plus at most N restarts |
| `Restart.MinDelay` | continuous | `worker.DefaultMinRestartDelay` (3 s), the first and shortest wait |
| `Restart.MaxDelay` | continuous | the larger of `worker.DefaultMaxRestartDelay` (1 min) and `MinDelay`; a positive value below `MinDelay` panics |
| `RunOnStart` | scheduled | no extra run at start |
| `RunTimeout` | scheduled | no timeout |
| `MaxConsecutiveFailures` | scheduled | unlimited failures |
| `UnreadyWhenFailed` | both | off ([Readiness](#readiness)) |
| `UnreadyUntilFirstSuccess` | scheduled | off |
| `UnreadyAfterSuccessAge` | scheduled | off |

A setting of the other kind cannot be written: `ContinuousConfig` has no schedule fields and `ScheduledConfig` no restart policy.

The configuration lives in code. The package reads no configuration section, so a setting that should come from the environment — a schedule that differs per deployment, a restart delay you tune in production — is a typed section of your own, read once in the composition root and passed in:

```go
type WorkerSettings struct {
    CleanupSchedule string        // workers.cleanup_schedule
    RestartMinDelay time.Duration // workers.restart_min_delay
    RestartMaxDelay time.Duration // workers.restart_max_delay
}

settings := app.MustGetConfig[WorkerSettings]("workers")
if _, err := worker.ParseSchedule(settings.CleanupSchedule); err != nil {
    log.Fatalf("workers.cleanup_schedule: %v", err)
}

workers.ContinuousProvided[*OrderConsumer]("order-consumer", worker.ContinuousConfig{
    Restart: worker.Restart{MinDelay: settings.RestartMinDelay, MaxDelay: settings.RestartMaxDelay},
})
workers.ScheduledProvided[*SessionCleanup]("session-cleanup", settings.CleanupSchedule)
```

Zero settings fall through to the package defaults. A changed schedule, limit or timeout takes effect at the next start: a [config reload](#reloadable-settings) does not re-register workers.

### Supported schedule formats

- standard 5-field cron: `0 */6 * * *` — with lists (`1,15`), ranges (`1-5`), steps (`*/10`), month/weekday names (`jan`, `sat`), `?` as `*`, and `7` as Sunday
- descriptors: `@hourly`, `@daily` (alias `@midnight`), `@weekly`, `@monthly`
- intervals: `@every 5m`, `@every 90s`, `@every 1h30m`

Cron schedules use the process's local time zone and fire at second 0 of the matching minute. There is no per-worker time zone, and `TZ=`/`CRON_TZ=` prefixes are rejected: run the process in the zone the schedules are written for. Go reads the local zone from the `TZ` environment variable on Unix — a Linux container can set `TZ=Europe/Istanbul`, and an image without zoneinfo files can embed them with `import _ "time/tzdata"` — and from the operating system's time-zone setting on Windows. `@every` measures elapsed time and does not replace a calendar rule: "09:00 local time every day" and `@every 24h` are different schedules. The 6-field seconds form is not supported — for sub-minute periods use `@every`. `@every` needs a positive whole number of seconds: `@every 0s`, `@every -1h` and `@every 1500ms` are rejected. As in crontab(5), when both day-of-month and day-of-week are restricted, the schedule fires when either matches.

---

## Restart Backoff

A continuous worker that fails is restarted after a wait. The first wait is `Restart.MinDelay` (3 s by default). While failures repeat, each wait is drawn from a window that doubles up to `Restart.MaxDelay` (1 minute by default):

| Failure in a row | Wait with the defaults |
| --- | --- |
| 1st | 3s |
| 2nd | 3–6s |
| 3rd | 6–12s |
| 4th | 12–24s |
| 5th | 24–48s |
| 6th and later | 30–60s |

The wait is randomized inside the window, so replicas failing on the same dependency do not retry in lockstep. It is never shorter than `MinDelay` and never longer than `MaxDelay`.

A run that lasted at least `MaxDelay` resets the sequence: its failure waits `MinDelay` again. A worker that runs for hours and fails once a day therefore restarts after 3s, not after a minute. The reset affects only the wait; `Restarts` and the `Limit` budget keep counting.

**The trade-off.** A worker whose dependency is down for an hour restarts about 80 times instead of 1,200 with a fixed 3s delay — and writes that many `worker run failed` lines and makes that many connection attempts. The price is recovery time: once the dependency is back, the worker may wait up to `MaxDelay` before it tries again. Raise it for dependencies that tend to stay down for long; lower it where a minute without the worker is too long.

**Visibility.** Each failure line carries the chosen wait, `next_restart_in=24.3s`, while the worker waits in status `backoff`, and the snapshot reports the resolved `min_delay` and `max_delay`. The attribute is omitted when no restart follows: the `Limit` budget was just exhausted, restarts are disabled, or the worker is stopping.

**Fixed delay.** Set both delays to the same value:

```go
workers.ContinuousProvided[*Poller]("poller", worker.ContinuousConfig{
    Restart: worker.Restart{MinDelay: 10 * time.Second, MaxDelay: 10 * time.Second},
})
```

A `MinDelay` of a minute or more alone is fixed as well, since a zero `MaxDelay` resolves to it.

**Limits.** Backoff makes `Limit` take longer to exhaust: with the defaults, `Limit: 5` reaches `failed` after roughly 48–93s of waiting instead of 15s, and an `UnreadyWhenFailed` readiness drop comes correspondingly later. `Restart{Disabled: true}` makes the first failure terminal.

---

## Timeouts

`ScheduledConfig.RunTimeout` bounds every run of a scheduled worker, including the `RunOnStart` run. When the budget runs out, Credo cancels the run's context; the worker should notice and return.

```go
func (w *ReportWorker) Run(ctx context.Context) error {
    for _, tenant := range w.tenants {
        if err := w.buildReport(ctx, tenant); err != nil {
            if errors.Is(context.Cause(ctx), worker.ErrRunTimeout) {
                return fmt.Errorf("report for %s: budget exhausted: %w", tenant, err)
            }
            return err
        }
    }
    return nil
}
```

What to expect:

- A run cut short by its timeout is a **failure, even if `Run` returns nil**. It is recorded as `worker: run timed out after 15s…`, logged with `timed_out=true`, and counts toward `MaxConsecutiveFailures`. A half-finished run can therefore never stamp `LastSucceededAt` or satisfy a readiness condition.
- `errors.Is(context.Cause(ctx), worker.ErrRunTimeout)` tells the budget running out from the worker being stopped. Whichever happens first wins, because the context keeps its first cancellation cause: when the stop comes first, a nil return is a graceful stop; when the timeout fires first and the stop follows, the run is a timed-out failure, nil return included.
- The timeout is cooperative: it cancels the context and never kills the goroutine. A `Run` that ignores its context keeps the worker busy past the budget, and activations that pass meanwhile are skipped. There is never more than one run of a worker at a time.
- Continuous workers have no run timeout — their `Run` is meant to last until the worker stops. Put timeouts on the individual operations inside the loop instead.

Replace hand-written `context.WithTimeout` wrappers inside scheduled `Run` methods with `RunTimeout`: the budget becomes visible in the snapshot and a timed-out run is classified consistently.

---

## Writing a Continuous `Run`

### A continuous `Run` is re-entered

A restart calls `Run` again **on the same value**. Build what one run uses — a subscription, a watcher, a connection — inside `Run`, and release it before `Run` returns, so the next call starts clean:

```go
func (w *OrderWatcher) Run(ctx context.Context) error {
    sub, err := w.bus.Subscribe(ctx, "orders") // a per-run resource, built inside Run
    if err != nil {
        return err
    }
    defer sub.Close() // released before Run returns, so a restart starts clean

    for {
        select {
        case <-ctx.Done():
            return nil
        case ev, ok := <-sub.Events():
            if !ok {
                return errors.New("orders subscription closed")
            }
            w.handle(ctx, ev)
        }
    }
}
```

A subscription opened in the constructor and closed when `Run` returns would leave every restart reading from a closed subscription. Long-lived clients the worker shares with others — a database pool, an HTTP client — stay constructor parameters; it is what `Run` opens for itself that it must close. A worker that cannot be re-entered is registered with `Restart{Disabled: true}`, so its first failure is terminal.

### Reporting a cancelled call at shutdown

At shutdown the worker's context is cancelled, and a blocking call inside `Run` returns early. When the call's error wraps `ctx.Err()` — as `database/sql`, `net/http` and most Go clients do — returning it is a graceful stop. Some clients report the cancellation in words of their own, an error that does not wrap `ctx.Err()`, and Credo records that as a failure: the stop then logs `worker run failed` and sets `LastError`.

Translate that one error, at the call that returns it, and only while the context is done:

```go
func (r *Relay) Run(ctx context.Context) error {
    for {
        batch, err := r.client.Fetch(ctx)
        if err != nil {
            if ctx.Err() != nil && errors.Is(err, broker.ErrCanceled) {
                return ctx.Err() // the client's word for our own cancellation: a graceful stop
            }
            return err // a real failure, during shutdown too
        }
        if err := r.store.Save(ctx, batch); err != nil {
            return err
        }
    }
}
```

Never write the blanket form, `if ctx.Err() != nil { return nil }`, at the end of `Run` or around its errors: it also turns a real failure that happens during shutdown — a final batch that could not be written — into a clean stop, and the data loss goes unreported. The same holds for a final flush: give it a short budget of its own, since the worker's context is already cancelled, and return its error as it is:

```go
case <-ctx.Done():
    // Write what is buffered under a budget of its own: ctx is already cancelled.
    flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
    defer cancel()
    return b.write(flushCtx) // nil: a graceful stop; an error: a recorded failure
```

### Finite background work

A continuous worker must keep running until it is stopped. Returning nil early is treated as a bug: Credo logs `worker run failed` with `unexpected_exit=true` and restarts the worker with the [restart backoff](#restart-backoff). This catches a consumer loop that quietly ended — `for msg := range ch { … }; return nil` on a closed channel — before it goes unnoticed.

For work that genuinely finishes, pick one of two homes:

- **Startup should wait for it** (priming a cache the first request needs): use `app.OnStart`, or the `Start` of a component.

  ```go
  app.OnStart(func(ctx context.Context) error {
      return cache.Warm(ctx)
  })
  ```

- **It should run in the background** without delaying startup: do the work, then wait to be stopped.

  ```go
  func (w *Warmup) Run(ctx context.Context) error {
      if err := w.cache.Warm(ctx); err != nil {
          return err // retried with the restart backoff
      }
      <-ctx.Done() // a continuous worker lives until it is stopped
      return nil
  }
  ```

Recurring finite work is a scheduled worker.

---

## Every Replica Runs Every Schedule

The scheduler is per process. Three replicas of an application run a scheduled cleanup three times, at the same activation. Make each scheduled job idempotent — deleting expired rows twice deletes nothing the second time — or guard it with an application-level lock: for SQL, a PostgreSQL advisory lock (`pg_try_advisory_lock`) taken at the start of the run, or a claim row that one replica wins per activation. For a cron expression, `CurrentRun(ctx).ScheduledAt` names the same activation on every replica, which makes it a natural claim key; an `@every` grid starts when each process starts, so its activations differ between replicas. Activations can also be skipped, and a timed-out run may have done part of its work, so idempotence pays off on a single replica too.

---

## Run Context

The context passed to `Run` carries the run's metadata. `worker.CurrentRun(ctx)` returns it:

```go
func (w *InvoiceWorker) Run(ctx context.Context) error {
    run, _ := worker.CurrentRun(ctx)
    w.log.InfoContext(ctx, "processing invoices",
        "run_id", run.ID, "scheduled_at", run.ScheduledAt)
    return w.repo.ProcessPending(ctx)
}
```

| `RunInfo` field | Value |
| --- | --- |
| `Worker` | the registration name |
| `ID` | a fresh identifier per run, equal to the `run_id` of Credo's own log lines for that run |
| `ScheduledAt` | the intended activation time; zero for continuous workers and for the `RunOnStart` run |

`CurrentRun` returns false for a context that is not a run context — in a unit test that calls `Run` directly, for example — so code that reads it works there too. The context is cancelled when the worker stops and, with `RunTimeout`, when the run's budget runs out; always watch `ctx.Done()` in long-running work. Restart and failure counters are not in the context; read them from the [snapshot](#observing-worker-state).

---

## Observing Worker State

`workers.Snapshot()` returns one `worker.Info` per registered worker, in registration order, and `workers.Lookup(name)` returns the worker registered under exactly that name, or false. Both work from the moment of registration — before `Finalize`, before the App starts — and read memory only. An admin endpoint serves them directly:

```go
admin := app.Group("/admin") // protect it: auth middleware, a separate listener, or both
admin.GET("/workers", func(ctx *credo.Context) error {
    return ctx.Response().JSON(http.StatusOK, workers.Snapshot())
})
admin.GET("/workers/{name}", func(ctx *credo.Context) error {
    info, ok := workers.Lookup(ctx.Request().RouteParam("name"))
    if !ok {
        return credo.ErrNotFound
    }
    return ctx.Response().JSON(http.StatusOK, info)
})
```

A controller built by the container takes `*worker.Supervisor` as a constructor parameter once you bind it with `app.ProvideValue(workers)`.

Each `Info` carries:

- `Name`, `Kind` (`worker.KindContinuous` / `worker.KindScheduled`) and, for a scheduled worker, `Schedule` — the expression as registered
- `Continuous` or `Scheduled` — exactly one is set, holding the **resolved** configuration the worker runs with: the tier, the restart floor and cap after defaults, the timeout and the limits. Each snapshot holds its own copy
- `Status`:

  | Status | Meaning |
  | --- | --- |
  | `pending` | registered; its component has not started, or a continuous worker has not yet begun its first run |
  | `running` | a run is executing |
  | `backoff` | continuous only: waiting to restart after a failure — unhealthy, recovering |
  | `waiting` | scheduled only: waiting for the next activation, whatever the last run did |
  | `stopped` | the worker was stopped with the App; it will not run again |
  | `failed` | a limit was exhausted, restarts are disabled and a run failed, or the schedule has no future activation; permanent until the process restarts |

- `Restarts` (continuous) and `ConsecutiveFailures` (scheduled) — a scheduled worker's signal of trouble between activations is `consecutive_failures > 0`
- `LastStartedAt` (start of the latest run), `LastSucceededAt` (end of the latest successful scheduled run; always zero for continuous workers) and `LastError` (the latest failure, never with a stack trace; a successful scheduled run clears it)

**JSON.** `Info` is shaped for direct encoding: field names are snake_case, the nested configuration included (`continuous.restart.min_delay`, `scheduled.run_timeout`, `consecutive_failures`, `last_error`, …); `schedule`, `continuous`, `scheduled`, `last_started_at`, `last_succeeded_at` and `last_error` are omitted while empty; every other field is always present. Durations encode as integer nanoseconds under Credo's JSON response profile — `"run_timeout": 15000000000` is 15 seconds — and `tier` as `"ingress"` or `"internal"`. The spec shows a [full example](../specs/worker.md#json-shape).

---

## Reading the Logs

Every worker that starts writes exactly one `worker started` line and exactly one `worker stopped` line, both at Info:

```text
level=INFO msg="worker started" module=worker worker=invoice-worker kind=scheduled schedule="@every 1m" next_run=…
level=INFO msg="worker stopped" module=worker worker=invoice-worker kind=scheduled status=stopped reason=shutdown
```

| Message | Level | When |
| --- | --- | --- |
| `worker started` | Info | the worker's loop started; a scheduled worker adds `schedule` and either `next_run` or `run_on_start=true` |
| `worker run failed` | Error | a run failed, for both kinds: `error`, `run_id`, `duration`; `restarts` and, when a restart follows, `next_restart_in` for a continuous worker; `scheduled_at` and `consecutive_failures` for a scheduled one; `timed_out=true`, `unexpected_exit=true` or `stack` when they apply |
| `worker run completed` | Debug | a scheduled run succeeded |
| `worker activations skipped` | Warn | runs outlasted their schedule: one line per resumption with `skipped`, `first_scheduled_at`, `last_scheduled_at` |
| `worker failed` | Error | the worker became failed, once, with `reason`: `restart_limit`, `restart_disabled`, `failure_limit` or `schedule_exhausted` |
| `worker stopped` | Info | the loop ended: `status` and `reason` (`shutdown` or `failed`) |

- `worker failed` is the line to alert on; the `reason=failed` stop line that follows is bookkeeping.
- A successful scheduled run logs at **Debug** only; a worker running every minute would otherwise write 1,440 Info lines a day. Use the snapshot or a readiness condition to answer "is it alive", and the logs as the audit trail.
- To join Credo's lines with your own for one run, log `CurrentRun(ctx).ID` as `run_id`.

---

## Readiness

A worker can take the instance out of rotation through `/ready` (`app.UseHealth()`). Nothing is bound automatically — a failed metrics reporter should not take the instance out of rotation — so each condition is opt-in on the configuration:

```go
app.UseHealth()

workers.ScheduledProvided[*Recovery]("recovery", "@every 5m", worker.ScheduledConfig{
    RunOnStart:               true,
    UnreadyUntilFirstSuccess: true,             // unready until the first run succeeds
    UnreadyWhenFailed:        true,             // unready once the worker is failed
    UnreadyAfterSuccessAge:   15 * time.Minute, // unready when the last success is older
    MaxConsecutiveFailures:   3,
})
```

- `UnreadyUntilFirstSuccess` is a startup barrier: the instance stays unready until the worker's first run succeeds, then stays ready even if later runs fail. A timed-out run never satisfies it. Pair it with `RunOnStart` unless waiting for the first cron activation is intended.
- `UnreadyWhenFailed` reports unready once the worker reaches `failed` — a limit exhausted, or a failure under `Restart.Disabled`. For a continuous worker this includes one that keeps returning early. With unlimited restarts and failures a worker never reaches `failed`, so set a limit for this condition to mean anything. Use it only for workers the instance cannot serve without: every replica hitting the same persistent failure leaves rotation together.
- `UnreadyAfterSuccessAge` reports unready when the last success is older than the limit; it is not applied before the first success, so combine it with `UnreadyUntilFirstSuccess` to cover that window.
- `UnreadyWhenFailed` exists on both configurations; the other two only on `ScheduledConfig`, because a continuous worker never records a success.

The worker's component answers readiness only when a condition is set, and `/ready` reports it as a check named `worker:<name>`, evaluated in memory, without I/O. Worker registration and `app.UseHealth()` may come in either order.

### Escalating a terminal failure

Readiness takes an instance out of rotation and restarts nothing. A worker that reached `failed` leaves a process that is alive, unready, and never restarted: no component ends the App ([deployment guide](deployment.md#the-limit-of-readiness)). When a process restart is the right repair — the worker holds state a fresh process rebuilds — tie a liveness check to the worker through `Lookup`:

```go
app.UseHealth()

workers.ContinuousProvided[*OrderConsumer]("order-consumer", worker.ContinuousConfig{
    Restart: worker.Restart{Limit: 5}, // with unlimited restarts it never fails
})

app.AddLivenessCheck("order-consumer", credo.HealthCheckFunc(func(context.Context) error {
    info, ok := workers.Lookup("order-consumer")
    switch {
    case !ok:
        return errors.New(`no worker named "order-consumer"`)
    case info.Status == worker.StatusFailed:
        return fmt.Errorf("worker %q failed: %s", info.Name, info.LastError)
    }
    return nil
}))
```

The check fails when the worker is failed **or when `Lookup` does not know the name** — a renamed or forgotten registration is a mistake to report, never a pass. `/health` then answers 503 and the orchestrator restarts the container. Know the cost first: when the failure comes from a dependency every replica shares — the database, the broker — every replica's worker fails, every liveness probe fails, and the orchestrator restarts every replica at once, which cannot fix the dependency and adds a restart storm to its outage. Tie liveness only to failures a restart repairs, and give the worker a restart budget that outlasts a dependency's ordinary blips.

---

## Reloadable Settings

Workers are registered once and run for the life of the process; a [config reload](configuration.md#reloading-configuration) does not re-register them or change their configuration. For a setting a worker should honour without a restart — a batch size, a concurrency cap, a polling interval — give the worker an atomic holder and swap it from an `OnConfigChange[T]` subscriber:

```go
type Sync struct {
    cfg atomic.Pointer[SyncConfig]
}

func (s *Sync) Run(ctx context.Context) error {
    for {
        cfg := s.cfg.Load() // read the live value on every iteration
        if err := s.runBatch(ctx, cfg.BatchSize); err != nil {
            return err
        }
        select {
        case <-ctx.Done():
            return nil
        case <-time.After(cfg.Interval):
        }
    }
}

// After Finalize:
syncer := app.MustResolve[*Sync]()
app.OnConfigChange("sync", func(ctx context.Context, next SyncConfig) error {
    syncer.cfg.Store(&next)
    return nil
})
```

A changed cron expression is restart-only; the reload logs it as `restart required` when nothing subscribes to its section.

---

## Testing Workers

A worker is a type with a `Run` method, so its logic is tested by calling `Run` directly — no supervisor, no App:

```go
func TestInvoiceWorker(t *testing.T) {
    repo := &fakeInvoices{}
    w := NewInvoiceWorker(credo.Infra{Logger: slog.New(slog.DiscardHandler)}, repo)

    if err := w.Run(t.Context()); err != nil {
        t.Fatal(err)
    }
    if repo.processed != 1 {
        t.Fatalf("processed = %d, want 1", repo.processed)
    }
}
```

For a continuous worker, run `Run` on a goroutine with a context you cancel, and assert that it returns nil or a context error once cancelled. Timing behaviour inside your own workers is easiest to test with `testing/synctest`.

The registration — names, schedules, tiers, limits, timeouts — is tested through the snapshot without running anything, since it reports the resolved configuration from the moment of registration:

```go
func TestBillingRegistration(t *testing.T) {
    app := testutil.NewApp(t)
    workers := worker.Use(app)
    RegisterBilling(app, workers)

    info, ok := workers.Lookup("invoice-worker")
    if !ok {
        t.Fatal(`no worker named "invoice-worker"`)
    }
    if got := info.Scheduled.RunTimeout; got != 15*time.Second {
        t.Fatalf("invoice-worker run timeout = %s, want 15s", got)
    }
}
```

To run the workers inside the App, start it without a listener. `app.Start` runs the start phase — the workers' loops included — and `app.Shutdown` stops them, so a test can assert that every worker stops within the deadline:

```go
func TestBillingWorkersStop(t *testing.T) {
    app := testutil.NewApp(t, testutil.WithWiring(wire))
    workers := worker.Use(app)
    RegisterBilling(app, workers)

    if err := app.Start(t.Context()); err != nil {
        t.Fatal(err)
    }
    ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
    defer cancel()
    if err := app.Shutdown(ctx); err != nil {
        t.Fatal(err) // names worker:<name> when a Run ignored its cancellation
    }
    for _, info := range workers.Snapshot() {
        if info.Status != worker.StatusStopped {
            t.Errorf("%s: status %s, want stopped", info.Name, info.Status)
        }
    }
}
```

An integration test that drives the workers through HTTP uses `testutil.Start`, which starts the App and shuts it down when the test ends, after the test server has drained:

```go
app := testutil.NewApp(t, testutil.WithWiring(wire))
workers := worker.Use(app)
RegisterBilling(app, workers)

testutil.Start(t, app) // starts the workers; the App shuts down when the test ends
srv := httptest.NewServer(app)
t.Cleanup(srv.Close) // drained before the workers stop
```

Swap a worker's dependencies with `testutil.WithOverride[T]` as for any binding; the provided worker is built from the override.

---

## Best Practices

- register a worker that uses infrastructure in its provided form, so the App starts it after its dependencies and stops it before them
- continuous workers own their loop, are re-enterable, and return only when stopped or on a real failure; scheduled workers do one execution and return
- treat shutdown as normal control flow: return nil or `ctx.Err()` once `ctx.Done()` closes, translate a client's own "cancelled" error at the call, and never swallow errors with a blanket `ctx.Err()` check
- keep the default tiers unless work enters the process through the worker
- give scheduled workers a `RunTimeout` budget instead of hand-rolled timeouts inside `Run`
- make scheduled jobs idempotent or lock-guarded: every replica runs every schedule
- keep worker names stable and unique; they key logs, snapshots, readiness and liveness checks
- keep the supervisor from `worker.Use` in the composition root and pass it to modules and admin endpoints; there is nothing to resolve

---

## Related Guides

- [Getting Started](getting-started.md)
- [Dependency Injection Guide](dependency-injection.md)
- [Deployment Guide](deployment.md)
- [Data Access Guide](data-access.md)
- [Pre-v1 Migration Guide](pre-v1-migration.md#workers)
