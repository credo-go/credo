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
- `ProvideFactory[T]`: register a compiler-checked factory closure
- `ProvideValue[T]`: register a pre-built singleton
- `Alias[I, T]`: resolve an interface `I` as the singleton of concrete type `T`
- `BindMany[I, T]`: add a concrete singleton `T` to the ordered collection for interface `I`
- `Finalize(app)`: freeze registrations and validate the dependency graph
- `Resolve[T]`: retrieve a fully wired singleton
- `ResolveAll[I]`: retrieve the ordered collection bound for interface `I`

`Run()` and `RunTLS()` call `Finalize()` implicitly, but explicit `Finalize(app)` is recommended so dependency errors fail fast during startup.

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
    DSN string `credo:"dsn"`
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

    raw := credo.MustResolve[credo.RawConfig](app)

    var dbCfg DatabaseConfig
    if err := raw.Unmarshal("databases.default", &dbCfg); err != nil {
        log.Fatal(err)
    }

    credo.MustProvideValue(app, &dbCfg)
    credo.MustProvide[*DB](app, NewDB)
    credo.MustProvide[*PgUserRepository](app, NewPgUserRepository)
    credo.MustAlias[UserRepository, *PgUserRepository](app)
    credo.MustProvide[*UserService](app, NewUserService)
    credo.MustProvide[*UserController](app, NewUserController)

    if err := credo.Finalize(app); err != nil {
        log.Fatal(err)
    }

    users := credo.MustResolve[*UserController](app)
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

## `Provide` vs `ProvideValue` vs `ProvideFactory`

Use `Provide` when Credo should create the singleton for you:

```go
credo.MustProvide[*DB](app, NewDB)
credo.MustProvide[*UserService](app, NewUserService)
```

Use `ProvideValue` when you already have the instance:

```go
cfg := &DatabaseConfig{DSN: "postgres://localhost/app"}
credo.MustProvideValue(app, cfg)
```

Typical `ProvideValue` use cases:

- typed config structs
- pre-built SDK clients
- test doubles
- values created by another bootstrap system

### `ProvideFactory`: compiler-checked factory registration

`Provide`'s `constructor` parameter is typed `any` — Go cannot express "a function with arbitrary parameters returning `T`" — so a signature mistake is reported as an error at registration time, not at compile time. When you want the whole registration checked by the compiler, use `ProvideFactory`: `fn`'s signature is enforced (and `T` inferred), and it resolves its own dependencies:

```go
credo.MustProvideFactory(app, func(app *credo.App) (*UserService, error) {
    repo, err := credo.Resolve[*UserRepository](app)
    if err != nil {
        return nil, err
    }
    return NewUserService(app.NewInfra("UserService"), repo), nil
})
```

The trade-off: `fn` is opaque to the container. Dependencies resolved inside it are invisible to `Finalize`'s graph validation (a missing one surfaces at first resolution instead), and `credo.Infra` is not auto-injected — use `app.NewInfra` as shown. Prefer plain `Provide` with a named constructor as the default; reach for `ProvideFactory` when you want compiler-checked wiring or inline construction logic.

Some Credo feature packages build on top of DI with package-level helpers instead of asking you to wire every internal singleton manually. Examples:

- `store.Register[*sqldb.DB](app, db)`
- `worker.Register(app, myWorker, opts...)`

These helpers still use the DI container under the hood, but they also attach extra framework behavior such as startup validation, lifecycle tracking, and shutdown integration. Use them before `credo.Finalize(app)`. See the [Data Access Guide](data-access.md) and [Worker Guide](worker.md) for the user-facing patterns.

---

## Interface Wiring with `Alias`

In most applications, constructors return concrete types while services depend on interfaces. `Alias` connects those two worlds without duplicate registration.

```go
type UserRepository interface {
    FindByID(ctx context.Context, id string) (*User, error)
}

type PgUserRepository struct{ /* ... */ }

credo.MustProvide[*PgUserRepository](app, NewPgUserRepository)
credo.MustAlias[UserRepository, *PgUserRepository](app)
```

After that:

```go
func NewUserService(infra credo.Infra, repo UserRepository) *UserService {
    return &UserService{repo: repo}
}
```

`Alias` is the preferred way to program to interfaces while keeping constructor return types concrete.

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

credo.MustProvide[*EmailSender](app, NewEmailSender)
credo.MustProvide[*InAppSender](app, NewInAppSender)

