# Router Spec

**Status**: Approved **Package**: Root (`github.com/credo-go/credo`), `internal/radix/` **Sources**: Chi (MIT, primary), Goyave (MIT), httprouter (BSD-3, reference) **Depends on**: — **ADRs**: [007-router-and-routing](../adr/007-router-and-routing.md), [018-host-routing-and-rewrite](../adr/018-host-routing-and-rewrite.md) — v0.24.0 decisions accepted, pending implementation ([plan](../plans/components-and-sequential-bootstrap.md))

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
router.QUERY(pattern, handler)   *Route
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

Names are unique per router tree. Duplicate names panic at startup. `BuildURL` auto-fills the host from the route's host pattern. Host parameters are consumed first, then path parameters. For default (non-host-scoped) routes, `BuildURL` is equivalent to `BuildURI`. Values are decoded parameter values, exactly what `RouteParam` reports for the generated URI: each path value is checked against its regex constraint and percent-encoded for one segment with `url.PathEscape` (`a/b` becomes `a%2Fb`, `ç` becomes `%C3%A7`, `+` stays `+`), a catch-all value keeps its slashes as separators and escapes each segment, and static pattern text is written in its wire spelling (`/café/{id}` generates `/caf%C3%A9/42`). A value that could not route back is an error: it must be valid UTF-8 (the router answers `%FF` with 400) and its wire spelling must not show the byte that delimits its parameter in the pattern, checked in the canonical form where matching cuts (`a.b` for `{name}.json` would be cut at the first `.`, since `%2E` and `.` spell the same URI; `a;b` for `{id};v` is written `a%3Bb` and routes back, since matching keeps that reserved escape; `a=b` for `{k}={v}` cannot, since `=` stays literal; `a%b` for `{id}%done` cannot, since its `%25` is the delimiter unit); a slash-delimited value may contain a slash, which is escaped as data. Host values fill one label each: they must be non-empty, consist of letters, digits, hyphens and underscores, satisfy the label's constraint, and are never percent-encoded. Both methods return an error when a value is missing, empty, not valid UTF-8, contains its delimiter, fails its constraint or is not a valid host label, when extra parameters are provided, or when the stored pattern is malformed. Round trips preserve values, not the client's spelling of percent-encoded octets (see [Encoded Parameter Values](#encoded-parameter-values)). Wildcard host patterns such as `*.example.com` cannot generate concrete URLs; use `{tenant}.example.com` when URL generation needs a subdomain value.

### StatusHandler System (Goyave-inspired)

App-level customizable handlers for the router's own 404 and 405 answers.

```go
// Custom 404 handler
app.StatusHandler(http.StatusNotFound, func(ctx *credo.Context) error {
    return ctx.Response().JSON(404, map[string]string{"error": "not found"})
})
```

`StatusHandler` is consulted for two codes only: 404, when no route matches the request, and 405, when routes match the path but none of them serves the method (the `Allow` header is set before the handler runs). The handler answers in place of the default error response, inside the global middleware and without a matched route. With no custom handler registered, the two outcomes are the `ErrNotFound` and `ErrMethodNotAllowed` errors of the central error pipeline. A handler registered for any other code is accepted and never called — there is no status handler for 500 or any other status. Status handlers are not error handlers either: an error a route handler or middleware returns, `ErrNotFound` from a route handler included, goes through the central error pipeline, whose body an `ErrorRenderer` shapes, and does not reach the custom 404 handler. StatusHandler is set on the `App` only; group-level overrides are not supported.

