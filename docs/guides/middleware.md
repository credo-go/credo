# Middleware Guide

This guide explains how to use, configure, and write middleware in Credo. For internal design rationale, see the [Middleware Spec](../specs/middleware.md) and [ADR-010](../adr/010-middleware-architecture.md).

---

## Quick Start

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

    // Built-in: panic recovery, request ID, and access logging are
    // auto-enabled — no middleware registration needed for these.

    app.GET("/", func(ctx *credo.Context) error {
        return ctx.Response().Text(200, "ok")
    })

    if err := app.Run(); err != nil {
        log.Fatal(err)
    }
}
```

---

## The Middleware Type

Credo middleware has a single type:

```go
type Middleware func(next credo.Handler) Handler
```

A middleware receives the next handler in the chain, wraps it, and returns a new handler. This is the classic "onion" model: the outermost middleware runs first on the way in and last on the way out.

```go
func Timer(next credo.Handler) credo.Handler {
    return func(ctx *credo.Context) error {
        start := time.Now()
        err := next(ctx)
        duration := time.Since(start)
        ctx.Response().Header().Set("X-Duration", duration.String())
        return err
    }
}
```

---

## The 3-Tier Model

Credo provides three tiers for middleware registration. Each tier controls a different scope:

| Tier | Registration | Scope | Runs on 404/405? |
| --- | --- | --- | --- |
| **Global** | `app.GlobalMiddleware(m...)` | Every request | Yes |
| **Group** | `group.Middleware(m...)` | Routes under that group | No |
| **Route** | `route.Middleware(m...)` | Single route only | No |

### Global Middleware

Global middleware runs on **every** request, including unmatched routes (404) and method-not-allowed (405). Use it for cross-cutting concerns that must always execute.

```go
// Built-in: recover, requestID, access log — already active.
// Add extra global middleware:
app.GlobalMiddleware(
    middleware.CORS(),
    middleware.Secure(),
)
```

Without a global tier, a 404 response would bypass CORS headers and compression. The global tier closes that gap. (Request ID, access logging, and panic recovery are built-in and always active unless opted out via `WithoutRequestID()`, `WithoutAccessLog()`, `WithoutRecover()`.)

### Group Middleware

Group middleware runs only on routes within that group. Apply it after creating the group:

```go
api := app.Group("/api/v1")
api.Middleware(authMiddleware)

api.GET("/users", listUsers)       // authMiddleware runs
api.POST("/users", createUser)     // authMiddleware runs

public := app.Group("/public")
public.GET("/status", statusCheck) // authMiddleware does NOT run
```

Group middleware is inherited by sub-groups:

```go
api := app.Group("/api")
api.Middleware(authMiddleware)

admin := api.Group("/admin")
admin.Middleware(requireAdmin)

// /api/admin/stats runs: authMiddleware -> requireAdmin -> handler
admin.GET("/stats", adminStats)
```

### Route Middleware

Route middleware applies to a single route. Use the fluent API:

```go
app.GET("/admin/dashboard", adminDashboard).
    Middleware(requireAdmin, auditLog)
```

### Execution Order

```
Request
  -> Global middleware (outer to inner)
    -> Group middleware (outer to inner, parent to child)
      -> Route middleware (outer to inner)
        -> Handler
      <- Route middleware
    <- Group middleware
  <- Global middleware
Response
```

All middleware chains are precompiled at startup. There is no per-request allocation for middleware dispatch.

---

## Writing Custom Middleware

### Basic Pattern

A middleware is any function matching `func(credo.Handler) credo.Handler`:

```go
func RequestTimer(next credo.Handler) credo.Handler {
    return func(ctx *credo.Context) error {
        start := time.Now()

        err := next(ctx)

        duration := time.Since(start)
        slog.Info("request duration",
            "path", ctx.Request().URL.Path,
            "duration", duration,
        )

        return err
    }
}

