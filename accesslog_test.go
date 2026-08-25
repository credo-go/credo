package credo_test

import (
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

type countingAccessLogLeveler struct {
	level slog.Level
	calls atomic.Int32
}

func (l *countingAccessLogLeveler) Level() slog.Level {
	l.calls.Add(1)
	return l.level
}

func TestBuiltinAccessLog_Logs200(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
	app.GET("/test", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "hello")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log: %v\nraw: %s", err, buf.String())
	}

	if entry["method"] != "GET" {
		t.Errorf("method = %v, want GET", entry["method"])
	}
	if entry["path"] != "/test" {
		t.Errorf("path = %v, want /test", entry["path"])
	}
	if s, ok := entry["status"].(float64); !ok || int(s) != 200 {
		t.Errorf("status = %v, want 200", entry["status"])
	}
	if b, ok := entry["bytes"].(float64); !ok || int(b) != 5 {
		t.Errorf("bytes = %v, want 5", entry["bytes"])
	}
	if entry["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", entry["level"])
	}
	if entry["msg"] != "request completed" {
		t.Errorf("msg = %v, want 'request completed'", entry["msg"])
	}
}

func TestBuiltinAccessLog_UsesRealIP(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID(), credo.WithTrustedProxies("10.0.0.0/8"))
	app.GET("/test", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "hello")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.10")
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log: %v\nraw: %s", err, buf.String())
	}
	if entry["remote_addr"] != "203.0.113.10" {
		t.Fatalf("remote_addr = %v, want 203.0.113.10", entry["remote_addr"])
	}
}

func TestBuiltinAccessLog_LogLevel(t *testing.T) {
	tests := []struct {
		name   string
		status int
		level  string
	}{
		{"2xx → INFO", 200, "INFO"},
		{"1xx → INFO", 101, "INFO"},
		{"3xx → INFO", 301, "INFO"},
		{"4xx → WARN", 404, "WARN"},
		{"5xx → ERROR", 500, "ERROR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, buf := newTestLogger(t)

			app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
			status := tt.status
			app.GET("/", func(ctx *credo.Context) error {
				return ctx.Response().NoContent(status)
			})

			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/", nil)
			app.ServeHTTP(w, r)

			var entry map[string]any
			if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
				t.Fatalf("failed to parse log: %v\nraw: %s", err, buf.String())
			}

			if entry["level"] != tt.level {
				t.Errorf("level = %v, want %s for status %d", entry["level"], tt.level, tt.status)
			}
			if s, ok := entry["status"].(float64); !ok || int(s) != tt.status {
				t.Errorf("status = %v, want %d", entry["status"], tt.status)
			}
		})
	}
}

func TestBuiltinAccessLog_404(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
	app.GET("/exists", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/not-found", nil)
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log: %v\nraw: %s", err, buf.String())
	}

	if s, ok := entry["status"].(float64); !ok || int(s) != 404 {
		t.Errorf("status = %v, want 404", entry["status"])
	}
	if entry["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", entry["level"])
	}
}

func TestBuiltinAccessLog_405(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
	app.GET("/test", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", nil)
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log: %v\nraw: %s", err, buf.String())
	}

	if s, ok := entry["status"].(float64); !ok || int(s) != 405 {
		t.Errorf("status = %v, want 405", entry["status"])
	}
	if entry["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", entry["level"])
	}
}

