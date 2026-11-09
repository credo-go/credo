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
         Resolve / ResolveAll   (the first call finalizes)
                    ->
                  Run
```

- `Provide`: register a constructor, bound as the type it returns
- `ProvideValue[T]`: register a pre-built singleton
- registration options — `credo.Ingress()`, `credo.Borrowed()`, `credo.CloseOnShutdown()`, `credo.Override()`: decide a binding's tier, its owner, its teardown, or that it replaces an earlier binding ([Registration options](#registration-options))
- `app.Manage(v, opts...)`: add a component — a value with `Shutdown`, or a constructor — that is not a binding ([Shutdown and Lifecycle](#shutdown-and-lifecycle))
- `Has[T]`: check whether `T` is registered, without constructing anything ([Checking for an optional binding](#checking-for-an-optional-binding))
- `Alias[I, T]`: resolve an interface `I` as the singleton of concrete type `T`; an alias is a binding of `I`
- `BindMany[I, T]`: add a concrete singleton `T` to the ordered collection for interface `I`
- `Resolve[T]`: retrieve a fully wired singleton; the first resolving call freezes registrations and validates the dependency graph
- `app.Finalize()`: an optional early check that freezes registrations and validates the graph before anything is resolved, returning the whole report as an error
- `ResolveAll[I]`: retrieve the ordered collection bound for interface `I`

That is the whole DI surface: registration calls return nothing and panic on misuse, so there are no `Must*` registration twins, and `MustResolve`/`MustResolveAll` are the resolve conveniences for a composition root that cannot continue without the value.

Constructors never run against an unvalidated graph. The first `Resolve`, `ResolveAll`, `MustResolve` or `MustResolveAll` finalizes the container — it freezes registrations and validates the whole graph — then resolves; `Run`, `App.Start` and the first request do the same for an App that resolved nothing. A failed validation is that call's error, `MustResolve` panics with the whole report, and every later `Resolve` returns the same error. `Has` and the configuration reads (`GetConfig`, `MustGetConfig`, `ConfigExists`) never finalize. `app.Finalize()` stays as an optional early check that returns the whole report as an error, useful in tests or before serving ([`Finalize`](#finalize)).

Bootstrap is sequential: registration calls come from the goroutine that builds the App, before it runs, and are not safe for concurrent use; an application that registers from several goroutines serializes the calls itself. The whole bootstrap follows one order, every satellite included:

1. configuration — `credo.New()`, or `credo.New(credo.WithRawConfig(raw))`;
2. `Provide` and `ProvideValue`, with `Alias` and `BindMany`;
3. feature mounts and satellite registrations, in any order among themselves — `UseI18n`, `UseHealth`, workers, `Manage`;
4. `Resolve`, routes and anything built from a resolved value, a readiness check included — the first resolving call finalizes the container;
5. `Run`.

Mistakes surface in one phase each. Registration panics at the line that misused it: a constructor of the wrong shape, a duplicate binding, an `Alias` or `BindMany` whose types do not fit, a DI registration after the container was finalized or after shutdown began. A registration after the freeze names what finalized the container, where, and the calls that led there:

```text
credo: App.Provide[*app.Mailer]: called after the container was finalized by Resolve[*app.Hub] at /src/wiring/wiring.go:17 (called from /src/main.go:31); register before that call
```

The site is the file path the binary records, with the line. Finalizing returns what only the whole graph reveals, joined in registration order: every missing dependency with its whole path, such as `di: missing dependency: *app.OrderService → *app.PaymentClient → *http.Client (not registered); provide *http.Client before Finalize`, and every cycle — the error of `Finalize`, or of the first resolving call when nothing called `Finalize`. Starting and serving return errors for I/O.

---

## Quick Start

This example shows the common Credo pattern:

1. load config
2. register typed config
3. register constructors
4. alias interfaces
5. resolve a controller, which validates the graph
6. bind routes

```go
package main

import (
    "context"
    "log"
    "net/http"

    "github.com/credo-go/credo"
    "github.com/credo-go/credo/config"
)

type DatabaseConfig struct {
    DSN string
}

var DatabaseSection = config.Section[DatabaseConfig]("databases.default")

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

func (h *UserController) Show(c *credo.Context) error {
    user, err := h.svc.FindByID(c.Context(), c.Request().RouteParam("id"))
    if err != nil {
        return err
    }
    return c.Response().JSON(http.StatusOK, user)
}

