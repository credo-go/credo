# ADR-015: Data Access

**Status:** Accepted **Date:** 2026-03-04 **Depends on:** ADR-004, ADR-005

## Context

Credo needs a data access layer that lets applications work with SQL databases without coupling the framework core to any ORM. The design must:

- Keep the core package (`store/`) free of external dependencies
- Provide universal error types that the framework error handler can map to HTTP status codes (404, 409, 504, etc.)
- Support context-based transaction propagation as an opt-in convenience while preserving the ability to use the ORM's native transaction API
- Wrap a single ORM (Bun) behind a proxy layer that owns lifecycle, enriches queries, and provides an escape hatch to the raw client
- Integrate with lifecycle management, health checks, and DI registration
- Support multi-database setups via DI wrapper types

### Why Single-ORM (Bun)?

Maintaining adapters for multiple ORMs multiplies the wrapper surface area and testing cost. Framework-level features (query proxies, error mapping, pagination helpers) are impossible to build on a generic ORM-agnostic interface — every adapter would need its own implementation, resulting in near-identical duplicated code.

Bun was chosen because it maps closely to SQL (no magic struct-scan), supports raw SQL alongside the query builder, and has a clean `bun.IDB` interface that unifies `*bun.DB` and `bun.Tx`.

Applications that need a different ORM (GORM, sqlx, sqlc) can register their client directly in DI without using `store/sqldb/` — the `store/` contracts (errors, health, registry) still apply.

## Decision

### Two-Package Split: store/ + store/sqldb/

- **`store/`** — universal contracts (errors, `Lifecycle`, `Health`, `Registry`, `Register[R]`, TX context helpers). Zero external dependencies. Part of the main `github.com/credo-go/credo` module.
- **`store/sqldb/`** — Bun SQL wrapper (`DB`, `Config`, query builders, `RunInTx`, error mapping). Separate Go submodule (`github.com/credo-go/credo/store/sqldb`) so the Bun dependency is opt-in.

### Universal Error Types

Store errors are typed values implementing `HTTPStatus() int`:

```go
var (
    ErrNotFound  error = &statusError{"store: record not found", 404}
    ErrDuplicate error = &statusError{"store: duplicate record", 409}
    ErrConflict  error = &statusError{"store: conflict", 409}
    ErrTimeout   error = &statusError{"store: timeout", 504}
    ErrReadOnly  error = &statusError{"store: read-only", 503}
)
```

The default error handler detects `HTTPStatus()` via type-safe error-chain matching (`errors.AsType`) — it does not import `store/`, avoiding the circular dependency (`store → credo` for DI registration, `credo → store` would be a cycle). `errors.As` unwraps automatically, so `fmt.Errorf("create user: %w", store.ErrNotFound)` still maps to 404.

Adapters translate ORM/driver errors to these sentinels. Application code uses `errors.Is(err, store.ErrNotFound)` — ORM-agnostic.

### Context-Based TX

- `store.WithTx[T](ctx, tx)` / `store.GetTx[T](ctx)` — store/retrieve a TX handle in context for simple type-keyed flows.
- `store.NewTxScope()` + `store.WithTxInScope[T](ctx, scope, tx)` / `store.ConnInScope[T](ctx, scope, fallback)` — isolate transactions for multiple logical connections that share the same Go type.
- `sqldb.RunInTx(ctx, db, fn)` — start TX, store in context, execute callback, commit/rollback. Nested calls use savepoints.

This is an **opt-in convenience**. Repositories that don't call `Conn[T]`/`ConnInScope[T]` are unaffected. The native Bun TX API (`db.RunInTx`) also works directly.

### 3-Tier Bun Wrapping

The `sqldb.DB` wrapper applies three strategies to Bun's API:

1. **Own** — lifecycle methods (`Open`, `Close`, `Ping`, `Health`). The wrapper manages the connection pool.
2. **Enrich** — query builder proxies (`Select`, `Insert`, `Update`, `Delete`) that inject TX from context, apply query hooks for tracing, and map errors to `store.Err*` sentinels.
3. **Passthrough** — `Client() *bun.DB` escape hatch for features not covered by the proxy layer (raw SQL, migrations, model registration).

