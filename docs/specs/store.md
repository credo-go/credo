# Store Spec

**Status**: Implemented **Package**: `store/` (core contracts), `store/sqldb/` (Bun wrapper, separate submodule) **Sources**: GoFr (Apache-2.0, health/interface design), Goyave (MIT, connection patterns) **ADRs**: [015-data-access.md](../adr/015-data-access.md)

---

## Canonical Source

Implementation-level details for Credo's data access layer are defined in this file. Other documents should keep only high-level references and link here.

---

## Overview

Credo's data access layer is split into two packages:

- **`store/`** — universal contracts with zero external dependencies. Part of the main `github.com/credo-go/credo` module. Defines error sentinels, lifecycle/health interfaces, a connection registry, registration API, and context-based TX helpers.
- **`store/sqldb/`** — Bun SQL wrapper in a separate Go submodule (`github.com/credo-go/credo/store/sqldb`). Wraps `*bun.DB` with lifecycle management, query builder proxies, error mapping, and transaction management.

Key design properties:

- **Semantic errors** — transport-neutral `fault.Kind` / `store.Kind`, structured `store.Error`, exact constraint/concurrency/unavailable sentinels, the deprecated `ErrDuplicate` alias, and deprecated `ErrConflict` umbrella.
- **Context-based TX** — a transaction type is fixed by `TxScope[T]`; each logical connection owns a distinct scope. `store/sqldb` hides that scope behind `InTx` and exposes `DB.Conn(ctx)` / `DB.RequireTx(ctx)` for transaction-aware native Bun work.
- **3-tier wrapping** — own (lifecycle), enrich (query proxies), passthrough (`Client()` escape hatch).
- **Single ORM focus** — deep Bun integration. Other ORMs via raw DI.

---

## Goals

1. **Universal semantic errors**: adapters classify driver failures as transport-neutral `Kind` values while preserving cause and safe diagnostic metadata. Root HTTP policy and future gRPC policy consume the shared stdlib-only `fault.Provider` seam without importing `store/`.
2. **Context-based TX**: `db.InTx` / `RunInTx` plus a per-connection `TxScope[T]` provide opt-in transaction participation without concrete/interface fallback or same-type multi-DB collisions. Repositories that don't need TX are unaffected.
3. **3-tier Bun wrapping**: Own lifecycle, enrich queries (TX injection and error mapping), passthrough via `Client()`; DB-level tracing hooks remain planned for Phase 3.5.
4. **Clean module boundary**: `store/` is stdlib-only in the main module. `store/sqldb/` is a separate submodule with the Bun dependency.
5. **Unified registration**: `store.Register[R]` validates and reserves locally,
   then commits protected DI registration and Registry visibility only after
   Ping succeeds. Name, type, and declared resource identity are unique within
   the store Registry ledger.
6. **Lifecycle management**: direct `Lifecycle` values become framework-owned
   only after successful registration; separate handles require an explicit
   caller-owned opt-out. DI is the sole framework shutdown owner, subject to
   the teardown deadline; the Registry aggregates health and never closes resources.
7. **Escape hatches**: `DB.Conn(ctx) bun.IDB` for transaction-aware native Bun operations; `Client() *bun.DB` for model registration, migrations, and work intentionally tied to the base pool.
8. **Migrations without a second engine**: a thin wrapper over `bun/migrate` (`RegisterMigrations` + `Migrate`) whose signature plugs straight into `app.OnStart` — no goose, no new dependency.

---

## Core Package: store/

### Semantic Errors

The stdlib-only `fault` package owns transport-neutral kinds and the provider
contract. `store.Kind` aliases `fault.Kind`; root HTTP policy imports only the
leaf package, so the existing `store → credo` registration dependency does not
form a cycle.

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

func KindOf(err error) (Kind, bool)
func IsTransient(err error) bool
```

`Error.Unwrap` preserves the original cause. `Code`, `Constraint`, `Resource`,
and `Cause` are diagnostic metadata and are never emitted by Credo's default
Problem Details renderer. `Transient` says a condition may clear; it is not a
promise that replaying a statement, transaction callback, or externally
visible operation is safe.

| Exact sentinel | Kind | Transient default | Root HTTP default |
| --- | --- | ---: | ---: |
| `ErrNotFound` | `KindNotFound` | no | 404 |
| `ErrAlreadyExists` | `KindAlreadyExists` | no | 409 |
| `ErrConstraint` | `KindConstraint` | no | 409 |
| `ErrSerialization` | `KindSerialization` | yes | 409 |
| `ErrDeadlock` | `KindDeadlock` | yes | 409 |
| `ErrContention` | `KindContention` | yes | 409 |
| `ErrTimeout` | `KindTimeout` | yes | 504 |
| `ErrUnavailable` | `KindUnavailable` | yes | 503 |
| `ErrReadOnly` | `KindReadOnly` | no | 503 |

`ErrDuplicate` is a deprecated alias of `ErrAlreadyExists`. `ErrConflict` is a
deprecated umbrella: `ErrConstraint`, `ErrSerialization`, `ErrDeadlock`, and
`ErrContention` continue to match it through `errors.Is`. Their exact kind is
not lost. Store errors retain `HTTPStatus()` only as a deprecated compatibility
bridge; root classifies `fault.Provider` first. An outer `*credo.HTTPError`
remains the service/domain override and preserves the internal store cause.

Outcome ambiguity is not a cause kind. A commit failure may leave the operation
outcome unknown, but Credo does not infer that from a connection/transient flag
and never treats `Transient` as blind-retry permission. A separate operation-
phase outcome model is deferred until reliable evidence is available.

### Lifecycle Interface

```go
// Lifecycle manages connection health and shutdown.
// Adapters implement this interface for use with Register[R].
type Lifecycle interface {
    Ping(ctx context.Context) error
    Shutdown(ctx context.Context) error
    Health(ctx context.Context) Health
}

// LifecycleIdentityProvider is an optional extension for a Lifecycle wrapper
// that represents another physical resource.
type LifecycleIdentityProvider interface {
    Lifecycle
    ResourceIdentity() any
}
```

Register needs a stable, comparable resource key to reject contradictory
ownership inside its Registry ledger:

- The default identity is the top-level Lifecycle value itself. Pointer-backed
  implementations are the normal and recommended shape. A pointer to a
  composite Lifecycle remains a valid identity; Credo does not inspect its
  fields.
- A semantic or named-field wrapper that represents another resource implements
  `LifecycleIdentityProvider` and returns that resource's pointer or another
  explicit token. An embedded `*sqldb.DB` inherits `ResourceIdentity` by normal
  Go method promotion, not framework reflection.
- A token must be non-nil, comparable, reflexively equal to itself, and stable
  for the resource lifetime. Non-pointer values/tokens are allowed only when
  they satisfy those rules. A slice/map-bearing value without an explicit
  token and non-reflexive values such as NaN fail before Ping.

No struct-field scanning or automatic contained-Lifecycle inference occurs.
Correct identity forwarding is part of a semantic wrapper's contract.

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

`Cause` is preserved by `Health.Clone` and never encoded by `encoding/json`.
The root readiness policy logs it and includes its text only when
`HealthConfig.ExposeErrors` is explicitly enabled. Free-form
`Details["error"]` values are adapter metadata, not a trusted error contract.
Readiness responses do not serialize adapter `Details`; pool statistics remain
available to application code and future metrics without becoming a public
probe-response schema.

### Registry

The `Registry` tracks successfully registered data stores for health
aggregation. It is created automatically on the first `Register` call and
stored in the DI container; a composition root may also provide its own empty
Registry value or constructor before registration.

Behavior:

- `HealthAll(ctx)` — return defensive health snapshots keyed by name (direct
  compatibility API; the root readiness endpoint instead consumes stable
  per-entry probes so stores can run concurrently and independently)
- Registration privately reserves the store name, DI value type, and declared
  resource identity. Pending entries are invisible to `HealthAll` and
  readiness and are committed only after Ping and DI publication both succeed.

The Registry has no public mutation method: bypassing `Register` would also
bypass Ping, DI publication, and shutdown ownership. It does not close
connections. The Registry DI binding is protected against `App.Replace` once
store integration adopts it, so the readiness seam cannot diverge from the
resolved Registry. For framework-owned values, DI is the sole framework
shutdown owner: it traverses registrations in reverse order and makes at most
one Shutdown attempt per teardown when the still-live deadline reaches that
entry. If the deadline is already exhausted, the entry may receive no attempt.

```go
// Registry exposes health snapshots for successfully registered stores.
// It does not close connections; shutdown follows the ownership mode
// selected by Register.
type Registry struct { /* fields unexported */ }

