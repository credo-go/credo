# DI Container & credo.Infra Spec

**Status**: Approved
**Implementation**: `internal/di/` (private), Root package API (`credo.Provide[T]`, `credo.Resolve[T]`, `credo.BindMany[I, T]`, `credo.ResolveAll[I]`)
**Sources**: samber/do (MIT)
**Depends on**: ---
**ADRs**: [004-dependency-injection-and-infra](../adr/004-dependency-injection-and-infra.md)
**Roadmap**: [`TODO.md` Phase 2.1, 2.2](../../TODO.md)

---

## Canonical Source

Implementation-level details for Credo's dependency injection system are defined
in this file. Other documents should keep only high-level references and link
here.

---

## Overview

Credo's DI system consists of two parts:

1. **`internal/di/`** --- A generics-based container adapted from samber/do.
   Uses Go 1.26+ generics for type-safe registration and resolution. No
   code generation. `Seal()` freezes and validates the graph at startup.

2. **`credo.Infra`** --- An explicit infrastructure carrier defined in the root
   package. Carries the per-service Logger (metrics/tracing carriers return
   with the Phase 3.5 observability release). Produced automatically by
   the container when seen as a constructor parameter.

The container lives in `internal/di` because it is Credo-specific --- not a
standalone DI library. The public API is exposed through root package generic
functions such as `credo.Provide[T](app, constructor)`,
`credo.BindMany[I, T](app)`, `credo.Resolve[T](app)`, and
`credo.ResolveAll[I](app)`.

---

## Goals

1. **Generics over reflection**: `Resolve[T]` is fully compile-time typed.
   `Provide[T]`'s `constructor` parameter is necessarily `any` (Go cannot
   express "a function with arbitrary parameters returning T"), so its
   signature is checked at registration time --- mistakes surface as an
   immediate error from `Provide`, and the dependency graph is validated at
   `Finalize`; `ProvideFactory` offers a fully compiler-checked alternative.
   Reflection is used at registration time to inspect constructor signatures
   and once per singleton during first construction (`reflect.Value.Call`).
   Subsequent resolves of the same singleton are pure cache lookups --- zero
   reflection.
2. **Explicit infrastructure**: `credo.Infra` delivers cross-cutting
   infrastructure (currently the Logger) as an explicit constructor
   parameter. No implicit auto-populate, no struct tag scanning.
3. **Composition Root first**: The container is primarily used at startup
   (`main()` or `App.Run()`). Credo's recommended pattern is constructor
   injection and bootstrap-time wiring. Runtime `Resolve` calls remain
   available for advanced use cases, but the framework does not provide
   request-time DI helpers such as `Context.Resolve`.
4. **Always available with default-logger fallback**: `credo.Infra` works
   without configuring a Logger. An unset Logger falls back to the
   framework-owned stderr logger. Tests never need container setup for
   infrastructure access.
5. **Single lifecycle: Singleton**: One instance per container lifetime. This
   covers web framework needs without unnecessary complexity.
6. **Ordered interface collections**: Components can depend on `[]I` when
   startup wiring needs an ordered set of implementations (hooks, senders,
   subscribers) without introducing named services or keyed lookups.

---

## credo.Infra --- Explicit Infrastructure Carrier

### Definition

`credo.Infra` is a framework-defined, non-extensible struct in the **root
package**. It carries infrastructure that most services need:

```go
// root package (credo)
type Infra struct {
    _ struct{} // forces keyed literals so new fields can be added compatibly

    Logger *slog.Logger
}
```

Users cannot add fields to `Infra`. The framework defines the set.

### Observability Carriers (deferred to Phase 3.5)

Earlier drafts defined speculative `MeterProvider`/`TracerProvider` (plus
`Counter`/`Histogram`/`Span`) interfaces in the root package, carried as
`Infra.Metrics`/`Infra.Tracer` with noop fallbacks. They were removed before
v1 (2026-06-11): no real adapter existed, and shipping placeholder interfaces
would have frozen an untested API surface. The observability release
(Phase 3.5, aligned with the v1 / Go 1.27 window) reintroduces metrics and
tracing carriers designed against real OpenTelemetry and Prometheus adapters;
`Infra`'s keyed-literal guard (`_ struct{}`) lets those fields land without
breaking existing constructors.

