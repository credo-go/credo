# DI Container & credo.Infra Spec

**Status:** Implemented (beta) **Package:** `internal/di/`; root `di.go`, `component.go`, `infra.go`, `dierrors.go`, `lifecycle_error.go` **ADR:** [ADR-004](../adr/004-dependency-injection-and-infra.md), [ADR-022](../adr/022-bootstrap-and-di-ownership.md), [ADR-024](../adr/024-lifecycle-components.md) **Guide:** [Dependency injection guide](../guides/dependency-injection.md) **Sources:** samber/do (MIT)

This spec states the DI container: its surface, registration and its options, `Finalize` and the three error phases, resolution, resource identity, the container's part in teardown, `credo.Infra`, and the [sequential bootstrap](#sequential-bootstrap) with its documented order. Preparation, the start phase and the drain sequence are the [lifecycle spec](lifecycle.md)'s.

## Overview

Credo's DI system has two parts: a generics-based container in `internal/di`, adapted from samber/do and exposed as methods on `*App` — `app.Provide(constructor)`, `app.BindMany[I, T]()`, `app.Resolve[T]()`, `app.ResolveAll[I]()` — with no code generation; and `credo.Infra`, a framework-managed carrier defined in the root package, which the container produces for a constructor parameter while it stays visible in the constructor's signature. The container is Credo-specific, not a standalone DI library ([ADR-004](../adr/004-dependency-injection-and-infra.md)).

## Goals

> **Current until v0.25.0 (W6):** the third goal's explicit `Finalize` step holds until W6; with it, the first resolving call finalizes the container.

1. **Generics over reflection**: `Resolve[T]` is fully compile-time typed. `Provide`'s `constructor` parameter is necessarily `any` (Go cannot express "a function with arbitrary parameters"), so its signature is checked at the `Provide` call and the dependency graph at `Finalize`. Reflection stays on the cold path ([Registration](#registration-root-package)).
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

### Log Keys

The framework's records use four keys to say where a record comes from or what it is about ([ADR-004](../adr/004-dependency-injection-and-infra.md#one-key-names-an-application-part)):

