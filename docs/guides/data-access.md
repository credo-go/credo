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
- you want startup ping, DI-owned deadline-aware shutdown, and health registration
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
    "context"
    "errors"
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

    if err := store.Register[*sqldb.DB](app, db); err != nil {
        // Register did not take ownership.
        return errors.Join(err, db.Shutdown(context.Background()))
    }
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
- unmarshal `sqldb.Config` from `credo.RawConfig`
- use `store.Register[*sqldb.DB]` instead of raw `ProvideValue`

`store.Register` adds more than DI registration:

- rejects local name/lifecycle/DI conflicts before network I/O
- pings the connection at startup
- tracks it in the store registry for health reporting
- makes DI the sole framework shutdown owner: `*sqldb.DB` implements
  `credo.Shutdowner`, so a live teardown deadline allows the container to
  attempt it in reverse registration order

Ownership transfers only when `Register` succeeds. If it returns an error,
including a Ping error, close `db` yourself before returning from composition.

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

Example production config file (capacity values are illustrative; size them
against the database's connection budget and the service's replica count):

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

`redacted` is a placeholder; load the real password through the application's
environment or secret-backed configuration source.

If `DSN` is set, the structured connection fields are ignored.

There is intentionally no universal finite pool default. `max_open: 0` (and an
omitted `max_open`) retains `database/sql`'s unlimited-open behavior. A
successful `store.Register` logs one structured warning with code
`sqldb.pool.max_open_unlimited` when the effective pool maximum is still
unlimited; it never silently changes the value. Services that open a DB
without `store.Register` can inspect
`db.StoreRegistrationWarningCodes()` during bootstrap and send the returned
secret-free codes to their own logger.

`max_idle` distinguishes omission from an explicit zero. Omit it to leave the
idle setter to `database/sql` (its effective default remains subject to
`max_open`), set it to `0` to retain no idle connections, or set a positive
limit. With a finite `max_open`, `max_idle` must not be greater than `max_open`;
`sqldb.Open` rejects that combination rather than accepting the stdlib's
silent clamp. `max_idle_time: 0` disables idle-age expiry, while
`max_lifetime: 0` disables connection-lifetime expiry. Explicit positive values
are applied unchanged; Credo does not overwrite them with defaults.

For operational telemetry, `db.Stats()` returns the complete `sql.DBStats`
snapshot. Track at least `InUse`, `Idle`, `WaitCount`, `WaitDuration`,
`MaxIdleClosed`, `MaxIdleTimeClosed`, and `MaxLifetimeClosed`. Wait and closure
counters are cumulative: alert on windowed rates/deltas tied to an SLO, not on
raw totals. Credo does not mark a pool `DEGRADED` from a universal saturation
threshold. Such a policy needs explicit opt-in thresholds and hysteresis;
today `DEGRADED` removes readiness for every store and a noisy threshold could
cause cascading traffic shifts.

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

`SelectQuery.Limit` and `Offset` add one adapter guard around Bun v1.2.18. Bun's API accepts `int` but stores the values as signed `int32`; a value outside that range records `sqldb.ErrInvalidLimitOffset`, and the terminal returns before sending SQL. Values inside the range, including zero and negatives, keep Bun's normal semantics. This applies to the curated proxy methods only. If `Apply` or `Unwrap` is used to call raw Bun, Bun's own conversion behavior applies.

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

Mapped values are `*store.Error`: the original driver cause and code remain in
the error chain, while Credo's default HTTP response sees only the semantic
kind. Use `store.KindOf(err)` when a switch is clearer than several
`errors.Is` checks. `store.IsTransient(err)` means only that the condition may
clear; it does **not** mean replaying the statement, transaction callback, or
external side effects is safe.

`store.ErrDuplicate` remains an alias of `ErrAlreadyExists`. The deprecated
`ErrConflict` remains an umbrella match for constraint, serialization,
deadlock, and contention during migration, but new code should branch on the
exact sentinel or kind.

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

