# ADR-004: Dependency Injection & credo.Infra

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** ADR-001, ADR-003 **Related:** ADR-002, ADR-005, ADR-014, ADR-015, ADR-016, ADR-022, ADR-023, ADR-024 **Specification:** [Container spec](../specs/container.md) **Guide:** [Dependency injection guide](../guides/dependency-injection.md)

## Context

Dependency injection is a fundamental need for enterprise applications (ADR-001, ADR-003). Credo's DI mechanism must address two distinct needs:

1. **Business dependencies** (DB, repo, service): Each service requires different combinations. Must be explicit, mockable, and type-safe.

2. **Infrastructure dependencies** (a logger today; metrics and tracing once they are designed): Nearly every service needs them. Passing each one as its own parameter to every constructor is verbose, but implicit injection — an embedded struct the framework populates by reflection — hides what a service depends on, which conflicts with Go's explicitness (Rejected Alternatives).

## Decision

### Container: Generics-Based DI

The DI container lives in `internal/di` and is exposed as typed methods on `*App` — `app.Provide(constructor)`, `app.Resolve[T]()` and the rest of the surface below. It is Credo-specific, not a standalone DI library.

- **Lifecycle**: Singleton
- Reflection inspects a constructor's signature at registration and calls the constructor once per singleton (`reflect.Value.Call`). Resolving a built singleton is a map lookup with no `reflect.Value` call; `ResolveAll` assembles its slice with reflection on each call.

### Interface Alias

