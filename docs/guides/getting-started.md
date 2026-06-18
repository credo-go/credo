# Getting Started

This guide walks through building a Credo application from scratch. By the end, you will have a working HTTP server with routing, middleware, dependency injection, validation, health checks, error handling, graceful shutdown, and clear extension points for background work.

For deeper coverage of individual topics, see the linked guides and specs.

---

## Installation

```bash
go get github.com/credo-go/credo@latest
```

Requires Go 1.26 or later.

---

## Hello, Credo

The smallest useful Credo application:

```go
package main

import (
    "log"

    "github.com/credo-go/credo"
)

func main() {
    app, err := credo.New()
    if err != nil {
        log.Fatal(err)
    }

    app.GET("/", func(ctx *credo.Context) error {
        return ctx.Response().JSON(200, map[string]string{
            "message": "Hello, Credo!",
        })
    })

    if err := app.Run(); err != nil {
        log.Fatal(err)
    }
}
```

`credo.New()` auto-loads configuration, creates a DI container, and sets up defaults. `Run()` binds the port and blocks until shutdown.

---

## Core Concepts

Before going further, a few definitions:

- **Handler**: `func(*credo.Context) error` — every handler returns an error. Returning `nil` means success; returning an error triggers the centralized error handler.
- **Context**: request-scoped struct with `Request()` and `Response()` accessors. Pooled for zero allocation.
- **Middleware**: `func(credo.Handler) credo.Handler` — wraps handlers.
- **Route**: returned by `app.GET(...)` etc. Fluent API for naming, metadata, and per-route middleware.

---

## Configuration

Credo auto-discovers `config.json` / `config.yaml` in the working directory, merges `.env` and `CREDO_*` environment variables on top.

```json
{
  "server": {
    "host": "0.0.0.0",
    "port": 8080,
    "read_timeout": "30s",
    "write_timeout": "30s"
  },
  "app": {
    "name": "my-service",
    "debug": false
  }
}
```

Access config by unmarshalling into typed structs at the module boundary:

```go
type AppConfig struct {
    Name  string `credo:"name"`
    Debug bool   `credo:"debug"`
}

rc := credo.MustResolve[credo.RawConfig](app)

var cfg AppConfig
if err := rc.Unmarshal("app", &cfg); err != nil {
    log.Fatal(err)
}

// Register in DI so services can receive it.
credo.MustProvideValue(app, &cfg)
```

String keys appear once. After that, everything is typed.

For explicit file selection, call `config.Load(config.WithFiles(...))` yourself and pass the result with `credo.WithRawConfig(raw)`. Passing `WithRawConfig` bypasses `credo.New()`'s auto-load path.

See [Configuration Guide](configuration.md) for env-specific files, `.env` loading, and environment variable conventions.

---

## Routing

Register routes with HTTP method shortcuts:

```go
app.GET("/users", listUsers)
app.POST("/users", createUser)
app.GET("/users/{id}", getUser)
app.PUT("/users/{id}", updateUser)
app.DELETE("/users/{id}", deleteUser)
```

### Path Parameters

`{name}` captures a segment. `{name:[0-9]+}` adds a regex constraint. `{path...}` is a catch-all.

```go
app.GET("/files/{path...}", func(ctx *credo.Context) error {
    filePath := ctx.Request().RouteParam("path")
    return ctx.Response().Text(200, "file: "+filePath)
})
```

### Named Routes

```go
app.GET("/users/{id}", getUser).Name("users.show")
```

### Route Groups

Groups share a prefix and middleware:

```go
api := app.Group("/api/v1")

api.GET("/users", listUsers)
api.POST("/users", createUser)

admin := api.Group("/admin")
admin.Middleware(requireAdmin)
admin.GET("/stats", adminStats)
```

### Host-Based Groups and Rewrite

Credo can route by host as well as path:

```go
api := app.Host("api.example.com")
api.GET("/users", listUsers)

tenant := app.Host("{tenant}.example.com")
tenant.GET("/dashboard", tenantDashboard)
```

You can also normalize URLs before routing:

```go
app.GlobalMiddleware(middleware.Rewrite(
    middleware.RewriteRule{From: "/v1/{path...}", To: "/api/v1/{path}"},
))
```

