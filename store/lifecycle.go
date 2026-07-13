package store

import "context"

// Lifecycle manages connection health and shutdown for a data store.
// Adapters (e.g., store/sqldb) implement this interface for use with
// [Register]. Implementations should normally be pointer-backed so Register can
// retain a stable physical-resource identity and reject duplicate ownership.
type Lifecycle interface {
	// Ping verifies the connection is alive and must honor ctx cancellation;
	// Register invokes it synchronously with a deadline.
	Ping(ctx context.Context) error

	// Shutdown gracefully closes the connection.
	// Implementations should respect ctx.Done() for timely cleanup.
	Shutdown(ctx context.Context) error

	// Health returns structured health information including status,
	// latency, and adapter-specific details (pool stats, version, etc.).
	Health(ctx context.Context) Health
}

// LifecycleIdentityProvider is an optional Lifecycle extension for semantic
// wrappers that represent another physical resource. ResourceIdentity must
// return a non-nil, comparable, reflexively equal token that remains stable for
// the resource lifetime; the underlying resource pointer is the usual token.
// Wrapper types that embed an implementation inherit this method automatically.
type LifecycleIdentityProvider interface {
	Lifecycle
	ResourceIdentity() any
}
