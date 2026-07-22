# Middleware Spec

**Status**: Approved **Package**: `middleware/` **Sources**: Chi (MIT), Echo (MIT), Goyave (MIT) **Depends on**: Root package **ADRs**: [010-middleware-architecture](../adr/010-middleware-architecture.md), [018-host-routing-and-rewrite](../adr/018-host-routing-and-rewrite.md)

---

## Overview

Credo middleware returns `credo.Middleware` (`func(Handler) Handler`). Stdlib middleware works via `WrapStdMiddleware` adapter. A 3-tier execution model (Global / Group / Route) provides fine-grained control over which middleware runs where. URL rewriting is implemented as middleware at the global/group layer when path normalization must happen before route matching.

---

## Signatures

```go
type Middleware func(Handler) Handler
```

The single middleware type used throughout Credo. Has full access to `credo.Context`, can return errors, and can read route Meta declaratively.

### Stdlib Adapter

Existing Go middleware (chi, gorilla, etc.) can be adapted via `WrapStdMiddleware`:

```go
app.GlobalMiddleware(credo.WrapStdMiddleware(thirdPartyMiddleware))
```

Adapted stdlib middleware is deliberately second-class: it sees only `*http.Request` and `r.Context()`, never `*credo.Context`, so it cannot read route Meta, the typed principal (`ctx.GetUser[T]`), or the renderer. If it short-circuits by writing to the `ResponseWriter` directly, that response bypasses the RFC 7807 error pipeline — only responses produced by calling `next` flow back through Credo's error handling. The first-class path for anything that needs the principal or the error pipeline is a native `func(Handler) Handler`. See [ADR-010](../adr/010-middleware-architecture.md) and [ADR-012](../adr/012-authentication-and-authorization.md).

---

## Built-in Tier (Auto-Enabled)

Three built-in middleware are applied automatically by `compile()` and require zero configuration:

| Built-in           | Purpose                        | Opt-out              |
| ------------------ | ------------------------------ | -------------------- |
| `builtinRecover`   | Outermost panic recovery       | `WithoutRecover()`   |
| `builtinRequestID` | Request ID + logger enrichment | `WithoutRequestID()` |
| `builtinAccessLog` | Structured access logging      | `WithoutAccessLog()` |

**Execution chain:** `builtinRequestID → builtinAccessLog → builtinRecover → builtinErrorHandler → globalMW → dispatch`

RequestID custom headers/generators still use the configurable middleware. AccessLog custom sinks, thresholds, request skipping, and result filtering stay on the authoritative built-in layer:

```go
app, err := credo.New(
	credo.WithAccessLogLogger(accessLogger),
	credo.WithAccessLogMinLevel(slog.LevelWarn),
	credo.WithAccessLogSkipper(skipAssets),
	credo.WithAccessLogResultFilter(keepInterestingResults),
	credo.WithoutRequestID(), // custom RequestID middleware below
)
app.GlobalMiddleware(
	middleware.RequestID(middleware.RequestIDConfig{Header: "X-Trace-Id"}),
)
```

`middleware.RequestID` behaves like the built-in tier: it also enriches the request-scoped logger with `request_id` (via `ctx.AddLogAttrs`), so handler logs and the access log carry the ID automatically.

**request_id sourcing rule** (shared by built-in and `middleware` AccessLog/Recover): the `request_id` attribute is added explicitly only when the target logger does not already carry it — that is, when a custom `Logger` was configured, or when no request-scoped logger was set (`ctx.HasRequestLogger()`). This keeps `request_id` appearing exactly once per log record in every combination.

**Access-log contract.** Status maps to an actual record level: `1xx/2xx/3xx → Info`, `4xx → Warn`, `5xx+ → Error`. `WithAccessLogMinLevel(slog.Leveler)` is an admission threshold, not a level rewrite; nil defaults to Info, a typed-nil provider is a `New` error, and concurrency-safe `slog.LevelVar` supports runtime changes. `WithAccessLogResultFilter(func(*Context, AccessLogEntry) bool)` is positive (`true = emit`), synchronous, concurrency-safe, and cannot restore an entry rejected by MinLevel. Its Context is pooled and must not be retained. The equivalent configurable fields are `AccessLogConfig.MinLevel` and `ResultFilter`; typed-nil MinLevel panics at middleware construction.

