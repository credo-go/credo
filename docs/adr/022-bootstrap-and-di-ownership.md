# ADR-022: Bootstrap Phases and DI Ownership

**Status:** Accepted **Date:** 2026-09-05 **Depends on:** ADR-004, ADR-006, ADR-009 **Related:** ADR-001, ADR-015, ADR-023, ADR-024 **Specification:** [Container spec](../specs/container.md#sequential-bootstrap), [Lifecycle spec](../specs/lifecycle.md) **Guide:** [Dependency injection guide](../guides/dependency-injection.md)

## Context

A DI container that owns teardown fails in predictable ways: closing in registration order closes a dependency before its consumer; a replaced instance is abandoned; a closure factory hides edges from cycle detection and from the teardown order; resolution racing teardown hands out values that are being closed; and constructor and shutdown panics are lost, or surface without their value and stack. These are ownership and coordination problems; replacing the whole DI API or introducing a general service framework is unnecessary.

Bootstrap has forces of its own. Registration that promises concurrency safety buys synchronization no bootstrap uses, and gives it unevenly; overlapping notions of "closed" make phase errors unpredictable; and an undocumented order makes a natural bootstrap — provide, routes, workers, run — meet ordering panics: a worker registration must precede `Finalize` while the controllers beside it are resolved after it, and a readiness check built from a resolved value must follow `UseHealth`.

Ownership has one more: when it is decided in more than one place, a resource held by several bindings is shut down once per holder — the first time while consumers of another holder still run — and an integration that takes ownership of an application binding must then defend it against overrides.

## Decision

Keep the App, typed constructors, the explicit `Infra`, singleton scope, aliases and ordered collections ([ADR-004](004-dependency-injection-and-infra.md)), and fix ownership and coordination around them. The [container spec](../specs/container.md) states the DI rules; the [lifecycle spec](../specs/lifecycle.md) states preparation, the start phase and the drain.

### Two freezes and one preparation

DI validation and freeze (`Finalize`) are separate from HTTP preparation and freeze: controllers and DI-backed renderers are built from resolved values after `Finalize` and their routes registered before the App prepares, so `Finalize` closes DI writes only, and DI-independent HTTP setup may precede it. Every entry point — `Run`, `RunContext`, `ServeContext`, `App.Start` and direct `ServeHTTP` — reaches the same validated runtime model through one stored preparation, `Finalize` → compile → publish. A preparation failure is a repeatable developer error: the managed entry points return it, and `ServeHTTP` panics with the stored failure on every request while lifecycle admission is open, so no later request reaches a partly compiled handler ([lifecycle spec](../specs/lifecycle.md#preparation)).

### Bootstrap teardown

`Shutdown` is accepted while the App is building: it competes atomically with a managed start and with direct preparation, freezes HTTP and DI writes without requiring a successful `Finalize`, drains with no servers and ends stopped, so a composition root that fails after registering resources, or a test that only used `ServeHTTP`, still releases what bootstrap built. Owners of external HTTP servers drain them themselves before `App.Shutdown`. A request the lifecycle rejects receives the callback-free default 503 ([ADR-009](009-handler-and-error-handling.md)), since a renderer or resolver it would otherwise reach may capture a component that is already shut down; a prepared stopping App keeps serving its drain, and a stopped one rejects every new dispatch ([lifecycle spec](../specs/lifecycle.md#bootstrap-teardown)).

### Resolution after Finalize, overrides before it

Constructors run only after `Finalize`, so the only graph that ever executes is a validated one, and no registration helper can trigger construction as a side effect. `Resolve` belongs after `Finalize` and an override before it: `credo.Override()` replaces a binding while no constructor has run ([Ownership through the component registry](#ownership-through-the-component-registry)). There is no general early `Resolve`/`Peek`; the non-resolving `Has[T]` is the registration phase's one read, a snapshot that never constructs and reserves nothing. There is no factory registration: a constructor's dependencies are its parameters, and a constructor that captures `app.Resolve` is unsupported ([ADR-004](004-dependency-injection-and-infra.md)).

### Terminal construction

Construction completes once per singleton with one result — value, error or panic — shared by every waiter and never retried automatically. A panic is recovered on the goroutine that ran the constructor and returned as `*credo.DIPanicError`, with the type, the phase, the original value and the stack captured where it panicked; it unwraps an error-valued panic, and `MustResolve` panics with that error as its payload.

### Closing begins at the internal tier

The container enters closing when the drain reaches the internal tier ([ADR-024](024-lifecycle-components.md#two-tiers), [lifecycle spec](../specs/lifecycle.md#shutdown-in-tiers)), not when `Shutdown` is called, so the HTTP drain, the ingress tier and the internal stop hooks still see a live container. From then on resolution — a cached result, and a delivery that races teardown, included — fails with an error wrapping the sentinel `credo.ErrDIClosed`, and an instance whose construction completes after closing is still tracked and torn down. Stop hooks take their dependencies at registration; resolving from them is unsupported, and the Debug diagnostic for a resolve during the drain does not call it a hook violation, since an in-flight request may legitimately resolve.

### Dependency-ordered, bounded teardown

Teardown follows the static dependency graph — constructor parameters, aliases and collection edges, constructor and value bindings alike — consumers before the dependencies they were built from, with reverse registration order where the graph does not decide. A pending build blocks its dependencies while independent ready vertices proceed, and a built value that is not a component is transparent, so ordering survives intermediate services. Each teardown gets one attempt under the one shared drain deadline, waited for on a helper goroutine; one that misses the deadline is abandoned and keeps its dependencies open, because nothing may close a database under a component that may still use it. There is no out-of-order fallback: a blocked dependency is reported with its blockers. The [container spec](../specs/container.md#shutdown) and the [lifecycle spec](../specs/lifecycle.md#shutdown-in-tiers) state the rules.

### One late attempt

An instance whose construction completes after the drain deadline would otherwise never be torn down, so it alone gets one separate, fixed five-second best-effort cleanup attempt, on a helper goroutine with its own deadline, logged and never reported. It has no configuration option and is never granted to an ordinary teardown that missed the deadline; it is best effort because the process may exit once `Shutdown` returns.

### One immutable report

`Shutdown` keeps returning `error`. A failed or incomplete start or drain is one `*credo.LifecycleError` ([ADR-024](024-lifecycle-components.md#one-error)): a deterministic snapshot taken at the boundary, which a call returning later only logs and never mutates, with `Unwrap() []error` over every failure and the context cause, so `errors.Is` and `errors.AsType` work through App-level joins.

### Sequential bootstrap and three error phases

**The contract.** Bootstrap is sequential: registration calls come from the goroutine that builds the App, before it runs, and are not safe for concurrent use; each checks once that it is in its phase, and an application that registers from several goroutines serializes the calls itself. The root package documentation and the [container spec](../specs/container.md#sequential-bootstrap) state it.

**The documented order** gives every DI, feature and component registration its place — configuration, the application's bindings, feature mounts and component registrations, `Finalize`, then resolution and routes, then `Run` — so that a bootstrap written by following it meets no ordering panic ([container spec](../specs/container.md#documented-order)). There is no resolve-then-provide step: a value that needs a resolved dependency and I/O to exist is a component whose `Start` does that work ([ADR-024](024-lifecycle-components.md)), not a late registration. `credo.New()`, routes and `Run()` stay the minimal shape ([ADR-001](001-framework-identity-and-goals.md)).

**Three error phases.** Every mistake has one phase: registration panics at the line that misused it and performs no I/O; `Finalize` returns what only the whole graph reveals, joined in registration order; the start phase returns the I/O errors — a component's `Start`, a store's ping, the i18n catalog reads — and rolls back ([container spec](../specs/container.md#three-error-phases)). Registration calls therefore return nothing, `store.Register`, `UseI18n` and the worker registrations included.

**Messages are part of the API.** Every error and panic of the three phases names what failed — the component, binding or call — the phase and the remedy: a missing dependency with its path, an internal component with the ingress path it depends on and both fixes, `ServeHTTP`'s refusal with `App.Start` and `testutil.Start`, a misused option with the call that misused it, two holders of one resource that disagree on its owner or its teardown with both bindings and the remedies, a failed start with each component's phase and outcome. The test that triggers one asserts those parts of its message, so a message changes only on purpose.

**What stays concurrent.** The contract covers registration, not the running App: after `Finalize`, `Resolve` is safe for concurrent use, and the one-time preparation that concurrent first `ServeHTTP` calls share is synchronized.

### Ownership through the component registry

Ownership is decided where a value is registered, and the kernel's component registry holds it ([ADR-024](024-lifecycle-components.md)):

- The App owns what it builds and what it is given, unless `credo.Borrowed()` on `ProvideValue` leaves starting and shutting down to the caller; which bindings are components is [ADR-024](024-lifecycle-components.md#registration-and-discovery)'s rule.
- Within one App the registry keys components by resource identity, so one resource has one teardown, run once, after the consumers of every holder, and holders that disagree on its owner or on its kind of teardown are refused ([container spec](../specs/container.md#resource-identity-and-teardown)).
- Nothing framework-owned is bound in the container, so nothing is adopted or protected. A store registration names a binding by type and the start phase builds it once, after `Finalize` and every override ([ADR-015](015-data-access.md#registration)); the worker supervisor is not bound at all ([ADR-023](023-worker-system.md)).
- `credo.Override()` replaces a binding before `Finalize`, when no constructor has run, so an override never supersedes an instance the App built: the replaced value never becomes the App's, and whoever built it releases it.

Every component is torn down by the rules above ([Dependency-ordered, bounded teardown](#dependency-ordered-bounded-teardown)), once per resource.

### Adaptation

The dependency bookkeeping and the teardown diagnostics are adapted from samber/do, reviewed against [samber/do v2.1.0](https://github.com/samber/do/releases/tag/v2.1.0) (tag commit `f0d927f`); Credo adds terminal state, static-graph ownership, panic isolation and unwrapping. The [container spec](../specs/container.md#samberdo-adaptation-scope) lists what is kept and what is cut.

## Rejected Alternatives

- **A concurrent-registration contract.** It defends a bootstrap nobody performs, and a guarantee that some registration calls give and routes and hooks do not is no guarantee; without it, an application that registers from several goroutines serializes the calls itself.
- **A `Builder → Build() → App` type split.** Its own gain is that `Resolve` on the builder and `Provide` on the built App become compile errors; nothing else moves to compile time, because handlers close over resolved services and routes therefore stay on the App. The price is a second type to learn, a `credo.New()` shortcut that keeps the old shape alive beside the new one, and module helpers whose two halves take two different types. With framework infrastructure out of the container the DI surface on the App is seven methods and `Finalize` ([ADR-004](004-dependency-injection-and-infra.md)), which does not justify a second type; the split is revisited only if the App remains a hub for reasons other than DI.
- **A registration window after `Finalize`** — registering workers, stores or other components once values have been resolved. It reopens the ordering problem the documented order closes; a value that needs resolved dependencies is a component whose `Start` does the work, and a DI-provided worker is registered before `Finalize` in its provided form.
- **A general early `Resolve`/`Peek` during registration, or protection on every read.** It would run constructors against an unvalidated graph, or freeze an invalid binding before validation. `Has` answers the composition root's presence question, and no framework registration reads the container.
- **Reverse registration order alone for teardown.** It ignores the dependency graph: a service registered before its database would close after it.
- **A bulk wait-for-builds phase before any cleanup.** One runaway constructor would consume the budget before any independent cleanup ran; a pending build is a vertex of the ordered pass instead.
- **An unbounded construction barrier.** One runaway constructor would block process exit.
- **Running the late cleanup inline on the goroutine that completed the construction**, presented as a hard timeout. A `Shutdown` that ignores its context would block that goroutine forever, so the bound could only be cooperative.
- **Automatic retry after a constructor panic.** Construction is terminal (Terminal construction).
- **samber/do v2's out-of-order fallback**, which shuts down everything left when no vertex is ready. It closes a dependency underneath a live consumer; a blocked dependency is reported with its blockers instead.
- **samber/do v2.1.0's `ShutdownReport` shape.** It implements `error` without unwrapping, so adopting it would hide the causes from `errors.Is` and typed inspection.

## Consequences

Bootstrap has an explicit composition boundary and a cleanup path even after failed validation. Shutdown order follows observable dependencies, and cancellation limits waiting without claiming to stop arbitrary user code.

Sequential bootstrap is a promise about what every bootstrap does, and registration carries no synchronization. Every mistake has one phase: the line that misused a registration panics, `Finalize` reports the whole graph at once, and `Start` reports the I/O — a component's `Start`, a store's ping and the i18n catalog reads.

The cost: the composition root calls an error-checked `Finalize` before its first `Resolve`, an application that registers from several goroutines serializes the calls itself, and a value that needs resolved dependencies and I/O is written as a component rather than registered late.
