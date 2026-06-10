# Worker Guide

This guide explains how to run background tasks with Credo's `worker/` package.
For low-level contracts and runtime semantics, see the
[Worker Spec](../specs/worker.md).

---

## What Workers Are For

Credo workers are for application work that should run outside the HTTP request
path:

- queue consumers
- event processors
- periodic cleanup jobs
- cache warmers
- reconciliation / sync loops

Workers are optional. If your app only serves HTTP requests, you do not need
the package.

---

## Mental Model

Credo supports two worker modes:

### Continuous worker

No schedule is configured.

- Credo calls `Run(ctx)` once at startup.
- The worker owns its own loop.
- If `Run` returns an error or panics, Credo applies the restart policy.
- `app.Shutdown(ctx)` cancels the same context and waits for the worker to exit.

Use this for long-lived background processes such as consumers and watchers.

### Scheduled worker

`worker.WithSchedule(...)` is configured.

- Credo calls `Run(ctx)` once per cron tick.
- Each call should represent one execution, not an internal infinite loop.
- If a tick fires while the previous execution is still running, Credo skips the
  new tick and logs the skip.
- `worker.WithStartImmediately()` adds one synthetic startup execution before
  the normal schedule begins.

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

func (c *EmailConsumer) Name() string { return "email-consumer" }

func (c *EmailConsumer) Run(ctx context.Context) error {
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        default:
        }

        msg, err := c.queue.Receive(ctx)
        if err != nil {
            return err
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

    worker.Register(app, consumer,
        worker.WithMaxRestarts(0),
        worker.WithRestartDelay(5*time.Second),
    )

    if err := app.Run(); err != nil {
        log.Fatal(err)
    }
}
```

Important points:

- `WithMaxRestarts(0)` means unlimited restarts.
- `Run` should block until shutdown or a real failure.
- Returning `ctx.Err()` during shutdown is treated as a graceful stop, not a failure.
- `newQueue()` and `newSender()` are placeholders for your application's dependencies.

---

## Quick Start: Scheduled Worker

```go
package main

import (
    "context"
    "log"
    "time"

    "github.com/credo-go/credo"
    "github.com/credo-go/credo/worker"
)

type CleanupWorker struct {
    repo CleanupRepository
}

func (w *CleanupWorker) Name() string { return "cleanup" }

func (w *CleanupWorker) Run(ctx context.Context) error {
    return w.repo.DeleteExpired(ctx)
}

func main() {
    app, err := credo.New()
    if err != nil {
        log.Fatal(err)
    }

    cleanup := &CleanupWorker{repo: newCleanupRepo()}

    worker.Register(app, cleanup,
        worker.WithSchedule("0 */6 * * *"),
        worker.WithStartImmediately(),
        worker.WithMaxConsecutiveFailures(5),
    )

    if err := app.Run(); err != nil {
        log.Fatal(err)
    }
}
```

Important points:

- `Run` should do one cleanup execution and return.
- Overlapping ticks are skipped automatically.
- `WithStartImmediately()` runs once at startup before waiting for the first cron tick.

---

## `worker.Func`

For small jobs, you do not need a custom struct type:

```go
worker.Register(app,
    worker.Func("heartbeat", func(ctx context.Context) error {
        ticker := time.NewTicker(30 * time.Second)
        defer ticker.Stop()

        for {
            select {
            case <-ctx.Done():
                return ctx.Err()
            case <-ticker.C:
                log.Println("still alive")
            }
        }
    }),
)
```

`worker.Func` is especially useful for tiny continuous workers or simple
scheduled tasks created directly in bootstrap code.

---

## Registration and Lifecycle

`worker.Register(app, w, opts...)` does more than store the worker value:

1. validates worker options
2. parses schedules immediately
3. creates the worker pool on first registration
4. registers the pool in DI
5. attaches pool startup to `app.OnStart`
6. lets the DI shutdown chain stop workers automatically

This means the normal lifecycle is:

```text
worker.Register(...) -> app.Run() -> workers start
app.Shutdown(ctx) -> worker contexts cancel -> pool waits for exit
```

Register workers before `credo.Finalize(app)` or before `Run()`/`RunTLS()`.

---

## DI Integration

Workers fit naturally into Credo's constructor-injection model.

```go
type InvoiceWorker struct {
    infra credo.Infra
    repo  InvoiceRepository
}

func NewInvoiceWorker(infra credo.Infra, repo InvoiceRepository) *InvoiceWorker {
    return &InvoiceWorker{infra: infra, repo: repo}
}

func (w *InvoiceWorker) Name() string { return "invoice-worker" }

