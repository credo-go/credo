# Deployment Guide

> Audience: operators and developers running a Credo service under a process supervisor or container runtime. **Related:** [Getting Started](getting-started.md), [Configuration Guide](configuration.md), [Lifecycle Spec](../specs/lifecycle.md), [ADR-006](../adr/006-application-lifecycle.md), [ADR-020](../adr/020-reload-and-partial-config-reload.md)

This guide covers what a Credo process expects from the environment that runs it: which signals it handles, how shutdown and reload map onto systemd and containers, and how to wire certificate rotation.

---

## Signals at a Glance

`app.Run()` installs the only signal handling Credo does. `RunContext` and `ServeContext` install none — cancellation and reload are entirely the caller's.

| Signal | Under `Run()` | Notes |
| --- | --- | --- |
| `SIGINT`, `SIGTERM` | Graceful shutdown within `WithShutdownTimeout` (default 30s) | A second signal during the drain force-kills the process. |
| `SIGHUP` (Unix only) | `app.Reload(ctx)` within `WithReloadTimeout` (default 30s) | Never terminates the process; a failed reload is logged and the previous configuration keeps serving. Signals arriving mid-reload coalesce into at most one follow-up. |

There is no `SIGHUP` on Windows. The programmatic `app.Reload(ctx)` works identically on every platform and is the trigger to expose from an admin endpoint when the runtime has no reload verb (see [Reload Without a Signal](#reload-without-a-signal)).

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
- **`TimeoutStopSec` must exceed `WithShutdownTimeout`** (or `server.shutdown_timeout`). The drain budget is Credo's; `TimeoutStopSec` is the point at which systemd escalates to `SIGKILL`. Leave a few seconds of headroom so an `OnPreDrain` or `OnShutdown` hook that runs to the deadline is not killed mid-flight.
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
