package credo

import (
	"bufio"
	"errors"
	"io"
	"math"
	"net"
	"net/http"

	"github.com/credo-go/credo/internal/httpwriter"
)

// stdMiddlewareWriter is the http.ResponseWriter [WrapStdMiddleware] hands to
// a stdlib middleware. It forwards to the writer the response had when the
// middleware was entered and records what passes: the status, the body bytes
// and a successful hijack. [Response] sees only what is written through it, so
// without the record a response the middleware writes by itself — a 401, a
// redirect, a file — would leave it uncommitted with status 0.
//
// The record follows net/http: a 1xx status other than 101 is informational
// and commits nothing, a body write or a flush commits 200 when no status was
// written, and only the first status counts. Once the
// middleware hijacked the connection nothing more is forwarded or recorded.
type stdMiddlewareWriter struct {
	http.ResponseWriter

	status    int
	size      int64
	committed bool
	hijacked  bool
}

// WriteHeader forwards the status and records the first final one.
func (w *stdMiddlewareWriter) WriteHeader(code int) {
	if w.hijacked {
		return
	}
	w.ResponseWriter.WriteHeader(code)
	// Recorded after the call: net/http panics on an invalid code and has
	// committed nothing then.
	if !w.committed && !informationalStatus(code) {
		w.status, w.committed = code, true
	}
}

// Write forwards the bytes and counts the ones the writer accepted.
func (w *stdMiddlewareWriter) Write(b []byte) (int, error) {
	if w.hijacked {
		return 0, http.ErrHijacked
	}
	n, err := w.ResponseWriter.Write(b)
	w.wrote(n, err)
	return n, err
}

// WriteString implements [io.StringWriter], so that a string the chain writes
// through [Response.WriteString] still reaches the underlying writer without
// a []byte copy.
func (w *stdMiddlewareWriter) WriteString(s string) (int, error) {
	if w.hijacked {
		return 0, http.ErrHijacked
	}
	n, err := io.WriteString(w.ResponseWriter, s)
	w.wrote(n, err)
	return n, err
}

// wrote records a body write. As in net/http the first one commits 200 when
// no status was written. A write the connection refused because it had been
// hijacked past this writer is no part of a response.
func (w *stdMiddlewareWriter) wrote(n int, err error) {
	if errors.Is(err, http.ErrHijacked) {
		return
	}
	if !w.committed {
		w.status, w.committed = http.StatusOK, true
	}
	w.size += int64(n)
}

// ReadFrom copies src to the response the way [Response.ReadFrom] does. The
// underlying writer's own ReadFrom takes the copy when it has one, which keeps
// sendfile for a file served behind an adapted middleware; a pooled buffer
// does otherwise. An uncommitted response sends its first buffer through
// Write, so the commit is recorded before the copy is delegated.
func (w *stdMiddlewareWriter) ReadFrom(src io.Reader) (int64, error) {
	if w.hijacked {
		return 0, http.ErrHijacked
	}
	rf, ok := w.ResponseWriter.(io.ReaderFrom)
	if !ok {
		return w.copyPooled(src)
	}
	var n int64
	if !w.committed {
		n0, err := w.copyPooled(io.LimitReader(src, copyBufferSize))
		n += n0
		if err != nil || n0 < copyBufferSize {
			// Failed, or src ended within the first buffer.
			return n, err
		}
	}
	n1, err := w.delegate(rf, src)
	return n + n1, err
}

// delegate hands src to the underlying writer's ReadFrom and counts what it
// accepted. As in [Response.delegate] the count survives a Read of src that
// panics and so skips the return: what the writer had read by then it had also
// written, and an io.LimitedReader records how much that was. A source that
// already is one — the one Response passes down — is forwarded as it is,
// because net's sendfile and splice paths unwrap exactly one.
func (w *stdMiddlewareWriter) delegate(rf io.ReaderFrom, src io.Reader) (n int64, err error) {
	lr, ok := src.(*io.LimitedReader)
	if !ok {
		lr = &io.LimitedReader{R: src, N: math.MaxInt64}
	}
	before := lr.N
	returned := false
	defer func() {
		if !returned {
			w.size += before - lr.N
		}
	}()
	n, err = rf.ReadFrom(lr)
	returned = true
	w.size += n
	return n, err
}

// copyPooled copies src through Write with a pooled buffer, so the commit and
// every byte are recorded as they go out.
func (w *stdMiddlewareWriter) copyPooled(src io.Reader) (int64, error) {
	bufp := copyBufferPool.Get().(*[]byte)
	n, err := io.CopyBuffer(writerOnly{w}, src, *bufp)
	copyBufferPool.Put(bufp)
	return n, err
}

// FlushError flushes the underlying writer and reports whether it could.
// [http.ResponseController] prefers this method to Flush, and resolving the
// underlying writer through a controller reaches a Flusher behind an Unwrap
// chain as well. A flush that reached a writer commits 200 when no status was
// written, as [Response.FlushError] records it.
func (w *stdMiddlewareWriter) FlushError() error {
	if w.hijacked {
		return http.ErrHijacked
	}
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if !w.committed && !errors.Is(err, http.ErrNotSupported) {
		w.status, w.committed = http.StatusOK, true
	}
	return err
}

// Flush implements [http.Flusher].
func (w *stdMiddlewareWriter) Flush() {
	_ = w.FlushError()
}

// Hijack takes over the connection behind the underlying writer, resolving
// Unwrap chains as [Response.Hijack] does, and records the hijack only when it
// succeeded.
func (w *stdMiddlewareWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := httpwriter.Hijack(w.ResponseWriter)
	if err != nil {
		return nil, nil, err
	}
	w.hijacked = true
	return conn, rw, nil
}

// Push forwards HTTP/2 server push to the underlying writer.
func (w *stdMiddlewareWriter) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return http.ErrNotSupported
}

// Unwrap returns the underlying writer; [http.ResponseController] reaches the
// connection's deadlines and full-duplex switch through it.
func (w *stdMiddlewareWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// adopt takes over what a stdlib middleware wrote to w without going through
// r. A response that is still uncommitted takes the recorded status, size and
// commit. A response the chain wrote through r stays as r counted it: those
// writes passed w as well. A hijack the middleware performed is taken over
// either way. Nothing is taken once r itself was hijacked, because what w saw
// after that reached no response.
func (r *Response) adopt(w *stdMiddlewareWriter) {
	if r.hijacked {
		return
	}
	if !r.committed && w.committed {
		r.status, r.size, r.committed = w.status, w.size, true
	}
	if w.hijacked {
		r.hijacked = true
	}
}
