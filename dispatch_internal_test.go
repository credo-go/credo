package credo

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestDiscardBodyWriter_Unwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &discardBodyWriter{ResponseWriter: rec}

	// http.ResponseController reaches optional interfaces through Unwrap.
	// Without Unwrap, Flush would fail with ErrNotSupported even though
	// the underlying recorder implements http.Flusher.
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		t.Fatalf("Flush through discardBodyWriter = %v, want nil", err)
	}
	if !rec.Flushed {
		t.Error("expected the underlying writer to be flushed")
	}
}

// TestMount_IntrospectionMethodsMatchRegistration guards the single-source
// contract: the method set introspection reports for a mount equals both
// mountForwardedMethods and the set actually inserted into the radix store.
func TestMount_IntrospectionMethodsMatchRegistration(t *testing.T) {
	app, err := New()
	if err != nil {
		t.Fatal(err)
	}
	app.Mount("/svc", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	var introspected []string
	for _, ri := range app.Routes() {
		if ri.Kind == RouteKindMount && ri.Pattern == "/svc" {
			introspected = ri.Methods
		}
	}

	// Methods actually registered for the exact mount pattern (the exact-match
	// handler; the catch-all lives at "/svc/{_mount...}").
	var registered []string
	for _, e := range app.mux.store.snapshot() {
		if e.pattern == "/svc" {
			registered = append(registered, e.method)
		}
	}
	slices.Sort(registered)

	if !slices.Equal(introspected, mountForwardedMethods()) {
		t.Errorf("introspection Methods %v != mountForwardedMethods %v", introspected, mountForwardedMethods())
	}
	if !slices.Equal(introspected, registered) {
		t.Errorf("introspection Methods %v != radix-registered set %v", introspected, registered)
	}
}
