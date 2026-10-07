# ADR-004: Dependency Injection & credo.Infra

**Status:** Accepted; v0.24.0 decisions accepted, pending implementation ([plan](../plans/components-and-sequential-bootstrap.md)) **Date:** 2026-03-01 **Amended:** 2026-09-05 by [ADR-022](022-bootstrap-and-di-ownership.md) (phase-gated resolution, adoption, replacement ownership, dependency-ordered teardown) **Depends on:** ADR-001, ADR-003

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

**Accepted, pending implementation (v0.24.0, W3).** When it ships, this section replaces "ProvideValue Preflight and Protected Bindings" and "Registration-Time Reads and Replacement Ownership" below, and the other mentions of the deleted methods — in the Finalize phase, the rejected alternatives and the consequences — go.

**Problem.** The application's container doubled as the framework's internal plugin bus. `store`, `worker` and health coordinated through it: protected values, readiness seams installed with `Replace`, and a store seam resolved on every `/ready` request — the service-locator pattern this ADR rejects for applications. The public surface grew to about twenty methods, and several existed only for two in-tree integrations — `ProvideProtectedValue`, `ProtectBinding`, `AdoptValue`, `CanProvideValue` and an ownership-transferring `Replace` — as the protected-binding section below says itself ("intended for framework/integration registration paths"). A container that holds framework infrastructure must defend it against the application that owns the container: protection against `Replace`, adoption against races, preflights against a closing phase.

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

Registration calls return nothing and panic on misuse ([ADR-022](022-bootstrap-and-di-ownership.md)), so one form suffices and the `Must*` registration twins go. `Has` stays as the one non-resolving presence probe: "is the optional module present?" is a composition-root question with no other answer, although its in-tree callers leave with the store and worker registries. `MustResolve` stays until the pre-v1 pruning judges it.

Nothing framework-owned is bound in the container, so nothing needs protecting. The guarantee protection gave — that no integration keeps monitoring or shutting down one value while DI resolves another — holds by construction, because the framework's registrations name bindings, not values. A component is its binding ([ADR-024](024-lifecycle-components.md), W4): an override of the binding is the value that is started, asked for readiness and shut down. A store registration names the store's binding by type, and the start phase resolves it once, after `Finalize` and therefore after every override, so the value that is pinged, reported by `/ready` and shut down is the value DI hands the store's consumers, in tests too ([ADR-015](015-data-access.md), W5).

**Removes.** `ProvideProtectedValue`, `ProtectBinding`, `AdoptValue`, `CanProvideValue`, `Replace` and `MustReplace` with the Warn log for a superseded `Shutdowner`, and the registration twins `MustProvide`, `MustProvideValue`, `MustAlias` and `MustBindMany`. `Replace`'s test role passes to `credo.Override()` and `testutil.WithOverride` (Registration Options); protected bindings have no successor, because nothing framework-owned is bound.

### Registration Options

**Accepted, pending implementation (v0.24.0, W4).** When it ships, the Shutdown section below keeps only the container's part, `Shutdowner` becomes `Component`, and the component lifecycle — tiers, start, stop order, the drain deadline — is [ADR-024](024-lifecycle-components.md)'s.

**Problem.** Handing a value to DI and handing over its teardown are separate decisions, and the container could express only one combination: a bound `Shutdowner` is shut down by DI. A resource whose teardown is a `Close` method had no place in the dependency order, so applications closed it in an `OnShutdown` hook; a pool shared by two Apps in one process could not be bound without being shut down twice; and the only override was `Replace`, which silently added a binding when none existed.

**Decision.** A DI singleton the App owns is a component when it has `Shutdown` — its binding's type shows the method, or its built value has it — because ownership, not the view its consumers take, decides the teardown. `Start`, `Ready` and the tier are planned from the binding's type at registration, before any constructor runs: a `Start` or `Ready` that only the value has is neither called nor asked, and a type that has only `Start` is not a component. To have the App start or ask a value, provide the concrete type and `Alias` the interface the application uses. [ADR-024](024-lifecycle-components.md) states the rules in full.