**Accepted, pending implementation (v0.24.0, W8).** When it ships, this paragraph replaces the sentence above that accepts a handler for any other code: `StatusHandler(code, h)` panics at registration for any code but 404 and 405, with a message naming the two supported codes, so a registration that would never be consulted fails at startup instead of being silently ignored. A 5xx handler would re-enter application code inside error rendering, so no other code is consulted. 404 and 405 register and are consulted as described above. An application that registered a handler for another code — 403 is the likely one — deletes that registration rather than porting it, and shapes that response through `UseErrorRenderer`; a 404 or 405 handler that only returns the matching sentinel (`ErrNotFound`, `ErrMethodNotAllowed`) equals the default and can go as well. See [ADR-007](../adr/007-router-and-routing.md#status-handlers).

### UseI18n (i18n integration)

```go
app.UseI18n(credo.I18nConfig{Dir: "locales/"})  // frozen-guarded, like SetMeta/StatusHandler
```

Registers i18n: the call validates the configuration and returns nothing, and the start phase loads the locale files and stores the bundle on App ([i18n spec](i18n.md#registration-and-start-phase)); the request locale is resolved lazily on the first `Locale()`/translation access through `I18nConfig.Detect`. See [ADR-013](../adr/013-internationalization.md).

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
- A host parameter has a name, used once in the pattern, and a regex-constrained one has a constraint: `{}.example.com`, `{:[a-z]+}.example.com`, `{a}.{a}.example.com` and `{org:}.example.com` panic at registration time.
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

**Middleware scope:** mounted handlers receive only global middleware (plus the framework features that wrap every request). Group and route middleware do not apply because mounted handlers are plain `http.Handler` instances dispatched outside the per-route compiled chain. If the mounted sub-application requires authentication or other protections, it must enforce them internally or the protections must be registered as global middleware.

**Path handoff:** the child receives the remainder below the prefix spelled for the wire — `URL.Path` decoded and `URL.RawPath` set when the two spellings differ — so `/admin/a%2Fb` reaches the child as `/a%2Fb` (`EscapedPath`), never as `/a/b`, `/admin/a%3Bb` as `/a%3Bb` (a reserved escape is handed on as spelled), a prefix such as `/été` is matched in its canonical form, and a nested Credo app decodes its own captures once. The prefix followed by a slash is the child's root, like the bare prefix: `/admin/` and `/admin` both reach the child as `/`.

**Parametric prefix:** the prefix may carry parameters — `Mount("/t/{tenant}", h)`, several of them, also within one segment (`/v/{major}.{minor}`), and regex-constrained ones. The child receives the path below the matched prefix, handed over as under a static prefix (`/t/acme/x/y` → `/x/y`, `/t/acme/a%2Fb` → `/a%2Fb`, the exact prefix `/t/acme` and `/t/acme/` → `/`), and each parameter of the prefix as a stdlib path value, decoded once like every capture (`/t/ac%2Fme/x` → `tenant` = `ac/me`, path `/x`). A stdlib handler reads it with `r.PathValue("tenant")` or `credo.URLParam(r, "tenant")`; a mounted Credo app with `ctx.Request().PathValue("tenant")`, while its `RouteParam`/`RouteParams` hold its own route parameters only. The values are set on a clone of the request (`http.Request.Clone`), so a request the caller still holds — the one an `http.ServeMux` in front of the app matched, or the child of an outer parametric mount — keeps its path values; the child inherits them, nested parametric mounts add up, and a name used again holds the value of the prefix closest to the handler. The child still gets no parent route context, and the internal `_mount` capture is not a path value. The name `_mount` is reserved for that capture: a prefix parameter of that name, or a name the prefix uses twice, panics before anything is registered (`credo: Mount "/t/{_mount}": the parameter name "_mount" is reserved`). A static prefix pays for none of this: its child stays a shallow copy. A catch-all parameter is not a prefix — it consumes the rest of the path, so the handler would always be handed `/`.

**Method scope:** the mounted handler is registered for all standard HTTP methods except CONNECT and TRACE, which are excluded deliberately (CONNECT is a proxy mechanism; TRACE enables cross-site tracing). Requests using them receive 405.

**Atomic registration:** a single `Mount` makes sixteen radix registrations — every forwarded method on both the exact prefix (`/admin`) and the catch-all (`/admin/{_mount...}`). Because the radix tree has no delete, a conflict discovered partway through would strand the registrations that already succeeded as orphan routes — reachable by dispatch yet hidden from introspection (they carry no `*Route`). `Mount` therefore preflights: it probes every method/pattern pair against the tree and panics before mutating anything if an explicit route already occupies one of them, so a conflicting `Mount` registers nothing and leaves the router exactly as it was. Only duplicate endpoints need the preflight; a structural conflict (a second regexp matcher or a mismatched regexp tail in the prefix) always surfaces on the very first registration, since the catch-all is registered before the exact prefix and shares its entire path, so it can never leave a partial state. The prefix's own parameter names are checked before the preflight; across routes, parameter names never conflict: they belong to endpoints.

### HEAD Auto-handling

GET routes automatically respond to HEAD requests (body discarded). Explicit HEAD registration overrides the auto-generated one.

### QUERY (RFC 10008)

`App.QUERY` and `Group.QUERY` register explicit safe, idempotent QUERY routes. The query representation travels in request content and is normally decoded and validated with `ctx.Request().BindBody(&input)`. Credo does not generate a GET twin or a HEAD twin: GET query parameters and a QUERY body are different input contracts.

Every matched QUERY request, including one dispatched to a mounted `http.Handler`, must carry a non-blank `Content-Type`. Missing or blank values fail before the application handler with `400 content_type_required`, even when the body is empty. A present but unsupported media type follows the normal binder contract and returns 415; malformed supported content follows the bind-error contract. The guard is innermost, so the framework features and ordinary global/group/route middleware still run first.

`Accept-Query` advertisement is optional and application-owned. Set the response header directly or from application middleware when needed. Credo adds no QUERY-only registration option, metadata key, automatic OPTIONS handler, or media contract; `middleware.ContractGuard` with `MetaAccept` remains the generic opt-in request media contract.

QUERY responses are cacheable only when the cache key incorporates request content and relevant metadata. Deployments that cannot guarantee body-aware behavior across caches, CDNs, proxies, WAFs, gateways, and observability tooling should use `Cache-Control: no-store` and verify that the entire chain preserves and records QUERY.

### Trailing Slash Redirect

When a request path does not match any route, the router probes the path with the trailing slash toggled (`/users/` ↔ `/users`). If the alternate matches, the router issues a redirect:

- **GET / HEAD** → `301 Moved Permanently`
- **Other methods** → `308 Permanent Redirect` (preserves method)

Query strings are preserved. The root path `/` is never redirected. 405 takes precedence over redirect. A catch-all's prefix with the slash is a match, not a redirect: `/files/` reaches `/files/{path...}` with an empty capture, and `/files` — with no route of its own — is redirected to `/files/`.

Enabled by default. Disable via option or config:

```go
credo.New(credo.WithRedirectTrailingSlash(false))
```

```json
{"server": {"redirect_trailing_slash": false}}
```

### URL Parameters

| Syntax         | Example              | Description                              |
| -------------- | -------------------- | ---------------------------------------- |
| `{name}`       | `/users/{id}`        | Named parameter                          |
| `{name:regex}` | `/users/{id:[0-9]+}` | Regex-constrained parameter              |
| `{name...}`    | `/files/{path...}`   | Catch-all (rest of path, possibly empty) |

The same `{name}` / `{name:regex}` syntax is reused for host labels in `app.Host(...)`.

A catch-all matches an empty rest, as in chi and `net/http.ServeMux`: `/files/{path...}` serves `/files/` with `path` = `""`, and `BuildURI("")` builds `/files/`. A `{name}` or `{name:regex}` parameter never matches an empty segment.

**Parameter names belong to the endpoint, not to the tree.** The radix tree identifies a route by its HTTP method and its name-stripped shape (`/users/{}`, `/users/{:[0-9]+}`, `/files/{...}`): dynamic nodes carry no name, matching captures values positionally, and the matched endpoint maps those captures to the names spelled in its own pattern (adapted from Chi's endpoint-key model). Routes that share a dynamic segment may therefore name it differently, and each handler sees only its own names — a sibling endpoint's name is never visible, and `RouteParams()` holds exactly the matched route's parameters.

```go
// Valid: shared dynamic segment, endpoint-specific names.
app.GET("/v1/crm/customers/{id}", showCustomer)                        // RouteParam("id")
app.GET("/v1/crm/customers/{customer_id}/timeline", customerTimeline)  // RouteParam("customer_id")
app.DELETE("/v1/crm/customers/{cid}", deleteCustomer)                  // RouteParam("cid")

// Duplicate: same method and same shape — names do not distinguish routes.
app.GET("/v1/crm/customers/{id}", showCustomer)
app.GET("/v1/crm/customers/{customer_id}", showCustomer) // panics: already registered as "/v1/crm/customers/{id}"
```

The duplicate policy stays strict: registering the same method on the same shape panics with `credo: duplicate route: GET "/…/{customer_id}" is already registered as "/…/{id}" (parameter names do not distinguish routes)` plus both call sites, exactly like a literal re-registration, and automatic HEAD twins follow the existing overwrite rules. Structural conflicts are unchanged and still panic at registration: two different regex matchers at one path level, or one matcher followed by different tail bytes. The same model applies to regex-constrained and catch-all segments; `BuildURI`/`BuildURL` read the names from the selected route's own pattern, and path trees under `app.Host(...)` behave identically while host-label captures are unaffected.

**A pattern names each parameter once.** A name is how a capture is read, so `/a/{id}/b/{id}` panics at registration with `credo: pattern: duplicate parameter name "id" in "/a/{id}/b/{id}"`, as in chi and `net/http.ServeMux`. The check runs on the whole registered pattern, so it covers a group prefix joined to its routes, a `Mount` prefix and `Static` under a parametric group (`/t/{_static}` repeats the name `Static` captures with); a rewrite rule's `From` pattern follows the same rule.

### Matching Order and Method Not Allowed

At each path level the candidates are tried from the most to the least specific: static text, a regex-constrained parameter, a plain parameter, a catch-all. A mount is a catch-all under its prefix. The first candidate that matches the path **and** serves the request method answers. A candidate that matches the path but has no endpoint for the method does not end the search — the next, less specific candidate is tried, as in chi and `net/http.ServeMux`:

```go
app.GET("/users/new", newUserForm)
app.POST("/users/{id}", updateUser)

// GET    /users/new → newUserForm
// POST   /users/new → updateUser, id = "new"
// DELETE /users/new → 405, Allow: GET, HEAD, POST
```

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

A decoded value is data, not a path: it may contain `/`, `\` and `..`, and the router makes no filesystem guarantee. Confining file access is the consumer's job — `os.Root` for handlers ([routing guide](../guides/routing.md#route-parameters-are-not-file-names)), the built-in sanitization for `app.Static` and `app.File` ([static spec](static.md)).

A well-formed value that fails its constraint is a route non-match: the tree backtracks to the sibling parameter or catch-all node and answers 404 when nothing else matches. A candidate that decodes to invalid UTF-8 (`%FF`) is skipped, and when no route matches the request receives 400 with the code `invalid_path_encoding` instead of 404; malformed percent-encoding (`%zz`) never reaches the router because net/http rejects the request line first. Static text follows the canonical form above; a static segment that decodes to invalid UTF-8 is simply a non-match. `Context.OriginalPath` reports the wire-form path, a `Context.Rewrite` target is a wire-form path (a malformed escape makes `Rewrite` return an error) and a mounted handler receives the raw remainder below its prefix (see [Sub-router Mounting](#sub-router-mounting)).

The split-before-decode and no-double-decode rules follow [RFC 3986 §2.4](https://www.rfc-editor.org/rfc/rfc3986#section-2.4); the line between escapes that are normalized and escapes that are kept follows [§2.2](https://www.rfc-editor.org/rfc/rfc3986#section-2.2) and [§2.3](https://www.rfc-editor.org/rfc/rfc3986#section-2.3). Path parameter `+` behavior matches [Go PathUnescape](https://pkg.go.dev/net/url#PathUnescape). The root test package covers the table in matching and generation, decoded-value constraints with backtracking, encoded unreserved and reserved delimiters, a `%` delimiter, tail-bounded and catch-all captures, invalid UTF-8, net/http rejection of malformed escapes, trailing-slash redirects, rewrite targets, mount handoff and host-label validation.

### Router Interface

```go
// App implements http.Handler
app.ServeHTTP(w, r)

// Named route lookup
app.GetRoute(name string) *Route

// Route introspection (free functions)
credo.Walk(app, func(method, pattern string) error {
    fmt.Println(method, pattern)
    return nil
})

credo.WalkRoutes(app, func(ri credo.RouteInfo) error {
    fmt.Println(ri.Kind, ri.Method, ri.Host, ri.Pattern, ri.Name, ri.Meta)
    return nil
})
```

`*App` satisfies the `Routes` introspection interface directly (`credo.Walk(app, …)` / `credo.WalkRoutes(app, …)`), covering the default mux and all host-scoped muxes. `Walk` keeps the simple `(method, pattern)` callback and visits real routes only; `WalkRoutes` (like `app.Routes()`) exposes the full `RouteInfo`: `Method` for a normal route, or — for a mount — an empty `Method` and the sorted forwarded method set (every standard method except CONNECT/TRACE) in `Methods`; the route `Name`; the resolved `Meta` (route ← group ← app) as a fresh shallow map (nil if none, values read-only by convention); `Kind` (`RouteKindRoute` or `RouteKindMount`); and `AutoHead` (true for an auto-generated HEAD twin, false for an explicit HEAD). Mounts appear as a single `RouteKindMount` entry with the cleaned prefix (`/admin/` and `/admin` both normalize to `/admin`, `/` stays `/`) — the internal catch-all and method fan-out are hidden, and `Walk` skips mounts entirely. `Routes()` output is a deterministic total order `(Host, Pattern, Method, Kind)`; introspection reads live route state, so call it after wiring is complete, not concurrently with route registration.

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

// StatusHandler is app-level only and consulted for 404 and 405;
// groups inherit the app's handlers.
// Use middleware with route meta for group-specific error responses.
```
