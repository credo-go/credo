package credo_test

import (
	"bufio"
	"compress/gzip"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

// responseState is what ctx.Response() reports at one point of a request.
type responseState struct {
	status    int
	size      int64
	committed bool
	hijacked  bool
}

// responseProbe captures the state of ctx.Response() as the chain unwinds
// past it. It is mutex-guarded because the live-server tests read it from the
// test goroutine.
type responseProbe struct {
	mu    sync.Mutex
	state responseState
}

// observe returns a Credo middleware that records the response state once the
// rest of the chain has returned. A non-nil after replaces the chain's error.
func (p *responseProbe) observe(after error) credo.Middleware {
	return func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			err := next(ctx)
			res := ctx.Response()
			p.mu.Lock()
			p.state = responseState{res.Status(), res.Size(), res.Committed(), res.Hijacked()}
			p.mu.Unlock()
			if after != nil {
				return after
			}
			return err
		}
	}
}

func (p *responseProbe) get() responseState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

// recordsWithMsg returns the JSON log records whose message is msg.
func recordsWithMsg(t *testing.T, raw, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rec := range parseJSONLines(t, []byte(raw)) {
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// assertAccessRecord checks the single access record of a request.
func assertAccessRecord(t *testing.T, raw string, status int, size int64) {
	t.Helper()
	recs := recordsWithMsg(t, raw, "request completed")
	if len(recs) != 1 {
		t.Fatalf("got %d access records, want 1:\n%s", len(recs), raw)
	}
	if got, _ := recs[0]["status"].(float64); int(got) != status {
		t.Errorf("access record status = %v, want %d", recs[0]["status"], status)
	}
	if got, _ := recs[0]["bytes"].(float64); int64(got) != size {
		t.Errorf("access record bytes = %v, want %d", recs[0]["bytes"], size)
	}
}

// answering adapts a stdlib middleware that never calls next: it answers the
// request by itself, as an authentication or rate-limit middleware does.
func answering(answer http.HandlerFunc) credo.Middleware {
	return credo.WrapStdMiddleware(func(http.Handler) http.Handler { return answer })
}

// passingOn is a stdlib middleware that hands its own writer wrapper to next.
func passingOn(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&writerSpy{ResponseWriter: w}, r)
	})
}

// serveRecovering serves one request and fails the test, instead of crashing
// the test binary, when the App panics out of ServeHTTP.
func serveRecovering(t *testing.T, app *credo.App, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	defer func() {
		if rvr := recover(); rvr != nil {
			t.Fatalf("ServeHTTP panicked: %v", rvr)
		}
	}()
	app.ServeHTTP(w, r)
}

