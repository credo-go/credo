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

    // Panic recovery is on by default. Request IDs, access logging and
    // compression are framework features, enabled explicitly:
    app.UseRequestID()
    app.UseAccessLog()

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
app.GlobalMiddleware(
    middleware.CORS(),
    middleware.Secure(),
)
```

Without a global tier, a 404 response would bypass CORS and security headers. The global tier closes that gap. (Panic recovery, request IDs, access logging and compression are framework features rather than middleware — see [Framework Features](#framework-features-not-middleware) below — and they cover 404/405 responses as well.)

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
  -> Framework features (request ID, request decompression, compression writer)
    -> Global middleware (outer to inner)
      -> Group middleware (outer to inner, parent to child)
        -> Route middleware (outer to inner)
          -> Handler
        <- Route middleware
      <- Group middleware
    <- Global middleware
  <- Framework error rendering, panic recovery, finalization, access log
Response
```

All middleware chains are precompiled at startup. There is no per-request allocation for middleware dispatch. The framework features around the chain run in that fixed order whatever order their `Use*` calls were made in.

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
            return credo.NewHTTPError(415)
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
            return credo.NewHTTPError(503)
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

## Framework Features (Not Middleware)

Panic recovery, request IDs, access logging, response compression and request decompression are framework-owned HTTP features of the root package. They run outside the middleware chain — before Global middleware on the way in and after error rendering on the way out — so they see the final response, error envelopes and panic responses included, and cannot be scoped to a group or route. Each has exactly one public path:

| Feature | Default | Enable / configure |
| --- | --- | --- |
| **Recovery** | On | `credo.WithRecoverConfig(cfg)` customizes; `credo.WithoutRecover()` disables (wins regardless of option order) |
| **RequestID** | Off | `app.UseRequestID(cfg...)` |
| **AccessLog** | Off | `app.UseAccessLog(cfg...)` |
| **Compress** | Off | `app.UseCompress(cfg...)` |
| **Decompress** | Off | `app.UseDecompress(cfg...)` |

A `Use*` call takes zero configs for the defaults or one config; it panics for more than one config, when called twice, or after the app has prepared to serve (`Run`, the first `ServeHTTP`) or shut down. The window stays open after `app.Finalize()`, so a feature or renderer can be configured from a DI-resolved service. Leaving a call out leaves the feature off. The full contract, including failure handling, is in the [HTTP features spec](../specs/http-features.md).

```go
app, _ := credo.New(credo.WithRecoverConfig(credo.RecoverConfig{
    StackSize: 16 << 10, // bytes of stack captured in the panic record
}))
app.UseRequestID()
app.UseAccessLog()
app.UseCompress()
```

### Recovery

Recovery is the outermost layer and covers everything: user middleware, handlers, renderers, locale detectors and access-log filters. A panic is logged as `panic recovered` with a stack trace and rendered as a generic 500 through the error pipeline, unless the response was already committed or hijacked. `http.ErrAbortHandler` is re-panicked so the server aborts the connection.

```go
app, _ := credo.New(credo.WithRecoverConfig(credo.RecoverConfig{
    Logger:            panicLogger, // nil: the request-scoped logger (carries request_id)
    DisableStackTrace: false,
    StackSize:         8192,        // maximum bytes of stack trace; 0 selects 8192
}))
```

`credo.WithoutRecover()` turns recovery off — for tests that want panics to propagate, or for an application-owned recovery layer. Request cleanup (compressor finalization, access-log observation, Context release) still runs while the panic propagates. There is no per-group recovery; a group that needs its own policy writes ordinary middleware that recovers and returns an error.

### RequestID

`app.UseRequestID()` gives every request an ID: the incoming `X-Request-Id` value when it is present, within the length limit and made of safe characters, otherwise a freshly generated 128-bit cryptographic ID. The ID is echoed on the response header, readable through `ctx.RequestID()` and attached as `request_id` to the request-scoped logger the first time `ctx.Logger()` is used, so handler logs, the access log and panic records carry it automatically.

```go
app.UseRequestID(credo.RequestIDConfig{
    Header:    "X-Correlation-Id",
    Limit:     128,
    Generator: func() string { return uuid.NewString() },
})

func myHandler(ctx *credo.Context) error {
    reqID := ctx.RequestID() // "" when UseRequestID is not installed
    ctx.Logger().Info("processing")
    return ctx.Response().JSON(200, map[string]string{"id": reqID})
}
```

