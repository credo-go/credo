# ADR-018: Host Routing & URL Rewrite

**Status:** Accepted **Date:** 2026-03-12 **Depends on:** ADR-007, ADR-008, ADR-010 **Related:** ADR-009, ADR-017 **Specification:** [Router spec](../specs/router.md), [Context spec](../specs/context.md), [Middleware spec](../specs/middleware.md) **Guide:** [Routing guide](../guides/routing.md)

## Context

Credo applications often need to serve multiple domains from one process: `api.example.com`, `admin.example.com`, tenant subdomains, and a default website. Middleware-only host checks are possible, but they collapse all domains into one path tree and make route isolation, 404/405 behavior, route introspection, and named-route metadata less reliable.

Credo also rewrites URLs: legacy URLs, versioned prefixes and vanity paths are mapped, by the path alone, to the routes that serve them. A second mechanism after the match — a handler-driven internal forward — carries a contract of its own: an internal sentinel the compiled chain swallows, a loop owned by dispatch with a limit, a wire-form target, and group and route middleware that run once per dispatch round, so a middleware with side effects must check whether a forward is pending. Its common cases have a path of their own: a single-page app's client routes are static's SPA mode, and a decision that needs handler logic can call the target's handler.

A pre-dispatch rewrite must also agree with the router on what a segment is. Matched against the decoded `URL.Path`, where an encoded slash has become a separator, a rule would match across a slash the client sent as data, a single-segment pattern would not match a segment that contains `%2F`, and a rewritten path set with `RawPath` cleared would turn a captured escape into a separator, so the router would see segments the client never sent.

## Decision

Credo adds host routing as a first-class routing dimension and rewrites a path only before routing:

1. `app.Host(pattern)` creates a host-scoped `*Group` backed by its own mux.
2. `middleware.Rewrite(cfg)` performs pre-dispatch path rewriting (`RewriteConfig{Rules, Skipper}`).
3. `ctx.OriginalPath()` preserves the client-sent path across the rewrite.

### Host Routing

Each registered host pattern gets a dedicated mux. Dispatch first matches the request host, selects the matching mux, then performs the normal radix-tree path lookup inside that mux; a request whose host matches no pattern falls back to the default mux. Keeping the host out of the radix tree leaves path matching unchanged and isolates each domain's routes, 404/405 answers and introspection. Returning a `*Group` reuses prefixes, middleware, meta and names for host-scoped routes with no new API. Pattern syntax (static, `{param}`, `{param:regex}`, a leftmost `*`), normalization and the matching rules: [router spec](../specs/router.md#host-based-route-groups).

When several patterns could match, the most specific wins — a static label over a regex-constrained one over a param or wildcard, compared right to left — rather than the first registered, so a broader pattern registered earlier never shadows a more specific one; registration order only breaks ties between equally specific patterns with different semantics (two regex-constrained labels, say). Host patterns with identical match semantics panic at registration: only registration order would decide between them, and one of them could never be reached. Exact static hosts use a hash-map lookup, so the common case pays no scan.

Host params share one namespace with path params and are read through the same `RouteParam`/`RouteParams` accessors; a separate host-param accessor is deliberately not added, so a handler reads every capture the same way. A name used by both the host and the path therefore panics at registration. A wildcard label captures nothing: it matches only, and a route that needs to generate its subdomain uses a named host param. Introspection reports each route's host pattern (`RouteInfo.Host`) across the default mux and every host-scoped mux.

### Pre-Dispatch Rewrite

`middleware.Rewrite` rewrites the request path before routing, so the first lookup already sees the rewritten path. It is middleware rather than router configuration, so it is registered, scoped and composed like other cross-cutting concerns. It runs once in the normal middleware chain: it does not create a loop and does not re-enter `PreRouting` middleware. Rules are evaluated in order and the first match wins, so overlapping rules resolve predictably. `RewriteConfig`, `RewriteRule` and placement: [middleware spec](../specs/middleware.md#rewrite).

**Rewrite matches the wire form.** `middleware.Rewrite` matches on the canonical wire form the router matches ([URL Round Trip](007-router-and-routing.md#url-round-trip)), not on the decoded `URL.Path`, and sets the rewritten path in wire form, keeping `Path` and `RawPath` consistent. An encoded `%2F` inside a segment stays data and is never a separator, so a single-segment pattern matches it and the rewrite keeps the escape; a catch-all keeps its slashes; a `{name:regex}` parameter is one segment and its constraint applies to the whole decoded value, as in the router; a captured value is decoded once, as a route parameter is; and the router sees, after the rewrite, the segments the client sent. A custom `Regexp` rule stays the caller's regexp over the canonical path. A malformed escape in a target is an error. Rewrite and routing therefore agree on what a segment is, as they already agree on a catch-all's empty rest.

### No Post-Match Re-Dispatch

A path is rewritten before routing or not at all. `middleware.Rewrite` stays, for a path mapped before routing: legacy URLs, versioned prefixes and vanity paths are decided by the path alone, and the first lookup already sees the mapped path. The common post-match cases have their own path: static's SPA mode serves the index for a client route ([static spec](../specs/static.md)), and a handler whose decision needs handler logic calls the target's handler itself. Kept, the post-match re-dispatch would freeze at v1 with its contract — the pending signal, at most ten re-dispatches, the wire-form target, group and route middleware run again — while adding it back later is additive.

### Original Path Preservation

`ctx.OriginalPath()` reports the client-sent wire-form path, captured once before any middleware or rewrite runs and immutable for the request's lifetime, so logs, analytics and debugging see what the client asked for whatever the rewrite changed; the access log adds `path_original` when the served path differs ([context spec](../specs/context.md#originalpath)).

## Rejected Alternatives

- _Host labels inside the radix tree_: rejected — it bakes host logic into the path tree every route lookup walks; a selector over per-host muxes keeps the tree unchanged and the domains isolated.
- _Middleware-only host checks_: rejected — every domain shares one path tree, so routes, 404/405 answers, introspection and named-route metadata cannot be isolated per domain (see Context).
- _Rewrite as router configuration_: rejected — stateless path normalization belongs in the middleware tier, where it is registered, scoped and composed like other cross-cutting concerns.

## Consequences

**Positive:**

- Host routing remains explicit and isolated without modifying the radix tree.
- Existing `*Group` semantics are reused for host-scoped routing.
- A path is rewritten once, before routing, so every middleware runs once per request.
- Original-path preservation improves debugging, analytics, and logging.
- Introspection can report host-aware route information.

**Negative:**

- Param/regex host matching adds a small linear scan before path lookup. Exact static hosts use a hash-map fast path.
- Shared route-param namespace requires registration-time collision checks.
- Wildcard host routes cannot generate URLs; a route that needs URL generation uses a named host param.