// TestWrapStdMiddleware_ShortCircuitIsRecorded: a response an adapted stdlib
// middleware writes by itself is the request's response. Response and the
// access record report its status and size instead of an uncommitted 200/0.
func TestWrapStdMiddleware_ShortCircuitIsRecorded(t *testing.T) {
	tests := []struct {
		name       string
		answer     http.HandlerFunc
		wantStatus int
		wantBody   string
	}{
		{
			name: "WriteHeader and Write",
			answer: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte("denied"))
			},
			wantStatus: http.StatusUnauthorized,
			wantBody:   "denied",
		},
		{
			name: "http.Error",
			answer: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "denied", http.StatusForbidden)
			},
			wantStatus: http.StatusForbidden,
			wantBody:   "denied\n",
		},
		{
			name: "a Write alone commits 200",
			answer: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("cached"))
			},
			wantStatus: http.StatusOK,
			wantBody:   "cached",
		},
		{
			name: "io.WriteString",
			answer: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, "slow down")
			},
			wantStatus: http.StatusTooManyRequests,
			wantBody:   "slow down",
		},
		{
			name: "status only",
			answer: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			},
			wantStatus: http.StatusNoContent,
		},
		{
			name: "a second WriteHeader does not replace the status",
			answer: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("denied"))
			},
			wantStatus: http.StatusUnauthorized,
			wantBody:   "denied",
		},
		{
			name: "through the middleware's own wrapper",
			answer: func(w http.ResponseWriter, _ *http.Request) {
				ww := &writerSpy{ResponseWriter: w}
				ww.WriteHeader(http.StatusUnauthorized)
				_, _ = ww.Write([]byte("denied"))
			},
			wantStatus: http.StatusUnauthorized,
			wantBody:   "denied",
		},
		{
			name: "http.ServeContent",
			answer: func(w http.ResponseWriter, r *http.Request) {
				http.ServeContent(w, r, "note.txt", time.Time{}, strings.NewReader("served by the middleware"))
			},
			wantStatus: http.StatusOK,
			wantBody:   "served by the middleware",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, logs := newTestLogger(t)
			app := mustNew(t, credo.WithLogger(logger))
			app.UseAccessLog()

			var probe responseProbe
			handlerRan := false
			app.GlobalMiddleware(probe.observe(nil), answering(tt.answer))
			app.GET("/", func(ctx *credo.Context) error {
				handlerRan = true
				return ctx.Response().Text(http.StatusOK, "handler")
			})

			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

			if handlerRan {
				t.Fatal("the handler ran although the middleware never called next")
			}
			if w.Code != tt.wantStatus || w.Body.String() != tt.wantBody {
				t.Fatalf("wire = %d %q, want %d %q", w.Code, w.Body.String(), tt.wantStatus, tt.wantBody)
			}
			want := responseState{status: tt.wantStatus, size: int64(len(tt.wantBody)), committed: true}
			if got := probe.get(); got != want {
				t.Errorf("Response after the middleware = %+v, want %+v", got, want)
			}
			assertAccessRecord(t, logs.String(), tt.wantStatus, int64(len(tt.wantBody)))
		})
	}
}

// TestWrapStdMiddleware_PassThroughIsCountedOnce: what the handler writes
// through Response passes the adapter's recording writer on its way out. It is
// counted by Response alone, however many adapted middlewares it passes.
func TestWrapStdMiddleware_PassThroughIsCountedOnce(t *testing.T) {
	sameWriter := func(next http.Handler) http.Handler { return next }
	tests := []struct {
		name string
		mws  []credo.StdMiddleware
	}{
		{"the writer it was given", []credo.StdMiddleware{sameWriter}},
		{"its own wrapper", []credo.StdMiddleware{passingOn}},
		{"two adapted middlewares", []credo.StdMiddleware{passingOn, sameWriter}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, logs := newTestLogger(t)
			app := mustNew(t, credo.WithLogger(logger))
			app.UseAccessLog()

			var probe responseProbe
			app.GlobalMiddleware(probe.observe(nil))
			for _, m := range tt.mws {
				app.GlobalMiddleware(credo.WrapStdMiddleware(m))
			}
			app.GET("/", func(ctx *credo.Context) error {
				return ctx.Response().Text(http.StatusCreated, "hello")
			})

			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

			if w.Code != http.StatusCreated || w.Body.String() != "hello" {
				t.Fatalf("wire = %d %q, want 201 %q", w.Code, w.Body.String(), "hello")
			}
			want := responseState{status: http.StatusCreated, size: 5, committed: true}
			if got := probe.get(); got != want {
				t.Errorf("Response after the middleware = %+v, want %+v", got, want)
			}
			assertAccessRecord(t, logs.String(), http.StatusCreated, 5)
		})
	}
}

// TestWrapStdMiddleware_NestedShortCircuitIsCountedOnce: the answer of an
// inner adapted middleware passes the outer adapter's recording writer too.
func TestWrapStdMiddleware_NestedShortCircuitIsCountedOnce(t *testing.T) {
	app := mustNew(t)
	var probe responseProbe
	app.GlobalMiddleware(
		probe.observe(nil),
		credo.WrapStdMiddleware(passingOn),
		answering(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "denied", http.StatusUnauthorized)
		}),
	)
	app.GET("/", func(ctx *credo.Context) error { return ctx.Response().NoContent(http.StatusNoContent) })

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	want := responseState{status: http.StatusUnauthorized, size: int64(len("denied\n")), committed: true}
	if got := probe.get(); got != want {
		t.Errorf("Response after the middlewares = %+v, want %+v", got, want)
	}
}