func TestBuiltinAccessLog_PanicLogged(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
	app.GET("/", func(ctx *credo.Context) error {
		panic("boom")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	if w.Code != 500 {
		t.Errorf("status = %d, want 500", w.Code)
	}

	entries := parseJSONLines(t, buf.Bytes())
	if len(entries) < 2 {
		t.Fatalf("expected at least 2 log entries (access log + panic), got %d:\n%s",
			len(entries), buf.String())
	}

	// Find the access log entry.
	var accessEntry map[string]any
	for _, e := range entries {
		if e["msg"] == "request completed" {
			accessEntry = e
			break
		}
	}
	if accessEntry == nil {
		t.Fatal("expected 'request completed' log entry")
	}
	if s, ok := accessEntry["status"].(float64); !ok || int(s) != 500 {
		t.Errorf("access log status = %v, want 500", accessEntry["status"])
	}
	if accessEntry["level"] != "ERROR" {
		t.Errorf("access log level = %v, want ERROR", accessEntry["level"])
	}
	// bytes must be > 0: builtinRecover writes the 500 response body
	// before builtinAccessLog's defer fires (recover is an inner frame).
	if b, ok := accessEntry["bytes"].(float64); !ok || int(b) == 0 {
		t.Errorf("access log bytes = %v, want > 0 (panic response body)", accessEntry["bytes"])
	}

	// Verify panic log entry also exists.
	var panicEntry map[string]any
	for _, e := range entries {
		if e["msg"] == "panic recovered" {
			panicEntry = e
			break
		}
	}
	if panicEntry == nil {
		t.Fatal("expected 'panic recovered' log entry")
	}
}

func TestBuiltinAccessLog_ErrorResponse_BytesAndStatus(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
	app.GET("/fail", func(ctx *credo.Context) error {
		return credo.NewHTTPError(404)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/fail", nil)
	app.ServeHTTP(w, r)

	if w.Code != 404 {
		t.Fatalf("status = %d, want 404", w.Code)
	}

	entries := parseJSONLines(t, buf.Bytes())
	var accessEntry map[string]any
	for _, e := range entries {
		if e["msg"] == "request completed" {
			accessEntry = e
			break
		}
	}
	if accessEntry == nil {
		t.Fatal("expected 'request completed' log entry")
	}

	if s, ok := accessEntry["status"].(float64); !ok || int(s) != 404 {
		t.Errorf("access log status = %v, want 404", accessEntry["status"])
	}
	if accessEntry["level"] != "WARN" {
		t.Errorf("access log level = %v, want WARN", accessEntry["level"])
	}
	// bytes must be > 0 because the default JSON error response body
	// is now written before the access log fires.
	if b, ok := accessEntry["bytes"].(float64); !ok || int(b) == 0 {
		t.Errorf("access log bytes = %v, want > 0 (error response body)", accessEntry["bytes"])
	}
}

func TestBuiltinAccessLog_ErrorResponse_DurationIncludesRenderer(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
	app.GET("/fail", func(ctx *credo.Context) error {
		return credo.NewHTTPError(500).WithMessageKey("test.error")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/fail", nil)
	app.ServeHTTP(w, r)

	entries := parseJSONLines(t, buf.Bytes())
	var accessEntry map[string]any
	for _, e := range entries {
		if e["msg"] == "request completed" {
			accessEntry = e
			break
		}
	}
	if accessEntry == nil {
		t.Fatal("expected 'request completed' log entry")
	}

	// Verify duration is present and non-negative.
	if d, ok := accessEntry["duration"].(float64); !ok || d < 0 {
		t.Errorf("access log duration = %v, want >= 0", accessEntry["duration"])
	}
}

func TestBuiltinAccessLog_IncludesRequestID(t *testing.T) {
	logger, buf := newTestLogger(t)

	// Both built-in RequestID and AccessLog active.
	app := mustNew(t, credo.WithLogger(logger))
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log: %v\nraw: %s", err, buf.String())
	}

	reqID, ok := entry["request_id"].(string)
	if !ok || reqID == "" {
		t.Error("expected request_id in access log entry")
	}
	if got := w.Header().Get("X-Request-Id"); got != reqID {
		t.Errorf("header X-Request-Id = %q, log request_id = %q", got, reqID)
	}
}

func TestWithoutAccessLog_Disables(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutAccessLog())
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	if buf.Len() != 0 {
		t.Errorf("expected no log with WithoutAccessLog, got: %s", buf.String())
	}
}

func TestBuiltinAccessLog_BothDisabled(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID(), credo.WithoutAccessLog())
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no log with both disabled, got: %s", buf.String())
	}
	if got := w.Header().Get("X-Request-Id"); got != "" {
		t.Errorf("X-Request-Id = %q, want empty", got)
	}
}

func TestBuiltinAccessLog_FallbackRequestID(t *testing.T) {
	// When built-in RequestID is disabled but middleware.RequestID() is used,
	// the access log should still include request_id via the context store fallback.
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
	app.GlobalMiddleware(middleware.RequestID())
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log: %v\nraw: %s", err, buf.String())
	}

	reqID, ok := entry["request_id"].(string)
	if !ok || reqID == "" {
		t.Error("expected request_id in access log via context store fallback")
	}
	if got := w.Header().Get("X-Request-Id"); got != reqID {
		t.Errorf("header = %q, log request_id = %q", got, reqID)
	}
}

// --- MetaAccessLog (per-route/group silencing) ---

func TestMetaAccessLog_ConstantValue(t *testing.T) {
	if credo.MetaAccessLog != "credo.accesslog" {
		t.Errorf("MetaAccessLog = %q, want %q", credo.MetaAccessLog, "credo.accesslog")
	}
}

func TestBuiltinAccessLog_MetaSilencesRoute(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
	app.GET("/silent", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	}).SetMeta(credo.MetaAccessLog, false)
	app.GET("/loud", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	})

	// Silenced route → no access log line.
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/silent", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no log for silenced route, got: %s", buf.String())
	}

	// Sibling route without the meta → logged.
	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/loud", nil))
	if buf.Len() == 0 {
		t.Error("expected log for non-silenced sibling route")
	}
}