app.GlobalMiddleware(RequestTimer)
```

### Pre-Handler Logic (Before)

Run code before the handler executes:

```go
func RequireJSON(next credo.Handler) credo.Handler {
    return func(ctx *credo.Context) error {
        ct := ctx.Request().Header.Get("Content-Type")
        if ct != "" && !strings.HasPrefix(ct, "application/json") {
            return credo.NewHTTPError(415, "http.unsupported_media_type")
        }
        return next(ctx)
    }
}
```

### Post-Handler Logic (After)

Run code after the handler executes:

```go
func AddServerHeader(next credo.Handler) credo.Handler {
    return func(ctx *credo.Context) error {
        err := next(ctx)
        ctx.Response().Header().Set("X-Powered-By", "Credo")
        return err
    }
}
```

If handlers can call `ctx.Rewrite()`, remember that group and route middleware may run again for the rewritten route. Use `ctx.IsRewriting()` when your post-handler logic should skip side effects during an internal forward.

### Short-Circuiting

Return an error or write a response without calling `next` to stop the chain:

```go
func MaintenanceMode(next credo.Handler) credo.Handler {
    return func(ctx *credo.Context) error {
        if isUnderMaintenance() {
            return credo.NewHTTPError(503, "http.service_unavailable")
        }
        return next(ctx)
    }
}
```

### Error Handling in Middleware

Middleware can inspect and transform errors returned by downstream handlers:

```go
func ErrorEnricher(next credo.Handler) credo.Handler {
    return func(ctx *credo.Context) error {
        err := next(ctx)
        if err != nil {
            slog.Error("handler failed",
                "path", ctx.Request().URL.Path,
                "error", err,
            )
        }
        return err
    }
}
```

Always propagate the error unless you have a specific reason to swallow it. Credo's error pipeline handles classification and response formatting.

---

## Config Struct Pattern

Middleware with options uses an optional config parameter:

- `Middleware()` — creates the middleware with sensible defaults
- `Middleware(cfg)` — creates the middleware with custom options

```go
// Default config
app.GlobalMiddleware(middleware.CORS())

// Custom config
app.GlobalMiddleware(middleware.CORS(middleware.CORSConfig{
    AllowOrigins: []string{"https://example.com", "https://app.example.com"},
    AllowMethods: []string{"GET", "POST", "PUT", "DELETE"},
    AllowHeaders: []string{"Authorization", "Content-Type"},
    MaxAge:       3600,
}))
```

---

## Built-in Middleware (Auto-Enabled)

Credo auto-enables three built-in middleware with zero configuration. They form the outermost layer of every request:

```
builtinRequestID → builtinAccessLog → builtinRecover → builtinErrorHandler → globalMW → dispatch
```

| Built-in | What it does | Opt-out |
| --- | --- | --- |
| **Recover** | Catches panics, logs stack trace, returns 500 | `WithoutRecover()` |
| **RequestID** | Sets `X-Request-Id` header, enriches `ctx.Logger()` | `WithoutRequestID()` |
| **AccessLog** | Logs method, path, status, bytes, duration | `WithoutAccessLog()` |

The built-in RequestID enriches `ctx.Logger()` with a `request_id` attribute. All downstream logging (including the access log and panic recovery) automatically includes the request ID.

### Silencing the Built-in Access Log

You rarely need to disable the access logger to quiet noisy traffic — two filters keep it on while skipping selected requests:

- **A skipper predicate.** `credo.WithAccessLogSkipper(func(*credo.Context) bool)` runs before routing, so decide from request-level data (path, headers). Return `true` to skip:

```go
app, _ := credo.New(credo.WithAccessLogSkipper(func(ctx *credo.Context) bool {
    return ctx.Request().URL.Path == "/metrics"
}))
```

- **Route or group meta.** Set `credo.MetaAccessLog` to `false` on a route, or on a whole group through inheritance:

```go
app.GET("/metrics", metricsHandler).SetMeta(credo.MetaAccessLog, false)

