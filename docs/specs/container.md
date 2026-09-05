# DI Container & credo.Infra Spec

**Status**: Approved **Implementation**: `internal/di/` (private), Root package API (`app.Provide[T]`, `app.Resolve[T]`, `app.BindMany[I, T]`, `app.ResolveAll[I]`) **Sources**: samber/do (MIT) **Depends on**: --- **ADRs**: [004-dependency-injection-and-infra](../adr/004-dependency-injection-and-infra.md) **Roadmap**: [`TODO.md` Phase 2.1, 2.2](../../TODO.md)

---

## Canonical Source

Implementation-level details for Credo's dependency injection system are defined in this file. Other documents should keep only high-level references and link here. The phase, ownership and teardown rules — post-Finalize resolution, registration-time adoption, ownership-transferring `Replace`, dependency-ordered shutdown, closing admission and the teardown/panic diagnostics — are specified in the [bootstrap and DI lifecycle contract](bootstrap-and-di-lifecycle.md) ([ADR-022](../adr/022-bootstrap-and-di-ownership.md)); this document states the same behavior in API terms.

---

## Overview

Credo's DI system consists of two parts:

1. **`internal/di/`** --- A generics-based container adapted from samber/do. Uses Go generics for type-safe registration and resolution. No code generation. `Seal()` freezes and validates the graph at startup.

2. **`credo.Infra`** --- A framework-managed infrastructure carrier defined in the root package. Carries the per-service Logger (metrics/tracing carriers return with the Phase 3.5 observability release). Produced automatically by the container when seen as a constructor parameter, but still visible in the service constructor signature.

The container lives in `internal/di` because it is Credo-specific --- not a standalone DI library. The public API is exposed through root package generic functions such as `app.Provide[T](constructor)`, `app.BindMany[I, T]()`, `app.Resolve[T]()`, and `app.ResolveAll[I]()`.

---

## Goals

1. **Generics over reflection**: `Resolve[T]` is fully compile-time typed. `Provide[T]`'s `constructor` parameter is necessarily `any` (Go cannot express "a function with arbitrary parameters returning T"), so its signature is checked at registration time --- mistakes surface as an immediate error from `Provide`, and the dependency graph is validated at `Finalize`. Reflection is used at registration time to inspect constructor signatures and once per singleton during first construction (`reflect.Value.Call`). Subsequent resolves of the same singleton are pure cache lookups --- zero reflection.
2. **Framework-managed infrastructure, explicit boundary**: `credo.Infra` delivers cross-cutting infrastructure (currently the Logger) as a visible constructor parameter. The framework produces it by convention; services do not receive hidden field population or struct-tag injection.
3. **Composition Root first**: The container is primarily used at startup (`main()` or `App.Run()`). Credo's recommended pattern is constructor injection and bootstrap-time wiring: registrations first, then `Finalize`, then resolution. Runtime `Resolve` calls remain available for advanced use cases, but the framework does not provide request-time DI helpers such as `Context.Resolve`.
4. **Always available with default-logger fallback**: `credo.Infra` works without configuring a Logger. An unset Logger falls back to the framework-owned stderr logger. Tests never need container setup for infrastructure access.
5. **Single lifecycle: Singleton**: One instance per container lifetime. This covers web framework needs without unnecessary complexity.
6. **Ordered interface collections**: Components can depend on `[]I` when startup wiring needs an ordered set of implementations (hooks, senders, subscribers) without introducing named services or keyed lookups.

---

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

Users cannot add fields to `Infra`. The framework defines the set.

### Observability Carriers (deferred to Phase 3.5)

Earlier drafts defined speculative `MeterProvider`/`TracerProvider` (plus `Counter`/`Histogram`/`Span`) interfaces in the root package, carried as `Infra.Metrics`/`Infra.Tracer` with noop fallbacks. They were removed before v1 (2026-06-11): no real adapter existed, and shipping placeholder interfaces would have frozen an untested API surface. The observability release (Phase 3.5, aligned with the v1 / Go 1.27 window) reintroduces metrics and tracing carriers designed against real OpenTelemetry and Prometheus adapters; `Infra`'s keyed-literal guard (`_ struct{}`) lets those fields land without breaking existing constructors.

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

Every log line from `OrderService` automatically includes `"service"="OrderService"` without the developer passing it manually.

### Non-DI Infra

For components outside the DI container (middleware factories, startup helpers, manually created workers), use `app.NewInfra(name)`:

```go
rbacMW := rbac.NewMiddleware(app.NewInfra("RBACMiddleware"))
app.GlobalMiddleware(rbacMW)
```

The returned Infra has a Logger scoped with `"service"=name`. `NewInfra` is nil-safe: when the app has no logger configured it falls back to the framework default logger. For DI-managed services, Infra is injected automatically — prefer DI when possible.

### Config Is Not in Infra

Config is NOT part of `credo.Infra`. See [ADR-005](../adr/005-configuration-architecture.md) for rationale:

