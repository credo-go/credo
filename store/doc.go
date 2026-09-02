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
// Use [Register] to register a data store connection in the DI container
// with automatic ping, lifecycle tracking, and health aggregation:
//
//	store.Register[*sqldb.DB](app, db)
//
// A value that implements [Lifecycle] is framework-owned after Register
// succeeds; the DI container is its sole framework shutdown owner. During one
// teardown DI makes at most one Shutdown attempt if the still-live shutdown
// context reaches that registration. A deadline exhausted earlier may leave it
// uncalled. A value that cannot implement Lifecycle may supply its health
// handle with [WithLifecycle] only together with
// [WithCallerOwnedLifecycle]; in that explicit mode the caller remains
// responsible for shutdown. Ownership never transfers on a failed registration.
//
// Registration validates the local DI state and reserves the store name, value
// type, and resource identity before Ping. By default the Lifecycle value
// itself is the identity; pointer-backed implementations are recommended.
// Semantic wrappers around another resource implement
// [LifecycleIdentityProvider] and return the underlying pointer or another
// stable token. Embedding a provider promotes that method through ordinary Go
// method promotion; there is no reflective field scanning. Within one
// Registry, the same identity cannot be registered under another type or
// ownership mode; use credo.App.Alias for interface access to an existing
// registration.
// Pending reservations are invisible to [Registry] readers and readiness; the
// health entry becomes visible only after Ping and DI publication both
// succeed. The successful value binding and validated Registry binding are
// protected against credo.App.Replace so DI cannot diverge from
// lifecycle/readiness state. A composition-root Registry is adopted with
// the expected-value compare-and-protect form of credo.App.ProtectBinding: if
// the resolved pointer changed before protection, registration fails without
// protecting the replacement. [Registry] exposes [Registry.HealthAll] as a
// read-only view and has no public mutation API.
//
// Identity uniqueness covers only resources registered through [Register].
// Publishing the same Lifecycle again through raw credo.App.Provide,
// credo.App.ProvideFactory, credo.App.ProvideValue,
// credo.App.ProvideProtectedValue, or credo.App.Replace is unsupported and can
// create contradictory ownership or multiple shutdown attempts. In particular,
// a caller-owned lifecycle handle must not also be registered in DI as a
// Shutdowner.
//
// Registered stores contribute stable readiness probes. Named and store checks
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