```go
// package credo
type AccessLogResultFilter func(ctx *Context, entry AccessLogEntry) bool

func WithAccessLogLogger(*slog.Logger) Option
func WithAccessLogMinLevel(slog.Leveler) Option
func WithAccessLogSkipper(func(*Context) bool) Option
func WithAccessLogResultFilter(AccessLogResultFilter) Option

// package middleware
type AccessLogConfig struct {
	Logger       *slog.Logger
	MinLevel     slog.Leveler
	Skipper      Skipper
	ResultFilter credo.AccessLogResultFilter
}
```

Nil logger/filter values preserve defaults; nil MinLevel normalizes to Info. All four built-in controls perform no request-time work under `WithoutAccessLog`, although invalid typed-nil configuration is still rejected during `New`. A non-nil custom Leveler must be concurrency-safe and is read exactly once per eligible request.

**Evaluation order:** `Skipper → handler → MetaAccessLog → status/level → MinLevel → snapshot → ResultFilter → emit`. The pre-dispatch Skipper (`true = skip`) sees only request data. `MetaAccessLog: false` then has precedence over result policies. Snapshot construction and ResultFilter are skipped for threshold/meta rejections. `AccessLogEntry.RequestID` is always populated from request state; the emitted `request_id` attribute is separately added only when the target logger does not already carry it. `RouteName` is filter metadata and is not emitted as `route_name`.

The built-in producer runs outside recovery/error rendering and observes final status, bytes, and total duration when the inner pipeline completes. If recovery is disabled and a panic escapes, it records the 500 fallback and bytes written before the panic. `middleware.AccessLog` runs at its configured position; on returned-error paths status is its best pre-render classification, bytes are those written so far, and duration excludes later rendering. A custom access logger does not inherit arbitrary `ctx.AddLogAttrs` enrichment, although standard fields and request ID are emitted. Using built-in and configurable AccessLog together intentionally produces two records; disable the built-in only when that is not desired.

The rule is convention-based: `HasRequestLogger` reports only that a request-scoped logger was set, not which attributes it carries (slog loggers are opaque). Middleware that replaces the logger without deriving from `ctx.Logger()` silently drops `request_id`, and the framework cannot detect it — enrich via `ctx.AddLogAttrs`, which derives by construction, and reserve `ctx.SetLogger` for genuine wholesale replacement.

---

## 3-Tier Model (Goyave-inspired)

| Tier | Registration | Scope | Runs on 404/405? |
| --- | --- | --- | --- |
| **Built-in** | Automatic (compile-time) | Every request | **Yes** |
| **Global** | `app.GlobalMiddleware(m...)` | Every request | **Yes** |
| **Group** | `group.Middleware(m...)` | Routes under this group | No |
| **Route** | `route.Middleware(m...)` | Single route only | No |

### Execution Order

```
Request
  → Built-in middleware (requestID → accessLog → recover)
    → Global middleware (outer to inner)
      → Group middleware (outer to inner, parent to child)
        → Route middleware (outer to inner)
          → Handler
        ← Route middleware
      ← Group middleware
    ← Global middleware
  ← Built-in middleware
Response
```

### Group Middleware Is Collected at Compile Time

A route's chain is assembled when the app compiles (at `Run()` or the first request), by walking the route's group parent chain — the same model `LookupMeta` uses for metadata. Middleware added to a group after routes were registered or sub-groups created therefore still applies to them. Registration order determines middleware _order_ (parent groups before children, append order within a group), never _membership_. To exclude one route from a group middleware, register it on a sibling group or attach middleware per-route.

### Why Global Tier Matters