`Provide` and `ProvideValue` take registration options, each checked at the call; misuse panics with the call that misused it:

- **`credo.Ingress()`** places a component in the ingress tier, where work enters the process. It panics on a binding that is not a component at registration — whose type shows no `Shutdown` and that carries no `credo.Closer()` — since the tier is planned before the value exists.
- **`credo.Borrowed()`**, on `ProvideValue` only, keeps the binding and the value's readiness contribution and leaves starting and shutting it down to the caller — for a pool that two Apps in one process share, or a fixture a test suite reuses across the Apps it builds. Resource identity cannot say that, because it is unique only inside one App. The option is accepted on a binding whose type does not show `Shutdown`: that is how a caller keeps the teardown of a value that has the method behind an interface.
- **`credo.Closer()`** makes a binding whose type has a `Close` method — `Close() error`, `Close()` or `Close(ctx) error`, the last receiving the drain deadline — a component the App closes after its consumers, abandoned at the deadline like any shutdown. It panics on a type with none of the three, on a type that is already a component, and beside `credo.Borrowed()`. It decides its binding's teardown: a built value that also has `Shutdown` is closed once, with `Close`. `Close` is never discovered on its own: the method is too common to mean that the App owns the value, and its common forms take no deadline.
- **`credo.Override()`** replaces an earlier binding of the same type before `Finalize` and panics when there is none, so an override that no longer matches the wiring fails instead of adding a binding nothing resolves while the test runs against the real dependency. Without the option a duplicate binding still panics, so an accidental double registration is caught. `testutil.WithOverride[T]` builds on it and is equally strict; adding a binding is `testutil.WithWiring`'s job.

`app.Manage(v, opts...)` hands the App a component that is not a binding — a value, or a constructor over DI parameters that the start walk builds — and takes `credo.Ingress()` and `credo.Named("…")`, the name the readiness aggregate and the lifecycle report use, defaulting to the type name; a duplicate name panics. [ADR-024](024-lifecycle-components.md) specifies it.

Within one App the component registry keys components by resource identity — the token a value's `ResourceIdentity()` returns (the `credo.ResourceIdentifier` capability), otherwise the value itself when it is comparable (a pointer, or a struct over one), otherwise its binding alone — so one resource held by several bindings has one teardown, run once, after the consumers of every holder. The holders that have a teardown agree on its kind, `Shutdown` or `Close` through `credo.Closer()`, and the one registered first runs it; a borrowed resource is torn down through none of its holders. The [container spec](../specs/container.md) states the rules as the container applies them.

**A teardown that is not a `Close`** — a client's `Disconnect(ctx)`, a `Drain` that returns at once and must be waited for — belongs to a type that embeds the client and implements `Shutdown`, registered as the binding itself. Consumers depend on that type, so its edges are right, and its constructor's parameters give the teardown's own dependencies their edges; `sqldb.DB` wraps Bun this way. No API is added for it. Rejected: a teardown-function option, `credo.ShutdownWith(func(ctx, T) error)` — Go cannot tie the option's type parameter to `Provide`'s at compile time, so a mismatch would surface only as a registration panic, and the dependencies the function uses would have no edges, so the graph could close them first; its non-generic form, `func(ctx) error`, would fit `ProvideValue` only. Deferred: a constructor returning `(T, func(context.Context) error, error)`, the cleanup function of Google Wire's providers — compile-time typed and written beside the construction — until a type proves costly to wrap, such as a third-party API whose raw type many consumers need. Adding it later breaks nothing, because a three-value constructor is rejected today; removing it after v1 would.

**Removes.** `testutil.WithOverride`'s silent addition of a binding that was never wired.

### Several Instances of One Type

