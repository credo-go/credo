# ADR-004: Dependency Injection & credo.Infra

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** ADR-001, ADR-003 **Related:** ADR-005, ADR-014, ADR-015, ADR-022, ADR-023, ADR-024 **Specification:** [Container spec](../specs/container.md) **Guide:** [Dependency injection guide](../guides/dependency-injection.md)

## Context

Dependency injection is a fundamental need for enterprise applications (ADR-001, ADR-003). Credo's DI mechanism must address two distinct needs:

1. **Business dependencies** (DB, repo, service): Each service requires different combinations. Must be explicit, mockable, and type-safe.

2. **Infrastructure dependencies** (a logger today; metrics and tracing once they are designed): Nearly every service needs them. Passing each one as its own parameter to every constructor is verbose, but implicit injection — an embedded struct the framework populates by reflection — hides what a service depends on, which conflicts with Go's explicitness (Rejected Alternatives).

## Decision

### Container: Generics-Based DI

The DI container lives in `internal/di` and is exposed as typed generic methods on `*App` — `app.Provide[T](constructor)`, `app.Resolve[T]()` and the rest of the surface below. It is Credo-specific, not a standalone DI library.

- **Lifecycle**: Singleton
- Reflection inspects a constructor's signature at registration and calls the constructor once per singleton (`reflect.Value.Call`). Resolving a built singleton is a map lookup with no `reflect.Value` call; `ResolveAll` assembles its slice with reflection on each call.

### Interface Alias