internal := app.Group("/internal")
internal.SetMeta(credo.MetaAccessLog, false)                          // silence everything under /internal
internal.GET("/audit", auditHandler).SetMeta(credo.MetaAccessLog, true) // ...except this one
```

A route-level value overrides its group's, so you can silence a group and keep one route loud. Only a bool `false` silences; any other value is ignored (the request is logged). The same meta is honoured by `middleware.AccessLog`. Health probes (`/health`, `/ready`) are silent by default — re-enable them with `HealthConfig{LogRequests: true}`.

### Using Custom Configuration

When you need custom headers, custom generators, or access-log skippers, disable the built-in and use the `middleware` package version:

```go
app, _ := credo.New(
    credo.WithoutRequestID(),     // disable built-in
    credo.WithoutAccessLog(), // disable built-in
)
app.GlobalMiddleware(
    middleware.RequestID(middleware.RequestIDConfig{
        Header: "X-Correlation-Id",
        Generator: func() string { return uuid.NewString() },
    }),
    middleware.AccessLog(middleware.AccessLogConfig{
        Skipper: func(ctx *credo.Context) bool {
            return ctx.Request().URL.Path == "/health"
        },
    }),
)
```

> **Important:** Do not use both the built-in and middleware versions simultaneously — this produces duplicate headers and log entries.

---

## Configurable Middleware (middleware package)

### Recover

> **Note:** Panic recovery is built into the framework by default. The `middleware.Recover()` middleware is only needed when you want per-group or per-route recovery with custom configuration (e.g., a custom logger, stack size control, or disabled stack traces). To disable built-in recovery entirely, use `credo.WithoutRecover()`.

Recovers from panics, logs the panic with a stack trace, and returns a 500 error through the error pipeline.

```go
// Per-group recovery with custom config
api.Middleware(middleware.Recover(middleware.RecoverConfig{
    Logger:            myLogger,
    DisableStackTrace: false,
    StackSize:         8192,  // max bytes for stack trace
}))
```

`http.ErrAbortHandler` is re-panicked to allow the HTTP server to abort the connection.

### RequestID

> **Note:** Request ID injection is built into the framework by default. The `middleware.RequestID()` middleware is only needed when you want custom configuration (e.g., different header name, custom generator, different length limit). Disable the built-in first with `credo.WithoutRequestID()`.

Injects a unique request ID into each request and response. If the client sends an `X-Request-Id` header (within the length limit), that value is reused. Otherwise, a new 128-bit cryptographic ID is generated.

Read the request ID downstream (works with both built-in and middleware):

```go
func myHandler(ctx *credo.Context) error {
    reqID := ctx.RequestID()
    slog.Info("processing", "request_id", reqID)
    return ctx.Response().JSON(200, map[string]string{"id": reqID})
}
```

If you prefer the helper function, `middleware.GetRequestID(ctx)` returns the same value.

With custom config:

```go
app.GlobalMiddleware(middleware.RequestID(middleware.RequestIDConfig{
    Header:    "X-Correlation-Id",
    Limit:     128,
    Generator: func() string { return uuid.NewString() },
}))
```

### AccessLog

> **Note:** Access logging is built into the framework by default. The `middleware.AccessLog()` middleware is only needed when you want custom configuration (e.g., a Skipper function, custom logger). Disable the built-in first with `credo.WithoutAccessLog()`.

Logs each request with structured attributes: method, path, status, bytes, duration, real client address, user agent, and request ID (when RequestID middleware is active). Log level varies by status: 2xx/3xx = Info, 4xx = Warn, 5xx = Error.

Skip logging for specific routes:

```go
app.GlobalMiddleware(middleware.AccessLog(middleware.AccessLogConfig{
    Skipper: func(ctx *credo.Context) bool {
        return ctx.Request().URL.Path == "/health"
    },
}))
```

The `credo.MetaAccessLog` route meta (`SetMeta(credo.MetaAccessLog, false)`) also silences logging here, exactly as it does for the built-in logger — useful for muting a route or group without a path check.

When the final served path differs from the client path because of `middleware.Rewrite()` or `ctx.Rewrite()`, Credo includes `path_original` in the log entry.

### Rewrite

`middleware.Rewrite(...)` is Credo's pre-dispatch path rewrite middleware.

```go
app.GlobalMiddleware(middleware.Rewrite(
    middleware.RewriteRule{From: "/v1/{path...}", To: "/api/v1/{path}"},
    middleware.RewriteRule{Host: "old.example.com", From: "/{path...}", To: "/legacy/{path}"},
))
```

Use it when you want routing to see a normalized path on the first lookup. For conditional handler-driven forwarding, use `ctx.Rewrite()` instead.

See the [Routing Guide](routing.md) for host routing, rewrite patterns, `OriginalPath()`, and rewrite-specific middleware caveats.

### CORS

Handles Cross-Origin Resource Sharing preflight and actual requests.

```go
// Allow all origins (default)
app.GlobalMiddleware(middleware.CORS())

