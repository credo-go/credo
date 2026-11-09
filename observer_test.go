package credo_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

// exchangeSnapshot copies what an observer keeps from an Exchange, which is
// valid only during Observe.
type exchangeSnapshot struct {
	Method, Path, OriginalPath, Route, RouteName string
	RemoteAddr, UserAgent, RequestID, ErrorCode  string
	Status                                       int
	Bytes                                        int64
	Duration                                     time.Duration
	PanicRecovered                               bool
}

func snapshotOf(ex *credo.Exchange) exchangeSnapshot {
	return exchangeSnapshot{
		Method: ex.Method(), Path: ex.Path(), OriginalPath: ex.OriginalPath(),
		Route: ex.Route(), RouteName: ex.RouteName(),
		RemoteAddr: ex.RemoteAddr(), UserAgent: ex.UserAgent(), RequestID: ex.RequestID(),
		ErrorCode: ex.ErrorCode(), Status: ex.Status(), Bytes: ex.Bytes(), Duration: ex.Duration(),
		PanicRecovered: ex.PanicRecovered(),
	}
}

// observerFunc adapts a function to credo.Observer for the tests.
type observerFunc func(ex *credo.Exchange)

func (f observerFunc) Observe(ex *credo.Exchange) { f(ex) }

// recordObserver keeps a snapshot of every exchange it observes.
func recordObserver(app *credo.App) *[]exchangeSnapshot {
	var seen []exchangeSnapshot
	app.AddObserver(observerFunc(func(ex *credo.Exchange) { seen = append(seen, snapshotOf(ex)) }))
	return &seen
}

func observeOne(app *credo.App, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	return w
}

func TestObserver_SeesTheFinalResponse(t *testing.T) {
	t.Run("error rendering", func(t *testing.T) {
		app := mustNew(t)
		seen := recordObserver(app)
		app.GET("/x", func(*credo.Context) error {
			return credo.NewHTTPError(http.StatusConflict, "order_locked")
		}).Name("orders.lock")
		w := observeOne(app, httptest.NewRequest(http.MethodGet, "/x", nil))
		got := (*seen)[0]
		if got.Status != http.StatusConflict || got.Bytes != int64(w.Body.Len()) || got.ErrorCode != "order_locked" {
			t.Fatalf("exchange = %+v, want 409 order_locked with %d bytes", got, w.Body.Len())
		}
		if got.Route != "/x" || got.RouteName != "orders.lock" || got.Method != http.MethodGet || got.PanicRecovered {
			t.Fatalf("exchange = %+v", got)
		}
	})
	t.Run("recovery", func(t *testing.T) {
		logger, _ := newTestLogger(t)
		app := mustNew(t, credo.WithLogger(logger))
		seen := recordObserver(app)
		app.GET("/x", func(*credo.Context) error { panic("boom") })
		observeOne(app, httptest.NewRequest(http.MethodGet, "/x", nil))
		if got := (*seen)[0]; got.Status != 500 || !got.PanicRecovered || got.ErrorCode != "internal_server_error" {
			t.Fatalf("exchange = %+v, want a recovered 500 internal_server_error", got)
		}
	})
	t.Run("compression", func(t *testing.T) {
		app := mustNew(t)
		app.UseCompress()
		seen := recordObserver(app)
		body := strings.Repeat("compressible ", 200)
		app.GET("/x", func(c *credo.Context) error { return c.Response().Text(http.StatusOK, body) })
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.Header.Set("Accept-Encoding", "gzip")
		w := observeOne(app, r)
		if w.Header().Get("Content-Encoding") != "gzip" {
			t.Fatalf("response not compressed: %v", w.Header())
		}
		if got := (*seen)[0]; got.Bytes != int64(w.Body.Len()) || got.Bytes >= int64(len(body)) {
			t.Fatalf("exchange bytes = %d, want the %d compressed bytes", got.Bytes, w.Body.Len())
		}
	})
}

