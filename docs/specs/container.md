# DI Container & credo.Infra Spec

**Status:** Implemented (beta) **Package:** `internal/di/`; root `di.go`, `component.go`, `infra.go`, `dierrors.go`, `lifecycle_error.go` **ADR:** [ADR-004](../adr/004-dependency-injection-and-infra.md), [ADR-022](../adr/022-bootstrap-and-di-ownership.md), [ADR-024](../adr/024-lifecycle-components.md) **Guide:** [Dependency injection guide](../guides/dependency-injection.md) **Sources:** samber/do (MIT)

This spec states the DI container: its surface, registration and its options, `Finalize` and the three error phases, resolution, resource identity, the container's part in teardown, `credo.Infra`, and the [sequential bootstrap](#sequential-bootstrap) with its documented order. Preparation, the start phase and the drain sequence are the [lifecycle spec](lifecycle.md)'s.

## Overview

Credo's DI system has two parts: a generics-based container in `internal/di`, adapted from samber/do and exposed as generic methods on `*App` — `app.Provide[T](constructor)`, `app.BindMany[I, T]()`, `app.Resolve[T]()`, `app.ResolveAll[I]()` — with no code generation; and `credo.Infra`, a framework-managed carrier defined in the root package, which the container produces for a constructor parameter while it stays visible in the constructor's signature. The container is Credo-specific, not a standalone DI library ([ADR-004](../adr/004-dependency-injection-and-infra.md)).

## Goals

