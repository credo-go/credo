# Routing Guide

This guide explains how to use Credo's routing features in application code: path routing, route groups, host-based routing, pre-dispatch rewrite, conditional forwarding, introspection, and testing handlers.

For internal design rationale, see the [Router Spec](../specs/router.md), [Middleware Spec](../specs/middleware.md), [Context Spec](../specs/context.md), and [ADR-018](../adr/018-host-routing-and-rewrite.md).

---

## Quick Start

```go
package main

import (
    "log"

    "github.com/credo-go/credo"
    "github.com/credo-go/credo/middleware"
)

func main() {
    app, err := credo.New()
    if err != nil {
        log.Fatal(err)
    }

    app.PreRouting(middleware.Rewrite(middleware.RewriteConfig{Rules: []middleware.RewriteRule{
        {From: "/v1/{path...}", To: "/api/v1/{path}"},
    }}))

    api := app.Host("api.example.com")
    v1 := api.Group("/api/v1")
    v1.GET("/users/{id}", func(c *credo.Context) error {
        return c.Response().JSON(200, map[string]string{
            "id": c.Request().RouteParam("id"),
        })
    })

    app.GET("/", func(c *credo.Context) error {
        return c.Response().Text(200, "landing")
    })

    if err := app.Run(); err != nil {
        log.Fatal(err)
    }
}
```

---

## Path Routing

Credo supports three path parameter forms:

| Syntax         | Example              | Meaning                             |
| -------------- | -------------------- | ----------------------------------- |
| `{name}`       | `/users/{id}`        | single segment                      |
| `{name:regex}` | `/users/{id:[0-9]+}` | constrained segment                 |
| `{name...}`    | `/files/{path...}`   | catch-all remainder, possibly empty |

```go
app.GET("/users/{id}", getUser)
app.GET("/users/{id:[0-9]+}", getNumericUser)
app.GET("/files/{path...}", serveFile)
```

Read a single param with `c.Request().RouteParam(name)`:

```go
func getUser(c *credo.Context) error {
    id := c.Request().RouteParam("id")
    return c.Response().Text(200, id)
}
```

`c.Request().RouteParams()` returns all params as a `map[string]string`. The map is a read-only view built on first use, so prefer `RouteParam` for single values.

Parameter names belong to the route that declares them. Routes that share a dynamic segment may name it differently, and each handler reads its own names:

```go
app.GET("/v1/crm/customers/{id}", showCustomer)                        // RouteParam("id")
app.GET("/v1/crm/customers/{customer_id}/timeline", customerTimeline)  // RouteParam("customer_id")
```

What the router compares is the method and the shape of the pattern with names stripped, so two registrations of the same method on the same shape are a duplicate whatever they call the parameter:

```go
app.GET("/v1/crm/customers/{id}", showCustomer)
app.GET("/v1/crm/customers/{customer_id}", showCustomer) // panics: already registered as "/v1/crm/customers/{id}"
```

Regex constraints are structural: two different constraints at the same position (`{id:[0-9]+}` next to `{slug:[a-z]+}`) still conflict at registration, while the same constraint under different names is shared.

Within one pattern each name is used once, counting a group or mount prefix and the host pattern: `/a/{id}/b/{id}` panics at registration, since `RouteParam("id")` could read only one of the two captures.

### Encoded Values

Routing matches a canonical form of the path: every escape is decoded except those of the RFC 3986 reserved characters (such as `/`, `;` and `?`) and of `%` itself. Any spelling of the same bytes therefore reaches the same route (`/caf%C3%A9` and `/caf%c3%a9` both meet `/café`), a percent-encoded slash is data inside one segment, each captured value is decoded exactly once, and a constraint is evaluated on the decoded value:

```go
app.GET("/files/{name}", func(c *credo.Context) error {
    // GET /files/a%2Fb      -> "a/b"  (one segment; the encoded slash is data)
    // GET /files/%31        -> "1"
    // GET /files/caf%C3%A9  -> "café"
    // GET /files/a/b        -> 404    (a raw slash ends the segment)
    return c.Response().Text(200, c.Request().RouteParam("name"))
})

app.GET("/orders/{id:[0-9]+}", getOrder) // GET /orders/%31 matches with id "1"
app.GET("/docs/{path...}", serveDoc)      // GET /docs/a%2Fb/c -> "a/b/c"
```

