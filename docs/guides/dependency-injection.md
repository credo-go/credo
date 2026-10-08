# Dependency Injection Guide

This guide explains how to use Credo's DI container in real applications. For low-level contracts and internal rationale, see the [DI Container Spec](../specs/container.md) and [ADR-004](../adr/004-dependency-injection-and-infra.md).

---

## What Credo's DI Is For

Credo's container exists to wire application components at startup:

- database pools and external clients
- repositories
- services
- controllers
- typed config structs
- framework-managed infrastructure via `credo.Infra`

Credo's DI is intentionally simple:

- **Singleton only**: one instance per app
- **Constructor injection first**: dependencies are visible in function signatures
- **No request scope**: request data belongs in `*credo.Context` / `context.Context`
- **No `Context.Resolve` helper**: Credo does not push service locator usage into handlers

DI is optional. You can use Credo without the container and wire dependencies manually if you prefer.

---

## Mental Model

Credo's DI flow is:

```text
Provide / ProvideValue / Alias / BindMany
                    ->
                Finalize
                    ->
         Resolve / ResolveAll
                    ->
                  Run
```

- `Provide[T]`: register a constructor
- `ProvideValue[T]`: register a pre-built singleton
- registration options — `credo.Ingress()`, `credo.Borrowed()`, `credo.Closer()`, `credo.Override()`: decide a binding's tier, its owner, its teardown, or that it replaces an earlier binding ([Registration options](#registration-options))
- `app.Manage(v, opts...)`: add a component — a value with `Shutdown`, or a constructor — that is not a binding ([Shutdown and Lifecycle](#shutdown-and-lifecycle))
- `Has[T]`: check whether `T` is registered, without constructing anything
- `CanProvideValue[T]`: point-in-time frozen/direct-duplicate preflight
- `ProvideProtectedValue[T]`: low-level pre-built binding that rejects later `Replace[T]`
- `ProtectBinding[T](expected ...T)`: low-level blind or CAS-style protection for an existing direct binding
- `AdoptValue[T](validate)`: low-level registration-time read that validates a pre-built value and protects its binding
- `Replace[T]`: overwrite an ordinary pre-built binding and receive the superseded instance; protected bindings reject it
- `Alias[I, T]`: resolve an interface `I` as the singleton of concrete type `T`
- `BindMany[I, T]`: add a concrete singleton `T` to the ordered collection for interface `I`
- `app.Finalize()`: freeze registrations and validate the dependency graph
- `Resolve[T]`: retrieve a fully wired singleton (available after `Finalize`)
- `ResolveAll[I]`: retrieve the ordered collection bound for interface `I`

Constructors never run before `Finalize`, and `Resolve` panics until then. `Run()` and `RunContext()` call `Finalize()` implicitly as a safeguard, but a composition root that resolves services before running must call it explicitly, and explicit `app.Finalize()` is recommended in any case so dependency errors fail fast during startup.

Bootstrap is sequential: registration calls come from the goroutine that builds the App, before it runs, and are not safe for concurrent use; an application that registers from several goroutines serializes the calls itself. The whole bootstrap follows one order, every satellite included:

1. configuration — `credo.New()`, or `credo.New(credo.WithRawConfig(raw))`;
2. `Provide` and `ProvideValue`, with `Alias` and `BindMany`;
3. feature mounts and satellite registrations, in any order among themselves — `UseI18n`, `UseHealth`, stores, workers;
4. `Finalize`, handling its error;
5. `Resolve`, routes and anything built from a resolved value, a readiness check included;
6. `Run`.

Mistakes surface in one phase each. Registration panics at the line that misused it: a constructor of the wrong shape, a duplicate binding, an `Alias` or `BindMany` whose types do not fit, a DI registration after `Finalize` or after shutdown began, and `Resolve` before `Finalize`. `Finalize` returns what only the whole graph reveals, joined in registration order: every missing dependency with its whole path, such as `di: missing dependency: *app.OrderService → *app.PaymentClient → *http.Client (not registered); provide *http.Client before Finalize`, and every cycle. Starting and serving return errors for I/O.

---

## Quick Start

This example shows the common Credo pattern:

1. load config
2. register typed config
3. register constructors
4. alias interfaces
5. finalize
6. resolve a controller
7. bind routes

```go
package main

import (
    "context"
    "log"
    "net/http"

    "github.com/credo-go/credo"
)

type DatabaseConfig struct {
    DSN string
}

type User struct {
    ID   string `json:"id"`
    Name string `json:"name"`
}

type DB struct {
    DSN string
}

type UserRepository interface {
    FindByID(ctx context.Context, id string) (*User, error)
}

type PgUserRepository struct {
    db *DB
}

func NewDB(cfg *DatabaseConfig) (*DB, error) {
    return &DB{DSN: cfg.DSN}, nil
}

func NewPgUserRepository(db *DB) *PgUserRepository {
    return &PgUserRepository{db: db}
}

func (r *PgUserRepository) FindByID(ctx context.Context, id string) (*User, error) {
    _ = ctx
    return &User{ID: id, Name: "demo"}, nil
}

type UserService struct {
    infra credo.Infra
    repo  UserRepository
}

func NewUserService(infra credo.Infra, repo UserRepository) *UserService {
    infra.Logger.Info("user service initialized")
    return &UserService{infra: infra, repo: repo}
}

func (s *UserService) FindByID(ctx context.Context, id string) (*User, error) {
    return s.repo.FindByID(ctx, id)
}

type UserController struct {
    svc *UserService
}

func NewUserController(svc *UserService) *UserController {
    return &UserController{svc: svc}
}

func (c *UserController) Show(ctx *credo.Context) error {
    user, err := c.svc.FindByID(ctx.Context(), ctx.Request().RouteParam("id"))
    if err != nil {
        return err
    }
    return ctx.Response().JSON(http.StatusOK, user)
}

func main() {
    app, err := credo.New()
    if err != nil {
        log.Fatal(err)
    }

    // Configuration is read during registration; Resolve waits for Finalize.
    dbCfg, err := app.GetConfig[DatabaseConfig]("databases.default")
    if err != nil {
        log.Fatal(err)
    }

    app.ProvideValue(&dbCfg)
    app.Provide[*DB](NewDB)
    app.Provide[*PgUserRepository](NewPgUserRepository)
    app.Alias[UserRepository, *PgUserRepository]()
    app.Provide[*UserService](NewUserService)
    app.Provide[*UserController](NewUserController)

    if err := app.Finalize(); err != nil {
        log.Fatal(err)
    }

    users := app.MustResolve[*UserController]()
    app.GET("/users/{id}", users.Show)

    if err := app.Run(); err != nil {
        log.Fatal(err)
    }
}
```

---

## Constructors

Credo supports two constructor shapes:

### Pure constructor injection

```go
func NewPgUserRepository(db *DB) *PgUserRepository {
    return &PgUserRepository{db: db}
}
```

### Constructor injection with `credo.Infra`

```go
func NewUserService(infra credo.Infra, repo UserRepository) *UserService {
    infra.Logger.Info("user service initialized")
    return &UserService{repo: repo}
}
```

Use `credo.Infra` when a type needs framework infrastructure such as logging. The recommended convention is to place it first.

---

## `credo.Infra`

`credo.Infra` carries cross-cutting infrastructure:

- `Logger`

The container creates `credo.Infra` automatically when a constructor asks for it. You do not register it yourself. This is framework-managed infrastructure, not a service locator: the boundary stays visible because `credo.Infra` appears in the constructor signature.

Important rules:

- `credo.Infra` is for infrastructure, not business dependencies
- config does not belong in `credo.Infra`
- request data does not belong in `credo.Infra`
- the logger is scoped per service automatically
- services can still be tested by constructing `credo.Infra` directly or by using `app.NewInfra(name)` outside DI

Tracing and metrics carriers are planned for the observability release. They are not part of the v0.1 `Infra` surface.

---

## `Provide` vs `ProvideValue`

Use `Provide` when Credo should create the singleton for you:

```go
app.Provide[*DB](NewDB)
app.Provide[*UserService](NewUserService)
```

Use `ProvideValue` when you already have the instance:

```go
cfg := &DatabaseConfig{DSN: "postgres://localhost/app"}
app.ProvideValue(cfg)
```

Typical `ProvideValue` use cases:

- typed config structs
- pre-built SDK clients
- test doubles
- values created by another bootstrap system

### Registration options

`Provide` and `ProvideValue` take registration options. Handing a value to DI and handing over its teardown are separate decisions, and the options make the second one explicit:

```go
app.Provide[*OrderConsumer](NewOrderConsumer, credo.Ingress())  // stops with the HTTP drain
app.ProvideValue[*pgxpool.Pool](sharedPool, credo.Borrowed())   // the caller starts and closes it
app.ProvideValue[*search.Client](searchClient, credo.Closer())  // the App calls Close
app.ProvideValue[UserRepository](fakeRepo, credo.Override())    // replaces the earlier binding
```

| Option | Accepted by | Effect |
| --- | --- | --- |
| `credo.Ingress()` | `Provide`, `ProvideValue`, `ProvideProtectedValue`, `Manage`, `OnStart`, `OnStop` | Places a component in the ingress tier, which stops first, concurrently with the HTTP drain. On a binding it requires a component at registration: a type that shows `Shutdown`, or a binding with `credo.Closer()` |
| `credo.Borrowed()` | `ProvideValue` | Keeps the binding and the value's `Ready`, and leaves starting and shutting the value down to the caller — a pool two Apps in one process share, a fixture a test suite reuses |
| `credo.Closer()` | `Provide`, `ProvideValue`, `ProvideProtectedValue` | Makes a binding whose type has `Close() error`, `Close()` or `Close(ctx) error` a component the App closes after its consumers; the context form receives the drain deadline |
| `credo.Override()` | `Provide`, `ProvideValue` | Replaces an earlier binding of the same type before `Finalize` |
| `credo.Named(name)` | `Manage` | Names a managed component in reports and readiness |

Each option is checked at the call, and misuse panics there: an option the call does not accept, a zero or repeated option, an empty name, `credo.Ingress()` on a binding that is not a component, `credo.Closer()` on a type without one of the three `Close` methods, on a type that already has `Shutdown` or beside `credo.Borrowed()`, and `credo.Override()` on a protected binding or without an earlier one — so an override that no longer matches the wiring fails instead of adding a binding nothing resolves. `Close` is never discovered without `credo.Closer()`: the method is too common to mean that the App owns the value. The value an override replaces never becomes the App's; whoever built it releases it.

`app.CanProvideValue[T]()` is a non-mutating preflight for helpers that should avoid work before a predictable registration failure. It checks only whether the container is finalized or `T` already has a direct registration. It does not reserve `T`: a registration made in between can still register or finalize before the real call, so the final publication remains authoritative — `ProvideValue` panics on a conflict, and `ProvideProtectedValue`'s error must still be handled.

### Protected integration bindings

Most application values should stay replaceable: use `ProvideValue`, especially when tests use overrides. A framework integration may also publish lifecycle or health state that must keep referring to the exact DI value. For that narrow case Credo exposes:

```go
if err := app.ProvideProtectedValue[Client](client); err != nil {
    return err
}

// Or atomically verify and protect a value resolved by the composition root.
if err := app.ProtectBinding[Client](client); err != nil {
    return err
}
```

After either path, `app.Replace[Client](other)` returns an error. Protection is about binding consistency only: it does not register stop hooks, health checks, aliases, or collection membership. `ProtectBinding[T]()` blindly protects an existing direct binding without resolving it and is idempotent. `ProtectBinding[T](expected)` is the CAS-style form: it atomically verifies, against `Replace`, that the already-created singleton is comparable and still equals expected before protecting it. An unresolved, non-comparable, changed, or multiply supplied expected value returns an error without adding protection. Both forms require an existing binding and must run before Finalize.

An integration that must read a value the composition root registered ahead of it uses `app.AdoptValue[T](validate)`. It reads the pre-built binding, runs `validate`, and atomically protects that same binding only when validation passes, so an invalid value (a typed-nil pointer) stays repairable with `Replace`. It never runs a constructor: a `T` registered through `Provide` is rejected with an explanatory error. No framework caller remains: stores and workers no longer bind anything into the container. For a plain "is it registered?" question use `app.Has[T]()`; it constructs, adopts and protects nothing.

### Replacing a binding

`Replace` overwrites an ordinary pre-built binding and hands the superseded instance back to you:

```go
old, existed, err := app.Replace[*sql.DB](newDB)
if err != nil {
    return err // protected binding, or container already finalized
}
if existed {
    defer old.Close() // the container no longer tracks or closes old
}
```

`existed` means an already-created instance was superseded — a value registered with `ProvideValue`. Replacing a constructor registration that never ran yields the zero value and `false`; Replace never runs the old constructor just to return its result. On success the container owns the new value and stops tracking the old one, so its cleanup is yours; if the old instance is a component (it has `Shutdown`), Credo logs `credo: Replace superseded a component; the caller now owns its shutdown` at Warn, naming the type. A rejected replacement changes nothing. `MustReplace` returns the same `(old, existed)` pair and panics on error. To swap a binding during registration, before `Finalize`, prefer `credo.Override()`, which fails when there is nothing to replace.

### No factory closures

`Provide`'s `constructor` parameter is typed `any` — Go cannot express "a function with arbitrary parameters returning `T`" — so a signature mistake panics at the `Provide` call, not at compile time. Credo does not offer a closure factory that resolves its own dependencies, and a constructor that captures `app` and calls `Resolve` inside its body is unsupported: those dependencies are invisible to `Finalize` validation, cycle detection and dependency-ordered shutdown, and constructors run only after `Finalize`, so the call cannot be scheduled against the graph. Declare dependencies as constructor parameters and let the container inject them.

Some Credo feature packages build on top of DI with package-level helpers instead of asking you to wire every internal singleton manually. Examples:

- `store.Register[*sqldb.DB](app)`, which names a binding you made with `ProvideValue` or `Provide` as a data store
- `workers.ContinuousProvided[*MyWorker]("name")` and `workers.ScheduledProvided[*MyWorker]("name", expr)` on the supervisor `worker.Use(app)` returns, for a worker the container provides

These helpers build on the DI container and attach framework behavior to it: `store.Register` adds a start-phase ping and a readiness probe to the store's binding and leaves its ownership and teardown to that binding, and each worker registration adds a component — a constructor over the worker's type — that the start phase builds after the worker's dependencies and that stops before them. Neither binds anything into the container. Use them before `app.Finalize()`; a `store.Register` or a provided worker whose type has no binding fails `Finalize`. See the [Data Access Guide](data-access.md) and [Worker Guide](worker.md) for the user-facing patterns.

---

## Interface Wiring with `Alias`

In most applications, constructors return concrete types while services depend on interfaces. `Alias` connects those two worlds without duplicate registration.

```go
type UserRepository interface {
    FindByID(ctx context.Context, id string) (*User, error)
}

type PgUserRepository struct{ /* ... */ }

app.Provide[*PgUserRepository](NewPgUserRepository)
app.Alias[UserRepository, *PgUserRepository]()
```

After that:

```go
func NewUserService(infra credo.Infra, repo UserRepository) *UserService {
    return &UserService{repo: repo}
}
```

`Alias` is the preferred way to program to interfaces while keeping constructor return types concrete.

It is also the one recipe for a value the App should start or ask for readiness. The App plans `Start`, `Ready` and the tier at registration, from the binding's type, before any constructor runs; a `Start` or `Ready` that only the built value has is neither called nor asked. Provide the concrete type, whose method set shows them, and `Alias` the interface the application uses:

```go
type OrderEvents interface {
    Publish(ctx context.Context, e OrderEvent) error
}

// *KafkaPublisher has Start, Ready and Shutdown.
app.Provide[*KafkaPublisher](NewKafkaPublisher)
app.Alias[OrderEvents, *KafkaPublisher]()
```

Registered as `app.Provide[OrderEvents](NewKafkaPublisher)` instead, the publisher would still be shut down — the App finds `Shutdown` on the value it owns ([Shutdown and Lifecycle](#shutdown-and-lifecycle)) — but never started or asked. A type with `Start` and without `Shutdown` is not a component and is never started.

---

## Ordered Interface Collections with `BindMany`

Use `BindMany` when a component needs an ordered set of implementations rather than one default interface implementation.

Typical examples:

- notification senders
- startup hooks
- plugin chains
- policy evaluators
- event subscribers

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

app.Provide[*EmailSender](NewEmailSender)
app.Provide[*InAppSender](NewInAppSender)

app.BindMany[Sender, *EmailSender]()
app.BindMany[Sender, *InAppSender]()

app.Provide[*SenderRegistry](NewSenderRegistry)
```

You can also resolve the same collection explicitly:

```go
senders := app.MustResolveAll[Sender]()
```

Important rules:

- `BindMany` targets an interface `I`
- `T` must already be registered and must implement `I`
- binding order is preserved
- `Alias` and `BindMany` are independent
- `ResolveAll[I]` returns `[]I{}` when no bindings exist
- constructor injection of `[]I` also receives `[]I{}` when no bindings exist

This makes collection dependencies explicit while avoiding manual bootstrap registries built from repeated `Resolve` calls.

---

## Multiple Instances Of The Same Type

Credo DI keys services by Go type. If you need two instances of the same concrete type, register semantic wrapper types instead of trying to register the same type twice.

This is especially common with data stores:

```go
type PrimaryDB struct{ *sqldb.DB }
type AnalyticsDB struct{ *sqldb.DB }
```

Bind each wrapper once and register it by its type with `store.Register[PrimaryDB](app, store.WithName("primary"))`, then inject `PrimaryDB` or `AnalyticsDB` explicitly where needed; an interface view is an `Alias`, never a second binding ([Data Access Guide](data-access.md#multiple-databases)). If those wrappers embed `*sqldb.DB`, `store/sqldb` keeps transaction context scoped per database instance, so same-type Bun connections do not collide implicitly.

### Wrappers and resource identity

A wrapper and the handle it wraps can be one resource held through two bindings, and one resource has one teardown. The App keys components by **resource identity**: the token a value's `ResourceIdentity() any` method returns (`credo.ResourceIdentifier`); without the method, a comparable value — a pointer, or a struct over one — is its own identity. Values that share an identity are shut down once, when the last holder retires, so the consumers of every holder stop first: one pointer under several bindings, a wrapper and the handle it embeds, two wrappers over one handle.

- **A wrapper that embeds a handle which identifies itself inherits that identity.** `*sqldb.DB` returns itself from `ResourceIdentity()`, so `PrimaryDB{db}` and `db` are one database: bound both ways, it is closed once, after the consumers of both. Two wrappers over two handles are two databases, each closed once.
- **A wrapper over a handle that does not identify itself forwards the identity with one method.** Without it, the wrapper value would be a resource of its own:

  ```go
  type ProductIndex struct{ *search.Client }

  func (p ProductIndex) ResourceIdentity() any { return p.Client }

  app.ProvideValue[*search.Client](client, credo.Closer())
  app.ProvideValue[ProductIndex](ProductIndex{client})
  ```

  The client is closed once, after the consumers of `*search.Client` and of `ProductIndex`.
- **A wrapper that releases state of its own shares nothing.** Only a value without state of its own to release may share an identity, because the App runs one teardown for the resource, and Go cannot tell a `Shutdown` that a wrapper inherits from one it declares. Such a wrapper holds the handle in a named field, so it neither inherits the handle's identity nor its methods; its own `Shutdown` releases its state and the handle, which is then the wrapper's alone:

  ```go
  type ReportingDB struct {
      db     *sqldb.DB // named field: no shared identity
      buffer *AuditBuffer
  }

  func (r *ReportingDB) Shutdown(ctx context.Context) error {
      return errors.Join(r.buffer.Flush(ctx), r.db.Shutdown(ctx))
  }
  ```

The holders of one resource that have a teardown agree on its kind: `Shutdown`, or `Close` through `credo.Closer()`. A client bound with `credo.Closer()` beside a holder whose teardown is `Shutdown` is refused — the later `ProvideValue` panics when both values are given, and otherwise the construction of the holder built later fails — with both bindings and both remedies named: drop `credo.Closer()`, or bind the holder that has `Shutdown` under a type that shows `Close` and not `Shutdown`, with `credo.Closer()`. Among holders of one kind, the one registered first runs the teardown, whichever is built first. A holder without a teardown, such as `ProductIndex` above, takes no part in the choice, and the resource still waits for its consumers. A resource handed to `app.Manage` that a `ProvideValue` binding already holds, or handed to `Manage` twice, panics; a `ResourceIdentity()` that panics or returns nil or a non-comparable token panics at `ProvideValue` and `Manage` and fails a constructor's construction.

For the full multi-database pattern, see the [Data Access Guide](data-access.md).

---

## `Finalize`

`app.Finalize()` does two things:

1. freezes the container
2. validates the dependency graph

After `Finalize`:

- `Provide`, `ProvideValue`, `Alias`, `BindMany` and `Manage` panic at the call
- `Replace` and `AdoptValue` return an error
- `Resolve` and `ResolveAll` become available (before `Finalize` they panic and run no constructor; after a failed `Finalize` they return its error)

`Finalize` is DI-only. Routes, middleware, hooks and renderers stay open until the App prepares to serve (the first request or `Run`), so you can resolve a controller after `Finalize` and still bind its routes.

Validation catches startup problems early and reports all of them at once, joined in registration order with the same text on every run:

- missing dependencies, each with its whole path from the registration that needs it
- dependency cycles, every one of them
- constructors that take a `context.Context`
- internal components that depend on an ingress one, each with its path

`Run()` and `RunContext()` call `Finalize()` implicitly, but that safeguard cannot precede a `Resolve` your composition root has already executed. Explicit, error-checked finalize between the last registration and the first `Resolve` is the recommended pattern:

```go
if err := app.Finalize(); err != nil {
    log.Fatal(err)
}
```

---

## `Resolve` and `ResolveAll`

`Resolve` retrieves a singleton from the container:

```go
svc, err := app.Resolve[*UserService]()
if err != nil {
    return err
}
```

Credo's **recommended** use of `Resolve` is bootstrap/composition-root code:

- reading `credo.RawConfig` during startup
- resolving a controller before route registration
- resolving a top-level service in `main()`

Runtime `Resolve` is technically allowed because the API is public, but it is not Credo's primary application pattern. Stop hooks (`OnStop`) and `Shutdown` methods must not resolve at all: capture the dependency in the hook closure at registration time, or take it as a constructor parameter so that the dependency order stops the component first.

A constructor runs exactly once, at first resolution, and its outcome is final. If it panics, every caller receives the same `*credo.DIPanicError` (constructor type, original panic value, stack), and `MustResolve` panics with that error as its payload — do not recover and retry. Once the drain begins stopping the internal tier's components, `Resolve` returns an error matching `credo.ErrDIClosed`. A panic in a component's `Start` or `Shutdown` is a `*credo.DIPanicError` too, with the phase `credo.DIPanicStart` or `credo.DIPanicShutdown`.

`ResolveAll[I]` follows the same guidance: use it mainly in bootstrap/setup code when you explicitly need the whole ordered collection. Inside normal application code, prefer constructor injection of `[]I`.

Recommended:

```go
controller := app.MustResolve[*UserController]()
app.GET("/users/{id}", controller.Show)
```

Not recommended as the default style:

```go
app.GET("/users/{id}", func(ctx *credo.Context) error {
    svc, err := app.Resolve[*UserService]()
    if err != nil {
        return err
    }
    _, err = svc.FindByID(ctx.Context(), ctx.Request().RouteParam("id"))
    return err
})
```

Why Credo discourages request-time `Resolve` as the main style:

- dependencies are no longer visible in the handler's structure
- it moves toward service locator usage
- constructor injection becomes less meaningful

Credo does **not** provide `Context.Resolve`, which keeps this as an advanced, explicit choice instead of a framework-default pattern.

---

## No Request Scope

Credo intentionally does not have `RequestScoped` DI.

In Go, request-bound state belongs in `context.Context` and `*credo.Context`, not in the container.

Put these in request context, middleware state, or method parameters:

- request ID
- authenticated user
- locale
- trace/span context
- tenant
- per-request authorization data
- current transaction

Put these in DI:

- long-lived clients
- repositories
- services
- controllers
- typed config

This keeps the container simple and matches normal Go application structure.

---

## DI Is Optional

Credo does not require DI.

You can wire everything manually:

```go
db, err := NewDB(&DatabaseConfig{DSN: dsn})
if err != nil {
    log.Fatal(err)
}

repo := NewPgUserRepository(db)
svc := NewUserService(app.NewInfra("UserService"), repo)
ctrl := NewUserController(svc)

app.GET("/users/{id}", ctrl.Show)
```

Use manual wiring when:

- the application is small
- the dependency graph is simple
- you prefer direct construction
- you do not need container validation or lifecycle management

Use `app.NewInfra(name)` to get a scoped Infra with Logger from the app's base infrastructure. For tests without an App instance, construct `credo.Infra` directly with the fields your code uses.

---

## Shutdown and Lifecycle

All DI-managed objects are singletons. A singleton that needs cleanup is a **component**: it has a `Shutdown` method, the one method of `credo.Component`:

```go
type Cache struct{ /* ... */ }

func (c *Cache) Shutdown(ctx context.Context) error {
    return c.flush(ctx) // ctx carries the drain deadline
}
```

The App shuts a component down after its consumers: a consumer stops before the singletons it received as constructor parameters, whatever the registration order, and values without edges stop in reverse registration order. A component with neither `Start` nor `Ready` stays lazy and is shut down only if it was built. Two optional capabilities, planned from the binding's type at registration, let the App do more:

- **`Start(ctx) error`** (`credo.Starter`): the start phase builds the component and calls `Start` after the `Start` of everything it depends on, before the App accepts requests. `Start` returns once the component is usable and never blocks for its lifetime; its context ends when it returns, so work that outlives it runs on a goroutine whose context derives from `context.WithoutCancel(ctx)`, stopped by `Shutdown`. A `Start` that returns an error has released what it opened and is not shut down.
- **`Ready(ctx) error`** (`credo.Readier`): `/ready` asks it under the component's name, from the value the start phase built, with no DI resolution per probe. A failure after `Start` has returned is reported here; no component ends the App.

```go
type Consumer struct{ /* ... */ }

func NewConsumer(infra credo.Infra, orders *OrderService) *Consumer { /* ... */ }

func (c *Consumer) Start(ctx context.Context) error {
    if err := c.client.Connect(ctx); err != nil {
        return err
    }
    loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
    c.cancel = cancel
    c.done = make(chan struct{})
    go c.loop(loopCtx) // closes c.done when it returns
    return nil
}

func (c *Consumer) Ready(ctx context.Context) error { return c.client.Ping(ctx) }

func (c *Consumer) Shutdown(ctx context.Context) error {
    if c.cancel != nil {
        c.cancel()
        select {
        case <-c.done:
        case <-ctx.Done():
            return ctx.Err()
        }
    }
    return c.client.Close()
}

app.Provide[*Consumer](NewConsumer, credo.Ingress())
```

`Shutdown` runs on a component that was built but never started — the rollback of a failed start stops it too — so it tolerates a `Start` that never ran.

### Two tiers

Every component belongs to a tier. The **ingress** tier is where work enters the process — the WebSocket server, a scheduled worker, a consumer of an external queue — and is chosen with `credo.Ingress()` (a worker's tier is a field of its configuration). Everything else is **internal**, the default. The start phase starts the internal tier in dependency order, then the ingress tier, then the listener accepts. The drain mirrors it: the ingress tier stops first, concurrently with the HTTP drain, with its components that no edge orders stopping concurrently; the internal tier then stops one component at a time in reverse dependency order.

No dependency crosses the tiers backwards: an internal component that depends on an ingress one, directly or through bindings that are not components, fails `Finalize` with the path and both remedies — declare the dependent ingress, or split the ingress component so that what internal components use is an internal part. A producer that hands work to a consumer component takes the consumer (or a handle it owns) as a constructor parameter, and "consumers before dependencies" then stops the producer first; there is no separate ordering API.

### A value whose binding's type does not show `Shutdown`

Ownership, not the view consumers take, decides the teardown. A value the App owns whose `Shutdown` only the built value has — bound as an interface without the method, say — is still a component and is shut down after its consumers. Because `Finalize` cannot see it, it is internal, and if it depends on an ingress component its construction fails with the path and the remedies. Its `Start` and `Ready` are neither called nor asked, since they are planned from the binding's type: to have the App start or ask it, provide the concrete type and `Alias` the interface ([Interface Wiring with `Alias`](#interface-wiring-with-alias)). To keep the teardown of a value you built with yourself, bind it with `app.ProvideValue[T](v, credo.Borrowed())`.

A value that a constructor builds over a borrowed resource stays the caller's as well, even when it has `Shutdown`, unless its binding carries `credo.Closer()`. A teardown that is neither `Shutdown` nor a `Close` — a client's `Disconnect(ctx)`, a `Drain` that must be waited for — belongs to a type that embeds the client and implements `Shutdown`, registered as the binding itself, so its constructor's parameters give the teardown's own dependencies their edges.

### Components that are not bindings, and hooks

| Mechanism | When to use | Order |
| --- | --- | --- |
| A DI singleton with `Shutdown` (optionally `Start`, `Ready`) | Anything the application wires through DI that holds a resource | Its tier, after its consumers; started before them |
| `credo.Closer()` on a binding | A client whose teardown is `Close` | Like a component with `Shutdown` |
| `app.Manage(v, opts...)` | A value with `Shutdown` built outside DI, a constructor over DI parameters that should not be injectable, a mounted child App | Its tier; a value has no edges and stops in reverse registration order, a constructor has its parameters' edges |
| `app.OnStart(fn, opts...)` | A leaf action at startup, such as warming a cache | FIFO after its tier's components have started |
| `app.OnStop(fn, opts...)` | A leaf action at shutdown that may still use its tier's components | LIFO before its tier's components stop |

`Manage` names a component with `credo.Named` (default: its type name; a duplicate name panics). A hook is an anonymous component of a tier, internal unless `credo.Ingress()` says otherwise; anything with a teardown of its own is a component, not a hook, and process-level cleanup that must outlive every component belongs after `Run` returns.

During graceful shutdown the full sequence is:

1. Withdraw readiness (`/ready` answers 503 `shutting_down`).
2. Concurrently with the HTTP drain, stop the ingress tier: ingress `OnStop` hooks in LIFO order, then ingress components.
3. Wait for an in-flight reload.
4. Stop the internal tier: internal `OnStop` hooks in LIFO order, then internal components in reverse dependency order. Once the internal components begin to stop, `Resolve` returns an error matching `credo.ErrDIClosed`.

All steps share one deadline — `WithShutdownTimeout` for a signal or a cancelled `RunContext` context, the caller's for `app.Shutdown(ctx)` — and spend it in order. A `Shutdown`, `Close` or hook that has not returned at the deadline is abandoned: the components it depends on are not stopped, since it may still use them, and it is reported with every component it kept open. A `Shutdown` that returns an error promptly is reported, and its dependencies are still stopped. Every `Start`, `Shutdown` and hook is panic-isolated. A construction still running when the drain reaches it blocks its dependencies and is shut down in order when it completes; one that completes after the deadline gets one bounded late cleanup, logged and not reported. Failures are inspectable with `errors.AsType[*credo.LifecycleError]` through the joined `Shutdown` error: each entry names the component, its tier, its phase (`start` or `shutdown`) and its outcome (`failed`, `panicked`, `abandoned`, `kept_open`).

`app.Shutdown` also works on an App that was never run: in the `building` state it freezes registrations and runs the same teardown without an HTTP drain, so a composition root can clean up registered resources after a later bootstrap failure. An App served through `ServeHTTP` by a server you own — an external `http.Server`, or `httptest` in a test — is started with `app.Start(ctx)` (`testutil.Start(t, app)` in tests) when it has anything to start: a component with `Start` or `Ready` (a WebSocket server among them), a constructor handed to `Manage`, a store registered with `store.Register`, `UseI18n`, a worker, or a start hook; until then `ServeHTTP` panics. The server's owner drains it before calling `app.Shutdown`, because the internal tier stops after the HTTP drain only if that drain has happened.

---

## Config Changes and Singletons

DI singletons are built once and never rebuilt — a [reload](configuration.md#reloading-configuration) does not re-run constructors or replace bindings. A typed config struct injected at startup is therefore a startup snapshot. When a service needs a value that can change at runtime, keep the live value inside the service behind an atomic holder and let an `OnConfigChange[T]` subscriber swap it:

```go
type RateLimiter struct {
    limits atomic.Pointer[Limits]
}

func NewRateLimiter(infra credo.Infra, initial *Limits) *RateLimiter {
    rl := &RateLimiter{}
    rl.limits.Store(initial)
    return rl
}

func (rl *RateLimiter) Apply(next Limits) { rl.limits.Store(&next) }

// Composition root: the subscriber owns the swap; handlers only ever Load().
rl := app.MustResolve[*RateLimiter]()
app.OnConfigChange("limits", func(ctx context.Context, next Limits) error {
    rl.Apply(next)
    return nil
})
```

Resources that cannot be swapped atomically — a database pool built from a changed DSN, a listener on a new port — are restart-only by design; the reload logs them as `restart required`.

---

## Testing

Most unit tests do not need the container at all. Construct the type directly:

```go
repo := &FakeUserRepository{}
svc := NewUserService(credo.Infra{Logger: slog.Default()}, repo)
```

Use the container in tests when you want to verify wiring:

```go
app, err := credo.New()
if err != nil {
    t.Fatal(err)
}

app.ProvideValue(&DatabaseConfig{DSN: "test"})
app.Provide[*UserService](NewUserService)

if err := app.Finalize(); err != nil {
    t.Fatal(err)
}

svc := app.MustResolve[*UserService]()
_ = svc
```

The `testutil` package builds a hermetic test App: `testutil.NewApp(t, testutil.WithWiring(wire), testutil.WithOverride[UserRepository](fakeRepo))`. `WithOverride` replaces a binding through `credo.Override()` and panics when the wiring has no earlier binding of that type; adding a binding is `WithWiring`'s job. An App served through `httptest` that has anything to start is started with `testutil.Start(t, app)`, which shuts it down when the test ends:

```go
app := testutil.NewApp(t, testutil.WithWiring(wire))
testutil.Start(t, app)
srv := httptest.NewServer(app)
t.Cleanup(srv.Close) // drained before the components stop
```

Good rule:

- test behavior with direct construction
- test wiring with the container

---

## Common Mistakes

### Using DI for request state

Do not try to inject request ID, auth user, or transactions through DI. Use `*credo.Context` / `context.Context`.

### Putting config into `credo.Infra`

RawConfig should be unmarshaled into typed structs and registered with `ProvideValue`.

### Resolving before `Finalize`

`Resolve` panics until `app.Finalize()` has run; a composition root that resolves controllers before `Run` must finalize first, and reads configuration during registration with `app.GetConfig[T]` rather than resolving `credo.RawConfig`. `Run()` finalizes implicitly only as a safeguard, and explicit `app.Finalize()` gives earlier feedback and clearer startup failures.

### Resolving inside stop hooks

Stop hooks and `Shutdown` methods run during teardown, when construction is the last thing you want. Resolve during bootstrap and capture the value in the hook closure, or make the dependency a constructor parameter.

### Starting work in a constructor or an `OnStart` goroutine

A constructor wires; it never relies on a dependency having started, because the bootstrap order resolves controllers — and most of the graph with them — before `Run`. I/O that needs a running dependency belongs in `Start`. A goroutine launched from an `OnStart` hook on the hook's context stops as soon as the hook returns: background work belongs to a component whose `Start` launches it and whose `Shutdown` stops it, or to a [worker](worker.md).

### Overusing `Resolve`

Prefer constructor injection. Reach for `Resolve` mainly in bootstrap/setup code, not as the primary way handlers find services.

### Returning interfaces from every constructor

Usually return a concrete type and use `Alias` when another component depends on an interface.

---

## Recommended Pattern

For medium and large Credo applications, the default shape should be:

1. load config
2. unmarshal typed config at the module boundary
3. register config with `ProvideValue`
4. register concrete constructors with `Provide`
5. connect single implementations with `Alias` and collections with `BindMany` when needed
6. mount features and register satellites — `UseI18n`, `UseHealth`, stores, workers
7. call `app.Finalize()`
8. resolve top-level controllers/services needed for startup wiring
9. start the app

This keeps dependency graphs explicit, startup failures early, and runtime behavior simple.

---

## Related Documents

- [Configuration Guide](configuration.md)
- [Data Access Guide](data-access.md)
- [DI Container Spec](../specs/container.md)
- [ADR-004](../adr/004-dependency-injection-and-infra.md)
- [ADR-005](../adr/005-configuration-architecture.md)
- [Lifecycle Spec](../specs/lifecycle.md)
- [ADR-024: Lifecycle Components](../adr/024-lifecycle-components.md)