func TestBuiltinAccessLog_MetaSilencesGroup(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
	g := app.Group("/internal")
	g.SetMeta(credo.MetaAccessLog, false)
	g.GET("/metrics", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/internal/metrics", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no log for route under silenced group (meta inheritance), got: %s", buf.String())
	}
}

func TestBuiltinAccessLog_RouteMetaOverridesGroup(t *testing.T) {
	logger, buf := newTestLogger(t)

	// A silenced group with one route that re-enables logging at the route
	// level. LookupMeta reads the route before its parents, so the route's
	// true overrides the group's false.
	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
	g := app.Group("/internal")
	g.SetMeta(credo.MetaAccessLog, false)
	g.GET("/audit", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	}).SetMeta(credo.MetaAccessLog, true)

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/internal/audit", nil))
	if buf.Len() == 0 {
		t.Error("expected log: route-level true must override group-level false")
	}
}

func TestBuiltinAccessLog_NonBoolMetaFailsOpen(t *testing.T) {
	logger, buf := newTestLogger(t)

	// A non-bool meta value is ignored (fail-open): the request is logged.
	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
	app.GET("/x", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	}).SetMeta(credo.MetaAccessLog, "false") // string, not bool

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	if buf.Len() == 0 {
		t.Error("expected log: a non-bool MetaAccessLog value must fail open")
	}
}

func TestBuiltinAccessLog_StaticRouteMetaSilences(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
	fsys := fstest.MapFS{
		"app.js": {Data: []byte("console.log('x')")},
	}
	app.Static("/assets", fsys).SetMeta(credo.MetaAccessLog, false)

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/assets/app.js", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no access log for silenced static route, got: %s", buf.String())
	}
}

func TestBuiltinAccessLog_SkipperOption(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t,
		credo.WithLogger(logger),
		credo.WithoutRequestID(),
		credo.WithAccessLogSkipper(func(ctx *credo.Context) bool {
			return ctx.Request().URL.Path == "/skip"
		}),
	)
	app.GET("/skip", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	})
	app.GET("/keep", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	})

	// Skipped path → no log.
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/skip", nil))
	if buf.Len() != 0 {
		t.Errorf("expected no log for skipped path, got: %s", buf.String())
	}

	// Other path → logged.
	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/keep", nil))
	if buf.Len() == 0 {
		t.Error("expected log for non-skipped path")
	}
}

