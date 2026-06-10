package store

import (
	"maps"
	"time"
)

// HealthStatus represents the health state of a data store connection.
type HealthStatus string

const (
	// StatusUp indicates the connection is healthy.
	StatusUp HealthStatus = "UP"

	// StatusDown indicates the connection is unreachable.
	StatusDown HealthStatus = "DOWN"

	// StatusDegraded indicates the connection is available but impaired.
	StatusDegraded HealthStatus = "DEGRADED"
)

// Health is the result of a connection health check.
type Health struct {
	// Status is the current health state.
	Status HealthStatus

	// Latency is the round-trip time of the health check probe.
	Latency time.Duration

	// Details holds adapter-specific information such as database version,
	// connection pool statistics, or replication lag.
	Details map[string]any
}

// Clone returns a defensive copy of the health snapshot.
func (h Health) Clone() Health {
	clone := Health{
		Status:  h.Status,
		Latency: h.Latency,
	}
	if h.Details != nil {
		clone.Details = maps.Clone(h.Details)
	}
	return clone
}
