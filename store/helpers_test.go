package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/credo-go/credo"
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
	pingCalls   int
	shutCalls   int
	healthCalls int
	pingCtx     context.Context
}

func (m *mockLifecycle) Ping(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pingCalls++
	m.pingCtx = ctx
	return m.pingErr
}

func (m *mockLifecycle) Shutdown(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shutCalls++
	if m.shutdownSeq != nil {
		*m.shutdownSeq = append(*m.shutdownSeq, m.name)
	}
	return m.shutdownErr
}

func (m *mockLifecycle) Health(context.Context) store.Health {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.healthCalls++
	return m.health.Clone()
}

// ResourceIdentity names the mock as the resource, so wrappers that embed one
// mock are holders of one resource.
func (m *mockLifecycle) ResourceIdentity() any { return m }

func (m *mockLifecycle) calls() (ping, shutdown, health int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pingCalls, m.shutCalls, m.healthCalls
}

func newTestApp(t *testing.T, opts ...credo.Option) *credo.App {
	t.Helper()
	app, err := credo.New(opts...)
	if err != nil {
		t.Fatalf("credo.New() = %v", err)
	}
	return app
}

// startApp runs the App's start phase and shuts the App down when the test
// ends.
func startApp(t *testing.T, app *credo.App) {
	t.Helper()
	t.Cleanup(func() { shutdownApp(t, app) })
	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("Start() = %v", err)
	}
}

// shutdownApp shuts the App down; a state error (already stopped) is fine.
func shutdownApp(t *testing.T, app *credo.App) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = app.Shutdown(ctx)
}
