# Architecture Decision Records

An Architecture Decision Record (ADR) records one decision: what was decided, the reasons for it and the alternatives rejected with their reasons, stated as the current design and read as if written today; git history and the release notes hold how the design got there. The contract itself (signatures, option tables, step lists and examples) lives in the specification each ADR links, and the [specifications index](../specs/README.md) lists them. Status is `Accepted`, `Accepted; <part> deferred` when part of the decision waits for later work, or `Proposed`.

| ADR | Title | Status | Specification |
| --- | --- | --- | --- |
| [ADR-001](001-framework-identity-and-goals.md) | Framework Identity & Goals | Accepted | — |
| [ADR-002](002-code-acquisition-strategy.md) | Code Acquisition Strategy | Accepted | — |
| [ADR-003](003-application-architecture.md) | Application Architecture | Accepted | — |
| [ADR-004](004-dependency-injection-and-infra.md) | Dependency Injection & credo.Infra | Accepted | [Container spec](../specs/container.md) |
| [ADR-005](005-configuration-architecture.md) | Configuration Architecture | Accepted | [Config spec](../specs/config.md) |
| [ADR-006](006-application-lifecycle.md) | Application Lifecycle | Accepted | [Lifecycle spec](../specs/lifecycle.md) |
| [ADR-007](007-router-and-routing.md) | Router & Routing | Accepted | [Router spec](../specs/router.md) |
| [ADR-008](008-context-design.md) | Context Design | Accepted | [Context spec](../specs/context.md) |
| [ADR-009](009-handler-and-error-handling.md) | Handler and Error Handling | Accepted | [Context spec](../specs/context.md), [HTTP features spec](../specs/http-features.md) |
| [ADR-010](010-middleware-architecture.md) | Middleware Architecture | Accepted | [Middleware spec](../specs/middleware.md), [HTTP features spec](../specs/http-features.md) |
| [ADR-011](011-validation-strategy.md) | Validation Strategy | Accepted | [Validation spec](../specs/validation.md) |
| [ADR-012](012-authentication-and-authorization.md) | Authentication & Authorization | Accepted | [Auth spec](../specs/auth.md) |
| [ADR-013](013-internationalization.md) | Internationalization | Accepted | [i18n spec](../specs/i18n.md) |
| [ADR-014](014-observability.md) | Observability | Accepted; tracing and metrics deferred | [HTTP features](../specs/http-features.md#observability-contract), [HTTP client](../specs/httpclient.md#trace-propagation-semantics), [DI container](../specs/container.md) |
| [ADR-015](015-data-access.md) | Data Access | Accepted; cursor/keyset pagination deferred | [Store spec](../specs/store.md), [Pagination spec](../specs/pagination.md) |
| [ADR-016](016-health-checks.md) | Health Checks | Accepted; optional/critical store readiness deferred | [Health spec](../specs/health.md) |
| [ADR-017](017-static-file-serving.md) | Static File Serving | Accepted | [Static file serving spec](../specs/static.md) |
| [ADR-018](018-host-routing-and-rewrite.md) | Host Routing & URL Rewrite | Accepted | [Router spec](../specs/router.md), [Context spec](../specs/context.md), [Middleware spec](../specs/middleware.md) |
| [ADR-019](019-websocket-integration-and-drain.md) | WebSocket Integration and Drain | Accepted | [WebSocket spec](../specs/websocket.md) |
| [ADR-020](020-reload-and-partial-config-reload.md) | Reload Signal, Partial Config Reload, and TLS Certificate Rotation | Accepted | [Lifecycle spec](../specs/lifecycle.md#appreloadctx-contextcontext-error), [Config spec](../specs/config.md#reload--partial-reload-adr-020) |
| [ADR-021](021-json-output-profile.md) | JSON Output Profile | Accepted | [Context spec](../specs/context.md#json-output-profile) |
| [ADR-022](022-bootstrap-and-di-ownership.md) | Bootstrap Phases and DI Ownership | Accepted | [Container spec](../specs/container.md#sequential-bootstrap), [Lifecycle spec](../specs/lifecycle.md) |
| [ADR-023](023-worker-system.md) | Worker System | Accepted | [Worker spec](../specs/worker.md) |
| [ADR-024](024-lifecycle-components.md) | Lifecycle Components | Accepted | [Lifecycle spec](../specs/lifecycle.md), [Container spec](../specs/container.md) |
