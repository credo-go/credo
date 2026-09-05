package credo_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

// Static pattern text and the request path meet in one canonical form: every
// client spelling of the same bytes reaches the route, and an encoded slash
// stays data.
func TestURLRoundTrip_StaticTextCanonicalForm(t *testing.T) {
	app := mustNew(t)
	route := app.GET("/café/{id}", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, ctx.Request().RouteParam("id"))
	})
	app.GET("/100%/{id}", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, "pct:"+ctx.Request().RouteParam("id"))
	})

	tests := []struct {
		target   string
		status   int
		wantBody string
	}{
		{"/caf%C3%A9/42", 200, "42"},
		{"/caf%c3%a9/42", 200, "42"},
		{"/%63af%C3%A9/42", 200, "42"},
		{"/caf%C3%A9/a%2Fb", 200, "a/b"},
		{"/caf%C3%A9/%C3%A7", 200, "ç"},
		{"/caf%C3%A9/%FF", 400, "invalid_path_encoding"},
		{"/cafe/42", 404, "not_found"},
		{"/100%25/7", 200, "pct:7"},
		{"/100/7", 404, "not_found"},
	}
	for _, tt := range tests {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tt.target, nil))
		if w.Code != tt.status || !strings.Contains(w.Body.String(), tt.wantBody) {
			t.Errorf("GET %s = %d %q, want %d containing %q", tt.target, w.Code, w.Body.String(), tt.status, tt.wantBody)
		}
	}

	// Generation writes static text in its wire spelling and the result routes.
	uri, err := route.BuildURI("42")
	if err != nil {
		t.Fatal(err)
	}
	if uri != "/caf%C3%A9/42" {
		t.Fatalf("BuildURI = %q, want /caf%%C3%%A9/42", uri)
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, uri, nil))
	if w.Code != http.StatusOK || w.Body.String() != "42" {
		t.Fatalf("generated URI %s = %d %q", uri, w.Code, w.Body.String())
	}
}

func TestURLRoundTrip_CanonicalFormTrailingSlashAndMount(t *testing.T) {
	app := mustNew(t)
	app.GET("/café/list/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, "list")
	})
	var childPath, childRaw string
	app.Mount("/été", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		childPath, childRaw = r.URL.Path, r.URL.EscapedPath()
	}))

	// The redirect Location is spelled for the wire, not with raw octets.
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/caf%c3%a9/list?x=1", nil))
	if w.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301", w.Code)
	}
	if got := w.Header().Get("Location"); got != "/caf%C3%A9/list/?x=1" {
		t.Fatalf("Location = %q, want /caf%%C3%%A9/list/?x=1", got)
	}

	// A mount below non-ASCII static text hands over the remainder with the
	// client's segment boundaries intact.
	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/%C3%A9t%C3%A9/a%2Fb/%C3%A7", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("mount status = %d", w.Code)
	}
	if childPath != "/a/b/ç" || childRaw != "/a%2Fb/%C3%A7" {
		t.Fatalf("child Path/EscapedPath = %q/%q, want /a/b/ç and /a%%2Fb/%%C3%%A7", childPath, childRaw)
	}
}

// Generation refuses values that cannot round-trip: a value containing the
// parameter's delimiter (which matching cuts at, since "%2E" and "." are the
// same path) and invalid UTF-8 (which the router answers with 400).
func TestURLRoundTrip_GenerationRejectsNonRoundTrippingValues(t *testing.T) {
	app := mustNew(t)
	tail := app.GET("/files/{name}.json", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, ctx.Request().RouteParam("name"))
	})
	dash := app.GET("/v/{a}-{b}", func(ctx *credo.Context) error { return nil })
	plain := app.GET("/item/{id}", func(ctx *credo.Context) error { return nil })
	nested := app.GET("/dir/{id}/x", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, ctx.Request().RouteParam("id"))
	})

	if _, err := tail.BuildURI("a.b"); err == nil || !strings.Contains(err.Error(), `contains its delimiter "."`) {
		t.Fatalf("BuildURI(\"a.b\") error = %v, want delimiter rejection", err)
	}
	if uri, err := tail.BuildURI("ab"); err != nil || uri != "/files/ab.json" {
		t.Fatalf("BuildURI(\"ab\") = %q, %v", uri, err)
	}
	if _, err := dash.BuildURI("x-y", "z"); err == nil {
		t.Fatal("BuildURI with a dash in a dash-delimited value should fail")
	}
	if uri, err := dash.BuildURI("x", "y-z"); err != nil || uri != "/v/x-y-z" {
		t.Fatalf("BuildURI last parameter may contain the dash: %q, %v", uri, err)
	}
	for _, r := range []*credo.Route{plain, nested} {
		if _, err := r.BuildURI(string([]byte{0xff})); err == nil || !strings.Contains(err.Error(), "not valid UTF-8") {
			t.Fatalf("BuildURI(invalid UTF-8) error = %v, want UTF-8 rejection", err)
		}
	}
	// A slash-delimited parameter still accepts a slash: it is escaped and
	// stays data.
	uri, err := nested.BuildURI("a/b")
	if err != nil || uri != "/dir/a%2Fb/x" {
		t.Fatalf("BuildURI(\"a/b\") = %q, %v", uri, err)
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, uri, nil))
	if w.Code != http.StatusOK || w.Body.String() != "a/b" {
		t.Fatalf("GET %s = %d %q", uri, w.Code, w.Body.String())
	}
}