### Always Available (Default-Logger Fallback)

When no logger is configured, the framework produces `Infra` with a safe
fallback:

- `Logger`: a framework-owned stderr logger (deliberately independent of
  `slog.SetDefault`, so framework behavior does not change under global
  mutable state)

This guarantees:
- **Tests work without container setup.** Construct a service directly, pass a
  custom `Infra{Logger: testLogger}`. No panics.
- **Gradual opt-in.** Services work immediately; observability is layered on
  when ready.

### Per-Service Scoping

When the container produces `Infra`, the Logger is **scoped** to the service's
type name:

```go
// Container internally does:
infra.Logger = appLogger.With("service", "OrderService")
```

Every log line from `OrderService` automatically includes
`"service"="OrderService"` without the developer passing it manually.

### Non-DI Infra

For components outside the DI container (middleware factories, startup helpers,
manually created workers), use `app.NewInfra(name)`:

```go
rbacMW := rbac.NewMiddleware(app.NewInfra("RBACMiddleware"))
app.GlobalMiddleware(rbacMW)
```

The returned Infra has a Logger scoped with `"service"=name`. `NewInfra` is
nil-safe: when the app has no logger configured it falls back to the framework
default logger. For DI-managed services, Infra is injected automatically —
prefer DI when possible.

### Config Is Not in Infra

Config is NOT part of `credo.Infra`. See
[ADR-005](../adr/005-configuration-architecture.md) for rationale:
- Each service may need a different config section (`*OrderConfig` vs
  `*DatabaseConfig`)
- Config is an immutable startup-time snapshot; the Logger is runtime
  infrastructure
- Typed config via DI is more type-safe than a universal accessor

### What Goes Where

| Dependency | Access | Rationale |
|---|---|---|
| DB connections | Constructor param | Behavior-defining, must be mockable |
| Repositories | Constructor param | Behavior-defining |
| Domain services | Constructor param | Behavior-defining |
| Typed config struct | Constructor param | Structural, read at startup |
| Logger | `infra.Logger` (via `credo.Infra`) | Ubiquitous, cross-cutting |
| Metrics / Tracer | *Phase 3.5* | Carriers return with the observability release |
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

The container inspects the constructor signature at registration time. Each
parameter is checked independently:

1. Parameter type is `credo.Infra` --- **Model 1**: produce Infra specially
   (scoped Logger, default-logger fallback). Convention: place `credo.Infra`
   as the first parameter.
2. Otherwise --- **Pure constructor injection**: parameter resolved normally
   from the container.

This is a type check on the cold path --- no extra reflection beyond what the
container already does for constructor inspection.

---

## API Surface

### Registration (root package)

```go
// Provide registers a constructor for type T. The constructor's parameters
// are resolved from the container automatically. Lifecycle: Singleton.
func Provide[T any](app *App, constructor any) error

// MustProvide is like Provide but panics on error. Intended for use at
// startup (Composition Root) where a failed registration is fatal.
func MustProvide[T any](app *App, constructor any)

// ProvideFactory registers a compile-time-checked factory for type T.
// fn receives the App and resolves its own dependencies; T is inferred.
func ProvideFactory[T any](app *App, fn func(*App) (T, error)) error

// MustProvideFactory is like ProvideFactory but panics on error.
func MustProvideFactory[T any](app *App, fn func(*App) (T, error))

// ProvideValue registers a pre-built value as a Singleton.
func ProvideValue[T any](app *App, value T) error

// MustProvideValue is like ProvideValue but panics on error.
func MustProvideValue[T any](app *App, value T)
```

The `constructor` parameter accepts any function whose parameters are
resolvable types and whose first return value is `T`:

```go
// All valid constructor signatures:
func NewUserRepo(db *sql.DB) *UserRepo
func NewUserRepo(db *sql.DB) (*UserRepo, error)
func NewUserRepo(infra credo.Infra, db *sql.DB) (*UserRepo, error)
```

