// Package store provides universal data access contracts for the Credo framework.
//
// This package defines error sentinels, lifecycle/health interfaces,
// a connection registry, a registration API, and context-based
// transaction helpers. It has zero external dependencies — only
// the Go standard library and the credo root package are imported.
//
// The companion package store/sqldb (a separate Go submodule) wraps
// *bun.DB with lifecycle management, query builder proxies, error
// mapping, and transaction support.
//
// # Universal Errors
//
// Store errors implement HTTPStatus() int. The default error handler
// detects this interface via errors.As without importing store/,
// avoiding circular dependencies.
//
//	var se interface{ HTTPStatus() int }
//	if errors.As(err, &se) {
//	    status = se.HTTPStatus()
//	}
//
// # Registration
//
// Use [Register] to register a data store connection in the DI container
// with automatic ping, lifecycle tracking, and health aggregation:
//
//	store.Register[*sqldb.DB](app, db)
//
// # Context-Based Transactions
//
// Use [WithTx], [GetTx], and [Conn] for simple type-keyed transaction
// participation. When the same client type can appear more than once,
// create a [TxScope] and use [WithTxInScope] / [ConnInScope].
//
//	func (r *UserRepo) GetByID(ctx context.Context, id int) (*User, error) {
//	    conn := store.Conn[bun.IDB](ctx, r.db.Client())
//	    // conn is the TX if one is active, otherwise the raw DB
//	}
package store
