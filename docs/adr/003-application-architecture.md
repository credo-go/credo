# ADR-003: Application Architecture

**Status:** Accepted
**Date:** 2026-03-01
**Depends on:** ADR-001

## Context

An enterprise-targeted framework (ADR-001) must take a position on the
architectural structure of applications. There are two extremes:

1. **Enforce structure**: The framework mandates a specific layered
   architecture. Common in Java/Spring and .NET worlds. Provides
   consistency but creates boilerplate overhead for small projects.

2. **Structure-agnostic**: The framework makes no architectural choices;
   the developer is free. Most routers/frameworks in the Go ecosystem take
   this position. Provides flexibility but leads to inconsistency in large
   projects.

Credo's target audience is mid-to-large scale projects. These projects
effectively already use layered architecture in practice. However, forcing
it through the framework creates unnecessary ceremony for small projects
and prototypes.

Additionally, Go's own tools (interfaces, package structure) already
naturally support clean architecture. What the framework should do is
facilitate this natural structure, not hinder it.

## Decision

### Clean Architecture: Default, Not Mandatory

Credo **offers** the Controller → Service → Repository layered architecture
as the **default** but **does not mandate** it.

**Layer responsibilities:**

| Layer | Responsibility | Dependency direction |
|-------|---------------|---------------------|
| Controller (Handler) | HTTP request/response, input validation, response formatting | → Service |
| Service | Business logic, orchestration, transaction management | → Repository |
| Repository | Data access, queries, persistence | → Store |

The dependency direction is always from outer to inner. Inner layers are
unaware of outer layers.

### Default Mechanisms

1. **CLI scaffold**: The `credo generate service users` command
   automatically generates Controller + Service + Repository files,
   interfaces, and DI registration by default. Alternative templates are
   also provided for small projects and prototypes (e.g., `--template flat`).

2. **Documentation**: All examples and guides are written with clean
   architecture. The "recommended" path is always the layered structure.

3. **Framework tools**: Tools such as DI lifecycle, transaction propagation,
   and scoped logging reward the layered structure — using these tools is
   easiest when working with a layered architecture.

### Bypass Path

For small projects and prototypes, the layered structure can be bypassed.
The CLI offers alternative templates, and the framework does not prevent
direct handler usage:

```go
// Minimal — no scaffold, direct handler
app.GET("/health", func(ctx *credo.Context) error {
    return ctx.Response().JSON(200, map[string]string{"status": "ok"})
})
```

## Consequences

**Positive:**
- Enterprise projects have a consistent, predictable structure
- CLI scaffold minimizes boilerplate overhead
- Small projects can avoid unnecessary ceremony
- Domain/Service layers are not dependent on the framework (reduces framework lock-in)

**Negative:**
- The "default but not mandatory" position may create different styles in the community
- CLI scaffold maintenance burden
- Layered structure may feel like over-engineering for small CRUD endpoints
