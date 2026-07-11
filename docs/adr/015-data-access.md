# ADR-015: Data Access

**Status:** Accepted **Date:** 2026-03-04 **Depends on:** ADR-004, ADR-005

## Context

Credo needs a data access layer that lets applications work with SQL databases without coupling the framework core to any ORM. The design must:

- Keep the core package (`store/`) free of external dependencies
- Provide universal semantic error types that HTTP and future gRPC policy can map independently
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

### Semantic Error Types

The stdlib-only `fault` leaf package defines transport-neutral `Kind` values
and a `Provider` interface. Both root HTTP policy and `store` import that leaf;
root never imports the feature package, so `store → credo` registration does
not create a cycle. `store.Kind` aliases `fault.Kind` for application-facing
data-access code.

`store.Error` preserves structured diagnostics without placing them on the
wire:

```go
type Error struct {
    Kind       store.Kind
    Op         string
    Resource   string
    Constraint string
    Code       string
    Transient  bool
    Cause      error
}
```

Exact sentinels distinguish not-found, already-exists, persistent constraint,
serialization, deadlock, lock/busy contention, timeout, unavailable, and
read-only conditions. `ErrDuplicate` remains an alias of `ErrAlreadyExists`.
`ErrConflict` remains a deprecated compatibility umbrella: constraint,
serialization, deadlock, and contention errors still match it through
`errors.Is`, while their exact `Kind` stays available.

`Transient` means only that the condition may clear. It never promises that
replaying a statement, transaction callback, or externally visible operation
is safe. Commit outcome is deliberately not inferred from a driver cause kind;
an `OutcomeUnknown` model is deferred until operation-phase evidence can make
it reliable.

Credo root maps `fault.Provider` after `*HTTPError` and before the legacy
`HTTPStatus()` seam. This preserves service/domain override through
`NewHTTPError(...).WithInternal(err)` while giving future gRPC policy the same
semantic input. The store sentinels retain `HTTPStatus()` only as a deprecated
compatibility bridge; semantic `Kind` is the primary contract. Unknown kinds
fail closed as 500. Constraint names, driver codes, resources, and causes are
never serialized by the default Problem Details renderer.

### Context-Based TX

- `store.NewTxScope[T]()` fixes the transaction contract at scope construction. Its `WithTx`, `GetTx`, `RequireTx`, and `Conn` methods must all use that same `T`, preventing a concrete/interface mismatch from silently selecting a fallback connection.
- Each `sqldb.DB` owns a distinct `TxScope[bun.IDB]`, isolating multiple logical databases that share Bun's interface type.
- `sqldb.RunInTx(ctx, db, fn)` starts a transaction, stores it in that DB's private scope, executes the callback, and commits/rolls back. Nested calls use savepoints.
- `sqldb.DB.Conn(ctx)` exposes the active `bun.IDB` for transaction-aware native Bun operations; `RequireTx(ctx)` returns `store.ErrTxMissing` instead of falling back.

This is an **opt-in convenience**. Proxy users participate automatically when they pass the callback context. Custom adapters use their own typed scope. The older unscoped `store.WithTx` / `GetTx` / `Conn` helpers remain deprecated compatibility APIs because type-only context keys cannot protect a producer/consumer concrete-interface mismatch or isolate same-type logical connections.

### 3-Tier Bun Wrapping

The `sqldb.DB` wrapper applies three strategies to Bun's API:

1. **Own** — lifecycle methods (`Open`, `Close`, `Ping`, `Health`). The wrapper manages the connection pool.
2. **Enrich** — query builder proxies (`Select`, `Insert`, `Update`, `Delete`) that inject TX from context and map errors to semantic `store.Error` values. DB-level query-hook tracing is planned for Phase 3.5 and is not installed today.
3. **Passthrough** — `Conn(ctx) bun.IDB` for transaction-aware native Bun work and `Client() *bun.DB` for migrations, model registration, or operations intentionally tied to the base pool.

### Connection-Pool Policy

Credo does not invent a workload-independent connection-pool size. `MaxOpen=0`
retains `database/sql`'s unlimited-open behavior; silently choosing a finite
default could either under-provision a busy service or overrun a smaller
database. Instead, a pool that is still unlimited when canonical
`store.Register` inspects it emits one structured warning with code
`sqldb.pool.max_open_unlimited`. Standalone users that do not call
`store.Register` can inspect `DB.StoreRegistrationWarningCodes()` and route the
same secret-free code through their own bootstrap logging.

Pool settings preserve the distinction between absence and an explicit value:

- `MaxIdle == nil` means Credo does not call `SetMaxIdleConns`; the effective
  `database/sql` default remains subject to `MaxOpen`. `MaxIdle: new(0)`
  disables idle retention, and a positive value is applied exactly.
- When `MaxOpen > 0`, an explicit `MaxIdle` greater than `MaxOpen` is rejected
  at `Open`; Credo does not rely on `database/sql`'s silent clamp.
- `MaxIdleTime == 0` disables idle-age expiry; a positive value is passed to
  `SetConnMaxIdleTime`.
- `MaxLifetime == 0` disables lifetime expiry; a positive value is passed to
  `SetConnMaxLifetime`.

`DB.Stats()` exposes the complete `sql.DBStats` snapshot. SQL health metadata
includes current open/in-use/idle counts plus cumulative wait duration/count
and idle-time, idle-count, and lifetime closure counters. Readiness does not
serialize adapter `Details`, so these values are operational diagnostics for
code and future metrics rather than an accidental public probe schema.