// TestWrapStdMiddleware_ShortCircuitUnderCompression: the middleware's answer
// goes through the response compressor like a handler's. Response reports what
// the middleware wrote, the access record what the transport accepted.
func TestWrapStdMiddleware_ShortCircuitUnderCompression(t *testing.T) {
	logger, logs := newTestLogger(t)
	app := mustNew(t, credo.WithLogger(logger))
	app.UseCompress()
	app.UseAccessLog()

	body := strings.Repeat("denied ", 200)
	var probe responseProbe
	app.GlobalMiddleware(probe.observe(nil), answering(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, body)
	}))
	app.GET("/", func(ctx *credo.Context) error { return ctx.Response().NoContent(http.StatusNoContent) })

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	app.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized || w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("wire = %d with Content-Encoding %q, want 401 gzip", w.Code, w.Header().Get("Content-Encoding"))
	}
	wire := int64(w.Body.Len())
	zr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader() = %v", err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil || string(plain) != body {
		t.Fatalf("decoded body = %d bytes, %v; want the %d bytes the middleware wrote", len(plain), err, len(body))
	}
	want := responseState{status: http.StatusUnauthorized, size: int64(len(body)), committed: true}
	if got := probe.get(); got != want {
		t.Errorf("Response after the middleware = %+v, want %+v", got, want)
	}
	assertAccessRecord(t, logs.String(), http.StatusUnauthorized, wire)
}

// bareResponseWriter exposes only the three ResponseWriter methods: neither
// Flush nor Unwrap is reachable through it.
type bareResponseWriter struct{ http.ResponseWriter }

// TestWrapStdMiddleware_FlushReachesTheUnderlyingWriter: the recording writer
// neither swallows a flush nor claims one the underlying writer cannot do.
func TestWrapStdMiddleware_FlushReachesTheUnderlyingWriter(t *testing.T) {
	tests := []struct {
		name    string
		writer  func(rec *httptest.ResponseRecorder) http.ResponseWriter
		wantErr error
	}{
		{"a Flusher", func(rec *httptest.ResponseRecorder) http.ResponseWriter { return rec }, nil},
		{"a Flusher behind Unwrap", func(rec *httptest.ResponseRecorder) http.ResponseWriter {
			return &unwrapResponseWriter{ResponseWriter: rec}
		}, nil},
		{"no Flusher", func(rec *httptest.ResponseRecorder) http.ResponseWriter {
			return bareResponseWriter{rec}
		}, http.ErrNotSupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			var flushErr error
			app.GlobalMiddleware(credo.WrapStdMiddleware(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					flushErr = http.NewResponseController(w).Flush()
					next.ServeHTTP(w, r)
				})
			}))
			app.GET("/", func(ctx *credo.Context) error {
				return ctx.Response().Text(http.StatusOK, "ok")
			})

			rec := httptest.NewRecorder()
			app.ServeHTTP(tt.writer(rec), httptest.NewRequest(http.MethodGet, "/", nil))

			if !errors.Is(flushErr, tt.wantErr) {
				t.Errorf("ResponseController.Flush() = %v, want %v", flushErr, tt.wantErr)
			}
			if rec.Flushed != (tt.wantErr == nil) {
				t.Errorf("underlying writer flushed = %v, want %v", rec.Flushed, tt.wantErr == nil)
			}
			if rec.Body.String() != "ok" {
				t.Errorf("body = %q, want %q", rec.Body.String(), "ok")
			}
		})
	}
}

// pushSpy is a ResponseWriter that supports HTTP/2 server push.
type pushSpy struct {
	*httptest.ResponseRecorder
	pushed []string
}

