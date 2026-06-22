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
	Logger: slog.Default(),
	Skipper: func(ctx *credo.Context) bool {
		return ctx.Request().URL.Path == "/health"
	},
}))
```

### Access-Log Filtering

Access logging is on by default (philosophy #6, "observable by default"): the built-in tier logs every request through the framework default logger even when `WithLogger` is not called. Two opt-out mechanisms keep that default while taming log volume, and both the built-in logger and the configurable `middleware.AccessLog` honour them:

- **`WithAccessLogSkipper(func(*Context) bool)`** — a predicate consulted by the built-in logger before routing. Because it runs pre-dispatch, only request-level data is reliable (method, path, headers); `ctx.Route()` and the response status are not yet set. It suits blanket path/header skips (metrics scrape, static assets). The configurable middleware has the equivalent `AccessLogConfig.Skipper`.
- **`MetaAccessLog` route meta** — `route.SetMeta(credo.MetaAccessLog, false)` silences a single route, and the same call on a `Group` silences everything under it via `LookupMeta` inheritance. A route-level value overrides a group-level one (the route is read before its parents), so a noisy group can be silenced while one route inside it stays logged. Only a bool `false` silences; any non-bool value is ignored and the request is logged (fail-open). The built-in logger reads this in its defer (after the route is known); the configurable middleware reads it after `next`.

The attribute set, message, and status-derived level are produced once in `internal/observe.EmitAccessLog`, shared by both loggers; only the per-request primitive collection differs (the internal package cannot import the root `credo` package). Status drives the log _level_ (2xx/3xx → Info, 4xx → Warn, 5xx → Error), never whether a line is emitted.

Health probes use `MetaAccessLog` internally: `UseHealth` registers `/health` and `/ready` with the meta set to `HealthConfig.LogRequests` (default `false`), so probe traffic is silent unless re-enabled. See [ADR-016](016-health-checks.md).

#### Alternatives considered

A **default-off** access logger was considered and rejected. The framework's nearest philosophical peer, GoFr (all-in-one), logs requests by default; the frameworks that default off — Goyave, Hertz, Echo, Chi — are all composable toolkits, the model Credo positions against (philosophy #1). Keeping the log on but easy to scope preserves "observable by default" without the volume cost.

A **status-code skip list** was also rejected. Across the ecosystem the skip mechanism is a predicate — Hertz `WithLogConditionFunc(func(ctx, c) bool)`, Echo `Skipper`, Gin `Skip`/`SkipPaths` — while status codes drive _level_ mapping, not skipping. Credo follows the same split: a predicate (`Skipper`) and route meta for skipping, plus `Level()` for status. A success-level/sampling control (2xx → Debug, or 1/N sampling) remains an open question — it changes default semantics and is tracked separately.

### Stdlib Adapter

`WrapStdMiddleware` converts stdlib middleware for use with Credo:

```go
app.GlobalMiddleware(credo.WrapStdMiddleware(corsMiddleware))
```

The adapter handles request/response writer updates that stdlib middleware may apply (e.g., wrapping the writer, modifying the request).

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
| `AccessLog` | Request logging with Skipper, `MetaAccessLog` silencing, and custom logger |
| `RequestID` | X-Request-Id with custom header/generator/limit   |

### Frozen Guard

Middleware registration panics after compile:

```go
app.Run()
app.GlobalMiddleware(m) // panic: called after app was compiled
```

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
