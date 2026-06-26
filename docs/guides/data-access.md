# Data Access Guide

This guide explains how to use Credo's data access stack in application code. For low-level contracts and design rationale, see the [Store Spec](../specs/store.md) and [ADR-015](../adr/015-data-access.md).

All config examples in this guide use JSON for consistency. Credo also supports YAML/YML with the same structure.

Credo's data access story has two layers:

- `store/`: core contracts, lifecycle tracking, health registry, transaction helpers
- `store/sqldb/`: Bun-based SQL wrapper with query proxies and transaction support

---

## When To Use What

Use `store/sqldb` when:

- you want Credo's first-class SQL integration
- you want startup ping, automatic close on shutdown (via DI), and health registration
- you want Bun query builders with Credo error mapping
- you want Credo's `InTx` / `RunInTx` convenience
- you want migrations to run on app start (`bun/migrate` wrapper)

Use raw DI instead when:

- you use another ORM or SQL toolkit
- you want to register an existing client directly
- you do not need the Bun wrapper

For example, `store/sqldb` is first-class. GORM, sqlx, sqlc, or a custom client can still be injected through Credo DI without using `store/sqldb`.

---

## Single Database Quick Start

The most common setup is one SQL database registered as `*sqldb.DB`.

```go
package main

import (
    "log"

    "github.com/credo-go/credo"
    "github.com/credo-go/credo/store"
    "github.com/credo-go/credo/store/sqldb"

    _ "github.com/jackc/pgx/v5/stdlib"
)

func setupStore(app *credo.App) error {
    raw := app.MustResolve[credo.RawConfig]()

    var cfg sqldb.Config
    if err := raw.Unmarshal("databases.default", &cfg); err != nil {
        return err
    }

    db, err := sqldb.Open(&cfg)
    if err != nil {
        return err
    }

    return store.Register[*sqldb.DB](app, db)
}

func main() {
    app, err := credo.New()
    if err != nil {
        log.Fatal(err)
    }

    if err := setupStore(app); err != nil {
        log.Fatal(err)
    }

    if err := app.Finalize(); err != nil {
        log.Fatal(err)
    }

    if err := app.Run(); err != nil {
        log.Fatal(err)
    }
}
```

Important points:

- import the SQL driver with a blank import
- unmarshal `sqldb.Config` from `credo.RawConfig`
- use `store.Register[*sqldb.DB]` instead of raw `ProvideValue`

`store.Register` adds more than DI registration:

- pings the connection at startup
- tracks it in the store registry for health reporting
- leaves closing to the DI container: `*sqldb.DB` implements `credo.Shutdowner`, so the container closes it on app shutdown

---

## Configuration

`sqldb.Config` is designed to be loaded from Credo config:

```go
type Config struct {
    Driver         string
    Host           string
    Port           int
    Name           string
    User           string
    Password       string
    DSN            string
    ConnectTimeout time.Duration
    MaxOpen        int
    MaxIdle        int
    MaxLifetime    time.Duration
    SSLMode        string
    Options        map[string]string
}
```

Example config file:

```json
{
  "databases": {
    "default": {
      "driver": "pgx",
      "host": "localhost",
      "port": 5432,
      "name": "app",
      "user": "postgres",
      "password": "secret",
      "ssl_mode": "disable",
      "max_open": 25,
      "max_idle": 10
    }
  }
}
```

If `DSN` is set, the structured connection fields are ignored.

---

## Injecting The Database

With a single database, inject `*sqldb.DB` directly:

```go
type UserRepo struct {
    db *sqldb.DB
}

func NewUserRepo(db *sqldb.DB) *UserRepo {
    return &UserRepo{db: db}
}
```

Then use the Credo query proxies:

```go
type User struct {
    ID   int64
    Name string
}

func (r *UserRepo) FindByID(ctx context.Context, id int64) (*User, error) {
    var user User
    err := r.db.Select(&user).
        Where("id = ?", id).
        Scan(ctx)
    if err != nil {
        return nil, err
    }
    return &user, nil
}
```

These proxies add:

- transaction pickup from context
- error mapping to `store.Err*`
- escape hatches via `Apply(...)`, `ApplyQueryBuilder(...)`, and `Unwrap()`

### The Terminal Contract

Both guarantees are attached by the **terminal** methods (`Scan`, `Count`, `Exists`, `Exec`): the connection is resolved from the context at execution time — inside an `InTx` block that is the transaction — and the returned error is already mapped. Terminals execute a _copy_ of the builder, never the builder itself, so a built query can be executed more than once and even reused across transaction boundaries: running the same builder first inside `InTx` and again after the transaction finished is safe.