1. **Generics over reflection**: `Resolve[T]` is fully compile-time typed. `Provide[T]`'s `constructor` parameter is necessarily `any` (Go cannot express "a function with arbitrary parameters returning T"), so its signature is checked at the `Provide` call and the dependency graph at `Finalize`. Reflection stays on the cold path ([Registration](#registration-root-package)).
2. **Framework-managed infrastructure, explicit boundary**: `credo.Infra` is a visible constructor parameter; services do not receive hidden field population or struct-tag injection.
3. **Composition root first**: registrations, then `Finalize`, then resolution ([Resolution](#resolution-root-package)).
4. **Always available**: `credo.Infra` works without a configured logger ([below](#always-available-default-logger-fallback)).
5. **Single lifecycle: Singleton**: One instance per container lifetime.
6. **Ordered interface collections**: Components can depend on `[]I` when startup wiring needs an ordered set of implementations (hooks, senders, subscribers) without named services or keyed lookups.

## credo.Infra --- Explicit Infrastructure Carrier

### Definition

`credo.Infra` is a framework-defined, non-extensible struct in the **root package**. It carries infrastructure that most services need:

```go
// root package (credo)
type Infra struct {
    _ struct{} // forces keyed literals so new fields can be added compatibly

    Logger *slog.Logger
}
```

Users cannot add fields to `Infra`. The framework defines the set. Metrics and tracing are deliberately not on `Infra`: they are deferred to the observability release, which decides whether and how they reach it ([ADR-014](../adr/014-observability.md)). The keyed-literal guard (`_ struct{}`) keeps adding fields compatible with existing `credo.Infra{Logger: ...}` literals.

### Always Available (Default-Logger Fallback)

When no logger is configured, the framework produces `Infra` with a safe fallback:

- `Logger`: a framework-owned stderr logger (deliberately independent of `slog.SetDefault`, so framework behavior does not change under global mutable state)

This guarantees:

- **Tests work without container setup.** Construct a service directly, pass a custom `Infra{Logger: testLogger}`. No panics.
- **Gradual opt-in.** Services work immediately; observability is layered on when ready.

### Per-Service Scoping

When the container produces `Infra`, the Logger is **scoped** to the service's type name:

```go
// Container internally does:
infra.Logger = appLogger.With("service", "OrderService")
```

`appLogger` is the App's logger — the one `credo.WithLogger` configures, else the framework default — captured at `credo.New`; a `*slog.Logger` binding in the container is never consulted. Every log line from `OrderService` automatically includes `"service"="OrderService"` without the developer passing it manually.

### Non-DI Infra

For components outside the DI container (middleware factories, startup helpers, manually created workers), use `app.NewInfra(name)`:

```go
rbacMW := rbac.NewMiddleware(app.NewInfra("RBACMiddleware"))
app.GlobalMiddleware(rbacMW)
```

The returned Infra has a Logger scoped with `"service"=name`. `NewInfra` is nil-safe: when the app has no logger configured it falls back to the framework default logger. For DI-managed services, Infra is injected automatically — prefer DI when possible.

### Config Is Not in Infra

Config is not part of `credo.Infra`; typed config sections are constructor parameters ([ADR-005](../adr/005-configuration-architecture.md#config-is-not-in-infra)).

### What Goes Where

| Dependency | Access | Rationale |
| --- | --- | --- |
| DB connections | Constructor param | Behavior-defining, must be mockable |
| Repositories | Constructor param | Behavior-defining |
| Domain services | Constructor param | Behavior-defining |
| Typed config struct | Constructor param | Structural, read at startup |
| Logger | `infra.Logger` (via `credo.Infra`) | Ubiquitous, cross-cutting |
| Metrics / Tracer | Not available yet | Deferred to the observability release ([ADR-014](../adr/014-observability.md)) |
| i18n | App (`UseI18n`) | [ADR-013](../adr/013-internationalization.md) |
| Request ID | Context | `ctx.RequestID()` --- per-request |

## Injection Models

The container supports two injection models. Developers choose per-service.

### The Infra Parameter

Convention: `credo.Infra` is the first parameter, like `context.Context`.

```go
func NewUserService(infra credo.Infra, repo *UserRepo, db *sql.DB) *UserService {
    infra.Logger.Info("user service initialized")
    return &UserService{
        infra: infra,
        repo:  repo,
        db:    db,
    }
}
```

Best for services needing infrastructure alongside business dependencies.

### Pure Constructor Injection (No Infra)

Services that don't need infrastructure simply omit `credo.Infra`:

```go
func NewUserRepo(db *sql.DB) *UserRepo {
    return &UserRepo{db: db}
}
```

The container resolves all parameters normally. No Infra magic.

### Container Detection Logic

The container inspects the constructor signature at registration time. Each parameter is checked independently:

1. Parameter type is `credo.Infra` --- **the Infra parameter**: produced specially (scoped Logger, default-logger fallback), never resolved from a registration. Convention: place `credo.Infra` as the first parameter.
2. Otherwise --- **Pure constructor injection**: parameter resolved normally from the container.

The root package registers `credo.Infra` with the container as a framework-provided type, so producing it is one map lookup per constructor parameter on the cold path --- no extra reflection beyond what the container already does for constructor inspection.

## API Surface

### The Surface

The container's public surface is seven methods and the phase verb ([ADR-004](../adr/004-dependency-injection-and-infra.md#the-public-surface)):

```text
// Registration — returns nothing, panics on misuse
app.Provide[T](constructor, options...)
app.ProvideValue[T](value, options...)
app.Alias[I, T]()
app.BindMany[I, T]()

// Observation — never constructs
app.Has[T]() bool

// Resolution — after Finalize only
app.Resolve[T]() (T, error)
app.ResolveAll[I]() ([]I, error)

// Phase verb
app.Finalize() error
```

`options` are the registration options below. `Has[T]` reports whether T is registered, directly or through `Alias`, without constructing; it is the one non-resolving presence probe, for a composition root that asks whether an optional module is present ("is the optional module registered?"). It reserves nothing and never claims that an instance is healthy or usable, and its result is a snapshot of the registrations made so far: an override after `Has` still replaces the binding. `MustResolve` and `MustResolveAll` sit beside the surface as resolve conveniences for the composition root (Resolution); they are not registration twins. After `Finalize`, a `Provide`, `ProvideValue`, `Alias` or `BindMany` call panics and `Resolve` becomes available.

No integration monitors or shuts down one value while DI resolves another: the framework's registrations name bindings, not values — a component is its binding (Registration Options), the store registry names the store's binding by type and resolves it once in the start phase, after every override ([store spec](store.md)), and a DI-provided worker's component is a constructor over its binding, built by the start walk ([worker spec](worker.md#provided-workers)).

### Registration Misuse

Registration performs no I/O and panics on misuse known at the call site, with a message naming the call, the phase and the remedy:

| Misuse | Panics at |
| --- | --- |
| A constructor that is nil, not a function, does not return T first, or returns more than `(T, error)` | `Provide` |
| A second binding of the same type without `credo.Override()` | `Provide`, `ProvideValue` |
| `Alias` or `BindMany` whose types do not fit (non-interface I, unregistered or non-implementing T, existing alias or pair) | `Alias`, `BindMany` |
| A misused registration option (Registration Options) | `Provide`, `ProvideValue`, `Manage` |
| Any DI or component registration after `Finalize` — `Provide`, `ProvideValue`, `Alias`, `BindMany`, `Manage`, store and worker registrations | The call |
| Any registration after the App is prepared or shut down | The call |
| `Resolve`, `MustResolve`, `ResolveAll` or `MustResolveAll` before `Finalize` | The call |

Registration calls are not safe for concurrent use; bootstrap is sequential ([Sequential Bootstrap](#sequential-bootstrap)). `Finalize` returns an error, because what it reports needs the whole graph (Finalize and Container Lifecycle).

### Registration Options

**Which bindings are components.** A singleton the App owns is a component when it has `Shutdown(ctx) error` — its binding's type shows the method at registration, or its built value has it — because ownership, not the view its consumers take, decides the teardown; a component found only on its built value is internal. `Start`, `Ready` and the tier are planned from the binding's type before any constructor runs ([lifecycle spec](lifecycle.md#component-types)), and [the start walk](lifecycle.md#the-start-walk) states what that means for a value behind an interface.

`Provide` and `ProvideValue` take registration options of type `credo.RegistrationOption`. Each is checked at the call, and misuse — an option the call does not accept, a zero option, an option given twice — panics there:

| Option | Accepted on | Effect | Panics |
| --- | --- | --- | --- |
| `credo.Ingress()` | `Provide`, `ProvideValue`, `Manage`, `OnStart`, `OnStop` | Places the component, or the hook, in the ingress tier | On a binding that is not a component at registration — whose type shows no `Shutdown` and that carries no `credo.Closer()` — since the tier is planned before the value exists |
| `credo.Borrowed()` | `ProvideValue` only | Keeps the binding and the value's readiness contribution; starting and shutting down stay with the caller | On `Provide`; beside `credo.Closer()`; at the later call, or the later construction, of a holder of the same resource that claims its teardown (Resource Identity) |
| `credo.Closer()` | `Provide`, `ProvideValue` | Makes a binding whose type has a `Close` method a component that the App closes after its consumers | On a type with none of the three `Close` shapes; on a type that is already a component; beside `credo.Borrowed()` |
| `credo.Override()` | `Provide`, `ProvideValue` | Replaces an earlier binding of the same type before `Finalize` | When there is no earlier binding of the type; after `Finalize` |
| `credo.Named(name)` | `Manage` only | Names the managed component in reports and readiness (default: its type name) | On an empty name; on a name another managed component has |

- **`Borrowed`** is for a pool that two Apps in one process share, or a fixture a test suite reuses across the Apps it builds ([ADR-024](../adr/024-lifecycle-components.md#ownership-and-registration-options) gives why resource identity cannot express it). It is accepted on a binding whose type does not show `Shutdown`: that is how a caller keeps the teardown of a value that has the method behind an interface.
- **`Closer`** accepts the three shapes `Close` takes, checked from the binding's type: `Close() error` (`*sql.DB`, a Redis client), `Close()` (`*pgxpool.Pool`) and `Close(ctx) error` (`*pgx.Conn`), the last receiving the drain deadline. A `Close` that blocks past the deadline is abandoned like any `Shutdown`, and its dependencies stay open. The option decides its binding's teardown: a built value that also has `Shutdown` is closed once, with `Close`. `Close` is never discovered without the option.
- **`Override`** is strict: without an earlier binding of the type it panics, so an override that no longer matches the wiring fails instead of adding a binding nothing resolves. Without the option a duplicate still panics. The replaced binding may be a constructor or a value; since constructors run only after `Finalize`, an override never supersedes a built instance. A value that an override replaces never becomes the App's: the App neither starts nor shuts it down, and a replaced `ProvideValue` value stays with the caller who created it.
- **`testutil.WithOverride[T](v)`** builds on `credo.Override()` and is equally strict: without an earlier binding of T, wired by the test or by `testutil.WithWiring`, it panics. Adding a binding is `WithWiring`'s job.

`app.Manage(v, opts...)` adds a component that is not a binding — a value with `Shutdown`, or a constructor `func(deps...) C` or `func(deps...) (C, error)` that the start walk builds — and accepts `credo.Ingress()` and `credo.Named(name)`; the [lifecycle spec](lifecycle.md#appmanagev-opts) states its rules, and a child App handed to its parent's `Manage` runs its start phase in the parent's ([mounted Apps](lifecycle.md#mounted-apps)).

A teardown that is neither `Shutdown` nor one of the `Close` shapes — a client's `Disconnect(ctx)`, a `Drain` that must be waited for — belongs to a type that embeds the client and implements `Shutdown`, registered as the binding itself, so its constructor's parameters give the teardown's own dependencies their edges. There is no teardown-function option, and a constructor returning `(T, func(context.Context) error, error)` is rejected at `Provide`; [ADR-024](../adr/024-lifecycle-components.md#rejected-alternatives) records why, and [defers the three-value form](../adr/024-lifecycle-components.md#consequences) until a type proves costly to wrap.

### Resource Identity and Teardown

**One resource, one teardown.** Within one App the component registry keys components by resource identity:

1. the token the value's `ResourceIdentity()` method returns, the `credo.ResourceIdentifier` capability — `*sqldb.DB` returns itself, and a wrapper that embeds it inherits the method;
2. otherwise the value itself, when it is comparable — a pointer, or a struct over one;
3. otherwise its binding alone.

Values that share an identity — one pointer under several bindings, the interface view an adapter constructor returns, a wrapper and the handle it embeds, two wrappers over one handle, a value that a constructor returns after `Manage` received it — are one component. It is torn down once, when the last holder retires, so the consumers of every holder stop first, and started once, through the first holder the start walk reaches whose binding plans a `Start`. A holder that obtains the resource through one of its consumers does not keep that consumer open: the consumer stops first, then the resource. Only that route is dropped — the edge a holder which reached the resource through another holder contributes, when it closes a cycle; a dependency that a holder which reached the resource on its own contributes always holds. Component values that are not comparable are their bindings' own, each torn down once.

**The holders agree on the teardown.** The application chooses the teardown, not the order of construction. The holders that have a teardown agree on its kind — `Shutdown`, or `Close` through `credo.Closer()` — and a disagreement panics at the later `ProvideValue` call when both values are given, and otherwise fails the construction of the holder built later. The message names both bindings and both remedies: drop `credo.Closer()`, or bind the holder that has `Shutdown` under a type that shows `Close` and not `Shutdown`, with `credo.Closer()`. Among holders of one kind, the one registered first runs it, whichever is built first. A holder without a teardown takes no part in the choice, and the resource still waits for its consumers.

Kinds are all the registry can compare: Go does not tell a `Shutdown` that a wrapper inherits from one it declares. The choice therefore rests on a contract — only a value without state of its own to release may share an identity. A wrapper that embeds a handle which identifies itself inherits that identity, so a wrapper that releases state of its own holds the handle in a named field instead; a wrapper that forwards an identity promises that it keeps none.

**Misuse.** A `ResourceIdentity()` that panics, or returns a token that is nil, not comparable or not equal to itself, panics at `ProvideValue` and `Manage` and fails a constructor's construction. `Manage` of a value whose resource a `ProvideValue` binding holds panics, and so does a resource handed to `Manage` twice. A value whose construction the registry fails after the constructor returned it — an unusable identity, an internal component found on its value that depends on an ingress one — is released by the drain when no other holder carries it; one refused for a holder conflict is the shared resource and is left to its owner.

**Borrowed resources** are started and torn down through none of their holders: a holder whose type shows `Start` does not start a resource another holder borrows. A holder that claims the teardown — a component bound with `ProvideValue`, a value handed to `Manage`, a binding with `credo.Closer()` — disagrees with `credo.Borrowed()` on the owner and panics at the later call or fails its construction. A value that a constructor builds over a borrowed resource without `credo.Closer()` stays the caller's even when it has `Shutdown`, since a `Shutdown` found on a value is no claim.

The order in which components stop — the tiers, the shared deadline, the abandonment of a teardown that misses it — is the [lifecycle spec](lifecycle.md#shutdown-in-tiers)'s; the container contributes the static graph that orders it ([Shutdown](#shutdown)).

### Registration (root package)

```go
// Provide registers a constructor for type T. The constructor's parameters
// are resolved from the container automatically. Lifecycle: Singleton.
// It accepts credo.Ingress(), credo.Closer() and credo.Override(), and
// panics on misuse (Registration Misuse).
func (app *App) Provide[T any](constructor any, opts ...RegistrationOption)

// ProvideValue registers a pre-built value as a Singleton. It accepts
// credo.Ingress(), credo.Borrowed(), credo.Closer() and credo.Override(),
// and panics on misuse, like Provide.
func (app *App) ProvideValue[T any](value T, opts ...RegistrationOption)

// Has reports whether T is registered (directly or through Alias). It never
// constructs and reserves nothing; the result is a snapshot of the
// registrations made so far.
func (app *App) Has[T any]() bool
```

A binding is replaced with `credo.Override()` (Registration Options), never after the fact.

The `constructor` parameter accepts any function whose parameters are resolvable types and whose first return value is `T`:

```go
// All valid constructor signatures:
func NewUserRepo(db *sql.DB) *UserRepo
func NewUserRepo(db *sql.DB) (*UserRepo, error)
func NewUserRepo(infra credo.Infra, db *sql.DB) (*UserRepo, error)
```

Constructor parameter types are inspected via reflection at registration time and matched against registered types. The first construction of each singleton invokes the constructor with `reflect.Value.Call`. Resolving a built singleton afterwards is a map lookup with no `reflect.Value` call; `ResolveAll` assembles its slice with reflection on each call.

Because `constructor` is typed `any`, a signature mistake (wrong return type, not a function) panics at the `Provide` call --- not at compile time. There is no closure-based factory alternative: a constructor that captures `app` and calls `Resolve` inside its body is unsupported. Such a call is invisible to `Finalize` graph validation, cycle detection and dependency-ordered shutdown, and since constructors run only after Finalize it cannot be scheduled against the graph. Declare every dependency as a constructor parameter.

### Interface Aliasing

```go
// Alias creates an alias so that Resolve[I] returns the instance registered
// for concrete type T. I must be an interface, T must implement I, and T
// must already be registered. It panics on misuse.
func (app *App) Alias[I, T any]()
```

Alias enables resolving by interface without requiring the constructor to return the interface type:

```go
// Register the concrete type.
app.Provide[*PgUserRepo](NewPgUserRepo)

// Alias interface to concrete type.
app.Alias[UserRepo, *PgUserRepo]()

// Now resolving by interface returns the *PgUserRepo singleton.
repo := app.MustResolve[UserRepo]()
```

Contract rules enforced by `Alias`, each violation a panic at the call:

- `I` must be an interface type
- `T` must implement `I`
- `T` must already be registered via `Provide` or `ProvideValue`
- `I` must not already have a direct registration or existing alias
- `Finalize` must not have been called yet, and shutdown must not have begun

### Ordered Interface Collections

```go
// BindMany adds concrete type T to the ordered collection for interface I.
// I must be an interface, T must be registered already, T must be concrete,
// and T must implement I. It panics on misuse.
func (app *App) BindMany[I, T any]()
```

`BindMany[I, T]` is collection wiring, not default resolution. It does not change `Resolve[I]`; it only affects `ResolveAll[I]` and constructor injection of `[]I`.

```go
app.Provide[*EmailSender](NewEmailSender)
app.Provide[*InAppSender](NewInAppSender)

app.BindMany[Sender, *EmailSender]()
app.BindMany[Sender, *InAppSender]()
```

Contract rules enforced by `BindMany`, each violation a panic at the call:

- `I` must be an interface type
- `T` must be a concrete type (not an interface)
- `T` must implement `I`
- `T` must already be registered via `Provide` or `ProvideValue`
- The same `(I, T)` pair must not already exist
- `Finalize` must not have been called yet, and shutdown must not have begun

### Finalize and Container Lifecycle

The container moves through registration, `Finalize`, runtime and closing:

1. **Registration** --- `Provide` and `ProvideValue` with their registration options, `Alias`, `BindMany` and `Manage` are allowed; `Has` observes without mutating or reserving. `Resolve` and `ResolveAll` panic --- constructors never run in this phase; neither `store.Register` nor the worker registrations read the container.
2. **Finalize** --- `app.Finalize()` freezes the container and validates the dependency graph. After Finalize, `Provide`, `ProvideValue`, `Alias`, `BindMany` and `Manage` panic at the call, naming the call and the phase. If validation fails, subsequent `Resolve` and `ResolveAll` calls return the finalize error. Finalize is DI-only: routes, hooks, renderers and other HTTP registrations stay open until the App prepares to serve, so controllers built from resolved services can still be wired afterwards (see the [lifecycle spec](lifecycle.md#preparation)).
3. **Runtime** --- `Resolve` creates and caches singletons on demand. The dependency graph is guaranteed valid. `app.Run()`, `app.RunContext()`, `app.ServeContext()`, `app.Start(ctx)` and the first direct `ServeHTTP` call Finalize implicitly.
4. **Closing** --- the drain freezes the container when it begins and enters closing when the internal tier's components begin to stop, after the HTTP drain, the ingress tier and the internal stop hooks ([lifecycle spec](lifecycle.md#shutdown-in-tiers)). New `Resolve` calls, cached results included, return an error wrapping `credo.ErrDIClosed`; builds already admitted are tracked for cleanup even when their caller's delivery loses to closing. The closing check precedes the failed-Finalize check, so teardown rejection wins even after a failed `Finalize`. A never-finalized container can still be shut down (bootstrap teardown): the cleanup graph is derived from the frozen registrations and current instances without requiring successful validation.

```go
// Finalize freezes the container and validates the dependency graph.
// After Finalize, a Provide, ProvideValue, Alias or BindMany call panics,
// and Resolve becomes available. Finalize returns every problem the graph
// holds, joined in registration order.
// Finalize is idempotent --- subsequent calls return the same result via sync.Once.
//
// Finalize is side-effect-free: it does not instantiate singletons or perform I/O.
// It only freezes the container and validates the graph. It does not close
// HTTP registration.
//
// The serve entry points, App.Start and the first ServeHTTP call Finalize
// implicitly. Explicit, error-checked Finalize is the recommended
// composition-root step between registration and the first Resolve.
func (app *App) Finalize() error
```

```go
// Registration phase
app.Provide[*sql.DB](NewDB)
app.Provide[*PgUserRepo](NewPgUserRepo)
app.Alias[UserRepo, *PgUserRepo]()
app.Provide[*UserService](NewUserService)

// Finalize phase --- freeze + validate
if err := app.Finalize(); err != nil {
    log.Fatal(err) // e.g. "di: missing dependency: *UserService → *Mailer (not registered); ..."
}

// Runtime phase --- safe to resolve
userSvc := app.MustResolve[*UserService]()

// These panic after Finalize ("called after Finalize; make every
// registration before Finalize, ..."):
// app.Provide[*Foo](NewFoo)
// app.Alias[Bar, *Baz]()
// app.BindMany[Qux, *Baz]()
```

If `app.Finalize()` is not called explicitly, the serve entry points and `App.Start` call it implicitly as the first step of preparation, and a direct `ServeHTTP` call does the same on the first request. A composition root that resolves services before serving must call it explicitly first.

`Finalize` returns what only the whole graph reveals, joined in registration order:

- each missing dependency, with its whole path from the entry-point registration that needs it — `di: missing dependency: *app.OrderService → *app.PaymentClient → *http.Client (not registered); provide *http.Client before Finalize` — where an alias hop reads `app.Gateway (alias of *app.Stripe)` and a collection member `*app.Audit (in []app.Plugin)`;
- each circular dependency (A → B → A), through constructor parameters, aliases and collections alike, listing its members and asking to remove one of those constructor parameters;
- each constructor parameter of type `context.Context`: constructors run at startup, not per request;
- an internal component that depends on an ingress one, directly or through bindings that are not components, with the path and both remedies — declare the dependent ingress, or split the ingress component so that what internal components use is an internal part;
- after the graph's findings, a store registration whose type has no binding — neither a direct binding nor an interface an `Alias` names: `di: store.Register[T]: T has no binding; bind it with Provide or ProvideValue before Finalize` ([store spec](store.md#registration)).

The report is deterministic: the same wiring yields the same text on every run. A consumer registered earlier is reported earlier, its parameters left to right, and every cycle's text starts at the member that was registered first. Registration order does not otherwise matter: a constructor may depend on a type registered after it. `Alias` and `BindMany` check their types when the binding is made and no registration is ever removed, so `Finalize` does not check them again. Dependencies inside pre-built values are opaque to the container and take no part in validation or shutdown ordering. A component found only on its built value is internal and is checked when it is built: such a component that depends on an ingress one fails its construction with the same message as the `Finalize` finding.

### Resolution (root package)

```go
// Resolve returns the instance of T, creating it if necessary.
func (app *App) Resolve[T any]() (T, error)

// MustResolve panics if T cannot be resolved. It is primarily intended for
// bootstrap/composition-root code. Runtime use is supported, but Credo's
// recommended application pattern is constructor injection.
func (app *App) MustResolve[T any]() T

// ResolveAll returns all instances bound to interface I via BindMany,
// preserving bind order. When no bindings exist, it returns []I{}.
func (app *App) ResolveAll[I any]() ([]I, error)

// MustResolveAll panics if the collection cannot be resolved.
func (app *App) MustResolveAll[I any]() []I
```

`Resolve` is admitted only after `Finalize()`: before it, the call panics and no constructor runs; after a failed `Finalize` it returns the `Finalize` error. It remains public afterwards and can be called at runtime. Credo intentionally keeps that low-level capability available, but does not make it part of the preferred request-time programming model. There is no `Context.Resolve` helper, and the recommended approach remains wiring dependencies through constructors during bootstrap. Stop hooks (`OnStop`) and components do not resolve during the drain: they take their dependencies at registration — a hook by capture, a component through its constructor. `OnStart` hooks run after `Finalize` and before traffic, and may resolve. A resolve while the App is stopping is logged at Debug (`credo: Resolve during drain`) because an in-flight request may still legitimately resolve.

Construction completes exactly once per singleton with one terminal result — value, error or panic — shared by the first, concurrent and later callers. A constructor panic is recovered and returned as `*credo.DIPanicError` (`Type`, `Phase == DIPanicConstruction`, the original `Value`, and the `Stack` captured on the panicking goroutine; it unwraps an error-valued panic), and a constructor that depends on it fails through its parameter. There is no automatic retry, and `MustResolve` panics with that error as its payload rather than the original value. Construction failures are diagnostics only: they are not teardown failures. Once the container is closing, callers receive an error wrapping `credo.ErrDIClosed` even for a cached instance or a recorded failure; an instance created after closing is still owned and cleaned up by the container. A constructor's parameters are resolved before it is invoked, so a constructor whose parameter meets closing never runs; a constructor body already running is not forced to fail — the container rejects new resolution and withholds results, nothing more.

### `[]I` Constructor Injection

When a constructor parameter is `[]I` and there is no direct registration for that exact slice type, the container resolves it from `BindMany[I, T]` bindings:

```go
func NewSenderRegistry(senders []Sender) *SenderRegistry {
    return &SenderRegistry{senders: senders}
}
```

Rules:

- only slices whose element type is an interface use this collection path
- direct registrations for `[]I` take precedence over `BindMany`
- binding order is preserved
- when no bindings exist, the constructor receives `[]I{}`
- `Resolve[[]I]` remains a normal direct lookup; collection semantics are exposed through `ResolveAll[I]` and constructor injection only

### Shutdown

```go
// root package (credo)

// Component is a value whose teardown the App owns. The context carries the
// drain deadline; a Shutdown that has not returned by then is abandoned and
// the components it depends on stay open.
type Component interface {
    Shutdown(ctx context.Context) error
}
```

Which singletons are components is stated under [Registration Options](#registration-options); a value or constructor handed to `app.Manage` is a component that is not a binding. The capabilities and the tiers are the lifecycle spec's ([component types](lifecycle.md#component-types)), and so is the drain sequence ([shutdown in tiers](lifecycle.md#shutdown-in-tiers)); this section states what the container contributes to the drain.

Ordering follows the static graph built at `Finalize` (or extracted from the frozen registrations for a never-finalized container): constructor parameters, aliases mapped to their canonical singleton, and `BindMany` collection edges. Both constructor and value bindings are vertices, so a `Service` closes before the DB it received even when that DB came from `ProvideValue`; dependencies hidden inside pre-built values are opaque. Holders that share a [resource identity](#resource-identity-and-teardown) form one vertex. Rules:

- The ingress tier stops while resolutions are still admitted; its components that no edge orders stop concurrently with each other. The internal tier enters closing first and stops one component at a time, in reverse dependency order with reverse registration order as the tie-break; it also stops an ingress component built after the ingress tier ran.
- A built value that is no component, and a borrowed resource, is transparent: edges pass through it, so ordering survives intermediate services, and it retires without calling user code. Aliases and collection members are the same vertex as their singleton and are attempted once.
- Never-started and terminally failed constructors are inactive and contribute no live edges. The HTTP drain proceeds past its deadline with requests still running, so a construction can be in flight when closing begins. A build still in progress is a pending vertex: it blocks its dependencies while independent ready vertices proceed; a failed build retires and releases them, a successful one becomes an ordinary vertex.
- Each eligible teardown — `Shutdown`, or `Close` for a `credo.Closer()` binding — runs on a helper goroutine with the shared drain context; the container waits for completion or context end, preferring a completed result at the boundary. A returned error or recovered panic completes the attempt as a failure and releases the vertex's dependencies. A wait that ends because the context ended is not completion: the component is abandoned, its dependencies stay open (kept open by it), and the same instance is never retried nor given extra budget. A later result or panic is logged without mutating the returned report.
- Once the context ends, no further teardown starts and the remaining vertices are reported with their actual state and blockers; there is no out-of-order fallback. A tier's pass also ends when none of its vertices is ready or pending; a vertex still held by a live consumer when the drain ends — the dependency of an abandoned consumer, or a graph inconsistency — is reported as kept open with its blockers, never closed out of order.
- An instance whose construction completes after the context ended gets one separate, fixed five-second best-effort cleanup attempt on its own goroutine (result-or-deadline, a completed result wins the boundary) — for a component its binding's type shows and for one found only on its built value alike — unless another holder still holds its resource, that resource was already torn down, or a holder borrows it. The attempt is logged, never reported, not configurable, never granted to ordinary entries, and does not guarantee dependency order; it is best effort because the process may exit once `Shutdown` returns.
- Recovery wraps every invocation, construction and `Start` included. A recovered teardown panic is a completed failed attempt reported as `*credo.DIPanicError` with phase `DIPanicShutdown` or `DIPanicLateCleanup`; a `Start` panic has phase `DIPanicStart`.

The App reports a failed or incomplete drain as one `*credo.LifecycleError` ([lifecycle spec](lifecycle.md#the-lifecycle-error)); a constructor that panicked before the drain is a diagnostic, not a teardown failure, so it alone does not make `Shutdown` return an error. A second teardown of the same container returns an error wrapping `ErrDIClosed`.

### Concurrency and Lifecycle

- **Registration** (`Provide`, `ProvideValue`, `Alias`, `BindMany`, `Manage`): sequential, from the goroutine that builds the App, before it runs; not safe for concurrent use and not serialized against `Finalize`. A call out of phase panics.
- **`Has`**: a non-mutating point-in-time observation made during the same sequential bootstrap; it reserves nothing.
- **`Finalize`**: Idempotent via `sync.Once`. Safe to call from multiple goroutines but typically called once at startup.
- **`Resolve` / `MustResolve` / `ResolveAll` / `MustResolveAll`**: Panic before Finalize. Safe for concurrent use afterwards: per-singleton completion ensures each constructor runs exactly once and every waiter receives the same terminal result (value, error or `DIPanicError`). Different singletons resolve concurrently without blocking each other.
- **Teardown**: the internal tier enters closing atomically with respect to resolution admission and result delivery, then runs the dependency-ordered pass described above; the ingress tier, before it, still admits resolutions. Ordinary attempts share the drain context; only late construction gets the separate five-second attempt.

## Sequential Bootstrap

Bootstrap is sequential. Registration calls — DI bindings, feature mounts, component registrations, routes, hooks and renderers — come from the goroutine that builds the App, before it runs, and are not safe for concurrent use. Each call checks once that it is in its phase, and a call out of phase panics; since bootstrap is sequential, nothing can prepare the App between that check and the call's publication. Registration promises nothing about concurrent calls, so an application that registers from several goroutines serializes them itself. The running App is concurrent: `Resolve` after `Finalize` ([Concurrency and Lifecycle](#concurrency-and-lifecycle)) and the preparation that concurrent first `ServeHTTP` calls share ([lifecycle spec](lifecycle.md#preparation)) are safe for concurrent use.

### Documented Order

The order gives every DI, feature and component registration its place, so that a bootstrap written by following it meets no ordering panic:

1. **Configuration** — `credo.New()`, which loads configuration, or `credo.New(credo.WithRawConfig(rawCfg))`.
2. **`Provide`** — the application's bindings (`Provide`, `ProvideValue`, `Alias`, `BindMany`), with their registration options.
3. **Feature mounts and component registrations**, in any order among themselves — `UseI18n`, `UseHealth`, workers, stores, `Manage`.
4. **`Finalize`** — graph validation; handle its error.
5. **`Resolve`, routes and anything built from a resolved value** — a hook, a renderer or a readiness check included.
6. **`Run`** — or `RunContext`, `ServeContext`, or `App.Start` for an App that an external server serves.

The runnable `Example_bootstrapOrder` (`example_bootstrap_test.go`) follows this order with stand-in types.

```go
func main() {
    app, err := credo.New() // 1. configuration
    if err != nil {
        log.Fatal(err)
    }

    // 2. Provide
    app.Provide[*sqldb.DB](NewDatabase) // built and pinged by the start phase
    app.Provide[*PaymentClient](NewPaymentClient)
    app.Provide[*OrderService](NewOrderService)
    app.Provide[*OrderController](NewOrderController)
    app.Provide[*OutboxRelay](NewOutboxRelay)

    // 3. Feature mounts and component registrations, in any order
    app.UseI18n(credo.I18nConfig{Dir: "locales"})
    app.UseHealth()
    store.Register[*sqldb.DB](app)
    worker.Use(app).ContinuousProvided[*OutboxRelay]("outbox-relay")
    app.Manage(NewSearchIndexer, credo.Named("search-indexer"))

    // 4. Finalize
    if err := app.Finalize(); err != nil {
        log.Fatal(err)
    }

    // 5. Resolve, routes, and what is built from resolved values
    orders := app.MustResolve[*OrderController]()
    app.POST("/orders", orders.Create)
    payments := app.MustResolve[*PaymentClient]()
    app.AddReadinessCheck("payments", credo.HealthCheckFunc(payments.Ping))

    // 6. Run
    if err := app.Run(); err != nil {
        log.Fatal(err)
    }
}
```

There is no resolve-then-provide step: no DI or component registration follows `Finalize`. A value that needs a resolved dependency and I/O to exist — a provider whose settings are read from a database table, for example — is a component whose constructor takes its dependencies and whose `Start` reads the settings and opens its backend ([lifecycle spec](lifecycle.md#the-start-walk)). HTTP setup that uses no resolved value — global middleware, the [built-in HTTP features](http-features.md), a route that calls no service — may come at any point before preparation. `credo.New()`, routes and `Run()` stay the minimal shape: an App that resolves nothing before serving needs no explicit `Finalize`, since the serve entry points call it.

### Three Error Phases

The package documentation's "Panics and Errors" names three phases:

| Phase | What it reports | How |
| --- | --- | --- |
| Registration | Misuse known at the call site ([Registration Misuse](#registration-misuse)) | Panics with a message naming the call, the phase and the remedy; performs no I/O |
| `Finalize` | What only the whole graph reveals ([Finalize and Container Lifecycle](#finalize-and-container-lifecycle)) | Returns the errors joined in registration order, with the same text on every run |
| `Start` | I/O: a component's `Start`, a store's ping ([store spec](store.md#registration)), a catalog read ([i18n spec](i18n.md#registration-and-start-phase)), a constructor that the start walk runs | Returns the error; the start rolls back ([lifecycle spec](lifecycle.md#a-failed-start)) |

`Provide`, `ProvideValue`, `Alias`, `BindMany`, `Manage`, `store.Register`, `UseI18n` and the worker registrations therefore return no error, and leave their I/O to the start phase: a store's ping, i18n's catalog reads and a provided worker's construction.

## Design Decisions

- The DI surface, `credo.Infra`, the registration options and several instances of one type: [ADR-004](../adr/004-dependency-injection-and-infra.md).
- The bootstrap phases, resolution after `Finalize`, terminal construction, closing admission and the dependency-ordered, bounded teardown: [ADR-022](../adr/022-bootstrap-and-di-ownership.md).
- Components, tiers, ownership and resource identity: [ADR-024](../adr/024-lifecycle-components.md).

## samber/do Adaptation Scope

The dependency bookkeeping and the teardown diagnostics were reviewed against [samber/do v2.1.0](https://github.com/samber/do/releases/tag/v2.1.0), tag commit `f0d927f`, and adapted to Credo's static graph; that reference is not evidence of the upstream revision Credo originally copied. NOTICES records an adapted upstream revision only when provenance supports it; otherwise it says `upstream revision unknown` and records a known adaptation date separately, and the copyright and license notices are retained.

### What We Keep

| samber/do source | Credo file | Notes |
| --- | --- | --- |
| Container core + type registry | `internal/di/container.go` | Adapted to typed constructors (not `func(Injector)` signature) |
| Shutdown interface (`credo.Component`) | Root package `component.go`; `internal/di/shutdown.go` | Dependency-ordered shutdown (ready queue; do v2.1.0's dependent bookkeeping adapted to Credo's static graph) |

### What We Cut

| samber/do feature | Reason |
| --- | --- |
| `func(do.Injector) (T, error)` constructor signature | Service locator inside constructor --- replaced with typed params, so a constructor's dependencies are visible in its signature |
| `do.MustInvoke[T](i)` inside constructors | Same --- service locator pattern |
| Named services (`do.ProvideNamed`) | Type-based resolution is sufficient. Named variants add complexity |
| `do.As[T]()` aliasing | `Alias[I, T]()` keeps registration and aliasing explicit and separate |
| `do.Package()` grouping | Module functions that take `*credo.App` and register their bindings |
| Scope tree (parent/child) | Not needed --- single Singleton lifecycle, no request-scoped containers |
| Out-of-order shutdown fallback, automatic build retries | A blocked dependency is reported with its blockers instead of being closed underneath a live consumer; a failed constructor is terminal |
| Transient services, implicit aliases, a global container, container cloning, per-invocation frame bookkeeping | Singleton is the only lifecycle, an alias is declared with `Alias`, each App owns its container, and the static graph orders what per-call bookkeeping would track |

## File Layout

The container is `internal/di` — registration, resolution and validation, and the component registry the start walk and the drain run on — and its public API is the root package's `di.go`, `component.go`, `infra.go`, `dierrors.go` and `lifecycle_error.go`.

## Examples

The bootstrap as a whole is the [Documented Order](#documented-order) example; aliasing and collections are shown with their API above.

### Service with Infra

```go
type UserStore interface {
    Create(ctx context.Context, input CreateUserInput) (*User, error)
}

type UserService struct {
    infra credo.Infra
    repo  UserStore
}

func NewUserService(infra credo.Infra, repo UserStore) *UserService {
    return &UserService{infra: infra, repo: repo}
}

func (s *UserService) Create(ctx context.Context, input CreateUserInput) (*User, error) {
    s.infra.Logger.InfoContext(ctx, "creating user", "email", input.Email)

    user, err := s.repo.Create(ctx, input)
    if err != nil {
        return nil, fmt.Errorf("create user: %w", err)
    }
    return user, nil
}
```

### Testing --- Direct Construction

```go
type fakeUserStore struct{}

func (fakeUserStore) Create(_ context.Context, input CreateUserInput) (*User, error) {
    return &User{Email: input.Email}, nil
}

func TestUserService_Create(t *testing.T) {
    svc := NewUserService(credo.Infra{Logger: slog.New(slog.DiscardHandler)}, fakeUserStore{})

    user, err := svc.Create(t.Context(), CreateUserInput{Email: "alice@example.com"})
    if err != nil {
        t.Fatalf("Create: %v", err)
    }
    if user.Email != "alice@example.com" {
        t.Fatalf("Email = %q, want %q", user.Email, "alice@example.com")
    }
}
```

Infra is a plain struct --- construct it directly, no ceremony. Set the Logger your test needs to observe; a zero-value `Infra{}` leaves Logger nil (the default-logger fallback applies only to container-produced Infra).
