package credo_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

func renderGET(app *credo.App, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

func TestRender_DefaultIsPlainJSON(t *testing.T) {
	app := mustNew(t)
	app.GET("/x", func(c *credo.Context) error {
		return c.Render(http.StatusOK, map[string]string{"a": "b"})
	})

	w := renderGET(app, "/x")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body not a plain JSON object: %v (body %q)", err, w.Body.String())
	}
	if len(got) != 1 || got["a"] != "b" {
		t.Errorf("body = %v, want {a:b} with no envelope", got)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestRender_UsesInstalledRenderer(t *testing.T) {
	app := mustNew(t)
	app.SetSuccessRenderer(func(c *credo.Context, status int, data any) error {
		return c.Response().JSON(status, map[string]any{"ok": true, "data": data})
	})
	app.GET("/x", func(c *credo.Context) error {
		return c.Render(http.StatusCreated, map[string]string{"a": "b"})
	})

	w := renderGET(app, "/x")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad body: %v", err)
	}
	if got["ok"] != true {
		t.Errorf("envelope missing ok=true: %v", got)
	}
	if data, _ := got["data"].(map[string]any); data["a"] != "b" {
		t.Errorf("envelope data = %v, want {a:b}", got["data"])
	}
}

func TestRender_RawHelpersBypassRenderer(t *testing.T) {
	app := mustNew(t)
	app.SetSuccessRenderer(func(c *credo.Context, status int, data any) error {
		return c.Response().JSON(status, map[string]any{"ok": true, "data": data})
	})
	app.GET("/raw", func(c *credo.Context) error {
		// Raw helper — must NOT be wrapped in the envelope.
		return c.Response().JSON(http.StatusOK, map[string]string{"a": "b"})
	})

	w := renderGET(app, "/raw")
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad body: %v", err)
	}
	if _, enveloped := got["ok"]; enveloped {
		t.Errorf("raw Response().JSON was wrapped by the renderer: %v", got)
	}
	if got["a"] != "b" {
		t.Errorf("raw body = %v, want {a:b}", got)
	}
}

func TestRender_RendererErrorFlowsToErrorPipeline(t *testing.T) {
	app := mustNew(t)
	app.SetSuccessRenderer(func(c *credo.Context, status int, data any) error {
		// Return without writing: the error must reach the error pipeline.
		return credo.NewHTTPError(http.StatusServiceUnavailable, "render failed")
	})
	app.GET("/x", func(c *credo.Context) error {
		return c.Render(http.StatusOK, map[string]string{"a": "b"})
	})

	w := renderGET(app, "/x")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (renderer error must reach the error pipeline; body %s)",
			w.Code, w.Body.String())
	}
}

func TestRender_NilAppFallsBackToJSON(t *testing.T) {
	w := httptest.NewRecorder()
	c := credo.NewContext(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if err := c.Render(http.StatusOK, map[string]string{"a": "b"}); err != nil {
		t.Fatalf("Render on app-less context: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad body: %v (body %q)", err, w.Body.String())
	}
	if got["a"] != "b" {
		t.Errorf("body = %v, want {a:b}", got)
	}
}

func TestSetSuccessRenderer_FrozenPanics(t *testing.T) {
	app := mustNew(t)
	app.GET("/x", func(c *credo.Context) error { return nil })
	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic from SetSuccessRenderer after compile")
		}
	}()
	app.SetSuccessRenderer(func(c *credo.Context, status int, data any) error { return nil })
}
