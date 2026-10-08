# Data Access Guide

This guide explains how to use Credo's data access stack in application code. For low-level contracts and design rationale, see the [Store Spec](../specs/store.md) and [ADR-015](../adr/015-data-access.md).

All config examples in this guide use JSON for consistency. Credo also supports YAML/YML with the same structure.

Credo's data access story has two layers:

- `store/`: core contracts, store registration (start-phase ping and readiness reporting), transaction helpers
- `store/sqldb/`: Bun-based SQL wrapper with query proxies and transaction support

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
    "github.com/credo-go/credo/store"
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

    app.ProvideValue(db)           // the App owns db from here on
    store.Register[*sqldb.DB](app) // pinged at start, reported by /ready
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
- read `sqldb.Config` with `app.GetConfig` during registration (`Resolve` is available only after `Finalize`)
- bind the handle with `ProvideValue`, then name that binding as a store with `store.Register[*sqldb.DB]`

The binding decides ownership; `store.Register` adds what a data store needs on top of it, without I/O at the call:

- the start phase pings the store before the components that depend on it start, and a failed ping fails the start
- `/ready` reports its typed health under the store's name
- its secret-free configuration warnings are logged after a successful ping

From `ProvideValue` on, the App owns `db`: `*sqldb.DB` is a `credo.Component` (it has `Shutdown`), so the drain shuts it down in the internal tier, after the components constructed from it, while the shared deadline remains, and a composition root that fails after the binding releases it with `app.Shutdown(ctx)`, which tears down an App that never ran. A handle the caller shares — with a second App, or across the tests of a suite — is bound with `app.ProvideValue(db, credo.Borrowed())`: it is still pinged and reported, and its shutdown stays the caller's.

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

PostgreSQL represents `connect_timeout` in whole seconds, so Credo rounds any positive fractional value up rather than silently truncating it to disabled. `Options` may add driver parameters but cannot override core PostgreSQL endpoint/credential keys, MySQL's required `parseTime=true`, or a simultaneously set `SSLMode`/`ConnectTimeout`; ambiguous values fail without being echoed in the error. `SSLMode` itself is driver-specific (`sslmode` for PostgreSQL, `tls` for MySQL). Credo sets no universal TLS default—production configuration must choose a verified mode and trust setup supported by the selected driver. See the canonical [store specification](../specs/store.md#config) for the complete precedence and escape-hatch contract.

There is intentionally no universal finite pool default. `max_open: 0` (and an omitted `max_open`) retains `database/sql`'s unlimited-open behavior. A store registered with `store.Register` logs one structured warning with code `sqldb.pool.max_open_unlimited` after its start-phase ping succeeds when the effective pool maximum is still unlimited; it never silently changes the value. Services that open a DB without `store.Register` can inspect `db.StoreRegistrationWarningCodes()` during bootstrap and send the returned secret-free codes to their own logger.

`max_idle` distinguishes omission from an explicit zero. Omit it to leave the idle setter to `database/sql` (its effective default remains subject to `max_open`), set it to `0` to retain no idle connections, or set a positive limit. With a finite `max_open`, `max_idle` must not be greater than `max_open`; `sqldb.Open` rejects that combination rather than accepting the stdlib's silent clamp. `max_idle_time: 0` disables idle-age expiry, while `max_lifetime: 0` disables connection-lifetime expiry. Explicit positive values are applied unchanged; Credo does not overwrite them with defaults.

