// Originally derived from Echo (https://github.com/labstack/echo),
// Copyright (c) 2024 LabStack, MIT licensed. Substantially modified for Credo;
// see the NOTICES file for full attribution.

package credo

import (
	"bufio"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

const cacheControlNoCacheMustRevalidate = "no-cache, must-revalidate"

// Response wraps http.ResponseWriter to track the response status code,
// byte count, and whether headers have been committed.
//
// The embedded ResponseWriter may be swapped by middleware that wraps the
// writer (e.g., compression); the tracking state is framework-owned and
// exposed read-only via [Response.Status], [Response.Size], and
// [Response.Committed].
type Response struct {
	http.ResponseWriter

	// status is the HTTP status code written.
	status int

	// size is the number of bytes written to the response body.
	size int64

	// committed is true after WriteHeader has been called.
	committed bool
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

// WriteHeader sends an HTTP response header with the given status code.
// It can only be called once per response.
func (r *Response) WriteHeader(code int) {
	if r.committed {
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
	if !r.committed {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.size += int64(n)
	return n, err
}

// Flush sends any buffered data to the client.
func (r *Response) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack implements the http.Hijacker interface.
func (r *Response) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := r.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("credo: response does not implement http.Hijacker")
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
}

// String implements fmt.Stringer for debugging.
func (r *Response) String() string {
	return fmt.Sprintf("Response{Status:%d Size:%d Committed:%v}", r.status, r.size, r.committed)
}

// --- Response helpers (moved from context.go) ---

// JSON sends a JSON response with the given status code.
func (r *Response) JSON(code int, v any) error {
	r.Header().Set("Content-Type", "application/json; charset=utf-8")
	r.WriteHeader(code)
	return json.NewEncoder(r).Encode(v)
}

// Render sends a successful response through the app's [SuccessRenderer] when
// one is installed via [App.SetSuccessRenderer], letting an application apply a
// uniform response envelope at a single seam. With no renderer installed (the
// default), it falls back to plain JSON via [Response.JSON] and imposes no
// envelope.
//
// Render is the only success path that consults the renderer: the raw helpers
// ([Response.JSON], [Response.XML], [Response.Text], [Response.Blob], and the
// streaming writers) stay un-intercepted, so handlers serving webhooks, health
// probes, or third-party-dictated shapes can always bypass the envelope by
// calling them directly. A renderer error propagates to the caller (and thus
// the error pipeline) like any handler error.
func (c *Context) Render(status int, data any) error {
	if c.app != nil && c.app.successRenderer != nil {
		return c.app.successRenderer(c, status, data)
	}
	return c.response.JSON(status, data)
}

// Text sends a plain text response with the given status code.
// Named Text (not String) to avoid conflict with the fmt.Stringer interface.
func (r *Response) Text(code int, s string) error {
	r.Header().Set("Content-Type", "text/plain; charset=utf-8")
	r.WriteHeader(code)
	_, err := io.WriteString(r, s)
	return err
}

// HTML sends an HTML response with the given status code.
func (r *Response) HTML(code int, html string) error {
	r.Header().Set("Content-Type", "text/html; charset=utf-8")
	r.WriteHeader(code)
	_, err := io.WriteString(r, html)
	return err
}

// XML sends an XML response with the given status code.
func (r *Response) XML(code int, v any) error {
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
		return NewHTTPError(http.StatusInternalServerError, "invalid redirect status code")
	}
	r.Header().Set("Location", url)
	r.WriteHeader(code)
	return nil
}

// Blob sends a binary response with the given content type.
func (r *Response) Blob(code int, contentType string, b []byte) error {
	r.Header().Set("Content-Type", contentType)
	r.WriteHeader(code)
	_, err := r.Write(b)
	return err
}

// Stream sends a streaming response from the given reader.
func (r *Response) Stream(code int, contentType string, rd io.Reader) error {
	r.Header().Set("Content-Type", contentType)
	r.WriteHeader(code)
	_, err := io.Copy(r, rd)
	return err
}

// SetCookie adds a Set-Cookie header to the response.
func (r *Response) SetCookie(cookie *http.Cookie) {
	http.SetCookie(r, cookie)
}
