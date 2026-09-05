// Copyright (c) 2015-present Peter Kieltyka (https://github.com/pkieltyka), Google Inc.
// Originally derived from github.com/go-chi/chi/middleware (MIT License).

package credo

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/credo-go/credo/internal/httpheader"
	"github.com/credo-go/credo/internal/httpwriter"
)

// Writer pools keyed by compression level.
var (
	gzipWriterPools  = sync.Map{} // map[int]*sync.Pool
	flateWriterPools = sync.Map{} // map[int]*sync.Pool

	compressWriterPool = sync.Pool{}
)

var defaultCompressibleContentTypes = []string{
	"text/html",
	"text/css",
	"text/plain",
	"text/javascript",
	"application/javascript",
	"application/x-javascript",
	"application/json",
	"application/atom+xml",
	"application/rss+xml",
	"image/svg+xml",
}

// CompressConfig configures the response compression feature installed by
// [App.UseCompress]. The zero value selects every default.
type CompressConfig struct {
	// Skipper excludes a request from compression. It is evaluated once, on
	// the original request as received (method, path and headers), before
	// routing and before any user middleware; route data and the response
	// are not available to it. A rewrite does not re-evaluate it. nil
	// compresses every eligible request.
	Skipper func(ctx *Context) bool

	// Level is the gzip/deflate compression level, 1–9. Zero selects 5.
	// Level 0 (no compression) cannot be requested: leave the feature out or
	// use Skipper.
	Level int

	// Types limits compression to specific response content types. Exact
	// values ("application/json") and wildcards ("text/*") are supported.
	// Default: common textual MIME types.
	Types []string
}

// compressFeature is the normalized compression configuration.
type compressFeature struct {
	skipper       func(*Context) bool
	level         int
	exactTypes    map[string]struct{}
	wildcardTypes map[string]struct{}
}

// UseCompress installs response compression. For a request whose
// Accept-Encoding negotiates gzip or deflate, the response writer is wrapped
// before any user middleware runs and the wrapper stays in place through
// centralized error rendering, so error envelopes are compressed like handler
// output. Compression is applied only to responses whose Content-Type is in
// Types and that carry no Content-Encoding of their own; HEAD and bodiless
// responses, streaming (Flush), committed responses and hijacked connections
// keep their behavior. The compressor is finalized at the framework's
// response-completion boundary, before the access record observes the
// response, so access-log bytes count the compressed output the transport
// accepted.
//
// The feature is off by default. UseCompress accepts zero configs for the
// defaults or one config; it panics for more than one config, for a Level
// outside 1–9, when called twice, or after the App was prepared or shut down.
func (app *App) UseCompress(cfgs ...CompressConfig) {
	cfg := oneConfig("App.UseCompress", cfgs)
	f := &compressFeature{skipper: cfg.Skipper, level: cfg.Level}
	if f.level == 0 {
		f.level = 5
	}
	if f.level < gzip.BestSpeed || f.level > gzip.BestCompression {
		panic(fmt.Sprintf("credo: App.UseCompress: Level %d outside 1–9", cfg.Level))
	}
	types := cfg.Types
	if len(types) == 0 {
		types = defaultCompressibleContentTypes
	}
	f.exactTypes, f.wildcardTypes = buildCompressibleTypes(types)
	app.installFeature("App.UseCompress", func() {
		if app.compress != nil {
			panic("credo: App.UseCompress called twice")
		}
		app.compress = f
	})
}

// apply selects compression for the request and installs the writer. The
// selection happens once, on the original request.
func (f *compressFeature) apply(c *Context) {
	if f.skipper != nil && f.skipper(c) {
		return
	}
	encoding := selectCompressionEncoding(c.request.Header.Get("Accept-Encoding"))
	if encoding == "" {
		return
	}
	cw := acquireCompressResponseWriter(c.response.ResponseWriter, encoding, f.level, f.exactTypes, f.wildcardTypes)
	c.response.ResponseWriter = cw
	c.exec.compress = cw
}

