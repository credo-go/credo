# ADR-001: Framework Identity & Goals

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** — **Related:** ADR-003, ADR-004, ADR-009, ADR-010, ADR-011, ADR-014

## Context

Web development tools in the Go ecosystem are positioned on a spectrum:

- **Library/Toolkit** (Chi, Alice, httprouter): Small, composable pieces. The developer selects and assembles them. The natural inclination of Go culture.
- **Hybrid** (Echo, Gin, Fiber): Provides router + middleware + context but leaves DI, config, DB, and similar concerns to the user.
- **Framework** (GoFr, Beego, Buffalo): Opinionated, batteries-included. Offers a consistent experience but limits flexibility.

Each approach has its trade-offs. With the toolkit approach, the developer integrates tools that may be mutually incompatible — version conflicts, API mismatches, and integration maintenance burden fall on the developer. With the framework approach, this burden shifts to the framework, letting the developer focus on business logic.

Credo's target audience includes both smaller teams that want low-boilerplate defaults and enterprise teams that need typed, reviewable boundaries for large applications. In both cases, the goal is to spend less time integrating technical infrastructure and more time building application behavior.

## Decision

Credo is built on the following core goals:

### 1. All-in-one Framework

Credo provides tools whose mutual compatibility is guaranteed by the framework. Components such as router, DI, config, validation, observability, and data access are offered under a single umbrella with consistent APIs and shared conventions.

This choice positions Credo opposite the composable toolkit approach. The flexibility provided by the toolkit approach is deliberately deprioritized; instead, consistency and integration quality take precedence.

### 2. Enterprise Target

Credo targets enterprise-grade support for mid-to-large scale projects. Clean architecture encouragement ([ADR-003](003-application-architecture.md)), structured dependency injection, built-in observability, and graceful lifecycle management are reflections of this goal. These defaults should not make small services heavy: a minimal Credo app should still be `credo.New()`, route registration, and `Run()`.

### 3. Integrated First, Explicit Boundaries

Credo does not optimize for users manually assembling every framework layer. The framework wires its own infrastructure by documented convention: logging, panic recovery and config bootstrap work with no setup, and request IDs, access logs and health probes are one `Use*` call each ([ADR-010](010-middleware-architecture.md#built-in-http-feature-configuration-criterion)).

At application boundaries, Credo stays explicit. Business dependencies remain visible in constructor signatures, config crosses module boundaries as typed structs via DI, and framework-managed infrastructure is carried through the documented `credo.Infra` type. Those boundaries are typed, reviewable, and override-friendly.

### 4. Errors Are Values

A handler is `func(*credo.Context) error`. Handlers return failures as errors instead of writing error responses, and one centralized pipeline classifies and renders them ([ADR-009](009-handler-and-error-handling.md)).

### 5. Generics Over Reflection

Typed APIs are generic, so callers work with typed values instead of runtime type assertions: `app.Provide[T]`/`Resolve[T]` in the container ([ADR-004](004-dependency-injection-and-infra.md)) and `Rule[T]`/`Field[T]` in validation ([ADR-011](011-validation-strategy.md)). Reflection is kept to what generics cannot express: JSON decoding, binder struct tags, DI constructor inspection and validation's field-name lookup.

### 6. Observable by Default

Structured `slog` logging works with no setup: each service receives a scoped logger through `credo.Infra`, and panics, server errors and `net/http` server diagnostics are logged as structured records. Request correlation and access records are explicit `Use*` features (goal 3). Tracing and metrics are deferred to the observability release ([ADR-014](014-observability.md)).

### 7. stdlib as Secondary Priority

Integrated tools (goal 1) need a shared handler signature, context type and middleware model, so Credo defines its own and optimizes for that path. The standard library stays a boundary rather than the design center: `*App` is an `http.Handler`, and `WrapStdMiddleware` adapts `func(http.Handler) http.Handler` middleware. The position taken is: "you can be stdlib-compatible, but we recommend the Credo path."

### 8. Go 1.27+ Baseline

Credo tracks the current Go release and builds on its modern standard-library APIs (`errors.AsType`, `crypto/rand.Text`, testing's `b.Loop`/`t.Context`, `sync.WaitGroup.Go`). The 1.27 floor is driven by generic methods — `app.Provide[T]`/`Resolve[T]`, `q.One[T]`/`All[T]`/`Page[T]`, and `ctx.GetUser[T]` all require the 1.27 language version.

## Consequences

**Positive:**

- Developers learn a single framework and use all layers
- Compatibility between components is guaranteed by the framework
- Consistent, predictable structure for enterprise projects
- Integrated defaults reduce setup work for small teams
- Explicit application boundaries make debugging and testing easier

**Negative:**

- Larger API surface = greater maintenance burden
- Not being stdlib-first contradicts some Go developers' expectations
- All-in-one structure makes independent use of individual components harder
- Framework-managed conventions must stay documented and override-friendly
- Framework lock-in risk (mitigated by the layering of [ADR-003](003-application-architecture.md): business logic lives in services that hold no HTTP or request types, and their framework dependencies are limited to infrastructure and data-access types such as the `credo.Infra` carrier and store handles)