// Restrict to specific origins
app.GlobalMiddleware(middleware.CORS(middleware.CORSConfig{
    AllowOrigins:     []string{"https://example.com"},
    AllowMethods:     []string{"GET", "POST", "PUT", "DELETE"},
    AllowHeaders:     []string{"Authorization", "Content-Type"},
    AllowCredentials: true,
    ExposeHeaders:    []string{"X-Total-Count"},
    MaxAge:           86400,
}))
```

Wildcard subdomains are supported:

```go
middleware.CORS(middleware.CORSConfig{
    AllowOrigins: []string{"https://*.example.com"},
})
```

For dynamic origin validation:

```go
middleware.CORS(middleware.CORSConfig{
    AllowOriginFunc: func(ctx *credo.Context, origin string) (string, bool, error) {
        allowed, err := db.IsAllowedOrigin(origin)
        if err != nil {
            return "", false, err
        }
        return origin, allowed, nil
    },
})
```

### CSRF

Rejects state-changing cross-origin browser requests — no tokens, cookies, or session state. Wraps the standard library's `net/http.CrossOriginProtection`, which detects cross-origin requests via the `Sec-Fetch-Site` header (all modern browsers) with an Origin/Host fallback for older ones.

```go
// Zero config: blocks cross-origin POST/PUT/PATCH/DELETE
app.GlobalMiddleware(middleware.CSRF())

// Frontend on another origin + webhook endpoints
app.GlobalMiddleware(middleware.CSRF(middleware.CSRFConfig{
    TrustedOrigins:         []string{"https://app.example.com"},
    InsecureBypassPatterns: []string{"/webhooks/"},
}))
```

What passes without configuration:

- `GET`, `HEAD`, `OPTIONS` (safe methods — never change state in them)
- same-origin browser requests
- non-browser clients (curl, server-to-server, mobile SDKs) — requests without `Sec-Fetch-Site`/`Origin` headers are allowed

**Subdomains are cross-origin.** A form on `app.example.com` posting to `api.example.com` is rejected (browsers send `Sec-Fetch-Site: same-site`) unless the frontend origin is listed in `TrustedOrigins`.

Rejections return a 403 Problem Details response through the framework error pipeline; the detector's reason is logged but never exposed. Override with `ErrorHandler`:

```go
middleware.CSRF(middleware.CSRFConfig{
    ErrorHandler: func(ctx *credo.Context, err error) error {
        ctx.Logger().Warn("csrf rejected", "origin", ctx.Request().Header.Get("Origin"))
        return credo.NewHTTPError(http.StatusForbidden)
    },
})
```

CSRF and CORS are complementary: CORS controls whether a browser may _read_ a cross-origin response; CSRF stops cross-origin state changes from being _processed_. A browser frontend on another origin typically needs its origin in both `CORSConfig.AllowOrigins` and `CSRFConfig.TrustedOrigins`.

### Compress

Compresses responses using gzip or deflate based on the client's `Accept-Encoding` header. Only compresses textual content types by default.

```go
app.GlobalMiddleware(middleware.Compress())
```

Custom compression level and content types:

```go
app.GlobalMiddleware(middleware.Compress(middleware.CompressConfig{
    Level: 9,  // max compression
    Types: []string{
        "application/json",
        "text/*",  // wildcard supported
    },
}))
```

### Secure

Sets common security headers: `X-XSS-Protection`, `X-Content-Type-Options`, `X-Frame-Options`, `Strict-Transport-Security`, `Content-Security-Policy`, and `Referrer-Policy`.

```go
app.GlobalMiddleware(middleware.Secure())
```

Full configuration:

```go
app.GlobalMiddleware(middleware.Secure(middleware.SecureConfig{
    XSSProtection:         "1; mode=block",
    ContentTypeNosniff:    "nosniff",
    XFrameOptions:         "DENY",
    HSTSMaxAge:            31536000,
    HSTSPreloadEnabled:    true,
    HSTSExcludeSubdomains: false,
    ContentSecurityPolicy: "default-src 'self'",
    CSPReportOnly:         false,
    ReferrerPolicy:        "strict-origin-when-cross-origin",
}))
```

When running behind a reverse proxy, configure trusted proxy CIDRs on the app so `Secure` can use `ctx.Request().Scheme()` safely:

```go
app, err := credo.New(credo.WithTrustedProxies("10.0.0.0/8"))
```

### Timeout

Sets a deadline on the request context. If the handler does not complete in time, `context.DeadlineExceeded` is returned and converted to a 503 response.

```go
api := app.Group("/api")
api.Middleware(middleware.Timeout(middleware.TimeoutConfig{Timeout: 5 * time.Second}))
```

Custom error handling:

```go
api.Middleware(middleware.Timeout(middleware.TimeoutConfig{
    Timeout: 10 * time.Second,
    ErrorHandler: func(ctx *credo.Context, err error) error {
        if errors.Is(err, context.DeadlineExceeded) {
            return credo.NewHTTPError(504, "http.gateway_timeout")
        }
        return err
    },
}))
```

### RateLimit

Token bucket rate limiting per client IP. Sets `X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset`, and `Retry-After` headers.

```go
// 60 requests per minute (default)
app.GlobalMiddleware(middleware.RateLimit())