`Alias[I, T]()` maps an interface to one singleton: `Resolve[I]` and a constructor parameter of type `I` receive the instance of `T`, which is registered once with `Provide` or `ProvideValue`. Constructors return concrete types while their consumers depend on interfaces, with no second registration. The [container spec](../specs/container.md#interface-aliasing) states the rules.

### Ordered Interface Collections

Some application components need an ordered set of implementations rather than one default implementation: notification senders, hooks, subscribers, policy evaluators, or plugin chains. `BindMany[I, T]()` adds a registered concrete type to the ordered collection of `I`, which `ResolveAll[I]()` and a constructor parameter of type `[]I` receive in binding order — empty when nothing is bound. It is collection wiring, independent of `Alias`: neither implies the other, and `Resolve[I]` is unaffected. Named or keyed bindings are deliberately not introduced for this ([container spec](../specs/container.md#ordered-interface-collections)).

### The Public Surface

A container that also holds framework infrastructure must defend that infrastructure against the application that owns the container — protection against overrides, adoption against registration races, preflights against a closing phase — and the framework ends up resolving its own seams from it per request, the service-locator pattern this ADR rejects for applications. Each such defence is a method that exists for a framework integration, not for the application.

**Decision.** Framework infrastructure lives in kernel-owned registries — the component registry ([ADR-024](024-lifecycle-components.md)), the readiness aggregate and the store registry ([ADR-015](015-data-access.md#registration)) — and the container holds only the application's bindings. Its public surface is seven methods — `Provide`, `ProvideValue`, `Alias`, `BindMany`, `Has`, `Resolve` and `ResolveAll` — and the phase verb `Finalize`; the [container spec](../specs/container.md#the-surface) gives the signatures.

Registration calls return nothing and panic on misuse ([ADR-022](022-bootstrap-and-di-ownership.md#sequential-bootstrap-and-three-error-phases)), so one form suffices and there are no `Must*` registration twins. `Has` is the one non-resolving presence probe: it never runs a constructor, reserves nothing and reports a snapshot of the registrations made so far, because "is the optional module present?" is a composition-root question with no other answer. `MustResolve` and `MustResolveAll` sit beside the surface as resolve conveniences for the composition root, not registration twins: they panic with the error `Resolve` or `ResolveAll` would return.

Nothing framework-owned is bound in the container, so nothing needs protecting: no integration monitors or shuts down one value while DI resolves another, because the framework's registrations name bindings, not values. A component is its binding ([ADR-024](024-lifecycle-components.md)): an override of the binding is the value that is started, asked for readiness and shut down. A store registration names the store's binding by type, and the start phase resolves it once, after `Finalize` and therefore after every override, so the value that is pinged, reported by `/ready` and shut down is the value DI hands the store's consumers, in tests too ([ADR-015](015-data-access.md#registration)).

### Registration Options

Handing a value to DI and handing over its teardown are separate decisions: a resource whose teardown is `Close` needs a place in the dependency order, a pool two Apps in one process share must not be shut down twice, and an override that matches no binding must fail rather than add one.

**Decision.** A binding the App owns is a component when it has `Shutdown`; what is started and asked for readiness, and the tier, are planned from the binding's type ([ADR-024](024-lifecycle-components.md#registration-and-discovery)). The tier, the owner, a `Close` teardown and an override are registration options of the type `credo.RegistrationOption` on `Provide` and `ProvideValue` — `credo.Ingress()`, `credo.Borrowed()`, `credo.Closer()` and `credo.Override()` — rather than methods, so the surface does not grow per concern and misuse panics at the call that wrote it. `credo.Ingress()` also places a component handed to `app.Manage`, or a hook, in the ingress tier, and `credo.Named(name)` names a managed component. The [container spec](../specs/container.md#registration-options) states what each option accepts and when it panics; [ADR-024](024-lifecycle-components.md#ownership-and-registration-options) gives the ownership reasons — why `Borrowed` exists beside resource identity, and why `Close` is never discovered without `Closer`.

`credo.Override()` is strict rather than an upsert, so an override that no longer matches the wiring fails instead of adding a binding nothing resolves while the test runs against the real dependency, and an accidental double registration still panics ([container spec](../specs/container.md#registration-options)).

A teardown that is not a `Close` — a client's `Disconnect(ctx)`, a `Drain` that returns at once and must be waited for — belongs to a wrapper type that embeds the client and implements `Shutdown`, registered as the binding; its constructor's parameters give the teardown's own dependencies their edges, and `sqldb.DB` wraps Bun this way. No API is added for it: [ADR-024](024-lifecycle-components.md#rejected-alternatives) rejects a teardown-function option and [defers](024-lifecycle-components.md#consequences) a constructor that returns its own cleanup function.

### Several Instances of One Type

The container keys bindings by type, so several instances of one type are several wrapper types — `type PrimaryDB struct{ *sqldb.DB }` and `type AnalyticsDB struct{ *sqldb.DB }` — each bound once. The type is the qualifier: the constructor's signature names the instance (`NewReportRepo(db AnalyticsDB)`), the compiler and `Finalize` check the choice, and the graph sees the edge, so each instance starts and stops in order. Embedding carries every capability of the handle. The rule composes with generics: `NewOutboxRepo[PrimaryDB]` and `NewOutboxRepo[AnalyticsDB]` are two bindings with no name between them. Google Wire answers the same question the same way, and the [httpclient spec](../specs/httpclient.md) applies it to several `*http.Client`s. A wrapper and the handle it embeds share one resource identity, so the wrapper, its interface view and its raw handle share one teardown ([container spec](../specs/container.md#resource-identity-and-teardown), [ADR-024](024-lifecycle-components.md#resource-identity-one-resource-one-teardown)); the [store spec](../specs/store.md#several-databases) shows the wiring for databases.

Two cases sit outside the rule. Instances whose number comes from configuration — shards, tenants — are one binding of a collection type that owns and closes them all, since a type per instance cannot be written. A read replica of one database is routing, not a second instance.

Rejected:

- **Named bindings** (`ProvideNamed`, name tags on parameter objects) — the signature does not say which instance, the name travels in a tag or a parameter object read by reflection, and a typo surfaces at `Finalize` at best: the cost for which struct-tag injection is rejected below.
- **A qualifier type in the container** (`credo.Named[*sqldb.DB, Analytics]`) — typed, but it puts the container's plumbing in every business signature and unwraps a field at each use: a wrapper with a worse name.
- **Scoped child containers** — the signature stays `*sqldb.DB`, and the instance a repository receives depends on where it was registered, which hides what the wrapper shows, for a large feature beside one graph.
- **Wiring the instances outside the container** by closure — the graph loses the edges, and an instance can close before its consumers stop.

### DI Stays the Spine

Rejected: a kernel that is complete without DI, with DI as an opt-in layer, and, as its radical form, Wire-style code generation. DI is part of Credo's identity: [ADR-001](001-framework-identity-and-goals.md) names structured dependency injection as a reflection of the enterprise target and has configuration cross module boundaries as typed structs via DI. With framework infrastructure in kernel-owned registries (The Public Surface), the framework resolves none of its own infrastructure from the container, although its start walk, drain order, resource identity and tier validation run on the DI graph; a minimal App is `credo.New()`, routes and `Run()` without a binding, so an opt-in layer would change only where DI's public API lives and how the documentation positions it. Code generation adds a tool step to every developer's loop.

The real cost of reflection-based registration is not startup time, which is paid once: a wrong constructor surfaces when `Provide` runs and a missing dependency at `Finalize`, not in the compiler. A hand-wired `main` is checked by the compiler more strictly, at the price of churn at every call site when a constructor's signature changes, and Go has no variadic type parameters, so a reflection-free `Provide` over constructors of any arity cannot be written: the choice was between arity families, code generation and reflection, and Credo keeps reflection. That cost is answered by better reporting: `Provide` checks the constructor's signature at the call and panics there ([ADR-022](022-bootstrap-and-di-ownership.md)), `Finalize` reports a missing dependency with its whole path — `OrderService → PaymentClient → *http.Client (not registered)` — and, optionally after v1, a `go/analysis` checker reports signature mistakes before the program runs.

### Finalize Phase

Constructors run only against a validated graph. `app.Finalize()` freezes the container and validates the dependency graph; `Resolve` is admitted only after it, `Has` is the registration phase's one read, and a DI registration after it panics. Construction is terminal: one value, error or panic per singleton, shared by every waiter and never retried. [ADR-022](022-bootstrap-and-di-ownership.md#resolution-after-finalize-overrides-before-it) records why and the bootstrap phases around it; the [container spec](../specs/container.md#finalize-and-container-lifecycle) states what `Finalize` reports and when it runs implicitly.

### Shutdown

The container keeps the static dependency graph that orders teardown — constructor parameters, aliases and collection edges, not dependencies hidden inside pre-built values — along which the component registry tears components down consumers first, once per resource, with bounded, never-retried attempts ([ADR-024](024-lifecycle-components.md#shutdown), [ADR-022](022-bootstrap-and-di-ownership.md#dependency-ordered-bounded-teardown)); the [container spec](../specs/container.md#shutdown) states the container's rules and the [lifecycle spec](../specs/lifecycle.md#shutdown-in-tiers) the drain sequence.

### credo.Infra: Explicit Infrastructure Carrier

`credo.Infra` is a fixed struct defined by the framework; applications cannot add fields. It carries the service-scoped Logger. Metrics and tracing are deferred to the observability release, which decides whether and how they reach `Infra` ([ADR-014](014-observability.md)); the keyed-literal guard (`_ struct{}`) is deliberate, so that adding fields does not break existing `credo.Infra{Logger: ...}` literals.

When a constructor has a parameter of type `credo.Infra`, at any position, the container produces it instead of resolving a registration: a framework provider builds it from the App's logger (`credo.WithLogger`, else the framework default stderr logger), scoped with a `service=<name>` attribute named after the type the constructor builds. A constructor without such a parameter is pure constructor injection; the developer chooses per service, and `Infra` goes first by convention, like `context.Context`. The [container spec](../specs/container.md#credoinfra-----explicit-infrastructure-carrier) gives the struct, the detection rule and examples.

### Infra Design Decisions

| Decision | Rationale |
| --- | --- |
| **Fixed struct, not extensible** | Fields are known, no field-scan/tag needed |
| **In the root package** | `Infra` is referenceable without importing the DI package, which keeps the dependency graph clean |
| **Always available** | Like `context.Context` — no need to register, container knows how to produce it |
| **Default fallback** | If no logger is configured with `credo.WithLogger`, the framework default logger is used — no panic |
| **Scoped Logger** | Each service gets a logger scoped with its own name |
| **First parameter convention** | Like Go's `context.Context` convention, Infra is placed first by convention; the container still detects it at any parameter position |
| **Config not included** | Config is a separate concern — distributed via DI as typed struct (ADR-005) |
| **Immutable** | Cannot be changed after production — snapshot semantics |

## Rejected Alternatives

| Alternative | Reason for rejection |
| --- | --- |
| Implicit Base (an embedded struct auto-populated by reflection, like Spring's `@Autowired`) | Not visible in the constructor signature; needs a reflective struct-field scan; tends to a god object (logger, config, metrics and tracer in one struct); needs special machinery to mock in tests; not idiomatic Go |
| Container as parameter (service locator) | Dependencies not visible in signature, unclear what to mock in tests |
| Struct tag injection (`credo:"inject"`) | Tag typos not caught at compile time, visual noise, field-scan reflection |
| Setter injection (`SetLogger`, `SetTracer`) | Object returned from constructor is half-initialized, lifecycle problem, implicit |
| Pure constructor params (each infra separate) | One parameter per infrastructure dependency in every constructor is verbose; `Infra` groups them into one |
| Container as separate public package | No standalone usage scenario, Credo-specific — internal is sufficient |
| RequestScoped lifecycle | Go's `context.Context` + middleware pattern provides sufficient request-scoped dependency management without DI container complexity |
| Hybrid embed (struct with embedded `credo.Infra` + resolved fields) | Reflective field population hides application boundaries. The Infra parameter beside visible constructor parameters is clearer and sufficient |
| Closure factory (`fn func(*App) (T, error)` resolving its own dependencies) | Compiler-checked signature, but the dependencies resolved inside the closure are invisible to Finalize validation, cycle detection and dependency-ordered shutdown; typed constructors keep the graph static. Capturing `app.Resolve` inside a constructor is unsupported for the same reason |
| Binding framework infrastructure in the application's container, defended by protected bindings, registration-time adoption and preflights | Each defence is a method that exists for a framework integration, and the framework would resolve its own seams from the application's container; with that infrastructure in kernel-owned registries, the framework's registrations name bindings, not values (The Public Surface) |
| Runtime replacement of a built instance | It must hand the superseded instance and its cleanup back to the caller, abandon it, or close it while the caller may still hold it; an override is a registration option checked before `Finalize`, when nothing is built (Registration Options) |

## Consequences

**Positive:**

- Every dependency is visible in the constructor signature — explicit, reviewable
- `credo.Infra` consolidates infra boilerplate into a single parameter — not verbose
- Reflection is limited to constructor inspection at registration, one constructor call per singleton and assembling `ResolveAll`'s slice; resolving a built singleton involves no `reflect.Value` call
- Easy to mock in tests — provide your own Infra struct
- Scoped logger is automatic — each service logs with its own name
- Always available — no registration dependency
- Immutable — snapshot semantics, no race conditions
- `Alias[I, T]()` enables programming to interfaces without duplicate registrations
- `BindMany[I, T]()` / `ResolveAll[I]` support ordered plugin-style composition without manual registry bootstrapping
- `Finalize()` catches dependency graph errors at startup, not at first request, and is the only gate before any constructor runs
- Registration misuse panics at the line that caused it, and `Finalize`'s report names each missing dependency's whole path
- A resource's teardown, its owner and its tier are declared where it is bound; a resource several bindings hold is torn down once
- The container holds only the application's bindings; seven methods and `Finalize` are its whole surface, and none of them exists for a framework integration
- An override is never a runtime replacement: `credo.Override()`, checked before `Finalize`, is the one way to override a binding, and an override that matches nothing panics

**Negative:**

- A constructor that needs infrastructure takes an `Infra` parameter (minimal boilerplate)
- Infra is not extensible — adding a new infra type requires a framework change
- A framework-provider hook in `internal/di` produces `credo.Infra` — one map lookup per constructor parameter
- `BindMany` adds ordering and empty-collection semantics that must be documented clearly
- Container is in `internal/di` — cannot be used as a standalone DI library
- The composition root needs an explicit, error-checked `Finalize` between registration and the first `Resolve`
- A store takes its binding and a registration by type, and several instances of one type take a wrapper type each — more types in exchange for signatures that say which instance they receive
