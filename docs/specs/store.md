# Store Spec

**Status**: Implemented
**Package**: `store/` (core contracts), `store/sqldb/` (Bun wrapper, separate submodule)
**Sources**: GoFr (Apache-2.0, health/interface design), Goyave (MIT, connection patterns)
**ADRs**: [015-data-access.md](../adr/015-data-access.md)

---

## Canonical Source

Implementation-level details for Credo's data access layer are defined
in this file. Other documents should keep only high-level references and
link here.

---

## Overview

Credo's data access layer is split into two packages:

- **`store/`** — universal contracts with zero external dependencies. Part
  of the main `github.com/credo-go/credo` module. Defines error sentinels,
  lifecycle/health interfaces, a connection registry, registration API,
  and context-based TX helpers.
- **`store/sqldb/`** — Bun SQL wrapper in a separate Go submodule
  (`github.com/credo-go/credo/store/sqldb`). Wraps `*bun.DB` with lifecycle
  management, query builder proxies, error mapping, and transaction
  management.

Key design properties:

- **Universal errors** — `ErrNotFound`, `ErrDuplicate`, `ErrConflict`,
  `ErrTimeout`, `ErrReadOnly` implement `HTTPStatus() int`; the framework's
  internal error handler detects this interface without importing `store/`.
- **Context-based TX** — opt-in convenience via `Conn[T]` for simple cases
  and `ConnInScope[T]` for same-type multi-connection cases. Native ORM TX
  API also works.
- **3-tier wrapping** — own (lifecycle), enrich (query proxies),
  passthrough (`Client()` escape hatch).
- **Single ORM focus** — deep Bun integration. Other ORMs via raw DI.

---

## Goals

1. **Universal error types**: Errors in `store/` implement `HTTPStatus()
   int`. The framework's internal error handler detects this interface via
   `errors.As` without importing `store/`, avoiding circular dependencies.
   Adapters translate ORM/driver errors to these sentinels.
2. **Context-based TX**: `db.InTx` / `RunInTx` + `Conn[T]` / `ConnInScope[T]`
   for opt-in transaction participation. Repositories that don't need TX are
   unaffected.
3. **3-tier Bun wrapping**: Own lifecycle, enrich queries (TX injection,
   error mapping, tracing hooks), passthrough via `Client()`.
4. **Clean module boundary**: `store/` is stdlib-only in the main module.
   `store/sqldb/` is a separate submodule with the Bun dependency.
5. **Unified registration**: `store.Register[R]` handles ping, DI
   registration, and lifecycle tracking in one call.
6. **Lifecycle management**: startup ping and health aggregation in the
   Registry; closing owned solely by the DI container (registered values
   implementing `Shutdowner` are closed in reverse registration order).
7. **Escape hatch**: `Client() *bun.DB` for raw SQL, model registration,
   advanced migration operations, and any Bun feature not covered by proxies.
8. **Migrations without a second engine**: a thin wrapper over `bun/migrate`
   (`RegisterMigrations` + `Migrate`) whose signature plugs straight into
   `app.OnStart` — no goose, no new dependency.

---

## Core Package: store/

### Universal Errors

Store errors implement `error` and `HTTPStatus() int`. The framework's internal
error handler detects this via type-safe error-chain matching
(`errors.AsType`) — no import of `store/` needed, avoiding circular
dependencies (`store → credo` for DI registration is fine; `credo → store`
would be a cycle).

```go
package store

// statusError carries both the error message and an HTTP status code.
type statusError struct {
    msg    string
    status int
}

func (e *statusError) Error() string   { return e.msg }
func (e *statusError) HTTPStatus() int { return e.status }

var (
    ErrNotFound  error = &statusError{"store: record not found", 404}
    ErrDuplicate error = &statusError{"store: duplicate record", 409}
    ErrConflict  error = &statusError{"store: conflict", 409}
    ErrTimeout   error = &statusError{"store: timeout", 504}
    ErrReadOnly  error = &statusError{"store: read-only", 503}
)
```