Do not decode `RouteParam` values again: a second `PathUnescape` would turn `%2F` data into a slash. A parameter is always one segment — `{name}.json` does not match `/a/b.json` — so use `{name...}` when a value may span segments. An encoded unreserved delimiter is the delimiter: `2024%2D09` and `2024-09` are the same URI, so a value can never contain an unreserved byte that follows its parameter in the pattern. Reserved characters keep their escape (RFC 3986 §2.2): `/t/a%3Bb;v` gives `{id};v` the value `a;b`, and `/lit/a%3Bb` does not match static `/lit/a;b`. A value that decodes to invalid UTF-8 (`%FF`) fails with 400 `invalid_path_encoding`; a malformed escape such as `%zz` is rejected by net/http before Credo sees it. A `middleware.Rewrite` target is wire form too, so `To: "/files/a%2Fb"` reaches the handler as `a/b`.

URL generation is the mirror image: name the route, look it up with `app.GetRoute`, pass decoded values and let `BuildURI` escape them per segment; static text comes out in its wire spelling. A value that could not route back — invalid UTF-8, or one containing its parameter's delimiter (`a.b` for `{name}.json`) — is an error.

```go
app.GET("/files/{name}", h).Name("file.show")
file, _ := app.GetRoute("file.show") // the route's RouteInfo; false for an unknown name
uri, _ := file.BuildURI("a/b")       // "/files/a%2Fb"
uri, _ = file.BuildURI("café")       // "/files/caf%C3%A9"
_, err := file.BuildURI("")          // error: empty value for parameter "name"

app.GET("/docs/{path...}", serveDoc).Name("docs")
docs, _ := app.GetRoute("docs")
uri, _ = docs.BuildURI("")           // "/docs/": a catch-all matches an empty rest

app.GET("/orders/{id:[0-9]+}", getOrder).Name("order.show")
order, _ := app.GetRoute("order.show")
_, err = order.BuildURI("x1")        // error: does not match constraint "[0-9]+"
```

