# Middleware Spec

**Status:** Implemented (beta) **Package:** `middleware/`, `middleware/ratelimit/` **ADR:** [ADR-010](../adr/010-middleware-architecture.md), [ADR-018](../adr/018-host-routing-and-rewrite.md) **Guide:** [Middleware guide](../guides/middleware.md) **Sources:** Echo (MIT), Goyave (MIT), go-limiter (Apache-2.0)

> **Pending v0.26.0 (R14):** this spec is not yet split into its normative contract and current behaviour; until R14 ships, every section describes current behaviour.

## Overview

> **Current until v0.26.0 (R13):** the next paragraph's 3-tier model and its global placement of URL rewriting.

Credo middleware returns `credo.Middleware` (`func(Handler) Handler`). Stdlib middleware works via `WrapStdMiddleware` adapter. A 3-tier execution model (Global / Group / Route) provides fine-grained control over which middleware runs where. URL rewriting is implemented as global middleware when path normalization must happen before route matching.

> **Pending v0.26.0 (R13):** the next paragraph replaces the one above; until R13 ships, the tiers are Global / Group / Route.

Credo middleware returns `credo.Middleware` (`func(Handler) Handler`). Stdlib middleware works via `WrapStdMiddleware` adapter. Four tiers — PreRouting, routed, Group, Route — decide which middleware runs where ([tiers](#tiers-prerouting-routed-group-route)). URL rewriting runs before routing, on `PreRouting`, since it changes the path routing matches.

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

> **Pending v0.26.0 (R4):** the next paragraph's boundary is not implemented yet; until R4 ships, the adapter hands the middleware the embedded request, whose context does not carry the values set through `ctx.Set`, and a context the middleware derives does not reach `ctx.Context()`.

The adapter hands the request on through `c.Request().Std()`, whose context is the Context's current head, and takes the context of the request the middleware passes on as the head of the inner chain. A value set before the middleware is read there with `Value` and its key, and a deadline or span the middleware adds is visible to the handler through `c.Context()`; when the middleware returns, the head reverts to the outer one and the values set inside are applied again over it. The contract excludes a middleware that runs the next handler on another goroutine, as `http.TimeoutHandler` does: the Context is not safe for concurrent use, and the goroutine would move the head while the outer layer reverts it. A middleware that passes on a context not derived from the request's — which drops the request's cancellation too — cuts the handlers after it off from the values set before it, through `c.Get` as through `Value`. The user's key stays unexported, so the middleware cannot read the user ([ADR-012](../adr/012-authentication-and-authorization.md)).

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

> **Current until v0.26.0 (R13):** this section's table, its execution order and "Why Global Tier Matters"; [Tiers](#tiers-prerouting-routed-group-route) replaces them, with `PreRouting` in place of `GlobalMiddleware`.

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
api.Middleware(ratelimit.Middleware())
```

## Tiers: PreRouting, routed, Group, Route

> **Pending v0.26.0 (R13):** not implemented yet; `app.Middleware` and `app.PreRouting` do not exist, the tier before routing is `app.GlobalMiddleware`, and a mounted handler is dispatched raw, with global middleware only.

| Tier | Registration | Scope | Runs on 404/405? |
| --- | --- | --- | --- |
| **PreRouting** | `app.PreRouting(m...)` | Every request, before the match | **Yes** |
| **Routed** | `app.Middleware(m...)` | Every route registered through the App — directly, in a group, in a host group, as a static route or as a mount — after the match | No |
| **Group** | `group.Middleware(m...)` | Routes under this group | No |
| **Route** | `route.Middleware(m...)` | Single route only | No |

```
Request
  → Framework features (request ID, decompression, compression writer)
    → PreRouting middleware (outer to inner)
      → match
        → Routed middleware (outer to inner)
          → Group middleware (outer to inner, parent to child)
            → Route middleware (outer to inner)
              → Handler
  ← Framework error rendering, recovery, finalization, observers
Response
```

`app.Middleware` delegates to the root group's `Middleware`: the App's routes, groups and host groups descend from that group, and group middleware is collected when the chain is compiled, so a routed middleware added after a group's routes were registered applies to them too. The routed tier reads the route's meta and never runs for the router's own 404 and 405, which reach `PreRouting` only. After preparation either call panics, as every registration does.

**A mount is a route.** A request a `Mount` matches carries a `*Route` of kind mount — the mount's pattern, the App's meta, no name — so `HasRoute` is true for it, App-level meta such as `MetaAccessLog` applies to it, its access record names the mount's pattern, and its handler runs inside the routed tier through a chain compiled as a route's is. A mount belongs to no group and has no route value of its own, so group and route middleware do not apply to it; authentication on the routed tier answers 401 for a mounted path.

**What stays before routing.** A middleware that acts before the match, or on a request no route matches, stays on `PreRouting`; one that reads the route goes on the routed tier or below:

| Middleware | Tier | Why |
| --- | --- | --- |
| `CORS` | PreRouting | answers a preflight, an `OPTIONS` request for which the router has no route; on the routed tier the router's 405 would answer it |
| `Rewrite` | PreRouting | changes the path that routing matches |
| `Secure` | PreRouting | its headers cover the router's 404 and 405 |
| `ratelimit.Middleware` | PreRouting | its budget covers the router's 404 and 405 |
| `CSRF` | either | needs no route |
| `auth.Middleware`, `ContractGuard` | routed, Group or Route | read the route and its meta |

```go
app.PreRouting(middleware.CORS(), middleware.Secure())
app.Middleware(middleware.ContractGuard()) // reads every route's meta; never runs for a 404 or a 405
```

## Meta-Driven Middleware (Goyave-inspired)

Middleware can read Route Meta to change behavior per-route declaratively, instead of hardcoding path checks.

> **Current until v0.26.0 (R4):** the next example and the paragraph after it, which key meta by string and read it with `LookupMeta`.

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

> **Pending v0.26.0 (R4):** the next example and paragraph replace the two above; until R4 ships, meta is keyed by string and read with `LookupMeta`.

```go
var RequireAuth = credo.NewKey[bool]("auth")

func Auth(next credo.Handler) credo.Handler {
    return func(c *credo.Context) error {
        if required, _ := c.Route().GetMeta(RequireAuth); required {
            if !validateToken(c.Request().Header.Get("Authorization")) {
                return credo.NewHTTPError(http.StatusUnauthorized)
            }
        }
        return next(c)
    }
}

api := app.Group("/api")
api.SetMeta(RequireAuth, true).Middleware(Auth)

api.GET("/users", listUsers)                               // authenticated
api.GET("/health", healthCheck).SetMeta(RequireAuth, false) // not authenticated
```

A meta key is a `credo.Key[T]` made by `credo.NewKey[T]`, the key type of request values too; `SetMeta` with a value of another type does not compile, and `GetMeta` returns `(T, bool)`, the zero value and false when no scope set the key. The key's name is for diagnostics; identity is the key value, so the keys of two packages never collide. A route's meta is resolved into a per-route snapshot when its chain is compiled — the route, then its groups, then the App — and a route's `RemoveMeta` lets its group's value show through. `Group.SetMeta` returns its `*Group`, as `Route.SetMeta` does. `c.Route()` is nil until dispatch has matched a route — in `PreRouting` middleware before `next` runs and on the router's own 404/405 — so meta-reading middleware is registered on the routed tier, a group or a route.

### Framework Meta Keys

These keys are read by framework features and framework middleware. Application middleware may also define its own convention keys — the `"auth"` key in the example above is one such user-defined key, not a framework key.

> **Current until v0.26.0 (R4):** the next table's string keys and their accepted value types; from R4 each is a typed meta key, as the paragraph after the table states.

| Key | Type | Used by |
| --- | --- | --- |
| `credo.MetaAccessLog` (`"credo.accesslog"`) | `bool` (`false` silences) | Access log feature (`app.UseAccessLog`) |
| `middleware.MetaAccept` (`"accept"`) | `string` \| `[]string` | `ContractGuard` — 415 on Content-Type mismatch; a missing/empty header passes unless `ContractConfig.RequireContentType` is set and the request carries a body |
| `middleware.MetaMaxBody` (`"max_body"`) | `int` \| `int32` \| `int64` (bytes) | `ContractGuard` — 413 over the per-route cap |
| `middleware.MetaRequireHeaders` (`"require_headers"`) | `string` \| `[]string` | `ContractGuard` — 400 if a header is missing |
| `middleware.MetaRequireQuery` (`"require_query"`) | `string` \| `[]string` | `ContractGuard` — 400 if a query param is missing |
| `middleware.MetaAPIVersion` (`"api_version"`) | `string` \| `[]string` | `ContractGuard` — 400 on version mismatch |
| `middleware.MetaScope` (`"scope"`) | `string` \| `[]string` | `ContractGuard` — 403 (requires `ScopeChecker`) |

> **Pending v0.26.0 (R4):** until R4 ships, the keys are strings and the guard checks each value's type at request time, as the paragraphs below describe.

`credo.MetaAccessLog`, `MetaAccept`, `MetaMaxBody`, `MetaRequireHeaders`, `MetaRequireQuery`, `MetaAPIVersion` and `MetaScope` keep their names and become typed meta keys, so a value of another type does not compile where today the guard rejects it at request time. The request ID moves from its string key to a typed one; `c.RequestID()` stays its accessor.

> **Current until v0.26.0 (R13):** the next paragraph; from R13 the guard goes on `app.Middleware`, a group or a route, and registered before routing it fails closed.

Register `ContractGuard` as group or route middleware: it reads the matched route's meta, and global middleware runs before a route has matched, so as global middleware the guard does nothing.

> **Pending v0.26.0 (R13):** until R13 ships, `ContractGuard` before routing lets every request through, as the paragraph above says.

Register `ContractGuard` on the routed tier, `app.Middleware`, or on a group or a route: it reads the matched route's meta. Registered with `PreRouting`, it sees no route on any request; it answers 500 through the error pipeline and logs one Error naming its registration before routing and the remedy, `app.Middleware`, a group or a route.

> **Pending v0.26.0 (R2):** until R2 ships, the guard's rejections pass literal text as a message key, and a missing scope is named in the key.

Each rejection renders its code's built-in message, with its value in `Details`: the rejected content type, which is client input, the header or query parameter the route requires, and the missing scope. No rejection carries a message key of its own ([ADR-009](../adr/009-handler-and-error-handling.md#one-error-channel-keys-messages-and-fault-codes)).

> **Current until v0.26.0 (R4):** the next paragraph, the request-time type check that typed meta keys replace with the compiler's.

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
| `Timeout` | Echo | Request timeout through a cooperative context deadline that handlers observe via `ctx.Context()` |
| `ContractGuard` | Credo | Declarative per-route request contracts (Content-Type, body size, required headers/query, API version, scope) read from route meta |

> **Pending v0.26.0 (R4):** until R4 ships, `Timeout` assigns `ctx.Request().Request` a request with the deadline's context and restores the earlier request when the handler returns, so a user set inside it is lost to the outer layers.

**`Timeout` opens a scope.** `Timeout` sets its deadline with one call, `c.Within(ctx, next)`, on the request's own goroutine: the inner chain reads the deadline through `c.Context()` and `c.Request().Context()`, the head reverts to the outer one when `next` returns or panics, and a value set inside — a user, a tenant — is applied again over the outer head, so it reaches the outer layers, the error rendering and the observers. A middleware that assigns `c.Request().Request` a request with a new context no longer moves the head; in debug mode the Context reports it once per route.

> **Pending v0.26.0 (R2):** until R2 ships, `Timeout` sets a message key of its own on its 503.

`Timeout`'s 503 carries the code `request_timeout` and no message key, so `I18nConfig.ResolveMessageKey` maps it as every other code ([i18n spec](i18n.md#message-key-resolution)).

Rate limiting is `ratelimit.Middleware` in its own package, [`middleware/ratelimit`](#rate-limiting-middlewareratelimit) (go-limiter; the default key is `Request.RealIP()`). Authentication is `auth.Middleware[T]` with the `auth` package's authenticators (Basic, API key, JWT); see the [auth spec](auth.md).

### Rewrite

```go
func Rewrite(cfg ...RewriteConfig) credo.Middleware
func DefaultRewriteConfig() RewriteConfig

type RewriteConfig struct {
    Skipper Skipper
    Rules   []RewriteRule
}
```

> **Current until v0.26.0 (R7):** the next paragraph's first sentence, under which `Rewrite` matches and sets the decoded `URL.Path`.

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

> **Pending v0.26.0 (R7):** not implemented yet; until R7 ships, `Rewrite` matches the decoded `URL.Path`, so a rule can match across an encoded slash.

**Wire form.** `Rewrite` matches on the canonical wire form the router matches, not the decoded `URL.Path`, and sets the rewritten path in wire form, keeping `Path` and `RawPath` consistent, as ADR-018 says its targets are ([ADR-018](../adr/018-host-routing-and-rewrite.md)). An encoded `%2F` inside a segment stays data and is never a separator, so a single-segment pattern matches it and the rewrite keeps the escape; a catch-all keeps its slashes; a captured value is decoded once, as a route parameter is. The router sees the same segments after the rewrite as the client sent, and a malformed escape in a target is an error.

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

> **Current until v0.26.0 (R14):** the next paragraph; `Context.Rewrite` and `IsRewriting` go, and a client route is served through static's SPA mode or mapped by `Rewrite` before routing.

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

### Rate Limiting (`middleware/ratelimit`)

```go
package ratelimit // github.com/credo-go/credo/middleware/ratelimit

func Middleware(cfg ...Config) credo.Middleware
func DefaultConfig() Config

type Config struct {
    Skipper              middleware.Skipper
    Store                limiter.Store // default: in-memory store with Tokens/Interval
    Tokens               uint64        // default 60
    Interval             time.Duration // default 1 minute
    KeyFunc              func(ctx *credo.Context) (string, error) // default: Request.RealIP()
    InternalErrorHandler func(ctx *credo.Context, err error) error
    DeniedHandler        func(ctx *credo.Context, limit, remaining uint64, reset time.Time) error
}
```

Token-bucket rate limiting over go-limiter lives in a package of its own, so that importing `middleware` — for `CORS`, say — does not link go-limiter: the dependency graph of `middleware` contains no go-limiter package. The package stays in the root module. `Middleware` follows the configuration-struct convention (zero arguments for the defaults, one to customize) and the naming of `auth.Middleware`; `Config.Skipper` keeps the type `middleware.Skipper`.

| `middleware` before | `middleware/ratelimit` |
| --- | --- |
| `RateLimit(cfg...)` | `Middleware(cfg...)` |
| `RateLimitConfig` | `Config` |
| `DefaultRateLimitConfig()` | `DefaultConfig()` |
| `NewRateLimiter(cfg)`; `RateLimiter` with `Middleware`, `Close` and `Shutdown` | none: `Middleware(cfg...)` is the one constructor |

**One budget per value.** The middleware `Middleware` returns shares its store across every route it is attached to: one value attached to two routes spends one budget, and two calls build two budgets.

```go
limit := ratelimit.Middleware(ratelimit.Config{Tokens: 120})
app.GET("/search", search).Middleware(limit)
app.GET("/export", export).Middleware(limit) // spends the /search budget
```

**Lifecycle.** The rate limiter is not a component, so nothing is handed to `app.Manage` ([why](../adr/010-middleware-architecture.md#rate-limiting-one-constructor-in-its-own-package)). Without `Config.Store`, `Middleware` creates an in-memory store that the middleware owns for its lifetime; the store runs no goroutine — expired buckets are swept inline during `Take` — so nothing must be stopped at shutdown. The in-memory store keeps `Close`, which `limiter.Store` requires. A store passed in through `Config.Store` stays its owner's: the middleware never closes it, and a store that holds a connection or a goroutine is registered by the application as a component of its own.

### Planned (Not Yet Implemented)

| Middleware | Source | Description                     |
| ---------- | ------ | ------------------------------- |
| `Metrics`  | GoFr   | Prometheus request metrics      |
| `Tracer`   | GoFr   | OpenTelemetry trace propagation |

Both arrive with the observability release ([ADR-014](../adr/014-observability.md)).

## Design Decisions

- The single middleware type with its stdlib adapter, the Global/Group/Route tiers, meta-driven behavior, the config struct pattern, CSRF through the stdlib detector, and the framework features outside the chain are recorded in [ADR-010](../adr/010-middleware-architecture.md).

> **Current until v0.26.0 (R14):** the next item's "with conditional internal forwarding on `ctx.Rewrite()`".

- Pre-dispatch rewrite as middleware rather than router configuration, with conditional internal forwarding on `ctx.Rewrite()`, is recorded in [ADR-018](../adr/018-host-routing-and-rewrite.md#rejected-alternatives).
