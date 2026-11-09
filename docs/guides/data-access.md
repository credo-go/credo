# Data Access Guide

This guide explains how to use Credo's data access stack in application code. For low-level contracts and design rationale, see the [Store Spec](../specs/store.md) and [ADR-015](../adr/015-data-access.md).

All config examples in this guide use JSON for consistency. Credo also supports YAML/YML with the same structure.

Credo's data access story has two layers:

- `store/`: core contracts (semantic errors, `Health`) and transaction helpers
- `store/sqldb/`: Bun-based SQL wrapper with query proxies and transaction support; its `*sqldb.DB` is a component the App pings at start, reports in `/ready` and shuts down

---

## When To Use What

Use `store/sqldb` when:

- you want Credo's first-class SQL integration
- you want a start-phase ping, App-owned deadline-aware shutdown, and readiness reporting
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
    "github.com/credo-go/credo/store/sqldb"

    _ "github.com/jackc/pgx/v5/stdlib"
)

func setupStore(app *credo.App) error {
    cfg, err := app.GetConfig[sqldb.Config]("databases.default")
    if err != nil {
        return err
    }

    db, err := sqldb.Open(&cfg)
    if err != nil {
        return err
    }

    app.ProvideValue(db) // the App owns db: pinged at start, reported by /ready, shut down after its consumers
    return nil
}

func main() {
    app, err := credo.New()
    if err != nil {
        log.Fatal(err)
    }

    if err := setupStore(app); err != nil {
        log.Fatal(err)
    }

    if err := app.Run(); err != nil {
        log.Fatal(err)
    }
}
```

Important points:

- import the SQL driver with a blank import
- read `sqldb.Config` with `app.GetConfig` during registration (the first `Resolve` would finalize the container; `GetConfig` never does)
- bind the handle with `ProvideValue`; the binding is all it takes, and it does no I/O at the call

`*sqldb.DB` is a component, so its binding's type plans what a database needs ([What a Bound Database Does](#what-a-bound-database-does)):

- the start phase calls its `Start`, which pings it before the components that depend on it start; a failed ping fails the start
- `/ready` reports it through its `Ready`, under the name `*sqldb.DB` or its `credo.ComponentName` name
- its secret-free pool warnings are logged after a successful ping

From `ProvideValue` on, the App owns `db`: `*sqldb.DB` is a `credo.Component` (it has `Shutdown`), so the drain shuts it down in the internal tier, after the components constructed from it, while the shared deadline remains, and a composition root that fails after the binding releases it with `app.Shutdown(ctx)`, which tears down an App that never ran. A handle the caller shares — with a second App, or across the tests of a suite — is bound with `app.ProvideValue(db, credo.Borrowed())`: it is still reported by `/ready`, and its start and shutdown stay the caller's.

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
    PingTimeout    time.Duration
    MaxOpen        int
    MaxIdle        *int
    MaxLifetime    time.Duration
    MaxIdleTime    time.Duration
    SSLMode        string
    Options        map[string]string
}
```

Example production config file (capacity values are illustrative; size them against the database's connection budget and the service's replica count):

```json
{
  "databases": {
    "default": {
      "driver": "pgx",
      "host": "postgres.internal",
      "port": 5432,
      "name": "app",
      "user": "app",
      "password": "redacted",
      "ssl_mode": "verify-full",
      "connect_timeout": "5s",
      "max_open": 25,
      "max_idle": 10,
      "max_idle_time": "5m",
      "max_lifetime": "30m"
    }
  }
}
```

`redacted` is a placeholder; load the real password through the application's environment or secret-backed configuration source.

If `DSN` is set, it is used as-is; structured connection fields are not merged into it. For generated PostgreSQL/MySQL DSNs, set `port` explicitly in the `1..65535` range. Credo rejects zero instead of producing `:0`, and correctly brackets IPv6 hosts. Driver detection recognizes only the exact aliases `postgres`/`pgx`, `mysql`, and `sqlite`/`sqlite3`/`sqliteshim`; a custom registered name uses its native `Config.DSN` plus `sqldb.WithDialect`, while a custom connector uses `sqldb.WithConnector` plus `sqldb.WithDialect`. Explicit nil values and known driver/dialect family mismatches fail at startup.

`ping_timeout` is the deadline of the ping `Start` sends: zero means `sqldb.DefaultPingTimeout` (5s), not "no deadline" as for `connect_timeout`, since an unbounded ping would hang the start, and a negative value fails `sqldb.Open`. PostgreSQL represents `connect_timeout` in whole seconds, so Credo rounds any positive fractional value up rather than silently truncating it to disabled. `Options` may add driver parameters but cannot override core PostgreSQL endpoint/credential keys, MySQL's required `parseTime=true`, or a simultaneously set `SSLMode`/`ConnectTimeout`; ambiguous values fail without being echoed in the error. `SSLMode` itself is driver-specific (`sslmode` for PostgreSQL, `tls` for MySQL). Credo sets no universal TLS default—production configuration must choose a verified mode and trust setup supported by the selected driver. See the canonical [store specification](../specs/store.md#config) for the complete precedence and escape-hatch contract.

There is intentionally no universal finite pool default. `max_open: 0` (and an omitted `max_open`) retains `database/sql`'s unlimited-open behavior. A database's `Start` logs one structured warning, `credo: store configuration warning` with `code=sqldb.pool.max_open_unlimited` and `module=sqldb`, once its ping succeeds when the effective pool maximum is still unlimited; it never silently changes the value. The pool is read at that moment, so a `SetMaxOpenConns` on `db.Client()` before the start counts. A program that runs the database without an App calls `db.Start(ctx)` itself or reads `db.Stats().MaxOpenConnections`.

`max_idle` distinguishes omission from an explicit zero. Omit it to leave the idle setter to `database/sql` (its effective default remains subject to `max_open`), set it to `0` to retain no idle connections, or set a positive limit. With a finite `max_open`, `max_idle` must not be greater than `max_open`; `sqldb.Open` rejects that combination rather than accepting the stdlib's silent clamp. `max_idle_time: 0` disables idle-age expiry, while `max_lifetime: 0` disables connection-lifetime expiry. Explicit positive values are applied unchanged; Credo does not overwrite them with defaults.