For operational telemetry, `db.Stats()` returns the complete `sql.DBStats` snapshot. Track at least `InUse`, `Idle`, `WaitCount`, `WaitDuration`, `MaxIdleClosed`, `MaxIdleTimeClosed`, and `MaxLifetimeClosed`. Wait and closure counters are cumulative: alert on windowed rates/deltas tied to an SLO, not on raw totals. Credo does not mark a pool `DEGRADED` from a universal saturation threshold. Such a policy needs explicit opt-in thresholds and hysteresis; today `DEGRADED` removes readiness for every store and a noisy threshold could cause cascading traffic shifts.

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
- escape hatches via `Apply(...)`, `ApplyQueryBuilder(...)`, and `Unwrap()`

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
func (h *UserHandler) List(ctx *credo.Context) error {
    var req pagination.PageRequest
    if err := ctx.Request().BindQuery(&req); err != nil {
        return err
    }
    page, err := h.users.List(ctx.Context(), &req)
    if err != nil {
        return err
    }
    return ctx.Response().JSON(http.StatusOK, page)
}
```

Outside a handler, call `req.Normalize()` (or `NormalizeWithMax` for a higher per-page cap) yourself when you want the same forgiving policy. Directly constructed requests may also be passed as-is, but `Page` requires positive values and a representable execution window; it never silently defaults or clamps them. Nil, zero/negative and native `int` offset overflow all return an error matching `pagination.ErrInvalidPageRequest` before COUNT. A custom normalized `PerPage` such as 100 is valid and remains 100. For direct offset calculations, handle the new strict signature: `offset, err := req.Offset()`.

When COUNT reports zero rows, SELECT is skipped and the page comes back with a non-nil empty `Records` slice and the snapshot's page/per-page preserved. Use a stable total order for every offset-paginated query. If the primary sort key can repeat, append a unique tie-breaker such as `id`; `created_at DESC` alone does not determine which equal-timestamp record belongs to which page.

#### What `Total` counts

`Page.Total` is the number of complete logical projection rows before ordering and the Page-owned LIMIT/OFFSET window. Credo removes root ORDER/LIMIT/OFFSET/FOR state and counts a universal outer `_credo_count_source` derived table:

| Query | Total |
| --- | --- |
| Plain filtered projection | Projection rows |
| Ungrouped aggregate projection | Normally one row, including `COUNT(*)` over empty input |
| `Column(...).Distinct()` | Distinct selected projection tuples |
| `GroupExpr(...)` | Groups |
| `GroupExpr(...).Having(...)` | Groups left after `Having` |

Credo pins both the outer SQL shape and its behavior with conformance tests. Two shapes are rejected before database I/O because their Count+window semantics are not safe:

```go
_, err := db.Select((*User)(nil)).
    Having("COUNT(*) > 0").
    Count(ctx)
// errors.Is(err, sqldb.ErrUnsupportedCountQuery) == true

_, err = db.Select().
    Apply(func(q *bun.SelectQuery) *bun.SelectQuery {
        return q.UnionAll(other)
    }).
    Page[User](ctx, req)
// errors.Is(err, sqldb.ErrUnsupportedCountQuery) == true
```

For a compound query, place the compound SELECT behind an outer derived-table or CTE count source. If the data side also needs a custom source or destination, run an explicit count query and data query, then call `pagination.NewPage(records, int64(total), req.Page, req.PerPage)`. Typed `Page[T]` remains a model-owned terminal; wrapping a projection does not turn it into a general projection API.

MySQL requires unique derived-table output names. Credo renders the logical count source once and lets the server apply its actual naming and `sql_mode` rules, so wildcard and implicit/unaliased expressions are accepted when their derived names are unique. If MySQL returns `ER_DUP_FIELDNAME` (1060) while executing Count/Page's COUNT statement, Credo wraps `sqldb.ErrUnsupportedCountQuery` after I/O and preserves the driver cause. Give colliding projections explicit unique aliases:

```go
total, err := db.Select((*User)(nil)).
    ColumnExpr("LOWER(name) AS normalized_name").
    Count(ctx)
```

The wrapper is local to the logical count execution point. A raw query, `Scan`, `Exists`, or other non-count operation returning MySQL 1060 remains the original driver error. Because the server does not identify which derived-table level failed, an indistinguishable 1060 from a caller-supplied nested source during Count/Page is wrapped too. Keep the retained cause for logs and diagnostics; never render raw driver messages directly to HTTP clients. Real conformance covers normal mode and `NO_BACKSLASH_ESCAPES`. See MySQL's [derived-table rule](https://dev.mysql.com/doc/mysql/en/derived-tables.html).

Relation callbacks are evaluated once while Credo renders the count source. They may add predicates or relation projections. Do not use them to replace the root model or add root ORDER/LIMIT/OFFSET/FOR, standalone `Having`, or a direct compound query; those mutations return `sqldb.ErrUnsupportedCountQuery` before I/O.

The universal count source evaluates the complete projection. This is what makes aggregate and set-returning cardinality exact, but a costly or volatile expression may run once for COUNT and again for the data SELECT.

Model SELECT hooks are not bypassed by the logical count. Credo runs `BeforeSelect`, `BeforeAppendModel`, and successful-query `AfterSelect` on the private count source; when Page also runs its data SELECT, the normal Bun scan invokes them again. A hook-added tenant predicate or projection therefore contributes to both `Total` and `Records`. Query hooks still receive the model through `QueryEvent.Model`; soft-delete filtering is kept inside the derived source so it is applied once rather than again by the outer count. Count does not scan or change a bound model, so its successful `AfterSelect` observes the value that existed before Count.

Keep query-shaping hooks deterministic. Repeatable Read can stabilize rows seen by the database, but it cannot make a volatile expression or an application-side hook decision produce the same result in COUNT and SELECT.

There is no custom-count callback/strategy on `Page`. For an expensive or volatile projection, reuse common predicates between a deliberately cheaper count builder and the data builder with `ApplyQueryBuilder`; use `Apply` for Bun-specific builder features, execute both explicitly, and construct `pagination.NewPage`. The repository owns query equivalence, PageRequest/window validation, and the shared transaction context. A first-class strategy waits until two real consumers repeat the same abstraction.

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

## What `store.Register` Does

`store.Register[R]` names a binding as a data store. It takes no value and returns nothing: the store is bound where its ownership is decided — `app.ProvideValue`, or `app.Provide` with a constructor, with `credo.Borrowed()` for a handle the caller shares — and `Register` refers to that binding by its type:

```go
cfg := app.MustGetConfig[sqldb.Config]("databases.default")