credo.MustBindMany[Sender, *EmailSender](app)
credo.MustBindMany[Sender, *InAppSender](app)

credo.MustProvide[*SenderRegistry](app, NewSenderRegistry)
```

You can also resolve the same collection explicitly:

```go
senders := credo.MustResolveAll[Sender](app)
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

Then inject `PrimaryDB` or `AnalyticsDB` explicitly where needed. If those wrappers embed `*sqldb.DB`, `store/sqldb` keeps transaction context scoped per database instance, so same-type Bun connections do not collide implicitly.

For the full multi-database pattern, see the [Data Access Guide](data-access.md).

---

## `Finalize`

`credo.Finalize(app)` does two things:

1. freezes the container
2. validates the dependency graph

After `Finalize`:

- `Provide` fails
- `ProvideValue` fails
- `Alias` fails
- `BindMany` fails
- `Resolve` still works
- `ResolveAll` still works

Validation catches startup problems early:

- missing dependencies
- dependency cycles
- invalid constructor signatures

`Run()` and `RunTLS()` call `Finalize()` implicitly, but explicit finalize is the recommended pattern:

```go
if err := credo.Finalize(app); err != nil {
    log.Fatal(err)
}
```

---

## `Resolve` and `ResolveAll`

`Resolve` retrieves a singleton from the container:

```go
svc, err := credo.Resolve[*UserService](app)
if err != nil {
    return err
}
```

Credo's **recommended** use of `Resolve` is bootstrap/composition-root code:

- reading `credo.RawConfig` during startup
- resolving a controller before route registration
- resolving a top-level service in `main()`

Runtime `Resolve` is technically allowed because the API is public, but it is not Credo's primary application pattern.

`ResolveAll[I]` follows the same guidance: use it mainly in bootstrap/setup code when you explicitly need the whole ordered collection. Inside normal application code, prefer constructor injection of `[]I`.

Recommended:

```go
controller := credo.MustResolve[*UserController](app)
app.GET("/users/{id}", controller.Show)
```

Not recommended as the default style:

```go
app.GET("/users/{id}", func(ctx *credo.Context) error {
    svc, err := credo.Resolve[*UserService](app)
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

All DI-managed objects are singletons. If a singleton needs cleanup, implement `credo.Shutdowner`:

```go
type Cache struct{}

func (c *Cache) Shutdown(ctx context.Context) error {
    _ = ctx
    return nil
}
```

Credo calls `Shutdown(ctx)` during app shutdown in reverse registration order.

This is useful for:

- database pools
- caches
- message clients
- background worker coordinators

### Shutdowner vs OnShutdown

Credo offers two shutdown mechanisms. Choose based on how the component is created:

| Mechanism | When to use | Order |
| --- | --- | --- |
| `credo.Shutdowner` interface | DI-managed singletons (registered via `credo.Provide` / `credo.ProvideValue`) | Reverse registration order |
| `app.OnShutdown(fn)` | Components created outside DI — manual connections, background goroutines, third-party handles | LIFO (last registered, first called) |

During graceful shutdown the full sequence is:

1. Cancel app context
2. Drain in-flight HTTP requests
3. **Container shutdown** — calls `Shutdown(ctx)` on every singleton that implements `Shutdowner`
4. **OnShutdown hooks** — runs registered hook functions in LIFO order

Container shutdown (step 3) always runs before OnShutdown hooks (step 4), so DI-managed resources are released first.

If your service is already in the container, prefer `Shutdowner` — it requires no extra registration and the container handles ordering automatically. Use `OnShutdown` only for things the container does not own.

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

credo.MustProvideValue(app, &DatabaseConfig{DSN: "test"})
credo.MustProvide[*UserService](app, NewUserService)

if err := credo.Finalize(app); err != nil {
    t.Fatal(err)
}

svc := credo.MustResolve[*UserService](app)
_ = svc
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

### Skipping `Finalize`

`Run()` will finalize implicitly, but explicit `Finalize(app)` gives earlier feedback and clearer startup failures.

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
6. call `Finalize(app)`
7. resolve top-level controllers/services needed for startup wiring
8. start the app

This keeps dependency graphs explicit, startup failures early, and runtime behavior simple.

---

## Related Documents

- [Configuration Guide](configuration.md)
- [Data Access Guide](data-access.md)
- [DI Container Spec](../specs/container.md)
- [ADR-004](../adr/004-dependency-injection-and-infra.md)
- [ADR-005](../adr/005-configuration-architecture.md)