- Each service may need a different config section (`*OrderConfig` vs `*DatabaseConfig`)
- Config is an immutable startup-time snapshot; the Logger is runtime infrastructure
- Typed config via DI is more type-safe than a universal accessor

### What Goes Where

| Dependency | Access | Rationale |
| --- | --- | --- |
| DB connections | Constructor param | Behavior-defining, must be mockable |
| Repositories | Constructor param | Behavior-defining |
| Domain services | Constructor param | Behavior-defining |
| Typed config struct | Constructor param | Structural, read at startup |
| Logger | `infra.Logger` (via `credo.Infra`) | Ubiquitous, cross-cutting |
| Metrics / Tracer | _Phase 3.5_ | Carriers return with the observability release |
| i18n | App (`UseI18n`) | [ADR-013](../adr/013-internationalization.md) |
| Request ID | Context | `ctx.RequestID()` --- per-request |

---

## Injection Models

The container supports two injection models. Developers choose per-service.

### Model 1: Infra as First Parameter

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

1. Parameter type is `credo.Infra` --- **Model 1**: produce Infra specially (scoped Logger, default-logger fallback). Convention: place `credo.Infra` as the first parameter.
2. Otherwise --- **Pure constructor injection**: parameter resolved normally from the container.

This is a type check on the cold path --- no extra reflection beyond what the container already does for constructor inspection.

---

## API Surface

### Registration (root package)

```go
// Provide registers a constructor for type T. The constructor's parameters
// are resolved from the container automatically. Lifecycle: Singleton.
func (app *App) Provide[T any](constructor any) error

// MustProvide is like Provide but panics on error. Intended for use at
// startup (Composition Root) where a failed registration is fatal.
func (app *App) MustProvide[T any](constructor any)

// ProvideValue registers a pre-built value as a Singleton.
func (app *App) ProvideValue[T any](value T) error

// ProvideProtectedValue registers a pre-built Singleton whose direct binding
// cannot later be overwritten by Replace.
func (app *App) ProvideProtectedValue[T any](value T) error

// ProtectBinding prevents Replace from overwriting an existing direct binding.
// With one expected value it atomically compares the already-resolved,
// comparable singleton and protects only when that value still matches.
func (app *App) ProtectBinding[T any](expected ...T) error

// CanProvideValue performs a non-mutating point-in-time check for a frozen
// container or an existing direct T registration. It does not reserve T.
func (app *App) CanProvideValue[T any]() error

// MustProvideValue is like ProvideValue but panics on error.
func (app *App) MustProvideValue[T any](value T)

// Has reports whether T is registered (directly or through Alias). It never
// constructs, adopts or protects; the result is a snapshot, not a reservation.
func (app *App) Has[T any]() bool

// AdoptValue reads the pre-built value bound to T during registration,
// validates it and atomically compare-and-protects that same binding. It never
// runs a constructor: a constructor binding is rejected with an explanatory
// error. Validation failure leaves the binding unprotected and repairable.
func (app *App) AdoptValue[T any](validate func(T) error) (T, error)

// Replace overwrites an ordinary direct binding with a pre-built value and
// returns the superseded, already-created instance (existed == true) whose
// cleanup now belongs to the caller. An unbuilt constructor yields zero, false.
// It returns an error when the existing binding is protected or the container
// is finalized.
func (app *App) Replace[T any](value T) (old T, existed bool, err error)

// MustReplace is like Replace but panics on error.
func (app *App) MustReplace[T any](value T) (old T, existed bool)
```

`CanProvideValue` is a preflight, not a success guarantee: another goroutine can register T or finalize the container before the real publication. The final `ProvideValue` or `ProvideProtectedValue` call remains authoritative.

Protected bindings are a low-level integration facility for a DI value coupled to external lifecycle, health, or registration state. `Replace` rejecting such a binding prevents DI from resolving a different value than the integration continues to monitor or shut down. Protection does not itself create lifecycle ownership, aliases, health checks, or collection membership. Ordinary application/test bindings should remain override-friendly with `ProvideValue`.

`ProtectBinding[T]()` protects an existing direct binding without resolving T; this no-argument form is idempotent. `ProtectBinding[T](expected)` is the CAS-style compare-and-protect form for a singleton the caller already holds. Comparison and protection are atomic with respect to `Replace`: the expected value must already be created, comparable, and equal to the current singleton. An unresolved, non-comparable, or changed value returns an error without adding protection. More than one expected value is rejected, and both forms must run before Finalize.

`AdoptValue[T](validate)` is the registration-time read that framework integrations (`store.Register`, `worker.Register`) use to take ownership of a value the composition root supplied ahead of them: read the existing pre-built binding → validate → atomic compare-and-protect of the accepted instance. Protection follows successful validation, never the read itself, so a rejected instance (a typed-nil Registry, for example) stays replaceable and repairable through `Replace`. A `Replace` or `Finalize` that wins during validation makes the adoption fail instead of protecting or returning a stale instance. Constructors run only after Finalize, so a constructor binding is rejected without being invoked; there is no general early-resolve exemption. `Has[T]` is the non-adopting existence query for optional wiring (for example, "is a worker pool registered yet?"); it never constructs, protects or claims that an instance is usable.