// Custom limits
app.GlobalMiddleware(middleware.RateLimit(middleware.RateLimitConfig{
    Tokens:   120,
    Interval: time.Minute,
}))
```

Behind a reverse proxy, configure trusted proxy CIDRs on the app. The default rate-limit key uses `ctx.Request().RealIP()`:

```go
app, err := credo.New(credo.WithTrustedProxies("10.0.0.0/8"))

app.GlobalMiddleware(middleware.RateLimit(middleware.RateLimitConfig{
    Tokens:   100,
    Interval: time.Minute,
}))
```

Custom key function (e.g., rate limit by API key):

```go
app.GlobalMiddleware(middleware.RateLimit(middleware.RateLimitConfig{
    Tokens:   1000,
    Interval: time.Hour,
    KeyFunc: func(ctx *credo.Context) (string, error) {
        key := ctx.Request().Header.Get("X-API-Key")
        if key == "" {
            return "", errors.New("missing API key")
        }
        return key, nil
    },
}))
```

#### RateLimiter Lifecycle

The convenience constructor `RateLimit()` creates an internal in-memory store that is not automatically closed. For explicit lifecycle management, use `NewRateLimiter`:

```go
rl := middleware.NewRateLimiter(middleware.RateLimitConfig{
    Tokens:   120,
    Interval: time.Minute,
})
app.GlobalMiddleware(rl.Middleware())
app.OnShutdown(rl.Shutdown)
```

`RateLimiter` implements `credo.Shutdowner`, so it can also be registered in the DI container for automatic cleanup.

---

## Skipper

Every built-in middleware accepts a `Skipper` function. When the skipper returns `true`, the middleware is bypassed for that request.

```go
type Skipper func(ctx *credo.Context) bool
```

Common patterns:

```go
// Skip middleware for health checks
middleware.AccessLog(middleware.AccessLogConfig{
    Skipper: func(ctx *credo.Context) bool {
        return ctx.Request().URL.Path == "/health"
    },
})

// Skip middleware for internal IPs
middleware.RateLimit(middleware.RateLimitConfig{
    Skipper: func(ctx *credo.Context) bool {
        return strings.HasPrefix(ctx.Request().RealIP(), "10.")
    },
    Tokens: 60,
})
```

Custom middleware can adopt the same pattern:

```go
type MyConfig struct {
    Skipper middleware.Skipper
}