func TestBuiltinAccessLog_SilencedRoutePanicStillRecovers(t *testing.T) {
	logger, buf := newTestLogger(t)

	// builtinRecover is on by default. A silenced route that panics must not
	// emit an access-log line, but the recover layer still logs the panic.
	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
	app.GET("/boom", func(ctx *credo.Context) error {
		panic("boom")
	}).SetMeta(credo.MetaAccessLog, false)

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/boom", nil))
	if w.Code != 500 {
		t.Errorf("status = %d, want 500", w.Code)
	}

	entries := parseJSONLines(t, buf.Bytes())
	for _, e := range entries {
		if e["msg"] == "request completed" {
			t.Errorf("silenced route must not emit an access-log line on panic; got: %s", buf.String())
		}
	}
	var sawPanic bool
	for _, e := range entries {
		if e["msg"] == "panic recovered" {
			sawPanic = true
		}
	}
	if !sawPanic {
		t.Errorf("expected 'panic recovered' log even for a silenced route; got: %s", buf.String())
	}
}

func TestBuiltinAccessLog_MinLevel(t *testing.T) {
	tests := []struct {
		name    string
		level   slog.Level
		status  int
		wantLog bool
	}{
		{"Info keeps success", slog.LevelInfo, 200, true},
		{"Warn drops success", slog.LevelWarn, 200, false},
		{"Warn keeps client error", slog.LevelWarn, 404, true},
		{"Warn keeps server error", slog.LevelWarn, 500, true},
		{"Error drops client error", slog.LevelError, 404, false},
		{"Error keeps server error", slog.LevelError, 500, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, buf := newTestLogger(t)
			app := mustNew(t,
				credo.WithLogger(logger),
				credo.WithoutRequestID(),
				credo.WithAccessLogMinLevel(tt.level),
			)
			app.GET("/", func(ctx *credo.Context) error {
				return ctx.Response().NoContent(tt.status)
			})

			app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
			if got := buf.Len() > 0; got != tt.wantLog {
				t.Fatalf("logged = %v, want %v; raw: %s", got, tt.wantLog, buf.String())
			}
		})
	}
}

func TestBuiltinAccessLog_LevelVarRuntimeUpdate(t *testing.T) {
	logger, buf := newTestLogger(t)
	var level slog.LevelVar
	level.Set(slog.LevelWarn)

	app := mustNew(t,
		credo.WithLogger(logger),
		credo.WithoutRequestID(),
		credo.WithAccessLogMinLevel(&level),
	)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	})

	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if buf.Len() != 0 {
		t.Fatalf("Warn threshold logged 200: %s", buf.String())
	}

	level.Set(slog.LevelInfo)
	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if buf.Len() == 0 {
		t.Fatal("Info threshold did not log 200 after LevelVar update")
	}
}

func TestBuiltinAccessLog_LevelerReadOncePerRequest(t *testing.T) {
	logger, _ := newTestLogger(t)
	level := &countingAccessLogLeveler{level: slog.LevelInfo}
	app := mustNew(t,
		credo.WithLogger(logger),
		credo.WithoutRequestID(),
		credo.WithAccessLogMinLevel(level),
	)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	})

	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if got := level.calls.Load(); got != 1 {
		t.Fatalf("Level calls = %d, want 1", got)
	}
}

func TestWithAccessLogMinLevel_TypedNilRejected(t *testing.T) {
	var level *slog.LevelVar
	_, err := credo.New(credo.WithAccessLogMinLevel(level))
	if err == nil || !strings.Contains(err.Error(), "typed-nil slog.Leveler") {
		t.Fatalf("New error = %v, want typed-nil Leveler error", err)
	}
}

