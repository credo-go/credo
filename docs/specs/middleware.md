# Middleware Spec

**Status:** Implemented (beta) **Package:** `middleware/` **ADR:** [ADR-010](../adr/010-middleware-architecture.md), [ADR-018](../adr/018-host-routing-and-rewrite.md) **Guide:** [Middleware guide](../guides/middleware.md) **Sources:** Echo (MIT), Goyave (MIT), go-limiter (Apache-2.0)

## Overview

Credo middleware returns `credo.Middleware` (`func(Handler) Handler`). Stdlib middleware works via `WrapStdMiddleware` adapter. A 3-tier execution model (Global / Group / Route) provides fine-grained control over which middleware runs where. URL rewriting is implemented as global middleware when path normalization must happen before route matching.

Panic recovery, request IDs, access logging, response compression, request-body decompression and locale detection are framework-owned HTTP features, not middleware: user middleware runs inside the feature executor, whose order and contract the [HTTP features spec](http-features.md) defines. This document covers the user chain and the `middleware` package.

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

Adapted middleware sees only `*http.Request` and `r.Context()`, never `*credo.Context`. A response it writes to the `ResponseWriter` by itself bypasses Credo's error pipeline; only responses produced by calling `next` flow back through centralized handling ([why](../adr/010-middleware-architecture.md#stdlib-adapter)).

A response the middleware writes by itself is still observed, because the adapter hands it a recording writer. When the middleware returns, `Response.Status()`, `Size()` and `Committed()` report what it wrote and the access record carries that status and byte count. An error returned further up the chain, or a panic in the middleware after it answered, is logged (`credo: error after response committed`) instead of appending an error body to the response already sent. The record follows `net/http`: a 1xx status other than 101 is informational, a body write or a flush without a status commits 200, and the first status wins. A connection the middleware hijacks is reported by `Response.Hijacked()`; the error pipeline then writes nothing to it and the response compressor is abandoned rather than flushed into it.

What the chain writes through `Response` is counted by `Response` alone, so nothing is counted twice; bytes the middleware adds around a response the chain wrote are not part of `Size()`. The recording writer forwards `Flush`, `Hijack`, `ReadFrom` (a file served behind the middleware keeps sendfile) and `Push`, and exposes the underlying writer through `Unwrap` for `http.ResponseController`.

## Framework Features Around the Chain

