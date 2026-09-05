# ADR-007: Router & Routing

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** ADR-001

## Pre-v1 endpoint parameter amendment

**Accepted 2026-09-05; implementation pending (P4).** Store path parameter names on endpoints; collect captures positionally in the radix tree. Shared segments may have different names across endpoints. Remove Node.ParamKey/name-mismatch rejection, retaining strict duplicate method plus name-stripped shape detection and structural kind/regex conflicts. BuildURI uses route-pattern names; host-pattern matching is unchanged. This adapts Chi's endpoint-key model while keeping Credo's strict duplicate policy. The [router target contract](../specs/router.md#pre-v1-endpoint-parameter-contract) defines regression coverage. This is an independent behavioral minor.

P5's generation/decoding decision is accepted separately below; P6's compiled route model remains backlog. The remaining current-router sections are migrated when their respective minor lands.

## Pre-v1 URL round-trip amendment

**Accepted 2026-09-05; implementation pending (P5/G3).** Match raw segment boundaries, decode each captured value once, then evaluate regex constraints on the decoded value supplied to RouteParam. A captured `%2F` is data within its segment; `%252F` becomes literal `%2F`, not a slash. `%31` matches a numeric constraint as `1`; `+` stays plus and valid Unicode is preserved.

Generation accepts decoded values, validates regex constraints and uses PathEscape per segment; catch-all slash separators are retained and host labels are validated independently. Round trips preserve parameter values rather than identical percent-encoded spelling. Malformed percent encoding or invalid UTF-8 is 400; a valid value failing a regex is a route non-match. Generation rejects invalid inputs with an error. Decoding the entire path before routing is rejected.

The [router contract and table](../specs/router.md#pre-v1-url-round-trip-contract) define positive and negative cases. This closes G3; it still ships in its own wire-contract minor, independently from P4's parameter-name change.

## Context

An all-in-one framework (ADR-001) needs a fast, feature-rich router. Go's stdlib `http.ServeMux` (even with Go 1.22 enhancements) lacks regex constraints, named routes, route metadata, and URL generation.

Credo adapts (ADR-002) Chi's radix tree for fast path matching, and Goyave's routing features (route meta, named routes, status handlers, fluent API) for developer experience.

Host-based routing and rewrite behavior were added later and are documented in ADR-018.

## Decision

### Radix Tree

The router is built on a radix tree adapted from Chi, living in `internal/radix/`. The tree supports:

- **Static segments**: `/users/list`
- **Parameterized segments**: `{id}` — matches any single segment
- **Regex-constrained params**: `{id:[0-9]+}` — matches only digits
- **Catch-all (wildcard)**: `{path...}` — matches remainder of path

The tree uses HTTP method bitmasks for efficient method-based lookup.

### Router Lives in Root Package

The router is not a separate package — it lives in the root `credo` package. This eliminates import cycles between router and middleware, enables unified handler/middleware types, and simplifies the API:

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
app.QUERY(pattern, handler)   *Route
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

Routes carry arbitrary key-value metadata via `SetMeta(key, val)`. Middleware reads meta declaratively via `LookupMeta(key)`, which walks the parent chain (route → group → app) for inheritance:

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

`BuildURI` and `BuildURL` return errors for missing parameters, extra parameters, or malformed stored patterns. URL generation is intentionally strict; Credo does not leave placeholders in generated URLs or silently drop inputs. Wildcard host patterns such as `*.example.com` are matching-only and cannot generate concrete URLs; use named host params for generated subdomains.

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

Every `GET` registration automatically registers a `HEAD` handler that runs the same handler chain. An explicit `HEAD` registration overrides the auto-generated one.

### QUERY Method

Credo recognizes RFC 10008 QUERY as a standard safe, idempotent method and exposes the same registration shape on `App` and `Group`: `QUERY(pattern, handler) *Route`. The request content carries the query representation, so handlers normally use `Request.BindBody`; Credo never creates a GET or HEAD twin.

A matched QUERY route requires a non-blank `Content-Type` before the application handler. Missing or blank values produce `400 content_type_required`, including for an empty body. This method-level guard also covers mounted handlers and runs inside ordinary middleware so authentication and observability keep their established order. A present header leaves media support and content consistency to `BindBody` or an application parser.

Credo deliberately adds no QUERY-only content-type option, `Accept-Query` helper, reserved metadata key, automatic OPTIONS behavior, cache, or exported method constant. Applications may emit `Accept-Query` directly; a future automatic contract, if justified, must be generic across body-bearing methods. QUERY-aware caches must include request content and relevant metadata in their cache key.

### Mounting

Stdlib `http.Handler` can be mounted at a prefix:

```go
app.Mount("/debug", http.DefaultServeMux)
```

A mounted handler answers both its exact prefix (`/debug`) and every path beneath it (`/debug/...`), receiving the request with the prefix stripped. A root mount (`Mount("/", h)`) therefore forwards the entire path space, including the bare `/`. Registration is atomic: `Mount` preflights all of its method/pattern registrations and panics before mutating the tree if any explicit route already conflicts, so a conflicting mount leaves no orphan routes behind (the radix tree has no delete, so the guarantee is check-before-insert, not rollback).

### Route Introspection

`app.Routes()` returns the live route surface as `[]RouteInfo`; `Walk` (method + pattern) and `WalkRoutes` (full `RouteInfo`) iterate it. Because a route's `Name` and `SetMeta` are chained onto the `*Route` after the registration call returns, `RouteInfo` is derived live from the route store at call time rather than snapshotted at registration. Each `RouteInfo` carries `Method` for a normal route (a mount leaves it empty and lists its sorted forwarded method set — every standard method except CONNECT and TRACE — in `Methods`); the route `Name`; the fully resolved `Meta` (route ← group ← app) as a fresh, shallow, nil-if-none map whose values are read-only by convention; `Kind` (`RouteKindRoute` or `RouteKindMount`); and `AutoHead`, true only for the auto-generated HEAD twin of a GET route.

Mounts are opaque: a single `RouteKindMount` entry reports the cleaned prefix (`/admin/` and `/admin` both normalize to `/admin`; `/` stays `/`) and forwarded methods, never the internal catch-all pattern or method fan-out. `Walk` skips mounts because the method+pattern shape cannot represent them; `WalkRoutes` includes them. `Routes()` output is a deterministic total order `(Host, Pattern, Method, Kind)`, independent of registration order and host compile-sort state, so route/permission catalogs and golden-file tests are stable. Introspection reads live `*Route` fields without locking; it is a post-wiring (or post-freeze) operation and must not run concurrently with route registration or configuration.

**Alternatives considered:**

- _Snapshot meta at registration_: rejected — `Name`/`SetMeta` are chained after the registration call returns, so a registration-time snapshot would miss them; deriving `RouteInfo` live from the store is the only correct option.
- _A separate `LocalMeta` field (route-only, unresolved)_: deferred (YAGNI) — consumers need the resolved view (route ← group ← app); a local-only view can be added later if a use case appears.
- _Deep-cloning `Meta` values_: rejected — selectively cloning `[]string`/map values gives false confidence for custom types (`[]Permission`, `*T`) that cannot be cloned generically, so `Meta` is a shallow copy with a documented read-only-by-convention contract.
- _Registration-order output_: rejected — `compile()` sorts host entries in place, so registration order is not stable across the first request; a total-order sort guarantees deterministic output regardless of compile state.

### Compile & Freeze

On first `ServeHTTP` (or `Run`), the router compiles:

1. Precompiles per-route middleware chains (group MW → route MW → handler)
2. Builds global middleware chain (global MW → dispatch)
3. Freezes the app — late registration panics

### Trailing Slash Redirect

When a request results in 404 (not 405), the router probes the radix tree with the trailing slash toggled (`/users/` ↔ `/users`). If the alternate path matches a handler for the requested method, the router issues a redirect instead of returning 404:

- **GET / HEAD** → `301 Moved Permanently`
- **Other methods** → `308 Permanent Redirect` (preserves method)

The root path `/` is never redirected. Query strings are preserved. 405 (Method Not Allowed) takes precedence — no redirect when the original path matches a route but not the requested method.

Enabled by default. Disable via `WithRedirectTrailingSlash(false)` or `server.redirect_trailing_slash: false` in config.

**Alternatives considered:**

- _Silent fallback (rewrite)_: Hides canonical URL decisions from clients. Client wouldn't know the canonical URL, making debugging harder and creating SEO duplicate-content risk.
- _Middleware-based_: Would require either a blind path normalization (without knowing registered routes) or a double tree lookup. Dispatch-level implementation is more efficient and semantically correct.

## Consequences

**Positive:**

- Fast radix tree matching (adapted from battle-tested Chi)
- Route Meta enables declarative middleware configuration
- Named routes + URL generation prevent hardcoded paths
- Fluent API is readable and chainable
- HEAD auto-handling follows HTTP spec
- Live route introspection (`Routes`/`WalkRoutes`) exposes `Name`, resolved `Meta`, and mounts — enabling drift-free route/permission catalogs

**Negative:**

- Adapted radix tree requires maintenance when upstream Chi evolves
- Route Meta is `map[string]any` — no compile-time key/type safety
- Compile-once means no dynamic route addition at runtime
