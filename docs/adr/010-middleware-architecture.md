# ADR-010: Middleware Architecture

**Status:** Accepted **Date:** 2026-03-01 **Last revised:** 2026-09-05 **Depends on:** ADR-007, ADR-008

**2026-09-05 amendment (HTTP minor):** panic recovery, request IDs, access logging, response compression, request decompression and locale detection left the middleware tiers and became framework-owned HTTP features with one registration path each. The built-in middleware tier, the `middleware.Recover`/`RequestID`/`AccessLog`/`Compress`/`Decompress` constructors, the `WithoutRequestID`/`WithoutAccessLog`/`WithAccessLog*` options and the renderer setters were removed in the same change. The [HTTP features spec](../specs/http-features.md) is the feature contract; this ADR records the decision and the middleware model around it.

## Context

Middleware is the primary extension mechanism for a web framework. It intercepts requests before/after handlers for cross-cutting concerns: logging, authentication, rate limiting, CORS, compression, etc.

Credo needs a middleware model that supports three scopes (global, group, route), integrates with route metadata, and interoperates with the Go stdlib ecosystem.

Pre-dispatch rewrite middleware and its interaction with handler-level re-dispatch are documented in ADR-018.

## Decision

### Built-in HTTP Feature Configuration Criterion

The default activation policy determines the public configuration path for selectable built-in HTTP features:

| Default | Sole public path | Application |
| --- | --- | --- |
| On | Constructor configuration plus a `Without*` opt-out | `WithRecoverConfig(cfg)` configures recovery; `WithoutRecover()` disables it |
| Off | A `Use*(config)` registration that activates and configures the feature | RequestID, AccessLog, Compress, Decompress and i18n; custom error/success renderers use their own `Use*` registrations |

Recovery's constructor config does not re-enable it: `WithoutRecover()` takes precedence regardless of option order. No separate positive recovery activation helper exists. Optional features have one successful registration, including a configured-but-inactive `UseI18n` result; they do not also expose constructor fields/options, configuration setters, or an `Enabled` switch for the same decision. Applications evaluate external enable flags in bootstrap code before calling `Use*`. Required feature config travels as one typed value rather than a family of per-field helpers.

The criterion concerns feature activation, not every zero-valued setting. Foundational App inputs such as logger, raw configuration, server/TLS and network timeouts remain constructor settings. Mandatory protocol/error handling is not made optional by this rule. Request-state and route-meta mutators keep their separate purpose.

Custom renderer installation is optional even though core error rendering always exists. A renderer may be bound from a DI-resolved object during bootstrap, so `UseErrorRenderer` and `UseSuccessRenderer` provide their sole registration path; there are no constructor duplicates and no setters. The core pipeline retains its default rendering when no extension is registered.

`Use*` registers a feature once during HTTP setup. Invocation order does not determine execution order, and registration ends at the shared HTTP preparation/shutdown gate ([bootstrap and DI lifecycle](../specs/bootstrap-and-di-lifecycle.md)); DI `Finalize` alone does not freeze HTTP setup. AccessLog activation does not govern framework/application diagnostic logs, which continue through their normal levels and logger filtering.

### Framework features around the user chain

Recovery, RequestID, AccessLog, i18n, Compress and Decompress are framework-owned HTTP processing around the three Global/Group/Route user middleware tiers. One request executor owns request initialization, the user chain, centralized error and panic handling, response finalization, optional access logging and Context release. It observes the final response after error rendering and compressor finalization, keeps framework diagnostics independent from access logging, and applies the installed features in one fixed plan whatever the `Use*` call order.

Why not middleware: these features need to see the final response (status after error rendering and recovery, bytes after compression), the original request (transport selection before rewrites and body transformation), or both. A middleware position gives neither, which is why the previous built-in tier had to infer the final status from a pre-render error and why the configurable `middleware.AccessLog` observed an earlier boundary than the built-in one. Moving them into the executor gives one authoritative observation and removes the duplicate implementations. The price is that these features cannot be scoped per group or route; an application that needs route-specific policy writes ordinary middleware (for example a group-level recover-and-return-error wrapper) or uses the selection knobs the features expose (`Skipper`, `MetaAccessLog`, `ResultFilter`).