func MyMiddleware(cfg ...MyConfig) credo.Middleware {
    var c MyConfig
    if len(cfg) > 0 {
        c = cfg[0]
    }
    if c.Skipper == nil {
        c.Skipper = middleware.DefaultSkipper
    }
    return func(next credo.Handler) credo.Handler {
        return func(ctx *credo.Context) error {
            if c.Skipper(ctx) {
                return next(ctx)
            }
            // ... middleware logic ...
            return next(ctx)
        }
    }
}
```

---

## Meta-Driven Middleware

Middleware can read Route Meta to change behavior per-route declaratively, eliminating hardcoded path checks.

```go
// Middleware reads Meta instead of maintaining a path list
func RequirePermission(next credo.Handler) credo.Handler {
    return func(ctx *credo.Context) error {
        perm, ok := ctx.Route().LookupMeta("permission")
        if !ok {
            // No permission required for this route.
            return next(ctx)
        }

        user, ok := auth.GetUser[*User](ctx.Context())
        if !ok || user == nil || !user.HasPermission(perm.(string)) {
            return credo.ErrForbidden
        }

        return next(ctx)
    }
}

// Register the middleware at the group level
api := app.Group("/api")
api.Middleware(RequirePermission)

// Declare permissions on individual routes
api.GET("/users", listUsers).SetMeta("permission", "users.read")
api.POST("/users", createUser).SetMeta("permission", "users.write")
api.GET("/public/status", statusCheck)  // no "permission" Meta -> skipped
```

Group-level Meta is inherited by child routes and can be overridden:

```go
api := app.Group("/api")
api.SetMeta("auth", true)

api.GET("/users", listUsers)                           // auth = true (inherited)
api.GET("/health", healthCheck).SetMeta("auth", false) // auth = false (override)
```

`LookupMeta` traverses the parent chain: route -> group -> parent group -> root. The first match wins.

### ContractGuard — Built-in Contract Enforcement

`middleware.ContractGuard` is a ready-made meta-driven middleware: instead of writing per-route checks by hand, you declare request contracts as Route Meta and one middleware enforces them. It covers the most common gates:

| Meta key | Value type | Enforced as |
| --- | --- | --- |
| `middleware.MetaAccept` | `string` / `[]string` | Content-Type allow-list -> 415 |
| `middleware.MetaMaxBody` | `int` / `int64` | body byte cap (`MaxBytesReader`) -> 413 |
| `middleware.MetaRequireHeaders` | `string` / `[]string` | required headers -> 400 |
| `middleware.MetaRequireQuery` | `string` / `[]string` | required query params -> 400 |
| `middleware.MetaAPIVersion` | `string` / `[]string` | API version (header or `version` param) -> 400 |
| `middleware.MetaScope` | `string` / `[]string` | scope check -> 403 (needs `ScopeChecker`) |

```go
api := app.Group("/api")
api.Middleware(middleware.ContractGuard())

api.POST("/users", createUser).
    SetMeta(middleware.MetaAccept, "application/json").
    SetMeta(middleware.MetaMaxBody, int64(1<<20)).            // 1 MiB, on top of the global limit
    SetMeta(middleware.MetaRequireHeaders, []string{"X-Request-Id"})
```

Register ContractGuard at the **group or route level**, not via `app.GlobalMiddleware`. It reads matched-route metadata, and a route is only matched _after_ app-global middleware runs — group and route middleware run after the match, so the route (and its inherited group meta) is available there. Applied globally it degrades to a safe no-op rather than an error.

`MetaMaxBody` complements the global body limit (`WithMaxBodyBytes`) as defense in depth: the global cap protects every route, while the per-route contract can tighten (or, with a negative value, lift) it for a specific endpoint.

Because authenticated users are stored generically (`auth.GetUser[T]`), ContractGuard cannot inspect scopes on its own. Supply a `ScopeChecker` to bridge to your auth model; a route that declares `MetaScope` without a configured checker is denied (a declared scope is never silently bypassed). Use `CustomChecks` for contracts beyond the built-ins:

```go
api.Middleware(middleware.ContractGuard(middleware.ContractConfig{
    ScopeChecker: func(ctx *credo.Context, scope string) bool {
        u, ok := auth.GetUser[*User](ctx.Context())
        return ok && u.HasScope(scope)
    },
    CustomChecks: []func(*credo.Context) error{
        func(ctx *credo.Context) error {
            if ctx.Request().Header.Get("X-Tenant-Id") == "" {
                return credo.NewHTTPError(http.StatusBadRequest, "tenant required")
            }
            return nil
        },
    },
}))
```

---

## Using Stdlib Middleware

Existing Go middleware written for `net/http` can be adapted with `WrapStdMiddleware`:

```go
// Any func(http.Handler) http.Handler works
stdMiddleware := func(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("X-Custom", "value")
        next.ServeHTTP(w, r)
    })
}

