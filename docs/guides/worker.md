# Worker Guide

This guide explains how to run background tasks with Credo's `worker/` package. For the exact contracts — outcome classification, admission order, log attributes — see the [Worker Spec](../specs/worker.md); for the reasoning behind them, [ADR-023](../adr/023-worker-system.md).

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

A worker is anything with a `Run(ctx context.Context) error` method. You give it a name when you register it, and the registration decides its kind.

### Continuous worker

No schedule is configured.

- Credo calls `Run(ctx)` once at startup.
- The worker owns its loop and **must keep running until `ctx` is cancelled**.
- If `Run` returns an error, panics, or returns nil while the application is still running, Credo records a failure and restarts it after a delay that grows while failures repeat ([Restart backoff](#restart-backoff)).
- `app.Shutdown(ctx)` cancels `ctx` and waits for `Run` to return.

Use this for long-lived background processes such as consumers and watchers.

### Scheduled worker

`worker.WithSchedule(...)` is configured.

- Credo calls `Run(ctx)` once per activation; each call does one execution and returns.
- Returning nil is a success; returning an error or panicking is a failure.
- If an activation comes due while the previous run is still in progress, it is skipped (and logged), never queued or run in parallel.
- `worker.WithStartImmediately()` adds one run at startup before the schedule begins.

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
    queue  Queue
    sender Sender
}

func (c *EmailConsumer) Run(ctx context.Context) error {
    for {
        msg, err := c.queue.Receive(ctx) // returns when a message arrives or ctx is done
        if err != nil {
            if ctx.Err() != nil {
                return nil // shutdown: a graceful stop
            }
            return err // a failure: Credo restarts the worker
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

    consumer := &EmailConsumer{queue: newQueue(), sender: newSender()}

    if err := worker.Register(app, "email-consumer", consumer,
        worker.WithRestartDelay(5*time.Second),
    ); err != nil {
        log.Fatal(err)
    }

    if err := app.Run(); err != nil {
        log.Fatal(err)
    }
}
```

Important points:

- `Run` blocks until shutdown or a real failure. Returning nil (or `ctx.Err()`, wrapped or not) after `ctx` is cancelled is a graceful stop. An error that carries anything else is a failure even then: `errors.Join(ctx.Err(), flushErr)` is recorded and logged, so a final write that failed during shutdown is never lost.
- Restarts are unlimited unless you set `WithMaxRestarts`. `WithRestartDelay(5*time.Second)` sets the first wait; repeated failures back off from there up to a one-minute cap.
- `newQueue()` and `newSender()` are placeholders for your application's dependencies; the [DI Integration](#di-integration) section shows the constructor-injected form.

---

## Quick Start: Scheduled Worker

```go
type CleanupWorker struct {
    repo CleanupRepository
}

func (w *CleanupWorker) Run(ctx context.Context) error {
    return w.repo.DeleteExpired(ctx)
}

func main() {
    app, err := credo.New()
    if err != nil {
        log.Fatal(err)
    }

    cleanup := &CleanupWorker{repo: newCleanupRepo()}

    if err := worker.Register(app, "cleanup", cleanup,
        worker.WithSchedule("0 */6 * * *"),
        worker.WithStartImmediately(),
        worker.WithRunTimeout(10*time.Minute),
        worker.WithMaxConsecutiveFailures(5),
    ); err != nil {
        log.Fatal(err)
    }

    if err := app.Run(); err != nil {
        log.Fatal(err)
    }
}
```

Important points:

- `Run` does one cleanup execution and returns.
- `WithStartImmediately()` runs once at startup before waiting for the first cron activation.
- `WithRunTimeout` gives every run a budget; see [Timeouts](#timeouts).
- After five consecutive failures the worker is marked failed until the application restarts.

---

## `worker.Func`

For small jobs you do not need a struct type. `worker.Func` adapts a function, like `http.HandlerFunc`:

```go
if err := worker.Register(app, "heartbeat", worker.Func(func(ctx context.Context) error {
    ticker := time.NewTicker(30 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return nil
        case <-ticker.C:
            log.Println("still alive")
        }
    }
})); err != nil {
    log.Fatal(err)
}
```

A method value works too: `worker.Func(reports.Send)`.

---

## Registration and Lifecycle

`worker.Register(app, name, w, opts...)` does more than store the worker:

1. validates the name and the options, and parses the schedule
2. creates the worker pool on first registration and publishes it in DI as a protected binding (`Replace[*Pool]` is rejected)
3. attaches pool startup to `app.OnStart` and pool drain to `app.OnDrain`, so workers finish before any DI resource is shut down

The normal lifecycle is:

```text
worker.Register(...) -> app.Run() -> workers start (after the port is bound, before traffic)
app.Shutdown(ctx) -> worker contexts cancel -> pool waits for exit -> DI resources close
```

A worker's bounded cleanup after cancellation — flushing a last batch, acknowledging in-flight messages — therefore always runs against still-open resources, whatever order the worker and the resource were registered in. A worker that ignores cancellation past the shutdown deadline is reported as an incomplete drain task and teardown proceeds.

**Names.** The name identifies the registration: it appears in every log line, in `pool.Workers()`, and in the readiness check `worker:<name>`, and `worker.WorkerName(ctx)` returns it inside `Run`. It must be unique, non-empty, and free of surrounding whitespace and control characters; names are never trimmed for you. Keep names stable — dashboards and alerts will key on them.

**Registration window.** Register workers before `app.Finalize()` (or before `Run()`/`RunContext()`, which finalize implicitly); every registration after `Finalize` returns an error. Use `worker.MustRegister` when bootstrap code should panic on a registration error instead of returning it.

---

## DI Integration

Workers fit Credo's constructor-injection model through `worker.RegisterProvided`. Provide the worker like any service and register it by type; Credo resolves it when the pool starts:

```go
type InvoiceWorker struct {
    log  *slog.Logger
    repo InvoiceRepository
}

func NewInvoiceWorker(infra credo.Infra, repo InvoiceRepository) *InvoiceWorker {
    return &InvoiceWorker{log: infra.Logger, repo: repo}
}

func (w *InvoiceWorker) Run(ctx context.Context) error {
    w.log.InfoContext(ctx, "processing invoices", "run_id", worker.RunID(ctx))
    return w.repo.ProcessPending(ctx)
}

func bootstrap(app *credo.App) error {
    app.MustProvide[*InvoiceWorker](NewInvoiceWorker)

    return worker.RegisterProvided[*InvoiceWorker](app, "invoice-worker",
        worker.WithSchedule("@every 1m"),
        worker.WithRunTimeout(15*time.Second),
    )
}
```

How it fits together:

- `RegisterProvided` records the type, not an instance, so it works before `Finalize` — exactly when registration is open — and may come before or after the matching `Provide`.
- The worker is resolved once, in the `OnStart` phase: after `Finalize`, before the server accepts traffic.
- If the type is not provided, or its constructor fails or panics, startup fails with an error naming the worker and the type, and no worker is started.
- The type argument may be an interface bound with `app.Alias`.
- If the worker implements `credo.Shutdowner`, the container closes it after the pool has drained.

Use `worker.Register` with a value when the worker has no DI dependencies or is assembled by hand. See the [Dependency Injection Guide](dependency-injection.md) for broader DI patterns.

---

## Options

Options are kind-specific. Using an option with the wrong kind of worker makes `Register` return an error (and `MustRegister` panic) rather than being ignored.

### Continuous worker options

- `worker.WithMaxRestarts(n)` — `n > 0` allows the first run plus at most `n` restarts; the worker is marked failed when a run fails after the `n`-th restart. `WithMaxRestarts(1)` means "try once more". `n == 0` (the default) means unlimited restarts.
- `worker.WithRestartDelay(d)` — the first and shortest wait before a restart (default 3s, or `worker.restart_delay` from config). Zero means `DefaultRestartDelay` (3s), so a worker that fails instantly is throttled instead of busy-looping.
- `worker.WithMaxRestartDelay(d)` — the longest wait while failures repeat (default 1m, or `worker.max_restart_delay` from config); see [Restart Backoff](#restart-backoff). Zero means `DefaultMaxRestartDelay` (1m), or the restart delay when that is longer; a positive value below the restart delay is a registration error.

Shutdown never consumes restart budget: a restart is counted only when the next run actually starts.

### Scheduled worker options

- `worker.WithSchedule(expr)` — makes the worker scheduled.
- `worker.WithStartImmediately()` — one extra run at startup.
- `worker.WithRunTimeout(d)` — a budget for every run; see [Timeouts](#timeouts).
- `worker.WithMaxConsecutiveFailures(n)` — the worker is marked failed after `n` failed runs in a row; a success resets the streak. `n == 0` (the default) means unlimited.

### Both kinds

- `worker.WithReadiness(policy)` — see [Readiness Integration](#readiness-integration).

### Supported schedule formats

- standard 5-field cron: `0 */6 * * *` — with lists (`1,15`), ranges (`1-5`), steps (`*/10`), month/weekday names (`jan`, `sat`), `?` as `*`, and `7` as Sunday
- descriptors: `@hourly`, `@daily` (alias `@midnight`), `@weekly`, `@monthly`
- intervals: `@every 5m`, `@every 90s`, `@every 1h30m`

Cron schedules use the process's local time zone and fire at second 0 of the matching minute. There is no per-worker time zone, and `TZ=`/`CRON_TZ=` prefixes are rejected: run the process in the zone the schedules are written for. Go reads the local zone from the `TZ` environment variable on Unix — a Linux container can set `TZ=Europe/Istanbul`, and an image without zoneinfo files can embed them with `import _ "time/tzdata"` — and from the operating system's time-zone setting on Windows. `@every` measures elapsed time and does not replace a calendar rule: "09:00 local time every day" and `@every 24h` are different schedules. The 6-field seconds form is not supported — for sub-minute periods use `@every`. `@every` needs a positive whole number of seconds: `@every 0s`, `@every -1h` and `@every 1500ms` are registration errors. As in crontab(5), when both day-of-month and day-of-week are restricted, the schedule fires when either matches.

---

## Restart Backoff

A continuous worker that fails is restarted after a wait. The first wait is the restart delay (`WithRestartDelay`, 3s by default). While failures repeat, each wait is drawn from a window that doubles up to the cap (`WithMaxRestartDelay`, 1 minute by default):

| Failure in a row | Wait with the defaults |
| --- | --- |
| 1st | 3s |
| 2nd | 3–6s |
| 3rd | 6–12s |
| 4th | 12–24s |
| 5th | 24–48s |
| 6th and later | 30–60s |

The wait is randomized inside the window, so replicas failing on the same dependency do not retry in lockstep. It is never shorter than the restart delay and never longer than the cap.

A run that lasted at least the cap resets the sequence: its failure waits the restart delay again. A worker that runs for hours and fails once a day therefore restarts after 3s, not after a minute. The reset affects only the wait; `Restarts` and the `WithMaxRestarts` budget keep counting.

**The trade-off.** A worker whose dependency is down for an hour restarts about 80 times instead of 1,200 with a fixed 3s delay — and writes that many `worker run failed` lines and makes that many connection attempts. The price is recovery time: once the dependency is back, the worker may wait up to the cap before it tries again. Raise the cap (per worker, or per pool with `worker.max_restart_delay`) for dependencies that tend to stay down for long; lower it where a minute without the worker is too long.

**Visibility.** Each failure line carries the chosen wait, `next_restart_in=24.3s`, and `pool.Workers()` reports the effective `RestartDelay` and `MaxRestartDelay`. The attribute is omitted when no restart follows: the `WithMaxRestarts` budget was just exhausted, or the application is shutting down.

**Fixed delay.** Set both options to the same value:

```go
worker.MustRegister(app, "poller", poller,
    worker.WithRestartDelay(10*time.Second),
    worker.WithMaxRestartDelay(10*time.Second),
)
```

`WithRestartDelay(time.Minute)` alone is fixed under the default one-minute cap, but with `worker.max_restart_delay: 5m` in the configuration it grows from one minute to five. Only equal options stay fixed whatever the configuration says.

**Limits.** Backoff makes `WithMaxRestarts(n)` take longer to exhaust: with the defaults, `WithMaxRestarts(5)` reaches `failed` after roughly 48–93s of waiting instead of 15s, and a `FailWhenFailed` readiness check drops correspondingly later.

---

## Timeouts

`worker.WithRunTimeout(d)` bounds every run of a scheduled worker, including the startup run of `WithStartImmediately`. When the budget runs out, Credo cancels the run's context; the worker should notice and return.

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

- A run cut short by its timeout is a **failure, even if `Run` returns nil**. It is recorded as `worker: run timed out after 15s…`, logged with `timed_out=true`, and counts toward `WithMaxConsecutiveFailures`. A half-finished run can therefore never stamp `LastSuccess` or satisfy a readiness barrier.
- `errors.Is(context.Cause(ctx), worker.ErrRunTimeout)` tells the budget running out from the application shutting down. Whichever happens first wins, because the context keeps its first cancellation cause: when shutdown comes first, the cause is the application's and a nil return is a graceful stop; when the timeout fires first and shutdown follows, the cause stays `ErrRunTimeout` and the run is a timed-out failure, nil return included.
- The timeout is cooperative: it cancels the context and never kills the goroutine. A `Run` that ignores its context keeps the worker busy past the budget, and activations that pass meanwhile are skipped. There is never more than one run of a worker at a time.
- Continuous workers have no run timeout — their `Run` is meant to last for the whole process. Put timeouts on the individual operations inside the loop instead.

Replace hand-written `context.WithTimeout` wrappers inside scheduled `Run` methods with `WithRunTimeout`: the budget becomes visible in `pool.Workers()` and a timed-out run is classified consistently.

---

## Finite Background Work

A continuous worker must keep running until shutdown. Returning nil early is treated as a bug: Credo logs `worker run failed` with `unexpected_exit=true` and restarts the worker with the [restart backoff](#restart-backoff). This catches a consumer loop that quietly ended — `for msg := range ch { … }; return nil` on a closed channel — before it goes unnoticed.

For work that genuinely finishes, pick one of two homes:

- **Startup should wait for it** (priming a cache the first request needs): use `app.OnStart`.

  ```go
  app.OnStart(func(ctx context.Context) error {
      return cache.Warm(ctx)
  })
  ```

- **It should run in the background** without delaying startup: do the work, then wait for shutdown.

  ```go
  func (w *Warmup) Run(ctx context.Context) error {
      if err := w.warm(ctx); err != nil {
          return err // retried with the restart backoff
      }
      <-ctx.Done() // a continuous worker lives until shutdown
      return nil
  }
  ```

Recurring finite work is a scheduled worker.

---

## Worker Context

Credo enriches the `context.Context` passed to `Run` with execution metadata:

- `worker.WorkerName(ctx)` — the registration name
- `worker.RunID(ctx)` — a fresh identifier per run; Credo's own log lines for that run carry the same value as `run_id`
- `worker.ScheduledAt(ctx)` — the intended activation time of a scheduled run

```go
func (w *CleanupWorker) Run(ctx context.Context) error {
    w.log.InfoContext(ctx, "cleanup started",
        "worker", worker.WorkerName(ctx),
        "run_id", worker.RunID(ctx),
        "scheduled_at", worker.ScheduledAt(ctx),
    )
    return w.repo.DeleteExpired(ctx)
}
```

Notes:

- `ScheduledAt(ctx)` is zero for continuous workers and for the `WithStartImmediately()` startup run.
- The context is cancelled on application shutdown, and — with `WithRunTimeout` — when the run's budget runs out.
- Restart and failure counters are not in the context; read them from `pool.Workers()`.

Always watch `ctx.Done()` in long-running work.

---

## Observing Worker State

The worker pool is available from DI as `*worker.Pool`. `pool.Workers()` returns one `worker.Info` per registered worker, in registration order:

```go
if err := app.Finalize(); err != nil {
    log.Fatal(err)
}
pool := app.MustResolve[*worker.Pool]()

app.GET("/admin/workers", func(ctx *credo.Context) error {
    return ctx.Response().JSON(http.StatusOK, pool.Workers())
})
```

Each `Info` carries:

- `Name` and `Kind` (`worker.KindContinuous` / `worker.KindScheduled`)
- `Config` — the **effective** configuration: `Schedule`, `StartImmediately`, `RunTimeout`, `MaxConsecutiveFailures`, `MaxRestarts`, `RestartDelay` and `MaxRestartDelay` (after config and defaults are applied) and a copy of the `Readiness` policy; fields that do not apply to the kind are zero, and zero limits mean unlimited
- `Status` — `idle`, `running`, `waiting`, `stopped` or `failed`
- `Restarts` (continuous) and `ConsecutiveFailures` (scheduled)
- `LastRun` (start of the latest run), `LastSuccess` (end of the latest successful scheduled run; always zero for continuous workers) and `LastError` (the latest failure, never with a stack trace; a successful scheduled run clears it)

Because `Config` is the effective policy, a registration test can assert it without running anything:

```go
func TestWorkerRegistration(t *testing.T) {
    app := testutil.NewApp(t)
    registerWorkers(app) // your composition root's worker registrations
    if err := app.Finalize(); err != nil {
        t.Fatal(err)
    }
    pool := app.MustResolve[*worker.Pool]()

    for _, info := range pool.Workers() {
        if info.Name == "invoice-worker" && info.Config.RunTimeout != 15*time.Second {
            t.Fatalf("invoice-worker timeout = %s", info.Config.RunTimeout)
        }
    }
}
```

**JSON.** `Info` is shaped for direct encoding: field names are snake_case (`restart_delay`, `consecutive_failures`, `last_error`, …); `last_run`, `last_success`, `last_error` and `config.readiness` are omitted while empty; every other field is always present. Durations encode as integer nanoseconds under Credo's JSON response profile — `"run_timeout": 15000000000` is 15 seconds. The spec shows a [full example](../specs/worker.md#json-shape).

---

## Reading the Logs

Every worker writes exactly one `worker started` line when the pool starts it and exactly one `worker stopped` line when it ends, both at Info:

```text
level=INFO msg="worker started" module=worker worker=invoice-worker kind=scheduled schedule="@every 1m" next_run=…
level=INFO msg="worker stopped" module=worker worker=invoice-worker kind=scheduled status=stopped reason=shutdown
```

- `reason=shutdown` — the application stopped the worker. `reason=failed` — the worker exhausted its failure limit (or its schedule has no future activation); it is preceded by an Error line such as `worker exceeded max consecutive failures`, which is the one to alert on.
- A failed run logs `worker run failed` (continuous) or `scheduled worker run failed` at Error, with `error`, `run_id` and `duration`, plus `timed_out=true` for a timeout, `unexpected_exit=true` for a continuous `Run` that returned nil early, and `stack` for a panic. A continuous failure line also carries `next_restart_in` when a restart follows ([Restart Backoff](#restart-backoff)).
- A successful scheduled run logs `scheduled worker run completed` at **Debug** only; a worker running every minute would otherwise write 1,440 Info lines a day. Use `pool.Workers()` or `WithReadiness` to answer "is it alive", and the logs as the audit trail.
- When runs outlast their schedule, one `worker ticks skipped` Warn line per resumption reports how many activations were skipped (`skipped`, `first_scheduled_at`, `last_scheduled_at`).
- To join Credo's lines with your own for one run, log `worker.RunID(ctx)` as `run_id`.

---

## Readiness Integration

A worker can take part in the readiness probe (`app.UseHealth()`, `/ready`) through an explicit policy. Nothing is bound automatically — a failed metrics reporter should not take the instance out of rotation — so each condition is opt-in:

```go
app.UseHealth()

worker.MustRegister(app, "recovery", recovery,
    worker.WithSchedule("@every 5m"),
    worker.WithStartImmediately(),
    worker.WithReadiness(worker.ReadinessPolicy{
        RequireFirstSuccess: true,             // unready until the first run succeeds
        FailWhenFailed:      true,             // unready once the failure threshold is exhausted
        MaxSuccessAge:       15 * time.Minute, // unready when the last success is too old
    }),
)
```

- `RequireFirstSuccess` is a startup barrier: the instance stays unready until the worker's first run succeeds, then stays ready even if later runs fail. A timed-out run never satisfies it. Pair it with `WithStartImmediately()` unless waiting for the first cron activation is intended.
- `FailWhenFailed` reports unready once the worker reaches `failed` (`WithMaxRestarts` / `WithMaxConsecutiveFailures` exhausted). For a continuous worker this includes one that keeps returning early, and the [restart backoff](#restart-backoff) spaces the restarts that lead there. Use it only for workers the instance cannot serve without: every replica hitting the same persistent failure leaves rotation together.
- `MaxSuccessAge` reports unready when the last success is older than the limit; it is not applied before the first success.
- `RequireFirstSuccess` and `MaxSuccessAge` are for scheduled workers; `FailWhenFailed` works for both kinds. The zero policy is rejected at registration.

The contribution appears in the `/ready` body as a check named `worker:<name>`. Worker registration and `app.UseHealth()` may run in either order.

---

## Configuration

Credo reads two worker defaults from app config:

```json
{
  "worker": {
    "restart_delay": "5s",
    "max_restart_delay": "5m"
  }
}
```

`worker.restart_delay` and `worker.max_restart_delay` are the default first wait and cap of the [restart backoff](#restart-backoff) for continuous workers. `worker.WithRestartDelay(...)` and `worker.WithMaxRestartDelay(...)` override them per worker; zero falls back to `DefaultRestartDelay` (3s) and `DefaultMaxRestartDelay` (1m), and a negative value is a registration error. A configured cap below one worker's restart delay is raised to that delay for that worker. Schedules, limits and timeouts are per-worker options only.

---

## Reloadable Settings

Workers are registered once and run for the life of the process; a [config reload](configuration.md#reloading-configuration) does not re-register them or change their options. For a setting a worker should honour without a restart — a batch size, a concurrency cap, a polling interval — give the worker an atomic holder and swap it from an `OnConfigChange[T]` subscriber:

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

app.OnConfigChange("sync", func(ctx context.Context, next SyncConfig) error {
    sync.cfg.Store(&next)
    return nil
})
```

A changed cron expression is restart-only; the reload logs it as `restart required`.

---

## Testing Workers

A worker is a type with a `Run` method, so its logic is tested by calling `Run` directly:

```go
func TestCleanupWorker(t *testing.T) {
    repo := &fakeCleanupRepo{}
    w := &CleanupWorker{repo: repo}

    if err := w.Run(t.Context()); err != nil {
        t.Fatal(err)
    }
    if repo.deleted != 1 {
        t.Fatalf("deleted = %d, want 1", repo.deleted)
    }
}
```

Test the registration — names, schedules, limits, timeouts — through `pool.Workers()` as shown in [Observing Worker State](#observing-worker-state). Timing behaviour inside your own workers is easiest to test with `testing/synctest`.

---

## Best Practices

- continuous workers own their loop and return only at shutdown or on a real failure; scheduled workers do one execution and return
- treat shutdown as normal control flow: return nil or `ctx.Err()` once `ctx.Done()` closes
- give scheduled workers a `WithRunTimeout` budget instead of hand-rolled timeouts inside `Run`
- keep worker names stable and unique; they key logs, snapshots and readiness checks
- make scheduled jobs idempotent where possible, because activations can be skipped and a timed-out run may have done part of its work
- inject dependencies via constructors and `RegisterProvided`; avoid building service graphs inside `Run`
- capture `*worker.Pool` during bootstrap if you want admin/debug endpoints; avoid request-time `Resolve` as the default pattern

---

## Related Guides

- [Getting Started](getting-started.md)
- [Dependency Injection Guide](dependency-injection.md)
- [Data Access Guide](data-access.md)
- [Pre-v1 Migration Guide](pre-v1-migration.md#workers)