func main() {
    // 1. Configuration.
    app, err := credo.New()
    if err != nil {
        log.Fatal(err)
    }

    // Configuration is read during registration: GetConfig never finalizes.
    dbCfg, err := app.GetConfig(DatabaseSection)
    if err != nil {
        log.Fatal(err)
    }

    // 2. Provide, with Alias for the interface consumers take.
    app.ProvideValue(&dbCfg)
    app.Provide(NewDB)
    app.Provide(NewPgUserRepository)
    app.Alias[UserRepository, *PgUserRepository]()
    app.Provide(NewUserService)
    app.Provide(NewUserController)

    // 3. Feature mounts and satellites would go here (UseHealth, workers).

    // 4. Resolve the controller and bind its routes. The first resolving call
    // validates the whole graph and panics here with every problem it finds.
    users := app.MustResolve[*UserController]()
    app.GET("/users/{id}", users.Show)

    // 5. Run.
    if err := app.Run(); err != nil {
        log.Fatal(err)
    }
}
```

---

## Modules

An application split into parts — a package per domain, each with its services and its handlers — gives each part a `credo.Module`, and the composition root passes every module to one `app.UseModules` call:

```go
// Module is a part of an application: its services, then its handlers.
type Module interface {
    Services(app *App) // Provide, ProvideValue, Manage, workers
    Handlers(app *App) // routes and other entry points; may Resolve
}
```

`UseModules` runs every module's `Services` in order, then every module's `Handlers`. The first `Resolve` inside a `Handlers` method therefore comes after every module has registered its services, so a module's handlers can resolve a service another module provides, whichever module comes first, and the implicit `Finalize` that `Resolve` triggers sees the whole graph:

```go
// users/module.go
package users

type Module struct{}

func (Module) Services(app *credo.App) {
    app.Provide(NewPgUserRepository)
    app.Alias[UserRepository, *PgUserRepository]()
    app.Provide(NewUserService)
    app.Provide(NewUserController)
}

func (Module) Handlers(app *credo.App) {
    users := app.MustResolve[*UserController]() // after every module's services
    users.Routes(app.Group("/users"))
}

// Routes registers the controller's routes on its group.
func (h *UserController) Routes(g *credo.Group) {
    g.GET("/{id}", h.Show)
}
```

```go
// billing/module.go
package billing

type Module struct{}

func (Module) Services(app *credo.App) {
    app.Provide(NewStripeGateway)
    app.Alias[PaymentGateway, *StripeGateway]()
    app.Provide(NewInvoiceService) // takes *users.UserService from the users module
    app.Provide(NewInvoiceController)
}