func (w *InvoiceWorker) Run(ctx context.Context) error {
    w.infra.Logger.Info("processing invoices", "run_id", worker.RunID(ctx))
    return w.repo.ProcessPending(ctx)
}

func bootstrap(app *credo.App) error {
    credo.MustProvide[*InvoiceWorker](app, NewInvoiceWorker)

    invoiceWorker := credo.MustResolve[*InvoiceWorker](app)
    worker.Register(app, invoiceWorker,
        worker.WithSchedule("@every 1m"),
    )

    return credo.Finalize(app)
}
```

Recommended pattern:

- use DI to construct the worker
- use `worker.Register` to attach lifecycle and scheduling behavior
- call `Finalize` only after all worker registrations are complete

See the [Dependency Injection Guide](dependency-injection.md) for broader DI patterns.

---

## Options

### Continuous worker options

Use these only when **no** schedule is configured:

- `worker.WithMaxRestarts(n)`
- `worker.WithRestartDelay(d)`

Behavior:

- `n == 0` means unlimited restarts
- restart count increases only for real failures / panics
- shutdown cancellation does not consume restart budget

### Scheduled worker options

Use these only when `worker.WithSchedule(...)` is present:

- `worker.WithSchedule(expr)`
- `worker.WithStartImmediately()`
- `worker.WithMaxConsecutiveFailures(n)`

Behavior:

- consecutive failures reset after a successful run
- exceeding the failure limit marks the worker failed until app restart
- wrong-mode option combinations make `Register` panic (registration misuse)

### Supported schedule formats

Credo supports:

- standard 5-field cron: `0 */6 * * *` — with lists (`1,15`), ranges (`1-5`),
  steps (`*/10`), month/weekday names (`jan`, `sat`), `?` as `*`, and `7` as Sunday
- descriptors: `@hourly`, `@daily` (alias `@midnight`), `@weekly`, `@monthly`
- intervals: `@every 5m`, `@every 1h30m`

Schedules run in the server's local time and fire at second 0 of the
matching minute. The 6-field seconds form is not supported — for sub-minute
periods use `@every`. As in crontab(5), when both day-of-month and
day-of-week are restricted, the schedule fires when either matches.

---

## Worker Context

Credo enriches the `context.Context` passed to `Run` with execution metadata:

- `worker.WorkerName(ctx)`
- `worker.Attempt(ctx)`
- `worker.RunID(ctx)`
- `worker.ScheduledAt(ctx)`

Example:

```go
func (w *CleanupWorker) Run(ctx context.Context) error {
    log.Printf(
        "worker=%s attempt=%d run_id=%s scheduled_at=%s",
        worker.WorkerName(ctx),
        worker.Attempt(ctx),
        worker.RunID(ctx),
        worker.ScheduledAt(ctx),
    )
    return nil
}
```

Notes:

- for continuous workers, `ScheduledAt(ctx)` is zero
- for `WithStartImmediately()`, `ScheduledAt(ctx)` is also zero because the startup tick is synthetic
- the same parent context is canceled on app shutdown

Always check `ctx.Done()` in long-running workers.

---

## Observing Worker State

The worker pool is available from DI as `*worker.Pool`.

```go
pool := credo.MustResolve[*worker.Pool](app)

app.GET("/admin/workers", func(ctx *credo.Context) error {
    return ctx.Response().JSON(200, pool.Workers())
})
```

`pool.Workers()` returns snapshot data such as:

- worker name
- kind (`continuous` / `scheduled`)
- current status
- last run time
- attempt counter
- last error text

This is useful for admin endpoints, debugging, and operational visibility.

---

## Configuration

Credo can read worker defaults from app config.

Example:

```json
{
  "worker": {
    "restart_delay": "5s"
  }
}
```

Currently, `worker.restart_delay` is used as the default restart delay for
continuous workers. `worker.WithRestartDelay(...)` overrides it per worker.
A zero delay (`WithRestartDelay(0)`) falls back to the default
(`DefaultRestartDelay`, 3s), so a worker that fails immediately is throttled
instead of busy-looping.

---

## Best Practices

- continuous workers should own their loop; scheduled workers should do one execution and return
- treat shutdown as normal control flow: return `ctx.Err()` or stop cleanly when `ctx.Done()` closes
- keep worker names stable and unique; they appear in logs and status snapshots
- make scheduled jobs idempotent where possible because skipped ticks can happen
- inject dependencies via constructors; avoid building service graphs inside `Run`
- capture `*worker.Pool` during bootstrap if you want admin/debug endpoints; avoid request-time `Resolve` as the default pattern

---

## Related Guides

- [Getting Started](getting-started.md)
- [Dependency Injection Guide](dependency-injection.md)
- [Data Access Guide](data-access.md)
