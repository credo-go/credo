# ADR-003: Application Architecture

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** ADR-001 **Related:** ADR-004, ADR-015 **Guide:** [Dependency injection guide](../guides/dependency-injection.md)

## Context

An enterprise-targeted framework (ADR-001) must take a position on the architectural structure of applications. There are two extremes:

1. **Enforce structure**: The framework mandates a specific layered architecture. Common in Java/Spring and .NET worlds. Provides consistency but creates boilerplate overhead for small projects.

2. **Structure-agnostic**: The framework makes no architectural choices; the developer is free. Most routers/frameworks in the Go ecosystem take this position. Provides flexibility but leads to inconsistency in large projects.

Credo's primary target is mid-to-large scale projects ([ADR-001](001-framework-identity-and-goals.md)), which in practice already use a layered architecture. However, forcing it through the framework creates unnecessary ceremony for small projects and prototypes, which must stay light.

Additionally, Go's own tools (interfaces, package structure) already naturally support clean architecture. What the framework should do is facilitate this natural structure, not hinder it.

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