### Automatic Error Mapping

Terminal methods (`Scan`, `Count`, `Exists`, `Exec`) translate driver errors into `store.Err*` sentinels before returning, so you can branch with `errors.Is` without importing `database/sql` or driver-specific packages:

```go
var user User
err := db.Select(&user).Where("id = ?", id).Scan(ctx)
if errors.Is(err, store.ErrNotFound) {
    return nil, credo.NewHTTPError(http.StatusNotFound, credo.MsgKeyNotFound)
}
```

| Driver error               | Mapped sentinel      |
| -------------------------- | -------------------- |
| `sql.ErrNoRows`            | `store.ErrNotFound`  |
| Unique violation           | `store.ErrDuplicate` |
| Foreign-key violation      | `store.ErrConflict`  |
| Read-only / replica        | `store.ErrReadOnly`  |
| `context.DeadlineExceeded` | `store.ErrTimeout`   |

`Update.Exec` and `Delete.Exec` do **not** convert "no rows affected" into `ErrNotFound`. If you need that behavior, inspect the returned `sql.Result`:

```go
res, err := db.Update().Model(&user).WherePK().Exec(ctx)
if err != nil {
    return err
}
n, _ := res.RowsAffected()
if n == 0 {
    return store.ErrNotFound
}
```

### Joining Tables

JOIN methods are part of the curated proxy set, so no `Apply` escape hatch is needed:

```go
var results []UserWithOrder
err := db.Select(&results).
    Join("JOIN orders AS o ON o.user_id = ?TableAlias.id").
    Where("o.total > ?", 100).
    OrderExpr("o.total DESC").
    Scan(ctx)
```

`JoinOn` and `JoinOnOr` compose the ON clause separately:

```go
n, err := db.Select((*User)(nil)).
    Join("JOIN orders AS o").
    JoinOn("o.user_id = ?TableAlias.id").
    JoinOn("o.status = ?", "paid").
    Count(ctx)
```

For model-less queries (reporting, ad-hoc projections), use `TableExpr` and `ColumnExpr`:

```go
var total int
err := db.Select().
    ColumnExpr("SUM(o.total)").
    TableExpr("orders AS o").
    Join("JOIN users AS u ON u.id = o.user_id").
    Where("u.name = ?", name).
    Scan(ctx, &total)
```

---

## What `store.Register` Does

`store.Register[R]` is the preferred registration API for data stores.

```go
if err := store.Register[*sqldb.DB](app, db); err != nil {
    return err
}
```

It performs these steps:

1. checks lifecycle support
2. pings the store
3. ensures the store registry exists
4. tracks the connection for health reporting
5. registers the value in DI

Closing has a single owner — the DI container: the registered value implements `credo.Shutdowner`, so it is closed during app shutdown in reverse registration order. The registry never closes connections.

Useful options:

```go
store.Register[*sqldb.DB](
    app,
    db,
    store.WithName("primary"),
    store.WithPingTimeout(10*time.Second),
)
```

Use raw `app.ProvideValue` only when you intentionally do not want store registry integration.

---

## Multiple Databases

When you need more than one `sqldb.DB`, do not register both as `*sqldb.DB`. Credo DI keys services by Go type, so two values of the same type collide.

The solution is to introduce semantic wrapper types:

```go
type PrimaryDB struct{ *sqldb.DB }
type AnalyticsDB struct{ *sqldb.DB }
```

Then register each wrapper separately:

```go
func setupMultiDB(app *credo.App) error {
    raw := app.MustResolve[credo.RawConfig]()

    var primaryCfg sqldb.Config
    if err := raw.Unmarshal("databases.primary", &primaryCfg); err != nil {
        return err
    }

    var analyticsCfg sqldb.Config
    if err := raw.Unmarshal("databases.analytics", &analyticsCfg); err != nil {
        return err
    }

    primary, err := sqldb.Open(&primaryCfg)
    if err != nil {
        return err
    }

    analytics, err := sqldb.Open(&analyticsCfg)
    if err != nil {
        return err
    }

    if err := store.Register[PrimaryDB](
        app,
        PrimaryDB{primary},
        store.WithLifecycle(primary),
        store.WithName("primary"),
    ); err != nil {
        return err
    }

    if err := store.Register[AnalyticsDB](
        app,
        AnalyticsDB{analytics},
        store.WithLifecycle(analytics),
        store.WithName("analytics"),
    ); err != nil {
        return err
    }

    return nil
}
```

