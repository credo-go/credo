# Deployment Guide

> Audience: operators and developers running a Credo service under a process supervisor or container runtime. **Related:** [Getting Started](getting-started.md), [Configuration Guide](configuration.md), [Lifecycle Spec](../specs/lifecycle.md), [ADR-006](../adr/006-application-lifecycle.md), [ADR-024](../adr/024-lifecycle-components.md), [ADR-020](../adr/020-reload-and-partial-config-reload.md)

This guide covers what a Credo process expects from the environment that runs it: which signals it handles, how shutdown and reload map onto systemd and containers, what a stop does to the components the App owns, and how to wire certificate rotation.

---

## Signals at a Glance

`app.Run()` installs the only signal handling Credo does. `RunContext` and `ServeContext` install none — cancellation and reload are entirely the caller's.

| Signal | Under `Run()` | Notes |
| --- | --- | --- |
| `SIGINT`, `SIGTERM` | Graceful shutdown within `WithShutdownTimeout` (default 30s) | A second signal during the drain force-kills the process. |
| `SIGHUP` (Unix only) | `app.Reload(ctx)` within `WithReloadTimeout` (default 30s) | Never terminates the process; a failed reload is logged and the previous configuration keeps serving. Signals arriving mid-reload coalesce into at most one follow-up. |

