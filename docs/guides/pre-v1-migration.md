# Pre-v1 Migration Preview

**Status:** The bootstrap/DI changes (DI minor) and the router parameter-name change (router minor) are implemented as of 2026-09-05; the [Bootstrap and DI](#bootstrap-and-di) and [Router](#router) sections below describe shipped behavior. The built-in HTTP feature changes and the URL round-trip change are accepted but not yet implemented; the names in those sections are not yet callable. Follow the [implementation plan](../plans/pre-v1-implementation.md) for boundaries and the accepted G1–G4 decisions, and [TODO](../../TODO.md#pre-v1-contract-migration) for progress.

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

| Current surface/default | Migration in the HTTP minor |
| --- | --- |
| Recovery on; optional middleware.Recover configuration | `WithRecoverConfig(cfg)` configures default-on root recovery; `WithoutRecover()` wins |
| Default RequestID and WithoutRequestID | Explicit `app.UseRequestID(cfg...)`; omit it to disable |
| Default AccessLog, WithoutAccessLog and WithAccessLog field helpers | Explicit `app.UseAccessLog(cfg...)` with one config |
| middleware.RequestID / middleware.AccessLog | Root Use registration; access policy through metadata/config filters |
| middleware.Compress / middleware.Decompress | `app.UseCompress(cfg...)` / `app.UseDecompress(cfg...)` |
| SetErrorRenderer / SetSuccessRenderer | `app.UseErrorRenderer(renderer)` / `app.UseSuccessRenderer(renderer)`, one successful installation |
| UseI18n adding Global middleware | Keep UseI18n; switch custom Detect from *http.Request to *Context and resolve lazily on first use |

Optional features have no parallel constructor/setter/Enabled route. Evaluate external enable flags in application bootstrap. Use-call order does not choose execution order. `WithoutRecover` wins over recovery configuration regardless of option order. Foundational logger, raw config, server, TLS and timeouts stay constructor settings.

AccessLog off does not disable framework or application diagnostics. Logger filtering and access record selection remain separate; WithDebug does not set the slog minimum level. To preserve request correlation and access records, explicitly enable both features.

Scoped recovery is removed. Applications needing their own route policy can author ordinary middleware; Credo does not keep duplicate compatibility wrappers. CORS, CSRF, Secure, Timeout, RateLimit, Rewrite and ContractGuard retain their middleware APIs and ordering responsibilities.

Lazy locale is accepted: first Locale/translation access fixes the language and all translation paths share it. Custom Detect receives *Context and can access the request or GetUser. It must handle missing auth data; Timeout/stdlib wrappers may restore a request without downstream auth data. To retain the authenticated language in later errors, call Locale after setting the user, before unwinding; an earlier read still wins. Empty/unresolvable detection uses the configured default. Recursive detection is misuse; panic/re-entry stores the fallback and never retries the detector. Recovery enabled uses the 500 path; disabled recovery propagates panic after cleanup.

A successful UseI18n that finds no conventional catalogs now consumes registration as configured-but-inactive. Do not call it again as a fallback strategy. Explicit-source failures still return errors and permit correction before HTTP preparation.

Decompress runs before Global middleware. Its Skipper uses the original request's method/path/ headers once, so raw-body webhook paths must be selected there, without route/auth dependencies. Rewrite does not repeat selection. Global body readers and binders see the same decoded stream under separate wire/decoded limits.

AccessLog bytes become post-compression accepted body bytes; headers/framing/TLS are excluded. Duration includes finalization and excludes the access filter/log write. Preserve actual committed status when transfer fails. With recovery enabled, an error-rendering failure falls back without callbacks; a post-response ResultFilter panic logs a diagnostic and skips that record. No failure after commitment appends a second JSON body; incomplete compressed output is aborted as required. These failure paths follow WithoutRecover for panics and always release request state.

## Router

**Implemented (router minor, 2026-09-05).** Path parameter names belong to the endpoint: `/customers/{id}` and `/customers/{customer_id}/timeline` coexist, and each handler reads its own names. Nothing needs to change in existing applications — every registration that was valid stays valid with the same captures — and routes that were previously split or renamed to satisfy the shared-name rule may now use their natural names. The `conflicting … parameter` registration panic no longer exists; the same method on the same name-stripped shape is a duplicate (`GET "/users/{name}" is already registered as "/users/{id}"`), and structural regex conflicts remain errors. `BuildURI` reads the selected route's names; host pattern semantics stay unchanged. P5 escaping/decoding is accepted separately and is not delivered by the router minor. It keeps raw segment boundaries, decodes captures once and evaluates regex on the decoded value: %31 becomes numeric 1, %2F is captured slash data, %252F stays literal %2F, and plus stays plus. BuildURI takes decoded values and escapes them per segment. Malformed encoding/UTF-8 is 400; regex mismatch is no match; generation rejects invalid values. See the [round-trip table](../specs/router.md#pre-v1-url-round-trip-contract).

## Examples and downstream impact

The [example migration map](../../examples/README.md) identifies runnable changes by release. SaaS already finalizes before resolving TenantService (DI minor); in the HTTP minor it enables RequestID/AccessLog explicitly and moves Compress out of GlobalMiddleware. Hello remains a minimal default-profile example.

DI evidence comes from a 2026-09-05 scan of the maintainer's downstream applications: no factory/Replace calls, pre-Run resolution, and a worker-pool existence probe. The same applications install renderers once at bootstrap and use no scoped Recover. Internal framework integrations and recovery contract tests still require migration, regardless of downstream counts.
