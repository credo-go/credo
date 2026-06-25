# Router Spec

**Status**: Approved **Package**: Root (`github.com/credo-go/credo`), `internal/radix/` **Sources**: Chi (MIT, primary), Goyave (MIT), httprouter (BSD-3, reference) **Depends on**: — **ADRs**: [007-router-and-routing](../adr/007-router-and-routing.md), [018-host-routing-and-rewrite](../adr/018-host-routing-and-rewrite.md)

---

## Overview

Credo's router combines Chi's radix tree and stdlib-compatible `http.Handler` design with Goyave's route metadata, named routes, status handlers, and fluent API. Host-based routing extends the path router with a host selector that chooses between the default mux and host-scoped muxes before the radix lookup runs.

---

## API Surface

### Route Registration (fluent — returns `*Route`)

```go
router.GET(pattern, handler)     *Route
router.POST(pattern, handler)    *Route
router.PUT(pattern, handler)     *Route
router.DELETE(pattern, handler)  *Route
router.PATCH(pattern, handler)   *Route
router.HEAD(pattern, handler)    *Route
router.OPTIONS(pattern, handler) *Route
```

All registration methods return `*Route` for chaining.

### Route Fluent Methods

```go
route.Name(name string) *Route           // Named route for URL generation
route.SetMeta(key string, val any) *Route // Attach metadata
route.Middleware(m ...Middleware) *Route   // Per-route middleware
```

### Route Meta System (Goyave-inspired)

Key/value metadata attached to routes and routers. `LookupMeta` searches the parent chain recursively until a value is found.

```go
// Router-level — inherited by all child routes
router.SetMeta("auth", true)

// Route-level — overrides parent
router.GET("/public", handler).SetMeta("auth", false)

// Middleware reads meta declaratively
func authMiddleware(next credo.Handler) credo.Handler {
    return func(ctx *credo.Context) error {
        if val, ok := ctx.Route().LookupMeta("auth"); ok && val.(bool) {
            // authenticate
        }
        return next(ctx)
    }
}
```

**API:**

```go
// On App / Group — set at registration time
app.SetMeta(key string, val any)
app.RemoveMeta(key string)
group.SetMeta(key string, val any)
group.RemoveMeta(key string)

// On Route — set at registration time, read at request time
route.SetMeta(key string, val any) *Route
route.LookupMeta(key string) (any, bool) // traverses route → group → parent chain
```

### Named Routes + URL Generation (Goyave-inspired)

```go
router.GET("/products/{id}", handler).Name("product.show")

// URL generation
route := router.GetRoute("product.show")
uri, err := route.BuildURI("42") // → "/products/42"
url, err := route.BuildURL("42") // → "/products/42" (default route, same as BuildURI)
route.GetHost() // → "" for default routes, host pattern for host-scoped routes

// Host-scoped: host params consumed first, then path params
// Host("{tenant}.myapp.com").GET("/products/{id}", handler).Name("product.show")
url, err = route.BuildURL("acme", "42") // → "acme.myapp.com/products/42"
```

Names are unique per router tree. Duplicate names panic at startup. `BuildURL` auto-fills the host from the route's host pattern. Host parameters are consumed first, then path parameters. For default (non-host-scoped) routes, `BuildURL` is equivalent to `BuildURI`. Both methods return an error when parameters are missing, extra parameters are provided, or the stored pattern is malformed. Wildcard host patterns such as `*.example.com` cannot generate concrete URLs; use `{tenant}.example.com` when URL generation needs a subdomain value.

### StatusHandler System (Goyave-inspired)

App-level customizable handlers for HTTP error status codes.

```go
// Custom 404 handler
app.StatusHandler(http.StatusNotFound, func(ctx *credo.Context) error {
    return ctx.Response().JSON(404, map[string]string{"error": "not found"})
})
```

Default status handlers are registered for common codes (404, 405, 500). StatusHandler is set on the `App` only; group-level overrides are not supported.

### UseI18n (i18n integration)

```go
app.UseI18n(credo.I18nConfig{Dir: "locales/"})  // frozen-guarded, like SetMeta/StatusHandler
```

Initializes i18n: loads locale files, stores the bundle on App, and adds a global middleware for locale detection. See [ADR-013](../adr/013-internationalization.md).

### 3-Tier Middleware

| Tier | Scope | Registration | Runs on 404/405? |
| --- | --- | --- | --- |
| **Global** | Every request | `app.GlobalMiddleware(m...)` | Yes |
| **Group** | Routes under this group | `group.Middleware(m...)` | No |
| **Route** | Single route only | `route.Middleware(m...)` | No |

Execution order: Global → Group (outer to inner) → Route → Handler.

### Route Groups and Sub-routers

```go
// Group — shared prefix + middleware
api := router.Group("/api")
api.Middleware(authMiddleware)
api.GET("/users", listUsers)

// Nested groups
v1 := api.Group("/v1")
v1.GET("/products", listProductsV1)
```

### Host-Based Route Groups

```go
host := app.Host("api.example.com")
tenant := app.Host("{tenant}.example.com")
org := app.Host("{org:[a-z][a-z0-9-]+}.platform.io")
wildcard := app.Host("*.acme.io")
```

