// Package sqldb wraps *bun.DB with lifecycle management, query builder
// proxies, error mapping, and transaction support.
//
// This is a separate Go submodule (github.com/credo-go/credo/store/sqldb)
// so that applications not using SQL databases do not pull in the Bun
// dependency.
//
// # Creating a Connection
//
//	db, err := sqldb.Open(&sqldb.Config{
//	    Driver: "postgres",
//	    Host:   "localhost",
//	    Port:   5432,
//	    Name:   "myapp",
//	    User:   "postgres",
//	    Password: "secret",
//	    MaxOpen: 25,
//	    MaxIdle: new(10),
//	    MaxIdleTime: 5 * time.Minute,
//	    MaxLifetime: 30 * time.Minute,
//	})
//
// Driver-family detection uses an exact, case-insensitive allowlist:
// postgres/pgx, mysql, and sqlite/sqlite3/sqliteshim. A differently named
// registered driver uses its native Config.DSN plus WithDialect; a connector
// uses WithConnector plus WithDialect. Credo does not infer a family from a
// substring or a connector's concrete type, and rejects known driver/dialect
// family mismatches. When Credo builds a PostgreSQL or MySQL DSN, Port must be
// between 1 and 65535 and IPv6 hosts are bracketed correctly. Use Config.DSN or
// WithConnector for driver-native default-port or socket behavior.
//
// Positive PostgreSQL ConnectTimeout values are rounded up to whole seconds so
// a sub-second value cannot silently disable the timeout. Config.Options cannot
// override reserved endpoint/credential fields or a simultaneously configured
// SSLMode/ConnectTimeout; conflicting sources fail without including option
// values in the error. Explicit nil WithDialect/WithConnector values also fail.
// SSLMode is driver-specific (PostgreSQL sslmode, MySQL tls), and Credo imposes
// no universal TLS default; production configuration must choose the driver's
// verified mode explicitly.
//
// # Connection Pool
//
// Credo does not choose a workload-independent finite pool size. MaxOpen=0
// preserves database/sql's unlimited-open behavior. If the pool is still
// unlimited when a successful canonical store.Register inspects it, the app
// logger emits one structured warning with code sqldb.pool.max_open_unlimited.
// Standalone users can inspect DB.StoreRegistrationWarningCodes during
// bootstrap and route the same secret-free codes through their own logger.
//
// MaxIdle uses a pointer to distinguish absence from an explicit zero: nil
// makes Credo leave the idle limit unset (the effective database/sql default
// remains subject to MaxOpen), new(0) retains no idle connections, and a
// positive value is applied exactly. With MaxOpen > 0, MaxIdle must not exceed
// MaxOpen; Open rejects the configuration instead of relying on database/sql's
// silent clamp. MaxIdleTime=0 disables idle-age expiry and MaxLifetime=0
// disables lifetime expiry. Positive values are applied unchanged.
//
// DB.Stats returns the complete sql.DBStats snapshot. Health details include
// current open/in-use/idle counts and cumulative wait/closure counters, but
// readiness does not serialize adapter details. Credo does not derive
// StatusDegraded from pool saturation: that policy remains gated on production
// metrics, an explicit SLO, windowed deltas, hysteresis, and opt-in thresholds.
// Because all stores are currently critical, DEGRADED removes readiness and a
// noisy universal threshold could cascade across replicas.
//
// # Lifecycle Identity
//
// DB implements store.LifecycleIdentityProvider. ResourceIdentity returns the
// *DB pointer, giving store.Register a stable physical-resource token. A
// semantic wrapper that embeds *DB inherits this method through ordinary Go
// method promotion. A named-field wrapper that implements Lifecycle itself
// must explicitly forward ResourceIdentity to the underlying DB; Credo does not
// inspect wrapper fields.
//
// The duplicate-resource guarantee is scoped to one store.Registry and its
// store.Register calls. Do not publish the same *DB again under another DI type
// with raw Provide/ProvideFactory/ProvideValue/ProvideProtectedValue/Replace;
// use App.Alias for an interface view.
//
// # Query Builder Proxies
//
// The DB type exposes Select, Insert, Update, and Delete methods that
// return proxy query builders. These proxies inject transactions from
// context, map errors to store.Err* sentinels, and provide escape
// hatches (Apply, ApplyQueryBuilder, Unwrap) for advanced usage.
// Each builder accepts at most one optional model. Supplying more causes the
// builder to record an error that its terminal returns without executing.
// SelectQuery's curated Limit and Offset methods also guard Bun v1.2.18's
// signed-int32 storage: an out-of-range int records ErrInvalidLimitOffset and
// the terminal returns before database execution. Values inside the int32
// range, including zero and negatives, retain Bun's native semantics. Apply
// and Unwrap expose raw Bun builders and therefore retain Bun's own narrowing
// contract instead of this curated-method guard.
//
//	var user User
//	err := db.Select(&user).Where("id = ?", id).Scan(ctx)
//	// err is already mapped: sql.ErrNoRows → store.ErrNotFound
//
// ApplyQueryBuilder surfaces Bun's shared bun.QueryBuilder so a single
// WHERE predicate (tenant scoping, soft-delete filters, ownership checks)
// can be reused across Select, Update, and Delete instead of being
// duplicated per query type. Interceptors are preserved, like Apply.
//
// # Bun Visibility Policy
//
// Credo does not hide Bun — it integrates it. The proxy layer exists to
// attach two guarantees to query execution, not to abstract Bun away:
//
//   - Transaction injection: terminal methods (Scan, Count, Exists, Exec)
//     resolve the connection from the context at execution time, so code
//     inside InTx/RunInTx transparently runs on the transaction. Select
//     terminals use an internal execution snapshot that preserves the
//     explicit connection, builder error, WherePK, soft-delete flags, and
//     model/relation state, so the builder may be reused across executions
//     and TX boundaries.
//   - Error mapping: the same terminals pass driver errors through the
//     store.Err* mapping before returning.
//
// Bun types therefore appear in proxy signatures (bun.IConn,
// bun.QueryBuilder, Apply callbacks) by design. When the curated proxy
// surface lacks something:
//
//   - Missing builder method: use Apply (per query type) or
//     ApplyQueryBuilder (shared WHERE predicates). Both keep the
//     terminal guarantees intact.
//   - Missing terminal method: request an addition to the curated set —
//     the guarantees live in the terminals, so they must be on the proxy.
//   - Conn is the transaction-aware native Bun escape hatch. It returns the
//     active transaction or the base DB, but native executions still bypass
//     Credo error mapping.
//   - Unwrap and Client are deliberate opt-outs: executions through the raw
//     Bun objects they return get neither automatic TX injection nor error
//     mapping.
//
// SelectQuery.Clone is the public, top-level builder-fork API. It preserves the
// execution fields patched by Credo but is not a recursive object-graph copy:
// a bound destination and nested CTE/relation query values may remain shared.
// Do not mutate or scan shared values concurrently through source and clone.
//
// # Transactions
//
// Use InTx (or the package-level RunInTx) to execute a function within a
// transaction. The adapter stores the TX in a private per-DB scope so
// repositories using sqldb proxies pick it up automatically without
// cross-DB collisions:
//
//	err := db.InTx(ctx, func(ctx context.Context) error {
//	    // repos pick up the scoped TX automatically
//	    return nil
//	})
//
// InTxWith / RunInTxWith accept sql.TxOptions for configuring isolation
// level and read-only mode on the outer transaction. Nested calls use a Bun
// savepoint; because a savepoint cannot apply new transaction options, a
// nested call with non-default options returns ErrNestedTxOptions instead of
// silently ignoring them. From a handler, pass the request context:
// db.InTx(ctx.Context(), fn).
// Nested savepoint creation and cleanup are cancellation-safe and bounded:
// callback queries retain their original context, while savepoint operations
// use an internal context controlled by Credo. An uncertain begin/release/
// rollback marks the shared transaction rollback-only before a fail-safe
// ambient abort, so an outer callback cannot swallow the error and commit;
// that outer InTx returns ErrTxRollbackOnly.
// Savepoint operations and ambient abort each use a five-second default budget;
// WithTxCleanupTimeout overrides it without limiting callback execution time.
//
// A callback error is returned unchanged after rollback; only begin,
// rollback, and commit driver errors are mapped. Panic rollback preserves the
// original panic value. A nil callback returns ErrNilTxCallback before BEGIN.
// A commit error can leave the database outcome unknown, so it must not be
// treated as proof that no changes were applied or as an unconditional retry
// signal.
//
// For a native Bun operation that must participate in the transaction, use
// Conn with the callback context. The returned bun.IDB is borrowed and must
// not escape the callback:
//
//	err := db.InTx(ctx, func(txCtx context.Context) error {
//	    return db.Conn(txCtx).NewSelect().Model(&users).Scan(txCtx)
//	})
//
// # Pagination
//
// SelectQuery.Page is the typed pagination terminal: it runs COUNT + a
// LIMIT/OFFSET SELECT and returns a ready *pagination.Page[T]. Like One and
// All it is a Form-A terminal, so T drives the table and destination and the
// query is built model-less:
//
//	req := &pagination.PageRequest{Page: 2, PerPage: 20} // normalized by BindQuery
//	page, err := db.Select().
//	    Where("active = ?", true).
//	    OrderExpr("created_at DESC, id DESC").
//	    Page[User](ctx, req)
//
// One, All, and Page require T to be the actual table model and reject a model
// bound through Select, Model, or Apply with ErrTypedTerminalModel before
// executing. TableExpr does not turn a typed terminal into a projection API;
// use TableExpr(...).Scan(ctx, &dest) for projections. For relations, bind the
// destination and use Scan instead:
//
//	var users []User
//	err := db.Select(&users).Relation("Orders").Scan(ctx)
//
// BindQuery applies PageRequest.Validate, whose input policy defaults/clamps the
// request. Page does not repeat that policy. Instead it copies req and strictly
// validates the snapshot before touching the database: nil, non-positive Page
// or PerPage, native-int offset overflow, and values outside Bun v1.2.18's
// signed-int32 LIMIT/OFFSET range return pagination.ErrInvalidPageRequest before
// COUNT. The caller's request is never mutated; a valid PerPage above the
// package default cap (for example a custom normalized value of 100) remains
// valid and is not clamped by the terminal. When COUNT reports zero rows, SELECT
// is skipped and the page keeps the snapshot's page/per-page with a non-nil
// empty slice.
//
// Total is complete logical projection-row cardinality before ordering and the
// Page-owned window. Credo removes root ORDER/LIMIT/OFFSET/FOR and counts a
// universal outer _credo_count_source: an ungrouped aggregate normally
// contributes one row, Distinct counts selected projection tuples, Group counts
// groups, and Group with Having counts groups left after Having. Count and Page
// reject Having without Group and direct UNION/INTERSECT/EXCEPT roots with
// ErrUnsupportedCountQuery before I/O. Advanced callers restructure those
// shapes behind an outer derived table or CTE, compose an explicit count query
// and data query, and build NewPage. MySQL validates derived-table output names
// when the logical COUNT executes. If it returns ER_DUP_FIELDNAME (1060), Count
// and Page wrap ErrUnsupportedCountQuery after I/O while preserving the driver
// cause; unique aliases resolve collisions. Non-count MySQL 1060 errors remain
// unchanged. The server error cannot identify which derived-table level failed,
// so an indistinguishable 1060 from a caller-supplied nested source is wrapped
// too. Driver causes are diagnostic and must not be rendered directly to HTTP.
// Because the logical projection is evaluated by the count source, expensive or
// volatile projections should use that explicit custom
// composition. There is no custom-count strategy until two real consumers
// require one, and Page never carries an unknown total. CursorPage is the
// accepted working shape for forward keyset pagination; Slice is only a working
// name for a future total-free offset result. Neither API is exported yet. Use
// a stable ORDER BY with a unique tie-breaker for deterministic offset pages.
// Relation callbacks are applied exactly once while rendering this private
// source. Predicates/projections are allowed; replacing the model or adding
// root ORDER/LIMIT/OFFSET/FOR or another unsupported shape fails before I/O.
//
// Logical Count runs BeforeSelect, BeforeAppendModel, and successful-query
// AfterSelect on the private source. Page runs the hooks again when its data
// SELECT executes, so hook-added predicates/projections affect Total and
// Records. The outer count preserves QueryEvent.Model for observability while
// soft-delete policy is applied only inside the source. Count never scans or
// mutates a bound model, so AfterSelect observes its pre-count value. Hooks must
// be deterministic: transaction isolation cannot stabilize a volatile
// projection or application-side decision across the two statements.
//
// COUNT and SELECT use internal execution snapshots and join the ambient
// transaction, but remain separate database statements. Page never starts an
// implicit transaction. PostgreSQL Read Committed can observe statement-level
// drift; PostgreSQL/InnoDB callers that require one snapshot establish a
// read-only Repeatable Read outer transaction and pass its txCtx to Page.
// SQLite keeps the first-read snapshot of a plain explicit InTx; WAL permits a
// concurrent writer while rollback-journal mode may serialize it. The pinned
// modernc SQLite driver does not reliably enforce TxOptions Isolation/ReadOnly,
// so those options are not a SQLite guarantee.
//
// Page responds with the queried type directly. For a model→DTO response,
// run Page[Model] and reshape it with pagination's Page.Map, which carries the
// metadata over unchanged:
//
//	modelPage, err := db.Select().Where("active = ?", true).Page[Model](ctx, req)
//	if err != nil {
//	    return nil, err
//	}
//	dtoPage := modelPage.Map(func(m Model) DTO { return toDTO(m) })
//
// When the conversion itself can fail, fetch the model page, map its records
// with ordinary error handling, and build the DTO page with NewPage:
//
//	modelPage, err := q.Page[Model](ctx, req)
//	// ...map modelPage.Records to dtos, returning any conversion error...
//	page := pagination.NewPage(
//	    dtos, modelPage.Total, modelPage.Page, modelPage.PerPage,
//	)
//
// NewPage computes TotalPages with overflow-safe quotient-and-remainder
// ceiling division, including totals near math.MaxInt64.
//
// # Migrations
//
// The DB wraps Bun's migration engine (bun/migrate — part of the already
// pinned Bun module) behind two methods: RegisterMigrations stores a
// *migrate.Migrations set at wiring time, and Migrate runs the pending ones.
// Multi-replica production should run the same Migrate method in one
// deadline-bounded pre-deploy job. Migrate also matches credo's App.OnStart
// hook; this is an opt-in convenience for development and deliberate
// single-replica deployments:
//
//	//go:embed migrations/*.sql
//	var sqlMigrations embed.FS
//
//	migrations := migrate.NewMigrations()
//	if err := migrations.Discover(sqlMigrations); err != nil {
//	    return err
//	}
//	db.RegisterMigrations(migrations)
//	app.OnStart(db.Migrate) // dev/single-replica convenience
//
// Seeding is a plain migration file (for example 2_seed_plans.up.sql) — there
// is no separate seed mechanism. Mark-on-success gives at-least-once retry,
// not atomic rollback: migrations must be transactional where supported or
// idempotent/reconcilable. Bun's table lock is fail-fast. Unlock is detached
// from parent cancellation and caller-bounded to five seconds; timeout leaves
// its outcome uncertain and is not automatically retried. Direct Bun
// migrators used for rollback/status/generation do not inherit Credo's
// options. Status/generation repeat only relevant options; DB-mutating
// apply/rollback callers additionally own Init, Lock, and bounded Unlock.
//
// # Error Mapping
//
// Terminal methods on the proxy types (Scan, Count, Exists, Exec) pass
// driver errors through mapError before returning. Common mappings:
//
//   - sql.ErrNoRows         → store.ErrNotFound
//   - unique violation      → store.ErrAlreadyExists (ErrDuplicate compatible)
//   - foreign-key violation → store.ErrConstraint (ErrConflict compatible)
//   - serialization failure → store.ErrSerialization
//   - deadlock              → store.ErrDeadlock
//   - lock/busy contention  → store.ErrContention
//   - bad connection        → store.ErrUnavailable
//   - read-only / replica   → store.ErrReadOnly
//   - context deadline      → store.ErrTimeout
//
// The classifier is context- and driver-family-aware. Structured SQLSTATE,
// MySQL number envelopes, and SQLite numeric codes produce a *store.Error
// that preserves the original cause and driver code. Loose message matching
// is not used. Callers can branch with errors.Is or inspect store.KindOf;
// store.IsTransient describes a condition, not permission to retry an
// operation. Update.Exec and Delete.Exec do not convert "no rows affected"
// into ErrNotFound — inspect sql.Result for that.
//
// # Escape Hatch
//
// Client() returns the underlying *bun.DB for model registration, advanced
// migration operations, and calls intentionally tied to the base DB. Queries
// executed via Client() bypass the proxy interceptors: there is no automatic
// TX injection from context and no error mapping to store.Err* sentinels.
// For native Bun work that must join an ambient transaction use Conn(ctx);
// for normal repository code use the proxy layer. Conn selects the right
// connection but still does not add Credo error mapping.
//
// # Stability
//
// Beta, versioned independently from the root module (see the project README's
// "Maturity by Area" table). Breaking changes are possible before v1.
//
// Maturity: beta
package sqldb
