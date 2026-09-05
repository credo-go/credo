package credo_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/credo-go/credo"
)

func expectPanicContaining(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil || !strings.Contains(fmt.Sprint(r), want) {
			t.Fatalf("panic = %v, want containing %q", r, want)
		}
	}()
	fn()
}

func TestUseErrorRenderer_Misuse(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		app := mustNew(t)
		expectPanicContaining(t, "App.UseErrorRenderer: nil renderer", func() { app.UseErrorRenderer(nil) })
	})
	t.Run("twice", func(t *testing.T) {
		app := mustNew(t)
		app.UseErrorRenderer(credo.RFC9457ErrorRenderer())
		expectPanicContaining(t, "App.UseErrorRenderer called twice", func() {
			app.UseErrorRenderer(credo.RFC9457ErrorRenderer())
		})
	})
	t.Run("after prepare", func(t *testing.T) {
		app := mustNew(t)
		app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		expectPanicContaining(t, "App.UseErrorRenderer", func() {
			app.UseErrorRenderer(credo.RFC9457ErrorRenderer())
		})
	})
}

func TestUseSuccessRenderer_Misuse(t *testing.T) {
	renderer := func(*credo.Context, credo.RenderInfo) any { return nil }
	t.Run("nil", func(t *testing.T) {
		app := mustNew(t)
		expectPanicContaining(t, "App.UseSuccessRenderer: nil renderer", func() { app.UseSuccessRenderer(nil) })
	})
	t.Run("twice", func(t *testing.T) {
		app := mustNew(t)
		app.UseSuccessRenderer(renderer)
		expectPanicContaining(t, "App.UseSuccessRenderer called twice", func() {
			app.UseSuccessRenderer(renderer)
		})
	})
}

func TestUseAccessLog_Misuse(t *testing.T) {
	t.Run("twice", func(t *testing.T) {
		app := mustNew(t)
		app.UseAccessLog()
		expectPanicContaining(t, "App.UseAccessLog called twice", func() { app.UseAccessLog() })
	})
	t.Run("two configs", func(t *testing.T) {
		app := mustNew(t)
		expectPanicContaining(t, "at most one config", func() {
			app.UseAccessLog(credo.AccessLogConfig{}, credo.AccessLogConfig{})
		})
	})
	t.Run("after shutdown", func(t *testing.T) {
		app := mustNew(t)
		if err := app.Shutdown(t.Context()); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		expectPanicContaining(t, "App.UseAccessLog", func() { app.UseAccessLog() })
	})
}

// TestFeatures_InstallOrderIrrelevant installs every feature in two orders
// and checks the request executor produces the same runtime plan: request
// ID resolved before the access record, decompression before global
// middleware, compression around error rendering, access bytes counted after
// the compressor closed.
func TestFeatures_InstallOrderIrrelevant(t *testing.T) {
	type observed struct {
		status    int
		encoding  string
		requestID string
		logged    map[string]any
		mwBody    string
	}
	run := func(t *testing.T, install func(app *credo.App)) observed {
		t.Helper()
		logger, buf := newTestLogger(t)
		app := mustNew(t, credo.WithLogger(logger))
		install(app)
		var mwBody string
		app.GlobalMiddleware(func(next credo.Handler) credo.Handler {
			return func(ctx *credo.Context) error {
				if ctx.RequestID() == "" {
					t.Error("request ID not resolved before global middleware")
				}
				var in struct {
					Name string `json:"name"`
				}
				if err := ctx.Request().BindBody(&in); err != nil {
					return err
				}
				mwBody = in.Name
				return next(ctx)
			}
		})
		app.POST("/items", func(ctx *credo.Context) error {
			return credo.NewHTTPError(http.StatusConflict, "conflict").WithMessageKey(strings.Repeat("x", 300))
		})

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/items", strings.NewReader(string(gzipBytes(t, `{"name":"Bob"}`))))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Content-Encoding", "gzip")
		r.Header.Set("Accept-Encoding", "gzip")
		r.Header.Set("X-Request-Id", "order-test")
		app.ServeHTTP(w, r)

		var entry map[string]any
		if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
			t.Fatalf("parse log: %v\nraw: %s", err, buf.String())
		}
		return observed{
			status:    w.Code,
			encoding:  w.Header().Get("Content-Encoding"),
			requestID: w.Header().Get("X-Request-Id"),
			logged:    entry,
			mwBody:    mwBody,
		}
	}

	a := run(t, func(app *credo.App) {
		app.UseRequestID()
		app.UseAccessLog()
		app.UseCompress()
		app.UseDecompress()
	})
	b := run(t, func(app *credo.App) {
		app.UseDecompress()
		app.UseCompress()
		app.UseAccessLog()
		app.UseRequestID()
	})

	for name, o := range map[string]observed{"a": a, "b": b} {
		if o.status != http.StatusConflict || o.encoding != "gzip" || o.requestID != "order-test" || o.mwBody != "Bob" {
			t.Fatalf("%s: status/encoding/requestID/mwBody = %d/%q/%q/%q", name, o.status, o.encoding, o.requestID, o.mwBody)
		}
		if o.logged["request_id"] != "order-test" || o.logged["status"] != float64(http.StatusConflict) {
			t.Fatalf("%s: access record = %v", name, o.logged)
		}
	}
	if a.logged["bytes"] != b.logged["bytes"] {
		t.Fatalf("logged bytes differ between install orders: %v vs %v", a.logged["bytes"], b.logged["bytes"])
	}
}