app.GlobalMiddleware(credo.WrapStdMiddleware(stdMiddleware))
```

This works with any community middleware from Chi, Gorilla, or other ecosystem packages:

```go
import "github.com/some/middleware/pkg"

app.GlobalMiddleware(credo.WrapStdMiddleware(pkg.SomeMiddleware))
```

The adapter handles request and response writer synchronization between the stdlib and Credo worlds.

---

## Recommended Middleware Stack

A typical production application:

```go
// Built-in: recover, requestID, access log — automatically active.

// Global: always runs, even on 404/405
app.GlobalMiddleware(
    middleware.Secure(),                // 1. security headers
    middleware.CORS(),                  // 2. CORS headers
    middleware.CSRF(),                  // 3. cross-origin write protection
)

// API group: only matched routes
api := app.Group("/api")
api.Middleware(
    middleware.RateLimit(),             // rate limiting
    middleware.Timeout(middleware.TimeoutConfig{Timeout: 10 * time.Second}),
    middleware.Compress(),              // response compression
    authMiddleware,                     // authentication
)

// Admin sub-group
admin := api.Group("/admin")
admin.Middleware(requireAdmin)
```

Order matters:

- Built-in recover/requestID/accessLog are automatically the outermost layers in `compile()` — no registration needed
- `CORS` must run globally so preflight and 404 responses include CORS headers
- Group-level middleware runs only on matched routes

---

## Security Considerations

Credo's middleware chains are precompiled at startup into immutable function closures. There is no per-request slice manipulation or dynamic chain building, which eliminates an entire class of concurrency-based middleware bypass vulnerabilities. That said, a few architectural boundaries require developer awareness:

### Mounted Handlers Bypass Group/Route Middleware

`app.Mount()` registers a plain `http.Handler`. Mounted handlers receive only **built-in and global** middleware. Group and route middleware do not apply because the mounted handler is called directly after dispatch, outside the per-route compiled chain.

If the mounted sub-application requires authentication or authorization, the sub-application must enforce it internally:

```go
// Global middleware (CORS, Secure) applies.
// Group-level authMiddleware does NOT apply to /legacy routes.
app.Mount("/legacy", legacyApp)

// To protect mounted handlers, either:
// 1. Add auth as global middleware (affects all routes), or
// 2. Ensure the mounted handler has its own auth layer.
```

### Custom 404/405 Handlers Lack Route Context

Custom status handlers registered via `app.StatusHandler()` execute when no route matches. In this context:

- `ctx.Route()` is **nil** — there is no matched route.
- Group and route middleware have not run (no route to attach them to).
- Only built-in and global middleware are active.

Do not assume route-level auth, RBAC checks, or Meta lookups are available inside custom 404/405 handlers. Use `ctx.HasRoute()` to guard against nil before calling `ctx.Route()` methods — this prevents nil pointer panics in middleware that may run on both matched and unmatched request paths:

```go
// Safe: guard with HasRoute before accessing Route methods.
func RequirePermission(next credo.Handler) credo.Handler {
    return func(ctx *credo.Context) error {
        if !ctx.HasRoute() {
            return next(ctx) // no route matched (404/405 path)
        }
        perm, ok := ctx.Route().LookupMeta("permission")
        if !ok {
            return next(ctx)
        }
        // ... check permission ...
        return next(ctx)
    }
}
```

### Rewrite Ordering Affects Auth Decisions

`middleware.Rewrite()` runs as global middleware and modifies the URL path **before** dispatch. Any global middleware registered **before** the rewrite middleware evaluates the **original** path; middleware registered **after** evaluates the **rewritten** path.

If auth middleware runs globally before Rewrite, it makes decisions based on the original path. If the rewritten path targets a route with different authorization requirements, the auth decision may be incorrect:

```go
// Risky: auth runs on original path, rewrite changes target.
app.GlobalMiddleware(authMiddleware)
app.GlobalMiddleware(middleware.Rewrite(
    middleware.RewriteRule{From: "/public/{p...}", To: "/internal/{p}"},
))