**Error handler detection** (in the framework's internal error handler, no store import):

```go
type httpStatusProvider interface {
    error
    HTTPStatus() int
}
if se, ok := errors.AsType[httpStatusProvider](err); ok {
    status = se.HTTPStatus()
}
```

> `errors.Is` still works: `errors.Is(err, store.ErrNotFound)` matches
> pointer identity. `errors.As` unwraps `fmt.Errorf("%w", err)` chains.

**HTTP mapping:**

| Error | HTTP Status | RFC 7807 Type |
|-------|-------------|---------------|
| `ErrNotFound` | 404 Not Found | `not-found` |
| `ErrDuplicate` | 409 Conflict | `duplicate` |
| `ErrConflict` | 409 Conflict | `conflict` |
| `ErrTimeout` | 504 Gateway Timeout | `timeout` |
| `ErrReadOnly` | 503 Service Unavailable | `read-only` |

### Lifecycle Interface

```go
// Lifecycle manages connection health and shutdown.
// Adapters implement this interface for use with Register[R].
type Lifecycle interface {
    Ping(ctx context.Context) error
    Shutdown(ctx context.Context) error
    Health(ctx context.Context) Health
}
```

### Health

```go
// HealthStatus represents the health state of a connection.
type HealthStatus string

const (
    StatusUp       HealthStatus = "UP"
    StatusDown     HealthStatus = "DOWN"
    StatusDegraded HealthStatus = "DEGRADED"
)

// Health is the result of a connection health check.
type Health struct {
    Status  HealthStatus
    Latency time.Duration
    Details map[string]any // adapter-specific details (version, pool stats)
}
```

### Registry

The `Registry` tracks data store connections for startup ping and health
aggregation. Created automatically on the first `Register` call and
stored in the DI container.

Behavior:
- `Add(name, lifecycle)` — append an entry; reject duplicate names
- `HealthAll(ctx)` — return health status for all entries, keyed by name

The Registry does not close connections. Shutdown ownership lies with the
DI container alone: registered values implementing `credo.Shutdowner` are
closed during app shutdown, in reverse registration order.

```go
// Registry tracks store connections for startup ping and health
// aggregation. It does not close connections — shutdown is owned
// by the DI container.
type Registry struct { /* unexported */ }

func (r *Registry) Add(name string, lc Lifecycle) error
func (r *Registry) HealthAll(ctx context.Context) map[string]Health
```

### Registration API

```go
const DefaultPingTimeout = 5 * time.Second

// Register registers value as type R in DI, pings the connection,
// and tracks it in the Registry for lifecycle and health management.
//
// If value implements Lifecycle, it is used directly for ping/shutdown/health.
// Otherwise, provide the Lifecycle handle via WithLifecycle.
func Register[R any](app *credo.App, value R, opts ...RegisterOption) error
```

Steps:
1. **Resolve Lifecycle** — use `value` if it implements `Lifecycle`,
   otherwise use the handle from `WithLifecycle` (error if neither)
2. **Ping** — verify connection is alive (fail-fast at startup)
3. **Ensure Registry** — resolve or create `Registry` in DI
4. **Track** — add `Lifecycle` handle to Registry for ping/health aggregation
5. **DI register** — register `value` as type `R` via `credo.ProvideValue[R]`

Shutdown ownership: closing is the DI container's job alone. A value that
implements `credo.Shutdowner` (every `Lifecycle` does) is closed by the
container during app shutdown, in reverse registration order. The Registry
never closes connections. When `value` does not implement `Shutdowner` —
possible only with `WithLifecycle` — `Register` logs a warning and closing
stays with the caller (e.g. via `app.OnShutdown`).

On failure, framework-owned state is rolled back. Caller-owned lifecycle
values are not shut down automatically.

```go
type RegisterOption func(*registerOptions)

// WithName sets the display name for health reporting.
// Default: derived from type parameter R via reflection.
func WithName(name string) RegisterOption

// WithPingTimeout overrides the default ping timeout (5s).
func WithPingTimeout(d time.Duration) RegisterOption

// WithLifecycle provides the Lifecycle handle when value R does not
// implement Lifecycle itself (e.g., a wrapper that keeps the connection
// in a named field — embedding wrappers inherit Lifecycle and need
// no explicit handle).
func WithLifecycle(lc Lifecycle) RegisterOption
```

> **Health/readiness options** (`WithCritical`, `WithTags`) are deferred
> to [ADR-016](../adr/016-health-checks.md). They will be added to
> `RegisterOption` when the health package design is finalized.

### TX Context

```go
// WithTx stores a transaction handle in the context.
// T is the transaction type (e.g., bun.Tx).
func WithTx[T any](ctx context.Context, tx T) context.Context

// GetTx retrieves a transaction handle from the context.
// Returns the zero value and false if no TX is stored.
func GetTx[T any](ctx context.Context) (T, bool)

// Conn returns the transaction from context if present,
// otherwise returns the fallback connection.
// Repositories call this in every method for opt-in TX participation.
func Conn[T any](ctx context.Context, fallback T) T

// NewTxScope creates a unique logical transaction scope.
func NewTxScope() *TxScope

// WithTxInScope stores a transaction handle in the context for a scope.
func WithTxInScope[T any](ctx context.Context, scope *TxScope, tx T) context.Context

// GetTxInScope retrieves a transaction handle for the scope.
func GetTxInScope[T any](ctx context.Context, scope *TxScope) (T, bool)

// ConnInScope returns the scoped transaction from context if present,
// otherwise returns the fallback connection.
func ConnInScope[T any](ctx context.Context, scope *TxScope, fallback T) T
```

`WithTx` / `GetTx` / `Conn` remain useful for simple type-keyed flows.
Adapters such as `store/sqldb` should prefer scoped helpers so that two
connections using the same Go type do not collide in `context.Context`.

---

## Bun Wrapper: store/sqldb/

### DB Type

```go
package sqldb

import "github.com/uptrace/bun"

// DB wraps *bun.DB with lifecycle management, query builder proxies,
// error mapping, and transaction support.
type DB struct { /* unexported: db *bun.DB */ }

// Open creates a DB from Config.
func Open(cfg *Config, opts ...Option) (*DB, error)

// Client returns the underlying *bun.DB for raw SQL, migrations,
// model registration, and features not covered by proxies.
func (db *DB) Client() *bun.DB

// Lifecycle methods — satisfies store.Lifecycle.
func (db *DB) Ping(ctx context.Context) error
func (db *DB) Shutdown(ctx context.Context) error
func (db *DB) Health(ctx context.Context) store.Health
```

### Config

```go
// Config holds connection parameters for SQL databases.
type Config struct {
    Driver         string        // "postgres", "mysql", "sqlite"
    Host           string
    Port           int
    Name           string        // database name
    User           string
    Password       string
    DSN            string        // override: raw DSN string (if set, Host/Port/Name ignored)
    ConnectTimeout time.Duration // connection establishment timeout
    MaxOpen        int           // max open connections (0 = unlimited)
    MaxIdle        int           // max idle connections
    MaxLifetime    time.Duration // max connection lifetime
    SSLMode        string        // "disable", "require", "verify-full"
    Options        map[string]string // driver-specific connection params
}
```

### Query Builders

Four query builder proxy types wrap Bun's query builders:

```go
func (db *DB) Select(model ...any) *SelectQuery
func (db *DB) Insert(model ...any) *InsertQuery
func (db *DB) Update(model ...any) *UpdateQuery
func (db *DB) Delete(model ...any) *DeleteQuery
```

Each proxy type:
- Proxies a curated subset of builder methods for clean chaining.
  Methods not in the proxy set are available via `Apply`.
- Injects TX from context via `store.Conn` on terminal operations
- Maps errors to `store.Err*` sentinels after execution

**Visibility policy: Credo does not hide Bun — it integrates it.** The
proxy exists to attach the two terminal guarantees above, not to abstract
Bun away; bun types appear in proxy signatures by design. A missing
builder method is reached via `Apply`/`ApplyQueryBuilder` (guarantees
preserved); a missing *terminal* method is a request to extend the curated
set — the guarantees live in the terminals, so terminals must be on the
proxy. `Unwrap` and `Client()` are deliberate opt-outs from both
guarantees.

**SelectQuery proxy methods** (~20):
`Model`, `Column`, `ColumnExpr`, `ExcludeColumn`, `TableExpr`,
`Join`, `JoinOn`, `JoinOnOr`,
`Where`, `WhereOr`, `WherePK`, `OrderExpr`,
`Limit`, `Offset`, `Relation`, `Distinct`, `GroupExpr`, `Having`,
`Clone`, `Conn`.

**Terminal methods** (Scan, Exec, Count, Exists) execute the query and
return mapped errors. Driver errors are translated to `store.Err*`
sentinels before returning, so callers can branch with `errors.Is`
without importing `database/sql` or driver-specific packages:

| Driver error                | Mapped sentinel       |
|-----------------------------|-----------------------|
| `sql.ErrNoRows`             | `store.ErrNotFound`   |
| Unique violation            | `store.ErrDuplicate`  |
| Foreign-key violation       | `store.ErrConflict`   |
| Read-only / replica         | `store.ErrReadOnly`   |
| `context.DeadlineExceeded`  | `store.ErrTimeout`    |

`Update.Exec` and `Delete.Exec` do **not** convert "no rows affected"
into `ErrNotFound` — callers must inspect `sql.Result` for that.

**Escape hatches** on each query type:

```go
// Apply delegates to Bun's native Apply for advanced builder methods.
// Nil functions are filtered out. Typed per query type.
func (q *SelectQuery) Apply(fns ...func(*bun.SelectQuery) *bun.SelectQuery) *SelectQuery

// ApplyQueryBuilder applies fn to Bun's shared bun.QueryBuilder (the
// builder-only Where/WhereOr/WhereGroup/WherePK/WhereDeleted interface
// common to select/update/delete), so a single WHERE predicate can be
// reused across all three query types. Interceptors preserved; nil is a
// no-op. Available on SelectQuery, UpdateQuery, DeleteQuery.
func (q *SelectQuery) ApplyQueryBuilder(fn func(bun.QueryBuilder) bun.QueryBuilder) *SelectQuery

// Unwrap returns the underlying *bun.SelectQuery for builder-only use.
// Terminal methods on the unwrapped query bypass Credo interceptors.
func (q *SelectQuery) Unwrap() *bun.SelectQuery
```

### 6 Guardrails

1. **TX inject: clone, don't mutate** — terminal methods copy the
   underlying query before applying the TX connection. `SelectQuery`
   uses Bun's `Clone()` (deep copy). `InsertQuery`, `UpdateQuery`, and
   `DeleteQuery` use a Go struct shallow copy (`copied := *q.raw`)
   since Bun does not provide `Clone()` on these types — this isolates
   the `conn` field without affecting shared slices; it suffices because
   bun reads, never mutates, the builder while generating SQL. The
   original `q.raw` is never modified, so query builders can be reused
   safely — including executing the same builder inside a transaction
   and again after that transaction finished (pinned by the
   `Test*Exec_BuilderReusableAfterTxRollback` tests in
   `query_copy_test.go`). The wrapper tracks a `connSet` bool; if the
   user explicitly called `.Conn()` on the wrapper, context TX does not
   override it.

2. **Apply: Bun-native ergonomics** — `Apply(fns ...func(*bun.XQuery)
   *bun.XQuery)` matches Bun's own varargs signature. Delegates directly
   via `q.raw.Apply(fns...)`. Nil functions are filtered out to prevent
   panics.

3. **Unwrap: builder-only escape** — `Unwrap()` returns the underlying
   `bun.*Query` for use as a subquery or parameter to other Bun methods.
   Calling terminal methods on the unwrapped query bypasses all Credo
   interceptors (TX injection, error mapping). Documented as advanced-only.
   `Apply` is the primary escape hatch (interceptors preserved).

4. **Trace: DB-level QueryHook** — observability is implemented via
   `bun.QueryHook` attached at `Open()` time, not in terminal wrapper
   methods. This covers 100% of queries including `Client()` escape
   hatch, raw SQL, migrations, and create table operations.

5. **Raw terminal wrappers** — `DB` exposes `Exec`, `QueryRow`, and
   `Query` methods for raw SQL that go through the same TX inject and
   error mapping pipeline as query builder terminals. Without these,
   error mapping behavior would split between wrapped and raw queries.

```go
// Raw SQL with TX injection and error mapping.
func (db *DB) Exec(ctx context.Context, query string, args ...any) (sql.Result, error)
func (db *DB) QueryRow(ctx context.Context, dest any, query string, args ...any) error
func (db *DB) Query(ctx context.Context, dest any, query string, args ...any) error
```

6. **ApplyQueryBuilder: cross-type filter reuse** — `Apply` is typed per
   query type, so a WHERE predicate shared across read and write had to be
   duplicated three times. `ApplyQueryBuilder(fn func(bun.QueryBuilder)
   bun.QueryBuilder)` on `SelectQuery`/`UpdateQuery`/`DeleteQuery` surfaces
   Bun's shared `bun.QueryBuilder` so one predicate (tenant scope,
   soft-delete filter, ownership check) applies to all three. It is
   implemented as `q.raw = fn(q.raw.QueryBuilder()).Unwrap().(*bun.XQuery)`,
   mirroring Bun's own `ApplyQueryBuilder`. Conditions land on the proxied
   query, so terminal methods still apply TX injection and error mapping —
   interceptors are preserved, exactly like `Apply`. A nil fn is a no-op.
   The form is preferred over a raw `QueryBuilder()` accessor: it stays in
   the proxy fluent chain and contains the bun type inside a function
   boundary, whereas `QueryBuilder()` would break the chain and act as a
   second `Unwrap` (the interface carries `Unwrap() any`). `InsertQuery` is
   excluded — no WHERE clause.