func TestRecover_CoversFeatureSelectors(t *testing.T) {
	logger, buf := newTestLogger(t)
	app := mustNew(t, credo.WithLogger(logger))
	app.UseAccessLog(credo.AccessLogConfig{
		Skipper: func(*credo.Context) bool { panic("skipper boom") },
	})
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, "unreachable")
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if !strings.Contains(buf.String(), "panic recovered") || !strings.Contains(buf.String(), "skipper boom") {
		t.Fatalf("expected panic record for the selector panic, got: %s", buf.String())
	}
}

func TestAccessLog_ResultFilterPanic(t *testing.T) {
	t.Run("recovery on: diagnostic, entry skipped", func(t *testing.T) {
		logger, buf := newTestLogger(t)
		app := mustNew(t, credo.WithLogger(logger))
		app.UseAccessLog(credo.AccessLogConfig{
			ResultFilter: func(*credo.Context, credo.AccessLogEntry) bool { panic("filter boom") },
		})
		app.GET("/", func(ctx *credo.Context) error {
			return ctx.Response().Text(http.StatusOK, "ok")
		})

		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		if w.Code != http.StatusOK || w.Body.String() != "ok" {
			t.Fatalf("response = %d %q, want 200 ok (filter runs after the final response)", w.Code, w.Body.String())
		}
		if strings.Contains(buf.String(), "request completed") {
			t.Fatalf("entry emitted despite filter panic: %s", buf.String())
		}
		if !strings.Contains(buf.String(), "access-log result filter panicked") {
			t.Fatalf("expected framework diagnostic, got: %s", buf.String())
		}
	})

	t.Run("recovery off: propagates after cleanup", func(t *testing.T) {
		app := mustNew(t, credo.WithoutRecover())
		app.UseAccessLog(credo.AccessLogConfig{
			ResultFilter: func(*credo.Context, credo.AccessLogEntry) bool { panic("filter boom") },
		})
		app.GET("/", func(ctx *credo.Context) error {
			return ctx.Response().Text(http.StatusOK, "ok")
		})
		expectPanicContaining(t, "filter boom", func() {
			app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		})
		// The Context was released: the next request works normally.
		w := httptest.NewRecorder()
		func() {
			defer func() { _ = recover() }()
			app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		}()
		if w.Code != http.StatusOK {
			t.Fatalf("second request status = %d, want 200", w.Code)
		}
	})
}

func TestRecover_DisabledStillObservesAccessLog(t *testing.T) {
	logger, buf := newTestLogger(t)
	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRecover())
	app.UseAccessLog()
	app.GET("/", func(*credo.Context) error { panic("boom") })

	expectPanicContaining(t, "boom", func() {
		app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	})
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("parse log: %v\nraw: %s", err, buf.String())
	}
	if entry["status"] != float64(http.StatusInternalServerError) {
		t.Fatalf("escaped-panic access status = %v, want 500 fallback", entry["status"])
	}
}

// --- lazy i18n ---