// outputCounter is the compressor's destination: it forwards to the
// underlying writer and counts the bytes that writer accepted. Every byte
// leaving compressResponseWriter passes through it, compressed or not, so
// out.n is the output-boundary byte count the access record reports.
type outputCounter struct {
	w http.ResponseWriter
	n int64
}

func (o *outputCounter) Write(p []byte) (int, error) {
	n, err := o.w.Write(p)
	o.n += int64(n)
	return n, err
}

type compressResponseWriter struct {
	http.ResponseWriter

	out        outputCounter
	compressor io.WriteCloser
	encoding   string
	level      int

	exactTypes    map[string]struct{}
	wildcardTypes map[string]struct{}

	wroteHeader bool
	enabled     bool
}

func acquireCompressResponseWriter(
	w http.ResponseWriter,
	encoding string,
	level int,
	exactTypes map[string]struct{},
	wildcardTypes map[string]struct{},
) *compressResponseWriter {
	cw, ok := compressWriterPool.Get().(*compressResponseWriter)
	if !ok {
		cw = &compressResponseWriter{}
	}
	cw.ResponseWriter = w
	cw.out = outputCounter{w: w}
	cw.encoding = encoding
	cw.level = level
	cw.exactTypes = exactTypes
	cw.wildcardTypes = wildcardTypes
	cw.wroteHeader = false
	cw.enabled = false
	cw.compressor = nil
	return cw
}

func releaseCompressResponseWriter(cw *compressResponseWriter) {
	cw.ResponseWriter = nil
	cw.out = outputCounter{}
	cw.compressor = nil
	cw.exactTypes = nil
	cw.wildcardTypes = nil
	compressWriterPool.Put(cw)
}

func (w *compressResponseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true

	headers := w.Header()
	if headers.Get("Content-Encoding") == "" && w.isCompressible(headers.Get("Content-Type")) {
		if compressor, err := newCompressor(w.encoding, w.level, &w.out); err == nil {
			w.compressor = compressor
			w.enabled = true
			headers.Set("Content-Encoding", w.encoding)
			httpheader.AddToken(headers, "Vary", "Accept-Encoding")
			headers.Del("Content-Length")
		}
	}

	w.ResponseWriter.WriteHeader(code)
}

func (w *compressResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}

	if w.enabled {
		return w.compressor.Write(p)
	}

	return w.out.Write(p)
}

func (w *compressResponseWriter) Flush() {
	if w.enabled {
		if fw, ok := w.compressor.(interface{ Flush() error }); ok {
			_ = fw.Flush()
		}
	}

	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *compressResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return httpwriter.Hijack(w.ResponseWriter)
}

func (w *compressResponseWriter) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return errors.New("credo: http.Pusher is unavailable")
}

