# ADR-014: Observability

**Status:** Accepted; tracing and metrics deferred **Date:** 2026-03-01 **Depends on:** ADR-004, ADR-010 **Related:** ADR-001, ADR-002, ADR-009 **Specification:** [HTTP features](../specs/http-features.md#observability-contract), [HTTP client](../specs/httpclient.md#trace-propagation-semantics), [DI container](../specs/container.md) **Guide:** [Middleware guide](../guides/middleware.md)

## Context

Credo targets enterprise applications, where structured logs and request correlation are baseline production needs and distributed tracing and metrics follow close behind. Logging needs only the standard library (`log/slog`), so it can be part of the root framework. Tracing and metrics need fast-evolving external libraries (OpenTelemetry, Prometheus), whose integration surface is only known once a real adapter exists. An exporter that records a metric with the span's exemplar keeps the request's context past its observation of the final response, so the record it observes carries that context and reads the request's values as the Context does.

## Decision

Credo treats observability in two layers:

1. **Implemented logging baseline.** Structured `slog` logging, panic recovery diagnostics, server-error (5xx) logging by the centralized error pipeline ([ADR-009](009-handler-and-error-handling.md)), `http.Server` error bridging and the service-scoped `Infra.Logger` are part of the root framework and ready by default. The App's logger is the one given with `credo.WithLogger`, else one built from the configuration's `log` section (`log.level`, `log.format`), which a reload applies to the logger and to every logger derived from it, else a text handler on stderr at Info ([ADR-005](005-configuration-architecture.md#the-default-logger-is-configured-the-log-section)). Request ID propagation and access logging are explicit features (`app.UseRequestID`, `app.UseAccessLog`; [ADR-010](010-middleware-architecture.md#built-in-http-feature-configuration-criterion)): neither enables the other, and leaving access logging out never silences framework or application diagnostics. The access-record contract lives in the [HTTP features spec](../specs/http-features.md#observability-contract); access-record level and filtering are decided in [ADR-010](010-middleware-architecture.md#access-log-filtering). Outbound trace continuity is opt-in: with `httpclient.WithTracePropagation()`, outbound calls carry a W3C `traceparent` derived, without an OpenTelemetry SDK, from the inbound trace context the application extracts with `TraceContextFromRequest` and attaches with `SetTraceContext` (a new span ID per attempt), so trace IDs stay continuous across services ([httpclient spec](../specs/httpclient.md#trace-propagation-semantics)).
2. **Deferred telemetry adapters.** Credo deliberately exposes no public metrics or tracing carrier interfaces, no no-op providers and no `Infra` fields for them yet. Shipping `Metrics`/`Tracer` fields and their root-package interfaces before a real OpenTelemetry or Prometheus adapter exists would freeze placeholder interfaces, forcing the eventual implementation to conform to guesses rather than to real adapter needs. Tracing and metrics are designed against real adapters in the observability release ([TODO.md](../../TODO.md#35-observability-observability)), which covers:
   - OpenTelemetry trace provider wiring
   - OpenTelemetry propagators and span creation for server and outbound requests, and whether httpclient's manual `traceparent` API stays beside them, as the option for an application that does not link the OpenTelemetry SDK, or is superseded by them; decided before v1 freezes its names (`TraceContext`, `TraceContextFromRequest`, `SetTraceContext`, `GetTraceContext`, `NewTraceTransport`, `WithTracePropagation`)
   - SQL query trace hooks
   - a Prometheus metrics registry and request metrics
   - reload metrics: a `credo_reload_total{result}` counter and a reload-duration histogram ([ADR-020](020-reload-and-partial-config-reload.md#observability))
   - telemetry-wide cost guardrails, sampling and no-op defaults based on the real adapters (distinct from the access log's `MinLevel`/`ResultFilter`)

When tracing and metrics land, they follow the wrap + pin strategy ([ADR-002](002-code-acquisition-strategy.md)): OpenTelemetry and Prometheus remain external, version-pinned libraries, and the root package avoids importing their SDK and exporter packages. Whether `credo.Infra` gains metrics and tracing fields, and the type of each — Credo-owned interfaces validated against the adapters, or the OpenTelemetry API types, which would add a root dependency — is decided before v1 ([v1 Gate](../../TODO.md#v1-gate)). The keyed-literal guard on `credo.Infra` ([ADR-004](004-dependency-injection-and-infra.md)) keeps such fields additive for existing constructor calls.

### Observers read the request's values

An observer of the final response ([ADR-010](010-middleware-architecture.md#observers-of-the-final-response)) reads the request's values as the Context does. `Exchange.Context()` returns the request's context head as it stands when the response is final: the values set through `c.Set`, read with `Value` and their keys, and a span that a wrap enclosing the observers put there, from which the OpenTelemetry SDK takes exemplars when a metric records through that context (`Record(ctx, …)`). The head is made of immutable nodes, so an exporter may keep it beyond `Observe`; it is cancelled when the request ends. `Exchange.Get(key)` reads a typed key's value from the head, `(T, bool)` as `c.Get` returns it, with `T` inferred from the key, so an observer reads a value without a type assertion. The user is read with `Exchange.GetUser[T]`, as on the Context, since its key stays unexported ([ADR-012](012-authentication-and-authorization.md)). Where the tracing wrap sits is decided with the observability release.

The record is checked against its real consumers before v1: until the observability release, the access log is the only consumer of `Observer` and `Exchange`, and the OpenTelemetry and Prometheus adapters written for that release's decision run on them before the stability window opens, so a change they need lands before v1 freezes the record.

Rejected:

- **Reading a value only through `ex.Context().Value(key)`:** a type assertion where the handler's `c.Get` needs none, for a record that already reads the user through a method.
- **Typed keys alone, without the context:** a metrics observer would have no context to record through, so the OpenTelemetry SDK, which takes the span for an exemplar from that context, would attach none.

## Consequences

**Positive:**

- The observable-by-default goal ([ADR-001](001-framework-identity-and-goals.md)) holds today for structured logging.
- The public API avoids speculative metrics/tracing interfaces before real adapters exist.
- Telemetry fields, if added, extend `Infra` without breaking keyed literals.

**Negative:**

- Users that need tracing or metrics before the observability release must wire those tools in application code.
- The eventual tracing/metrics design remains a v1 API decision and must be reviewed separately.