### TX Management

```go
// RunInTx starts a transaction, stores it in context via store.WithTx,
// executes fn, and commits on nil / rolls back on error.
// Nested calls use savepoints.
func RunInTx(ctx context.Context, db *DB, fn func(ctx context.Context) error) error

// RunInTxWith is like RunInTx but accepts sql.TxOptions.
func RunInTxWith(ctx context.Context, db *DB, opts *sql.TxOptions, fn func(ctx context.Context) error) error

// InTx / InTxWith are the method forms of RunInTx / RunInTxWith —
// handler-side ergonomics: db.InTx(ctx.Context(), fn).
func (db *DB) InTx(ctx context.Context, fn func(ctx context.Context) error) error
func (db *DB) InTxWith(ctx context.Context, opts *sql.TxOptions, fn func(ctx context.Context) error) error
```

**Semantics:**

| Callback return | Action |
|-----------------|--------|
| `nil` | Commit |
| `error` | Rollback, return error |
| `panic` | Rollback, re-panic |

**Nested TX:** When `RunInTx` is called within an existing TX context,
Bun creates a `SAVEPOINT` automatically.

### Migrations (bun/migrate wrapper)

```go
// RegisterMigrations stores the migration set that Migrate runs.
// opts pass through to migrate.NewMigrator (table names, hooks, ...).
// Panics if m is nil or if already registered (wiring-time misuse).
func (db *DB) RegisterMigrations(m *migrate.Migrations, opts ...migrate.MigratorOption)

// Migrate runs pending migrations: Init (bookkeeping tables, IF NOT
// EXISTS) → Lock (table-based advisory lock, fail-fast) → Migrate →
// Unlock. Signature matches App.OnStart, so auto-run on start is:
//
//	db.RegisterMigrations(migrations)
//	app.OnStart(db.Migrate)
func (db *DB) Migrate(ctx context.Context) error
```