Pool saturation does not currently produce `StatusDegraded`. A useful signal
requires production metrics, an explicit SLO, windowed counter deltas,
hysteresis, and opt-in thresholds. A universal instantaneous threshold would
be noisy; because every store is currently critical and `DEGRADED` removes
readiness, such noise could also trigger a replica-wide traffic cascade.

### Registration

`store.Register[R](app, value, opts...)` is the unified registration function.
Its ownership and publication boundary is the successful return:

- If `value` implements `Lifecycle`, that same object supplies Ping, Health,
  and Shutdown. A successful registration makes it framework-owned; the DI
  container is its sole framework shutdown owner. During one teardown DI makes
  at most one Shutdown attempt if the still-live deadline reaches the
  registration in reverse order; deadline exhaustion may skip it entirely.
- If `value` cannot implement `Lifecycle`, a separate `WithLifecycle(lc)`
  handle is accepted only with `WithCallerOwnedLifecycle()`. The handle
  supplies Ping and Health, while the caller remains responsible for Shutdown.
  `WithLifecycle` by itself is an error rather than an implicit warning-only
  ownership transfer.
- A Lifecycle value with an explicit lifecycle or caller-owned option is
  rejected. A Shutdowner-only value cannot be paired with a separate lifecycle
  either. These checks prevent Ping/Health and Shutdown from silently targeting
  different objects.
- Every failure, including Ping and final DI publication failure, leaves
  ownership with the caller.

Registration proceeds as a guarded commit:

1. Validate value, name, lifecycle/ownership combination, and the DI
   container's point-in-time ability to provide `R` before network I/O.
2. Resolve or create the Registry and idempotently wire readiness to that
   exact instance.
3. Privately reserve the canonical store name, value type, and declared
   resource identity. Existing and pending duplicates fail before Ping;
   pending entries are invisible to `HealthAll` and readiness.
4. Re-check DI, then call Ping synchronously with the configured deadline.
   Lifecycle implementations must honor the context; Register cannot
   hard-bound a non-cooperative Ping.
5. Publish `R` through `ProvideProtectedValue` and commit the Registry entry as
   one Registry-visible transition. A failed authoritative publication
   releases the reservation and exposes no health entry. Protection prevents a
   later `Replace[R]` from detaching DI from tracked lifecycle/health state.

`App.CanProvideValue[T]` performs only frozen-container and direct duplicate-T
preflight. It neither mutates nor reserves the DI registration, so external
concurrent mutation may still make the final protected publication fail; that
final call remains authoritative.

Resource identity is explicit rather than reflection-derived. By default the
top-level Lifecycle value identifies itself; pointer-backed implementations are
recommended. The optional `store.LifecycleIdentityProvider` extension lets a
semantic wrapper return the underlying resource pointer or another stable
token via `ResourceIdentity() any`. Tokens must be non-nil, comparable,
reflexively equal, and stable for the resource lifetime, so non-comparable
values and NaN-like tokens fail before Ping. Credo does not scan wrapper fields.
A named wrapper must forward identity explicitly; an embedded `*sqldb.DB`
inherits its provider method through ordinary Go method promotion. A composite
pointer Lifecycle remains valid as its own resource.

Within one `store.Register` Registry, identical tokens are rejected across
concrete/interface registrations, explicit wrapper types, and mixed
framework/caller ownership. If application code needs an interface view of an
existing store, it uses `app.Alias[I, T]()` instead of a second `Register`; the
alias resolves the same singleton without adding another health entry or
shutdown owner.

This uniqueness guarantee is store-ledger-scoped, not container-wide. Raw
`app.Provide`, `app.ProvideFactory`, `app.ProvideValue`,
`app.ProvideProtectedValue`, or `app.Replace` under another T can bypass it and
publishing the same Lifecycle that way is unsupported: DI may acquire
contradictory ownership or attempt Shutdown more than once through distinct
registrations. A caller-owned handle must not also be registered in DI as a
Shutdowner. A general resource registry is deferred until a second concrete
infrastructure subsystem needs the same primitive.

Store names share the named-health validator. Explicit empty, padded,
control-character, and reserved `credo.` names are rejected rather than
normalized. When `WithName` is omitted, pointer layers are unwrapped and the
package-qualified name of a named `R` is used; unnamed types require an
explicit name.

The Registry exposes only the read-only `HealthAll` view. Its former public
`Add` method is removed because an externally added health-only entry bypassed
Ping, DI publication, and shutdown ownership. The Registry itself never closes
connections, eliminating the historical DI-plus-Registry double shutdown. A
new Registry is published protected. A composition-root-provided Registry is
first resolved and validated, then passed as the expected value to
`ProtectBinding`. This compare-and-protect is atomic with `Replace`: a changed
or non-comparable resolved value fails without protecting the wrong binding.
The Registry is re-resolved before wiring. A nil value or failing constructor
also remains unprotected so composition can repair it with `Replace` before
Finalize and retry. `Replace[*store.Registry]` and `Replace[R]` consequently
fail only after successful store adoption rather than disconnecting readiness
or leaking an untracked resource.

`store.Register` is a free function rather than a root `App` method, even though the DI surface itself is method-based (`app.Provide[T]` / `app.Resolve[T]`): the `store` package cannot add methods to `*credo.App` (Go forbids methods on a type from another package), and having the root import `store` to host the method would invert the dependency direction the architecture enforces. `worker.Register` stays a function for the same reason — feature-package registration lives in the feature package, while only the generic container surface lives on `App`.

### Bun-Only

The framework ships a single SQL adapter (Bun). Other ORMs work via raw DI registration (`app.ProvideValue[*gorm.DB](db)`). The `store/` contracts (errors, health, registry) are ORM-agnostic and can be used with any backend.

## Consequences

**Positive:**