For operational telemetry, `db.Stats()` returns the complete `sql.DBStats` snapshot. Track at least `InUse`, `Idle`, `WaitCount`, `WaitDuration`, `MaxIdleClosed`, `MaxIdleTimeClosed`, and `MaxLifetimeClosed`. Wait and closure counters are cumulative: alert on windowed rates/deltas tied to an SLO, not on raw totals. Credo does not mark a pool `DEGRADED` from a universal saturation threshold. Such a policy needs explicit opt-in thresholds and hysteresis; a database's `Ready` fails on any status but `UP`, so a noisy threshold would take a gating database's instances out of rotation and could cause cascading traffic shifts.

Nested savepoint operations are bounded separately from query/callback execution. The default is five seconds of caller wait for each savepoint creation/release/rollback and fail-safe ambient abort; override it at construction when driver/network characteristics require a different budget:

```go
db, err := sqldb.Open(&cfg, sqldb.WithTxCleanupTimeout(10*time.Second))
```

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
- escape hatches via `Apply(...)` and `Unwrap()`, and predicates shared across query types via `ApplyFilter(...)`

`Select`, `Insert`, `Update`, and `Delete` accept at most one optional model. Supplying more causes the builder to record `sqldb: <Op> accepts at most one model, got N`; the terminal returns that error without executing, and no model is silently ignored.

`SelectQuery.Limit` and `Offset` take an `int` and forward it to Bun, which stores both values as `int64`, so every value is representable. Zero and negative values keep Bun's semantics: the clause is omitted. Through `Apply` or `Unwrap`, Bun's own `Limit` and `Offset` take `int64`.

### The Terminal Contract

Both guarantees are attached by the **terminal** methods (`Scan`, `Count`, `Exists`, `Exec`): the connection is resolved from the context at execution time — inside an `InTx` block that is the transaction — and the returned error is already mapped. Select terminals execute an internal snapshot that preserves the explicit connection, builder error, `WherePK`, soft-delete flags, and model/relation state; they never mutate the builder itself. A built query can therefore be executed more than once and even reused across transaction boundaries.

Public `SelectQuery.Clone` is the separate, top-level builder-fork API. It preserves the execution fields patched by Credo, but is not a recursive object-graph copy: a bound destination and nested CTE/relation query values may remain shared. Do not mutate or scan shared values concurrently through source and clone.

### Automatic Error Mapping

Terminal methods (`Scan`, `Count`, `Exists`, `Exec`) translate driver errors into `store.Err*` sentinels before returning, so you can branch with `errors.Is` without importing `database/sql` or driver-specific packages:

```go
var user User
err := db.Select(&user).Where("id = ?", id).Scan(ctx)
if errors.Is(err, store.ErrNotFound) {
    return nil, credo.NewHTTPError(http.StatusNotFound, credo.MsgKeyNotFound)
}
```

| Driver error | Exact mapped sentinel |
| --- | --- |
| `sql.ErrNoRows` | `store.ErrNotFound` |
| Unique/primary-key violation | `store.ErrAlreadyExists` |
| Other integrity constraint | `store.ErrConstraint` |
| Serialization failure | `store.ErrSerialization` |
| Deadlock | `store.ErrDeadlock` |
| Lock/busy contention | `store.ErrContention` |
| Bad connection / unavailable database | `store.ErrUnavailable` |
| Read-only transaction/server | `store.ErrReadOnly` |
| Verified deadline/statement timeout | `store.ErrTimeout` |

Mapped values are `*store.Error`: the original driver cause and code remain in the error chain, while Credo's default HTTP response sees only the semantic kind. Use `store.KindOf(err)` when a switch is clearer than several `errors.Is` checks. `store.IsTransient(err)` means only that the condition may clear; it does **not** mean replaying the statement, transaction callback, or external side effects is safe.

Classification is family-scoped and depends on the driver being recognized. PostgreSQL mapping is SQLSTATE-based and works with any driver exposing it (pgx, lib/pq); MySQL parses the strict server error envelope; SQLite code extraction recognizes the modernc, mattn, and ncruces drivers (matched structurally, so none becomes a Credo dependency). Errors from an unrecognized driver pass through unmapped — `errors.Is` branches against `store.Err*` silently stop matching — so verify mapping coverage before adopting a different driver.

`store.ErrDuplicate` remains an alias of `ErrAlreadyExists`. The deprecated `ErrConflict` remains an umbrella match for constraint, serialization, deadlock, and contention during migration, but new code should branch on the exact sentinel or kind.

### NUL bytes in strings

Reject a string that contains a NUL byte (`0x00`) at the validation boundary; no database stores it the way the application saw it. What happens below that boundary depends on the database: on SQLite and PostgreSQL, Bun refuses to render the value and the statement fails with an unmapped error (`store.KindOf` reports no kind), so nothing is persisted. On MySQL, Bun's dialect still drops the byte and the statement succeeds, so `"admin\x00x"` is stored as `"adminx"` — a value that passed a uniqueness or denylist check as something else. Credo does not patch this: the MySQL behavior is an upstream defect ([uptrace/bun#1443](https://github.com/uptrace/bun/issues/1443)) pinned by a canary test in the real-MySQL job, and a validation rule for it is the application's, until `validation` gains one.

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

## Typed Terminals: One, All, Page

`Scan` is the general terminal: you supply a destination, it fills it. For the common case where you query a type and want that same type back, `store/sqldb` adds three **typed terminals** that own their result through a type parameter — `One[T]`, `All[T]`, and `Page[T]` (Go 1.27 concrete-type generic methods). `T` drives both the table and the scan destination, so the query is built model-less and the terminal returns the result directly, with the same transaction pickup and error mapping the other terminals guarantee. The result shape follows the name: `One → T`, `All → []T`, `Page → *pagination.Page[T]`.

Typed terminals require that model-less form, and `T` must be the actual table model. A model bound through `Select`, `Model`, or `Apply` is not overridden: the terminal returns `sqldb.ErrTypedTerminalModel` before the database is touched. `TableExpr` does not turn `All[DTO]` into a projection query; use `TableExpr(...).Scan(ctx, &rows)` with an explicit destination. Relations likewise stay on the bound-model `Scan` path:

```go
var users []User
err := r.db.Select(&users).Relation("Orders").Scan(ctx)
```

### `One[T]` — a single row

The Scan-based `FindByID` above becomes a typed one-liner that returns the value directly — no `var user User`, no `&user`:

```go
func (r *UserRepo) FindByID(ctx context.Context, id int64) (User, error) {
    return r.db.Select().Where("id = ?", id).One[User](ctx)
}
```

`One` applies `LIMIT 1`, so multiple matches are not an error — it returns the first row; add an `OrderExpr` for a deterministic choice. A missing row maps to `store.ErrNotFound`, so callers branch exactly as they do with `Scan`:

```go
user, err := r.db.Select().Where("email = ?", email).One[User](ctx)
if errors.Is(err, store.ErrNotFound) {
    return credo.NewHTTPError(http.StatusNotFound, credo.MsgKeyNotFound)
}
```

### `All[T]` — every matching row

```go
func (r *UserRepo) Active(ctx context.Context) ([]User, error) {
    return r.db.Select().Where("active = ?", true).OrderExpr("id").All[User](ctx)
}
```

Unlike `One`, an empty result is **not** an error: `All` returns a non-nil empty slice and a nil error, so callers can range over it without a nil check.

### `Page[T]` — a paginated result

`Page` runs a COUNT plus a LIMIT/OFFSET SELECT and assembles a ready `*pagination.Page[T]` from a `*pagination.PageRequest`:

```go
func (r *UserRepo) List(ctx context.Context, req *pagination.PageRequest) (*pagination.Page[User], error) {
    return r.db.Select().
        Where("active = ?", true).
        OrderExpr("created_at DESC, id DESC").
        Page[User](ctx, req)
}
```

`BindQuery` applies the request-input policy automatically — `pagination.PageRequest` implements `Validate`, so binding it from the request query fills defaults and clamps `page`/`per_page` in place before the repository sees it. `Page` itself does not repeat that forgiving policy. It copies the request and strictly validates the snapshot without mutating the caller:

```go
func (h *UserHandler) List(c *credo.Context) error {
    var req pagination.PageRequest
    if err := c.Request().BindQuery(&req); err != nil {
        return err
    }
    page, err := h.users.List(c.Context(), &req)
    if err != nil {
        return err
    }
    return c.Response().JSON(http.StatusOK, page)
}
```

Outside a handler, call `req.Normalize()` (or `NormalizeWithMax` for a higher per-page cap) yourself when you want the same forgiving policy. Directly constructed requests may also be passed as-is, but `Page` requires positive values and a representable execution window; it never silently defaults or clamps them. Nil, zero/negative and native `int` offset overflow all return an error matching `pagination.ErrInvalidPageRequest` before COUNT. A custom normalized `PerPage` such as 100 is valid and remains 100. For direct offset calculations, handle the new strict signature: `offset, err := req.Offset()`.

When COUNT reports zero rows, SELECT is skipped and the page comes back with a non-nil empty `Records` slice and the snapshot's page/per-page preserved. Use a stable total order for every offset-paginated query. If the primary sort key can repeat, append a unique tie-breaker such as `id`; `created_at DESC` alone does not determine which equal-timestamp record belongs to which page.

#### What `Total` counts

`Page.Total` is the number of rows the data query returns without its LIMIT and OFFSET. `Count` and `Page` compute it with Bun's own `Count` on a query whose total the wrapper can prove, and fail before database I/O with `sqldb.ErrUnsupportedCountQuery` on every other query. A query is provable when it was built without `Apply`, `Unwrap` or a `Relation` callback (`ApplyFilter` keeps it provable), its model type does not implement Bun's `BeforeSelectHook`, and it carries `GroupExpr` or `Distinct`, or has no raw `ColumnExpr` and no `Having`:

| Query | Total |
| --- | --- |
| Filtered model or `Column(...)` projection | Rows |
| `Column(...).Distinct()` | Distinct rows |
| `GroupExpr(...)`, with or without `Having(...)` | Groups, after `Having` |
| `ColumnExpr(...)` or `Having(...)` without `GroupExpr`/`Distinct`, an ungrouped aggregate included | `ErrUnsupportedCountQuery` |
| Built with `Apply`, `Unwrap` or a `Relation` callback | `ErrUnsupportedCountQuery` |
| Model implements `BeforeSelectHook` | `ErrUnsupportedCountQuery` |

```go
_, err := db.Select((*User)(nil)).
    ColumnExpr("COUNT(*) AS n").
    Count(ctx)
// errors.Is(err, sqldb.ErrUnsupportedCountQuery) == true
```

The error's message names the rule, its culprit and the remedy, `PageWithCount`. Every count drops `ORDER BY`, `LIMIT`, `OFFSET` and `FOR`, and runs on the query's own connection, so it joins the ambient transaction exactly like the data query.

A `BeforeAppendModel` hook keeps the automatic total: on a SELECT, Bun calls it only for values already in the destination, `Page` scans into an empty slice of its own, and `Count` runs no hook. A `BeforeSelect` hook is the case the wrapper cannot count, because it may change the data query in a way Bun's count would not see.

On MySQL, Bun counts a grouped or `DISTINCT` query through a CTE, which requires unique output names. If such a count returns `ER_DUP_FIELDNAME` (1060), Credo wraps it with `sqldb.ErrUnsupportedCountQuery` and keeps the driver cause; give the projection's columns unique aliases. A 1060 from any other statement passes through unchanged. Keep the retained cause for logs and diagnostics; never render raw driver messages directly to HTTP clients.

#### Paging with your own count

`PageWithCount` pages a query whose total `Page` cannot prove with a count the repository supplies. Another query's `Count` passes as it is, so the usual shape is a cheap, deliberately equivalent count query next to the expensive data query, both scoped by one `ApplyFilter` predicate:

```go
scope := func(f *sqldb.Filter) { f.Where("tenant_id = ?", tenantID) }

page, err := db.Select().
    Apply(withScore). // a Bun-only projection: Page cannot prove its total
    ApplyFilter(scope).
    OrderExpr("created_at DESC, id DESC").
    PageWithCount[Report](ctx, req, db.Select((*Report)(nil)).ApplyFilter(scope).Count)
```

`PageWithCount` checks `req`, a nil `count` and a bound model before any I/O, like `Page`. It then calls `count` with the context it was given, so pass `txCtx` inside a transaction, and runs the data query even when the count is zero. An error from `count` is returned as it is, and `Total` is the count's result: the repository owns the equivalence of the two queries and the count's sort and lock clauses.

