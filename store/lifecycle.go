package store

import "context"

// Lifecycle manages connection health and shutdown for a data store.
// Adapters (e.g., store/sqldb) implement this interface for use
// with [Register].
type Lifecycle interface {
	// Ping verifies the connection is alive.
	Ping(ctx context.Context) error

	// Shutdown gracefully closes the connection.
	// Implementations should respect ctx.Done() for timely cleanup.
	Shutdown(ctx context.Context) error

	// Health returns structured health information including status,
	// latency, and adapter-specific details (pool stats, version, etc.).
	Health(ctx context.Context) Health
}
