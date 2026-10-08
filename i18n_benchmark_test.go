package credo_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/validation"
)

func newI18nBenchApp(b *testing.B) *credo.App {
	b.Helper()
	app := mustNewBench(b)

	fsys := fstest.MapFS{
		// Keys are exact: validation violations resolve their bare Code
		// ("required") and HTTP errors their bare default code ("not_found").
		"en/messages.json": &fstest.MapFile{
			Data: []byte(`{"required": "is required", "not_found": "Not found"}`),
		},
		"tr/messages.json": &fstest.MapFile{
			Data: []byte(`{"required": "zorunludur", "not_found": "Bulunamadı"}`),
		},
	}

	app.UseI18n(credo.I18nConfig{
		DirFS:   fsys,
		Default: "en",
	})

	return app
}

// startI18nBenchApp runs the start phase, which reads the catalogs, once the
// benchmark's routes are registered.
func startI18nBenchApp(b *testing.B, app *credo.App) {
	b.Helper()
	b.Cleanup(func() { _ = app.Shutdown(context.WithoutCancel(b.Context())) })
	if err := app.Start(b.Context()); err != nil {
		b.Fatal(err)
	}
}

func BenchmarkUseI18n_T(b *testing.B) {
	app := newI18nBenchApp(b)
	app.GET("/bench", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, ctx.T("required"))
	})

	r := httptest.NewRequest("GET", "/bench", nil)
	r.Header.Set("Accept-Language", "tr")
	w := newNoopResponseWriter()
	startI18nBenchApp(b, app)
	benchExpect(b, app, r, http.StatusOK, "zorunludur")

	b.ReportAllocs()
	for b.Loop() {
		clear(w.h)
		app.ServeHTTP(w, r)
	}
}

func BenchmarkUseI18n_ValidationError(b *testing.B) {
	app := newI18nBenchApp(b)
	ve := validation.Errors{
		{Field: "email", Code: "required", Message: "is required"},
		{Field: "name", Code: "required", Message: "is required"},
	}

	app.POST("/bench", func(ctx *credo.Context) error {
		return ve
	})

	r := httptest.NewRequest("POST", "/bench", nil)
	r.Header.Set("Accept-Language", "tr")
	w := newNoopResponseWriter()
	startI18nBenchApp(b, app)
	benchExpect(b, app, r, http.StatusUnprocessableEntity, "zorunludur")

	b.ReportAllocs()
	for b.Loop() {
		clear(w.h)
		app.ServeHTTP(w, r)
	}
}

func BenchmarkUseI18n_HTTPError(b *testing.B) {
	app := newI18nBenchApp(b)
	app.GET("/bench", func(ctx *credo.Context) error {
		return credo.NewHTTPError(http.StatusNotFound)
	})

	r := httptest.NewRequest("GET", "/bench", nil)
	r.Header.Set("Accept-Language", "tr")
	w := newNoopResponseWriter()
	startI18nBenchApp(b, app)
	benchExpect(b, app, r, http.StatusNotFound, "Bulunamadı")

	b.ReportAllocs()
	for b.Loop() {
		clear(w.h)
		app.ServeHTTP(w, r)
	}
}
