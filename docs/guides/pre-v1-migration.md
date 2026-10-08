# Pre-v1 Migration Guide

**Status:** The bootstrap/DI changes (DI minor), the router parameter-name change (router minor) and the built-in HTTP feature changes (HTTP minor) are implemented as of 2026-09-05; the [Bootstrap and DI](#bootstrap-and-di), [Built-in HTTP features](#built-in-http-features) and [Router](#router) sections below describe shipped behavior. The URL round-trip change (wire minor) is implemented as of 2026-09-05 and described under [Router](#router) as well. The accepted decisions are recorded in [ADR-022](../adr/022-bootstrap-and-di-ownership.md) (bootstrap and DI ownership), [ADR-007](../adr/007-router-and-routing.md#url-round-trip-amendment) (URL round trips) and [ADR-010](../adr/010-middleware-architecture.md#built-in-http-feature-configuration-criterion) (built-in HTTP features); [TODO](../../TODO.md#pre-v1-contract-migration) tracks progress. The worker contract of v0.20.0, the restart backoff of v0.21.0 and the v0.24.0 workers as components are described under [Workers](#workers); their decisions are recorded in [ADR-023](../adr/023-worker-system.md). The `store/sqldb` move to Bun v1.3.0 in v0.22.0 is described under [Data access](#data-access); [ADR-015](../adr/015-data-access.md) records the data-access decisions. The v0.23.0 router fixes are described under [Router](#router), and its YAML change under [Configuration](#configuration). The v0.24.0 sequential bootstrap is described under [Sequential bootstrap](#sequential-bootstrap), its lifecycle components under [Lifecycle components](#lifecycle-components), its stores, WebSocket server and i18n on the components under [Stores, WebSocket and i18n](#stores-websocket-and-i18n), its seven-method DI surface under [DI surface](#di-surface), and its workers as components under [Workers as components](#workers-as-components); [ADR-024](../adr/024-lifecycle-components.md) records the component decisions. Its error rules are described under [Error handling](#error-handling) and its `StatusHandler` check under [Router](#router).

## Bootstrap and DI

Complete dependency registrations, store/worker setup and overrides before an explicit, error-checked `app.Finalize()`. Then resolve controllers/services, capture hook dependencies and bind routes or DI-backed renderers. HTTP setup remains open until shared preparation or shutdown admission. Run's implicit Finalize remains an idempotent safeguard; it cannot precede a Resolve that the composition root has already executed.

| Before the DI minor | Now |
| --- | --- |
| Resolve before Run, relying on Run to Finalize | Add an error-checked Finalize after all DI writes and before the first Resolve; Resolve before Finalize panics ([Sequential bootstrap](#sequential-bootstrap)) |
| Resolve just to test optional registration | Use the non-resolving `Has[T]`; it is a snapshot, not a reservation or health check |
| ProvideFactory/MustProvideFactory | Use typed constructors with explicit dependency parameters |
| Preprovided Registry constructor adopted during registration | Nothing to provide: v0.24.0 removed `store.Registry` ([Stores, WebSocket and i18n](#stores-websocket-and-i18n)) |
| Replace / MustReplace | `credo.Override()` on `Provide` or `ProvideValue` before `Finalize`, or `testutil.WithOverride` in tests ([DI surface](#di-surface)) |
| Resolve from a drain hook | Resolve during bootstrap and capture the dependency in the hook closure |

Do not recover a constructor panic to retry resolution: the container stores a typed terminal failure. MustResolve still panics on error, with `*credo.DIPanicError` as its payload. Closing/closed resolution matches `credo.ErrDIClosed`; teardown failure is inspectable as `*credo.LifecycleError` (`*credo.DIShutdownError` before v0.24.0) through App-level error joins. Only construction finishing after the shutdown context ends gets the separate bounded late cleanup; ordinary `Shutdown` calls share the drain deadline.

Building-state Shutdown provides cleanup even after a failed Finalize. It does not drain an externally owned http.Server. Owners of such servers must stop admission and complete the HTTP drain before `App.Shutdown`. Stopped ServeHTTP returns the framework's default 503 envelope without custom renderers, i18n callbacks or DI access; it does not restart App.

### Sequential bootstrap

**Implemented (v0.24.0).** Bootstrap is sequential and every mistake has one phase: registration panics on misuse at the line that made it, and `Finalize` reports the whole graph at once. The [bootstrap spec](../specs/bootstrap-and-di-lifecycle.md#sequential-bootstrap) gives the documented order.

| Before v0.24.0 | Now |
| --- | --- |
| `Provide`, `ProvideValue`, `Alias` and `BindMany` returned an error | They return nothing and panic on misuse — a constructor of the wrong shape, a duplicate binding, types that do not fit, a call after `Finalize` or after shutdown began — with a message naming the call and the remedy (`credo: App.Provide[*app.X]: *app.X is already registered; …`). Drop the error checks: `if err := app.Provide[T](ctor); err != nil` no longer compiles |
| `Resolve`, `ResolveAll` and their `Must` forms before `Finalize` returned a "not finalized" error | They panic (`credo: App.Resolve[T]: called before Finalize; call Finalize first …`); call `Finalize` first. After a failed `Finalize`, `Resolve` returns the `Finalize` error |
| Registration calls were safe for concurrent use | Registration comes from the goroutine that builds the App, before it runs, and is not safe for concurrent use; an application that registers from several goroutines serializes the calls itself |
| `Finalize` reported the first cycle it found and each missing dependency by its direct consumer (`di: Validate: … dependency … is not registered`) | It reports every missing dependency with its whole path from the registration that needs it (`di: missing dependency: *app.OrderService → *app.PaymentClient → *http.Client (not registered); provide *http.Client before Finalize`) and every cycle, joined in registration order with the same text on every run. Code or tests that match the old `di: Validate: …` text must change |

The `Must*` registration twins (`MustProvide`, `MustProvideValue`, `MustAlias`, `MustBindMany`) are removed, since the plain calls panic on misuse themselves ([DI surface](#di-surface)). A `Use*` call after preparation or shutdown panics as before; it no longer synchronizes with a preparation running on another goroutine.

### Lifecycle components

**Implemented (v0.24.0).** The App has one lifecycle abstraction, the component: a value with `Shutdown(ctx) error` whose teardown the App owns, started when its binding's type shows `Start`, asked for readiness when it shows `Ready`, and stopped in two tiers — ingress beside the HTTP drain, internal after it — after its consumers. Every former teardown mechanism became a component or a hook that is one. The [lifecycle spec](../specs/lifecycle.md) is the contract; the [dependency-injection guide](dependency-injection.md#shutdown-and-lifecycle) and the [deployment guide](deployment.md#shutdown-and-readiness) show the calls.

| Before v0.24.0 | Now |
| --- | --- |
| `credo.Shutdowner` | `credo.Component`, with the same method set: every `Shutdowner` is a component without a change. Rename the assertions (`var _ credo.Component = (*X)(nil)`) |
| `*credo.DIShutdownError` | `*credo.LifecycleError`, which covers a failed or interrupted start as well as the drain; each entry names the component, its tier, its phase and its outcome (`failed`, `panicked`, `abandoned`, `kept_open`) |
| `app.OnShutdown(fn)`, run LIFO after DI teardown | `app.OnStop(fn)`, which runs LIFO **before** its tier's components stop, so it can still use them. A hook that closed a resource becomes a component — a DI binding with `Shutdown` or with `credo.Closer()`, whose consumers' constructor parameters order its shutdown after theirs, or, for a value nothing in DI uses, a value handed to `app.Manage`, which has no dependency edges — or `app.OnStop(fn, credo.Ingress())` when it must run alongside the HTTP drain; cleanup that must outlive every component moves after `Run` returns |
| `app.OnPreDrain(fn)` / `app.OnDrain(fn)` | An ingress component (`credo.Ingress()` on its binding, or `app.Manage(v, credo.Ingress())`), which stops concurrently with the HTTP drain and before the internal components it uses, or `app.OnStop(fn, credo.Ingress())` |
| The `OnStart` context lived for the App and was cancelled at shutdown; goroutines launched from a hook watched it | The context ends when the hook returns, or earlier when a shutdown interrupts the start phase. Background work belongs to a component whose `Start` launches it on `context.WithoutCancel(ctx)` with its own cancel and whose `Shutdown` stops it, or to a worker |
| A shutdown requested during startup waited for every `OnStart` hook, then logged "server started" and drained; `Shutdown` refused a starting App | The first signal, the context of `RunContext` or `ServeContext`, or `Shutdown` — now accepted while starting — interrupts the start: the running `Start` sees its context cancelled, nothing further starts, what was built is rolled back, and `Run` returns nil after a clean rollback |
| A type that shows `Start` and `Shutdown`, started by an `OnStart` hook | The App starts it itself, so the hook would start it a second time: delete the hook. Behind an interface that hides either method, the hook alone starts it |
| A value behind an interface that the App should start or ask for readiness | Provide the concrete type and `Alias` the interface: `Start`, `Ready` and the tier are planned from the binding's type |
| `ServeHTTP` (an external `http.Server`, `httptest`) never ran `OnStart` hooks | An App with anything to start — a component with `Start` or `Ready`, a constructor handed to `Manage`, a start hook, workers — panics in `ServeHTTP` until `app.Start(ctx)` (`testutil.Start(t, app)` in tests) has succeeded. The server's owner still drains it before `app.Shutdown` |
| A Credo App mounted with `Mount` served on its own | Hand it to the parent with `parent.Manage(child)`, so it starts in the parent's start phase and stops after the parent's HTTP drain; otherwise its first request panics when it has anything to start |
| An internal component could depend on anything | An internal component that depends on an ingress one, directly or through bindings that are not components, fails `Finalize` with the path: declare it ingress, or split the ingress component so that what internal components use is an internal part |
| One pointer held by several bindings was shut down once per binding, the first time before its consumers | A resource — one pointer, or a wrapper and the handle it embeds — is shut down once, through the holder registered first and after the consumers of all of them. A wrapper that embeds a handle which identifies itself, such as `*sqldb.DB`, and releases state of its own in its own `Shutdown` holds the handle in a named field instead |
| `testutil.WithOverride[T]` added a binding when the wiring had none | It replaces through `credo.Override()` and panics without an earlier binding; add bindings with `testutil.WithWiring` |

New with it: the capabilities `credo.Starter`, `credo.Readier` and `credo.ResourceIdentifier`; `credo.Tier`; the registration options `credo.Ingress()`, `credo.Borrowed()`, `credo.Closer()`, `credo.Override()` and `credo.Named(name)`; `app.Manage`, `app.Start` and `testutil.Start`; the panic phase `credo.DIPanicStart`; and the per-component `Ready` aggregated by `/ready` from the values the start phase built.

### Stores, WebSocket and i18n

**Implemented (v0.24.0).** Stores, the WebSocket server and i18n run on the lifecycle components. A store is a DI binding that `store.Register` names and the start phase pings; the WebSocket server is an ingress component the application registers; `UseI18n` validates at the call and reads its locale files in the start phase. Registering any of them does no I/O and panics on misuse, and what touches the outside world fails the start instead. The [store spec](../specs/store.md#registration) is the contract; the [data-access guide](data-access.md#what-storeregister-does), the [WebSocket guide](websocket.md#registering-the-server) and the [localization guide](localization.md#enabling-i18n) show the calls.

| Before v0.24.0 | Now |
| --- | --- |
| `store.Register[R](app, value, opts...) error`, which pinged the store at the call and published a protected binding | Bind the store where its ownership is decided — `app.ProvideValue(value)`, or `app.Provide[R](ctor)`, with `credo.Borrowed()` on `ProvideValue` for a handle the caller shares — and name that binding with `store.Register[R](app, opts...)`, which takes no value and returns nothing. Drop the error check and the cleanup on its failure: misuse panics at the call, a registration whose `R` has no binding fails `Finalize`, and the ping runs in the start phase, where a failure is a start failure that rolls back what was built. A store constructor now runs in the start phase, not at registration |
| A registered store's binding could not be replaced | The binding is an ordinary one: `credo.Override()` or `testutil.WithOverride[R]` replaces it before `Finalize`, and the override is what the App pings, reports and shuts down |
| `store.WithLifecycle(lc)` with `store.WithCallerOwnedLifecycle()` | A wrapper type that implements `store.Lifecycle` around the value, bound and registered by its type; caller ownership is `credo.Borrowed()` on the wrapper's `ProvideValue` |
| `store.LifecycleIdentityProvider` on a named-field wrapper | `credo.ResourceIdentifier`, with the same method `ResourceIdentity() any`; a wrapper that embeds `*sqldb.DB` inherits it |
| `store.Registry` and `Registry.HealthAll` | Removed, with no replacement API: `/ready` reports every registered store |
| `/ready` resolved each store from DI per request | Each store's readiness probe is built once, when the start phase pings it; before the App has started there are no store checks |
| `websocket.Use(app, cfg)` | `ws := websocket.New(app.NewInfra("websocket"), cfg)` and `app.Manage(ws, credo.Ingress())`, or a binding with `credo.Ingress()` when controllers take the server as a dependency. A server that was never started refuses every upgrade with a 500 and logs the missing registration |
| `if err := app.UseI18n(cfg); err != nil { … }` | `app.UseI18n(cfg)`: it returns nothing and panics on misuse — more than one config, `Dir` with `DirFS`, an `i18n` section that does not decode, an invalid `Default` or programmatic message, a second call. Locale files are read in the start phase, and a missing explicit source or a malformed file fails the start |
| An App served through `ServeHTTP` that used stores, i18n or WebSocket needed no start | Each has start work — the store ping, the locale read, opening WebSocket admission — so the App is started with `app.Start(ctx)` (`testutil.Start(t, app)` in tests) before `ServeHTTP`, which panics until then |

From the binding on, the App owns a store bound with `ProvideValue`: a composition root that fails before `Run` releases it with `app.Shutdown(ctx)`, which tears down an App that never ran, instead of closing the value itself.

### DI surface

**Implemented (v0.24.0).** The container's public surface is seven methods and `Finalize`: `Provide`, `ProvideValue`, `Alias`, `BindMany`, `Has`, `Resolve` and `ResolveAll`, with `MustResolve` and `MustResolveAll` kept as resolve conveniences. Nothing framework-owned is bound in the container any more — stores are named through the store registry and workers are components — so the methods that existed to defend framework values in it are removed, and so are the registration twins that duplicated calls which already panic. The [container spec](../specs/container.md) is the contract and the [dependency-injection guide](dependency-injection.md#replacing-a-binding) shows the calls.

Every change here is a compile error, so the compiler finds each call site:

| Before v0.24.0 | Now |
| --- | --- |
| `app.Replace[T](v)`, which returned the superseded instance and handed its shutdown to the caller, and `app.MustReplace[T](v)`, with its `credo: Replace superseded a component; the caller now owns its shutdown` Warn line | `app.ProvideValue[T](v, credo.Override())` (or `app.Provide[T](ctor, credo.Override())`) before `Finalize`, or `testutil.WithOverride[T](v)` in tests; both panic when there is no earlier binding of `T`. Nothing is handed back: an override's replaced value never becomes the App's, so the App neither starts nor shuts it down, and a value the caller built and then overrode is the caller's to release |
| A `Replace` after `Finalize`, which returned an error | No equivalent: after `Finalize` the graph is fixed. Move the override before `Finalize` |
| `app.ProvideProtectedValue[T](v)`, `app.ProtectBinding[T](...)`, `app.AdoptValue[T](validate)` | No successor: they existed for framework integrations that bound their infrastructure in the container, and nothing framework-owned is bound any more. An application that used them for its own values binds them with `app.ProvideValue[T](v)`; a binding is now always replaceable with `credo.Override()` |
| `app.CanProvideValue[T]()` | `app.Has[T]()` for the presence question — it never constructs and reserves nothing — or simply `app.ProvideValue[T](v)`, which panics on a duplicate |
| `app.MustProvide[T](ctor)`, `app.MustProvideValue[T](v)`, `app.MustAlias[I, T]()`, `app.MustBindMany[I, T]()` | `app.Provide[T](ctor)`, `app.ProvideValue[T](v)`, `app.Alias[I, T]()`, `app.BindMany[I, T]()`: the plain call panics on misuse |

## Error handling

**Implemented (v0.24.0).** Both changes compile unchanged and change the status a client receives, so look for them first. The rules are recorded in [ADR-011](../adr/011-validation-strategy.md#rule-errors-client-messages-and-internal-failures) and [ADR-009](../adr/009-handler-and-error-handling.md#an-explicit-status-wins); the [error-handling guide](error-handling.md#rule-errors-client-messages-and-internal-failures) shows them.

| Before v0.24.0 | Now |
| --- | --- |
| Any error a validation rule returned — `errors.New("must be a 2-letter code")` in a `validation.By` function, or a database error from a lookup inside a rule — became a 422 violation with code `invalid` and the error's text as its message, and validation went on with the other fields | Only a `*validation.ValidationError` or `validation.Errors` is a violation. Any other error is internal: validation stops, and the error leaves `Validate` and `BindBody`/`BindQuery` unchanged, so the pipeline renders a plain error as a 500 with the default message (its text is logged, never rendered) and a fault with its mapped status (`store`'s unavailable kind → 503). A rule whose message is meant for the client returns `validation.NewError(code, message)` |
| `credo.NewHTTPError(409, "tenant_conflict").WithInternal(vErrs)` rendered 422 `validation_failed`, because classification looked for validation errors anywhere in the chain first | An explicitly constructed `HTTPError` decides the status: it renders 409 `tenant_conflict`, and the wrapped errors stay in `ErrorInfo.Err`. Validation errors that no `HTTPError` wraps still render as 422 |
| A `Validate` method that returned a single `*validation.ValidationError` (not `validation.Errors`) rendered a 500 | It renders 422 `validation_failed` with one violation |

Migration: search for `validation.By(` and for `Validate(value` methods of custom rules, and turn each `errors.New`/`fmt.Errorf` that carries a message for the client into `validation.NewError("<code>", "<message>")`, choosing a lowercase snake-case code; keep returning a plain error where the failure is the server's. A client or test that expected 422 `invalid` for such a message now sees the new code. Search for `WithInternal(` around validation errors: the status you constructed is now the one sent.

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

A UseI18n that finds no conventional catalogs consumes registration as configured-but-inactive. Do not call it again as a fallback strategy. From v0.24.0 every call consumes the registration: misuse panics at the call, and an explicit source that is missing or invalid fails the start ([Stores, WebSocket and i18n](#stores-websocket-and-i18n)).

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

**Implemented (v0.23.0).** A catch-all matches an empty rest, and a pattern names each parameter once, as in chi and `net/http.ServeMux`.

| Before v0.23.0 | Now |
| --- | --- |
| `/files/{path...}` needed a non-empty rest: `/files/` answered 404, or a trailing-slash redirect to a sibling `/files` route | `/files/` reaches the catch-all with `path` = `""`; `/files` without a route of its own is redirected to `/files/` |
| A mount was reached through `/admin` only; `/admin/` was redirected to `/admin` (404 with `WithRedirectTrailingSlash(false)`), likewise `/t/acme/` under `Mount("/t/{tenant}", h)` | `/admin/` and `/t/acme/` reach the mounted handler as its root `/` |
| `Static("/static", fsys)` served `/static/` through a redirect to `/static` | `/static/` serves the index directly |
| `BuildURI("")` failed for a catch-all | It builds the prefix with the slash (`/files/`) |
| `/a/{id}/b/{id}` registered; `RouteParam("id")` returned the first capture, `RouteParams()` and `URLParam` the last | Registration panics with `duplicate parameter name "id"`, whether the repeat comes from the route, a group or mount prefix, a rewrite rule or a host pattern (`{a}.{a}.example.com`) |
| A host parameter without a name (`{}.example.com`) or with an empty constraint (`{org:}.example.com`) registered | `app.Host` panics |
| `Mount("/t/{_mount}", h)` registered and handed the child the rest of the path as `_mount`; under `Static`, a group prefix parameter `_static` was served as the file path | Both panic: `_mount` and `_static` are reserved for the captures `Mount` and `Static` add |

Migration: an application that registers both `GET /files` and `GET /files/{path...}` and relied on `/files/` being redirected to `/files` handles the empty capture in the catch-all handler, or registers `GET /files/` explicitly. A pattern that repeats a parameter name gives each capture its own name (`/a/{a_id}/b/{b_id}`).

**Implemented (v0.24.0).** `StatusHandler(code, h)` accepts 404 and 405 only, the two codes the router consults.

| Before v0.24.0 | Now |
| --- | --- |
| A handler for any other code — 403, 500 — was accepted and never called | Registration panics with `credo: App.StatusHandler(403): only 404 (http.StatusNotFound) and 405 (http.StatusMethodNotAllowed) are consulted; …`, so the App fails at startup |

Migration: delete such a registration rather than porting it — it never ran — and shape that response through `app.UseErrorRenderer`. A 404 or 405 handler that only returns the matching sentinel (`credo.ErrNotFound`, `credo.ErrMethodNotAllowed`) equals the default and can go in the same change. See [ADR-007](../adr/007-router-and-routing.md#status-handlers).

## Configuration

**Implemented (v0.23.0).** A YAML config file holds one document.

| Before v0.23.0 | Now |
| --- | --- |
| A file with a second document (`a: 1`, `---`, `b: 2`) loaded the first and dropped the rest without a word, a malformed second document included; `---`, `---`, `a: 1` loaded an empty config | The load fails with `yaml: a config file holds one document; found another after the first`, or with the second document's syntax error |
| A trailing `---` with nothing after it was ignored | It opens a second, empty document and fails the load |

Migration: merge a multi-document config into one mapping — the application ran with the first document only, so its values are the ones in effect — and delete a stray trailing `---`. A leading `---` and a closing `...` stay valid.

## Workers

### Workers as components

**Implemented (v0.24.0).** Each worker is a lifecycle component of the App. `worker.Use(app)` returns a supervisor — a registry and reporting object with no lifecycle of its own — whose registration method chooses the worker's kind, and each registration adds a component named `worker:<name>` that the App starts in the start phase and stops in its tier: a scheduled worker with the HTTP drain, a continuous worker after it and before the components it depends on. Configuration is a per-kind struct in code. The [worker spec](../specs/worker.md) is the contract and the [worker guide](worker.md) shows the calls.

None of these changes is a compile error, so check them first:

| Before v0.24.0 | Now |
| --- | --- |
| The `worker` configuration section (`worker.restart_delay`, `worker.max_restart_delay`) set the restart floor and cap of every continuous worker | No configuration section is read. A leftover `worker` section is ignored — it is an unknown key only when the application decodes the whole configuration tree into a struct under `WithStrictDecoding` — and every worker runs with the package defaults (3 s, 1 min) unless its registration sets `Restart{MinDelay, MaxDelay}`. Settings that must come from the environment move to a typed section the application reads itself ([worker guide](worker.md#configuration)) |
| Snapshot JSON: the configuration under `config` (`schedule`, `start_immediately`, `run_timeout`, `max_consecutive_failures`, `max_restarts`, `restart_delay`, `max_restart_delay`, `readiness`); `last_run`; `last_success` | `schedule` at the top level, and the resolved configuration under `continuous` (`tier`, `restart.disabled`, `restart.limit`, `restart.min_delay`, `restart.max_delay`, `unready_when_failed`) or `scheduled` (`tier`, `run_on_start`, `run_timeout`, `max_consecutive_failures`, `unready_when_failed`, `unready_until_first_success`, `unready_after_success_age`); `last_started_at`; `last_succeeded_at`. Dashboards and alert rules on the old field names must change |
| Statuses `idle`, and `waiting` for a continuous worker waiting to restart | `pending`, and `backoff` for a continuous worker waiting to restart; `waiting` belongs to a scheduled worker between activations |
| The log messages in the next table | Their new names; queries and alerts on the old messages must change |
| Registration misuse returned an error (`MustRegister` panicked), so a schedule read from configuration that did not parse was an error the composition root could handle | Every registration panics on misuse at the call, with a message naming the call and the remedy (`worker: Scheduled("report") after app.Finalize; register workers before Finalize`). Validate a schedule from configuration with `worker.ParseSchedule` first, which returns the error |
| Every worker's context was cancelled when the drain began, concurrently with the HTTP drain | A worker is stopped in its tier's turn. A scheduled worker still stops with the HTTP drain; a continuous worker now stops **after** it, once the handlers that enqueue its work have finished, and before the components it depends on. A continuous worker that consumes an external queue declares `Tier: credo.TierIngress` to stop with the listener as before |
| A `Run` that ignored cancellation past the drain deadline made the drain report a timeout, and the teardown went on to close the resources that `Run` still used | The worker's component is abandoned at the deadline: the components it depends on stay open, and `Run` (or `Shutdown`) returns a `*credo.LifecycleError` naming `worker:<name>` as abandoned |

| Log line before v0.24.0 | Now |
| --- | --- |
| `scheduled worker run failed` | `worker run failed` with `kind=scheduled`: one message for both kinds |
| `scheduled worker run completed` (Debug) | `worker run completed` (Debug), with `kind=scheduled` |
| `worker ticks skipped` | `worker activations skipped` |
| `worker exceeded max restarts` with `max_restarts` | `worker failed` with `reason=restart_limit` and `limit` |
| `worker exceeded max consecutive failures` with `max_consecutive_failures` | `worker failed` with `reason=failure_limit` and `limit` |
| `worker schedule has no future activation` | `worker failed` with `reason=schedule_exhausted` and `schedule` |
| `worker started` with `start_immediately=true` | `worker started` with `run_on_start=true` |

`worker failed` is now the single terminal line, also written with `reason=restart_disabled` for a worker registered with `Restart{Disabled: true}`. `worker started`, the continuous `worker run failed` line and `worker stopped` keep their messages and attributes.

The calls and types are renamed:

| Before v0.24.0 | Now |
| --- | --- |
| `worker.Register(app, name, w, opts...) error` / `worker.MustRegister(app, name, w, opts...)` | `workers := worker.Use(app)`, then `workers.Continuous(name, w, cfg)` or `workers.Scheduled(name, expr, w, cfg)`; both return nothing and panic on misuse |
| `worker.RegisterProvided[T](app, name, opts...) error` / `worker.MustRegisterProvided[T](app, name, opts...)` | `workers.ContinuousProvided[T](name, cfg)` or `workers.ScheduledProvided[T](name, expr, cfg)`: `T` is built in the start phase, after the components it depends on, and the worker stops before them. One `T` under two names panics |
| `WithSchedule(expr)` | the `expr` argument of `Scheduled` and `ScheduledProvided`; the method chooses the kind |
| `WithStartImmediately()` | `ScheduledConfig{RunOnStart: true}` |
| `WithRunTimeout(d)` | `ScheduledConfig{RunTimeout: d}` |
| `WithMaxConsecutiveFailures(n)` | `ScheduledConfig{MaxConsecutiveFailures: n}` |
| `WithMaxRestarts(n)` | `ContinuousConfig{Restart: worker.Restart{Limit: n}}` |
| `WithRestartDelay(d)` / `WithMaxRestartDelay(d)` | `worker.Restart{MinDelay: d}` / `worker.Restart{MaxDelay: d}` |
| `WithReadiness(worker.ReadinessPolicy{RequireFirstSuccess, FailWhenFailed, MaxSuccessAge})` | The configuration fields `UnreadyUntilFirstSuccess`, `UnreadyWhenFailed` and `UnreadyAfterSuccessAge`; `UnreadyWhenFailed` exists on both kinds, the other two on `ScheduledConfig` |
| `pool.Workers()` | `workers.Snapshot()`, or `workers.Lookup(name)` for one worker |
| `info.Config` | `info.Continuous` or `info.Scheduled`, the resolved configuration of the worker's kind (`info.Config.MaxRestarts` → `info.Continuous.Restart.Limit`, `info.Config.RestartDelay` → `info.Continuous.Restart.MinDelay`, `info.Config.StartImmediately` → `info.Scheduled.RunOnStart`, …); `info.Config.Schedule` → `info.Schedule` |
| `info.LastRun` / `info.LastSuccess` | `info.LastStartedAt` / `info.LastSucceededAt` |
| `worker.StatusIdle` | `worker.StatusPending`; `worker.StatusBackoff` is new |
| `worker.RunID(ctx)`, `worker.WorkerName(ctx)`, `worker.ScheduledAt(ctx)` | `run, ok := worker.CurrentRun(ctx)` with `run.ID`, `run.Worker` and `run.ScheduledAt` |
| `worker.DefaultRestartDelay` | `worker.DefaultMinRestartDelay` |
| `*worker.Pool` resolved from DI or taken as a constructor parameter | The supervisor `worker.Use` returns, passed on from the composition root; to inject it, bind it yourself with `app.ProvideValue(workers)` |
| `Pool.Start`, `Pool.Shutdown`, `worker.Option`, `worker.Config`, `worker.ReadinessPolicy` | Removed: the App starts and stops each worker's component, and the configuration is the two per-kind structs |

Before v0.24.0 the pool stopped every worker before DI teardown whatever the registration order, so a worker registered by value could use a database safely. Now a worker registered by value has no dependency edges — the App cannot see what a value uses — and takes its place in its tier by registration order alone. Register a worker that uses infrastructure in its provided form, so the App starts it after its dependencies and stops it before them. Workers also start as components now: in the start phase, before their tier's `OnStart` hooks, where the pool used to start inside an `OnStart` hook in registration order.

New with it: per-worker `Tier`, `Restart.Disabled` (the first failure is terminal), `Supervisor.Lookup`, `worker.CurrentRun` with `worker.RunInfo`, and the escalation of a terminal failure through a liveness check built on `Lookup` ([worker guide](worker.md#escalating-a-terminal-failure)).

### Worker contract and restart backoff

**Implemented (v0.20.0; restart backoff in v0.21.0).** The rows below name today's calls; [Workers as components](#workers-as-components) maps the v0.20.0 names to them. One change compiles unchanged but behaves differently, so check it first:

> **A continuous worker whose `Run` returns nil while the application is running is restarted.** Before v0.20.0 it stopped silently. Now the early return is a failure: it is logged as `worker run failed` with `unexpected_exit=true` and the message `worker: Run returned nil before shutdown; a continuous worker must run until its context is cancelled`, and the worker is restarted with the restart backoff (3 s at first, up to a minute while failures repeat) — for ever, unless `Restart.Limit` is set. Before upgrading, look for continuous workers that return nil on purpose. Move finite work to `app.OnStart`, or end `Run` with `<-ctx.Done()` after the work is done. Returning nil after the context is cancelled remains a graceful stop.

| Before v0.20.0 | Now |
| --- | --- |
| `worker.Register(app, w, opts...)` | a name at registration: `workers.Continuous("name", w)` or `workers.Scheduled("name", expr, w)` |
| `worker.Func("name", fn)` | `worker.Func(fn)`; the name is passed at registration |
| `Name() string` on worker types | delete it (harmless if kept; it is no longer called) |
| constructing a worker by hand before `Finalize` because `Resolve` was unavailable | `app.Provide[T](constructor)` + `workers.ContinuousProvided[T]("name")` or `workers.ScheduledProvided[T]("name", expr)`; `T` is built in the start phase |
| continuous `Run` returns nil → the worker stops | restarted like a failure; see above |
| `WithMaxRestarts(N)`, `N > 0` → failed after N failures (N−1 restarts) | `Restart.Limit` N: the first run plus N restarts → failed after N+1 failures |
| `WithMaxRestarts(0)` → unlimited restarts | `Restart.Limit` zero: unchanged |
| `info.Kind == "scheduled"` | still compiles; prefer `worker.KindScheduled` |
| `info.Attempts` | `info.Restarts` (continuous) / `info.ConsecutiveFailures` (scheduled) |
| `worker.Attempt(ctx)` | removed; use `worker.CurrentRun(ctx)` for the run's identity, and the snapshot for counters |
| `LastSuccess` set when a continuous `Run` returned nil | `LastSucceededAt` is never set for continuous workers |
| a scheduled `Run` returning nil during shutdown stamps `LastSuccess` and resets `ConsecutiveFailures` | when shutdown cancellation came first, a graceful stop: `LastSucceededAt` and `ConsecutiveFailures` unchanged, status `stopped` |
| a graceful stop clears `LastError` | a graceful stop changes only the status; `LastError` keeps the most recent failure until a successful scheduled run clears it |
| a hand-rolled `context.WithTimeout` inside `Run` | `ScheduledConfig.RunTimeout`; a timed-out run is a failure even if it returns nil |
| `@every 0s`, a negative `@every` or `@every 1500ms` silently became one second | rejected: registration panics, and `worker.ParseSchedule` returns the error |
| `LastError` may contain a panic stack trace | never; the stack is the `stack` attribute of the failure log line |
| log `worker stopped during scheduled run`; continuous `worker stopped` only on some exit paths | exactly one `worker started` and one `worker stopped` (`reason=shutdown` or `reason=failed`) per worker |
| log `worker tick skipped`, one line per skipped activation | one `worker activations skipped` line per resumption with `skipped=N` |
| `restart` attribute of the continuous `worker run failed` line, counting failed runs (`1` on the first failure) | renamed `restarts`; equals `Info.Restarts` — restarts that actually started, so `0` on the first failure |
| names silently trimmed | surrounding whitespace and control characters are rejected |
| untagged (Go field name) JSON from the pool's snapshot | snake_case field names from `workers.Snapshot()`; empty `schedule`, `continuous`/`scheduled`, `last_started_at`, `last_succeeded_at` and `last_error` omitted |
| `worker.Definition` (exported, returned by no API) | removed; `info.Continuous` or `info.Scheduled` is the public view of a registration |
| a continuous worker reported `running` as soon as the pool started | `pending` until its first run is admitted |

New and unchanged behavior worth knowing while migrating: `RunTimeout` is scheduled-only; failure log lines carry `run_id` (equal to `worker.CurrentRun(ctx).ID`) and `duration`; a successful scheduled run logs `worker run completed` at Debug; the snapshot reports the resolved configuration, so a registration test can assert every worker's policy without running it.

v0.21.0 changes the wait between continuous restarts without a compile error. Both rows apply to workers registered without a restart policy too:

| Before v0.21.0 | Now |
| --- | --- |
| every restart waits the fixed restart delay (3 s by default) | the first restart waits `Restart.MinDelay`; repeated failures back off with jitter up to `Restart.MaxDelay` (1 min by default), and a run that lasted at least the cap resets the sequence. The same value in `MinDelay` and `MaxDelay` keeps a fixed delay |
| `WithMaxRestarts(N)` reaches `failed` after N fixed waits (15 s for N = 5 with the defaults) | the waits back off, so `failed` — and an `UnreadyWhenFailed` readiness drop — comes later: roughly 48–93 s for N = 5 with the defaults |

New with it: the restart cap (`Restart.MaxDelay`, `restart.max_delay` in JSON, default `DefaultMaxRestartDelay`) and the `next_restart_in` attribute of `worker run failed`. A `MinDelay` above one minute stays fixed unless a larger cap is set.

## Data access

**Implemented (v0.22.0).** `store/sqldb` requires Bun v1.3.0 (`github.com/uptrace/bun` and its three dialect modules) and pgx v5.11.0. Credo's curated API keeps its signatures: `SelectQuery.Limit`, `Offset` and `Page` take `int`, and `Count` returns `int`. What changes is visible where an application touches Bun directly — inside an `Apply` closure, through `Unwrap`, `Conn`, `RequireTx` or `Client()` — and in what the database accepts.

| Before v0.22.0 | Now |
| --- | --- |
| `bun.SelectQuery.Limit`/`Offset` take `int`; `Count` returns `int` | `int64`. An untyped constant (`q.Limit(10)`) compiles unchanged; an `int` variable needs `int64(n)`, and a `Count` result assigned to an `int` needs a conversion |
| curated `Limit`/`Offset` reject values outside the signed 32-bit range with `ErrInvalidLimitOffset`; `Page` rejects such windows with `ErrInvalidPageRequest` | every value reaches the database; `Page` executes any window whose offset fits in `int`. `ErrInvalidLimitOffset` is removed, so an `errors.Is` check against it stops compiling and can be deleted |
| a string argument carrying a NUL byte (`0x00`) is stored with the byte removed | on SQLite and PostgreSQL the statement fails with an error that `store.KindOf` leaves unmapped, and nothing is persisted; MySQL still strips the byte ([data-access guide](data-access.md#nul-bytes-in-strings)). Reject NUL at the validation boundary |
| two migration files with the same numeric prefix and different names: the later file silently replaced the earlier one's `Up`/`Down` | `migrate.Migrations.Discover` returns `migrate: duplicate migration ID …`, so `RegisterMigrations` is never reached |
| a top-level `WhereOr` on a soft-delete model (or next to `WherePK`) rendered `WHERE a OR b AND deleted_at IS NULL` | Bun parenthesizes the OR group: `WHERE (a OR b) AND deleted_at IS NULL`. Queries that relied on the old precedence change their result set |
| a `.tx.up.sql` migration whose COMMIT failed was marked applied | the COMMIT or ROLLBACK error reaches `Migrate` and the migration stays unapplied ([data-access guide](data-access.md#migrations)) |
| a `jsonb`, `bytea` or `numeric` column scanned into `map[string]any` on Go 1.27 with pgx deadlocked | scans normally (Bun v1.3.0 fix, pinned by a real-PostgreSQL subtest) |
| pgx returns a text-format `timestamptz` in a location that differs from the binary format's | both formats return `time.Local` (or the codec's `ScanLocation`); `date` rejects impossible dates such as `2024-02-30` instead of normalizing them |

Unchanged: `SelectQuery.Clone` still restores the execution state Bun's `Clone` used to omit (v1.3.0 copies it itself; the compatibility layer is removed in a later minor), zero and negative `Limit`/`Offset` values still omit the clause, and standalone `Having` or a direct compound root still returns `ErrUnsupportedCountQuery` from `Count` and `Page`.

## Examples and downstream impact

The [example migration map](../../examples/README.md) identifies runnable changes by release. SaaS finalizes before resolving TenantService (DI minor), enables RequestID/AccessLog explicitly and installs compression with `UseCompress` instead of global middleware (HTTP minor). Hello remains a minimal default-profile example.

DI evidence comes from a 2026-09-05 scan of the maintainer's downstream applications: no factory/Replace calls, pre-Run resolution, and a worker-pool existence probe. The same applications install renderers once at bootstrap and use no scoped Recover, so their HTTP-minor migration is a rename of the renderer setters plus explicit `UseRequestID`/`UseAccessLog` calls.