- Semantic `fault.Kind` values allow independent HTTP/gRPC policy without a root→store cycle
- Single ORM focus allows deep integration (query proxies, error mapping, pagination helpers) instead of shallow generic interfaces
- Context-based TX is opt-in — repositories that don't need TX are simpler
- `store/` contracts remain ORM-agnostic — custom adapters possible
- Separate submodule keeps Bun dependency out of core
- `Client()` escape hatch prevents the wrapper from becoming a bottleneck
- Registration has an explicit ownership-transfer boundary: DI is the sole
  framework shutdown owner for successful direct Lifecycle values, making at
  most one attempt per teardown when the live deadline reaches each entry;
  explicit caller-owned values remain outside DI shutdown and failures remain
  caller-owned
- Private name/type/resource-identity reservations prevent duplicate
  concurrent registration from leaking pending health entries
- Protected store and Registry bindings prevent later Replace calls from
  diverging DI resolution from lifecycle/readiness state

**Negative:**

- Applications using GORM lose the first-class adapter — must use raw DI
- Context-based TX uses `context.WithValue` (type-safe via generics, but invisible in function signatures compared to explicit TX passing)
- Query builder proxies add a thin layer over Bun's API that must be kept in sync with Bun releases
- Bun version is pinned by `store/sqldb/` module — coordinated upgrades
- Error mapping is driver-family-aware but still cannot cover every custom driver without an explicit classifier seam
- Named-field wrappers that do not implement Lifecycle must explicitly opt into
  caller-owned shutdown; this is more ceremony but removes ambiguous ownership
- Identity forwarding by semantic wrappers is explicit; the framework does not
  guess ownership by scanning arbitrary fields
- Store-ledger uniqueness does not cover raw `app.Provide`,
  `app.ProvideFactory`, `app.ProvideValue`, `app.ProvideProtectedValue`, or
  `app.Replace` publication of the same lifecycle; callers must not bypass
  Register for another ownership view
- The identity ledger remains store-specific until another infrastructure
  subsystem justifies a general resource registry

## SelectQuery Curated Set

**SelectQuery curated set expanded** with `Join`, `JoinOn`, `JoinOnOr`, `TableExpr`, `ColumnExpr`, and `ExcludeColumn`.

**Rationale**: the original curated set forced every non-model JOIN query (reporting, auth, analytics) through the `Apply` escape hatch, turning an "advanced" path into the normal one. Adding these six methods eliminates that friction without API breakage — all return `*SelectQuery` (fluent), and interceptors (TX injection, error mapping) are preserved because terminal methods are unchanged.

**Client() escape hatch documented**: `Client()` now carries an explicit GoDoc warning that queries via `*bun.DB` bypass TX injection and error mapping. Spec and guide updated accordingly.

**Optional model arity is strict**: `Select`, `Insert`, `Update`, and `Delete` accept zero or one optional model. Supplying more than one causes the builder to record an error that the terminal returns without executing; no argument is silently ignored.

**Curated LIMIT/OFFSET narrowing is strict:** Bun v1.2.18 accepts `int` in `SelectQuery.Limit`/`Offset` but stores the result in signed `int32` fields. Credo's curated methods now range-check before delegation, recording `sqldb.ErrInvalidLimitOffset` so the terminal fails before database execution instead of silently using a narrowed value. The full in-range signed-int32 domain is preserved, including Bun's zero/negative semantics. `Apply` and `Unwrap` are raw Bun escape hatches, so calls made through them deliberately remain governed by Bun's conversion contract.

## ApplyQueryBuilder

**`ApplyQueryBuilder` added** to `SelectQuery`, `UpdateQuery`, and `DeleteQuery`:

```go
func (q *SelectQuery) ApplyQueryBuilder(fn func(bun.QueryBuilder) bun.QueryBuilder) *SelectQuery
```

**Rationale**: the typed `Apply` escape hatch is per-query-type (`func(*bun.SelectQuery) *bun.SelectQuery` cannot be applied to an update or delete). A predicate shared across reads and writes — soft-delete filters, account scoping, ownership checks, and authorization scopes — therefore had to be duplicated once per query type. Bun's native `QueryBuilder()` exposes a builder-only interface (`Where`, `WhereOr`, `WhereGroup`, `WherePK`, `WhereDeleted`, `WhereAllWithDeleted`) common to select/update/delete; `ApplyQueryBuilder` surfaces it through the proxy so one `func(bun.QueryBuilder) bun.QueryBuilder` predicate applies to all three. As a bonus it unlocks `WhereGroup`, which the curated proxy set does not expose directly.

**Form — `ApplyQueryBuilder(fn)`, not a raw `QueryBuilder()` accessor**: exposing `QueryBuilder()` directly would return a bun interface that breaks the proxy fluent chain and is essentially a second `Unwrap` (`bun.QueryBuilder` carries `Unwrap() any`). `ApplyQueryBuilder(fn)` instead returns the proxy type (fluent, like `Apply`/`Conn`), contains the bun type inside a function boundary, and mirrors Bun's own `ApplyQueryBuilder`. Conditions added through the builder land on the proxied query, so terminal methods still apply TX injection and error mapping — interceptors are preserved, verified against bun v1.2.18 (`selectQueryBuilder` wraps the same `*bun.SelectQuery` pointer; Where-family methods mutate in place). The builder's `Unwrap() any` remains a terminal-bypass escape — the same caveat already documented for `Unwrap()`, and no easier to misuse than today's `Apply`, which hands out the concrete query directly.

