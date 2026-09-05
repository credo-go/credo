// Originally derived from Echo (https://github.com/labstack/echo),
// Copyright (c) 2024 LabStack, MIT licensed. Substantially modified for Credo;
// see the NOTICES file for full attribution.

package credo

import (
	"bufio"
	jsonv2 "encoding/json/v2"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/credo-go/credo/internal/httpwriter"
)

// copyBufferSize is the size of the pooled buffers [Response.ReadFrom] copies
// through when the underlying writer offers no [io.ReaderFrom]; it matches
// the buffer io.Copy would otherwise allocate per call.
const copyBufferSize = 32 << 10

// copyBufferPool holds the [Response.ReadFrom] copy buffers. Pointers are
// pooled so that Get and Put stay allocation-free.
var copyBufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, copyBufferSize)
		return &buf
	},
}

// writerOnly hides a writer's optional ReadFrom method from io.CopyBuffer so
// that the fallback copy cannot re-enter [Response.ReadFrom].
type writerOnly struct {
	io.Writer
}

const cacheControlNoCacheMustRevalidate = "no-cache, must-revalidate"

// Response wraps http.ResponseWriter to track the response status code,
// byte count, and whether headers have been committed.
//
// The embedded ResponseWriter may be swapped by middleware that wraps the
// writer (e.g., compression); the tracking state is framework-owned and
// exposed read-only via [Response.Status], [Response.Size], and
// [Response.Committed], and [Response.Hijacked].
type Response struct {
	http.ResponseWriter

	// status is the HTTP status code written.
	status int

	// size is the number of bytes written to the response body.
	size int64

	// committed is true after WriteHeader has been called.
	committed bool

	// hijacked is true only after the effective underlying Hijack call succeeds.
	hijacked bool

	// exemptJSON marks a framework-internal JSON write in progress
	// (Context.Render, the error pipeline), so it does not count as an
	// envelope bypass.
	exemptJSON bool

	// envelopeBypassed is true after a body-carrying JSON write outside
	// Context.Render; read by the debug-mode envelope-bypass diagnostic.
	envelopeBypassed bool

	// delegated wraps the source of a delegated ReadFrom (see
	// Response.delegate); it lives on the pooled Response so delegation
	// allocates nothing. R is cleared as soon as the delegation returns.
	delegated io.LimitedReader

	// app supplies the JSON encoding profile. Nil for a Response built with
	// NewResponse, which then uses the framework default profile.
	app *App
}

// NewResponse creates a new Response wrapping the given http.ResponseWriter.
func NewResponse(w http.ResponseWriter) *Response {
	return &Response{ResponseWriter: w}
}

// Status returns the HTTP status code written, or 0 when the response
// has not been committed yet.
func (r *Response) Status() int {
	return r.status
}

// Size returns the number of bytes written to the response body.
func (r *Response) Size() int64 {
	return r.size
}

// Committed reports whether the response header has been written.
// Once committed, the status code and headers can no longer change.
func (r *Response) Committed() bool {
	return r.committed
}

// Hijacked reports whether the underlying HTTP connection was successfully
// taken over through [Response.Hijack]. Writing status or Upgrade headers alone
// does not mark the response as hijacked. Middleware that bypasses Response and
// hijacks a raw underlying writer cannot be observed by this state.
func (r *Response) Hijacked() bool {
	return r.hijacked
}

// WriteHeader sends an HTTP response header with the given status code.
// It can only be called once per response.
func (r *Response) WriteHeader(code int) {
	if r.committed || r.hijacked {
		return
	}
	if code >= http.StatusBadRequest && hasCacheControlDirective(r.Header().Get("Cache-Control"), "immutable") {
		r.Header().Set("Cache-Control", cacheControlNoCacheMustRevalidate)
	}
	r.status = code
	r.committed = true
	r.ResponseWriter.WriteHeader(code)
}

func hasCacheControlDirective(value, directive string) bool {
	for part := range strings.SplitSeq(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), directive) {
			return true
		}
	}
	return false
}

