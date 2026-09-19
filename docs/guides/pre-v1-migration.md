# Pre-v1 Migration Guide

**Status:** The bootstrap/DI changes (DI minor), the router parameter-name change (router minor) and the built-in HTTP feature changes (HTTP minor) are implemented as of 2026-09-05; the [Bootstrap and DI](#bootstrap-and-di), [Built-in HTTP features](#built-in-http-features) and [Router](#router) sections below describe shipped behavior. The URL round-trip change (wire minor) is implemented as of 2026-09-05 and described under [Router](#router) as well. The accepted decisions are recorded in [ADR-022](../adr/022-bootstrap-and-di-ownership.md) (bootstrap and DI ownership), [ADR-007](../adr/007-router-and-routing.md#url-round-trip-amendment) (URL round trips) and [ADR-010](../adr/010-middleware-architecture.md#built-in-http-feature-configuration-criterion) (built-in HTTP features); [TODO](../../TODO.md#pre-v1-contract-migration) tracks progress. The worker contract of v0.20.0 and the restart backoff of v0.21.0 are described under [Workers](#workers); their decisions are recorded in [ADR-023](../adr/023-worker-system.md).

## Bootstrap and DI

Complete dependency registrations, store/worker setup and overrides before an explicit, error-checked `app.Finalize()`. Then resolve controllers/services, capture hook dependencies and bind routes or DI-backed renderers. HTTP setup remains open until shared preparation or shutdown admission. Run's implicit Finalize remains an idempotent safeguard; it cannot precede a Resolve that the composition root has already executed.

| Before the DI minor | Now |
| --- | --- |
| Resolve before Run, relying on Run to Finalize | Add an error-checked Finalize after all DI writes and before the first Resolve; Resolve before Finalize returns a "not finalized" error |
| Resolve just to test optional registration | Use the non-resolving `Has[T]`; it is a snapshot, not a reservation or health check |
| ProvideFactory/MustProvideFactory | Use typed constructors with explicit dependency parameters |
| Preprovided Registry constructor adopted during registration | Provide a ready Registry value; store registration uses AdoptValue and never executes a constructor |
| Replace returning only error | Receive previous instance, existence boolean and error; clean up the old instance only after success |
| MustReplace | Receive the same previous-instance information; replacement error still panics |
| Resolve from a drain hook | Resolve during bootstrap and capture the dependency in the hook closure |

The Replace boolean means an already-created instance existed. An unbuilt constructor yields zero/false; a rejected replacement transfers nothing. A validated adopted binding is protected. Do not recover a constructor panic to retry resolution: the container stores a typed terminal failure. MustResolve still panics on error, with `*credo.DIPanicError` as its payload. Closing/closed resolution matches `credo.ErrDIClosed`; teardown failure is inspectable as `*credo.DIShutdownError` through App-level error joins. Only construction finishing after the shutdown context ends gets the separate five-second cleanup wait; normal Shutdowner calls retain the shared budget.

Building-state Shutdown provides cleanup even after a failed Finalize. It does not drain an externally owned http.Server. Owners of such servers must stop admission and coordinate active HTTP drain before DI teardown. Stopped ServeHTTP returns the framework's default 503 envelope without custom renderers, i18n callbacks or DI access; it does not restart App.

## Built-in HTTP features

**Implemented (HTTP minor, 2026-09-05).** The [HTTP features spec](../specs/http-features.md) is the contract; the [middleware guide](middleware.md#framework-features-not-middleware) shows the new calls.

| Before the HTTP minor | Now |
| --- | --- |
| Recovery on; optional `middleware.Recover` configuration | `WithRecoverConfig(cfg)` configures default-on root recovery; `WithoutRecover()` wins |
| Default RequestID and `WithoutRequestID` | Explicit `app.UseRequestID(cfg...)`; omit it to disable |
| Default AccessLog, `WithoutAccessLog` and the `WithAccessLog*` field helpers | Explicit `app.UseAccessLog(cfg...)` with one `AccessLogConfig{Logger, MinLevel, Skipper, ResultFilter}` |
| `middleware.RequestID` / `middleware.AccessLog` / `middleware.GetRequestID` | Root `Use` registration and `ctx.RequestID()`; access policy through `MetaAccessLog` and config filters |
| `middleware.Compress` / `middleware.Decompress` | `app.UseCompress(cfg...)` / `app.UseDecompress(cfg...)` with root `CompressConfig` / `DecompressConfig` |
| `SetErrorRenderer` / `SetSuccessRenderer` | `app.UseErrorRenderer(renderer)` / `app.UseSuccessRenderer(renderer)`, one successful installation |
| `UseI18n` adding Global middleware | Keep `UseI18n`; custom `Detect` takes `*Context` instead of `*http.Request` and resolves lazily on first use |

Optional features have no parallel constructor/setter/Enabled route. Evaluate external enable flags in application bootstrap. Use-call order does not choose execution order. `WithoutRecover` wins over recovery configuration regardless of option order. Foundational logger, raw config, server, TLS and timeouts stay constructor settings.

AccessLog off does not disable framework or application diagnostics. Logger filtering and access record selection remain separate; WithDebug does not set the slog minimum level. To preserve request correlation and access records, explicitly enable both features. From v0.21.0 the `credo: server started` line lists the features in effect (`"features":["recover","request_id","access_log"]`); the reliable check is still a request to an ordinary route whose response carries a request ID and produces an access record with the same ID.

Scoped recovery is removed. Applications needing their own route policy can author ordinary middleware; Credo does not keep duplicate compatibility wrappers. CORS, CSRF, Secure, Timeout, RateLimit, Rewrite and ContractGuard retain their middleware APIs and ordering responsibilities.

Lazy locale: first Locale/translation access fixes the language and all translation paths share it. Custom Detect receives *Context and can access the request or GetUser. It must handle missing auth data; Timeout/stdlib wrappers may restore a request without downstream auth data. To retain the authenticated language in later errors, call Locale after setting the user, before unwinding; an earlier read still wins. Empty/unresolvable detection uses the configured default. Recursive detection is misuse; panic/re-entry stores the fallback and never retries the detector. Recovery enabled uses the 500 path; disabled recovery propagates panic after cleanup.

A successful UseI18n that finds no conventional catalogs now consumes registration as configured-but-inactive. Do not call it again as a fallback strategy. Explicit-source failures still return errors and permit correction before HTTP preparation.

Decompress runs before Global middleware. Its Skipper uses the original request's method/path/ headers once, so raw-body webhook paths must be selected there, without route/auth dependencies. Rewrite does not repeat selection. Global body readers and binders see the same decoded stream under separate wire/decoded limits.

AccessLog bytes are post-compression accepted body bytes; headers/framing/TLS are excluded. Duration includes finalization and excludes the access filter/log write. Preserve actual committed status when transfer fails. With recovery enabled, an error-rendering failure falls back without callbacks; a post-response ResultFilter panic logs a diagnostic and skips that record. No failure after commitment appends a second JSON body; incomplete compressed output is aborted as required. These failure paths follow WithoutRecover for panics and always release request state.

## Router

**Implemented (router minor, 2026-09-05).** Path parameter names belong to the endpoint: `/customers/{id}` and `/customers/{customer_id}/timeline` coexist, and each handler reads its own names. Nothing needs to change in existing applications — every registration that was valid stays valid with the same captures — and routes that were previously split or renamed to satisfy the shared-name rule may now use their natural names. The `conflicting … parameter` registration panic no longer exists; the same method on the same name-stripped shape is a duplicate (`GET "/users/{name}" is already registered as "/users/{id}"`), and structural regex conflicts remain errors. `BuildURI` reads the selected route's names; host pattern semantics stay unchanged.

**Implemented (wire minor, 2026-09-05).** Route parameters are decoded once and reported in decoded form; constraints apply to the decoded value; parameters are single-segment; generation validates and escapes; malformed input has defined outcomes.

| Before the wire minor | Now |
| --- | --- |
| `RouteParam` returned the raw, still-encoded text: `%2F` stayed `%2F`, `%31` did not satisfy `[0-9]+` | Values are decoded once: `%2F` is `/` inside one segment, `%31` is `1` and satisfies numeric constraints, `%252F` is `%2F`, `+` stays `+`. A decoded value can contain `/` or `..`: check handlers that build file paths from parameters against [Route Parameters Are Not File Names](routing.md#route-parameters-are-not-file-names) |
| A regex could match a prefix of a segment, and a tail-bounded parameter could span a raw slash (`/{name}.json` matched `/a/b.json`) | Constraints apply to the whole decoded value and `{name}`/`{name:regex}` are single-segment; `{name...}` captures several segments |
| `BuildURI`/`BuildURL` pasted values verbatim | Values are validated against their constraints and percent-encoded per segment (`a/b` becomes `a%2Fb`); static text is written in its wire spelling; host labels are validated, never encoded; empty values, invalid UTF-8 and a value whose wire spelling shows its parameter's delimiter are errors |
| Static text with non-ASCII bytes (`/café`) was unreachable through `EscapedPath` matching | Static text and the request meet in one canonical form: every escape except those of reserved characters and `%` is decoded before matching, so any equivalent spelling routes alike, an encoded unreserved delimiter is the delimiter and an encoded reserved character (`%3B`) stays distinct from the literal |
| Invalid UTF-8 in a parameter was captured as bytes | 400 with the code `invalid_path_encoding` when no route matches |
| `ctx.Rewrite` took a decoded path | The target is a wire-form path; a malformed escape is an error |
| Mounted handlers received a decoded remainder with `RawPath` cleared | Mounted handlers receive `URL.Path` decoded plus `URL.RawPath` when the spellings differ |

Migration: remove any second `PathUnescape` of `RouteParam` values, pass raw values to `BuildURI`/`BuildURL` instead of pre-escaped ones, replace `{name:.+}` with `{name...}` where several segments were intended, and expect 400 rather than a captured byte sequence for invalid UTF-8. `OriginalPath` now reports the wire-form path. See [Encoded Parameter Values](../specs/router.md#encoded-parameter-values).

## Workers

**Implemented (v0.20.0; restart backoff in v0.21.0).** The [worker spec](../specs/worker.md) is the contract and the [worker guide](worker.md) shows the new calls. One change compiles unchanged but behaves differently, so check it first:

> **A continuous worker whose `Run` returns nil while the application is running is restarted.** Before v0.20.0 it stopped silently. Now the early return is a failure: it is logged as `worker run failed` with `unexpected_exit=true` and the message `worker: Run returned nil before shutdown; a continuous worker must run until its context is cancelled`, and the worker is restarted with the restart backoff (3 s at first, up to a minute while failures repeat) — for ever, unless `WithMaxRestarts` is set. Before upgrading, look for continuous workers that return nil on purpose. Move finite work to `app.OnStart`, or end `Run` with `<-ctx.Done()` after the work is done. Returning nil after the context is cancelled remains a graceful stop.

| Before v0.20.0 | Now |
| --- | --- |
| `worker.Register(app, w, opts...)` | `worker.Register(app, "name", w, opts...)` |
| `worker.Func("name", fn)` | `worker.Func(fn)`; the name is passed to `Register` |
| `Name() string` on worker types | delete it (harmless if kept; it is no longer called) |
| constructing a worker by hand before `Finalize` because `Resolve` was unavailable | `app.Provide[T](constructor)` + `worker.RegisterProvided[T](app, "name", opts...)`; T is resolved when the pool starts |
| continuous `Run` returns nil → the worker stops | restarted like a failure; see above |
| `WithMaxRestarts(N)`, `N > 0` → failed after N failures (N−1 restarts) | the first run plus N restarts → failed after N+1 failures |
| `WithMaxRestarts(0)` → unlimited restarts | unchanged |
| `info.Schedule` | `info.Config.Schedule` |
| `info.Kind == "scheduled"` | still compiles; prefer `worker.KindScheduled` |
| `info.Attempts` | `info.Restarts` (continuous) / `info.ConsecutiveFailures` (scheduled) |
| `worker.Attempt(ctx)` | removed; use `worker.RunID`/`worker.ScheduledAt`, and `Info` for counters |
| `LastSuccess` set when a continuous `Run` returned nil | never set for continuous workers |
| a scheduled `Run` returning nil during shutdown stamps `LastSuccess` and resets `ConsecutiveFailures` | when shutdown cancellation came first, a graceful stop: both values unchanged, status `stopped` |
| a graceful stop clears `LastError` | a graceful stop changes only the status; `LastError` keeps the most recent failure until a successful scheduled run clears it |
| a hand-rolled `context.WithTimeout` inside `Run` | `worker.WithRunTimeout(d)`; a timed-out run is a failure even if it returns nil |
| `@every 0s`, a negative `@every` or `@every 1500ms` silently became one second | a registration error |
| `LastError` may contain a panic stack trace | never; the stack is the `stack` attribute of the failure log line |
| log `worker stopped during scheduled run`; continuous `worker stopped` only on some exit paths | exactly one `worker started` and one `worker stopped` (`reason=shutdown` or `reason=failed`) per worker |
| log `worker tick skipped`, one line per skipped activation | one `worker ticks skipped` line per resumption with `skipped=N` |
| `restart` attribute of the continuous `worker run failed` line, counting failed runs (`1` on the first failure) | renamed `restarts`; equals `Info.Restarts` — restarts that actually started, so `0` on the first failure |
| names silently trimmed | surrounding whitespace and control characters are rejected |
| untagged (Go field name) JSON from `pool.Workers()` | snake_case field names; empty `last_run`/`last_success`/`last_error`/`config.readiness` omitted |
| `worker.Definition` (exported, returned by no API) | removed; `info.Config` is the public view of a registration |
| a continuous worker reported `running` as soon as the pool started | `idle` until its first run is admitted |

New and unchanged behavior worth knowing while migrating: `WithRunTimeout` is scheduled-only; failure log lines now carry `run_id` (equal to `worker.RunID(ctx)`) and `duration`; a successful scheduled run logs `scheduled worker run completed` at Debug; `Info.Config` reports the effective configuration, so a registration test can assert every worker's policy without running it.

v0.21.0 changes the wait between continuous restarts without a compile error. Both rows apply to workers registered without restart options too:

| Before v0.21.0 | Now |
| --- | --- |
| every restart waits the fixed restart delay (3 s by default) | the first restart waits the restart delay; repeated failures back off with jitter up to the cap (`DefaultMaxRestartDelay`, 1 min, or `worker.max_restart_delay`), and a run that lasted at least the cap resets the sequence. The same value in `WithRestartDelay` and `WithMaxRestartDelay` keeps a fixed delay |
| `WithMaxRestarts(N)` reaches `failed` after N fixed waits (15 s for N = 5 with the defaults) | the waits back off, so `failed` — and a `FailWhenFailed` readiness drop — comes later: roughly 48–93 s for N = 5 with the defaults |

New with it: `WithMaxRestartDelay`, `DefaultMaxRestartDelay`, the `worker.max_restart_delay` configuration key, `Config.MaxRestartDelay` (`max_restart_delay` in JSON) and the `next_restart_in` attribute of `worker run failed`. A restart delay above one minute, per worker or per pool, stays fixed unless a larger cap is configured.

## Examples and downstream impact

The [example migration map](../../examples/README.md) identifies runnable changes by release. SaaS finalizes before resolving TenantService (DI minor), enables RequestID/AccessLog explicitly and installs compression with `UseCompress` instead of global middleware (HTTP minor). Hello remains a minimal default-profile example.

DI evidence comes from a 2026-09-05 scan of the maintainer's downstream applications: no factory/Replace calls, pre-Run resolution, and a worker-pool existence probe. The same applications install renderers once at bootstrap and use no scoped Recover, so their HTTP-minor migration is a rename of the renderer setters plus explicit `UseRequestID`/`UseAccessLog` calls.
