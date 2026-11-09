# ADR-015: Data Access

**Status:** Accepted; cursor/keyset pagination deferred **Date:** 2026-03-04 **Depends on:** ADR-004, ADR-009, ADR-024 **Related:** ADR-014, ADR-016 **Specification:** [Store spec](../specs/store.md), [Pagination spec](../specs/pagination.md) **Guide:** [Data access guide](../guides/data-access.md)

## Context

Credo needs a data access layer that lets applications work with SQL databases without coupling the framework core to any ORM. The design must:

- Keep the core package (`store/`) free of external dependencies
- Provide universal semantic error types that HTTP and future gRPC policy can map independently, through one status channel: a second contract for the same error, such as a status method beside the kind, would freeze two statuses for one error, and a code derived from the status would give every 409 kind one code, so a client could not tell a duplicate from a failure a retry may clear
- Support context-based transaction propagation as an opt-in convenience while preserving the ability to use the ORM's native transaction API
- Wrap a single ORM (Bun) behind a proxy layer that owns lifecycle, enriches queries, and provides an escape hatch to the raw client
- Build on Bun's exported API only: a wrapper that writes Bun's private fields is pinned to one release's memory layout, so every upgrade needs a layout check and an application that minimal version selection gives a newer Bun fails to open; and one closure must behave alike on a select and on a write, so a connection chosen inside an escape hatch cannot be honoured by one query type alone
- Keep a page's total exact: a count that has to describe a query whose shape it cannot see — an escape hatch, a raw projection, a model hook that rewrites the query — can only guess, and a guessed total is a wrong count; such a query fails, and a repository whose query needs a count of its own hands it to the page instead of reassembling the page by hand
- Integrate with lifecycle management, health checks, and DI registration
- Support multi-database setups via DI wrapper types
- Give each operation one exported spelling before v1 freezes the surface: an API exported beside its replacement freezes with it, and an umbrella sentinel over kinds whose handling differs — a serialization failure or a deadlock may clear, a constraint violation does not — lets code branch on the coarse condition the exact sentinels exist to replace
- Fail loud wherever a silent fallback would change behavior: a transaction read back under a different type must not fall back to the pool, transaction options a savepoint cannot honour must not be dropped, a query whose total the wrapper cannot prove must not produce a count, and a driver message that merely contains "duplicate key" must not be classified as a duplicate

### Why Single-ORM (Bun)?

Maintaining adapters for multiple ORMs multiplies the wrapper surface area and testing cost. Framework-level features (query proxies, error mapping, pagination helpers) are impossible to build on a generic ORM-agnostic interface — every adapter would need its own implementation, resulting in near-identical duplicated code.

Bun was chosen because it maps closely to SQL (no magic struct-scan), supports raw SQL alongside the query builder, and has a clean `bun.IDB` interface that unifies `*bun.DB` and `bun.Tx`.