func (Module) Handlers(app *credo.App) {
    app.MustResolve[*InvoiceController]().Routes(app.Group("/invoices"))
}
```

```go
// main.go: the composition root.
func main() {
    app, err := credo.New() // 1. configuration
    if err != nil {
        log.Fatal(err)
    }

    db, err := sqldb.Open(dbConfig)
    if err != nil {
        log.Fatal(err)
    }
    app.ProvideValue(db) // shared infrastructure, before the modules
    app.UseHealth()

    app.UseModules(users.Module{}, billing.Module{}) // services of both, then handlers of both

    if err := app.Run(); err != nil {
        log.Fatal(err)
    }
}
```

The rules:

- **Both methods are required.** A module with nothing for one phase leaves that method empty; a misspelled method is then a compile error, not a phase that silently never runs.
- **`UseModules` is called once.** A second call would register its services after the first call's handlers had finalized the container, so it panics: pass every module to one call. A module composed of others calls their `Services` and `Handlers` from its own.
- **Registrations belong in `Services`.** A `Provide`, a `Manage` or a worker registration from a `Handlers` method after a `Resolve` panics like every late registration, naming the call that finalized the container. Shared infrastructure registers before `UseModules`, or in a module of its own placed first.
- **A controller's routes are a method its module's `Handlers` calls on a group.** There is no controller registration of its own: the module is the unit the application composes.

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
- the logger is scoped automatically: it writes `component=<name>`, the name of the binding the constructor builds (`*users.Service`)
- services can still be tested by constructing `credo.Infra` directly or by using `app.NewInfra(name)` outside DI

Tracing and metrics carriers are planned for the observability release. They are not part of the current `Infra` surface.

---

## `Provide` vs `ProvideValue`

Use `Provide` when Credo should create the singleton for you:

```go
app.Provide(NewDB)
app.Provide(NewUserService)
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
app.Provide(NewOrderConsumer, credo.Ingress())                          // stops with the HTTP drain
app.ProvideValue[*pgxpool.Pool](sharedPool, credo.Borrowed())           // the caller starts and closes it
app.ProvideValue[*search.Client](searchClient, credo.CloseOnShutdown()) // the App calls Close
app.ProvideValue[UserRepository](fakeRepo, credo.Override())            // replaces the earlier binding
```

| Option | Accepted by | Effect |
| --- | --- | --- |
| `credo.Ingress()` | `Provide`, `ProvideValue`, `Manage`, `OnStart`, `OnStop` | Places a component in the ingress tier, which stops first, concurrently with the HTTP drain. On a binding it requires a component at registration: a type that shows `Shutdown`, or a binding with `credo.CloseOnShutdown()` |
| `credo.Borrowed()` | `ProvideValue` | Keeps the binding and the value's `Ready`, and leaves starting and shutting the value down to the caller — a pool two Apps in one process share, a fixture a test suite reuses |
| `credo.CloseOnShutdown()` | `Provide`, `ProvideValue` | Makes a binding whose type has `Close() error`, `Close()` or `Close(ctx) error` a component the App closes after its consumers; the context form receives the drain deadline |
| `credo.Override()` | `Provide`, `ProvideValue`, `Alias` | Replaces an earlier binding of the same type, or an alias of it, before the container is finalized; on `Alias`, points the interface at another type |
| `credo.ComponentName(name)` | `Provide`, `ProvideValue`, `Manage` | Names the component in logs, `/ready` and lifecycle errors (default: its type, such as `*sqldb.DB`); it is not a DI key |
| `credo.OptionalReadiness()` | `Provide`, `ProvideValue`, `Manage` | Reports the component in `/ready` without letting it gate readiness: a failing entry shows `down`, and `/ready` answers 200 `degraded` while every gating entry is up. `Start` and `Shutdown` are unchanged |

Each option is checked at the call, and misuse panics there: an option the call does not accept, a zero or repeated option, a `credo.ComponentName` name the health-name rules refuse or that another component has, `credo.Ingress()` or `credo.ComponentName` on a binding that is not a component, `credo.OptionalReadiness()` on a registration that plans no readiness entry (a type without `Ready`, or one that is neither a component nor borrowed), `credo.CloseOnShutdown()` on a type without one of the three `Close` methods, on a type that already has `Shutdown` or beside `credo.Borrowed()`, and `credo.Override()` without an earlier one — so an override that no longer matches the wiring fails instead of adding a binding nothing resolves. `Close` is never discovered without `credo.CloseOnShutdown()`: the method is too common to mean that the App owns the value. The value an override replaces never becomes the App's; whoever built it releases it.

### Replacing a binding

A binding is replaced at registration, before the container is finalized, with `credo.Override()` on `Provide` or `ProvideValue` — in a composition root that swaps one implementation for another, or in a module that lets the application substitute a default:

```go
app.Provide(NewDB)                                     // the default wiring
app.ProvideValue[*sql.DB](replicaDB, credo.Override()) // replaces it; NewDB never runs
```

The override panics when there is no earlier binding of the type, so an override that no longer matches the wiring fails instead of adding a binding nothing resolves. Since constructors run only after the container is finalized, an override never supersedes an instance the App built, and the value it replaces never becomes the App's: the App neither starts nor shuts it down. A value you built, bound and then overrode is yours to release. In tests, `testutil.WithOverride[T](v)` applies `credo.Override()` to the App the test builds and is equally strict ([Testing](#testing)).

There is no runtime replacement: once the container is finalized the graph is fixed, and every binding the App resolves is the one it validated.

### Checking for an optional binding

`app.Has[T]()` reports whether `T` is registered, directly or through `Alias`, for a composition root that asks whether an optional module was wired. It never runs a constructor, says nothing about whether the instance is healthy or usable, and reserves nothing: its result is a snapshot of the registrations made so far, and an override after it still replaces the binding.

```go
if !app.Has[*AuditLog]() {
    app.Provide(NewNoopAuditLog)
}
```

### No factory closures

`Provide`'s `constructor` parameter is typed `any` — Go cannot express "a function with arbitrary parameters returning `T`" — so a signature mistake panics at the `Provide` call, not at compile time. Credo does not offer a closure factory that resolves its own dependencies, and a constructor that captures `app` and calls `Resolve` inside its body is unsupported: those dependencies are invisible to `Finalize` validation, cycle detection and dependency-ordered shutdown, and constructors run only after the container is finalized, so the call cannot be scheduled against the graph. Declare dependencies as constructor parameters and let the container inject them.

Some Credo subsystems build on top of DI with helpers instead of asking you to wire every internal singleton manually. Examples:

- `app.Workers().Continuous("name", NewMyWorker)` and `app.Workers().Scheduled("name", expr, NewMyWorker)` on the App's worker registry, which take a worker's constructor, or a run function such as `func(ctx context.Context, db *sqldb.DB) error`, whose parameters the container supplies

These helpers build on the DI container and attach framework behavior to it: each worker registration adds components that are not bindings — the worker's loop, and the value its constructor builds — which the start phase builds after the worker's dependencies and which stop before them, and binds nothing into the container. Use them before the first `Resolve`, which finalizes the container; a parameter nothing provides fails `Finalize` with its path. A worker that handlers also call is bound with `app.Provide(NewMyWorker)` and registered through its `Run`, `app.Workers().Continuous("name", (*MyWorker).Run)`, so both share the binding's one singleton. See the [Worker Guide](worker.md) for the user-facing patterns. A database needs no helper: `*sqldb.DB` is a component, so its binding alone pings it at start, reports it in `/ready` and shuts it down after its consumers ([Data Access Guide](data-access.md#what-a-bound-database-does)).

---

## Interface Wiring with `Alias`

In most applications, constructors return concrete types while services depend on interfaces. `Alias` connects those two worlds without duplicate registration.

```go
type UserRepository interface {
    FindByID(ctx context.Context, id string) (*User, error)
}

