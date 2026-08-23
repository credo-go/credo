package credo_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

const bypassWarn = "response bypassed the success envelope"

// newBypassApp builds a debug-mode app with a SuccessRenderer installed and
// its logger captured, the arming conditions of the envelope-bypass
// diagnostic.
func newBypassApp(t *testing.T, logs *syncBuffer, opts ...credo.Option) *credo.App {
	t.Helper()
	all := append([]credo.Option{
		credo.WithDebug(),
		credo.WithLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		credo.WithoutAccessLog(),
	}, opts...)
	app := mustNew(t, all...)
	app.SetSuccessRenderer(func(c *credo.Context, info credo.RenderInfo) any {
		return map[string]any{"data": info.Data}
	})
	return app
}

func doGET(app *credo.App, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

func TestEnvelopeBypass_RawJSONWarnsInDebug(t *testing.T) {
	logs := &syncBuffer{}
	app := newBypassApp(t, logs)
	app.GET("/leak", func(c *credo.Context) error {
		return c.Response().JSON(http.StatusOK, map[string]string{"a": "b"})
	}).Name("leaky")

	doGET(app, "/leak")
	out := logs.String()
	if !strings.Contains(out, bypassWarn) {
		t.Fatalf("expected bypass warning, logs:\n%s", out)
	}
	if !strings.Contains(out, "/leak") || !strings.Contains(out, "leaky") {
		t.Errorf("warning should carry route pattern and name, logs:\n%s", out)
	}
}

func TestEnvelopeBypass_MetaRawResponseSilences(t *testing.T) {
	logs := &syncBuffer{}
	app := newBypassApp(t, logs)
	app.POST("/webhook", func(c *credo.Context) error {
		return c.Response().JSON(http.StatusOK, map[string]string{"received": "true"})
	}).SetMeta(credo.MetaRawResponse, true)

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/webhook", nil))
	if out := logs.String(); strings.Contains(out, bypassWarn) {
		t.Errorf("MetaRawResponse route must not warn, logs:\n%s", out)
	}
}

func TestEnvelopeBypass_GroupMetaInheritsAndRouteOverrides(t *testing.T) {
	logs := &syncBuffer{}
	app := newBypassApp(t, logs)
	g := app.Group("/callbacks")
	g.SetMeta(credo.MetaRawResponse, true)
	g.GET("/quiet", func(c *credo.Context) error {
		return c.Response().JSON(http.StatusOK, map[string]string{"a": "b"})
	})
	g.GET("/loud", func(c *credo.Context) error {
		return c.Response().JSON(http.StatusOK, map[string]string{"a": "b"})
	}).SetMeta(credo.MetaRawResponse, false)

	doGET(app, "/callbacks/quiet")
	if out := logs.String(); strings.Contains(out, bypassWarn) {
		t.Fatalf("group MetaRawResponse must silence inherited routes, logs:\n%s", out)
	}
	doGET(app, "/callbacks/loud")
	if out := logs.String(); !strings.Contains(out, bypassWarn) {
		t.Errorf("route-level false must override the group's true, logs:\n%s", out)
	}
}

func TestEnvelopeBypass_SilentWithoutDebug(t *testing.T) {
	logs := &syncBuffer{}
	app := mustNew(t,
		credo.WithLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		credo.WithoutAccessLog(),
	)
	app.SetSuccessRenderer(func(c *credo.Context, info credo.RenderInfo) any {
		return map[string]any{"data": info.Data}
	})
	app.GET("/leak", func(c *credo.Context) error {
		return c.Response().JSON(http.StatusOK, map[string]string{"a": "b"})
	})

	doGET(app, "/leak")
	if out := logs.String(); strings.Contains(out, bypassWarn) {
		t.Errorf("diagnostic must stay silent without debug mode, logs:\n%s", out)
	}
}

func TestEnvelopeBypass_SilentWithoutRenderer(t *testing.T) {
	logs := &syncBuffer{}
	app := mustNew(t,
		credo.WithDebug(),
		credo.WithLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		credo.WithoutAccessLog(),
	)
	app.GET("/raw", func(c *credo.Context) error {
		return c.Response().JSON(http.StatusOK, map[string]string{"a": "b"})
	})

	doGET(app, "/raw")
	if out := logs.String(); strings.Contains(out, bypassWarn) {
		t.Errorf("diagnostic must stay silent with no SuccessRenderer, logs:\n%s", out)
	}
}

func TestEnvelopeBypass_RenderDoesNotWarn(t *testing.T) {
	logs := &syncBuffer{}
	app := newBypassApp(t, logs)
	app.GET("/ok", func(c *credo.Context) error {
		return c.Render(http.StatusOK, map[string]string{"a": "b"})
	})

	doGET(app, "/ok")
	if out := logs.String(); strings.Contains(out, bypassWarn) {
		t.Errorf("Render is the seam itself and must not warn, logs:\n%s", out)
	}
}

func TestEnvelopeBypass_ErrorPipelineDoesNotWarn(t *testing.T) {
	logs := &syncBuffer{}
	app := newBypassApp(t, logs)
	// An ErrorRenderer body makes the pipeline itself write JSON.
	app.SetErrorRenderer(func(c *credo.Context, info credo.ErrorInfo) any {
		return map[string]any{"error": info.Problem.Title}
	})
	app.GET("/fail", func(c *credo.Context) error {
		return credo.ErrNotFound
	})

	doGET(app, "/fail")
	if out := logs.String(); strings.Contains(out, bypassWarn) {
		t.Errorf("error-pipeline JSON writes are framework-internal, logs:\n%s", out)
	}
}

func TestEnvelopeBypass_NonJSONWritersExempt(t *testing.T) {
	logs := &syncBuffer{}
	app := newBypassApp(t, logs)
	app.GET("/text", func(c *credo.Context) error {
		return c.Response().Text(http.StatusOK, "plain")
	})
	app.GET("/blob", func(c *credo.Context) error {
		return c.Response().Blob(http.StatusOK, "application/octet-stream", []byte{1})
	})

	doGET(app, "/text")
	doGET(app, "/blob")
	if out := logs.String(); strings.Contains(out, bypassWarn) {
		t.Errorf("Text/Blob are never envelope targets, logs:\n%s", out)
	}
}
