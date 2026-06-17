package credo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestReadinessHandler_Draining verifies the readiness endpoint reports the
// instance unready (503 shutting_down) once shutdown has begun, while a
// non-draining app reports ready.
func TestReadinessHandler_Draining(t *testing.T) {
	app, err := New()
	if err != nil {
		t.Fatal(err)
	}
	app.UseHealth()

	// Not draining: ready (no checks registered -> "up").
	w := httptest.NewRecorder()
	c := NewContext(w, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if err := app.readinessHandler(c); err != nil {
		t.Fatalf("readinessHandler (not draining): %v", err)
	}
	if w.Code != http.StatusOK {
		t.Errorf("not draining: status = %d, want %d", w.Code, http.StatusOK)
	}

	// Draining: 503 with a shutting_down status.
	app.draining.Store(true)
	w = httptest.NewRecorder()
	c = NewContext(w, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if err := app.readinessHandler(c); err != nil {
		t.Fatalf("readinessHandler (draining): %v", err)
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("draining: status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(w.Body.String(), "shutting_down") {
		t.Errorf("draining: body = %q, want it to contain %q", w.Body.String(), "shutting_down")
	}
}

// TestLivenessHandler_UpWhileDraining verifies liveness stays up during
// shutdown — the process is alive even as it drains, so it must not be killed.
func TestLivenessHandler_UpWhileDraining(t *testing.T) {
	app, err := New()
	if err != nil {
		t.Fatal(err)
	}
	app.UseHealth()
	app.draining.Store(true)

	w := httptest.NewRecorder()
	c := NewContext(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	if err := app.livenessHandler(c); err != nil {
		t.Fatalf("livenessHandler: %v", err)
	}
	if w.Code != http.StatusOK {
		t.Errorf("liveness while draining: status = %d, want %d", w.Code, http.StatusOK)
	}
}

// TestShutdown_MarksDraining verifies graceful shutdown sets the draining flag,
// which is clear while the server is running.
func TestShutdown_MarksDraining(t *testing.T) {
	app, err := New(WithAddr("127.0.0.1", 0))
	if err != nil {
		t.Fatal(err)
	}
	app.GET("/ping", func(c *Context) error { return c.Response().Text(http.StatusOK, "pong") })

	errCh := make(chan error, 1)
	go func() { errCh <- app.RunContext(context.Background()) }()
	for i := 0; i < 200 && !app.IsRunning(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !app.IsRunning() {
		t.Fatal("server did not reach running state")
	}
	if app.draining.Load() {
		t.Error("draining should be false while running")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	<-errCh

	if !app.draining.Load() {
		t.Error("draining should be true after shutdown")
	}
}