func (r *Registry) HealthAll(ctx context.Context) map[string]Health
```

### Registration API

```go
const DefaultPingTimeout = 5 * time.Second

// Register registers value as type R in DI, pings the connection,
// and tracks it in the Registry for lifecycle and health management.
//
// A value that implements Lifecycle is framework-owned after success.
// A separate Lifecycle handle requires an explicit caller-owned opt-out.
func Register[R any](app *credo.App, value R, opts ...RegisterOption) error
```

Steps:

1. **Validate locally** — validate the value, options, canonical health name,
   lifecycle/ownership combination, and the point-in-time DI ability to
   provide `R`. Predictable local failures happen before network I/O.
2. **Ensure Registry** — resolve or create the Registry and idempotently bind
   the internal readiness seam to that exact instance.
3. **Reserve** — privately reserve the name, `R` type, and validated resource
   identity. Existing or pending duplicates fail before Ping; the
   reservation is not visible to readers.
4. **Re-check DI and Ping** — re-check the point-in-time DI preflight after
   infrastructure setup, then call Ping with a deadline-scoped context.
   `Lifecycle.Ping` must honor `ctx`; Register calls it synchronously and cannot
   hard-bound a non-cooperative implementation.
5. **Publish and commit** — `ProvideProtectedValue[R]` remains authoritative
   against an external concurrent registration/finalization and prevents a
   later `Replace[R]` from detaching DI from lifecycle/health state. Only a
   successful DI publication commits the Registry entry; every failure
   releases the pending reservation.
6. **Emit registration diagnostics** — after a successful commit, a value that
   reports registration warning codes is logged once per code through the
   app's structured logger. `sqldb.DB` reports
   `sqldb.pool.max_open_unlimited` when the effective pool maximum is still
   unlimited at inspection time; failed registrations do not emit a
   misleading success-time warning.

Shutdown ownership is explicit:

- When `value` implements `Lifecycle`, that same value supplies Ping, Health,
  and Shutdown. Ownership transfers to the framework only when `Register`
  succeeds. DI is the sole framework shutdown owner. During one teardown it
  makes at most one `Shutdown(ctx)` attempt if the still-live deadline reaches
  the registration in reverse order; it may make zero attempts when the
  deadline expires first.
- A value that cannot implement `Lifecycle` may use `WithLifecycle(lc)` only
  together with `WithCallerOwnedLifecycle()`. The handle supplies Ping and
  Health, but the caller retains Shutdown responsibility (for example through
  `app.OnShutdown(lc.Shutdown)`). `WithLifecycle` alone is an error.
- A `Lifecycle` value combined with either explicit option is rejected, as is
  a Shutdowner-only value combined with a separate lifecycle. Ping/Health and
  Shutdown cannot silently target different objects.

On every failure, including Ping or authoritative DI publication failure,
ownership remains with the caller. No value binding or health entry is
committed. The Registry/readiness seam established during infrastructure setup
may remain as empty idempotent framework infrastructure.

Identity equality is enforced only among entries and pending reservations in
this `store.Register` Registry. The top-level Lifecycle value is the default
identity. Concrete/interface views of the same pointer therefore collide, and
wrappers that explicitly return the same `ResourceIdentity` token collide
across wrapper and ownership modes. There is deliberately no field scanning:
a named wrapper must forward identity explicitly, while an embedded
`*sqldb.DB` inherits the adapter's method through Go promotion. A composite
pointer Lifecycle is valid as its own resource. When another interface should
resolve to the same store, register the concrete type once and use
`app.Alias[I, T]()`; an alias does not create another health entry or shutdown
owner.

This is not a container-wide resource ledger. Publishing the same lifecycle
again under another T with raw `app.Provide`, `app.ProvideFactory`,
`app.ProvideValue`, `app.ProvideProtectedValue`, or `app.Replace` is unsupported
and can produce contradictory ownership or multiple Shutdown attempts. A
caller-owned handle must not also be registered in DI as a Shutdowner. A
general resource registry across store, pubsub, gRPC, workers, and other
infrastructure remains deferred until a second concrete consumer requires it.

The module-internal readiness seam is idempotently replaced around the
resolved Registry during registration. A Registry provided earlier by the
composition root is resolved and checked for a non-nil successful value before
`app.ProtectBinding[*Registry](resolved)` atomically compares and protects that
same already-resolved, comparable pointer against `Replace`; it is then
re-resolved for wiring. A mismatch fails without protecting the replacement. A
nil or failing constructor binding is also left unprotected so composition can
repair it with `Replace` before Finalize and retry. An interrupted seam publish
can be retried without creating a second Registry.

`app.CanProvideValue[R]()` is a non-mutating, point-in-time preflight for the
container's frozen and duplicate-type checks. It is not a reservation: an
external concurrent mutation may still make the final protected publication
fail.

Both a successfully registered `R` and a validated/adopted `*Registry` direct
binding are protected. `app.Replace[R]` and
`app.Replace[*store.Registry]` therefore return an error after store adoption
instead of creating an untracked resource or disconnecting readiness from the
Registry. Invalid Registry bindings remain replaceable until adoption. The
repair must occur before Finalize. The low-level protection APIs are documented
in ADR-004; normal application bindings remain replaceable.

```go
type RegisterOption func(*registerOptions)

// WithName sets the canonical health name. Explicit empty, padded,
// control-character, and reserved credo.* names are rejected.
// If omitted, the pointer-unwrapped package-qualified named type is used.
func WithName(name string) RegisterOption

// WithPingTimeout overrides the Ping context deadline (default 5s).
// Lifecycle.Ping must honor ctx; Register invokes it synchronously.
func WithPingTimeout(d time.Duration) RegisterOption

// WithLifecycle supplies Ping and Health for a value that does not implement
// Lifecycle. It is valid only with WithCallerOwnedLifecycle.
func WithLifecycle(lc Lifecycle) RegisterOption

