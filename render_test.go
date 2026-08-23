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
	app.SetSuccessRenderer(func(c *credo.Context, info credo.RenderInfo) any {
		return map[string]any{"ok": true, "data": info.Data}
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
	app.SetSuccessRenderer(func(c *credo.Context, info credo.RenderInfo) any {
		return map[string]any{"ok": true, "data": info.Data}
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

func TestRender_RendererNilReturnWritesDataPlain(t *testing.T) {
	app := mustNew(t)
	app.SetSuccessRenderer(func(c *credo.Context, info credo.RenderInfo) any {
		// Envelope selectively: nil = write info.Data without an envelope.
		return nil
	})
	app.GET("/x", func(c *credo.Context) error {
		return c.Render(http.StatusOK, map[string]string{"a": "b"})
	})

	w := renderGET(app, "/x")
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad body: %v (body %q)", err, w.Body.String())
	}
	if len(got) != 1 || got["a"] != "b" {
		t.Errorf("body = %v, want plain {a:b} on nil renderer return", got)
	}
}

func TestRender_CommittedRendererKeepsFullControl(t *testing.T) {
	app := mustNew(t)
	app.SetSuccessRenderer(func(c *credo.Context, info credo.RenderInfo) any {
		_ = c.Response().Text(http.StatusAccepted, "custom")
		// Committed: this return value must be ignored.
		return map[string]string{"must": "not appear"}
	})
	app.GET("/x", func(c *credo.Context) error {
		return c.Render(http.StatusOK, map[string]string{"a": "b"})
	})

	w := renderGET(app, "/x")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (renderer committed)", w.Code)
	}
	if body := w.Body.String(); body != "custom" {
		t.Errorf("body = %q, want the renderer's own committed output", body)
	}
}

func TestRender_OptionsReachRenderInfo(t *testing.T) {
	app := mustNew(t)
	var got credo.RenderInfo
	app.SetSuccessRenderer(func(c *credo.Context, info credo.RenderInfo) any {
		got = info
		return map[string]any{"data": info.Data, "meta": info.Meta}
	})
	app.GET("/x", func(c *credo.Context) error {
		return c.Render(http.StatusCreated, map[string]string{"a": "b"},
			credo.RenderMessageKey("user.created"),
			credo.RenderMeta(map[string]int{"page": 2}),
		)
	})

	w := renderGET(app, "/x")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", w.Code, w.Body.String())
	}
	if got.Status != http.StatusCreated || got.MessageKey != "user.created" {
		t.Errorf("RenderInfo = %+v, want Status=201 MessageKey=user.created", got)
	}
	meta, _ := got.Meta.(map[string]int)
	if meta["page"] != 2 {
		t.Errorf("RenderInfo.Meta = %v, want {page:2}", got.Meta)
	}
}

func TestRender_OptionsSilentlyDropWithoutRenderer(t *testing.T) {
	app := mustNew(t)
	app.GET("/x", func(c *credo.Context) error {
		return c.Render(http.StatusOK, map[string]string{"a": "b"},
			credo.RenderMessageKey("user.created"),
			credo.RenderMeta(map[string]int{"page": 2}),
		)
	})

	w := renderGET(app, "/x")
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad body: %v (body %q)", err, w.Body.String())
	}
	if len(got) != 1 || got["a"] != "b" {
		t.Errorf("body = %v, want plain {a:b} with side channels dropped", got)
	}
}

func TestRender_BodilessStatusSkipsRendererBody(t *testing.T) {
	app := mustNew(t)
	app.SetSuccessRenderer(func(c *credo.Context, info credo.RenderInfo) any {
		return map[string]string{"must": "not appear"}
	})
	app.DELETE("/x", func(c *credo.Context) error {
		return c.Render(http.StatusNoContent, nil)
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/x", nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("body = %q, want empty (bodiless status is status-only)", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "" {
		t.Errorf("Content-Type = %q, want unset", ct)
	}
}

func TestRender_RendererPanicIsRecoveredAs500(t *testing.T) {
	app := mustNew(t, credo.WithoutAccessLog())
	app.SetSuccessRenderer(func(c *credo.Context, info credo.RenderInfo) any {
		panic("renderer exploded")
	})
	app.GET("/x", func(c *credo.Context) error {
		return c.Render(http.StatusOK, map[string]string{"a": "b"})
	})

	w := renderGET(app, "/x")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (renderer panic must hit built-in recovery; body %s)",
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
	app.SetSuccessRenderer(func(c *credo.Context, info credo.RenderInfo) any { return nil })
}