`app.Host(pattern)` returns a `*Group` backed by a dedicated mux. Routes registered on that group only match when the request `Host` header matches the host pattern.

**Host pattern syntax:**

| Syntax   | Example                    | Description                          |
| -------- | -------------------------- | ------------------------------------ |
| Exact    | `api.example.com`          | Static host match                    |
| Param    | `{tenant}.example.com`     | Named host parameter                 |
| Regex    | `{org:[a-z]+}.platform.io` | Regex-constrained host parameter     |
| Wildcard | `*.acme.io`                | Anonymous single-label host wildcard |

**Semantics:**

- Host matching runs before path lookup. A matched host selects its dedicated mux; otherwise the default mux handles the request.
- Host params are exposed alongside path params: `ctx.Request().RouteParam(name)` for single values, `ctx.Request().RouteParams()` for the full map.
- Host and path params share one namespace. Registering a route whose path params collide with host param names panics at registration time.
- Host patterns are normalized to lowercase and may not include a port. Incoming request hosts are normalized by lowercasing, stripping any port, and trimming a trailing dot.
- Matching is case-insensitive.
- Wildcard `*` is matching-only, captures no route param, and may only appear once as the leftmost complete label. `*.acme.io` matches `api.acme.io`, but not `acme.io` or `a.b.acme.io`.
- `*` and `*.io` are valid but broad patterns. `api*.acme.io`, `foo.*.io`, `*.*.acme.io`, and mixed wildcard/param patterns such as `*.{tenant}.acme.io` and `{tenant}.*.acme.io` are rejected at registration time.
- Host patterns with identical match semantics panic at registration time. `{a}.acme.io`, `{b}.acme.io`, and `*.acme.io` are equivalent; choose one. Regex-constrained patterns with different semantics remain valid.
- Exact static hosts use a hash-map fast path. Param, regex, and wildcard host patterns use the specificity-ordered scan below.

**Priority:**

When multiple host patterns could match, the most specific one wins:

1. Static label
2. Regex-constrained label
3. Param or wildcard label

Comparison is evaluated right-to-left by host label (`com` → `example` → `api`). Identical match semantics are rejected at registration time; remaining equal-specificity ties retain registration order.

### Sub-router Mounting

```go
// Mount any http.Handler as a sub-router under a prefix
adminMux := http.NewServeMux()
adminMux.HandleFunc("/dashboard", dashboard)
app.Mount("/admin", adminMux)
```

**Middleware scope:** mounted handlers receive only built-in and global middleware. Group and route middleware do not apply because mounted handlers are plain `http.Handler` instances dispatched outside the per-route compiled chain. If the mounted sub-application requires authentication or other protections, it must enforce them internally or the protections must be registered as global middleware.

**Method scope:** the mounted handler is registered for all standard HTTP methods except CONNECT and TRACE, which are excluded deliberately (CONNECT is a proxy mechanism; TRACE enables cross-site tracing). Requests using them receive 405.

### HEAD Auto-handling

GET routes automatically respond to HEAD requests (body discarded). Explicit HEAD registration overrides the auto-generated one.

### Trailing Slash Redirect

When a request path does not match any route, the router probes the path with the trailing slash toggled (`/users/` ↔ `/users`). If the alternate matches, the router issues a redirect:

- **GET / HEAD** → `301 Moved Permanently`
- **Other methods** → `308 Permanent Redirect` (preserves method)

Query strings are preserved. The root path `/` is never redirected. 405 takes precedence over redirect.

Enabled by default. Disable via option or config:

```go
credo.New(credo.WithRedirectTrailingSlash(false))
```

```json
{"server": {"redirect_trailing_slash": false}}
```

### URL Parameters

| Syntax         | Example              | Description                 |
| -------------- | -------------------- | --------------------------- |
| `{name}`       | `/users/{id}`        | Named parameter             |
| `{name:regex}` | `/users/{id:[0-9]+}` | Regex-constrained parameter |
| `{name...}`    | `/files/{path...}`   | Catch-all (rest of path)    |

The same `{name}` / `{name:regex}` syntax is reused for host labels in `app.Host(...)`.

Dynamic segment names are part of the radix tree shape. Routes that share the same static parent and dynamic segment position must use the same parameter name, even when one route continues with additional path segments.

```go
// Valid: same dynamic segment position, same param name.
app.GET("/v1/crm/customers/{id}", showCustomer)
app.GET("/v1/crm/customers/{id}/timeline", customerTimeline)

// Invalid: same dynamic segment position, different param names.
app.GET("/v1/crm/customers/{id}", showCustomer)
app.GET("/v1/crm/customers/{customer_id}/timeline", customerTimeline) // panics
```

Use the route-level name consistently and map it to domain-specific variable names in handlers when needed. The same rule applies to regex-constrained and catch-all dynamic segments.

### Router Interface

