# ADR-014: Observability

**Status:** Draft (logging baseline accepted; tracing/metrics pending)
**Date:** 2026-03-01
**Depends on:** ADR-004

## Context

Credo targets enterprise applications where request correlation and structured
logs are baseline production needs. The framework already provides this
baseline in the root package:

- built-in request IDs
- default-on access logs
- `slog`-based framework logging
- `credo.Infra.Logger`, scoped per service by the DI container

Earlier drafts also placed speculative metrics and tracing carriers directly on
`credo.Infra` (`Metrics`, `Tracer`, and related root-package interfaces). Those
fields were removed before v1 on 2026-06-11. No real OpenTelemetry or
Prometheus adapter existed yet, and freezing placeholder interfaces would have
made the future implementation conform to guesses rather than to real adapter
needs.

The `observability/` directory therefore remains a planned package marker for
now. Structured logging is implemented; tracing and metrics are planned for
Phase 3.5.

## Decision

Credo treats observability in two layers:

1. **Implemented logging baseline.** Request ID propagation, access logging,
   error logging, and service-scoped `Infra.Logger` are part of the root
   framework and are enabled by default, with explicit opt-out where
   appropriate.
2. **Planned telemetry adapters.** OpenTelemetry tracing and Prometheus metrics
   will be designed in Phase 3.5 against real adapters. Until that work is
   implemented, Credo does not expose public metrics/tracing carrier
   interfaces, no-op providers, or `Infra` fields for them.

When tracing and metrics land, they should follow the wrap + pin strategy:
OpenTelemetry and Prometheus remain external, version-pinned infrastructure
libraries behind Credo-owned integration APIs. The root package should avoid
importing adapter packages directly; any root-level surface must be a small,
Credo-owned contract validated against the adapter implementation.

`credo.Infra` keeps its keyed-literal guard so future infrastructure fields can
be added without breaking constructor calls.

## Scope

Current v0.1 scope:

- `slog` logging through `Infra.Logger`
- request ID generation and propagation
- structured access logs
- server-error logging through the centralized error pipeline

Deferred Phase 3.5 scope:

- OpenTelemetry trace provider wiring
- request/server trace propagation
- outbound HTTP and SQL trace hooks
- Prometheus metrics registry and request metrics
- cost guardrails, sampling, and no-op defaults based on real adapter behavior

## Consequences

Positive:

- Credo's "observable by default" claim is true today for structured logging.
- The public API avoids speculative metrics/tracing interfaces before real
  adapters exist.
- Future telemetry work can still extend `Infra` without breaking keyed
  literals.

Negative:

- Users that need tracing or metrics before Phase 3.5 must wire those tools in
  application code.
- The eventual tracing/metrics design remains a v1 API decision and must be
  reviewed separately.