There is no `SIGHUP` on Windows. The programmatic `app.Reload(ctx)` works identically on every platform and is the trigger to expose from an admin endpoint when the runtime has no reload verb (see [Reload Without a Signal](#reload-without-a-signal)).

For deployments where configuration must never change through a signal — an immutable, file-only setup, or an environment where something else already sends `SIGHUP` (logrotate postrotate scripts, orchestration habits) — `credo.WithoutReloadSignals()` turns the signal into a logged no-op: `SIGHUP` is still captured, so it can never terminate the process, but no reload runs (`credo: reload signal ignored (reload signals disabled)` at Info). `SIGINT`/`SIGTERM` shutdown and programmatic `app.Reload` keep working.

What a reload does and does not change is defined in the [Configuration Guide](configuration.md#reloading-configuration): every live section the change touches is validated and replaced, its `OnChange` callbacks run, and every other key that differs from the start is reported as needing a restart — in the result `app.Reload` returns and in a Warn log line. File-based TLS certificates are not part of a reload: they rotate when their files change.

---

## systemd

A minimal unit for a service built with `app.Run()`:

```ini
[Unit]
Description=Example Credo service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=credo
WorkingDirectory=/srv/example
EnvironmentFile=/etc/example/env
ExecStart=/srv/example/bin/example
ExecReload=/bin/kill -HUP $MAINPID
KillSignal=SIGTERM
TimeoutStopSec=35
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

Points worth matching to your app:

- **`ExecReload=/bin/kill -HUP $MAINPID`** makes `systemctl reload example` trigger `app.Reload`. systemd reports the reload as successful as soon as the signal is delivered; the reload's own outcome is in the service log (`credo: reload complete` with an `errors` count, or `credo: reload aborted before publish`). If you need `systemctl reload` itself to fail on a bad reload, use an admin endpoint instead — see below.
- **`TimeoutStopSec` must exceed `WithShutdownTimeout`** (or `server.shutdown_timeout`). The drain budget is Credo's; `TimeoutStopSec` is the point at which systemd escalates to `SIGKILL`. Leave a few seconds of headroom so a component or stop hook that runs to the deadline is not killed mid-flight ([Shutdown and Readiness](#shutdown-and-readiness)).
- **`KillSignal=SIGTERM`** (the default) is what `Run` handles. Do not set `KillSignal=SIGHUP` — that turns stop into reload.
- **`EnvironmentFile=` is read once at process start.** A reload re-reads config files, `.env`, and the process environment, but the process environment is what systemd handed the process at `ExecStart`; editing the environment file and running `systemctl reload` changes nothing until the next restart. Keep values you want to change at runtime in the config file.

### Certificate rotation with certbot

File-based TLS (`WithTLSFiles` or `server.tls.*`) checks its two files at most once a second, at a handshake, and swaps in the pair when either file changed, so an ACME client that renews the files in place needs no deploy hook and no reload. The check follows symbolic links, so certbot's `live/` links count: a renewal that points them at new files is a change.

New TLS handshakes see the new certificate; open connections are untouched. If the new pair fails to load (a half-written file, a key that does not match), the previous certificate keeps serving, the failure is logged at Error once per change (`credo: TLS certificate pair failed to load after a change on disk; keeping the current pair`), and the pair is retried when its other file changes — rotation never takes the service down. The paths are read at start: pointing `server.tls.*` at new paths takes a restart. `WithTLSConfig` is the exception: Credo never touches a caller-supplied `*tls.Config`, so its owner drives rotation through their own `GetCertificate`.

---

## Containers

`docker stop` sends `SIGTERM` and waits `--time` (default 10s) before `SIGKILL`; raise it above your drain budget (`docker stop --time 35 example`, or `stop_grace_period` in Compose). Make sure the Credo binary is PID 1 or runs under an init that forwards signals (`docker run --init`, `tini`), otherwise signals never reach `Run`.

To reload a running container:

```sh
docker kill --signal=HUP example
```

Kubernetes has no reload verb. Mounted ConfigMap and Secret files change in place — after the kubelet's sync delay, and never when mounted through `subPath` — and no signal reaches the process.

- **A structural change takes a rollout.** A change that needs a restart — a key a reload reports in `restart_required`, a new `server.*` setting, an image or environment change — goes out as a rolling restart (`kubectl rollout restart deployment/example`). A Credo pod drains gracefully on `SIGTERM`; set `terminationGracePeriodSeconds` above `WithShutdownTimeout`, and readiness gates the rollout.
- **Certificates need nothing.** A Secret volume holding the TLS pair is checked at the handshake, and the kubelet's link swap is a change, so a renewed pair is served without a restart or a signal.
- **A live section needs a trigger.** Credo watches no configuration file, so a live section follows the volume only when something calls `app.Reload`. Credo ships no endpoint, whose authentication is yours, and no watcher, which would apply a change at each kubelet's sync rather than when you decide. Three triggers, from the simplest:
  1. **An admin endpoint** behind your authentication that calls `app.Reload` and returns its result ([Reload Without a Signal](#reload-without-a-signal)); you or a script call it on a pod.
  2. **A job that walks the pods** and calls that endpoint on each in turn, stopping at the first failure, so a bad value stops at one pod instead of reaching every replica:

     ```sh
     for pod in $(kubectl get pods -l app=example -o name); do
       kubectl exec "$pod" -- curl --fail --silent -X POST \
         --unix-socket /run/example/admin.sock http://localhost/admin/reload || exit 1
     done
     ```

  3. **A sidecar that sends `SIGHUP`** when the mounted volume changes. `Run` maps `SIGHUP` to a reload, so no endpoint is needed, at the cost of `shareProcessNamespace: true` on the pod, which lets the sidecar's container see and signal the application's process. Every replica then reloads at its own kubelet sync, with no gate between them.

  `kubectl exec example -- kill -HUP 1` works for one-off use.

---

## Shutdown and Readiness

### What a stop does

On `SIGTERM` (or a cancelled `RunContext` context, or `app.Shutdown`), the App drains what it owns in two tiers:

1. `/ready` answers 503 `shutting_down`, so load balancers stop routing; `/health` stays 200, since the process is alive and draining.
2. The **ingress tier** — where work enters the process: the WebSocket server, scheduled workers, consumers of external queues (a continuous worker registered with `Tier: credo.TierIngress` among them) and anything registered with `credo.Ingress()` or whose type declares the tier — stops concurrently with the HTTP drain: its `OnStop` hooks in reverse registration order, then its components, those that no dependency orders stopping concurrently with each other.
3. A reload that overlapped the stop finishes.
4. The **internal tier** — everything else: continuous workers, which consume what the handlers enqueue, and the database pools and clients the handlers and workers use — stops: its `OnStop` hooks in reverse registration order, then its components one at a time, each consumer before the components it depends on.

The steps share one deadline: `WithShutdownTimeout` (`server.shutdown_timeout`, 30 seconds by default) counted from the signal, or the deadline of the context passed to `app.Shutdown`. They spend it in order, so a slow HTTP or WebSocket drain leaves less for the internal tier. Size it as:

```text
max(slowest in-flight request, WebSocket drain, slowest ingress component)
+ internal stop hooks and components
+ safety margin
```

A component or hook that has not returned at the deadline is abandoned: the components it depends on are not stopped — nothing closes a database under a consumer that may still use it — and `Run` returns a `*credo.LifecycleError` naming it and every component it kept open. Raise the timeout or fix the component; the supervisor's `SIGKILL` (`TimeoutStopSec`, `terminationGracePeriodSeconds`, `docker stop --time`) must come after it.

### A stop during start-up

The listener is bound before the start phase — each component's `Start` in dependency order, then the start hooks — but serves no request until it has completed, so a probe sent during a long start waits for it; give it a `startupProbe` (or an initial delay) that covers the start. A signal during the start phase interrupts it: the running `Start` sees its context cancelled, nothing further starts, what was built is rolled back under the same shutdown timeout counted from the signal, and `Run` returns nil after a clean rollback. "server started" is never logged and the listener never accepts. A `Start` that ignores the cancellation is abandoned at the deadline and reported, and a second signal still kills the process. A start that fails on its own rolls back the same way and makes `Run` return a `*credo.LifecycleError`, so the process exits non-zero and the supervisor restarts it.

### What `/ready` reports

`/ready` reports every readiness entry — each component whose type shows `Ready`, a bound `*sqldb.DB` among them, and each check added with `app.AddReadinessCheck` — with its status and the time its check took:

```json
{
  "status": "degraded",
  "checks": {
    "*sqldb.DB": {"status": "up", "latency": "1.234ms"},
    "search": {"status": "down", "latency": "5s"},
    "worker:reindex": {"status": "up", "latency": "12µs"}
  }
}
```

An entry is `up` or `down`, and carries `error`, its cause text, only with `HealthConfig.ExposeErrors`; the cause always goes to the log, as `credo: health check failed` with `kind`, `check`, `error` and `latency`, whenever a `/ready` request finds the entry failing. A check that outlives `CheckTimeout` reports the time until the deadline. The top-level status is `up` (200) when every entry is up, `down` (503) when an entry that gates readiness is down, and `degraded` (200) when every gating entry is up and an optional one is not; during the drain `/ready` answers 503 `shutting_down`, whatever its entries show.

Every entry gates readiness unless its registration says otherwise. A dependency the instance can serve without — a cache, a search index, a third-party API — is reported with `credo.OptionalReadiness()`, on its binding or on the check:

```go
app.ProvideValue(searchClient, credo.OptionalReadiness())                    // a component whose type shows Ready
app.AddReadinessCheck("billing-api", billingPing, credo.OptionalReadiness()) // an application check
```

`credo.OptionalReadiness()` changes readiness only: an unreachable database whose readiness is optional still fails the start, because its `Start` pings it. A database whose outage must not stop the start is opened with `sqldb.WithOptionalStart()` as well; its `Start` then logs the failed ping at Warn and returns nil, and the pool connects once the server is back:

```go
db, err := sqldb.Open(&cfg, sqldb.WithOptionalStart())
if err != nil {
    return err
}
app.ProvideValue(db, credo.OptionalReadiness())
```

**Why a database gates by default, and what it costs.** A process that has lost its database while the other replicas have not should leave rotation, so its traffic goes where it can be served; that is the default. A database's `Ready` is a ping through its own pool, though. With `max_open` set, as the pool warning asks, a pool whose connections are all busy holds the ping in its queue until `CheckTimeout`, and an instance that is merely busy leaves rotation and moves its load to the replicas that remain — which are as busy. Size the pool for the load plus the probe, and keep the readiness deadline above the pool's normal wait; a database the instance can serve without, partially, is bound with `credo.OptionalReadiness()` instead.

### The limit of readiness

A component reports a failure that happens after its `Start` has returned through its `Ready` method, which `/ready` aggregates under the component's name. No component ends the App — only the App's own listeners do. That is a deliberate policy, not a recovery guarantee: a failing readiness probe takes the instance out of rotation and restarts nothing, so a component that has stopped for good leaves the process alive, unready and never restarted. To have the supervisor restart the process, tie a liveness check (`app.AddLivenessCheck`) to the component itself — knowing that a check that fails because of a dependency every replica shares, such as the database, makes the orchestrator restart every replica at once, which cannot fix the database and adds a restart storm to its outage. Tie liveness only to state that a restart repairs. The [worker guide](worker.md#escalating-a-terminal-failure) shows the check for a worker that has failed for good.

### An `http.Server` you own

An App served through `ServeHTTP` by a server you build yourself does not run Credo's serving lifecycle: start it with `app.Start(ctx)`, which runs the start phase without a listener, and stop it with `app.Shutdown(ctx)`. An App that has anything to start — a component with `Start` or `Ready`, a start hook, a WebSocket server, a bound database, `UseI18n`, workers — panics in `ServeHTTP` until `app.Start` has succeeded; after a failed start it answers 503. The server's owner owns its admission and drain and completes them **before** `app.Shutdown`, because the internal tier stops after the HTTP drain only if that drain has happened:

```go
if err := app.Start(ctx); err != nil { // the start phase, without a listener
    log.Fatal(err)
}
srv := &http.Server{Addr: ":8080", Handler: app}
go func() {
    if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
        log.Print(err)
    }
}()

<-stop // the owner's shutdown signal

drainCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
_ = srv.Shutdown(drainCtx)                     // first: the owner drains HTTP
if err := app.Shutdown(drainCtx); err != nil { // then: the App stops its tiers
    log.Print(err)
}
```

`app.Start` is accepted once, on an App that has not started; `Run`, `RunContext` or `ServeContext` after it return an error, since the App is single-use. A WebSocket server needs its drain alongside the HTTP drain ([WebSocket guide](websocket.md#external-httpserver)).

---

## What Lands in Your Logs

Beyond Credo's own access and error logs, the standard library's server diagnostics are bridged into the application logger at `ERROR` with `module=net/http`. In production these are the entries worth alerting on:

| Message | Usually means |
| --- | --- |
| `http: TLS handshake error from …` | Wrong protocol on the TLS port, an expired or untrusted client certificate, a scanner, or a cipher/version mismatch |
| `http: Accept error: …; retrying in …` | File-descriptor exhaustion or a listener fault — check `ulimit -n` |
| `http: panic serving …` | A panic outside the framework recovery (only when `WithoutRecover` is set, or from a hijacked connection) |
| `http: superfluous response.WriteHeader call from …` | A handler bug: the response was already committed |

Filter them with `module=net/http` to separate them from Credo's own records. Note that header-limit rejections (`431`) never appear here — `net/http` writes that response directly to the connection without logging it; if you need to count them, terminate at a proxy that records status codes, or install a `ConnState` hook through [`WithHTTPServer`](#tuning-the-http-server).

## Tuning the HTTP Server

Most operational knobs are config keys (`read_timeout`, `max_header_bytes`, `max_header_value_count`, and the rest of the [`server` section](configuration.md#server--server)). For everything else `net/http` offers, `WithHTTPServer` hands you the server the framework built:

```go
app, err := credo.New(credo.WithHTTPServer(func(s *http.Server) {
    // Serve HTTP/2 without TLS, behind a proxy that already terminated it.
    s.Protocols = new(http.Protocols)
    s.Protocols.SetHTTP1(true)
    s.Protocols.SetUnencryptedHTTP2(true)

    // Cap concurrent HTTP/2 streams per connection.
    s.HTTP2 = &http.HTTP2Config{MaxConcurrentStreams: 100}

    // Count connections — including the 431s that never reach the logger.
    s.ConnState = func(_ net.Conn, state http.ConnState) {
        connGauge.WithLabelValues(state.String()).Inc()
    }
}))
```

The callback runs after every field Credo sets, so it wins on all of them, config keys included. Three exceptions are re-imposed afterwards, because the lifecycle depends on them: `Handler` (always the `App`), `Addr` (the listener is bound from it), and `TLSConfig` — configure TLS through `WithTLSFiles`/`WithTLSConfig`, never here, so a plaintext-configured listener can never quietly start serving TLS.

Two more rules: the server's lifecycle methods (`Serve`, `ServeTLS`, `Shutdown`, `Close`, `RegisterOnShutdown`) belong to the framework, and the pointer must not be retained past the callback. The `WithHTTPRedirect` listener is a separate, fixed-function server and is deliberately left untouched. Everything set here is restart-only — a reload does not rebuild the server.

## Reload Without a Signal

When the runtime cannot send `SIGHUP`, or when you want the reload's result to be the exit status of the operation, expose `app.Reload` behind an authenticated admin route. `Reload` is safe to call from a handler: it is serialized, runs only while the server is running, and returns what it did — `config.ReloadResult` — beside the joined errors of what failed.

```go
admin := app.Group("/admin").Middleware(auth.Middleware(adminAuth, nil))

admin.POST("/reload", func(ctx *credo.Context) error {
    res, err := app.Reload(ctx.Context())
    if err != nil {
        return credo.NewHTTPError(http.StatusInternalServerError).WithInternal(err)
    }
    // {"published":true,"changed":[…],"replaced":["log"],"restart_required":["app.name"],"overridden":[]}
    return ctx.Response().JSON(http.StatusOK, res)
})
```

With that in place, a unit file can make `systemctl reload` fail loudly:

```ini
ExecReload=/usr/bin/curl --fail --silent -X POST --unix-socket /run/example/admin.sock http://localhost/admin/reload
```

Bind the admin group to a Unix socket or an internal host via `app.Host` / `ServeContext` rather than the public listener. The result names key paths, never values, so it answers the operator with what was applied and what still needs a restart; the error, which may quote a value, stays in the service log, where `app.Reload` writes it.

---

## Checklist

- `TimeoutStopSec` / `terminationGracePeriodSeconds` / `docker stop --time` > `WithShutdownTimeout`.
- `ExecReload` (or the container equivalent) sends `SIGHUP`; never use `SIGHUP` as the stop signal.
- Runtime-changeable values live in the config file, not in `EnvironmentFile=`.
- Every section you expect to change at runtime is declared live (`Section.Live`); the reload's `restart_required` keys name the ones that are not.
- File-based certificates rotate when their files change; only `WithTLSConfig` needs your own rotation.
- Health probes: `/ready` returns 503 as soon as shutdown starts, so load balancers stop routing before the drain completes ([Getting Started: Health Checks](getting-started.md#health-checks)).
- `WithShutdownTimeout` covers the HTTP drain, the ingress tier and the internal tier together; a component that misses it is abandoned and reported ([Shutdown and Readiness](#shutdown-and-readiness)).
- Liveness checks depend only on state a restart repairs, never on a dependency every replica shares.
- An App served by your own `http.Server` is started with `app.Start` and stopped with `app.Shutdown` after that server has drained.
