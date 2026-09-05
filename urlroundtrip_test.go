package credo_test

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

// echoParam registers pattern and returns a function that serves path and
// reports the status plus the named parameter as seen by the handler.
func echoParam(t *testing.T, app *credo.App, pattern, name string) func(path string) (int, string) {
	t.Helper()
	app.GET(pattern, func(ctx *credo.Context) error {
		return ctx.Response().Text(200, ctx.Request().RouteParam(name))
	})
	return func(path string) (int, string) {
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec.Code, rec.Body.String()
	}
}

func TestURLRoundTrip_MatchingDecodesOnce(t *testing.T) {
	app := mustNew(t)
	serve := echoParam(t, app, "/one/{v}", "v")

	tests := []struct {
		path string
		want string
	}{
		{"/one/%2F", "/"},
		{"/one/a%2Fb", "a/b"},
		{"/one/%252F", "%2F"},
		{"/one/%31", "1"},
		{"/one/+", "+"},
		{"/one/a+b", "a+b"},
		{"/one/%C3%A7", "ç"},
		{"/one/caf%C3%A9", "café"},
		{"/one/a%20b", "a b"},
		{"/one/plain", "plain"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			status, got := serve(tt.path)
			if status != 200 || got != tt.want {
				t.Fatalf("GET %s = %d %q, want 200 %q", tt.path, status, got, tt.want)
			}
		})
	}

	t.Run("encoded slash does not create a segment", func(t *testing.T) {
		if status, _ := serve("/one/a/b"); status != 404 {
			t.Fatalf("GET /one/a/b = %d, want 404", status)
		}
	})
}

func TestURLRoundTrip_RegexOnDecodedValue(t *testing.T) {
	t.Run("encoded digit satisfies numeric constraint", func(t *testing.T) {
		app := mustNew(t)
		serve := echoParam(t, app, "/num/{id:[0-9]+}", "id")
		if status, got := serve("/num/%31"); status != 200 || got != "1" {
			t.Fatalf("GET /num/%%31 = %d %q, want 200 \"1\"", status, got)
		}
		if status, _ := serve("/num/1%2F2"); status != 404 {
			t.Fatalf("GET /num/1%%2F2 = %d, want 404 (decoded \"1/2\" fails [0-9]+)", status)
		}
	})

	t.Run("constraint applies to the whole value", func(t *testing.T) {
		app := mustNew(t)
		serve := echoParam(t, app, "/num/{id:[0-9]+}", "id")
		if status, _ := serve("/num/12ab"); status != 404 {
			t.Fatalf("GET /num/12ab = %d, want 404 (no prefix match)", status)
		}
	})

	t.Run("mismatch backtracks to the parameter sibling", func(t *testing.T) {
		app := mustNew(t)
		app.GET("/x/{id:[0-9]+}", func(ctx *credo.Context) error {
			return ctx.Response().Text(200, "id="+ctx.Request().RouteParam("id"))
		})
		serve := echoParam(t, app, "/x/{name}", "name")
		if status, got := serve("/x/%31"); status != 200 || got != "id=1" {
			t.Fatalf("GET /x/%%31 = %d %q, want 200 \"id=1\"", status, got)
		}
		if status, got := serve("/x/%61bc"); status != 200 || got != "abc" {
			t.Fatalf("GET /x/%%61bc = %d %q, want 200 \"abc\"", status, got)
		}
	})

	t.Run("encoded delimiter is data", func(t *testing.T) {
		app := mustNew(t)
		app.GET("/rel/{year:[0-9]{4}}-{month:[0-9]{2}}", func(ctx *credo.Context) error {
			r := ctx.Request()
			return ctx.Response().Text(200, r.RouteParam("year")+"|"+r.RouteParam("month"))
		})
		get := func(path string) (int, string) {
			rec := httptest.NewRecorder()
			app.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
			return rec.Code, rec.Body.String()
		}
		if status, got := get("/rel/2024-09"); status != 200 || got != "2024|09" {
			t.Fatalf("GET /rel/2024-09 = %d %q", status, got)
		}
		if status, _ := get("/rel/2024%2D09"); status != 404 {
			t.Fatalf("GET /rel/2024%%2D09 = %d, want 404 (\"%%2D\" is not the delimiter)", status)
		}
	})

	t.Run("a constrained parameter is one segment", func(t *testing.T) {
		app := mustNew(t)
		serve := echoParam(t, app, "/span/{rest:.+}", "rest")
		if status, got := serve("/span/a%2Fb"); status != 200 || got != "a/b" {
			t.Fatalf("GET /span/a%%2Fb = %d %q, want 200 \"a/b\"", status, got)
		}
		if status, _ := serve("/span/a/b"); status != 404 {
			t.Fatalf("GET /span/a/b = %d, want 404 (regex never spans a raw slash)", status)
		}
	})
}