Constructor parameter types are inspected via reflection at registration time
and matched against registered types. The first construction of each singleton
also uses `reflect.Value.Call` to invoke the constructor. Subsequent resolves
of the same singleton are pure cache lookups with zero reflection cost.

Because `constructor` is typed `any`, a signature mistake (wrong return type,
not a function) is reported as an error by `Provide` at registration time ---
not by the compiler. `ProvideFactory` closes that gap: `fn`'s signature is
enforced by the compiler, and `fn` resolves its own dependencies explicitly.
The trade-off is that `fn` is opaque to the container --- dependencies
resolved inside it do not participate in `Finalize` graph validation or cycle
detection (the same holds for any constructor closure that captures `app` and
calls `Resolve`), and `credo.Infra` is not auto-injected (use `app.NewInfra`
inside `fn` instead):

```go
credo.ProvideFactory(app, func(app *credo.App) (*UserService, error) {
    repo, err := credo.Resolve[*UserRepository](app)
    if err != nil {
        return nil, err
    }
    return NewUserService(app.NewInfra("UserService"), repo), nil
})
```

**`context.Context` as a constructor parameter is always an error.** Constructors
run at startup, not per-request. If `Seal()` encounters a constructor with a
`context.Context` parameter, it reports a clear error.

### Interface Aliasing

```go
// Alias creates an alias so that Resolve[I] returns the instance registered
// for concrete type T. I must be an interface, T must implement I, and T
// must already be registered.
func Alias[I, T any](app *App) error

// MustAlias is like Alias but panics on error.
func MustAlias[I, T any](app *App)
```

Alias enables resolving by interface without requiring the constructor to return
the interface type:

```go
// Register the concrete type.
credo.MustProvide[*PgUserRepo](app, NewPgUserRepo)

// Alias interface to concrete type.
credo.MustAlias[UserRepo, *PgUserRepo](app)

// Now resolving by interface returns the *PgUserRepo singleton.
repo := credo.MustResolve[UserRepo](app)
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
func BindMany[I, T any](app *App) error

// MustBindMany is like BindMany but panics on error.
func MustBindMany[I, T any](app *App)
```

`BindMany[I, T]` is collection wiring, not default resolution. It does not
change `Resolve[I]`; it only affects `ResolveAll[I]` and constructor injection
of `[]I`.

```go
credo.MustProvide[*EmailSender](app, NewEmailSender)
credo.MustProvide[*InAppSender](app, NewInAppSender)

credo.MustBindMany[Sender, *EmailSender](app)
credo.MustBindMany[Sender, *InAppSender](app)
```

Contract rules enforced by `BindMany`:
- `I` must be an interface type
- `T` must be a concrete type (not an interface)
- `T` must implement `I`
- `T` must already be registered via `Provide` or `ProvideValue`
- The same `(I, T)` pair must not already exist
- Container must not be frozen (`Finalize` must not have been called yet)

### Finalize and Container Lifecycle

The container has three phases:

1. **Bootstrap** --- `Provide`, `ProvideValue`, `Alias`, `BindMany`, `Resolve`,
   and `ResolveAll` are all allowed. This supports patterns like
   `ensureRegistry` where code probes with `Resolve` and falls back to
   `ProvideValue` if not yet registered.
2. **Finalize** --- `credo.Finalize(app)` freezes the container (internally
   calling `Seal()`) and validates the dependency graph. After Finalize,
   `Provide`, `ProvideFactory`, `ProvideValue`, `Replace`, `Alias`, and
   `BindMany` return errors. If validation fails, subsequent `Resolve` and
   `ResolveAll` calls return the finalize error.
3. **Runtime** --- `Resolve` creates and caches singletons on demand. The
   dependency graph is guaranteed valid. `app.Run()` and `app.RunTLS()` call
   Finalize implicitly.

**Concurrency**: During bootstrap, `Provide`/`ProvideFactory`/`ProvideValue`/
`Alias`/`BindMany` and `Resolve`/`ResolveAll` must not be called concurrently. The
container uses internal locking for singleton resolution, but registration and
bootstrap resolution are not designed for concurrent use. In practice, all
registration and bootstrap resolution happens sequentially in `main()` or setup
functions before `Run()`.