**Design points:**

- **Thin wrapper**: the `*migrate.Migrations` set is Bun's own type — populated via
  `Discover(fsys)` for SQL files (works with `embed.FS`) or
  `MustRegister` for Go migrations. Credo does not re-wrap it.
- **Mark-applied-on-success by default**: the wrapper passes
  `migrate.WithMarkAppliedOnSuccess(true)`. Bun's bare default records a
  migration *before* running it, so a failed migration would be skipped
  as "applied" on the next start — wrong for unattended `OnStart`
  auto-run. With the wrapper default, a failed migration is retried on
  the next run. Users can pass `WithMarkAppliedOnSuccess(false)` through
  `RegisterMigrations` to restore Bun's behavior.
- **Lock semantics**: if another instance holds the lock (second replica
  starting concurrently), `Migrate` fails immediately rather than
  waiting; the failed instance can be restarted. Unlock runs under
  `context.WithoutCancel` so a cancelled ctx cannot leak the lock row;
  an unlock failure is joined into the returned error.
- **Seeding** is a plain migration file (e.g. `2_seed_plans.up.sql`) —
  no separate mechanism.
- **No CLI here**: `credo migrate:*` (Phase 5.1) is optional sugar over
  this wrapper. Rollback / status / file generation stay on Bun's
  migrator via the escape hatch: `migrate.NewMigrator(db.Client(), ms)`.