`Alias[I, T]()` maps an interface to one singleton: `Resolve[I]` and a constructor parameter of type `I` receive the instance of `T`, which is registered once with `Provide` or `ProvideValue`. Constructors return concrete types while their consumers depend on interfaces, with no second registration. The [container spec](../specs/container.md#interface-aliasing) states the rules.

### The Binding's Type and Aliases

**`Provide` binds what its constructor returns.** `app.Provide(constructor, opts...)` takes no type argument: the binding's type is the constructor's first result type. A constructor declared to return a concrete type binds that type, and the interfaces it serves are bound with `Alias`; a constructor declared to return an interface binds the interface. `ProvideValue[T]`, `Resolve[T]`, `Alias[I, T]` and `BindMany[I, T]` keep their type parameters, since each of them names a type its arguments do not carry. A message that names a binding keeps the form `App.Provide[*Mailer]`, the call with its binding's type, because a constructor written as a function literal has no useful name.

**An alias is a binding.** For the duplicate and override rules an `Alias` counts as a binding of its interface: a direct binding of an aliased type without `credo.Override()` panics, as `Alias` over a bound type does, and with the option it replaces the alias. `Alias` takes `credo.Override()` as its one option, to point an interface at another type, and panics on any other.

**`Finalize` drops what an override leaves unreached.** The type an overridden alias pointed at stays registered until `Finalize`, which drops it before validating the graph when no other alias, collection entry or constructor parameter — of a binding, a worker or a constructor handed to `app.Manage` — reaches it. A dropped type is never built or started, and `Resolve` of it reports the override that dropped it. A test that overrides an interface with a fake therefore does not build or start the real implementation behind it.

Rejected:

- **Writing the type arguments and keeping `Provide[T]`** — the compiler cannot check a type argument against a constructor typed `any`, so the type argument repeats what the constructor says and can only disagree with it.
- **Both forms side by side** — two spellings of one registration, and the old one keeps the unchecked repetition alive.
- **Dropping the alias's target at the `Override` call** — the registrations that follow the override may still need the target, which only the whole graph shows at `Finalize`.
- **Keeping the target built while the interface resolves the override** — the App would build and start an implementation nothing uses, which is what a test override is written to avoid.

### Ordered Interface Collections

Some application components need an ordered set of implementations rather than one default implementation: notification senders, hooks, subscribers, policy evaluators, or plugin chains. `BindMany[I, T]()` adds a registered concrete type to the ordered collection of `I`, which `ResolveAll[I]()` and a constructor parameter of type `[]I` receive in binding order — empty when nothing is bound. It is collection wiring, independent of `Alias`: neither implies the other, and `Resolve[I]` is unaffected. Named or keyed bindings are deliberately not introduced for this ([container spec](../specs/container.md#ordered-interface-collections)).

### The Public Surface

A container that also holds framework infrastructure must defend that infrastructure against the application that owns the container — protection against overrides, adoption against registration races, preflights against a closing phase — and the framework ends up resolving its own seams from it per request, the service-locator pattern this ADR rejects for applications. Each such defence is a method that exists for a framework integration, not for the application.

**Decision.** Framework infrastructure lives in kernel-owned registries — the component registry ([ADR-024](024-lifecycle-components.md)) and the readiness aggregate ([ADR-016](016-health-checks.md)) — and the container holds only the application's bindings. Its public surface is seven methods — `Provide`, `ProvideValue`, `Alias`, `BindMany`, `Has`, `Resolve` and `ResolveAll` — and the phase verb `Finalize`; the [container spec](../specs/container.md#the-surface) gives the signatures.

Registration calls return nothing and panic on misuse ([ADR-022](022-bootstrap-and-di-ownership.md#sequential-bootstrap-and-three-error-phases)), so one form suffices and there are no `Must*` registration twins. `Has` is the one non-resolving presence probe: it never runs a constructor, reserves nothing and reports a snapshot of the registrations made so far, because "is the optional module present?" is a composition-root question with no other answer. `MustResolve` and `MustResolveAll` sit beside the surface as resolve conveniences for the composition root, not registration twins: they panic with the error `Resolve` or `ResolveAll` would return.

Nothing framework-owned is bound in the container, so nothing needs protecting: no integration monitors or shuts down one value while DI resolves another, because the framework's registrations name bindings, not values. A component is its binding ([ADR-024](024-lifecycle-components.md)): an override of the binding is the value that is started, asked for readiness and shut down.

### No Automatic Bindings

Framework components are never bound in the container automatically; the one binding the framework makes is the `RawConfig` that `credo.New` loads or is given. A hidden binding collides with `credo.Override()` and with an application's second instance of the same type, and a binding is created where ownership is decided — by the application, at its registration call. Infrastructure that many constructors need is never a binding either: if the observability release gives constructors metrics or tracing, they arrive as `Infra` fields ([credo.Infra](#credoinfra-explicit-infrastructure-carrier)), not as types to resolve.

### Registration Options

Handing a value to DI and handing over its teardown are separate decisions: a resource whose teardown is `Close` needs a place in the dependency order, a pool two Apps in one process share must not be shut down twice, and an override that matches no binding must fail rather than add one.

**Decision.** A binding the App owns is a component when it has `Shutdown`; what is started and asked for readiness, and the tier, are planned from the binding's type ([ADR-024](024-lifecycle-components.md#registration-and-discovery)). The tier, the owner, a `Close` teardown and an override are registration options of the type `credo.RegistrationOption` on `Provide` and `ProvideValue` — `credo.Ingress()`, `credo.Borrowed()`, `credo.CloseOnShutdown()` and `credo.Override()` — rather than methods, so the surface does not grow per concern and misuse panics at the call that wrote it. `credo.Ingress()` also places a component handed to `app.Manage`, or a hook, in the ingress tier, and `credo.ComponentName(name)` names a managed component. The names say what the App does with the registration: `CloseOnShutdown` where `Closer` read as `io.Closer`, an interface the value would implement, and `ComponentName` where `Named` read as a DI key — the name labels a component in logs, `/ready` and a `LifecycleError`, and a second value of one type under another name is still a duplicate binding (Several Instances of One Type), which the duplicate-binding panic says when either registration carries the option. The [container spec](../specs/container.md#registration-options) states what each option accepts and when it panics; [ADR-024](024-lifecycle-components.md#ownership-and-registration-options) gives the ownership reasons — why `Borrowed` exists beside resource identity, and why `Close` is never discovered without the option — and the reasons for each name.

`credo.ComponentName(name)` is accepted on `Provide` and `ProvideValue` as well, naming the binding's component, and `credo.OptionalReadiness()` reports a component in `/ready` without letting it gate readiness ([ADR-024](024-lifecycle-components.md#ownership-and-registration-options), [ADR-016](016-health-checks.md)). Both shape what is planned before the value exists, so both panic on a registration that plans nothing for them.

`credo.Override()` is strict rather than an upsert, so an override that no longer matches the wiring fails instead of adding a binding nothing resolves while the test runs against the real dependency, and an accidental double registration still panics ([container spec](../specs/container.md#registration-options)).

A teardown that is not a `Close` — a client's `Disconnect(ctx)`, a `Drain` that returns at once and must be waited for — belongs to a wrapper type that embeds the client and implements `Shutdown`, registered as the binding; its constructor's parameters give the teardown's own dependencies their edges, and `sqldb.DB` wraps Bun this way. No API is added for it: [ADR-024](024-lifecycle-components.md#rejected-alternatives) rejects a teardown-function option and [defers](024-lifecycle-components.md#consequences) a constructor that returns its own cleanup function.

### Several Instances of One Type

The container keys bindings by type, so several instances of one type are several wrapper types — `type PrimaryDB struct{ *sqldb.DB }` and `type AnalyticsDB struct{ *sqldb.DB }` — each bound once. The type is the qualifier: the constructor's signature names the instance (`NewReportRepo(db AnalyticsDB)`), the compiler and `Finalize` check the choice, and the graph sees the edge, so each instance starts and stops in order. Embedding carries every capability of the handle. The rule composes with generics: `NewOutboxRepo[PrimaryDB]` and `NewOutboxRepo[AnalyticsDB]` are two bindings with no name between them. Google Wire answers the same question the same way, and the [httpclient spec](../specs/httpclient.md) applies it to several `*http.Client`s. A wrapper and the handle it embeds share one resource identity, so the wrapper, its interface view and its raw handle share one teardown ([container spec](../specs/container.md#resource-identity-and-teardown), [ADR-024](024-lifecycle-components.md#resource-identity-one-resource-one-teardown)); the [store spec](../specs/store.md#naming-and-several-databases) shows the wiring for databases.

Two cases sit outside the rule. Instances whose number comes from configuration — shards, tenants — are one binding of a collection type that owns and closes them all, since a type per instance cannot be written. A read replica of one database is routing, not a second instance.

Rejected:

- **Named bindings** (`ProvideNamed`, name tags on parameter objects) — the signature does not say which instance, the name travels in a tag or a parameter object read by reflection, and a typo surfaces at `Finalize` at best: the cost for which struct-tag injection is rejected below.
- **A qualifier type in the container** (`credo.Named[*sqldb.DB, Analytics]`) — typed, but it puts the container's plumbing in every business signature and unwraps a field at each use: a wrapper with a worse name.
- **Scoped child containers** — the signature stays `*sqldb.DB`, and the instance a repository receives depends on where it was registered, which hides what the wrapper shows, for a large feature beside one graph.
- **Wiring the instances outside the container** by closure — the graph loses the edges, and an instance can close before its consumers stop.

### DI Stays the Spine

Rejected: a kernel that is complete without DI, with DI as an opt-in layer, and, as its radical form, Wire-style code generation. DI is part of Credo's identity: [ADR-001](001-framework-identity-and-goals.md) names structured dependency injection as a reflection of the enterprise target and has configuration cross module boundaries as typed structs via DI. With framework infrastructure in kernel-owned registries (The Public Surface), the framework resolves none of its own infrastructure from the container, although its start walk, drain order, resource identity and tier validation run on the DI graph; a minimal App is `credo.New()`, routes and `Run()` without a binding, so an opt-in layer would change only where DI's public API lives and how the documentation positions it. Code generation adds a tool step to every developer's loop.

The real cost of reflection-based registration is not startup time, which is paid once: a wrong constructor surfaces when `Provide` runs and a missing dependency at `Finalize`, not in the compiler. A hand-wired `main` is checked by the compiler more strictly, at the price of churn at every call site when a constructor's signature changes, and Go has no variadic type parameters, so a reflection-free `Provide` over constructors of any arity cannot be written: the choice was between arity families, code generation and reflection, and Credo keeps reflection. That cost is answered by better reporting: `Provide` checks the constructor's signature at the call and panics there ([ADR-022](022-bootstrap-and-di-ownership.md)), `Finalize` reports a missing dependency with its whole path — `OrderService → PaymentClient → *http.Client (not registered)` — and, optionally after v1, a `go/analysis` checker reports signature mistakes before the program runs.

### DI Stays in the Core

The container is not a separate public package; it lives in `internal/di` behind the App's methods, for these reasons:

- `app.Resolve[T]` exists because the API is a method of the App. A package would offer either the free-function form that Go 1.27's generic methods removed, or a container value that travels, which is the service-locator door.
- Start order, drain order, resource identity and tier validation all run on the DI graph ([ADR-024](024-lifecycle-components.md)).
- A public package would have to export the hook that produces framework parameters such as `credo.Infra`.
- DI is already optional: a minimal App binds nothing, and a CLI or batch job builds an App, resolves and shuts it down with no listener.
- The root package links only its allowlist of modules, and the placement rule puts a subsystem the App holds one of and reaches through `app.` in the root ([ADR-002](002-code-acquisition-strategy.md#what-lives-in-the-root-package)).

> **Pending v0.25.0 (W9):** not implemented yet; the root package's documentation is not yet grouped by subject.

The cost accepted is the size of the root package, answered by grouping its package documentation by subject — DI, components, workers; the symbol index of `go doc` and pkg.go.dev stays alphabetical.

### Finalize Phase

> **Current until v0.25.0 (W6):** this paragraph states the current rule — `Resolve` before `Finalize` panics; with W6 the next section replaces it.

Constructors run only against a validated graph. `app.Finalize()` freezes the container and validates the dependency graph; `Resolve` is admitted only after it, `Has` is the registration phase's one read, and a DI registration after it panics. Construction is terminal: one value, error or panic per singleton, shared by every waiter and never retried. [ADR-022](022-bootstrap-and-di-ownership.md#resolution-after-finalize-overrides-before-it) records why and the bootstrap phases around it; the [container spec](../specs/container.md#finalize-and-container-lifecycle) states what `Finalize` reports and when it runs implicitly.

> **Pending v0.25.0 (W6):** not implemented yet; until W6 ships, `Resolve` before `Finalize` panics, as the previous paragraph states.

Constructors run only against a validated graph, and the first resolving call validates it: the first `Resolve`, `ResolveAll`, `MustResolve` or `MustResolveAll` finalizes the container when it is not finalized yet, then resolves. `app.Finalize()` stays public as an optional early check, and the serve entry points, `App.Start` and the first `ServeHTTP` call still finalize implicitly. `Has` and the configuration reads (`GetConfig`, `MustGetConfig`, `ConfigExists`) never finalize, because registration uses them to decide what to register. A `Finalize` failure is the triggering call's error and every later `Resolve` returns it; a registration after the freeze panics naming what finalized the container and where. Construction is terminal: one value, error or panic per singleton, shared by every waiter and never retried. [ADR-022](022-bootstrap-and-di-ownership.md#the-registration-window-ends-at-the-first-resolution) records why the window ends at the first resolution; the [container spec](../specs/container.md#finalize-and-container-lifecycle) states the trigger, its record and the message.

### Shutdown

The container keeps the static dependency graph that orders teardown — constructor parameters, aliases and collection edges, not dependencies hidden inside pre-built values — along which the component registry tears components down consumers first, once per resource, with bounded, never-retried attempts ([ADR-024](024-lifecycle-components.md#shutdown), [ADR-022](022-bootstrap-and-di-ownership.md#dependency-ordered-bounded-teardown)); the [container spec](../specs/container.md#shutdown) states the container's rules and the [lifecycle spec](../specs/lifecycle.md#shutdown-in-tiers) the drain sequence.

### credo.Infra: Explicit Infrastructure Carrier

`credo.Infra` is a fixed struct defined by the framework; applications cannot add fields. It carries the service-scoped Logger. Metrics and tracing are deferred to the observability release, which decides whether and how they reach `Infra` ([ADR-014](014-observability.md)); the keyed-literal guard (`_ struct{}`) is deliberate, so that adding fields does not break existing `credo.Infra{Logger: ...}` literals.

When a constructor has a parameter of type `credo.Infra`, at any position, the container produces it instead of resolving a registration: a framework provider builds it from the App's logger (`credo.WithLogger`, else the framework default stderr logger), scoped with a `component=<name>` attribute naming the binding the constructor builds ([One Key Names an Application Part](#one-key-names-an-application-part)). A constructor without such a parameter is pure constructor injection; the developer chooses per service, and `Infra` goes first by convention, like `context.Context`. The [container spec](../specs/container.md#credoinfra-----explicit-infrastructure-carrier) gives the struct, the detection rule and examples.

### The Context Carrier for Framework Modules

Infrastructure travels through the visible `credo.Infra` carrier, and a framework module — a value built outside the App and started by it, such as the WebSocket server or `sqldb`'s database — reaches it through the context of the calls the App makes on it. Every call the App makes on a component — `Start`, `Ready`, `Shutdown`, the `Close` of a `credo.CloseOnShutdown()` binding, and the `OnStart` and `OnStop` hooks — receives a context carrying the component's scoped `Infra`, read with `credo.InfraFrom(ctx)`; outside such a call it returns the zero `Infra`, whose logger falls back to the framework default. Only framework infrastructure travels in the context, never application services: a business dependency stays a constructor parameter, where the signature shows it.

**The rule.** A framework module whose value is built outside the App and started by it takes an optional logger in its own configuration and otherwise uses the App's. Precedence: the logger given to the module, unscoped; then `InfraFrom(ctx).Logger`; then the framework default. Every record a module writes carries `module=<name>`, added with `Logger.With`, whichever logger it goes to. The App's own subsystems — the `Use*` features and the worker registry — already log to the App's logger and are outside the rule; `httpclient` stays free of the root package and takes a `*slog.Logger`. Request code logs through `c.Logger()`, and a constructor takes `credo.Infra`; `InfraFrom`'s godoc names both, since a call in a handler compiles and logs to the framework default.

Rejected:

- **A required logger option per module** — repeated for every module, and an application that forgets it loses the module's records.
- **`credo.Infra` as the first parameter of every module constructor** — the same repetition at every construction, and a module built for a test needs an `Infra` it does not use.
- **A logger registration option on the DI methods** — a logging concern on the container surface, and a module handed to `app.Manage` is no binding.
- **A setter capability the App calls after the build** — hidden field population, racing a value already in use.
- **A warnings capability the start walk logs** — it solves one log line, not the next module's logging.

### One Key Names an Application Part

One key names an application part in the framework's records: `component=<name>`, the key and value the lifecycle's own records use about a component. The scoped `Infra` a component's call receives writes it with the component's unit name — the name its readiness entry, a `LifecycleError` and the lifecycle's records show, which `credo.ComponentName` sets; the logger a constructor receives writes it with the name of the binding it builds — its type as the lifecycle prints it (`*users.Service`), or its `credo.ComponentName` name; and `app.NewInfra(name)` writes `component=name`. `module=<name>` names the framework package that wrote a record, the `http.Server.ErrorLog` bridge's `module=net/http` included. Beside them stand `request_id` and `connection_id`. A stable event identity for framework records is the observability release's: it adds a key and renames none.

Rejected:

- **`service` as the one key** — OpenTelemetry's `service.name` names the application, not a part of it, and the lifecycle's records already use `component`.
- **A key per role**, `service` for the code that wrote a record and `component` for the part a framework record is about — the carrier's logger serves a module's code and the application's alike, so the two keys would name one thing.
- **Keeping the constructor's `service` until a later revision** — its value drops the package and the pointer, a component logs under two keys meanwhile, and the keys break twice.

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
| Container as separate public package | The API is a method of the App, the lifecycle runs on the DI graph, a package would export the framework-parameter hook, DI is already optional, and the root links only its allowlist (DI Stays in the Core) |
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

> **Current until v0.25.0 (W6):** the item below on an explicit `Finalize` holds until W6; with it, the first `Resolve` finalizes and `Finalize` is an optional early check.

**Negative:**

- A constructor that needs infrastructure takes an `Infra` parameter (minimal boilerplate)
- Infra is not extensible — adding a new infra type requires a framework change
- A framework-provider hook in `internal/di` produces `credo.Infra` — one map lookup per constructor parameter
- `BindMany` adds ordering and empty-collection semantics that must be documented clearly
- Container is in `internal/di` — cannot be used as a standalone DI library
- The composition root needs an explicit, error-checked `Finalize` between registration and the first `Resolve`
- Several instances of one type take a wrapper type each — more types in exchange for signatures that say which instance they receive
