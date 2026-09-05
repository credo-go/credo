# Middleware Spec

**Status**: Approved **Package**: `middleware/` **Sources**: Chi (MIT), Echo (MIT), Goyave (MIT) **Depends on**: Root package **ADRs**: [010-middleware-architecture](../adr/010-middleware-architecture.md), [018-host-routing-and-rewrite](../adr/018-host-routing-and-rewrite.md)

---

## Overview

Credo middleware returns `credo.Middleware` (`func(Handler) Handler`). Stdlib middleware works via `WrapStdMiddleware` adapter. A 3-tier execution model (Global / Group / Route) provides fine-grained control over which middleware runs where. URL rewriting is implemented as middleware at the global/group layer when path normalization must happen before route matching.

Panic recovery, request IDs, access logging, response compression, request-body decompression and locale detection are not middleware. They are framework-owned HTTP features of the root package, installed once per App (`WithRecoverConfig`/`WithoutRecover` for recovery, `app.UseRequestID`, `app.UseAccessLog`, `app.UseCompress`, `app.UseDecompress`, `app.UseI18n` for the rest) and run by one request executor in a fixed order around the whole user chain. The [HTTP features spec](http-features.md) is their contract; this document covers the user chain and the `middleware` package.

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

Adapted stdlib middleware is deliberately second-class: it sees only `*http.Request` and `r.Context()`, never `*credo.Context`, so it cannot read route Meta, the typed principal (`ctx.GetUser[T]`), or the renderer. If it short-circuits by writing to the `ResponseWriter` directly, that response bypasses Credo's error pipeline — only responses produced by calling `next` flow back through centralized handling. The first-class path for anything that needs the principal or the error pipeline is a native `func(Handler) Handler`. See [ADR-010](../adr/010-middleware-architecture.md) and [ADR-012](../adr/012-authentication-and-authorization.md).

---

## Framework Features Around the Chain

The request executor (`executor.go`) wraps the compiled user chain. Per admitted request it runs, in this fixed order and independently of `Use*` call order:

```
RequestID → AccessLog start → Decompress → Compress writer
  → Global middleware → dispatch → Group middleware → Route middleware → Handler
  → centralized error rendering → recovery → compressor finalization
  → AccessLog observation → Context release
```

Consequences for middleware authors:

- Global middleware already sees the request ID (`ctx.RequestID()`, empty when `UseRequestID` is not installed), the decoded body when `UseDecompress` applied, and writes through the compressing writer when `UseCompress` negotiated an encoding. Error envelopes produced after the chain are compressed like handler output.
- The access record is observed after error rendering, recovery and compressor finalization, so its status, bytes and duration are final. Middleware cannot alter or replace it; use `AccessLogConfig` (Skipper, MinLevel, ResultFilter, Logger) and the `credo.MetaAccessLog` route meta for selection.
- Recovery is the outermost layer and covers middleware, handlers and every feature callback. There is no per-group or per-route recovery; a group that needs its own panic policy writes an ordinary middleware that recovers and returns an error.
- The `request_id` attribute is added to access and panic records only when the target logger does not already carry it (a dedicated logger, or no request-scoped logger). Middleware that enriches the logger should derive from `ctx.Logger()` through `ctx.AddLogAttrs`; a wholesale `ctx.SetLogger` replacement drops the ID and the framework cannot detect it.

---

## 3-Tier Model (Goyave-inspired)

| Tier | Registration | Scope | Runs on 404/405? |
| --- | --- | --- | --- |
| **Global** | `app.GlobalMiddleware(m...)` | Every request | **Yes** |
| **Group** | `group.Middleware(m...)` | Routes under this group | No |
| **Route** | `route.Middleware(m...)` | Single route only | No |

### Execution Order

```
Request
  → Framework features (request ID, decompression, compression writer)
    → Global middleware (outer to inner)
      → Group middleware (outer to inner, parent to child)
        → Route middleware (outer to inner)
          → Handler
        ← Route middleware
      ← Group middleware
    ← Global middleware
  ← Framework error rendering, recovery, finalization, access log
Response
```

### Group Middleware Is Collected at Compile Time

A route's chain is assembled when the app compiles (at `Run()` or the first request), by walking the route's group parent chain — the same model `LookupMeta` uses for metadata. Middleware added to a group after routes were registered or sub-groups created therefore still applies to them. Registration order determines middleware _order_ (parent groups before children, append order within a group), never _membership_. To exclude one route from a group middleware, register it on a sibling group or attach middleware per-route.

### Why Global Tier Matters

Without a global tier, 404/405 responses bypass all group/route middleware — no CORS headers, no security headers. The global tier ensures these cross-cutting concerns always run. (Recovery, request IDs, access logging and compression are framework features and cover 404/405 as well.)