### Registration

`store.Register[R](app, value, opts...)` is the unified registration function. If `value` implements `Lifecycle`, it is used directly; otherwise `WithLifecycle(lc)` must be provided:

1. **Ping** — fail-fast health check at startup
2. **Ensure Registry** — resolve or create `Registry` in DI
3. **Track** — add `Lifecycle` handle for ping and health aggregation
4. **DI register** — register as type `R` via `credo.ProvideValue[R]`

Closing has a single owner: the DI container shuts down registered values implementing `credo.Shutdowner` in reverse registration order. The `Registry` never closes connections — this removes the historical double shutdown (DI + Registry both closing the same connection).

### Bun-Only

The framework ships a single SQL adapter (Bun). Other ORMs work via raw DI registration (`credo.ProvideValue[*gorm.DB](app, db)`). The `store/` contracts (errors, health, registry) are ORM-agnostic and can be used with any backend.

## Consequences

**Positive:**

- Universal errors with `HTTPStatus()` interface enable error→HTTP mapping without circular imports
- Single ORM focus allows deep integration (query proxies, error mapping, pagination helpers) instead of shallow generic interfaces
- Context-based TX is opt-in — repositories that don't need TX are simpler
- `store/` contracts remain ORM-agnostic — custom adapters possible
- Separate submodule keeps Bun dependency out of core
- `Client()` escape hatch prevents the wrapper from becoming a bottleneck
- Registry provides startup ping and health aggregation; shutdown has a single owner (the DI container), so connections close exactly once

**Negative:**

- Applications using GORM lose the first-class adapter — must use raw DI
- Context-based TX uses `context.WithValue` (type-safe via generics, but invisible in function signatures compared to explicit TX passing)
- Query builder proxies add a thin layer over Bun's API that must be kept in sync with Bun releases
- Bun version is pinned by `store/sqldb/` module — coordinated upgrades
- Error mapping may not cover all driver-specific error codes

## SelectQuery Curated Set

**SelectQuery curated set expanded** with `Join`, `JoinOn`, `JoinOnOr`, `TableExpr`, `ColumnExpr`, and `ExcludeColumn`.

**Rationale**: the original curated set forced every non-model JOIN query (reporting, auth, analytics) through the `Apply` escape hatch, turning an "advanced" path into the normal one. Adding these six methods eliminates that friction without API breakage — all return `*SelectQuery` (fluent), and interceptors (TX injection, error mapping) are preserved because terminal methods are unchanged.

**Client() escape hatch documented**: `Client()` now carries an explicit GoDoc warning that queries via `*bun.DB` bypass TX injection and error mapping. Spec and guide updated accordingly.

## ApplyQueryBuilder

**`ApplyQueryBuilder` added** to `SelectQuery`, `UpdateQuery`, and `DeleteQuery`:

```go
func (q *SelectQuery) ApplyQueryBuilder(fn func(bun.QueryBuilder) bun.QueryBuilder) *SelectQuery
```

**Rationale**: the typed `Apply` escape hatch is per-query-type (`func(*bun.SelectQuery) *bun.SelectQuery` cannot be applied to an update or delete). A predicate shared across reads and writes — soft-delete filters, account scoping, ownership checks, and authorization scopes — therefore had to be duplicated once per query type. Bun's native `QueryBuilder()` exposes a builder-only interface (`Where`, `WhereOr`, `WhereGroup`, `WherePK`, `WhereDeleted`, `WhereAllWithDeleted`) common to select/update/delete; `ApplyQueryBuilder` surfaces it through the proxy so one `func(bun.QueryBuilder) bun.QueryBuilder` predicate applies to all three. As a bonus it unlocks `WhereGroup`, which the curated proxy set does not expose directly.