**Bun type leakage** is the one real cost: a shared predicate's signature is `func(bun.QueryBuilder) bun.QueryBuilder`, importing `bun` into repository code. This sits at the same documented escape-hatch tier as `Apply`/`Unwrap`/`Conn`/`Client` and is positioned as an escape hatch, not the default path. If bun coupling later proves painful, the follow-up is a Credo-owned `WhereScope` interface (Where/WhereOr/WherePK only, no `Unwrap`) — deferred until real pain appears (it would reinvent bun's interface and cannot cheaply express `WhereGroup` recursion).

**Scope**: select/update/delete only. `InsertQuery` is excluded — it has no WHERE clause, matching Bun's own `QueryBuilder` interface assertions. No API breakage; additive only, all three return the proxy type (fluent), and a nil fn is a no-op.

## Typed Terminals (One[T] / All[T] / Page[T])

**`One[T]`, `All[T]`, and `Page[T]` added** to `SelectQuery` as generic terminal methods (Go 1.27 concrete-type generic methods):

```go
func (q *SelectQuery) One[T any](ctx context.Context) (T, error)
func (q *SelectQuery) All[T any](ctx context.Context) ([]T, error)
func (q *SelectQuery) Page[T any](ctx context.Context, req *pagination.PageRequest) (*pagination.Page[T], error)
```

**Rationale**: the existing terminals (`Scan(ctx, &dest)`) require the caller to declare a destination and pass it back in — `var u User; err := db.Select(&u).Where(...).Scan(ctx)`. With generic methods finally usable on a concrete type, the destination becomes the type parameter: `u, err := db.Select().Where(...).One[User](ctx)`. `T` drives both the FROM table (model inferred from `T`) and the scan destination, so `Select` is called model-less and the terminal owns the destination. A model bound through `Select`, `Model`, or `Apply` is not overridden: `One`, `All`, and `Page` return `sqldb.ErrTypedTerminalModel` before executing a query.

**Semantics (locked by tests, not left to Bun)**: `One` applies an explicit `LIMIT 1` and returns the first matching row, so multiple matches are not an error (callers add `OrderExpr` for a deterministic choice); a missing row maps `sql.ErrNoRows` to `store.ErrNotFound` and returns the zero `T`. `All` returns a non-nil empty slice with a nil error when nothing matches — an empty list is not `ErrNotFound`. Both execute an internal snapshot that preserves explicit connection, builder error, `WherePK`, soft-delete flags, CTE materialization flags, and Bun's model/relation clone behavior before adding the terminal-owned model and ambient transaction. The receiver's top-level builder state is never mutated and a terminal's model or `LIMIT 1` never leaks into a sequentially reused query.

**Name — `One`, not `First`**: the terminal is named for its return shape (`One → T`, `All → []T`), consistent with the result-shape naming used elsewhere in the data layer; it is a single-row read, not an exactly-one assertion. For "exactly one or error", a caller composes `Count`/`Exists`. `Scan` stays for projection/DTO cases where `T` is not the table model and for eager loading: use `TableExpr(...).Scan(ctx, &dest)` for an explicit-source projection, or bind the model and call `Model(&dest).Relation(...).Scan(ctx)` for relations (the equivalent `Select(&dest)` form also works). `TableExpr` does not turn a typed terminal into a projection API; typed `T` must be the actual table model.