// Safer: rewrite first, then auth sees the final path.
app.GlobalMiddleware(middleware.Rewrite(
    middleware.RewriteRule{From: "/public/{p...}", To: "/internal/{p}"},
))
app.GlobalMiddleware(authMiddleware)
```

When possible, prefer **group-level** auth middleware over global auth. Group middleware runs after dispatch — it always sees the final matched route and its metadata, regardless of rewrite ordering.

### Group Middleware Is Collected at Compile Time

Per-route middleware chains are assembled when the app compiles (at `Run()` or the first request) by walking the group parent chain — the same model route metadata uses. Middleware added to a group **after** routes or sub-groups were created therefore still applies to them:

```go
api := app.Group("/api")
api.GET("/users", listUsers)

api.Middleware(authMiddleware)    // added AFTER /api/users registration
api.GET("/orders", listOrders)

// Result: BOTH /api/users and /api/orders run authMiddleware.
```

Registration order affects only the order middleware runs in (parent groups before children, append order within a group) — never whether it applies. To exclude a specific route from a group's middleware, register the route on a sibling group or attach the middleware per-route instead.

---

## Complete Example

```go
package main

import (
    "log"
    "log/slog"
    "net/http"
    "time"

    "github.com/credo-go/credo"
    "github.com/credo-go/credo/middleware"
)

// Custom middleware: require a valid API key.
func APIKeyAuth(apiKey string) credo.Middleware {
    return func(next credo.Handler) credo.Handler {
        return func(ctx *credo.Context) error {
            // Skip if route opts out via Meta.
            if val, ok := ctx.Route().LookupMeta("public"); ok && val.(bool) {
                return next(ctx)
            }

            key := ctx.Request().Header.Get("X-API-Key")
            if key != apiKey {
                return credo.ErrUnauthorized
            }
            return next(ctx)
        }
    }
}

func main() {
    app, err := credo.New()
    if err != nil {
        log.Fatal(err)
    }

    // Built-in: recover, requestID, access log — automatically active.
    // Add global middleware for additional cross-cutting concerns:
    app.GlobalMiddleware(
        middleware.Secure(middleware.SecureConfig{
            HSTSMaxAge:    31536000,
            XFrameOptions: "DENY",
        }),
        middleware.CORS(middleware.CORSConfig{
            AllowOrigins:     []string{"https://example.com"},
            AllowCredentials: true,
        }),
    )

    // Health (no auth needed)
    app.UseHealth()

    // API group with auth, rate limit, timeout, and compression
    api := app.Group("/api")
    api.Middleware(
        APIKeyAuth("secret-key"),
        middleware.RateLimit(middleware.RateLimitConfig{
            Tokens:   100,
            Interval: time.Minute,
        }),
        middleware.Timeout(middleware.TimeoutConfig{Timeout: 5 * time.Second}),
        middleware.Compress(),
    )

    // Public route: no API key required (Meta override)
    api.GET("/status", func(ctx *credo.Context) error {
        return ctx.Response().JSON(http.StatusOK, map[string]string{
            "status": "ok",
        })
    }).SetMeta("public", true)

    // Protected routes
    api.GET("/users", func(ctx *credo.Context) error {
        return ctx.Response().JSON(http.StatusOK, []string{"alice", "bob"})
    })

    api.POST("/users", func(ctx *credo.Context) error {
        return ctx.Response().JSON(http.StatusCreated, map[string]string{
            "message": "created",
        })
    })

    // Run blocks until SIGINT/SIGTERM, then drains gracefully and returns nil.
    if err := app.Run(); err != nil {
        slog.Error("server error", "error", err)
    }
}
```