func newLazyI18nApp(t *testing.T, calls *atomic.Int32, detect func(*credo.Context) string, opts ...credo.Option) *credo.App {
	t.Helper()
	app := mustNew(t, opts...)
	if err := app.UseI18n(credo.I18nConfig{
		DirFS:   i18nTestFS(),
		Default: "en",
		Detect: func(ctx *credo.Context) string {
			calls.Add(1)
			return detect(ctx)
		},
	}); err != nil {
		t.Fatalf("UseI18n: %v", err)
	}
	return app
}

func acceptLanguage(ctx *credo.Context) string { return ctx.Request().Header.Get("Accept-Language") }

func TestI18n_Lazy_UnusedLocaleDoesNotDetect(t *testing.T) {
	var calls atomic.Int32
	app := newLazyI18nApp(t, &calls, acceptLanguage)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, "plain")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Language", "tr")
	app.ServeHTTP(w, r)
	if w.Code != http.StatusOK || calls.Load() != 0 {
		t.Fatalf("status = %d, detector calls = %d, want 200 and 0", w.Code, calls.Load())
	}
}

func TestI18n_Lazy_DetectsOnceAndMemoizes(t *testing.T) {
	var calls atomic.Int32
	app := newLazyI18nApp(t, &calls, acceptLanguage)
	app.GET("/", func(ctx *credo.Context) error {
		first := ctx.Locale()
		// Later request mutation does not change the memoized result.
		ctx.Request().Header.Set("Accept-Language", "en")
		second := ctx.Locale()
		return ctx.Response().Text(http.StatusOK, first+"/"+second+"/"+ctx.T("required")+"/"+ctx.TPlural("items", 1))
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Language", "tr")
	app.ServeHTTP(w, r)
	if got := w.Body.String(); got != "tr/tr/zorunludur/tek öğe" {
		t.Fatalf("body = %q", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("detector calls = %d, want 1", calls.Load())
	}
}

func TestI18n_Lazy_PoolReuseResetsMemo(t *testing.T) {
	var calls atomic.Int32
	app := newLazyI18nApp(t, &calls, acceptLanguage)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, ctx.Locale())
	})

	for i, lang := range []string{"tr", "en", "tr"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Accept-Language", lang)
		app.ServeHTTP(w, r)
		if w.Body.String() != lang {
			t.Fatalf("request %d: locale = %q, want %q", i, w.Body.String(), lang)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("detector calls = %d, want 3 (one per request)", calls.Load())
	}
}

func TestI18n_Lazy_ErrorPathTriggersFirstDetection(t *testing.T) {
	var calls atomic.Int32
	app := newLazyI18nApp(t, &calls, acceptLanguage)
	app.GET("/", func(*credo.Context) error { return credo.ErrNotFound })

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Language", "tr")
	app.ServeHTTP(w, r)
	var body credo.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Message != "Bulunamadı" || calls.Load() != 1 {
		t.Fatalf("message = %q, calls = %d; want Bulunamadı and 1", body.Error.Message, calls.Load())
	}
}

func TestI18n_Lazy_EarlyMiddlewareReadWins(t *testing.T) {
	var calls atomic.Int32
	app := newLazyI18nApp(t, &calls, func(ctx *credo.Context) string {
		if _, ok := ctx.GetUser[string](); ok {
			return "tr"
		}
		return "en"
	})
	var early string
	app.GlobalMiddleware(func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			early = ctx.Locale() // before auth: default
			ctx.SetUser("alice")
			return next(ctx)
		}
	})
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, ctx.Locale())
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if early != "en" || w.Body.String() != "en" || calls.Load() != 1 {
		t.Fatalf("early = %q, handler = %q, calls = %d; want en/en/1 (first read fixes the result)", early, w.Body.String(), calls.Load())
	}
}

func TestI18n_Lazy_PostAuthReadSeesUser(t *testing.T) {
	var calls atomic.Int32
	app := newLazyI18nApp(t, &calls, func(ctx *credo.Context) string {
		if _, ok := ctx.GetUser[string](); ok {
			return "tr"
		}
		return "en"
	})
	app.GlobalMiddleware(func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			ctx.SetUser("alice")
			return next(ctx)
		}
	})
	app.GET("/", func(*credo.Context) error { return credo.ErrNotFound })

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	var body credo.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Message != "Bulunamadı" {
		t.Fatalf("message = %q, want the authenticated user's language", body.Error.Message)
	}
}

