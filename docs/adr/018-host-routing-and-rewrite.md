# ADR-018: Host Routing & URL Rewrite

**Status:** Accepted **Date:** 2026-03-12 **Depends on:** ADR-007, ADR-008, ADR-010 **Related:** ADR-009, ADR-017 **Specification:** [Router spec](../specs/router.md), [Context spec](../specs/context.md), [Middleware spec](../specs/middleware.md) **Guide:** [Routing guide](../guides/routing.md)

## Context

Credo applications often need to serve multiple domains from one process: `api.example.com`, `admin.example.com`, tenant subdomains, and a default website. Middleware-only host checks are possible, but they collapse all domains into one path tree and make route isolation, 404/405 behavior, route introspection, and named-route metadata less reliable.

Credo also needs two kinds of URL rewrite behavior:

- **Pre-dispatch path normalization** for legacy URLs, versioned prefixes, and vanity paths.
- **Post-match internal forwarding** for handler-driven fallbacks, feature flags, and server-side forwards.

These are related but not the same concern.

## Decision

> **Current until v0.26.0 (R14):** the next paragraph and its list keep rewrite as two features, the second being `ctx.Rewrite`.

Credo adds host routing as a first-class routing dimension and keeps rewrite as two separate features:

1. `app.Host(pattern)` creates a host-scoped `*Group` backed by its own mux.
2. `middleware.Rewrite(cfg)` performs pre-dispatch path rewriting (`RewriteConfig{Rules, Skipper}`).
3. `ctx.Rewrite(path)` performs post-match internal re-dispatch.
4. `ctx.OriginalPath()` preserves the client-sent path across both rewrite forms.

> **Pending v0.26.0 (R14):** the next paragraph and its list replace the two above when `Context.Rewrite` goes.

Credo adds host routing as a first-class routing dimension and rewrites a path only before routing:

1. `app.Host(pattern)` creates a host-scoped `*Group` backed by its own mux.
2. `middleware.Rewrite(cfg)` performs pre-dispatch path rewriting (`RewriteConfig{Rules, Skipper}`).
3. `ctx.OriginalPath()` preserves the client-sent path across the rewrite.

### Host Routing

Each registered host pattern gets a dedicated mux. Dispatch first matches the request host, selects the matching mux, then performs the normal radix-tree path lookup inside that mux; a request whose host matches no pattern falls back to the default mux. Keeping the host out of the radix tree leaves path matching unchanged and isolates each domain's routes, 404/405 answers and introspection. Returning a `*Group` reuses prefixes, middleware, meta and names for host-scoped routes with no new API. Pattern syntax (static, `{param}`, `{param:regex}`, a leftmost `*`), normalization and the matching rules: [router spec](../specs/router.md#host-based-route-groups).

When several patterns could match, the most specific wins — a static label over a regex-constrained one over a param or wildcard, compared right to left — rather than the first registered, so a broader pattern registered earlier never shadows a more specific one; registration order only breaks ties between equally specific patterns with different semantics (two regex-constrained labels, say). Host patterns with identical match semantics panic at registration: only registration order would decide between them, and one of them could never be reached. Exact static hosts use a hash-map lookup, so the common case pays no scan.

Host params share one namespace with path params and are read through the same `RouteParam`/`RouteParams` accessors; a separate host-param accessor is deliberately not added, so a handler reads every capture the same way. A name used by both the host and the path therefore panics at registration. A wildcard label captures nothing: it matches only, and a route that needs to generate its subdomain uses a named host param. Introspection reports each route's host pattern (`RouteInfo.Host`) across the default mux and every host-scoped mux.

### Pre-Dispatch Rewrite

