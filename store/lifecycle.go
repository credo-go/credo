package store

import "context"

// Lifecycle is the contract a data store binding meets for [Register]: the
// start phase pings it, /ready reports its health, and the App shuts it down
// after its consumers, as it does every component. Adapters such as
// store/sqldb implement it. A value that cannot is registered through a
// wrapper type that does.
//
// An implementation that represents a resource another value also holds —
// a wrapper over a *sqldb.DB — shares its teardown through
// credo.ResourceIdentifier: values with one identity are shut down once.
type Lifecycle interface {
	// Ping verifies the connection is alive and must honor ctx cancellation;
	// the start phase calls it once, with a deadline.
	Ping(ctx context.Context) error

	// Shutdown gracefully closes the connection.
	// Implementations should respect ctx.Done() for timely cleanup.
	Shutdown(ctx context.Context) error

	// Health returns structured health information including status,
	// latency, and adapter-specific details (pool stats, version, etc.).
	Health(ctx context.Context) Health
}