// WithCallerOwnedLifecycle explicitly keeps shutdown ownership with the
// caller. It is valid only together with WithLifecycle.
func WithCallerOwnedLifecycle() RegisterOption
```

Names share the same validator as named liveness/readiness checks. They are
never normalized: empty names, leading/trailing whitespace, control
characters, and the reserved `credo.` prefix are rejected. Omitting
`WithName` is distinct from `WithName("")`; the default unwraps pointer layers
and uses the package-qualified name of a named type. Unnamed `R` values must
provide `WithName`.

> **Health/readiness options** (`WithCritical`, `WithTags`) remain deferred to
> [ADR-016](../adr/016-health-checks.md). All stores are currently critical;
> both `DOWN` and `DEGRADED` make readiness return 503.

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

The older unscoped `WithTx[T]` / `GetTx[T]` / `Conn[T]` functions remain source-compatible but are deprecated: separate call sites can choose different concrete/interface forms, and the type-only context key cannot isolate two logical connections that share `T`. New adapters must use a typed scope.

---

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

// Client returns the underlying *bun.DB for raw SQL, migrations,
// model registration, and features not covered by proxies.
func (db *DB) Client() *bun.DB

// Stats returns the current database/sql pool statistics snapshot.
func (db *DB) Stats() sql.DBStats

// StoreRegistrationWarningCodes returns secret-free warning codes that the
// canonical store.Register path logs after successful registration.
func (db *DB) StoreRegistrationWarningCodes() []string

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

// WithTxCleanupTimeout bounds caller wait for each nested savepoint create,
// release, rollback, and fail-safe ambient rollback. Default: 5s; d must be > 0.
func WithTxCleanupTimeout(d time.Duration) Option
```

Driver-family detection is an exact, case-insensitive allowlist:
`postgres`/`pgx`, `mysql`, and `sqlite`/`sqlite3`/`sqliteshim`. Names such as
`postgres-proxy` or `notmysql` are custom drivers, not implicit aliases; combine
their registered driver name and native DSN with `WithDialect`. Connectors do
not undergo concrete-type guessing. An explicitly nil `WithDialect` or
`WithConnector` (including typed-nil values) makes `Open` fail. A known explicit
dialect that conflicts with a known driver family also fails; Credo will not
build one family's DSN, emit another family's SQL, and classify errors as the
first family.

When Credo constructs a PostgreSQL or MySQL DSN, `Port` must be in `1..65535`;
zero is rejected rather than emitted as `:0`. `Config.DSN` and `WithConnector`
remain the escape hatches for driver-native default-port, socket, or custom DSN
behavior. Host/port serialization uses `net.JoinHostPort`, including bracketed
IPv6. Empty host and database-name values are preserved for driver-native
interpretation. A positive PostgreSQL `ConnectTimeout` is rounded up to whole
seconds, so a sub-second request becomes one second instead of silently turning
the timeout off; MySQL retains its duration syntax.

`Options` is for additional driver parameters, not a second source for core
PostgreSQL endpoint/credential keys. It may supply `sslmode`/`connect_timeout`
or MySQL `tls`/`timeout` only when the corresponding structured field is unset;
MySQL `parseTime` is fixed to `true`. Ambiguous duplicates fail and their values
are not included in the error. A raw `DSN` is used as-is when full driver-native
control is needed. TLS values are deliberately driver-specific: `SSLMode` maps
to PostgreSQL `sslmode` and MySQL `tls`. Credo does not impose a universal TLS
default; production deployments explicitly choose and provision a verified
mode supported by their selected driver.

Credo deliberately has no workload-independent finite pool default.
`MaxOpen=0` preserves `database/sql`'s unlimited-open behavior. A pool that is
still unlimited when a successful canonical `store.Register` inspects it emits
one structured warning with code `sqldb.pool.max_open_unlimited`; standalone
users can inspect `DB.StoreRegistrationWarningCodes()` and log the same codes
themselves.

`MaxIdle` is a pointer because omission and zero have different stdlib
semantics: `nil` does not call `SetMaxIdleConns` (the effective stdlib default
remains subject to `MaxOpen`), `new(0)` explicitly disables idle retention,
and a positive value is applied exactly. If `MaxOpen > 0`, an explicit
`MaxIdle > MaxOpen` fails `Open` instead of relying on `database/sql`'s silent
clamp. Negative pool counts or durations fail validation. A zero `MaxIdleTime`
or `MaxLifetime` disables that expiry policy; positive values are passed to
`SetConnMaxIdleTime` and `SetConnMaxLifetime`.

`DB.Stats()` returns the complete `sql.DBStats` snapshot. `DB.Health` includes
the current open/in-use/idle counts and the cumulative `WaitCount`,
`WaitDuration`, `MaxIdleClosed`, `MaxIdleTimeClosed`, and
`MaxLifetimeClosed` counters in adapter details. These counters are cumulative,
so production saturation policy must use time-window deltas rather than raw
totals.

Pool saturation does not automatically produce `StatusDegraded`. Such a
policy is deferred until production metrics and an explicit SLO justify
opt-in thresholds, windowed deltas, and hysteresis. Today all stores are
critical and `DEGRADED` removes readiness; a universal threshold could
therefore cause a cascading traffic shift.

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

**SelectQuery proxy methods** (~20): `Model`, `Column`, `ColumnExpr`, `ExcludeColumn`, `TableExpr`, `Join`, `JoinOn`, `JoinOnOr`, `Where`, `WhereOr`, `WherePK`, `OrderExpr`, `Limit`, `Offset`, `Relation`, `Distinct`, `GroupExpr`, `Having`, `Clone`, `Conn`.

The curated `Limit(int)` and `Offset(int)` methods protect the pinned Bun v1.2.18 representation. Bun accepts `int` publicly but stores both values as signed `int32`; an input outside that range records `sqldb.ErrInvalidLimitOffset`, and the terminal returns the builder error without executing. Inputs inside the range — including zero and negatives — are passed through unchanged, preserving Bun semantics. This guard belongs to the curated methods. Calling raw Bun through `Apply` or `Unwrap` is an explicit escape hatch and retains Bun's own narrowing behavior.

**Terminal methods** (Scan, Exec, Count, Exists, plus the generic `One[T]`/`All[T]`/`Page[T]` below) execute the query and return mapped errors. Driver errors are translated to `store.Err*` sentinels before returning, so callers can branch with `errors.Is` without importing `database/sql` or driver-specific packages:

| Driver error | Exact mapped sentinel |
| --- | --- |
| `sql.ErrNoRows` | `store.ErrNotFound` |
| Unique/primary-key violation | `store.ErrAlreadyExists` |
| Other integrity constraint | `store.ErrConstraint` |
| Serialization / deadlock / lock contention | `store.ErrSerialization` / `ErrDeadlock` / `ErrContention` |
| Bad connection / DB unavailable | `store.ErrUnavailable` |
| Read-only transaction/server | `store.ErrReadOnly` |
| Verified deadline/statement timeout | `store.ErrTimeout` |

`Update.Exec` and `Delete.Exec` do **not** convert "no rows affected" into `ErrNotFound` — callers must inspect `sql.Result` for that.

