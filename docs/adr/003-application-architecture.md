# ADR-003: Application Architecture

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** ADR-001 **Related:** ADR-004, ADR-007, ADR-015, ADR-022 **Specification:** [Container spec](../specs/container.md#sequential-bootstrap) **Guide:** [Dependency injection guide](../guides/dependency-injection.md)

## Context

An enterprise-targeted framework (ADR-001) must take a position on the architectural structure of applications. There are two extremes:

1. **Enforce structure**: The framework mandates a specific layered architecture. Common in Java/Spring and .NET worlds. Provides consistency but creates boilerplate overhead for small projects.

2. **Structure-agnostic**: The framework makes no architectural choices; the developer is free. Most routers/frameworks in the Go ecosystem take this position. Provides flexibility but leads to inconsistency in large projects.

Credo's primary target is mid-to-large scale projects ([ADR-001](001-framework-identity-and-goals.md)), which in practice already use a layered architecture. However, forcing it through the framework creates unnecessary ceremony for small projects and prototypes, which must stay light.

Additionally, Go's own tools (interfaces, package structure) already naturally support clean architecture. What the framework should do is facilitate this natural structure, not hinder it.

A larger application is also split into parts — a package per domain, each with its services and its handlers. Registered from a function per part that takes `*credo.App`, a part that resolves its services to build its handlers finalizes the container, so a part registered after it can no longer register a service: the composition root would order every part's services before every part's handlers by hand, and a test's override, applied after that wiring, would come after the handlers had finalized the container and panic.

## Decision

### Clean Architecture: Default, Not Mandatory

Credo **offers** the Controller → Service → Repository layered architecture as the **default** but **does not mandate** it.

**Layer responsibilities:**

| Layer | Responsibility | Dependency direction |
| --- | --- | --- |
| Controller (Handler) | HTTP request/response, input validation, response formatting | → Service |
| Service | Business logic, orchestration, transaction management | → Repository |
| Repository | Data access, queries, persistence | → Store |

The dependency direction is always from outer to inner. Inner layers are unaware of outer layers.

### Default Mechanisms

1. **CLI scaffold (planned)**: A project-scaffolding CLI ([`cmd/credo`](../../cmd/credo/README.md), not yet implemented) is to generate the layered structure by default, with an alternative flat template for small projects and prototypes. Until it exists, the layers are wired by hand as the [DI guide](../guides/dependency-injection.md) shows.

2. **Documentation**: Application-scale material — the [DI guide](../guides/dependency-injection.md) and [`examples/saas`](../../examples/saas) — uses the layered structure; quickstarts stay flat. The "recommended" path is always the layered structure.

3. **Framework tools**: Tools such as DI lifecycle, transaction propagation, and scoped logging reward the layered structure — using these tools is easiest when working with a layered architecture.

### Bypass Path

For small projects and prototypes, the layered structure can be bypassed. The framework does not prevent direct handler usage, and the planned CLI is to offer a flat template:

```go
// Minimal — no scaffold, direct handler
app.GET("/ping", func(ctx *credo.Context) error {
    return ctx.Response().JSON(http.StatusOK, map[string]string{"status": "ok"})
})
```

### Application Modules

An application can be split into modules. A module is a part of an application, its services, then its handlers:

```go
// Module is a part of an application: its services, then its handlers.
type Module interface {
	Services(app *App) // Provide, ProvideValue, Manage, workers
	Handlers(app *App) // routes and other entry points; may Resolve
}

app.UseModules(users.Module{}, billing.Module{})
```

`UseModules` runs every module's `Services` in order, then every module's `Handlers`. The two phases exist whatever the type count, since handlers close over resolved services, and running all services first is what keeps the implicit `Finalize` safe in a modular application: the first `Resolve` inside a `Handlers` method comes after every registration ([container spec](../specs/container.md#sequential-bootstrap)). The second phase is named for what it registers, the application's entry points: routes now, and the gRPC services and event handlers of the planned packages, which register through the App as routes do. Both methods are required; a module with nothing for one phase leaves that method empty, since an optional phase interface would let a misspelled method go unnoticed. `UseModules` is called once: a second call would register services after the first call's handlers had finalized the container, so it panics, naming the remedy of passing every module to one call. A module composed of others calls their methods from its own.

A test passes the modules it needs to `testutil.WithModules(mods...)`, which hands them to `UseModules` with one more module last, whose `Services` applies the test's overrides: they land after every module's services and before any module's handlers resolve, with no seam beyond the public API. An override given beside a `UseModules` call in `WithWiring` would come after the handlers had finalized the container and panic, and a `UseModules` in the wiring beside `WithModules` is a second call.

`Module` also takes the place of a controller registration: a controller's routes are a method that its module's `Handlers` calls on a group, and the planned `credo make:controller` generates a module.

Rejected alternatives:

- _Collecting the modules and running them later_: at the App's preparation, a `MustResolve` in the composition root would finalize the container before any module had registered; at `Finalize`, the first `Resolve` would register routes as a side effect. Either moves registration panics away from the call that caused them.
- _A registration interface of its own for controllers, with `app.Register(controllers...)`_: a second pattern for the composition `Module` expresses.

### The App Is the One Facade

The App is the one public way to build and serve a routing tree; no `Router` is exported. A handler test runs the real pipeline — `testutil.NewApp`, the route under test, and `app.ServeHTTP` with an `httptest` recorder — so route parameters come from the path, meta from the route, and a returned error is rendered as in production ([routing guide](../guides/routing.md#testing-handlers)). An App built by `testutil.NewApp` loads no configuration from disk or the environment, so the test stays hermetic.

Rejected alternative: an exported `Router`, with `testutil` helpers that set parameters and meta and render errors without DI or the lifecycle. It would be a second public way to build a routing tree, and its helpers would reimplement the dispatch and the error pipeline.

## Consequences

**Positive:**

- Enterprise projects have a consistent, predictable structure
- Once the planned CLI exists, its scaffold removes the layered structure's boilerplate
- Small projects can avoid unnecessary ceremony
- Domain types and services hold no HTTP or request types; services depend on Credo only for infrastructure and data-access types such as the `credo.Infra` carrier and store handles, which limits framework lock-in

**Negative:**

- The "default but not mandatory" position may create different styles in the community
- The planned CLI's templates add a maintenance burden once it ships
- Layered structure may feel like over-engineering for small CRUD endpoints