Applications that need a different ORM (GORM, sqlx, sqlc) bind their client directly in DI without using `store/sqldb/`; the `store/` contracts (errors, health) are ORM-agnostic and still apply, and an adapter type that implements `Start`, `Ready` and `Shutdown` is a component like `*sqldb.DB` ([A Database Is a Component](#a-database-is-a-component)).

## Decision

### Two-Package Split: store/ + store/sqldb/

- **`store/`** — universal contracts (errors, `Health`, TX context helpers). Zero external dependencies. Part of the main `github.com/credo-go/credo` module.
- **`store/sqldb/`** — Bun SQL wrapper (`DB`, `Config`, query builders, transactions, error mapping). Separate Go submodule (`github.com/credo-go/credo/store/sqldb`) so the Bun dependency is opt-in.

`store/` holds errors, `Health` and the TX context helpers; it has no registration and no lifecycle contract. `store/sqldb/`'s `*DB` implements `Start`, `Ready` and `Shutdown` and is a component like any other ([A Database Is a Component](#a-database-is-a-component)).

### Semantic Error Types

The stdlib-only `fault` leaf package defines transport-neutral `Kind` values and a `Provider` interface. Both root HTTP policy and `store` import that leaf, and `store` imports nothing else, so neither the root nor `store` imports the other. `store.Kind` aliases `fault.Kind` for application-facing data-access code.

`store.Error` carries the kind together with structured diagnostics — operation, resource, constraint, driver code, a transient flag and the cause — that never reach the wire: the default error renderer serializes none of them. Exact sentinels distinguish not-found, already-exists, persistent constraint, serialization, deadlock, lock/busy contention, timeout, unavailable, and read-only conditions, so callers branch on the condition rather than on a coarse conflict. The [store spec](../specs/store.md#semantic-errors) lists the struct, the sentinels and their default statuses.

`Transient` means only that the condition may clear. It never promises that replaying a statement, transaction callback, or externally visible operation is safe. Commit outcome is deliberately not inferred from a driver cause kind; an `OutcomeUnknown` model is deferred until operation-phase evidence can make it reliable.

A store error reaches the client through its kind alone. Root HTTP policy reads `FaultKind` and maps the kind to its status ([ADR-009](009-handler-and-error-handling.md)); no store type has an `HTTPStatus` method. The classification order is `*HTTPError`, `*fault.Error`, the validation errors, `*BindError`, any other `fault.Provider`, then 500, so a service overrides a store error by returning an `HTTPError`, or a `fault.Error` that names a code of its own, with the store error as its internal cause, which is logged and never rendered; future gRPC policy reads the same kind. Unknown kinds fail closed as 500.

A fault without an explicit code renders its kind as its code — `not_found`, `already_exists`, `constraint`, `serialization`, `deadlock`, `contention`, `conflict`, `timeout`, `unavailable`, `read_only` — so a client tells a duplicate from a serialization or deadlock failure that may clear, where a code derived from the status would name all of them `conflict`. The statuses do not change. `store.Error`'s `Code` is the driver's code, a diagnostic that is never rendered; the client-visible code is the kind's, or the code a service names through `fault.Error`.

**Classification reads structure, never text.** `sqldb` classifies with the configured driver family (the selected Bun dialect for a custom connector) and the operation context. SQLSTATE is the cross-driver path; when a MySQL error exposes both its number and a broad SQLSTATE class, the more specific number wins (1213/40001 is a deadlock, not a generic serialization failure). MySQL's strict `Error NNNN (STATE): ...` envelope is parsed only for MySQL, and SQLite's structured code only for SQLite. Loose substrings such as "duplicate key" or "read-only" are never inspected, so a scanner, query-hook or domain error that contains such text keeps its exact identity. PostgreSQL `57014` means query canceled, not timeout: caller cancellation stays cancellation, a caller deadline maps to `Timeout`, and with a live context only a structured driver error that explicitly says statement timeout does. MySQL 1290 is not classified as read-only because it is the generic option-prevents-statement code; the read-only transaction/server codes 1792 and 1836 are. Every mapped result is a `*store.Error` that preserves the cause and the driver code. A public classifier registry is deferred until a custom-driver consumer demonstrates the need. The code-level table is the store spec's [Error Mapping](../specs/store.md#error-mapping).

### Context-Based TX

- `store.NewTxScope[T]()` fixes the transaction contract at scope construction. Its `WithTx`, `GetTx`, `RequireTx`, and `Conn` methods all use that same `T`: a per-method `T` would let a concrete transaction be stored and read back through an interface type under a different context key, silently selecting the fallback connection.
- Each `sqldb.DB` owns a distinct `TxScope[bun.IDB]`, isolating multiple logical databases that share Bun's interface type.
- `db.InTx(ctx, fn)` starts a transaction, stores it in that DB's private scope, executes the callback, and commits/rolls back; `db.InTxWith(ctx, opts, fn)` also takes `sql.TxOptions`. Nested calls use savepoints.
- `sqldb.DB.Conn(ctx)` exposes the active `bun.IDB` for transaction-aware native Bun operations; `RequireTx(ctx)` returns `store.ErrTxMissing` instead of falling back.

This is an **opt-in convenience**. Proxy users participate automatically when they pass the callback context. Custom adapters use their own typed scope; its methods are the one transaction API ([One API per operation](#one-api-per-operation)).

`InTx` and `InTxWith` are methods, so the operation is on the value the caller already holds; their name avoids confusion with Bun's native `(*bun.DB).RunInTx(ctx, opts, fn(ctx, tx))`, reachable through `Client()`.

The transaction boundary fails loud and closed rather than guessing ([store spec](../specs/store.md#tx-management) for the mechanics and budgets):

- **Callback errors stay application values.** After a successful rollback `InTx` returns the callback's exact error without SQL error mapping: domain text such as "duplicate key validation" must not acquire `store.ErrAlreadyExists`.
- **No silent option loss.** Bun implements a nested transaction as a savepoint and ignores `sql.TxOptions` there, so non-default options on a nested call return `ErrNestedTxOptions`; the owner of the outer transaction chooses isolation and read-only mode.
- **Rollback-only instead of a commit race.** An uncertain savepoint operation synchronously marks the shared transaction state rollback-only before its bounded asynchronous abort, and every outer level checks that state before commit. Whether the transaction commits never depends on how the abort goroutine is scheduled, even when a nested error is swallowed.
- **Commit errors are outcome-ambiguous.** A commit failure does not prove that the transaction was not applied; its classification describes the driver cause, and callers must not turn it into a blind retry.

Rejected:

- **A `Session`/Unit-of-Work object** beside the typed ambient scope — deferred until an outbox or another transaction-bound multi-repository consumer demonstrates a boundary that `InTx` plus explicit `DB.Conn(ctx)` cannot express cleanly.
- **An ambient Bun `ConnResolver`** to make `Client()` transaction-aware — Bun v1.3.0 gives a resolver one owned slot and closes it from `bun.DB.Close`; explicit query connections win over it, while direct `Client().ExecContext`, `QueryContext`, `QueryRowContext`, and `BeginTx` bypass it. Credo's proxy terminals bind an explicit connection, so a resolver would make only part of `Client()` transaction-aware and would complicate read-replica composition and ownership. Terminal-level injection stays the default and `DB.Conn(ctx)` is the explicit native path. A composite order (`explicit query conn → active Credo TX → user/replica resolver → base DB`) is reconsidered only when automatic native-builder or replica routing is a demonstrated requirement.

### One API per operation

Each data-access operation has one exported spelling before v1 freezes the surface:

- **One transaction scope API.** The `TxScope` methods — `WithTx`, `GetTx`, `RequireTx` and `Conn`, whose `T` the scope fixes — are the one transaction API, and `sqldb` uses it. No free function keyed by the type alone exists beside them: separate call sites could choose different concrete or interface forms of one transaction, and a type-only key cannot isolate two logical connections that share `T`.
- **One way to run a transaction.** `db.InTx(ctx, fn)` and `db.InTxWith(ctx, opts, fn)` put the operation on the database the caller holds, and no free function runs a transaction beside them.
- **The exact sentinels are the error contract.** No alias names a sentinel twice, and no umbrella sentinel matches several kinds: each sentinel matches its own kind only. `KindConflict` is a kind an adapter may return, which `store.KindOf` reports and the root maps to 409; no sentinel stands for it, and none of the constraint and concurrency sentinels matches it.

Kept: `store.Health`, the type `sqldb`'s `Health` returns and its `Ready` reads ([A Database Is a Component](#a-database-is-a-component)).

### 3-Tier Bun Wrapping

The `sqldb.DB` wrapper applies three strategies to Bun's API:

1. **Own** — lifecycle (`sqldb.Open`, `Ping`, `Health`, `Shutdown`). The wrapper manages the connection pool.
2. **Enrich** — query builder proxies (`Select`, `Insert`, `Update`, `Delete`) that inject TX from context and map errors to semantic `store.Error` values. DB-level query-hook tracing is planned for the observability release ([ADR-014](014-observability.md)) and is not installed today.
3. **Passthrough** — `Conn(ctx) bun.IDB` for transaction-aware native Bun work, which bypasses error mapping, and `Client() *bun.DB` for model registration, migration operations beyond `Migrate`, and work intentionally tied to the base pool, which bypasses both TX injection and error mapping.

### Bun Through Its Exported API

`sqldb` uses Bun through its exported API only. Terminals keep their snapshot semantics through Bun's own `Clone()`, complete from v1.3.0, and the public `Clone()` forwards to it. Whether a connection was set explicitly comes from the wrapper's `Conn` on all four query types: a `Conn` set inside an `Apply` closure is overridden by transaction injection. The curated forwarders, transaction injection and error mapping at the terminals, the typed `One`/`All`/`Page`/`PageWithCount` terminals and the `Apply`/`Unwrap` escape hatches are the thin adapter [ADR-002](002-code-acquisition-strategy.md#wrap--pin--components-with-fast-evolution-and-high-security-pressure) describes; no `sqldb` file imports `unsafe`. A Bun upgrade passes the conformance tests that pin what the adapter relies on — a connection kept or overridden, the clauses every count drops, the totals, and the two hook facts of [Offset Pagination](#offset-pagination) — on SQLite, PostgreSQL and MySQL, not a check of Bun's private layout. A predicate shared across query types goes through `ApplyFilter` ([Query Proxies](#query-proxies)), and the total of a query the wrapper cannot prove is the caller's ([Offset Pagination](#offset-pagination)). The wrapper learns how a select was built from its own builder methods, never from Bun's state, so a Bun builder error recorded on a query surfaces at its terminal: a typed terminal on a model-bound query reports `ErrTypedTerminalModel` first, and either fails before I/O.

### Query Proxies

The proxies exist to attach the two terminal guarantees — TX injection and error mapping — not to hide Bun: Bun types appear in their signatures by design. The guarantees live in the terminals, so a missing builder method is reached through `Apply`, a predicate shared across query types through `ApplyFilter`, while a missing terminal is a request to extend the curated set. The [store spec](../specs/store.md#query-builders) lists each proxy's methods.

The curated `SelectQuery` set includes `Join`, `JoinOn`, `JoinOnOr`, `TableExpr`, `ColumnExpr`, and `ExcludeColumn` so that non-model JOIN queries (reporting, auth, analytics) do not turn the `Apply` escape hatch into the normal path. They return `*SelectQuery`, and the terminals that carry the interceptors are unchanged.

`SelectQuery`, `UpdateQuery` and `DeleteQuery` offer `ApplyFilter(fn func(*sqldb.Filter))`, because the typed `Apply` is per query type: a predicate shared across reads and writes — soft-delete filters, account or tenant scoping, ownership checks, authorization scopes — is one function that scopes a select, an update and a delete. `Filter` is Credo's type with the predicate methods and nothing else — `Where`, `WhereOr`, `WhereGroup`, whose callback takes a `*Filter`, `WherePK`, `WhereDeleted` and `WhereAllWithDeleted` — so the callback has no Bun type to assert and no builder to return. A query built with it keeps its automatic total whatever the callback does, and since `Filter` forwards only the methods it names, a method a later Bun adds to its builder changes nothing here. A callback that needs more than predicates uses `Apply`, whose query `Count` and `Page` cannot prove. Each `Filter` method returns the `*Filter` for chaining, and a `Filter` is valid only during the callback it is passed to. `InsertQuery` has no WHERE clause and no `ApplyFilter`.

Rejected: handing the callback a builder that embeds Bun's `bun.QueryBuilder` interface, on which a method a later Bun adds runs where Credo cannot see it, so that it needs a method-set comparison, a log at `Open` and a classification at every Bun upgrade, while an application that minimal version selection gives a newer Bun fails its filtered pages at request time; tracking `Unwrap` on Bun's own builder, which a type assertion passes unseen; and implementing Bun's builder interface method by method, which breaks every application that upgrades Bun before Credo does.

### Typed Terminals

`SelectQuery` has generic terminal methods — `One[T]`, `All[T]`, `Page[T]` and `PageWithCount[T]` — that return the queried type. `Scan(ctx, &dest)` makes the caller declare a destination and pass it back in; with Go 1.27's generic methods on a concrete type the destination becomes the type parameter: `u, err := db.Select().Where(...).One[User](ctx)`. `T` drives both the FROM table and the scan destination, so the query is built model-less and the terminal owns the destination. A model already bound through `Select`, `Model`, or `Apply` returns `sqldb.ErrTypedTerminalModel` before executing, rather than letting one of the two silently win.

The semantics are pinned by tests rather than left to Bun: `One` returns the first row under `LIMIT 1`, so several matches are not an error, and an empty `All` is a non-nil empty slice, not `ErrNotFound`. Each terminal executes a private snapshot, so its model or `LIMIT 1` never leaks into a reused builder.

- **Named for the return shape.** `One → T`, `All → []T`, `Page → *Page[T]`, and `PageWithCount → *Page[T]` with the caller's count. `One`, not `First`: it is a single-row read, not an exactly-one assertion; "exactly one or error" composes `Count`/`Exists`. The pagination terminal is the noun `Page`, not the verb `Paginate`, and there is no destination-taking `Paginate` beside it: a second, differently shaped pagination API is rejected.
- **`Scan` stays** for projections and DTOs whose `T` is not the table model, and for eager loading through a bound model. `TableExpr` does not turn a typed terminal into a projection API: `T` must be the actual table model.

Public `Clone` forwards to Bun's `Clone()`: a top-level builder fork with Bun's sharing semantics, not a recursive object-graph copy. The terminal execution snapshots are Bun's `Clone()` too ([Bun Through Its Exported API](#bun-through-its-exported-api)).

### Offset Pagination

`Page[T]` runs Bun's count and a LIMIT/OFFSET SELECT and returns a `*pagination.Page[T]` with an exact total; the [pagination spec](../specs/pagination.md#adapter-selectquerypaget) owns its contract. The decisions behind it:

- **Input policy and execution are separate.** `PageRequest.Normalize` and its `Validate` hook are forgiving and mutating: they apply defaults and clamp `PerPage`. `PageRequest.Offset` and `Page` are strict and non-mutating: `Offset` returns `(int, error)`, and `Page` rejects nil, non-positive values, and native `int` overflow with `pagination.ErrInvalidPageRequest` without clamping a valid custom page size, so an invalid request never becomes a silently different query.
- **The total is Bun's count, the caller's, or an error.** `Total` is the number of rows the data query returns without its `LIMIT` and `OFFSET`, never another number. `Count` and `Page` run Bun's native `Count` on a query the wrapper can prove: built without `Apply`, `Unwrap` or a `Relation` callback; whose root model's type does not implement `BeforeSelectHook`; and that either carries `GroupExpr` or `Distinct` or has no raw `ColumnExpr` and no `Having`. Every other query fails before I/O with `ErrUnsupportedCountQuery`, and the caller pages it with `PageWithCount`. No count Credo runs sorts or locks. Rejected: the derived-table count as the fallback for queries the wrapper cannot prove, which carries their sort and any lock clause; a derived count for a raw `ColumnExpr`, even with the sort held back until the data query, a second count path, which on MySQL fails with error 1060 for a projection that repeats a column name; `COUNT(*) OVER()`, which adds a column the model scan cannot place and miscounts under `DISTINCT`; and treating every query with a filter callback as unknown, which sends the tenant pattern down the explicit path on every page.
- **`BeforeAppendModelHook` keeps the total.** On a SELECT, Bun calls it only for values already in the destination, `Page` scans into an empty slice of its own, and `Count` runs no hook, so the hook never sees a query whose total Credo computes, and a model that sets timestamps in it keeps its automatic total. A conformance test pins both facts against the pinned Bun, so a release that changes either fails `sqldb`'s tests, not an application's count. Rejected: making a model's `BeforeAppendModelHook` turn its query unknown, which takes the automatic total from Bun's timestamp idiom, although the hook never sees a counted query.
- **The caller's count is `PageWithCount`.** `PageWithCount[T](ctx, req, count func(context.Context) (int, error))` validates the request as `Page` does and refuses a bound model, both before any I/O, then runs `count` with its own context — another query's `Count` passes as it is — and the data query with the page's window, and builds the page with `NewPage`. An error from `count` is returned as it is, and a nil `count` is an error before any I/O. The data query runs whatever the count returns, so the page is exactly what the caller's count and the query describe. The count's sort and lock clauses are the caller's, in the caller's code.
- **The error is part of the API.** `ErrUnsupportedCountQuery` (`sqldb: unsupported Count/Page query`) carries a message that names the rule that made the query unknown, its culprit — the escape hatch, the raw `ColumnExpr` or `Having`, the model type and its `BeforeSelect` hook — and the remedy, `PageWithCount`; `errors.Is` keeps matching the sentinel, and no exported reason type is added.
- **MySQL is its own name oracle.** Bun's count wraps a query with `GroupExpr` or `Distinct` in a CTE whose output names MySQL requires unique, but a local SQL parser cannot reproduce the server's naming rules across expressions, wildcards, comments, and session modes. Credo lets the server judge the count under the connection's real `sql_mode`: a count's `ER_DUP_FIELDNAME` (1060) is wrapped with `ErrUnsupportedCountQuery`, whose message names unique aliases as the remedy, and keeps its driver cause, while every other 1060 passes through.
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

Credo does not invent a workload-independent connection-pool size. `MaxOpen=0` retains `database/sql`'s unlimited-open behavior; silently choosing a finite default could either under-provision a busy service or overrun a smaller database. Instead, `DB.Start` logs one structured warning with code `sqldb.pool.max_open_unlimited` once its ping has succeeded, when the effective pool is still unlimited at that moment, so a `SetMaxOpenConns` on its client before the start counts. The record goes to the database's logger (`sqldb.WithLogger`, else the App's logger scoped to the component) with `module=sqldb`. A program that runs the database without an App calls `Start` or reads `Stats().MaxOpenConnections`.

Pool settings distinguish absence from an explicit value (`MaxIdle` is a pointer: `nil` leaves the `database/sql` default, `new(0)` disables idle retention) and refuse an explicit `MaxIdle` above a finite `MaxOpen` at `Open` instead of relying on `database/sql`'s silent clamp; see the store spec's [Config](../specs/store.md#config).

`DB.Stats()` exposes the complete `sql.DBStats` snapshot, and SQL health metadata carries the pool's gauges and cumulative counters. Readiness does not serialize adapter `Details`, so these values are operational diagnostics for code and future metrics rather than an accidental public probe schema.

Pool saturation does not produce `StatusDegraded`. A useful signal requires production metrics, an explicit SLO, windowed counter deltas, hysteresis, and opt-in thresholds. A universal instantaneous threshold would be noisy, and such noise could trigger a replica-wide traffic cascade.

A database is a component, and its `Ready` fails whenever its `Health` is not up, so a `DEGRADED` it reported would take the instance out of rotation like `DOWN`; the same cascade argument holds for every database that gates readiness, which a bound database does unless it is bound with `credo.OptionalReadiness()` ([ADR-016](016-health-checks.md#optional-readiness)). A component that reports `degraded` itself stays deferred for the same reason: no component produces such a status ([ADR-016](016-health-checks.md#optional-readiness)).

### Migrations

`sqldb` wraps Bun's migration engine thinly — `db.RegisterMigrations(set, opts...)` and `db.Migrate(ctx)` ([store spec](../specs/store.md#migrations-bunmigrate-wrapper)). `bun/migrate` is part of the pinned Bun module, so there is no second migration engine and no new dependency, and Bun's own `*migrate.Migrations` set (SQL files through `Discover`, `embed.FS` included; Go migrations through `MustRegister`) is not re-wrapped. Seeding is a plain migration file; rollback, status, and file generation stay on Bun's migrator through `Client()`.

- **Production runs one pre-deploy job.** A single deadline-bounded job calls `DB.Migrate` before rollout. `app.OnStart(db.Migrate)` suits development, tests, single replicas, and deliberately small deployments, but simultaneous replica starts would make the lock losers fail their start and can crash-loop during a long migration.
- **No lock wait in the library.** Lock is fail-fast. Waiting or retrying is rejected: overlapping release jobs are a coordination error, and waiting could run migrations from different releases in the wrong order. An unlock that times out has an uncertain outcome, so Credo neither repeats it nor retries automatically.
- **Mark on success, deliberately diverging from Bun's default.** `Migrate` passes `migrate.WithMarkAppliedOnSuccess(true)`, so an Up error leaves the migration unapplied. That is at-least-once bookkeeping, not safe retry: non-transactional work can be partial, a successful body and its marker write are separate, and a group is not all-or-nothing. Migrations must be transactional where supported or idempotent/reconcilable, with expand/backfill/contract rollouts for destructive changes. `RegisterMigrations` options restore Bun's record-before-running trade-off.
- **A transactional migration's COMMIT counts.** Bun v1.3.0 returns the COMMIT or ROLLBACK error of a `.tx.up.sql` file, so a failed commit leaves the migration unapplied. Mark-on-success depends on that contract, which earlier Bun releases broke ([Bun #1389](https://github.com/uptrace/bun/issues/1389)), so a conformance test on SQLite and PostgreSQL pins it.
- **Signature compatibility, not coupling.** `Migrate` matches the `App.OnStart` hook signature and depends on no hook. Registration misuse (a nil set, a second registration) panics per the panic-vs-error policy; `Migrate` itself only returns errors.

`sqldb` imports the root package for `credo.InfraFrom`, which `DB.Start` reads for its logger, so `store/sqldb/go.mod` requires the root module of the same release.

### A Database Is a Component

A bound store is an ordinary component: the type of its binding plans the ping, the readiness entry and the teardown, as for every component ([ADR-024](024-lifecycle-components.md)). One call is enough:

```go
app.ProvideValue(db) // pinged before its dependants start, reported by /ready, shut down after its consumers
```

- **`Start` pings.** `*sqldb.DB` implements `Start(ctx)`, which pings under `Config.PingTimeout` (zero means `sqldb.DefaultPingTimeout`, five seconds; a negative value fails `Open`). Zero is not "no deadline", unlike `ConnectTimeout`, because an unbounded ping would hang the start. The start walk runs it before the components that depend on the database start, in dependency order and after every override, so the value pinged is the value DI hands its consumers. An unreachable database therefore fails the start, and the start rolls back.
- **A failed ping closes the database.** `Open` opened the pool, not `Start`, and the App does not shut down a component whose `Start` failed ([ADR-024](024-lifecycle-components.md)): that rule assumes `Start` released what it holds, so `Start` closes the database before it returns the error, which names the component and the deadline.
- **`Ready` reports.** `*sqldb.DB` implements `Ready(ctx)`, which returns an error naming the status when `Health` is not up. Its readiness entry is planned from the binding's type like any component's, under the component's name — `*sqldb.DB`, or its `credo.ComponentName` name — and carries the ping's latency ([ADR-016](016-health-checks.md#readiness-from-the-kernels-registries)). Every bound database enters `/ready` and gates readiness by default; one that must not is bound with `credo.OptionalReadiness()` ([ADR-016](016-health-checks.md#optional-readiness)).
- **An optional start is the module's.** `sqldb.WithOptionalStart()`, an `Open` option, makes a failed ping non-fatal: `Start` logs it at Warn, with the component's name, the deadline and the error, logs the pool warnings as after a successful ping, and returns nil, leaving the pool open, since `database/sql` opens connections on demand once the server is back. `Ready` keeps pinging, so the database gates readiness until it answers unless it is bound with `credo.OptionalReadiness()`. Only the module knows that its value is usable after a failed ping, so the option is the module's, and ADR-024's failed-start rule is unchanged; a component in general whose failed `Start` does not fail the start stays deferred, since it would need a reconnect contract.
- **The pool warning is logged by `Start`** ([Connection-Pool Policy](#connection-pool-policy)), with `module=sqldb`, to `sqldb.WithLogger`'s logger when one is given, else to the App's logger scoped to the component, which adds `component=<name>`.
- **A borrowed database is not pinged.** Its owner starts it, so the App never calls its `Start` or `Shutdown`; readiness still reports it, since `/ready` asks a borrowed value's `Ready`.
- **One resource, one entry.** A raw handle and a wrapper that embeds it, bound together, are one resource ([ADR-024](024-lifecycle-components.md#resource-identity-one-resource-one-teardown)): one ping, one readiness entry and one teardown, after the consumers of both. The registrations that hold the resource agree on the options that shape its entry — `credo.OptionalReadiness()` on all of them or on none, and one `credo.ComponentName` name, which the entry and the teardown carry — and a disagreement is refused, naming both registrations, the option and the remedies: the same options on both, or a named field. A wrapper with readiness of its own, one that overrides the `Ready` it embeds, holds the handle in a named field, so that it is a resource of its own.
- **Several databases** stay wrapper types, each bound once, named with `credo.ComponentName` on its binding; a consumer names its database in its constructor signature. [Several Instances of One Type](004-dependency-injection-and-infra.md#several-instances-of-one-type) records the rule.
- **An adapter other than `sqldb`** implements `Start`, `Ready` and `Shutdown` itself, as every component does.

`store.Health` is what `sqldb`'s `Health` returns and its `Ready` reads. `store/` has no lifecycle interface for adapters: without a registration whose type parameter it would constrain, no code would check it — the root cannot import `store` — so it would be a contract frozen at v1 that nothing enforces.

**Rejected alternatives:**

- **A store registration that takes the store's value**, pings it at the call and publishes it itself — after an override it would ping, report and shut down a value DI does not hand its consumers. The start pings and `/ready` reports the bound value, overrides included.
- **A separate health handle passed as a value**, with its own flag for caller ownership — it drifts from an override the same way, and lets ping, health and shutdown target different objects; a wrapper type gives the same reach with one value, and `credo.Borrowed()` gives caller ownership.
- **A store-specific ownership ledger** reserving name, type and resource identity — the component registry's resource identity covers every binding.
- **A generic handle in `sqldb`** (`sqldb.Of[Analytics]`) for several databases — still a marker type per database; the wrapper type is that marker with a better name.
- **`store.Register` kept as optional sugar** beside the plan from the binding's type: two ways to one ping and one readiness entry, with ordering and duplicate-name rules between them.
- **The health engine recognizing `store.Lifecycle` on any component:** the root cannot import `store`, and an internal seam for one interface rebuilds the store registry under another name.
- **Readiness without a start ping:** it drops the guarantee that an unreachable database fails the start.
- **Moving `store.Health` into the root** to keep the typed status in `/ready`: it inverts the dependency direction.
- **Keeping `store.Lifecycle` as the adapter contract:** once `store.Register` goes, no code checks it.
- **A start forgiven for every registration whose readiness is optional:** the App cannot know whether a component is usable after its failed `Start`, which is why `WithOptionalStart` is the module's.
- **An optional component whose dependants are refused at `Finalize` and whose failed `Start` lets the App continue:** unusable for a database that repositories use, and the database stays closed after its failed `Start`.
- **A module option that makes `Ready` succeed:** an entry that reports up while its database is down.

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
- A store costs one call, its binding; an unreachable database fails the start in its `Start`, unless it is opened with `sqldb.WithOptionalStart()`
- A custom adapter implements `Start`, `Ready` and `Shutdown` with no framework interface to name them; identity sharing stays explicit through `ResourceIdentifier`, and the framework does not guess ownership by scanning arbitrary fields

**Negative:**

- Applications using GORM lose the first-class adapter — must use raw DI
- Context-based TX uses `context.WithValue` (type-safe via generics, but invisible in function signatures compared to explicit TX passing)
- Query builder proxies add a thin layer over Bun's API that must be kept in sync with Bun releases
- Bun version is pinned by `store/sqldb/` module — coordinated upgrades, each one checked by the count conformance tests of [Bun Through Its Exported API](#bun-through-its-exported-api) on SQLite, PostgreSQL and MySQL
- A paginated query the wrapper cannot prove — built with an escape hatch, a raw projection without `GroupExpr` or `Distinct`, or a model with a `BeforeSelect` hook — costs its repository a count of its own (`PageWithCount`)
- Error mapping is driver-family-aware but cannot cover every custom driver until a classifier seam exists
- Every bound database gates readiness unless it is bound with `credo.OptionalReadiness()`
- `sqldb` imports the root package for `credo.InfraFrom` and is released with the root it requires