RequestID and AccessLog are independent: access records carry an empty `request_id` when only access logging is installed.

### AccessLog

`app.UseAccessLog()` logs each completed request with structured attributes: method, path, status, bytes, duration, real client address, user agent, request ID and the matched route pattern. It observes the final response after error rendering, recovery and compressor finalization, so status, bytes (post-compression, as accepted by the transport) and duration are final. Log level follows the status: 1xx/2xx/3xx = Info, 4xx = Warn, 5xx+ = Error.

```go
app.UseAccessLog(credo.AccessLogConfig{
    Logger:   accessLogger,    // nil: the request-scoped logger
    MinLevel: slog.LevelInfo,  // nil: Info
    Skipper:  func(ctx *credo.Context) bool { return ctx.Request().URL.Path == "/metrics" },
    ResultFilter: func(ctx *credo.Context, entry credo.AccessLogEntry) bool {
        return entry.Status >= 400 || entry.Duration >= time.Second
    },
})
```

- **Skipper** runs before routing, so decide from request-level data (path, headers). Return `true` to skip.
- **Route or group meta.** Set `credo.MetaAccessLog` to `false` on a route, or on a whole group through inheritance; a route-level value overrides its group's, so you can silence a group and keep one route loud. Only a bool `false` silences; any other value is ignored. Health probes (`/health`, `/ready`) are silent by default — re-enable them with `HealthConfig{LogRequests: true}`.

```go
app.GET("/metrics", metricsHandler).SetMeta(credo.MetaAccessLog, false)

internal := app.Group("/internal")
internal.SetMeta(credo.MetaAccessLog, false)                          // silence everything under /internal
internal.GET("/audit", auditHandler).SetMeta(credo.MetaAccessLog, true) // ...except this one
```

- **MinLevel** admits records at or above a level without changing their actual level: `slog.LevelWarn` keeps 4xx and 5xx, `slog.LevelError` keeps 5xx only. Pass a concurrency-safe `*slog.LevelVar` to change the threshold at runtime; it is read once per eligible request.
- **ResultFilter** runs after the final response, after meta silencing and the MinLevel check. Return `true` to emit. MinLevel and ResultFilter intersect: `Warn` would reject successful requests before the filter, so the filter cannot restore slow `2xx` entries. Use `entry`, not `ctx.Response()`, for status, bytes and duration. The Context is pooled and the callback may run concurrently; do not retain it and synchronize mutable state. A filter that panics is logged as a framework error and the record is skipped; the response is unaffected. Request/result combinations work as expected:

```go
ResultFilter: func(ctx *credo.Context, entry credo.AccessLogEntry) bool {
    user, ok := ctx.GetUser[User]()
    return (ok && user.TenantID == "audit-all") || entry.Status >= 400
},
```

- **Logger** routes access records to a dedicated sink, for example a JSON logger on its own file. It receives the standard fields and an explicit request ID, but it is not derived from `ctx.Logger()` and therefore does not inherit attributes added with `ctx.AddLogAttrs` or `ctx.SetLogger`.

When the final served path differs from the client path because of `middleware.Rewrite()` or `ctx.Rewrite()`, the entry includes `path_original`. When a route matched, it also carries `route`, the registered pattern (`/v1/jobs/{job_id}`) rather than the concrete path. Credo does not transform log fields itself; a deployment that must not persist identifiers from paths keeps `route` and drops `path`/`path_original` in the configured logger, using `slog.HandlerOptions.ReplaceAttr`.

Turning access logging off does not silence anything else: panic records, server errors, lifecycle, health and application logs keep flowing through their normal loggers and levels.

### Compress

`app.UseCompress()` compresses responses with gzip or deflate according to the client's `Accept-Encoding` header. Only textual content types are compressed by default; responses that already carry a `Content-Encoding`, HEAD and bodiless responses, streaming (`Flush`), committed responses and hijacked connections keep their behavior. Because the writer stays installed through error rendering, error envelopes and panic responses are compressed too, and the access log counts the compressed bytes.

```go
app.UseCompress(credo.CompressConfig{
    Level: 9, // 1–9; 0 selects 5
    Types: []string{
        "application/json",
        "text/*", // wildcard supported
    },
    Skipper: func(ctx *credo.Context) bool { // evaluated once on the original request
        return strings.HasPrefix(ctx.Request().URL.Path, "/stream/")
    },
})
```

### Decompress