type PgUserRepository struct{ /* ... */ }

app.Provide(NewPgUserRepository)
app.Alias[UserRepository, *PgUserRepository]()
```

After that:

```go
func NewUserService(infra credo.Infra, repo UserRepository) *UserService {
    return &UserService{repo: repo}
}
```

`Alias` is the preferred way to program to interfaces while keeping constructor return types concrete.

It is also the one recipe for a value the App should start or ask for readiness. `Provide` binds the type the constructor is declared to return, and the App plans `Start`, `Ready` and the tier at registration, from that type, before any constructor runs. Declare the constructor to return the concrete type, whose method set shows them, and `Alias` the interface the application uses:

```go
type OrderEvents interface {
    Publish(ctx context.Context, e OrderEvent) error
}

// *KafkaPublisher has Start, Ready and Shutdown.
app.Provide(NewKafkaPublisher)
app.Alias[OrderEvents, *KafkaPublisher]()
```

Were `NewKafkaPublisher` declared to return `OrderEvents` instead, the binding would be the interface, which hides the publisher's `Start` and `Ready`, and the start would fail once the publisher is built, naming that remedy; `ProvideValue[OrderEvents](publisher)` panics at the call for the same reason. The refusal holds only when no other registration of the same resource plans the hidden method: after `app.ProvideValue(publisher)`, a second `app.ProvideValue[OrderEvents](publisher)` is accepted, since the first binding starts it. A type with `Start` and without `Shutdown` is not a component and is never started.

An alias is a binding of its interface: a direct binding of `OrderEvents` after the `Alias` panics as a duplicate unless it carries `credo.Override()`, which replaces the alias — what `testutil.WithOverride[OrderEvents](fake)` does in a test. `Alias[OrderEvents, *FakePublisher](credo.Override())` points the interface at another registered type. When nothing else reaches the type an overridden alias pointed at, `Finalize` drops it, so the real publisher is never built or started.

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

app.Provide(NewEmailSender)
app.Provide(NewInAppSender)

app.BindMany[Sender, *EmailSender]()
app.BindMany[Sender, *InAppSender]()

app.Provide(NewSenderRegistry)
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

Bind each wrapper once, named with `app.ProvideValue(PrimaryDB{primary}, credo.ComponentName("primary"))`, then inject `PrimaryDB` or `AnalyticsDB` explicitly where needed; an interface view is an `Alias`, never a second binding ([Data Access Guide](data-access.md#multiple-databases)). If those wrappers embed `*sqldb.DB`, `store/sqldb` keeps transaction context scoped per database instance, so same-type Bun connections do not collide implicitly.

### Wrappers and resource identity

A wrapper and the handle it wraps can be one resource held through two bindings, and one resource has one teardown. The App keys components by **resource identity**: the token a value's `ResourceIdentity() any` method returns (`credo.ResourceIdentifier`); without the method, a comparable value — a pointer, or a struct over one — is its own identity. Values that share an identity are started once and shut down once, when the last holder retires, so the consumers of every holder stop first: one pointer under several bindings, a wrapper and the handle it embeds, two wrappers over one handle.

- **A wrapper that embeds a handle which identifies itself inherits that identity.** `*sqldb.DB` returns itself from `ResourceIdentity()`, so `PrimaryDB{db}` and `db` are one database: bound both ways, it is closed once, after the consumers of both. Two wrappers over two handles are two databases, each closed once.
- **A wrapper over a handle that does not identify itself forwards the identity with one method.** Without it, the wrapper value would be a resource of its own:

  ```go
  type ProductIndex struct{ *search.Client }

  func (p ProductIndex) ResourceIdentity() any { return p.Client }

  app.ProvideValue[*search.Client](client, credo.CloseOnShutdown())
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