func TestObserver_ReadsTheRequestsValues(t *testing.T) {
	logger, _ := newTestLogger(t)
	app := mustNew(t, credo.WithLogger(logger))
	var (
		user               string
		inner              string
		sawInner, sawOther bool
		other              string
		kept               context.Context
	)
	app.AddObserver(observerFunc(func(ex *credo.Exchange) {
		if kept != nil {
			return // the second request only reuses the Context
		}
		if u, ok := ex.GetUser[*headUser](); ok {
			user = u.name
		}
		inner, sawInner = ex.Get(keyInner)
		other, sawOther = ex.Get(keyBefore)
		kept = ex.Context()
	}))
	app.GET("/x", func(c *credo.Context) error {
		c.Set(keyInner, "set-in-scope")
		return c.Response().NoContent(http.StatusNoContent)
	}).Middleware(middleware.Timeout(middleware.TimeoutConfig{Timeout: time.Minute}), setHeadUser)

	observeOne(app, httptest.NewRequest(http.MethodGet, "/x", nil))
	if user != "alice" {
		t.Fatalf("GetUser = %q, want the user set inside the scope", user)
	}
	if !sawInner || inner != "set-in-scope" {
		t.Fatalf("Get(keyInner) = %q, %v; want the value set inside the scope", inner, sawInner)
	}
	if sawOther || other != "" {
		t.Fatalf("Get of a key no layer set = %q, %v; want the zero value and false", other, sawOther)
	}
	// The context outlives Observe, and the request after it does not change it.
	observeOne(app, httptest.NewRequest(http.MethodGet, "/missing", nil))
	if v, _ := kept.Value(keyInner).(string); v != "set-in-scope" {
		t.Fatalf("kept context value = %q, want set-in-scope", v)
	}
}

func TestObserver_ErrorCode(t *testing.T) {
	type handlerCase struct {
		name    string
		opts    []credo.Option
		setup   func(*credo.App)
		handler credo.Handler
		code    string
		escapes bool
	}
	cases := []handlerCase{
		{name: "success", handler: okHandler, code: ""},
		{
			name:    "classified",
			handler: func(*credo.Context) error { return credo.NewHTTPError(http.StatusTeapot, "tea_only") },
			code:    "tea_only",
		},
		{
			name:    "recovered panic",
			handler: func(*credo.Context) error { panic("boom") },
			code:    "internal_server_error",
		},
		{
			name: "renderer panic falls back",
			setup: func(app *credo.App) {
				app.UseErrorRenderer(func(*credo.Context, *credo.ErrorInfo) any { panic("renderer boom") })
			},
			handler: func(*credo.Context) error { return credo.NewHTTPError(http.StatusTeapot, "tea_only") },
			code:    "internal_server_error",
		},
		{
			name: "invalid renderer status falls back",
			setup: func(app *credo.App) {
				app.UseErrorRenderer(func(_ *credo.Context, info *credo.ErrorInfo) any {
					info.Status = 99
					return nil
				})
			},
			handler: func(*credo.Context) error { return credo.NewHTTPError(http.StatusTeapot, "tea_only") },
			code:    "internal_server_error",
		},
		{
			name: "renderer that wrote its own response keeps the classified code",
			setup: func(app *credo.App) {
				app.UseErrorRenderer(func(c *credo.Context, _ *credo.ErrorInfo) any {
					_ = c.Response().Text(http.StatusTeapot, "custom")
					return nil
				})
			},
			handler: func(*credo.Context) error { return credo.NewHTTPError(http.StatusTeapot, "tea_only") },
			code:    "tea_only",
		},
		{
			name: "error after commit",
			handler: func(c *credo.Context) error {
				_ = c.Response().Text(http.StatusOK, "partial")
				return errors.New("late")
			},
			code: "",
		},
		{
			name: "handler wrote its own error response",
			handler: func(c *credo.Context) error {
				return c.Response().JSON(http.StatusConflict, map[string]string{"code": "own"})
			},
			code: "",
		},
		{
			name:    "escaped panic without recovery",
			opts:    []credo.Option{credo.WithoutRecover()},
			handler: func(*credo.Context) error { panic("boom") },
			code:    "",
			escapes: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logger, _ := newTestLogger(t)
			app := mustNew(t, append([]credo.Option{credo.WithLogger(logger)}, tc.opts...)...)
			if tc.setup != nil {
				tc.setup(app)
			}
			seen := recordObserver(app)
			app.GET("/x", tc.handler)
			func() {
				defer func() {
					if r := recover(); (r != nil) != tc.escapes {
						t.Fatalf("recover() = %v, escapes = %v", r, tc.escapes)
					}
				}()
				observeOne(app, httptest.NewRequest(http.MethodGet, "/x", nil))
			}()
			if len(*seen) != 1 || (*seen)[0].ErrorCode != tc.code {
				t.Fatalf("exchanges = %+v, want one with error code %q", *seen, tc.code)
			}
		})
	}
}

