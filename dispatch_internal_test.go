package credo

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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

// dispatchOnceForTest runs a single dispatchOnce pass against app for the
// given request, returning the recorder and the handler-chain error. It mirrors
// ServeHTTP's pool acquire/reset/release but stops below the rewrite loop and
// the centralized error renderer, so sentinel returns stay observable.
func dispatchOnceForTest(t *testing.T, app *App, method, target string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	app.handlerOnce.Do(app.compile)
	rec := httptest.NewRecorder()
	c := app.ctxPool.get()
	c.reset(rec, httptest.NewRequest(method, target, nil))
	err := app.dispatchOnce(c)
	app.ctxPool.put(c)
	return rec, err
}

// TestDispatchOnce_UnknownMethod_NotFound locks the documented subtlety that a
// method absent from the radix method map short-circuits to 404 — never 405 —
// because no route could possibly match it.
func TestDispatchOnce_UnknownMethod_NotFound(t *testing.T) {
	app, err := New()
	if err != nil {
		t.Fatal(err)
	}
	app.GET("/x", func(c *Context) error { return c.Response().NoContent(http.StatusNoContent) })

	_, dispatchErr := dispatchOnceForTest(t, app, "FROB", "/x")
	if !errors.Is(dispatchErr, ErrNotFound) {
		t.Fatalf("unknown method dispatch = %v, want ErrNotFound", dispatchErr)
	}
}

// TestDispatchOnce_MethodNotAllowed_SetsAllow verifies the 405 path returns
// the sentinel and stamps the Allow header with the registered method set
// before the error leaves dispatchOnce.
func TestDispatchOnce_MethodNotAllowed_SetsAllow(t *testing.T) {
	app, err := New()
	if err != nil {
		t.Fatal(err)
	}
	app.GET("/x", func(c *Context) error { return c.Response().NoContent(http.StatusNoContent) })
	app.PUT("/x", func(c *Context) error { return c.Response().NoContent(http.StatusNoContent) })

	rec, dispatchErr := dispatchOnceForTest(t, app, http.MethodPost, "/x")
	if !errors.Is(dispatchErr, ErrMethodNotAllowed) {
		t.Fatalf("POST to GET/PUT route = %v, want ErrMethodNotAllowed", dispatchErr)
	}
	allow := rec.Header().Get("Allow")
	for _, want := range []string{http.MethodGet, http.MethodPut} {
		if !strings.Contains(allow, want) {
			t.Errorf("Allow = %q, want it to contain %s", allow, want)
		}
	}
}

// TestDispatchOnce_TrailingSlashRedirect verifies the alternate-path probe:
// GET/HEAD redirect with 301, other methods with 308, and the query string is
// preserved on the Location target.
func TestDispatchOnce_TrailingSlashRedirect(t *testing.T) {
	app, err := New()
	if err != nil {
		t.Fatal(err)
	}
	app.GET("/users", func(c *Context) error { return c.Response().NoContent(http.StatusNoContent) })
	app.POST("/users", func(c *Context) error { return c.Response().NoContent(http.StatusNoContent) })

	tests := []struct {
		name       string
		method     string
		target     string
		wantStatus int
		wantLoc    string
	}{
		{"GET keeps query, 301", http.MethodGet, "/users/?q=1", http.StatusMovedPermanently, "/users?q=1"},
		{"POST preserves method, 308", http.MethodPost, "/users/", http.StatusPermanentRedirect, "/users"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec, dispatchErr := dispatchOnceForTest(t, app, tc.method, tc.target)
			if dispatchErr != nil {
				t.Fatalf("dispatchOnce = %v, want nil (redirect written)", dispatchErr)
			}
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if got := rec.Header().Get("Location"); got != tc.wantLoc {
				t.Errorf("Location = %q, want %q", got, tc.wantLoc)
			}
		})
	}
}

// TestTrailingSlashAlternate locks the pure alternate-path derivation,
// including the root-path guard.
func TestTrailingSlashAlternate(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/", ""},
		{"/users", "/users/"},
		{"/users/", "/users"},
		{"/a/b/", "/a/b"},
	}
	for _, tc := range tests {
		if got := trailingSlashAlternate(tc.path); got != tc.want {
			t.Errorf("trailingSlashAlternate(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}
