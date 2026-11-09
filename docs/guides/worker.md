# Worker Guide

This guide explains how to run background tasks through the App's worker registry, `app.Workers()`. For the exact contracts — outcome classification, admission order, log attributes, the snapshot's JSON shape — see the [Worker Spec](../specs/worker.md); for the reasoning behind them, [ADR-023](../adr/023-worker-system.md). Workers are lifecycle components, so the [lifecycle spec](../specs/lifecycle.md) and the [dependency-injection guide](dependency-injection.md#shutdown-and-lifecycle) describe the start and the drain they take part in.

---

## What Workers Are For

Credo workers are for application work that runs outside the HTTP request path:

- queue consumers
- event processors
- periodic cleanup jobs
- cache warmers
- reconciliation / sync loops

Workers are optional. If your app only serves HTTP requests, it never calls `app.Workers()`.

---

## Mental Model

A worker is anything with a `Run(ctx context.Context) error` method, or a plain `func(ctx context.Context) error`. You register it through the App's **worker registry** — `workers := app.Workers()` — under a name, and the registration method decides its kind:

| Method | Kind |
| --- | --- |
| `workers.Continuous(name, w, cfg...)` | continuous |
| `workers.Scheduled(name, expr, w, cfg...)` | scheduled |

`w` takes one of [three forms](#three-forms): the worker itself, a run function whose other parameters the container supplies, or a constructor of the worker.

Each registration adds the worker's loop to the App as a lifecycle component named `worker:<name>`. The App starts it in the start phase and stops it in the drain like every other component: its `Start` launches the worker's loop, its `Shutdown` cancels the worker's context and waits for `Run` to return. The registry itself has no lifecycle — it is what you register through and what you ask for each worker's state.

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

    app.Provide(NewQueue)
    app.Provide(NewSender)

    app.Workers().Continuous("email-consumer", NewEmailConsumer, credo.ContinuousConfig{
        Restart: credo.RestartPolicy{MinDelay: 5 * time.Second},
    })

    if err := app.Run(); err != nil {
        log.Fatal(err)
    }
}
```

Important points:

- The registration takes the constructor, `NewEmailConsumer`, and the App calls it in the start phase with the queue and the sender. It therefore knows the consumer depends on them: it starts the worker after them and stops it before them. The constructor is not a `Provide` binding — nothing else resolves the consumer.
- `Run` blocks until shutdown or a real failure. Returning nil or `ctx.Err()` — wrapped or not — after `ctx` is cancelled is a graceful stop. An error that carries anything else is a failure even then: `errors.Join(ctx.Err(), flushErr)` is recorded and logged, so a final write that failed during shutdown is never lost. A client that reports a cancelled call in words of its own needs [one translation](#reporting-a-cancelled-call-at-shutdown).
- Restarts are unlimited unless you set `Restart.Limit`. `MinDelay: 5 * time.Second` sets the first wait; repeated failures back off from there up to a one-minute cap.
- `Queue`, `Sender` and their constructors stand for your application's dependencies.

---

## Quick Start: Scheduled Worker

```go
// cleanup does one execution and returns. The registry supplies ctx on every
// run, and the container supplies repo once.
func cleanup(ctx context.Context, repo *CleanupRepository) error {
    return repo.DeleteExpired(ctx)
}

func main() {
    app, err := credo.New()
    if err != nil {
        log.Fatal(err)
    }

    app.Provide(NewCleanupRepository)

    app.Workers().Scheduled("cleanup", "0 */6 * * *", cleanup, credo.ScheduledConfig{
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

- A job this small needs no type of its own. `cleanup` is a run function: it takes one `context.Context`, which each run receives, and its other parameters come from the container, resolved once after their components have started. The worker stops before the repository it uses.
- Each run does one cleanup execution and returns.
- `RunOnStart` runs once as soon as the worker starts, before waiting for the first cron activation.
- `RunTimeout` gives every run a budget; see [Timeouts](#timeouts).
- After five consecutive failures the worker is failed until the application restarts.
- Every replica of the application runs this schedule; see [Every replica runs every schedule](#every-replica-runs-every-schedule).

---

## Small Workers

A small job needs only a small type: anything whose `Run(ctx context.Context) error` method implements `credo.Worker` is a worker.

```go
// Heartbeat logs a line every 30 seconds until the worker stops.
type Heartbeat struct {
    log *slog.Logger
}

func (h Heartbeat) Run(ctx context.Context) error {
    ticker := time.NewTicker(30 * time.Second)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return nil
        case <-ticker.C:
            h.log.InfoContext(ctx, "still alive")
        }
    }
}

app.Workers().Continuous("heartbeat", Heartbeat{log: app.Logger()})
```

A function you already have needs no type at all: a `func(context.Context) error` is a worker as it is.

```go
app.Workers().Scheduled("metrics-report", "@every 1m", reportMetrics) // func reportMetrics(ctx context.Context) error
```

Both are registered as values, so read [Registering a value](#registering-a-value) before you give one a database to use.

---

## Registration

### The registry

`app.Workers()` returns the App's one registry, a `*credo.Workers` created on first use; every call returns the same value. It registers nothing by itself: it binds nothing into the container, installs no hook and adds no component. It logs through the App's logger with `module=worker`, and each worker's records carry `component=worker:<name>`. A module that registers workers takes the `*credo.App` it already receives:

```go
// RegisterBilling is the billing module's registration.
func RegisterBilling(app *credo.App) {
    app.Workers().Scheduled("invoice-worker", "@every 1m", NewInvoiceWorker, credo.ScheduledConfig{
        RunTimeout: 15 * time.Second,
    })
}
```

The registry is not in the container. A controller that wants it as a constructor parameter — an admin endpoint, say — gets it from a binding you write: `app.ProvideValue(app.Workers())`. Whichever module registered a worker, `Snapshot` and `Lookup` report it, and its name is unique across the App.

### Names

The name identifies the registration: it is `Name` in the snapshot, the suffix of the component name `worker:<name>` — the `component` attribute of every log line the worker writes, and the name under which `/ready` and the lifecycle's error reports name the worker — and `Worker` in [`CurrentWorkerRun`](#run-context). It must be non-empty, unique within the App, and free of surrounding whitespace and control characters; names are never trimmed for you. Keep names stable — dashboards and alerts will key on them.

### Registration window and misuse

Register workers before the container is finalized: before the first `Resolve` (or `ResolveAll`, `MustResolve`, `MustResolveAll`), before an explicit `app.Finalize()`, and before `Run`, which finalizes an App that resolved nothing. Registration does no I/O and checks everything it can at the call: a misuse panics there, with a message naming the call, the problem and the remedy. A registration after the freeze names what finalized the container, where, and the calls that led there:

```text
credo: Workers.Continuous("order-consumer"): Restart.MaxDelay 1s is below MinDelay 3s (the default); set MinDelay to at most 1s, or raise MaxDelay
credo: Workers.Continuous("x"): a worker named "x" is already registered; give each worker its own name
credo: Workers.Scheduled("report"): called after the container was finalized by MustResolve[*app.OrderController] at /src/main.go:42 (called from /src/main.go:12); register before that call
```

A nil worker, a `w` of none of the [three forms](#three-forms), a malformed name, more than one configuration, a negative value and a cron expression that does not parse panic the same way. A schedule that comes from configuration is checked first with `credo.ParseSchedule`, which returns the error instead of panicking ([Configuration](#configuration)). What only the whole graph reveals — a parameter of a run function or a constructor that nothing provides — is an error `Finalize` returns, with its path, and a constructor that fails in the start phase fails the start.

### Three forms

`Continuous` and `Scheduled` take `w` in one of three forms, and the registration tells them apart by their shape:

| Form | Shape | Fits |
| --- | --- | --- |
| a worker | a value whose type has `Run(ctx) error`, or a `func(context.Context) error` | self-contained work that uses no component: a heartbeat, a ticker that logs |
| a run function | a function that takes exactly one `context.Context`, in any position, and returns only an `error`; its other parameters come from the container | a job over components that keeps nothing between runs: `func(ctx context.Context, db *sqldb.DB) error` needs no type of its own |
| a constructor | any other function, `func(deps...) W` or `func(deps...) (W, error)`, whose `W` is a worker in either form | a worker with fields of its own — state, settings, a teardown — built from its dependencies |

```go
workers := app.Workers()
workers.Continuous("heartbeat", Heartbeat{log: app.Logger()})             // a worker
workers.Scheduled("session-cleanup", "0 */6 * * *", purgeExpiredSessions) // a run function over *sqldb.DB
workers.Continuous("email-consumer", NewEmailConsumer)                    // a constructor
workers.Continuous("outbox-relay", (*OutboxRelay).Run)                    // the Run of a bound worker
```

The last line is a run function too: a method expression's first parameter is its receiver, so `(*OutboxRelay).Run` is a `func(*OutboxRelay, context.Context) error`, and the container supplies the receiver ([A worker that other code uses](#a-worker-that-other-code-uses)). A function that takes two contexts, or takes a context and returns anything but a lone `error`, panics at registration, naming the run function's shape; any other shape panics naming the three forms.

### Resources come in as parameters

A worker gets the components it uses — a database, a client, a queue — as parameters of its constructor or its run function, so the App sees its dependencies: it starts the worker after them and stops it before them, and the database a consumer writes to is still open while the consumer flushes its last batch. A run function's parameters are resolved once, after their components have started, and every run receives them with its own context. A constructor is called once in the start phase, after its parameters' components have started:

- A parameter nothing provides fails `Finalize` with its path, which starts at the worker's registration call: `di: missing dependency: the value Workers.Continuous("email-consumer") builds → *app.Queue (not registered); provide *app.Queue before Finalize`. For a run function the path starts at `Workers.Continuous("<name>")`, the loop.
- A constructor error, a panic or a nil worker fails the start, reported as the start failure of `worker-value:<name>`: what was built is rolled back, the loop never starts, and the worker stays `pending`.
- The constructor is not a binding. Nothing else resolves the value it builds, and each registration calls its constructor separately, so two registrations of one constructor get two values, `worker-value:a` and `worker-value:b`.

A worker that embeds a bound handle shares it with the binding: a consumer whose type embeds a `*sqldb.DB` bound with `ProvideValue` is one resource with the database, whose one readiness entry and one teardown stay the binding's, with its name and its options, after the worker's loop and every other consumer of the database. A worker with a readiness or a teardown of its own therefore holds the handle in a named field, so that it is a resource of its own ([dependency-injection guide](dependency-injection.md#wrappers-and-resource-identity)).

### A worker's own `Shutdown`

A restart calls `Run` again on the same value, so what one run uses is built inside `Run` ([A continuous `Run` is re-entered](#a-continuous-run-is-re-entered)). A `Shutdown` on the worker type is for state that outlives a single `Run` — a buffer flushed once at the end, a connection the constructor opened:

```go
type AuditShipper struct {
    db      *sqldb.DB // named field: the database keeps its own teardown
    pending *Batch    // filled across runs
}

func NewAuditShipper(db *sqldb.DB) *AuditShipper {
    return &AuditShipper{db: db, pending: NewBatch()}
}

func (s *AuditShipper) Run(ctx context.Context) error { /* ... */ }

// Shutdown writes what is still pending, once, after the loop has stopped.
func (s *AuditShipper) Shutdown(ctx context.Context) error {
    return s.pending.WriteTo(ctx, s.db)
}

app.Workers().Continuous("audit-shipper", NewAuditShipper)
```

The value a constructor builds is a component like any constructor result. Its `Shutdown` runs once, after its loop has stopped and before the components it depends on, in the internal tier — for a scheduled worker too, whose loop stops with the HTTP drain while its value is torn down after it. Its `Start` and `Ready` are planned from the constructor's declared result type, so declare the constructor to return the concrete type: one declared to return `credo.Worker` hides a `Start` or `Ready` its value has, and the start fails naming that remedy, while a hidden `Shutdown` still runs. A worker value you pass in stays yours: the App runs it and never calls its `Shutdown`. A constructor that returns a method value such as `shipper.Run` hands over the function alone, which has no teardown.

### A worker that other code uses

The value a constructor builds is not a binding, so handlers cannot reach it. A worker they also use — an outbox relay whose `Enqueue` they call — is bound with `Provide` and registered through the method expression of its `Run`:

```go
app.Provide(NewOutboxRelay) // handlers take *OutboxRelay and call Enqueue
app.Workers().Continuous("outbox-relay", (*OutboxRelay).Run)
```

Handlers and the loop share the binding's one singleton, and the binding keeps everything a binding has. The singleton starts once, before the loop. `/ready` shows its one entry, with the binding's options — `credo.ComponentName`, `credo.OptionalReadiness()`. Its `Shutdown` runs once, after the loop, whichever of the two calls comes first, and `testutil.WithOverride[*OutboxRelay]` replaces it for both. The compiler checks that `Run` exists; the registration checks its shape.

Bind it without `credo.Ingress()`. The binding's teardown then runs in the internal tier, after the HTTP drain, which the handlers that call `Enqueue` need; bound ingress, the relay would be torn down beside the drain while they still call it, and a continuous worker's internal loop could not take it at all — `Finalize` refuses an internal loop over an ingress binding. The loop's own tier comes from the worker's configuration. Registering `NewOutboxRelay` itself as the worker's constructor beside its binding would build a second relay, one the handlers never see.

### Registering a value

A worker you pass as a value — a struct with `Run`, or a `func(context.Context) error` — is one the App cannot see into: a closure's captures and a struct's fields are invisible to the dependency graph, so the worker has **no edges**. It takes its place in its tier by registration order alone, and nothing orders it after the database it writes to at start or before it at shutdown. A worker that uses infrastructure — a database, a client, a queue — is therefore registered as a run function or a constructor, so the graph orders it in both directions. Register a value only for what is self-contained: a heartbeat, a ticker that logs, a function over values that are not components.

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
app.Workers().Continuous("payments-events", NewBrokerConsumer, credo.ContinuousConfig{
    Tier: credo.TierIngress, // work enters the process here: stop taking it with the listener
})
```

The two mistakes are not equal: an external consumer left internal merely stops a little later, while an in-process consumer moved to ingress stops before the handlers that feed it have finished and loses what they enqueue during the drain. A worker of the internal tier whose parameters — of its run function or its constructor — depend on an ingress component fails `Finalize` with the path; declare the worker `Tier: credo.TierIngress`, or split the ingress component so that what the worker uses is an internal part. The value a constructor builds is internal whatever its worker's tier, so one with a teardown of its own that depends on an ingress component fails `Finalize` with the remedy of splitting the worker — or, when that component is the binding the constructor returns, of binding it without `credo.Ingress()`.

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
| `Restart.MinDelay` | continuous | `credo.DefaultMinRestartDelay` (3 s), the first and shortest wait |
| `Restart.MaxDelay` | continuous | the larger of `credo.DefaultMaxRestartDelay` (1 min) and `MinDelay`; a positive value below `MinDelay` panics |
| `RunOnStart` | scheduled | no extra run at start |
| `RunTimeout` | scheduled | no timeout |
| `MaxConsecutiveFailures` | scheduled | unlimited failures |
| `UnreadyWhenFailed` | both | off ([Readiness](#readiness)) |
| `UnreadyUntilFirstSuccess` | scheduled | off |
| `UnreadyAfterSuccessAge` | scheduled | off |

A setting of the other kind cannot be written: `ContinuousConfig` has no schedule fields and `ScheduledConfig` no restart policy.

The configuration lives in code. The registry reads no configuration section, so a setting that should come from the environment — a schedule that differs per deployment, a restart delay you tune in production — is a typed section of your own, read once in the composition root and passed in:

```go
type WorkerSettings struct {
    CleanupSchedule string        // workers.cleanup_schedule
    RestartMinDelay time.Duration // workers.restart_min_delay
    RestartMaxDelay time.Duration // workers.restart_max_delay
}

settings := app.MustGetConfig[WorkerSettings]("workers")
if _, err := credo.ParseSchedule(settings.CleanupSchedule); err != nil {
    log.Fatalf("workers.cleanup_schedule: %v", err)
}

workers := app.Workers()
workers.Continuous("order-consumer", NewOrderConsumer, credo.ContinuousConfig{
    Restart: credo.RestartPolicy{MinDelay: settings.RestartMinDelay, MaxDelay: settings.RestartMaxDelay},
})
workers.Scheduled("session-cleanup", settings.CleanupSchedule, NewSessionCleanup)
```

Zero settings fall through to the defaults. A changed schedule, limit or timeout takes effect at the next start: a [config reload](#reloadable-settings) does not re-register workers.

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
app.Workers().Continuous("poller", NewPoller, credo.ContinuousConfig{
    Restart: credo.RestartPolicy{MinDelay: 10 * time.Second, MaxDelay: 10 * time.Second},
})
```

A `MinDelay` of a minute or more alone is fixed as well, since a zero `MaxDelay` resolves to it.

**Limits.** Backoff makes `Limit` take longer to exhaust: with the defaults, `Limit: 5` reaches `failed` after roughly 48–93s of waiting instead of 15s, and an `UnreadyWhenFailed` readiness drop comes correspondingly later. `RestartPolicy{Disabled: true}` makes the first failure terminal.

---

## Timeouts

`ScheduledConfig.RunTimeout` bounds every run of a scheduled worker, including the `RunOnStart` run. When the budget runs out, Credo cancels the run's context; the worker should notice and return.

```go
func (w *ReportWorker) Run(ctx context.Context) error {
    for _, tenant := range w.tenants {
        if err := w.buildReport(ctx, tenant); err != nil {
            if errors.Is(context.Cause(ctx), credo.ErrWorkerRunTimeout) {
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
- `errors.Is(context.Cause(ctx), credo.ErrWorkerRunTimeout)` tells the budget running out from the worker being stopped. Whichever happens first wins, because the context keeps its first cancellation cause: when the stop comes first, a nil return is a graceful stop; when the timeout fires first and the stop follows, the run is a timed-out failure, nil return included.
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

A subscription opened in the constructor and closed when `Run` returns would leave every restart reading from a closed subscription. Long-lived clients the worker shares with others — a database pool, an HTTP client — stay constructor parameters; it is what `Run` opens for itself that it must close. A worker that cannot be re-entered is registered with `RestartPolicy{Disabled: true}`, so its first failure is terminal.

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

The scheduler is per process. Three replicas of an application run a scheduled cleanup three times, at the same activation. Make each scheduled job idempotent — deleting expired rows twice deletes nothing the second time — or guard it with an application-level lock: for SQL, a PostgreSQL advisory lock (`pg_try_advisory_lock`) taken at the start of the run, or a claim row that one replica wins per activation. For a cron expression, `CurrentWorkerRun(ctx).ScheduledAt` names the same activation on every replica, which makes it a natural claim key; an `@every` grid starts when each process starts, so its activations differ between replicas. Activations can also be skipped, and a timed-out run may have done part of its work, so idempotence pays off on a single replica too.

---

## Run Context

The context passed to `Run` carries the run's metadata. `credo.CurrentWorkerRun(ctx)` returns it:

```go
func (w *InvoiceWorker) Run(ctx context.Context) error {
    run, _ := credo.CurrentWorkerRun(ctx)
    w.log.InfoContext(ctx, "processing invoices",
        "run_id", run.ID, "scheduled_at", run.ScheduledAt)
    return w.repo.ProcessPending(ctx)
}
```

| `WorkerRun` field | Value |
| --- | --- |
| `Worker` | the registration name |
| `ID` | a fresh identifier per run, equal to the `run_id` of Credo's own log lines for that run |
| `ScheduledAt` | the intended activation time; zero for continuous workers and for the `RunOnStart` run |

`CurrentWorkerRun` returns false for a context that is not a run context — in a unit test that calls `Run` directly, for example — so code that reads it works there too. The context is cancelled when the worker stops and, with `RunTimeout`, when the run's budget runs out; always watch `ctx.Done()` in long-running work. Restart and failure counters are not in the context; read them from the [snapshot](#observing-worker-state).

---

## Observing Worker State

`app.Workers().Snapshot()` returns one `credo.WorkerInfo` per worker of the App, in registration order, and `app.Workers().Lookup(name)` returns the worker registered under exactly that name, or false. Both work from the moment of registration — before `Finalize`, before the App starts — and read memory only. An admin endpoint serves them directly:

```go
workers := app.Workers()
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

A controller built by the container takes `*credo.Workers` as a constructor parameter once you bind it with `app.ProvideValue(app.Workers())`. The registry is also the way to a worker's state: a worker its constructor builds is in no binding, so code that needs to know whether it runs asks `Lookup` for it by name rather than resolving it.

Each `WorkerInfo` carries:

- `Name`, `Kind` (`credo.WorkerKindContinuous` / `credo.WorkerKindScheduled`) and, for a scheduled worker, `Schedule` — the expression as registered
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

**JSON.** `WorkerInfo` is shaped for direct encoding: field names are snake_case, the nested configuration included (`continuous.restart.min_delay`, `scheduled.run_timeout`, `consecutive_failures`, `last_error`, …); `schedule`, `continuous`, `scheduled`, `last_started_at`, `last_succeeded_at` and `last_error` are omitted while empty; every other field is always present. Durations encode as integer nanoseconds under Credo's JSON response profile — `"run_timeout": 15000000000` is 15 seconds — and `tier` as `"ingress"` or `"internal"`. The spec shows a [full example](../specs/worker.md#json-shape).

---

## Reading the Logs

Every worker that starts writes exactly one `worker started` line and exactly one `worker stopped` line, both at Info. Every line of a worker carries `module=worker` and `component=worker:<name>`, the key and value of the lifecycle's records about the worker:

```text
level=INFO msg="worker started" module=worker component=worker:invoice-worker kind=scheduled schedule="@every 1m" next_run=…
level=INFO msg="worker stopped" module=worker component=worker:invoice-worker kind=scheduled status=stopped reason=shutdown
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
- To join Credo's lines with your own for one run, log `CurrentWorkerRun(ctx).ID` as `run_id`.

---

## Readiness

A worker can take the instance out of rotation through `/ready` (`app.UseHealth()`). Nothing is bound automatically — a failed metrics reporter should not take the instance out of rotation — so each condition is opt-in on the configuration:

```go
app.UseHealth()

app.Workers().Scheduled("recovery", "@every 5m", NewRecovery, credo.ScheduledConfig{
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

workers := app.Workers()
workers.Continuous("order-consumer", NewOrderConsumer, credo.ContinuousConfig{
    Restart: credo.RestartPolicy{Limit: 5}, // with unlimited restarts it never fails
})

app.AddLivenessCheck("order-consumer", credo.HealthCheckFunc(func(context.Context) error {
    info, ok := workers.Lookup("order-consumer")
    switch {
    case !ok:
        return errors.New(`no worker named "order-consumer"`)
    case info.Status == credo.WorkerStatusFailed:
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

// The subscriber resolves the worker, so the worker is a binding.
app.Provide(NewSync)
app.Workers().Continuous("sync", (*Sync).Run)

// After every registration; the first resolving call finalizes the container:
syncer := app.MustResolve[*Sync]()
app.OnConfigChange("sync", func(ctx context.Context, next SyncConfig) error {
    syncer.cfg.Store(&next)
    return nil
})
```

A changed cron expression is restart-only; the reload logs it as `restart required` when nothing subscribes to its section.

---

## Testing Workers

A worker is a type with a `Run` method or a function, so its logic is tested by calling it directly — `Run`, or the run function with its parameters — with no registry and no App:

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
    RegisterBilling(app)

    info, ok := app.Workers().Lookup("invoice-worker")
    if !ok {
        t.Fatal(`no worker named "invoice-worker"`)
    }
    if got := info.Scheduled.RunTimeout; got != 15*time.Second {
        t.Fatalf("invoice-worker run timeout = %s, want 15s", got)
    }
}
```

To run the workers inside the App, start it without a listener. `testutil.Start` runs the start phase — the workers' loops included — and fails the test when it fails; an explicit `app.Shutdown` stops them, so a test can assert that every worker stops within the deadline:

```go
func TestBillingWorkersStop(t *testing.T) {
    app := testutil.NewApp(t, testutil.WithWiring(wire))
    RegisterBilling(app)

    testutil.Start(t, app)
    ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
    defer cancel()
    if err := app.Shutdown(ctx); err != nil {
        t.Fatal(err) // names worker:<name> when a Run ignored its cancellation
    }
    for _, info := range app.Workers().Snapshot() {
        if info.Status != credo.WorkerStatusStopped {
            t.Errorf("%s: status %s, want stopped", info.Name, info.Status)
        }
    }
}
```

An integration test that drives the workers through HTTP uses `testutil.Start`, which starts the App and shuts it down when the test ends, after the test server has drained:

```go
app := testutil.NewApp(t, testutil.WithWiring(wire))
RegisterBilling(app)

testutil.Start(t, app) // starts the workers; the App shuts down when the test ends
srv := httptest.NewServer(app)
t.Cleanup(srv.Close) // drained before the workers stop
```

A worker its constructor builds is not a binding, so a test swaps its dependencies: `testutil.WithOverride[T]` on a parameter's type, as for any binding, and the constructor receives the override. A worker bound with `Provide` and registered through `(*T).Run` is replaced by `testutil.WithOverride[*T]`, for the handlers and the loop alike.

---

## Best Practices

- give a worker what it uses as parameters of a run function or a constructor, so the App starts it after its dependencies and stops it before them
- bind a worker that other code calls with `Provide`, without `credo.Ingress()`, and register `(*T).Run`; register a constructor only for a worker nothing else uses
- continuous workers own their loop, are re-enterable, and return only when stopped or on a real failure; scheduled workers do one execution and return
- treat shutdown as normal control flow: return nil or `ctx.Err()` once `ctx.Done()` closes, translate a client's own "cancelled" error at the call, and never swallow errors with a blanket `ctx.Err()` check
- keep the default tiers unless work enters the process through the worker
- give scheduled workers a `RunTimeout` budget instead of hand-rolled timeouts inside `Run`
- make scheduled jobs idempotent or lock-guarded: every replica runs every schedule
- keep worker names stable and unique; they key logs, snapshots, readiness and liveness checks
- reach the registry through `app.Workers()` wherever the `*credo.App` is at hand; there is nothing to resolve, and an admin controller that takes it as a parameter gets it from `app.ProvideValue(app.Workers())`

---

## Related Guides

- [Getting Started](getting-started.md)
- [Dependency Injection Guide](dependency-injection.md)
- [Deployment Guide](deployment.md)
- [Data Access Guide](data-access.md)
- [Pre-v1 Migration Guide](pre-v1-migration.md#workers)
