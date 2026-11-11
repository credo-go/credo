// Package sqldb wraps *bun.DB with lifecycle management, query builder
// proxies, error mapping, and transaction support.
//
// A *DB is a component: bound with app.ProvideValue(db), its Start pings the
// database under Config.PingTimeout (zero means DefaultPingTimeout) before
// the components that depend on it start, its Ready reports it in /ready
// (named *sqldb.DB, or by credo.ComponentName), and the App shuts it down
// after its consumers. A failed ping fails the start and closes the
// database; with WithOptionalStart it is logged at Warn instead and the
// start goes on, while Ready keeps pinging, so the database gates readiness
// until it answers unless it is bound with credo.OptionalReadiness().
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
// unlimited when Start's ping succeeds, Start logs one structured warning,
// credo: store configuration warning with code sqldb.pool.max_open_unlimited
// and module=sqldb, to WithLogger's logger or the App's. A program that runs
// the database without an App calls Start itself or reads
// Stats().MaxOpenConnections.
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
// A database gates readiness unless it is bound with
// credo.OptionalReadiness(), so a DEGRADED that removed readiness on a noisy
// universal threshold could cascade across replicas.
//
// # Resource Identity
//
// DB is a credo.ResourceIdentifier: ResourceIdentity returns the *DB pointer.
// A wrapper type that embeds *DB inherits it, so the *DB and its wrappers are
// one database with one owner: a wrapper bound beside the handle is bound with
// credo.View(), and the App starts, asks and closes the database once,
// through its owner, after the consumers of every binding. Only a wrapper
// without state of its own to release may share the identity; one that
// releases state of its own holds the *DB in a named field and does not
// forward ResourceIdentity.
//
// # Query Builder Proxies
//
// The DB type exposes Select, Insert, Update, and Delete methods that
// return proxy query builders. These proxies inject transactions from
// context, map errors to store.Err* sentinels, and provide escape
// hatches (Apply, Unwrap) for advanced usage.
// Each builder accepts at most one optional model. Supplying more causes the
// builder to record an error that its terminal returns without executing.
// SelectQuery's curated Limit and Offset forward every value; zero and
// negative values keep Bun's semantics and omit the clause.
//
//	var user User
//	err := db.Select(&user).Where("id = ?", id).Scan(ctx)
//	// err is already mapped: sql.ErrNoRows → store.ErrNotFound
//
// ApplyFilter runs a func(*Filter) on the query's WHERE clause, so a single
// predicate (tenant scoping, soft-delete filters, ownership checks) scopes
// Select, Update, and Delete alike instead of being duplicated per query type.
// Filter is Credo's type with the predicate methods only (Where, WhereOr,
// WhereGroup, WherePK, WhereDeleted, WhereAllWithDeleted), so a select scoped
// through it keeps its automatic total in Count and Page.
//
//	func tenantScope(id int64) func(*sqldb.Filter) {
//	    return func(f *sqldb.Filter) { f.Where("tenant_id = ?", id) }
//	}
//
// # Bun Visibility Policy
//
// Credo does not hide Bun — it integrates it. The proxy layer exists to
// attach two guarantees to query execution, not to abstract Bun away:
//
//   - Transaction injection: terminal methods (Scan, Count, Exists, Exec)
//     resolve the connection from the context at execution time, so code
//     inside InTx transparently runs on the transaction. Only a
//     connection set with the wrapper's Conn is kept; one set inside an Apply
//     callback is overridden by the injection. Select terminals run on an
//     execution snapshot taken with Bun's own Clone, so the builder may be
//     reused across executions and TX boundaries.
//   - Error mapping: the same terminals pass driver errors through the
//     store.Err* mapping before returning.
//
// Bun types therefore appear in proxy signatures (bun.IConn, Apply callbacks)
// by design. The wrapper reads and writes no Bun private state: it builds on
// Bun's public API only, and the count conformance tests pin the Bun behaviour
// Count and Page rely on. When the curated proxy
// surface lacks something:
//
//   - Missing builder method: use Apply (per query type) or ApplyFilter
//     (shared WHERE predicates). Both keep the terminal guarantees intact;
//     a select built with Apply pages with PageWithCount.
//   - Missing terminal method: request an addition to the curated set —
//     the guarantees live in the terminals, so they must be on the proxy.
//   - Conn is the transaction-aware native Bun escape hatch. It returns the
//     active transaction or the base DB, but native executions still bypass
//     Credo error mapping.
//   - Unwrap and Client are deliberate opt-outs: executions through the raw
//     Bun objects they return get neither automatic TX injection nor error
//     mapping.
//
// SelectQuery.Clone is the public, top-level builder-fork API. It forwards to
// Bun's Clone and is not a recursive object-graph copy: a bound destination and
// nested CTE/relation query values may remain shared.
// Do not mutate or scan shared values concurrently through source and clone.
//
// # Transactions
//
// Use InTx to execute a function within a transaction. The adapter stores
// the TX in a private per-DB scope so repositories using sqldb proxies pick
// it up automatically without cross-DB collisions:
//
//	err := db.InTx(ctx, func(ctx context.Context) error {
//	    // repos pick up the scoped TX automatically
//	    return nil
//	})
//
// InTxWith accepts sql.TxOptions for configuring isolation
// level and read-only mode on the outer transaction. Nested calls use a Bun
// savepoint; because a savepoint cannot apply new transaction options, a
// nested call with non-default options returns ErrNestedTxOptions instead of
// silently ignoring them. From a handler, pass the request context:
// db.InTx(c.Context(), fn).
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
// SelectQuery.Page is the typed pagination terminal: it runs a count and a
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
// or PerPage, and native-int offset overflow return
// pagination.ErrInvalidPageRequest before the count. The caller's request is never
// mutated; a valid PerPage above the
// package default cap (for example a custom normalized value of 100) remains
// valid and is not clamped by the terminal. When the count reports zero rows,
// the data query is skipped and the page keeps the snapshot's page/per-page with a non-nil
// empty slice.
//
// Total is the number of rows the data query returns without its LIMIT and
// OFFSET, or an error, never another number. Count and Page run Bun's native
// count, which drops ORDER BY, LIMIT, OFFSET and FOR, counts groups or
// distinct tuples for a query with GroupExpr or Distinct, and runs no model
// hook. They run it only on a query the wrapper can prove: built without Apply,
// Unwrap or a Relation callback; whose model's type does not implement
// bun.BeforeSelectHook; and that either carries GroupExpr or Distinct or has no
// raw ColumnExpr and no Having. Every other query returns an error matching
// ErrUnsupportedCountQuery before I/O, naming the rule and its culprit, and the
// caller pages it with PageWithCount and a count of its own; another query's
// Count passes as it is:
//
//	page, err := db.Select().
//	    Apply(withReportColumns).
//	    OrderExpr("created_at DESC, id DESC").
//	    PageWithCount[Report](ctx, req, db.Select((*Report)(nil)).ApplyFilter(scope).Count)
//
// PageWithCount validates the request as Page does, runs the count and the data
// query with the page's window, and builds the page with NewPage; the count's
// sort and lock clauses are the caller's. A model whose type implements only
// bun.BeforeAppendModelHook keeps its automatic total: on a SELECT Bun runs
// that hook only for values already in the destination, Page scans into an
// empty slice of its own, and the count runs no hook.
//
// MySQL validates the output names of the count's derived source when it
// executes. If it returns ER_DUP_FIELDNAME (1060), Count and Page wrap
// ErrUnsupportedCountQuery after I/O while preserving the driver cause; unique
// aliases resolve collisions. Non-count MySQL 1060 errors remain unchanged.
// Driver causes are diagnostic and must not be rendered directly to HTTP.
// CursorPage is the accepted working shape for forward keyset pagination;
// Slice is only a working name for a future total-free offset result. Neither
// API is exported yet. Use a stable ORDER BY with a unique tie-breaker for
// deterministic offset pages.
//
// The count and the data query use internal execution snapshots and join the
// ambient transaction, but remain separate database statements. Page never
// starts an implicit transaction. PostgreSQL Read Committed can observe statement-level
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
//   - unique violation      → store.ErrAlreadyExists
//   - foreign-key violation → store.ErrConstraint
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
// Beta (see the project README's "Maturity by Area" table): the module is
// released in lockstep with the root module, and its beta label keeps it outside
// the v1 compatibility promise. Breaking changes are possible before v1.
//
// Maturity: beta
package sqldb
