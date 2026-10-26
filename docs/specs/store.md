# Store Spec

**Status:** Implemented (beta) **Package:** `store/`, `store/sqldb/` **ADR:** [ADR-015](../adr/015-data-access.md), [ADR-016](../adr/016-health-checks.md), [ADR-024](../adr/024-lifecycle-components.md) **Guide:** [Data access guide](../guides/data-access.md) **Sources:** GoFr (Apache-2.0), Goyave (MIT)

This spec owns the data access contracts: semantic errors, the store registration, transactions, and the `sqldb` Bun wrapper with its query proxies, typed terminals, migrations, and error mapping. Offset pagination — `Page[T]`'s request validation, total semantics, and snapshot consistency — and the cursor/keyset design gate live in the [pagination spec](pagination.md); the general resource-identity rule lives in the [container spec](container.md#resource-identity-and-teardown). The rationale and the rejected alternatives are in [ADR-015](../adr/015-data-access.md).

## Overview

- **`store/`** — universal contracts with no external dependencies, part of the main `github.com/credo-go/credo` module: [semantic errors](#semantic-errors) over the stdlib-only `fault` leaf package, the [`Lifecycle` interface](#lifecycle-interface) and [`Health`](#health), the [store registration](#registration), and the typed [TX scope](#tx-context). Applications on another ORM register their client directly in DI and still use these contracts.
- **`store/sqldb/`** — the Bun wrapper, a separate Go submodule (`github.com/credo-go/credo/store/sqldb`) so the Bun dependency is opt-in: the [`DB` type](#db-type) that owns the pool, [query proxies](#query-builders) that inject this DB's ambient transaction and map driver errors, [transactions](#tx-management), the [transaction-aware native connection](#transaction-aware-bun-connection), [migrations](#migrations-bunmigrate-wrapper), and [error mapping](#error-mapping).
- **Escape hatches** — `Apply` and `ApplyQueryBuilder` keep both proxy guarantees; `DB.Conn(ctx)` joins the ambient transaction without error mapping; `Unwrap` and `Client()` opt out of both.

## Core Package: store/

### Semantic Errors

The stdlib-only `fault` package owns transport-neutral kinds and the provider contract. `store.Kind` aliases `fault.Kind`; root HTTP policy imports only the leaf package, so the `store → credo` registration dependency does not form a cycle.

```go
type Error struct {
    Kind       Kind
    Op         string
    Resource   string
    Constraint string
    Code       string
    Transient  bool
    Cause      error
}

// KindOf finds the first store Kind in err's unwrap/join tree.
func KindOf(err error) (Kind, bool)
func IsTransient(err error) bool

// Wrap classifies cause with a store sentinel's kind as a *Error whose
// Transient flag is the kind's default. A nil cause returns nil; a nil or
// non-store kind, or a cause that already matches it, returns cause unchanged.
func Wrap(kind error, cause error) error
```

`Error.Unwrap` preserves the original cause. `Code`, `Constraint`, `Resource`, and `Cause` are diagnostic metadata and are never emitted by Credo's default error renderer. `Transient` says a condition may clear; it is not a promise that replaying a statement, transaction callback, or externally visible operation is safe, and Credo never treats it as blind-retry permission. Outcome ambiguity is not a cause kind: a commit failure may leave the outcome unknown, Credo does not infer that from a connection or transient flag, and an operation-phase outcome model is deferred until reliable evidence is available.

| Exact sentinel | Kind | Transient default | Root HTTP default |
| --- | --- | --: | --: |
| `ErrNotFound` | `KindNotFound` | no | 404 |
| `ErrAlreadyExists` | `KindAlreadyExists` | no | 409 |
| `ErrConstraint` | `KindConstraint` | no | 409 |
| `ErrSerialization` | `KindSerialization` | yes | 409 |
| `ErrDeadlock` | `KindDeadlock` | yes | 409 |
| `ErrContention` | `KindContention` | yes | 409 |
| `ErrTimeout` | `KindTimeout` | yes | 504 |
| `ErrUnavailable` | `KindUnavailable` | yes | 503 |
| `ErrReadOnly` | `KindReadOnly` | no | 503 |

`ErrDuplicate` is a deprecated alias of `ErrAlreadyExists`. `ErrConflict` is a deprecated umbrella: `ErrConstraint`, `ErrSerialization`, `ErrDeadlock`, and `ErrContention` match it through `errors.Is` without losing their exact kind. The store errors' `HTTPStatus()` is a deprecated compatibility bridge; root classifies `fault.Provider` first. An outer `*credo.HTTPError` is the service/domain override and preserves the internal store cause.

### Lifecycle Interface

```go
// Lifecycle is the contract a data store binding meets for Register[R]: the
// start phase pings it, /ready reports its health, and the App shuts it down
// after its consumers, as it does every component.
type Lifecycle interface {
    Ping(ctx context.Context) error
    Shutdown(ctx context.Context) error
    Health(ctx context.Context) Health
}
```

`Lifecycle` includes `Shutdown`, so every store binding is a [component](lifecycle.md#components-and-the-start-phase). An implementation that represents a resource another value also holds — a wrapper over a `*sqldb.DB` — shares its teardown through the root's identity capability `credo.ResourceIdentifier` (`ResourceIdentity() any`); see [Resource identity](#resource-identity). A value that cannot implement `Lifecycle` is registered through a wrapper type that does: the wrapper is the binding, and its `Ping`, `Health` and `Shutdown` act on one value.

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
    Cause   error          `json:"-"` // typed internal diagnostic cause
    Details map[string]any // adapter-specific details (version, pool stats)
}
```

Status, latency, details, and the typed cause travel as one snapshot, so a caller never parses free-form `Details` for the reason: `Details["error"]` values are adapter metadata, not a trusted error contract. `Health.Clone` preserves the `Cause` identity and copies the top-level `Details` map (nested values stay shared). `Cause` is never encoded by `encoding/json`. `/ready` logs the cause, includes its text only under `HealthConfig.ExposeErrors`, and never serializes `Details` ([health spec](health.md#readiness-aggregation)), so pool statistics remain available to application code and future metrics without becoming a public probe-response schema.

### Registration

```go
const DefaultPingTimeout = 5 * time.Second

// Register adds the binding of R to the App's store registrations. It
// performs no I/O and holds no value: the start phase resolves R once, pings
// it, and keeps it for readiness. Misuse panics.
func Register[R Lifecycle](app *credo.App, opts ...RegisterOption)

type RegisterOption func(*registerOptions)

// WithName sets the stable identifier used in health reporting.
func WithName(name string) RegisterOption

// WithPingTimeout overrides the deadline (default 5s) of the ping the start
// phase sends the store. Lifecycle.Ping must honor ctx.
func WithPingTimeout(d time.Duration) RegisterOption
```

Names share the same validator as named liveness/readiness checks. They are never normalized: empty names, leading/trailing whitespace, control characters, and the reserved `credo.` prefix are rejected. Omitting `WithName` is distinct from `WithName("")`; the default unwraps pointer layers and uses the package-qualified name of a named type (`*sqldb.DB` → `sqldb.DB`). An `R` without a stable default name — an unnamed type — must provide `WithName`.

There is no store registry API: `/ready` reports the registered stores, and one store's health is the value's own `Health`. The rationale and the rejected alternatives are in [ADR-015](../adr/015-data-access.md#registration).

> Every store is critical: both `DOWN` and `DEGRADED` make readiness return 503, and a per-store optional/critical policy is deferred ([health spec](health.md#readiness-aggregation)).

#### Two calls

The application binds the store where ownership is decided, then registers the binding by type:

```go
db, err := sqldb.Open(&cfg)
if err != nil {
    return err
}
app.ProvideValue(db)           // owned by the App: shut down after its consumers
store.Register[*sqldb.DB](app) // pinged at start, reported by /ready
```

The binding may also be a constructor (`app.Provide[*sqldb.DB](openDB)`), which the start phase builds when nothing resolved it earlier — a store constructor therefore runs in the start phase, not at registration — or a shared handle bound with `credo.Borrowed()` on `ProvideValue`, which is pinged and reported and never shut down by the App. `R` may be bound directly or be an interface an `app.Alias` names.

#### Phases

1. **Registration.** `Register` records `R`, its name and its ping timeout, and panics on misuse known at the call, with the call site (`store.Register[T]`) in the message: a nil App, a nil option, a ping timeout ≤ 0, an invalid explicit name (the name rules above), an `R` without a stable default name and no `WithName`, a name or an `R` already registered, and any registration after `Finalize` or after the App is prepared or shut down. An `R` that does not implement `Lifecycle` does not compile.
2. **`Finalize`.** A registration whose `R` has no binding is a `Finalize` error naming the type and `store.Register` ([container spec](container.md#finalize-and-container-lifecycle)), joined with the graph's other errors in registration order.
3. **Start.** When the [start walk](lifecycle.md#the-start-walk) reaches the binding — in dependency order, in the internal tier unless the binding is ingress — it builds the value, after `Finalize` and therefore after every override, and runs the registration's start step before the value's own `Start`: the ping, with a context bounded by the ping timeout and by the start phase (a shutdown requested during the start cancels it). The components that depend on `R` start after it succeeds. A constructor error while building `R` or a failed ping is a start failure, reported in the `*credo.LifecycleError` under the component's name (its type name); the App rolls back as [ADR-024](../adr/024-lifecycle-components.md) describes, and the value is still rolled back — shut down unless borrowed — since the ping opened nothing the value had not. A nil or typed-nil bound value fails with `store: "name": the bound value is nil`, a failed ping with `store: ping "name": <err>`. After a successful ping, each secret-free registration warning code the value reports (`sqldb.pool.max_open_unlimited`) is logged once at Warn (`credo: store configuration warning`, with `component=store`, `store` and `code`); the codes are read before the ping, so an invalid code fails the start before pinging and without the code being logged, and a failed ping logs none. `Lifecycle.Ping` must honor its context: the start phase abandons a `Ping` that ignores it only at the rollback deadline.
4. **Readiness.** Each pinged store contributes one `/ready` entry under its name, whose probe is built once at start from the value the start phase pinged and reports its typed `Health`; the [health spec](health.md#readiness-aggregation) holds the aggregation rules, name conflicts and status handling included.
5. **Teardown.** Store registration never closes anything. `Lifecycle` includes `Shutdown`, so the binding of `R` is a component: the App shuts it down after its consumers, under the drain deadline, unless it is borrowed. Independent stores with no edges between them shut down in reverse registration order.

The registered store is the value DI hands its consumers, in tests too: an override of `R` (`credo.Override()`, `testutil.WithOverride`) is what is pinged, reported and shut down, and the original value is never pinged. The root owns the store registrations; `store` reaches them through an internal seam, not through the container.

#### Resource identity

`*sqldb.DB` implements `credo.ResourceIdentifier`; its `ResourceIdentity` returns the `*DB`. A wrapper that embeds `*sqldb.DB` inherits it, so the wrapper and the handle are one resource, and two wrappers over one handle are one resource too: shut down once, when the last holder retires, after the consumers of every holder. Two store registrations of one resource are not refused: each is pinged and reported under its own name, and the resource is still shut down once. The rule itself — mixed ownership, named-field wrappers, token validity — is the [container spec's](container.md#resource-identity-and-teardown), with its rationale in [ADR-024](../adr/024-lifecycle-components.md#resource-identity-one-resource-one-teardown).

#### Several databases

Each database beyond a default `*sqldb.DB` is a wrapper type, bound once and registered by its type:

```go
type PrimaryDB struct{ *sqldb.DB }
type AnalyticsDB struct{ *sqldb.DB }

app.ProvideValue(PrimaryDB{primary})
app.ProvideValue(AnalyticsDB{analytics})
store.Register[PrimaryDB](app, store.WithName("primary"))
store.Register[AnalyticsDB](app, store.WithName("analytics"))

func NewReportRepo(db AnalyticsDB) *ReportRepo { return &ReportRepo{db: db.DB} }
```

- The constructor's signature names the database; the compiler and `Finalize` check the choice, and the graph orders each database's start and stop by its consumers. Each `*sqldb.DB` owns its transaction scope, so the transactions of two databases never meet.
- An interface view is `app.Alias[I, T]()`, never a second binding.
- A constructor that needs the raw handle unwraps it (`db.DB`, or `db.Client()` for Bun) instead of binding it again.
- A generic repository is instantiated per wrapper: `NewOutboxRepo[PrimaryDB]` and `NewOutboxRepo[AnalyticsDB]` are two bindings.
- A wrapper may also be bound through a constructor that returns it (`func NewPrimaryDB(cfg PrimaryConfig) (PrimaryDB, error)`); the start phase builds it.
- Databases whose number comes from configuration — shards, tenants — are one binding of a collection type that owns and closes them all.
- A read replica of one database is routing, not a second store, and is outside the rule; `sqldb` does not expose it.

### TX Context

```go
var ErrTxMissing = errors.New("store: transaction missing from context")

// NewTxScope creates a unique logical transaction scope and fixes its
// transaction type T for every subsequent operation.
func NewTxScope[T any]() *TxScope[T]

// WithTxInScope stores a transaction handle in the context for a scope.
func WithTxInScope[T any](ctx context.Context, scope *TxScope[T], tx T) context.Context

// GetTxInScope retrieves a transaction handle for the scope.
func GetTxInScope[T any](ctx context.Context, scope *TxScope[T]) (T, bool)

// RequireTxInScope returns ErrTxMissing instead of falling back.
func RequireTxInScope[T any](ctx context.Context, scope *TxScope[T]) (T, error)

// ConnInScope returns the scoped transaction from context if present,
// otherwise returns the fallback connection.
func ConnInScope[T any](ctx context.Context, scope *TxScope[T], fallback T) T

// Preferred method form; T is already fixed by TxScope[T].
func (s *TxScope[T]) WithTx(ctx context.Context, tx T) context.Context
func (s *TxScope[T]) GetTx(ctx context.Context) (T, bool)
func (s *TxScope[T]) RequireTx(ctx context.Context) (T, error)
func (s *TxScope[T]) Conn(ctx context.Context, fallback T) T
```

`TxScope[T]` deliberately binds the type at construction. For example, `NewTxScope[bun.IDB]()` accepts a concrete `bun.Tx` through the `bun.IDB` contract and reads it back with the same context key; a later call cannot accidentally select `bun.Tx` as a different `T`. Distinct scopes still isolate two databases that share `bun.IDB`. `WithTx` rejects nil values — including a typed-nil pointer held by an interface `T` — as a programming error, so `RequireTx` cannot report a nil handle as present. `RequireTx` is for operations where fallback would violate correctness.

The unscoped `WithTx[T]`, `GetTx[T]` and `Conn[T]` functions are deprecated: separate call sites can choose different concrete/interface forms, and the type-only context key cannot isolate two logical connections that share `T`. New adapters must use a typed scope.

## Bun Wrapper: store/sqldb/

### DB Type

```go
package sqldb

import (
    "database/sql"

    "github.com/uptrace/bun"
)

// DB wraps *bun.DB with lifecycle management, query builder proxies,
// error mapping, and transaction support.
type DB struct { /* unexported: db *bun.DB */ }

// Open creates a DB from Config.
func Open(cfg *Config, opts ...Option) (*DB, error)

// Client returns the underlying *bun.DB for model registration, migration
// operations beyond Migrate, and work tied to the base pool. It bypasses TX
// injection and error mapping; raw SQL uses DB.Exec, Query, and QueryRow.
func (db *DB) Client() *bun.DB

// Stats returns the current database/sql pool statistics snapshot.
func (db *DB) Stats() sql.DBStats

// ResourceIdentity makes *DB a credo.ResourceIdentifier; wrappers that embed
// *DB inherit it.
func (db *DB) ResourceIdentity() any

// StoreRegistrationWarningCodes returns secret-free warning codes that the
// start phase logs after it has pinged a store.Register registration.
func (db *DB) StoreRegistrationWarningCodes() []string

// Lifecycle methods — satisfies store.Lifecycle.
func (db *DB) Ping(ctx context.Context) error
func (db *DB) Shutdown(ctx context.Context) error
func (db *DB) Health(ctx context.Context) store.Health
```

`Shutdown` closes the pool even when its context is already done: `sql.DB.Close` is a fast local operation, and leaving the pool open would leak connections.

### Config

```go
// Config holds connection parameters for SQL databases.
type Config struct {
    Driver         string        // "postgres", "mysql", "sqlite"
    Host           string
    Port           int           // required (1..65535) for generated network DSNs
    Name           string        // database name
    User           string
    Password       string
    DSN            string        // raw driver DSN; structured DSN fields are not merged
    ConnectTimeout time.Duration // connection timeout; PostgreSQL rounds positive values up to seconds
    MaxOpen        int           // max open connections (0 = unlimited)
    MaxIdle        *int          // nil = no Credo idle setter; new(0) = retain none
    MaxLifetime    time.Duration // max connection lifetime (0 = disabled)
    MaxIdleTime    time.Duration // max idle age (0 = disabled)
    SSLMode        string        // driver-specific: PostgreSQL sslmode / MySQL tls
    Options        map[string]string // driver-specific connection params
}
```

A structured `Config` lets `Open` validate the connection settings and lets applications decode them from configuration; `DSN` is the override for full driver-native control.

Driver-family detection is an exact, case-insensitive allowlist: `postgres`/`pgx`, `mysql`, and `sqlite`/`sqlite3`/`sqliteshim`. Names such as `postgres-proxy` or `notmysql` are custom drivers, not implicit aliases; combine their registered driver name and native DSN with `WithDialect`. Connectors do not undergo concrete-type guessing. An explicitly nil `WithDialect` or `WithConnector` (including typed-nil values) makes `Open` fail. A known explicit dialect that conflicts with a known driver family also fails; Credo will not build one family's DSN, emit another family's SQL, and classify errors as the first family.

When Credo constructs a PostgreSQL or MySQL DSN, `Port` must be in `1..65535`; zero is rejected rather than emitted as `:0`. `Config.DSN` and `WithConnector` remain the escape hatches for driver-native default-port, socket, or custom DSN behavior. Host/port serialization uses `net.JoinHostPort`, including bracketed IPv6. Empty host and database-name values are preserved for driver-native interpretation. A positive PostgreSQL `ConnectTimeout` is rounded up to whole seconds, so a sub-second request becomes one second instead of silently turning the timeout off; MySQL retains its duration syntax.

`Options` is for additional driver parameters, not a second source for core PostgreSQL endpoint/credential keys. It may supply `sslmode`/`connect_timeout` or MySQL `tls`/`timeout` only when the corresponding structured field is unset; MySQL `parseTime` is fixed to `true`. Ambiguous duplicates fail and their values are not included in the error. A raw `DSN` is used as-is when full driver-native control is needed. TLS values are deliberately driver-specific: `SSLMode` maps to PostgreSQL `sslmode` and MySQL `tls`. Credo does not impose a universal TLS default; production deployments explicitly choose and provision a verified mode supported by their selected driver.

There is no workload-independent finite pool default ([ADR-015](../adr/015-data-access.md#connection-pool-policy)). `MaxOpen=0` preserves `database/sql`'s unlimited-open behavior. A pool that is still unlimited when the start phase inspects a registered store emits, after a successful ping, one structured warning with code `sqldb.pool.max_open_unlimited`; standalone users can inspect `DB.StoreRegistrationWarningCodes()` and log the same codes themselves.

`MaxIdle` is a pointer because omission and zero have different stdlib semantics: `nil` does not call `SetMaxIdleConns` (the effective stdlib default remains subject to `MaxOpen`), `new(0)` explicitly disables idle retention, and a positive value is applied exactly. If `MaxOpen > 0`, an explicit `MaxIdle > MaxOpen` fails `Open` instead of relying on `database/sql`'s silent clamp. Negative pool counts or durations fail validation. A zero `MaxIdleTime` or `MaxLifetime` disables that expiry policy; positive values are passed to `SetConnMaxIdleTime` and `SetConnMaxLifetime`.

`DB.Stats()` returns the complete `sql.DBStats` snapshot. `DB.Health` pings the database and reports its latency and these `Details` keys: the gauges `max_open`, `open_connections`, `in_use`, and `idle`, and the cumulative `wait_count`, `wait_duration`, `max_idle_closed`, `max_idle_time_closed`, and `max_lifetime_closed`. Cumulative counters require time-window deltas for any saturation policy. A failed ping reports `DOWN` with the error as its typed `Cause`, not copied into `Details`. Pool saturation does not produce `StatusDegraded` ([ADR-015](../adr/015-data-access.md#connection-pool-policy) gives the reasons).

### Options

```go
// Option configures an Open call.
type Option func(*options)

// WithDialect overrides the dialect derived from Config.Driver.
func WithDialect(dialect schema.Dialect) Option

// WithConnector opens through a custom driver.Connector; Config.DSN and the
// DSN built from Config fields are not used.
func WithConnector(connector driver.Connector) Option

// WithTxCleanupTimeout bounds caller wait for each nested savepoint create,
// release, rollback, and fail-safe ambient rollback. Default: 5s; d must be > 0.
func WithTxCleanupTimeout(d time.Duration) Option
```

### Query Builders

Four query builder proxy types wrap Bun's query builders:

```go
func (db *DB) Select(model ...any) *SelectQuery
func (db *DB) Insert(model ...any) *InsertQuery
func (db *DB) Update(model ...any) *UpdateQuery
func (db *DB) Delete(model ...any) *DeleteQuery
```

The optional model argument accepts zero or one value. Supplying more than one causes the builder to record an error (`sqldb: <Op> accepts at most one model, got N`) that the terminal returns without executing the query; extra arguments are never silently ignored.

Each proxy type:

- Proxies a curated subset of builder methods for clean chaining. Methods not in the proxy set are available via `Apply`.
- Selects TX from the owning DB's private `TxScope[bun.IDB]` on terminal operations
- Maps errors to structured semantic `store.Error` values after execution

**Visibility policy: Credo does not hide Bun — it integrates it.** The proxy exists to attach the two terminal guarantees above, not to abstract Bun away; bun types appear in proxy signatures by design. A missing builder method is reached via `Apply`/`ApplyQueryBuilder` (guarantees preserved); a missing _terminal_ method is a request to extend the curated set — the guarantees live in the terminals, so terminals must be on the proxy. `Unwrap` and `Client()` are deliberate opt-outs from both guarantees.

Curated builder methods:

- **`SelectQuery`**: `Model`, `Column`, `ColumnExpr`, `ExcludeColumn`, `TableExpr`, `Join`, `JoinOn`, `JoinOnOr`, `Where`, `WhereOr`, `WherePK`, `OrderExpr`, `Limit`, `Offset`, `Relation`, `Distinct`, `GroupExpr`, `Having`, `Clone`, `Conn`.
- **`InsertQuery`**: `Model`, `Column`, `Value`, `On`, `Set`, `Returning`, `Conn`.
- **`UpdateQuery`**: `Model`, `Column`, `Set`, `Where`, `WherePK`, `Returning`, `OmitZero`, `Conn`.
- **`DeleteQuery`**: `Model`, `Where`, `WherePK`, `Returning`, `Conn`.

Every proxy also has `Apply` and `Unwrap`, and `SelectQuery`, `UpdateQuery`, and `DeleteQuery` have `ApplyQueryBuilder` ([escape hatches](#escape-hatches)). Any other builder method — `WhereOr` on an update or a delete, for example — goes through `Apply` or `ApplyQueryBuilder`.

The curated `Limit(int)` and `Offset(int)` methods forward every value to Bun, which takes and stores both as `int64`. Zero and negative values keep Bun's semantics: the clause is omitted. Calling raw Bun through `Apply` or `Unwrap` is an explicit escape hatch whose `Limit` and `Offset` take `int64`.

**Terminal methods** — `Scan`, `Count`, `Exists` and the generic `One[T]`/`All[T]`/`Page[T]` on `SelectQuery`, `Exec` on the write proxies — execute the query and return mapped errors. Driver errors become `store.Err*` sentinels as listed in [Error Mapping](#error-mapping), so callers branch with `errors.Is` without importing `database/sql` or driver-specific packages. `Update.Exec` and `Delete.Exec` do **not** convert "no rows affected" into `ErrNotFound` — callers must inspect `sql.Result` for that.

`SelectQuery.Count` returns the number of complete logical projection rows, the count `Page.Total` reports; the [pagination spec](pagination.md#logical-total-and-supported-count-shapes) is its contract, including the shapes that return `sqldb.ErrUnsupportedCountQuery`, the MySQL output-name rule, relation callbacks, and model SELECT hooks.

**Typed terminals: `One[T]` / `All[T]`.** Two generic terminals (Go 1.27 concrete-type generic methods) return the queried type directly instead of scanning into a caller-provided destination. `T` drives both the table and the scan destination, so the query is built model-less and the terminal owns the destination:

```go
user, err := db.Select().Where("id = ?", id).One[User](ctx)        // (User, error)
users, err := db.Select().Where("active = ?", true).All[User](ctx) // ([]User, error)
```

`One` applies `LIMIT 1` and returns the first matching row, so multiple matches are not an error — add `OrderExpr` for a deterministic choice; no row returns `store.ErrNotFound` with the zero `T`. `All` returns every matching row, or a non-nil empty slice with a nil error when none match (an empty result is not `ErrNotFound`). Both execute an internal snapshot and inject the ambient transaction exactly like `Scan`, so the receiver is never mutated and a terminal's model/`LIMIT 1` never leaks back into a reused query. The query must be model-less: a model bound through `Select`, `Model`, or `Apply` returns `sqldb.ErrTypedTerminalModel` before execution. `T` must be the actual table model; `TableExpr` does not make `All[DTO]` a projection terminal. Use `TableExpr(...).Scan(ctx, &dest)` or another explicit-destination `Scan` form for projections.

Relations require a bound model and therefore use the general `Scan` terminal, not `One`/`All`/`Page`:

```go
var users []User
err := db.Select(&users).Relation("Orders").Scan(ctx)
```

**Pagination terminal: `Page[T]`.** The third typed terminal runs COUNT and a LIMIT/OFFSET SELECT and returns a `*pagination.Page[T]`:

```go
page, err := db.Select().Where("active = ?", true).OrderExpr("id").Page[User](ctx, req)
```

It is model-less like `One` and `All`; the [pagination spec](pagination.md#adapter-selectquerypaget) is its contract: request validation, the unique tie-breaker the order needs, the total, snapshot consistency, and model→DTO mapping. Cursor/keyset pagination is designed but not exported ([design gate](pagination.md#cursorkeyset-design-gate)).

#### Escape hatches

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

### Proxy Guardrails

1. **TX inject: snapshot, don't mutate** — terminal methods copy the underlying query before applying the TX connection. `SelectQuery` uses an internal execution snapshot that preserves the explicit connection, builder error, `WherePK` fields, soft-delete flags, and model/relation state. `InsertQuery`, `UpdateQuery`, and `DeleteQuery` use a Go struct shallow copy (`copied := *q.raw`) since Bun does not provide `Clone()` on these types — this isolates the `conn` field without affecting shared slices; it suffices because Bun reads, never mutates, the builder while generating SQL. The original `q.raw` is never modified, so query builders can be reused safely — including executing the same builder inside a transaction and again after that transaction finished. Write-query wrappers track `.Conn()` explicitly. Select reads the actual raw connection after `Conn`/`Apply`: a non-nil explicit connection wins, while clearing or replacing it with a connection-less query re-enables ambient TX injection.

2. **Public `Clone`: top-level builder fork, nested sharing** — `SelectQuery.Clone` keeps the execution state (explicit connection, builder error, `WherePK` fields, soft-delete and CTE materialization flags) and otherwise follows Bun's clone semantics. It is not a recursive object-graph copy: bound destinations and nested CTE/relation query values may remain shared. Source and clone must not mutate or scan shared values concurrently.

3. **Apply: Bun-native ergonomics** — `Apply(fns ...func(*bun.XQuery) *bun.XQuery)` matches Bun's own varargs signature and delegates to `q.raw.Apply(fns...)`. Nil functions are filtered out. Terminals keep their interceptors.

4. **Unwrap: builder-only escape** — `Unwrap()` returns the underlying `bun.*Query` for use as a subquery or parameter to other Bun methods. Terminal methods called on the unwrapped query bypass TX injection and error mapping.

5. **Raw terminal wrappers** — `DB` exposes `Exec`, `QueryRow`, and `Query` methods for raw SQL that go through the same TX inject and error mapping pipeline as query builder terminals. Without these, error mapping behavior would split between wrapped and raw queries.

```go
// Raw SQL with TX injection and error mapping.
func (db *DB) Exec(ctx context.Context, query string, args ...any) (sql.Result, error)
func (db *DB) QueryRow(ctx context.Context, dest any, query string, args ...any) error
func (db *DB) Query(ctx context.Context, dest any, query string, args ...any) error
```

6. **ApplyQueryBuilder: predicates land on the proxied query** — `q.raw = fn(q.raw.QueryBuilder()).Unwrap().(*bun.XQuery)`, mirroring Bun's own `ApplyQueryBuilder`, so terminals keep TX injection and error mapping exactly as with `Apply`. A nil fn is a no-op. Why it is a function rather than a `QueryBuilder()` accessor is in [ADR-015](../adr/015-data-access.md#query-proxies).

7. **The count-shape guard sees `Apply`** — `Count` and `Page` inspect the builder that `Apply` produced, so `Apply` stays inside the [count-shape guard](pagination.md#logical-total-and-supported-count-shapes); executing a terminal through `Unwrap` bypasses it with the other proxy guarantees.

**Planned observability:** automatic tracing via a DB-level `bun.QueryHook` belongs to the observability release ([ADR-014](../adr/014-observability.md)) and is not installed by `Open()` today. Attaching it at the Bun DB boundary will cover proxy queries, native `Client()`/`Conn()` builders, raw SQL, and migrations without duplicating instrumentation in terminal wrappers.

### TX Management

```go
var (
    ErrNilTxCallback  = errors.New("sqldb: transaction callback must not be nil")
    ErrNestedTxOptions = errors.New("sqldb: nested transaction options are not supported")
    ErrTxRollbackOnly = errors.New("sqldb: transaction is rollback-only")
)

// RunInTx starts a transaction, stores it in this DB's typed scope,
// executes fn, and commits on nil / rolls back on error.
// Nested calls use savepoints. A nil fn fails before BEGIN.
func RunInTx(ctx context.Context, db *DB, fn func(ctx context.Context) error) error

// RunInTxWith applies sql.TxOptions to an outer transaction. Non-default
// options on a nested savepoint return ErrNestedTxOptions.
func RunInTxWith(ctx context.Context, db *DB, opts *sql.TxOptions, fn func(ctx context.Context) error) error

// InTx / InTxWith are the method forms of RunInTx / RunInTxWith —
// handler-side ergonomics: db.InTx(ctx.Context(), fn).
func (db *DB) InTx(ctx context.Context, fn func(ctx context.Context) error) error
func (db *DB) InTxWith(ctx context.Context, opts *sql.TxOptions, fn func(ctx context.Context) error) error
```

**Semantics:**

| Callback state | Action |
| --- | --- |
| nil callback | Return `ErrNilTxCallback`; do not begin |
| returns `nil` | Commit |
| returns `error` | Roll back; after successful rollback return the exact callback error without SQL mapping |
| panics | Attempt rollback; re-panic the original value |

Only begin/commit/rollback driver errors pass through `mapError`. If rollback also fails, the callback error and the mapped rollback error are joined so `errors.Is` reaches both causes. `sql.ErrTxDone` is treated as an already-completed rollback only when the transaction context is canceled; an unexpected `ErrTxDone` is retained as a cleanup failure.

The callback is the transaction lifetime boundary: it must not retain its transaction context or launch nested/transaction work that outlives the callback return. The rollback-only state machine guarantees supported sequential/nested use; detached concurrent work is outside the contract.

**Commit outcome:** a commit error does not universally prove that the transaction was not applied. The mapped error describes the driver cause, not a definite rollback result. Calling code must not retry blindly from the mere presence of a commit error; retry policy requires a driver/state classification whose outcome is known.

**Nested TX:** When `RunInTx` is called within an existing transaction for the same `DB`, Bun creates a `SAVEPOINT`. Bun's savepoint implementation cannot change isolation or read-only mode. A nil or zero-valued `TxOptions` is accepted; a nested non-default option returns `ErrNestedTxOptions` before the savepoint and callback rather than being silently ignored. Bun also stores the context used to create a savepoint and reuses it for `RELEASE` / `ROLLBACK TO SAVEPOINT`; Credo therefore controls a cancellation-detached savepoint context while callback queries keep the original context. Savepoint creation observes callback cancellation and the cleanup budget; the callback does not run if creation becomes uncertain. If a callback returns nil after cancellation, Credo rolls back and returns the context error; a canceled nested callback rolls back only its savepoint, and a healthy outer transaction stays usable. Creation, release/rollback, and fail-safe ambient abort each have a five-second default wait budget, configurable with `WithTxCleanupTimeout`; callback execution time is not counted. Before an uncertain savepoint operation starts its asynchronous ambient abort, Credo synchronously marks shared transaction state rollback-only. Every outer level checks that state before commit; a level that would otherwise commit returns `ErrTxRollbackOnly`, while an existing callback or context error remains the more specific rollback result. This prevents abort goroutine scheduling from creating a commit race, and further nested calls fail immediately without invoking their callback. Panic cleanup follows the same fail-safe path while re-raising the original panic value. A driver that ignores cancellation can retain its goroutine/connection until it returns, but the caller path is bounded and the transaction remains fail-closed.

### Transaction-Aware Bun Connection

```go
// Conn returns this DB's active transaction from ctx, or the base *bun.DB.
// Native calls bypass Credo error mapping.
func (db *DB) Conn(ctx context.Context) bun.IDB

// RequireTx returns the active transaction or store.ErrTxMissing; it never
// falls back to the base DB.
func (db *DB) RequireTx(ctx context.Context) (bun.IDB, error)
```

`Conn` is a borrowed connection selector, not a dedicated `sql.Conn` lease. When it returns a transaction, the value must not escape the `InTx` callback. It is the explicit native-Bun path:

```go
err := db.InTx(ctx, func(txCtx context.Context) error {
    return db.Conn(txCtx).
        NewSelect().
        Model(&users).
        Relation("Orders").
        Scan(txCtx)
})
```

Each `DB` owns a separate `TxScope[bun.IDB]`, so passing a primary-DB transaction context to `analytics.Conn(ctx)` selects the analytics base DB, not the primary transaction.

### Migrations (bun/migrate wrapper)

```go
// RegisterMigrations stores the migration set that Migrate runs.
// opts pass through to migrate.NewMigrator (table names, hooks, ...).
// Panics if m is nil or if already registered (wiring-time misuse).
func (db *DB) RegisterMigrations(m *migrate.Migrations, opts ...migrate.MigratorOption)

// Migrate runs pending migrations: Init (bookkeeping tables, IF NOT
// EXISTS) → Lock (table-based advisory lock, fail-fast) → Migrate →
// bounded Unlock. Signature matches App.OnStart; use that convenience
// for dev/single-replica deployments:
//
//	db.RegisterMigrations(migrations)
//	app.OnStart(db.Migrate)
func (db *DB) Migrate(ctx context.Context) error
```

`Migrate` without a registered set returns an error. The design rationale — one pre-deploy job, no lock wait, mark-on-success — is in [ADR-015](../adr/015-data-access.md#migrations).

- **Thin wrapper**: the `*migrate.Migrations` set is Bun's own type — populated via `Discover(fsys)` for SQL files (works with `embed.FS`) or `MustRegister` for Go migrations. Credo does not re-wrap it.
- **Mark-applied-on-success by default**: the wrapper passes `migrate.WithMarkAppliedOnSuccess(true)`. An Up error surfaced by Bun leaves the migration unapplied, so the next run attempts it again. This is at-least-once bookkeeping, not an atomicity guarantee: non-transactional effects can be partial, successful body execution and marker insertion are separate operations, and a migration group is not all-or-nothing. Migrations must be transactional where supported or idempotent/reconcilable. Users can pass `WithMarkAppliedOnSuccess(false)` to choose Bun's record-before-running recovery tradeoff explicitly.
- **Lock semantics**: Lock is a fail-fast unique-row insert; there is no library wait/retry. Unlock starts only after Lock succeeds. Its context preserves parent values but ignores parent cancellation/deadline and gets a fresh fixed five-second budget. A buffered-result goroutine also bounds caller wait when a driver ignores context. Migration and unlock errors are joined and mapped. Unlock timeout is an uncertain outcome: the old operation may still delete later, so no automatic second Unlock, blind lock-row deletion, or immediate retry is safe.
- **Production topology**: multi-replica production runs one deadline-bounded pre-deploy job that calls the same `DB.Migrate` path before rollout. `app.OnStart(db.Migrate)` is for local/dev/test, single replicas, or a consciously accepted small-deployment tradeoff. Running it on every replica can make lock losers fail startup and crash-loop while the winner is still migrating. Expand → compatible deploy/backfill → later contract is the default destructive-change strategy.
- **Transaction boundary**: ordinary `.up.sql` files are not transactional; `.tx.up.sql` runs in one transaction, whose COMMIT or ROLLBACK error reaches `Migrate` and leaves the migration unapplied (pinned by a conformance test on SQLite and PostgreSQL). Database DDL support still applies, and replay/repair must stay safe even then.
- **Seeding** is a plain migration file (e.g. `2_seed_plans.up.sql`) — no separate mechanism.
- **No CLI here**: the planned `credo migrate:*` commands are optional sugar over this wrapper. Rollback/status/file generation stay on Bun's migrator, but a directly constructed migrator does not inherit `RegisterMigrations` options. Read-only status and file generation repeat only relevant options; DB-mutating apply/rollback paths also own Init, Lock, and bounded cancellation-detached Unlock.
- **OnStart integration is signature compatibility**, not coupling: `sqldb` imports only `credo/store`, never the root framework package.

### Error Mapping

The mapper receives the operation context and the `DB`'s driver family (derived from the driver name, or from the configured Bun dialect for custom connectors). An error that already carries a `fault` kind passes through unchanged. SQLSTATE is the structured cross-driver path; when MySQL provides both a number and a broad SQLSTATE class, its more specific number wins. Strict MySQL error envelopes are parsed only for MySQL, and SQLite numeric codes only for SQLite (`Code() int` for modernc, allowlisted `Code`/`ExtendedCode` fields for mattn, typed `ExtendedCode()`/`Code()` methods for ncruces — the latter two matched by package path via reflection, so no driver becomes a dependency). Unmapped errors pass through with exact identity. Loose message matching is forbidden because a scanner/query hook/domain error may legitimately contain text such as “duplicate key validation”.

| Source | Codes / condition | Semantic result |
| --- | --- | --- |
| database/sql | `sql.ErrNoRows` | `ErrNotFound` |
| PostgreSQL | `23505` | `ErrAlreadyExists` |
| PostgreSQL | other class `23` | `ErrConstraint` |
| PostgreSQL | `40001` / `40P01` / `55P03` | `ErrSerialization` / `ErrDeadlock` / `ErrContention` |
| PostgreSQL | `25006` | `ErrReadOnly` |
| PostgreSQL | class `08`, `57P01-3`, `53300` | `ErrUnavailable` |
| MySQL | 1022/1062 | `ErrAlreadyExists` |
| MySQL | 1048, 1216/1217, 1451/1452, 3819 | `ErrConstraint` |
| MySQL | 1213 / 1205 or 3572 | `ErrDeadlock` / `ErrContention` |
| MySQL | 1792/1836 | `ErrReadOnly` |
| MySQL | 1040 | `ErrUnavailable` |
| SQLite | UNIQUE/PRIMARYKEY/ROWID extended codes | `ErrAlreadyExists` |
| SQLite | other constraint codes | `ErrConstraint` |
| SQLite | BUSY_SNAPSHOT | `ErrSerialization` |
| SQLite | BUSY/LOCKED (including extended codes) | `ErrContention` |
| SQLite | READONLY (including extended codes) | `ErrReadOnly` |
| SQLite | IOERR, CANTOPEN (including extended codes) | `ErrUnavailable` |
| generic | `context.DeadlineExceeded` | `ErrTimeout` |
| generic | `driver.ErrBadConn`, `sql.ErrConnDone` | `ErrUnavailable` |

The SQLSTATE rows apply to any driver that exposes SQLSTATE, not only PostgreSQL's. PostgreSQL `57014` is `query_canceled`, not intrinsically timeout. A canceled context is returned as cancellation; a deadline maps to timeout; a live context maps only a structured driver error that explicitly identifies statement timeout. SQLite `INTERRUPT` follows the same context rule and otherwise passes through unchanged. MySQL 1290 is deliberately not treated as read-only because it is a generic option-prevents-statement code. Each mapped error is `*store.Error` and preserves the original cause plus driver `Code`. Driver-family-specific classifier registration and a public normalizer are deferred until a real custom-driver consumer appears.

### Client Escape Hatch

**Warning**: queries executed via the `*bun.DB` that `Client()` returns bypass the proxy interceptors. There is no automatic TX injection from context (so `InTx` / `RunInTx` will not affect them) and no error mapping to `store.Err*` sentinels. Use `DB.Exec`, `Query`, and `QueryRow` for raw SQL and `DB.Conn(ctx)` for a native Bun operation that must join the ambient transaction. Reserve `Client()` for model registration, work intentionally tied to the base pool, and migration operations beyond `Migrate` (rollback, status, file generation); use the proxy layer for normal repository code.

Credo installs no Bun `ConnResolver`; `DB.Conn(ctx)` is the explicit transaction-aware native path ([ADR-015](../adr/015-data-access.md#context-based-tx)).

## Examples

### Basic Repository

```go
import (
    "context"
    "fmt"

    "github.com/credo-go/credo/store/sqldb"
)

type UserRepo struct {
    db *sqldb.DB
}

func NewUserRepo(db *sqldb.DB) *UserRepo {
    return &UserRepo{db: db}
}

func (r *UserRepo) GetByID(ctx context.Context, id int64) (*User, error) {
    user, err := r.db.Select().Where("id = ?", id).One[User](ctx)
    if err != nil {
        // err is already mapped: sql.ErrNoRows → store.ErrNotFound
        return nil, fmt.Errorf("user get by id: %w", err)
    }
    return &user, nil
}

func (r *UserRepo) Create(ctx context.Context, user *User) error {
    _, err := r.db.Insert(user).Exec(ctx)
    return err // unique violation → store.ErrAlreadyExists
}
```

### Service with TX

```go
import (
    "context"

    "github.com/credo-go/credo"
    "github.com/credo-go/credo/store/sqldb"
)

type OrderService struct {
    infra     credo.Infra
    db        *sqldb.DB
    orderRepo *OrderRepo
    stockRepo *StockRepo
}

func (s *OrderService) PlaceOrder(ctx context.Context, input OrderInput) (*Order, error) {
    var order *Order
    err := sqldb.RunInTx(ctx, s.db, func(ctx context.Context) error {
        // TX is in context — sqldb proxies pick up this DB's typed scope
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

## Design Decisions

The rationale and the rejected alternatives are in ADR-015:

- One ORM, Bun, integrated deeply; other ORMs through raw DI — [Why Single-ORM (Bun)?](../adr/015-data-access.md#why-single-orm-bun)
- A transport-neutral `fault` kind with structural, family-scoped classification — [Semantic Error Types](../adr/015-data-access.md#semantic-error-types)
- An opt-in, per-database typed transaction scope that fails loud rather than falling back — [Context-Based TX](../adr/015-data-access.md#context-based-tx)
- Proxies that carry the terminal guarantees, with explicit escape hatches — [3-Tier Bun Wrapping](../adr/015-data-access.md#3-tier-bun-wrapping), [Query Proxies](../adr/015-data-access.md#query-proxies)
- Typed terminals named for their return shape — [Typed Terminals](../adr/015-data-access.md#typed-terminals)
- No finite pool default and no automatic `DEGRADED` — [Connection-Pool Policy](../adr/015-data-access.md#connection-pool-policy)
- A thin `bun/migrate` wrapper run as a pre-deploy job — [Migrations](../adr/015-data-access.md#migrations)
- Ownership follows the binding; registration pings and reports but never closes — [Registration](../adr/015-data-access.md#registration)
