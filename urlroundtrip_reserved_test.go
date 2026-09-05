package credo_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

// RFC 3986 draws the line at reserved characters: an escaped unreserved byte
// is the same URI as the literal one (section 2.3), an escaped reserved
// character is not (section 2.2). The canonical form keeps reserved escapes,
// so "%3B" never meets static ";", is never a ";" delimiter, and survives a
// mount handoff — while a captured value still decodes it.
func TestURLRoundTrip_ReservedCharactersKeepTheirEncoding(t *testing.T) {
	app := mustNew(t)
	app.GET("/lit/a;b", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, "lit")
	})
	app.GET("/item/{id}", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, ctx.Request().RouteParam("id"))
	})
	tail := app.GET("/t/{id};v", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, ctx.Request().RouteParam("id"))
	})
	kv := app.GET("/kv/{k}={v}", func(ctx *credo.Context) error {
		r := ctx.Request()
		return ctx.Response().Text(http.StatusOK, r.RouteParam("k")+"|"+r.RouteParam("v"))
	})
	var childPath, childRaw string
	app.Mount("/api", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		childPath, childRaw = r.URL.Path, r.URL.EscapedPath()
	}))

	tests := []struct {
		target   string
		status   int
		wantBody string
	}{
		{"/lit/a;b", 200, "lit"},
		{"/lit/a%3Bb", 404, "not_found"}, // a reserved escape is a different URI
		{"/item/a;b", 200, "a;b"},
		{"/item/a%3Bb", 200, "a;b"}, // the captured value is decoded
		{"/item/a%2Db", 200, "a-b"}, // an unreserved escape is the same URI
		{"/t/a%3Bb;v", 200, "a;b"},  // an encoded ";" is not the delimiter
		{"/t/a%3bb;v", 200, "a;b"},
		{"/t/a;b;v", 404, "not_found"}, // the literal ";" is
		{"/kv/a%3Db=c", 200, "a=b|c"},
		{"/kv/a=b=c", 200, "a|b=c"},
	}
	for _, tt := range tests {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tt.target, nil))
		if w.Code != tt.status || !strings.Contains(w.Body.String(), tt.wantBody) {
			t.Errorf("GET %s = %d %q, want %d containing %q", tt.target, w.Code, w.Body.String(), tt.status, tt.wantBody)
		}
	}

	// The mount handoff keeps the reserved escape and normalizes the
	// unreserved one, so the child sees the client's URI.
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x%3By/%2D", nil))
	if w.Code != http.StatusOK || childRaw != "/x%3By/-" || childPath != "/x;y/-" {
		t.Fatalf("mount = %d, child Path/EscapedPath = %q/%q, want /x;y/- and /x%%3By/-", w.Code, childPath, childRaw)
	}

	// Generation escapes a reserved delimiter that url.PathEscape encodes
	// (";"), so the value routes back; one it leaves literal ("=") cannot.
	uri, err := tail.BuildURI("a;b")
	if err != nil || uri != "/t/a%3Bb;v" {
		t.Fatalf("BuildURI(\"a;b\") = %q, %v", uri, err)
	}
	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, uri, nil))
	if w.Code != http.StatusOK || w.Body.String() != "a;b" {
		t.Fatalf("GET %s = %d %q", uri, w.Code, w.Body.String())
	}
	if _, err := kv.BuildURI("a=b", "c"); err == nil || !strings.Contains(err.Error(), `contains its delimiter "="`) {
		t.Fatalf("BuildURI(\"a=b\") error = %v, want delimiter rejection", err)
	}
	if uri, err := kv.BuildURI("a", "b=c"); err != nil || uri != "/kv/a=b=c" {
		t.Fatalf("BuildURI(\"a\", \"b=c\") = %q, %v", uri, err)
	}
}

// A literal "%" after a parameter is the "%25" unit in the canonical form;
// the candidate scan must not mistake the "%" of another escape for it.
func TestURLRoundTrip_PercentDelimiter(t *testing.T) {
	app := mustNew(t)
	route := app.GET("/x/{id}%done", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, ctx.Request().RouteParam("id"))
	})

	tests := []struct {
		target   string
		status   int
		wantBody string
	}{
		{"/x/7%25done", 200, "7"},
		{"/x/%2F%25done", 200, "/"},
		{"/x/a%3Bb%25done", 200, "a;b"},
		{"/x/a%25b%25done", 404, "not_found"}, // the first "%25" is the delimiter
		{"/x/7done", 404, "not_found"},
	}
	for _, tt := range tests {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tt.target, nil))
		if w.Code != tt.status || !strings.Contains(w.Body.String(), tt.wantBody) {
			t.Errorf("GET %s = %d %q, want %d containing %q", tt.target, w.Code, w.Body.String(), tt.status, tt.wantBody)
		}
	}

	uri, err := route.BuildURI("/")
	if err != nil || uri != "/x/%2F%25done" {
		t.Fatalf("BuildURI(\"/\") = %q, %v", uri, err)
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, uri, nil))
	if w.Code != http.StatusOK || w.Body.String() != "/" {
		t.Fatalf("GET %s = %d %q, want 200 \"/\"", uri, w.Code, w.Body.String())
	}
	if _, err := route.BuildURI("a%b"); err == nil || !strings.Contains(err.Error(), `contains its delimiter "%"`) {
		t.Fatalf("BuildURI(\"a%%b\") error = %v, want delimiter rejection", err)
	}
}
