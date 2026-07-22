# ADR-010: Middleware Architecture

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** ADR-007, ADR-008

## Context

Middleware is the primary extension mechanism for a web framework. It intercepts requests before/after handlers for cross-cutting concerns: logging, authentication, rate limiting, CORS, compression, etc.

Credo needs a middleware model that supports three scopes (global, group, route), integrates with route metadata, and interoperates with the Go stdlib ecosystem.

Pre-dispatch rewrite middleware and its interaction with handler-level re-dispatch are documented in ADR-018.

## Decision

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

**Global middleware runs even on 404/405.** This ensures logging, request ID, and CORS headers are always present.

**Group middleware membership is resolved at compile time from the group parent chain** — the same model `LookupMeta` uses for metadata. Middleware added to a group after a route or sub-group was registered still applies to it; registration order within a group affects execution order only, never membership.

### Compile-Time Chain Building

Middleware chains are precompiled at startup (during `compile()`):

1. Per-route: group middlewares + route middlewares + handler → single compiled `Handler`
2. Global: built-in MW + global middlewares + dispatch → single compiled `Handler`

The built-in tier wraps the global chain:

```
builtinRequestID → builtinAccessLog → builtinRecover → builtinErrorHandler → globalMW → dispatch
```

Each built-in has an opt-out: `WithoutRecover()`, `WithoutRequestID()`, `WithoutAccessLog()`. This supports "observable by default" — every request gets an ID and access log entry with zero configuration.

At runtime, `ServeHTTP` calls the precompiled global chain. Dispatch looks up the matched route and calls its precompiled chain. Zero allocation, no slice iteration on the hot path.

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
app.GlobalMiddleware(middleware.AccessLog())

