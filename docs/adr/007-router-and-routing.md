# ADR-007: Router & Routing

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** ADR-001, ADR-002 **Related:** ADR-009, ADR-010, ADR-018 **Specification:** [Router spec](../specs/router.md) **Guide:** [Routing guide](../guides/routing.md)

## Context

An all-in-one framework (ADR-001) needs a fast, feature-rich router. Go's stdlib `http.ServeMux` (even with Go 1.22 enhancements) lacks regex constraints, named routes, route metadata, and URL generation.

Credo adapts (ADR-002) Chi's radix tree for fast path matching, and Goyave's routing features (route meta, named routes, status handlers, fluent API) for developer experience.

A router also decides which spellings of a URL are one route. The path a client sends is percent-encoded: routing must not invent a segment the client never sent, must not let two spellings of one URI reach different routes, and must hand a captured value to the handler, to a nested app and to URL generation without decoding it twice or losing it.

Host routing and URL rewrite: [ADR-018](018-host-routing-and-rewrite.md).

## Decision

### Radix Tree

The router is built on a radix tree adapted from Chi, living in `internal/radix/`. The tree supports:

- **Static segments**: `/users/list`
- **Parameterized segments**: `{id}` — matches any single segment
- **Regex-constrained params**: `{id:[0-9]+}` — matches only digits
- **Catch-all (wildcard)**: `{path...}` — matches the remainder of the path, an empty remainder included

The tree uses HTTP method bitmasks for efficient method-based lookup. Chi is the source rather than httprouter (kept as a reference): Chi already supports `{param}` syntax, regex constraints, method bitflags and sub-router mounting, which httprouter would need extensive refactoring to provide.

**A catch-all matches an empty rest.** `/files/{path...}` serves `/files/` with an empty capture, as chi and `net/http.ServeMux` do, and generation accepts an empty catch-all value. Static, regex and plain candidates keep their priority over the catch-all, and a single-segment parameter never matches an empty value. Rejected alternative: a catch-all that needs at least one byte — the prefix with a slash would then never reach the route, so a mount's own root would be redirected to the bare prefix (404 with the trailing-slash redirect off), `Static` would serve `/static/` only through that redirect, and the router would disagree with the rewrite middleware, whose `{name...}` matches an empty rest. Registering `prefix + "/"` for mounts alone was rejected as well: an explicit `GET /admin/` would then collide with the mount, and every other catch-all would still miss its empty rest.

**Endpoint-owned parameter names.** Path parameter names are stored on endpoints, not on tree nodes: a dynamic node is identified by its name-stripped shape (`{}`, `{:regex}`, `{...}`), matching captures values positionally, and the matched endpoint maps them to its own keys. Routes that share a dynamic segment may name it differently (`/customers/{id}` and `/customers/{customer_id}/timeline`), and each handler sees only its endpoint's names. This adapts Chi's endpoint-key model while keeping Credo's strict duplicate policy: the same method on the same shape is a duplicate whatever the names, and structural conflicts — a second regex matcher at one level or mismatched regex tails — fail at registration. Rejected alternative: names owned by tree nodes, which would force every route under a shared prefix to agree on one name and turn a naming preference into a "conflicting parameter" registration error, at no gain in matching speed; the positional model needs no such rule and adds no per-request allocation. See [URL Parameters](../specs/router.md#url-parameters).

**A parameter name is used once per pattern.** `/a/{id}/b/{id}` panics at registration, whether the repeat comes from the route, a group or mount prefix joined to it, or a host pattern, as in chi and `net/http.ServeMux`; host and path names share one namespace. Rejected alternative: accepting the pattern under a first-wins or last-wins rule — under either rule one of the two captures stays unreadable.

### URL Round Trip

The router matches the canonical form of the wire path: `URL.EscapedPath()` keeping exactly the escapes RFC 3986 §2.2 protects from normalization — the reserved characters, the slash among them — plus the percent sign against double decoding, and decoding everything else (§2.3). Segment boundaries and reserved spellings therefore come from the client, static text registered as written is met by every equivalent spelling, and a path without escapes costs nothing. Each captured value is decoded exactly once, a regex constraint applies to the whole decoded value that `RouteParam` reports, and `{name}` and `{name:regex}` are single-segment — `{name...}` is the only way to capture several segments. Generation takes decoded values and refuses a value that could not route back. Rewrite targets and mount handoffs are wire-form for the same reason: a percent-encoded value must survive as one parameter through an internal re-dispatch or a nested app. Round trips preserve parameter values, not the client's spelling of percent-encoded octets. The [router spec](../specs/router.md#encoded-parameter-values) holds the contract with its positive and negative cases.