For conditional handler-driven forwards, use `ctx.Rewrite("/new-path")`. See the [Routing Guide](routing.md) for host patterns, rewrite semantics, and `OriginalPath()`.

---

## Middleware

Middleware wraps handlers. Three tiers, evaluated in order:

1. **Global** — every request (including 404/405)
2. **Group** — routes under that group
3. **Route** — single route

```go
// Global middleware you add yourself.
// Request IDs, access logging, and panic recovery are already built in.
app.GlobalMiddleware(
    middleware.CORS(),
    middleware.Secure(),
)

// Group
api := app.Group("/api")
api.Middleware(authMiddleware)

// Route
app.GET("/admin", adminHandler).Middleware(requireAdmin)
```

Credo includes built-in request IDs, access logging, and panic recovery (`WithoutRequestID()`, `WithoutAccessLog()`, `WithoutRecover()`). Additional middleware: `CORS`, `Secure`, `Compress`, `Timeout`, `RateLimit`. Use `middleware.RequestID()` / `middleware.AccessLog()` when you disable the built-ins and need custom configuration. `middleware.Recover()` is available for per-group/route custom recovery config.

See the [Middleware Guide](middleware.md) for the full list, configuration options, and custom middleware patterns.

---

## Request and Response

### Reading Input

```go
func createUser(ctx *credo.Context) error {
    // Path parameters
    id := ctx.Request().RouteParam("id")

    // Query parameters
    page := ctx.Request().QueryParam("page")

    // JSON body (auto-validates if struct implements Validatable)
    var req CreateUserRequest
    if err := ctx.Request().BindBody(&req); err != nil {
        return err // validation errors become RFC 7807 response
    }

    _ = id
    _ = page
    return ctx.Response().JSON(201, req)
}
```

### Writing Output

```go
ctx.Response().JSON(200, data)           // application/json
ctx.Response().Text(200, "hello")        // text/plain
ctx.Response().HTML(200, "<h1>Hi</h1>")  // text/html
ctx.Response().XML(200, data)            // application/xml
ctx.Response().NoContent(204)            // no body
ctx.Response().Redirect(302, "/login")   // redirect
```

---

## Validation

Credo uses programmatic validation — no struct tags. Implement the `Validatable` interface and `BindBody` calls it automatically.

> **Tip:** Enable `WithDebug()` or `server.debug: true` to get a warning when a bind target does not implement `Validatable`.

```go
type CreateUserRequest struct {
    Name  string `json:"name"`
    Email string `json:"email"`
    Age   int    `json:"age"`
}

func (r *CreateUserRequest) Validate() error {
    return validation.ValidateStruct(r,
        validation.Field(&r.Name, validation.Required[string](), validation.Length(2, 100)),
        validation.Field(&r.Email, validation.Required[string](), validation.Email()),
        validation.Field(&r.Age, validation.Min(0), validation.Max(150)),
    )
}
```

Validation errors are automatically converted to RFC 7807 Problem Details with a 422 status code:

```json
{
  "type": "https://credo.dev/errors/validation",
  "title": "Validation Failed",
  "status": 422,
  "errors": [
    {"field": "email", "code": "email", "message": "must be a valid email address"}
  ]
}
```

See [Validation Spec](../specs/validation.md) for the full rule catalog.

---

## Error Handling

Handlers return errors. The internal error handling pipeline converts them to RFC 7807 JSON responses:

```go
func getUser(ctx *credo.Context) error {
    user, err := svc.FindByID(ctx.Context(), ctx.Request().RouteParam("id"))
    if err != nil {
        // Wrap with HTTP status
        return credo.NewHTTPError(404, "user.not_found").WithInternal(err)
    }
    return ctx.Response().JSON(200, user)
}
```

Use sentinel errors for common cases:

```go
return credo.ErrNotFound        // 404
return credo.ErrUnauthorized    // 401
return credo.ErrForbidden       // 403
return credo.ErrBadRequest      // 400
```

Sentinel errors use built-in MsgKey constants (e.g., `"http.not_found"`). Custom keys are supported: `credo.NewHTTPError(404, "user.not_found")`. When i18n is configured, MessageKey is used as the translation key.

Internal errors (5xx) are logged but never leaked to the client.

