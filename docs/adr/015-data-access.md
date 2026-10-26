# ADR-015: Data Access

**Status:** Accepted; cursor/keyset pagination deferred **Date:** 2026-03-04 **Depends on:** ADR-004, ADR-009, ADR-024 **Related:** ADR-014, ADR-016 **Specification:** [Store spec](../specs/store.md), [Pagination spec](../specs/pagination.md) **Guide:** [Data access guide](../guides/data-access.md)

## Context

Credo needs a data access layer that lets applications work with SQL databases without coupling the framework core to any ORM. The design must:

- Keep the core package (`store/`) free of external dependencies
- Provide universal semantic error types that HTTP and future gRPC policy can map independently
- Support context-based transaction propagation as an opt-in convenience while preserving the ability to use the ORM's native transaction API
- Wrap a single ORM (Bun) behind a proxy layer that owns lifecycle, enriches queries, and provides an escape hatch to the raw client
- Integrate with lifecycle management, health checks, and DI registration
- Support multi-database setups via DI wrapper types
- Fail loud wherever a silent fallback would change behavior: a transaction read back under a different type must not fall back to the pool, transaction options a savepoint cannot honour must not be dropped, a query shape the total cannot describe must not produce a wrong count, and a driver message that merely contains "duplicate key" must not be classified as a duplicate

### Why Single-ORM (Bun)?

Maintaining adapters for multiple ORMs multiplies the wrapper surface area and testing cost. Framework-level features (query proxies, error mapping, pagination helpers) are impossible to build on a generic ORM-agnostic interface — every adapter would need its own implementation, resulting in near-identical duplicated code.

Bun was chosen because it maps closely to SQL (no magic struct-scan), supports raw SQL alongside the query builder, and has a clean `bun.IDB` interface that unifies `*bun.DB` and `bun.Tx`.

Applications that need a different ORM (GORM, sqlx, sqlc) register their client directly in DI (`app.ProvideValue[*gorm.DB](db)`) without using `store/sqldb/`; the `store/` contracts (errors, health, `Lifecycle` registration) are ORM-agnostic and still apply.

## Decision

### Two-Package Split: store/ + store/sqldb/

- **`store/`** — universal contracts (errors, `Lifecycle`, `Health`, `Register[R]`, TX context helpers). Zero external dependencies. Part of the main `github.com/credo-go/credo` module.
- **`store/sqldb/`** — Bun SQL wrapper (`DB`, `Config`, query builders, `RunInTx`, error mapping). Separate Go submodule (`github.com/credo-go/credo/store/sqldb`) so the Bun dependency is opt-in.

### Semantic Error Types

The stdlib-only `fault` leaf package defines transport-neutral `Kind` values and a `Provider` interface. Both root HTTP policy and `store` import that leaf; root never imports the feature package, so `store → credo` registration does not create a cycle. `store.Kind` aliases `fault.Kind` for application-facing data-access code.