Outside a handler, call `req.Normalize()` (or `NormalizeWithMax` for a higher per-page cap) yourself when you want the same forgiving policy. Directly constructed requests may also be passed as-is, but `Page` requires positive values and a representable execution window; it never silently defaults or clamps them. Nil, zero/negative, native `int` offset overflow, and Bun v1.2.18 signed-int32 LIMIT/OFFSET overflow all return an error matching `pagination.ErrInvalidPageRequest` before COUNT. A custom normalized `PerPage` such as 100 is valid and remains 100. For direct offset calculations, handle the new strict signature: `offset, err := req.Offset()`.

When COUNT reports zero rows, SELECT is skipped and the page comes back with a non-nil empty `Records` slice and the snapshot's page/per-page preserved. Use a stable total order for every offset-paginated query. If the primary sort key can repeat, append a unique tie-breaker such as `id`; `created_at DESC` alone does not determine which equal-timestamp record belongs to which page.

#### What `Total` counts

`Page.Total` is the number of complete logical projection rows before ordering
and the Page-owned LIMIT/OFFSET window. Credo removes root
ORDER/LIMIT/OFFSET/FOR state and counts a universal outer
`_credo_count_source` derived table:

| Query | Total |
| --- | --- |
| Plain filtered projection | Projection rows |
| Ungrouped aggregate projection | Normally one row, including `COUNT(*)` over empty input |
| `Column(...).Distinct()` | Distinct selected projection tuples |
| `GroupExpr(...)` | Groups |
| `GroupExpr(...).Having(...)` | Groups left after `Having` |

Credo pins both the outer SQL shape and its behavior with conformance tests. Two
shapes are rejected before database I/O because their Count+window semantics
are not safe:

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

For a compound query, place the compound SELECT behind an outer derived-table
or CTE count source. If the data side also needs a custom source or destination,
run an explicit count query and data query, then call
`pagination.NewPage(records, int64(total), req.Page, req.PerPage)`. Typed
`Page[T]` remains a model-owned terminal; wrapping a projection does not turn it
into a general projection API.

On MySQL, give every raw projected expression a distinct portable ASCII `AS`
alias. MySQL requires unique derived-table output names, so Credo rejects
duplicate names, wildcards, implicit aliases, and output names it cannot prove
before database I/O:

```go
total, err := db.Select((*User)(nil)).
    ColumnExpr("LOWER(name) AS normalized_name").
    Count(ctx)
```

