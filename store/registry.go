package store

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sync"

	internalhealth "github.com/credo-go/credo/internal/health"
	"github.com/credo-go/credo/internal/resourceid"
)

// Registry tracks data store connections for startup ping and health
// aggregation. It is created automatically on the first [Register] call
// and stored in the DI container.
//
// Registry is a read-only public facade; [Register] is its only mutation path.
// It never closes connections. The DI container owns direct Lifecycle values
// and makes at most one shutdown attempt per value if the live shutdown
// deadline reaches its entry. Explicitly caller-owned lifecycle handles remain
// the caller's responsibility.
type Registry struct {
	mu      sync.RWMutex
	entries []registryEntry

	// pending holds reservations that have not been committed or released.
	// Each reservation uniquely owns its name, DI value type, and lifecycle
	// identity against both entries and other pending reservations, so one
	// ledger replaces per-key maps; the slice is registration-time only and
	// stays tiny.
	pending []*registryReservation
}

type registryEntry struct {
	name              string
	valueType         reflect.Type
	lifecycleIdentity lifecycleIdentity
	lifecycle         Lifecycle
	probe             *internalhealth.Probe
}

type registryReservation struct {
	registry *Registry
	entry    registryEntry
	active   bool
}

type lifecycleIdentity = resourceid.Identity

// reserve keeps a store name, DI value type, and physical lifecycle identity
// private until registration has passed Ping and DI publication. Pending
// reservations never appear in HealthAll or the readiness seam.
func (r *Registry) reserve(name string, valueType reflect.Type, lc Lifecycle) (*registryReservation, error) {
	if r == nil {
		return nil, fmt.Errorf("store: registry must not be nil")
	}
	if isNilDynamicValue(lc) {
		return nil, fmt.Errorf("store: lifecycle must not be nil for %q", name)
	}
	if valueType == nil {
		return nil, fmt.Errorf("store: value type must not be nil for %q", name)
	}
	identity, err := identifyLifecycle(lc)
	if err != nil {
		return nil, fmt.Errorf("store: lifecycle identity for %q: %w", name, err)
	}
	return r.reserveIdentified(name, valueType, lc, identity, nil)
}

// reserveIdentified performs the reservation as one atomic step under the
// registry lock: conflict checks against committed entries and pending
// reservations, then the optional preflight, then recording the reservation.
// preflight lets the caller fold a point-in-time check that must hold at the
// moment of reservation (Register's DI publication preflight) into the same
// critical section instead of re-checking after the fact; its error is
// returned unwrapped.
func (r *Registry) reserveIdentified(
	name string,
	valueType reflect.Type,
	lc Lifecycle,
	identity lifecycleIdentity,
	preflight func() error,
) (*registryReservation, error) {
	if r == nil {
		return nil, fmt.Errorf("store: registry must not be nil")
	}
	if isNilDynamicValue(lc) {
		return nil, fmt.Errorf("store: lifecycle must not be nil for %q", name)
	}
	if valueType == nil {
		return nil, fmt.Errorf("store: value type must not be nil for %q", name)
	}
	if !identity.Valid() {
		return nil, fmt.Errorf("store: lifecycle identity must not be empty for %q", name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, e := range r.entries {
		if e.name == name {
			return nil, fmt.Errorf("store: duplicate store name %q", name)
		}
		if e.valueType == valueType {
			return nil, fmt.Errorf("store: duplicate store type %s", valueType)
		}
		if e.lifecycleIdentity == identity {
			return nil, fmt.Errorf("store: lifecycle for %q is already registered as %q", name, e.name)
		}
	}
	if r.findPending(func(p *registryEntry) bool { return p.name == name }) != nil {
		return nil, fmt.Errorf("store: duplicate store name %q", name)
	}
	if r.findPending(func(p *registryEntry) bool { return p.valueType == valueType }) != nil {
		return nil, fmt.Errorf("store: duplicate store type %s", valueType)
	}
	if existing := r.findPending(func(p *registryEntry) bool { return p.lifecycleIdentity == identity }); existing != nil {
		return nil, fmt.Errorf("store: lifecycle for %q is already pending as %q", name, existing.entry.name)
	}
	if preflight != nil {
		if err := preflight(); err != nil {
			return nil, err
		}
	}

	reservation := &registryReservation{
		registry: r,
		active:   true,
		entry: registryEntry{
			name:              name,
			valueType:         valueType,
			lifecycleIdentity: identity,
			lifecycle:         lc,
			probe:             newLifecycleProbe(lc),
		},
	}
	r.pending = append(r.pending, reservation)
	return reservation, nil
}

// findPending returns the first pending reservation whose entry satisfies
// match. The caller must hold r.mu.
func (r *Registry) findPending(match func(*registryEntry) bool) *registryReservation {
	for _, p := range r.pending {
		if match(&p.entry) {
			return p
		}
	}
	return nil
}

// isPending reports whether reservation is currently recorded in the ledger.
// The caller must hold r.mu.
func (r *Registry) isPending(reservation *registryReservation) bool {
	return slices.Contains(r.pending, reservation)
}

// dropPending removes reservation from the ledger. The caller must hold r.mu.
func (r *Registry) dropPending(reservation *registryReservation) {
	r.pending = slices.DeleteFunc(r.pending, func(p *registryReservation) bool { return p == reservation })
}

func identifyLifecycle(lc Lifecycle) (lifecycleIdentity, error) {
	return resourceid.Of(lc)
}

func newLifecycleProbe(lc Lifecycle) *internalhealth.Probe {
	return internalhealth.NewProbe(func(ctx context.Context) internalhealth.Result {
		health := lc.Health(ctx).Clone()
		return internalhealth.Result{
			Status:  string(health.Status),
			Latency: health.Latency,
			Cause:   health.Cause,
		}
	})
}

// commit publishes the DI value and Registry entry while Registry readers are
// excluded. A failed publication leaves the reservation private so the
// caller's deferred release can discard it.
func (reservation *registryReservation) commit(publish func() error) error {
	r := reservation.registry
	r.mu.Lock()
	defer r.mu.Unlock()

	if !reservation.active || !r.isPending(reservation) {
		panic("store: commit inactive registry reservation")
	}
	if err := publish(); err != nil {
		return err
	}
	r.dropPending(reservation)
	r.entries = append(r.entries, reservation.entry)
	reservation.active = false
	return nil
}

// release idempotently discards a failed registration. Pointer identity in the
// ledger prevents a stale release from deleting a later reservation for the
// same key.
func (reservation *registryReservation) release() {
	if reservation == nil || reservation.registry == nil {
		return
	}
	r := reservation.registry
	r.mu.Lock()
	defer r.mu.Unlock()

	if !reservation.active {
		return
	}
	r.dropPending(reservation)
	reservation.active = false
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

func (r *Registry) storeChecks() []internalhealth.StoreCheck {
	r.mu.RLock()
	entries := slices.Clone(r.entries)
	r.mu.RUnlock()

	checks := make([]internalhealth.StoreCheck, 0, len(entries))
	for _, entry := range entries {
		checks = append(checks, internalhealth.StoreCheck{
			Name:  entry.name,
			Probe: entry.probe,
		})
	}
	return checks
}