```go
// Finalize freezes the container and validates the dependency graph.
// After Finalize, no more Provide, ProvideFactory, ProvideValue, Replace, Alias,
// or BindMany calls are allowed.
// Finalize is idempotent --- subsequent calls return the same result via sync.Once.
//
// Finalize is side-effect-free: it does not instantiate singletons or perform I/O.
// It only freezes the container (via Seal) and validates the graph.
//
// app.Run() and app.RunTLS() call Finalize implicitly. Explicit Finalize is
// optional but recommended for fail-fast at startup.
func Finalize(app *App) error
```

```go
// Registration phase
credo.MustProvide[*sql.DB](app, NewDB)
credo.MustProvide[*UserRepo](app, NewUserRepo)
credo.MustProvide[*UserService](app, NewUserService)
credo.MustAlias[UserRepo, *PgUserRepo](app)

// Finalize phase --- freeze + validate
if err := credo.Finalize(app); err != nil {
    log.Fatal(err) // "missing dependency: *UserRepo required by *UserService"
}

// Runtime phase --- safe to resolve
userSvc := credo.MustResolve[*UserService](app)

// These would fail after Finalize:
// credo.Provide[*Foo](app, NewFoo)  // error: container is frozen
// credo.Alias[Bar, *Baz](app)       // error: container is frozen
// credo.BindMany[Qux, *Baz](app)    // error: container is frozen
```

If `credo.Finalize(app)` is not called explicitly, `Run()` and `RunTLS()` call
it implicitly before starting the HTTP server.

Duplicate registration of the same type returns an error.

Circular dependencies (A -> B -> A) are detected during Finalize and produce a
clear error listing the cycle. (Edges hidden inside `ProvideFactory` constructors
are the exception --- see the registration section above.)

### Resolution (root package)

```go
// Resolve returns the instance of T, creating it if necessary.
func Resolve[T any](app *App) (T, error)

// MustResolve panics if T cannot be resolved. It is primarily intended for
// bootstrap/composition-root code. Runtime use is supported, but Credo's
// recommended application pattern is constructor injection.
func MustResolve[T any](app *App) T

// ResolveAll returns all instances bound to interface I via BindMany,
// preserving bind order. When no bindings exist, it returns []I{}.
func ResolveAll[I any](app *App) ([]I, error)

// MustResolveAll panics if the collection cannot be resolved.
func MustResolveAll[I any](app *App) []I
```

`Resolve` remains public after `Finalize()` and can be called at runtime.
Credo intentionally keeps that low-level capability available, but does not
make it part of the preferred request-time programming model. There is no
`Context.Resolve` helper, and the recommended approach remains wiring
dependencies through constructors during bootstrap.

### `[]I` Constructor Injection

When a constructor parameter is `[]I` and there is no direct registration for
that exact slice type, the container resolves it from `BindMany[I, T]`
bindings:

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
- `Resolve[[]I]` remains a normal direct lookup; collection semantics are
  exposed through `ResolveAll[I]` and constructor injection only

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
// internal/di/lifecycle.go