func TestBuiltinAccessLog_ResultFilterSnapshot(t *testing.T) {
	logger, buf := newTestLogger(t)
	var captured credo.AccessLogEntry
	var calls atomic.Int32

	app := mustNew(t,
		credo.WithLogger(logger),
		credo.WithAccessLogResultFilter(func(ctx *credo.Context, entry credo.AccessLogEntry) bool {
			calls.Add(1)
			captured = entry
			return true
		}),
	)
	app.POST("/users", func(ctx *credo.Context) error {
		return ctx.Response().Text(201, "created")
	}).Name("users.create")

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("POST", "/users", nil))
	if calls.Load() != 1 {
		t.Fatalf("filter calls = %d, want 1", calls.Load())
	}
	if captured.Method != "POST" || captured.Path != "/users" || captured.Status != 201 {
		t.Errorf("snapshot method/path/status = %s %s %d", captured.Method, captured.Path, captured.Status)
	}
	if captured.Bytes != int64(len("created")) || captured.Duration < 0 {
		t.Errorf("snapshot bytes/duration = %d/%v", captured.Bytes, captured.Duration)
	}
	if captured.RouteName != "users.create" {
		t.Errorf("RouteName = %q, want users.create", captured.RouteName)
	}
	if captured.RequestID == "" || captured.RequestID != w.Header().Get("X-Request-Id") {
		t.Errorf("RequestID = %q, header = %q", captured.RequestID, w.Header().Get("X-Request-Id"))
	}
	if strings.Count(buf.String(), `"request_id"`) != 1 {
		t.Errorf("request_id must be emitted exactly once: %s", buf.String())
	}
	if strings.Contains(buf.String(), `"route_name"`) {
		t.Errorf("RouteName is filter metadata and must not be emitted: %s", buf.String())
	}
}

func TestBuiltinAccessLog_ResultFilterOrdering(t *testing.T) {
	t.Run("false skips log but not handler", func(t *testing.T) {
		logger, buf := newTestLogger(t)
		var handled atomic.Bool
		app := mustNew(t,
			credo.WithLogger(logger),
			credo.WithoutRequestID(),
			credo.WithAccessLogResultFilter(func(*credo.Context, credo.AccessLogEntry) bool {
				return false
			}),
		)
		app.GET("/", func(ctx *credo.Context) error {
			handled.Store(true)
			return ctx.Response().NoContent(200)
		})

		app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
		if !handled.Load() {
			t.Fatal("handler did not run")
		}
		if buf.Len() != 0 {
			t.Fatalf("filter=false emitted log: %s", buf.String())
		}
	})

	t.Run("MinLevel runs before filter", func(t *testing.T) {
		logger, buf := newTestLogger(t)
		var calls atomic.Int32
		app := mustNew(t,
			credo.WithLogger(logger),
			credo.WithoutRequestID(),
			credo.WithAccessLogMinLevel(slog.LevelWarn),
			credo.WithAccessLogResultFilter(func(*credo.Context, credo.AccessLogEntry) bool {
				calls.Add(1)
				return true
			}),
		)
		app.GET("/", func(ctx *credo.Context) error {
			return ctx.Response().NoContent(200)
		})

		app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
		if calls.Load() != 0 {
			t.Fatalf("filter calls = %d, want 0", calls.Load())
		}
		if buf.Len() != 0 {
			t.Fatalf("MinLevel-rejected entry was logged: %s", buf.String())
		}
	})

	t.Run("MetaAccessLog runs before filter", func(t *testing.T) {
		logger, buf := newTestLogger(t)
		var calls atomic.Int32
		app := mustNew(t,
			credo.WithLogger(logger),
			credo.WithoutRequestID(),
			credo.WithAccessLogResultFilter(func(*credo.Context, credo.AccessLogEntry) bool {
				calls.Add(1)
				return true
			}),
		)
		app.GET("/", func(ctx *credo.Context) error {
			return ctx.Response().NoContent(404)
		}).SetMeta(credo.MetaAccessLog, false)

		app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
		if calls.Load() != 0 {
			t.Fatalf("filter calls = %d, want 0", calls.Load())
		}
		if buf.Len() != 0 {
			t.Fatalf("meta-silenced entry was logged: %s", buf.String())
		}
	})
}