Credo never decompresses request bodies on its own: a body sent with `Content-Encoding: gzip` reaches `BindBody`, which answers 415 `unsupported_content_encoding` rather than mis-reporting the compressed bytes as malformed JSON. Accepting compressed uploads is an opt-in, because decompression turns a small wire body into a potentially huge one:

```go
app.UseDecompress(credo.DecompressConfig{
    MaxBytes: 16 << 20, // bound on the *decompressed* body; default 4 MiB
    Skipper: func(ctx *credo.Context) bool { // keep raw bodies for signed webhooks
        return strings.HasPrefix(ctx.Request().URL.Path, "/webhooks/")
    },
})
```

The feature understands `gzip` and `deflate` (zlib-wrapped, with raw DEFLATE tolerated), unwraps the body as a stream, removes the header and sets `ContentLength` to -1 so Global middleware, handlers and `BindBody` all see the same plain stream. The server-wide `max_body_bytes` still applies to the compressed bytes on the wire; reading past `MaxBytes` after decompression fails with the same 413 as any oversized body. Unsupported codings (`br`, `zstd`, or a list such as `gzip, br`) return 415 and a corrupt stream returns 400 `bind_failed`, both before any user middleware runs. The Skipper sees the original request only (method, path, headers): no route, no user, and a later rewrite does not re-evaluate it.

---

## Configurable Middleware (middleware package)

### Rewrite

`middleware.Rewrite(cfg)` is Credo's pre-dispatch path rewrite middleware. Like every configurable middleware it takes one config struct: `RewriteConfig.Rules` holds the ordered rule list (first match wins) and the optional `Skipper` bypasses rewriting for selected requests.

```go
app.GlobalMiddleware(middleware.Rewrite(middleware.RewriteConfig{Rules: []middleware.RewriteRule{
    {From: "/v1/{path...}", To: "/api/v1/{path}"},
    {Host: "old.example.com", From: "/{path...}", To: "/legacy/{path}"},
}}))

// Skip rewriting for a health probe path:
app.GlobalMiddleware(middleware.Rewrite(middleware.RewriteConfig{
    Skipper: func(ctx *credo.Context) bool { return ctx.Request().URL.Path == "/healthz" },
    Rules:   []middleware.RewriteRule{{From: "/v1/{path...}", To: "/api/v1/{path}"}},
}))
```

`Rules` must not be empty and every `From` pattern must parse; both are configuration errors and panic when the middleware is built. Use it when you want routing to see a normalized path on the first lookup. For conditional handler-driven forwarding, use `ctx.Rewrite()` instead.

See the [Routing Guide](routing.md) for host routing, rewrite patterns, `OriginalPath()`, and rewrite-specific middleware caveats.

### CORS

Handles Cross-Origin Resource Sharing preflight and actual requests.

```go
// Allow all origins (default)
app.GlobalMiddleware(middleware.CORS())

// Restrict to specific origins
app.GlobalMiddleware(middleware.CORS(middleware.CORSConfig{
    AllowOrigins:     []string{"https://example.com"},
    AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "QUERY"},
    AllowHeaders:     []string{"Authorization", "Content-Type"},
    AllowCredentials: true,
    ExposeHeaders:    []string{"X-Total-Count"},
    MaxAge:           86400,
}))
```

`AllowOrigins` uses the same strict origin grammar as the websocket adapter's `AllowedOrigins`: each entry is `"*"` or a `scheme://host[:port]` origin (`http`/`https` only; no path, query, fragment, or userinfo; case-insensitive; default port implied). One wildcard may stand for exactly the left-most DNS label:

```go
middleware.CORS(middleware.CORSConfig{
    AllowOrigins: []string{"https://*.example.com"}, // app.example.com yes; example.com, a.b.example.com no
})
```

Any other shape — a mid-label `*` such as `https://api-*-prod.example.com`, several wildcards, an IP wildcard, or an empty entry — is a configuration error and panics when the middleware is constructed.

Behind a reverse proxy, let exactly one layer answer CORS. If the proxy (nginx `add_header`, an ingress annotation, a CDN rule) also appends `Access-Control-*` headers, the browser sees two values for `Access-Control-Allow-Origin` and rejects the response even though each value alone would have been accepted. Either remove the proxy rule or drop this middleware for the proxied routes.

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
// Zero config: blocks cross-origin state-changing methods; QUERY is safe.
app.GlobalMiddleware(middleware.CSRF())

