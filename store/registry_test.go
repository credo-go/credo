package store_test

import (
	"context"
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
}

func (m *mockLifecycle) Ping(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pingCalled = true
	return m.pingErr
}

func (m *mockLifecycle) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shutCalled = true
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

func TestRegistry_Add(t *testing.T) {
	r := &store.Registry{}
	lc := &mockLifecycle{}

	if err := r.Add("primary", lc); err != nil {
		t.Fatalf("Add() = %v, want nil", err)
	}
}

func TestRegistry_Add_NilLifecycle(t *testing.T) {
	r := &store.Registry{}
	if err := r.Add("nil-lc", nil); err == nil {
		t.Fatal("Add(nil lifecycle) should return error")
	}
}

func TestRegistry_Add_DuplicateName(t *testing.T) {
	r := &store.Registry{}
	lc := &mockLifecycle{}

	if err := r.Add("primary", lc); err != nil {
		t.Fatalf("first Add() = %v, want nil", err)
	}
	if err := r.Add("primary", lc); err == nil {
		t.Fatal("second Add() with same name should return error")
	}
}

func TestRegistry_HealthAll(t *testing.T) {
	r := &store.Registry{}

	r.Add("primary", &mockLifecycle{health: store.Health{Status: store.StatusUp}})
	r.Add("replica", &mockLifecycle{health: store.Health{Status: store.StatusDegraded}})

	result := r.HealthAll(context.Background())
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
	r := &store.Registry{}
	r.Add("primary", &mockLifecycle{health: store.Health{
		Status:  store.StatusUp,
		Details: map[string]any{"driver": "sqlite"},
	}})

	result := r.HealthAll(context.Background())
	result["primary"].Details["driver"] = "mutated"

	refreshed := r.HealthAll(context.Background())
	if got := refreshed["primary"].Details["driver"]; got != "sqlite" {
		t.Fatalf("HealthAll() leaked Details mutation, got %v", got)
	}
}

func TestRegistry_HealthAll_Empty(t *testing.T) {
	r := &store.Registry{}
	result := r.HealthAll(context.Background())
	if len(result) != 0 {
		t.Fatalf("HealthAll() on empty registry returned %d entries, want 0", len(result))
	}
}
