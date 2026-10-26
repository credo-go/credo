# ADR-010: Middleware Architecture

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** ADR-007, ADR-008 **Related:** ADR-001, ADR-005, ADR-009, ADR-012, ADR-013, ADR-014, ADR-016, ADR-018 **Specification:** [Middleware spec](../specs/middleware.md), [HTTP features spec](../specs/http-features.md) **Guide:** [Middleware guide](../guides/middleware.md)

## Context

Middleware is the primary extension mechanism for a web framework. It intercepts requests before/after handlers for cross-cutting concerns: logging, authentication, rate limiting, CORS, compression, etc.

Credo needs a middleware model that supports three scopes (global, group, route), integrates with route metadata, and interoperates with the Go stdlib ecosystem.

Some concerns cannot sit at any position in such a chain. Recovery and access logging must observe the final response: the status after error rendering and recovery, the bytes after compression. Transport features must select on the original request, before rewrites and body transformation. A middleware sees a returned error before it is rendered, so it can only infer the final status, and two observers at different boundaries disagree on status, bytes and duration. Features that are off by default fail silently: an application that omits a call compiles and runs without request IDs or access records, and nothing at runtime says which features are in effect.

Pre-dispatch rewrite middleware and its interaction with handler-level re-dispatch are documented in [ADR-018](018-host-routing-and-rewrite.md). The [middleware spec](../specs/middleware.md) is the user-chain contract and the [HTTP features spec](../specs/http-features.md) the feature contract; this ADR records the decisions and their rationale.

## Decision

### Single Middleware Type

```go
type Middleware func(next Handler) Handler
```

One type for all three tiers. No separate types for global vs group vs route middleware. This simplifies the mental model and allows reuse.

### Three-Tier Execution

```
Request → Global MW → Group MW → Route MW → Handler
                                                ↓
Response ← Global MW ← Group MW ← Route MW ← Handler
```

