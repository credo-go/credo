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
	mu                 sync.RWMutex
	entries            []registryEntry
	reservedNames      map[string]*registryReservation
	reservedTypes      map[reflect.Type]*registryReservation
	reservedLifecycles map[lifecycleIdentity]*registryReservation
}

type registryEntry struct {
	name              string
	valueType         reflect.Type
	lifecycleIdentity lifecycleIdentity
	lifecycle         Lifecycle
	probe             *internalhealth.Probe
}

type registryReservation struct {
	registry          *Registry
	name              string
	valueType         reflect.Type
	lifecycleIdentity lifecycleIdentity
	entry             registryEntry
	active            bool
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
	return r.reserveIdentified(name, valueType, lc, identity)
}

func (r *Registry) reserveIdentified(
	name string,
	valueType reflect.Type,
	lc Lifecycle,
	identity lifecycleIdentity,
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
	if _, exists := r.reservedNames[name]; exists {
		return nil, fmt.Errorf("store: duplicate store name %q", name)
	}
	if _, exists := r.reservedTypes[valueType]; exists {
		return nil, fmt.Errorf("store: duplicate store type %s", valueType)
	}
	if existing, exists := r.reservedLifecycles[identity]; exists {
		return nil, fmt.Errorf("store: lifecycle for %q is already pending as %q", name, existing.name)
	}

	reservation := &registryReservation{
		registry:          r,
		name:              name,
		valueType:         valueType,
		lifecycleIdentity: identity,
		active:            true,
	}
	reservation.entry = registryEntry{
		name:              name,
		valueType:         valueType,
		lifecycleIdentity: identity,
		lifecycle:         lc,
		probe:             newLifecycleProbe(lc),
	}
	if r.reservedNames == nil {
		r.reservedNames = make(map[string]*registryReservation)
	}
	if r.reservedTypes == nil {
		r.reservedTypes = make(map[reflect.Type]*registryReservation)
	}
	if r.reservedLifecycles == nil {
		r.reservedLifecycles = make(map[lifecycleIdentity]*registryReservation)
	}
	r.reservedNames[name] = reservation
	r.reservedTypes[valueType] = reservation
	r.reservedLifecycles[identity] = reservation
	return reservation, nil
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

	if !reservation.active || r.reservedNames[reservation.name] != reservation ||
		r.reservedTypes[reservation.valueType] != reservation ||
		r.reservedLifecycles[reservation.lifecycleIdentity] != reservation {
		panic("store: commit inactive registry reservation")
	}
	if err := publish(); err != nil {
		return err
	}
	delete(r.reservedNames, reservation.name)
	delete(r.reservedTypes, reservation.valueType)
	delete(r.reservedLifecycles, reservation.lifecycleIdentity)
	r.entries = append(r.entries, reservation.entry)
	reservation.active = false
	return nil
}

// release idempotently discards a failed registration. Token identity checks
// prevent a stale release from deleting a later reservation for the same key.
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
	if r.reservedNames[reservation.name] == reservation {
		delete(r.reservedNames, reservation.name)
	}
	if r.reservedTypes[reservation.valueType] == reservation {
		delete(r.reservedTypes, reservation.valueType)
	}
	if r.reservedLifecycles[reservation.lifecycleIdentity] == reservation {
		delete(r.reservedLifecycles, reservation.lifecycleIdentity)
	}
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