func TestObserver_PanicIsContained(t *testing.T) {
	t.Run("recovery on: logged, the next observer runs", func(t *testing.T) {
		logger, buf := newTestLogger(t)
		app := mustNew(t, credo.WithLogger(logger))
		app.AddObserver(observerFunc(func(*credo.Exchange) { panic("observer boom") }))
		seen := recordObserver(app)
		app.GET("/x", okHandler)
		w := observeOne(app, httptest.NewRequest(http.MethodGet, "/x", nil))
		if w.Code != http.StatusOK || len(*seen) != 1 {
			t.Fatalf("status = %d, later observer calls = %d; want 200 and 1", w.Code, len(*seen))
		}
		lines := parseJSONLines(t, buf.Bytes())
		var found bool
		for _, l := range lines {
			if l["msg"] == "credo: observer panicked" {
				found = l["level"] == "ERROR" && l["panic"] == "observer boom" &&
					l["observer"] == "credo_test.observerFunc" && l["path"] == "/x" && l["status"] == float64(200)
			}
		}
		if !found {
			t.Fatalf("missing the observer panic diagnostic: %s", buf.String())
		}
	})
	t.Run("recovery off: propagates", func(t *testing.T) {
		app := mustNew(t, credo.WithoutRecover())
		app.AddObserver(observerFunc(func(*credo.Exchange) { panic("observer boom") }))
		app.GET("/x", okHandler)
		expectPanicContaining(t, "observer boom", func() {
			observeOne(app, httptest.NewRequest(http.MethodGet, "/x", nil))
		})
	})
	t.Run("abort propagates", func(t *testing.T) {
		app := mustNew(t)
		app.AddObserver(observerFunc(func(*credo.Exchange) { panic(http.ErrAbortHandler) }))
		app.GET("/x", okHandler)
		defer func() {
			if err, _ := recover().(error); !errors.Is(err, http.ErrAbortHandler) {
				t.Fatalf("recover() = %v, want http.ErrAbortHandler", err)
			}
		}()
		observeOne(app, httptest.NewRequest(http.MethodGet, "/x", nil))
	})
}

func TestObserver_RunInRegistrationOrder(t *testing.T) {
	var buf bytes.Buffer
	app := mustNew(t)
	var before, after int
	app.AddObserver(observerFunc(func(*credo.Exchange) { before = buf.Len() }))
	app.UseAccessLog(credo.AccessLogConfig{Logger: slog.New(slog.NewJSONHandler(&buf, nil))})
	app.AddObserver(observerFunc(func(*credo.Exchange) { after = buf.Len() }))
	app.GET("/x", okHandler)
	observeOne(app, httptest.NewRequest(http.MethodGet, "/x", nil))
	if before != 0 || after == 0 {
		t.Fatalf("record bytes seen before/after = %d/%d; want the first observer before the access log and the last after it", before, after)
	}
}

