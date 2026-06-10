package credo_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/validation"
)

func newI18nBenchApp(b *testing.B) *credo.App {
	b.Helper()
	app, err := credo.New()
	if err != nil {
		b.Fatal(err)
	}

	fsys := fstest.MapFS{
		"en/messages.json": &fstest.MapFile{
			Data: []byte(`{"v.required": "is required", "http.not_found": "Not found"}`),
		},
		"tr/messages.json": &fstest.MapFile{
			Data: []byte(`{"v.required": "zorunludur", "http.not_found": "Bulunamadı"}`),
		},
	}

	if err := app.UseI18n(credo.I18nConfig{
		DirFS:   fsys,
		Default: "en",
	}); err != nil {
		b.Fatal(err)
	}

	return app
}

func BenchmarkUseI18n_T(b *testing.B) {
	app := newI18nBenchApp(b)
	app.GET("/bench", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, ctx.T("v.required"))
	})

	r := httptest.NewRequest("GET", "/bench", nil)
	r.Header.Set("Accept-Language", "tr")

	b.ReportAllocs()
	for b.Loop() {
		w := httptest.NewRecorder()
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

	b.ReportAllocs()
	for b.Loop() {
		w := httptest.NewRecorder()
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

	b.ReportAllocs()
	for b.Loop() {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
	}
}
