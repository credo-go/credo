# Routing Guide

This guide explains how to use Credo's routing features in application code:
path routing, route groups, host-based routing, pre-dispatch rewrite, and
handler-driven internal forwarding.

For internal design rationale, see the [Router Spec](../specs/router.md),
[Middleware Spec](../specs/middleware.md), [Context Spec](../specs/context.md),
and [ADR-018](../adr/018-host-routing-and-rewrite.md).

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

    app.GlobalMiddleware(middleware.Rewrite(
        middleware.RewriteRule{From: "/v1/{path...}", To: "/api/v1/{path}"},
    ))

    api := app.Host("api.example.com")
    v1 := api.Group("/api/v1")
    v1.GET("/users/{id}", func(ctx *credo.Context) error {
        return ctx.Response().JSON(200, map[string]string{
            "id": ctx.Request().RouteParam("id"),
        })
    })

    app.GET("/", func(ctx *credo.Context) error {
        return ctx.Response().Text(200, "landing")
    })

    if err := app.Run(); err != nil {
        log.Fatal(err)
    }
}
```

---

## Path Routing

Credo supports three path parameter forms:

| Syntax | Example | Meaning |
|--------|---------|---------|
| `{name}` | `/users/{id}` | single segment |
| `{name:regex}` | `/users/{id:[0-9]+}` | constrained segment |
| `{name...}` | `/files/{path...}` | catch-all remainder |

```go
app.GET("/users/{id}", getUser)
app.GET("/users/{id:[0-9]+}", getNumericUser)
app.GET("/files/{path...}", serveFile)
```

Read a single param with `ctx.Request().RouteParam(name)`:

```go
func getUser(ctx *credo.Context) error {
    id := ctx.Request().RouteParam("id")
    return ctx.Response().Text(200, id)
}
```

`ctx.Request().RouteParams()` returns all params as a `map[string]string`. The
map is owned by the framework and recycled after the request completes, so
prefer `RouteParam` for single values.

Keep dynamic segment names consistent at the same path level. These routes are
valid because the dynamic segment under `/customers/` is named `{id}` in both
patterns:

```go
app.GET("/v1/crm/customers/{id}", showCustomer)
app.GET("/v1/crm/customers/{id}/timeline", customerTimeline)
```

These routes conflict because `{id}` and `{customer_id}` occupy the same path
level:

```go
app.GET("/v1/crm/customers/{id}", showCustomer)
app.GET("/v1/crm/customers/{customer_id}/timeline", customerTimeline) // panics
```

Prefer keeping the route parameter as `{id}` and mapping it to a domain-specific
variable name inside the handler.

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
tenant.GET("/dashboard", func(ctx *credo.Context) error {
    slug := ctx.Request().RouteParam("tenant")
    return ctx.Response().Text(200, slug)
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

Wildcard `*` matches one anonymous host label and does not add anything to
`RouteParams()`. `*.acme.io` matches `api.acme.io`, but not
`acme.io` or `a.b.acme.io`.

The wildcard must be the leftmost complete label and may appear only once.
Patterns such as `api*.acme.io`, `foo.*.io`, `*.*.acme.io`,
`*.{tenant}.acme.io`, and `{tenant}.*.acme.io` panic at registration.

`*` and `*.io` are allowed, but they are broad patterns and are usually best
reserved for local development or carefully controlled environments.

Wildcard host patterns are matching-only. `BuildURL` cannot turn
`*.acme.io` into a concrete host; use `{tenant}.acme.io` when URL
generation needs a subdomain value.

### Matching Rules

- Exact host patterns beat regex host patterns.
- Regex host patterns beat plain host params and wildcard labels.
- Plain host params and wildcard labels have equal specificity.
- Unmatched hosts fall back to routes registered directly on `app`.
- Matching is case-insensitive.
- Request ports are stripped before matching.
- Host patterns may not include a port.
- Host patterns with identical match semantics panic at registration time:
  `{a}.acme.io`, `{b}.acme.io`, and `*.acme.io` are equivalent.

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

Use `middleware.Rewrite(...)` when you want to normalize URLs before routing.

```go
app.GlobalMiddleware(middleware.Rewrite(
    middleware.RewriteRule{From: "/v1/{path...}", To: "/api/v1/{path}"},
    middleware.RewriteRule{From: "/blog/{slug}", To: "/articles/{slug}", PreserveQuery: true},
))
```

### When to Use It

- legacy path migration
- version prefix mapping
- vanity URLs
- host-specific path normalization

### Host Filter

```go
app.GlobalMiddleware(middleware.Rewrite(
    middleware.RewriteRule{
        Host: "old.example.com",
        From: "/{path...}",
        To:   "/legacy/{path}",
    },
))
```

### Regex Rules

```go
app.GlobalMiddleware(middleware.Rewrite(
    middleware.RewriteRule{
        Regexp: regexp.MustCompile(`^/blog/(?P<year>\d{4})/(?P<slug>[^/]+)$`),
        To:     "/posts/{year}/{slug}",
    },
))
```

### Query Behavior

- If `To` contains a query string, it replaces the current query string.
- If `To` has no query string and `PreserveQuery` is true, Credo keeps the
  original query string.

---

## Handler-Level Internal Forwarding

Use `ctx.Rewrite()` when the decision depends on handler logic.

```go
app.GET("/checkout", func(ctx *credo.Context) error {
    if useNewCheckout(ctx) {
        return ctx.Rewrite("/checkout/v2")
    }
    return ctx.Rewrite("/checkout/v1")
})
```

### When to Use It

- A/B routing
- SPA fallback
- custom error-page routing
- conditional forwarding based on cookies, auth, or database state

### Rules

- Return it directly: `return ctx.Rewrite("/new")`
- The target must start with `/`
- A request can rewrite at most 10 times
- Re-dispatch stays in the same host scope
- Group and route middleware run again for the rewritten route
- Built-in and global middleware do not run again

---

## Original Path and Logging

`ctx.OriginalPath()` always returns the path that entered the framework before
any rewrite happened.

```go
app.GET("/debug", func(ctx *credo.Context) error {
    return ctx.Response().JSON(200, map[string]string{
        "original": ctx.OriginalPath(),
        "current":  ctx.Request().URL.Path,
    })
})
```

When the final served path differs, Credo logs both:

- `path` — final served path
- `path_original` — client-sent path

This works in the built-in access log and in `middleware.AccessLog`.

---

## Middleware Gotchas with `ctx.Rewrite()`

If a handler calls `ctx.Rewrite()`, group and route middleware for the target
route execute again.

That means `after` logic is per dispatch round, not per whole request:

```go
func Timer(next credo.Handler) credo.Handler {
    return func(ctx *credo.Context) error {
        start := time.Now()
        err := next(ctx)
        if ctx.IsRewriting() {
            return err
        }
        slog.Info("route duration", "duration", time.Since(start))
        return err
    }
}
```

Use app/global middleware when you want one measurement for the whole request.

---

## Introspection

Use `Walk` for simple method/pattern traversal and `WalkRoutes` when you also
need the host pattern.

```go
credo.Walk(app.Mux(), func(method, pattern string) error {
    fmt.Println(method, pattern)
    return nil
})

credo.WalkRoutes(app.Mux(), func(ri credo.RouteInfo) error {
    fmt.Println(ri.Method, ri.Host, ri.Pattern)
    return nil
})
```

---

## Recommended Split of Responsibilities

- Use `app.Host()` for domain isolation.
- Use `app.Group()` for path prefixes and scoped middleware.
- Use `middleware.Rewrite()` for deterministic URL normalization.
- Use `ctx.Rewrite()` for conditional internal forwards.
- Use `OriginalPath()` when you care about analytics, debugging, or audit logs.