The request executor wraps the compiled user chain in a fixed order ([request execution](http-features.md#request-execution)). For middleware authors:

- Global middleware already sees the request ID (`ctx.RequestID()`, empty when `UseRequestID` is not installed), the decoded body when `UseDecompress` applied, and writes through the compressing writer when `UseCompress` negotiated an encoding.
- The access record is observed after the chain and cannot be altered or replaced by middleware; select it with `AccessLogConfig` and the `credo.MetaAccessLog` route meta.
- Recovery has no per-group or per-route variant; a group that needs its own panic policy writes an ordinary middleware that recovers and returns an error.
- Middleware that enriches the logger derives from `ctx.Logger()` through `ctx.AddLogAttrs`; a wholesale `ctx.SetLogger` replacement drops the request ID ([request_id sourcing](http-features.md#observability-contract)).

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

A route's chain is assembled when the App prepares to serve ([preparation](lifecycle.md#preparation)), by walking the route's group parent chain — the same model `LookupMeta` uses for metadata. Middleware added to a group after routes were registered or sub-groups created therefore still applies to them. Registration order determines middleware _order_ (parent groups before children, append order within a group), never _membership_. To exclude one route from a group middleware, register it on a sibling group or attach middleware per-route.

### Why Global Tier Matters

Without a global tier, 404/405 responses bypass all group/route middleware — no CORS headers, no security headers. The global tier ensures these cross-cutting concerns always run.

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

## Meta-Driven Middleware (Goyave-inspired)

Middleware can read Route Meta to change behavior per-route declaratively, instead of hardcoding path checks.

```go
// Auth middleware — checks Meta instead of path list
func Auth(next credo.Handler) credo.Handler {
    return func(ctx *credo.Context) error {
        val, _ := ctx.Route().LookupMeta("auth")
        if required, _ := val.(bool); required {
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

`LookupMeta` returns the value and whether any scope set the key; the two-value type assertion treats a missing or non-bool value as `false` instead of panicking. `ctx.Route()` is nil until dispatch has matched a route — in global middleware before `next` runs and on the router's own 404/405 — so register meta-reading middleware on a group or route, as here, or guard the call with `ctx.HasRoute()` ([route meta](router.md#route-meta-system-goyave-inspired)).

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

Register `ContractGuard` as group or route middleware: it reads the matched route's meta, and global middleware runs before a route has matched, so as global middleware the guard does nothing.

`ContractGuard` matches these types exactly; a `[]any` whose elements are all strings is read like a `[]string`. A declared contract is never skipped: when the guard reaches a value of any other type — a named string type (`type Scope string`), `nil`, a `[]any` holding a non-string element, an unsigned or floating-point `MetaMaxBody` — it rejects the request with the generic 500 instead of calling the handler. The cause (route, meta key, the value's Go type and the fix) is the error's internal cause, so it appears once in the server-error log and never in the response. No reflection is involved, and the accepted set does not grow to named types: the declaration converts (`string(scope)`).

A route overrides a contract inherited from its group by setting the key itself. The values that require nothing are `[]string{}` for `MetaRequireHeaders`, `MetaRequireQuery` and `MetaScope`, `"*/*"` for `MetaAccept` (every media type; `RequireContentType` still applies) and a negative `MetaMaxBody`. An empty `MetaAPIVersion` list accepts no version, so that contract can be replaced but not lifted. `nil` is not a contract value and lifts nothing.

## Config Struct Pattern (Echo-inspired)

Each constructor takes `cfg ...XConfig`: no argument selects `DefaultXConfig()`, one argument customizes.

```go
// Default config
app.GlobalMiddleware(middleware.CORS())

// Custom config
app.GlobalMiddleware(middleware.CORS(middleware.CORSConfig{
    AllowOrigins: []string{"https://example.com"},
    AllowMethods: []string{"GET", "POST"},
}))
```

## Middleware Catalog

| Middleware | Source | Description |
| --- | --- | --- |
| `Rewrite` | Credo | Pre-dispatch path rewriting with Credo route syntax |
| `CORS` | Echo | Cross-Origin Resource Sharing; `AllowOrigins` uses the strict origin grammar shared with `websocket` (exact origin or one left-most wildcard label; invalid entries panic at construction) |
| `CSRF` | stdlib wrap | Cross-origin request rejection via `net/http.CrossOriginProtection` (Sec-Fetch-Site based, no tokens) |
| `Secure` | Echo | Security headers (HSTS, CSP, X-Frame). HSTS uses `Request.Scheme()` |
| `RateLimit` | go-limiter | Token bucket rate limiting. Default key uses `Request.RealIP()` |
| `Timeout` | Echo | Request timeout through a cooperative context deadline that handlers observe via `ctx.Context()` |
| `ContractGuard` | Credo | Declarative per-route request contracts (Content-Type, body size, required headers/query, API version, scope) read from route meta |

Authentication is `auth.Middleware[T]` with the `auth` package's authenticators (Basic, API key, JWT); see the [auth spec](auth.md).

### Rewrite

```go
func Rewrite(cfg ...RewriteConfig) credo.Middleware
func DefaultRewriteConfig() RewriteConfig

type RewriteConfig struct {
    Skipper Skipper
    Rules   []RewriteRule
}
```

`middleware.Rewrite` mutates `req.URL.Path` before dispatch so that routing sees the rewritten path on the first lookup. It follows the package-wide config-struct convention (`Middleware(cfg ...Config)`): the single `RewriteConfig` argument carries the `Rules` list and an optional `Skipper`.

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
- `From` uses Credo route syntax (`{name}`, `{name...}`, `{name:regex}`) unless `Regexp` is provided. Brace matching follows the same parser as the router, including regex quantifiers, escaped braces, and character classes. Like a route pattern, it names each parameter once: a repeated name panics with `duplicate parameter name`.
- `To` expands named placeholders (`{name}`) from the matched captures.
- `Host` is an optional exact host filter. Matching is case-insensitive, with request ports stripped before comparison.
- If `To` contains a query string, it replaces the current query string.
- If `To` does not contain a query string and `PreserveQuery` is true, the original query string is preserved.
- `Rewrite()` panics when `Rules` is empty (a rule list is required); `DefaultRewriteConfig()` therefore carries no rules.

**Placement:**

Register `Rewrite` as global middleware when routing must see the rewritten path:

```go
app.GlobalMiddleware(middleware.Rewrite(middleware.RewriteConfig{Rules: []middleware.RewriteRule{
    {From: "/v1/{path...}", To: "/api/v1/{path}"},
}}))
```

At group or route scope it only mutates the request seen by downstream middleware/handler for an already matched route. It does not trigger a re-dispatch loop.

When a handler later calls `ctx.Rewrite()`, framework features and global middleware do not run again. Group and route middleware for the newly matched route do run again, so `after` logic must be written with per-dispatch semantics in mind. The [context spec](context.md#rewrite) defines `ctx.Rewrite`.

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

**Credo integration:** QUERY bypasses Go 1.27's older GET/HEAD/OPTIONS-only safe list, then every other method is passed to the detector's `Check` method. Rejections flow through the framework error pipeline — the default `ErrorHandler` returns `credo.NewHTTPError(403)` with the detector's reason attached as internal error ([default error envelope](../adr/009-handler-and-error-handling.md#default-error-envelope); the reason is logged at debug level and never exposed). The stdlib deny handler is not used.

QUERY being safe and QUERY requiring CORS preflight are separate properties. A browser cannot send a cross-origin fetch/XHR QUERY without preflight, and HTML forms or navigation cannot produce QUERY, so the classic preflight-free CSRF channel does not exist. Treating QUERY as state-changing is a protocol violation; CSRF middleware does not compensate for it. CORS's default method list includes QUERY so a default-config preflight can authorize the request.

No test pins the stdlib's own handling of QUERY ([why](../adr/010-middleware-architecture.md#csrf-through-crossoriginprotection)).

**Panics** if a `TrustedOrigins` entry is malformed or an `InsecureBypassPatterns` entry is invalid/conflicting — middleware construction is startup configuration (fail-fast, panic-vs-error policy).

CSRF and CORS are complementary: CORS governs whether a browser may _read_ a cross-origin response; CSRF protection stops state-changing cross-origin requests from being _processed_.

### RateLimit Lifecycle

`RateLimit()` is a convenience constructor. For explicit lifecycle management, use `NewRateLimiter(...)`: the limiter is a `credo.Component`, so hand it to `app.Manage` and the App shuts it down after the HTTP drain, in the internal tier:

```go
rl := middleware.NewRateLimiter(middleware.RateLimitConfig{Tokens: 120})
app.GlobalMiddleware(rl.Middleware())
app.Manage(rl, credo.Named("ratelimiter"))
```

### Planned (Not Yet Implemented)

| Middleware | Source | Description                     |
| ---------- | ------ | ------------------------------- |
| `Metrics`  | GoFr   | Prometheus request metrics      |
| `Tracer`   | GoFr   | OpenTelemetry trace propagation |

Both arrive with the observability release ([ADR-014](../adr/014-observability.md)).

## Design Decisions

- The single middleware type with its stdlib adapter, the Global/Group/Route tiers, meta-driven behavior, the config struct pattern, CSRF through the stdlib detector, and the framework features outside the chain are recorded in [ADR-010](../adr/010-middleware-architecture.md).
- Pre-dispatch rewrite as middleware rather than router configuration, with conditional internal forwarding on `ctx.Rewrite()`, is recorded in [ADR-018](../adr/018-host-routing-and-rewrite.md#rejected-alternatives).