`SelectQuery.Count` reports complete logical projection-row cardinality. Credo
removes root ORDER/LIMIT/OFFSET/FOR state and counts a universal outer
`_credo_count_source` derived table. A normal projection counts its rows; an
ungrouped aggregate normally counts as one result row; `Distinct` counts
selected projection tuples; `GroupExpr` counts groups; and `GroupExpr` +
`Having` counts the groups left after the filter. Behavioral and SQL conformance
tests pin this Credo wrapper. Standalone `Having` and a direct compound root
introduced through `Apply` (`UNION`/`INTERSECT`/`EXCEPT`, including their
variants) return `sqldb.ErrUnsupportedCountQuery` before database I/O. Put the
compound source behind an outer derived table/CTE when it must be counted.

MySQL requires unique derived-table output names. Credo renders the post-hook
count source once and lets the server apply its real naming and `sql_mode`
rules. Wildcards, implicit aliases, and unaliased expressions are accepted when
their server-derived output names are unique. If logical COUNT returns
`ER_DUP_FIELDNAME` (1060 / SQLSTATE `42S21`), Count/Page wraps
`sqldb.ErrUnsupportedCountQuery` after I/O while preserving the driver cause;
use explicit unique aliases to resolve the collision. The wrapper is local to
logical COUNT, so raw and other non-count 1060 errors pass through unchanged.
Real MySQL tests pin normal mode and `NO_BACKSLASH_ESCAPES`. See MySQL's
[derived-table contract](https://dev.mysql.com/doc/mysql/en/derived-tables.html).

Relation callbacks run during Bun SQL rendering. The count source is rendered
once, then its post-render state is validated and those exact bytes execute.
Predicates/projections are allowed; replacing the model, reintroducing root
ORDER/LIMIT/OFFSET/FOR, or adding standalone `Having`/a compound root fails
before I/O with `sqldb.ErrUnsupportedCountQuery`.

Logical Count runs the model SELECT hook lifecycle on its private source:
`BeforeSelect`, `BeforeAppendModel`, then `AfterSelect` after a successful query.
If Page proceeds to its data statement, normal Bun scanning runs the same
lifecycle a second time. Filters or projections added by hooks therefore affect
both `Total` and `Records`. The outer count keeps the model identity in
`QueryEvent.Model` for observability, but soft-delete policy remains on the inner
source and is not duplicated outside it. Count does not scan or mutate a bound
model, so successful `AfterSelect` observes its pre-count value. Hook logic must
be deterministic: transaction isolation cannot stabilize volatile SQL or an
application-side decision across COUNT and SELECT.

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

**Pagination terminal: `Page[T]`.** The third typed terminal runs COUNT + a LIMIT/OFFSET SELECT and assembles a ready `*pagination.Page[T]`, completing the result-shape naming (`One → T`, `All → []T`, `Page → *Page[T]`):

```go
page, err := db.Select().Where("active = ?", true).OrderExpr("id").Page[User](ctx, req)
```

`BindQuery` applies `PageRequest.Validate`, which retains the forgiving input policy: absent/zero/negative values receive defaults and `PerPage` is clamped to the configured cap. `Page` does not repeat that policy. It takes a value snapshot of `req` and strictly validates the snapshot before touching the database: nil, non-positive values, native `int` offset overflow, or values beyond Bun v1.2.18's signed-int32 LIMIT/OFFSET representation wrap `pagination.ErrInvalidPageRequest`. The terminal never mutates the caller's request, and it does not clamp a valid custom page size above `pagination.MaxPerPage` (for example, `PerPage: 100`). The query must be model-less; a bound model returns `sqldb.ErrTypedTerminalModel` before either statement executes. COUNT-shape validation also runs pre-I/O: standalone `Having` and direct compound roots return `sqldb.ErrUnsupportedCountQuery`. On zero rows SELECT is skipped and the page keeps the snapshot's page/per-page with a non-nil empty slice. COUNT and SELECT use separate internal execution snapshots and both join the ambient transaction, so the query receiver is never mutated.

`Page.Total` is the number of complete logical projection rows before ordering and the Page-owned LIMIT/OFFSET window, with the same ungrouped-aggregate/distinct/group/post-`Having` semantics as `Count`. Repositories must add a stable total order and a unique tie-breaker—for example `OrderExpr("created_at DESC, id DESC")`—so equal primary sort values cannot move nondeterministically between offset pages.

The two statements can drift under concurrent writes. `Page` does not open an implicit transaction, and a normal PostgreSQL/MySQL Read Committed transaction still takes statement-level snapshots. For PostgreSQL and InnoDB, callers that need one snapshot establish a read-only Repeatable Read transaction at the outermost boundary and pass its `txCtx` to `Page`:

```go
err := db.InTxWith(ctx, &sql.TxOptions{
    Isolation: sql.LevelRepeatableRead,
    ReadOnly:  true,
}, func(txCtx context.Context) error {
    var err error
    page, err = db.Select().
        Where("active = ?", true).
        OrderExpr("created_at DESC, id DESC").
        Page[User](txCtx, req)
    return err
})
```

PostgreSQL defaults to statement-snapshot Read Committed. InnoDB defaults to Repeatable Read and ordinary nonlocking reads share the first-read snapshot, but server configuration, storage engine, and locking reads can change that behavior, so portable code requests the level explicitly. SQLite keeps its first-read snapshot in a plain explicit `InTx`; WAL permits a concurrent writer while rollback-journal mode may serialize it, and shared-cache `read_uncommitted` is an exception. The pinned modernc SQLite driver does not reliably enforce the isolation/read-only fields in `sql.TxOptions`, so those options are not presented as SQLite guarantees. Non-default options belong on the outer transaction—nested use returns `ErrNestedTxOptions`. See the detailed database matrix and primary sources in [ADR-015](../adr/015-data-access.md).

`Page` answers with the queried type directly; for a model→DTO response run `Page[Model]` and map it with `Page.Map`, which carries the metadata over (`q.Page[Model](ctx, req)` then `modelPage.Map(func(m Model) DTO { return toDTO(m) })`). When the conversion itself can fail, map the records from `Page[Model]` with ordinary error handling and construct `pagination.NewPage(dtos, modelPage.Total, modelPage.Page, modelPage.PerPage)`. `NewPage` calculates `TotalPages` as quotient plus remainder, avoiding ceiling-division overflow near `math.MaxInt64`.

The universal count source evaluates the projection, which is necessary for exact aggregate and set-returning cardinality. A costly or volatile expression can consequently run once for COUNT and again for the page SELECT. There is deliberately no custom-count strategy on `Page`: advanced repositories share predicates with `ApplyQueryBuilder`, run a deliberately equivalent cheaper `Count` (or count an outer derived-table/CTE source), execute their data query, and call `pagination.NewPage`; a first-class abstraction waits for two real consumers. Likewise, `Page` never represents an unknown total. Total-free offset pagination uses the separate working name `Slice[T]` and gets its own design gate rather than changing `Page`/`Meta` JSON or navigation semantics.

**Cursor/keyset implementation is gated.** The accepted first shape is a
forward-only `CursorPage[T]`; `Slice[T]` remains only the working name for
total-free offset pagination. A cursor response has `per_page`, `has_next`, and nullable
`next_cursor`, never Page totals or a previous cursor, and its terminal performs
no COUNT. Ordering belongs to an immutable adapter spec with non-null immutable
keys and an explicit unique final tie-breaker. The terminal will reject an
existing root ORDER/LIMIT/OFFSET/lock and non-row-shaped queries rather than
silently composing incompatible state, then use a portable lexicographic
OR-of-AND predicate plus `per_page + 1`. Joins are also excluded initially:
they can multiply a root row and invalidate a root-id uniqueness assertion.
Only a model-less default full-model SELECT plus curated predicates is accepted;
custom projections/tables, raw `Apply`, and hook-capable models fail before I/O.
Top-level `WhereOr` also fails; OR filters must be enclosed in one `WhereGroup`
that joins the root with AND before the cursor predicate is appended.

The public HTTP token policy requires an explicit rotatable HMAC-SHA256 keyring;
there is no generated secret or implicit unsigned fallback. Signing binds the
endpoint/query, canonical order, normalized filters, and tenant/authorization
scope, but does not hide cursor values. Sensitive keys, encryption, backward
navigation, optional totals, and nullable ordering remain outside the first
delivery.

The cursor is not authorization. Every request re-applies the normal auth,
tenant, and filter predicates; signed scope binding only prevents replay under
a different query.

No symbols ship yet. The gate opens only with a concrete consumer, a fail-loud
answer for Bun model hooks that can mutate terminal-owned order/window state,
invalid-argument transport mapping, canonical wire-format vectors, and real
PostgreSQL/MySQL/SQLite SQL, mutation, and typed-value conformance. The terminal
must retain existing model-less, execution-snapshot, receiver-reuse, explicit
connection/ambient-transaction, and store-error-mapping invariants. The canonical reserved contract
and mutation semantics live in the [pagination spec](pagination.md#cursorkeyset-design-gate);
the decision rationale is in [ADR-015](../adr/015-data-access.md).

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

### 8 Implemented Guardrails

1. **TX inject: snapshot, don't mutate** — terminal methods copy the underlying query before applying the TX connection. `SelectQuery` uses an internal execution snapshot that preserves the explicit connection, builder error, `WherePK` fields, soft-delete flags, and model/relation state. `InsertQuery`, `UpdateQuery`, and `DeleteQuery` use a Go struct shallow copy (`copied := *q.raw`) since Bun does not provide `Clone()` on these types — this isolates the `conn` field without affecting shared slices; it suffices because Bun reads, never mutates, the builder while generating SQL. The original `q.raw` is never modified, so query builders can be reused safely — including executing the same builder inside a transaction and again after that transaction finished (pinned by the `Test*Exec_BuilderReusableAfterTxRollback` tests in `query_copy_test.go`). Write-query wrappers track `.Conn()` explicitly. Select reads the actual raw connection after `Conn`/`Apply`: a non-nil explicit connection wins, while clearing or replacing it with a connection-less query re-enables ambient TX injection.

2. **Public `Clone`: top-level builder fork, nested sharing** — `SelectQuery.Clone` patches the execution fields Bun v1.2.18 omits and otherwise retains Bun's clone semantics. It is not a recursive object-graph copy: bound destinations and nested CTE/relation query values may remain shared. Source and clone must not mutate or scan shared values concurrently.

3. **Apply: Bun-native ergonomics** — `Apply(fns ...func(*bun.XQuery) *bun.XQuery)` matches Bun's own varargs signature. Delegates directly via `q.raw.Apply(fns...)`. Nil functions are filtered out to prevent panics.

4. **Unwrap: builder-only escape** — `Unwrap()` returns the underlying `bun.*Query` for use as a subquery or parameter to other Bun methods. Calling terminal methods on the unwrapped query bypasses all Credo interceptors (TX injection, error mapping). Documented as advanced-only. `Apply` is the primary escape hatch (interceptors preserved).

5. **Raw terminal wrappers** — `DB` exposes `Exec`, `QueryRow`, and `Query` methods for raw SQL that go through the same TX inject and error mapping pipeline as query builder terminals. Without these, error mapping behavior would split between wrapped and raw queries.

```go
// Raw SQL with TX injection and error mapping.
func (db *DB) Exec(ctx context.Context, query string, args ...any) (sql.Result, error)
func (db *DB) QueryRow(ctx context.Context, dest any, query string, args ...any) error
func (db *DB) Query(ctx context.Context, dest any, query string, args ...any) error
```

6. **ApplyQueryBuilder: cross-type filter reuse** — `Apply` is typed per query type, so a WHERE predicate shared across read and write had to be duplicated three times. `ApplyQueryBuilder(fn func(bun.QueryBuilder) bun.QueryBuilder)` on `SelectQuery`/`UpdateQuery`/`DeleteQuery` surfaces Bun's shared `bun.QueryBuilder` so one predicate (tenant scope, soft-delete filter, ownership check) applies to all three. It is implemented as `q.raw = fn(q.raw.QueryBuilder()).Unwrap().(*bun.XQuery)`, mirroring Bun's own `ApplyQueryBuilder`. Conditions land on the proxied query, so terminal methods still apply TX injection and error mapping — interceptors are preserved, exactly like `Apply`. A nil fn is a no-op. The form is preferred over a raw `QueryBuilder()` accessor: it stays in the proxy fluent chain and contains the bun type inside a function boundary, whereas `QueryBuilder()` would break the chain and act as a second `Unwrap` (the interface carries `Unwrap() any`). `InsertQuery` is excluded — no WHERE clause.

7. **Limit/Offset narrowing fails before execution** — Bun v1.2.18 stores Select LIMIT/OFFSET in signed `int32` fields although its methods accept `int`. Credo's curated `SelectQuery.Limit` and `Offset` record `ErrInvalidLimitOffset` for an out-of-range value, so a terminal cannot silently execute a narrowed window. In-range zero/negative behavior is Bun's and remains unchanged; raw `Apply`/`Unwrap` paths deliberately retain the upstream contract.

8. **Count shape fails loud and logical rows stay aligned** — `Count` and `Page` count a Credo-owned outer `_credo_count_source` after removing root ORDER/LIMIT/OFFSET/FOR, so plain projections, ungrouped aggregates, distinct tuples, groups, and post-`Having` groups use one logical-row contract. Model SELECT hooks run on the private source and the outer `QueryEvent.Model` remains populated without duplicating soft-delete policy. Standalone `Having` and direct compound roots are rejected with `ErrUnsupportedCountQuery` before I/O. MySQL is the oracle for derived output names: logical-count 1060 is wrapped with the same sentinel after I/O and its cause is retained, while non-count 1060 passes through. `Apply` remains inside this guard because the proxy terminal inspects its resulting builder; executing a terminal through `Unwrap` bypasses it with the other proxy guarantees.

**Planned observability:** automatic tracing via a DB-level `bun.QueryHook` belongs to Phase 3.5 and is not installed by `Open()` today. Attaching it at the Bun DB boundary will eventually cover proxy queries, native `Client()`/`Conn()` builders, raw SQL, and migrations without duplicating instrumentation in terminal wrappers.

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

**Nested TX:** When `RunInTx` is called within an existing transaction for the same `DB`, Bun creates a `SAVEPOINT`. Bun's savepoint implementation cannot change isolation or read-only mode. A nil or zero-valued `TxOptions` is accepted; a nested non-default option returns `ErrNestedTxOptions` before the savepoint and callback rather than being silently ignored. Bun also stores the context used to create a savepoint and reuses it for `RELEASE` / `ROLLBACK TO SAVEPOINT`; Credo therefore controls a cancellation-detached savepoint context while callback queries keep the original context. Savepoint creation observes callback cancellation and the cleanup budget; the callback does not run if creation becomes uncertain. If a callback returns nil after cancellation, Credo rolls back and returns the context error. Creation, release/rollback, and fail-safe ambient abort each have a five-second default wait budget, configurable with `WithTxCleanupTimeout`; callback execution time is not counted. Before an uncertain savepoint operation starts its asynchronous ambient abort, Credo synchronously marks shared transaction state rollback-only. Every outer level checks that state before commit; a level that would otherwise commit returns `ErrTxRollbackOnly`, while an existing callback or context error remains the more specific rollback result. This prevents abort goroutine scheduling from creating a commit race, and further nested calls fail immediately without invoking their callback. Panic cleanup follows the same fail-safe path while re-raising the original panic value. A driver that ignores cancellation can retain its goroutine/connection until it returns, but the caller path is bounded and the transaction remains fail-closed.

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
// Unlock. Signature matches App.OnStart, so auto-run on start is:
//
//	db.RegisterMigrations(migrations)
//	app.OnStart(db.Migrate)
func (db *DB) Migrate(ctx context.Context) error
```

**Design points:**

- **Thin wrapper**: the `*migrate.Migrations` set is Bun's own type — populated via `Discover(fsys)` for SQL files (works with `embed.FS`) or `MustRegister` for Go migrations. Credo does not re-wrap it.
- **Mark-applied-on-success by default**: the wrapper passes `migrate.WithMarkAppliedOnSuccess(true)`. Bun's bare default records a migration _before_ running it, so a failed migration would be skipped as "applied" on the next start — wrong for unattended `OnStart` auto-run. With the wrapper default, a failed migration is retried on the next run. Users can pass `WithMarkAppliedOnSuccess(false)` through `RegisterMigrations` to restore Bun's behavior.
- **Lock semantics**: if another instance holds the lock (second replica starting concurrently), `Migrate` fails immediately rather than waiting; the failed instance can be restarted. Unlock runs under `context.WithoutCancel` so a cancelled ctx cannot leak the lock row; an unlock failure is joined into the returned error.
- **Seeding** is a plain migration file (e.g. `2_seed_plans.up.sql`) — no separate mechanism.
- **No CLI here**: `credo migrate:*` (Phase 5.1) is optional sugar over this wrapper. Rollback / status / file generation stay on Bun's migrator via the escape hatch: `migrate.NewMigrator(db.Client(), ms)`.
- **OnStart integration is signature compatibility**, not coupling: `sqldb` still imports only `credo/store`, never the root framework package.

### Error Mapping

The mapper receives the operation context and the `DB`'s driver family (derived
from the driver name, or from the configured Bun dialect for custom connectors).
SQLSTATE is the structured cross-driver path; when MySQL provides both a number
and a broad SQLSTATE class, its more specific number wins. Strict MySQL error
envelopes are parsed only for MySQL, and SQLite numeric codes only for SQLite
(`Code() int` for modernc, allowlisted `Code`/`ExtendedCode` fields for mattn).
Unmapped errors pass through with exact identity. Loose message matching is
forbidden because a scanner/query hook/domain error may legitimately contain
text such as “duplicate key validation”.

| Source | Codes / condition | Semantic result |
| --- | --- | --- |
| database/sql | `sql.ErrNoRows` | `ErrNotFound` |
| PostgreSQL | `23505` | `ErrAlreadyExists` |
| PostgreSQL | other class `23` | `ErrConstraint` |
| PostgreSQL | `40001` / `40P01` / `55P03` | `ErrSerialization` / `ErrDeadlock` / `ErrContention` |
| PostgreSQL | class `08`, `57P01-3`, `53300` | `ErrUnavailable` |
| MySQL | 1022/1062 | `ErrAlreadyExists` |
| MySQL | 1048, 1216/1217, 1451/1452, 3819 | `ErrConstraint` |
| MySQL | 1213 / 1205 or 3572 | `ErrDeadlock` / `ErrContention` |
| MySQL | 1792/1836 | `ErrReadOnly` |
| SQLite | UNIQUE/PRIMARYKEY/ROWID extended codes | `ErrAlreadyExists` |
| SQLite | other constraint codes | `ErrConstraint` |
| SQLite | BUSY_SNAPSHOT | `ErrSerialization` |
| SQLite | BUSY/LOCKED (including extended codes) | `ErrContention` |
| generic | `context.DeadlineExceeded` | `ErrTimeout` |
| generic | `driver.ErrBadConn`, `sql.ErrConnDone` | `ErrUnavailable` |

PostgreSQL `57014` is `query_canceled`, not intrinsically timeout. A canceled
context is returned as cancellation; a deadline maps to timeout; a live context
maps only a structured driver error that explicitly identifies statement
timeout. MySQL 1290 is deliberately not treated as read-only because it is a
generic option-prevents-statement code. Each mapped error is `*store.Error` and
preserves the original cause plus driver `Code`. Driver-family-specific
classifier registration and a public normalizer remain deferred until a real
custom-driver consumer appears.

### Client Escape Hatch

```go
// Client returns the underlying *bun.DB.
// Use for raw SQL, model registration, advanced migration operations,
// and any Bun feature not covered by the proxy layer.
db.Client() *bun.DB
```

**Warning**: queries executed via the returned `*bun.DB` bypass the proxy interceptors. There is no automatic TX injection from context (so `InTx` / `RunInTx` will not affect them) and no error mapping to `store.Err*` sentinels. Use `DB.Conn(ctx)` for a native Bun operation that must join the ambient transaction. Reserve `Client()` for model registration, work intentionally tied to the base pool, and migration operations beyond `Migrate` (rollback, status, file generation); use the proxy layer for normal repository code.

Credo deliberately does not install a Bun `ConnResolver` in this phase. Bun has one owned resolver slot, closes it from `bun.DB.Close`, and uses it for query builders but not direct `Client().ExecContext`, `QueryContext`, `QueryRowContext`, or `BeginTx`. Moreover, current proxy terminals bind an explicit connection, which takes priority over a resolver. Making `Client()` appear automatically transaction-aware would therefore be partial and would pre-empt future read-replica resolver composition. The explicit `DB.Conn(ctx)` boundary is complete and visible; terminal-level injection remains the default. Revisit a composite resolver only when automatic native-builder routing or replicas provide a concrete requirement.

---

## File Layout

```text
fault/
├── doc.go              ← package contract and transport-neutral rationale
└── kind.go             ← Kind, Provider, KindOf

store/
├── doc.go              ← package documentation
├── errors.go           ← structured Error, semantic sentinels, compatibility aliases
├── lifecycle.go        ← Lifecycle + optional LifecycleIdentityProvider
├── health.go           ← Health, HealthStatus
├── registry.go         ← read-only Registry + private name/type/lifecycle reservations
├── register.go         ← Register[R], ownership/name/ping options, atomic publication
├── tx.go               ← TxScope[T], ErrTxMissing, scoped WithTx/GetTx/RequireTx/Conn; deprecated unscoped compatibility helpers
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
├── query_select.go     ← SelectQuery proxy (incl. One/All/Page typed terminals)
├── query_errors.go     ← typed-terminal, Limit/Offset, and Page precondition errors
├── query_insert.go     ← InsertQuery proxy
├── query_update.go     ← UpdateQuery proxy
├── query_delete.go     ← DeleteQuery proxy
├── tx.go               ← RunInTx, RunInTxWith + method forms InTx, InTxWith
├── migrate.go          ← RegisterMigrations, Migrate (bun/migrate wrapper)
├── errors.go           ← context/family-aware driver → semantic store mapper
├── options.go          ← Option, WithDialect, WithConnector, WithTxCleanupTimeout
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
    return err // unique violation → store.ErrAlreadyExists (ErrDuplicate compatible)
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

    // Embedding promotes *sqldb.DB's Lifecycle and ResourceIdentity methods,
    // so each wrapper identifies its distinct underlying DB. Do not pass a
    // second lifecycle handle.
    store.Register[PrimaryDB](app, PrimaryDB{primaryDB},
        store.WithName("primary"))
    store.Register[AnalyticsDB](app, AnalyticsDB{analyticsDB},
        store.WithName("analytics"))
}
```

---

## Design Decisions

1. **Single ORM (Bun) over multi-ORM adapters** — deep integration (query proxies, error mapping, pagination) is more valuable than shallow generic interfaces. Escape hatch for other ORMs via raw DI.

2. **Semantic fault contract over transport methods** — the stdlib-only `fault` leaf package breaks the dependency cycle. Store errors expose a `Kind`; root HTTP and future gRPC policy consume the same provider. `HTTPStatus()` remains only as a deprecated compatibility seam.

3. **Context-based TX over explicit TX passing** — a per-connection `TxScope[T]` is opt-in and keeps repository method signatures clean; `store/sqldb` owns it behind its proxies and `DB.Conn(ctx)`. Trade-off: TX participation is less visible in function signatures. See [ADR-015](../adr/015-data-access.md) for rationale.

4. **Query builder proxies over raw pass-through** — proxies inject TX and map errors; DB-level query-hook tracing is planned for Phase 3.5. `Client()` and `Conn(ctx)` escape hatches prevent the proxy from becoming a bottleneck.

5. **Separate submodule for store/sqldb/** — Bun dependency is opt-in. Applications not using SQL don't pull in Bun.

6. **Config over DSN-only** — structured `Config` enables validation, env var mapping, and consistent documentation. `DSN` field is an override for advanced use cases.

7. **Fail-fast at startup** — `Register[R]` rejects predictable local
   lifecycle, name/type/resource-identity, frozen-container, and duplicate
   conflicts before network I/O, then pings the connection. Identity defaults
   to the top-level Lifecycle value; wrappers explicitly forward another
   resource via `LifecycleIdentityProvider`. `CanProvideValue` is only a
   point-in-time preflight; final DI publication remains authoritative.

8. **Explicit shutdown owner** — a directly registered `Lifecycle` value is
   framework-owned after success and DI is its sole framework shutdown owner.
   If the live deadline reaches the entry, DI makes at most one attempt per
   teardown; an entry skipped after deadline exhaustion gets no attempt. A
   separate lifecycle handle requires the explicit caller-owned opt-out. Every
   failed registration leaves ownership with the caller. The Registry
   aggregates health only and never shuts resources down.

9. **Health returns one struct** — status, latency, pool stats, and an optional
   typed `Cause` travel as one snapshot. The additive `Cause error` field keeps
   the `Lifecycle.Health(ctx) Health` interface stable while avoiding
   free-form `Details` parsing. It is diagnostic-only and excluded from JSON.

10. **Wrapper types for multi-DB** — applications define distinct struct types (`PrimaryDB`, `AnalyticsDB`). Compile-time DI safety with zero string keys.

11. **Structured, family-aware error mapping** — SQLSTATE is primary; MySQL's strict numeric envelope and SQLite's numeric code are interpreted only for that configured family. Loose arbitrary-message matching is prohibited. Unmapped errors pass through with exact identity.

12. **Private, commit-after-publish Registry mutation** — callers cannot add
    health-only entries. Register reserves name, type, and declared resource
    identity privately, keeps pending entries invisible, and commits the entry
    only when Ping and protected DI publication both succeed. The successful
    store binding and Registry binding reject later `Replace` calls.

13. **Store-scoped identity ledger, not a general resource registry** — the
    duplicate-resource guarantee covers only `store.Register` entries. Raw
    `app.Provide`, `app.ProvideFactory`, `app.ProvideValue`,
    `app.ProvideProtectedValue`, or `app.Replace` publication of the same
    lifecycle under another type is unsupported. A cross-infrastructure
    registry remains deferred until a second concrete consumer exists.

---

## Test Requirements

### store/ (core)

- Every semantic sentinel works through `errors.Is`, `KindOf`, and wrapping/join traversal
- `ErrDuplicate` aliases `ErrAlreadyExists`; exact constraint/concurrency sentinels retain the legacy `ErrConflict` match
- Structured errors preserve cause/code/metadata; constraint metadata does not enter default HTTP responses
- Constraint/duplicate errors are not transient; serialization/deadlock/contention are transient without promising retry safety
- Deprecated `HTTPStatus()` compatibility remains stable while root semantic policy takes precedence
- `WithTx` / `GetTx` round-trip
- `TxScope[T]` round-trips a concrete value through an interface-typed scope without fallback
- `WithTx` panics for nil pointers and typed-nil interface values before storing them
- `WithTxInScope` / `GetTxInScope` isolate same-type transactions by scope
- `(*TxScope[T]).WithTx` / `GetTx` / `RequireTx` / `Conn` round-trip identically to the matching `*InScope` free functions and isolate distinct scopes/types
- `RequireTx` returns `ErrTxMissing` outside a transaction and never falls back
- Nested contexts shadow a scoped transaction without mutating the parent context
- `Conn` returns TX from context when present, fallback otherwise
- `ConnInScope` returns scoped TX from context when present, fallback otherwise
- `Registry.HealthAll` returns health for all entries
- Registry exposes no public mutation method; only successful `Register`
  commits entries
- Pending name/type/resource-identity reservations are invisible to
  `HealthAll` and readiness, reject concurrent duplicates, and are released on
  every failure
- Registry-backed readiness probes are stable across requests, run in parallel,
  enforce per-check deadlines, isolate panics, and coalesce overlapping calls
- `Health.Clone` preserves typed cause identity while defensively cloning the
  top-level details map
- Typed-nil values/lifecycle handles and typed-nil pre-provided Registries are
  rejected before ping or health execution
- Store failure causes are logged and masked by default; `ExposeErrors` is the
  only response opt-in
- `Register` preflights local DI/name/lifecycle conflicts before Ping, then
  publishes DI before committing the Registry entry
- Direct Lifecycle values are framework-owned only after success; DI is the
  sole framework shutdown owner and, per teardown, attempts Shutdown at most
  once when its live deadline reaches the registration (or zero times when the
  deadline expires first)
- `WithLifecycle` alone fails; pairing it with
  `WithCallerOwnedLifecycle` succeeds without framework shutdown
- Lifecycle values with explicit lifecycle/ownership options and
  Shutdowner-only values with separate lifecycle handles fail before Ping
- Every failed registration, including Ping and final DI publication failure,
  leaves ownership with the caller
- Concurrent same-name and same-type registrations have one internally
  consistent winner and no leaked Registry entry
- Default identity is the top-level Lifecycle value; pointer-backed values are
  accepted, stable comparable values/tokens work, and nil/non-comparable/
  non-reflexive identities fail before Ping
- `LifecycleIdentityProvider` explicitly maps semantic/named wrappers to an
  underlying pointer/token; no struct fields are scanned, while embedding
  `*sqldb.DB` promotes its provider method normally
- A pointer-backed composite Lifecycle is valid as its own default identity;
  contained Lifecycle fields are not inferred or rejected
- Repeated identity tokens across concrete/interface views, explicit wrappers,
  and mixed ownership are rejected inside the Register ledger; interface
  access uses `Alias[I, T]` instead
- Raw `app.Provide`, `app.ProvideFactory`, `app.ProvideValue`,
  `app.ProvideProtectedValue`, or `app.Replace` publication of the same
  lifecycle under another type is documented as unsupported; caller-owned
  handles are not also registered as Shutdowners
- Valid pre-provided Registry instances remain the resolved/readiness instance
- Successful store and validated/adopted Registry bindings reject
  `App.Replace`; invalid nil/failing Registry bindings remain replaceable for
  repair, and ordinary non-protected DI bindings remain replaceable
- Registry adoption uses expected-value compare-and-protect, so a replacement
  raced after validation is rejected without protecting the wrong instance
- `WithName` sets a custom name; explicit empty, padded, control-character,
  and reserved `credo.` names fail before Ping; omitted names use the stable
  pointer-unwrapped type default
- `WithPingTimeout` overrides default timeout

### store/sqldb/ (Bun wrapper)

- `Open` creates a working connection
- `Open` returns error on invalid Config
- Pool config applies finite `MaxOpen`, nil/zero/positive `MaxIdle`,
  `MaxLifetime`, and `MaxIdleTime` exactly; negative values and explicit
  `MaxIdle > MaxOpen` fail before connection use
- `Client()` returns `*bun.DB`
- `Stats()` returns the underlying complete `sql.DBStats` snapshot
- `Ping` verifies connection
- `Health` returns UP with latency, current pool counts, and cumulative
  wait/idle/lifetime closure counters
- An effective unlimited maximum reports `sqldb.pool.max_open_unlimited`;
  canonical successful registration logs it once, while failed registration
  does not
- `Health` returns DOWN with a typed `Cause` when the connection is dead; it
  does not copy the cause string into `Details["error"]`
- `Shutdown` closes connection
- Query proxies (`Select`, `Insert`, `Update`, `Delete`):
  - Execute queries correctly
  - Reject more than one optional model argument through the terminal builder error without executing
  - Reject curated Select `Limit`/`Offset` values outside Bun's signed-int32 range with `ErrInvalidLimitOffset` before execution, while preserving in-range Bun semantics
  - Inject TX from context
  - Map errors to structured semantic store values
  - `ApplyQueryBuilder` reuses one predicate across Select/Update/Delete, preserves error mapping, treats a nil fn as a no-op, and reaches `WhereGroup` (not in the curated set)
  - Select execution snapshots preserve explicit connection, builder errors, `WherePK`, soft-delete flags, and model/relation state
  - Public Select `Clone` preserves the documented top-level execution fields and documents nested destination/CTE/relation sharing
- `One[T]`, `All[T]`, and `Page[T]` reject models bound through `Select`, `Model`, or `Apply` with `sqldb.ErrTypedTerminalModel` before execution
- `Count` and `Page` use `_credo_count_source` to align plain projection, ungrouped aggregate, distinct tuple, group, and post-`Having` cardinality; root ORDER/LIMIT/OFFSET/FOR does not constrain the total
- Logical Count runs `BeforeSelect`, `BeforeAppendModel`, and successful-query `AfterSelect` on the private source; Page runs the hooks again for its data statement, keeps `QueryEvent.Model`, and applies soft-delete policy only inside the source
- Standalone `Having` and direct UNION/UNION ALL/INTERSECT/EXCEPT roots return `ErrUnsupportedCountQuery` before database I/O; an outer derived-table source remains countable
- Real MySQL normal and `NO_BACKSLASH_ESCAPES` sessions pin Count/Page 1060 wrapping with the driver cause, bind-secret-free messages, accepted wildcard/implicit-expression projections, and unsentinelled raw 1060; relation callback single-render behavior remains a local structural test
- Model-bound `Relation(...).Scan()` loads relations without losing relation callbacks or state
- `RunInTx` commits on nil return
- `RunInTxWith` commits on nil return
- `RunInTx` returns the exact callback error after successful rollback and never reclassifies domain text as a SQL error
- Callback + rollback failure preserves both causes and maps only the rollback side
- `RunInTx` re-panics the exact panic value after attempting rollback
- A nil callback returns `ErrNilTxCallback` before BEGIN
- Nested calls use savepoints; zero options work and non-default nested options return `ErrNestedTxOptions` before executing
- Nested callback cancellation rolls back inner work for error/nil/panic exits while leaving a healthy outer transaction usable
- Savepoint rollback/release failures attempt an ambient SQL transaction abort and preserve operation causes
- Nested savepoint creation/cleanup and ambient abort respect the default/custom `WithTxCleanupTimeout` wait budget without counting callback duration
- An uncertain nested operation synchronously marks shared state rollback-only; an outer callback that swallows the nested error still returns `ErrTxRollbackOnly` instead of committing
- `RunInTx` stores TX in context with per-DB scoping
- `DB.Conn(ctx)` and `DB.RequireTx(ctx)` expose the correct native Bun connection, including same-type multi-DB isolation
- `InTx` / `InTxWith` method forms commit on nil, roll back on error
- `Migrate` applies pending migrations; re-run is a no-op
- `Migrate` retries a failed migration on the next run (mark-on-success) and releases the advisory lock after failure
- `Migrate` discovers SQL migrations from `embed.FS` (incl. a seed file)
- `Migrate` without registration returns an error
- `RegisterMigrations` panics on nil set or double registration
- `Migrate` satisfies the `App.OnStart` hook signature (compile-time check)
- PostgreSQL, MySQL, and SQLite classifier tables preserve exact kinds, driver code, cause, and transient metadata
- Context cancellation, deadline, PostgreSQL 57014, and SQLite interrupt are distinguished
- `driver.ErrBadConn`, SQL connection closure, and SQLSTATE class 08 map unavailable
- Arbitrary scanner/hook/domain messages containing duplicate/foreign-key/read-only text pass through unchanged
- Real SQLite unique, constraint, and lock-contention integrations match the documented kinds
- `Config.DSN` override takes precedence
- `Page[T]` snapshots its `PageRequest`, rejects nil/non-positive/native-overflow/Bun-int32-range violations with `pagination.ErrInvalidPageRequest` before COUNT, never normalizes or mutates the caller's request, preserves valid custom page sizes above 50, skips SELECT on zero rows with a non-nil empty slice, and runs COUNT + SELECT on the ambient transaction
- SQLite WAL conformance deterministically demonstrates transaction-free COUNT/SELECT drift and one stable first-read snapshot inside explicit `InTx`