#### Keeping COUNT and SELECT on one database snapshot

COUNT and SELECT are separate statements. `Page` never starts an implicit transaction, and without an explicit transaction the pool can run them on different connections and snapshots. Even inside a transaction, the guarantee depends on the database and isolation level.

For PostgreSQL or InnoDB, request Repeatable Read on the **outermost** transaction when a shared snapshot is required, and pass the callback's `txCtx`—not the outer `ctx`—to `Page`:

```go
var page *pagination.Page[User]
err := db.InTxWith(ctx, &sql.TxOptions{
    Isolation: sql.LevelRepeatableRead,
    ReadOnly:  true,
}, func(txCtx context.Context) error {
    var err error
    page, err = db.Select().
        Where("tenant_id = ?", tenantID).
        OrderExpr("created_at DESC, id DESC").
        Page[User](txCtx, req)
    return err
})
```

Credo rejects non-default transaction options on a nested savepoint with `sqldb.ErrNestedTxOptions`; a nested call cannot upgrade an outer transaction's isolation.

| Database | COUNT/SELECT visibility |
| --- | --- |
| PostgreSQL | Default Read Committed takes a fresh snapshot for each statement, so drift is allowed. Repeatable Read fixes the snapshot at the transaction's first non-control statement. |
| MySQL/InnoDB | Default Repeatable Read makes ordinary nonlocking consistent reads share the first-read snapshot. Server configuration may change the default; other engines, locking reads, and Read Committed differ, so request Repeatable Read explicitly. |
| SQLite | A plain explicit transaction keeps its first-read snapshot. WAL permits another connection to commit while the reader keeps that snapshot; rollback-journal mode may block the writer. Shared cache with `PRAGMA read_uncommitted=ON` is the exception. |

