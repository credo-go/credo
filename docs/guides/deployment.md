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

What a reload does and does not change is defined in the [Configuration Guide](configuration.md#reloading-configuration): typed `OnConfigChange[T]` subscribers receive their re-decoded sections, file-based TLS certificates are re-read, `OnReload` hooks run, and every other changed key is logged as **restart required**.

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

File-based TLS (`WithTLSFiles` or `server.tls.*`) re-reads its key pair on every reload, so an ACME client only needs to signal the service after renewal:

```ini
# /etc/letsencrypt/renewal-hooks/deploy/example.sh
#!/bin/sh
systemctl reload example
```

New TLS handshakes see the new certificate immediately; open connections are untouched. If the new pair fails to load (a half-written file, a key that does not match), the previous certificate keeps serving and the failure is logged — rotation never takes the service down. `WithTLSConfig` is the exception: Credo never touches a caller-supplied `*tls.Config`, so its owner drives rotation through their own `GetCertificate` (optionally from an `OnReload` hook).

---

## Containers

`docker stop` sends `SIGTERM` and waits `--time` (default 10s) before `SIGKILL`; raise it above your drain budget (`docker stop --time 35 example`, or `stop_grace_period` in Compose). Make sure the Credo binary is PID 1 or runs under an init that forwards signals (`docker run --init`, `tini`), otherwise signals never reach `Run`.

To reload a running container:

```sh
docker kill --signal=HUP example
```

Kubernetes has no reload verb. Its two idioms are:

- **Rolling restart** (`kubectl rollout restart deployment/example`) — the default answer for config and certificate changes delivered through ConfigMaps, Secrets, and image updates. A Credo pod drains gracefully on `SIGTERM`; set `terminationGracePeriodSeconds` above `WithShutdownTimeout`.
- **In-place reload** — only when a restart is too disruptive. Mounted ConfigMap/Secret files update in place (with a delay, and not when mounted via `subPath`), so a sidecar or an operator action can trigger `app.Reload` through an admin endpoint; `kubectl exec example -- kill -HUP 1` works for one-off use.

---

## Shutdown and Readiness

### What a stop does

On `SIGTERM` (or a cancelled `RunContext` context, or `app.Shutdown`), the App drains what it owns in two tiers:

1. `/ready` answers 503 `shutting_down`, so load balancers stop routing; `/health` stays 200, since the process is alive and draining.
2. The **ingress tier** — where work enters the process: the WebSocket server, the worker pool, consumers of external queues and anything registered with `credo.Ingress()` — stops concurrently with the HTTP drain: its `OnStop` hooks in reverse registration order, then its components, those that no dependency orders stopping concurrently with each other.
3. A reload that overlapped the stop finishes.
4. The **internal tier** — everything else, the database pools and clients the handlers and workers use — stops: its `OnStop` hooks in reverse registration order, then its components one at a time, each consumer before the components it depends on.

The steps share one deadline: `WithShutdownTimeout` (`server.shutdown_timeout`, 30 seconds by default) counted from the signal, or the deadline of the context passed to `app.Shutdown`. They spend it in order, so a slow HTTP or WebSocket drain leaves less for the internal tier. Size it as:

```text
max(slowest in-flight request, WebSocket drain, slowest ingress component)
+ internal stop hooks and components
+ safety margin
```

A component or hook that has not returned at the deadline is abandoned: the components it depends on are not stopped — nothing closes a database under a consumer that may still use it — and `Run` returns a `*credo.LifecycleError` naming it and every component it kept open. Raise the timeout or fix the component; the supervisor's `SIGKILL` (`TimeoutStopSec`, `terminationGracePeriodSeconds`, `docker stop --time`) must come after it.

### A stop during start-up

The listener is bound before the start phase — each component's `Start` in dependency order, then the start hooks — but serves no request until it has completed, so a probe sent during a long start waits for it; give it a `startupProbe` (or an initial delay) that covers the start. A signal during the start phase interrupts it: the running `Start` sees its context cancelled, nothing further starts, what was built is rolled back under the same shutdown timeout counted from the signal, and `Run` returns nil after a clean rollback. "server started" is never logged and the listener never accepts. A `Start` that ignores the cancellation is abandoned at the deadline and reported, and a second signal still kills the process. A start that fails on its own rolls back the same way and makes `Run` return a `*credo.LifecycleError`, so the process exits non-zero and the supervisor restarts it.

### The limit of readiness

A component reports a failure that happens after its `Start` has returned through its `Ready` method, which `/ready` aggregates under the component's name. No component ends the App — only the App's own listeners do. That is a deliberate policy, not a recovery guarantee: a failing readiness probe takes the instance out of rotation and restarts nothing, so a component that has stopped for good leaves the process alive, unready and never restarted. To have the supervisor restart the process, tie a liveness check (`app.AddLivenessCheck`) to the component itself — knowing that a check that fails because of a dependency every replica shares, such as the database, makes the orchestrator restart every replica at once, which cannot fix the database and adds a restart storm to its outage. Tie liveness only to state that a restart repairs.

### An `http.Server` you own

An App served through `ServeHTTP` by a server you build yourself does not run Credo's serving lifecycle: start it with `app.Start(ctx)`, which runs the start phase without a listener, and stop it with `app.Shutdown(ctx)`. An App that has anything to start — a component with `Start` or `Ready`, a start hook, workers — panics in `ServeHTTP` until `app.Start` has succeeded; after a failed start it answers 503. The server's owner owns its admission and drain and completes them **before** `app.Shutdown`, because the internal tier stops after the HTTP drain only if that drain has happened:

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

Beyond Credo's own access and error logs, the standard library's server diagnostics are bridged into the application logger at `ERROR` with `component=net/http`. In production these are the entries worth alerting on:

| Message | Usually means |
| --- | --- |
| `http: TLS handshake error from …` | Wrong protocol on the TLS port, an expired or untrusted client certificate, a scanner, or a cipher/version mismatch |
| `http: Accept error: …; retrying in …` | File-descriptor exhaustion or a listener fault — check `ulimit -n` |
| `http: panic serving …` | A panic outside the framework recovery (only when `WithoutRecover` is set, or from a hijacked connection) |
| `http: superfluous response.WriteHeader call from …` | A handler bug: the response was already committed |

Filter them with `component=net/http` to separate them from Credo's own records. Note that header-limit rejections (`431`) never appear here — `net/http` writes that response directly to the connection without logging it; if you need to count them, terminate at a proxy that records status codes, or install a `ConnState` hook through [`WithHTTPServer`](#tuning-the-http-server).

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

When the runtime cannot send `SIGHUP`, or when you want the reload's result to be the exit status of the operation, expose `app.Reload` behind an authenticated admin route. `Reload` is safe to call from a handler: it is serialized, runs only while the server is running, and returns the joined errors of the steps that failed.

```go
admin := app.Group("/admin").Middleware(auth.Middleware(adminAuth, nil))

admin.POST("/reload", func(ctx *credo.Context) error {
    if err := app.Reload(ctx.Context()); err != nil {
        return credo.NewHTTPError(http.StatusInternalServerError).WithInternal(err)
    }
    return ctx.Response().NoContent(http.StatusNoContent)
})
```

With that in place, a unit file can make `systemctl reload` fail loudly:

```ini
ExecReload=/usr/bin/curl --fail --silent -X POST --unix-socket /run/example/admin.sock http://localhost/admin/reload
```

Bind the admin group to a Unix socket or an internal host via `app.Host` / `ServeContext` rather than the public listener, and keep the reload details (which keys changed) in the service log rather than the HTTP response.

---

## Checklist

- `TimeoutStopSec` / `terminationGracePeriodSeconds` / `docker stop --time` > `WithShutdownTimeout`.
- `ExecReload` (or the container equivalent) sends `SIGHUP`; never use `SIGHUP` as the stop signal.
- Runtime-changeable values live in the config file, not in `EnvironmentFile=`.
- Every section you expect to change at runtime has an `OnConfigChange[T]` subscriber; watch the log for `restart required` to find the ones that do not.
- ACME deploy hooks call `systemctl reload` (file-based TLS) or your own rotation (`WithTLSConfig`).
- Health probes: `/ready` returns 503 as soon as shutdown starts, so load balancers stop routing before the drain completes ([Getting Started: Health Checks](getting-started.md#health-checks)).
- `WithShutdownTimeout` covers the HTTP drain, the ingress tier and the internal tier together; a component that misses it is abandoned and reported ([Shutdown and Readiness](#shutdown-and-readiness)).
- Liveness checks depend only on state a restart repairs, never on a dependency every replica shares.
- An App served by your own `http.Server` is started with `app.Start` and stopped with `app.Shutdown` after that server has drained.
