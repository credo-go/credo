# ADR-010: Middleware Architecture

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** ADR-007, ADR-008 **Related:** ADR-001, ADR-005, ADR-009, ADR-012, ADR-013, ADR-014, ADR-016, ADR-018 **Specification:** [Middleware spec](../specs/middleware.md), [HTTP features spec](../specs/http-features.md) **Guide:** [Middleware guide](../guides/middleware.md)

## Context

Middleware is the primary extension mechanism for a web framework. It intercepts requests before/after handlers for cross-cutting concerns: logging, authentication, rate limiting, CORS, compression, etc.

Credo needs a middleware model that supports a tier before routing, a tier on every matched route, group and route scopes, integrates with route metadata, and interoperates with the Go stdlib ecosystem.

A middleware that runs before the match cannot read route meta, so a meta-driven middleware placed there does nothing and has to be repeated on every group. A middleware that must run on every route, but not for the router's own 404 and 405, needs a tier of its own. A mounted handler dispatched outside the compiled chain would let a path through the authentication every route gets. And a tier's name has to say when it runs, not only where it applies.

Some concerns cannot sit at any position in such a chain. Recovery and access logging must observe the final response: the status after error rendering and recovery, the bytes after compression. Transport features must select on the original request, before rewrites and body transformation. A middleware sees a returned error before it is rendered, so it can only infer the final status, and two observers at different boundaries disagree on status, bytes and duration. Features that are off by default fail silently: an application that omits a call compiles and runs without request IDs or access records, and nothing at runtime says which features are in effect. A consumer of the final response other than the access log — a metrics or tracing exporter — needs a place to observe it that agrees with the access record on status, bytes and duration, and an application attribute added to the access record must not risk a framework attribute's key.

Pre-dispatch rewrite middleware is documented in [ADR-018](018-host-routing-and-rewrite.md). The [middleware spec](../specs/middleware.md) is the user-chain contract and the [HTTP features spec](../specs/http-features.md) the feature contract; this ADR records the decisions and their rationale.

## Decision

### Single Middleware Type

```go
type Middleware func(next Handler) Handler
```

One type for every tier. No separate types for pre-routing, routed, group or route middleware. This simplifies the mental model and allows reuse.

### The routed tier

```
Request → PreRouting MW → match → Routed MW → Group MW → Route MW → Handler
                                                                    ↓
Response ← PreRouting MW ←───────── Routed MW ← Group MW ← Route MW ← Handler
```

The user tiers are PreRouting → routed → Group → Route. `app.Middleware(m...)` adds middleware that runs on every route registered through the App — directly, in a group, in a host group, as a static route or as a mount — after the match and before group and route middleware, so it reads the route's meta:

```go
app.Middleware(middleware.ContractGuard()) // reads every route's meta; never runs for a 404 or a 405
```

The method delegates to the root group's `Middleware`: the App's routes, groups and host groups descend from that group, and group middleware is collected when the chain is compiled, so a middleware added after a group was created applies to its routes too. It does not run for the router's own 404 and 405, which reach the pre-routing tier only. After preparation the call panics, as every registration does. The [middleware spec](../specs/middleware.md#tiers-prerouting-routed-group-route) defines the tiers and their order.

**Routed and group middleware membership is resolved at compile time from the group parent chain** — the same model the route's meta snapshot uses. Middleware added to a group after a route or sub-group was registered still applies to it; registration order within a group affects execution order only, never membership.

**`PreRouting` is named for when it runs:** before the match, on every request, the router's own 404 and 405 included. A middleware that acts before the match, or on a request no route matches, stays there: `CORS` answers a preflight, an `OPTIONS` request for which the router has no route, so on the routed tier the router's 405 would answer it instead; `Rewrite` changes the path that routing matches; and `Secure`'s headers and rate limiting's budget cover the router's 404 and 405 too. `CSRF` needs no route and works on either tier; authentication and `ContractGuard` go on the routed tier. The [middleware guide](../guides/middleware.md) shows the tier of each of Credo's middleware in one table.

**A mount is a route.** A request a `Mount` matches carries a `*Route` of kind mount — the mount's prefix as its pattern, the App's meta, no name and no method — so `HasRoute` is true for it, and its handler runs inside the routed tier, through a chain compiled as a route's is. A mount belongs to no group and has no route value of its own, so group and route middleware still do not apply to it, while authentication on the routed tier covers a mounted path.

