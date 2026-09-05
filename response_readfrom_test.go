package credo_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/credo-go/credo"
)

// patternReader implements io.Reader only (no io.WriterTo) and yields a
// deterministic byte pattern so a receiver can verify the whole body.
type patternReader struct {
	off, n int
}

func (r *patternReader) Read(p []byte) (int, error) {
	if r.off >= r.n {
		return 0, io.EOF
	}
	c := min(len(p), r.n-r.off)
	for i := range c {
		p[i] = patternByte(r.off + i)
	}
	r.off += c
	return c, nil
}

func patternByte(i int) byte { return byte(i % 251) }

func checkPattern(t *testing.T, body []byte, n int) {
	t.Helper()
	if len(body) != n {
		t.Fatalf("body length = %d, want %d", len(body), n)
	}
	for i, b := range body {
		if b != patternByte(i) {
			t.Fatalf("body[%d] = %d, want %d", i, b, patternByte(i))
		}
	}
}

// readerFromRecorder is a ResponseWriter whose ReadFrom is observable, so a
// test can tell the delegation branch from the pooled copy.
type readerFromRecorder struct {
	*httptest.ResponseRecorder
	readFromCalls int
}

func (w *readerFromRecorder) ReadFrom(src io.Reader) (int64, error) {
	w.readFromCalls++
	return io.Copy(w.ResponseRecorder.Body, src)
}

// failingWriter accepts limit bytes and then fails; it offers no ReadFrom.
type failingWriter struct {
	*httptest.ResponseRecorder
	limit    int
	accepted int
}

var errWriterClosed = errors.New("writer closed")

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.accepted >= w.limit {
		return 0, errWriterClosed
	}
	n := min(len(p), w.limit-w.accepted)
	w.accepted += n
	return n, errWriterClosed
}

// hijackableWriter satisfies http.Hijacker with a pipe connection.
type hijackableWriter struct {
	*httptest.ResponseRecorder
}

func (w *hijackableWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	server, _ := net.Pipe()
	return server, bufio.NewReadWriter(bufio.NewReader(server), bufio.NewWriter(server)), nil
}

func TestResponse_ReadFrom_DelegatesToUnderlyingReaderFrom(t *testing.T) {
	const size = 100 << 10
	w := &readerFromRecorder{ResponseRecorder: httptest.NewRecorder()}
	resp := credo.NewResponse(w)

	n, err := resp.ReadFrom(&patternReader{n: size})
	if err != nil {
		t.Fatalf("ReadFrom() error = %v", err)
	}
	if n != size {
		t.Fatalf("ReadFrom() n = %d, want %d", n, size)
	}
	if w.readFromCalls != 1 {
		t.Fatalf("underlying ReadFrom calls = %d, want 1", w.readFromCalls)
	}
	if got := resp.Size(); got != size {
		t.Fatalf("Size() = %d, want %d", got, size)
	}
	if got := resp.Status(); got != http.StatusOK {
		t.Fatalf("Status() = %d, want 200 (first write commits)", got)
	}
	checkPattern(t, w.Body.Bytes(), size)
}

func TestResponse_ReadFrom_PooledCopyWithoutReaderFrom(t *testing.T) {
	const size = 100 << 10 // several 32 KiB buffer rounds
	w := httptest.NewRecorder()
	resp := credo.NewResponse(w)
	resp.WriteHeader(http.StatusCreated)

	n, err := resp.ReadFrom(&patternReader{n: size})
	if err != nil {
		t.Fatalf("ReadFrom() error = %v", err)
	}
	if n != size {
		t.Fatalf("ReadFrom() n = %d, want %d", n, size)
	}
	if got := resp.Size(); got != size {
		t.Fatalf("Size() = %d, want %d", got, size)
	}
	if got := resp.Status(); got != http.StatusCreated {
		t.Fatalf("Status() = %d, want 201 (explicit header kept)", got)
	}
	checkPattern(t, w.Body.Bytes(), size)
}

func TestResponse_ReadFrom_WriterToSourceIsCounted(t *testing.T) {
	// A src implementing io.WriterTo writes itself into the destination;
	// the pooled branch must still route it through Response.Write.
	body := strings.Repeat("y", 3000)
	w := httptest.NewRecorder()
	resp := credo.NewResponse(w)

	n, err := resp.ReadFrom(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ReadFrom() error = %v", err)
	}
	if n != int64(len(body)) || resp.Size() != int64(len(body)) {
		t.Fatalf("n = %d, Size() = %d, want %d", n, resp.Size(), len(body))
	}
	if w.Body.String() != body {
		t.Fatal("body mismatch")
	}
}

