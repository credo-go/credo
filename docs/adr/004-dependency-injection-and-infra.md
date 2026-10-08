# ADR-004: Dependency Injection & credo.Infra

**Status:** Accepted **Date:** 2026-03-01 **Amended:** 2026-09-05 by [ADR-022](022-bootstrap-and-di-ownership.md) (phase-gated resolution, dependency-ordered teardown); v0.24.0 (the seven-method surface, registration options and component teardown, [ADR-024](024-lifecycle-components.md)) **Depends on:** ADR-001, ADR-003

## Context

Dependency injection is a fundamental need for enterprise applications (ADR-001, ADR-003). Credo's DI mechanism must address two distinct needs:

1. **Business dependencies** (DB, repo, service): Each service requires different combinations. Must be explicit, mockable, and type-safe.

2. **Infrastructure dependencies** (Logger, Metrics, Tracer): Nearly every service needs them. Passing 3-4 infra parameters to every constructor is verbose, but implicit injection (auto-populate) in Go conflicts with Go's philosophy.

### Why Not Implicit Base?

An embeddable `Base` struct auto-populated via reflection (similar to Spring's `@Autowired`) may seem attractive at first glance, but it has serious problems in Go:

- **Implicit**: Not visible in the constructor signature — it's unclear what is being injected
- **Reflection**: Requires scanning struct fields via reflect
- **God object tendency**: Logger + Config + Metrics + Tracer all in one struct
- **Testing difficulty**: Requires special mechanisms for mocking
- **Conflicts with Go philosophy**: Not idiomatic in Go

## Decision

### Container: Generics-Based DI

The DI container implementation lives in the `internal/di` package. Type-safe generic functions are exposed through the root package:

```go
app.Provide[T](constructor)  // Register
app.Resolve[T]()               // Resolve
```

The container is a Credo-specific component — it is not intended for standalone use as an independent DI library. For this reason, `internal/di` is preferred over a public `container/` package.

- **Lifecycle**: Singleton
- Reflection is used at registration time (constructor inspection) and once per singleton during first construction (`reflect.Value.Call`). Subsequent resolves are pure cache lookups — zero reflection.

### Interface Alias

Interface alias via `Alias[I, T]()` creates an alias so `Resolve[I]` returns T's singleton. Contract: I is an interface, T implements I, and T is already registered via `Provide`.

```go
app.Provide[*UserRepo](NewUserRepo)
app.Alias[UserRepository, *UserRepo]()  // Resolve[UserRepository] returns *UserRepo
```

### Ordered Interface Collections

Some application components need an ordered set of implementations rather than one default implementation: notification senders, hooks, subscribers, policy evaluators, or plugin chains.

Credo supports this via `BindMany[I, T]()` and `ResolveAll[I]()`:

```go
app.Provide[*EmailSender](NewEmailSender)
app.Provide[*InAppSender](NewInAppSender)

app.BindMany[Sender, *EmailSender]()
app.BindMany[Sender, *InAppSender]()

senders := app.MustResolveAll[Sender]()
```

The same ordered collection is also injectable via constructor parameters of type `[]I`:

```go
func NewSenderRegistry(senders []Sender) *SenderRegistry {
    return NewSenderRegistryWithSenders(senders...)
}
```

Rules:

1. `I` must be an interface type
2. `T` must already be registered via `Provide` or `ProvideValue`
3. `T` must be a concrete type and implement `I`
4. Binding order is preserved
5. `ResolveAll[I]` and `[]I` injection return an empty slice when no bindings exist
6. `Alias` and `BindMany` are independent; one does not imply the other

### The Public Surface

**Problem.** Before v0.24.0 the application's container doubled as the framework's internal plugin bus. `store`, `worker` and health coordinated through it: protected values, readiness seams installed with `Replace`, and a store seam resolved on every `/ready` request — the service-locator pattern this ADR rejects for applications. The public surface had grown to about twenty methods, and several existed only for those two in-tree integrations: `ProvideProtectedValue` and `ProtectBinding`, which made a binding refuse `Replace` so that an integration never kept monitoring or shutting down one value while DI resolved another; `AdoptValue`, a registration-time read that validated a pre-built value and protected its binding; `CanProvideValue`, a preflight for a `ProvideValue` that could still fail; and a `Replace` that handed the superseded instance, and its shutdown, back to the caller. A container that holds framework infrastructure must defend it against the application that owns the container: protection against `Replace`, adoption against races, preflights against a closing phase. The store side moved to the kernel's store registry ([ADR-015](015-data-access.md#registration)) and the worker side to one component per worker ([ADR-023](023-worker-system.md)), which left those methods without a framework caller.

**Decision.** Framework infrastructure lives in kernel-owned registries — the component registry ([ADR-024](024-lifecycle-components.md)), the readiness aggregate and the store registry ([ADR-015](015-data-access.md)) — and the container's public surface is seven methods and the phase verb:

| Method | Role |
| --- | --- |
| `app.Provide[T](constructor, options...)` | Register a constructor; registration options below |
| `app.ProvideValue[T](value, options...)` | Register a pre-built value; registration options below |
| `app.Alias[I, T]()` | `Resolve[I]` returns T's singleton |
| `app.BindMany[I, T]()` | Add T to the ordered collection of I |
| `app.Has[T]() bool` | Non-resolving presence probe |
| `app.Resolve[T]() (T, error)` | Resolve after `Finalize` |
| `app.ResolveAll[I]() ([]I, error)` | Resolve the ordered collection after `Finalize` |
| `app.Finalize() error` | Freeze and validate the graph ([ADR-022](022-bootstrap-and-di-ownership.md)) |

Registration calls return nothing and panic on misuse ([ADR-022](022-bootstrap-and-di-ownership.md)), so one form suffices and there are no `Must*` registration twins. `Has` is the one non-resolving presence probe: it never runs a constructor, reserves nothing and reports a snapshot of the registrations made so far, because "is the optional module present?" is a composition-root question with no other answer, although its in-tree callers left with the store and worker registries. `MustResolve` and `MustResolveAll` remain beside the table as resolve conveniences for the composition root, not registration twins: they panic with the error `Resolve` or `ResolveAll` would return. After `Finalize`, a `Provide`, `ProvideValue`, `Alias` or `BindMany` call panics and `Resolve` becomes available (Finalize Phase).

Nothing framework-owned is bound in the container, so nothing needs protecting. The guarantee protection gave — that no integration keeps monitoring or shutting down one value while DI resolves another — holds by construction, because the framework's registrations name bindings, not values. A component is its binding ([ADR-024](024-lifecycle-components.md)): an override of the binding is the value that is started, asked for readiness and shut down. A store registration names the store's binding by type, and the start phase resolves it once, after `Finalize` and therefore after every override, so the value that is pinged, reported by `/ready` and shut down is the value DI hands the store's consumers, in tests too ([ADR-015](015-data-access.md#registration)).

**Removes.** `ProvideProtectedValue`, `ProtectBinding`, `AdoptValue`, `CanProvideValue`, `Replace` and `MustReplace` with the Warn log for a superseded component, and the registration twins `MustProvide`, `MustProvideValue`, `MustAlias` and `MustBindMany`. `Replace`'s role passes to `credo.Override()` before `Finalize` and to `testutil.WithOverride` in tests (Registration Options); an override's replaced value never becomes the App's, so there is no superseded instance to hand back. Protected bindings and adoption have no successor, because nothing framework-owned is bound; `CanProvideValue`'s question is answered by `Has`, or by `ProvideValue` itself, which panics on a duplicate.

### Registration Options

**Problem.** Handing a value to DI and handing over its teardown are separate decisions, and the container could express only one combination: a bound `Shutdowner` was shut down by DI. A resource whose teardown is a `Close` method had no place in the dependency order, so applications closed it in an `OnShutdown` hook; a pool shared by two Apps in one process could not be bound without being shut down twice; and the only override was `Replace`, which silently added a binding when none existed.

**Decision.** A DI singleton the App owns is a component when it has `Shutdown` — its binding's type shows the method, or its built value has it — because ownership, not the view its consumers take, decides the teardown. `Start`, `Ready` and the tier are planned from the binding's type at registration, before any constructor runs: a `Start` or `Ready` that only the value has is neither called nor asked, and a type that has only `Start` is not a component. To have the App start or ask a value, provide the concrete type and `Alias` the interface the application uses. [ADR-024](024-lifecycle-components.md) states the rules in full.

`Provide` and `ProvideValue` take registration options of the type `credo.RegistrationOption`, each checked at the call: `Provide` accepts `credo.Ingress()`, `credo.Closer()` and `credo.Override()`, `ProvideValue` those and `credo.Borrowed()`. An option the call does not accept, a zero option or a repeated one panics with the call that misused it:

- **`credo.Ingress()`** places a component in the ingress tier, where work enters the process. It panics on a binding that is not a component at registration — whose type shows no `Shutdown` and that carries no `credo.Closer()` — since the tier is planned before the value exists.
- **`credo.Borrowed()`**, on `ProvideValue` only, keeps the binding and the value's readiness contribution and leaves starting and shutting it down to the caller — for a pool that two Apps in one process share, or a fixture a test suite reuses across the Apps it builds. Resource identity cannot say that, because it is unique only inside one App. The option is accepted on a binding whose type does not show `Shutdown`: that is how a caller keeps the teardown of a value that has the method behind an interface.
- **`credo.Closer()`** makes a binding whose type has a `Close` method — `Close() error`, `Close()` or `Close(ctx) error`, the last receiving the drain deadline — a component the App closes after its consumers, abandoned at the deadline like any shutdown. It panics on a type with none of the three, on a type that is already a component, and beside `credo.Borrowed()`. It decides its binding's teardown: a built value that also has `Shutdown` is closed once, with `Close`. `Close` is never discovered on its own: the method is too common to mean that the App owns the value, and its common forms take no deadline.
- **`credo.Override()`** replaces an earlier binding of the same type before `Finalize` and panics when there is none, so an override that no longer matches the wiring fails instead of adding a binding nothing resolves while the test runs against the real dependency. Without the option a duplicate binding still panics, so an accidental double registration is caught. The replaced value never becomes the App's: whoever built it releases it. `testutil.WithOverride[T]` builds on it and is equally strict; adding a binding is `testutil.WithWiring`'s job.

`app.Manage(v, opts...)` hands the App a component that is not a binding — a value, or a constructor over DI parameters that the start walk builds and that never becomes a binding — and takes `credo.Ingress()` and `credo.Named("…")`, the name the readiness aggregate and the lifecycle report use, defaulting to the type name; a duplicate or empty name panics. `OnStart` and `OnStop` take `credo.Ingress()` only. [ADR-024](024-lifecycle-components.md) specifies them.

Within one App the component registry keys components by resource identity — the token a value's `ResourceIdentity()` returns (the `credo.ResourceIdentifier` capability), otherwise the value itself when it is comparable (a pointer, or a struct over one), otherwise its binding alone — so one resource held by several bindings has one teardown, run once, after the consumers of every holder. The holders that have a teardown agree on its kind, `Shutdown` or `Close` through `credo.Closer()`, and the one registered first runs it; a borrowed resource is torn down through none of its holders. The [container spec](../specs/container.md) states the rules as the container applies them.

**A teardown that is not a `Close`** — a client's `Disconnect(ctx)`, a `Drain` that returns at once and must be waited for — belongs to a type that embeds the client and implements `Shutdown`, registered as the binding itself. Consumers depend on that type, so its edges are right, and its constructor's parameters give the teardown's own dependencies their edges; `sqldb.DB` wraps Bun this way. No API is added for it. Rejected: a teardown-function option, `credo.ShutdownWith(func(ctx, T) error)` — Go cannot tie the option's type parameter to `Provide`'s at compile time, so a mismatch would surface only as a registration panic, and the dependencies the function uses would have no edges, so the graph could close them first; its non-generic form, `func(ctx) error`, would fit `ProvideValue` only. Deferred: a constructor returning `(T, func(context.Context) error, error)`, the cleanup function of Google Wire's providers — compile-time typed and written beside the construction — until a type proves costly to wrap, such as a third-party API whose raw type many consumers need. Adding it later breaks nothing, because a three-value constructor is rejected; removing it after v1 would.

**Removes.** `testutil.WithOverride`'s silent addition of a binding that was never wired.

### Several Instances of One Type

The container keys bindings by type, so several instances of one type are several wrapper types — `type PrimaryDB struct{ *sqldb.DB }` and `type AnalyticsDB struct{ *sqldb.DB }` — each bound once and registered as a store by its type ([ADR-015](015-data-access.md#registration)). The type is the qualifier: the constructor's signature names the instance (`NewReportRepo(db AnalyticsDB)`), the compiler and `Finalize` check the choice, and the graph sees the edge, so each instance starts and stops in order. Embedding carries every capability of the handle — `Shutdown`, `Ping`, `Health`, `ResourceIdentity` — and each `*sqldb.DB` owns its transaction scope, so the transactions of two databases never meet. The rule composes with generics: `NewOutboxRepo[PrimaryDB]` and `NewOutboxRepo[AnalyticsDB]` are two bindings with no name between them. Google Wire answers the same question the same way, and the [httpclient spec](../specs/httpclient.md) already applies it to several `*http.Client`s. Under the component registry a wrapper and the handle it embeds are one resource through resource identity, so the wrapper, its interface view and its raw handle share one teardown.

Two cases sit outside the rule. Instances whose number comes from configuration — shards, tenants — are one binding of a collection type that owns and closes them all, since a type per instance cannot be written. A read replica of one database is routing, not a second instance.

Rejected:

- **Named bindings** (`ProvideNamed`, name tags on parameter objects) — the signature no longer says which instance, the name travels in a tag or a parameter object read by reflection, and a typo surfaces at `Finalize` at best: the cost for which struct-tag injection is rejected below.
- **A qualifier type in the container** (`credo.Named[*sqldb.DB, Analytics]`) — typed, but it puts the container's plumbing in every business signature and unwraps a field at each use: a wrapper with a worse name.
- **Scoped child containers** — the signature stays `*sqldb.DB`, and the instance a repository receives depends on where it was registered, which hides what the wrapper shows, for a large feature beside one graph.
- **Wiring the instances outside the container** by closure — the graph loses the edges, and an instance can close before its consumers stop.

### DI Stays the Spine

Rejected: a kernel that is complete without DI, with DI as an opt-in layer, and, as its radical form, Wire-style code generation. DI is part of Credo's identity: [ADR-001](001-framework-identity-and-goals.md) names structured dependency injection as a reflection of the enterprise target and has configuration cross module boundaries as typed structs via DI. With framework infrastructure in kernel-owned registries (The Public Surface), the kernel does not depend on DI internally, and a minimal App is `credo.New()`, routes and `Run()` without a binding, so an opt-in layer would change only where DI's API lives and how the documentation positions it. Code generation adds a tool step to every developer's loop.

The real cost of reflection-based registration is not startup time, which is paid once: a wrong constructor surfaces when `Provide` runs and a missing dependency at `Finalize`, not in the compiler. A hand-wired `main` is checked by the compiler more strictly, at the price of churn at every call site when a constructor's signature changes, and Go has no variadic type parameters, so a reflection-free `Provide` over constructors of any arity cannot be written: the choice was between arity families, code generation and reflection, and Credo keeps reflection. That cost is answered by better reporting: `Provide` checks the constructor's signature at the call and panics there ([ADR-022](022-bootstrap-and-di-ownership.md)), `Finalize` reports a missing dependency with its whole path — `OrderService → PaymentClient → *http.Client (not registered)` — and, optionally after v1, a `go/analysis` checker reports signature mistakes before the program runs.

### Finalize Phase

Constructors run only after Finalize, so the registration phase has no general `Resolve`; `Has` is its one read, and it never constructs.

`app.Finalize()` freezes the container and validates the dependency graph. After Finalize, a `Provide`, `ProvideValue`, `Alias` or `BindMany` call panics at the call site, as it does after shutdown began. `Resolve` is admitted only after Finalize and panics before it: constructors never run during registration, so the composition root finishes every DI write (store/worker registration included), calls Finalize and handles its error, and only then resolves controllers and binds routes. `Run()`, `RunContext()`, `ServeContext()` and the first direct `ServeHTTP` call Finalize implicitly as an idempotent safeguard, which cannot precede a Resolve the composition root has already executed. Finalize is DI-only; HTTP registration stays open until the App prepares to serve (ADR-006). Credo's recommended usage keeps `Resolve` in bootstrap/composition-root code; runtime `Resolve` remains available but is not the preferred application pattern, and `OnStop` hooks capture their dependencies instead of resolving; a component's `Start` and an `OnStart` hook run after Finalize and may resolve, although a component that needs a dependency takes it as a constructor parameter, as a DI-provided worker does. After a failed Finalize, `Resolve` returns the error. A `Manage` call after Finalize panics like the DI registrations, and so does a worker registration, which adds a component. `Finalize` returns what only the whole graph reveals — missing dependencies, each with its whole path, cycles, and an internal component that depends on an ingress one, directly or through bindings that are not components, with the path and both remedies — joined in registration order. The phases and the documented bootstrap order are [ADR-022](022-bootstrap-and-di-ownership.md)'s.

Construction completes once per singleton with a terminal result — value, error or panic — shared by every waiter; nothing is retried. A constructor panic is returned as `*credo.DIPanicError` (type, phase, original value, stack) and `MustResolve` panics with that error as its payload. Once the drain reaches the internal tier the container is closing: new resolution, cached results included, returns an error wrapping `credo.ErrDIClosed`, while instances created by already-admitted builds remain owned and cleaned up by the container.

### Shutdown

The container keeps the dependency graph that orders teardown; the teardown itself belongs to the component registry ([ADR-024](024-lifecycle-components.md)), keyed by resource identity, so a resource several bindings hold is shut down once, after the consumers of every holder. A DI singleton the App owns that has `Shutdown(ctx) error` — the `credo.Component` interface, which replaced `Shutdowner` with the same method set — or that is bound with `credo.Closer()` stops in its tier's turn: the ingress tier beside the HTTP drain, the internal tier after it, consumers before the singletons they were constructed from, through a Kahn ready queue over the static graph (constructor parameters, aliases, collection edges; value bindings are vertices, dependencies hidden inside pre-built values are not) with reverse registration order as the tie-break. A lazily built singleton is shut down only if it was built. Each teardown gets at most one attempt bounded by the shared drain deadline through a helper goroutine; one that misses the deadline is abandoned, keeps its dependencies open and is reported, never retried or closed around. Only construction completing after the deadline receives one separate fixed five-second best-effort attempt, logged and not reported. Failure or incompleteness is reported as `*credo.LifecycleError` ([ADR-024](024-lifecycle-components.md#one-error)), an immutable snapshot that unwraps its causes; shutdown panics are isolated as `DIPanicError`. The [container spec](../specs/container.md#shutdown), the [lifecycle spec](../specs/lifecycle.md) and the [bootstrap contract](../specs/bootstrap-and-di-lifecycle.md) carry the full rules.

### credo.Infra: Explicit Infrastructure Carrier

`credo.Infra` is a fixed struct defined by the framework. It carries framework-managed infrastructure. Today that is the service-scoped Logger; the observability release (Phase 3.5, aligned with the v1 / Go 1.27 window) extends the same carrier with metrics and tracing, designed against real OpenTelemetry and Prometheus adapters rather than speculative placeholders:

```go
// Defined by the framework, not extensible by the user.
type Infra struct {
    _ struct{} // forces keyed literals so new fields (metrics, tracing) land compatibly

    Logger *slog.Logger
}
```

The `_ struct{}` keyed-literal guard is deliberate: it lets Phase 3.5 add the metrics and tracing fields without breaking existing `credo.Infra{Logger: ...}` construction sites.

When the container sees the `credo.Infra` type as a constructor parameter, it runs a special code path:

1. Resolves the Logger from the container (or uses the framework default)
2. Scopes the Logger with a `service=<name>` attribute
3. Places the produced `Infra` value into the parameter

```go
// Model 1: Infra as first parameter in the constructor (convention)
func NewUserService(infra credo.Infra, repo UserRepo) *UserService {
    infra.Logger.Info("user service initialized")
    return &UserService{infra: infra, repo: repo}
}
```

### Container Detection Logic

The container automatically determines which injection model is being used by inspecting the constructor signature:

1. If any parameter is of type `credo.Infra` -> **Model 1**: the container produces that parameter (a scoped Infra) and resolves the rest normally. Placing it first is the recommended convention (above), but detection is position-independent.
2. Otherwise -> **Pure constructor injection**: All parameters resolved normally (no Infra magic)

The developer chooses on a per-service basis.

### Infra Design Decisions

| Decision | Rationale |
| --- | --- |
| **Fixed struct, not extensible** | Fields are known, no field-scan/tag needed |
| **Always available** | Like `context.Context` — no need to register, container knows how to produce it |
| **Default fallback** | If no Logger is registered, the framework default logger is used — no panic |
| **Scoped Logger** | Each service gets a logger scoped with its own name |
| **First parameter convention** | Like Go's `context.Context` convention, Infra is placed first by convention; the container still detects it at any parameter position |
| **Reflection constrained to cold path** | Constructor inspection at registration + `reflect.Call` once per singleton first construction; subsequent resolves are cache lookups |
| **Config not included** | Config is a separate concern — distributed via DI as typed struct (ADR-005) |
| **Immutable** | Cannot be changed after production — snapshot semantics |

### Considered and Rejected

| Alternative | Reason for rejection |
| --- | --- |
| Implicit Base (auto-populate) | Conflicts with Go philosophy, reflection-based field population, implicit |
| Container as parameter (service locator) | Dependencies not visible in signature, unclear what to mock in tests |
| Struct tag injection (`credo:"inject"`) | Tag typos not caught at compile time, visual noise, field-scan reflection |
| Setter injection (`SetLogger`, `SetTracer`) | Object returned from constructor is half-initialized, lifecycle problem, implicit |
| Pure constructor params (each infra separate) | 6-7 parameters are verbose, Infra consolidates them into a single parameter |
| Container as separate public package | No standalone usage scenario, Credo-specific — internal is sufficient |
| RequestScoped lifecycle | Go's `context.Context` + middleware pattern provides sufficient request-scoped dependency management without DI container complexity |
| Model 3: Hybrid Embed (struct with embedded `credo.Infra` + resolved fields) | Reflective field population hides application boundaries. Model 1 with visible constructor parameters is clearer and sufficient |
| Closure factory (`fn func(*App) (T, error)` resolving its own dependencies) | Compiler-checked signature, but the dependencies resolved inside the closure are invisible to Finalize validation, cycle detection and dependency-ordered shutdown; typed constructors keep the graph static. Capturing `app.Resolve` inside a constructor is unsupported for the same reason |
| General early Resolve/Peek during registration; protect-on-read | Would run constructors against an unvalidated graph or freeze an invalid binding before validation. `Has` answers the composition root's presence question, and framework integrations read nothing from the container during registration |
| Protected bindings (`ProvideProtectedValue`, `ProtectBinding`), registration-time adoption (`AdoptValue`) and a `ProvideValue` preflight (`CanProvideValue`) — the surface before v0.24.0 | They existed to defend framework infrastructure bound in the application's container. With that infrastructure in kernel-owned registries nothing framework-owned is bound, and the guarantee protection gave holds by construction, because the framework's registrations name bindings, not values (The Public Surface) |
| Runtime replacement (`Replace`, `MustReplace`) returning the superseded instance — the surface before v0.24.0 | An override is a registration option checked before `Finalize`, which panics when it matches nothing; the replaced value never becomes the App's, so no ownership has to be handed back (Registration Options) |
| Reverse registration order for shutdown | Ignores the dependency graph: a service registered before its DB closed after it. Dependency order with reverse-registration tie-break keeps the old order where the graph does not decide |
| A kernel complete without DI, DI as an opt-in layer; Wire-style code generation | DI is part of Credo's identity (ADR-001); the kernel's independence from DI comes from kernel-owned registries, and reflection's real cost is answered by reporting (DI Stays the Spine) |
| Named bindings; a qualifier type in the container; scoped child containers; wiring several instances outside the container | A wrapper type per instance keeps the choice in the constructor's signature, where the compiler and `Finalize` check it and the graph sees the edge (Several Instances of One Type) |
| A teardown-function option (`credo.ShutdownWith`) | Its type parameter cannot be tied to `Provide`'s at compile time, and the function's own dependencies would have no edges; a wrapper type that implements `Shutdown` is the binding instead (Registration Options) |

## Consequences

**Positive:**

- Every dependency is visible in the constructor signature — explicit, reviewable
- `credo.Infra` consolidates infra boilerplate into a single parameter — not verbose
- Reflection is limited to constructor/infra metadata inspection; resolve hot path uses cached mappings
- Easy to mock in tests — provide your own Infra struct
- Scoped logger is automatic — each service logs with its own name
- Always available — no registration dependency
- Immutable — snapshot semantics, no race conditions
- `Alias[I, T]()` enables programming to interfaces without duplicate registrations
- `BindMany[I, T]()` / `ResolveAll[I]` support ordered plugin-style composition without manual registry bootstrapping
- `Finalize()` catches dependency graph errors at startup, not at first request, and is the only gate before any constructor runs
- Registration misuse panics at the line that caused it, and `Finalize`'s report names each missing dependency's whole path
- Shutdown follows observable dependencies; cancellation bounds waiting and the report says what did not complete and why
- A resource's teardown, its owner and its tier are declared where it is bound; a resource several bindings hold is torn down once
- The container holds only the application's bindings; seven methods and `Finalize` are its whole surface, and none of them exists for a framework integration
- An override is never a runtime replacement: `credo.Override()`, a registration option checked before `Finalize`, is the one way to override a binding, and an override that matches nothing panics

**Negative:**

- `credo.Infra` parameter must be added to every constructor (minimal boilerplate)
- Infra is not extensible — adding a new infra type requires a framework change
- Special code path in container for `credo.Infra` — but it's a simple type switch
- `BindMany` adds ordering and empty-collection semantics that must be documented clearly
- Container is in `internal/di` — cannot be used as a standalone DI library
- The composition root needs an explicit, error-checked `Finalize` between registration and the first `Resolve`
- A store takes its binding and a registration by type, and several instances of one type take a wrapper type each — more types in exchange for signatures that say which instance they receive