**Form — `ApplyQueryBuilder(fn)`, not a raw `QueryBuilder()` accessor**: exposing `QueryBuilder()` directly would return a bun interface that breaks the proxy fluent chain and is essentially a second `Unwrap` (`bun.QueryBuilder` carries `Unwrap() any`). `ApplyQueryBuilder(fn)` instead returns the proxy type (fluent, like `Apply`/`Conn`), contains the bun type inside a function boundary, and mirrors Bun's own `ApplyQueryBuilder`. Conditions added through the builder land on the proxied query, so terminal methods still apply TX injection and error mapping — interceptors are preserved, verified against bun v1.2.18 (`selectQueryBuilder` wraps the same `*bun.SelectQuery` pointer; Where-family methods mutate in place). The builder's `Unwrap() any` remains a terminal-bypass escape — the same caveat already documented for `Unwrap()`, and no easier to misuse than today's `Apply`, which hands out the concrete query directly.

**Bun type leakage** is the one real cost: a shared predicate's signature is `func(bun.QueryBuilder) bun.QueryBuilder`, importing `bun` into repository code. This sits at the same documented escape-hatch tier as `Apply`/`Unwrap`/`Conn`/`Client` and is positioned as an escape hatch, not the default path. If bun coupling later proves painful, the follow-up is a Credo-owned `WhereScope` interface (Where/WhereOr/WherePK only, no `Unwrap`) — deferred until real pain appears (it would reinvent bun's interface and cannot cheaply express `WhereGroup` recursion).

**Scope**: select/update/delete only. `InsertQuery` is excluded — it has no WHERE clause, matching Bun's own `QueryBuilder` interface assertions. No API breakage; additive only, all three return the proxy type (fluent), and a nil fn is a no-op.

## Migrations and TX Ergonomics

**Method-form TX sugar added**: `(*DB).InTx(ctx, fn)` and `(*DB).InTxWith(ctx, opts, fn)` delegate to `RunInTx` / `RunInTxWith`. Rationale: handler-side ergonomics (`db.InTx(ctx.Context(), fn)`) and discoverability — the operation lives on the value the developer already holds. The distinct name also avoids signature confusion with Bun's native `(*bun.DB).RunInTx(ctx, opts, fn(ctx, tx))` reachable via `Client()`.

**Migration wrapper added** as a thin wrapper over Bun's migration engine:

```go
func (db *DB) RegisterMigrations(m *migrate.Migrations, opts ...migrate.MigratorOption)
func (db *DB) Migrate(ctx context.Context) error
```

- `bun/migrate` is part of the already-pinned Bun module — no new dependency and no second migration engine.
- The `*migrate.Migrations` set is Bun's own type (`Discover(fsys)` for SQL files incl. `embed.FS`; `MustRegister` for Go migrations) — Credo does not re-wrap it.
- `Migrate` = Init (bookkeeping tables, `IF NOT EXISTS`) → Lock (table-based advisory lock, fail-fast if another replica is migrating) → run pending → Unlock (under `context.WithoutCancel` so a cancelled ctx cannot leak the lock row; an unlock failure is joined into the returned error). Errors are mapped to `store.Err*` where applicable.
- **Lifecycle integration is signature compatibility, not coupling**: `Migrate` matches the `App.OnStart` hook signature, so opt-in auto-run is `app.OnStart(db.Migrate)`. `sqldb` still imports only `credo/store`, never the root framework package.
- **Deliberate divergence from Bun's default**: the wrapper passes `migrate.WithMarkAppliedOnSuccess(true)`, so a failed migration stays unapplied and is retried on the next start. Bun's bare default (record before running) would mark the failure as applied and silently skip it on restart — wrong for unattended on-start runs. Users can restore Bun's behavior through `RegisterMigrations`'s variadic options.
- Registration misuse (nil set, double registration) panics per the panic-vs-error policy; `Migrate` itself only returns errors.
- Seeding is a plain migration file (no separate mechanism). Rollback, status, and file generation stay on Bun's migrator via the escape hatch: `migrate.NewMigrator(db.Client(), migrations)`.