Global middleware (`app.GlobalMiddleware`) runs on every request, group middleware (`group.Middleware`) on the routes under that group, route middleware (`route.Middleware`) on a single route; the [middleware spec](../specs/middleware.md#3-tier-model-goyave-inspired) defines the tiers and their order.

**Global middleware runs even on 404/405.** This ensures CORS and security headers are always present. Framework features cover 404/405 as well.

**Group middleware membership is resolved at compile time from the group parent chain** — the same model `LookupMeta` uses for metadata. Middleware added to a group after a route or sub-group was registered still applies to it; registration order within a group affects execution order only, never membership.

### Compile-Time Chain Building

Middleware chains are precompiled when the App prepares to serve:

1. Per-route: group middlewares + route middlewares + handler → single compiled `Handler`
2. Global: global middlewares + dispatch → single compiled `Handler`

At runtime, `ServeHTTP` applies the lifecycle admission gate and hands the request to the executor, which runs the installed framework features around the precompiled global chain. Dispatch looks up the matched route and calls its precompiled chain. Zero allocation, no slice iteration on the hot path.

Because the chains are built once, middleware, route and feature registration closes when the App prepares to serve or admits shutdown, and a later call panics ([registration guards](../specs/lifecycle.md#registration-guards)). DI `Finalize` alone does not close it, so DI-resolved controllers and renderers can still be registered after it.

### Meta-Driven Behavior

Middleware reads route metadata declaratively instead of being configured per route or maintaining path allowlists and denylists: a route or group declares its intent with `SetMeta`, and the middleware reads it with `LookupMeta`, which walks the parent chain (route → group → app) for inherited values. The [middleware spec](../specs/middleware.md#meta-driven-middleware-goyave-inspired) shows the pattern and lists the framework's own meta keys.

### Config Struct Pattern

Configurable middleware takes `cfg ...Config`: no argument selects the defaults, one argument customizes ([example](../specs/middleware.md#config-struct-pattern-echo-inspired)). Framework features follow the same shape at registration: `app.UseAccessLog()` for defaults, `app.UseAccessLog(credo.AccessLogConfig{...})` for customization, at most one config.

### Stdlib Adapter

`WrapStdMiddleware` adapts `func(http.Handler) http.Handler` middleware to the Credo chain. It is kept as a deliberate ecosystem escape hatch — the large stdlib-middleware corpus (OTel instrumentation, vendor CORS/gzip) works without being rewritten, consistent with the override-friendly boundaries of [ADR-001 goal 3](001-framework-identity-and-goals.md#3-integrated-first-explicit-boundaries) and the other escape hatches (`ServeContext`, `WithRawConfig`, `JWTAdvanced`). It is second-class by design, and that is documented rather than hidden: adapted middleware sees only `*http.Request` and `r.Context()`, never `*credo.Context`, so it cannot read route Meta, the typed principal (`ctx.GetUser[T]`), the renderer, or the error pipeline. There is no selective leak (no public `context.Context`-based principal accessor exists — see [ADR-012](012-authentication-and-authorization.md)). The first-class path for anything needing the principal, route meta, or the error pipeline is a native `func(Handler) Handler`. The [middleware spec](../specs/middleware.md#stdlib-adapter) defines how the adapter records a response the middleware writes itself.

### CSRF through CrossOriginProtection

`middleware.CSRF` wraps the standard library's `net/http.CrossOriginProtection` instead of implementing token plumbing. Token or double-submit-cookie CSRF requires session state, template helpers and header plumbing across the stack; `Sec-Fetch-Site` has shipped in all browsers since 2023 and reduces the problem to a header check. Credo wraps the stdlib detector (maintained upstream, so security patches ride Go releases) and adds only config-struct ergonomics, error-pipeline integration and QUERY as a safe method; the older-browser fallback (Origin/Host comparison) is inherited from the stdlib. Token and double-submit-cookie CSRF are rejected for the plumbing they need.

No test pins the stdlib's own handling of QUERY. A Go release that adds QUERY to `CrossOriginProtection`'s safe methods is a correct upstream improvement after which Credo's bypass is redundant, not wrong; a canary that failed on it would break the suite or cap the toolchain, so none is added.

### Framework features around the user chain

Recovery, RequestID, AccessLog, i18n, Compress and Decompress are framework-owned HTTP processing around the three Global/Group/Route user middleware tiers. One request executor owns request initialization, the user chain, centralized error and panic handling, response finalization, optional access logging and Context release. It observes the final response after error rendering and compressor finalization, keeps framework diagnostics independent from access logging, and applies the installed features in one fixed plan whatever the `Use*` call order ([request execution](../specs/http-features.md#request-execution)).

Why not middleware: these features need to see the final response, the original request, or both, and a middleware position gives neither. A middleware can only infer the final status from an error that has not been rendered yet, and an observation at its boundary disagrees with any other observer's on status, bytes and duration. Owning the features in the executor gives one authoritative observation and one implementation of each. The price is that these features cannot be scoped per group or route; an application that needs route-specific policy writes ordinary middleware (for example a group-level recover-and-return-error wrapper) or uses the selection knobs the features expose (`Skipper`, `MetaAccessLog`, `ResultFilter`).

Timeout, CORS, CSRF, Secure, RateLimit, Rewrite and ContractGuard are user middleware because their route/group scope and their order relative to application policy are useful.

### Built-in HTTP Feature Configuration Criterion

The default activation policy determines the public configuration path for selectable built-in HTTP features:

| Default | Sole public path | Application |
| --- | --- | --- |
| On | Constructor configuration plus a `Without*` opt-out | `WithRecoverConfig(cfg)` configures recovery; `WithoutRecover()` disables it |
| Off | A `Use*(config)` registration that activates and configures the feature | RequestID, AccessLog, Compress, Decompress and i18n; custom error/success renderers use their own `Use*` registrations |

Each decision has one public path, so there is never a question of which path wins. Recovery's constructor config does not re-enable it: `WithoutRecover()` takes precedence regardless of option order, and no separate positive recovery activation helper exists. An optional feature has one successful registration — a `UseI18n` that leaves i18n inactive included — and does not also expose constructor fields/options, configuration setters, or an `Enabled` switch for the same decision. Applications evaluate external enable flags in bootstrap code before calling `Use*`. Required feature config travels as one typed value rather than a family of per-field helpers.

The criterion concerns feature activation, not every zero-valued setting. Foundational App inputs such as logger, raw configuration, server/TLS and network timeouts remain constructor settings. Mandatory protocol/error handling is not made optional by this rule. Request-state and route-meta mutators keep their separate purpose.

Custom renderer installation is optional even though core error rendering always exists. A renderer may be bound from a DI-resolved object during bootstrap, so `UseErrorRenderer` and `UseSuccessRenderer` provide their sole registration path; there are no constructor duplicates and no setters. The core pipeline retains its default rendering when no extension is registered.

`Use*` registers a feature once during HTTP setup, and invocation order does not determine execution order. AccessLog activation does not govern framework/application diagnostic logs, which continue through their normal levels and logger filtering.

### Access-Log Filtering

Access logging is explicit (`app.UseAccessLog(cfg...)`) and observes the final response outside recovery and centralized error rendering, so it is the one authoritative access record. `AccessLogConfig.Logger` selects a dedicated sink without giving up that boundary; the logger is not derived from `ctx.Logger()`, so arbitrary request-scoped enrichment is not inherited, while `request_id` is restored explicitly and exactly once.

Filtering is split by observation phase, because each phase knows different things. The `Skipper` runs before routing and sees request data only, which suits blanket path/header skips (metrics scrape, static assets). The `MetaAccessLog` route meta is read once the route is known, so a route or a whole group is silenced declaratively, and a route-level value overrides its group's. `MinLevel` and `ResultFilter` run after the final response and see its status, duration and bytes along with route and request metadata.

Status drives the record's level (`1xx/2xx/3xx → Info`, `4xx → Warn`, `5xx+ → Error`); `MinLevel` controls admission and never rewrites that level, and `ResultFilter` cannot restore a record the threshold rejected. The default is Info, so an installed access log preserves every status class and keeps the traffic/latency denominator while request metrics remain deferred; high-volume applications opt into Warn or Error. The [observability contract](../specs/http-features.md#observability-contract) defines the exact order, the record and its measurements.

#### Alternatives considered

**Default-on access logging** was rejected. The argument for it is [ADR-001 goal 6](001-framework-identity-and-goals.md#6-observable-by-default), "observable by default": the framework's nearest all-in-one peer, GoFr, logs requests by default, while the composable toolkits (Goyave, Hertz, Echo, Chi) default off. Against it, the configuration criterion above gives every feature exactly one public path and keeps plain `New()` free of implicit scaffold behavior. The logging _infrastructure_ is ready by default (structured `slog`, `Infra.Logger`, server-error bridging, recovery diagnostics), while request correlation and access records are two explicit `Use*` calls; an enterprise scaffold makes the two calls visible.

A **status-code skip list** was rejected. Across the ecosystem request-level skipping is a predicate — Hertz `WithLogConditionFunc(func(ctx, c) bool)`, Echo `Skipper`, Gin `Skip`/`SkipPaths`. Credo keeps the package-wide `Skipper` convention and adds the more general post-response `MinLevel` + `ResultFilter` composition instead of a fixed list.

Changing successful requests to **Debug by default** or enabling a built-in fixed **1/N sampler** was rejected. Both silently weaken the traffic record and conflate the status-derived severity contract with admission. Applications can set `MinLevel` explicitly and implement deterministic request-ID sampling (or concurrency-safe counter sampling) in `ResultFilter`; general metrics and telemetry sampling belong to the observability release ([ADR-014](014-observability.md)).

A **route-scoped access-log middleware** beside the feature was rejected. A middleware observes returned errors before centralized rendering, so its status, bytes and duration would be approximations, and two producers mean two schemas to keep aligned. Route and group selection uses `MetaAccessLog` and `ResultFilter` on the single record; a deliberate second audit sink is application middleware.

### Startup visibility of effective features

The managed start line, `credo: server started`, carries a `features` attribute listing the built-in features in effect, derived from the App's effective state after the start phase rather than from registration calls ([spec](../specs/http-features.md#startup-visibility)).

The attribute is a permanent diagnostic, in keeping with observable-by-default. Rejected: a separate startup line (the start line already exists and carries the address); a Warn for each feature that is off (default-off is this ADR's deliberate choice, and a warning on every start would train operators to silence it); a public feature-introspection API or registry (no consumer needs more than the log line).

## Rejected Alternatives

### Configuration-Driven Activation (Rejected)

Activating or parameterizing middleware from configuration files — framework-read `middleware.*` keys (`middleware.cors.enabled`, `middleware.timeout.duration`) behind a `UseConfiguredMiddleware()` bootstrapper — was considered and rejected. Discoverability: `credo.With*` options and the `middleware` package surface in IDE autocomplete, while stringly-typed config keys do not, and a typo silently disables the middleware it names. Doctrine: Credo's configuration architecture (ADR-005) is built on typed snapshots — string keys never appear in business code. Credo reads its own `server` section, and the `i18n` section once `UseI18n` is called, but no configuration key activates a feature or a middleware; the call in code does. Explicitness ([ADR-001 goal 3](001-framework-identity-and-goals.md#3-integrated-first-explicit-boundaries)): the middleware stack is part of the application's composition and belongs visibly in code. Environment-dependent parameters remain fully supported the doctrinal way — the application unmarshals its own typed config and passes it to the middleware constructor (`middleware.CORS(middleware.CORSConfig{AllowOrigins: cfg.CORS.Origins})`).

## Consequences

**Positive:**

- Single type — no confusion about which middleware type to use
- Precompiled chains — zero allocation on hot path
- Meta-driven — declarative, no per-route middleware wiring
- Config struct pattern — readable, discoverable options via `Middleware(cfg ...Config)`
- Stdlib interop via WrapStdMiddleware
- One authoritative access record and one recovery layer, observed after error rendering and compression, with one public path per feature

**Negative:**

- Global MW on 404/405 runs full chain even for unmatched routes
- Meta values are `any` — no compile-time type safety
- Precompilation means no dynamic middleware addition at runtime
- Framework features cannot be scoped per group or route; route-specific policy is expressed through their selection knobs or ordinary middleware