- **OnStart integration is signature compatibility**, not coupling:
  `sqldb` still imports only `credo/store`, never the root framework
  package.

### Error Mapping

| Bun/Driver Error | store.Err* |
|------------------|------------|
| `sql.ErrNoRows` | `store.ErrNotFound` |
| Unique constraint violation (PG: 23505, MySQL: 1062) | `store.ErrDuplicate` |
| Foreign key violation (PG: 23503) | `store.ErrConflict` |
| Context deadline exceeded | `store.ErrTimeout` |
| Read-only transaction error | `store.ErrReadOnly` |

Unmapped errors pass through unwrapped. Mapped errors preserve the original
driver/ORM cause while still matching `store.Err*` via `errors.Is` and still
reporting the sentinel's `HTTPStatus()`. Mapping remains best-effort:
driver-specific codes are detected via string/code matching without
importing driver packages directly.

### Client Escape Hatch

```go
// Client returns the underlying *bun.DB.
// Use for raw SQL, model registration, advanced migration operations,
// and any Bun feature not covered by the proxy layer.
db.Client() *bun.DB
```

**Warning**: queries executed via the returned `*bun.DB` bypass the proxy
interceptors. There is no automatic TX injection from context (so
`InTx` / `RunInTx` will not affect them) and no error mapping to
`store.Err*` sentinels. Reserve `Client()` for model registration, raw
SQL the proxy layer cannot express, and migration operations beyond
`Migrate` (rollback, status, file generation); use the proxy layer for
normal repository code.

