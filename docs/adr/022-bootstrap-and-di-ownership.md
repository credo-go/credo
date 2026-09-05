# ADR-022: Bootstrap Phases and DI Ownership

**Status:** Accepted, implemented 2026-09-05 (DI minor) **Date:** 2026-09-05 **Depends on:** ADR-004, ADR-006, ADR-009 **Specification:** [Bootstrap and DI lifecycle](../specs/bootstrap-and-di-lifecycle.md) **Delivery:** [Pre-v1 implementation plan](../plans/pre-v1-implementation.md)

## Context

The review of v0.18.0 identified separate lifecycle defects: shutdown uses reverse registration order, replacement abandons the previous instance, opaque factories can lose cycle detection, resolution has no terminal teardown boundary, and constructor/shutdown panics lack a consistent completion contract. These are ownership and coordination problems; replacing the whole DI API or introducing a general service framework is unnecessary.

A scan of the maintainer's downstream applications found no factory or Replace use. It did find pre-Run resolution and an optional worker-pool existence probe. Framework store/worker adoption uses the registration-time AdoptValue path. Consumer absence does not prove an API is safe to remove internally; migration includes those integration flows.

## Decision

Keep App, typed constructors, explicit `Infra`, singleton scope, aliases and ordered collections. Separate DI validation/freeze from HTTP preparation/freeze:

1. Construct App, register dependencies and apply overrides.
2. Finalize the DI plan; build controllers and DI-backed renderers afterward.
3. Register HTTP routes/features/hooks, then prepare once through Finalize → compile → publish.
4. Drain before DI closing, then tear down consumers before their visible dependencies.

DI-independent HTTP setup may precede Finalize. Managed serving and direct `ServeHTTP` share one stored preparation result. Preparation admission closes HTTP writes; DI Finalize alone does not. Bootstrap Shutdown competes atomically with managed start and direct preparation publication. It accepts an App in building, freezes writes without requiring successful Seal, runs serverless drain and ends stopped. Owners of external HTTP servers still drain those servers themselves.

Preparation failures remain repeatable developer errors: managed entry points return them, and `ServeHTTP` panics with the stored failure while lifecycle admission is open. Lifecycle rejection returns the callback-free default 503 response specified by ADR-009 and the lifecycle contract. A prepared stopping App retains its drain behavior; stopped always rejects new dispatch.

Public Resolve belongs after Finalize, Replace before it. `AdoptValue[T]` is the shared registration operation: read an existing prebuilt binding, validate, then atomically compare-and-protect the same binding. It does not execute constructors; invalid values remain repairable and a concurrent replacement/phase change cannot publish stale adoption. A preprovided Registry constructor is rejected without invocation. No general early Resolve/Peek API is introduced. Successful Replace returns the previous created instance and transfers its cleanup responsibility to the caller; the boolean means an instance existed, not merely a binding. Failed replacement changes neither registration nor ownership. Add non-resolving `Has[T]`; remove factory registration and its proposed runtime-edge machinery. Constructor-captured service-locator calls are unsupported.

Teardown enters closing only at the DI stage. Closing/closed resolution rejects with an inspectable `credo.ErrDIClosed` sentinel, including cached resolution and delivery racing teardown. Track successfully constructed instances even when caller delivery is rejected. Constructor errors and panics are terminal, shared by waiters, and never retried automatically.

Use a Kahn ready queue with reverse-registration tie breaking over canonical singleton vertices. Aliases and collection edges participate; hidden dependencies inside prebuilt values do not. Pending builds block their dependencies while independent ready vertices can close. Retiring an intermediate non-Shutdowner preserves transitive order. Report graph inconsistencies; never fall back to closing blocked dependencies out of order.

Normal Shutdowner calls remain sequential, with helper-based completion-or-context waiting under one shared budget. Recover on the invoking goroutine. A timed-out helper is still incomplete and keeps dependencies blocked; error/panic completion retires the vertex. Only construction completing after the shutdown context ends gets one separate, fixed five-second best-effort cleanup attempt. This budget has no configuration option and does not apply to normal shutdown callbacks. No ordinary skipped or still-running callback receives that new budget.

Keep `Shutdown(ctx) error`; failures expose `*credo.DIShutdownError`, a deterministic immutable snapshot with per-vertex state, blockers, failures and timing, plus `Unwrap() []error`. `*credo.DIPanicError` retains type, phase, original value and stack, and unwraps error-valued panics. Late completion is logged and cannot mutate the returned report. Hooks capture dependencies; resolving from drain hooks is unsupported. A stopping-state Debug diagnostic must not misclassify active HTTP work as a hook violation.

## Decision closure

G1/G2 were accepted on 2026-09-05: reject Registry constructors during registration, use one AdoptValue operation, expose ErrDIClosed/DIShutdownError/DIPanicError, and use a fixed five-second late-construction cleanup wait. Their regression requirements are in the specification. These decisions closed the design gates; the DI minor implements them with the regression tests the specification requires.

## Adaptation and alternatives

The adaptation reference is [samber/do v2.1.0](https://github.com/samber/do/releases/tag/v2.1.0), tag commit `f0d927f`, reviewed during the design. Reuse dependency-bookkeeping and diagnostic ideas; Credo adds terminal state, static-graph ownership, panic isolation and unwrapping. The reference does not establish the upstream revision originally copied into Credo. Preserve notices and separate a known adaptation date from an unknown upstream revision.

Rejected: protect-on-read before validation; bulk wait-for-builds before any cleanup; unbounded construction barriers; inline cleanup presented as a hard timeout; automatic retry after a panic; out-of-order fallback; dynamic factory graphs. Scopes, transient services, cloning and a global container remain outside this work. Read-only DI explanation is backlog, independent of OTel.

## Consequences

Bootstrap has an explicit composition boundary and a cleanup path even after failed validation. Shutdown order follows observable dependencies, and cancellation limits waiting without claiming to stop arbitrary user code. The change landed as coordinated changes across root/internal DI, store, worker, testutil and lifecycle tests in one DI minor. Consumer migration adds an error-checked Finalize before constructor resolution; no one-minor announcement or v1-batch deferral was required.