func (w *pushSpy) Push(target string, _ *http.PushOptions) error {
	w.pushed = append(w.pushed, target)
	return nil
}

// TestWrapStdMiddleware_PushIsForwarded: server push is available to the
// middleware exactly when the underlying writer supports it.
func TestWrapStdMiddleware_PushIsForwarded(t *testing.T) {
	newApp := func(pushErr *error) *credo.App {
		app := mustNew(t)
		app.GlobalMiddleware(credo.WrapStdMiddleware(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				*pushErr = w.(http.Pusher).Push("/app.css", nil)
				next.ServeHTTP(w, r)
			})
		}))
		app.GET("/", func(ctx *credo.Context) error { return ctx.Response().NoContent(http.StatusNoContent) })
		return app
	}

	t.Run("supported", func(t *testing.T) {
		var pushErr error
		w := &pushSpy{ResponseRecorder: httptest.NewRecorder()}
		newApp(&pushErr).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		if pushErr != nil || !slices.Equal(w.pushed, []string{"/app.css"}) {
			t.Errorf("Push() = %v with %v pushed, want nil with [/app.css]", pushErr, w.pushed)
		}
	})

	t.Run("unsupported", func(t *testing.T) {
		var pushErr error
		newApp(&pushErr).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		if !errors.Is(pushErr, http.ErrNotSupported) {
			t.Errorf("Push() = %v, want http.ErrNotSupported", pushErr)
		}
	})
}

// stringWriterSpy counts the strings that reach the writer as strings.
type stringWriterSpy struct {
	*httptest.ResponseRecorder
	stringWrites int
}

func (w *stringWriterSpy) WriteString(s string) (int, error) {
	w.stringWrites++
	return w.ResponseRecorder.WriteString(s)
}

// TestWrapStdMiddleware_KeepsWriteString: a string the handler writes still
// reaches the underlying writer's WriteString behind an adapted middleware,
// without the []byte copy a writer lacking the method would force.
func TestWrapStdMiddleware_KeepsWriteString(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(credo.WrapStdMiddleware(func(next http.Handler) http.Handler { return next }))
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, "hello")
	})

	w := &stringWriterSpy{ResponseRecorder: httptest.NewRecorder()}
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if w.stringWrites != 1 || w.Body.String() != "hello" {
		t.Errorf("underlying WriteString calls = %d with body %q, want 1 with %q", w.stringWrites, w.Body.String(), "hello")
	}
}

// TestWrapStdMiddleware_ErrorAfterShortCircuitWritesNoSecondResponse: once an
// adapted middleware has answered, the response is committed. A failure that
// surfaces afterwards is logged; it does not append an error body to the
// answer that is already on the wire.
func TestWrapStdMiddleware_ErrorAfterShortCircuitWritesNoSecondResponse(t *testing.T) {
	t.Run("an outer middleware returns an error", func(t *testing.T) {
		logger, logs := newTestLogger(t)
		app := mustNew(t, credo.WithLogger(logger))

		var probe responseProbe
		app.GlobalMiddleware(
			probe.observe(errors.New("failure after the answer")),
			answering(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte("denied"))
			}),
		)
		app.GET("/", func(ctx *credo.Context) error { return ctx.Response().NoContent(http.StatusNoContent) })

		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

		if w.Code != http.StatusUnauthorized || w.Body.String() != "denied" {
			t.Fatalf("wire = %d %q, want the middleware's 401 %q alone", w.Code, w.Body.String(), "denied")
		}
		raw := logs.String()
		if got := recordsWithMsg(t, raw, "credo: error after response committed"); len(got) != 1 {
			t.Errorf("got %d after-commit records, want 1:\n%s", len(got), raw)
		}
		if got := recordsWithMsg(t, raw, "credo: unhandled error"); len(got) != 0 {
			t.Errorf("the error was rendered although the response was committed:\n%s", raw)
		}
	})

	t.Run("the middleware panics after answering", func(t *testing.T) {
		logger, logs := newTestLogger(t)
		app := mustNew(t, credo.WithLogger(logger))
		app.UseAccessLog()

		app.GlobalMiddleware(answering(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("denied"))
			panic("after the answer")
		}))
		app.GET("/", func(ctx *credo.Context) error { return ctx.Response().NoContent(http.StatusNoContent) })

		w := httptest.NewRecorder()
		serveRecovering(t, app, w, httptest.NewRequest(http.MethodGet, "/", nil))

		if w.Code != http.StatusUnauthorized || w.Body.String() != "denied" {
			t.Fatalf("wire = %d %q, want the middleware's 401 %q alone", w.Code, w.Body.String(), "denied")
		}
		raw := logs.String()
		if got := recordsWithMsg(t, raw, "panic recovered"); len(got) != 1 {
			t.Errorf("got %d panic records, want 1:\n%s", len(got), raw)
		}
		assertAccessRecord(t, raw, http.StatusUnauthorized, 6)
	})
}

