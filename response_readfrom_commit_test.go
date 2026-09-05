package credo_test

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("source failed before the first byte")
}

// ReadFrom must not commit the response before the first byte: a source that
// fails up front leaves the handler free to return the error, and the central
// error handler still renders it — on the pooled branch, the delegation
// branch and a real net/http server.
func TestResponse_ReadFrom_DoesNotCommitBeforeFirstByte(t *testing.T) {
	for _, delegate := range []bool{false, true} {
		var w http.ResponseWriter = httptest.NewRecorder()
		if delegate {
			w = &readerFromRecorder{ResponseRecorder: w.(*httptest.ResponseRecorder)}
		}
		resp := credo.NewResponse(w)
		n, err := io.Copy(resp, failingReader{})
		if err == nil || n != 0 {
			t.Fatalf("delegate=%v: io.Copy = %d, %v", delegate, n, err)
		}
		if resp.Committed() || resp.Status() != 0 || resp.Size() != 0 {
			t.Fatalf("delegate=%v: committed=%v status=%d size=%d after a failed first read",
				delegate, resp.Committed(), resp.Status(), resp.Size())
		}
		// An empty source commits nothing either, exactly like Write never
		// being called.
		if n, err := io.Copy(resp, strings.NewReader("")); err != nil || n != 0 || resp.Committed() {
			t.Fatalf("delegate=%v: empty source: n=%d err=%v committed=%v", delegate, n, err, resp.Committed())
		}
		// The first byte commits 200.
		if _, err := io.Copy(resp, &patternReader{n: 10}); err != nil {
			t.Fatal(err)
		}
		if !resp.Committed() || resp.Status() != http.StatusOK || resp.Size() != 10 {
			t.Fatalf("delegate=%v: committed=%v status=%d size=%d after the first byte",
				delegate, resp.Committed(), resp.Status(), resp.Size())
		}
	}

	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		_, err := io.Copy(ctx.Response(), failingReader{})
		return err
	})
	srv := httptest.NewServer(app)
	defer srv.Close()
	res, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusInternalServerError || !strings.Contains(string(body), "internal_server_error") {
		t.Fatalf("live server: %d %q, want 500 error envelope", res.StatusCode, body)
	}
}

// prefixThenPanicReader yields prefix on its first read and panics on the
// next one: a streaming source whose producer fails after the body started.
type prefixThenPanicReader struct {
	prefix string
	done   bool
}

func (r *prefixThenPanicReader) Read(p []byte) (int, error) {
	if r.done {
		panic("source failed after the first bytes")
	}
	r.done = true
	return copy(p, r.prefix), nil
}

// A source that panics after its first bytes must leave the response
// committed: the bytes are on the wire with an implicit 200, so recovery has
// nothing left to render. Before the fix the delegation branch recorded the
// commit only after the writer's ReadFrom returned, which a panic skips; a
// real server then answered 200 with an error envelope appended to the
// prefix and logged the request as 500.
func TestResponse_ReadFrom_PanicAfterFirstByteKeepsCommit(t *testing.T) {
	for _, delegate := range []bool{false, true} {
		var w http.ResponseWriter = httptest.NewRecorder()
		if delegate {
			w = &readerFromRecorder{ResponseRecorder: w.(*httptest.ResponseRecorder)}
		}
		resp := credo.NewResponse(w)
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("delegate=%v: the source panic must propagate", delegate)
				}
			}()
			_, _ = io.Copy(resp, &prefixThenPanicReader{prefix: "PREFIX"})
		}()
		if !resp.Committed() || resp.Status() != http.StatusOK || resp.Size() != 6 {
			t.Fatalf("delegate=%v: committed=%v status=%d size=%d after a panic past the first byte",
				delegate, resp.Committed(), resp.Status(), resp.Size())
		}
	}

	var logs syncBuffer
	app := mustNew(t)
	app.UseAccessLog(credo.AccessLogConfig{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	app.GET("/", func(ctx *credo.Context) error {
		_, err := io.Copy(ctx.Response(), &prefixThenPanicReader{prefix: "PREFIX"})
		return err
	})
	srv := httptest.NewServer(app)
	defer srv.Close()
	res, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || string(body) != "PREFIX" {
		t.Fatalf("live server: %d %q, want 200 with the prefix alone (no envelope appended)", res.StatusCode, body)
	}
	if got := logs.String(); !strings.Contains(got, "status=200") || strings.Contains(got, "status=500") {
		t.Fatalf("access record must observe the committed 200, got:\n%s", got)
	}
}
