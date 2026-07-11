package store

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
)

type registryTestLifecycle struct {
	health Health
}

func (*registryTestLifecycle) Ping(context.Context) error      { return nil }
func (*registryTestLifecycle) Shutdown(context.Context) error  { return nil }
func (l *registryTestLifecycle) Health(context.Context) Health { return l.health.Clone() }
func (l *registryTestLifecycle) ResourceIdentity() any         { return l }

type registryReservationTypeA struct{}
type registryReservationTypeB struct{}
type registryLifecycleWrapperA struct{ *registryTestLifecycle }
type registryLifecycleWrapperB struct{ *registryTestLifecycle }

type registryUnstableLifecycle struct {
	values []int
}

func (registryUnstableLifecycle) Ping(context.Context) error     { return nil }
func (registryUnstableLifecycle) Shutdown(context.Context) error { return nil }
func (registryUnstableLifecycle) Health(context.Context) Health  { return Health{Status: StatusUp} }

type registryNaNLifecycle float64

func (registryNaNLifecycle) Ping(context.Context) error     { return nil }
func (registryNaNLifecycle) Shutdown(context.Context) error { return nil }
func (registryNaNLifecycle) Health(context.Context) Health  { return Health{Status: StatusUp} }

type registryCompositeLifecycle struct {
	primary *registryTestLifecycle
	replica *registryTestLifecycle
}

func (*registryCompositeLifecycle) Ping(context.Context) error     { return nil }
func (*registryCompositeLifecycle) Shutdown(context.Context) error { return nil }
func (*registryCompositeLifecycle) Health(context.Context) Health  { return Health{Status: StatusUp} }

func TestRegistryReservation_IsPrivateUntilCommit(t *testing.T) {
	registry := &Registry{}
	lifecycle := &registryTestLifecycle{health: Health{Status: StatusUp}}
	reservation, err := registry.reserve(
		"primary",
		reflect.TypeFor[*registryReservationTypeA](),
		lifecycle,
	)
	if err != nil {
		t.Fatalf("reserve() = %v", err)
	}
	if got := len(registry.HealthAll(t.Context())); got != 0 {
		t.Fatalf("pending HealthAll entries = %d, want 0", got)
	}
	if got := len(registry.storeChecks()); got != 0 {
		t.Fatalf("pending storeChecks entries = %d, want 0", got)
	}

	published := false
	if err := reservation.commit(func() error {
		published = true
		return nil
	}); err != nil {
		t.Fatalf("commit() = %v", err)
	}
	if !published {
		t.Fatal("commit did not publish the DI value")
	}
	if got := len(registry.HealthAll(t.Context())); got != 1 {
		t.Fatalf("committed HealthAll entries = %d, want 1", got)
	}
	if got := len(registry.storeChecks()); got != 1 {
		t.Fatalf("committed storeChecks entries = %d, want 1", got)
	}
}

func TestRegistryReservation_FailedPublishIsInvisibleAndReusable(t *testing.T) {
	registry := &Registry{}
	typeOfA := reflect.TypeFor[*registryReservationTypeA]()
	reservation, err := registry.reserve("primary", typeOfA, &registryTestLifecycle{})
	if err != nil {
		t.Fatalf("reserve() = %v", err)
	}
	wantErr := errors.New("publish failed")
	if err := reservation.commit(func() error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("commit() = %v, want %v", err, wantErr)
	}
	if got := len(registry.HealthAll(t.Context())); got != 0 {
		t.Fatalf("failed commit HealthAll entries = %d, want 0", got)
	}
	reservation.release()

	retry, err := registry.reserve("primary", typeOfA, &registryTestLifecycle{})
	if err != nil {
		t.Fatalf("reserve() after release = %v", err)
	}
	retry.release()
}

func TestRegistryReservation_CommitHidesDIRegistryTransitionFromReaders(t *testing.T) {
	registry := &Registry{}
	reservation, err := registry.reserve(
		"primary",
		reflect.TypeFor[*registryReservationTypeA](),
		&registryTestLifecycle{health: Health{Status: StatusUp}},
	)
	if err != nil {
		t.Fatalf("reserve() = %v", err)
	}
	readLockAvailableDuringPublish := false
	if err := reservation.commit(func() error {
		if registry.mu.TryRLock() {
			readLockAvailableDuringPublish = true
			registry.mu.RUnlock()
		}
		return nil
	}); err != nil {
		t.Fatalf("commit() = %v", err)
	}
	if readLockAvailableDuringPublish {
		t.Fatal("Registry read lock was available while DI publication was in progress")
	}
	if !registry.mu.TryRLock() {
		t.Fatal("Registry read lock remained unavailable after commit")
	}
	registry.mu.RUnlock()
	if got := len(registry.HealthAll(t.Context())); got != 1 {
		t.Fatalf("HealthAll entries after commit = %d, want 1", got)
	}
}