The error matches `sqldb.ErrUnsupportedCountQuery`. Qualified model columns are
recognized automatically. Credo checks both normal backslash escaping and
`NO_BACKSLASH_ESCAPES`; SQL mode cannot hide a projection separator or
executable comment from the guard. This extra guard is MySQL-only. See MySQL's
[derived-table rule](https://dev.mysql.com/doc/mysql/en/derived-tables.html).

Relation callbacks are evaluated once while Credo renders the count source.
They may add predicates or relation projections. Do not use them to replace the
root model or add root ORDER/LIMIT/OFFSET/FOR, standalone `Having`, or a direct
compound query; those mutations return `sqldb.ErrUnsupportedCountQuery` before
I/O.

The universal count source evaluates the complete projection. This is what
makes aggregate and set-returning cardinality exact, but a costly or volatile
expression may run once for COUNT and again for the data SELECT.

Model SELECT hooks are not bypassed by the logical count. Credo runs
`BeforeSelect`, `BeforeAppendModel`, and successful-query `AfterSelect` on the
private count source; when Page also runs its data SELECT, the normal Bun scan
invokes them again. A hook-added tenant predicate or projection therefore
contributes to both `Total` and `Records`. Query hooks still receive the model
through `QueryEvent.Model`; soft-delete filtering is kept inside the
derived source so it is applied once rather than again by the outer count.
Count does not scan or change a bound model, so its successful `AfterSelect`
observes the value that existed before Count.

Keep query-shaping hooks deterministic. Repeatable Read can stabilize rows seen
by the database, but it cannot make a volatile expression or an
application-side hook decision produce the same result in COUNT and SELECT.

There is no custom-count callback/strategy on `Page`. For an expensive or
volatile projection, reuse common predicates between a deliberately cheaper
count builder and the data builder with `ApplyQueryBuilder`; use `Apply` for
Bun-specific builder features, execute both explicitly, and construct
`pagination.NewPage`. The repository owns query equivalence,
PageRequest/window validation, and the shared transaction context. A
first-class strategy waits until two real consumers repeat the same
abstraction.

#### Keeping COUNT and SELECT on one database snapshot

COUNT and SELECT are separate statements. `Page` never starts an implicit
transaction, and without an explicit transaction the pool can run them on
different connections and snapshots. Even inside a transaction, the guarantee
depends on the database and isolation level.

For PostgreSQL or InnoDB, request Repeatable Read on the **outermost**
transaction when a shared snapshot is required, and pass the callback's
`txCtx`—not the outer `ctx`—to `Page`:

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

Credo rejects non-default transaction options on a nested savepoint with
`sqldb.ErrNestedTxOptions`; a nested call cannot upgrade an outer transaction's
isolation.

| Database | COUNT/SELECT visibility |
| --- | --- |
| PostgreSQL | Default Read Committed takes a fresh snapshot for each statement, so drift is allowed. Repeatable Read fixes the snapshot at the transaction's first non-control statement. |
| MySQL/InnoDB | Default Repeatable Read makes ordinary nonlocking consistent reads share the first-read snapshot. Server configuration may change the default; other engines, locking reads, and Read Committed differ, so request Repeatable Read explicitly. |
| SQLite | A plain explicit transaction keeps its first-read snapshot. WAL permits another connection to commit while the reader keeps that snapshot; rollback-journal mode may block the writer. Shared cache with `PRAGMA read_uncommitted=ON` is the exception. |

The pinned modernc SQLite driver does not reliably enforce
`sql.TxOptions.Isolation` or `ReadOnly`; for SQLite, use plain `db.InTx` as the
explicit snapshot boundary instead of presenting those options as a guarantee.
Fail-loud driver-capability validation is deferred. See
[PostgreSQL transaction isolation](https://www.postgresql.org/docs/current/transaction-iso.html),
[InnoDB transaction isolation](https://dev.mysql.com/doc/refman/8.4/en/innodb-transaction-isolation-levels.html),
[InnoDB consistent reads](https://dev.mysql.com/doc/refman/8.4/en/innodb-consistent-read.html),
[SQLite isolation](https://www.sqlite.org/isolation.html), and
[SQLite transactions](https://www.sqlite.org/lang_transaction.html).

#### Why there is no `WithCount(false)`

`Page` always has exact `Total`/`TotalPages` metadata, and `HasNext` derives
from it. An unknown total is not encoded as zero, `-1`, a pointer, or an omitted
field. Total-free offset pagination uses `Slice[T]` as a working name pending
its own design gate;
keyset pagination keeps the separate `CursorPage[T]` name. Neither changes the
meaning or JSON contract of `Page`.

The cursor design is accepted but intentionally not exported yet. Its first
delivery is forward-only (`after` + `per_page`), fetches one extra row, returns
`per_page`/`has_next`/nullable `next_cursor`, and never runs COUNT. It requires
terminal-owned stable ordering with immutable non-null keys and an explicit
unique tie-breaker. Public HTTP cursors require an explicit signing keyring;
signing prevents tampering but does not hide key values.

A cursor never replaces authorization. Each request must re-apply its normal
authentication, tenant, permission, and filter predicates; signed scope binding
only prevents a token from being replayed under a different query.

Implementation waits for a concrete consumer, a fail-loud boundary for Bun
hooks that mutate cursor-owned ordering/window state, and real
PostgreSQL/MySQL/SQLite conformance. Until then, repositories that need keyset
pagination own the query and token codec explicitly. See the
[cursor design gate](../specs/pagination.md#cursorkeyset-design-gate).

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

`store.Register[R]` is the preferred registration API for data stores.

```go
if err := store.Register[*sqldb.DB](app, db); err != nil {
    return err
}
```

It performs these steps:

1. validates the value, canonical health name, lifecycle ownership, and local DI state
2. ensures the Registry/readiness seam and privately reserves the store name,
   value type, and validated resource identity
3. re-checks DI and pings the store
4. publishes a Replace-protected value in DI, then commits the Registry entry
   before releasing the private reservation

Pending reservations are invisible to health consumers. A duplicate name or
type, frozen container, invalid lifecycle combination, or failed Ping therefore
cannot leave a health-only entry behind. The initial DI preflight is
point-in-time: an external concurrent mutation can still make the final
protected publication fail, and that final publication remains authoritative.
`WithPingTimeout` supplies Ping a deadline-scoped context, but Register calls
Ping synchronously; custom Lifecycle implementations must honor `ctx` because a
non-cooperative Ping is not hard-bounded by the framework.

For the usual direct registration, the value itself implements
`store.Lifecycle`. Ownership transfers to the framework only after successful
registration, and DI becomes the sole framework shutdown owner. During one
teardown the container walks reverse registration order and makes at most one
`Shutdown(ctx)` attempt if the still-live deadline reaches this value; it may
make no attempt when the deadline expires first. The Registry never closes
resources. On any registration error, ownership remains with the caller.

Useful options:

```go
store.Register[*sqldb.DB](
    app,
    db,
    store.WithName("primary"),
    store.WithPingTimeout(10*time.Second),
)
```

Use raw `app.Provide`, `app.ProvideFactory`, `app.ProvideValue`,
`app.ProvideProtectedValue`, or `app.Replace` only when you intentionally do
not want store Registry integration and will not register the same lifecycle
through `store.Register`. Mixing either raw publication path with Register for
one resource is unsupported.

Store names use the same rules as named health checks. An explicit empty name,
leading/trailing whitespace, control characters, and the reserved `credo.`
prefix are rejected rather than normalized. If `WithName` is omitted, Credo
unwraps pointer layers and uses the package-qualified name of the named DI type;
unnamed types require an explicit name.

### Resource identity inside the Register ledger

Registry uniqueness includes a resource identity, not only the name and DI
type. The default identity is the top-level `Lifecycle` value itself, so
pointer-backed lifecycle implementations are the recommended shape. A
non-pointer value or explicit token must be non-nil, comparable, reflexively
equal, and stable; non-comparable values and NaN-like tokens fail before Ping.

Credo does not inspect struct fields to guess which client a wrapper delegates
to. A semantic or named-field wrapper that implements the full Lifecycle
contract explicitly forwards identity with the optional extension:

```go
type DelegatingDB struct {
    client *sqldb.DB
}

func (db *DelegatingDB) Ping(ctx context.Context) error {
    return db.client.Ping(ctx)
}
func (db *DelegatingDB) Shutdown(ctx context.Context) error {
    return db.client.Shutdown(ctx)
}
func (db *DelegatingDB) Health(ctx context.Context) store.Health {
    return db.client.Health(ctx)
}
func (db *DelegatingDB) ResourceIdentity() any {
    return db.client // underlying stable pointer
}

var _ store.LifecycleIdentityProvider = (*DelegatingDB)(nil)
```

An embedded `*sqldb.DB` promotes its `ResourceIdentity` method through ordinary
Go method promotion. A pointer to a composite Lifecycle is also a valid
top-level identity; multiple fields are not scanned or treated as ambiguous.
Inside one `store.Register` Registry, equal identity tokens are rejected across
concrete/interface registrations, explicit wrapper types, and mixed ownership.

If another interface should resolve to the same store, alias the existing
concrete registration instead of registering it again:

```go
type StoreHealth interface {
    Health(context.Context) store.Health
}

if err := store.Register[*sqldb.DB](app, db); err != nil {
    return errors.Join(err, db.Shutdown(context.Background()))
}
if err := app.Alias[StoreHealth, *sqldb.DB](); err != nil {
    return err
}
```

`Resolve[StoreHealth]` now returns the already registered `*sqldb.DB`; no
second health entry or shutdown owner is created. Distinct multi-database
wrappers remain valid when they contain distinct physical database clients.

The guarantee stops at the `store.Register` ledger. Registering the same client
again under another T with raw `app.Provide`, `app.ProvideFactory`,
`app.ProvideValue`, `app.ProvideProtectedValue`, or `app.Replace` is unsupported
and may give DI duplicate or contradictory shutdown ownership. Likewise, a
handle declared caller-owned must not also be registered in DI as a
`Shutdowner`. Use `Alias` for interface access. A general cross-infrastructure
resource registry remains deferred until a second concrete subsystem needs it.

Successful store value bindings and the adopted `*store.Registry` binding are
protected from `app.Replace`. This prevents DI from resolving a different
client than the one tracked for readiness and shutdown. Install a substitute
before `Register`, or register the desired fake/store on a fresh App; a later
`app.Replace[R]` is intentionally rejected.

A Registry supplied by the composition root is protected only after Register
successfully resolves and validates a non-nil value. Register passes that
resolved pointer to `app.ProtectBinding[*store.Registry](registry)`, which
atomically compares and protects it against `Replace`, then re-resolves it for
wiring. If a replacement wins before compare-and-protect, adoption fails and
the replacement remains unprotected. A nil value or failing Registry
constructor likewise remains unprotected, so it can be repaired with
`app.Replace[*store.Registry]` before Finalize and before retrying Register.

### Explicit caller-owned lifecycle

A named-field wrapper does not inherit its client's lifecycle methods. If it
cannot implement `store.Lifecycle` itself, registration requires both the
health handle and an explicit ownership opt-out:

```go
type ReportingDB struct {
    client *sqldb.DB
}

reporting := ReportingDB{client: db}
if err := store.Register[ReportingDB](
    app,
    reporting,
    store.WithName("reporting"),
    store.WithLifecycle(db),
    store.WithCallerOwnedLifecycle(),
); err != nil {
    // Registration never took ownership.
    return errors.Join(err, db.Shutdown(context.Background()))
}

// Register intentionally did not transfer shutdown ownership.
app.OnShutdown(db.Shutdown)
```

`WithLifecycle` by itself is an error; warning-only implicit caller ownership
is no longer supported. A value that already implements `Lifecycle` must not
also receive `WithLifecycle` or `WithCallerOwnedLifecycle`. A value that only
implements `credo.Shutdowner` cannot use a different object for Ping/Health;
implement the complete Lifecycle contract on the wrapper instead.

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
        store.WithName("primary"),
    ); err != nil {
        return err
    }

    if err := store.Register[AnalyticsDB](
        app,
        AnalyticsDB{analytics},
        store.WithName("analytics"),
    ); err != nil {
        return err
    }

    return nil
}
```

The embedded wrappers inherit `*sqldb.DB`'s complete `store.Lifecycle` and its
`ResourceIdentity` method, so each wrapper identifies its distinct underlying
DB and is the framework-owned lifecycle value. Passing
`WithLifecycle(primary)` or `WithLifecycle(analytics)` here is now rejected:
an explicit second handle would make ownership ambiguous. For a named-field
wrapper, either delegate the complete Lifecycle contract or use the explicit
caller-owned pair shown above.

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

`store/sqldb` wraps Bun's migration engine (`bun/migrate` — part of the already-pinned Bun module, not a new dependency). Register the set at wiring time, then opt in to auto-run on application start:

```go
import "github.com/uptrace/bun/migrate"

//go:embed migrations/*.sql
var sqlMigrations embed.FS

func main() {
    app, _ := credo.New(...)
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
- [Pagination Spec](../specs/pagination.md)
- [ADR-015](../adr/015-data-access.md)
