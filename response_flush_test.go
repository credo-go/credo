package credo_test

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

// TestResponse_FlushCommits: net/http writes status 200 when a flush comes
// before any status, so the flush commits the response. Response used to
// record nothing: an error the handler returned afterwards was rendered into
// the response already on the wire, its envelope appended to the flushed 200.
func TestResponse_FlushCommits(t *testing.T) {
	flushes := map[string]func(*credo.Response){
		"Flush":              func(res *credo.Response) { res.Flush() },
		"ResponseController": func(res *credo.Response) { _ = http.NewResponseController(res).Flush() },
	}
	for via, flush := range flushes {
		for _, compressed := range []bool{false, true} {
			name := via
			if compressed {
				name += " compressed"
			}
			t.Run(name, func(t *testing.T) {
				logs := &syncBuffer{}
				app := mustNew(t, credo.WithLogger(slog.New(slog.NewTextHandler(logs, nil))))
				if compressed {
					app.UseCompress()
				}
				var status int
				var committed bool
				app.GET("/", func(ctx *credo.Context) error {
					flush(ctx.Response())
					status, committed = ctx.Response().Status(), ctx.Response().Committed()
					return errors.New("failed after the flush")
				})
				srv := httptest.NewServer(app)
				defer srv.Close()

				got := liveExchange(t, srv, http.Header{"Accept-Encoding": {"gzip"}})

				if got.status != http.StatusOK || len(got.body) != 0 {
					t.Fatalf("wire = %d %q, want the flushed 200 with no envelope after it", got.status, got.body)
				}
				if status != http.StatusOK || !committed {
					t.Errorf("after the flush: Status() = %d, Committed() = %t, want 200, true", status, committed)
				}
				if out := logs.String(); !strings.Contains(out, "error after response committed") {
					t.Errorf("log = %q, want the error logged as arriving after the commit", out)
				}
			})
		}
	}
}

// TestResponse_FlushBeforeFirstWriteKeepsCompression: under UseCompress the
// compression decision is made when the header is written. A flush before
// the first write used to send the 200 header without it, and the next
// write then compressed the body that the header declared as identity.
func TestResponse_FlushBeforeFirstWriteKeepsCompression(t *testing.T) {
	app := mustNew(t)
	app.UseCompress()
	app.GET("/", func(ctx *credo.Context) error {
		res := ctx.Response()
		res.Header().Set("Content-Type", "text/html; charset=utf-8")
		res.Flush()
		_, err := res.Write([]byte("<p>streamed</p>"))
		return err
	})
	srv := httptest.NewServer(app)
	defer srv.Close()

	got := liveExchange(t, srv, http.Header{"Accept-Encoding": {"gzip"}})

	if got.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", got.status)
	}
	if encoding := got.header.Get("Content-Encoding"); encoding != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip on the flushed header", encoding)
	}
	if vary := got.header.Get("Vary"); !strings.Contains(vary, "Accept-Encoding") {
		t.Errorf("Vary = %q, want Accept-Encoding", vary)
	}
	if body := got.decodedBody(t); body != "<p>streamed</p>" {
		t.Errorf("body = %q, want the streamed document", body)
	}
}

// plainWriter is a ResponseWriter with no Flush, FlushError or Unwrap.
type plainWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newPlainWriter() *plainWriter { return &plainWriter{header: make(http.Header)} }

func (w *plainWriter) Header() http.Header { return w.header }

func (w *plainWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

func (w *plainWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

// TestResponse_FlushWithoutFlusher: when nothing in the chain can flush, the
// flush reports http.ErrNotSupported and commits nothing at any layer, so the
// handler's error is still rendered with its own status. Through Response it
// used to report success.
func TestResponse_FlushWithoutFlusher(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		name := "identity"
		if compressed {
			name = "compressed"
		}
		t.Run(name, func(t *testing.T) {
			app := mustNew(t, credo.WithLogger(slog.New(slog.NewTextHandler(&syncBuffer{}, nil))))
			if compressed {
				app.UseCompress()
			}
			var flushErr error
			var committed bool
			app.GET("/", func(ctx *credo.Context) error {
				flushErr = http.NewResponseController(ctx.Response()).Flush()
				committed = ctx.Response().Committed()
				return credo.ErrNotFound
			})

			w := newPlainWriter()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Accept-Encoding", "gzip")
			app.ServeHTTP(w, req)

			if !errors.Is(flushErr, http.ErrNotSupported) {
				t.Errorf("Flush() = %v, want http.ErrNotSupported", flushErr)
			}
			if committed {
				t.Error("Committed() = true after a flush that could not happen")
			}
			if w.status != http.StatusNotFound {
				t.Fatalf("status = %d, want the handler's 404", w.status)
			}
			body := w.body.String()
			if w.header.Get("Content-Encoding") == "gzip" {
				body = gunzipBody(t, &w.body)
			}
			if !strings.Contains(body, `"code":"not_found"`) {
				t.Errorf("body = %q, want the 404 envelope", body)
			}
		})
	}
}

// TestWrapStdMiddleware_FlushIsRecorded: a flush the stdlib middleware makes
// itself reaches the writer below Response, so the adapter's record has to
// carry it; Response adopts the record when the middleware returns.
func TestWrapStdMiddleware_FlushIsRecorded(t *testing.T) {
	logs := &syncBuffer{}
	app := mustNew(t, credo.WithLogger(slog.New(slog.NewTextHandler(logs, nil))))
	app.GlobalMiddleware(credo.WrapStdMiddleware(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Errorf("middleware Flush() = %v", err)
			}
			next.ServeHTTP(w, r)
		})
	}))
	app.GET("/", func(*credo.Context) error { return errors.New("failed after the flush") })
	srv := httptest.NewServer(app)
	defer srv.Close()

	got := liveExchange(t, srv, nil)

	if got.status != http.StatusOK || len(got.body) != 0 {
		t.Fatalf("wire = %d %q, want the flushed 200 with no envelope after it", got.status, got.body)
	}
	if out := logs.String(); !strings.Contains(out, "error after response committed") {
		t.Errorf("log = %q, want the error logged as arriving after the commit", out)
	}
}