// The constructor runs in the start phase, after Finalize and every override.
app.Provide[*sqldb.DB](func() (*sqldb.DB, error) { return sqldb.Open(&cfg) })
store.Register[*sqldb.DB](app,
    store.WithName("primary"),
    store.WithPingTimeout(10*time.Second),
)
```

A registration goes through four phases:

1. **Registration.** `Register` records the type, the name and the ping timeout, and performs no I/O. Misuse panics at the call: a nil App or option, a ping timeout that is not positive, an invalid name, an `R` without a stable default name and no `WithName`, a type or a name registered twice, and a call after `Finalize`, after the App is prepared, or after shutdown. An `R` that does not implement `store.Lifecycle` does not compile.
2. **`Finalize`.** `R` may be bound directly or be an interface an `Alias` names. A registration whose `R` has no binding fails `Finalize` — `di: store.Register[*sqldb.DB]: *sqldb.DB has no binding; bind it with Provide or ProvideValue before Finalize` — reported together with the graph's other findings.
3. **Start.** When the start walk reaches the binding, in dependency order, it builds the value and pings it as the binding's first start step — before the value's own `Start` and before the components that depend on it start — under a deadline of `WithPingTimeout` (default `store.DefaultPingTimeout`, 5 seconds) derived from the start context. A constructor error, a nil bound value (`store: "primary": the bound value is nil`) or a failed ping (`store: ping "primary": …`) fails the start: `Run` or `app.Start` returns a `*credo.LifecycleError` that names the component, and the App rolls back what it built, the store included unless it is borrowed. After a successful ping, the store's secret-free warning codes are logged at Warn (`credo: store configuration warning`, with `store` and `code` attributes).
4. **Readiness.** With [health checks](getting-started.md#health-checks) mounted, `/ready` reports the typed `Health` of each pinged store under its name, through a probe built once at start; no readiness request resolves anything from the container. Before the App has started there are no store checks.

The teardown is the binding's, not the registration's: the App shuts the store down after its consumers, in the internal tier, unless it is borrowed. A binding without `store.Register` is an ordinary component — still shut down after its consumers, but neither pinged at start nor reported by `/ready`.

Because the registration names a binding rather than holding a value, an override is what the App pings, reports and shuts down. A test that replaces the database with `testutil.WithOverride[*sqldb.DB](testDB)` or `credo.Override()` never pings the original value.

`Lifecycle.Ping` implementations must honor `ctx`: a ping that ignores its deadline is abandoned only at the rollback deadline.

Store names use the same rules as named health checks. An explicit empty name, leading or trailing whitespace, control characters, and the reserved `credo.` prefix are rejected rather than normalized. If `WithName` is omitted, Credo unwraps pointer layers and uses the package-qualified name of the registered type (`sqldb.DB` for `*sqldb.DB`); unnamed types require an explicit name. A store whose name collides with a custom readiness check makes `/ready` fail closed.

### One database, several holders

A store's teardown follows the component rule of the [Dependency Injection Guide](dependency-injection.md#wrappers-and-resource-identity): values that share a resource identity are one resource, shut down once, after the consumers of every holder. `*sqldb.DB` implements `credo.ResourceIdentifier` — `ResourceIdentity()` returns the `*DB` itself — and a wrapper that embeds it inherits that identity:

- A `*sqldb.DB` bound raw and a wrapper that embeds it are one database, closed once, after the last holder retires. Registering both with `store.Register` is not refused: each is pinged and reported under its own name.
- Only a wrapper without state of its own to release may share the identity. A wrapper that releases state of its own holds the `*DB` in a named field and does not forward `ResourceIdentity`; its own `Shutdown` releases its state and the handle, which is then the wrapper's alone.
- A named-field wrapper without such state forwards the identity with one method, `ResourceIdentity() any`. Credo never scans wrapper fields.

Another interface view of the same store is an alias, never a second binding:

```go
type StoreHealth interface {
    Health(context.Context) store.Health
}

