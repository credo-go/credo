package credo_test

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

// The router matches the path the client sent: it removes no dot-segment and
// merges no empty segment (ADR-007). These tests pin that, so that a change
// that cleans the path, or redirects to its clean form, fails here.

// serveText registers each pattern with a handler that answers its own
// pattern, and returns a function that serves a request target.
func serveText(t *testing.T, patterns ...string) func(target string) *httptest.ResponseRecorder {
	t.Helper()
	app := mustNew(t)
	for _, p := range patterns {
		app.GET(p, func(ctx *credo.Context) error {
			return ctx.Response().Text(http.StatusOK, p)
		})
	}
	return func(target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec
	}
}

func TestPathAsReceived_NoDotSegmentRemoval(t *testing.T) {
	serve := serveText(t, "/b", "/a/b")

	for _, target := range []string{"/a/../b", "/a/./b", "/./b", "/a/b/..", "/x/%2E%2E/b"} {
		t.Run(target, func(t *testing.T) {
			rec := serve(target)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("GET %s = %d %q, want 404: the path is matched as received, never cleaned", target, rec.Code, rec.Body.String())
			}
			if loc := rec.Header().Get("Location"); loc != "" {
				t.Fatalf("GET %s redirected to %q; the router never redirects to a clean form", target, loc)
			}
		})
	}
}

func TestPathAsReceived_NoEmptySegmentMerge(t *testing.T) {
	serve := serveText(t, "/a", "/a/b")

	for _, target := range []string{"//a", "/a//b", "//a/b", "/a/b//"} {
		t.Run(target, func(t *testing.T) {
			rec := serve(target)
			if rec.Code == http.StatusOK {
				t.Fatalf("GET %s reached %q; empty segments are never merged", target, rec.Body.String())
			}
			if loc := rec.Header().Get("Location"); loc == "/a" || loc == "/a/b" {
				t.Fatalf("GET %s redirected to %q; the router never redirects to a merged form", target, loc)
			}
		})
	}
}

func TestPathAsReceived_ParameterCapturesDotDot(t *testing.T) {
	app := mustNew(t)
	one := echoParam(t, app, "/files/{name}", "name")
	mid := echoParam(t, app, "/a/{x}/b", "x")
	all := echoParam(t, app, "/tree/{path...}", "path")

	tests := []struct {
		name   string
		serve  func(string) (int, string)
		target string
		want   string
	}{
		{"single segment", one, "/files/..", ".."},
		{"encoded dot-segment decodes once", one, "/files/%2E%2E", ".."},
		{"single dot", one, "/files/.", "."},
		{"between static segments", mid, "/a/../b", ".."},
		{"catch-all keeps dot-segments", all, "/tree/a/../b", "a/../b"},
		{"catch-all starting with dot-dot", all, "/tree/../etc", "../etc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, got := tt.serve(tt.target)
			if status != http.StatusOK || got != tt.want {
				t.Fatalf("GET %s = %d %q, want 200 %q: a parameter captures dot-segments as data", tt.target, status, got, tt.want)
			}
		})
	}
}

// TestPathAsReceived_OverTheWire sends the dot-segment on a raw connection, so
// neither a client nor httptest can resolve it before the server sees it.
func TestPathAsReceived_OverTheWire(t *testing.T) {
	app := mustNew(t)
	app.GET("/b", func(ctx *credo.Context) error { return ctx.Response().Text(http.StatusOK, "b") })
	app.GET("/a/{x}/b", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, "x="+ctx.Request().RouteParam("x"))
	})
	srv := httptest.NewServer(app)
	defer srv.Close()

	get := func(target string) (int, string) {
		t.Helper()
		conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err = conn.Write([]byte("GET " + target + " HTTP/1.1\r\nHost: example.test\r\nConnection: close\r\n\r\n")); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(body)
	}

	if status, body := get("/a/../b"); status != http.StatusOK || body != "x=.." {
		t.Fatalf("GET /a/../b = %d %q, want 200 \"x=..\"", status, body)
	}
	if status, body := get("/c/../b"); status != http.StatusNotFound {
		t.Fatalf("GET /c/../b = %d %q, want 404 (not /b)", status, body)
	}
	if status, body := get("//b"); status == http.StatusOK {
		t.Fatalf("GET //b = %d %q, want no match (not /b)", status, body)
	}
}

// TestPathAsReceived_NoStdlibPathValues pins that the dispatcher does not set
// the standard request's path values for a route (ADR-008): Credo code reads
// parameters through Request.RouteParam and Request.PathValue.
func TestPathAsReceived_NoStdlibPathValues(t *testing.T) {
	app := mustNew(t)
	app.GET("/users/{id}", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK,
			ctx.Request().RouteParam("id")+"|"+ctx.Request().PathValue("id")+"|"+ctx.Request().Request.PathValue("id"))
	})

	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/users/42", nil))
	if got, want := rec.Body.String(), "42|42|"; rec.Code != http.StatusOK || got != want {
		t.Fatalf("GET /users/42 = %d %q, want 200 %q: the embedded request carries no path values", rec.Code, got, want)
	}
}