Rejected alternatives:

- _Decoding the whole path before routing_: an encoded slash would create a segment the client never sent, and there is no way back.
- _Matching on the raw path alone_: static text with non-ASCII bytes would be unreachable, and two spellings of one URI would route differently.
- _Decoding the reserved characters too_: `/api/a%3Bb` would reach a mounted app as `/a;b`, a different URI.
- _Chi's prefix match (`^(re)` on the raw candidate)_: a constraint could consume part of a segment or span a raw slash, and a tail-bounded parameter would span segments (`/{name}.json` would accept `/a/b.json`), although the grammar says a parameter is one segment.

### Router Lives in Root Package

The router is not a separate package — it lives in the root `credo` package. This eliminates import cycles between router and middleware, enables unified handler/middleware types, and simplifies the API.

### Route Registration

HTTP method shortcuts on `App` and `Group` (`GET`, `POST`, `PUT`, `DELETE`, `PATCH`, `QUERY`, `HEAD`, `OPTIONS`) return a `*Route`, so a name, metadata and route middleware chain onto the registration: `app.GET("/users/{id}", h).Name("user.show").SetMeta("auth", true)`. Signatures: [router spec](../specs/router.md#api-surface).

### Route Meta

Routes, groups and the App carry key-value metadata (`SetMeta`). `Route.LookupMeta(key)` returns `(any, bool)` and walks the parent chain (route → group → app), so a route inherits its group's policy and overrides it locally. Middleware reads policy declaratively from the matched route instead of keeping path allow-lists ([router spec](../specs/router.md#route-meta-system-goyave-inspired), [ADR-010](010-middleware-architecture.md#meta-driven-behavior)).

### Named Routes & URL Generation

Named routes generate URLs (`BuildURI`, `BuildURL`) instead of hard-coding paths; a duplicate name panics at registration. Generation is intentionally strict: a missing, extra or invalid value is an error, never a URL with a placeholder left in or an input silently dropped. Wildcard host patterns such as `*.example.com` match only and cannot generate a concrete host; a named host parameter serves when generation needs the subdomain. Rules and errors: [router spec](../specs/router.md#named-routes--url-generation-goyave-inspired).

### Route Groups

Groups share a prefix and inherit their parent's middleware and metadata ([router spec](../specs/router.md#route-groups-and-sub-routers)).

### Status Handlers

Status handlers are custom handlers for the router's own 404 and 405 answers. They are app-level and consulted for these two codes only: 404 when no route matches, 405 when the path matches but no route serves the method. They are not error handlers — an error a handler returns, a 404 included, goes through the central error pipeline ([ADR-009](009-handler-and-error-handling.md)).

`StatusHandler(code, h)` panics at registration for any code but 404 and 405, with a message naming the two supported codes. Only those two are looked up, so a handler registered for another code would never run — a silent no-op — and supporting more codes is not the remedy: a 5xx handler would re-enter application code inside error rendering, the path that must stay safe when application code has just failed. The panic turns the no-op into a startup failure the developer sees at once. A 403 or 500 response is shaped through the error pipeline's `ErrorRenderer`, not a status handler.

### HEAD Auto-Handling

Every `GET` registration automatically registers a `HEAD` handler that runs the same handler chain. Route middleware and meta added to the GET route reach its HEAD twin, because a twin with a shorter chain would let a HEAD request silently bypass the authentication or rate limit that protects the GET route. An explicit `HEAD` registration overrides the auto-generated one, so an application that wants different HEAD behavior says so ([router spec](../specs/router.md#head-auto-handling)).

### QUERY Method

Credo recognizes RFC 10008 QUERY as a standard safe, idempotent method and exposes the same registration shape on `App` and `Group`: `QUERY(pattern, handler) *Route`. The request content carries the query representation, so handlers normally use `Request.BindBody`; Credo never creates a GET or HEAD twin, because GET query parameters and a QUERY body are different input contracts.

A matched QUERY request must carry a non-blank `Content-Type` ([router spec](../specs/router.md#query-rfc-10008)). The guard is method-level, so it covers mounted handlers too, and it runs inside ordinary middleware so authentication and observability keep their established order. A present header leaves media support and content consistency to `BindBody` or an application parser.

Credo deliberately adds no QUERY-only content-type option, `Accept-Query` helper, reserved metadata key, automatic OPTIONS behavior, cache, or exported method constant. Applications may emit `Accept-Query` directly; a future automatic contract, if justified, must be generic across body-bearing methods.

### Mounting

`Mount(prefix, h)` mounts any stdlib `http.Handler`. A mounted handler answers its exact prefix and every path beneath it, receiving the request with the prefix stripped. A conflicting `Mount` registers nothing: the radix tree has no delete, so `Mount` checks every registration before inserting any ([router spec](../specs/router.md#sub-router-mounting)).

The prefix may carry parameters (`Mount("/t/{tenant}", h)`). A parametric prefix is not text that could be stripped, so the child's path is cut by segment count on the canonical path — a parameter never crosses a slash and an encoded slash stays encoded there — and the prefix's parameters reach the child as stdlib path values (`http.Request.PathValue`), set on a clone of the request. Path values are the one mechanism every mounted handler already reads: a stdlib handler directly, a mounted Credo app through `Request.PathValue`, and no Credo-specific context key crosses the mount boundary. Passing the parent's route context down was rejected: it would hand the child internal captures (`_mount`) and switch a mounted Credo app from its own pooled route context to a pre-filled one, a dispatch path that otherwise never runs. Setting the values on the shallow copy a static mount uses was rejected too: a shallow copy shares the storage behind its path values with the request it was copied from, so the mount would overwrite the values of a request its caller still holds — an `http.ServeMux` in front of the app, or the parametric mount one level up. Refusing parametric prefixes at registration was rejected as well: it turns a pattern the router accepts everywhere else into a startup panic, and it is the worse stdlib interoperation — a stdlib handler mounted under a tenant or version prefix would have to re-parse the path to learn what the router already matched, where path values hand it over through the standard `PathValue`.

### Route Introspection

`app.Routes()`, `Walk` and `WalkRoutes` report the live route surface as `RouteInfo` ([router spec](../specs/router.md#router-interface)). `RouteInfo` is derived from the route store at call time rather than snapshotted at registration, and carries the resolved `Meta` (route ← group ← app) as a shallow copy whose values are read-only by convention. A mount is opaque: one `RouteKindMount` entry reports its cleaned prefix and forwarded methods, never the internal catch-all pattern or method fan-out, and `Walk` skips mounts because its method+pattern callback cannot represent them. Output is a deterministic total order `(Host, Pattern, Method, Kind)`, independent of registration order and host compile-sort state, so route/permission catalogs and golden-file tests are stable. Introspection reads live `*Route` fields without locking; it is a post-wiring operation and must not run concurrently with route registration or configuration.

**Alternatives considered:**

- _Snapshot meta at registration_: rejected — `Name`/`SetMeta` are chained after the registration call returns, so a registration-time snapshot would miss them; deriving `RouteInfo` live from the store is the only correct option.
- _A separate `LocalMeta` field (route-only, unresolved)_: deferred (YAGNI) — consumers need the resolved view (route ← group ← app); a local-only view can be added later if a use case appears.
- _Deep-cloning `Meta` values_: rejected — selectively cloning `[]string`/map values gives false confidence for custom types (`[]Permission`, `*T`) that cannot be cloned generically, so `Meta` is a shallow copy with a documented read-only-by-convention contract.
- _Registration-order output_: rejected — `compile()` sorts host entries in place, so registration order is not stable across the first request; a total-order sort guarantees deterministic output regardless of compile state.

### Compile & Freeze

Routes and their middleware chains are compiled once, during the App's preparation, after HTTP registration is frozen; a later registration panics ([lifecycle spec](../specs/lifecycle.md#preparation)).

### Trailing Slash Redirect

When no route matches but the path with its trailing slash toggled does, the router redirects to that path (301 for GET and HEAD, 308 otherwise, so the method is preserved) instead of answering 404. It is on by default ([router spec](../specs/router.md#trailing-slash-redirect)).

**Alternatives considered:**

- _Silent fallback (rewrite)_: Hides canonical URL decisions from clients. Client wouldn't know the canonical URL, making debugging harder and creating SEO duplicate-content risk.
- _Middleware-based_: Would require either a blind path normalization (without knowing registered routes) or a double tree lookup. Dispatch-level implementation is more efficient and semantically correct.

## Consequences

**Positive:**

- Fast radix tree matching (adapted from battle-tested Chi)
- Route Meta enables declarative middleware configuration
- Named routes + URL generation prevent hardcoded paths
- Fluent API is readable and chainable
- HEAD auto-handling follows HTTP spec
- Live route introspection (`Routes`/`WalkRoutes`) exposes `Name`, resolved `Meta`, and mounts — enabling drift-free route/permission catalogs

**Negative:**

- Adapted radix tree requires maintenance when upstream Chi evolves
- Route Meta is `map[string]any` — no compile-time key/type safety
- Compile-once means no dynamic route addition at runtime
