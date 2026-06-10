package health

import (
	"context"
	"time"
)

// StoreResult holds the outcome of a store health check
// (provided by store.Registry via [StoreFunc]).
type StoreResult struct {
	Name    string
	Status  string
	Latency time.Duration
}

// StoreFunc collects store health snapshots for the readiness endpoint.
// store.Register provides one into the DI container under this type; the
// root health engine resolves it lazily on each readiness check, so the
// registration order of stores and UseHealth does not matter.
type StoreFunc func(ctx context.Context) []StoreResult