You can replace the error renderer to customize the response format:

```go
app.SetErrorRenderer(func(ctx *credo.Context, info credo.ErrorInfo) {
    // info.Err — original error (for Sentry, errors.As, custom headers)
    // info.MessageKey — i18n key (for telemetry, client-side i18n)
    // info.Problem — classified *ProblemDetails (status, title, errors)
    ctx.Response().JSON(info.Problem.Status, myFormat(info))
})
```

---

## Dependency Injection

Credo's DI is singleton-only with constructor injection:

```go
// 1. Define a service
type UserService struct {
    infra credo.Infra
    repo  *UserRepository
}

func NewUserService(infra credo.Infra, repo *UserRepository) *UserService {
    infra.Logger.Info("UserService initialized")
    return &UserService{infra: infra, repo: repo}
}

// 2. Register
credo.MustProvide[*UserRepository](app, NewUserRepository)
credo.MustProvide[*UserService](app, NewUserService)

// 3. Finalize (catches missing deps, cycles)
if err := credo.Finalize(app); err != nil {
    log.Fatal(err)
}

// 4. Resolve for route wiring
svc := credo.MustResolve[*UserService](app)
app.GET("/users/{id}", svc.GetUser)
```

`credo.Infra` is injected automatically. Today it carries a service-scoped `Logger`; tracing and metrics carriers are planned for the observability release.

See [Dependency Injection Guide](dependency-injection.md) for `Alias`, `BindMany`/`ResolveAll`, `ProvideValue`, testing patterns, and the full mental model.

---

## Health Checks

One call enables K8s-compatible liveness and readiness probes:

```go
app.UseHealth()
```

This registers `GET /health` (liveness) and `GET /ready` (readiness).

### Custom Checks

```go
app.UseHealth()

app.AddLivenessCheck("disk", credo.HealthCheckFunc(func(ctx context.Context) error {
    // return non-nil to signal "down"
    return checkDiskSpace()
}))

app.AddReadinessCheck("cache", credo.HealthCheckFunc(func(ctx context.Context) error {
    return cache.Ping(ctx)
}))
```

### Configuration

```go
app.UseHealth(credo.HealthConfig{
    LivenessPath:  "/livez",       // default: "/health"
    ReadinessPath: "/readyz",      // default: "/ready"
    CheckTimeout:  3 * time.Second, // default: 5s
})
```

Register health routes on a specific group to apply shared middleware (e.g., IP restriction) and a path prefix:

```go
ops := app.Group("/-").Middleware(ipRestrict)
app.UseHealth(credo.HealthConfig{
    Group:         ops,
    LivenessPath:  "/healthz",  // becomes /-/healthz
    ReadinessPath: "/ready",    // becomes /-/ready
})
```

Toggle endpoints individually:

```go
app.UseHealth(credo.HealthConfig{
    Liveness:  boolPtr(true),   // nil = true (default)
    Readiness: boolPtr(false),  // disable /ready
})
```

### Store Integration

When using `store.Register`, store health is automatically wired into the readiness endpoint. No extra code needed — registered stores appear in the `/ready` response:

```json
{
  "status": "up",
  "checks": {
    "postgres": {"status": "up", "latency": "1.234ms"}
  }
}
```

For multi-database wiring and transaction behavior, see the [Data Access Guide](data-access.md).

### Response Codes

- `200` with `{"status": "up"}` when all checks pass
- `503` with `{"status": "down"}` when any check fails

---

## Lifecycle Hooks

Credo provides `OnStart` and `OnShutdown` hooks for startup and shutdown logic:

```go
func main() {
    app, _ := credo.New()

    app.OnStart(func(ctx context.Context) error {
        log.Println("server ready on", app.Addr())
        return nil
    })

    app.OnShutdown(func(ctx context.Context) error {
        log.Println("cleaning up...")
        return nil
    })

    // Run blocks until SIGINT/SIGTERM, then drains gracefully (default 30s;
    // pass credo.WithShutdownTimeout to New to change the budget).
    if err := app.Run(); err != nil {
        log.Fatal(err)
    }
}
```

OnStart hooks run after the port is bound (FIFO order). If any hook fails, the server does not start. `app.Addr()` is available inside hooks — useful when using port 0.