// hijackedConnWriter behaves like net/http's writer around a hijack: once
// Hijack succeeded the connection is no longer the response's, so every later
// write is refused, and counted.
type hijackedConnWriter struct {
	*httptest.ResponseRecorder
	hijackErr   error
	hijackCalls int
	hijacked    bool
	lateWrites  int
}

func (w *hijackedConnWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijackCalls++
	if w.hijackErr != nil {
		return nil, nil, w.hijackErr
	}
	w.hijacked = true
	return nil, nil, nil
}

func (w *hijackedConnWriter) WriteHeader(code int) {
	if w.hijacked {
		w.lateWrites++
		return
	}
	w.ResponseRecorder.WriteHeader(code)
}

func (w *hijackedConnWriter) Write(p []byte) (int, error) {
	if w.hijacked {
		w.lateWrites++
		return 0, http.ErrHijacked
	}
	return w.ResponseRecorder.Write(p)
}

func (w *hijackedConnWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

// TestWrapStdMiddleware_HijackIsRecorded: a connection an adapted stdlib
// middleware hijacks is no longer the response's. Response reports the hijack,
// so the error pipeline writes nothing to the connection and the response
// compressor is abandoned instead of flushed into it.
func TestWrapStdMiddleware_HijackIsRecorded(t *testing.T) {
	tests := []struct {
		name   string
		hijack func(w http.ResponseWriter) error
	}{
		{"through http.Hijacker", func(w http.ResponseWriter) error {
			_, _, err := w.(http.Hijacker).Hijack()
			return err
		}},
		{"through http.ResponseController", func(w http.ResponseWriter) error {
			_, _, err := http.NewResponseController(w).Hijack()
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, logs := newTestLogger(t)
			app := mustNew(t, credo.WithLogger(logger))
			app.UseCompress()

			var (
				probe     responseProbe
				hijackErr error
			)
			app.GlobalMiddleware(
				probe.observe(errors.New("failure after the hijack")),
				answering(func(w http.ResponseWriter, _ *http.Request) { hijackErr = tt.hijack(w) }),
			)
			app.GET("/", func(ctx *credo.Context) error { return ctx.Response().NoContent(http.StatusNoContent) })

			w := &hijackedConnWriter{ResponseRecorder: httptest.NewRecorder()}
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("Accept-Encoding", "gzip")
			serveRecovering(t, app, w, r)

			if hijackErr != nil || w.hijackCalls != 1 {
				t.Fatalf("Hijack() = %v after %d underlying calls, want nil after 1", hijackErr, w.hijackCalls)
			}
			if got, want := probe.get(), (responseState{hijacked: true}); got != want {
				t.Errorf("Response after the middleware = %+v, want %+v", got, want)
			}
			if w.lateWrites != 0 {
				t.Errorf("%d writes reached the hijacked connection, want none", w.lateWrites)
			}
			raw := logs.String()
			if got := recordsWithMsg(t, raw, "credo: error after response hijacked"); len(got) != 1 {
				t.Errorf("got %d after-hijack records, want 1:\n%s", len(got), raw)
			}
			if got := recordsWithMsg(t, raw, "credo: response compression failed"); len(got) != 0 {
				t.Errorf("the compressor was flushed into the hijacked connection:\n%s", raw)
			}
		})
	}

	t.Run("a failed hijack is not recorded", func(t *testing.T) {
		app := mustNew(t)
		var (
			probe     responseProbe
			hijackErr error
		)
		app.GlobalMiddleware(
			probe.observe(credo.NewHTTPError(http.StatusBadGateway)),
			answering(func(w http.ResponseWriter, _ *http.Request) {
				_, _, hijackErr = http.NewResponseController(w).Hijack()
			}),
		)
		app.GET("/", func(ctx *credo.Context) error { return ctx.Response().NoContent(http.StatusNoContent) })

		refused := errors.New("hijack refused")
		w := &hijackedConnWriter{ResponseRecorder: httptest.NewRecorder(), hijackErr: refused}
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

		if !errors.Is(hijackErr, refused) {
			t.Fatalf("Hijack() = %v, want %v", hijackErr, refused)
		}
		if got := probe.get(); got != (responseState{}) {
			t.Errorf("Response after the middleware = %+v, want the zero state", got)
		}
		if w.Code != http.StatusBadGateway {
			t.Errorf("status = %d, want the error pipeline's 502", w.Code)
		}
	})

	t.Run("a hijack through Response stays as Response recorded it", func(t *testing.T) {
		app := mustNew(t)
		var probe responseProbe
		app.GlobalMiddleware(
			probe.observe(nil),
			credo.WrapStdMiddleware(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					next.ServeHTTP(w, r)
					// Refused by the connection: this is not a response.
					w.WriteHeader(http.StatusInternalServerError)
				})
			}),
		)
		app.GET("/", func(ctx *credo.Context) error {
			_, _, err := ctx.Response().Hijack()
			return err
		})

		w := &hijackedConnWriter{ResponseRecorder: httptest.NewRecorder()}
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

		if w.hijackCalls != 1 {
			t.Fatalf("underlying Hijack calls = %d, want 1", w.hijackCalls)
		}
		if got, want := probe.get(), (responseState{hijacked: true}); got != want {
			t.Errorf("Response after the middleware = %+v, want %+v", got, want)
		}
	})
}