// Write writes the data to the connection as part of an HTTP reply.
// If WriteHeader has not been called, it calls WriteHeader(200).
func (r *Response) Write(b []byte) (int, error) {
	if r.hijacked {
		return 0, http.ErrHijacked
	}
	if !r.committed {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.size += int64(n)
	return n, err
}

// WriteString writes the string to the connection as part of an HTTP reply,
// implementing [io.StringWriter]. When the underlying ResponseWriter also
// implements it (net/http's does), the string is written without the
// []byte(s) copy that io.WriteString would otherwise allocate per call.
// If WriteHeader has not been called, it calls WriteHeader(200).
func (r *Response) WriteString(s string) (int, error) {
	if r.hijacked {
		return 0, http.ErrHijacked
	}
	if !r.committed {
		r.WriteHeader(http.StatusOK)
	}
	n, err := io.WriteString(r.ResponseWriter, s)
	r.size += int64(n)
	return n, err
}

// ReadFrom copies src into the response body until EOF, implementing
// [io.ReaderFrom] for [io.Copy] and [Response.Stream]. When the underlying
// writer implements io.ReaderFrom (net/http's HTTP/1.1 writer does, copying
// through a pooled buffer and using sendfile or splice on a plaintext TCP
// connection when src is a regular file or a socket) the copy is delegated to
// it. Otherwise — a compressing or other wrapping writer, HTTP/2 — the copy
// runs through a pooled 32 KiB buffer, so a Reader-only source never costs a
// buffer allocation per response.
//
// The bytes the writer accepted count toward [Response.Size]; a hijacked
// response returns [http.ErrHijacked]. The commit follows [Response.Write]:
// nothing is committed before the first byte, so a source that fails before
// producing any output leaves the response uncommitted and the handler's
// error still renders as an error response; the first byte commits 200 when
// no status was written. On an uncommitted response the first buffer's worth
// of src therefore always goes through [Response.Write] before the copy is
// delegated (as net/http itself does before switching to sendfile): the
// commit is recorded here, ahead of any later failure in src, so a source
// that panics after its first bytes leaves a committed response whose status
// and access record are the 200 already on the wire. The delegated part is
// counted the same way: the bytes the writer read from src before a panic it
// also wrote, so [Response.Size] still reports what the client received.
func (r *Response) ReadFrom(src io.Reader) (int64, error) {
	if r.hijacked {
		return 0, http.ErrHijacked
	}
	rf, ok := r.ResponseWriter.(io.ReaderFrom)
	if !ok {
		return r.copyPooled(src)
	}
	var n int64
	if !r.committed {
		n0, err := r.copyPooled(io.LimitReader(src, copyBufferSize))
		n += n0
		if err != nil || n0 < copyBufferSize {
			// Failed, or src ended within the first buffer.
			return n, err
		}
	}
	n1, err := r.delegate(rf, src)
	return n + n1, err
}

// delegate hands src to the writer's own ReadFrom and counts what it
// accepted. The source is wrapped in an io.LimitedReader rather than a private
// type: net's sendfile and splice paths unwrap exactly that type and keep the
// zero-copy path for a file or socket source, and its N records how much the
// writer consumed — a Read that panics returns nothing, so what was consumed
// was also written and is counted even when the panic skips the return. Kept
// out of ReadFrom so the deferred accounting costs nothing on the pooled path.
func (r *Response) delegate(rf io.ReaderFrom, src io.Reader) (n int64, err error) {
	lr := &r.delegated
	lr.R, lr.N = src, math.MaxInt64
	returned := false
	defer func() {
		if !returned {
			r.size += math.MaxInt64 - lr.N
		}
		lr.R = nil
	}()
	n, err = rf.ReadFrom(lr)
	returned = true
	r.size += n
	return n, err
}

// copyPooled copies src into the response through a pooled buffer. The
// destination is r itself (with ReadFrom hidden) rather than the bare writer:
// a src implementing io.WriterTo writes straight into the destination, and
// every path must pass through Response.Write so the commit and the byte
// count stay exact.
func (r *Response) copyPooled(src io.Reader) (int64, error) {
	bufp := copyBufferPool.Get().(*[]byte)
	n, err := io.CopyBuffer(writerOnly{r}, src, *bufp)
	copyBufferPool.Put(bufp)
	return n, err
}

// Flush sends any buffered data to the client.
func (r *Response) Flush() {
	if r.hijacked {
		return
	}
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack implements the http.Hijacker interface. It resolves nested Unwrap
// chains and marks the response only after the effective Hijack call succeeds.
func (r *Response) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := httpwriter.Hijack(r.ResponseWriter)
	if err != nil {
		return nil, nil, err
	}
	r.hijacked = true
	return conn, rw, nil
}

// Unwrap returns the underlying http.ResponseWriter.
func (r *Response) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// Reset resets the response for reuse from the pool.
func (r *Response) Reset(w http.ResponseWriter) {
	r.ResponseWriter = w
	r.status = 0
	r.size = 0
	r.committed = false
	r.hijacked = false
	r.exemptJSON = false
	r.envelopeBypassed = false
	r.delegated = io.LimitedReader{}
}

// String implements fmt.Stringer for debugging.
func (r *Response) String() string {
	return fmt.Sprintf("Response{Status:%d Size:%d Committed:%v}", r.status, r.size, r.committed)
}

// --- Response helpers (moved from context.go) ---

// bodilessStatus reports whether the status code forbids a response body per
// RFC 9110 (the inverse of net/http's bodyAllowedForStatus): 1xx
// informational, 204 No Content, and 304 Not Modified.
func bodilessStatus(code int) bool {
	return (code >= 100 && code <= 199) ||
		code == http.StatusNoContent ||
		code == http.StatusNotModified
}

// JSON sends a JSON response with the given status code, encoded with the
// application's JSON profile (see [WithJSONOptions]): deterministic map
// ordering, nanosecond durations, v2 defaults for everything else — nil
// slices and maps as [] and {}, no HTML escaping, no trailing newline.
//
// For status codes that forbid a response body (1xx, 204, 304), JSON — like
// every body-writing helper — skips both the body and the Content-Type header
// and writes the status line only, exactly as [Response.NoContent] would.
// Writing a body to such a status would otherwise fail inside net/http
// ("http: request method or response status code does not allow body") after
// the header is already committed, surfacing as a spurious error the pipeline
// can no longer render.
func (r *Response) JSON(code int, v any) error {
	if bodilessStatus(code) {
		r.WriteHeader(code)
		return nil
	}
	if !r.exemptJSON {
		// A body-carrying JSON write outside Context.Render; feeds the
		// debug-mode envelope-bypass diagnostic (see MetaRawResponse).
		r.envelopeBypassed = true
	}
	r.Header().Set("Content-Type", "application/json; charset=utf-8")
	r.WriteHeader(code)
	return jsonv2.MarshalWrite(r, v, r.app.jsonOptions())
}

// RenderOption attaches an optional envelope side channel — a message key or
// metadata — to a [Context.Render] call; see [RenderMessageKey] and
// [RenderMeta]. (Named Render*, not With*: the With prefix is reserved for
// construction-time App options.)
type RenderOption func(*RenderInfo)

// RenderMessageKey attaches an i18n message key to the [RenderInfo] a
// [SuccessRenderer] receives. With no renderer installed it has no effect —
// the side channel only exists for an envelope to consume.
func RenderMessageKey(key string) RenderOption {
	return func(info *RenderInfo) { info.MessageKey = key }
}

// RenderMeta attaches structured metadata (pagination, request echo, …) to
// the [RenderInfo] a [SuccessRenderer] receives. With no renderer installed
// it has no effect — the side channel only exists for an envelope to consume.
func RenderMeta(v any) RenderOption {
	return func(info *RenderInfo) { info.Meta = v }
}

// Render sends a successful response through the app's [SuccessRenderer] when
// one is installed via [App.UseSuccessRenderer], letting an application apply
// a uniform response envelope at a single seam. With no renderer installed
// (the default), it writes data as plain JSON via [Response.JSON], imposes no
// envelope, and any [RenderOption] side channels are dropped.
//
// With a renderer installed, the renderer returns the body shape and the
// framework writes it with status and the application JSON profile; a nil
// return writes data plain, and a renderer that committed the response itself
// keeps full control (see [SuccessRenderer] for the contract). Body-forbidding
// statuses (1xx, 204, 304) always render status-only.
//
// Render is the only success path that consults the renderer: the raw helpers
// ([Response.JSON], [Response.XML], [Response.Text], [Response.Blob], and the
// streaming writers) stay un-intercepted, so handlers serving webhooks, health
// probes, or third-party-dictated shapes can always bypass the envelope by
// calling them directly.
func (c *Context) Render(status int, data any, opts ...RenderOption) error {
	// Every write below is the envelope seam itself, never a bypass of it.
	c.response.exemptJSON = true
	defer func() { c.response.exemptJSON = false }()
	if c.app == nil || c.app.successRenderer == nil {
		return c.response.JSON(status, data)
	}
	info := RenderInfo{Status: status, Data: data}
	for _, opt := range opts {
		opt(&info)
	}
	body := c.app.successRenderer(c, info)
	if c.response.Hijacked() || c.response.Committed() {
		// The renderer took full control and wrote the response itself;
		// the returned body, if any, is irrelevant by contract.
		return nil
	}
	if body == nil {
		return c.response.JSON(info.Status, info.Data)
	}
	return c.response.JSON(info.Status, body)
}

// Text sends a plain text response with the given status code.
// Named Text (not String) to avoid conflict with the fmt.Stringer interface.
// Body-forbidding status codes (1xx, 204, 304) write the status only; see
// [Response.JSON].
func (r *Response) Text(code int, s string) error {
	if bodilessStatus(code) {
		r.WriteHeader(code)
		return nil
	}
	r.Header().Set("Content-Type", "text/plain; charset=utf-8")
	r.WriteHeader(code)
	_, err := io.WriteString(r, s)
	return err
}

// HTML sends an HTML response with the given status code.
// Body-forbidding status codes (1xx, 204, 304) write the status only; see
// [Response.JSON].
func (r *Response) HTML(code int, html string) error {
	if bodilessStatus(code) {
		r.WriteHeader(code)
		return nil
	}
	r.Header().Set("Content-Type", "text/html; charset=utf-8")
	r.WriteHeader(code)
	_, err := io.WriteString(r, html)
	return err
}

// XML sends an XML response with the given status code.
// Body-forbidding status codes (1xx, 204, 304) write the status only; see
// [Response.JSON].
func (r *Response) XML(code int, v any) error {
	if bodilessStatus(code) {
		r.WriteHeader(code)
		return nil
	}
	r.Header().Set("Content-Type", "application/xml; charset=utf-8")
	r.WriteHeader(code)
	return xml.NewEncoder(r).Encode(v)
}

// NoContent sends a response with no body.
func (r *Response) NoContent(code int) error {
	r.WriteHeader(code)
	return nil
}

// Redirect sends an HTTP redirect response.
func (r *Response) Redirect(code int, url string) error {
	if code < 300 || code > 308 {
		return NewHTTPError(http.StatusInternalServerError).
			WithMessageKey("invalid redirect status code")
	}
	r.Header().Set("Location", url)
	r.WriteHeader(code)
	return nil
}

// Blob sends a binary response with the given content type.
// Body-forbidding status codes (1xx, 204, 304) write the status only; see
// [Response.JSON].
func (r *Response) Blob(code int, contentType string, b []byte) error {
	if bodilessStatus(code) {
		r.WriteHeader(code)
		return nil
	}
	r.Header().Set("Content-Type", contentType)
	r.WriteHeader(code)
	_, err := r.Write(b)
	return err
}

// Stream sends a streaming response from the given reader. A reader that
// implements [io.WriterTo] writes itself; any other reader is copied through
// [Response.ReadFrom], which copies through a pooled buffer and delegates the
// remainder of a large source to the underlying writer instead of allocating
// a buffer per call.
// Body-forbidding status codes (1xx, 204, 304) write the status only and
// never read from rd; see [Response.JSON].
func (r *Response) Stream(code int, contentType string, rd io.Reader) error {
	if bodilessStatus(code) {
		r.WriteHeader(code)
		return nil
	}
	r.Header().Set("Content-Type", contentType)
	r.WriteHeader(code)
	_, err := io.Copy(r, rd)
	return err
}

// SetCookie adds a Set-Cookie header to the response.
func (r *Response) SetCookie(cookie *http.Cookie) {
	http.SetCookie(r, cookie)
}