`WithLifecycle(...)` is optional here: a wrapper that _embeds_ `*sqldb.DB` inherits its methods and therefore already implements `store.Lifecycle` (and `credo.Shutdowner` — the DI container closes it automatically). The option becomes required when the wrapper keeps the connection in a named field instead. Such a named-field wrapper has no `Shutdown` method either, so the container cannot close it — `Register` logs a warning and closing stays with you (e.g. via `app.OnShutdown`).

Inject the specific database where it is needed:

```go
type UserRepo struct {
    db PrimaryDB
}

func NewUserRepo(db PrimaryDB) *UserRepo {
    return &UserRepo{db: db}
}

type ReportRepo struct {
    db AnalyticsDB
}

func NewReportRepo(db AnalyticsDB) *ReportRepo {
    return &ReportRepo{db: db}
}
```

This gives:

- compile-time safety
- no string keys
- no ambiguity in constructors

---

## Transactions

For one database, `db.InTx` is the normal path:

```go
type OrderService struct {
    db    *sqldb.DB
    orders *OrderRepo
}

func NewOrderService(db *sqldb.DB, orders *OrderRepo) *OrderService {
    return &OrderService{db: db, orders: orders}
}

func (s *OrderService) Place(ctx context.Context, order *Order) error {
    return s.db.InTx(ctx, func(ctx context.Context) error {
        return s.orders.Create(ctx, order)
    })
}
```

From a handler, pass the request context: `db.InTx(ctx.Context(), fn)`. The package-level `sqldb.RunInTx(ctx, db, fn)` is equivalent; `InTxWith` / `RunInTxWith` accept `sql.TxOptions` for isolation level and read-only mode.

Repository methods do not need a separate transaction parameter when they use `sqldb.DB` query proxies or raw helpers. The active transaction is picked up from `context.Context`.

For multi-database applications, be careful:

- `store/sqldb` scopes transaction context per `*sqldb.DB`, so two Bun connections of the same Go type do not collide implicitly
- `store/sqldb` uses Bun transaction types under the hood
- a single context does not become a distributed transaction coordinator

Practical rule:

- use `InTx` / `RunInTx` freely for one database per unit of work
- if a use case spans multiple Bun databases, keep transactions explicit and local
- do not assume Credo will coordinate cross-database commit/rollback

---

## Migrations

`store/sqldb` wraps Bun's migration engine (`bun/migrate` — part of the already-pinned Bun module, not a new dependency). Register the set at wiring time, then opt in to auto-run on application start:

```go
import "github.com/uptrace/bun/migrate"

//go:embed migrations/*.sql
var sqlMigrations embed.FS

func main() {
    app := credo.New(...)
    db := mustOpenDB()

    migrations := migrate.NewMigrations()
    if err := migrations.Discover(sqlMigrations); err != nil {
        log.Fatal(err)
    }
    db.RegisterMigrations(migrations)

    app.OnStart(db.Migrate) // applies pending migrations before serving

    app.Run()
}
```

SQL migration files follow Bun's naming scheme — `1_create_users.up.sql`, `2_add_index.up.sql` (optionally with matching `.down.sql`). Go migrations use `migrations.MustRegister(up, down)` from files named the same way.

What the wrapper does on each `Migrate` call:

1. creates Bun's bookkeeping tables if missing (`IF NOT EXISTS`)
2. takes a table-based advisory lock — if another replica is migrating, `Migrate` fails immediately instead of waiting (restart the instance)
3. applies unapplied migrations in order
4. releases the lock (even when the context was cancelled)

A migration is recorded as applied only **after** it succeeds, so a failed migration aborts startup and is retried on the next start. (This is the wrapper's default — Bun's bare default records first; pass `migrate.WithMarkAppliedOnSuccess(false)` to `RegisterMigrations` to restore it.)

**Seeding** is just another migration file — there is no separate seed mechanism:

```sql
-- migrations/3_seed_plans.up.sql
INSERT INTO plans (name, price) VALUES ('free', 0), ('pro', 1900);
```

For rollback, status inspection, or generating migration files, drop down to Bun's migrator via the escape hatch:

```go
migrator := migrate.NewMigrator(db.Client(), migrations)
group, err := migrator.Rollback(ctx)
```

---

## Reusing Filters Across Queries

`Apply(...)` is typed per query — a `func(*bun.SelectQuery) *bun.SelectQuery` cannot be applied to an update or delete. When the _same_ WHERE logic must run across reads and writes — tenant scoping, soft-delete filters, ownership checks — use `ApplyQueryBuilder`, which accepts Bun's shared `bun.QueryBuilder` (the builder-only interface common to select, update, and delete):