The holders of one resource that have a teardown agree on its kind: `Shutdown`, or `Close` through `credo.CloseOnShutdown()`. A client bound with `credo.CloseOnShutdown()` beside a holder whose teardown is `Shutdown` is refused — the later `ProvideValue` panics when both values are given, and otherwise the construction of the holder built later fails — with both bindings and both remedies named: drop `credo.CloseOnShutdown()`, or bind the holder that has `Shutdown` under a type that shows `Close` and not `Shutdown`, with `credo.CloseOnShutdown()`. A holder without a teardown, such as `ProductIndex` above, takes no part in the choice, and the resource still waits for its consumers. A resource handed to `app.Manage` that a `ProvideValue` binding already holds, or handed to `Manage` twice, panics; a `ResourceIdentity()` that panics or returns nil or a non-comparable token panics at `ProvideValue` and `Manage` and fails a constructor's construction.

**The registration holds the resource.** The teardown and the `/ready` entry of a resource belong to its **holder of record**: the first registration that holds it — a binding, or a value handed to `app.Manage`, one that is a component by its type preferred (its type shows `Shutdown`, or it carries `credo.CloseOnShutdown()`) — whatever order the holders are built in. A value that a constructor builds without becoming a binding — the value of a worker's constructor, or of a constructor handed to `app.Manage` — claims nothing. When it holds a resource a registration holds too, such as a wrapper that embeds a bound `*sqldb.DB`, it defers to that registration: it adds no `/ready` entry of its own and never runs the teardown, so the database keeps the binding's entry, with its name and its options, and the binding's teardown, after the consumers of both, even when the constructor was registered first. Without such a registration, its first holder is the holder of record.

**The registrations of one resource agree on it.** One resource has one teardown, which runs in one tier, and one `/ready` entry, so the bindings and `Manage` values that hold it agree on what shapes them:

- **The tier.** A raw handle bound internal beside a wrapper that embeds it bound with `credo.Ingress()` is refused, naming both registrations, their tiers and the remedies — at the later `ProvideValue` or `Manage` call when both values are handed in, and at the start when a binding's constructor builds one of them. A value that defers takes no part. Give them one tier, or hold the handle in a named field. A borrowed resource is exempt, since the App neither starts nor tears it down.
- **The entry's options.** `credo.OptionalReadiness()` is on every registration that plans an entry for the resource or on none, and the names `credo.ComponentName` gives are one name, which the entry and the teardown carry. A borrowed resource is checked too, since `/ready` reports it.
- **A value that defers takes no options.** A constructor handed to `Manage` with `credo.ComponentName`, `credo.Ingress()` or `credo.OptionalReadiness()` whose value defers fails the start, since the value has no entry and no teardown of its own for the option to shape; give the option to the handle's binding instead.

A wrapper with readiness of its own — one that overrides the `Ready` it embeds — holds the handle in a named field, like `ReportingDB` above, so that it is a resource of its own with an entry of its own. Embedded, the wrapper and the handle are one resource with one entry, and only one of the two `Ready` methods is asked.

### What each registration form claims

A call site does not show what a registration takes over: `app.Manage(v)` claims the teardown of what `v` holds, while `app.Manage(func() T { return v })` claims nothing a registration already holds. A registration that claims a resource is a candidate for its holder of record, which gives the resource its `/ready` entry and runs its teardown; one that claims nothing defers to such a registration when there is one.