| Key | Names | Written by |
| --- | --- | --- |
| `component` | the application part a record comes from or is about | the lifecycle's records; the scoped `Infra` of a component's call ([The Context Carrier](#the-context-carrier)); a constructor's `Infra` logger; `app.NewInfra(name)`; a worker's records |
| `module` | the framework package that wrote the record | every framework module (`module=websocket`, `module=sqldb`, `module=worker`); the `http.Server.ErrorLog` bridge (`module=net/http`) |
| `request_id` | the request | the request's logger under `UseRequestID` |
| `connection_id` | a WebSocket connection | the WebSocket server's connection records |

When the container produces `Infra` for a constructor, its Logger is scoped with the name of the binding the constructor builds — its type as the lifecycle prints it, or its `credo.ComponentName` name:

```go
// Container internally does, for app.Provide(users.NewService):
infra.Logger = appLogger.With("component", "*users.Service")

// and for app.Provide(NewPrimaryDB, credo.ComponentName("primary")):
infra.Logger = appLogger.With("component", "primary")
```

`appLogger` is the App's logger — the one `credo.WithLogger` configures, else the framework default — captured at `credo.New`; a `*slog.Logger` binding in the container is never consulted. `*users.Service` and `*orders.Service` therefore stay apart, and a component's records from its constructor and the lifecycle's records about it share one key and value. For code outside the DI container — middleware factories, startup helpers, manually created workers — `app.NewInfra(name)` returns an Infra whose Logger is scoped with `component=name`; it is nil-safe, falling back to the framework default logger when the App has no logger configured:

```go
rbacMW := rbac.NewMiddleware(app.NewInfra("rbac")) // logs component=rbac
```

A stable event identity for framework records is the observability release's; it adds a key and renames none.

### The Context Carrier

```go
// InfraFrom returns the Infra the App scoped to the component whose call
// received ctx, or the zero Infra outside such a call.
func InfraFrom(ctx context.Context) Infra
```

Every call the App makes on a component — `Start`, `Ready`, `Shutdown`, the `Close` of a `credo.CloseOnShutdown()` binding, and the `OnStart` and `OnStop` hooks — receives a context carrying the component's scoped `Infra`: the App's logger with `component=<name>`, the component's unit name, which its readiness entry, a `LifecycleError` and the lifecycle's records show and `credo.ComponentName` sets. The root installs it around each call `internal/di` makes, through an internal hook, and around each readiness check the aggregation runs. Outside such a call `InfraFrom` returns the zero `Infra`, whose logger falls back to the framework default. `context.WithoutCancel` keeps values, so work derived from the `Start` context sees the same `Infra`. Only framework infrastructure travels in the context, never an application service. Request code logs through `c.Logger()`, and a constructor takes `credo.Infra`; the [lifecycle spec](lifecycle.md) states the carrier's place in the start walk and the drain.

A framework module whose value is built outside the App and started by it takes an optional logger in its own configuration (`websocket.Config.Logger`; `sqldb.WithLogger(l)` for the database's `Start`, [store spec](store.md#logging)) and otherwise uses the App's, in this precedence: the logger given to the module, unscoped; then `InfraFrom(ctx).Logger`; then the framework default. Every record a module writes carries `module=<name>`, added with `Logger.With`, whichever logger it goes to. The App's own subsystems — the `Use*` features and the worker registry — log to the App's logger and are outside the rule; `httpclient` takes a `*slog.Logger` and does not import the root package.

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

> **Current until v0.25.0 (W6):** the block's "after Finalize only" and the sentence after it ("`Resolve` becomes available") hold until W6; with it, the first resolving call finalizes ([Finalize and Container Lifecycle](#finalize-and-container-lifecycle)).

```text
// Registration — returns nothing, panics on misuse
app.Provide(constructor, options...)
app.ProvideValue[T](value, options...)
app.Alias[I, T](options...)
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

No integration monitors or shuts down one value while DI resolves another: the framework's registrations name bindings, not values — a component is its binding (Registration Options), a bound database included ([store spec](store.md#lifecycle-of-a-bound-database)). A worker's constructor is a unit of the component registry that is never a binding, and a value it builds that a binding holds too defers to that binding ([The Holder of Record](#the-holder-of-record)).

### Registration Misuse

Registration performs no I/O and panics on misuse known at the call site, with a message naming the call, the phase and the remedy:

| Misuse | Panics at |
| --- | --- |
| A constructor that is nil, not a function, or returns anything but `T` or `(T, error)` for some type `T` | `Provide` |
| A second binding of the same type without `credo.Override()` | `Provide`, `ProvideValue` |
| `Alias` or `BindMany` whose types do not fit (non-interface I, unregistered or non-implementing T, existing alias or pair) | `Alias`, `BindMany` |
| A misused registration option (Registration Options) | `Provide`, `ProvideValue`, `Manage` |
| Any DI or component registration after `Finalize` — `Provide`, `ProvideValue`, `Alias`, `BindMany`, `Manage`, worker registrations | The call |
| Any registration after the App is prepared or shut down | The call |
| `Resolve`, `MustResolve`, `ResolveAll` or `MustResolveAll` before `Finalize` | The call |

> **Pending v0.25.0 (W6):** not implemented yet; until W6 ships, the table's last row holds and a registration after `Finalize` names only the phase.

A resolving call before `Finalize` finalizes the container instead of panicking, so the table's last row goes. A registration after the freeze — `Provide`, `ProvideValue`, `Alias`, `BindMany`, `credo.Override()` on either binding call, `Manage`, `Workers().Continuous`, `Workers().Scheduled` and `testutil.WithOverride` — panics naming the call, what finalized the container with its site and callers, and the remedy ([Finalize and Container Lifecycle](#finalize-and-container-lifecycle)).

`Provide` panics on a constructor that is nil, not a function, or returns anything but `T` or `(T, error)` for some type `T`, naming the forms accepted. A direct binding of a type an `Alias` names, without `credo.Override()`, panics as a duplicate naming the alias and `credo.Override()`, and `Alias` panics on any option other than `credo.Override()`. A `ProvideValue` binding whose type hides its value's `Start`, `Ready` or declared ingress tier, when no earlier registration of the same resource plans it, panics at the call, naming the binding, the methods its type hides and the remedy (Registration Options).

`credo.ComponentName` on a binding that is not a component, and `credo.OptionalReadiness()` on a registration that plans no readiness entry, panic at the call (Registration Options).

Registration calls are not safe for concurrent use; bootstrap is sequential ([Sequential Bootstrap](#sequential-bootstrap)). `Finalize` returns an error, because what it reports needs the whole graph (Finalize and Container Lifecycle).

### Registration Options

**Which bindings are components.** A singleton the App owns is a component when it has `Shutdown(ctx) error` — its binding's type shows the method at registration, or its built value has it — because ownership, not the view its consumers take, decides the teardown; a component found only on its built value is internal. `Start`, `Ready` and the tier are planned from the binding's type before any constructor runs ([lifecycle spec](lifecycle.md#component-types)), and [the start walk](lifecycle.md#the-start-walk) states what that means for a value behind an interface.

**A capability the binding's type hides is refused.** A component whose binding's type hides its value's `Start` or `Ready` — and the ingress tier its type declares ([lifecycle spec](lifecycle.md#component-types)) — is refused instead of being run without it: `ProvideValue` panics at the call, and a value a constructor builds fails the start once it is built, where the container already finds a `Shutdown` the type hides. It is refused only when no other registration that holds the same resource ([Resource Identity](#resource-identity-and-teardown)) plans that capability, since the App calls it through that holder; `ProvideValue` decides against the registrations made before it, so `ProvideValue(db)` followed by `ProvideValue[Querier](db)` is accepted and the reverse order panics (`hiddenCapabilityErrorLocked`, `internal/di/unit.go`). The message names the binding, the methods its type hides and the remedy: bind the concrete type, and bind the interface with `Alias`. `credo.Borrowed()` is exempt, since the App neither starts nor stops the value. A hidden `Shutdown` is not refused: it still makes the value a component whose teardown follows it, so a binding whose type shows `Start` but hides the `Shutdown` its value has is a component, and the App starts it; a value with `Start` and no `Shutdown` at all is not a component and is never started. The values a worker constructor and a constructor handed to `Manage` build follow the same rule.

`Provide` and `ProvideValue` take registration options of type `credo.RegistrationOption`. Each is checked at the call, and misuse — an option the call does not accept, a zero option, an option given twice — panics there:

| Option | Accepted on | Effect | Panics |
| --- | --- | --- | --- |
| `credo.Ingress()` | `Provide`, `ProvideValue`, `Manage`, `OnStart`, `OnStop` | Places the component, or the hook, in the ingress tier | On a binding that is not a component at registration — whose type shows no `Shutdown` and that carries no `credo.CloseOnShutdown()` — since the tier is planned before the value exists |
| `credo.Borrowed()` | `ProvideValue` only | Keeps the binding and the value's readiness contribution; starting and shutting down stay with the caller | On `Provide`; beside `credo.CloseOnShutdown()`; at the later call, or the later construction, of a holder of the same resource that claims its teardown (Resource Identity) |
| `credo.CloseOnShutdown()` | `Provide`, `ProvideValue` | Makes a binding whose type has a `Close` method a component that the App closes after its consumers | On a type with none of the three `Close` shapes; on a type that is already a component; beside `credo.Borrowed()` |
| `credo.Override()` | `Provide`, `ProvideValue`, `Alias` | Replaces an earlier binding of the same type, or an alias of it, before `Finalize`; on `Alias`, points the interface at another type | When there is neither an earlier binding nor an alias of the type; after `Finalize` |
| `credo.ComponentName(name)` | `Provide`, `ProvideValue`, `Manage` | Names the component in logs, reports and readiness (default: its type as the lifecycle prints it, such as `*sqldb.DB`) | On a name the health-name rules refuse — empty, padded, with a control character, or with the reserved `credo.` prefix — as `credo: <call>: credo.ComponentName("<name>"): <reason>`; on a name another component has, except on registrations of one resource, which give it one name (Resource Identity), as `a component named "x" is already registered, as <type>; give each component its own name with credo.ComponentName` — for a `Provide` constructor, whose value is not known at the call, at the start; on a binding that is not a component at registration, since the name is planned before the value exists |
| `credo.OptionalReadiness()` | `Provide`, `ProvideValue`, `Manage` | Reports the component in `/ready` without letting it gate readiness ([health spec](health.md)) | On a registration that plans no readiness entry — a type without `Ready`, or one that is neither a component nor borrowed; for a constructor handed to `Manage` whose value defers to its resource's holder of record, at the start (Resource Identity) |

The name `credo.ComponentName(name)` gives is a label, not a DI key: the container keys bindings by type, and a second value of one type under another name is a duplicate binding ([Several Instances of One Type](../adr/004-dependency-injection-and-infra.md#several-instances-of-one-type)). When either registration of a duplicate binding carries the option, the panic says so beside the remedy it always gives (`duplicateError`, `internal/di/provide.go`):

```text
credo: App.ProvideValue[*sqldb.DB]: *sqldb.DB is already registered; credo.ComponentName("replica") names a component in logs, /ready and lifecycle errors and is not a DI key; bind a second instance under a wrapper type of its own
```

A duplicate binding where neither registration carries the option keeps the message without the middle clause.

- **`Borrowed`** is for a pool that two Apps in one process share, or a fixture a test suite reuses across the Apps it builds ([ADR-024](../adr/024-lifecycle-components.md#ownership-and-registration-options) gives why resource identity cannot express it). It is accepted on a binding whose type does not show `Shutdown`: that is how a caller keeps the teardown of a value that has the method behind an interface.
- **`CloseOnShutdown`** accepts the three shapes `Close` takes, checked from the binding's type: `Close() error` (`*sql.DB`, a Redis client), `Close()` (`*pgxpool.Pool`) and `Close(ctx) error` (`*pgx.Conn`), the last receiving the drain deadline. A `Close` that blocks past the deadline is abandoned like any `Shutdown`, and its dependencies stay open. The option decides its binding's teardown: a built value that also has `Shutdown` is closed once, with `Close`. `Close` is never discovered without the option.
- **`Override`** is strict: without an earlier binding of the type it panics, so an override that no longer matches the wiring fails instead of adding a binding nothing resolves. Without the option a duplicate still panics. The replaced binding may be a constructor or a value; since constructors run only after `Finalize`, an override never supersedes a built instance. A value that an override replaces never becomes the App's: the App neither starts nor shuts it down, and a replaced `ProvideValue` value stays with the caller who created it.
- **`testutil.WithOverride[T](v)`** builds on `credo.Override()` and is equally strict: without an earlier binding of T, wired by the test or by `testutil.WithWiring`, it panics. Adding a binding is `WithWiring`'s job.

**An alias is a binding** for the duplicate and override rules (`bindLocked` and `checkOverrideLocked` in `internal/di/provide.go` consult the aliases as well as the registrations). A direct binding of an aliased type with `credo.Override()` — `ProvideValue[I](fake, credo.Override())` or `testutil.WithOverride[I](fake)` after `Alias[I, T]()` — replaces the alias, and `Alias[I, T2](credo.Override())` points `I` at `T2`. The type the overridden alias pointed at stays registered until `Finalize`, which drops it before validating the graph when no other alias, `BindMany` entry or constructor parameter — of a binding, a worker or a constructor handed to `Manage` — reaches it, and removes its holder records. A dropped type is never built or started, and `Resolve` of it returns an error naming the override that dropped it; with a constructor that also takes `T`, or another alias or a `BindMany` entry naming it, `T` is kept, built and started.

`app.Manage(v, opts...)` adds a component that is not a binding — a value with `Shutdown`, or a constructor `func(deps...) C` or `func(deps...) (C, error)` that the start walk builds — and accepts the options the table gives it; the [lifecycle spec](lifecycle.md#appmanagev-opts) states its rules, and a child App handed to its parent's `Manage` runs its start phase in the parent's ([mounted Apps](lifecycle.md#mounted-apps)).

A teardown that is neither `Shutdown` nor one of the `Close` shapes — a client's `Disconnect(ctx)`, a `Drain` that must be waited for — belongs to a type that embeds the client and implements `Shutdown`, registered as the binding itself, so its constructor's parameters give the teardown's own dependencies their edges. There is no teardown-function option, and a constructor returning `(T, func(context.Context) error, error)` is rejected at `Provide`; [ADR-024](../adr/024-lifecycle-components.md#rejected-alternatives) records why, and [defers the three-value form](../adr/024-lifecycle-components.md#consequences) until a type proves costly to wrap.

### Resource Identity and Teardown

**One resource, one teardown.** Within one App the component registry keys components by resource identity:

1. the token the value's `ResourceIdentity()` method returns, the `credo.ResourceIdentifier` capability — `*sqldb.DB` returns itself, and a wrapper that embeds it inherits the method;
2. otherwise the value itself, when it is comparable — a pointer, or a struct over one;
3. otherwise its binding alone.

Values that share an identity — one pointer under several bindings, the interface view an adapter constructor returns, a wrapper and the handle it embeds, two wrappers over one handle, a value that a constructor returns after `Manage` received it — are one component. It is torn down once, when the last holder retires, so the consumers of every holder stop first, and started once, through the first holder the start walk reaches whose binding plans a `Start`. A holder that obtains the resource through one of its consumers does not keep that consumer open: the consumer stops first, then the resource. Only that route is dropped — the edge a holder which reached the resource through another holder contributes, when it closes a cycle; a dependency that a holder which reached the resource on its own contributes always holds. Component values that are not comparable are their bindings' own, each torn down once.

**The holders agree on the teardown.** The application chooses the teardown, not the order of construction. The holders that have a teardown agree on its kind — `Shutdown`, or `Close` through `credo.CloseOnShutdown()` — and a disagreement panics at the later `ProvideValue` call when both values are given, and otherwise fails the construction of the holder built later. The message names both bindings and both remedies: drop `credo.CloseOnShutdown()`, or bind the holder that has `Shutdown` under a type that shows `Close` and not `Shutdown`, with `credo.CloseOnShutdown()`. The resource's holder of record runs it, whichever is built first ([The Holder of Record](#the-holder-of-record)). A holder without a teardown takes no part in the choice, and the resource still waits for its consumers.

Kinds are all the registry can compare: Go does not tell a `Shutdown` that a wrapper inherits from one it declares. The choice therefore rests on a contract — only a value without state of its own to release may share an identity. A wrapper that embeds a handle which identifies itself inherits that identity, so a wrapper that releases state of its own holds the handle in a named field instead; a wrapper that forwards an identity promises that it keeps none.

**Misuse.** A `ResourceIdentity()` that panics, or returns a token that is nil, not comparable or not equal to itself, panics at `ProvideValue` and `Manage` and fails a constructor's construction. `Manage` of a value whose resource a `ProvideValue` binding holds panics, and so does a resource handed to `Manage` twice. A value whose construction the registry fails after the constructor returned it — an unusable identity, an internal component found on its value that depends on an ingress one — is released by the drain when no other holder carries it; one refused for a holder conflict is the shared resource and is left to its owner.

**Borrowed resources** are started and torn down through none of their holders: a holder whose type shows `Start` does not start a resource another holder borrows. A holder that claims the teardown — a component bound with `ProvideValue`, a value handed to `Manage`, a binding with `credo.CloseOnShutdown()` — disagrees with `credo.Borrowed()` on the owner and panics at the later call or fails its construction. A value that a constructor builds over a borrowed resource without `credo.CloseOnShutdown()` stays the caller's even when it has `Shutdown`, since a `Shutdown` found on a value is no claim.

#### The Holder of Record

**A registration is the holder of record.** A binding, or a value handed to `Manage`, that holds a resource is its holder of record — the first registered of them when several do, preferring one that is a component by its type (`holderOfRecord`, `internal/di/resource.go`). A value a constructor builds that is not a binding — a worker constructor's, or one from a constructor handed to `Manage` — claims nothing: when it holds a resource a registration holds, it defers to that registration, adds no readiness entry of its own and never runs the teardown, so `/ready` shows the registration's entry with its name and its options, and the teardown carries the registration's name and tier, even when the constructor was registered first. Without such a registration the first registered holder is the holder of record, so two worker constructors that return one value they did not create give one entry and one teardown. A borrowed resource is torn down by none of its holders. A constructor handed to `Manage` whose value embeds a handle bound with `ProvideValue`, with or without `credo.Borrowed()`, therefore starts, and its value shares the handle's one teardown, after the consumers of all of them; a value handed to `Manage` is still refused beside a `ProvideValue` binding or another `Manage` of one resource (Misuse).

**A value that defers takes no registration options.** A `Manage` call's `credo.ComponentName`, `credo.Ingress()` and `credo.OptionalReadiness()` apply to the value it builds, and a value that defers has no entry and no teardown of its own for them to shape. A constructor handed to `Manage` with any of the three whose value defers fails the start once the value is built, naming the holder it defers to and the remedies: move the option to the holder's registration, or, when the value has a readiness or a teardown of its own, hold the handle in a named field, so that it is a resource of its own. The message speaks the application's terms:

```text
*Relay shares the resource of the ProvideValue binding of *sqldb.DB, whose readiness and shutdown follow that binding; give credo.ComponentName to that binding, or hold the *sqldb.DB in a named field
```

**Constructors that are not bindings.** A constructor handed to `Manage` or to a worker registration is a unit of the component registry, not a binding: its parameters are resolved from the container and give it its edges, a missing parameter fails `Finalize` with its path, the start walk builds it after those dependencies have started, and a constructor error, a panic or a nil value fails the start. Nothing resolves its value. Each such registration calls its constructor separately, so two registrations get two values unless the constructor returns one it did not create; then the registry tears that value down once. A worker constructor's registration adds two units, the built value `worker-value:<name>` and the loop `worker:<name>`, with an edge from the loop to the value (`AddEdge`, `internal/di/manage.go`; [worker spec](worker.md#constructors)). A built value keeps its own name — `worker-value:<name>`, or the name of its `Manage` call — under which its build is reported, a missing parameter's path (`di: missing dependency: worker-value:orphan → *app.Queue (not registered)`) and a failed construction included, while a resource it defers to keeps the registration's name for its entry and its teardown. Inside `internal/di` the managed flag is two flags, "not a binding" and "claims what it holds"; `manage.go` does not claim for a constructor, and the teardown snapshot (`teardownHolderLocked`, `shutdown.go`) takes a resource's holder of record from those flags.

#### Registrations of One Resource Agree

**On the tier.** Registrations that hold one resource — bindings, and a value handed to `Manage`, that are components by their type — agree on its tier. Beside the owner and kind checks of `internal/di/resource.go`, the admission of whichever value becomes known second fails, naming both registrations, their tiers and the remedies: give them one tier, or, when one of them has a teardown of its own to run in its tier, hold the handle in a named field. That is a panic at registration when the value is handed in (`ProvideValue`, `Manage`) and a failed start when a binding's constructor builds it. A borrowed resource is exempt, since the App neither starts nor tears it down. A value a constructor builds for a worker or for `Manage` takes no part: one that defers carries no options, and one that is the holder of record holds a resource no registration holds. Such a built value is internal; one with its own teardown that depends on an ingress component fails `Finalize`, or its build when the teardown is found only on the value, with the path and the remedy of splitting it, or, when that ingress component is the binding whose type the constructor returns, of binding it without `credo.Ingress()`.

**One resource, one readiness entry.** A resource has one readiness entry, whose `Ready` is asked: the first of its registrations whose type shows `Ready` gives it, or, when no registration holds the resource, its holder of record (`ReadinessEntries`, `internal/di/readiness.go`). The registrations that hold it agree on the options that shape that entry: `credo.OptionalReadiness()` is on every registration that plans an entry for the resource or on none, and the names given with `credo.ComponentName` are one name, which the entry and the teardown carry — the holder of record's type name without one. The same admission checks them, a borrowed resource included, since readiness reports it; a disagreement panics at registration for a value handed in and fails the start for a value a binding's constructor builds, naming both registrations, the option and the remedies: the same options on both, or a named field. A wrapper with readiness of its own, one that overrides the `Ready` it embeds, holds the handle in a named field.

The order in which components stop — the tiers, the shared deadline, the abandonment of a teardown that misses it — is the [lifecycle spec](lifecycle.md#shutdown-in-tiers)'s; the container contributes the static graph that orders it ([Shutdown](#shutdown)).

### Registration (root package)

```go
// Provide registers a constructor. The binding's type is the constructor's
// first result type: func(deps...) T or func(deps...) (T, error). The
// constructor's parameters are resolved from the container. Lifecycle:
// Singleton. It accepts credo.Ingress(), credo.CloseOnShutdown() and
// credo.Override(), and panics on misuse (Registration Misuse).
func (app *App) Provide(constructor any, opts ...RegistrationOption)

// ProvideValue registers a pre-built value as a Singleton. It accepts
// credo.Ingress(), credo.Borrowed(), credo.CloseOnShutdown() and
// credo.Override(), and panics on misuse, like Provide.
func (app *App) ProvideValue[T any](value T, opts ...RegistrationOption)

// Has reports whether T is registered (directly or through Alias). It never
// constructs and reserves nothing; the result is a snapshot of the
// registrations made so far.
func (app *App) Has[T any]() bool
```

A binding is replaced with `credo.Override()` (Registration Options), never after the fact.

The `constructor` parameter accepts any function whose parameters are resolvable types; its first return value is the binding's type:

```go
// All valid constructor signatures:
func NewUserRepo(db *sql.DB) *UserRepo
func NewUserRepo(db *sql.DB) (*UserRepo, error)
func NewUserRepo(infra credo.Infra, db *sql.DB) (*UserRepo, error)
```

Constructor parameter types are inspected via reflection at registration time and matched against registered types. The first construction of each singleton invokes the constructor with `reflect.Value.Call`. Resolving a built singleton afterwards is a map lookup with no `reflect.Value` call; `ResolveAll` assembles its slice with reflection on each call.

A constructor declared to return a concrete type binds that type, and the interfaces it serves are bound with `Alias`; one declared to return an interface binds the interface, and its value's `Start` and `Ready` must be shown by that interface (Registration Options). `ProvideValue[T]`, `Resolve[T]`, `Alias[I, T]` and `BindMany[I, T]` keep their type parameters. A message that names a binding keeps the form `App.Provide[*Mailer]`, the call with its binding's type, since a constructor written as a function literal has no useful name.

```go
app.Provide(NewPgUserRepo)           // binds *PgUserRepo
app.Alias[UserRepo, *PgUserRepo]()   // UserRepo resolves the same singleton
app.Provide(NewMailer)               // func NewMailer(...) (Mailer, error) binds the interface Mailer
```

Because `constructor` is typed `any`, a signature mistake (wrong return type, not a function) panics at the `Provide` call --- not at compile time. There is no closure-based factory alternative: a constructor that captures `app` and calls `Resolve` inside its body is unsupported. Such a call is invisible to `Finalize` graph validation, cycle detection and dependency-ordered shutdown, and since constructors run only after Finalize it cannot be scheduled against the graph. Declare every dependency as a constructor parameter.

### Interface Aliasing

```go
// Alias creates an alias so that Resolve[I] returns the instance registered
// for concrete type T. I must be an interface, T must implement I, and T
// must already be registered. It accepts credo.Override(), its one option,
// which points an already aliased I at T, and panics on misuse and on any
// other option.
func (app *App) Alias[I, T any](opts ...RegistrationOption)
```

Alias enables resolving by interface without requiring the constructor to return the interface type:

```go
// Register the concrete type.
app.Provide(NewPgUserRepo)

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

An alias is a binding of `I` for the duplicate and override rules: with `credo.Override()`, the rule against an existing alias gives way and `I` is pointed at `T`, and a later direct binding of `I` without `credo.Override()` panics as a duplicate naming the alias. What an override leaves unreached is dropped at `Finalize` ([Registration Options](#registration-options)).

### Ordered Interface Collections

```go
// BindMany adds concrete type T to the ordered collection for interface I.
// I must be an interface, T must be registered already, T must be concrete,
// and T must implement I. It panics on misuse.
func (app *App) BindMany[I, T any]()
```

`BindMany[I, T]` is collection wiring, not default resolution. It does not change `Resolve[I]`; it only affects `ResolveAll[I]` and constructor injection of `[]I`.

```go
app.Provide(NewEmailSender)
app.Provide(NewInAppSender)

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

> **Current until v0.25.0 (W6):** the phase list, the `Finalize` godoc and the paragraph after the example state the current rule — `Resolve` and `ResolveAll` panic before `Finalize`, and a composition root that resolves before serving must call `Finalize` explicitly; W6 replaces them with [The First Resolve Finalizes](#the-first-resolve-finalizes).

The container moves through registration, `Finalize`, runtime and closing:

1. **Registration** --- `Provide` and `ProvideValue` with their registration options, `Alias`, `BindMany` and `Manage` are allowed; `Has` observes without mutating or reserving. `Resolve` and `ResolveAll` panic --- constructors never run in this phase; the worker registrations do not read the container.
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
app.Provide(NewDB)
app.Provide(NewPgUserRepo)
app.Alias[UserRepo, *PgUserRepo]()
app.Provide(NewUserService)

// Finalize phase --- freeze + validate
if err := app.Finalize(); err != nil {
    log.Fatal(err) // e.g. "di: missing dependency: *UserService → *Mailer (not registered); ..."
}

// Runtime phase --- safe to resolve
userSvc := app.MustResolve[*UserService]()

// These panic after Finalize ("called after Finalize; make every
// registration before Finalize, ..."):
// app.Provide(NewFoo)
// app.Alias[Bar, *Baz]()
// app.BindMany[Qux, *Baz]()
```

If `app.Finalize()` is not called explicitly, the serve entry points and `App.Start` call it implicitly as the first step of preparation, and a direct `ServeHTTP` call does the same on the first request. A composition root that resolves services before serving must call it explicitly first.

#### The First Resolve Finalizes

> **Pending v0.25.0 (W6):** not implemented yet; until W6 ships, a resolving call before `Finalize` panics with "called before Finalize; call Finalize first", and a registration after the freeze reports "called after Finalize; make every registration before Finalize".

**The trigger.** The first `Resolve`, `ResolveAll`, `MustResolve` or `MustResolveAll` finalizes the container if it is not finalized yet, then resolves. `app.Finalize()` stays public as an optional early check, and the serve entry points, `App.Start` and the first `ServeHTTP` call still finalize an App that resolved nothing. `Has`, `GetConfig`, `MustGetConfig` and `ConfigExists` never finalize. Finalizing stays DI-only: routes registered after the first `Resolve`, such as a controller's handlers, are accepted until the App is prepared, as after an explicit `Finalize`.

**Failures.** A `Finalize` failure is the triggering call's error, and `MustResolve` panics with the whole joined report. Every later `Resolve` returns that error, as after a failed explicit `Finalize`. Concurrent first resolvers finalize once, under the container lock.

**The trigger's record.** The container records what finalized it and where: `Finalize`, the first resolving call with its type, or the App's preparation with its entry point.

- The site is the first frame outside Credo's packages, as route registration records it (`sourceloc.go`); `testutil` frames are skipped too, so a test's trigger points at the test.
- The first `ServeHTTP` call has no site, since its caller is `net/http`.
- Up to three callers of the site are recorded with it, skipping Credo's packages, `testutil` and the standard library: when a helper library makes the first `Resolve`, the site names the helper's line, and its callers name the code that called it.

**The message.** A registration after the freeze names the call, the trigger with its site and callers, and the remedy. This applies to `Provide`, `ProvideValue`, `Alias`, `BindMany`, `credo.Override()` on either binding call, `Manage`, `Workers().Continuous`, `Workers().Scheduled` and `testutil.WithOverride`. One template serves every trigger:

```text
credo: App.Provide[*Mailer]: called after the container was finalized by Resolve[*UserController] at main.go:42; register before that call
credo: Workers.Continuous("outbox-relay"): called after the container was finalized by Finalize at main.go:40; register before that call
credo: App.Manage[*Cache]: called after the container was finalized by the first ServeHTTP call; register before the App serves
credo: App.Provide[*Mailer]: called after the container was finalized by Resolve[*Hub] at wiring/wiring.go:17 (called from main.go:31, main.go:12); register before that call
```

The message for a registration after a shutdown began is unchanged: "called after shutdown began; registration ends when the App shuts down".

`Finalize` returns what only the whole graph reveals, joined in registration order:

- each missing dependency, with its whole path from the entry-point registration that needs it — `di: missing dependency: *app.OrderService → *app.PaymentClient → *http.Client (not registered); provide *http.Client before Finalize` — where an alias hop reads `app.Gateway (alias of *app.Stripe)` and a collection member `*app.Audit (in []app.Plugin)`;
- each circular dependency (A → B → A), through constructor parameters, aliases and collections alike, listing its members and asking to remove one of those constructor parameters;
- each constructor parameter of type `context.Context`: constructors run at startup, not per request;
- an internal component that depends on an ingress one, directly or through bindings that are not components, with the path and both remedies — declare the dependent ingress, or split the ingress component so that what internal components use is an internal part.

The report is deterministic: the same wiring yields the same text on every run. A consumer registered earlier is reported earlier, its parameters left to right, and every cycle's text starts at the member that was registered first. Registration order does not otherwise matter: a constructor may depend on a type registered after it. `Alias` and `BindMany` check their types when the binding is made, so `Finalize` does not check them again. Dependencies inside pre-built values are opaque to the container and take no part in validation or shutdown ordering. A component found only on its built value is internal and is checked when it is built: such a component that depends on an ingress one fails its construction with the same message as the `Finalize` finding.

Before it validates the graph, `Finalize` drops the type an overridden alias pointed at when nothing else reaches it, as [Registration Options](#registration-options) states, so a dropped type contributes no finding and no edge.

### Resolution (root package)

> **Current until v0.25.0 (W6):** the paragraph after the block states the current rule, under which `Resolve` before `Finalize` panics; with W6 the first resolving call finalizes ([The First Resolve Finalizes](#the-first-resolve-finalizes)).

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

> **Pending v0.25.0 (W6):** not implemented yet; until W6 ships, the previous paragraph's rule holds.

`Resolve` finalizes the container on its first call when it is not finalized yet, so no constructor runs before the graph is validated; a failed `Finalize` is the call's error and every later call's ([The First Resolve Finalizes](#the-first-resolve-finalizes)). After it, `MustResolve` and `MustResolveAll` panic with the `Finalize` report or a constructor's failure — the startup misuse the panic rule covers at the composition root.

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
- Each eligible teardown — `Shutdown`, or `Close` for a `credo.CloseOnShutdown()` binding — runs on a helper goroutine with the shared drain context; the container waits for completion or context end, preferring a completed result at the boundary. A returned error or recovered panic completes the attempt as a failure and releases the vertex's dependencies. A wait that ends because the context ended is not completion: the component is abandoned, its dependencies stay open (kept open by it), and the same instance is never retried nor given extra budget. A later result or panic is logged without mutating the returned report.
- Once the context ends, no further teardown starts and the remaining vertices are reported with their actual state and blockers; there is no out-of-order fallback. A tier's pass also ends when none of its vertices is ready or pending; a vertex still held by a live consumer when the drain ends — the dependency of an abandoned consumer, or a graph inconsistency — is reported as kept open with its blockers, never closed out of order.
- An instance whose construction completes after the context ended gets one separate, fixed five-second best-effort cleanup attempt on its own goroutine (result-or-deadline, a completed result wins the boundary) — for a component its binding's type shows and for one found only on its built value alike — unless another holder still holds its resource, that resource was already torn down, or a holder borrows it. The attempt is logged, never reported, not configurable, never granted to ordinary entries, and does not guarantee dependency order; it is best effort because the process may exit once `Shutdown` returns.
- Recovery wraps every invocation, construction and `Start` included. A recovered teardown panic is a completed failed attempt reported as `*credo.DIPanicError` with phase `DIPanicShutdown` or `DIPanicLateCleanup`; a `Start` panic has phase `DIPanicStart`.

The App reports a failed or incomplete drain as one `*credo.LifecycleError` ([lifecycle spec](lifecycle.md#the-lifecycle-error)); a constructor that panicked before the drain is a diagnostic, not a teardown failure, so it alone does not make `Shutdown` return an error. A second teardown of the same container returns an error wrapping `ErrDIClosed`.

### Concurrency and Lifecycle

> **Current until v0.25.0 (W6):** the list's "Panic before Finalize" holds until W6; with it, the first resolving call finalizes, and concurrent first resolvers finalize once, under the container lock.

- **Registration** (`Provide`, `ProvideValue`, `Alias`, `BindMany`, `Manage`): sequential, from the goroutine that builds the App, before it runs; not safe for concurrent use and not serialized against `Finalize`. A call out of phase panics.
- **`Has`**: a non-mutating point-in-time observation made during the same sequential bootstrap; it reserves nothing.
- **`Finalize`**: Idempotent via `sync.Once`. Safe to call from multiple goroutines but typically called once at startup.
- **`Resolve` / `MustResolve` / `ResolveAll` / `MustResolveAll`**: Panic before Finalize. Safe for concurrent use afterwards: per-singleton completion ensures each constructor runs exactly once and every waiter receives the same terminal result (value, error or `DIPanicError`). Different singletons resolve concurrently without blocking each other.
- **Teardown**: the internal tier enters closing atomically with respect to resolution admission and result delivery, then runs the dependency-ordered pass described above; the ingress tier, before it, still admits resolutions. Ordinary attempts share the drain context; only late construction gets the separate five-second attempt.

## Sequential Bootstrap

Bootstrap is sequential. Registration calls — DI bindings, feature mounts, component registrations, routes, hooks and renderers — come from the goroutine that builds the App, before it runs, and are not safe for concurrent use. Each call checks once that it is in its phase, and a call out of phase panics; since bootstrap is sequential, nothing can prepare the App between that check and the call's publication. Registration promises nothing about concurrent calls, so an application that registers from several goroutines serializes them itself. The running App is concurrent: `Resolve` after `Finalize` ([Concurrency and Lifecycle](#concurrency-and-lifecycle)) and the preparation that concurrent first `ServeHTTP` calls share ([lifecycle spec](lifecycle.md#preparation)) are safe for concurrent use.

### Documented Order

> **Current until v0.25.0 (W6):** the six-step order, its example and the paragraph after it state the current composition root, with `Finalize` as a required step; W6 replaces them with [The Order Without Finalize](#the-order-without-finalize).

The order gives every DI, feature and component registration its place, so that a bootstrap written by following it meets no ordering panic:

1. **Configuration** — `credo.New()`, which loads configuration, or `credo.New(credo.WithRawConfig(rawCfg))`.
2. **`Provide`** — the application's bindings (`Provide`, `ProvideValue`, `Alias`, `BindMany`), with their registration options.
3. **Feature mounts and component registrations**, in any order among themselves — `UseI18n`, `UseHealth`, workers, `Manage`.
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
    app.Provide(NewDatabase) // built and pinged by the start phase
    app.Provide(NewPaymentClient)
    app.Provide(NewOrderService)
    app.Provide(NewOrderController)

    // 3. Feature mounts and component registrations, in any order
    app.UseI18n(credo.I18nConfig{Dir: "locales"})
    app.UseHealth()
    app.Workers().Continuous("outbox-relay", NewOutboxRelay) // a constructor, not a binding
    app.Manage(NewSearchIndexer, credo.ComponentName("search-indexer"))

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

### The Order Without Finalize

> **Pending v0.25.0 (W6):** not implemented yet; until W6 ships, the order above holds. The example below uses the registration forms of v0.25.0 — a bound database, `app.Workers()`, `Provide` without a type argument — which the other work items of the release introduce before W6.

The order has five steps, and `Finalize` is no longer one of them, since the first resolving call finalizes the container ([The First Resolve Finalizes](#the-first-resolve-finalizes)):

1. **Configuration** — `credo.New()`, or `credo.New(credo.WithRawConfig(rawCfg))`.
2. **`Provide`** — the application's bindings (`Provide`, `ProvideValue`, `Alias`, `BindMany`), with their registration options.
3. **Feature mounts and component registrations**, in any order among themselves — `UseI18n`, `UseHealth`, workers, `Manage`.
4. **`Resolve`, routes and anything built from a resolved value** — a hook, a renderer or a readiness check included. The first resolving call ends the registration window; an explicit `Finalize` before it is an optional early check whose error the composition root handles.
5. **`Run`** — or `RunContext`, `ServeContext`, or `App.Start` for an App that an external server serves.

```go
func main() {
    app, err := credo.New() // 1. configuration
    if err != nil {
        log.Fatal(err)
    }

    db, err := sqldb.Open(dbConfig) // opens the pool; no I/O yet
    if err != nil {
        log.Fatal(err)
    }

    // 2. Provide
    app.ProvideValue(db) // pinged by the start phase, reported by /ready
    app.Provide(NewPaymentClient)
    app.Provide(NewOrderController)

    // 3. Feature mounts and component registrations, in any order
    app.UseHealth()
    ws := websocket.New()
    app.Manage(ws)
    app.Workers().Continuous("outbox-relay", NewOutboxRelay)

    // 4. Resolve, routes, and what is built from resolved values
    orders := app.MustResolve[*OrderController]() // finalizes the container
    app.POST("/orders", orders.Create)
    app.GET("/ws", ws.Handler(chatHandler))

    // 5. Run
    if err := app.Run(); err != nil {
        log.Fatal(err)
    }
}
```

A registration after the first resolving call panics naming that call and its line. A value that needs a resolved dependency and I/O to exist is still a component whose `Start` does the work, not a late registration.

### Three Error Phases

The package documentation's "Panics and Errors" names three phases:

| Phase | What it reports | How |
| --- | --- | --- |
| Registration | Misuse known at the call site ([Registration Misuse](#registration-misuse)) | Panics with a message naming the call, the phase and the remedy; performs no I/O |
| `Finalize` | What only the whole graph reveals ([Finalize and Container Lifecycle](#finalize-and-container-lifecycle)) | Returns the errors joined in registration order, with the same text on every run |
| `Start` | I/O: a component's `Start`, a database's ping among them ([store spec](store.md#lifecycle-of-a-bound-database)), a catalog read ([i18n spec](i18n.md#registration-and-start-phase)), a constructor that the start walk runs | Returns the error; the start rolls back ([lifecycle spec](lifecycle.md#a-failed-start)) |

`Provide`, `ProvideValue`, `Alias`, `BindMany`, `Manage`, `UseI18n` and the worker registrations therefore return no error, and leave their I/O to the start phase: a database's ping in its `Start`, i18n's catalog reads and a worker constructor's call.

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