```go
// App implements http.Handler
app.ServeHTTP(w, r)

// Named route lookup
app.GetRoute(name string) *Route

// Route introspection (free functions)
credo.Walk(app.Mux(), func(method, pattern string) error {
    fmt.Println(method, pattern)
    return nil
})

credo.WalkRoutes(app.Mux(), func(ri credo.RouteInfo) error {
    fmt.Println(ri.Kind, ri.Method, ri.Host, ri.Pattern, ri.Name, ri.Meta)
    return nil
})
```

`app.Mux()` returns a route registry view across the default mux and all host-scoped muxes. `Walk` keeps the simple `(method, pattern)` callback and visits real routes only; `WalkRoutes` (like `app.Routes()`) exposes the full `RouteInfo`: `Method` for a normal route, or — for a mount — an empty `Method` and the sorted forwarded method set (every standard method except CONNECT/TRACE) in `Methods`; the route `Name`; the resolved `Meta` (route ← group ← app) as a fresh shallow map (nil if none, values read-only by convention); `Kind` (`RouteKindRoute` or `RouteKindMount`); and `AutoHead` (true for an auto-generated HEAD twin, false for an explicit HEAD). Mounts appear as a single `RouteKindMount` entry with the cleaned prefix (`/admin/` and `/admin` both normalize to `/admin`, `/` stays `/`) — the internal catch-all and method fan-out are hidden, and `Walk` skips mounts entirely. `Routes()` output is a deterministic total order `(Host, Pattern, Method, Kind)`; introspection reads live route state, so call it after wiring is complete, not concurrently with route registration.

---

## Design Decisions

1. **Chi radix tree as primary source** — Chi already supports `{param}` syntax, regex constraints, method bitflags, and sub-router mounting. Adapting from Chi avoids extensive refactoring that httprouter would require. See [ADR-007](../adr/007-router-and-routing.md).

2. **Goyave features adopted** — Meta system, named routes, StatusHandler, fluent Route API, 3-tier middleware, HEAD auto-handling provide significant value without conflicting with Chi's architecture. See [ADR-007](../adr/007-router-and-routing.md).

3. **Host routing uses a selector over per-host muxes** — Credo keeps the path radix tree unchanged and selects a host-specific mux before path lookup. This avoids baking host logic into the radix tree while preserving route isolation between domains. See [ADR-018](../adr/018-host-routing-and-rewrite.md).

4. **`*Route` return type** — HTTP registration methods return `*Route` instead of `void`. This enables fluent chaining without breaking existing patterns.

5. **No `ValidateBody`/`ValidateQuery` on Route** — Validation is handled by the "Parse, don't validate" pattern in Context (`BindBody`, `BindQuery`). An optional `.Validate()` convenience may be added in Phase 2. See [validation spec](./validation.md).

---

## File Layout

```
internal/radix/
├── method.go       HTTP method bitflags, MethodMap
├── context.go      RouteContext, RouteParams
├── pattern.go      PatNextSegment — {param}, {id:[0-9]+}, {path...}
├── sort.go         Node sorting
├── tree.go         Node, InsertRoute, FindRoute
├── pattern_test.go
└── tree_test.go

(root package)
├── credo.go         App struct, New(), HTTP shortcuts, Groups, Meta
├── server.go       ServeHTTP, Run, RunContext, ServeContext, Shutdown
├── host.go         Host pattern parsing, normalization, matching, specificity sort
├── dispatch.go     compile, dispatch, addRoute, Mount
├── mux.go          Radix tree storage (insert, Routes)
├── routectx.go     URLParam(), RouteContext(), context key
├── walk.go         Walk() and WalkRoutes() route introspection
├── route.go        Route struct, Meta, BuildURI/URL, host introspection
└── group.go        Group struct, Middleware, SetMeta, sub-groups
```

---

## Examples

### Basic

```go
app, err := credo.New()
if err != nil {
    log.Fatal(err)
}

app.GET("/", func(ctx *credo.Context) error {
    return ctx.Response().JSON(200, map[string]string{"message": "Hello, Credo!"})
})

if err := app.Run(); err != nil {
    log.Fatal(err)
}
```

### Named Routes + Meta

```go
app, err := credo.New()
if err != nil {
    panic(err)
}

app.GET("/products/{id:[0-9]+}", showProduct).
    Name("product.show").
    SetMeta("cache", 300)

api := app.Group("/api")
api.SetMeta("auth", true)
api.GET("/users", listUsers)
api.GET("/health", healthCheck).SetMeta("auth", false)
```

### Host Routing

```go
app, err := credo.New()
if err != nil {
    panic(err)
}

app.GET("/", landingPage)

api := app.Host("api.example.com")
api.GET("/users", listUsers)

tenant := app.Host("{tenant}.example.com")
tenant.GET("/dashboard", func(ctx *credo.Context) error {
    return ctx.Response().Text(200, ctx.Request().RouteParam("tenant"))
})
```

### Status Handlers

```go
app, err := credo.New()
if err != nil {
    panic(err)
}

// HTML site — 404 renders a page
app.StatusHandler(404, func(ctx *credo.Context) error {
    return ctx.Response().HTML(404, "<h1>Page Not Found</h1>")
})

// StatusHandler is app-level only; groups inherit the app's handlers.
// Use middleware with route meta for group-specific error responses.
```