func TestI18n_Lazy_EmptyOrUnresolvableSelectsDefault(t *testing.T) {
	for _, lang := range []string{"", "xx-unknown"} {
		var calls atomic.Int32
		app := newLazyI18nApp(t, &calls, func(*credo.Context) string { return lang })
		app.GET("/", func(ctx *credo.Context) error {
			return ctx.Response().Text(http.StatusOK, ctx.Locale())
		})
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		if w.Body.String() != "en" {
			t.Fatalf("detect %q: locale = %q, want default en", lang, w.Body.String())
		}
	}
}

func TestI18n_Lazy_DetectorPanicRecovered(t *testing.T) {
	logger, buf := newTestLogger(t)
	var calls atomic.Int32
	app := newLazyI18nApp(t, &calls, func(*credo.Context) string { panic("detector boom") }, credo.WithLogger(logger))
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, ctx.Locale())
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	var body credo.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Error rendering translated with the cached default instead of
	// detecting again.
	if body.Error.Message != "Internal server error" {
		t.Fatalf("message = %q, want the default-language translation", body.Error.Message)
	}
	if calls.Load() != 1 {
		t.Fatalf("detector calls = %d, want 1 (never invoked again for the request)", calls.Load())
	}
	if !strings.Contains(buf.String(), "detector boom") {
		t.Fatalf("expected the panic record, got: %s", buf.String())
	}
}

func TestI18n_Lazy_DetectorPanicPropagatesWithoutRecover(t *testing.T) {
	var calls atomic.Int32
	app := newLazyI18nApp(t, &calls, func(*credo.Context) string { panic("detector boom") }, credo.WithoutRecover())
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, ctx.Locale())
	})
	expectPanicContaining(t, "detector boom", func() {
		app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	})
	// Cleanup ran: a fresh request detects again from a clean state.
	func() {
		defer func() { _ = recover() }()
		app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	if calls.Load() != 2 {
		t.Fatalf("detector calls = %d, want 2", calls.Load())
	}
}

func TestI18n_Lazy_ReentryPanics(t *testing.T) {
	var calls atomic.Int32
	app := newLazyI18nApp(t, &calls, func(ctx *credo.Context) string { return ctx.Locale() })
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, ctx.Locale())
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusInternalServerError || calls.Load() != 1 {
		t.Fatalf("status = %d, calls = %d; want 500 and 1", w.Code, calls.Load())
	}
}

func TestI18n_InactiveBundleNeverDetects(t *testing.T) {
	var calls atomic.Int32
	app := mustNew(t)
	if err := app.UseI18n(credo.I18nConfig{
		Dir: filepath_nonexistent(),
		Detect: func(*credo.Context) string {
			calls.Add(1)
			return "tr"
		},
	}); err == nil {
		t.Fatal("explicit missing directory must fail")
	}
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, ctx.Locale()+"|"+ctx.T("required"))
	})
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Body.String() != "|required" || calls.Load() != 0 {
		t.Fatalf("body = %q, calls = %d; want empty locale, key fallback, no detection", w.Body.String(), calls.Load())
	}
}

func filepath_nonexistent() string { return "nonexistent_locales_for_lazy_test/" }

func TestUseI18n_TwicePanics(t *testing.T) {
	t.Run("active", func(t *testing.T) {
		app := mustNew(t)
		if err := app.UseI18n(credo.I18nConfig{DirFS: i18nTestFS()}); err != nil {
			t.Fatal(err)
		}
		expectPanicContaining(t, "App.UseI18n called twice", func() {
			_ = app.UseI18n(credo.I18nConfig{DirFS: i18nTestFS()})
		})
	})
	t.Run("inactive consumes the slot", func(t *testing.T) {
		app := mustNew(t)
		if err := app.UseI18n(); err != nil {
			t.Fatal(err)
		}
		expectPanicContaining(t, "App.UseI18n called twice", func() {
			_ = app.UseI18n(credo.I18nConfig{DirFS: i18nTestFS()})
		})
	})
	t.Run("error leaves the slot free", func(t *testing.T) {
		app := mustNew(t)
		if err := app.UseI18n(credo.I18nConfig{Dir: filepath_nonexistent()}); err == nil {
			t.Fatal("expected error")
		}
		if err := app.UseI18n(credo.I18nConfig{DirFS: i18nTestFS()}); err != nil {
			t.Fatalf("second UseI18n after an error: %v", err)
		}
	})
}