Without a global tier, 404/405 responses bypass all group/route middleware — no CORS headers, no compression. The global tier ensures these cross-cutting concerns always run. (Request ID, access logging, and panic recovery are built-in and always active unless opted out.)

```go
app, err := credo.New()
if err != nil {
    panic(err)
}

// Built-in: recover, requestID, access log — already active.
// Add extra global middleware for cross-cutting concerns:
app.GlobalMiddleware(
    middleware.CORS(),
    middleware.Secure(),
)

// These run only on matched routes within the group
api := app.Group("/api")
api.Middleware(middleware.Compress())
```

---

## Meta-Driven Middleware (Goyave-inspired)

Middleware can read Route Meta to change behavior per-route declaratively, instead of hardcoding path checks.

```go
// Auth middleware — checks Meta instead of path list
func Auth(next credo.Handler) credo.Handler {
    return func(ctx *credo.Context) error {
        if val, ok := ctx.Route().LookupMeta("auth"); ok && val.(bool) {
            token := ctx.Request().Header.Get("Authorization")
            if !validateToken(token) {
                return credo.NewHTTPError(401, "unauthorized")
            }
        }
        return next(ctx)
    }
}

// Usage — declarative, not path-based
api := app.Group("/api")
api.SetMeta("auth", true)
api.Middleware(Auth)

api.GET("/users", listUsers)                          // authenticated
api.GET("/health", healthCheck).SetMeta("auth", false) // not authenticated
```

### Framework Meta Keys

These keys are read by built-in and framework middleware. Application middleware may also define its own convention keys — the `"auth"` key in the example above is one such user-defined key, not a framework key.

| Key | Type | Used by |
| --- | --- | --- |
| `credo.MetaAccessLog` (`"credo.accesslog"`) | `bool` (`false` silences) | Access logger (built-in + `middleware.AccessLog`) |
| `middleware.MetaAccept` (`"accept"`) | `string` \| `[]string` | `ContractGuard` — 415 on Content-Type mismatch |
| `middleware.MetaMaxBody` (`"max_body"`) | `int` \| `int32` \| `int64` (bytes) | `ContractGuard` — 413 over the per-route cap |
| `middleware.MetaRequireHeaders` (`"require_headers"`) | `string` \| `[]string` | `ContractGuard` — 400 if a header is missing |
| `middleware.MetaRequireQuery` (`"require_query"`) | `string` \| `[]string` | `ContractGuard` — 400 if a query param is missing |
| `middleware.MetaAPIVersion` (`"api_version"`) | `string` \| `[]string` | `ContractGuard` — 400 on version mismatch |
| `middleware.MetaScope` (`"scope"`) | `string` \| `[]string` | `ContractGuard` — 403 (requires `ScopeChecker`) |

---

## Config Struct Pattern (Echo-inspired)

Complex middleware provides both default and configurable constructors:

```go
// Default config
app.GlobalMiddleware(middleware.CORS())

// Custom config
app.GlobalMiddleware(middleware.CORS(middleware.CORSConfig{
    AllowOrigins: []string{"https://example.com"},
    AllowMethods: []string{"GET", "POST"},
}))
```

---

## Built-in Middleware

### Auto-Enabled (Framework Built-in)

| Built-in | Description | Opt-out |
| --- | --- | --- |
| Panic recovery | Outermost layer, catches all panics | `WithoutRecover()` |
| Request ID | `X-Request-Id` header + `ctx.Logger()` enrichment | `WithoutRequestID()` |
| Access log | Final structured request logging via `slog`; configure with `WithAccessLogLogger`, `WithAccessLogMinLevel`, `WithAccessLogSkipper`, `WithAccessLogResultFilter`, or `MetaAccessLog` | `WithoutAccessLog()` |

### Configurable (middleware package)