```go
// One predicate, reused everywhere.
func tenantScope(tenantID int64) func(bun.QueryBuilder) bun.QueryBuilder {
    return func(qb bun.QueryBuilder) bun.QueryBuilder {
        return qb.Where("tenant_id = ?", tenantID)
    }
}

scope := tenantScope(tid)

err := db.Select(&users).ApplyQueryBuilder(scope).Scan(ctx)
_, err = db.Update((*User)(nil)).Set("status = ?", "archived").
    ApplyQueryBuilder(scope).Exec(ctx)
_, err = db.Delete((*User)(nil)).ApplyQueryBuilder(scope).Exec(ctx)
```

Conditions added through the builder land on the proxied query, so the terminal methods still apply TX injection and error mapping — interceptors are preserved, exactly like `Apply`. A nil predicate is a no-op.

`bun.QueryBuilder` also exposes `WhereOr`, `WherePK`, `WhereDeleted`, `WhereAllWithDeleted`, and `WhereGroup` — including `WhereGroup`, which the curated proxy set does not surface directly:

```go
err := db.Select(&users).
    ApplyQueryBuilder(func(qb bun.QueryBuilder) bun.QueryBuilder {
        return qb.WhereGroup(" AND ", func(g bun.QueryBuilder) bun.QueryBuilder {
            return g.Where("role = ?", "admin").WhereOr("role = ?", "owner")
        })
    }).
    Scan(ctx)
```

Because the predicate signature mentions `bun.QueryBuilder`, this path imports `bun` into repository code — it is an escape hatch like `Apply`, not the default. The builder's `Unwrap() any` returns the concrete query; calling terminal methods on it bypasses interceptors, the same caveat as `Unwrap()`.

---

## Raw SQL And Bun Escape Hatch

Credo does not hide Bun — it integrates it. If the proxy layer does not cover a Bun feature you need, use the escape hatches: a missing _builder_ method is reached with `Apply`/`ApplyQueryBuilder` (proxy guarantees preserved); a missing _terminal_ method is worth a feature request — the guarantees live in the terminals, so they belong on the proxy. `Unwrap()` and `Client()` opt out of the guarantees entirely.

Raw helpers:

```go
err := db.QueryRow(ctx, &user, "select * from users where id = ?", id)
```

Direct Bun client:

```go
client := db.Client()
```

Use `Client()` for:

- model registration
- migration operations beyond `db.Migrate` (rollback, status, file generation)
- raw Bun APIs not exposed by the proxy layer

**What you lose when you bypass the proxy layer**: queries executed via `db.Client()` skip both interceptors that the proxy layer provides:

- **No automatic TX injection** — an `InTx` / `RunInTx` block does not affect raw `*bun.DB` calls. The query runs against the base connection, outside any active transaction.
- **No error mapping** — `sql.ErrNoRows` is returned as-is, not as `store.ErrNotFound`. Driver-specific constraint codes leak through unchanged. Calling code must import `database/sql` (or the driver package) to interpret them.

Reserve `Client()` for model registration, advanced migration operations, and raw SQL the proxy layer cannot express. Use the proxy layer (`db.Select` / `db.Insert` / `db.Update` / `db.Delete`) for normal repository code, even when the query is non-trivial.

---

## Other ORMs

Credo ships one first-class SQL adapter: `store/sqldb` on top of Bun.

If you use another ORM or client:

- register it through DI directly
- keep Credo's higher-level application structure the same

Example:

```go
gormDB, err := gorm.Open(...)
if err != nil {
    return err
}

if err := app.ProvideValue(gormDB); err != nil {
    return err
}
```

That path works, but you do not get the Bun-specific features from `store/sqldb`.

---

## Recommended Patterns

For most applications:

1. load `sqldb.Config` from `credo.RawConfig`
2. open the connection with `sqldb.Open`
3. register it with `store.Register`
4. inject the resulting type into repositories
5. keep services and controllers free of DSN strings and runtime config lookups

For multiple databases:

1. create wrapper types such as `PrimaryDB` and `AnalyticsDB`
2. register each wrapper separately with `store.Register[R]`
3. inject wrappers explicitly in constructors
4. keep transaction boundaries local to a single database unless you have a very deliberate explicit strategy

---

## Related Documents

- [Dependency Injection Guide](dependency-injection.md)
- [Configuration Guide](configuration.md)
- [Store Spec](../specs/store.md)
- [ADR-015](../adr/015-data-access.md)
