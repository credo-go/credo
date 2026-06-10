package store

import (
	"context"
	"fmt"
	"slices"
	"sync"
)

// Registry tracks data store connections for startup ping and health
// aggregation. It is created automatically on the first [Register] call
// and stored in the DI container.
//
// The Registry does not close connections. Shutdown ownership lies with
// the DI container alone: a registered value that implements
// credo.Shutdowner is closed during app shutdown, in reverse
// registration order.
type Registry struct {
	mu      sync.RWMutex
	entries []registryEntry
}

type registryEntry struct {
	name      string
	lifecycle Lifecycle
}

// Add appends a lifecycle entry to the registry.
// Names must be unique; duplicate names are rejected.
func (r *Registry) Add(name string, lc Lifecycle) error {
	if lc == nil {
		return fmt.Errorf("store: lifecycle must not be nil for %q", name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, e := range r.entries {
		if e.name == name {
			return fmt.Errorf("store: duplicate store name %q", name)
		}
	}
	r.entries = append(r.entries, registryEntry{name: name, lifecycle: lc})
	return nil
}

// remove removes a lifecycle entry from the registry by name.
// Returns true when an entry was found and removed.
func (r *Registry) remove(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i := range r.entries {
		if r.entries[i].name != name {
			continue
		}
		r.entries = append(r.entries[:i], r.entries[i+1:]...)
		return true
	}
	return false
}

// HealthAll returns health status for all tracked connections.
// The returned map is keyed by the registration name.
func (r *Registry) HealthAll(ctx context.Context) map[string]Health {
	if ctx == nil {
		ctx = context.Background()
	}

	r.mu.RLock()
	entries := slices.Clone(r.entries)
	r.mu.RUnlock()

	result := make(map[string]Health, len(entries))
	for _, e := range entries {
		result[e.name] = e.lifecycle.Health(ctx).Clone()
	}
	return result
}