| Middleware | Source | Description |
| --- | --- | --- |
| `AccessLog` | Chi | Route/group-scoped request logging with Skipper, MinLevel, ResultFilter, `MetaAccessLog`, custom logger, and an earlier error-path observation boundary |
| `Recover` | Chi | Per-group/route panic recovery with custom config |
| `RequestID` | Chi | `X-Request-Id` with custom header, generator, limit |
| `Rewrite` | Credo | Pre-dispatch path rewriting with Credo route syntax |
| `CORS` | Echo | Cross-Origin Resource Sharing |
| `CSRF` | stdlib wrap | Cross-origin request rejection via `net/http.CrossOriginProtection` (Sec-Fetch-Site based, no tokens) |
| `Compress` | Chi | gzip/deflate response compression |
| `Secure` | Echo | Security headers (HSTS, CSP, X-Frame). HSTS uses `Request.Scheme()` |
| `RateLimit` | go-limiter | Token bucket rate limiting. Default key uses `Request.RealIP()` |
| `Timeout` | Echo | Request timeout |
| `ContractGuard` | Credo | Declarative per-route request contracts (Content-Type, body size, required headers/query, API version, scope) read from route meta |

### Rewrite

```go
func Rewrite(rules ...RewriteRule) credo.Middleware
func RewriteWithConfig(cfg RewriteConfig) credo.Middleware

type RewriteConfig struct {
    Skipper Skipper
    Rules   []RewriteRule
}
```

`middleware.Rewrite` mutates `req.URL.Path` before dispatch so that routing sees the rewritten path on the first lookup. `Rewrite(rules...)` is the rule-list shortcut; `RewriteWithConfig` adds a `Skipper` alongside the rules.

```go
type RewriteRule struct {
    Host string
    From string
    To   string

    Regexp *regexp.Regexp

    PreserveQuery bool
}
```

**Semantics:**

- Rules are evaluated in order; first match wins.
- `From` uses Credo route syntax (`{name}`, `{name...}`, `{name:regex}`) unless `Regexp` is provided. Brace matching follows the same parser as the router, including regex quantifiers, escaped braces, and character classes.
- `To` expands named placeholders (`{name}`) from the matched captures.
- `Host` is an optional exact host filter. Matching is case-insensitive, with request ports stripped before comparison.
- If `To` contains a query string, it replaces the current query string.
- If `To` does not contain a query string and `PreserveQuery` is true, the original query string is preserved.
- `Rewrite()`/`RewriteWithConfig()` panic when called with zero rules.

**Placement:**

Register rewrite as global middleware when it should affect routing for the whole app:

```go
app.GlobalMiddleware(middleware.Rewrite(
    middleware.RewriteRule{From: "/v1/{path...}", To: "/api/v1/{path}"},
))
```

Register `Rewrite` as global middleware when routing must see the rewritten path. When attached at group or route scope, it only mutates the request seen by downstream middleware/handler for an already matched route. It does not trigger a re-dispatch loop.

When a handler later calls `ctx.Rewrite()`, built-in and global middleware do not run again. Group and route middleware for the newly matched route do run again, so `after` logic must be written with per-dispatch semantics in mind.

### CSRF

```go
func CSRF(cfg ...CSRFConfig) credo.Middleware

type CSRFConfig struct {
    Skipper                Skipper
    TrustedOrigins         []string // "scheme://host[:port]", exact Origin match
    InsecureBypassPatterns []string // http.ServeMux patterns, skip checks entirely
    ErrorHandler           func(ctx *credo.Context, err error) error
}
```

Wraps the standard library's `net/http.CrossOriginProtection`: cross-origin detection via the `Sec-Fetch-Site` header (all modern browsers) with an Origin/Host comparison fallback. **No tokens, cookies, or session state** — the per-request cost is a header check.

**Semantics (inherited from the stdlib detector):**

- `GET`/`HEAD`/`OPTIONS` always pass (safe methods — handlers must not perform state changes in them).
- `Sec-Fetch-Site: same-origin` / `none` pass.
- Requests with neither `Sec-Fetch-Site` nor `Origin` pass — non-browser clients (curl, server-to-server, mobile SDKs) are unaffected.
- `Origin` matching the `Host` header passes (pre-2023 browsers).
- Everything else is rejected — **including `Sec-Fetch-Site: same-site`**: subdomains are cross-origin, so `app.example.com` → `api.example.com` needs `TrustedOrigins: []string{"https://app.example.com"}`.