func TestAddObserver_Panics(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		app := mustNew(t)
		defer func() {
			if got, _ := recover().(string); got != "credo: App.AddObserver: nil observer" {
				t.Fatalf("panic = %q", got)
			}
		}()
		app.AddObserver(nil)
	})
	t.Run("after preparation", func(t *testing.T) {
		app := mustNew(t)
		app.GET("/", okHandler)
		observeOne(app, httptest.NewRequest(http.MethodGet, "/", nil))
		defer func() {
			if got, _ := recover().(string); got != "credo: App.AddObserver called after app was compiled or shut down" {
				t.Fatalf("panic = %q", got)
			}
		}()
		app.AddObserver(observerFunc(func(*credo.Exchange) {}))
	})
}

// accessLogJSON is a JSON logger without the time and the duration, so a
// record compares byte for byte.
func accessLogJSON(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && (a.Key == slog.TimeKey || a.Key == "duration") {
				return slog.Attr{}
			}
			return a
		},
	}))
}

func TestAccessLog_RecordIsUnchanged(t *testing.T) {
	var buf bytes.Buffer
	app := mustNew(t)
	app.UseAccessLog(credo.AccessLogConfig{Logger: accessLogJSON(&buf)})
	app.GET("/items/{id}", func(c *credo.Context) error { return c.Response().Text(http.StatusOK, "ok") })
	r := httptest.NewRequest(http.MethodGet, "/items/7", nil)
	r.Header.Set("User-Agent", "probe/1")
	observeOne(app, r)
	want := `{"level":"INFO","msg":"request completed","method":"GET","path":"/items/7","status":200,"bytes":2,"remote_addr":"192.0.2.1","user_agent":"probe/1","route":"/items/{id}"}` + "\n"
	if buf.String() != want {
		t.Fatalf("record =\n%s\nwant\n%s", buf.String(), want)
	}
}