---

## File Layout

```text
store/
├── doc.go              ← package documentation
├── errors.go           ← ErrNotFound, ErrDuplicate, ErrConflict, ErrTimeout, ErrReadOnly
├── lifecycle.go        ← Lifecycle interface
├── health.go           ← Health, HealthStatus
├── registry.go         ← Registry (Add, HealthAll — no shutdown; DI owns closing)
├── register.go         ← Register[R], RegisterOption, WithName, WithPingTimeout
├── tx.go               ← WithTx[T], GetTx[T], Conn[T], NewTxScope, WithTxInScope, GetTxInScope, ConnInScope
├── errors_test.go
├── registry_test.go
├── register_test.go
└── tx_test.go

# Separate Go submodule: github.com/credo-go/credo/store/sqldb
store/sqldb/
├── go.mod              ← depends on github.com/uptrace/bun + github.com/credo-go/credo
├── doc.go
├── db.go               ← DB type, Open, Client, Lifecycle methods
├── config.go           ← Config struct, DSN builders
├── driver.go           ← driver family detection (postgres/mysql/sqlite)
├── query_common.go     ← shared query state (TX binding, error mapping)
├── query_select.go     ← SelectQuery proxy
├── query_insert.go     ← InsertQuery proxy
├── query_update.go     ← UpdateQuery proxy
├── query_delete.go     ← DeleteQuery proxy
├── paginate.go         ← Paginate, PaginateRequest (COUNT + page SELECT)
├── tx.go               ← RunInTx, RunInTxWith + method forms InTx, InTxWith
├── migrate.go          ← RegisterMigrations, Migrate (bun/migrate wrapper)
├── errors.go           ← mapError (Bun/driver → store.Err*)
├── options.go          ← Option, WithDialect, WithConnector
├── raw.go              ← Exec, QueryRow, Query (raw SQL with TX inject + error map)
├── config_test.go
├── db_test.go
├── errors_test.go
├── integration_test.go  ← TX, query proxy, nested savepoint, raw SQL, pagination tests
├── migrate_test.go      ← migration wrapper tests (incl. embed.FS discovery)
└── testdata/migrations/ ← SQL migration fixtures for embed.FS tests
```