// TestWrapStdMiddleware_LiveServer runs the cases that need net/http's own
// writer: informational responses, deadlines reached through Unwrap, and a
// real hijacked connection under response compression.
func TestWrapStdMiddleware_LiveServer(t *testing.T) {
	t.Run("an informational status is not the response status", func(t *testing.T) {
		app := mustNew(t)
		var probe responseProbe
		app.GlobalMiddleware(
			probe.observe(nil),
			answering(func(w http.ResponseWriter, _ *http.Request) {
				if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
					t.Errorf("SetWriteDeadline() = %v, want the connection reached through Unwrap", err)
				}
				w.Header().Set("Link", "</app.css>; rel=preload; as=style")
				w.WriteHeader(http.StatusEarlyHints)
				http.Error(w, "denied", http.StatusUnauthorized)
			}),
		)
		app.GET("/", func(ctx *credo.Context) error { return ctx.Response().NoContent(http.StatusNoContent) })

		srv := httptest.NewServer(app)
		var hints []int
		trace := &httptrace.ClientTrace{
			Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
				hints = append(hints, code)
				return nil
			},
		}
		req, err := http.NewRequestWithContext(httptrace.WithClientTrace(t.Context(), trace),
			http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		srv.Close()

		if res.StatusCode != http.StatusUnauthorized || string(body) != "denied\n" {
			t.Fatalf("wire = %d %q, want 401 %q", res.StatusCode, body, "denied\n")
		}
		if !slices.Equal(hints, []int{http.StatusEarlyHints}) {
			t.Errorf("informational responses = %v, want [103]", hints)
		}
		want := responseState{status: http.StatusUnauthorized, size: int64(len("denied\n")), committed: true}
		if got := probe.get(); got != want {
			t.Errorf("Response after the middleware = %+v, want %+v", got, want)
		}
	})

	t.Run("a hijacked connection is left alone", func(t *testing.T) {
		logs := &syncBuffer{}
		app := mustNew(t, credo.WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
		app.UseCompress()

		const answer = "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
		var probe responseProbe
		app.GlobalMiddleware(
			probe.observe(errors.New("failure after the hijack")),
			answering(func(w http.ResponseWriter, _ *http.Request) {
				conn, buf, err := http.NewResponseController(w).Hijack()
				if err != nil {
					t.Errorf("Hijack() = %v", err)
					return
				}
				defer conn.Close()
				_, _ = buf.WriteString(answer)
				_ = buf.Flush()
			}),
		)
		app.GET("/", func(ctx *credo.Context) error { return ctx.Response().NoContent(http.StatusNoContent) })

		// The client is done as soon as the middleware closed the connection;
		// the request is done when ServeHTTP returned.
		done := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(done)
			app.ServeHTTP(w, r)
		}))
		defer srv.Close()

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Accept-Encoding", "gzip")
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the request did not finish")
		}

		if res.StatusCode != http.StatusOK || string(body) != "ok" {
			t.Fatalf("wire = %d %q, want the middleware's own 200 %q", res.StatusCode, body, "ok")
		}
		if got, want := probe.get(), (responseState{hijacked: true}); got != want {
			t.Errorf("Response after the middleware = %+v, want %+v", got, want)
		}
		raw := logs.String()
		if got := recordsWithMsg(t, raw, "credo: error after response hijacked"); len(got) != 1 {
			t.Errorf("got %d after-hijack records, want 1:\n%s", len(got), raw)
		}
		if got := recordsWithMsg(t, raw, "credo: response compression failed"); len(got) != 0 {
			t.Errorf("the compressor was flushed into the hijacked connection:\n%s", raw)
		}
	})
}