The lazy `Detect(*Context)` locale contract memoizes one detection per request; Decompress precedes Global middleware with original-request selection; AccessLog counts post-compression body bytes and measures duration through finalization. Callback failures follow the recovery configuration, with a callback-free error-render fallback, post-response filter failure isolation and no second body after commitment. Genuine setup errors leave registration retryable; a successful inactive i18n configuration consumes its slot. Timeout, CORS, CSRF, Secure, RateLimit, Rewrite and ContractGuard remain user middleware because their route/group scope and their order relative to application policy are useful.

### Single Middleware Type

```go
type Middleware func(next Handler) Handler
```

One type for all three tiers. No separate types for global vs group vs route middleware. This simplifies the mental model and allows reuse.

### Three-Tier Execution

```
Request → Global MW → Group MW → Route MW → Handler
                                                ↓
Response ← Global MW ← Group MW ← Route MW ← Handler
```

| Tier   | Registration                 | Scope                           |
| ------ | ---------------------------- | ------------------------------- |
| Global | `app.GlobalMiddleware(m...)` | All requests, including 404/405 |
| Group  | `group.Middleware(m...)`     | Routes under that group         |
| Route  | `route.Middleware(m...)`     | Single route only               |

**Global middleware runs even on 404/405.** This ensures CORS and security headers are always present. Framework features cover 404/405 as well.

**Group middleware membership is resolved at compile time from the group parent chain** — the same model `LookupMeta` uses for metadata. Middleware added to a group after a route or sub-group was registered still applies to it; registration order within a group affects execution order only, never membership.

### Compile-Time Chain Building

Middleware chains are precompiled at startup (during `compile()`):

1. Per-route: group middlewares + route middlewares + handler → single compiled `Handler`
2. Global: global middlewares + dispatch → single compiled `Handler`

At runtime, `ServeHTTP` applies the lifecycle admission gate and hands the request to the executor, which runs the installed framework features around the precompiled global chain. Dispatch looks up the matched route and calls its precompiled chain. Zero allocation, no slice iteration on the hot path.

### Meta-Driven Behavior

Middleware reads route metadata declaratively instead of being configured per-route:

```go
// Registration: declare intent
app.GET("/admin", adminHandler).SetMeta("auth", true).SetMeta("permission", "admin")

// Middleware: reads meta, acts accordingly
func AuthMiddleware(next credo.Handler) credo.Handler {
    return func(ctx *credo.Context) error {
        if auth, _ := ctx.Route().LookupMeta("auth").(bool); !auth {
            return next(ctx) // skip auth
        }
        // validate token...
        return next(ctx)
    }
}
```

`LookupMeta` walks the parent chain (route → group → app) for inherited values.

### Config Struct Pattern

Middleware with options uses an optional config parameter:

```go
// Zero-config (sensible defaults)
app.GlobalMiddleware(middleware.CORS())

// Custom config
app.GlobalMiddleware(middleware.CORS(middleware.CORSConfig{
	AllowOrigins: []string{"https://example.com"},
	AllowMethods: []string{"GET", "POST"},
}))
```

Framework features follow the same shape at registration: `app.UseAccessLog()` for defaults, `app.UseAccessLog(credo.AccessLogConfig{...})` for customization, at most one config.

### Access-Log Filtering

Access logging is explicit (`app.UseAccessLog(cfg...)`) and observes the final response outside recovery and centralized error rendering, so it is the one authoritative access record. `AccessLogConfig.Logger` selects a dedicated sink without giving up that boundary; the logger is not derived from `ctx.Logger()`, so arbitrary request-scoped enrichment is not inherited, while `request_id` is restored explicitly and exactly once.