| Registration | A binding | What it claims |
| --- | --- | --- |
| `app.ProvideValue(v)` | yes | What `v` holds: its readiness entry and its teardown, after the consumers of every holder. Refused beside a `Manage` of the same resource |
| `app.Provide(NewT)` | yes | What the value `NewT` builds holds: its readiness entry and its teardown |
| `app.ProvideValue(v, credo.Borrowed())` | yes | Its readiness entry only: the App neither starts nor tears the value down, through this binding or any other holder |
| `app.Manage(v)` | no | What `v` holds, as `ProvideValue` does. Refused beside a `ProvideValue` binding or another `Manage` of the same resource |
| `app.Manage(NewT)` | no | Nothing a registration holds: the value it builds defers to the binding or `Manage` value that holds the same resource, and takes no `credo.ComponentName`, `credo.Ingress()` or `credo.OptionalReadiness()`. Holding a resource no registration holds, it is that resource's holder of record |
| `app.Workers().Continuous(name, w)`, a worker value | no | Nothing: the App runs `w` in the loop `worker:<name>` and never calls its `Shutdown` |
| `app.Workers().Continuous(name, fn)`, a run function | no | Nothing: it builds no value, and its parameters are the bindings' |
| `app.Workers().Continuous(name, NewW)`, a worker constructor | no | What `app.Manage(NewT)` claims: its value, `worker-value:<name>`, defers to a registration that holds the same resource, and is torn down after its loop |

`Scheduled` claims what `Continuous` does in each form. A worker that other code uses is therefore bound with `Provide` and registered through the method expression of its `Run`, `(*OutboxRelay).Run`, so that the binding keeps its readiness entry, its options and its teardown ([Worker Guide](worker.md)).

For the full multi-database pattern, see the [Data Access Guide](data-access.md).

---

## `Finalize`

Finalizing the container does two things:

1. freezes the container
2. validates the dependency graph

The first resolving call — `Resolve`, `ResolveAll`, `MustResolve` or `MustResolveAll` — finalizes the container when nothing has, then resolves; `Run`, `App.Start` and the first request finalize an App that resolved nothing. `app.Finalize()` does the same before anything is resolved: it is optional, an early check.

After the container is finalized:

- `Provide`, `ProvideValue`, `Alias`, `BindMany`, `Manage` and the worker registrations panic at the call, a `credo.Override()` included, naming what finalized the container, where, and the calls that led there — `credo: App.Provide[*app.Mailer]: called after the container was finalized by MustResolve[*app.UserController] at /src/main.go:42 (called from /src/main.go:12); register before that call`
- after a failed validation, every `Resolve` and `ResolveAll` returns its error

Finalizing is DI-only. Routes, middleware, hooks and renderers stay open until the App prepares to serve (the first request or `Run`), so you can resolve a controller and still bind its routes.

Validation catches startup problems early and reports all of them at once, joined in registration order with the same text on every run:

- missing dependencies, each with its whole path from the registration that needs it
- dependency cycles, every one of them
- constructors that take a `context.Context`
- internal components that depend on an ingress one, each with its path