`Replace[T]` transfers ownership explicitly. On success the container owns the new value and stops tracking the superseded one: an already-created instance (a pre-built value) is returned with `existed == true` and its cleanup — including any `Shutdowner` implementation — becomes the caller's responsibility, and a Warn log names the type when that instance implements `Shutdowner`. A superseded constructor binding that never ran yields the zero value and `false`; Replace never constructs an old provider merely to return it. A rejected replacement changes neither the binding nor ownership. `MustReplace` returns the same previous-instance information and panics on error.

The `constructor` parameter accepts any function whose parameters are resolvable types and whose first return value is `T`:

```go
// All valid constructor signatures:
func NewUserRepo(db *sql.DB) *UserRepo
func NewUserRepo(db *sql.DB) (*UserRepo, error)
func NewUserRepo(infra credo.Infra, db *sql.DB) (*UserRepo, error)
```

Constructor parameter types are inspected via reflection at registration time and matched against registered types. The first construction of each singleton also uses `reflect.Value.Call` to invoke the constructor. Subsequent resolves of the same singleton are pure cache lookups with zero reflection cost.

Because `constructor` is typed `any`, a signature mistake (wrong return type, not a function) is reported as an error by `Provide` at registration time --- not by the compiler. There is no closure-based factory alternative: a constructor that captures `app` and calls `Resolve` inside its body is unsupported. Such a call is invisible to `Finalize` graph validation, cycle detection and dependency-ordered shutdown, and since constructors run only after Finalize it cannot be scheduled against the graph. Declare every dependency as a constructor parameter.

**`context.Context` as a constructor parameter is always an error.** Constructors run at startup, not per-request. If `Seal()` encounters a constructor with a `context.Context` parameter, it reports a clear error.

### Interface Aliasing

```go
// Alias creates an alias so that Resolve[I] returns the instance registered
// for concrete type T. I must be an interface, T must implement I, and T
// must already be registered.
func (app *App) Alias[I, T any]() error

// MustAlias is like Alias but panics on error.
func (app *App) MustAlias[I, T any]()
```

Alias enables resolving by interface without requiring the constructor to return the interface type:

```go
// Register the concrete type.
app.MustProvide[*PgUserRepo](NewPgUserRepo)

// Alias interface to concrete type.
app.MustAlias[UserRepo, *PgUserRepo]()

// Now resolving by interface returns the *PgUserRepo singleton.
repo := app.MustResolve[UserRepo]()
```

Contract rules enforced by `Alias`:

- `I` must be an interface type
- `T` must implement `I`
- `T` must already be registered via `Provide` or `ProvideValue`
- `I` must not already have a direct registration or existing alias
- Container must not be frozen (`Finalize` must not have been called yet)

### Ordered Interface Collections

```go
// BindMany adds concrete type T to the ordered collection for interface I.
// I must be an interface, T must be registered already, T must be concrete,
// and T must implement I.
func (app *App) BindMany[I, T any]() error

// MustBindMany is like BindMany but panics on error.
func (app *App) MustBindMany[I, T any]()
```

`BindMany[I, T]` is collection wiring, not default resolution. It does not change `Resolve[I]`; it only affects `ResolveAll[I]` and constructor injection of `[]I`.

```go
app.MustProvide[*EmailSender](NewEmailSender)
app.MustProvide[*InAppSender](NewInAppSender)

app.MustBindMany[Sender, *EmailSender]()
app.MustBindMany[Sender, *InAppSender]()
```

Contract rules enforced by `BindMany`:

- `I` must be an interface type
- `T` must be a concrete type (not an interface)
- `T` must implement `I`
- `T` must already be registered via `Provide` or `ProvideValue`
- The same `(I, T)` pair must not already exist
- Container must not be frozen (`Finalize` must not have been called yet)

### Finalize and Container Lifecycle

The container has four phases:

1. **Bootstrap** --- `Provide`, `ProvideValue`, `ProvideProtectedValue`, `ProtectBinding`, `AdoptValue`, `Replace`, `Alias`, and `BindMany` are allowed; `Has` and `CanProvideValue` observe without mutating or reserving. `Resolve` and `ResolveAll` are rejected with a "not finalized" error --- constructors never run in this phase, and the framework's registration helpers (`store.Register`, `worker.Register`) read pre-built values only through `AdoptValue`.
2. **Finalize** --- `app.Finalize()` freezes the container (internally calling `Seal()`) and validates the dependency graph. After Finalize, every write above returns a "container is frozen" error. If validation fails, subsequent `Resolve` and `ResolveAll` calls return the finalize error. Finalize is DI-only: routes, hooks, renderers and other HTTP registrations stay open until the App prepares to serve, so controllers built from resolved services can still be wired afterwards (see the [lifecycle spec](lifecycle.md#preparation)).
3. **Runtime** --- `Resolve` creates and caches singletons on demand. The dependency graph is guaranteed valid. `app.Run()`, `app.RunContext()`, `app.ServeContext()` and the first direct `ServeHTTP` call Finalize implicitly.
4. **Closing** --- `Container.Shutdown` atomically freezes the container and enters closing. New `Resolve` calls, cached results included, return an error wrapping `credo.ErrDIClosed`; builds already admitted are tracked for cleanup even when their caller's delivery loses to closing. The closing check precedes the failed-Finalize check, so teardown rejection wins even after a failed Seal. A never-finalized container can still be shut down (bootstrap teardown): the cleanup graph is derived from the frozen registrations and current instances without requiring successful validation.

**Concurrency**: During bootstrap, mutation is normally performed sequentially in `main()` or setup functions before `Finalize()`. `CanProvideValue` and `Has` are deliberately point-in-time; their results do not reserve T against a concurrent publication or Finalize. `AdoptValue`, `Replace` and `Finalize` are serialized against each other so an adoption cannot protect a binding that a concurrent replacement changed.

```go
// Finalize freezes the container and validates the dependency graph.
// After Finalize, no more Provide, ProvideValue, ProvideProtectedValue,
// ProtectBinding, AdoptValue, Replace, Alias, or BindMany calls are allowed,
// and Resolve becomes available.
// Finalize is idempotent --- subsequent calls return the same result via sync.Once.
//
// Finalize is side-effect-free: it does not instantiate singletons or perform I/O.
// It only freezes the container (via Seal) and validates the graph. It does
// not close HTTP registration.
//
// The serve entry points and the first ServeHTTP call Finalize implicitly.
// Explicit, error-checked Finalize is the recommended composition-root step
// between registration and the first Resolve.
func (app *App) Finalize() error
```

```go
// Registration phase
app.MustProvide[*sql.DB](NewDB)
app.MustProvide[*UserRepo](NewUserRepo)
app.MustProvide[*UserService](NewUserService)
app.MustAlias[UserRepo, *PgUserRepo]()

// Finalize phase --- freeze + validate
if err := app.Finalize(); err != nil {
    log.Fatal(err) // "missing dependency: *UserRepo required by *UserService"
}

// Runtime phase --- safe to resolve
userSvc := app.MustResolve[*UserService]()

// These would fail after Finalize:
// app.Provide[*Foo](NewFoo)  // error: container is frozen
// app.Alias[Bar, *Baz]()       // error: container is frozen
// app.BindMany[Qux, *Baz]()    // error: container is frozen
```

If `app.Finalize()` is not called explicitly, the serve entry points call it implicitly as the first step of preparation, and a direct `ServeHTTP` call does the same on the first request. A composition root that resolves services before serving must call it explicitly first.

Duplicate registration of the same type returns an error.

Circular dependencies (A -> B -> A) are detected during Finalize and produce a clear error listing the cycle. Dependencies inside pre-built values are opaque to the container and take no part in validation or shutdown ordering.

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

`Resolve` is admitted only after `Finalize()`: before it, the call returns a "not finalized" error and no constructor runs. It remains public afterwards and can be called at runtime. Credo intentionally keeps that low-level capability available, but does not make it part of the preferred request-time programming model. There is no `Context.Resolve` helper, and the recommended approach remains wiring dependencies through constructors during bootstrap. Lifecycle hooks (`OnPreDrain`, `OnDrain`, `OnShutdown`) must not resolve; they capture their dependencies at registration time. A resolve while the App is stopping is logged at Debug (`credo: Resolve during drain`) because an in-flight request may still legitimately resolve.

Construction completes exactly once per singleton with one terminal result — value, error or panic — shared by the first, concurrent and later callers. A constructor panic is recovered and returned as `*credo.DIPanicError` (`Type`, `Phase == DIPanicConstruction`, the original `Value`, and the `Stack` captured on the panicking goroutine; it unwraps an error-valued panic). There is no automatic retry, and `MustResolve` panics with that error as its payload rather than the original value. Construction failures are diagnostics only: they are not teardown failures. Once the container is closing, callers receive an error wrapping `credo.ErrDIClosed` even for a cached instance or a recorded failure; an instance created after closing is still owned and cleaned up by the container.

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

// Shutdowner is implemented by services that need cleanup on shutdown.
// The context carries a deadline from the application's graceful shutdown
// timeout; implementations should respect ctx.Done() for timely cleanup.
type Shutdowner interface {
    Shutdown(ctx context.Context) error
}
```

```go
// internal/di/shutdown.go

// Shutdown freezes the container, enters closing, and tears down live
// singletons in dependency order — consumers before the singletons they were
// constructed from — using a Kahn ready queue over the static graph. Returns
// nil on full success or a *DIShutdownError snapshot.
func (c *Container) Shutdown(ctx context.Context) error
```

Ordering follows the static graph built at Seal (or extracted from the frozen registrations for a never-sealed container): constructor parameters, aliases mapped to their canonical singleton, and `BindMany` collection edges. Both constructor and value bindings are vertices, so a `Service` closes before the DB it received even when that DB came from `ProvideValue`; dependencies hidden inside pre-built values are opaque. The ready queue is ordered by remaining live dependents first and reverse registration index second, so registration order still decides where the graph does not. Rules:

- A built instance without `Shutdowner` stays a vertex until its dependents retire, then retires without calling user code, preserving order through intermediate services. Aliases and collection members are the same vertex as their singleton and are attempted once.
- Never-started and terminally failed constructors are inactive and contribute no live edges. A build still in progress is a pending vertex: it blocks its dependencies while independent ready vertices proceed; a failed build retires and releases them, a successful one becomes an ordinary vertex.
- Each eligible `Shutdowner` is invoked sequentially, in ready order, on a helper goroutine with the shared shutdown context; the container waits for completion or context end, preferring a completed result at the boundary. A returned error or recovered panic completes the attempt as a failure and releases the vertex's dependencies. A wait that ends because the context ended is not completion: the vertex stays incomplete, its dependencies stay blocked, and the same instance is never retried nor given extra budget. A later result or panic is logged without mutating the returned report.
- When no vertex is ready the pass waits on pending completion or the context. Once the context ends, no further ordinary attempt is admitted and the remaining vertices are reported with their actual state and blockers; there is no out-of-order fallback.
- An instance whose construction completes after the shutdown context ended gets one separate, fixed five-second best-effort cleanup attempt on its own goroutine (result-or-deadline, a completed result wins the boundary). The attempt is observed and logged; it is not configurable, never granted to ordinary entries, and does not guarantee dependency order.
- Recovery wraps every invocation, construction included. A recovered shutdown panic is a completed failed attempt reported as `*credo.DIPanicError` with phase `DIPanicShutdown` or `DIPanicLateCleanup`.

On failure or incompleteness `Shutdown` returns `*credo.DIShutdownError`, an immutable snapshot in registration order with one `DIShutdownEntry` per vertex: `Type`, `State` (`DIShutdownState`: retired without a Shutdowner, completed, failed, panicked, still running, construction pending, blocked with its live dependents, ready but unattempted, never constructed, terminal constructor failure), the failure and elapsed-or-completed timing. `Unwrap() []error` exposes the underlying failures and context causes so `errors.Is`, `errors.As` and `errors.AsType` work through App-level joins. Full success returns nil with Debug timing diagnostics. `Shutdown` is idempotent: a second call returns the already-closed error.

### Concurrency and Lifecycle

- **Registration mutation** (`Provide`, value publication, protection, `AdoptValue`, `Replace`, `Alias`, `BindMany`): intended for sequential startup composition before `app.Finalize()`. Adoption, replacement and Finalize are mutually serialized, so a concurrent winner makes the loser fail rather than publish a stale result.
- **`Has` / `CanProvideValue`**: non-mutating point-in-time observations; neither reserves T against another registration or Finalize.
- **`Finalize`**: Idempotent via `sync.Once`. Safe to call from multiple goroutines but typically called once at startup.
- **`Resolve` / `MustResolve` / `ResolveAll` / `MustResolveAll`**: Rejected before Finalize. Safe for concurrent use afterwards: per-singleton completion ensures each constructor runs exactly once and every waiter receives the same terminal result (value, error or `DIPanicError`). Different singletons resolve concurrently without blocking each other.
- **`Shutdown(ctx)`**: Enters closing atomically with respect to resolution admission and result delivery, then runs the dependency-ordered pass described above. Ordinary attempts share the caller's context; only late construction gets the separate five-second attempt.

---

## Design Decisions

1. **Framework-managed Infra over implicit Base** --- Embedding a struct and auto-populating via reflection (Spring `@Autowired`) would hide the dependency boundary. `credo.Infra` is produced by the framework but appears in constructor signatures --- visible, reviewable, mockable.

2. **Infra in root package** --- Infra must be referenceable without importing the DI package. Placing it in root keeps the dependency graph clean.

3. **Interfaces for infrastructure, not concrete types** --- infrastructure beyond the stdlib `*slog.Logger` will be expressed as root-package interfaces so root never imports `observability/` --- preserving the zero-external-dependency kernel. The first speculative cut of those interfaces (`MeterProvider`, `TracerProvider`) was removed pre-v1; Phase 3.5 redesigns them against real adapters.

4. **Always available with default-logger fallback** --- container-produced Infra never carries a nil Logger; it defaults to the framework stderr logger when unconfigured. This eliminates test ceremony and makes Infra usage truly optional.

5. **Single lifecycle: Singleton** --- One instance per container lifetime. Web frameworks rarely need per-request DI construction. Request-specific data flows through `context.Context` on method calls, not through constructors.

6. **Container detects injection model via type check** --- At registration time, the container checks if a parameter is `credo.Infra`. Standard type comparison on the cold path.

7. **Typed constructors over injector parameter** --- samber/do uses `func(do.Injector) (T, error)` which is a service locator inside the constructor. Credo uses `func(dep1 T1, dep2 T2) T` --- dependencies are visible in the signature.

8. **Finalize/Seal lifecycle** --- `app.Finalize()` seals the container and validates the graph. This separates registration from runtime and catches errors (missing deps, cycles, forbidden params) before the first request.

9. **Composition Root enforcement** --- The container is designed for startup use. Business code receives resolved services via constructors and Infra, never via `Resolve[T]` calls.

10. **Interface aliasing via Alias** --- `Alias[I, T]()` creates a type alias from interface to concrete type. This is simpler than samber/do's `As[T]()` and keeps the registration and aliasing steps explicit and separate.

11. **Ordered collections via BindMany** --- `BindMany[I, T]()` and `ResolveAll[I]` support plugin-style composition while keeping single resolution explicit. Credo intentionally does not introduce named/keyed bindings for this use case.

12. **Protected bindings are opt-in integration state** --- ordinary bindings stay replaceable for composition overrides and tests. Integrations that publish matching lifecycle/health state may protect the direct binding so `Replace` cannot make DI diverge from that external state.

13. **Resolution only after Finalize** --- constructors never run in the registration phase, so a validated graph is the only graph that ever executes and registration helpers cannot trigger construction as a side effect. Rejected: a general early Resolve/Peek exemption and protect-on-read, which would freeze an invalid binding before validation. Registration-time reads are `AdoptValue` (validate, then atomically protect) and `Has` (observe only).

14. **Replace transfers ownership explicitly** --- a successful replacement returns the superseded created instance and its cleanup responsibility to the caller; the container never closes a value it no longer hands out. The Warn log for a superseded `Shutdowner` is a diagnostic, not the transfer mechanism. Rejected: silently abandoning the old instance, and automatically closing it (the caller may still hold it).

15. **Dependency-ordered shutdown with bounded waiting** --- consumers close before the singletons they were built from, using a Kahn ready queue with reverse-registration tie-break over the static graph (adapted from samber/do v2.1.0's batched dependent bookkeeping). Every ordinary call is bounded by the shared context via helper goroutines; a runaway callback keeps its dependencies blocked and is reported rather than skipped around. Rejected: reverse registration order alone, a bulk wait-for-builds phase before any cleanup, an unbounded construction barrier, and do v2's out-of-order fallback.

16. **Terminal completion and inspectable diagnostics** --- one completion record per singleton covers value, error and panic and is shared by all waiters; nothing is retried automatically. Panics are recovered on the goroutine that invoked user code and surfaced as `DIPanicError` with phase, original value and stack; teardown results are an immutable `DIShutdownError` snapshot that unwraps its causes. Closing rejection is the `ErrDIClosed` sentinel from the moment DI teardown begins.

---

## samber/do Adaptation Scope

### What We Keep

| samber/do source | Credo file | Notes |
| --- | --- | --- |
| Container core + type registry | `internal/di/container.go` | Adapted to typed constructors (not `func(Injector)` signature) |
| Lifecycle primitives | `internal/di/option.go` | Singleton option only |
| `Shutdowner` interface | Root package `interfaces.go` | Dependency-ordered shutdown (Kahn ready queue; do v2.1.0's dependent bookkeeping adapted to Credo's static graph) |

### What We Cut

| samber/do feature | Reason |
| --- | --- |
| `func(do.Injector) (T, error)` constructor signature | Service locator inside constructor --- replaced with typed params |
| `do.MustInvoke[T](i)` inside constructors | Same --- service locator pattern |
| Named services (`do.ProvideNamed`) | Type-based resolution is sufficient. Named variants add complexity |
| `do.Package()` grouping | Replaced by App-level registration helpers |
| Scope tree (parent/child) | Not needed --- single Singleton lifecycle, no request-scoped containers |
| Out-of-order shutdown fallback, automatic build retries | A blocked dependency is reported with its blockers instead of being closed underneath a live consumer; a failed constructor is terminal |

### Key Divergence

The most significant adaptation is **constructor signatures**. samber/do passes an `Injector` to every constructor, making every constructor a service locator:

```go
// samber/do --- constructor = service locator
func NewUserService(i do.Injector) (*UserService, error) {
    db := do.MustInvoke[*sql.DB](i)       // hidden dependency
    logger := do.MustInvoke[*Logger](i)    // hidden dependency
    return &UserService{db: db, logger: logger}, nil
}
```

Credo uses typed constructor parameters:

```go
// Credo --- constructor = explicit dependencies
func NewUserService(infra credo.Infra, db *sql.DB) *UserService {
    return &UserService{infra: infra, db: db}
    // Logger comes from Infra, not a separate parameter
}
```

The container inspects the constructor's parameter types via reflection at registration time (cold path). Resolution uses cached type mappings --- the `reflect` package is never called on the hot path.

---

## File Layout

```text
internal/di/
+-- doc.go            <- package documentation (samber/do attribution)
+-- container.go      <- Container struct, New(), findRegistration (alias-aware)
+-- provide.go        <- Provide/ProvideValue registration, protection + preflight
+-- adopt.go          <- Has, AdoptValue (validate + atomic compare-and-protect)
+-- replace.go        <- Replace with ownership transfer
+-- resolve.go        <- Resolve[T], ResolveAll[I], phase/closing admission, completion
+-- bind.go           <- Alias[I,T], BindMany[I,T], binding management
+-- build.go          <- Seal(), freeze + validate via sync.Once
+-- lifecycle.go      <- validate (unexported), cycle detection, graph extraction
+-- shutdown.go       <- Shutdown: closing, Kahn ready queue, bounded calls, late cleanup, report
+-- errors.go         <- ErrClosed, PanicError, ShutdownError/Entry/State
+-- provider.go       <- provider strategies (constructor/value)
+-- infra.go          <- FrameworkProvider map (credo.Infra), deriveServiceName
+-- export_test.go    <- test-only helpers
+-- *_test.go

Root package:
+-- infra.go          <- Infra struct, newInfra (default-logger fallback), defaultLogger
+-- interfaces.go     <- Shutdowner, RawConfig alias
+-- di.go             <- root registration/preflight/protection/adoption/resolve/alias APIs
+-- dierrors.go       <- ErrDIClosed, DIPanicError, DIShutdownError aliases
+-- infra_test.go
```

---

## Examples

### Basic Wiring

```go
func main() {
    rawCfg, err := config.Load()
    if err != nil {
        log.Fatal(err)
    }

    app, err := credo.New(credo.WithRawConfig(rawCfg))
    if err != nil {
        log.Fatal(err)
    }

    // Register services (all Singleton)
    app.MustProvide[*sql.DB](NewDB)
    app.MustProvide[*UserRepo](NewUserRepo)
    app.MustProvide[*UserService](NewUserService)

    // Finalize: freeze container + validate dependency graph
    if err := app.Finalize(); err != nil {
        log.Fatal(err)
    }

    // Resolve services for handler setup
    userSvc := app.MustResolve[*UserService]()

    app.GET("/users/{id}", func(ctx *credo.Context) error {
        user, err := userSvc.FindByID(ctx.Context(), ctx.Request().RouteParam("id"))
        if err != nil {
            return err
        }
        return ctx.Response().JSON(200, user)
    })

    app.Run() // Run calls Finalize implicitly if not already called
}
```

### Service with Infra (Model 1)

```go
type UserService struct {
    infra credo.Infra
    repo  *UserRepo
    db    *sql.DB
}

func NewUserService(infra credo.Infra, repo *UserRepo, db *sql.DB) *UserService {
    return &UserService{infra: infra, repo: repo, db: db}
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

### Interface Aliasing

```go
// Define the interface.
type UserRepo interface {
    FindByID(ctx context.Context, id string) (*User, error)
    Create(ctx context.Context, input CreateUserInput) (*User, error)
}

// Register the concrete implementation.
app.MustProvide[*PgUserRepo](NewPgUserRepo)

// Alias interface to concrete type.
app.MustAlias[UserRepo, *PgUserRepo]()

// Services depend on the interface, resolved via the alias.
func NewUserService(infra credo.Infra, repo UserRepo) *UserService {
    return &UserService{infra: infra, repo: repo}
}
```

### Ordered Interface Collection

```go
type Sender interface {
    Send(ctx context.Context, msg Message) error
}

type SenderRegistry struct {
    senders []Sender
}

func NewSenderRegistry(senders []Sender) *SenderRegistry {
    return &SenderRegistry{senders: senders}
}

app.MustProvide[*EmailSender](NewEmailSender)
app.MustProvide[*InAppSender](NewInAppSender)

app.MustBindMany[Sender, *EmailSender]()
app.MustBindMany[Sender, *InAppSender]()

app.MustProvide[*SenderRegistry](NewSenderRegistry)

registry := app.MustResolve[*SenderRegistry]()
allSenders := app.MustResolveAll[Sender]()

_ = registry
_ = allSenders
```

### Testing --- Direct Construction

```go
func TestUserService_Create(t *testing.T) {
    svc := &UserService{
        infra: credo.Infra{Logger: slog.Default()},
        repo:  testutil.NewMockUserRepo(t),
        db:    testutil.NewMockDB(t),
    }

    user, err := svc.Create(t.Context(), validInput)
    assert.NoError(t, err)
    assert.Equal(t, "alice@example.com", user.Email)
}
```

Infra is a plain struct --- construct it directly, no ceremony. Set the Logger your test needs to observe; a zero-value `Infra{}` leaves Logger nil (the default-logger fallback applies only to container-produced Infra).

---

## Test Requirements

### Registration

- `Provide[T]` with valid constructor succeeds
- `Provide[T]` with non-function constructor returns error
- `Provide[T]` with nil constructor returns error (no panic)
- `MustProvide[T]` panics on invalid constructor
- Duplicate `Provide[T]` for same type returns error
- `ProvideValue[T]` registers value as Singleton
- `CanProvideValue[T]` reports frozen/direct-duplicate conflicts without mutating or reserving T; final publication remains authoritative
- `ProvideProtectedValue[T]` registers a Singleton that `Replace[T]` cannot overwrite
- `ProtectBinding[T]()` is idempotent for an existing direct registration, rejects missing/frozen bindings, does not resolve T, and makes Replace fail
- `ProtectBinding[T](expected)` atomically compare-and-protects only an already-resolved, comparable, matching singleton; unresolved, non-comparable, changed, or multiple expected values fail without adding protection
- Ordinary `ProvideValue[T]` bindings remain replaceable
- `Provide[T]` after `Finalize()` returns error (container frozen)
- `ProvideProtectedValue[T]` and `ProtectBinding[T]` after Finalize return errors
- `Has[T]` reports constructor, value and alias registrations without constructing, adopting or protecting
- `AdoptValue[T]` returns and protects a validated pre-built value; validation failure leaves the binding repairable; a constructor binding is rejected without being invoked; missing/frozen bindings error; a concurrent `Replace` or `Finalize` during validation aborts the adoption
- `Replace[T]` returns the superseded created instance with `existed == true`, zero/false for an unbuilt constructor, and never constructs the old provider; the container no longer closes the returned instance; `MustReplace` mirrors the result

### Aliasing

- `Alias[I, T]` succeeds when T is registered and implements I
- `Alias[I, T]` returns error when I is not an interface
- `Alias[I, T]` returns error when T does not implement I
- `Alias[I, T]` returns error when T is not registered
- `Alias[I, T]` returns error when I already has a registration or alias
- `Alias[I, T]` after `Finalize()` returns error (container frozen)
- `Resolve[I]` after `Alias[I, T]` returns the T singleton

### Ordered Collections

- `BindMany[I, T]` succeeds when T is registered, concrete, and implements I
- `BindMany[I, T]` returns error when I is not an interface
- `BindMany[I, T]` returns error when T is an interface type
- `BindMany[I, T]` returns error when T does not implement I
- `BindMany[I, T]` returns error when T is not registered
- `BindMany[I, T]` returns error when the same `(I, T)` pair already exists
- `BindMany[I, T]` after `Finalize()` returns error (container frozen)
- `ResolveAll[I]` preserves bind order
- `ResolveAll[I]` returns `[]I{}` when no bindings exist
- `[]I` constructor injection preserves bind order
- Direct registrations for `[]I` take precedence over `BindMany`
- Collection-based cycles are detected during `Seal()`

### Finalize / Seal

- `app.Finalize()` returns nil when dependency graph is valid
- `app.Finalize()` returns error listing missing dependencies
- `Seal()` detects circular dependencies with clear cycle description
- `Seal()` detects `context.Context` constructor parameters and returns error
- `app.Finalize()` is idempotent --- second call returns same result
- `app.Finalize()` freezes container --- subsequent `Provide`/`Alias` return errors
- `Resolve` before `Finalize()` returns a not-finalized error without running a constructor
- `Run()` and the first direct `ServeHTTP` call `Finalize()` implicitly; explicit `Finalize()` leaves route registration open

### Validation (via Seal)

- `Seal()` returns nil when all dependencies are resolvable
- `Seal()` returns error listing missing dependencies
- `Seal()` detects circular dependencies (A -> B -> A) with clear cycle description
- `Seal()` reports error for `context.Context` constructor parameters
- Registration order does not matter --- A depending on B registered after A is valid

### Resolution

- `Resolve[T]` returns correct instance
- `Resolve[T]` for unregistered type returns error
- `MustResolve[T]` panics on missing registration
- Singleton: same instance returned on multiple `Resolve` calls
- Constructor returning error: error propagated to `Resolve` caller
- Constructor panic: one construction, every concurrent and later caller receives the same terminal `DIPanicError` (type, phase, value, stack); a dependent constructor fails through its parameter; `Shutdown` still returns nil

### Infra Production

- Constructor with `credo.Infra` param (Model 1): Infra produced with scoped Logger
- Pure constructor (no Infra): all params resolved normally, no Infra magic
- Logger scoped with `"service"="TypeName"`
- No logger configured: framework default (stderr) logger used, no error

### Infra Zero-Value

- `Infra{}` (zero value) is constructible; Logger stays nil until set
- In tests, `Infra{Logger: customLogger}` works without container

### Lifecycle

- `Shutdown(ctx)` closes consumers before their dependencies regardless of registration order, with reverse registration order as the tie-break
- Aliases and collection members close once; a non-`Shutdowner` intermediate keeps transitive order
- Closing rejects new `Resolve` calls with `ErrDIClosed`, also after a failed Seal and for cached results; a never-finalized container tears down its built values
- A pending build blocks its dependencies and withholds its result from callers; a failed pending build releases them
- A hung `Shutdowner` is bounded by ctx, keeps its dependencies blocked, is reported, and is never retried; the returned report is immutable
- A shutdown panic is isolated and reported as a `DIPanicError`; `DIShutdownError.Unwrap` exposes every cause
- Construction finishing after ctx ended gets one five-second late cleanup attempt; full success returns nil

### Concurrency

- `Resolve[T]` is safe for concurrent use after Finalize
- Concurrent `Resolve` of same Singleton returns same instance (no double-init)
- Concurrent `AdoptValue`/`Replace`/`Finalize` never protect a stale or replaced instance