**`ContractGuard` fails closed without a route.** Registered before routing, it sees no route on any request; it answers 500 through the error pipeline and logs one Error naming its registration before routing and the remedy: `app.Middleware`, a group or a route.

Rejected:

- **An open stage or rank registry:** the tier is ordinary middleware in registration order.
- **A warning at preparation for a `ContractGuard` before routing:** it lets every request through until someone reads the log, and the framework could emit it only by identifying one middleware among the functions it is given.

### Compile-Time Chain Building

Middleware chains are precompiled when the App prepares to serve:

1. Per-route: routed middlewares + group middlewares + route middlewares + handler → single compiled `Handler`; a mount's chain is the routed tier around the mounted handler
2. Pre-routing: `PreRouting` middlewares + dispatch → single compiled `Handler`

At runtime, `ServeHTTP` applies the lifecycle admission gate and hands the request to the executor, which runs the installed framework features around the precompiled pre-routing chain. Dispatch looks up the matched route and calls its precompiled chain. Zero allocation, no slice iteration on the hot path.

Because the chains are built once, middleware, route and feature registration closes when the App prepares to serve or admits shutdown, and a later call panics ([registration guards](../specs/lifecycle.md#registration-guards)). DI `Finalize` alone does not close it, so DI-resolved controllers and renderers can still be registered after it.

### Meta-Driven Behavior

Middleware reads route metadata declaratively instead of being configured per route or maintaining path allowlists and denylists: a route or group declares its intent with `SetMeta` and a typed key made by `credo.NewKey[T]`, and the middleware reads the typed value with `GetMeta`. A route's meta is resolved into a per-route snapshot when its chain is compiled — the route, then its groups, then the App — so a request-time lookup walks nothing, and registration order never decides membership. The framework's meta keys are typed keys — the middleware's `Key[[]string]` and `Key[int64]` keys, `MetaAccessLog` and `MetaRawResponse` as `Key[bool]` — so a value of the wrong type does not compile; the [middleware spec](../specs/middleware.md#meta-driven-middleware-goyave-inspired) lists them.

### Config Struct Pattern

Configurable middleware takes `cfg ...Config`, at most one, and one rule reads every config struct: **a zero field selects its default** ([the rule and each config's defaults](../specs/middleware.md#config-struct-pattern-echo-inspired)). No argument is the zero config, a config that sets one field keeps the defaults of the others, and turning off something that is on by default takes an explicit field, an inverted bool such as `SecureConfig.DisableContentTypeNosniff`, as `RecoverConfig.DisableStackTrace` is. The rule covers `middleware`, `middleware/ratelimit` and `httpclient.RetryConfig`. Framework features follow the same shape at registration: `app.UseAccessLog()` for defaults, `app.UseAccessLog(credo.AccessLogConfig{...})` for customization, at most one config.

Rejected: a config that, once passed, replaces the defaults, with a `DefaultXConfig()` constructor to start from. Under that rule `Secure(SecureConfig{HSTSMaxAge: 31536000})` stops sending `X-Content-Type-Options` and `X-Frame-Options`: adding one header removes two protective ones, and nothing at the call shows it. The safe call would be to fetch the defaults and mutate them, which a struct literal, the form every example and every reader reaches for, does not do. With zero fields filled, the constructor is a second spelling of the zero value, and so is an exported default `Skipper`; neither exists. The price is that a zero value cannot mean "off" for a default that is on, which is what the `Disable…` fields pay.

### Stdlib Adapter

`WrapStdMiddleware` adapts `func(http.Handler) http.Handler` middleware to the Credo chain. It is kept as a deliberate ecosystem escape hatch — the large stdlib-middleware corpus (OTel instrumentation, vendor CORS/gzip) works without being rewritten, consistent with the override-friendly boundaries of [ADR-001 goal 3](001-framework-identity-and-goals.md#3-integrated-first-explicit-boundaries) and the other escape hatches (`ServeContext`, `WithRawConfig`, `jwtauth.Advanced`). It is second-class by design, and that is documented rather than hidden: adapted middleware sees only `*http.Request` and `r.Context()`, never `*credo.Context`, so it cannot read route Meta, the typed principal (`c.GetUser[T]`), the renderer, or the error pipeline. There is no selective leak (no public `context.Context`-based principal accessor exists — see [ADR-012](012-authentication-and-authorization.md)). The first-class path for anything needing the principal, route meta, or the error pipeline is a native `func(Handler) Handler`. The [middleware spec](../specs/middleware.md#stdlib-adapter) defines how the adapter records a response the middleware writes itself.

The adapter hands the request on through `Request.Std()`, whose context is the Context's current head, and takes the context of the request the stdlib middleware passes on as the head of the inner chain: the middleware reads a value set before it with `Value` and its key, and a deadline or span it adds is visible to the handler through `c.Context()`. Its contract excludes a middleware that runs the next handler on another goroutine, as `http.TimeoutHandler` does: the Context is not safe for concurrent use, and the goroutine would move the head while the outer layer reverts it. The user's key stays unexported, so the principal stays out of the adapter's reach.

### CSRF through CrossOriginProtection

`middleware.CSRF` wraps the standard library's `net/http.CrossOriginProtection` instead of implementing token plumbing. Token or double-submit-cookie CSRF requires session state, template helpers and header plumbing across the stack; `Sec-Fetch-Site` has shipped in all browsers since 2023 and reduces the problem to a header check. Credo wraps the stdlib detector (maintained upstream, so security patches ride Go releases) and adds only config-struct ergonomics, error-pipeline integration and QUERY as a safe method; the older-browser fallback (Origin/Host comparison) is inherited from the stdlib. Token and double-submit-cookie CSRF are rejected for the plumbing they need.

No test pins the stdlib's own handling of QUERY. A Go release that adds QUERY to `CrossOriginProtection`'s safe methods is a correct upstream improvement after which Credo's bypass is redundant, not wrong; a canary that failed on it would break the suite or cap the toolchain, so none is added.

### Framework features around the user chain

Recovery, RequestID, AccessLog, i18n, Compress and Decompress are framework-owned HTTP processing around the user middleware tiers. One request executor owns request initialization, the user chain, centralized error and panic handling, response finalization, optional access logging and Context release. It observes the final response after error rendering and compressor finalization, keeps framework diagnostics independent from access logging, and applies the installed features in one fixed plan whatever the `Use*` call order ([request execution](../specs/http-features.md#request-execution)).

Why not middleware: these features need to see the final response, the original request, or both, and a middleware position gives neither. A middleware can only infer the final status from an error that has not been rendered yet, and an observation at its boundary disagrees with any other observer's on status, bytes and duration. Owning the features in the executor gives one authoritative observation and one implementation of each. The price is that these features cannot be scoped per group or route; an application that needs route-specific policy writes ordinary middleware (for example a group-level recover-and-return-error wrapper) or uses the selection knobs the features expose (`Skipper`, `MetaAccessLog`, `ResultFilter`).

Timeout, CORS, CSRF, Secure, Rewrite, ContractGuard and rate limiting (`ratelimit.Middleware`) are user middleware because their route/group scope and their order relative to application policy are useful.

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

Access logging is explicit (`app.UseAccessLog(cfg...)`) and observes the final response outside recovery and centralized error rendering, so it is the one authoritative access record. `AccessLogConfig.Logger` selects a dedicated sink without giving up that boundary; the logger is not derived from `c.Logger()`, so arbitrary request-scoped enrichment is not inherited, while `request_id` is restored explicitly and exactly once.

Filtering is split by observation phase, because each phase knows different things. The `Skipper` runs before routing and sees request data only, which suits blanket path/header skips (metrics scrape, static assets). The `MetaAccessLog` route meta is read once the route is known, so a route or a whole group is silenced declaratively, and a route-level value overrides its group's. `MinLevel` and `ResultFilter` run after the final response; the filter reads the `*Exchange` ([Observers of the final response](#observers-of-the-final-response)), with its status, duration and bytes, its route and request metadata, and the request's values.

Status drives the record's level (`1xx/2xx/3xx → Info`, `4xx → Warn`, `5xx+ → Error`); `MinLevel` controls admission and never rewrites that level, and `ResultFilter` cannot restore a record the threshold rejected. The default is Info, so an installed access log preserves every status class and keeps the traffic/latency denominator while request metrics remain deferred; high-volume applications opt into Warn or Error. The [observability contract](../specs/http-features.md#observability-contract) defines the exact order, the record and its measurements.

#### Alternatives considered

**Default-on access logging** was rejected. The argument for it is [ADR-001 goal 6](001-framework-identity-and-goals.md#6-observable-by-default), "observable by default": the framework's nearest all-in-one peer, GoFr, logs requests by default, while the composable toolkits (Goyave, Hertz, Echo, Chi) default off. Against it, the configuration criterion above gives every feature exactly one public path and keeps plain `New()` free of implicit scaffold behavior. The logging _infrastructure_ is ready by default (structured `slog`, `Infra.Logger`, server-error bridging, recovery diagnostics), while request correlation and access records are two explicit `Use*` calls; an enterprise scaffold makes the two calls visible.

A **status-code skip list** was rejected. Across the ecosystem request-level skipping is a predicate — Hertz `WithLogConditionFunc(func(ctx, c) bool)`, Echo `Skipper`, Gin `Skip`/`SkipPaths`. Credo keeps the package-wide `Skipper` convention and adds the more general post-response `MinLevel` + `ResultFilter` composition instead of a fixed list.

Changing successful requests to **Debug by default** or enabling a built-in fixed **1/N sampler** was rejected. Both silently weaken the traffic record and conflate the status-derived severity contract with admission. Applications can set `MinLevel` explicitly and implement deterministic request-ID sampling (or concurrency-safe counter sampling) in `ResultFilter`; general metrics and telemetry sampling belong to the observability release ([ADR-014](014-observability.md)).

A **route-scoped access-log middleware** beside the feature was rejected. A middleware observes returned errors before centralized rendering, so its status, bytes and duration would be approximations, and two producers mean two schemas to keep aligned. Route and group selection uses `MetaAccessLog` and `ResultFilter` on the single record; a deliberate second audit sink is application middleware.

### Observers of the final response

An `Observer` sees one immutable record, `*Exchange`, after the response is final: the method, the path and the original path, the route pattern and name, the remote address and the user agent, the final status, the bytes after compression, the duration, the classified error code, whether a panic was recovered, the request ID, and the request's values, which an observer reads as [ADR-014](014-observability.md#observers-read-the-requests-values) states. `app.AddObserver(o)` adds one, and panics on a nil observer and after preparation, as the other registrations do. Observers run where the access log is observed, after finalization, in registration order. An `Exchange` is valid only during `Observe`; an exporter copies what it keeps, apart from the request's context.

**The classified error code** has a fixed meaning: the code the pipeline classified, captured before the renderer runs; for a recovered panic, the code of the 500 it rendered; after a fallback, the fallback's code; and empty for an error returned after the response was committed or hijacked, for a handler that wrote its own error response, and for an escaped panic with recovery disabled. It is never inferred from the status.

**A panicking observer is contained.** With recovery enabled, its panic is logged as a framework Error diagnostic and that observer is skipped for the exchange, while the observers after it still run; the response is already final. `http.ErrAbortHandler`, and every panic under `WithoutRecover`, propagate, as for `Attrs`.

**The access log is an observer.** `MinLevel`, `ResultFilter` and `Attrs` are options of that observer, and `UseAccessLog` stays as sugar that installs it. `ResultFilter` takes the `*Exchange`, `func(ex *Exchange) bool`: the `Exchange` carries each field the record is built from, so one record has one type, and the filter reads the request's values and the user as every observer does. `Attrs` receives the `*Exchange` and returns application attributes for a record that is a candidate for emission — after `MetaAccessLog`, `MinLevel` and `ResultFilter`, and only when the logger is enabled for the record's level — emitted inside one `attrs` group. The guarantee is narrow: an application attribute never shadows a framework attribute, present or future, and `attrs` is a reserved key on the access record. A panic in `Attrs` is logged and the record is emitted without the group. The returned slice passes to the framework and must not be reused.

**Checked against its real consumers before v1.** Until the observability release, the access log is the only consumer of `Observer` and `Exchange`. The OpenTelemetry and Prometheus adapters written for that release's decision run on them before the stability window opens, so a change they need lands before v1 freezes the record.

Rejected:

- **An open `Stage` or rank registry:** observers run at one point, after finalization, in registration order.
- **`Attrs` as a field of the framework contract:** the attributes are the application's, kept in their own group under the narrow guarantee above.
- **Top-level placement of the returned attributes:** an application attribute could shadow a framework attribute, present or future.
- **A group named `app`.**
- **A public constant for the access record's message.**

### Startup visibility of effective features

The managed start line, `credo: server started`, carries a `features` attribute listing the built-in features in effect, derived from the App's effective state after the start phase rather than from registration calls ([spec](../specs/http-features.md#startup-visibility)).

The attribute is a permanent diagnostic, in keeping with observable-by-default. Rejected: a separate startup line (the start line already exists and carries the address); a Warn for each feature that is off (default-off is this ADR's deliberate choice, and a warning on every start would train operators to silence it); a public feature-introspection API or registry (no consumer needs more than the log line).

### Rate limiting: one constructor, in its own package

`ratelimit.Middleware(cfg ...ratelimit.Config)`, in package `middleware/ratelimit`, is the one constructor of rate limiting. The middleware it returns shares its store across every route it is attached to, so one value attached to two routes spends one budget. There is no object form, and the rate limiter is not a component ([middleware spec](../specs/middleware.md#rate-limiting-middlewareratelimit)).

**Not a component.** A value is a component when it owns something the App must release or prepare — a goroutine, a connection it opened, a buffer to flush, state it loads at start ([ADR-024](024-lifecycle-components.md)); being middleware is a reason neither way. The default in-memory store runs no goroutine — expired buckets are swept inline during `Take` — so it holds nothing the drain must stop, and a `Shutdown` would only release its memory early. The in-memory store keeps `Close`, which go-limiter's `limiter.Store` interface requires. A store the application passes in through `Config.Store` stays its owner's: when it holds a connection or a goroutine, the application registers it as a component of its own, and the middleware never closes it.

**Its own package.** Rate limiting is the one built-in middleware that imports a third-party module: inside `middleware`, a build that imports the package for `CORS` alone would link go-limiter and need its `go.sum` entry. In `middleware/ratelimit` only its users compile it, and a test keeps go-limiter out of the dependency graph of `middleware`. The package keeps the configuration-struct convention and follows `auth.Middleware`'s naming, `Middleware(cfg ...Config)` and `Config`, with `Config.Skipper` keeping the type `middleware.Skipper`. It stays in the root module: a module of its own waits for a consumer that cannot resolve a version conflict or must keep go-limiter out of its module graph.

Rejected:

- **A no-op `Component` on every middleware**, so that any middleware could be handed to `Manage` alike: it registers teardowns that do nothing and makes the type say "component" for values that own nothing, which is the question the component rule answers.
- **"Middleware is never a component"**: a middleware that does own a goroutine or a connection would be left without a teardown; the rule is about what a value owns, not about its role.
- **Keeping `NewRateLimiter` without its lifecycle, for sharing a store across routes**: the function form already shares its store across every route its value is attached to, so the object would be a second spelling of one middleware.

## Rejected Alternatives

### Configuration-Driven Activation (Rejected)

Activating or parameterizing middleware from configuration files — framework-read `middleware.*` keys (`middleware.cors.enabled`, `middleware.timeout.duration`) behind a `UseConfiguredMiddleware()` bootstrapper — was considered and rejected. Discoverability: `credo.With*` options and the `middleware` package surface in IDE autocomplete, while stringly-typed config keys do not, and a typo silently disables the middleware it names. Doctrine: Credo's configuration architecture (ADR-005) is built on typed snapshots — string keys never appear in business code. Credo reads its own `server` section, and the `i18n` section once `UseI18n` is called, but no configuration key activates a feature or a middleware; the call in code does. Explicitness ([ADR-001 goal 3](001-framework-identity-and-goals.md#3-integrated-first-explicit-boundaries)): the middleware stack is part of the application's composition and belongs visibly in code. Environment-dependent parameters remain fully supported the doctrinal way — the application unmarshals its own typed config and passes it to the middleware constructor (`middleware.CORS(middleware.CORSConfig{AllowOrigins: cfg.CORS.Origins})`).

## Consequences

**Positive:**

- Single type — no confusion about which middleware type to use
- Precompiled chains — zero allocation on hot path
- Meta-driven — declarative, no per-route middleware wiring
- Config struct pattern — readable, discoverable options via `Middleware(cfg ...Config)`, read by one rule: a zero field selects its default
- Stdlib interop via WrapStdMiddleware
- One authoritative access record and one recovery layer, observed after error rendering and compression, with one public path per feature
- One record of the final response, `*Exchange`, for the access log and every other observer

**Negative:**

- `PreRouting` middleware runs for the router's 404 and 405 too; a middleware that must not belongs on the routed tier

- Precompilation means no dynamic middleware addition at runtime
- Framework features cannot be scoped per group or route; route-specific policy is expressed through their selection knobs or ordinary middleware