**Accepted, pending implementation (v0.24.0, W4)** for the component registry this section relies on; the wrapper rule itself already holds.

The container keys bindings by type, so several instances of one type are several wrapper types — `type PrimaryDB struct{ *sqldb.DB }` and `type AnalyticsDB struct{ *sqldb.DB }` — each bound once. The type is the qualifier: the constructor's signature names the instance (`NewReportRepo(db AnalyticsDB)`), the compiler and `Finalize` check the choice, and the graph sees the edge, so each instance starts and stops in order. Embedding carries every capability of the handle — `Shutdown`, `Ping`, `Health`, `ResourceIdentity` — and each `*sqldb.DB` owns its transaction scope, so the transactions of two databases never meet. The rule composes with generics: `NewOutboxRepo[PrimaryDB]` and `NewOutboxRepo[AnalyticsDB]` are two bindings with no name between them. Google Wire answers the same question the same way, and the [httpclient spec](../specs/httpclient.md) already applies it to several `*http.Client`s. Under the component registry (W4) a wrapper and the handle it embeds are one resource through resource identity, so the wrapper, its interface view and its raw handle share one teardown.

Two cases sit outside the rule. Instances whose number comes from configuration — shards, tenants — are one binding of a collection type that owns and closes them all, since a type per instance cannot be written. A read replica of one database is routing, not a second instance.

Rejected:

- **Named bindings** (`ProvideNamed`, name tags on parameter objects) — the signature no longer says which instance, the name travels in a tag or a parameter object read by reflection, and a typo surfaces at `Finalize` at best: the cost for which struct-tag injection is rejected below.
- **A qualifier type in the container** (`credo.Named[*sqldb.DB, Analytics]`) — typed, but it puts the container's plumbing in every business signature and unwraps a field at each use: a wrapper with a worse name.
- **Scoped child containers** — the signature stays `*sqldb.DB`, and the instance a repository receives depends on where it was registered, which hides what the wrapper shows, for a large feature beside one graph.
- **Wiring the instances outside the container** by closure — the graph loses the edges, and an instance can close before its consumers stop.

### DI Stays the Spine

Rejected: a kernel that is complete without DI, with DI as an opt-in layer, and, as its radical form, Wire-style code generation. DI is part of Credo's identity: [ADR-001](001-framework-identity-and-goals.md) names structured dependency injection as a reflection of the enterprise target and has configuration cross module boundaries as typed structs via DI. With framework infrastructure in kernel-owned registries (The Public Surface), the kernel does not depend on DI internally, and a minimal App is `credo.New()`, routes and `Run()` without a binding, so an opt-in layer would change only where DI's API lives and how the documentation positions it. Code generation adds a tool step to every developer's loop.

The real cost of reflection-based registration is not startup time, which is paid once: a wrong constructor surfaces when `Provide` runs and a missing dependency at `Finalize`, not in the compiler. A hand-wired `main` is checked by the compiler more strictly, at the price of churn at every call site when a constructor's signature changes, and Go has no variadic type parameters, so a reflection-free `Provide` over constructors of any arity cannot be written: the choice was between arity families, code generation and reflection, and Credo keeps reflection. That cost is answered by better reporting: `Provide` checks the constructor's signature at the call and panics there ([ADR-022](022-bootstrap-and-di-ownership.md)), `Finalize` reports a missing dependency with its whole path — `OrderService → PaymentClient → *http.Client (not registered)` — and, optionally after v1, a `go/analysis` checker reports signature mistakes before the program runs.

### ProvideValue Preflight and Protected Bindings

`app.CanProvideValue[T]() error` performs the same local frozen-container and direct duplicate-`T` checks that `ProvideValue[T]` applies, without registering or reserving anything. It exists for composition helpers such as `store.Register`, which should reject predictable local conflicts before doing network I/O.