Filtering is split by observation phase:

- **`AccessLogConfig.Skipper`** — a predicate consulted before routing. Because it runs pre-dispatch, only request-level data is reliable (method, path, headers); `ctx.Route()` and the response status are not yet set. It suits blanket path/header skips (metrics scrape, static assets).
- **`MetaAccessLog` route meta** — `route.SetMeta(credo.MetaAccessLog, false)` silences a single route, and the same call on a `Group` silences everything under it via `LookupMeta` inheritance. A route-level value overrides a group-level one (the route is read before its parents), so a noisy group can be silenced while one route inside it stays logged. Only a bool `false` silences; any non-bool value is ignored and the request is logged (fail-open). The meta is read at observation time, after the route is known.
- **`AccessLogConfig.MinLevel`** (`slog.Leveler`) — compares the status-derived record level with a dynamic minimum. `nil` means Info; `slog.LevelVar` permits a concurrency-safe runtime change; a typed-nil provider panics at registration.
- **`AccessLogConfig.ResultFilter`** (`func(*Context, AccessLogEntry) bool`) — a positive post-response predicate (`true` emits) for status, duration, byte count, route, request ID, and request/user metadata. The pooled Context is synchronous-only and callbacks must be concurrency-safe. A panic in the filter is a framework `Error` diagnostic and skips the record; the completed response is unaffected.

The exact order is `Skipper → chain → error rendering/recovery/finalization → MetaAccessLog → status/level → MinLevel → AccessLogEntry snapshot → ResultFilter → emit`. `MinLevel` and `ResultFilter` intersect: the filter cannot restore a record rejected by the threshold. Status drives the actual log level (`1xx/2xx/3xx → Info`, `4xx → Warn`, `5xx+ → Error`); `MinLevel` controls admission, never rewrites that level. The default is Info, so an installed access log preserves every status class and keeps the traffic/latency denominator while request metrics remain deferred; high-volume applications opt into Warn or Error.

The attribute set and `"request completed"` emit core remain centralized in `internal/observe.EmitAccessLog`. `AccessLogEntry.RouteName` is filter metadata only and does not silently extend that schema. Request ID snapshotting is independent from emit-time duplicate prevention, so filters always see the ID even when the request-scoped logger already carries it.

Health probes use `MetaAccessLog` internally: `UseHealth` registers `/health` and `/ready` with the meta set to `HealthConfig.LogRequests` (default `false`), so probe traffic is silent unless re-enabled. See [ADR-016](016-health-checks.md).

#### Alternatives considered

**Default-on access logging** was the original decision (2026-03) and was superseded on 2026-09-05. The argument for it was philosophy #6, "observable by default": the framework's nearest all-in-one peer, GoFr, logs requests by default, while the composable toolkits (Goyave, Hertz, Echo, Chi) default off. The configuration criterion above replaced it: the logging _infrastructure_ stays ready by default (structured `slog`, `Infra.Logger`, server-error bridging, recovery diagnostics), while request correlation and access records are two explicit `Use*` calls. This removed the `WithoutRequestID`/`WithoutAccessLog` opt-outs and the four `WithAccessLog*` field helpers, gave every feature exactly one public path, and keeps plain `New()` free of implicit scaffold behavior; an enterprise scaffold makes the two calls visible instead.

A **status-code skip list** was rejected. Across the ecosystem request-level skipping is a predicate — Hertz `WithLogConditionFunc(func(ctx, c) bool)`, Echo `Skipper`, Gin `Skip`/`SkipPaths`. Credo keeps the package-wide `Skipper` convention and adds the more general post-response `MinLevel` + `ResultFilter` composition instead of a fixed list.

Changing successful requests to **Debug by default** or enabling a built-in fixed **1/N sampler** was rejected. Both silently weaken the traffic record and conflate the status-derived severity contract with admission. Applications can set `MinLevel` explicitly and implement deterministic request-ID sampling (or concurrency-safe counter sampling) in `ResultFilter`; general metrics and telemetry sampling remain Phase 3.5 work.