`middleware.Rewrite` rewrites the request path before routing, so the first lookup already sees the rewritten path. It is middleware rather than router configuration, so it is registered, scoped and composed like other cross-cutting concerns. It runs once in the normal middleware chain: it does not create a loop and does not re-enter global middleware. Rules are evaluated in order and the first match wins, so overlapping rules resolve predictably. `RewriteConfig`, `RewriteRule` and placement: [middleware spec](../specs/middleware.md#rewrite).

> **Pending v0.26.0 (R7):** not implemented yet; `middleware.Rewrite` matches its rules against the decoded `URL.Path` and sets the rewritten path with `RawPath` cleared, so an encoded slash in the path is a separator to a rule and a captured `%2F` becomes a separator in the target.

**Rewrite matches the wire form.** `middleware.Rewrite` matches on the canonical wire form the router matches ([URL Round Trip](007-router-and-routing.md#url-round-trip)), not on the decoded `URL.Path`, and sets the rewritten path in wire form, keeping `Path` and `RawPath` consistent. An encoded `%2F` inside a segment stays data and is never a separator, so a single-segment pattern matches it and the rewrite keeps the escape; a catch-all keeps its slashes; a captured value is decoded once, as a route parameter is; and the router sees, after the rewrite, the segments the client sent. A malformed escape in a target is an error. Rewrite and routing therefore agree on what a segment is, as they already agree on a catch-all's empty rest.

**Problem.** The middleware matches the decoded `URL.Path`, where an encoded slash has become a separator: a rule matches across a slash the client sent as data, a single-segment pattern does not match a segment that contains `%2F`, and the rewritten path, set with `RawPath` cleared, turns the captured escape into a separator, so the router sees segments the client never sent.

**Removes.** Matching on the decoded path: a rule that matched across an encoded slash no longer does.

> **Current until v0.26.0 (R14):** this section describes `ctx.Rewrite`, which goes; the section after it replaces it.

### Post-Match Internal Re-Dispatch

`ctx.Rewrite(path)` is a handler-level internal forward for decisions that need handler logic. The handler API stays `return ctx.Rewrite(target)`: the call returns an internal sentinel that the compiled route chain swallows before user middleware sees it, and dispatch owns the loop, re-matching the target within the host scope the request already selected. Framework features and global middleware run once per request, while group and route middleware run again for the newly matched route, since that route may carry different policy. A hard limit of 10 rewrites per request stops loops. The complete rule list (target form, query replacement, committed responses, the limit's error): [context spec](../specs/context.md#rewrite).

### No Post-Match Re-Dispatch

> **Pending v0.26.0 (R14):** not implemented yet; `Context.Rewrite` and `IsRewriting` are exported and work as the section above states.

A path is rewritten before routing or not at all. `middleware.Rewrite` stays, for a path mapped before routing: legacy URLs, versioned prefixes and vanity paths are decided by the path alone, and the first lookup already sees the mapped path. The common post-match cases have their own path: static's SPA mode serves the index for a client route ([static spec](../specs/static.md)), and a handler whose decision needs handler logic calls the target's handler itself. Kept, the post-match re-dispatch would freeze at v1 with its contract — the pending signal, at most ten re-dispatches, the wire-form target, group and route middleware run again — while adding it back later is additive.

**Problem.** `Context.Rewrite` is a second rewrite mechanism with a contract of its own: an internal sentinel the compiled chain swallows, a loop owned by dispatch with a limit of ten, a wire-form target, and group and route middleware that run once per dispatch round, so middleware with side effects must check `IsRewriting`. Nothing in the repository calls it outside its own tests.

**Removes.** `Context.Rewrite` and `IsRewriting`: a `ctx.Rewrite(path)` becomes a `middleware.Rewrite` rule or static's SPA mode, or a handler that calls the target's handler itself.

### Original Path Preservation

`ctx.OriginalPath()` reports the client-sent wire-form path, captured once before any middleware or rewrite runs and immutable for the request's lifetime, so logs, analytics and debugging see what the client asked for whichever rewrite form ran; the access log adds `path_original` when the served path differs ([context spec](../specs/context.md#originalpath)).

## Rejected Alternatives

> **Current until v0.26.0 (R14):** the list's last two items keep `ctx.Rewrite` as the home of conditional forwarding: the third keeps its first clause and drops the one naming `ctx.Rewrite()`, the fourth goes, and [No Post-Match Re-Dispatch](#no-post-match-re-dispatch) states why.

- _Host labels inside the radix tree_: rejected — it bakes host logic into the path tree every route lookup walks; a selector over per-host muxes keeps the tree unchanged and the domains isolated.
- _Middleware-only host checks_: rejected — every domain shares one path tree, so routes, 404/405 answers, introspection and named-route metadata cannot be isolated per domain (see Context).
- _Rewrite as router configuration_: rejected — stateless path normalization belongs in the middleware tier, where it is registered, scoped and composed like other cross-cutting concerns; conditional forwarding stays on `ctx.Rewrite()`.
- _A single rewrite mechanism_: rejected — normalization must change the path before the first lookup, while a handler-driven forward is decided after a route matched; one mechanism would serve one of the two too early or too late.

## Consequences

**Positive:**

- Host routing remains explicit and isolated without modifying the radix tree.
- Existing `*Group` semantics are reused for host-scoped routing.
- Rewrite is available in both stateless (middleware) and handler-driven (context) forms.
- Original-path preservation improves debugging, analytics, and logging.
- Introspection can report host-aware route information.

**Negative:**

- Param/regex host matching adds a small linear scan before path lookup. Exact static hosts use a hash-map fast path.
- Shared route-param namespace requires registration-time collision checks.
- `ctx.Rewrite()` re-runs group/route middleware, so middleware with side effects must tolerate per-dispatch execution.
- Wildcard host routes cannot generate URLs; a route that needs URL generation uses a named host param.