// readFromCall is what one ReadFrom call on a readFromSpy received.
type readFromCall struct {
	limited bool      // the source was a *io.LimitedReader
	inner   io.Reader // the reader it wrapped
}

// readFromSpy is a ResponseWriter with an observable ReadFrom. It notes the
// shape of each source on arrival: one io.LimitedReader over the caller's
// reader is what net's sendfile path unwraps.
type readFromSpy struct {
	*httptest.ResponseRecorder
	calls []readFromCall
}

func (w *readFromSpy) ReadFrom(src io.Reader) (int64, error) {
	var call readFromCall
	if lr, ok := src.(*io.LimitedReader); ok {
		call = readFromCall{limited: true, inner: lr.R}
	}
	w.calls = append(w.calls, call)
	return io.Copy(w.ResponseRecorder.Body, src)
}

// TestWrapStdMiddleware_KeepsReadFromDelegation: the recording writer does not
// cost a handler the underlying writer's ReadFrom. The copy is still
// delegated, and the source arrives as Response wrapped it.
func TestWrapStdMiddleware_KeepsReadFromDelegation(t *testing.T) {
	const size = 100 << 10
	app := mustNew(t)
	var probe responseProbe
	app.GlobalMiddleware(
		probe.observe(nil),
		credo.WrapStdMiddleware(func(next http.Handler) http.Handler { return next }),
	)
	src := &patternReader{n: size}
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Stream(http.StatusOK, "application/octet-stream", src)
	})

	w := &readFromSpy{ResponseRecorder: httptest.NewRecorder()}
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if len(w.calls) != 1 {
		t.Fatalf("underlying ReadFrom calls = %d, want 1", len(w.calls))
	}
	if call := w.calls[0]; !call.limited || call.inner != io.Reader(src) {
		t.Errorf("underlying ReadFrom received %+v, want one io.LimitedReader over the handler's reader", call)
	}
	checkPattern(t, w.Body.Bytes(), size)
	want := responseState{status: http.StatusOK, size: size, committed: true}
	if got := probe.get(); got != want {
		t.Errorf("Response after the middleware = %+v, want %+v", got, want)
	}
}

