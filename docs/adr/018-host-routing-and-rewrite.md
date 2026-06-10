# ADR-018: Host Routing & URL Rewrite

**Status:** Accepted
**Date:** 2026-03-12
**Depends on:** ADR-007, ADR-008, ADR-010

## Context

Credo applications often need to serve multiple domains from one process:
`api.example.com`, `admin.example.com`, tenant subdomains, and a default
website. Middleware-only host checks are possible, but they collapse all
domains into one path tree and make route isolation, 404/405 behavior, route
introspection, and named-route metadata less reliable.

Credo also needs two kinds of URL rewrite behavior:

- **Pre-dispatch path normalization** for legacy URLs, versioned prefixes, and
  vanity paths.
- **Post-match internal forwarding** for handler-driven fallbacks, feature
  flags, and server-side forwards.

These are related but not the same concern.

## Decision

Credo adds host routing as a first-class routing dimension and keeps rewrite as
two separate features:

1. `app.Host(pattern)` creates a host-scoped `*Group` backed by its own mux.
2. `middleware.Rewrite(rules...)` performs pre-dispatch path rewriting.
3. `ctx.Rewrite(path)` performs post-match internal re-dispatch.
4. `ctx.OriginalPath()` preserves the client-sent path across both rewrite forms.

## Host Routing

```go
func (app *App) Host(pattern string) *Group
```

Each registered host pattern gets a dedicated mux. Dispatch first matches the
request host, selects the matching mux, then performs the normal radix-tree path
lookup inside that mux.

### Pattern Syntax

- Static: `api.example.com`
- Param: `{tenant}.example.com`
- Regex: `{tenant:[a-z]+}.example.com`
- Wildcard: `*.acme.io`

Patterns are normalized to lowercase and may not include a port. Incoming
request hosts are normalized by lowercasing, stripping an explicit port, and
trimming a trailing dot.

Wildcard `*` is an anonymous single-label match. It captures no parameter,
must be the leftmost complete label, and may appear at most once. `*` and
`*.io` are valid but broad. `api*.acme.io`, `foo.*.io`,
`*.*.acme.io`, `*.{tenant}.acme.io`, and `{tenant}.*.acme.io` panic
at registration time.

### Matching Rules

- If no host pattern matches, dispatch falls back to the default mux.
- Matching is case-insensitive.
- Exact static hosts use a hash-map fast path. Param, regex, and wildcard
  patterns use the specificity-ordered scan.
- Specificity wins over registration order:
  1. static label
  2. regex-constrained label
  3. param or wildcard label
- Comparison is evaluated right-to-left by label.
- Host patterns with identical match semantics panic at registration time.
  Param names are ignored for this comparison, and wildcard `*` is equivalent
  to an unconstrained param. For example, `{a}.acme.io`, `{b}.acme.io`,
  and `*.acme.io` are identical; `{org:[a-z]+}.acme.io` and
  `{tenant}.acme.io` are not.
- Registration order breaks remaining equal-specificity ties.

### Host Parameters

Host params are merged into `ctx.Request().RouteParams()` together with path
params. Credo does not introduce a separate host-param accessor.

Because host params and path params share one namespace, registration panics
when a host-scoped route reuses the same parameter name in the path.

Wildcard host labels are not host params. They do not appear in
`RouteParams()` and cannot be used by `BuildURL` to generate a concrete host;
use a named param host pattern when URL generation needs the subdomain value.

### Introspection

Route introspection includes host information through `RouteInfo.Host` and
`WalkRoutes`. `app.Mux()` returns a route registry view across the default mux
and all host-scoped muxes.

## Pre-Dispatch Rewrite

```go
func Rewrite(rules ...RewriteRule) credo.Middleware

type RewriteRule struct {
    Host string
    From string
    To   string

    Regexp *regexp.Regexp

    PreserveQuery bool
}
```

`middleware.Rewrite` mutates `req.URL.Path` before routing. Rules are evaluated
in order and the first match wins.

- `From` uses Credo route syntax unless `Regexp` is provided.
- `To` expands named placeholders from matched captures.
- `Host` is an optional exact host filter.
- If `To` contains a query string, it replaces the current query string.
- If `To` does not contain a query string and `PreserveQuery` is true, the
  original query string is preserved.
- `Rewrite()` panics when called with zero rules.

This middleware runs once in the normal middleware chain. It does not create a
loop and does not re-enter global middleware.

## Post-Match Internal Re-Dispatch

```go
func (ctx *Context) Rewrite(path string) error
```

`ctx.Rewrite()` is a handler-level internal forward. The handler returns a
sentinel error (`errRewrite`), and the leaf wrapper in the compiled route chain
swallows it before it reaches user middleware. Dispatch then:

1. reads the rewrite target from Context,
2. updates `req.URL.Path` / `req.URL.RawQuery`,
3. clears the previous route params,
4. re-runs route matching.

Rules:

- The rewrite target must begin with `/`.
- Re-dispatch stays within the same matched host scope.
- Group and route middleware run again for the newly matched route.
- Built-in and global middleware do not run again.
- A hard limit of 10 rewrites prevents loops.
- If the limit is exceeded, dispatch returns an error and the normal error
  pipeline produces a 500 response.
- If the response is already committed, `ctx.Rewrite()` returns an error.

## Original Path Preservation

```go
func (ctx *Context) OriginalPath() string
```

Context captures the original path once in `reset()`, before any middleware or
dispatch runs. The value is immutable for the lifetime of the request.

Both built-in access logging and `middleware.AccessLog` include a
`path_original` attribute when the final served path differs from the original
path.

## Consequences

**Positive:**

- Host routing remains explicit and isolated without modifying the radix tree.
- Existing `*Group` semantics are reused for host-scoped routing.
- Rewrite is available in both stateless (middleware) and handler-driven
  (context) forms.
- Original-path preservation improves debugging, analytics, and logging.
- Introspection can report host-aware route information.

**Negative:**

- Param/regex host matching adds a small linear scan before path lookup.
  Exact static hosts use a hash-map fast path.
- Shared route-param namespace requires registration-time collision checks.
- `ctx.Rewrite()` re-runs group/route middleware, so middleware with side
  effects must tolerate per-dispatch execution.
- `BuildURL` auto-fills the host from the route's host pattern; host params
  are consumed first, then path params. Wildcard host patterns are rejected by
  `BuildURL` because they do not carry a concrete label value.
