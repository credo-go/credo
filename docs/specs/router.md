# Router Spec

**Status:** Implemented (beta) **Package:** root (`github.com/credo-go/credo`), `internal/radix/`, `internal/pattern/`, `internal/wirepath/`, `internal/host/` **ADR:** [ADR-007](../adr/007-router-and-routing.md), [ADR-018](../adr/018-host-routing-and-rewrite.md) **Guide:** [Routing guide](../guides/routing.md) **Sources:** Chi (MIT, primary), Goyave (MIT), httprouter (BSD-3, reference)

> **Contract.** Every section of this spec is normative — what a client sees on the wire or application code can observe — except [Current Behaviour](#current-behaviour), which describes the data structures behind that contract as they are now: not guaranteed, they may change in any release. [Design Decisions](#design-decisions) link the reasons. The examples run as `Example*` tests in [`example_router_test.go`](../../example_router_test.go), so a drift fails the build.

## Overview

Credo's router combines Chi's radix tree and stdlib-compatible `http.Handler` design with Goyave's route metadata, named routes, status handlers, and fluent API. Host-based routing extends the path router with a host selector that chooses between the default mux and host-scoped muxes before the radix lookup runs.

This spec owns route registration, matching, the URL round trip, host routing, mounting and introspection. The middleware tiers and `middleware.Rewrite` are in the [middleware spec](middleware.md); `Context.Rewrite`, `OriginalPath` and the route-parameter accessors are in the [context spec](context.md).

## API Surface

### Route Registration (fluent — returns `*Route`)

```go
// On App and Group
app.GET(pattern, handler)     *Route
app.POST(pattern, handler)    *Route
app.PUT(pattern, handler)     *Route
app.DELETE(pattern, handler)  *Route
app.PATCH(pattern, handler)   *Route
app.QUERY(pattern, handler)   *Route
app.HEAD(pattern, handler)    *Route
app.OPTIONS(pattern, handler) *Route
```

All registration methods return `*Route` for chaining.

### Route Fluent Methods

```go
route.Name(name string) *Route        // Named route for URL generation
route.SetMeta(key Key[T], val T) *Route // Attach metadata under a typed key
route.RemoveMeta(key Key[T]) *Route     // Remove the route's own value
route.Middleware(m ...Middleware) *Route // Per-route middleware
```

### Route Meta System (Goyave-inspired)

Metadata attached to routes, groups and the App under typed keys. One key type, `credo.Key[T]`, built with `credo.NewKey[T](name)`, serves request values ([context spec](context.md)) and route meta. The name is for diagnostics; identity is the key value, so two `NewKey` calls with one name are two keys, and the meta keys of two packages never collide. A zero `Key[T]`, declared without `NewKey`, panics in every method that takes a key, naming the method. `GetMeta` returns the route's own value, then its groups' from the innermost out, then the App's.

```go
var RequiresAuth = credo.NewKey[bool]("auth")

// Group-level — inherited by all child routes
api := app.Group("/api").SetMeta(RequiresAuth, true)
api.Middleware(authMiddleware)

// Route-level — overrides parent
api.GET("/public", handler).SetMeta(RequiresAuth, false)

// Middleware reads meta declaratively; the value is a bool, no assertion
func authMiddleware(next credo.Handler) credo.Handler {
    return func(c *credo.Context) error {
        if auth, _ := c.Route().GetMeta(RequiresAuth); auth {
            // authenticate
        }
        return next(c)
    }
}
```

Runnable as `ExampleNewKey`.

`SetMeta` with a value of another type than the key's does not compile. Registration order never decides membership: a value set on a group after its routes were registered reaches them ([how it is resolved](#meta-resolution)). `RemoveMeta` on a route removes the route's own value, so its group's shows through. The framework's meta keys are typed: `MetaAccept`, `MetaRequireHeaders`, `MetaRequireQuery`, `MetaAPIVersion` and `MetaScope` are `credo.Key[[]string]`, `MetaMaxBody` is a `credo.Key[int64]`, and `credo.MetaAccessLog` and `credo.MetaRawResponse` are `credo.Key[bool]` ([middleware spec](middleware.md#framework-meta-keys)).

**API:** the methods are generic, and `T` is inferred from the key.

```go
// Keys
func NewKey[T any](name string) Key[T]

// On App / Group — set at registration time
app.SetMeta(key Key[T], val T)
app.RemoveMeta(key Key[T])
group.SetMeta(key Key[T], val T) *Group
group.RemoveMeta(key Key[T]) *Group

// On Route and StaticRoute — set at registration time
route.SetMeta(key Key[T], val T) *Route
route.RemoveMeta(key Key[T]) *Route // removes the route's own value
static.SetMeta(key Key[T], val T) *StaticRoute
static.RemoveMeta(key Key[T]) *StaticRoute

// On RouteInfo — the view c.Route(), Exchange.Route(), app.GetRoute and app.Routes return
info.GetMeta(key Key[T]) (T, bool) // route, then its groups, then the App; a mount reports the App's

// On Route — the registration handle, reading what has been set so far
route.GetMeta(key Key[T]) (T, bool)
```

`c.Route()` returns the matched route's `RouteInfo` once dispatch has matched a route, a mount included ([Sub-router Mounting](#sub-router-mounting)). Middleware added with `app.Middleware` runs after the match on every route of the App and reads its meta. In `PreRouting` middleware before `next` runs, on the router's own 404/405 and in status handlers there is no route: `c.HasRoute()` is false and `c.Route()` is the zero `RouteInfo`, whose `GetMeta` reports false for every key. Register meta-reading middleware with `app.Middleware`, on a group or on a route, and have a guard that enforces meta check `c.HasRoute()` first, so that it fails closed.

### The Route Handle and the Route View

| | `*Route` | `RouteInfo` |
| --- | --- | --- |
| Obtained from | a registration: `app.GET(...)`, `group.POST(...)` | `c.Route()`, `Exchange.Route()`, `app.GetRoute(name)`, `app.Routes()` |
| For | configuring the route before the App starts | reading the route, at any time |
| Offers | `Name`, `SetMeta`, `RemoveMeta`, `Middleware`, `GetMeta` | `Method`, `Methods`, `Pattern`, `Host`, `Name`, `Kind`, `AutoHead`, `RegisteredAt`, `GetMeta`, `BuildURI`, `BuildURL` |
| After the App is prepared | its setters panic | built once per route and returned by value; a request allocates nothing for it |

The four readers return equal values for one route. Where no route matched, `c.Route()` and `Exchange.Route()` return the zero `RouteInfo` — empty fields, an empty `Kind`, no meta — and its `BuildURI` and `BuildURL` return an error; `app.GetRoute` returns it with `false` for a name no route has.

### Named Routes + URL Generation (Goyave-inspired)

```go
app.GET("/products/{id}", handler).Name("product.show")

// URL generation
route, ok := app.GetRoute("product.show") // a RouteInfo; ok is false for an unknown name
uri, err := route.BuildURI("42") // → "/products/42"
url, err := route.BuildURL("42") // → "/products/42" (default route, same as BuildURI)
route.Host // → "" for default routes, host pattern for host-scoped routes

// Host-scoped: host params consumed first, then path params
// app.Host("{tenant}.myapp.com").GET("/products/{id}", handler).Name("tenant.product.show")
tenant, _ := app.GetRoute("tenant.product.show")
url, err = tenant.BuildURL("acme", "42") // → "acme.myapp.com/products/42"
```

Runnable as `ExampleRouteInfo_BuildURI`.

Names are unique per App, host-scoped routes included; a duplicate panics at the `Name` call. `BuildURL` auto-fills the host from the route's host pattern. Host parameters are consumed first, then path parameters. For default (non-host-scoped) routes, `BuildURL` is equivalent to `BuildURI`. Values are decoded parameter values, exactly what `RouteParam` reports for the generated URI: each path value is checked against its regex constraint and percent-encoded for one segment with `url.PathEscape` (`a/b` becomes `a%2Fb`, `ç` becomes `%C3%A7`, `+` stays `+`), a catch-all value keeps its slashes as separators and escapes each segment, and static pattern text is written in its wire spelling (`/café/{id}` generates `/caf%C3%A9/42`). A value that could not route back is an error: it must be valid UTF-8 (the router answers `%FF` with 400) and its wire spelling must not show the byte that delimits its parameter in the pattern, checked in the canonical form where matching cuts (`a.b` for `{name}.json` would be cut at the first `.`, since `%2E` and `.` spell the same URI; `a;b` for `{id};v` is written `a%3Bb` and routes back, since matching keeps that reserved escape; `a=b` for `{k}={v}` cannot, since `=` stays literal; `a%b` for `{id}%done` cannot, since its `%25` is the delimiter unit); a slash-delimited value may contain a slash, which is escaped as data. Host values fill one label each: they must be non-empty, consist of letters, digits, hyphens and underscores, satisfy the label's constraint, and are never percent-encoded. Both methods return an error when a value is missing, empty, not valid UTF-8, contains its delimiter, fails its constraint or is not a valid host label, when extra parameters are provided, or when the stored pattern is malformed. Round trips preserve values, not the client's spelling of percent-encoded octets (see [Encoded Parameter Values](#encoded-parameter-values)). Wildcard host patterns such as `*.example.com` cannot generate concrete URLs; use `{tenant}.example.com` when URL generation needs a subdomain value.

### StatusHandler System (Goyave-inspired)

App-level customizable handlers for the router's own 404 and 405 answers.

```go
// Custom 404 handler
app.StatusHandler(http.StatusNotFound, func(c *credo.Context) error {
    return c.Response().JSON(404, map[string]string{"error": "not found"})
})
```

Runnable as `ExampleApp_StatusHandler`.

`StatusHandler` is consulted for two codes only: 404, when no route matches the request, and 405, when routes match the path but none of them serves the method (the `Allow` header is set before the handler runs). The handler answers in place of the default error response, inside the `PreRouting` middleware and without a matched route; the routed tier does not run. With no custom handler registered, the two outcomes are the `ErrNotFound` and `ErrMethodNotAllowed` errors of the central error pipeline. `StatusHandler(code, h)` panics at registration for any other code, with a message naming the two supported codes; there is no status handler for 403, 500 or any other status, and those responses are shaped through `UseErrorRenderer` ([ADR-007](../adr/007-router-and-routing.md#status-handlers)). Status handlers are not error handlers either: an error a route handler or middleware returns, `ErrNotFound` from a route handler included, goes through the central error pipeline, whose body an `ErrorRenderer` shapes, and does not reach the custom 404 handler. StatusHandler is set on the `App` only; group-level overrides are not supported.

### Middleware Tiers

User middleware runs PreRouting → routed → Group (outer to inner) → Route → Handler ([middleware spec](middleware.md#tiers-prerouting-routed-group-route)). `app.PreRouting(m...)` adds middleware that runs before the match on every request, the router's own 404 and 405 included. `app.Middleware(m...)` adds middleware that runs after the match on every route registered through the App — directly, in a group, in a host group, as a static route or as a mount — before group and route middleware, and never for the router's own 404 and 405. It delegates to the root group's `Middleware`: the App's routes, groups and host groups descend from that group, and group middleware is collected when the chain is compiled, so a middleware added after a group was created applies to its routes too. After preparation the call panics, as every registration does.

### Route Groups and Sub-routers

```go
// Group — shared prefix + middleware
api := app.Group("/api")
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
- Host params are exposed alongside path params: `c.Request().RouteParam(name)` for single values, `c.Request().RouteParams()` for the full map.
- Host and path params share one namespace. Registering a route whose path params collide with host param names panics at registration time.
- A host parameter has a name, used once in the pattern, and a regex-constrained one has a constraint: `{}.example.com`, `{:[a-z]+}.example.com`, `{a}.{a}.example.com` and `{org:}.example.com` panic at registration time.
- Host patterns are normalized to lowercase and may not include a port. Incoming request hosts are normalized by lowercasing, stripping any port, and trimming a trailing dot.
- Matching is case-insensitive.
- Wildcard `*` is matching-only, captures no route param, and may only appear once as the leftmost complete label. `*.acme.io` matches `api.acme.io`, but not `acme.io` or `a.b.acme.io`.
- `*` and `*.io` are valid but broad patterns. `api*.acme.io`, `foo.*.io`, `*.*.acme.io`, and mixed wildcard/param patterns such as `*.{tenant}.acme.io` and `{tenant}.*.acme.io` are rejected at registration time.
- Host patterns with identical match semantics panic at registration time. Parameter names are ignored for this comparison, and `*` is equivalent to an unconstrained parameter: `{a}.acme.io`, `{b}.acme.io`, and `*.acme.io` are equivalent; choose one. Regex-constrained patterns with different semantics remain valid: `{org:[a-z]+}.acme.io` and `{tenant}.acme.io` are distinct.

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

Runnable as `ExampleApp_Mount`, with the path handoff and a parametric prefix below.

**A mount is a route.** A request a `Mount` matches has a `RouteInfo` of kind mount — the mount's prefix as its pattern, the App's meta, no name and no method — so `c.HasRoute()` is true for it and `c.Route().GetMeta` reads the App's meta. The mounted handler runs inside the routed tier, after `PreRouting` middleware and the framework features, through a chain compiled as a route's is, so authentication added with `app.Middleware` answers 401 for a mounted path as for any route. A mount belongs to no group and has no route value of its own, so group and route middleware do not apply to it; a mounted sub-application that needs a protection only some of the App's routes have enforces it internally. The access record of a mounted request names the mount's pattern, and the mount's introspection entry reports the App's meta. The prefix's parameters reach the mounted handler as path values (below); the mount's route carries none, so a routed middleware on a parametric mount reads none through `c.Request().RouteParam`. The QUERY `Content-Type` check runs at the end of a mount's chain, as at a route's, so `PreRouting` and routed middleware run before its 415.

**Path handoff:** the child receives the remainder below the prefix spelled for the wire — `URL.Path` decoded and `URL.RawPath` set when the two spellings differ — so `/admin/a%2Fb` reaches the child as `/a%2Fb` (`EscapedPath`), never as `/a/b`, `/admin/a%3Bb` as `/a%3Bb` (a reserved escape is handed on as spelled), a prefix such as `/été` is matched in its canonical form, and a nested Credo app decodes its own captures once. The prefix followed by a slash is the child's root, like the bare prefix: `/admin/` and `/admin` both reach the child as `/`. A root mount (`Mount("/", h)`) forwards the whole path space, the bare `/` included.

**Parametric prefix:** the prefix may carry parameters — `Mount("/t/{tenant}", h)`, several of them, also within one segment (`/v/{major}.{minor}`), and regex-constrained ones. The child receives the path below the matched prefix, handed over as under a static prefix (`/t/acme/x/y` → `/x/y`, `/t/acme/a%2Fb` → `/a%2Fb`, the exact prefix `/t/acme` and `/t/acme/` → `/`), and each parameter of the prefix as a stdlib path value, decoded once like every capture (`/t/ac%2Fme/x` → `tenant` = `ac/me`, path `/x`). A stdlib handler reads it with `r.PathValue("tenant")` or `credo.URLParam(r, "tenant")`; a mounted Credo app with `c.Request().PathValue("tenant")`, while its `RouteParam`/`RouteParams` hold its own route parameters only. The values are set on a clone of the request (`http.Request.Clone`), so a request the caller still holds — the one an `http.ServeMux` in front of the app matched, or the child of an outer parametric mount — keeps its path values; the child inherits them, nested parametric mounts add up, and a name used again holds the value of the prefix closest to the handler. The child still gets no parent route context, and the internal `_mount` capture is not a path value. The name `_mount` is reserved for that capture: a prefix parameter of that name, or a name the prefix uses twice, panics before anything is registered (`credo: Mount "/t/{_mount}": the parameter name "_mount" is reserved`). A catch-all parameter is not a prefix: it would take the rest of the path, leaving nothing for the child, so a prefix holding one panics before anything is registered (`credo: Mount "/t/{p...}": a prefix cannot hold the catch-all {p...}: …`).

**Method scope:** the mounted handler is registered for all standard HTTP methods except CONNECT and TRACE, which are excluded deliberately (CONNECT is a proxy mechanism; TRACE enables cross-site tracing). Requests using them receive 405.

**Atomic registration:** a `Mount` registers every forwarded method on both the exact prefix (`/admin`) and its catch-all (`/admin/{_mount...}`). A `Mount` that conflicts with an existing route — a route already registered on one of those method/pattern pairs, or a structural conflict in the prefix — panics and registers nothing, leaving the router exactly as it was. The prefix's own parameter names are checked first; across routes, parameter names never conflict: they belong to endpoints.

### HEAD Auto-handling

Every GET registration (`GET`, `Static`, `File`, on `App` or `Group`) also registers an automatic HEAD twin for the same pattern, which runs the GET handler and discards the body. `Route.Middleware` and `Route.SetMeta` on the GET route reach its twin, so HEAD runs the same chain and reads the same meta as GET and cannot bypass a GET route's authentication or rate limit ([ADR-007](../adr/007-router-and-routing.md#head-auto-handling)). An explicitly registered HEAD route for the same pattern replaces the twin, in either registration order, and keeps its own chain and meta. Introspection marks the twin with `RouteInfo.AutoHead`.

### QUERY (RFC 10008)

`App.QUERY` and `Group.QUERY` register explicit safe, idempotent QUERY routes. The query representation travels in request content and is normally decoded and validated with `c.Request().BindBody(&input)`. Credo does not generate a GET twin or a HEAD twin: GET query parameters and a QUERY body are different input contracts.

Every matched QUERY request, including one dispatched to a mounted `http.Handler`, must carry a non-blank `Content-Type`. Missing or blank values fail before the application handler with `400 content_type_required`, even when the body is empty. A present but unsupported media type follows the normal binder contract and returns 415; malformed supported content follows the bind-error contract. The guard is innermost, so the framework features and `PreRouting`, routed, group and route middleware still run first.

`Accept-Query` advertisement is optional and application-owned. Set the response header directly or from application middleware when needed. Credo adds no QUERY-only registration option, metadata key, automatic OPTIONS handler, or media contract; `middleware.ContractGuard` with `MetaAccept` remains the generic opt-in request media contract.

QUERY responses are cacheable only when the cache key incorporates request content and relevant metadata. Deployments that cannot guarantee body-aware behavior across caches, CDNs, proxies, WAFs, gateways, and observability tooling should use `Cache-Control: no-store` and verify that the entire chain preserves and records QUERY.

### Trailing Slash Redirect

When a request path does not match any route, the router probes the path with the trailing slash toggled (`/users/` ↔ `/users`). If the alternate matches a route for the requested method, the router issues a redirect:

- **GET / HEAD** → `301 Moved Permanently`
- **Other methods** → `308 Permanent Redirect` (preserves method)

Query strings are preserved. The root path `/` and a CONNECT request are never redirected. 405 takes precedence over redirect, and a capture that decodes to invalid UTF-8 answers 400 before the probe ([Encoded Parameter Values](#encoded-parameter-values)). A catch-all's prefix with the slash is a match, not a redirect: `/files/` reaches `/files/{path...}` with an empty capture, and `/files` — with no route of its own — is redirected to `/files/`.

Enabled by default. Disable via option or config:

```go
credo.New(credo.WithRedirectTrailingSlash(false))
```

```json
{"server": {"redirect_trailing_slash": false}}
```

### URL Parameters

| Syntax | Example | Description |
| --- | --- | --- |
| `{name}` | `/users/{id}` | Named parameter |
| `{name:regex}` | `/users/{id:[0-9]+}` | Regex-constrained parameter |
| `{name...}` | `/files/{path...}` | Catch-all (rest of path, possibly empty) |

The same `{name}` / `{name:regex}` syntax is reused for host labels in `app.Host(...)`.

A catch-all matches an empty rest, as in chi and `net/http.ServeMux`: `/files/{path...}` serves `/files/` with `path` = `""`, and `BuildURI("")` builds `/files/`. A `{name}` or `{name:regex}` parameter never matches an empty segment.

**Parameter names belong to the endpoint.** A route is identified by its HTTP method and its name-stripped shape (`/users/{}`, `/users/{:[0-9]+}`, `/files/{...}`), and each endpoint names the parameters of its own pattern ([how the tree captures them](#parameter-capture)). Routes that share a dynamic segment may therefore name it differently, and each handler sees only its own names — a sibling endpoint's name is never visible, and `RouteParams()` holds exactly the matched route's parameters.

```go
// Valid: shared dynamic segment, endpoint-specific names.
app.GET("/v1/crm/customers/{id}", showCustomer)                        // RouteParam("id")
app.GET("/v1/crm/customers/{customer_id}/timeline", customerTimeline)  // RouteParam("customer_id")
app.DELETE("/v1/crm/customers/{cid}", deleteCustomer)                  // RouteParam("cid")

// Duplicate: same method and same shape — names do not distinguish routes.
app.GET("/v1/crm/customers/{id}", showCustomer)
app.GET("/v1/crm/customers/{customer_id}", showCustomer) // panics: already registered as "/v1/crm/customers/{id}"
```

The valid half runs as `Example_parameterNames`.

Registering the same method on the same shape panics with `credo: duplicate route: GET "/…/{customer_id}" is already registered as "/…/{id}" (parameter names do not distinguish routes)` plus both call sites, exactly like a literal re-registration; an explicit HEAD replaces the automatic twin of a GET. Structural conflicts panic at registration: two different regex matchers at one path level, or one matcher followed by different tail bytes. The same model applies to regex-constrained and catch-all segments; `BuildURI`/`BuildURL` read the names from the selected route's own pattern, and path trees under `app.Host(...)` behave identically.

**A pattern names each parameter once.** A name is how a capture is read, so `/a/{id}/b/{id}` panics at registration with `credo: pattern: duplicate parameter name "id" in "/a/{id}/b/{id}"`, as in chi and `net/http.ServeMux`. The check runs on the whole registered pattern, so it covers a group prefix joined to its routes, a `Mount` prefix and `Static` under a parametric group (`/t/{_static}` repeats the name `Static` captures with); a rewrite rule's `From` pattern follows the same rule.

**A catch-all ends its pattern.** A catch-all takes the rest of the path, so text after it could never match: `/a/{p...}/x` panics at registration with `credo: pattern: text "/x" after the catch-all {p...} in "/a/{p...}/x": a catch-all takes the rest of the path, so end the pattern with it`, the message a rewrite rule's `From` pattern gets. The check runs on the whole registered pattern, a group prefix joined to its routes included. A `Mount` prefix, or a group prefix under `Static`, cannot hold a catch-all at all, since the registration adds its own capture below the prefix; the panic names the prefix as written.

### Matching Order and Method Not Allowed

At each path level the candidates are tried from the most to the least specific: static text, a regex-constrained parameter, a plain parameter, a catch-all. A mount is a catch-all under its prefix. The first candidate that matches the path **and** serves the request method answers. A candidate that matches the path but has no endpoint for the method does not end the search — the next, less specific candidate is tried, as in chi and `net/http.ServeMux`:

```go
app.GET("/users/new", newUserForm)
app.POST("/users/{id}", updateUser)

// GET    /users/new → newUserForm
// POST   /users/new → updateUser, id = "new"
// DELETE /users/new → 405, Allow: GET, HEAD, POST
```

Runnable as `Example_matchingOrder`; the redirect above as `Example_trailingSlashRedirect`.

A request is answered 405 only when no candidate that matches the path serves its method, and `Allow` then lists the methods of every such candidate. The route that answers brings its own group and route middleware: `POST /users/new` above runs `updateUser`'s chain, not `newUserForm`'s. An application that wants the static path to refuse a method registers that method there.

### Encoded Parameter Values

Matching runs on the canonical form of the wire path: `URL.EscapedPath()` with every escape decoded except those of the RFC 3986 reserved characters (`/ ? # [ ] : @` and `! $ & ' ( ) * + , ; =`) and of `%`, which stay in upper-case hexadecimal. Segment boundaries and reserved spellings therefore come from the client — an encoded slash stays data and `%3B` is not `;` (§2.2: `/lit/a%3Bb` does not match static `/lit/a;b`) — while every equivalent spelling meets the same route: `/caf%C3%A9/42`, `/caf%c3%a9/42` and `/%63af%C3%A9/42` all reach `/café/{id}`, and an encoded unreserved delimiter is the delimiter (`%2D` and `-` spell the same URI, §2.3). Registered static text is brought to the same form (a literal `%` becomes `%25`; every other byte, reserved characters included, matches only its literal spelling), and each captured value is percent-decoded exactly once, so a captured `%3B` is the value `;`. A parameter candidate is the canonical text up to its tail byte (the pattern byte after the closing brace, as in `{name}.json`; a literal `%` is its `%25` unit) or the next slash, whichever comes first, with every remaining escape treated as one unit (`%3B` is never a `;` delimiter, `%2F` never a boundary), so `{name}` and `{name:regex}` never span a raw slash; a catch-all takes the rest of the path, keeps its slashes as separators and decodes each segment. A regex constraint applies to the whole decoded value, never to a prefix of it, and `RouteParam` reports that same value.

| Incoming parameter text | RouteParam value | Generation from that value | Contract |
| --- | --- | --- | --- |
| `%2F` | `/` | `%2F` for a single-segment parameter | Captured data; does not create a routing segment |
| `%252F` | `%2F` | `%252F` | No second decoding |
| `%31` | `1` | `1` | Matches a decoded numeric constraint |
| `+` | `+` | `+` | No query-style conversion to space |
| `%C3%A7` | `ç` | `%C3%A7` | Valid Unicode value preserved |
| `2024%2D09` for `{year}-{month}` | `2024`, `09` | `2024-09` | An encoded unreserved delimiter is the delimiter; a value cannot contain it |
| `a%3Bb;v` for `{id};v` | `a;b` | `a%3Bb;v` | An encoded reserved character is not the delimiter; generation escapes it |
| `%2F%25done` for `{id}%done` | `/` | `%2F%25done` | A `%` delimiter is its `%25` unit, never the `%` of another escape |

**The path as received.** The router matches the path the client sent. It removes no dot-segment and merges no empty segment: `/a/../b` does not match a route `/b`, `//a` does not match a route `/a`, and neither is redirected to a clean form. A parameter may capture `..` as its value: `/files/..` gives `/files/{name}` the value `..`, and `/files/..%2F..%2Fetc` the value `../../etc`. Browsers and HTTP clients remove dot-segments before they send a request, so a `..` on the wire was sent deliberately; matched as received, it reaches only the handler for the path that a proxy or a guard in front of the application judged ([ADR-007](../adr/007-router-and-routing.md#the-path-as-received)).

A decoded value is data, not a path: it may contain `/`, `\` and `..`, and the router makes no filesystem guarantee. Confining file access is the consumer's job — `os.Root` for handlers ([routing guide](../guides/routing.md#route-parameters-are-not-file-names)), the built-in sanitization for `app.Static` and `app.File` ([static spec](static.md)).

A well-formed value that fails its constraint is a route non-match: the tree backtracks to the sibling parameter or catch-all node and answers 404 when nothing else matches. A candidate that decodes to invalid UTF-8 (`%FF`) is skipped, and when no route matches the request receives 400 with the code `invalid_path_encoding` instead of 404; malformed percent-encoding (`%zz`) never reaches the router because net/http rejects the request line first. Static text follows the canonical form above; a static segment that decodes to invalid UTF-8 is simply a non-match. `Context.OriginalPath` reports the wire-form path, a `middleware.Rewrite` target is a wire-form path (a malformed escape in it panics when the middleware is built) and a mounted handler receives the raw remainder below its prefix (see [Sub-router Mounting](#sub-router-mounting)).

The split-before-decode and no-double-decode rules follow [RFC 3986 §2.4](https://www.rfc-editor.org/rfc/rfc3986#section-2.4); the line between escapes that are normalized and escapes that are kept follows [§2.2](https://www.rfc-editor.org/rfc/rfc3986#section-2.2) and [§2.3](https://www.rfc-editor.org/rfc/rfc3986#section-2.3). Path parameter `+` behavior matches [Go PathUnescape](https://pkg.go.dev/net/url#PathUnescape).

### Router Interface

```go
// App implements http.Handler
app.ServeHTTP(w, r)

// Named route lookup: the route's view, and whether a route has the name
app.GetRoute(name string) (RouteInfo, bool)

// Route introspection: every route and every mount
for _, ri := range app.Routes() {
    if ri.Kind == credo.RouteKindMount {
        continue // routes only
    }
    fmt.Println(ri.Method, ri.Host, ri.Pattern, ri.Name)
}
```

Runnable as `ExampleApp_Routes`.

`app.Routes()` is the one introspection call. It covers the default mux and all host-scoped muxes and returns each route's `RouteInfo`, the view a request reads through `c.Route()`: `Method` for a normal route, or — for a mount — an empty `Method` and the sorted forwarded method set (every standard method except CONNECT/TRACE) in `Methods`; the `Pattern` (for a mount, its cleaned prefix); the `Host` pattern (empty for the default mux); the route `Name`; the resolved meta (route ← group ← app), read with `GetMeta(key)` — a mount reports the App's, and values are read-only by convention; no call enumerates a route's meta, since a typed key is reachable only through the value that declares it; `Kind` (`RouteKindRoute` or `RouteKindMount`); `AutoHead` (true for an auto-generated HEAD twin, false for an explicit HEAD); and `RegisteredAt`, the user call site (`file:line`) that registered the route or mount, or empty when it could not be captured. Mounts appear as a single `RouteKindMount` entry with the cleaned prefix (`/admin/` and `/admin` both normalize to `/admin`, `/` stays `/`) — the internal catch-all and method fan-out are hidden; a caller that wants routes only skips them in its loop, as above. The output is a deterministic total order `(Host, Pattern, Method, Kind)`; before the App is prepared introspection reads live route state, so call it after wiring is complete, not concurrently with route registration. No routing type of `internal/radix` is exported.

## Design Decisions

- **Chi radix tree, Goyave routing features** — [ADR-007](../adr/007-router-and-routing.md).
- **Host routing through a selector over per-host muxes** — [ADR-018](../adr/018-host-routing-and-rewrite.md).
- **No `ValidateBody`/`ValidateQuery` on Route** — validation follows the "parse, don't validate" binders on Context (`BindBody`, `BindQuery`; [validation spec](validation.md), [ADR-011](../adr/011-validation-strategy.md#rejected-alternatives)). A `.Validate()` convenience on Route is deferred until demand warrants it.

## Examples

### Basic

```go
app, err := credo.New()
if err != nil {
    log.Fatal(err)
}

app.GET("/", func(c *credo.Context) error {
    return c.Response().JSON(200, map[string]string{"message": "Hello, Credo!"})
})

if err := app.Run(); err != nil {
    log.Fatal(err)
}
```

### Named Routes + Meta

```go
var (
    CacheSeconds = credo.NewKey[int]("cache")
    RequiresAuth = credo.NewKey[bool]("auth")
)

app, err := credo.New()
if err != nil {
    panic(err)
}

app.GET("/products/{id:[0-9]+}", showProduct).
    Name("product.show").
    SetMeta(CacheSeconds, 300)

api := app.Group("/api").SetMeta(RequiresAuth, true)
api.GET("/users", listUsers)
api.GET("/health", healthCheck).SetMeta(RequiresAuth, false)
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
tenant.GET("/dashboard", func(c *credo.Context) error {
    return c.Response().Text(200, c.Request().RouteParam("tenant"))
})
```

Runnable as `ExampleApp_Host`.

### Status Handlers

```go
app, err := credo.New()
if err != nil {
    panic(err)
}

// HTML site — 404 renders a page
app.StatusHandler(404, func(c *credo.Context) error {
    return c.Response().HTML(404, "<h1>Page Not Found</h1>")
})

// StatusHandler is app-level only and accepts 404 and 405 (any other code
// panics). Group middleware never runs for the router's own 404/405; a
// handler-returned error is shaped by UseErrorRenderer.
```

## Current Behaviour

Not guaranteed: the data structures behind the contract above, as they are now. A release may change them without notice, provided the contract holds.

### Meta resolution

A route's meta is resolved into a per-route snapshot when its chain is compiled at preparation, so a request-time `GetMeta` does not walk route, group and App; the snapshot matches that walk.

### Host lookup

Exact static hosts use a hash-map fast path. Param, regex and wildcard host patterns use the specificity-ordered scan of [Host-Based Route Groups](#host-based-route-groups).

### Parameter capture

The radix tree identifies a route by its HTTP method and its name-stripped shape: dynamic nodes carry no name, matching captures values positionally, and the matched endpoint maps those captures to the names spelled in its own pattern (adapted from Chi's endpoint-key model).

### Mount registration

The radix tree has no delete, so a `Mount` checks every method/pattern pair before it inserts any. A static prefix hands its child a shallow copy of the request; only a parametric prefix clones it.