**Credo integration:** the middleware calls the detector's `Check` method and routes rejections through the framework error pipeline — the default `ErrorHandler` returns `credo.NewHTTPError(403)` with the detector's reason attached as internal error (RFC 7807 response, reason logged but never exposed). The stdlib deny handler is not used.

**Panics** if a `TrustedOrigins` entry is malformed or an `InsecureBypassPatterns` entry is invalid/conflicting — middleware construction is startup configuration (fail-fast, panic-vs-error policy).

CSRF and CORS are complementary: CORS governs whether a browser may _read_ a cross-origin response; CSRF protection stops state-changing cross-origin requests from being _processed_.

### Planned (Not Yet Implemented)

| Middleware  | Source | Description                     |
| ----------- | ------ | ------------------------------- |
| `BasicAuth` | Echo   | HTTP Basic authentication       |
| `APIKey`    | Echo   | API key (header/query)          |
| `JWT`       | Echo   | JWT token validation            |
| `Metrics`   | GoFr   | Prometheus request metrics      |
| `Tracer`    | GoFr   | OpenTelemetry trace propagation |

### RateLimit Lifecycle

`RateLimit()` is a convenience constructor. For explicit lifecycle management, use `NewRateLimiter(...)` and register shutdown on app stop:

```go
rl := middleware.NewRateLimiter(middleware.RateLimitConfig{Tokens: 120})
app.GlobalMiddleware(rl.Middleware())
app.OnShutdown(rl.Shutdown)
```

---

## Design Decisions

1. **Credo-native signature as primary** — `func(Handler) Handler` provides full access to `credo.Context` and error returns. Stdlib middleware is adapted via `WrapStdMiddleware`, keeping the community ecosystem accessible.

2. **3-tier model from Goyave** — Global/Router/Route tiers give precise control. Global tier solves the 404/405 middleware gap present in Chi/Echo.

3. **Meta-driven behavior from Goyave** — Middleware reads route metadata instead of maintaining allowlists/denylists. Declarative and composable.

4. **Config struct pattern** — `Middleware(cfg ...Config)` for optional configuration. Zero args for defaults, one arg for custom config.

5. **Rewrite lives in middleware, not router config** — Stateless path normalization belongs in the middleware tier so it can be registered, scoped, and composed like other cross-cutting concerns. Conditional internal forwarding remains on `ctx.Rewrite()`.

6. **CSRF via stdlib `CrossOriginProtection`, not token plumbing** — token/double-submit-cookie CSRF requires session state, template helpers, and header plumbing across the stack; `Sec-Fetch-Site` has shipped in all browsers since 2023 and reduces the problem to a header check. Credo wraps the stdlib detector (maintained upstream, security patches ride Go releases) and only adds config-struct ergonomics plus error-pipeline integration. Older-browser fallback (Origin/Host comparison) is inherited from the stdlib.

---

## File Layout

```
middleware/
├── rewrite.go      Pre-dispatch path rewriting
├── accesslog.go    Structured request logger (slog)
├── recover.go      Optional per-group/route panic recovery (built-in recovery is automatic)
├── requestid.go    X-Request-Id injection
├── cors.go         CORS with config struct
├── csrf.go         CSRF via stdlib CrossOriginProtection
├── compress.go     Response compression
├── secure.go       Security headers
├── ratelimit.go    RateLimit + NewRateLimiter API
├── ratelimit_store.go Internal in-memory limiter store
├── timeout.go      Request timeout
├── contractguard.go Declarative per-route request contracts
├── skipper.go      Shared skipper type
├── configresolve.go Shared config default/override/normalize helper
├── doc.go
└── *_test.go       Tests alongside source
```
