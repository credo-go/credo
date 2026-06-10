# ADR-001: Framework Identity & Goals

**Status:** Accepted
**Date:** 2026-03-01
**Depends on:** —

## Context

Web development tools in the Go ecosystem are positioned on a spectrum:

- **Library/Toolkit** (Chi, Alice, httprouter): Small, composable pieces.
  The developer selects and assembles them. The natural inclination of Go
  culture.
- **Hybrid** (Echo, Gin, Fiber): Provides router + middleware + context but
  leaves DI, config, DB, and similar concerns to the user.
- **Framework** (GoFr, Beego, Buffalo): Opinionated, batteries-included.
  Offers a consistent experience but limits flexibility.

Each approach has its trade-offs. With the toolkit approach, the developer
integrates tools that may be mutually incompatible — version conflicts,
API mismatches, and integration maintenance burden fall on the developer.
With the framework approach, this burden shifts to the framework, letting
the developer focus on business logic.

Credo's target audience is developer teams who want to focus on application
business logic rather than dealing with technical infrastructure decisions.

## Decision

Credo is built on the following core goals:

### 1. All-in-one Framework

Credo provides tools whose mutual compatibility is guaranteed by the
framework. Components such as router, DI, config, validation,
observability, and data access are offered under a single umbrella with
consistent APIs and shared conventions.

This choice positions Credo opposite the composable toolkit approach.
The flexibility provided by the toolkit approach is deliberately
deprioritized; instead, consistency and integration quality take
precedence.

### 2. Enterprise Target

Credo targets enterprise-grade support for mid-to-large scale projects.
Clean architecture encouragement, structured dependency injection,
built-in observability, and graceful lifecycle management are reflections
of this goal.

### 3. stdlib as Secondary Priority

The all-in-one choice naturally distances Credo from stdlib. This is not
a problem — it is a natural consequence of the primary goal. Credo defines
its own handler signature, context type, and middleware model.

stdlib compatibility is not entirely abandoned — adapters such as
`WrapStdMiddleware` are provided — but the path Credo optimizes for is its
own API. The position taken is: "you can be stdlib-compatible, but we
recommend the Credo path."

### 4. Explicit First

Credo prefers explicit APIs over implicit magic. Dependencies are visible
in constructor signatures, config is distributed as typed structs via DI,
and infrastructure is provided automatically but transparently.

This choice, aligned with Go's "explicit is better than implicit"
philosophy, makes code review, testing, and onboarding easier.

### 5. Go 1.26+ Baseline

Credo targets Go 1.26+ and actively uses modern stdlib APIs:
`errors.AsType[T]`, `crypto/rand.Text()`, `b.Loop()`, `t.Context()`,
`sync.WaitGroup.Go()`.

## Consequences

**Positive:**
- Developers learn a single framework and use all layers
- Compatibility between components is guaranteed by the framework
- Consistent, predictable structure for enterprise projects
- Explicit APIs make debugging and testing easier

**Negative:**
- Larger API surface = greater maintenance burden
- Not being stdlib-first contradicts some Go developers' expectations
- All-in-one structure makes independent use of individual components harder
- Framework lock-in risk (mitigated: clean arch layers enable domain/service
  layers that are not dependent on the framework)