The pinned modernc SQLite driver does not reliably enforce `sql.TxOptions.Isolation` or `ReadOnly`; for SQLite, use plain `db.InTx` as the explicit snapshot boundary instead of presenting those options as a guarantee. Fail-loud driver-capability validation is deferred. See [PostgreSQL transaction isolation](https://www.postgresql.org/docs/current/transaction-iso.html), [InnoDB transaction isolation](https://dev.mysql.com/doc/refman/8.4/en/innodb-transaction-isolation-levels.html), [InnoDB consistent reads](https://dev.mysql.com/doc/refman/8.4/en/innodb-consistent-read.html), [SQLite isolation](https://www.sqlite.org/isolation.html), and [SQLite transactions](https://www.sqlite.org/lang_transaction.html).

#### Why there is no `WithCount(false)`

`Page` always has exact `Total`/`TotalPages` metadata, and `HasNext` derives from it. An unknown total is not encoded as zero, `-1`, a pointer, or an omitted field. Total-free offset pagination uses `Slice[T]` as a working name pending its own design gate; keyset pagination keeps the separate `CursorPage[T]` name. Neither changes the meaning or JSON contract of `Page`.

The cursor design is accepted but intentionally not exported yet. Its first delivery is forward-only (`after` + `per_page`), fetches one extra row, returns `per_page`/`has_next`/nullable `next_cursor`, and never runs COUNT. It requires terminal-owned stable ordering with immutable non-null keys and an explicit unique tie-breaker. Public HTTP cursors require an explicit signing keyring; signing prevents tampering but does not hide key values.

A cursor never replaces authorization. Each request must re-apply its normal authentication, tenant, permission, and filter predicates; signed scope binding only prevents a token from being replayed under a different query.

Implementation waits for a concrete consumer, a fail-loud boundary for Bun hooks that mutate cursor-owned ordering/window state, and real PostgreSQL/MySQL/SQLite conformance. Until then, repositories that need keyset pagination own the query and token codec explicitly. See the [cursor design gate](../specs/pagination.md#cursorkeyset-design-gate).

### Mapping models to DTOs with `Page.Map`

A repository should page over its **table model**; the response usually needs a different **DTO** shape. Page once over the model, then reshape with `Page.Map`, which applies your function to every record and carries the pagination metadata (`Total`, `Page`, `PerPage`, `TotalPages`) over unchanged — so it is never recomputed or hand-copied:

```go
type UserResponse struct {
    ID   int64  `json:"id"`
    Name string `json:"name"`
}

func (s *UserService) List(ctx context.Context, req *pagination.PageRequest) (*pagination.Page[UserResponse], error) {
    page, err := s.repo.List(ctx, req) // *pagination.Page[User]
    if err != nil {
        return nil, err
    }
    return page.Map(func(u User) UserResponse {
        return UserResponse{ID: u.ID, Name: u.Name}
    }), nil
}
```

`Page[Model] → Map → Page[DTO]` is the idiomatic flow: the repository stays in model terms, the service owns the DTO boundary, and the metadata is computed once by `Page` and preserved by `Map`. The mapping function must be pure and must not be nil — `Map` panics on a nil function, even for an empty page, because a nil mapping is always a programming error. When the conversion itself can fail (it queries, validates, or otherwise returns an error), fetch `Page[Model]`, map its records with ordinary error handling, and create the DTO page with `NewPage`. `NewPage` uses overflow-safe quotient-and-remainder ceiling division for `TotalPages`, including totals near `math.MaxInt64`:

```go
modelPage, err := r.db.Select().Where(cond).Page[User](ctx, req)
// ...map modelPage.Records to dtos, returning any conversion error...
page := pagination.NewPage(
    dtos, modelPage.Total, modelPage.Page, modelPage.PerPage,
)
```

### Scan or a typed terminal?

- Reach for `One[T]` / `All[T]` / `Page[T]` whenever the result is the queried type — they drop the destination-variable ceremony and read top to bottom.
- Stay on `Scan(ctx, &dest)` for projections (aggregates, ad-hoc column lists), relation loading, and any case where `T` is not the table model or you are scanning into a value you already hold.

---

## What a Bound Database Does

A `*sqldb.DB` is a component: its type shows `Start`, `Ready` and `Shutdown`, so its binding is all the App needs. The binding is made where ownership is decided — `app.ProvideValue`, or `app.Provide` with a constructor, with `credo.Borrowed()` for a handle the caller shares — and `credo.ComponentName` names it:

```go
cfg := app.MustGetConfig[sqldb.Config]("databases.default")
cfg.PingTimeout = 10 * time.Second // zero means sqldb.DefaultPingTimeout, 5s

// The constructor runs in the start phase, after Finalize and every override.
app.Provide(func() (*sqldb.DB, error) { return sqldb.Open(&cfg) }, credo.ComponentName("primary"))
```

A bound database goes through four phases:

1. **Registration.** The binding performs no I/O. Misuse panics at the call: a `credo.ComponentName` name the health-name rules refuse (empty, padded, with a control character or the reserved `credo.` prefix), as `credo: App.Provide[*sqldb.DB]: credo.ComponentName(" primary"): name " primary" must not have leading or trailing whitespace`, and a name another component has, as `a component named "primary" is already registered, as <registration>; give each component its own name with credo.ComponentName`; for a `Provide` constructor, whose value is not known yet, that conflict fails the start instead. The name is not a DI key: a second `*sqldb.DB` under another name is still a duplicate binding ([Multiple Databases](#multiple-databases)).
2. **Start.** When the start walk reaches the binding, in dependency order, it builds the value and calls its `Start`, which pings the database under `Config.PingTimeout`, derived from the start context, before the components that depend on it start. A constructor error or a failed ping — `sqldb: ping did not answer within 10s: …` when the deadline passed, else `sqldb: ping: …` — fails the start: `Run` or `app.Start` returns a `*credo.LifecycleError` that names the component, the failed `Start` has already closed the database, and the App rolls back the rest of what it built. After a successful ping, the pool warnings are logged at Warn (`credo: store configuration warning`, with `code` and `module=sqldb`, and `component=<name>` on the App's logger; `sqldb.WithLogger(l)` sends them to `l` instead).
3. **Readiness.** With [health checks](getting-started.md#health-checks) mounted, `/ready` asks the database's `Ready`, which returns nil while its `Health` is `UP` and `sqldb: database is DOWN: <cause>` otherwise. The entry is named `*sqldb.DB` or its `credo.ComponentName` name, carries the ping's `latency`, and is built once at start; no readiness request resolves anything from the container. It gates readiness: while it fails, `/ready` answers 503. Before the App has started there is no entry.
4. **Teardown.** The App shuts the database down after its consumers, in the internal tier, unless it is borrowed. A borrowed database is never started or shut down by the App — its owner does both — and `/ready` still reports it.

Because the plan comes from the binding, an override is what the App pings, reports and shuts down. A test that replaces the database with `testutil.WithOverride[*sqldb.DB](testDB)` or `credo.Override()` never pings the original value.

### A database that must not gate readiness or stop the start

Two options, of different owners, relax the defaults:

```go
db, err := sqldb.Open(&cfg, sqldb.WithOptionalStart()) // a failed ping does not fail the start
if err != nil {
    return err
}
app.ProvideValue(db, credo.OptionalReadiness()) // reported in /ready without gating it
```

- **`credo.OptionalReadiness()`** on the binding reports the database without letting it gate readiness: a failing entry shows `down` and is logged, and while every gating entry is up `/ready` answers 200 with the top-level status `degraded`. It changes readiness only: without `WithOptionalStart`, an unreachable database still fails the start.
- **`sqldb.WithOptionalStart()`** on `Open` makes a failed ping non-fatal: `Start` logs `sqldb: ping failed; starting without the database, which gates readiness until it answers` at Warn, with `ping_timeout` and `error`, logs the pool warnings, and returns nil, leaving the pool open, since `database/sql` opens connections on demand once the server is back. `Ready` keeps pinging, so the database gates readiness until it answers unless it is also bound with `credo.OptionalReadiness()`.

Gating is the default for a reason, and it has a cost under load; the [deployment guide](deployment.md#shutdown-and-readiness) explains both.

### One database, several holders

A database's teardown follows the component rule of the [Dependency Injection Guide](dependency-injection.md#wrappers-and-resource-identity): values that share a resource identity are one resource. `*sqldb.DB` implements `credo.ResourceIdentifier` — `ResourceIdentity()` returns the `*DB` itself — and a wrapper that embeds it inherits that identity:

- A `*sqldb.DB` bound raw and a wrapper that embeds it are one database: one ping, one readiness entry and one teardown, after the consumers of both, in either registration order.
- Their registrations agree on `credo.OptionalReadiness()` and on the `credo.ComponentName` name, which the entry and the teardown carry. A disagreement panics at the second `ProvideValue`, or fails the start when a `Provide` constructor builds the second value, naming both registrations and the remedies: give both the same options, or hold the handle in a named field.
- Only a wrapper without state of its own to release, and without readiness of its own, may share the identity. A wrapper that releases state of its own, or overrides the `Ready` it embeds, holds the `*DB` in a named field and does not forward `ResourceIdentity`; it is then a resource of its own ([A wrapper with a lifecycle of its own](#a-wrapper-with-a-lifecycle-of-its-own)).
- A named-field wrapper without such state forwards the identity with one method, `ResourceIdentity() any`. Credo never scans wrapper fields.

Another interface view of the same database is an alias, never a second binding:

```go
type StoreHealth interface {
    Health(context.Context) store.Health
}

app.ProvideValue(db)
app.Alias[StoreHealth, *sqldb.DB]()
```

`Resolve[StoreHealth]` returns the bound `*sqldb.DB`; nothing else is pinged, reported or shut down.

### A wrapper with a lifecycle of its own

A store whose teardown must release more than the handle, or an adapter over another client, is a wrapper type that implements `Start`, `Ready` and `Shutdown` itself, as every component does; there is no framework interface to name them. The wrapper is the binding, and its methods act on one value:

```go
type ReportingDB struct {
    db     *sqldb.DB // named field: the wrapper owns the handle
    buffer *AuditBuffer
}

func (r *ReportingDB) Start(ctx context.Context) error { return r.db.Start(ctx) }
func (r *ReportingDB) Ready(ctx context.Context) error { return r.db.Ready(ctx) }

func (r *ReportingDB) Shutdown(ctx context.Context) error {
    return errors.Join(r.buffer.Flush(ctx), r.db.Shutdown(ctx))
}

app.ProvideValue(&ReportingDB{db: reportingHandle, buffer: NewAuditBuffer()}, credo.ComponentName("reporting"))
```

`Start` must honour its context — a start that ignores its deadline is abandoned only at the rollback deadline — and release what it opened when it fails, since the App does not shut down a component whose `Start` failed. To keep the start and the teardown with the caller instead, bind the wrapper with `credo.Borrowed()`: it is still reported by `/ready`, and the caller starts it and closes it after `Run` returns.

---

## Multiple Databases

Credo DI keys bindings by Go type, so two databases cannot both be bound as `*sqldb.DB`. Each database beyond a default `*sqldb.DB` is a wrapper type, bound once and named with `credo.ComponentName`:

```go
type PrimaryDB struct{ *sqldb.DB }
type AnalyticsDB struct{ *sqldb.DB }

func setupMultiDB(app *credo.App) error {
    primaryCfg, err := app.GetConfig[sqldb.Config]("databases.primary")
    if err != nil {
        return err
    }
    analyticsCfg, err := app.GetConfig[sqldb.Config]("databases.analytics")
    if err != nil {
        return err
    }

    primary, err := sqldb.Open(&primaryCfg)
    if err != nil {
        return err
    }
    app.ProvideValue(PrimaryDB{primary}, credo.ComponentName("primary"))

    // A constructor opens the database in the start phase instead.
    app.Provide(func() (AnalyticsDB, error) {
        db, err := sqldb.Open(&analyticsCfg)
        return AnalyticsDB{db}, err
    }, credo.ComponentName("analytics"))
    return nil
}
```

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

The type is the qualifier: the constructor's signature names the database, the compiler and `Finalize` check the choice, and the graph orders each database's ping, start and stop by its consumers — no string keys, no ambiguity in constructors. Embedding carries every capability of the handle — `Start`, `Ready`, `Shutdown`, `ResourceIdentity`, `Ping`, `Health` and the query proxies (`db.Select(...)`, `db.InTx(...)`) — and each `*sqldb.DB` owns its transaction scope, so the transactions of two databases never meet. Because the wrapper inherits the handle's resource identity, `PrimaryDB` and the `*sqldb.DB` inside it are one database with one teardown; a wrapper that releases state of its own holds the handle in a named field instead ([A wrapper with a lifecycle of its own](#a-wrapper-with-a-lifecycle-of-its-own)).

The rest of the rule:

- **An interface view is an alias.** `app.Alias[ReportingStore, AnalyticsDB]()` gives consumers an interface without a second binding to ping, report or shut down.
- **A constructor that needs the raw handle unwraps it** rather than binding it again: `&AuditRepo{db: db.DB}` inside `NewAuditRepo(db AnalyticsDB)`, or `db.Client()` for Bun.
- **A generic repository is instantiated per wrapper.** Each instantiation is its own binding, with no name between them:

  ```go
  // OutboxDB is what the repository uses; every wrapper over *sqldb.DB has it.
  type OutboxDB interface {
      Insert(model ...any) *sqldb.InsertQuery
      InTx(ctx context.Context, fn func(ctx context.Context) error) error
  }

  type OutboxRepo[D OutboxDB] struct{ db D }

  func NewOutboxRepo[D OutboxDB](db D) *OutboxRepo[D] { return &OutboxRepo[D]{db: db} }

  app.Provide(NewOutboxRepo[PrimaryDB])
  app.Provide(NewOutboxRepo[AnalyticsDB])
  ```

- **Databases whose number comes from configuration** — shards, tenants — are one binding of a collection type that owns and closes them all, since a type per instance cannot be written:

  ```go
  // Shards owns every shard database.
  type Shards struct{ dbs []*sqldb.DB }

  func OpenShards(cfgs []sqldb.Config) (*Shards, error) {
      s := &Shards{}
      for i := range cfgs {
          db, err := sqldb.Open(&cfgs[i])
          if err != nil {
              return nil, errors.Join(err, s.Shutdown(context.Background()))
          }
          s.dbs = append(s.dbs, db)
      }
      return s, nil
  }

  // For returns the shard of a tenant.
  func (s *Shards) For(tenantID uint64) *sqldb.DB {
      return s.dbs[tenantID%uint64(len(s.dbs))]
  }

  // Shutdown closes every shard; the App calls it once, after the consumers.
  func (s *Shards) Shutdown(ctx context.Context) error {
      var errs []error
      for _, db := range s.dbs {
          errs = append(errs, db.Shutdown(ctx))
      }
      return errors.Join(errs...)
  }

  shardCfgs := app.MustGetConfig[[]sqldb.Config]("databases.shards")
  app.Provide(func() (*Shards, error) { return OpenShards(shardCfgs) })
  ```

  To have the shards pinged at start and reported by `/ready`, give `*Shards` `Start` and `Ready` methods that ping every shard, and name its binding with `credo.ComponentName("shards")`.
- **A read replica of one database is routing**, not a second store, and sits outside the rule; `sqldb` does not expose it.

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

From a handler, pass the request context: `db.InTx(c.Context(), fn)`. The package-level `sqldb.RunInTx(ctx, db, fn)` is equivalent; `InTxWith` / `RunInTxWith` accept `sql.TxOptions` for isolation level and read-only mode.

The callback error is a domain value. When rollback succeeds, Credo returns the exact error unchanged; it is not reclassified from its text or passed through driver mapping. A panic triggers a rollback attempt and re-raises the same panic value. Passing a nil callback returns `sqldb.ErrNilTxCallback` before the transaction begins.

Treat the callback as the transaction lifetime boundary. Do not retain its context or launch transaction/nested work that can outlive the callback return; wait for all transaction work before returning.

Nested calls use Bun savepoints. Savepoints cannot change isolation or read-only state, so nested `InTxWith` accepts only nil or zero-valued options. Non-default nested options return `sqldb.ErrNestedTxOptions` before the savepoint and callback instead of being silently ignored. Configure isolation on the outermost transaction. Savepoint creation observes child cancellation and the configured wait budget; an uncertain begin does not invoke the callback. Cleanup remains usable after child cancellation: a nil callback result becomes `context.Canceled`/`DeadlineExceeded` and rolls back rather than releasing the savepoint. Creation, cleanup, and fail-safe ambient abort use the five-second default (or `WithTxCleanupTimeout` override) without counting callback duration. Uncertain nested state is synchronously marked rollback-only, so swallowing the inner error makes the outer `InTx` return `sqldb.ErrTxRollbackOnly` rather than commit; later nested calls fail immediately without running their callback. If a driver ignores cancellation, its connection may remain occupied until it returns, but Credo stops waiting at the budget and commit stays fail-closed.

Treat commit errors carefully: they do not universally prove that the transaction was rolled back or that retrying is safe. Retry only when the particular driver/state classification provides a definite retry contract.

Repository methods do not need a separate transaction parameter when they use `sqldb.DB` query proxies or raw helpers. The active transaction is picked up from `context.Context`.

For multi-database applications, be careful:

- `store/sqldb` scopes transaction context per `*sqldb.DB`, so two Bun connections of the same Go type do not collide implicitly
- `store/sqldb` uses Bun transaction types under the hood
- a single context does not become a distributed transaction coordinator

Practical rule:

- use `InTx` / `RunInTx` freely for one database per unit of work
- if a use case spans multiple Bun databases, keep transactions explicit and local
- do not assume Credo will coordinate cross-database commit/rollback

### Advanced Bun work inside a transaction

For a Bun feature not covered by the proxy surface, use `db.Conn(txCtx)` rather than `db.Client()`. It returns the active transaction for that specific `sqldb.DB`, or the base DB when no transaction exists:

```go
err := db.InTx(ctx, func(txCtx context.Context) error {
    var rows []AuditRow
    return db.Conn(txCtx).
        NewSelect().
        Model(&rows).
        Relation("Actor").
        Scan(txCtx)
})
```

The returned `bun.IDB` is borrowed; do not retain it beyond the callback. Native Bun executions through it participate in the transaction but do not receive Credo's `store.Err*` mapping. If transaction presence is mandatory, use `db.RequireTx(txCtx)` and handle `store.ErrTxMissing` rather than allowing a base-DB fallback.

---

## Migrations

`store/sqldb` wraps Bun's migration engine (`bun/migrate` — part of the already-pinned Bun module, not a new dependency). Register the set at wiring time. For development, tests, or a deliberate single-replica deployment, it can run as an application-start hook:

```go
import "github.com/uptrace/bun/migrate"

//go:embed migrations/*.sql
var sqlMigrations embed.FS

func main() {
    app, _ := credo.New(...)
    db := mustOpenDB()
    app.ProvideValue(db) // pinged before the start hooks run

    migrations := migrate.NewMigrations()
    if err := migrations.Discover(sqlMigrations); err != nil {
        log.Fatal(err)
    }
    db.RegisterMigrations(migrations)

    app.OnStart(db.Migrate) // dev/single-replica convenience

    app.Run()
}
```

SQL migration files follow Bun's naming scheme — `1_create_users.up.sql`, `2_add_index.up.sql` (optionally with matching `.down.sql`). Go migrations use `migrations.MustRegister(up, down)` from files named the same way.

### Production deployment model

For multi-replica production, run the same `db.Migrate` method in exactly one pre-deploy job and require it to succeed before rolling out application replicas. Give the job an explicit deadline that covers the expected migration duration:

```go
// jobCtx should be derived from the process signal / job runner context.
migrationCtx, cancel := context.WithTimeout(jobCtx, 15*time.Minute)
defer cancel()

if err := db.Migrate(migrationCtx); err != nil {
    return fmt.Errorf("apply database migrations: %w", err)
}
```

The job and `OnStart` forms share the same registration and migration behavior; only the deployment owner differs. Do not also register `app.OnStart(db.Migrate)` in every production replica. An `OnStart` hook's context carries no values from the caller and has no migration-specific deadline; it ends when the hook returns, and a shutdown requested during the start phase — a signal under `Run`, the cancelled context of `RunContext` or `ServeContext`, or `app.Shutdown` — cancels it, so `Migrate` is interrupted like any other start step and the App rolls back what it built. A hook that ignores the cancellation is abandoned when the rollback's deadline ends. The pre-deploy job gives the migration its own deadline and is never interrupted by an application replica's shutdown.

What the wrapper does on each `Migrate` call:

1. creates Bun's bookkeeping tables if missing (`IF NOT EXISTS`)
2. takes a table-based advisory lock — if another runner owns it, `Migrate` fails immediately instead of waiting or retrying
3. applies unapplied migrations in order
4. attempts to release the lock under a fresh five-second cleanup budget, even when the migration context was cancelled

The unlock wait is caller-bounded even if a driver ignores context. If it times out, `Migrate` returns a timeout (joined with any migration error), but the outcome is uncertain: the lock row may remain, or the delayed driver operation may delete it later. Bun's lock row has no owner token or lease. Let the old process/job terminate and inspect active database sessions before recovery; never blindly delete the row and immediately start another migrator while the old Unlock may still execute. Credo deliberately does not add automatic Unlock retries or lock wait/retry—overlapping release jobs are a coordination error, and waiting can let different releases run migrations in the wrong order.

### Retry and transaction boundaries

By default a migration is marked applied only after its Up function returns nil. This means an error surfaced by Bun leaves it eligible for another attempt; it does **not** make the attempt atomic or automatically safe to retry:

- ordinary `.up.sql` files and Go migrations are non-transactional unless they explicitly open a transaction
- earlier statements or earlier migrations in the group may already be committed
- the migration body can succeed and its separate applied-marker write can fail or be interrupted
- database DDL transaction rules still apply; some statements or engines implicitly commit or cannot run inside a transaction

Treat mark-on-success as at-least-once execution. Prefer database-supported transactions where appropriate, and make every retryable step idempotent, resumable, or accompanied by an explicit inspection/repair procedure. After a partial failure, inspect schema and data before rerunning instead of assuming “unapplied” means “nothing changed.” Passing `migrate.WithMarkAppliedOnSuccess(false)` selects Bun's record-before-running recovery tradeoff; it can make a failed body appear applied and requires explicit rollback/repair instead.

Bun recognizes `.tx.up.sql`: the file runs in one transaction, and the error of its COMMIT or ROLLBACK reaches `Migrate`, so a commit that fails — a deferred constraint violated at COMMIT, a serialization failure — leaves the migration unapplied and eligible for the next run. Driver-specific ambiguous commit outcomes (a timeout while the server was committing) and DDL that a database cannot roll back remain the application's concern.

### Expand-contract rollout

For a rolling deployment:

1. **Expand** with additive changes compatible with both the old and new binaries, using the one-shot migration job.
2. **Deploy** the compatible application version to all replicas.
3. **Backfill** large data changes as a separate bounded, resumable, idempotent job.
4. Verify no old replica or job remains.
5. **Contract** in a later release: remove/rename columns, add strict constraints, or perform other destructive changes only after every consumer is compatible.

**Seeding** is just another migration file — there is no separate seed mechanism:

```sql
-- migrations/3_seed_plans.up.sql
INSERT INTO plans (name, price) VALUES ('free', 0), ('pro', 1900);
```

For rollback, status inspection, or generating migration files, drop down to Bun's migrator via the escape hatch. A directly constructed migrator does not inherit the options passed to `RegisterMigrations` (including custom table names, hooks, or Credo's mark-on-success default). Read-only status and file generation repeat only the options they need. DB-mutating apply/rollback paths must additionally own Init, Lock, and a bounded cancellation-detached Unlock:

```go
migrator := migrate.NewMigrator(
    db.Client(),
    migrations,
    migrate.WithMarkAppliedOnSuccess(true),
    // Repeat the same WithTableName / WithLocksTableName / hook options.
)
// After caller-owned Init + Lock, and with a deferred bounded Unlock:
group, err := migrator.Rollback(ctx)
```

---

## Reusing Filters Across Queries

`Apply(...)` is typed per query — a `func(*bun.SelectQuery) *bun.SelectQuery` cannot be applied to an update or delete. When the _same_ WHERE logic must run across reads and writes — tenant scoping, soft-delete filters, ownership checks — use `ApplyFilter`, whose callback takes the `*sqldb.Filter` that the select, update and delete proxies share:

```go
// One predicate, reused everywhere.
func tenantScope(tenantID int64) func(*sqldb.Filter) {
    return func(f *sqldb.Filter) {
        f.Where("tenant_id = ?", tenantID)
    }
}

scope := tenantScope(tid)

err := db.Select(&users).ApplyFilter(scope).Scan(ctx)
_, err = db.Update((*User)(nil)).Set("status = ?", "archived").
    ApplyFilter(scope).Exec(ctx)
_, err = db.Delete((*User)(nil)).ApplyFilter(scope).Exec(ctx)
```

Conditions added through the filter land on the proxied query, so the terminal methods still apply TX injection and error mapping, and `Count` and `Page` keep their automatic total. A nil callback is a no-op.

`Filter` has `Where`, `WhereOr`, `WhereGroup`, `WherePK`, `WhereDeleted` and `WhereAllWithDeleted`, and nothing else; each returns the `*Filter` for chaining, and a `Filter` is valid only during the callback. `WhereGroup`, which the curated proxy set does not surface directly, puts its callback's conditions in one parenthesized group:

```go
err := db.Select(&users).
    ApplyFilter(func(f *sqldb.Filter) {
        f.WhereGroup(" AND ", func(g *sqldb.Filter) {
            g.Where("role = ?", "admin").WhereOr("role = ?", "owner")
        })
    }).
    Scan(ctx)
```

The callback's signature names only `sqldb.Filter`, so a shared predicate keeps `bun` out of repository code.

---

## Raw SQL And Bun Escape Hatch

Credo does not hide Bun — it integrates it. If the proxy layer does not cover a Bun feature you need, use the escape hatches: a missing _builder_ method is reached with `Apply` (proxy guarantees preserved; a select built with it pages with `PageWithCount`); a missing _terminal_ method is worth a feature request — the guarantees live in the terminals, so they belong on the proxy. `Unwrap()` and `Client()` opt out of the guarantees entirely.

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

- **No automatic TX injection** — an `InTx` / `RunInTx` block does not affect calls built directly from `db.Client()`. Unless the caller explicitly binds another connection with Bun's `.Conn(...)`, the query uses the base pool outside the ambient transaction.
- **No error mapping** — `sql.ErrNoRows` is returned as-is, not as `store.ErrNotFound`. Driver-specific constraint codes leak through unchanged. Calling code must import `database/sql` (or the driver package) to interpret them.

Reserve `Client()` for model registration, advanced migration operations, and raw SQL the proxy layer cannot express. Use the proxy layer (`db.Select` / `db.Insert` / `db.Update` / `db.Delete`) for normal repository code, even when the query is non-trivial.

When native Bun work must join an ambient transaction, use `db.Conn(ctx)` as shown above. It selects the active connection but intentionally does not add error mapping. Credo does not make `Client()` implicitly transaction-aware: doing so through Bun's single `ConnResolver` slot would cover query builders but not direct `ExecContext`, `QueryContext`, `QueryRowContext`, or `BeginTx`, conflict with future replica routing, and transfer resolver shutdown ownership to Bun.

---

## Other ORMs

Credo ships one first-class SQL adapter: `store/sqldb` on top of Bun.

If you use another ORM or client:

- register it through DI directly
- keep Credo's higher-level application structure the same
- to have it pinged at start, reported by `/ready` and shut down after its consumers, bind it through a wrapper that implements `Start`, `Ready` and `Shutdown` ([A wrapper with a lifecycle of its own](#a-wrapper-with-a-lifecycle-of-its-own))

Example:

```go
gormDB, err := gorm.Open(...)
if err != nil {
    return err
}

app.ProvideValue(gormDB)
```

That path works, but you do not get the Bun-specific features from `store/sqldb`.

---

## Recommended Patterns

For most applications:

1. load `sqldb.Config` with `app.GetConfig` during registration
2. open the connection with `sqldb.Open`, or in a constructor that the start phase runs
3. bind it (`app.ProvideValue` or `app.Provide`); the binding pings it at start, reports it in `/ready` and shuts it down after its consumers
4. inject the resulting type into repositories
5. keep services and controllers free of DSN strings and runtime config lookups

For multiple databases:

1. create wrapper types such as `PrimaryDB` and `AnalyticsDB`
2. bind each wrapper once, named with `credo.ComponentName`
3. inject wrappers explicitly in constructors; reach other views through `Alias`, not a second binding
4. keep transaction boundaries local to a single database unless you have a very deliberate explicit strategy

---

## Related Documents

- [Dependency Injection Guide](dependency-injection.md)
- [Configuration Guide](configuration.md)
- [Store Spec](../specs/store.md)
- [Pagination Spec](../specs/pagination.md)
- [ADR-015](../adr/015-data-access.md)
