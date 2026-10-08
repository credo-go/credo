# ADR-022: Bootstrap Phases and DI Ownership

**Status:** Accepted, implemented 2026-09-05 (DI minor); v0.24.0 decisions accepted, pending implementation ([plan](../plans/components-and-sequential-bootstrap.md)) **Date:** 2026-09-05 **Depends on:** ADR-004, ADR-006, ADR-009 **Specification:** [Bootstrap and DI lifecycle](../specs/bootstrap-and-di-lifecycle.md) **Delivery:** shipped in the DI minor of 2026-09-05; progress in [TODO](../../TODO.md#pre-v1-contract-migration)

## Context

The review of v0.18.0 identified separate lifecycle defects: shutdown uses reverse registration order, replacement abandons the previous instance, opaque factories can lose cycle detection, resolution has no terminal teardown boundary, and constructor/shutdown panics lack a consistent completion contract. These are ownership and coordination problems; replacing the whole DI API or introducing a general service framework is unnecessary.

A scan of the maintainer's downstream applications found no factory or Replace use. It did find pre-Run resolution and an optional worker-pool existence probe. Framework store/worker adoption uses the registration-time AdoptValue path. Consumer absence does not prove an API is safe to remove internally; migration includes those integration flows.

## Decision

Keep App, typed constructors, explicit `Infra`, singleton scope, aliases and ordered collections. Separate DI validation/freeze from HTTP preparation/freeze. Bootstrap follows the documented order of [Sequential bootstrap and three error phases](#sequential-bootstrap-and-three-error-phases): controllers and DI-backed renderers are built after `Finalize`, and the App prepares once through Finalize → compile → publish. Teardown drains before DI closing, then tears down consumers before their visible dependencies.

DI-independent HTTP setup may precede Finalize. Managed serving and direct `ServeHTTP` share one stored preparation result. Preparation admission closes HTTP writes; DI Finalize alone does not. Bootstrap Shutdown competes atomically with managed start and direct preparation publication. It accepts an App in building, freezes writes without requiring successful Seal, runs serverless drain and ends stopped. Owners of external HTTP servers still drain those servers themselves.

Preparation failures remain repeatable developer errors: managed entry points return them, and `ServeHTTP` panics with the stored failure while lifecycle admission is open. Lifecycle rejection returns the callback-free default 503 response specified by ADR-009 and the lifecycle contract. A prepared stopping App retains its drain behavior; stopped always rejects new dispatch.

Public Resolve belongs after Finalize, Replace before it. `AdoptValue[T]` is the shared registration operation: read an existing prebuilt binding, validate, then atomically compare-and-protect the same binding. It does not execute constructors; invalid values remain repairable and a concurrent replacement/phase change cannot publish stale adoption. A preprovided Registry constructor is rejected without invocation. No general early Resolve/Peek API is introduced. Successful Replace returns the previous created instance and transfers its cleanup responsibility to the caller; the boolean means an instance existed, not merely a binding. Failed replacement changes neither registration nor ownership. Add non-resolving `Has[T]`; remove factory registration and its proposed runtime-edge machinery. Constructor-captured service-locator calls are unsupported.

Teardown enters closing only when the drain reaches the internal tier ([ADR-024](024-lifecycle-components.md)). Closing/closed resolution rejects with an inspectable `credo.ErrDIClosed` sentinel, including cached resolution and delivery racing teardown. Track successfully constructed instances even when caller delivery is rejected. Constructor errors and panics are terminal, shared by waiters, and never retried automatically.

Use a Kahn ready queue with reverse-registration tie breaking over the component registry's resources, each keyed by resource identity ([ADR-024](024-lifecycle-components.md)). Aliases and collection edges participate; hidden dependencies inside prebuilt values do not. Pending builds block their dependencies while independent ready vertices can close. Retiring an intermediate binding that is not a component preserves transitive order. Report graph inconsistencies; never fall back to closing blocked dependencies out of order.

Teardown calls use helper-based completion-or-context waiting under one shared budget: sequential in the internal tier, concurrent in the ingress tier for resources no edge orders. Recover on the invoking goroutine. A timed-out helper is still incomplete and keeps dependencies blocked; error/panic completion retires the vertex. Only construction completing after the shutdown context ends gets one separate, fixed five-second best-effort cleanup attempt. This budget has no configuration option and does not apply to normal teardown calls. No ordinary skipped or still-running callback receives that new budget.

Keep `Shutdown(ctx) error`; failures expose `*credo.LifecycleError` ([ADR-024](024-lifecycle-components.md#one-error)), a deterministic immutable snapshot naming each component that failed, panicked, was abandoned or was kept open, with its tier, phase, blockers and timing, plus `Unwrap() []error`; it generalizes the `*credo.DIShutdownError` this decision first introduced. `*credo.DIPanicError` retains type, phase, original value and stack, and unwraps error-valued panics. Late completion is logged and cannot mutate the returned report. Stop hooks capture dependencies; resolving from them is unsupported. A stopping-state Debug diagnostic must not misclassify active HTTP work as a hook violation.

## Sequential bootstrap and three error phases

### Problem

Every DI and feature registration API was safe for concurrent use and checked its phase at runtime, and four notions of "closed" overlapped beside the lifecycle state: the App's `frozen` flag and `prepMu`, `installFeature`'s second `checkFrozen` under `prepMu`, and the container's `frozen`, `sealed` and `closing`. The concurrency was paid for, unevenly, and used by nobody. `store.Register` ran a preflight, a reservation, a second preflight inside it, a network ping and a commit, and adopted the registry when it lost a creation race; `worker` asked `CanProvideValue` about a type it never registers, only to learn whether the container was still open, and adopted a pool that a racing call created; route and hook registration, meanwhile, were not synchronized at all. Bootstrap was sequential in practice, and no document said so. Ordering mistakes surfaced as runtime panics that the five-step summary (provide, `Finalize`, resolve, routes, run) did not predict: a worker registration must precede `Finalize` while the controllers beside it are resolved after it, and a readiness check built from a resolved value must follow `UseHealth`, so a bootstrap written in the natural order — provide, routes, workers, run — met two panics at once.

### Decision

**The contract.** Bootstrap is sequential: registration calls come from the goroutine that builds the App, before it runs, and are not safe for concurrent use. The root package documentation, this ADR and the [bootstrap spec](../specs/bootstrap-and-di-lifecycle.md) state it.

**The documented order** names every satellite:

1. configuration (`credo.New`, or `credo.WithRawConfig` over `config.Load`);
2. `Provide` and `ProvideValue`;
3. feature mounts and satellite registrations, in any order among themselves — `UseI18n`, `UseHealth`, workers, stores, `Manage`;
4. `Finalize`;
5. `Resolve`, routes and anything built from a resolved value, a readiness check included;
6. `Run`.

There is no resolve-then-provide step. A value that needs a resolved dependency and I/O to exist is a component whose `Start` does that work ([ADR-024](024-lifecycle-components.md)), not a late registration. `credo.New()`, routes and `Run()` stay the minimal shape ([ADR-001](001-framework-identity-and-goals.md)).

**Three error phases** extend the package documentation's "Panics and Errors":

| Phase | Reports | How |
| --- | --- | --- |
| Registration | Misuse known at the call site: a constructor of the wrong shape, a duplicate binding, a misused registration option, a call in the wrong phase | Panics with the call site's message; registration performs no I/O |
| `Finalize` | What only the whole graph reveals: missing dependencies, each with its whole path (`OrderService → PaymentClient → *http.Client (not registered)`), cycles, and an internal component that depends on an ingress one | Returns the errors joined in registration order |
| `Start` | I/O: a store's ping, i18n catalog reads, a component's `Start` | Returns the error; the start rolls back ([ADR-024](024-lifecycle-components.md)) |

Consequently `Provide`, `ProvideValue`, `Alias` and `BindMany` return nothing and panic on misuse, a misused registration option included, and `Resolve` before `Finalize` panics. A DI registration or a `Manage` call after `Finalize` panics, as does any registration after the App is prepared or shut down, each with the call site's message. A component's `Start` runs in the start phase, where its error rolls the start back.

**Accepted, pending implementation (v0.24.0, W5, W6).** Until W5, `store.Register` pings its store and `UseI18n` reads its catalogs at the call and return those errors; until W6, the worker registrations return their errors instead of panicking.

**Unchanged.** The contract covers registration, not the running App. After `Finalize`, `Resolve` stays safe for concurrent use: first resolutions of one singleton share one construction, and a resolution that races the drain returns an error wrapping `ErrDIClosed`. The one-time preparation that concurrent first `ServeHTTP` calls share stays synchronized: it belongs to the running App, not to registration. `Finalize` stays the DI phase boundary, and bootstrap `Shutdown` from `building` stays accepted.

### Removes

- the statement, and the machinery, that registration is safe for concurrent use: `installFeature`'s double check around `prepMu`, and the container's freeze flags as public states;
- with W3, W5 and W6, the machinery whose only purpose was concurrent coordination through the container: `CanProvideValue`, `AdoptValue`, `registrationProbe`, `ensurePool`/`adoptPool`, `ensureRegistry`/`adoptRegistry` and `store.Register`'s reservation;
- the "not finalized" error of `Resolve`, and the errors the registration calls return;
- the tests of concurrent registration, which are deleted rather than loosened.

### Rejected

- **Keeping the concurrent-registration contract.** It defends a bootstrap nobody performs, unevenly — route and hook registration were never covered — and the cost of dropping it is that an application registering from several goroutines must serialize; none is known.
- **A `Builder → Build() → App` type split.** Its own gain is that `Resolve` on the builder and `Provide` on the built App become compile errors; nothing else moves to compile time, because handlers close over resolved services and routes therefore stay on the App. The price is a second type to learn, a `credo.New()` shortcut that keeps the old shape alive beside the new one, and module helpers whose two halves take two different types. With framework infrastructure out of the container the DI surface on the App is seven methods and `Finalize` ([ADR-004](004-dependency-injection-and-infra.md)), which does not justify a second type; the split is revisited only if the App remains a hub for reasons other than DI.
- **A registration window after `Finalize`** — registering workers, stores or other components once values have been resolved. It reopens the ordering problem this section closes; a value that needs resolved dependencies is a component whose `Start` does the work, and a DI-provided worker is registered before `Finalize` in its provided form.

## Ownership through the component registry

**Accepted, pending implementation (v0.24.0, W3, W5, W6)** for the parts that remove adoption, `Replace`, protected bindings, `store`'s ledger and the framework's own bindings; when they ship, this section replaces the Decision's paragraph on `AdoptValue` and `Replace`. The component registry, resource identity, `credo.Borrowed()`, `credo.Closer()`, `credo.Override()` and the teardown below are implemented.

### Problem

Ownership was decided in three places: the container closed a bound `Shutdowner`; `store.Register` kept a ledger of resource identities with its own caller-owned option; and `AdoptValue` and protected bindings let an integration take ownership of a value the application had bound, at the price of defending that value against `Replace`. A resource several bindings hold — one pointer under a concrete binding and an adapter's interface view — was shut down once per holder, the first time while consumers of another holder could still run.

### Decision

Ownership is decided where a value is registered, and the kernel's component registry holds it ([ADR-024](024-lifecycle-components.md)):

- A DI singleton the App owns is a component when it has `Shutdown`; a resource whose teardown is `Close` is one through `credo.Closer()`; a value or constructor handed to `Manage` is one by registration. The App owns what it builds and what it is given, unless `credo.Borrowed()` on `ProvideValue` leaves starting and shutting down to the caller ([ADR-004](004-dependency-injection-and-infra.md)).
- Within one App, the registry keys components by resource identity: one resource has one teardown, run once, after the consumers of every holder. The holders that have a teardown agree on its kind — `Shutdown`, or `Close` through `credo.Closer()` — and the one registered first runs it; a borrowed resource is torn down through none of them. `store`'s identity ledger becomes this rule, with its refusal of mixed ownership.
- Nothing framework-owned is bound in the container, so nothing is adopted or protected. A store registration names a binding by type and the start phase resolves it once, after `Finalize` and every override ([ADR-015](015-data-access.md)); the worker supervisor is not bound at all ([ADR-023](023-worker-system.md)).
- `credo.Override()` replaces a binding before `Finalize`, when no constructor has run, so an override never supersedes an instance the App built, and `Replace`'s ownership transfer has nothing left to transfer.

Teardown keeps this ADR's mechanics, now stated for every component in [ADR-024](024-lifecycle-components.md): the Kahn order over the static graph with reverse registration as the tie-break, one bounded attempt per component under the shared deadline, an abandoned teardown that keeps its dependencies open and is reported, panic isolation, the single fixed five-second late attempt for a construction completing after the deadline, and one immutable report that unwraps its causes, `*credo.LifecycleError`. `ErrDIClosed` and `DIPanicError` for construction are unchanged; the closing boundary is the start of the internal tier.

### Removes

`AdoptValue` and its registration-time adoption; `Replace`, `MustReplace` and the ownership they transfer; protected bindings; `store.Register`'s ledger, reservation and `WithCallerOwnedLifecycle`; DI-owned teardown as a mechanism separate from the components'.

## Decision closure

G1/G2 were accepted on 2026-09-05: reject Registry constructors during registration, use one AdoptValue operation, expose ErrDIClosed/DIShutdownError/DIPanicError (`DIShutdownError` since generalized into `LifecycleError`), and use a fixed five-second late-construction cleanup wait. Their regression requirements are in the specification. These decisions closed the design gates; the DI minor implements them with the regression tests the specification requires.

## Adaptation and alternatives

The adaptation reference is [samber/do v2.1.0](https://github.com/samber/do/releases/tag/v2.1.0), tag commit `f0d927f`, reviewed during the design. Reuse dependency-bookkeeping and diagnostic ideas; Credo adds terminal state, static-graph ownership, panic isolation and unwrapping. The reference does not establish the upstream revision originally copied into Credo. Preserve notices and separate a known adaptation date from an unknown upstream revision.

Rejected: protect-on-read before validation; bulk wait-for-builds before any cleanup; unbounded construction barriers; inline cleanup presented as a hard timeout; automatic retry after a panic; out-of-order fallback; dynamic factory graphs. Scopes, transient services, cloning and a global container remain outside this work. Read-only DI explanation is backlog, independent of OTel.

## Consequences

Bootstrap has an explicit composition boundary and a cleanup path even after failed validation. Shutdown order follows observable dependencies, and cancellation limits waiting without claiming to stop arbitrary user code. The change landed as coordinated changes across root/internal DI, store, worker, testutil and lifecycle tests in one DI minor. Consumer migration adds an error-checked Finalize before constructor resolution; no one-minor announcement or v1-batch deferral was required.

With sequential bootstrap, a contract that was true in practice becomes a promise, and registration loses its synchronization instead of gaining more. Every mistake has one phase: the line that misused a registration panics, `Finalize` reports the whole graph at once, and `Start` reports a component's I/O. Applications migrate by dropping the error checks of registration calls and by moving any registration that follows `Finalize` before it.

**Accepted, pending implementation (v0.24.0, W3, W5).** `Start` also reports a store's ping and i18n catalog reads, and applications drop the `Must*` registration twins.
