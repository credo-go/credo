# ADR-010: Middleware Architecture

**Status:** Accepted (amended 2026-06-11)
**Date:** 2026-03-01
**Depends on:** ADR-007, ADR-008

> **Amendment (2026-06-11):** Group middleware is no longer snapshotted at
> route-registration time. `compile()` now collects it from the group parent
> chain — the same model `LookupMeta` uses for metadata — so middleware added
> to a group after a route (or sub-group) was registered still applies to it.
> Registration order within a group affects execution order only, never
> membership. The `routeGroup`/`Group` split, the `hasRoutes` flag, and the
> debug warning for "Middleware after routes" were removed along with the
> snapshot.

## Context

Middleware is the primary extension mechanism for a web framework.
It intercepts requests before/after handlers for cross-cutting concerns:
logging, authentication, rate limiting, CORS, compression, etc.

Credo needs a middleware model that supports three scopes (global, group,
route), integrates with route metadata, and interoperates with the Go
stdlib ecosystem.

Pre-dispatch rewrite middleware and its interaction with handler-level
re-dispatch are documented in ADR-018.

## Decision

### Single Middleware Type

```go
type Middleware func(next Handler) Handler
```

One type for all three tiers. No separate types for global vs group vs
route middleware. This simplifies the mental model and allows reuse.

### Three-Tier Execution

```
Request → Global MW → Group MW → Route MW → Handler
                                                ↓
Response ← Global MW ← Group MW ← Route MW ← Handler
```

| Tier | Registration | Scope |
|------|-------------|-------|
| Global | `app.GlobalMiddleware(m...)` | All requests, including 404/405 |
| Group | `group.Middleware(m...)` | Routes under that group |
| Route | `route.Middleware(m...)` | Single route only |

**Global middleware runs even on 404/405.** This ensures logging,
request ID, and CORS headers are always present.

### Compile-Time Chain Building

Middleware chains are precompiled at startup (during `compile()`):

1. Per-route: group middlewares + route middlewares + handler → single
   compiled `Handler`
2. Global: built-in MW + global middlewares + dispatch → single compiled
   `Handler`

The built-in tier wraps the global chain:

```
builtinRecover → builtinRequestID → builtinAccessLog → globalMW → dispatch
```

Each built-in has an opt-out: `WithoutRecover()`, `WithoutRequestID()`,
`WithoutAccessLog()`. This supports "observable by default" — every
request gets an ID and access log entry with zero configuration.

At runtime, `ServeHTTP` calls the precompiled global chain. Dispatch
looks up the matched route and calls its precompiled chain. Zero
allocation, no slice iteration on the hot path.

### Meta-Driven Behavior

Middleware reads route metadata declaratively instead of being
configured per-route:

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

`LookupMeta` walks the parent chain (route → group → app) for
inherited values.

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

### Stdlib Adapter

`WrapStdMiddleware` converts stdlib middleware for use with Credo:

```go
app.GlobalMiddleware(credo.WrapStdMiddleware(corsMiddleware))
```

The adapter handles request/response writer updates that stdlib
middleware may apply (e.g., wrapping the writer, modifying the request).

### Built-in Middleware (Auto-Enabled)

| Built-in | Purpose | Opt-out |
|----------|---------|---------|
| `builtinRecover` | Outermost panic recovery | `WithoutRecover()` |
| `builtinRequestID` | X-Request-Id + logger enrichment | `WithoutRequestID()` |
| `builtinAccessLog` | Structured access logging | `WithoutAccessLog()` |

### Configurable Middleware (middleware package)

| Middleware | Purpose |
|-----------|---------|
| `Recover` | Per-group/route panic recovery with custom config |
| `AccessLog` | Request logging with Skipper and custom logger |
| `RequestID` | X-Request-Id with custom header/generator/limit |

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