// Shutdown calls Shutdown(ctx) on all cached singletons that implement
// Shutdowner, in reverse registration order.
func (c *Container) Shutdown(ctx context.Context) error
```

### Concurrency and Lifecycle

- **`Provide` / `MustProvide` / `ProvideFactory` / `ProvideValue` / `Alias` /
  `BindMany`**: Not concurrent-safe. Intended to be called sequentially at
  startup (Composition Root), before `credo.Finalize(app)` or `app.Run()`.
- **`Finalize`**: Idempotent via `sync.Once`. Safe to call from multiple
  goroutines but typically called once at startup.
- **`Resolve` / `MustResolve` / `ResolveAll` / `MustResolveAll`**: Safe for
  concurrent use after Finalize. Per-singleton `sync.Once` ensures each
  constructor runs exactly once, even under concurrent access. Different
  singletons resolve concurrently without blocking each other.
- **`Shutdown(ctx)`**: Should be called once during graceful shutdown with a
  deadline context.

---

## Design Decisions

1. **Explicit Infra over implicit Base** --- Embedding a struct and auto-populating
   via reflection (Spring `@Autowired`) violates Go's explicitness. `credo.Infra`
   appears in constructor signatures --- visible, reviewable, mockable.

2. **Infra in root package** --- Infra must be referenceable without importing the
   DI package. Placing it in root keeps the dependency graph clean.

3. **Interfaces for infrastructure, not concrete types** --- infrastructure
   beyond the stdlib `*slog.Logger` will be expressed as root-package
   interfaces so root never imports `observability/` --- preserving the
   zero-external-dependency kernel. The first speculative cut of those
   interfaces (`MeterProvider`, `TracerProvider`) was removed pre-v1;
   Phase 3.5 redesigns them against real adapters.

4. **Always available with default-logger fallback** --- container-produced
   Infra never carries a nil Logger; it defaults to the framework stderr
   logger when unconfigured. This eliminates test ceremony and makes Infra
   usage truly optional.

5. **Single lifecycle: Singleton** --- One instance per container lifetime. Web
   frameworks rarely need per-request DI construction. Request-specific data
   flows through `context.Context` on method calls, not through constructors.

6. **Container detects injection model via type check** --- At registration
   time, the container checks if a parameter is `credo.Infra`. Standard type
   comparison on the cold path.

7. **Typed constructors over injector parameter** --- samber/do uses
   `func(do.Injector) (T, error)` which is a service locator inside the
   constructor. Credo uses `func(dep1 T1, dep2 T2) T` --- dependencies are
   visible in the signature.

8. **Finalize/Seal lifecycle** --- `credo.Finalize(app)` seals the container and
   validates the graph. This separates registration from runtime and catches
   errors (missing deps, cycles, forbidden params) before the first request.

9. **Composition Root enforcement** --- The container is designed for startup
   use. Business code receives resolved services via constructors and Infra,
   never via `Resolve[T]` calls.

10. **Interface aliasing via Alias** --- `Alias[I, T]()` creates a type alias from
    interface to concrete type. This is simpler than samber/do's `As[T]()` and
    keeps the registration and aliasing steps explicit and separate.

11. **Ordered collections via BindMany** --- `BindMany[I, T]()` and
    `ResolveAll[I]` support plugin-style composition while keeping single
    resolution explicit. Credo intentionally does not introduce named/keyed
    bindings for this use case.

---

## samber/do Adaptation Scope

### What We Keep

| samber/do source | Credo file | Notes |
|---|---|---|
| Container core + type registry | `internal/di/container.go` | Adapted to typed constructors (not `func(Injector)` signature) |
| Lifecycle primitives | `internal/di/option.go` | Singleton option only |
| `Shutdowner` interface | Root package `interfaces.go` | Reverse-order shutdown |

### What We Cut

| samber/do feature | Reason |
|---|---|
| `func(do.Injector) (T, error)` constructor signature | Service locator inside constructor --- replaced with typed params |
| `do.MustInvoke[T](i)` inside constructors | Same --- service locator pattern |
| Named services (`do.ProvideNamed`) | Type-based resolution is sufficient. Named variants add complexity |
| `do.Package()` grouping | Replaced by App-level registration helpers |
| Scope tree (parent/child) | Not needed --- single Singleton lifecycle, no request-scoped containers |

### Key Divergence

The most significant adaptation is **constructor signatures**. samber/do passes
an `Injector` to every constructor, making every constructor a service locator:

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

The container inspects the constructor's parameter types via reflection at
registration time (cold path). Resolution uses cached type mappings --- the
`reflect` package is never called on the hot path.

---

## File Layout

```text
internal/di/
+-- doc.go            <- package documentation (samber/do attribution)
+-- container.go      <- Container struct, New(), findRegistration (alias-aware)
+-- provide.go        <- Provide[T], ProvideFactory[T], ProvideValue[T], registration logic
+-- resolve.go        <- Resolve[T], ResolveAll[I], dependency graph walk
+-- bind.go           <- Alias[I,T], BindMany[I,T], binding management
+-- build.go          <- Seal(), freeze + validate via sync.Once
+-- option.go         <- Singleton option
+-- lifecycle.go      <- Shutdown, validate (unexported), cycle detection
+-- infra.go          <- InfraProvider, isInfraType, deriveServiceName
+-- export_test.go    <- test-only helpers
+-- *_test.go

