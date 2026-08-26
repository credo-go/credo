package credo_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

type queryMethodInput struct {
	Term      string `json:"term"`
	validated bool
}

func (in *queryMethodInput) Validate() error {
	in.validated = true
	return nil
}

func TestApp_QUERY_BindBodyAndValidate(t *testing.T) {
	app := mustNew(t)
	app.QUERY("/search", func(ctx *credo.Context) error {
		var in queryMethodInput
		if err := ctx.Request().BindBody(&in); err != nil {
			return err
		}
		if !in.validated {
			t.Fatal("Validatable was not called")
		}
		return ctx.Response().JSON(http.StatusOK, map[string]string{"term": in.Term})
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("QUERY", "/search", strings.NewReader(`{"term":"credo"}`))
	r.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"term":"credo"`) {
		t.Fatalf("body = %q, want decoded term", w.Body.String())
	}
}

func TestGroup_QUERY(t *testing.T) {
	app := mustNew(t)
	app.Group("/api").QUERY("/search", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusNoContent)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("QUERY", "/api/search", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
}

func TestQUERY_ContentTypeRequired(t *testing.T) {
	tests := []struct {
		name string
		body string
		ct   string
	}{
		{name: "body", body: `{}`},
		{name: "empty body"},
		{name: "blank header", body: `{}`, ct: " \t "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			called := false
			middlewareRan := false
			route := app.QUERY("/search", func(ctx *credo.Context) error {
				called = true
				return nil
			})
			route.Middleware(func(next credo.Handler) credo.Handler {
				return func(ctx *credo.Context) error {
					middlewareRan = true
					return next(ctx)
				}
			})

			w := httptest.NewRecorder()
			r := httptest.NewRequest("QUERY", "/search", strings.NewReader(tt.body))
			if tt.ct != "" {
				r.Header.Set("Content-Type", tt.ct)
			}
			app.ServeHTTP(w, r)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusBadRequest, w.Body.String())
			}
			if called {
				t.Fatal("application handler was called")
			}
			if !middlewareRan {
				t.Fatal("route middleware did not run before QUERY content guard")
			}
			body := w.Body.String()
			if !strings.Contains(body, `"code":"content_type_required"`) {
				t.Fatalf("body = %q, want content_type_required code", body)
			}
			if !strings.Contains(body, `"message":"Content-Type is required for QUERY requests."`) {
				t.Fatalf("body = %q, want built-in English fallback", body)
			}
		})
	}
}

func TestQUERY_ContentTypeRequiredUsesI18n(t *testing.T) {
	app := mustNew(t)
	if err := app.UseI18n(credo.I18nConfig{
		Messages: credo.I18nMessages{
			"content_type_required": "QUERY içeriğinin türü belirtilmelidir.",
		},
	}); err != nil {
		t.Fatal(err)
	}
	app.QUERY("/search", func(*credo.Context) error {
		t.Fatal("application handler was called")
		return nil
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("QUERY", "/search", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"message":"QUERY içeriğinin türü belirtilmelidir."`) {
		t.Fatalf("body = %q, want localized message", w.Body.String())
	}
}

func TestQUERY_ContentTypeErrorsUseExistingPipeline(t *testing.T) {
	app := mustNew(t)
	app.QUERY("/search", func(ctx *credo.Context) error {
		var in queryMethodInput
		return ctx.Request().BindBody(&in)
	})

	tests := []struct {
		name     string
		ct       string
		body     string
		wantCode int
		wantWire string
	}{
		{name: "unsupported", ct: "application/octet-stream", body: "x", wantCode: http.StatusUnsupportedMediaType, wantWire: "unsupported_media_type"},
		{name: "malformed", ct: "application/json", body: `{`, wantCode: http.StatusBadRequest, wantWire: "bind_failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("QUERY", "/search", strings.NewReader(tt.body))
			r.Header.Set("Content-Type", tt.ct)
			app.ServeHTTP(w, r)
			if w.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tt.wantCode, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), `"code":"`+tt.wantWire+`"`) {
				t.Fatalf("body = %q, want code %q", w.Body.String(), tt.wantWire)
			}
		})
	}
}

func TestQUERY_MethodSemantics(t *testing.T) {
	app := mustNew(t)
	app.GET("/get-only", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusNoContent)
	})
	route := app.QUERY("/search/{kind}", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusNoContent)
	}).Name("search.query")

	uri, err := route.BuildURI("users")
	if err != nil {
		t.Fatalf("BuildURI: %v", err)
	}
	if uri != "/search/users" {
		t.Fatalf("BuildURI = %q, want /search/users", uri)
	}

	routes := app.Routes()
	found := false
	for _, info := range routes {
		if info.Method == "QUERY" && info.Pattern == "/search/{kind}" && info.Name == "search.query" {
			found = true
		}
	}
	if !found {
		t.Fatalf("QUERY route missing from introspection: %+v", routes)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/search/users", nil)
	app.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("HEAD status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
	if got := w.Header().Get("Allow"); !strings.Contains(got, "QUERY") {
		t.Fatalf("Allow = %q, want QUERY", got)
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest("QUERY", "/get-only", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("QUERY on GET-only path status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
	if got := w.Header().Get("Allow"); !strings.Contains(got, "GET") || !strings.Contains(got, "HEAD") || strings.Contains(got, "QUERY") {
		t.Fatalf("Allow = %q, want GET and HEAD without QUERY", got)
	}
}

func TestQUERY_TrailingSlashRedirect(t *testing.T) {
	app := mustNew(t)
	app.QUERY("/search/", func(ctx *credo.Context) error { return nil })

	w := httptest.NewRecorder()
	r := httptest.NewRequest("QUERY", "/search", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, r)
	if w.Code != http.StatusPermanentRedirect {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusPermanentRedirect)
	}
	if got := w.Header().Get("Location"); got != "/search/" {
		t.Fatalf("Location = %q, want /search/", got)
	}
}

func TestQUERY_MountForwardingAndContentGuard(t *testing.T) {
	app := mustNew(t)
	var gotMethod string
	app.Mount("/child", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.WriteHeader(http.StatusNoContent)
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("QUERY", "/child/search", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent || gotMethod != "QUERY" {
		t.Fatalf("status/method = %d/%q, want %d/QUERY", w.Code, gotMethod, http.StatusNoContent)
	}

	gotMethod = ""
	w = httptest.NewRecorder()
	r = httptest.NewRequest("QUERY", "/child/search", nil)
	app.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing Content-Type status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	if gotMethod != "" {
		t.Fatal("mounted handler ran without Content-Type")
	}
}