func TestResponse_ReadFrom_PartialWriteError(t *testing.T) {
	w := &failingWriter{ResponseRecorder: httptest.NewRecorder(), limit: 10}
	resp := credo.NewResponse(w)

	n, err := resp.ReadFrom(&patternReader{n: 1000})
	if !errors.Is(err, errWriterClosed) {
		t.Fatalf("ReadFrom() error = %v, want %v", err, errWriterClosed)
	}
	if n != 10 {
		t.Fatalf("ReadFrom() n = %d, want 10 (bytes the writer accepted)", n)
	}
	if got := resp.Size(); got != 10 {
		t.Fatalf("Size() = %d, want 10", got)
	}
}

func TestResponse_ReadFrom_Hijacked(t *testing.T) {
	w := &hijackableWriter{ResponseRecorder: httptest.NewRecorder()}
	resp := credo.NewResponse(w)
	conn, _, err := resp.Hijack()
	if err != nil {
		t.Fatalf("Hijack() error = %v", err)
	}
	defer conn.Close()

	n, err := resp.ReadFrom(&patternReader{n: 100})
	if !errors.Is(err, http.ErrHijacked) {
		t.Fatalf("ReadFrom() error = %v, want http.ErrHijacked", err)
	}
	if n != 0 || resp.Size() != 0 {
		t.Fatalf("n = %d, Size() = %d, want 0", n, resp.Size())
	}
}

// TestResponse_Stream_LiveServers streams a Reader-only body through real
// net/http servers: HTTP/1.1 plaintext (net/http delegation, sendfile-capable
// path), TLS (net/http delegation with its internal buffer fallback) and
// HTTP/2 (no io.ReaderFrom on the writer, pooled copy), each with and without
// the compression feature wrapping the writer.
func TestResponse_Stream_LiveServers(t *testing.T) {
	const size = 1 << 20 // well past net/http's 512-byte sniff prefix

	protocols := []struct {
		name  string
		start func(t *testing.T, app *credo.App) *httptest.Server
		proto int
	}{
		{"HTTP1", func(_ *testing.T, app *credo.App) *httptest.Server {
			return httptest.NewServer(app)
		}, 1},
		{"TLS", func(_ *testing.T, app *credo.App) *httptest.Server {
			return httptest.NewTLSServer(app)
		}, 1},
		{"HTTP2", func(_ *testing.T, app *credo.App) *httptest.Server {
			srv := httptest.NewUnstartedServer(app)
			srv.EnableHTTP2 = true
			srv.StartTLS()
			return srv
		}, 2},
	}

	for _, p := range protocols {
		for _, compress := range []bool{false, true} {
			name := p.name
			if compress {
				name += "/Compressed"
			}
			t.Run(name, func(t *testing.T) {
				app := mustNew(t)
				if compress {
					app.UseCompress()
				}
				var served atomic.Int64
				app.GET("/stream", func(ctx *credo.Context) error {
					err := ctx.Response().Stream(http.StatusOK, "text/plain", &patternReader{n: size})
					served.Store(ctx.Response().Size())
					return err
				})

				srv := p.start(t, app)
				defer srv.Close()

				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/stream", nil)
				if err != nil {
					t.Fatal(err)
				}
				if compress {
					req.Header.Set("Accept-Encoding", "gzip")
				}
				res, err := srv.Client().Do(req)
				if err != nil {
					t.Fatalf("GET /stream: %v", err)
				}
				defer res.Body.Close()
				if res.ProtoMajor != p.proto {
					t.Fatalf("ProtoMajor = %d, want %d", res.ProtoMajor, p.proto)
				}
				if res.StatusCode != http.StatusOK {
					t.Fatalf("status = %d, want 200", res.StatusCode)
				}

				var body io.Reader = res.Body
				if compress {
					if got := res.Header.Get("Content-Encoding"); got != "gzip" {
						t.Fatalf("Content-Encoding = %q, want gzip", got)
					}
					gz, err := gzip.NewReader(res.Body)
					if err != nil {
						t.Fatalf("gzip reader: %v", err)
					}
					defer gz.Close()
					body = gz
				}
				var buf bytes.Buffer
				if _, err := io.Copy(&buf, body); err != nil {
					t.Fatalf("read body: %v", err)
				}
				checkPattern(t, buf.Bytes(), size)
				if got := served.Load(); got != size {
					t.Fatalf("handler saw Size() = %d, want %d", got, size)
				}
			})
		}
	}
}