`app.GET` returns a `*credo.Route`, the handle that configures the route — `Name`, `SetMeta`, `Middleware` — before the App starts. Reading a route, and building its URLs, goes through its `credo.RouteInfo`: `app.GetRoute(name)` here, `c.Route()` in a handler or a middleware for the route the request matched, and `app.Routes()` for all of them ([Introspection](#introspection)).

#### Route Parameters Are Not File Names

The router matches the path as the client sent it: it removes no dot-segment and merges no empty segment, so `/a/../b` does not reach a route `/b`, `//a` does not reach a route `/a`, and neither is redirected to a clean form. A `..` therefore reaches a handler only as a parameter's value, never as a resolved path. Browsers and HTTP clients remove dot-segments before they send a request, so a `..` on the wire was sent on purpose — and it is exactly the value a handler that opens files must refuse.

A route parameter is untrusted, already-decoded data. One URL segment is not a safe file name: `/files/..%2F..%2Fetc` gives `{name}` the value `../../etc`, and `/files/..` gives it `..`. Before using a parameter to open a file, confine the access to a directory:

```go
uploads, err := os.OpenRoot("./uploads")
if err != nil {
    log.Fatal(err)
}
app.OnStop(func(context.Context) error { return uploads.Close() }) // after the HTTP drain

app.GET("/uploads/{name}", func(c *credo.Context) error {
    f, err := uploads.Open(c.Request().RouteParam("name"))
    if err != nil {
        return credo.ErrNotFound // "../../etc": "path escapes from parent"
    }
    defer f.Close()
    return c.Response().Stream(http.StatusOK, "application/octet-stream", f)
})
```

- **Prefer `os.Root`.** Opening through an [`os.Root`](https://go.dev/blog/osroot) — or through the `fs.FS` that `credo.DirFS` returns, which is backed by one ([static files](static-files.md#osroot-production-symlink-safe)) — keeps every access inside the directory, symlinks included. Close the root, or the closer `DirFS` returns, when the application stops.
- **`filepath.Join` is not a check.** It composes paths; `filepath.Join(base, "../../etc")` leaves `base`. Joining is fine once the name is known to be safe; it does not make it safe.
- **`filepath.IsLocal` is lexical.** It rejects names that escape lexically, absolute and empty names, and reserved names on Windows, but it cannot see symlinks. Use it to reject bad input early, not instead of `os.Root`.
- **A regex constraint narrows the format, not the location.** `{name:[a-z0-9-]+}` keeps identifiers simple, but a character class that admits `.` also admits `.` and `..`. A constraint describes what the application accepts; it does not confine file access.
- **Do not decode again.** The value is decoded once; a second `PathUnescape` turns `%2F` data into a separator.

`app.Static` and `app.File` apply their own path sanitization; this section concerns handlers that open files themselves. The URL contract is in the [router spec](../specs/router.md#encoded-parameter-values).

---

## QUERY Requests (RFC 10008)

Use QUERY when a safe, idempotent query needs a structured request representation that is impractical in a URL. Register it explicitly; Credo does not infer a QUERY route from GET because their input contracts differ.

```go
type ProductQuery struct {
    Category string `json:"category"`
    Limit    int    `json:"limit"`
}

func (q *ProductQuery) Validate() error {
    return validation.ValidateStruct(q,
        validation.Field(&q.Limit, validation.Between(1, 100)),
    )
}

app.QUERY("/products", func(c *credo.Context) error {
    var query ProductQuery
    if err := c.Request().BindBody(&query); err != nil {
        return err
    }
    return c.Response().JSON(http.StatusOK, searchProducts(query))
})
```

Clients must send a non-blank `Content-Type`, even with an empty body. Missing or blank values return `400 content_type_required` before the application handler; unsupported types handled through `BindBody` return 415. Framework features and `PreRouting`, routed, group and route middleware still run before this guard.

`Accept-Query` is optional capability advertisement, not permission to use QUERY. Set it directly when useful:

```go
c.Response().Header().Set("Accept-Query", "application/json")
```

When every response, including errors, should retain it, use ordinary application middleware:

```go
func AcceptQuery(mediaTypes ...string) credo.Middleware {
    value := strings.Join(mediaTypes, ", ")
    return func(next credo.Handler) credo.Handler {
        return func(c *credo.Context) error {
            c.Response().Header().Set("Accept-Query", value)
            return next(c)
        }
    }
}

app.QUERY("/products", queryProducts).
    Middleware(AcceptQuery("application/json"))
```

Credo does not synthesize OPTIONS. Advertise both method and media type explicitly when an application needs discovery:

```go
app.OPTIONS("/products", func(c *credo.Context) error {
    c.Response().Header().Set("Allow", "OPTIONS, QUERY")
    c.Response().Header().Set("Accept-Query", "application/json")
    return c.Response().NoContent(http.StatusNoContent)
})
```

If the application already uses `middleware.ContractGuard`, `MetaAccept` is the existing generic way to restrict request media types; it is optional and is not QUERY-specific:

```go
app.QUERY("/products", queryProducts).
    SetMeta(middleware.MetaAccept, []string{"application/json"})
```

Verify that every proxy, WAF, CDN, API gateway, and observability pipeline in front of the service passes and records QUERY correctly. A QUERY-aware cache must include request content and relevant metadata in its cache key; use `Cache-Control: no-store` when the deployment chain cannot guarantee that.

---

## Route Groups

Use groups for shared prefixes and middleware:

```go
api := app.Group("/api")
api.Middleware(authMiddleware)

v1 := api.Group("/v1")
v1.GET("/users", listUsers)
v1.POST("/users", createUser)
```

Nested groups inherit parent middleware and meta.

---

## Mounting

Attach a plain `http.Handler` — a stdlib `ServeMux`, a legacy application, or any third-party handler — under a path prefix with `app.Mount()`. The mounted handler receives the request with the prefix stripped, so it routes against its own path space:

```go
admin := http.NewServeMux()
admin.HandleFunc("/dashboard", dashboardHandler) // reached at /admin/dashboard

app.Mount("/admin", admin)
```

A mount answers both its exact prefix (`/admin`) and everything beneath it (`/admin/...`). A root mount forwards the entire path space, including the bare `/`:

```go
app.Mount("/", legacyApp) // every path, "/" included, goes to legacyApp
```

The prefix may carry parameters. The mounted handler gets the path below the matched prefix and reads the parameters as path values:

```go
app.Mount("/t/{tenant}", tenantApp) // /t/acme/reports reaches tenantApp as /reports

// in a mounted stdlib handler
tenant := r.PathValue("tenant") // or credo.URLParam(r, "tenant")

// in a mounted Credo app
tenant := c.Request().PathValue("tenant")
```

In a mounted Credo app `RouteParam` returns that app's own route parameters; the parameters of the mount prefix are reached through `PathValue`. Nested parametric mounts add up: the innermost handler sees the parameters of every prefix on the way down. A catch-all parameter does not belong in a mount pattern — it consumes the whole path, and the handler would always see `/`.

Mounts cover every standard method except `CONNECT` and `TRACE`, which return 405.

A mounted Credo app is an independent App, with its own DI graph and its own components. Hand it to the parent with `parent.Manage(child)`, so that it starts in the parent's start phase, before the parent's listener accepts, and stops after the parent's HTTP drain has finished the requests that reach it:

```go
billing := billingApp() // a *credo.App with its own components
app.Mount("/billing", billing)
app.Manage(billing, credo.ComponentName("billing"))
```

A managed component's default name is its type name, `*credo.App` here, so a parent that manages several children names each with `credo.ComponentName`. Without the `Manage` call, a child App that has anything to start — a component with `Start` or `Ready`, a start hook, a bound database, `UseI18n`, workers — panics on its first request (the parent's recovery turns it into a 500 and logs it).

`parent.Manage(child)` composes Apps that are independent by design, each with its own configuration, container and lifecycle. The modules of one application share one App and its DI graph: give them [groups](#route-groups) and constructors, not an App each.

A mount is a route: its handler runs inside `PreRouting` and the routed tier (`app.Middleware`), and the framework features apply. Group and route middleware do not, because a mount belongs to no group. Guard a mounted sub-app from within it, or register the check with `app.Middleware` ([Middleware Guide](middleware.md#mounted-handlers-get-the-routed-tier-not-group-or-route-middleware)). The mount's prefix parameters reach the sub-app as path values of its request; the mount's route carries none, so a routed middleware reads none through `c.Request().RouteParam`.

In introspection, a mount surfaces as a single `RouteKindMount` entry with its cleaned prefix and the App's meta, never its internal routes.

---

## Host-Based Routing

`app.Host(pattern)` returns a normal `*Group`, but scoped to a host pattern.

### Exact Hosts

```go
api := app.Host("api.example.com")
api.GET("/users", listUsers)

admin := app.Host("admin.example.com")
admin.GET("/dashboard", showDashboard)
```

### Host Parameters

```go
tenant := app.Host("{tenant}.example.com")
tenant.GET("/dashboard", func(c *credo.Context) error {
    slug := c.Request().RouteParam("tenant")
    return c.Response().Text(200, slug)
})
```

### Regex-Constrained Host Labels

```go
org := app.Host("{org:[a-z][a-z0-9-]+}.platform.io")
org.GET("/settings", settingsHandler)
```

### Wildcard Hosts

```go
tenantless := app.Host("*.acme.io")
tenantless.GET("/status", statusHandler)
```

Wildcard `*` matches one anonymous host label and does not add anything to `RouteParams()`. `*.acme.io` matches `api.acme.io`, but not `acme.io` or `a.b.acme.io`.

The wildcard must be the leftmost complete label and may appear only once. Patterns such as `api*.acme.io`, `foo.*.io`, `*.*.acme.io`, `*.{tenant}.acme.io`, and `{tenant}.*.acme.io` panic at registration.

`*` and `*.io` are allowed, but they are broad patterns and are usually best reserved for local development or carefully controlled environments.

Wildcard host patterns are matching-only. `BuildURL` cannot turn `*.acme.io` into a concrete host; use `{tenant}.acme.io` when URL generation needs a subdomain value. Host values fill one label each and are validated (letters, digits, hyphens and underscores, plus the label's constraint) rather than percent-encoded; path values are escaped per segment as described under [Encoded Values](#encoded-values).

### Matching Rules

- Exact host patterns beat regex host patterns.
- Regex host patterns beat plain host params and wildcard labels.
- Plain host params and wildcard labels have equal specificity.
- Unmatched hosts fall back to routes registered directly on `app`.
- Matching is case-insensitive.
- Request ports are stripped before matching.
- Host patterns may not include a port.
- Host patterns with identical match semantics panic at registration time: `{a}.acme.io`, `{b}.acme.io`, and `*.acme.io` are equivalent.

### Important: Shared Param Namespace

Host params and path params share the same `RouteParams()` map.

This is valid:

```go
tenant := app.Host("{tenant}.example.com")
tenant.GET("/users/{id}", showUser)
```

This is invalid and panics at registration time:

```go
tenant := app.Host("{tenant}.example.com")
tenant.GET("/users/{tenant}", showUser)
```

---

## Pre-Dispatch Rewrite

Use `middleware.Rewrite(cfg)` when you want to normalize URLs before routing. The rules live in `RewriteConfig.Rules`; see the [Middleware Guide](middleware.md#rewrite) for the `Skipper` option.

```go
app.PreRouting(middleware.Rewrite(middleware.RewriteConfig{Rules: []middleware.RewriteRule{
    {From: "/v1/{path...}", To: "/api/v1/{path}"},
    {From: "/blog/{slug}", To: "/articles/{slug}", PreserveQuery: true},
}}))
```

### When to Use It

- legacy path migration
- version prefix mapping
- vanity URLs
- host-specific path normalization

### Host Filter

```go
app.PreRouting(middleware.Rewrite(middleware.RewriteConfig{Rules: []middleware.RewriteRule{
    {
        Host: "old.example.com",
        From: "/{path...}",
        To:   "/legacy/{path}",
    },
}}))
```

### Regex Rules

```go
app.PreRouting(middleware.Rewrite(middleware.RewriteConfig{Rules: []middleware.RewriteRule{
    {
        Regexp: regexp.MustCompile(`^/blog/(?P<year>\d{4})/(?P<slug>[^/]+)$`),
        To:     "/posts/{year}/{slug}",
    },
}}))
```

### Query Behavior

- If `To` contains a query string, it replaces the current query string.
- If `To` has no query string and `PreserveQuery` is true, Credo keeps the original query string.

### Encoded Paths

A rule matches the path the way the router does, so an encoded slash is data inside a segment: `{From: "/files/{name}", To: "/blobs/{name}"}` rewrites `/files/a%2Fb` to `/blobs/a%2Fb`, and the `/blobs/{name}` route reads `name` as `a/b`. A rule never matches across an encoded slash: `/files/{dir}/{name}` does not match `/files/a%2Fb`. A `From` parameter matches as a route parameter does, so `{rest:.+}` is still one segment and its constraint sees the decoded value; use `{rest...}`, or a `Regexp` rule, when a capture spans segments. `To` is written in wire form, so `To: "/blobs/a%2Fb"` keeps the escape and a literal `%` is `%25`.

---

## Conditional Forwarding

A path is rewritten before routing or not at all. When the decision needs handler logic — a feature flag, a cookie, the user — call the target's handler yourself:

```go
app.GET("/checkout", func(c *credo.Context) error {
    if useNewCheckout(c) {
        return checkoutV2(c)
    }
    return checkoutV1(c)
})
```

The call runs inside the `/checkout` route, so that route's middleware and meta apply once, and the request keeps the client's URL. Serve a single-page app's client routes with static's SPA mode ([Static Files Guide](static-files.md#spa-mode)), and map a path decided by the path alone with `middleware.Rewrite` before routing.

---

## Original Path and Logging

`c.OriginalPath()` always returns the path that entered the framework before any rewrite happened.

```go
app.GET("/debug", func(c *credo.Context) error {
    return c.Response().JSON(200, map[string]string{
        "original": c.OriginalPath(),
        "current":  c.Request().URL.Path,
    })
})
```

When the final served path differs, Credo logs both:

- `path` — final served path
- `path_original` — client-sent path
- `route` — the matched route's registered pattern (absent for 404/405)

The access log (`app.UseAccessLog`) records both.

---

## Introspection

`app.Routes()` returns every route and every mount as a `RouteInfo`, the same view `c.Route()` gives a request and `app.GetRoute(name)` gives a name: the `Method` and `Pattern`, the route `Name`, the resolved meta (route ← group ← app) through `ri.GetMeta(key)`, `Kind` (route vs. mount), `Host`, `AutoHead` (true for the auto-generated HEAD twin of a GET), and — for a mount — its cleaned prefix plus the forwarded method set in `Methods`. A loop that wants routes only skips the mounts:

```go
for _, ri := range app.Routes() {
    if ri.Kind == credo.RouteKindMount {
        continue // a mount has no Method; its forwarded methods are in ri.Methods
    }
    perm, _ := ri.GetMeta(Permission) // Permission is a credo.Key[string]
    fmt.Println(ri.Method, ri.Host, ri.Pattern, ri.Name, perm)
}
```

The slice is in a deterministic total order (`Host`, `Pattern`, `Method`, `Kind`), so it is a stable source for route/permission catalogs and golden-file tests. Introspection reads live route state, so call it once wiring is complete — not concurrently with route registration.

---

## Testing Handlers

Test a handler through the App, the way production serves it: build a test App with `testutil.NewApp`, register the route under test, and call `app.ServeHTTP` with an `httptest` recorder. The request runs the real pipeline, so route parameters come from the path, a middleware that reads route meta reads the route's own, and an error the handler returns is rendered as in production. Credo exports no router of its own to test against.

```go
func TestShowUser(t *testing.T) {
    app := testutil.NewApp(t)
    app.GET("/users/{id}", showUser)

    rec := httptest.NewRecorder()
    app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/users/42", nil))
    if rec.Code != http.StatusOK {
        t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
    }
}

func TestShowUser_NotFound(t *testing.T) {
    app := testutil.NewApp(t)
    app.GET("/users/{id}", showUser) // returns credo.ErrNotFound for an unknown id

    rec := httptest.NewRecorder()
    app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/users/unknown", nil))

    var body struct {
        Error struct {
            Code string `json:"code"`
        } `json:"error"`
    }
    if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
        t.Fatal(err)
    }
    if rec.Code != http.StatusNotFound || body.Error.Code != "not_found" {
        t.Fatalf("got %d %q, want 404 not_found", rec.Code, body.Error.Code)
    }
}
```

`testutil.NewApp` loads no configuration from disk or the environment: `testutil.WithConfig` sets the values a test needs. A handler that is a method of a resolved service is registered as `main` registers it, after `testutil.WithWiring` has provided its dependencies and `testutil.WithOverride` has replaced the ones the test fakes ([dependency injection guide](dependency-injection.md#testing)). An App with something to start — a component with `Start` or `Ready`, a start hook, workers — refuses `ServeHTTP` until `testutil.Start(t, app)` has run its start phase. Group and route middleware run as registered, so test a protected route through the group that protects it.

---

## Recommended Split of Responsibilities

- Use `app.Host()` for domain isolation.
- Use `app.Group()` for path prefixes and scoped middleware.
- Use `middleware.Rewrite()` for deterministic URL normalization.
- Call the target's handler yourself for a forward that needs handler logic.
- Use `OriginalPath()` when you care about analytics, debugging, or audit logs.
