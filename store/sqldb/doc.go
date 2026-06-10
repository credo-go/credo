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
//	})
//
// # Query Builder Proxies
//
// The DB type exposes Select, Insert, Update, and Delete methods that
// return proxy query builders. These proxies inject transactions from
// context, map errors to store.Err* sentinels, and provide escape
// hatches (Apply, ApplyQueryBuilder, Unwrap) for advanced usage.
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
//     inside InTx/RunInTx transparently runs on the transaction. The
//     builder itself is never mutated — terminals execute a copy — so a
//     builder may be reused across executions and TX boundaries.
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
//   - Unwrap and Client are deliberate opt-outs: executions through the
//     raw Bun objects they return get neither TX injection nor error
//     mapping.
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
// level and read-only mode. From a handler, pass the request context:
// db.InTx(ctx.Context(), fn).
//
// # Migrations
//
// The DB wraps Bun's migration engine (bun/migrate — part of the already
// pinned Bun module) behind two methods: RegisterMigrations stores a
// *migrate.Migrations set at wiring time, and Migrate runs the pending
// ones. Migrate's signature matches credo's App.OnStart hook, so running
// migrations on application start is an explicit, opt-in one-liner:
//
//	//go:embed migrations/*.sql
//	var sqlMigrations embed.FS
//
//	migrations := migrate.NewMigrations()
//	if err := migrations.Discover(sqlMigrations); err != nil {
//	    return err
//	}
//	db.RegisterMigrations(migrations)
//	app.OnStart(db.Migrate) // applies pending migrations before serving
//
// Seeding is a plain migration file (for example 2_seed_plans.up.sql) —
// there is no separate seed mechanism. A failed migration is retried on
// the next run (see RegisterMigrations), and Bun's table-based advisory
// lock prevents two replicas from migrating concurrently. For rollback,
// status inspection, or migration file generation, use Bun's migrator
// directly: migrate.NewMigrator(db.Client(), migrations).
//
// # Error Mapping
//
// Terminal methods on the proxy types (Scan, Count, Exists, Exec) pass
// driver errors through mapError before returning. Common mappings:
//
//   - sql.ErrNoRows         → store.ErrNotFound
//   - unique violation      → store.ErrDuplicate
//   - foreign-key violation → store.ErrConflict
//   - read-only / replica   → store.ErrReadOnly
//   - context deadline      → store.ErrTimeout
//
// Callers can branch on these sentinels with errors.Is without importing
// database/sql or driver-specific packages. Update.Exec and Delete.Exec
// do not convert "no rows affected" into ErrNotFound — inspect sql.Result
// for that.
//
// # Escape Hatch
//
// Client() returns the underlying *bun.DB for raw SQL, model
// registration, advanced migration operations, and any Bun feature not
// covered by proxies. Queries executed via Client() bypass the proxy
// interceptors: there is no automatic TX injection from context and no
// error mapping to store.Err* sentinels. Reserve Client() for model
// registration, raw SQL the proxy layer cannot express, and migration
// operations beyond Migrate (rollback, status, file generation); use
// the proxy layer for normal repository code.
//
// # Stability
//
// Beta, versioned independently from the root module (see the project README's
// "Maturity by Area" table). Breaking changes are possible before v1.
package sqldb