func TestURLRoundTrip_TailParameterIsOneSegment(t *testing.T) {
	app := mustNew(t)
	serve := echoParam(t, app, "/f/{name}.json", "name")
	if status, got := serve("/f/a%2Fb.json"); status != 200 || got != "a/b" {
		t.Fatalf("GET /f/a%%2Fb.json = %d %q, want 200 \"a/b\"", status, got)
	}
	if status, got := serve("/f/a.b.json"); status != 404 {
		t.Fatalf("GET /f/a.b.json = %d %q, want 404 (tail byte bounds the value)", status, got)
	}
	if status, _ := serve("/f/a/b.json"); status != 404 {
		t.Fatalf("GET /f/a/b.json = %d, want 404 (a raw slash ends the segment)", status)
	}
}

func TestURLRoundTrip_CatchAllKeepsSeparators(t *testing.T) {
	app := mustNew(t)
	serve := echoParam(t, app, "/files/{path...}", "path")
	if status, got := serve("/files/a%2Fb/c%20d/%C3%A7"); status != 200 || got != "a/b/c d/ç" {
		t.Fatalf("GET catch-all = %d %q, want 200 \"a/b/c d/ç\"", status, got)
	}
}

func TestURLRoundTrip_InvalidEncodingIs400(t *testing.T) {
	app := mustNew(t)
	app.GET("/one/{v}", func(ctx *credo.Context) error { return ctx.Response().NoContent(204) })
	app.GET("/num/{id:[0-9]+}", func(ctx *credo.Context) error { return ctx.Response().NoContent(204) })
	app.GET("/files/{path...}", func(ctx *credo.Context) error { return ctx.Response().NoContent(204) })
	app.GET("/static/only", func(ctx *credo.Context) error { return ctx.Response().NoContent(204) })

	for _, path := range []string{"/one/%FF", "/num/%FF", "/files/a/%FF"} {
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 400 {
			t.Fatalf("GET %s = %d, want 400", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"code":"invalid_path_encoding"`) {
			t.Fatalf("GET %s body = %s, want invalid_path_encoding", path, rec.Body.String())
		}
	}

	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest("GET", "/static/%FF", nil))
	if rec.Code != 404 {
		t.Fatalf("GET /static/%%FF = %d, want 404 (no parameter could capture it)", rec.Code)
	}
}

func TestURLRoundTrip_MalformedEscapeRejectedByNetHTTP(t *testing.T) {
	app := mustNew(t)
	app.GET("/one/{v}", func(ctx *credo.Context) error { return ctx.Response().NoContent(204) })
	srv := httptest.NewServer(app)
	defer srv.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.Write([]byte("GET /one/%zz HTTP/1.1\r\nHost: example.test\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400 from net/http", resp.StatusCode)
	}
}

func TestURLRoundTrip_TrailingSlashRedirectKeepsEncoding(t *testing.T) {
	app := mustNew(t)
	app.GET("/one/{v}", func(ctx *credo.Context) error { return ctx.Response().NoContent(204) })
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest("GET", "/one/a%2Fb/", nil))
	if rec.Code != 301 || rec.Header().Get("Location") != "/one/a%2Fb" {
		t.Fatalf("redirect = %d %q, want 301 /one/a%%2Fb", rec.Code, rec.Header().Get("Location"))
	}
}

func TestURLRoundTrip_RewriteTargetIsWireForm(t *testing.T) {
	app := mustNew(t)
	var original string
	app.GET("/one/{v}", func(ctx *credo.Context) error {
		original = ctx.OriginalPath()
		return ctx.Response().Text(200, ctx.Request().RouteParam("v"))
	})
	app.GET("/old", func(ctx *credo.Context) error { return ctx.Rewrite("/one/a%2Fb?x=1") })
	var rewriteErr error
	app.GET("/bad", func(ctx *credo.Context) error {
		rewriteErr = ctx.Rewrite("/one/%zz")
		return rewriteErr
	})

	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest("GET", "/old", nil))
	if rec.Code != 200 || rec.Body.String() != "a/b" {
		t.Fatalf("rewrite = %d %q, want 200 \"a/b\"", rec.Code, rec.Body.String())
	}
	if original != "/old" {
		t.Fatalf("OriginalPath = %q, want /old", original)
	}

	rec = httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest("GET", "/bad", nil))
	if rewriteErr == nil || !strings.Contains(rewriteErr.Error(), "invalid URL escape") {
		t.Fatalf("Rewrite(/one/%%zz) error = %v, want invalid URL escape", rewriteErr)
	}
	if rec.Code != 500 {
		t.Fatalf("status after rejected rewrite = %d, want 500", rec.Code)
	}
}

func TestURLRoundTrip_OriginalPathIsWireForm(t *testing.T) {
	app := mustNew(t)
	var original string
	app.GET("/one/{v}", func(ctx *credo.Context) error {
		original = ctx.OriginalPath()
		return ctx.Response().NoContent(204)
	})
	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/one/a%25b", nil))
	if original != "/one/a%25b" {
		t.Fatalf("OriginalPath = %q, want /one/a%%25b", original)
	}
}

func TestURLRoundTrip_MountHandsOverRawRemainder(t *testing.T) {
	t.Run("stdlib handler sees Path and RawPath", func(t *testing.T) {
		app := mustNew(t)
		app.Mount("/admin", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(r.URL.Path + "|" + r.URL.RawPath + "|" + r.URL.EscapedPath()))
		}))
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/a%2Fb/c", nil))
		if got, want := rec.Body.String(), "/a/b/c|/a%2Fb/c|/a%2Fb/c"; got != want {
			t.Fatalf("mounted URL = %q, want %q", got, want)
		}
		rec = httptest.NewRecorder()
		app.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/plain", nil))
		if got, want := rec.Body.String(), "/plain||/plain"; got != want {
			t.Fatalf("mounted URL = %q, want %q", got, want)
		}
	})

	t.Run("nested credo app decodes once", func(t *testing.T) {
		child := mustNew(t)
		child.GET("/{v}", func(ctx *credo.Context) error {
			return ctx.Response().Text(200, ctx.Request().RouteParam("v"))
		})
		app := mustNew(t)
		app.Mount("/admin", child)
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/a%2Fb", nil))
		if rec.Code != 200 || rec.Body.String() != "a/b" {
			t.Fatalf("nested = %d %q, want 200 \"a/b\"", rec.Code, rec.Body.String())
		}
	})
}

func TestURLRoundTrip_Generation(t *testing.T) {
	h := func(ctx *credo.Context) error { return ctx.Response().Text(200, ctx.Request().RouteParam("v")) }

	t.Run("single segment values", func(t *testing.T) {
		app := mustNew(t)
		route := app.GET("/one/{v}", h)
		tests := []struct{ value, want string }{
			{"/", "/one/%2F"},
			{"a/b", "/one/a%2Fb"},
			{"%2F", "/one/%252F"},
			{"1", "/one/1"},
			{"+", "/one/+"},
			{"ç", "/one/%C3%A7"},
			{"a b", "/one/a%20b"},
			{"a?b#c", "/one/a%3Fb%23c"},
		}
		for _, tt := range tests {
			uri, err := route.BuildURI(tt.value)
			if err != nil || uri != tt.want {
				t.Fatalf("BuildURI(%q) = %q, %v; want %q", tt.value, uri, err, tt.want)
			}
			// Round trip: the generated URI reaches the handler with the same value.
			rec := httptest.NewRecorder()
			app.ServeHTTP(rec, httptest.NewRequest("GET", uri, nil))
			if rec.Code != 200 || rec.Body.String() != tt.value {
				t.Fatalf("GET %s = %d %q, want 200 %q", uri, rec.Code, rec.Body.String(), tt.value)
			}
		}
	})

	t.Run("empty value is rejected", func(t *testing.T) {
		app := mustNew(t)
		route := app.GET("/one/{v}", h)
		if _, err := route.BuildURI(""); err == nil || !strings.Contains(err.Error(), "empty value") {
			t.Fatalf("BuildURI(\"\") error = %v, want empty value", err)
		}
	})

	t.Run("constraint is validated", func(t *testing.T) {
		app := mustNew(t)
		route := app.GET("/num/{id:[0-9]+}", h)
		if uri, err := route.BuildURI("42"); err != nil || uri != "/num/42" {
			t.Fatalf("BuildURI(42) = %q, %v", uri, err)
		}
		if _, err := route.BuildURI("x1"); err == nil || !strings.Contains(err.Error(), "does not match constraint") {
			t.Fatalf("BuildURI(x1) error = %v, want constraint failure", err)
		}
		if _, err := route.BuildURI("1/2"); err == nil {
			t.Fatal("BuildURI(1/2) should fail the constraint")
		}
	})

	t.Run("catch-all keeps separators and escapes segments", func(t *testing.T) {
		app := mustNew(t)
		route := app.GET("/files/{v...}", h)
		uri, err := route.BuildURI("a/b c/ç")
		if err != nil || uri != "/files/a/b%20c/%C3%A7" {
			t.Fatalf("BuildURI = %q, %v", uri, err)
		}
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, httptest.NewRequest("GET", uri, nil))
		if rec.Code != 200 || rec.Body.String() != "a/b c/ç" {
			t.Fatalf("GET %s = %d %q", uri, rec.Code, rec.Body.String())
		}
	})

	t.Run("static text is written verbatim", func(t *testing.T) {
		app := mustNew(t)
		route := app.GET("/rel/{y:[0-9]{4}}-{m:[0-9]{2}}/x", h)
		uri, err := route.BuildURI("2024", "09")
		if err != nil || uri != "/rel/2024-09/x" {
			t.Fatalf("BuildURI = %q, %v", uri, err)
		}
	})
}

func TestURLRoundTrip_HostLabelValidation(t *testing.T) {
	h := func(ctx *credo.Context) error { return nil }

	t.Run("valid label", func(t *testing.T) {
		app := mustNew(t)
		route := app.Host("{tenant}.myapp.com").GET("/users/{id}", h)
		u, err := route.BuildURL("acme-1", "a/b")
		if err != nil || u != "acme-1.myapp.com/users/a%2Fb" {
			t.Fatalf("BuildURL = %q, %v", u, err)
		}
	})

	t.Run("label cannot contain a dot, slash or be empty", func(t *testing.T) {
		app := mustNew(t)
		route := app.Host("{tenant}.myapp.com").GET("/users/{id}", h)
		for _, bad := range []string{"", "a.b", "a/b", "ac me", "%41"} {
			if _, err := route.BuildURL(bad, "1"); err == nil {
				t.Fatalf("BuildURL(%q) should fail", bad)
			}
		}
	})

	t.Run("host constraint is validated", func(t *testing.T) {
		app := mustNew(t)
		route := app.Host("{org:[a-z]+}.myapp.com").GET("/x", h)
		if u, err := route.BuildURL("acme"); err != nil || u != "acme.myapp.com/x" {
			t.Fatalf("BuildURL(acme) = %q, %v", u, err)
		}
		if _, err := route.BuildURL("ACME1"); err == nil || !strings.Contains(err.Error(), "host constraint") {
			t.Fatalf("BuildURL(ACME1) error = %v, want host constraint failure", err)
		}
	})
}
