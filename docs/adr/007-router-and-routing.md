# ADR-007: Router & Routing

**Status:** Accepted
**Date:** 2026-03-01
**Depends on:** ADR-001

## Context

An all-in-one framework (ADR-001) needs a fast, feature-rich router.
Go's stdlib `http.ServeMux` (even with Go 1.22 enhancements) lacks regex
constraints, named routes, route metadata, and URL generation.

Credo adapts (ADR-002) Chi's radix tree for fast path matching, and
Goyave's routing features (route meta, named routes, status handlers,
fluent API) for developer experience.

Host-based routing and rewrite behavior were added later and are documented in
ADR-018.

## Decision

### Radix Tree

The router is built on a radix tree adapted from Chi, living in
`internal/radix/`. The tree supports:

- **Static segments**: `/users/list`
- **Parameterized segments**: `{id}` — matches any single segment
- **Regex-constrained params**: `{id:[0-9]+}` — matches only digits
- **Catch-all (wildcard)**: `{path...}` — matches remainder of path

The tree uses HTTP method bitmasks for efficient method-based lookup.

### Router Lives in Root Package

The router is not a separate package — it lives in the root `credo`
package. This eliminates import cycles between router and middleware,
enables unified handler/middleware types, and simplifies the API:

```go
app, err := credo.New()
if err != nil {
    panic(err)
}
app.GET("/users/{id}", handler)
```

### Route Registration

HTTP method shortcuts on both `App` and `Group`:

```go
app.GET(pattern, handler)    *Route
app.POST(pattern, handler)   *Route
app.PUT(pattern, handler)    *Route
app.DELETE(pattern, handler)  *Route
app.PATCH(pattern, handler)   *Route
app.HEAD(pattern, handler)    *Route
app.OPTIONS(pattern, handler) *Route
```

Each returns a `*Route` for fluent chaining.

### Fluent Route API

```go
app.GET("/users/{id}", handler).
    Name("user.show").
    SetMeta("auth", true).
    SetMeta("permission", "users.read").
    Middleware(rateLimit)
```

### Route Meta

Routes carry arbitrary key-value metadata via `SetMeta(key, val)`.
Middleware reads meta declaratively via `LookupMeta(key)`, which walks
the parent chain (route → group → app) for inheritance:

```go
// Registration
app.SetMeta("auth", false)             // app default: no auth
api := app.Group("/api")
api.GET("/admin", handler).SetMeta("auth", true)  // override

// Middleware reads it
func AuthMiddleware(next credo.Handler) credo.Handler {
    return func(ctx *credo.Context) error {
        if auth, _ := ctx.Route().LookupMeta("auth").(bool); auth {
            // validate token
        }
        return next(ctx)
    }
}
```

### Named Routes & URL Generation

```go
app.GET("/users/{id}", handler).Name("user.show")

// Generate URI (path only)
uri, err := app.GetRoute("user.show").BuildURI("42")  // "/users/42"

// Generate full URL (auto-host for host-scoped routes)
url, err := app.GetRoute("user.show").BuildURL("42")  // "/users/42" (default route)

// Host-scoped: host params first, then path params
// Host("{tenant}.myapp.com").GET("/users/{id}", handler).Name("tenant.user")
url, err = app.GetRoute("tenant.user").BuildURL("acme", "42")  // "acme.myapp.com/users/42"
```

`BuildURI` and `BuildURL` return errors for missing parameters, extra
parameters, or malformed stored patterns. URL generation is intentionally
strict; Credo does not leave placeholders in generated URLs or silently drop
inputs. Wildcard host patterns such as `*.example.com` are matching-only and
cannot generate concrete URLs; use named host params for generated subdomains.

Duplicate route names panic at registration time.

### Route Groups

```go
api := app.Group("/api")
api.Middleware(authMW)

v1 := api.Group("/v1")
v1.GET("/users", listUsers)   // matches /api/v1/users
```

Groups inherit parent middleware and metadata.

### Status Handlers

Custom handlers for 404, 405, and other status codes:

```go
app.StatusHandler(404, func(ctx *credo.Context) error {
    return ctx.Response().JSON(404, map[string]string{"error": "not found"})
})
```

Status handlers are resolved from the root group.

### HEAD Auto-Handling

Every `GET` registration automatically registers a `HEAD` handler that
runs the same handler chain. An explicit `HEAD` registration overrides
the auto-generated one.

### Mounting

Stdlib `http.Handler` can be mounted at a prefix:

```go
app.Mount("/debug", http.DefaultServeMux)
```

### Compile & Freeze

On first `ServeHTTP` (or `Run`), the router compiles:
1. Precompiles per-route middleware chains (group MW → route MW → handler)
2. Builds global middleware chain (global MW → dispatch)
3. Freezes the app — late registration panics

### Trailing Slash Redirect

When a request results in 404 (not 405), the router probes the radix tree
with the trailing slash toggled (`/users/` ↔ `/users`). If the alternate
path matches a handler for the requested method, the router issues a
redirect instead of returning 404:

- **GET / HEAD** → `301 Moved Permanently`
- **Other methods** → `308 Permanent Redirect` (preserves method)

The root path `/` is never redirected. Query strings are preserved.
405 (Method Not Allowed) takes precedence — no redirect when the original
path matches a route but not the requested method.

Enabled by default. Disable via `WithRedirectTrailingSlash(false)` or
`server.redirect_trailing_slash: false` in config.

**Alternatives considered:**
- *Silent fallback (rewrite)*: Hides canonical URL decisions from clients.
  Client wouldn't know the canonical URL, making debugging harder and
  creating SEO duplicate-content risk.
- *Middleware-based*: Would require either a blind path normalization
  (without knowing registered routes) or a double tree lookup. Dispatch-level
  implementation is more efficient and semantically correct.

## Consequences

**Positive:**
- Fast radix tree matching (adapted from battle-tested Chi)
- Route Meta enables declarative middleware configuration
- Named routes + URL generation prevent hardcoded paths
- Fluent API is readable and chainable
- HEAD auto-handling follows HTTP spec

**Negative:**
- Adapted radix tree requires maintenance when upstream Chi evolves
- Route Meta is `map[string]any` — no compile-time key/type safety
- Compile-once means no dynamic route addition at runtime