func TestRegistryReservation_RejectsPendingNameAndType(t *testing.T) {
	registry := &Registry{}
	typeOfA := reflect.TypeFor[*registryReservationTypeA]()
	typeOfB := reflect.TypeFor[*registryReservationTypeB]()
	first, err := registry.reserve("primary", typeOfA, &registryTestLifecycle{})
	if err != nil {
		t.Fatalf("first reserve() = %v", err)
	}
	defer first.release()

	if _, err := registry.reserve("primary", typeOfB, &registryTestLifecycle{}); err == nil {
		t.Fatal("reserve should reject a pending duplicate name")
	}
	if _, err := registry.reserve("replica", typeOfA, &registryTestLifecycle{}); err == nil {
		t.Fatal("reserve should reject a pending duplicate type")
	}
}

func TestRegistryReservation_RejectsPendingLifecycleIdentity(t *testing.T) {
	registry := &Registry{}
	lifecycle := &registryTestLifecycle{}
	first, err := registry.reserve(
		"primary",
		reflect.TypeFor[*registryReservationTypeA](),
		lifecycle,
	)
	if err != nil {
		t.Fatalf("first reserve() = %v", err)
	}
	if _, err := registry.reserve(
		"replica",
		reflect.TypeFor[*registryReservationTypeB](),
		lifecycle,
	); err == nil {
		t.Fatal("reserve should reject a pending duplicate lifecycle identity")
	}
	first.release()

	retry, err := registry.reserve(
		"replica",
		reflect.TypeFor[*registryReservationTypeB](),
		lifecycle,
	)
	if err != nil {
		t.Fatalf("reserve() after lifecycle release = %v", err)
	}
	retry.release()
}

func TestRegistryReservation_CanonicalizesEmbeddedLifecycleWrappers(t *testing.T) {
	registry := &Registry{}
	lifecycle := &registryTestLifecycle{}
	first, err := registry.reserve(
		"primary",
		reflect.TypeFor[*registryReservationTypeA](),
		registryLifecycleWrapperA{registryTestLifecycle: lifecycle},
	)
	if err != nil {
		t.Fatalf("first reserve() = %v", err)
	}
	defer first.release()

	if _, err := registry.reserve(
		"analytics",
		reflect.TypeFor[*registryReservationTypeB](),
		registryLifecycleWrapperB{registryTestLifecycle: lifecycle},
	); err == nil {
		t.Fatal("wrappers around the same embedded lifecycle should share one identity")
	}
}

func TestRegistryReservation_StaleReleaseDoesNotCancelRetry(t *testing.T) {
	registry := &Registry{}
	typeOfA := reflect.TypeFor[*registryReservationTypeA]()
	stale, err := registry.reserve("primary", typeOfA, &registryTestLifecycle{})
	if err != nil {
		t.Fatalf("first reserve() = %v", err)
	}
	stale.release()

	retry, err := registry.reserve("primary", typeOfA, &registryTestLifecycle{})
	if err != nil {
		t.Fatalf("retry reserve() = %v", err)
	}
	stale.release()
	if _, err := registry.reserve(
		"primary",
		reflect.TypeFor[*registryReservationTypeB](),
		&registryTestLifecycle{},
	); err == nil {
		t.Fatal("stale release removed the active retry reservation")
	}
	retry.release()
}

func TestRegistryReservation_RejectsNilInputs(t *testing.T) {
	registry := &Registry{}
	var typedNil *registryTestLifecycle
	if _, err := registry.reserve(
		"primary",
		reflect.TypeFor[*registryReservationTypeA](),
		typedNil,
	); err == nil {
		t.Fatal("reserve should reject typed-nil Lifecycle")
	}
	if _, err := registry.reserve("primary", nil, &registryTestLifecycle{}); err == nil {
		t.Fatal("reserve should reject nil value type")
	}
}

func TestRegistryReservation_RejectsLifecycleWithoutStableIdentity(t *testing.T) {
	registry := &Registry{}
	if _, err := registry.reserve(
		"unstable",
		reflect.TypeFor[*registryReservationTypeA](),
		registryUnstableLifecycle{values: []int{1}},
	); err == nil {
		t.Fatal("reserve should reject a non-comparable value Lifecycle without stable identity")
	}
}

func TestRegistryReservation_RejectsNonReflexiveLifecycleIdentity(t *testing.T) {
	registry := &Registry{}
	if _, err := registry.reserve(
		"nan",
		reflect.TypeFor[*registryReservationTypeA](),
		registryNaNLifecycle(math.NaN()),
	); err == nil {
		t.Fatal("reserve should reject a non-reflexive NaN Lifecycle identity")
	}
}

func TestRegistryReservation_CompositePointerUsesTopLevelIdentity(t *testing.T) {
	registry := &Registry{}
	composite := &registryCompositeLifecycle{
		primary: &registryTestLifecycle{},
		replica: &registryTestLifecycle{},
	}
	reservation, err := registry.reserve(
		"composite",
		reflect.TypeFor[*registryReservationTypeA](),
		composite,
	)
	if err != nil {
		t.Fatalf("reserve(composite) = %v", err)
	}
	reservation.release()
}