```go
app, err := credo.New()
if err != nil {
    panic(err)
}

// Framework features: recovery is on; the rest are explicit.
app.UseRequestID()
app.UseAccessLog()
app.UseCompress()

// Global middleware runs on every request, 404/405 included:
app.GlobalMiddleware(
    middleware.CORS(),
    middleware.Secure(),
)

// These run only on matched routes within the group
api := app.Group("/api")
api.Middleware(middleware.RateLimit())
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

These keys are read by framework features and framework middleware. Application middleware may also define its own convention keys — the `"auth"` key in the example above is one such user-defined key, not a framework key.

| Key | Type | Used by |
| --- | --- | --- |
| `credo.MetaAccessLog` (`"credo.accesslog"`) | `bool` (`false` silences) | Access log feature (`app.UseAccessLog`) |
| `middleware.MetaAccept` (`"accept"`) | `string` \| `[]string` | `ContractGuard` — 415 on Content-Type mismatch; a missing/empty header passes unless `ContractConfig.RequireContentType` is set and the request carries a body |
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

## Middleware Catalog

| Middleware | Source | Description |
| --- | --- | --- |
| `Rewrite` | Credo | Pre-dispatch path rewriting with Credo route syntax |
| `CORS` | Echo | Cross-Origin Resource Sharing; `AllowOrigins` uses the strict origin grammar shared with `websocket` (exact origin or one left-most wildcard label; invalid entries panic at construction) |
| `CSRF` | stdlib wrap | Cross-origin request rejection via `net/http.CrossOriginProtection` (Sec-Fetch-Site based, no tokens) |
| `Secure` | Echo | Security headers (HSTS, CSP, X-Frame). HSTS uses `Request.Scheme()` |
| `RateLimit` | go-limiter | Token bucket rate limiting. Default key uses `Request.RealIP()` |
| `Timeout` | Echo | Request timeout |
| `ContractGuard` | Credo | Declarative per-route request contracts (Content-Type, body size, required headers/query, API version, scope) read from route meta |

Recovery, request IDs, access logging, response compression and request decompression are framework features, not entries in this catalog; see the [HTTP features spec](http-features.md).

### Rewrite

```go
func Rewrite(cfg ...RewriteConfig) credo.Middleware
func DefaultRewriteConfig() RewriteConfig

type RewriteConfig struct {
    Skipper Skipper
    Rules   []RewriteRule
}
```

`middleware.Rewrite` mutates `req.URL.Path` before dispatch so that routing sees the rewritten path on the first lookup. It follows the package-wide config-struct convention (`Middleware(cfg ...Config)`): the single `RewriteConfig` argument carries the `Rules` list and an optional `Skipper`; there is no rule-list variadic form and no `RewriteWithConfig` twin.

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
- `Rewrite()` panics when `Rules` is empty (a rule list is required); `DefaultRewriteConfig()` therefore carries no rules.

**Placement:**

Register rewrite as global middleware when it should affect routing for the whole app:

```go
app.GlobalMiddleware(middleware.Rewrite(middleware.RewriteConfig{Rules: []middleware.RewriteRule{
    {From: "/v1/{path...}", To: "/api/v1/{path}"},
}}))
```

Register `Rewrite` as global middleware when routing must see the rewritten path. When attached at group or route scope, it only mutates the request seen by downstream middleware/handler for an already matched route. It does not trigger a re-dispatch loop.

When a handler later calls `ctx.Rewrite()`, framework features and global middleware do not run again. Group and route middleware for the newly matched route do run again, so `after` logic must be written with per-dispatch semantics in mind.

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

**Semantics (stdlib detector plus Credo's QUERY compatibility layer):**

- `GET`/`HEAD`/`OPTIONS`/`QUERY` always pass (safe methods — handlers must not perform state changes in them).
- `Sec-Fetch-Site: same-origin` / `none` pass.
- Requests with neither `Sec-Fetch-Site` nor `Origin` pass — non-browser clients (curl, server-to-server, mobile SDKs) are unaffected.
- `Origin` matching the `Host` header passes (pre-2023 browsers).
- Everything else is rejected — **including `Sec-Fetch-Site: same-site`**: subdomains are cross-origin, so `app.example.com` → `api.example.com` needs `TrustedOrigins: []string{"https://app.example.com"}`.

**Credo integration:** QUERY bypasses Go 1.27's older GET/HEAD/OPTIONS-only safe list, then every other method is passed to the detector's `Check` method. Rejections flow through the framework error pipeline — the default `ErrorHandler` returns `credo.NewHTTPError(403)` with the detector's reason attached as internal error (default Credo envelope, reason logged but never exposed). The stdlib deny handler is not used.

QUERY being safe and QUERY requiring CORS preflight are separate properties. A browser cannot send a cross-origin fetch/XHR QUERY without preflight, and HTML forms or navigation cannot produce QUERY, so the classic preflight-free CSRF channel does not exist. Treating QUERY as state-changing is a protocol violation; CSRF middleware does not compensate for it. CORS's default method list includes QUERY so a default-config preflight can authorize the request.

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

7. **Framework features are not middleware** — Recovery, request IDs, access logging, compression, decompression and locale detection need to see the final response (after error rendering and recovery), the original request (before rewrites and body transformation), or both. A middleware position cannot give them that, so the root package owns them with one registration each and one fixed execution plan; the per-route/per-group variants that used to live in this package were removed rather than kept as compatibility wrappers ([ADR-010](../adr/010-middleware-architecture.md#built-in-http-feature-configuration-criterion)).

---

## File Layout

```
middleware/
├── rewrite.go      Pre-dispatch path rewriting
├── cors.go         CORS with config struct
├── csrf.go         CSRF via stdlib CrossOriginProtection
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