func TestAccessLog_Attrs(t *testing.T) {
	attrs := func(calls *int) func(*credo.Exchange) []slog.Attr {
		return func(ex *credo.Exchange) []slog.Attr {
			*calls++
			tenant, _ := ex.Get(keyTenant)
			return []slog.Attr{slog.String("tenant", tenant), slog.String("status", "shadow")}
		}
	}
	setTenant := func(next credo.Handler) credo.Handler {
		return func(c *credo.Context) error {
			c.Set(keyTenant, "acme")
			return next(c)
		}
	}
	const group = `"attrs":{"tenant":"acme","status":"shadow"}}`

	t.Run("dedicated logger", func(t *testing.T) {
		var buf bytes.Buffer
		var calls int
		app := mustNew(t)
		app.UseAccessLog(credo.AccessLogConfig{Logger: accessLogJSON(&buf), Attrs: attrs(&calls)})
		app.PreRouting(setTenant)
		app.GET("/x", okHandler)
		observeOne(app, httptest.NewRequest(http.MethodGet, "/x", nil))
		if calls != 1 || !strings.Contains(buf.String(), `"status":200,`) || !strings.HasSuffix(strings.TrimSpace(buf.String()), group) {
			t.Fatalf("calls = %d, record = %s; want the attributes in the attrs group", calls, buf.String())
		}
	})
	t.Run("request logger", func(t *testing.T) {
		var buf bytes.Buffer
		var calls int
		app := mustNew(t, credo.WithLogger(accessLogJSON(&buf)))
		app.UseRequestID()
		app.UseAccessLog(credo.AccessLogConfig{Attrs: attrs(&calls)})
		app.PreRouting(setTenant)
		app.GET("/x", func(c *credo.Context) error {
			c.Logger().Info("handler")
			return c.Response().NoContent(http.StatusOK)
		})
		observeOne(app, httptest.NewRequest(http.MethodGet, "/x", nil))
		var record string
		for line := range strings.Lines(buf.String()) {
			if strings.Contains(line, "request completed") {
				record = strings.TrimSpace(line)
			}
		}
		if calls != 1 || !strings.Contains(record, `"request_id":`) || !strings.HasSuffix(record, group) {
			t.Fatalf("calls = %d, record = %s; want the attributes in the attrs group", calls, record)
		}
	})

	notCalled := []struct {
		name  string
		cfg   func(*credo.AccessLogConfig)
		route func(*credo.Route)
		level slog.Level
	}{
		{name: "skipped", cfg: func(c *credo.AccessLogConfig) { c.Skipper = func(*credo.Context) bool { return true } }},
		{name: "silenced", route: func(r *credo.Route) { r.SetMeta(credo.MetaAccessLog, false) }},
		{name: "below MinLevel", cfg: func(c *credo.AccessLogConfig) { c.MinLevel = slog.LevelWarn }},
		{name: "filtered", cfg: func(c *credo.AccessLogConfig) { c.ResultFilter = func(*credo.Exchange) bool { return false } }},
		{name: "logger disabled", level: slog.LevelError},
	}
	for _, tc := range notCalled {
		t.Run("not called when "+tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			var calls int
			cfg := credo.AccessLogConfig{
				Logger: slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: tc.level})),
				Attrs:  attrs(&calls),
			}
			if tc.cfg != nil {
				tc.cfg(&cfg)
			}
			app := mustNew(t)
			app.UseAccessLog(cfg)
			route := app.GET("/x", okHandler)
			if tc.route != nil {
				tc.route(route)
			}
			observeOne(app, httptest.NewRequest(http.MethodGet, "/x", nil))
			if calls != 0 || buf.Len() != 0 {
				t.Fatalf("Attrs calls = %d, record = %s; want neither", calls, buf.String())
			}
		})
	}

	t.Run("a panic keeps the base record", func(t *testing.T) {
		var buf bytes.Buffer
		logger, diag := newTestLogger(t)
		app := mustNew(t, credo.WithLogger(logger))
		app.UseAccessLog(credo.AccessLogConfig{
			Logger: accessLogJSON(&buf),
			Attrs:  func(*credo.Exchange) []slog.Attr { panic("attrs boom") },
		})
		app.GET("/x", okHandler)
		observeOne(app, httptest.NewRequest(http.MethodGet, "/x", nil))
		want := `{"level":"INFO","msg":"request completed","method":"GET","path":"/x","status":200,"bytes":2,"remote_addr":"192.0.2.1","user_agent":"","route":"/x"}` + "\n"
		if buf.String() != want {
			t.Fatalf("record = %s, want the base record without the group", buf.String())
		}
		if !strings.Contains(diag.String(), `"msg":"credo: access-log attrs panicked"`) || !strings.Contains(diag.String(), `"panic":"attrs boom"`) {
			t.Fatalf("missing the attrs panic diagnostic: %s", diag.String())
		}
	})
	t.Run("a panic propagates without recovery", func(t *testing.T) {
		app := mustNew(t, credo.WithoutRecover())
		app.UseAccessLog(credo.AccessLogConfig{
			Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
			Attrs:  func(*credo.Exchange) []slog.Attr { panic("attrs boom") },
		})
		app.GET("/x", okHandler)
		expectPanicContaining(t, "attrs boom", func() {
			observeOne(app, httptest.NewRequest(http.MethodGet, "/x", nil))
		})
	})
}

func TestObserver_SeesARequestTheAccessLogSkips(t *testing.T) {
	var buf bytes.Buffer
	app := mustNew(t)
	app.UseAccessLog(credo.AccessLogConfig{
		Logger:  slog.New(slog.NewJSONHandler(&buf, nil)),
		Skipper: func(*credo.Context) bool { return true },
	})
	seen := recordObserver(app)
	app.GET("/x", okHandler)
	observeOne(app, httptest.NewRequest(http.MethodGet, "/x", nil))
	if buf.Len() != 0 || len(*seen) != 1 || (*seen)[0].Status != http.StatusOK {
		t.Fatalf("record = %q, exchanges = %+v; want no access record and one observed 200", buf.String(), *seen)
	}
}