Without an explicit call, the report is the first resolving call's error, and `MustResolve` panics with it. Call `app.Finalize()` after the last registration when you want the whole report as an error before anything is resolved — in a test that checks the wiring, or in a composition root that handles the error itself before serving:

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
app.GET("/users/{id}", func(c *credo.Context) error {
    svc, err := app.Resolve[*UserService]()
    if err != nil {
        return err
    }
    _, err = svc.FindByID(c.Context(), c.Request().RouteParam("id"))
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

app.Provide(NewConsumer, credo.Ingress())
```

`Shutdown` runs on a component that was built but never started — the rollback of a failed start stops it too — so it tolerates a `Start` that never ran.

### Two tiers

Every component belongs to a tier. The **ingress** tier is where work enters the process — the WebSocket server, a scheduled worker, a consumer of an external queue — and is chosen with `credo.Ingress()` on the registration (a worker's tier is a field of its configuration). A type whose nature puts it there declares it instead: it implements `credo.IngressComponent`, whose `IngressTier()` method is never called, and every registration of it is ingress without the option — `websocket.Server` does, so `app.Manage(ws)` suffices. Your own consumer type can declare it the same way; a binding whose type hides the method is refused, as one that hides `Start` is. Everything else is **internal**, the default. The start phase starts the internal tier in dependency order, then the ingress tier, then the listener accepts. The drain mirrors it: the ingress tier stops first, concurrently with the HTTP drain, with its components that no edge orders stopping concurrently; the internal tier then stops one component at a time in reverse dependency order.

No dependency crosses the tiers backwards: an internal component that depends on an ingress one, directly or through bindings that are not components, fails `Finalize` with the path and both remedies — declare the dependent ingress, or split the ingress component so that what internal components use is an internal part. A producer that hands work to a consumer component takes the consumer (or a handle it owns) as a constructor parameter, and "consumers before dependencies" then stops the producer first; there is no separate ordering API.

### A value whose binding's type does not show `Shutdown`

Ownership, not the view consumers take, decides the teardown. A value the App owns whose `Shutdown` only the built value has — bound as an interface without the method, say — is still a component and is shut down after its consumers. Because `Finalize` cannot see it, it is internal, and if it depends on an ingress component its construction fails with the path and the remedies. Its `Start` and `Ready` are planned from the binding's type, so a binding whose type hides them is refused unless another registration of the same resource plans them: provide the concrete type and `Alias` the interface ([Interface Wiring with `Alias`](#interface-wiring-with-alias)). A binding whose type shows `Start` but hides the `Shutdown` its value has is a component, and the App starts it. To keep the teardown of a value you built with yourself, bind it with `app.ProvideValue[T](v, credo.Borrowed())`.

A value that a constructor builds over a borrowed resource stays the caller's as well, even when it has `Shutdown`, unless its binding carries `credo.CloseOnShutdown()`. A teardown that is neither `Shutdown` nor a `Close` — a client's `Disconnect(ctx)`, a `Drain` that must be waited for — belongs to a type that embeds the client and implements `Shutdown`, registered as the binding itself, so its constructor's parameters give the teardown's own dependencies their edges.

### Components that are not bindings, and hooks

| Mechanism | When to use | Order |
| --- | --- | --- |
| A DI singleton with `Shutdown` (optionally `Start`, `Ready`) | Anything the application wires through DI that holds a resource | Its tier, after its consumers; started before them |
| `credo.CloseOnShutdown()` on a binding | A client whose teardown is `Close` | Like a component with `Shutdown` |
| `app.Manage(v, opts...)` | A value with `Shutdown` built outside DI, a constructor over DI parameters that should not be injectable, a mounted child App | Its tier; a value has no edges and stops in reverse registration order, a constructor has its parameters' edges |
| `app.OnStart(fn, opts...)` | A leaf action at startup, such as warming a cache | FIFO after its tier's components have started |
| `app.OnStop(fn, opts...)` | A leaf action at shutdown that may still use its tier's components | LIFO before its tier's components stop |

`Manage`, like `Provide` and `ProvideValue`, names a component with `credo.ComponentName` (default: its type name; a duplicate name panics). A hook is an anonymous component of a tier, internal unless `credo.Ingress()` says otherwise; anything with a teardown of its own is a component, not a hook, and process-level cleanup that must outlive every component belongs after `Run` returns.

During graceful shutdown the full sequence is:

1. Withdraw readiness (`/ready` answers 503 `shutting_down`).
2. Concurrently with the HTTP drain, stop the ingress tier: ingress `OnStop` hooks in LIFO order, then ingress components.
3. Wait for an in-flight reload.
4. Stop the internal tier: internal `OnStop` hooks in LIFO order, then internal components in reverse dependency order. Once the internal components begin to stop, `Resolve` returns an error matching `credo.ErrDIClosed`.

All steps share one deadline — `WithShutdownTimeout` for a signal or a cancelled `RunContext` context, the caller's for `app.Shutdown(ctx)` — and spend it in order. A `Shutdown`, `Close` or hook that has not returned at the deadline is abandoned: the components it depends on are not stopped, since it may still use them, and it is reported with every component it kept open. A `Shutdown` that returns an error promptly is reported, and its dependencies are still stopped. Every `Start`, `Shutdown` and hook is panic-isolated. A construction still running when the drain reaches it blocks its dependencies and is shut down in order when it completes; one that completes after the deadline gets one bounded late cleanup, logged and not reported. Failures are inspectable with `errors.AsType[*credo.LifecycleError]` through the joined `Shutdown` error: each entry names the component, its tier, its phase (`start` or `shutdown`) and its outcome (`failed`, `panicked`, `abandoned`, `kept_open`).

`app.Shutdown` also works on an App that was never run: in the `building` state it freezes registrations and runs the same teardown without an HTTP drain, so a composition root can clean up registered resources after a later bootstrap failure. An App served through `ServeHTTP` by a server you own — an external `http.Server`, or `httptest` in a test — is started with `app.Start(ctx)` (`testutil.Start(t, app)` in tests) when it has anything to start: a component with `Start` or `Ready` (a WebSocket server among them), a constructor handed to `Manage`, a bound database, `UseI18n`, a worker, or a start hook; until then `ServeHTTP` panics. The server's owner drains it before calling `app.Shutdown`, because the internal tier stops after the HTTP drain only if that drain has happened.

---

## Config Changes and Singletons

DI singletons are built once and never rebuilt — a [reload](configuration.md#reloading-configuration) does not re-run constructors or replace bindings. A section bound through `Decode` is therefore a startup snapshot. When a service needs a value that can change at runtime, bind its section live and take the handle: the constructor's signature then says the value can change, and state derived from it follows the section through `OnChange`:

```go
var LimitsSection = config.Section[Limits]("limits")

app.Provide(LimitsSection.Live) // *config.Live[Limits]
app.Provide(NewRateLimiter)

type RateLimiter struct {
    limits *config.Live[Limits]
    bucket atomic.Pointer[Bucket] // derived from the section
}

func NewRateLimiter(infra credo.Infra, limits *config.Live[Limits]) *RateLimiter {
    rl := &RateLimiter{limits: limits}
    rl.bucket.Store(NewBucket(limits.Load()))
    limits.OnChange(func(ctx context.Context, prev, next Limits) error {
        rl.bucket.Store(NewBucket(next)) // runs after the reload published next
        return nil
    })
    return rl
}

func (rl *RateLimiter) Allow() bool { return rl.bucket.Load().Take() }
```

A unit test of `RateLimiter` passes `config.Fixed(Limits{RPS: 10, Burst: 20})`, a handle whose value never changes.

Resources that cannot be swapped atomically — a database pool built from a changed DSN, a listener on a new port — are restart-only by design; a reload reports their keys in its `restart_required` list.

---

## Testing

Most unit tests do not need the container at all. Construct the type directly:

```go
repo := &FakeUserRepository{}
svc := NewUserService(credo.Infra{Logger: slog.Default()}, repo)
```

Use the container in tests when you want to verify wiring:

```go
app := testutil.NewApp(t) // hermetic: reads no configuration from disk or the environment

app.ProvideValue[UserRepository](&FakeUserRepository{})
app.Provide(NewUserService)

if err := app.Finalize(); err != nil { // optional: a missing dependency fails here, with its path
    t.Fatal(err)
}

svc := app.MustResolve[*UserService]() // without Finalize, this call finalizes
_ = svc
```

The `testutil` package builds a hermetic test App: `testutil.NewApp(t, testutil.WithWiring(wire), testutil.WithOverride[UserRepository](fakeRepo))`. `WithOverride` replaces a binding through `credo.Override()` and panics when the wiring has no earlier binding of that type; adding a binding is `WithWiring`'s job. An App served through `httptest` that has anything to start is started with `testutil.Start(t, app)`, which shuts it down when the test ends:

```go
app := testutil.NewApp(t, testutil.WithWiring(wire))
testutil.Start(t, app)
srv := httptest.NewServer(app)
t.Cleanup(srv.Close) // drained before the components stop
```

An application split into modules is tested with `testutil.WithModules`, which hands the modules to `UseModules` with one more module last, whose `Services` applies the test's overrides. The overrides land after every module's services and before any module's handlers resolve, so the routes a module registers are built with the fake:

```go
app := testutil.NewApp(t,
    testutil.WithWiring(func(app *credo.App) {
        app.ProvideValue(testDB) // what main registers before UseModules
    }),
    testutil.WithModules(users.Module{}, billing.Module{}),
    testutil.WithOverride[billing.PaymentGateway](&FakeGateway{}),
)
testutil.Start(t, app)

rec := httptest.NewRecorder()
app.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/invoices", body))
// the invoice handler charged FakeGateway
```

The wiring runs before the modules. Calling `app.UseModules` yourself inside `WithWiring` does not combine with an override: the override would come after the modules' handlers had finalized the container and panic, and beside `WithModules` the call is a second `UseModules` call, which panics too.

Good rule:

- test behavior with direct construction
- test wiring with the container

---

## Common Mistakes

### Using DI for request state

Do not try to inject request ID, auth user, or transactions through DI. Use `*credo.Context` / `context.Context`.

### Putting config into `credo.Infra`

RawConfig should be decoded into typed structs through a section descriptor and bound with `app.Provide(DatabaseSection.Decode)`, or read with `app.GetConfig(DatabaseSection)` and registered with `ProvideValue`.

### Resolving before the last registration

The first resolving call ends registration, so a `Provide`, a `Manage` or a worker registration after it panics, naming that call and its line. Resolve after every registration — across an application split into parts, [`UseModules`](#modules) keeps that order — and read configuration during registration with `app.GetConfig(section)` rather than resolving `credo.RawConfig`: `GetConfig`, `ConfigExists` and `Has` never finalize.

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
6. mount features and register satellites — `UseI18n`, `UseHealth`, workers, `Manage`
7. resolve top-level controllers/services — the first call validates the graph — and register the routes built from them
8. `app.Run()`

An application split into parts keeps the same order through [modules](#modules): steps 4–6 are each module's `Services`, step 7 its `Handlers`, and one `app.UseModules` call runs them.

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