// TestWrapStdMiddleware_MiddlewareCopyIsRecorded: a body the middleware copies
// to its writer is counted whichever way the bytes go out, and the first byte
// commits 200 when the middleware wrote no status.
func TestWrapStdMiddleware_MiddlewareCopyIsRecorded(t *testing.T) {
	const size = 100 << 10
	tests := []struct {
		name       string
		status     int // 0 leaves the commit to the first byte
		readerFrom bool
	}{
		{"status first, ReadFrom underneath", http.StatusAccepted, true},
		{"status first, Write underneath", http.StatusAccepted, false},
		{"no status, ReadFrom underneath", 0, true},
		{"no status, Write underneath", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			var (
				probe   responseProbe
				copied  int64
				copyErr error
			)
			src := &patternReader{n: size}
			app.GlobalMiddleware(probe.observe(nil), answering(func(w http.ResponseWriter, _ *http.Request) {
				if tt.status != 0 {
					w.WriteHeader(tt.status)
				}
				copied, copyErr = io.Copy(w, src)
			}))
			app.GET("/", func(ctx *credo.Context) error { return ctx.Response().NoContent(http.StatusNoContent) })

			rec := httptest.NewRecorder()
			spy := &readFromSpy{ResponseRecorder: rec}
			var w http.ResponseWriter = rec
			if tt.readerFrom {
				w = spy
			}
			app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

			if copied != size || copyErr != nil {
				t.Fatalf("io.Copy() = %d, %v; want %d, nil", copied, copyErr, size)
			}
			checkPattern(t, rec.Body.Bytes(), size)
			wantStatus := tt.status
			if wantStatus == 0 {
				wantStatus = http.StatusOK
			}
			want := responseState{status: wantStatus, size: size, committed: true}
			if got := probe.get(); got != want {
				t.Errorf("Response after the middleware = %+v, want %+v", got, want)
			}
			if tt.readerFrom {
				if len(spy.calls) != 1 {
					t.Fatalf("underlying ReadFrom calls = %d, want 1", len(spy.calls))
				}
				if call := spy.calls[0]; !call.limited || call.inner != io.Reader(src) {
					t.Errorf("underlying ReadFrom received %+v, want one io.LimitedReader over the middleware's reader", call)
				}
			}
		})
	}
}

// TestWrapStdMiddleware_MiddlewareCopyPanicIsCounted: a source that panics in
// the middle of the middleware's copy leaves a committed response. Nothing is
// appended to it, and the access record counts the bytes that went out before
// the panic — the delegated ones included.
func TestWrapStdMiddleware_MiddlewareCopyPanicIsCounted(t *testing.T) {
	const size = 64 << 10
	for _, readerFrom := range []bool{false, true} {
		logger, logs := newTestLogger(t)
		app := mustNew(t, credo.WithLogger(logger))
		app.UseAccessLog()
		app.GlobalMiddleware(answering(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.Copy(w, &zeroThenPanicReader{remaining: size})
		}))
		app.GET("/", func(ctx *credo.Context) error { return ctx.Response().NoContent(http.StatusNoContent) })

		rec := httptest.NewRecorder()
		var w http.ResponseWriter = rec
		if readerFrom {
			w = &readFromSpy{ResponseRecorder: rec}
		}
		serveRecovering(t, app, w, httptest.NewRequest(http.MethodGet, "/", nil))

		if rec.Code != http.StatusOK || rec.Body.Len() != size {
			t.Fatalf("readerFrom=%v: wire = %d with %d bytes, want 200 with %d and nothing appended",
				readerFrom, rec.Code, rec.Body.Len(), size)
		}
		assertAccessRecord(t, logs.String(), http.StatusOK, size)
	}
}