app.ProvideValue(db)
store.Register[*sqldb.DB](app)
app.Alias[StoreHealth, *sqldb.DB]()
```

`Resolve[StoreHealth]` returns the registered `*sqldb.DB`; nothing else is pinged, reported or shut down.

### A wrapper that implements `Lifecycle`

A value that does not implement `store.Lifecycle`, or a store whose teardown must release more than the handle, is registered through a wrapper type that implements it: the wrapper is the binding, and its `Ping`, `Health` and `Shutdown` act on one value.

```go
type ReportingDB struct {
    db     *sqldb.DB // named field: the wrapper owns the handle
    buffer *AuditBuffer
}

func (r *ReportingDB) Ping(ctx context.Context) error         { return r.db.Ping(ctx) }
func (r *ReportingDB) Health(ctx context.Context) store.Health { return r.db.Health(ctx) }

func (r *ReportingDB) Shutdown(ctx context.Context) error {
    return errors.Join(r.buffer.Flush(ctx), r.db.Shutdown(ctx))
}

app.ProvideValue(&ReportingDB{db: reportingHandle, buffer: NewAuditBuffer()})
store.Register[*ReportingDB](app, store.WithName("reporting"))
```

To keep the teardown with the caller instead, bind the wrapper with `credo.Borrowed()`: it is still pinged and reported, and the caller closes it after `Run` returns.

---

## Multiple Databases

Credo DI keys bindings by Go type, so two databases cannot both be bound as `*sqldb.DB`. Each database beyond a default `*sqldb.DB` is a wrapper type, bound once and registered by its type:

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
    app.ProvideValue(PrimaryDB{primary})
    store.Register[PrimaryDB](app, store.WithName("primary"))

    // A constructor opens the database in the start phase instead.
    app.Provide[AnalyticsDB](func() (AnalyticsDB, error) {
        db, err := sqldb.Open(&analyticsCfg)
        return AnalyticsDB{db}, err
    })
    store.Register[AnalyticsDB](app, store.WithName("analytics"))
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

The type is the qualifier: the constructor's signature names the database, the compiler and `Finalize` check the choice, and the graph orders each database's ping, start and stop by its consumers — no string keys, no ambiguity in constructors. Embedding carries every capability of the handle — `Ping`, `Health`, `Shutdown`, `ResourceIdentity` and the query proxies (`db.Select(...)`, `db.InTx(...)`) — and each `*sqldb.DB` owns its transaction scope, so the transactions of two databases never meet. Because the wrapper inherits the handle's resource identity, `PrimaryDB` and the `*sqldb.DB` inside it are one database with one teardown; a wrapper that releases state of its own holds the handle in a named field instead ([A wrapper that implements `Lifecycle`](#a-wrapper-that-implements-lifecycle)).

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

  app.Provide[*OutboxRepo[PrimaryDB]](NewOutboxRepo[PrimaryDB])
  app.Provide[*OutboxRepo[AnalyticsDB]](NewOutboxRepo[AnalyticsDB])
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
  app.Provide[*Shards](func() (*Shards, error) { return OpenShards(shardCfgs) })
  ```

  To have the shards pinged at start and reported by `/ready`, give `*Shards` `Ping` and `Health` methods — it then implements `store.Lifecycle` — and register it with `store.Register[*Shards](app, store.WithName("shards"))`.
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

From a handler, pass the request context: `db.InTx(ctx.Context(), fn)`. The package-level `sqldb.RunInTx(ctx, db, fn)` is equivalent; `InTxWith` / `RunInTxWith` accept `sql.TxOptions` for isolation level and read-only mode.

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
    app.ProvideValue(db)
    store.Register[*sqldb.DB](app) // pinged before the start hooks run

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
3. bind it (`app.ProvideValue` or `app.Provide`) and name the binding with `store.Register`
4. inject the resulting type into repositories
5. keep services and controllers free of DSN strings and runtime config lookups

For multiple databases:

1. create wrapper types such as `PrimaryDB` and `AnalyticsDB`
2. bind each wrapper once and register it by its type with `store.Register[R]`
3. inject wrappers explicitly in constructors; reach other views through `Alias`, not a second binding
4. keep transaction boundaries local to a single database unless you have a very deliberate explicit strategy

---

## Related Documents

- [Dependency Injection Guide](dependency-injection.md)
- [Configuration Guide](configuration.md)
- [Store Spec](../specs/store.md)
- [Pagination Spec](../specs/pagination.md)
- [ADR-015](../adr/015-data-access.md)