---

## Examples

### Basic Repository

```go
import "github.com/credo-go/credo/store/sqldb"

type UserRepo struct {
    db *sqldb.DB
}

func NewUserRepo(db *sqldb.DB) *UserRepo {
    return &UserRepo{db: db}
}

func (r *UserRepo) GetByID(ctx context.Context, id uuid.UUID) (*User, error) {
    var user User
    err := r.db.Select(&user).Where("id = ?", id).Scan(ctx)
    if err != nil {
        return nil, fmt.Errorf("user get by id: %w", err)
        // err is already mapped: sql.ErrNoRows → store.ErrNotFound
    }
    return &user, nil
}

func (r *UserRepo) Create(ctx context.Context, user *User) error {
    _, err := r.db.Insert(user).Exec(ctx)
    return err // unique violation → store.ErrDuplicate
}
```

### Service with TX

```go
import "github.com/credo-go/credo/store/sqldb"

type OrderService struct {
    infra     credo.Infra
    db        *sqldb.DB
    orderRepo *OrderRepo
    stockRepo *StockRepo
}

func (s *OrderService) PlaceOrder(ctx context.Context, input OrderInput) (*Order, error) {
    var order *Order
    err := sqldb.RunInTx(ctx, s.db, func(ctx context.Context) error {
        // TX is in context — repos pick it up via store.Conn
        if err := s.stockRepo.Decrement(ctx, input.ProductID, input.Qty); err != nil {
            return err // rollback
        }
        var err error
        order, err = s.orderRepo.Create(ctx, input)
        return err // nil = commit, error = rollback
    })
    return order, err
}
```

### Registration

```go
import "github.com/credo-go/credo/store/sqldb"

func SetupStore(app *credo.App, rc credo.RawConfig) {
    var cfg sqldb.Config
    rc.Unmarshal("databases.default", &cfg)

    db, err := sqldb.Open(&cfg)
    if err != nil {
        log.Fatal(err)
    }

    // Single DB — *sqldb.DB implements Lifecycle, used directly.
    if err := store.Register[*sqldb.DB](app, db); err != nil {
        log.Fatal(err)
    }
}
```

### Multi-Database

```go
import "github.com/credo-go/credo/store/sqldb"

// Define wrapper types for compile-time safety.
type PrimaryDB struct{ *sqldb.DB }
type AnalyticsDB struct{ *sqldb.DB }

func SetupMultiDB(app *credo.App, rc credo.RawConfig) {
    var primaryCfg, analyticsCfg sqldb.Config
    rc.Unmarshal("databases.primary", &primaryCfg)
    rc.Unmarshal("databases.analytics", &analyticsCfg)

    primaryDB, _ := sqldb.Open(&primaryCfg)
    analyticsDB, _ := sqldb.Open(&analyticsCfg)

    // Embedding promotes *sqldb.DB's methods, so PrimaryDB already
    // implements Lifecycle (and Shutdowner — the DI container closes
    // it); WithLifecycle is shown for explicitness.
    store.Register[PrimaryDB](app, PrimaryDB{primaryDB},
        store.WithLifecycle(primaryDB), store.WithName("primary"))
    store.Register[AnalyticsDB](app, AnalyticsDB{analyticsDB},
        store.WithLifecycle(analyticsDB), store.WithName("analytics"))
}
```

---

## Design Decisions

1. **Single ORM (Bun) over multi-ORM adapters** — deep integration
   (query proxies, error mapping, pagination) is more valuable than
   shallow generic interfaces. Escape hatch for other ORMs via raw DI.

2. **Error interface over import-based detection** — store errors
   implement `HTTPStatus() int`. The framework's internal error handler
   detects this via `errors.As` without importing `store/`, avoiding
   circular dependencies (`store → credo` is fine; `credo → store` would
   be a cycle).