// Custom config
app.GlobalMiddleware(middleware.AccessLog(middleware.AccessLogConfig{
	Logger:   slog.Default(),
	MinLevel: slog.LevelWarn,
	Skipper: func(ctx *credo.Context) bool {
		return ctx.Request().URL.Path == "/health"
	},
}))
```

### Access-Log Filtering

Access logging is on by default (philosophy #6, "observable by default"): the built-in tier logs every request through the framework default logger even when `WithLogger` is not called. It is also the primary configuration surface because it observes the final response outside recovery and centralized error rendering. `WithAccessLogLogger` selects a dedicated sink without giving up that authoritative boundary; the logger is not derived from `ctx.Logger()`, so arbitrary request-scoped enrichment is not inherited, while `request_id` is restored explicitly and exactly once.

Filtering is split by observation phase, and the built-in and configurable variants share the same ordering:

- **`WithAccessLogSkipper(func(*Context) bool)`** — a predicate consulted by the built-in logger before routing. Because it runs pre-dispatch, only request-level data is reliable (method, path, headers); `ctx.Route()` and the response status are not yet set. It suits blanket path/header skips (metrics scrape, static assets). The configurable middleware has the equivalent `AccessLogConfig.Skipper`.
- **`MetaAccessLog` route meta** — `route.SetMeta(credo.MetaAccessLog, false)` silences a single route, and the same call on a `Group` silences everything under it via `LookupMeta` inheritance. A route-level value overrides a group-level one (the route is read before its parents), so a noisy group can be silenced while one route inside it stays logged. Only a bool `false` silences; any non-bool value is ignored and the request is logged (fail-open). The built-in logger reads this in its defer (after the route is known); the configurable middleware reads it after `next`.
- **`WithAccessLogMinLevel(slog.Leveler)`** — compares the status-derived record level with a dynamic minimum. `nil` means Info; `slog.LevelVar` permits a concurrency-safe runtime change. The configurable field is `AccessLogConfig.MinLevel`.
- **`WithAccessLogResultFilter(func(*Context, AccessLogEntry) bool)`** — a positive post-response predicate (`true` emits) for status, duration, byte count, route name, request ID, and request/user metadata. The configurable field is `AccessLogConfig.ResultFilter`. The pooled Context is synchronous-only and callbacks must be concurrency-safe.

The exact order is `Skipper → handler → MetaAccessLog → status/level → MinLevel → AccessLogEntry snapshot → ResultFilter → emit`. `MinLevel` and `ResultFilter` intersect: the filter cannot restore a record rejected by the threshold. Status drives the actual log level (`1xx/2xx/3xx → Info`, `4xx → Warn`, `5xx+ → Error`); `MinLevel` controls admission, never rewrites that level. The default remains Info, so zero configuration preserves every status class. Successful records retain the traffic/latency denominator while request metrics remain deferred; high-volume applications opt into Warn or Error.

The attribute set and `"request completed"` emit core remain centralized in `internal/observe.EmitAccessLog`. `AccessLogEntry.RouteName` is filter metadata only and does not silently extend that schema. Request ID snapshotting is independent from emit-time duplicate prevention, so filters always see the ID even when the request-scoped logger already carries it.

The two producers have different observation boundaries. The built-in snapshot contains final status, bytes, and error-rendering duration when the inner pipeline completes; with recovery explicitly disabled, an escaping panic instead uses the 500 fallback and bytes written so far. `middleware.AccessLog` runs at its configured global/group/route position; on returned-error paths its status is the best pre-render classification, bytes are those written so far, and duration excludes later error rendering. It remains useful for route/group-specific second sinks and policies. Enabling it while the built-in remains active intentionally emits two records; Credo does not silently reject this valid audit pattern.

Health probes use `MetaAccessLog` internally: `UseHealth` registers `/health` and `/ready` with the meta set to `HealthConfig.LogRequests` (default `false`), so probe traffic is silent unless re-enabled. See [ADR-016](016-health-checks.md).

#### Alternatives considered

A **default-off** access logger was considered and rejected. The framework's nearest philosophical peer, GoFr (all-in-one), logs requests by default; the frameworks that default off — Goyave, Hertz, Echo, Chi — are all composable toolkits, the model Credo positions against (philosophy #1). Keeping the log on but easy to scope preserves "observable by default" without the volume cost.

A **status-code skip list** was rejected. Across the ecosystem request-level skipping is a predicate — Hertz `WithLogConditionFunc(func(ctx, c) bool)`, Echo `Skipper`, Gin `Skip`/`SkipPaths`. Credo keeps the package-wide `Skipper` convention and adds the more general post-response `MinLevel` + `ResultFilter` composition instead of a fixed list.

Changing successful requests to **Debug by default** or enabling a built-in fixed **1/N sampler** was rejected. Both silently weaken the zero-config traffic record and conflate the status-derived severity contract with admission. Applications can set `MinLevel` explicitly and implement deterministic request-ID sampling (or concurrency-safe counter sampling) in `ResultFilter`; general metrics and telemetry sampling remain Phase 3.5 work.

### Stdlib Adapter

`WrapStdMiddleware` converts stdlib middleware for use with Credo:

```go
app.GlobalMiddleware(credo.WrapStdMiddleware(corsMiddleware))
```

The adapter handles request/response writer updates that stdlib middleware may apply (e.g., wrapping the writer, modifying the request).

`WrapStdMiddleware` is kept as a deliberate ecosystem escape hatch — the large `func(http.Handler) http.Handler` corpus (OTel instrumentation, vendor CORS/gzip) works without being rewritten, consistent with the "integrated first, override-friendly boundaries" philosophy and the other escape hatches (`ServeContext`, `WithRawConfig`, `JWTAdvanced`). It is second-class by design, and that is documented rather than hidden: adapted middleware sees only `*http.Request` and `r.Context()`, never `*credo.Context`, so it cannot read route Meta, the typed principal (`ctx.GetUser[T]`), the renderer, or the error pipeline. A short-circuit that writes directly to the `ResponseWriter` therefore bypasses RFC 7807 formatting; only responses produced by calling `next` flow back through Credo's error handling. There is no selective leak (no public `context.Context`-based principal accessor exists — see [ADR-012](012-authentication-and-authorization.md)). The first-class path for anything needing the principal, route meta, or the error pipeline is a native `func(Handler) Handler`.

### Built-in Middleware (Auto-Enabled)

| Built-in           | Purpose                          | Opt-out              |
| ------------------ | -------------------------------- | -------------------- |
| `builtinRecover`   | Outermost panic recovery         | `WithoutRecover()`   |
| `builtinRequestID` | X-Request-Id + logger enrichment | `WithoutRequestID()` |
| `builtinAccessLog` | Structured access logging        | `WithoutAccessLog()` |

### Configurable Middleware (middleware package)

| Middleware  | Purpose                                           |
| ----------- | ------------------------------------------------- |
| `Recover`   | Per-group/route panic recovery with custom config |
| `AccessLog` | Route/group-scoped request logging with Skipper, MinLevel, ResultFilter, `MetaAccessLog`, and custom logger |
| `RequestID` | X-Request-Id with custom header/generator/limit   |

### Frozen Guard

Middleware registration panics after compile:

```go
app.Run()
app.GlobalMiddleware(m) // panic: called after app was compiled
```

### Configuration-Driven Activation (Rejected)

Activating or parameterizing middleware from configuration files — framework-read `middleware.*` keys (`middleware.cors.enabled`, `middleware.timeout.duration`) behind a `UseConfiguredMiddleware()` bootstrapper — was considered and rejected. Discoverability: `credo.With*` options and the `middleware` package surface in IDE autocomplete, while stringly-typed config keys do not, and a typo silently disables the middleware it names. Doctrine: Credo's configuration architecture (ADR-005) is built on typed snapshots — string keys never appear in business code — and the framework reading `middleware.*` keys itself would violate the rule it sets. Explicitness (philosophy #3): the middleware stack is part of the application's composition and belongs visibly in code. Environment-dependent parameters remain fully supported the doctrinal way — the application unmarshals its own typed config and passes it to the middleware constructor (`middleware.CORS(middleware.CORSConfig{AllowOrigins: cfg.CORS.Origins})`). This rejection concerns file-driven activation only; the built-in tier stays default-on with explicit opt-outs as described above.

## Consequences

**Positive:**

- Single type — no confusion about which middleware type to use
- Precompiled chains — zero allocation on hot path
- Meta-driven — declarative, no per-route middleware wiring
- Config struct pattern — readable, discoverable options via `Middleware(cfg ...Config)`
- Stdlib interop via WrapStdMiddleware

**Negative:**

- Global MW on 404/405 runs full chain even for unmatched routes
- Meta values are `any` — no compile-time type safety
- Precompilation means no dynamic middleware addition at runtime