Root package:
+-- infra.go          <- Infra struct, newInfra (default-logger fallback), defaultLogger
+-- interfaces.go     <- Shutdowner, RawConfig alias
+-- di.go             <- Provide[T], ProvideFactory[T], ProvideValue[T], Resolve[T], ResolveAll[I], Alias[I,T], BindMany[I,T]
+-- infra_test.go
```

---

## Examples

### Basic Wiring

```go
func main() {
    store, err := config.Load()
    if err != nil {
        log.Fatal(err)
    }

    app, err := credo.New(credo.WithRawConfig(store))
    if err != nil {
        log.Fatal(err)
    }

    // Register services (all Singleton)
    credo.MustProvide[*sql.DB](app, NewDB)
    credo.MustProvide[*UserRepo](app, NewUserRepo)
    credo.MustProvide[*UserService](app, NewUserService)

    // Finalize: freeze container + validate dependency graph
    if err := credo.Finalize(app); err != nil {
        log.Fatal(err)
    }

    // Resolve services for handler setup
    userSvc := credo.MustResolve[*UserService](app)

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
credo.MustProvide[*PgUserRepo](app, NewPgUserRepo)

// Alias interface to concrete type.
credo.MustAlias[UserRepo, *PgUserRepo](app)

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

credo.MustProvide[*EmailSender](app, NewEmailSender)
credo.MustProvide[*InAppSender](app, NewInAppSender)

credo.MustBindMany[Sender, *EmailSender](app)
credo.MustBindMany[Sender, *InAppSender](app)

credo.MustProvide[*SenderRegistry](app, NewSenderRegistry)

registry := credo.MustResolve[*SenderRegistry](app)
allSenders := credo.MustResolveAll[Sender](app)

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

Infra is a plain struct --- construct it directly, no ceremony. Set the Logger
your test needs to observe; a zero-value `Infra{}` leaves Logger nil (the
default-logger fallback applies only to container-produced Infra).

---

## Test Requirements

### Registration
- `Provide[T]` with valid constructor succeeds
- `Provide[T]` with non-function constructor returns error
- `Provide[T]` with nil constructor returns error (no panic)
- `MustProvide[T]` panics on invalid constructor
- Duplicate `Provide[T]` for same type returns error
- `ProvideValue[T]` registers value as Singleton
- `Provide[T]` after `Finalize()` returns error (container frozen)
- `ProvideFactory[T]` runs fn lazily, exactly once; instance is cached
- `ProvideFactory[T]` fn can resolve dependencies from the container
- `ProvideFactory[T]` propagates fn's error to the `Resolve` caller
- `ProvideFactory[T]` rejects nil fn, duplicates, and frozen container
- `ProvideFactory[T]`-built instances participate in reverse-order `Shutdown`

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
- `credo.Finalize(app)` returns nil when dependency graph is valid
- `credo.Finalize(app)` returns error listing missing dependencies
- `Seal()` detects circular dependencies with clear cycle description
- `Seal()` detects `context.Context` constructor parameters and returns error
- `credo.Finalize(app)` is idempotent --- second call returns same result
- `credo.Finalize(app)` freezes container --- subsequent `Provide`/`Alias` return errors
- `Run()` calls `Finalize()` implicitly

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

### Infra Production
- Constructor with `credo.Infra` param (Model 1): Infra produced with scoped Logger
- Pure constructor (no Infra): all params resolved normally, no Infra magic
- Logger scoped with `"service"="TypeName"`
- No logger configured: framework default (stderr) logger used, no error

### Infra Zero-Value
- `Infra{}` (zero value) is constructible; Logger stays nil until set
- In tests, `Infra{Logger: customLogger}` works without container

### Lifecycle
- `Shutdown(ctx)` calls `Shutdown(ctx)` on services in reverse registration order
- `Shutdown(ctx)` skips services not implementing `Shutdowner`

### Concurrency
- `Resolve[T]` is safe for concurrent use after Finalize
- Concurrent `Resolve` of same Singleton returns same instance (no double-init)
