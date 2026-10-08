// Package store provides universal data access contracts for the Credo framework.
//
// This package defines error sentinels, lifecycle/health interfaces,
// a connection registry, a registration API, and context-based
// transaction helpers. It has zero external dependencies — only
// the Go standard library and Credo's root/fault contracts are imported.
//
// The companion package store/sqldb (a separate Go submodule) wraps
// *bun.DB with lifecycle management, query builder proxies, error
// mapping, and transaction support.
//
// # Universal Errors
//
// Store errors expose a transport-neutral semantic [Kind]. The default HTTP
// policy and future protocol adapters consume that kind independently:
//
//	if kind, ok := store.KindOf(err); ok {
//	    // branch on KindNotFound, KindConstraint, KindDeadlock, ...
//	}
//
// [Error] retains diagnostic code/constraint/resource metadata and the
// original cause without exposing them in Credo's default HTTP response.
// The sentinel HTTPStatus methods remain only as a deprecated compatibility
// bridge; new application policy should use Kind.
//
// # Registration
//
// A store is bound like any other component — app.ProvideValue, or
// app.Provide with a constructor, with credo.Borrowed() for a handle the
// caller shares — and [Register] adds that binding to the App's stores:
//
//	app.ProvideValue(db)
//	store.Register[*sqldb.DB](app)
//
// Register performs no I/O. The start phase resolves the binding once, after
// Finalize and so after every override, and pings it before the components
// that depend on it start; a failed ping fails the start. A registration
// whose type has no binding fails Finalize. Shutdown follows the binding: the
// App shuts the store down after its consumers unless it is borrowed.
//
// Several bindings may hold one physical resource — a *sqldb.DB bound raw and
// a wrapper type that embeds it. A value that names its resource through
// credo.ResourceIdentifier shares one teardown with the other holders of that
// resource, which happens once, after the last holder retires. Only a value
// without state of its own to release may share an identity.
//
// Every pinged store contributes a stable readiness probe, built once at
// start; nothing is resolved per readiness request. Named and store checks
// run in parallel with enforced per-check deadlines and panic isolation.
// [Health.Cause] carries typed diagnostics for logging while remaining excluded
// from JSON; free-form Details values are never interpreted as error causes.
// Overlapping readiness requests share one in-flight execution per store, so a
// cancellation-ignoring probe cannot accumulate a goroutine per request.
//
// # Context-Based Transactions
//
// A custom adapter creates one typed [TxScope] per logical connection. The
// transaction type is fixed at construction, preventing a concrete/interface
// mismatch from silently selecting the fallback connection:
//
//	scope := store.NewTxScope[Client]()
//	ctx = scope.WithTx(ctx, tx)
//	conn := scope.Conn(ctx, base)
//	tx, err := scope.RequireTx(ctx) // no fallback
//
// The companion store/sqldb adapter owns a private typed scope per DB. Its
// proxy terminals participate automatically; db.Conn(ctx) is the
// transaction-aware Bun escape hatch. The standalone [WithTx], [GetTx], and
// [Conn] helpers remain only as deprecated compatibility APIs.
//
// Maturity: beta
package store