**`Page[T]` completes the set** with the same Form-A model (`T` drives the table and destination, the query is built model-less, and the terminal owns the result). It runs COUNT + a LIMIT/OFFSET SELECT and assembles a `*pagination.Page[T]`, finishing the result-shape naming (`One → T`, `All → []T`, `Page → *Page[T]`) — the noun `Page` is chosen over the verb `Paginate` precisely because the terminal is named for its return shape. Internally it uses the shared logical-count path for the total (injecting `T`'s model so the model-less query still knows its table) and `All[T]` for the page slice, each on a private execution snapshot so `Offset`/`Limit` never leak into the receiver. `sqldb` already imports `pagination`, so there is no new dependency or cycle. Offset pagination still requires a stable total order chosen by the repository; append a unique tie-breaker such as `id` when the primary sort key is not unique.

**Public `Clone` is a builder-fork API, not the terminal snapshot API.** Credo patches the execution fields Bun v1.2.18 omits and otherwise retains Bun's clone semantics. It is a top-level builder fork, not a recursive object-graph copy: bound destinations and nested CTE/relation query values may remain shared. Source and clone must not mutate or scan shared values concurrently.

**Normalization policy and execution invariants are separate:** `PageRequest.Normalize` and its `Validate` hook remain forgiving, mutating input-policy operations: they apply defaults and clamp `PerPage`. `PageRequest.Offset` is deliberately strict and non-mutating, and now returns `(int, error)` so non-positive values or native `int` multiplication overflow cannot become a silent offset. This is a pre-v1 source break in favor of explicit correctness; callers must handle `pagination.ErrInvalidPageRequest`.

`Page` takes its own value snapshot of `req` and never re-normalizes or mutates the caller's object. Before COUNT, it rejects nil, `Page < 1`, `PerPage < 1`, native offset overflow, and any limit/offset outside Bun v1.2.18's signed-int32 representation; all failures wrap `pagination.ErrInvalidPageRequest`. The Bun guard is adapter-specific because Bun's public methods accept `int` but its pinned internal fields are `int32`, and its conformance test must be revisited on upgrade. This strict boundary does not impose the package's default cap: a valid custom `PerPage` above 50 remains honoured. When COUNT reports zero rows SELECT is skipped and the result is `NewPage([]T{}, 0, snapshot.Page, snapshot.PerPage)`, preserving the requested metadata with a non-nil empty slice.

**Total means complete logical projection-row cardinality.** It is computed before ordering and the Page-owned LIMIT/OFFSET window. Credo clones the SELECT, removes root ORDER/LIMIT/OFFSET/FOR state, and counts the resulting universal `_credo_count_source` derived table. A plain projection counts its produced rows; an ungrouped aggregate normally produces one row (including `COUNT(*)` over an empty input); `Distinct` counts distinct selected projection tuples; `Group` counts groups; and `Group` + `Having` counts only groups left after the `Having` filter. Behavioral and generated-SQL conformance tests pin this Credo-owned wrapper instead of relying on Bun v1.2.18's narrower grouped/distinct count rewrite. Bun does not give standalone `Having` or a direct `UNION`/`INTERSECT`/`EXCEPT` root a safe Count+window contract, so `Count` and `Page` return `ErrUnsupportedCountQuery` before database I/O. Advanced callers put the compound query behind an outer derived-table/CTE source, run an explicit count query and data query, then construct `pagination.NewPage`.

**MySQL projection names use the server as oracle.** MySQL requires every derived-table output name to be unique, but a local SQL parser cannot safely reproduce the server's naming rules across expressions, wildcards, comments, and session modes. After model hooks, Credo therefore renders the count source exactly once and lets MySQL evaluate those exact bytes under the connection's real `sql_mode`. If the generated logical COUNT returns `ER_DUP_FIELDNAME` (1060 / SQLSTATE `42S21`), the Count/Page execution point wraps `ErrUnsupportedCountQuery` while preserving the original driver cause. This is deliberately local: raw SQL, `Scan`, `Exists`, and every other non-count 1060 pass through unchanged, and the global MySQL classifier does not map 1060. Wildcards, implicit aliases, and unaliased expressions are accepted whenever the server derives unique output names; explicit unique aliases remain the remedy for a collision. Real MySQL conformance runs both normal mode and `NO_BACKSLASH_ESCAPES` and pins Count, Page, positive former false positives, cause preservation, and non-count passthrough. Because the server does not report which derived-table level produced 1060, a caller-supplied nested derived table that fails during logical COUNT receives the same wrapper. The retained driver cause is for diagnostics and must not be rendered directly to HTTP. PostgreSQL and SQLite are unaffected. See the [MySQL derived-table contract](https://dev.mysql.com/doc/mysql/en/derived-tables.html).

**Render-time relation state is guarded.** Bun applies relation callbacks from `AppendQuery`, after ordinary builder composition. Credo renders the private count source exactly once, revalidates it after callbacks, and embeds those same validated bytes in the outer count. Relation predicates and projections remain supported. A callback that replaces the model, reintroduces root ORDER/LIMIT/OFFSET/FOR, or creates standalone `Having`/a compound root returns `ErrUnsupportedCountQuery` before database I/O; render-time mutation cannot silently constrain the total or bypass shape guards.

The count source evaluates the logical projection. That is required for exact cardinality of aggregates and set-returning expressions, but a costly or volatile projection can therefore execute once for COUNT and again for the data SELECT. Such repositories should provide a deliberately equivalent, cheaper count query and compose it with the data query and `pagination.NewPage`. Credo does not add a first-class custom-count callback or strategy yet: the existing `ApplyQueryBuilder`/`Apply` + explicit `Count` + data query + `NewPage` composition is sufficient, while a callback cannot prove that two divergent queries describe the same logical set. Reconsider only after two real consumers repeat the same abstraction.

**Model SELECT hooks are part of logical cardinality.** The private count source runs `BeforeSelect`, `BeforeAppendModel`, and—only after a successful count query—`AfterSelect`. A Page whose data statement executes therefore invokes the lifecycle once for COUNT and once for SELECT; hook-added predicates or projections cannot be silently omitted from `Total`. The outer query retains the model in `QueryEvent.Model` for observability hooks, but its FROM is `_credo_count_source`; soft-delete policy is applied only inside that source so the outer count does not add a second predicate. Count scans no model rows and does not mutate a bound model, so its `AfterSelect` observes the pre-count model value. Hooks must be deterministic: Repeatable Read stabilizes database visibility, not a volatile projection or application-side decision made independently for the two statements.

`Page` retains an exact-total response contract: `Total`, `TotalPages`, and the derived navigation methods are always meaningful. An unknown total is not encoded as zero, `-1`, a pointer, or an omitted JSON field. A total-free offset window needs a separate future `Slice` shape and is designed together with cursor pagination; it does not become an option that changes `Page` semantics.

### Cursor/keyset pagination gate — design accepted, implementation deferred

No cursor symbols are exported yet. The first delivery is reserved as a
forward-only `CursorPage[T]`, not `Slice[T]`: `Slice` is only the working name
for a future total-free **offset** window and has its own design gate. The HTTP request uses `after` and
`per_page`; the response uses a non-nil `data` array plus
`meta.per_page`, `meta.has_next`, and nullable `meta.next_cursor`. It has no
total, page number, previous cursor, direction switch, or `WithTotal` option.
Callers that truly need a total compose a separate explicit `Count` and own the
application response envelope; `CursorPage` itself never runs COUNT.

The sqldb terminal will own an immutable keyset specification and reject a
caller-supplied root ORDER/LIMIT/OFFSET/lock or an aggregate, distinct, grouped,
compound, joined, or otherwise non-row-shaped source. A join can multiply a
root model row, invalidating the claimed unique tie-breaker. Every ordering
component must be selected, immutable, and non-null, and the final component
must be an explicit unique tie-breaker. The adapter will quote column
identifiers, format cursor values as typed data through Bun without raw token
concatenation, append the portable lexicographic OR-of-AND ladder as one
parenthesized condition, and fetch `per_page + 1` rows to derive `has_next`. It
will not use a row-constructor
comparison: the expanded form is portable across the supported databases and
can give MySQL more complete index use. See the
[MySQL row-constructor optimization guidance](https://dev.mysql.com/doc/refman/8.0/en/row-constructor-optimization.html).

Nullable keys are excluded from the first delivery. Row comparisons become
unknown around NULL, and default NULL placement is not portable: PostgreSQL
places NULL last for ascending order by default, while MySQL and SQLite place
it first. See [PostgreSQL row comparison](https://www.postgresql.org/docs/current/functions-comparisons.html),
[PostgreSQL ordering](https://www.postgresql.org/docs/current/queries-order.html),
[MySQL NULL ordering](https://dev.mysql.com/doc/refman/8.0/en/null-values.html),
and [SQLite type ordering](https://www.sqlite.org/datatype3.html).

The first delivery is narrower still: only a model-less default full-model
SELECT plus curated predicates is accepted. Custom projections/tables, raw
`Apply`, `WherePK`, top-level `WhereOr`, and hook-capable models fail pre-I/O.
`WherePK` cannot resolve before the typed terminal supplies its model. OR
filters must already be enclosed in one `WhereGroup` whose root separator is
AND; this prevents `A OR B AND cursor` precedence from leaking rows before the
boundary. This ensures every key is scanned into `T` and prevents pre-query or
post-scan hooks from changing the model,
query shape, order/window, or cursor key behind the terminal's back. The strict
request boundary also checks native addition and requires `per_page` to be at
most Bun's signed-int32 maximum minus one.

Cursor integrity has no hidden fallback. The planned public-HTTP codec requires
an explicit HMAC-SHA256 keyring and key identifier; Credo will not generate a
process-local secret or silently issue unsigned cursors. Keys are at least 32
cryptographically random bytes generated by a CSPRNG; code can enforce length,
while entropy remains an operator contract. Exactly one signs, previous keys
verify only, key-id lookup is direct, comparison is constant-time, and
tuple/keyring/payload/token sizes are bounded before parsing. A fixed Credo
cursor domain separator prevents cross-protocol
MAC reuse; key material is never serialized, logged, or returned in an error.
The signed data binds the token version, endpoint/query scope, canonical
ordering, normalized filter scope, and tenant/authorization scope so a cursor
cannot be replayed under a different query. The envelope must be versioned,
key-id-addressed, base64url-safe, and integrity-protected, but canonical tuple
encoding, key-id grammar, padding, scope canonicalization, MAC byte framing,
and golden vectors wait for the consumer. Expiry and rotation-retention
duration likewise wait for the consumer's request-lifetime needs; a verify-only
key cannot be retired safely without that horizon.

HMAC authenticates but does not encrypt: key values remain
observable, sensitive keys are outside the first delivery, and an AEAD or
server-side cursor is a later explicit codec. See [RFC 2104](https://www.rfc-editor.org/rfc/rfc2104.html).

The cursor is never an authorization capability. Each request re-applies
authentication, authorization, tenant predicates, and normalized filters;
scope binding only prevents cross-query replay. It is opaque to clients by
contract, but its signed payload remains visible.

Across requests, keyset pagination promises no database snapshot. Inserts or
deletes strictly before the cursor boundary do not shift later windows; new
rows after the boundary may appear, and deleting an unseen row removes it.
Updating an ordering key can still duplicate or skip a row, which is why cursor
keys must be immutable. Deterministic mutation tests must pin this contract and
contrast it with offset drift before the API ships.

The implementation gate remains closed for five concrete reasons:

1. no real Credo consumer has supplied an actual model/filter/order and key
   rotation requirement to validate the public generics;
2. Bun v1.2.18 hooks can mutate the model/query before render or cursor keys
   after scan; Credo must either reject hook-capable models before I/O or prove
   model/order/window/key alignment across the whole lifecycle;
3. PostgreSQL, MySQL, and SQLite conformance jobs must all pin the generated
   predicate, ordering, collation assumptions, insert/delete behavior, and
   typed round trips for large integers, time, strings, and consumer UUID or
   decimal keys;
4. invalid cursor input needs a transport-neutral invalid-argument kind pinned
   through the root renderer, observer, and access-log status paths; and
5. canonical wire framing needs cross-implementation golden vectors.

Until those gates open, applications use an explicit repository-owned keyset
query and token codec. Credo does not publish a speculative half-contract whose
ordering or signature policy would later require silent behavior changes.

**Snapshot consistency belongs to the caller's transaction boundary.** `Page` never starts an implicit transaction. Without one, COUNT and SELECT may run on different pool connections and snapshots. Even inside a transaction, the actual guarantee comes from the database, engine, configured isolation, read form, and driver. When a shared snapshot matters on PostgreSQL or InnoDB, establish the isolation on the outermost transaction and pass the callback's `txCtx` to `Page`:

```go
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

Non-default options cannot be introduced by a nested savepoint; Credo returns `ErrNestedTxOptions`, so the owner of the outer transaction must choose them. Database-specific boundaries are:

- **PostgreSQL:** default Read Committed takes a fresh snapshot for each statement, so COUNT and SELECT can drift. Repeatable Read uses the snapshot established by the transaction's first non-control statement. See [PostgreSQL transaction isolation](https://www.postgresql.org/docs/current/transaction-iso.html).
- **MySQL:** InnoDB defaults to Repeatable Read, and ordinary nonlocking consistent reads in one transaction share the snapshot established by the first such read. Server isolation can be reconfigured, other engines and locking reads differ, and Read Committed refreshes the snapshot per read; request explicit Repeatable Read instead of relying on deployment defaults. See [InnoDB isolation levels](https://dev.mysql.com/doc/refman/8.4/en/innodb-transaction-isolation-levels.html) and [consistent nonlocking reads](https://dev.mysql.com/doc/refman/8.4/en/innodb-consistent-read.html).
- **SQLite:** an explicit read transaction keeps the snapshot established by its first read. WAL permits another connection to commit while that reader keeps its snapshot; rollback-journal mode may block the writer instead. Shared-cache connections with `PRAGMA read_uncommitted=ON` are the documented exception. See [SQLite isolation](https://www.sqlite.org/isolation.html) and [transactions](https://www.sqlite.org/lang_transaction.html). The pinned modernc SQLite driver does not reliably enforce `sql.TxOptions.Isolation` or `ReadOnly`, so Credo recommends plain `InTx` for the SQLite snapshot boundary. Fail-loud driver capability validation is deferred.

**Overflow-safe result metadata:** `NewPage` computes `TotalPages` with quotient plus a remainder increment rather than `(total + perPage - 1) / perPage`. The result is the same ceiling division without overflowing when `total` approaches `math.MaxInt64`.

**`sqldb.Paginate` and `sqldb.PaginateRequest` removed** (BREAKING). With `All[T]` + `Count` available as terminals, the low-level `Paginate[T](ctx, q, page, perPage, &dest)` (caller-supplied destination) and its `PaginateRequest[T]` wrapper are redundant — `Page[T]` is the 1:1 replacement for the latter. For a fallible model→DTO conversion, execute the model-less query as `Page[Model]`, map its records with ordinary error handling, and construct `pagination.NewPage(dtos, modelPage.Total, modelPage.Page, modelPage.PerPage)`. They are deleted, not deprecated: pre-v1 with two controllable consumers, and keeping a second, differently-shaped pagination API alongside `Page[T]` would reintroduce exactly the surface inconsistency this campaign removes. Pagination's ORM-agnostic data shapes remain; the separate `PageRequest.Offset` signature break is the execution-safety decision above.

## Transaction Boundary Hardening

**Typed scope (breaking, pre-v1):** `TxScope` is now `TxScope[T]`, and `NewTxScope[T]()` fixes the transaction interface at wiring time. Previously each generic scope method selected its own `T`; storing a concrete transaction and reading the same scope through an implemented interface compiled but used different context key types and silently returned the fallback connection. The typed scope makes that mismatch impossible at the method boundary. `WithTx` also panics on nil transactions, including typed-nil pointers held in an interface, so `RequireTx` cannot succeed with an unusable handle. Its `RequireTx` form returns `store.ErrTxMissing` for flows where fallback is itself a correctness bug. The unscoped type-keyed helpers remain deprecated compatibility APIs; new adapters must not use them.

**Callback errors remain application values:** when a callback returns an error and rollback succeeds, `RunInTx` returns that exact error value. It does not run the callback result through SQL error mapping; domain text such as “duplicate key validation” must not accidentally acquire `store.ErrDuplicate`. If rollback also fails, only the rollback driver error is mapped and joined with the callback error, preserving both causes. Panic cleanup attempts rollback and re-raises the original panic value. A nil callback returns `ErrNilTxCallback` before opening a transaction.

**Nested options and operations fail loud:** Bun implements a nested transaction as a savepoint and ignores `sql.TxOptions` in `bun.Tx.BeginTx`. Credo accepts nil/zero options for a nested savepoint but returns `ErrNestedTxOptions` before creating it when isolation or read-only is non-default. Silent option loss is not permitted. Bun also reuses the savepoint's begin context for release/rollback; Credo controls a cancellation-detached context while callback queries retain the original context. Savepoint creation observes callback cancellation and the cleanup budget, and an uncertain begin never invokes the callback. Creation, release/rollback, and the fail-safe ambient abort each have a finite five-second default wait budget (`WithTxCleanupTimeout` overrides it); callback execution time is not counted. Before starting an asynchronous abort, Credo synchronously marks shared per-transaction state rollback-only. All outer transaction levels inspect it before commit: a level that would otherwise commit returns `ErrTxRollbackOnly`, while an existing callback or context error remains the more specific rollback result. This eliminates scheduler-dependent fail-open behavior even when a nested error is swallowed; subsequent nested calls fail before callback execution. Panic cleanup takes the same bounded fail-safe path while preserving panic identity. A driver that ignores cancellation may retain its operation goroutine/connection until it returns, but caller wait is finite and commit remains closed.

**Commit errors are outcome-ambiguous:** begin, rollback, and commit driver failures are still normalized at their own operation boundary. A commit error, however, is not generic proof that the transaction was not applied. Error classification describes the driver cause; callers must not turn every commit error into a blind retry.

**Native Bun boundary:** `DB.Conn(ctx) bun.IDB` exposes the DB's active scoped transaction, falling back to the base Bun DB outside a transaction. `DB.RequireTx(ctx)` removes that fallback. Returned transaction values are borrowed for the callback lifetime, and native operations still bypass Credo error mapping.

**ConnResolver decision — not adopted:** Bun v1.2.18 gives a resolver one owned slot and closes it from `bun.DB.Close`; explicit query connections win over it, while direct `Client().ExecContext`, `QueryContext`, `QueryRowContext`, and `BeginTx` bypass it. Credo's proxy terminals also bind an explicit connection today, so merely installing an ambient resolver would make only part of `Client()` transaction-aware and would complicate future read-replica composition and ownership. Terminal-level injection remains the default and `DB.Conn(ctx)` is the explicit native escape hatch. Revisit a composite order (`explicit query conn → active Credo TX → user/replica resolver → base DB`) only when automatic native-builder or replica routing is a demonstrated requirement.

## Semantic Error Classification Hardening

`sqldb` stores the configured driver family (falling back to the selected Bun
dialect for custom connectors) and passes both that family and the operation
context into normalization. SQLSTATE remains the primary cross-driver path;
when a MySQL error exposes both its number and a broad SQLSTATE class, the more
specific number wins (for example 1213/40001 is Deadlock, not generic
Serialization). MySQL's strict `Error NNNN (STATE): ...` envelope is parsed
only for MySQL, and SQLite's structured code is read only for SQLite.
Loose substrings such as “duplicate key” or “read-only” are never inspected on
arbitrary errors, so scanner/query-hook/domain failures retain exact identity.

Canonical classifications are:

| Source | Semantic result |
| --- | --- |
| `sql.ErrNoRows` | `NotFound` |
| PostgreSQL/MySQL/SQLite unique or primary-key violation | `AlreadyExists` |
| Other SQL integrity constraints | `Constraint` |
| PostgreSQL `40001`, SQLite busy-snapshot | `Serialization` |
| PostgreSQL `40P01`, MySQL 1213 | `Deadlock` |
| PostgreSQL lock-not-available, MySQL 1205/3572, SQLite busy/locked | `Contention` |
| Verified deadline or statement timeout | `Timeout` |
| `driver.ErrBadConn`, `sql.ErrConnDone`, SQLSTATE class 08, connect/shutdown codes | `Unavailable` |
| Driver-specific read-only transaction/server codes | `ReadOnly` |

PostgreSQL `57014` means query canceled, not timeout. Credo returns caller
cancellation without a store timeout, maps a caller deadline to `Timeout`, and
with a live context maps only a structured driver error that explicitly says
statement timeout. MySQL 1290 is not classified as read-only because it is the
generic option-prevents-statement code; the actual read-only transaction/server
codes 1792 and 1836 are used. Every mapped result is a `*store.Error` preserving
the original cause and driver code. A public classifier registry/normalizer is
deferred until a custom-driver consumer demonstrates the need.

## Migrations and TX Ergonomics

**Method-form TX sugar added**: `(*DB).InTx(ctx, fn)` and `(*DB).InTxWith(ctx, opts, fn)` delegate to `RunInTx` / `RunInTxWith`. Rationale: handler-side ergonomics (`db.InTx(ctx.Context(), fn)`) and discoverability — the operation lives on the value the developer already holds. The distinct name also avoids signature confusion with Bun's native `(*bun.DB).RunInTx(ctx, opts, fn(ctx, tx))` reachable via `Client()`.

**Migration wrapper added** as a thin wrapper over Bun's migration engine:

```go
func (db *DB) RegisterMigrations(m *migrate.Migrations, opts ...migrate.MigratorOption)
func (db *DB) Migrate(ctx context.Context) error
```

- `bun/migrate` is part of the already-pinned Bun module — no new dependency and no second migration engine.
- The `*migrate.Migrations` set is Bun's own type (`Discover(fsys)` for SQL files incl. `embed.FS`; `MustRegister` for Go migrations) — Credo does not re-wrap it.
- `Migrate` = Init (bookkeeping tables, `IF NOT EXISTS`) → Lock (table-based advisory lock, fail-fast if another runner owns it) → run pending → Unlock. Unlock starts only after a successful Lock, uses a fresh cancellation-detached five-second internal budget, and bounds caller wait with a buffered-result goroutine even if a driver ignores context. Migration and unlock errors are joined. A timeout means unlock outcome is uncertain; Bun's ownerless lock row may remain or a delayed DELETE may still complete, so Credo does not issue a second Unlock or retry automatically.
- **Production deployment is one-shot first**: a single deadline-bounded pre-deploy job calls the same `DB.Migrate` method before application rollout. `app.OnStart(db.Migrate)` remains convenient for development, tests, single replicas, and deliberately small deployments, but simultaneous replica startup would make lock losers fail startup and can create crash loops during a long migration. Library-level lock wait/retry is rejected for now because overlapping release jobs are a coordination error and waiting can let migrations from different releases run in the wrong order.
- **Lifecycle integration is signature compatibility, not coupling**: `Migrate` matches the `App.OnStart` hook signature, but `sqldb` still imports only `credo/store`, never the root framework package.
- **Deliberate divergence from Bun's default**: the wrapper passes `migrate.WithMarkAppliedOnSuccess(true)`, so an Up error surfaced by Bun leaves the migration unapplied. This is at-least-once bookkeeping, not safe retry by itself: non-transactional work can be partial, a successful body and its marker write are separate, and a group is not all-or-nothing. Migrations must be transactional where supported or idempotent/reconcilable; expand/backfill/contract rollout avoids coupling destructive schema changes to old replicas. Users can restore Bun's record-before-running recovery tradeoff through `RegisterMigrations` options.
- **Pinned upstream limitation**: Bun v1.2.18's SQL migration closure does not reliably return deferred transaction Commit/Rollback (or connection Close) errors. Credo therefore does not promise that a `.tx.up.sql` finalizer failure prevents the applied marker. Until upstream fixes and a conformance test prove that path, commit-sensitive work should use a Go migration with an explicit transaction whose error is returned. Direct `migrate.NewMigrator` use also does not inherit Credo's options: status/generation repeat what they need, while DB-mutating apply/rollback paths additionally own Init, Lock, and bounded cancellation-detached Unlock.
- Registration misuse (nil set, double registration) panics per the panic-vs-error policy; `Migrate` itself only returns errors.
- Seeding is a plain migration file (no separate mechanism). Rollback, status, and file generation stay on Bun's migrator via the escape hatch: `migrate.NewMigrator(db.Client(), migrations)`.