A **second, route-scoped access logger** (the former `middleware.AccessLog`) was removed rather than kept as a compatibility wrapper. It observed returned-error paths before the centralized renderer, so its status, bytes and duration were approximations, and two producers meant two schemas to keep aligned. Route/group selection is now expressed with `MetaAccessLog` and `ResultFilter` on the single record; a deliberate second audit sink is an application middleware.

### Stdlib Adapter

`WrapStdMiddleware` converts stdlib middleware for use with Credo:

```go
app.GlobalMiddleware(credo.WrapStdMiddleware(corsMiddleware))
```

The adapter handles request/response writer updates that stdlib middleware may apply (e.g., wrapping the writer, modifying the request).

`WrapStdMiddleware` is kept as a deliberate ecosystem escape hatch — the large `func(http.Handler) http.Handler` corpus (OTel instrumentation, vendor CORS/gzip) works without being rewritten, consistent with the "integrated first, override-friendly boundaries" philosophy and the other escape hatches (`ServeContext`, `WithRawConfig`, `JWTAdvanced`). It is second-class by design, and that is documented rather than hidden: adapted middleware sees only `*http.Request` and `r.Context()`, never `*credo.Context`, so it cannot read route Meta, the typed principal (`ctx.GetUser[T]`), the renderer, or the error pipeline. A short-circuit that writes directly to the `ResponseWriter` therefore bypasses Credo's centralized envelope; only responses produced by calling `next` flow back through error handling. There is no selective leak (no public `context.Context`-based principal accessor exists — see [ADR-012](012-authentication-and-authorization.md)). The first-class path for anything needing the principal, route meta, or the error pipeline is a native `func(Handler) Handler`.

### Frozen Guard

Middleware and feature registration panics after the App prepares to serve or admits shutdown:

```go
app.Run()
app.GlobalMiddleware(m) // panic: credo: App.GlobalMiddleware called after app was compiled or shut down
app.UseAccessLog()      // panic: credo: App.UseAccessLog called after app was compiled or shut down
```

### Configuration-Driven Activation (Rejected)

Activating or parameterizing middleware from configuration files — framework-read `middleware.*` keys (`middleware.cors.enabled`, `middleware.timeout.duration`) behind a `UseConfiguredMiddleware()` bootstrapper — was considered and rejected. Discoverability: `credo.With*` options and the `middleware` package surface in IDE autocomplete, while stringly-typed config keys do not, and a typo silently disables the middleware it names. Doctrine: Credo's configuration architecture (ADR-005) is built on typed snapshots — string keys never appear in business code — and the framework reading `middleware.*` keys itself would violate the rule it sets. Explicitness (philosophy #3): the middleware stack is part of the application's composition and belongs visibly in code. Environment-dependent parameters remain fully supported the doctrinal way — the application unmarshals its own typed config and passes it to the middleware constructor (`middleware.CORS(middleware.CORSConfig{AllowOrigins: cfg.CORS.Origins})`). The same rule applies to framework features: YAML/JSON may supply application-owned parameters, but only a `Use*` call in code activates a feature.

## Consequences

**Positive:**

- Single type — no confusion about which middleware type to use
- Precompiled chains — zero allocation on hot path
- Meta-driven — declarative, no per-route middleware wiring
- Config struct pattern — readable, discoverable options via `Middleware(cfg ...Config)`
- Stdlib interop via WrapStdMiddleware
- One authoritative access record and one recovery layer, observed after error rendering and compression, with one public path per feature

**Negative:**

- Global MW on 404/405 runs full chain even for unmatched routes
- Meta values are `any` — no compile-time type safety
- Precompilation means no dynamic middleware addition at runtime
- Framework features cannot be scoped per group or route; route-specific policy is expressed through their selection knobs or ordinary middleware
