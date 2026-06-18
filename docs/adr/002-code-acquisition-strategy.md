# ADR-002: Code Acquisition Strategy

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** ADR-001

## Context

An all-in-one framework (ADR-001) requires numerous components: router, DI, config, validation, observability, data access, etc. For each component, three paths are possible:

1. **Write from scratch**: Full control but high cost, unproven code.
2. **Adapt (fork & own)**: Copy proven open-source code in a license-compliant manner, adapt to Credo's architecture, and take ownership. Inter-component incompatibilities and unnecessary features are resolved at the framework level.
3. **Wrap (import & pin)**: Import the library directly, wrap it behind a thin adapter layer exposing the Credo API, and pin the version.

Each approach has different trade-offs. Adapt gives full control over behavior but requires manually tracking upstream changes. Wrap automatically receives security patches and compatibility updates from upstream, but control over the API surface is limited.

## Decision

A hybrid strategy is adopted. The criterion that determines which path to choose:

### Adapt (fork & own) — Components whose behavior we want to own

Components that shape the framework's core experience and directly affect the API surface are adapted:

- **Router & radix tree** (Chi source)
- **Context & binder** (Echo source)
- **Validation engine** (ozzo-validation source)
- **DI container** (samber/do source)
- **Config loader** (koanf source)
- **Middleware infrastructure** (Chi + Echo source)
- **i18n** (go-i18n source)
- **Worker cron parser** (robfig/cron v3 source, expression parser only)

For these components, Credo evolves the API independently from upstream, removes unnecessary features, and ensures inter-component consistency.

### Wrap + pin — Components with fast evolution and high security pressure

Components that implement specs/protocols, have high CVE pressure, and evolve rapidly are wrapped:

- **OpenTelemetry SDK** (trace, metrics)
- **Prometheus client**
- **DB drivers** (pgx, go-sql-driver/mysql, etc.)
- **gRPC runtime**
- **WebSocket protocol** (coder/websocket)
- **Migration engine** (`bun/migrate` — part of the already-wrapped uptrace/bun module)
- **OpenAPI types** (kin-openapi)
- **Rate limiting** (go-limiter)
- **Pub/Sub interfaces** (watermill)

For these components, Credo provides a thin adapter layer and pins the version. Security patches are received via `go get -u`.

### Direct import — Small, stable utilities

Low-level, small tools with stable APIs are imported directly:

- `golang.org/x/text/language` (i18n BCP 47)
- CLDR plural data
- Small utility libraries

### Adaptation Process

Every adapted file follows this sequence:

```
1. LICENSE CHECK  → Verify that the source license permits adaptation
2. COPY           → Bring files into Credo's directory structure
3. ATTRIBUTE      → Retain original copyright header at top of file
4. ADAPT          → Align package name, imports, naming with Credo
5. SIMPLIFY       → Remove code unnecessary for Credo's goals
6. TEST           → Copy/adapt original tests, add Credo-specific tests
7. DOCUMENT       → Update NOTICES file and package doc.go
```

### Attribution Rules

- Every adapted file retains the original copyright header at the top.
- The NOTICES file (project root) lists each source project.
- When code has been >80% rewritten, an "Originally derived from [project]" note suffices, but the NOTICES entry must remain.

## Consequences

**Positive:**

- Full control and consistency over core components
- Security/compatibility updates are easier for infrastructure components
- Building on a proven codebase — no risk of writing from scratch
- License compliance is maintained (MIT/BSD-3/ISC/Apache-2.0)

**Negative:**

- Upstream tracking burden for adapted core components
- Two different acquisition models = requires clarity within the team (which component belongs to which category)
- Limited API surface control for wrapped components