3. **Context-based TX over explicit TX passing** — `Conn[T]` is opt-in
   and keeps repository method signatures clean. Trade-off: TX
   participation is less visible in function signatures. See
   [ADR-015](../adr/015-data-access.md) for rationale.

4. **Query builder proxies over raw pass-through** — proxies inject TX,
   map errors, and add tracing. `Client()` escape hatch prevents the
   proxy from becoming a bottleneck.

5. **Separate submodule for store/sqldb/** — Bun dependency is opt-in.
   Applications not using SQL don't pull in Bun.

6. **Config over DSN-only** — structured `Config` enables validation,
   env var mapping, and consistent documentation. `DSN` field is an
   override for advanced use cases.

7. **Fail-fast at startup** — `Register[R]` pings the connection
   immediately. Misconfigured databases surface as startup errors.

8. **Registry with LIFO shutdown** — connections are closed in reverse
   registration order. Dependent connections shut down before their
   dependencies.

9. **Health returns struct, not error** — structured data (status,
   latency, pool stats) for dashboards and readiness probes.

10. **Wrapper types for multi-DB** — applications define distinct struct
    types (`PrimaryDB`, `AnalyticsDB`). Compile-time DI safety with zero
    string keys.

11. **Best-effort error mapping** — driver-specific error codes (PG 23505,
    MySQL 1062) are detected via string/code matching without importing
    driver packages. Unmapped errors pass through.

---

## Test Requirements

### store/ (core)

- `ErrNotFound` etc. work with `errors.Is` and `errors.As` (HTTPStatus)
- Wrapped errors (`fmt.Errorf("%w", store.ErrNotFound)`) preserve HTTPStatus
- `WithTx` / `GetTx` round-trip
- `WithTxInScope` / `GetTxInScope` isolate same-type transactions by scope
- `Conn` returns TX from context when present, fallback otherwise
- `ConnInScope` returns scoped TX from context when present, fallback otherwise
- `Registry.Add` appends entries, rejects duplicate names
- `Registry.Shutdown` closes in LIFO order
- `Registry.Shutdown` respects `ctx.Err()`, returns aggregated errors
- `Registry.HealthAll` returns health for all entries
- `Register` pings, registers in DI, tracks in Registry
- `Register` returns error on ping failure without shutting down caller-owned lifecycle
- `Register` returns error on duplicate DI type
- `WithName` sets custom name for health reporting
- `WithPingTimeout` overrides default timeout

### store/sqldb/ (Bun wrapper)

- `Open` creates a working connection
- `Open` returns error on invalid Config
- `Client()` returns `*bun.DB`
- `Ping` verifies connection
- `Health` returns UP with latency and pool stats
- `Health` returns DOWN when connection is dead
- `Shutdown` closes connection
- Query proxies (`Select`, `Insert`, `Update`, `Delete`):
  - Execute queries correctly
  - Inject TX from context
  - Map errors to `store.Err*` sentinels
  - `ApplyQueryBuilder` reuses one predicate across Select/Update/Delete,
    preserves error mapping, treats a nil fn as a no-op, and reaches
    `WhereGroup` (not in the curated set)
- `RunInTx` commits on nil return
- `RunInTxWith` commits on nil return
- `RunInTx` rolls back on error return
- `RunInTx` nested calls use savepoints
- `RunInTx` stores TX in context with per-DB scoping
- `InTx` / `InTxWith` method forms commit on nil, roll back on error
- `Migrate` applies pending migrations; re-run is a no-op
- `Migrate` retries a failed migration on the next run (mark-on-success)
  and releases the advisory lock after failure
- `Migrate` discovers SQL migrations from `embed.FS` (incl. a seed file)
- `Migrate` without registration returns an error
- `RegisterMigrations` panics on nil set or double registration
- `Migrate` satisfies the `App.OnStart` hook signature (compile-time check)
- Error mapping covers all documented driver errors
- `Config.DSN` override takes precedence
- `Paginate` validates query, destination, page, and per-page inputs
- `PaginateRequest` builds a `pagination.Page` from a normalized
  `PageRequest`; rejects nil requests; empty result keeps a non-nil slice
