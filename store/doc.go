// Package store provides universal data access contracts for the Credo framework.
//
// This package defines error sentinels, the typed [Health] report, and
// context-based transaction helpers. It has zero external dependencies — only
// the Go standard library and Credo's root/fault contracts are imported.
//
// The companion package store/sqldb (a separate Go submodule) wraps
// *bun.DB with lifecycle management, query builder proxies, error
// mapping, and transaction support.
//
// # Universal Errors
//
// Store errors expose a transport-neutral semantic kind, a [fault.Kind]: the
// kinds have one name each, in package fault, and this package declares no
// copy of them. The default HTTP policy and future protocol adapters consume
// that kind independently:
//
//	if kind, ok := store.KindOf(err); ok {
//	    // branch on fault.KindNotFound, fault.KindConstraint, fault.KindDeadlock, ...
//	}
//
// [Error] retains diagnostic code/constraint/resource metadata and the
// original cause without exposing them in Credo's default HTTP response.
// Its HTTP status comes from its kind alone, through [fault.Provider]: the
// error pipeline maps the kind to a status, and the kind is the response's
// code.
//
// # A Bound Store
//
// A store is a component like any other: it is bound with app.ProvideValue,
// or app.Provide with a constructor, with credo.Borrowed() for a handle the
// caller shares, and the binding alone is enough:
//
//	app.ProvideValue(db)
//
// The start walk calls the store's Start, which pings it, before the
// components that depend on it start; /ready reports it through its Ready;
// and the App shuts it down after its consumers unless it is borrowed. A store
// adapter implements Start, Ready and Shutdown, as store/sqldb's *DB does.
// credo.OptionalReadiness() reports a store in /ready without letting it gate
// readiness, and credo.ComponentName names its entry.
//
// Several bindings may hold one physical resource — a *sqldb.DB bound raw and
// a wrapper type that embeds it. A value that names its resource through
// credo.ResourceIdentifier is one resource with the other holders of that
// resource: one start, one readiness entry and one teardown, which happens
// once, after the last holder retires. Only a value without state of its own
// to release may share an identity.
//
// [Health.Cause] carries typed diagnostics for logging while remaining
// excluded from JSON; free-form Details values are never interpreted as error
// causes.
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
// transaction-aware Bun escape hatch. The scope's methods are the one
// transaction API.
//
// Maturity: beta
package store