The result is only a point-in-time observation. A registration made in between may register `T` or finalize the container, so a nil result is not a promise that a later regular or protected value publication will succeed. The final publication remains authoritative: `ProvideValue` panics on a conflict, and callers handle `ProvideProtectedValue`'s error.

Two low-level methods support integrations whose DI binding is coupled to external lifecycle, health, or registration state:

- `app.ProvideProtectedValue[T](value)` registers a pre-built singleton and marks its direct binding as protected from `app.Replace[T]`.
- `app.ProtectBinding[T](expected ...T)` protects an existing direct binding. With no expected value it is idempotent and does not resolve T. With one expected value it performs a CAS-style compare-and-protect: under the same lock used by `Replace`, it verifies that the already-resolved singleton is comparable and still equal to expected before protecting the binding. An unresolved, non-comparable, or changed value returns an error without adding protection. More than one expected value is rejected. Both forms require T to be registered and must run before Finalize.

`Replace[T]` returns an error for a protected binding. This prevents an integration from continuing to monitor or shut down one value while DI starts resolving another. Protection affects binding replacement only; it does not by itself create lifecycle ownership, health wiring, aliases, or collection bindings. Normal application and test bindings should continue to use override-friendly `ProvideValue`; the protected variants are intended for framework/integration registration paths such as `store.Register`.

### Registration-Time Reads and Replacement Ownership

Constructors run only after Finalize, so the registration phase has no general `Resolve`. Two observation/adoption operations cover what integrations need:

- `app.Has[T]() bool` reports registration presence (direct or alias) without constructing, adopting or protecting. It is a snapshot, not a reservation.
- `app.AdoptValue[T](validate func(T) error) (T, error)` reads an existing pre-built binding, validates it and atomically compare-and-protects that same binding. Protection is a consequence of successful validation, never of the read, so a rejected value (a typed-nil Registry) stays repairable through `Replace`. A replacement or phase change during validation makes the adoption fail rather than protect a stale instance. A constructor binding is rejected with an explanatory error and is never invoked; `store.Register` and `worker.Register` rely on this to refuse a Registry or Pool registered through `Provide`.

`Replace[T](value) (old T, existed bool, err error)` transfers ownership explicitly: on success the container owns the new value and returns any already-created superseded instance to the caller, who assumes its cleanup responsibility. `existed` means exactly "a previously created instance existed": a superseded constructor binding that never ran yields zero and false, and Replace never constructs an old provider merely to return it. A superseded `Shutdowner` is logged at Warn as a reminder, not as the transfer mechanism. A rejected replacement changes neither the binding nor ownership. `MustReplace` returns the same information and panics on error.

### Finalize Phase

`app.Finalize()` freezes the container and validates the dependency graph. After Finalize, a `Provide`, `ProvideValue`, `Alias` or `BindMany` call panics at the call site, as it does after shutdown began, and `ProvideProtectedValue`, `ProtectBinding`, `AdoptValue` and `Replace` return an error. `Resolve` is admitted only after Finalize and panics before it: constructors never run during registration, so the composition root finishes every DI write (store/worker registration included), calls Finalize and handles its error, and only then resolves controllers and binds routes. `Run()`, `RunContext()`, `ServeContext()` and the first direct `ServeHTTP` call Finalize implicitly as an idempotent safeguard, which cannot precede a Resolve the composition root has already executed. Finalize is DI-only; HTTP registration stays open until the App prepares to serve (ADR-006). Credo's recommended usage keeps `Resolve` in bootstrap/composition-root code; runtime `Resolve` remains available but is not the preferred application pattern, and the shutdown hooks (`OnPreDrain`, `OnDrain`, `OnShutdown`) capture their dependencies instead of resolving; `OnStart` runs after Finalize and may resolve (`worker.RegisterProvided` resolves its workers there). After a failed Finalize, `Resolve` returns the error. `Finalize` returns what only the whole graph reveals — missing dependencies, each with its whole path, and cycles — joined in registration order. The phases and the documented bootstrap order are [ADR-022](022-bootstrap-and-di-ownership.md)'s.