For full control over signal handling — a custom signal set, or coordinating shutdown across several servers — use `RunContext`, which installs **no** signal handler of its own. Cancel the context to trigger the same graceful drain (bounded by `WithShutdownTimeout`):

```go
func main() {
    app, _ := credo.New()

    // The caller owns signals here. RunContext drains when ctx is cancelled;
    // unlike Run it never installs its own handler.
    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    if err := app.RunContext(ctx); err != nil {
        log.Fatal(err)
    }
}
```

For programmatic shutdown — a test, or an admin endpoint — call `app.Shutdown(ctx)` from another goroutine; it runs the same drain and honours the deadline on the `ctx` you pass.

Shutdown sequence:

1. Readiness flips to 503 (`/ready`) so load balancers stop routing — liveness (`/health`) stays up, since the process is alive and draining
2. Cancel app context (signals background services)
3. Drain in-flight HTTP requests
4. DI Container shutdown (reverse-order singleton cleanup)
5. OnShutdown hooks (LIFO)

Services that implement `credo.Shutdowner` are cleaned up automatically by the DI container. For components **not** managed by DI, use `app.OnShutdown(fn)` instead. See the [Dependency Injection guide](dependency-injection.md#shutdown-and-lifecycle) for a detailed comparison.

If you need managed background tasks, use `worker.Register(...)` instead of manually starting goroutines in `main()`. Registered workers receive the app shutdown signal automatically and the worker pool waits for them during shutdown. See the [Worker Guide](worker.md).

---

## Putting It Together

A minimal but realistic application:

```go
package main

import (
    "log"
    "net/http"

    "github.com/credo-go/credo"
    "github.com/credo-go/credo/middleware"
    "github.com/credo-go/credo/validation"
)

type CreateItemRequest struct {
    Name  string `json:"name"`
    Price int    `json:"price"`
}

func (r *CreateItemRequest) Validate() error {
    return validation.ValidateStruct(r,
        validation.Field(&r.Name, validation.Required[string](), validation.Length(1, 200)),
        validation.Field(&r.Price, validation.Min(0)),
    )
}

type ItemService struct {
    infra credo.Infra
}

func NewItemService(infra credo.Infra) *ItemService {
    return &ItemService{infra: infra}
}

func (s *ItemService) Create(ctx *credo.Context) error {
    var req CreateItemRequest
    if err := ctx.Request().BindBody(&req); err != nil {
        return err
    }
    s.infra.Logger.Info("item created", "name", req.Name)
    return ctx.Response().JSON(http.StatusCreated, req)
}

func (s *ItemService) List(ctx *credo.Context) error {
    return ctx.Response().JSON(http.StatusOK, []string{"item1", "item2"})
}

func main() {
    app, err := credo.New()
    if err != nil {
        log.Fatal(err)
    }

    // DI
    credo.MustProvide[*ItemService](app, NewItemService)
    if err := credo.Finalize(app); err != nil {
        log.Fatal(err)
    }
    svc := credo.MustResolve[*ItemService](app)

    // Global middleware you add yourself.
    // Request IDs, access logging, and panic recovery are already built in.
    app.GlobalMiddleware(
        middleware.CORS(),
        middleware.Secure(),
    )

    // Health
    app.UseHealth()

    // Routes
    app.GET("/items", svc.List)
    app.POST("/items", svc.Create)

    // Run blocks until SIGINT/SIGTERM, then drains gracefully.
    if err := app.Run(); err != nil {
        log.Fatal(err)
    }
}
```

---

## What's Next

- [Configuration Guide](configuration.md) — env files, typed config, env vars
- [Dependency Injection Guide](dependency-injection.md) — Alias, Infra, testing
- [Data Access Guide](data-access.md) — store.Register, transactions, multi-DB
- [Routing Guide](routing.md) — host groups, rewrite middleware, internal forwarding
- [Worker Guide](worker.md) — continuous workers, schedules, shutdown, status snapshots
- [Localization Guide](localization.md) — locale detection, translation
- [Middleware Guide](middleware.md) — 3-tier model, built-in middleware, custom middleware
- [Validation Spec](../specs/validation.md) — rule catalog, custom rules
- [Router Spec](../specs/router.md) — regex constraints, named routes, URL generation
