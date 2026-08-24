package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/credo-go/credo/store"
)

// mockLifecycle records method calls for testing.
type mockLifecycle struct {
	pingErr     error
	shutdownErr error
	health      store.Health
	shutdownSeq *[]string // shared slice to record shutdown order
	name        string
	mu          sync.Mutex
	pingCalled  bool
	shutCalled  bool
	pingCalls   int
	shutCalls   int
	pingStarted chan<- struct{}
	pingRelease <-chan struct{}
}

func (m *mockLifecycle) Ping(ctx context.Context) error {
	m.mu.Lock()
	m.pingCalled = true
	m.pingCalls++
	err := m.pingErr
	started := m.pingStarted
	release := m.pingRelease
	m.mu.Unlock()

	if started != nil {
		started <- struct{}{}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (m *mockLifecycle) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shutCalled = true
	m.shutCalls++
	if m.shutdownSeq != nil {
		*m.shutdownSeq = append(*m.shutdownSeq, m.name)
	}
	return m.shutdownErr
}

func (m *mockLifecycle) Health(ctx context.Context) store.Health {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.health.Clone()
}

func (m *mockLifecycle) ResourceIdentity() any {
	return m
}

func TestRegistry_HasNoPublicMutationMethod(t *testing.T) {
	if _, exists := reflect.TypeOf(&store.Registry{}).MethodByName("Add"); exists {
		t.Fatal("Registry.Add bypasses Register invariants and must not be exported")
	}
}

type registryPrimaryStore struct{ *mockLifecycle }
type registryReplicaStore struct{ *mockLifecycle }

func registeredRegistry(t *testing.T, primary, replica *mockLifecycle) *store.Registry {
	t.Helper()
	app := newTestApp(t)
	if err := store.Register[*registryPrimaryStore](
		app,
		&registryPrimaryStore{mockLifecycle: primary},
		store.WithName("primary"),
	); err != nil {
		t.Fatalf("Register(primary) = %v", err)
	}
	if replica != nil {
		if err := store.Register[*registryReplicaStore](
			app,
			&registryReplicaStore{mockLifecycle: replica},
			store.WithName("replica"),
		); err != nil {
			t.Fatalf("Register(replica) = %v", err)
		}
	}
	registry, err := app.Resolve[*store.Registry]()
	if err != nil {
		t.Fatalf("Resolve[*Registry]() = %v", err)
	}
	return registry
}

func TestRegistry_HealthAll(t *testing.T) {
	r := registeredRegistry(t,
		&mockLifecycle{health: store.Health{Status: store.StatusUp}},
		&mockLifecycle{health: store.Health{Status: store.StatusDegraded}},
	)

	result := r.HealthAll(t.Context())
	if len(result) != 2 {
		t.Fatalf("HealthAll() returned %d entries, want 2", len(result))
	}
	if result["primary"].Status != store.StatusUp {
		t.Errorf("primary status = %q, want %q", result["primary"].Status, store.StatusUp)
	}
	if result["replica"].Status != store.StatusDegraded {
		t.Errorf("replica status = %q, want %q", result["replica"].Status, store.StatusDegraded)
	}
}

func TestRegistry_HealthAll_ClonesDetails(t *testing.T) {
	r := registeredRegistry(t, &mockLifecycle{health: store.Health{
		Status:  store.StatusUp,
		Details: map[string]any{"driver": "sqlite"},
	}}, nil)

	result := r.HealthAll(t.Context())
	result["primary"].Details["driver"] = "mutated"

	refreshed := r.HealthAll(t.Context())
	if got := refreshed["primary"].Details["driver"]; got != "sqlite" {
		t.Fatalf("HealthAll() leaked Details mutation, got %v", got)
	}
}

func TestHealthClone_PreservesCauseIdentity(t *testing.T) {
	cause := errors.New("connection refused")
	health := store.Health{Status: store.StatusDown, Cause: cause}

	clone := health.Clone()
	if !errors.Is(clone.Cause, cause) {
		t.Fatalf("Clone().Cause = %v, want original cause identity", clone.Cause)
	}
}

func TestHealthCause_IsExcludedFromJSON(t *testing.T) {
	const secret = "dial tcp internal-db:5432"
	payload, err := json.Marshal(store.Health{
		Status: store.StatusDown,
		Cause:  errors.New(secret),
	})
	if err != nil {
		t.Fatalf("json.Marshal(Health): %v", err)
	}
	if strings.Contains(string(payload), secret) || strings.Contains(string(payload), "Cause") {
		t.Fatalf("Health JSON leaked Cause: %s", payload)
	}
}

func TestRegistry_HealthAll_Empty(t *testing.T) {
	r := &store.Registry{}
	result := r.HealthAll(t.Context())
	if len(result) != 0 {
		t.Fatalf("HealthAll() on empty registry returned %d entries, want 0", len(result))
	}
}