// Frontend on another origin + webhook endpoints
app.GlobalMiddleware(middleware.CSRF(middleware.CSRFConfig{
    TrustedOrigins:         []string{"https://app.example.com"},
    InsecureBypassPatterns: []string{"/webhooks/"},
}))
```

What passes without configuration:

- `GET`, `HEAD`, `OPTIONS`, `QUERY` (safe methods — never change state in them)
- same-origin browser requests
- non-browser clients (curl, server-to-server, mobile SDKs) — requests without `Sec-Fetch-Site`/`Origin` headers are allowed

**Subdomains are cross-origin.** A form on `app.example.com` posting to `api.example.com` is rejected (browsers send `Sec-Fetch-Site: same-site`) unless the frontend origin is listed in `TrustedOrigins`.

Rejections return a centralized 403 error envelope; the detector's reason is logged but never exposed. Override with `ErrorHandler`:

```go
middleware.CSRF(middleware.CSRFConfig{
    ErrorHandler: func(ctx *credo.Context, err error) error {
        ctx.Logger().Warn("csrf rejected", "origin", ctx.Request().Header.Get("Origin"))
        return credo.NewHTTPError(http.StatusForbidden)
    },
})
```

CSRF and CORS are complementary: CORS controls whether a browser may _read_ a cross-origin response; CSRF stops cross-origin state changes from being _processed_. A browser frontend on another origin typically needs its origin in both `CORSConfig.AllowOrigins` and `CSRFConfig.TrustedOrigins`.

QUERY is safe under RFC 10008, but it is not a CORS-safelisted method. Cross-origin fetch/XHR therefore needs a successful preflight; the default CORS configuration includes QUERY. HTML forms and navigation cannot produce QUERY, so Credo lets QUERY bypass Go 1.27's older GET/HEAD/OPTIONS-only CSRF safe list. A handler must never mutate state from QUERY.

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

#### HSTS (Strict-Transport-Security)

HSTS tells browsers to use HTTPS for all future requests to your domain. It is **opt-in and never enabled automatically** — the framework sets the header only when you give `HSTSMaxAge` a non-zero value. The header is also sent **only over HTTPS** (guarded by `ctx.Request().Scheme()`), per RFC 6797; it is never emitted on a plaintext response.

```go
// Enable HSTS only — set HSTSMaxAge; the other Secure headers stay off because
// the zero-value config does not apply the X-XSS/nosniff/frame defaults.
app.GlobalMiddleware(middleware.Secure(middleware.SecureConfig{
    HSTSMaxAge:            31536000, // 1 year, in seconds
    HSTSExcludeSubdomains: false,    // includeSubDomains on (the default)
}))
```

To actually serve HTTPS, configure TLS at construction (`credo.WithTLSFiles` / `credo.WithTLSConfig`); to bounce plaintext callers to HTTPS, add `credo.WithHTTPRedirect(":80")`. File-based certificates rotate on every reload (`systemctl reload` / `app.Reload`), so an HSTS-committed domain never has to drop TLS to renew — see the [Deployment Guide](deployment.md#certificate-rotation-with-certbot). HSTS is the complementary, client-side half: it makes browsers _prefer_ HTTPS on their own.

> **`HSTSPreloadEnabled` is a near-permanent commitment.** Submitting your domain to the browser preload list (which requires `max-age` ≥ 1 year, `includeSubDomains`, and the `preload` token) is slow and painful to undo — every subdomain becomes HTTPS-only in shipped browsers. Only enable it once every subdomain can serve HTTPS and you understand the rollback cost.

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
            return credo.NewHTTPError(504)
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

Every configurable middleware in the `middleware` package accepts a `Skipper` function. When the skipper returns `true`, the middleware is bypassed for that request. (The framework features use the same shape: `AccessLogConfig`, `CompressConfig` and `DecompressConfig` each carry a `Skipper func(*credo.Context) bool`.)

```go
type Skipper func(ctx *credo.Context) bool
```

Common patterns:

```go
// Skip middleware for health checks
middleware.Timeout(middleware.TimeoutConfig{
    Skipper: func(ctx *credo.Context) bool {
        return ctx.Request().URL.Path == "/health"
    },
    Timeout: 5 * time.Second,
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

        user, ok := ctx.GetUser[*User]()
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
| `middleware.MetaAccept` | `string` / `[]string` | Content-Type allow-list -> 415 (missing header passes unless `RequireContentType`) |
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

By default a request with **no** `Content-Type` header passes the `MetaAccept` contract — there may be nothing to police. For a JSON API where every body must be labelled, set `ContractConfig.RequireContentType: true`: a request that carries a body (positive or unknown `Content-Length` — chunked and HTTP/2 streams included) but no or an empty `Content-Type` is rejected with 415. Bodiless requests (`GET`, `Content-Length: 0`) and routes without `MetaAccept` are unaffected, so the switch arms a declared contract rather than adding a new one:

```go
api.Middleware(middleware.ContractGuard(middleware.ContractConfig{
    RequireContentType: true, // JSON API: every body must say what it is
}))
```

`MetaMaxBody` complements the global body limit (`WithMaxBodyBytes`) as defense in depth: the global cap protects every route, while the per-route contract can tighten (or, with a negative value, lift) it for a specific endpoint.

Because authenticated users are stored generically (`ctx.GetUser[T]`), ContractGuard cannot inspect scopes on its own. Supply a `ScopeChecker` to bridge to your auth model; a route that declares `MetaScope` without a configured checker is denied (a declared scope is never silently bypassed). Use `CustomChecks` for contracts beyond the built-ins:

```go
api.Middleware(middleware.ContractGuard(middleware.ContractConfig{
    ScopeChecker: func(ctx *credo.Context, scope string) bool {
        u, ok := ctx.GetUser[*User]()
        return ok && u.HasScope(scope)
    },
    CustomChecks: []func(*credo.Context) error{
        func(ctx *credo.Context) error {
            if ctx.Request().Header.Get("X-Tenant-Id") == "" {
                return credo.NewHTTPError(http.StatusBadRequest, "tenant_required").
                    WithMessageKey("tenant required")
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

Adapted middleware is second-class by design: it receives only `*http.Request` and `r.Context()`, never `*credo.Context`. It cannot read route Meta or the typed principal (`ctx.GetUser[T]`), and if it short-circuits by writing to the `ResponseWriter` directly, that response bypasses Credo's centralized error envelope (responses produced by calling `next` still flow back through it). When a middleware needs the authenticated principal or the error pipeline, write it as a native `func(Handler) Handler` instead — that is the first-class path.

---

## Recommended Middleware Stack

A typical production application:

```go
// Framework features: recovery is on by default; the rest are explicit.
app.UseRequestID()
app.UseAccessLog()
app.UseCompress()

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
    authMiddleware,                     // authentication
)

// Admin sub-group
admin := api.Group("/admin")
admin.Middleware(requireAdmin)
```

Order matters:

- Framework features (recovery, request ID, access log, compression) are always the outermost layers; their `Use*` call order does not matter
- `CORS` must run globally so preflight and 404 responses include CORS headers
- Group-level middleware runs only on matched routes

---

## Security Considerations

Credo's middleware chains are precompiled at startup into immutable function closures. There is no per-request slice manipulation or dynamic chain building, which eliminates an entire class of concurrency-based middleware bypass vulnerabilities. That said, a few architectural boundaries require developer awareness:

### Mounted Handlers Bypass Group/Route Middleware

`app.Mount()` registers a plain `http.Handler`. Mounted handlers receive only **global** middleware (and the framework features, which wrap every request). Group and route middleware do not apply because the mounted handler is called directly after dispatch, outside the per-route compiled chain.

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
- Only framework features and global middleware are active.

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
app.GlobalMiddleware(middleware.Rewrite(middleware.RewriteConfig{Rules: []middleware.RewriteRule{
    {From: "/public/{p...}", To: "/internal/{p}"},
}}))

// Safer: rewrite first, then auth sees the final path.
app.GlobalMiddleware(middleware.Rewrite(middleware.RewriteConfig{Rules: []middleware.RewriteRule{
    {From: "/public/{p...}", To: "/internal/{p}"},
}}))
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

    // Framework features: recovery is on by default; enable the rest.
    app.UseRequestID()
    app.UseAccessLog()
    app.UseCompress()

    // Global middleware for additional cross-cutting concerns:
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

    // API group with auth, rate limit and timeout
    api := app.Group("/api")
    api.Middleware(
        APIKeyAuth("secret-key"),
        middleware.RateLimit(middleware.RateLimitConfig{
            Tokens:   100,
            Interval: time.Minute,
        }),
        middleware.Timeout(middleware.TimeoutConfig{Timeout: 5 * time.Second}),
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