func TestBuiltinAccessLog_CustomLogger(t *testing.T) {
	appLogger, appBuf := newTestLogger(t)
	accessLogger, accessBuf := newTestLogger(t)

	app := mustNew(t,
		credo.WithLogger(appLogger),
		credo.WithAccessLogLogger(accessLogger),
	)
	app.GET("/", func(ctx *credo.Context) error {
		ctx.AddLogAttrs("tenant_id", "acme")
		ctx.Logger().Info("handler log")
		return ctx.Response().NoContent(200)
	})

	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if strings.Contains(appBuf.String(), "request completed") {
		t.Errorf("access log leaked to app logger: %s", appBuf.String())
	}
	if !strings.Contains(appBuf.String(), "handler log") || !strings.Contains(appBuf.String(), `"tenant_id":"acme"`) {
		t.Errorf("handler enrichment missing from app logger: %s", appBuf.String())
	}
	if !strings.Contains(accessBuf.String(), "request completed") {
		t.Fatalf("access record missing from custom logger: %s", accessBuf.String())
	}
	if strings.Contains(accessBuf.String(), "tenant_id") {
		t.Errorf("custom access logger inherited request-scoped attrs: %s", accessBuf.String())
	}
	if strings.Count(accessBuf.String(), `"request_id"`) != 1 {
		t.Errorf("custom access logger request_id count != 1: %s", accessBuf.String())
	}
}

func TestBuiltinAccessLog_ResultFilterSeesFinalErrorResponse(t *testing.T) {
	tests := []struct {
		name    string
		handler credo.Handler
	}{
		{
			name: "returned error",
			handler: func(*credo.Context) error {
				return credo.NewHTTPError(500).WithMessageKey("test.error")
			},
		},
		{
			name: "panic",
			handler: func(*credo.Context) error {
				panic("boom")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, _ := newTestLogger(t)
			var captured credo.AccessLogEntry
			app := mustNew(t,
				credo.WithLogger(logger),
				credo.WithoutRequestID(),
				credo.WithAccessLogResultFilter(func(_ *credo.Context, entry credo.AccessLogEntry) bool {
					captured = entry
					return true
				}),
			)
			app.GET("/", tt.handler)

			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
			if captured.Status != 500 || captured.Bytes <= 0 {
				t.Fatalf("filter snapshot status/bytes = %d/%d, want 500/>0", captured.Status, captured.Bytes)
			}
			if captured.Duration < 0 {
				t.Errorf("filter snapshot duration = %v, want >= 0", captured.Duration)
			}
		})
	}
}

func TestBuiltinAccessLog_NilOptionsKeepDefaults(t *testing.T) {
	logger, buf := newTestLogger(t)
	app := mustNew(t,
		credo.WithLogger(logger),
		credo.WithoutRequestID(),
		credo.WithAccessLogLogger(nil),
		credo.WithAccessLogMinLevel(nil),
		credo.WithAccessLogResultFilter(nil),
	)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	})

	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if buf.Len() == 0 {
		t.Fatal("nil access-log options did not preserve default logging")
	}
}

func TestBuiltinAccessLog_OptionsHaveNoEffectWhenDisabled(t *testing.T) {
	appLogger, appBuf := newTestLogger(t)
	accessLogger, accessBuf := newTestLogger(t)
	var filterCalls atomic.Int32
	app := mustNew(t,
		credo.WithLogger(appLogger),
		credo.WithoutAccessLog(),
		credo.WithAccessLogLogger(accessLogger),
		credo.WithAccessLogMinLevel(slog.LevelDebug),
		credo.WithAccessLogSkipper(func(*credo.Context) bool { return false }),
		credo.WithAccessLogResultFilter(func(*credo.Context, credo.AccessLogEntry) bool {
			filterCalls.Add(1)
			return true
		}),
	)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(200)
	})

	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if appBuf.Len() != 0 || accessBuf.Len() != 0 || filterCalls.Load() != 0 {
		t.Fatalf("disabled access log produced work: app=%q access=%q calls=%d",
			appBuf.String(), accessBuf.String(), filterCalls.Load())
	}
}