func (w *compressResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// Close finalizes the compressed stream, writing the trailer, and returns
// the compressor to its pool. It reports the finalization error: the caller
// decides whether the transfer must be aborted.
func (w *compressResponseWriter) Close() error {
	if !w.enabled || w.compressor == nil {
		return nil
	}
	err := w.compressor.Close()
	w.recycleCompressor()
	return err
}

// abandon discards the compressor without writing anything further — used
// once the connection was hijacked and no longer belongs to the response.
func (w *compressResponseWriter) abandon() {
	if w.compressor == nil {
		return
	}
	w.recycleCompressor()
}

func (w *compressResponseWriter) recycleCompressor() {
	switch c := w.compressor.(type) {
	case *gzip.Writer:
		c.Reset(io.Discard)
		getGzipPool(w.level).Put(c)
	case *flate.Writer:
		c.Reset(io.Discard)
		getFlatePool(w.level).Put(c)
	}
	w.compressor = nil
}

func (w *compressResponseWriter) isCompressible(contentType string) bool {
	if contentType == "" {
		return false
	}

	contentType, _, _ = strings.Cut(contentType, ";")
	contentType = strings.TrimSpace(strings.ToLower(contentType))

	if _, ok := w.exactTypes[contentType]; ok {
		return true
	}

	if mediaType, _, found := strings.Cut(contentType, "/"); found {
		_, ok := w.wildcardTypes[mediaType]
		return ok
	}

	return false
}

func buildCompressibleTypes(types []string) (map[string]struct{}, map[string]struct{}) {
	exact := make(map[string]struct{}, len(types))
	wildcards := make(map[string]struct{})

	for _, t := range types {
		t = strings.TrimSpace(strings.ToLower(t))
		if t == "" {
			continue
		}

		if prefix, ok := strings.CutSuffix(t, "/*"); ok {
			if prefix != "" {
				wildcards[prefix] = struct{}{}
			}
			continue
		}

		exact[t] = struct{}{}
	}

	return exact, wildcards
}

func selectCompressionEncoding(acceptEncoding string) string {
	if acceptEncoding == "" {
		return ""
	}

	gzipQ := -1.0
	deflateQ := -1.0
	wildcardQ := -1.0

	for item := range strings.SplitSeq(acceptEncoding, ",") {
		name, q, ok := parseEncodingToken(item)
		if !ok {
			continue
		}

		switch name {
		case "gzip":
			gzipQ = max(gzipQ, q)
		case "deflate":
			deflateQ = max(deflateQ, q)
		case "*":
			wildcardQ = max(wildcardQ, q)
		}
	}

	// Wildcard applies only when a specific encoding wasn't explicitly listed.
	if gzipQ < 0 {
		gzipQ = wildcardQ
	}
	if deflateQ < 0 {
		deflateQ = wildcardQ
	}

	if gzipQ <= 0 && deflateQ <= 0 {
		return ""
	}
	if gzipQ >= deflateQ {
		return "gzip"
	}
	return "deflate"
}

func parseEncodingToken(token string) (name string, q float64, ok bool) {
	q = 1.0
	token = strings.TrimSpace(strings.ToLower(token))
	if token == "" {
		return "", 0, false
	}

	name = token
	if i := strings.IndexByte(token, ';'); i >= 0 {
		name = strings.TrimSpace(token[:i])
		for part := range strings.SplitSeq(token[i+1:], ";") {
			part = strings.TrimSpace(part)
			if !strings.HasPrefix(part, "q=") {
				continue
			}

			value, err := strconv.ParseFloat(strings.TrimSpace(part[2:]), 64)
			if err != nil {
				return "", 0, false
			}
			value = min(max(value, 0), 1)
			q = value
			break
		}
	}

	if name == "" {
		return "", 0, false
	}

	return name, q, true
}

func getGzipPool(level int) *sync.Pool {
	if p, ok := gzipWriterPools.Load(level); ok {
		return p.(*sync.Pool)
	}
	p := &sync.Pool{New: func() any {
		w, _ := gzip.NewWriterLevel(io.Discard, level)
		return w
	}}
	actual, _ := gzipWriterPools.LoadOrStore(level, p)
	return actual.(*sync.Pool)
}

func getFlatePool(level int) *sync.Pool {
	if p, ok := flateWriterPools.Load(level); ok {
		return p.(*sync.Pool)
	}
	p := &sync.Pool{New: func() any {
		w, _ := flate.NewWriter(io.Discard, level)
		return w
	}}
	actual, _ := flateWriterPools.LoadOrStore(level, p)
	return actual.(*sync.Pool)
}

func newCompressor(encoding string, level int, w io.Writer) (io.WriteCloser, error) {
	switch encoding {
	case "gzip":
		gw := getGzipPool(level).Get().(*gzip.Writer)
		gw.Reset(w)
		return gw, nil
	case "deflate":
		fw := getFlatePool(level).Get().(*flate.Writer)
		fw.Reset(w)
		return fw, nil
	default:
		return nil, errors.New("unsupported compression encoding")
	}
}