`store.Error` carries the kind together with structured diagnostics — operation, resource, constraint, driver code, a transient flag and the cause — that never reach the wire: the default error renderer serializes none of them. Exact sentinels distinguish not-found, already-exists, persistent constraint, serialization, deadlock, lock/busy contention, timeout, unavailable, and read-only conditions, so callers branch on the condition rather than on a coarse conflict. `ErrDuplicate` (an alias of `ErrAlreadyExists`) and the `ErrConflict` umbrella, which the constraint and concurrency sentinels match through `errors.Is`, are deprecated compatibility names; the exact `Kind` is the primary contract. The [store spec](../specs/store.md#semantic-errors) lists the struct, the sentinels and their default statuses.

`Transient` means only that the condition may clear. It never promises that replaying a statement, transaction callback, or externally visible operation is safe. Commit outcome is deliberately not inferred from a driver cause kind; an `OutcomeUnknown` model is deferred until operation-phase evidence can make it reliable.

Credo root maps `fault.Provider` after `*HTTPError` and before the legacy `HTTPStatus()` seam ([ADR-009](009-handler-and-error-handling.md)). This preserves service/domain override through `NewHTTPError(...).WithInternal(err)` while giving future gRPC policy the same semantic input. The store sentinels' `HTTPStatus()` is a deprecated compatibility bridge. Unknown kinds fail closed as 500.

**Classification reads structure, never text.** `sqldb` classifies with the configured driver family (the selected Bun dialect for a custom connector) and the operation context. SQLSTATE is the cross-driver path; when a MySQL error exposes both its number and a broad SQLSTATE class, the more specific number wins (1213/40001 is a deadlock, not a generic serialization failure). MySQL's strict `Error NNNN (STATE): ...` envelope is parsed only for MySQL, and SQLite's structured code only for SQLite. Loose substrings such as "duplicate key" or "read-only" are never inspected, so a scanner, query-hook or domain error that contains such text keeps its exact identity. PostgreSQL `57014` means query canceled, not timeout: caller cancellation stays cancellation, a caller deadline maps to `Timeout`, and with a live context only a structured driver error that explicitly says statement timeout does. MySQL 1290 is not classified as read-only because it is the generic option-prevents-statement code; the read-only transaction/server codes 1792 and 1836 are. Every mapped result is a `*store.Error` that preserves the cause and the driver code. A public classifier registry is deferred until a custom-driver consumer demonstrates the need. The code-level table is the store spec's [Error Mapping](../specs/store.md#error-mapping).

### Context-Based TX

- `store.NewTxScope[T]()` fixes the transaction contract at scope construction. Its `WithTx`, `GetTx`, `RequireTx`, and `Conn` methods all use that same `T`: a per-method `T` would let a concrete transaction be stored and read back through an interface type under a different context key, silently selecting the fallback connection.
- Each `sqldb.DB` owns a distinct `TxScope[bun.IDB]`, isolating multiple logical databases that share Bun's interface type.
- `sqldb.RunInTx(ctx, db, fn)` starts a transaction, stores it in that DB's private scope, executes the callback, and commits/rolls back. Nested calls use savepoints.
- `sqldb.DB.Conn(ctx)` exposes the active `bun.IDB` for transaction-aware native Bun operations; `RequireTx(ctx)` returns `store.ErrTxMissing` instead of falling back.

This is an **opt-in convenience**. Proxy users participate automatically when they pass the callback context. Custom adapters use their own typed scope. The unscoped `store.WithTx` / `GetTx` / `Conn` helpers are deprecated: type-only context keys cannot protect a producer/consumer concrete-interface mismatch or isolate same-type logical connections.

The method forms `db.InTx(ctx, fn)` and `db.InTxWith(ctx, opts, fn)` put the operation on the value the caller already holds; their distinct name avoids confusion with Bun's native `(*bun.DB).RunInTx(ctx, opts, fn(ctx, tx))`, reachable through `Client()`.

The transaction boundary fails loud and closed rather than guessing ([store spec](../specs/store.md#tx-management) for the mechanics and budgets):

- **Callback errors stay application values.** After a successful rollback `RunInTx` returns the callback's exact error without SQL error mapping: domain text such as "duplicate key validation" must not acquire `store.ErrAlreadyExists`.
- **No silent option loss.** Bun implements a nested transaction as a savepoint and ignores `sql.TxOptions` there, so non-default options on a nested call return `ErrNestedTxOptions`; the owner of the outer transaction chooses isolation and read-only mode.
- **Rollback-only instead of a commit race.** An uncertain savepoint operation synchronously marks the shared transaction state rollback-only before its bounded asynchronous abort, and every outer level checks that state before commit. Whether the transaction commits never depends on how the abort goroutine is scheduled, even when a nested error is swallowed.
- **Commit errors are outcome-ambiguous.** A commit failure does not prove that the transaction was not applied; its classification describes the driver cause, and callers must not turn it into a blind retry.

Rejected:

- **A `Session`/Unit-of-Work object** beside the typed ambient scope — deferred until an outbox or another transaction-bound multi-repository consumer demonstrates a boundary that `InTx` plus explicit `DB.Conn(ctx)` cannot express cleanly.
- **An ambient Bun `ConnResolver`** to make `Client()` transaction-aware — Bun v1.3.0 gives a resolver one owned slot and closes it from `bun.DB.Close`; explicit query connections win over it, while direct `Client().ExecContext`, `QueryContext`, `QueryRowContext`, and `BeginTx` bypass it. Credo's proxy terminals bind an explicit connection, so a resolver would make only part of `Client()` transaction-aware and would complicate read-replica composition and ownership. Terminal-level injection stays the default and `DB.Conn(ctx)` is the explicit native path. A composite order (`explicit query conn → active Credo TX → user/replica resolver → base DB`) is reconsidered only when automatic native-builder or replica routing is a demonstrated requirement.

### 3-Tier Bun Wrapping

The `sqldb.DB` wrapper applies three strategies to Bun's API:

1. **Own** — lifecycle (`sqldb.Open`, `Ping`, `Health`, `Shutdown`). The wrapper manages the connection pool.
2. **Enrich** — query builder proxies (`Select`, `Insert`, `Update`, `Delete`) that inject TX from context and map errors to semantic `store.Error` values. DB-level query-hook tracing is planned for the observability release ([ADR-014](014-observability.md)) and is not installed today.
3. **Passthrough** — `Conn(ctx) bun.IDB` for transaction-aware native Bun work, which bypasses error mapping, and `Client() *bun.DB` for model registration, migration operations beyond `Migrate`, and work intentionally tied to the base pool, which bypasses both TX injection and error mapping.

### Query Proxies

The proxies exist to attach the two terminal guarantees — TX injection and error mapping — not to hide Bun: Bun types appear in their signatures by design. The guarantees live in the terminals, so a missing builder method is reached through `Apply` or `ApplyQueryBuilder`, while a missing terminal is a request to extend the curated set. The [store spec](../specs/store.md#query-builders) lists each proxy's methods.

The curated `SelectQuery` set includes `Join`, `JoinOn`, `JoinOnOr`, `TableExpr`, `ColumnExpr`, and `ExcludeColumn` so that non-model JOIN queries (reporting, auth, analytics) do not turn the `Apply` escape hatch into the normal path. They return `*SelectQuery`, and the terminals that carry the interceptors are unchanged.

`SelectQuery`, `UpdateQuery`, and `DeleteQuery` offer `ApplyQueryBuilder(fn func(bun.QueryBuilder) bun.QueryBuilder)` because the typed `Apply` is per query type (`func(*bun.SelectQuery) *bun.SelectQuery` cannot be applied to an update or delete): a predicate shared across reads and writes — soft-delete filters, account scoping, ownership checks, authorization scopes — would otherwise be written once per query type. Bun's `QueryBuilder` is the builder-only interface (`Where`, `WhereOr`, `WhereGroup`, `WherePK`, `WhereDeleted`, `WhereAllWithDeleted`) common to the three, so one predicate applies to all of them, and `WhereGroup`, which the curated set does not expose, becomes reachable. `InsertQuery` has no WHERE clause and is excluded, matching Bun's own `QueryBuilder` interface assertions.

- **A function, not a raw `QueryBuilder()` accessor.** An accessor would return a Bun interface that breaks the proxy's fluent chain and is essentially a second `Unwrap` (`bun.QueryBuilder` carries `Unwrap() any`). `ApplyQueryBuilder(fn)` returns the proxy type, contains the Bun type inside a function boundary, and mirrors Bun's own `ApplyQueryBuilder`. Conditions land on the proxied query, so the terminals keep their interceptors — verified against Bun v1.3.0, whose `selectQueryBuilder` wraps the same `*bun.SelectQuery` pointer and whose Where-family methods mutate it in place. The builder's `Unwrap() any` is the same terminal-bypass escape as `Unwrap()`, and no easier to misuse than `Apply`, which hands out the concrete query directly.
- **The cost is Bun type leakage.** A shared predicate's signature imports `bun` into repository code, at the same documented escape-hatch tier as `Apply`, `Unwrap`, `Conn`, and `Client`. A Credo-owned `WhereScope` interface (`Where`/`WhereOr`/`WherePK`, no `Unwrap`) is deferred until that coupling proves painful: it would reinvent Bun's interface and cannot cheaply express `WhereGroup` recursion.

### Typed Terminals

`SelectQuery` has generic terminal methods — `One[T]`, `All[T]`, and `Page[T]` — that return the queried type. `Scan(ctx, &dest)` makes the caller declare a destination and pass it back in; with Go 1.27's generic methods on a concrete type the destination becomes the type parameter: `u, err := db.Select().Where(...).One[User](ctx)`. `T` drives both the FROM table and the scan destination, so the query is built model-less and the terminal owns the destination. A model already bound through `Select`, `Model`, or `Apply` returns `sqldb.ErrTypedTerminalModel` before executing, rather than letting one of the two silently win.

The semantics are pinned by tests rather than left to Bun: `One` returns the first row under `LIMIT 1`, so several matches are not an error, and an empty `All` is a non-nil empty slice, not `ErrNotFound`. Each terminal executes a private snapshot, so its model or `LIMIT 1` never leaks into a reused builder.

- **Named for the return shape.** `One → T`, `All → []T`, `Page → *Page[T]`. `One`, not `First`: it is a single-row read, not an exactly-one assertion; "exactly one or error" composes `Count`/`Exists`. The pagination terminal is the noun `Page`, not the verb `Paginate`, and there is no destination-taking `Paginate` beside it: a second, differently shaped pagination API is rejected.
- **`Scan` stays** for projections and DTOs whose `T` is not the table model, and for eager loading through a bound model. `TableExpr` does not turn a typed terminal into a projection API: `T` must be the actual table model.

Public `Clone` is a top-level builder fork with Bun's sharing semantics, not a recursive object-graph copy. The private-field compatibility layer beneath it is confined to that fork, the terminal execution snapshots, and the logical-count source built from them; any new private-state use needs its own decision. Bun v1.3.0's `Clone` copies the restored fields itself, and removing the restore step is planned.

### Offset Pagination

`Page[T]` runs COUNT and a LIMIT/OFFSET SELECT and returns a `*pagination.Page[T]` with an exact total; the [pagination spec](../specs/pagination.md#adapter-selectquerypaget) owns its contract. The decisions behind it:

- **Input policy and execution are separate.** `PageRequest.Normalize` and its `Validate` hook are forgiving and mutating: they apply defaults and clamp `PerPage`. `PageRequest.Offset` and `Page` are strict and non-mutating: `Offset` returns `(int, error)`, and `Page` rejects nil, non-positive values, and native `int` overflow with `pagination.ErrInvalidPageRequest` without clamping a valid custom page size, so an invalid request never becomes a silently different query.
- **The total counts logical projection rows** — rows, ungrouped-aggregate rows, distinct tuples, groups, or groups left after `Having` — through a Credo-owned derived source, pinned by conformance tests instead of Bun's narrower grouped/distinct count rewrite. Shapes without a safe Count+window contract (standalone `Having`, a direct compound root) fail with `ErrUnsupportedCountQuery` before I/O, and the model SELECT hooks run for the count too, so hook-added predicates cannot be missing from the total.
- **MySQL is its own name oracle.** MySQL requires unique derived-table output names, but a local SQL parser cannot reproduce the server's naming rules across expressions, wildcards, comments, and session modes. Credo renders the count source once and lets the server judge those bytes under the connection's real `sql_mode`; a logical-count `ER_DUP_FIELDNAME` (1060) is wrapped with `ErrUnsupportedCountQuery` and keeps its driver cause, while every other 1060 passes through.
- **No custom-count API yet.** A costly or volatile projection runs once for COUNT and again for SELECT; such repositories compose an explicit, deliberately equivalent count with the data query and `pagination.NewPage`. A first-class callback cannot prove that two divergent queries describe the same logical set, so it is reconsidered only after two real consumers repeat the same abstraction.
- **`Page` never carries an unknown total.** An unknown total is not encoded as zero, `-1`, a pointer, or an omitted field. A total-free offset window is a separate future shape (working name `Slice[T]`) with its own design gate, not an option that changes `Page`.
- **Snapshot consistency belongs to the caller.** `Page` never opens an implicit transaction: whether COUNT and SELECT see one snapshot depends on the caller's outer transaction, its isolation, and the database ([pagination spec](../specs/pagination.md#count--select-snapshot-consistency)).

### Cursor/Keyset Pagination

Keyset pagination is designed but not exported; the [pagination spec's design gate](../specs/pagination.md#cursorkeyset-design-gate) holds the reserved shape, the SQL, and the codec rules. The decisions:

- The first delivery is a forward-only `CursorPage[T]` — `after` and `per_page` in, `has_next` and a nullable `next_cursor` out — with no total, page number, previous cursor, or `WithTotal` option. It never runs COUNT and is distinct from the total-free offset `Slice[T]`.
- The terminal owns an immutable keyset whose final component is an explicit unique tie-breaker, and rejects a caller's root ORDER/LIMIT/OFFSET/lock, non-row-shaped sources, joins, and hook-capable models before I/O, so the token's order cannot diverge from the executed SQL.
- It appends a portable lexicographic OR-of-AND ladder rather than a row-constructor comparison: the expanded form handles mixed directions and gives MySQL fuller composite-index use ([MySQL row-constructor optimization](https://dev.mysql.com/doc/refman/8.0/en/row-constructor-optimization.html)).
- Nullable keys are excluded: row comparisons become unknown around NULL ([PostgreSQL row comparison](https://www.postgresql.org/docs/current/functions-comparisons.html)), and default NULL placement differs across the supported databases.
- The public-HTTP codec requires an explicit HMAC-SHA256 keyring; Credo never generates a process-local secret or issues unsigned cursors. HMAC authenticates but does not encrypt, so sensitive keys are excluded, and a cursor is never an authorization capability: every request re-applies authentication, authorization, tenant predicates, and filters.

The gate stays closed until (1) a real consumer supplies its model, filters, order, and key-rotation requirements; (2) `sqldb` rejects hook-capable models before I/O; (3) PostgreSQL, MySQL, and SQLite conformance pins the generated SQL, ordering, mutation behavior, and typed round trips; (4) the transport-neutral `fault.Kind` taxonomy gains an invalid-argument kind that the error pipeline classifies as 400; and (5) the wire framing has cross-implementation golden vectors. Until then applications own an explicit keyset query and token codec; Credo does not publish a half-contract whose ordering or signature policy would later have to change silently.

### Connection-Pool Policy

Credo does not invent a workload-independent connection-pool size. `MaxOpen=0` retains `database/sql`'s unlimited-open behavior; silently choosing a finite default could either under-provision a busy service or overrun a smaller database. Instead, a pool that is still unlimited when the start phase pings a store registered with `store.Register` emits one structured warning with code `sqldb.pool.max_open_unlimited`. Standalone users that do not call `store.Register` can inspect `DB.StoreRegistrationWarningCodes()` and route the same secret-free code through their own bootstrap logging.

Pool settings distinguish absence from an explicit value (`MaxIdle` is a pointer: `nil` leaves the `database/sql` default, `new(0)` disables idle retention) and refuse an explicit `MaxIdle` above a finite `MaxOpen` at `Open` instead of relying on `database/sql`'s silent clamp; see the store spec's [Config](../specs/store.md#config).

`DB.Stats()` exposes the complete `sql.DBStats` snapshot, and SQL health metadata carries the pool's gauges and cumulative counters. Readiness does not serialize adapter `Details`, so these values are operational diagnostics for code and future metrics rather than an accidental public probe schema.

Pool saturation does not produce `StatusDegraded`. A useful signal requires production metrics, an explicit SLO, windowed counter deltas, hysteresis, and opt-in thresholds. A universal instantaneous threshold would be noisy; because every store is critical and `DEGRADED` removes readiness, such noise could also trigger a replica-wide traffic cascade.

### Migrations

`sqldb` wraps Bun's migration engine thinly — `db.RegisterMigrations(set, opts...)` and `db.Migrate(ctx)` ([store spec](../specs/store.md#migrations-bunmigrate-wrapper)). `bun/migrate` is part of the pinned Bun module, so there is no second migration engine and no new dependency, and Bun's own `*migrate.Migrations` set (SQL files through `Discover`, `embed.FS` included; Go migrations through `MustRegister`) is not re-wrapped. Seeding is a plain migration file; rollback, status, and file generation stay on Bun's migrator through `Client()`.

- **Production runs one pre-deploy job.** A single deadline-bounded job calls `DB.Migrate` before rollout. `app.OnStart(db.Migrate)` suits development, tests, single replicas, and deliberately small deployments, but simultaneous replica starts would make the lock losers fail their start and can crash-loop during a long migration.
- **No lock wait in the library.** Lock is fail-fast. Waiting or retrying is rejected: overlapping release jobs are a coordination error, and waiting could run migrations from different releases in the wrong order. An unlock that times out has an uncertain outcome, so Credo neither repeats it nor retries automatically.
- **Mark on success, deliberately diverging from Bun's default.** `Migrate` passes `migrate.WithMarkAppliedOnSuccess(true)`, so an Up error leaves the migration unapplied. That is at-least-once bookkeeping, not safe retry: non-transactional work can be partial, a successful body and its marker write are separate, and a group is not all-or-nothing. Migrations must be transactional where supported or idempotent/reconcilable, with expand/backfill/contract rollouts for destructive changes. `RegisterMigrations` options restore Bun's record-before-running trade-off.
- **A transactional migration's COMMIT counts.** Bun v1.3.0 returns the COMMIT or ROLLBACK error of a `.tx.up.sql` file, so a failed commit leaves the migration unapplied. Mark-on-success depends on that contract, which earlier Bun releases broke ([Bun #1389](https://github.com/uptrace/bun/issues/1389)), so a conformance test on SQLite and PostgreSQL pins it.
- **Signature compatibility, not coupling.** `Migrate` matches the `App.OnStart` hook signature, while `sqldb` imports only `credo/store`, never the root framework package. Registration misuse (a nil set, a second registration) panics per the panic-vs-error policy; `Migrate` itself only returns errors.

### Registration

A store takes two calls, each with one job. The application binds the store where ownership is decided — `ProvideValue`, or `Provide` with a constructor, and `credo.Borrowed()` on `ProvideValue` for a handle it shares with another App or a test fixture — and `store.Register` names that binding by type and adds it to the App's store registry:

```go
func Register[R store.Lifecycle](app *credo.App, opts ...RegisterOption)

app.ProvideValue(db)           // the binding: the App owns db and shuts it down after its consumers
store.Register[*sqldb.DB](app) // the registration: pinged at start, reported by /ready
```

- **Registration does no I/O and holds no value.** `store.Register` records `R`, its name (`WithName`, or the package-qualified type name) and its ping timeout (`WithPingTimeout`, default `store.DefaultPingTimeout`, five seconds), returns nothing, and panics on misuse known at the call, like every registration: a nil App or option, a ping timeout that is not positive, an invalid name, an `R` without a stable default name and no `WithName`, a type or a name registered twice, and a call after `Finalize` or shutdown. An `R` that does not implement `Lifecycle` does not compile. `R` may be bound directly or be an interface an `Alias` names; a registration whose `R` has no binding fails `Finalize` with a message naming the type and `store.Register`, reported together with every other `Finalize` finding.
- **The start phase builds `R` once.** When the start walk reaches the binding of `R` — in dependency order, after `Finalize` and therefore after every override — it builds the value and runs the registration's start step before the value's own `Start`: it pings the store under the `WithPingTimeout` deadline, derived from the start context, before the components that depend on `R` start. The pinged value is what `/ready` reports, with its typed `Health`, without resolving anything per request ([health spec](../specs/health.md#readiness-aggregation)). A failed ping, a nil bound value, or a constructor error while the start phase builds `R` is a start failure, reported in the `*credo.LifecycleError` under the component's name, and rolls back ([ADR-024](024-lifecycle-components.md)); the value is still rolled back with the rest — shut down unless borrowed — since the ping opened nothing the value had not. A store constructor therefore runs in the start phase, not at registration. A borrowed store is pinged and reported and never shut down.
- **The registry's store is the value DI hands its consumers**, in tests too: an override of `R` (`credo.Override()`, `testutil.WithOverride`) is what gets pinged, reported and shut down, and the original value is never pinged. No integration monitors one value while DI resolves another, and the override a test needs is not forbidden.
- **Teardown belongs to the component registry, not to the store registry.** `Lifecycle` includes `Shutdown`, so the binding of `R` is a component whose teardown follows the App's ownership rule ([ADR-024](024-lifecycle-components.md)): owned unless borrowed, shut down after its consumers, abandoned at the drain deadline like any component. Independent stores with no edges between them shut down in reverse registration order. The store registry pings and reports; it never closes anything.
- **Identity is the root's.** Several bindings that hold one physical resource share one teardown through `credo.ResourceIdentifier` ([ADR-024](024-lifecycle-components.md#resource-identity-one-resource-one-teardown)), run once, when the last holder retires. `*sqldb.DB` implements it by returning itself, so a wrapper that embeds it stays one resource with it. The rule refuses mixed ownership: a holder that claims the teardown of a borrowed resource panics at the later call or fails its construction. Two store registrations of one resource are not refused: each is pinged and reported, and the resource is still shut down once.
- **Registration warnings** that a value reports through `StoreRegistrationWarningCodes` (`sqldb.pool.max_open_unlimited`) are logged at Warn once per code (`credo: store configuration warning`, with `component=store`, the store name and the code) after the ping has succeeded, since registration does not have the value. A code that is not a short, secret-free identifier fails the start without being logged.
- **The registry is reached through an internal seam** between the root and `store`, not through the container. An application that needs one store's health calls the value's own `Health`.

Store names share the named-health validator. Explicit empty, padded, control-character, and reserved `credo.` names are rejected rather than normalized. When `WithName` is omitted, pointer layers are unwrapped and the package-qualified name of a named `R` is used (`sqldb.DB`); unnamed types require an explicit name.

`store.Register` is a free function rather than a root `App` method, even though the DI surface itself is method-based (`app.Provide[T]` / `app.Resolve[T]`): the `store` package cannot add methods to `*credo.App` (Go forbids methods on a type from another package), and having the root import `store` to host the method would invert the dependency direction the architecture enforces. `worker.Use` is a function for the same reason — feature-package registration lives in the feature package, while only the generic container surface lives on `App`.

**A value without `Lifecycle`** is registered through a wrapper type that implements it — the rule ADR-024 gives a teardown that is not a `Close`: the wrapper is the binding, its constructor's parameters give its dependencies their edges, and its `Ping`, `Health` and `Shutdown` cannot target different objects.

**Several databases.** The container keys bindings by type, so each database beyond a default `*sqldb.DB` is a wrapper type such as `type AnalyticsDB struct{ *sqldb.DB }`, bound once and registered by its type with `store.WithName`; a consumer names its database in its constructor signature (`NewReportRepo(db AnalyticsDB)`). [ADR-004](004-dependency-injection-and-infra.md#several-instances-of-one-type) records the general rule — the type as qualifier, embedding, generic repositories, collection types for a configured number of instances — and the container-level alternatives it rejects; the [store spec](../specs/store.md#several-databases) shows the wiring. A read replica of one database is routing, not a second store: Bun offers it only experimentally (`extra/bunexp`), and `sqldb` does not expose it.

**Rejected alternatives:**

- **A store registration that takes the store's value**, pings it at the call and publishes it itself — after an override it would ping, report and shut down a value DI does not hand its consumers: a test's services would use the test database while the ping and `/ready` reached the real one. Closing that gap would mean forbidding the override, which `credo.Override()` and `testutil.WithOverride` exist to perform.
- **A separate health handle passed as a value**, with its own flag for caller ownership — it drifts from an override the same way, and lets ping, health and shutdown target different objects; the wrapper type above gives the same reach with one value, and `credo.Borrowed()` gives caller ownership.
- **A store-specific ownership ledger** reserving name, type and resource identity — it would see only `store.Register` calls, so the same resource bound another way would escape it; the component registry's resource identity covers every binding.
- **A generic handle in `sqldb`** (`sqldb.Of[Analytics]`) for several databases — still a marker type per database, for an API that turns generic throughout; the wrapper type is that marker with a better name.

## Consequences

**Positive:**

- Semantic `fault.Kind` values allow independent HTTP/gRPC policy without a root→store cycle
- Single ORM focus allows deep integration (query proxies, error mapping, pagination helpers) instead of shallow generic interfaces
- Context-based TX is opt-in — repositories that don't need TX are simpler
- `store/` contracts remain ORM-agnostic — custom adapters possible
- Separate submodule keeps Bun dependency out of core
- `Client()` escape hatch prevents the wrapper from becoming a bottleneck
- Ownership is decided once, by the binding: the App's component teardown is the sole framework shutdown owner of a store it owns, and a borrowed store stays outside it
- The store `/ready` reports is the value DI resolves, overrides included, and nothing is resolved per readiness request
- Registration performs no I/O; misuse panics at the call and an unbound registration fails `Finalize`

**Negative:**

- Applications using GORM lose the first-class adapter — must use raw DI
- Context-based TX uses `context.WithValue` (type-safe via generics, but invisible in function signatures compared to explicit TX passing)
- Query builder proxies add a thin layer over Bun's API that must be kept in sync with Bun releases
- Bun version is pinned by `store/sqldb/` module — coordinated upgrades, each one checked against the private-field layout the compatibility layer reads
- Error mapping is driver-family-aware but cannot cover every custom driver until a classifier seam exists
- A store costs two calls — the binding and the registration — and an unreachable database fails the start rather than the registration
- A value that does not implement `Lifecycle` needs a wrapper type; identity sharing is explicit through `ResourceIdentifier`, and the framework does not guess ownership by scanning arbitrary fields