**Accepted, pending implementation (v0.24.0, W4, W6).** `Manage` (W4) and the worker registrations (W6) after `Finalize` panic like the DI registrations, and `Finalize` also reports an internal component that depends on an ingress one (W4).

Construction completes once per singleton with a terminal result — value, error or panic — shared by every waiter; nothing is retried. A constructor panic is returned as `*credo.DIPanicError` (type, phase, original value, stack) and `MustResolve` panics with that error as its payload. Once shutdown reaches DI teardown the container is closing: new resolution, cached results included, returns an error wrapping `credo.ErrDIClosed`, while instances created by already-admitted builds remain owned and cleaned up by the container.

### Shutdown

`Container.Shutdown(ctx)` tears down live singletons in dependency order — consumers before the singletons they were constructed from — with a Kahn ready queue over the static graph (constructor parameters, aliases, collection edges; value bindings are vertices, dependencies hidden inside pre-built values are not) and reverse registration order as the tie-break. Each `Shutdowner` gets at most one sequential attempt bounded by the shared context through a helper goroutine; a hung callback keeps its dependencies blocked and is reported, never retried or closed around. Only construction completing after the context ended receives one separate fixed five-second best-effort attempt. Failure or incompleteness returns `*credo.DIShutdownError`, an immutable per-vertex snapshot that unwraps its causes; shutdown panics are isolated as `DIPanicError`. The [container spec](../specs/container.md#shutdown) and the [bootstrap contract](../specs/bootstrap-and-di-lifecycle.md) carry the full rules.

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
| General early Resolve/Peek during registration; protect-on-read | Would run constructors against an unvalidated graph or freeze an invalid binding before validation. `AdoptValue` (validate, then atomically protect) and `Has` cover integration needs |
| Reverse registration order for shutdown | Ignores the dependency graph: a service registered before its DB closed after it. Dependency order with reverse-registration tie-break keeps the old order where the graph does not decide |
| A kernel complete without DI, DI as an opt-in layer; Wire-style code generation | DI is part of Credo's identity (ADR-001); the kernel's independence from DI comes from kernel-owned registries, and reflection's real cost is answered by reporting (DI Stays the Spine) |
| Named bindings; a qualifier type in the container; scoped child containers; wiring several instances outside the container | A wrapper type per instance keeps the choice in the constructor's signature, where the compiler and `Finalize` check it and the graph sees the edge (Several Instances of One Type) |
| A teardown-function option (`credo.ShutdownWith`) | Its type parameter cannot be tied to `Provide`'s at compile time, and the function's own dependencies would have no edges; a wrapper type that implements `Shutdown` is the binding instead (Registration Options) |

## Consequences

**Accepted, pending implementation (v0.24.0, W3–W5).** When the work items ship, these consequences join the lists below, and the replacement-ownership item goes:

- The container holds only the application's bindings; seven methods and `Finalize` are its whole surface, and none of them exists for a framework integration.
- An override is a registration option checked before `Finalize`, never a runtime replacement, and an override that matches nothing panics.
- A resource's teardown, its owner and its tier are declared where it is bound; a resource several bindings hold is torn down once.
- A store takes its binding and a registration by type, and several instances of one type take a wrapper type each — more types in exchange for signatures that say which instance they receive.

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
- Replacement ownership is explicit: the caller receives the superseded instance instead of leaking it

**Negative:**

- `credo.Infra` parameter must be added to every constructor (minimal boilerplate)
- Infra is not extensible — adding a new infra type requires a framework change
- Special code path in container for `credo.Infra` — but it's a simple type switch
- `BindMany` adds ordering and empty-collection semantics that must be documented clearly
- Container is in `internal/di` — cannot be used as a standalone DI library
- The composition root needs an explicit, error-checked `Finalize` between registration and the first `Resolve`
