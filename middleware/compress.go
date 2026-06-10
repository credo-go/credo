// Copyright (c) 2015-present Peter Kieltyka (https://github.com/pkieltyka), Google Inc.
// Originally derived from github.com/go-chi/chi/middleware (MIT License).

package middleware

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/internal/httpheader"
)

// Writer pools keyed by compression level. Default level pools are pre-created.
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

// CompressConfig defines configuration for Compress middleware.
type CompressConfig struct {
	// Skipper defines a function to skip middleware.
	Skipper Skipper

	// Level is the gzip/deflate compression level (1–9). The zero value
	// selects the default (5); level 0 (NoCompression) cannot be requested
	// through this middleware — omit the middleware or use Skipper to skip
	// compression entirely.
	Level int

	// Types limits compression to specific content types.
	// Supports exact values ("application/json") and wildcards ("text/*").
	// Default: common textual MIME types.
	Types []string
}

// DefaultCompressConfig returns the default Compress middleware config.
// Each call returns a fresh value, so callers cannot mutate the
// package-wide defaults.
func DefaultCompressConfig() CompressConfig {
	return CompressConfig{
		Skipper: DefaultSkipper,
		Level:   5,
	}
}

// Compress returns response compression middleware.
func Compress(cfg ...CompressConfig) credo.Middleware {
	config := resolveConfig(cfg, DefaultCompressConfig(), normalizeCompressConfig)

	contentTypes := config.Types
	if len(contentTypes) == 0 {
		contentTypes = defaultCompressibleContentTypes
	}

	exactTypes, wildcardTypes := buildCompressibleTypes(contentTypes)

	return func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			if config.Skipper(ctx) {
				return next(ctx)
			}

			encoding := selectCompressionEncoding(ctx.Request().Request.Header.Get("Accept-Encoding"))
			if encoding == "" {
				return next(ctx)
			}

			origWriter := ctx.Response().ResponseWriter
			cw := acquireCompressResponseWriter(origWriter, encoding, config.Level, exactTypes, wildcardTypes)
			ctx.Response().ResponseWriter = cw
			defer func() {
				_ = cw.Close()
				ctx.Response().ResponseWriter = origWriter
				releaseCompressResponseWriter(cw)
			}()

			return next(ctx)
		}
	}
}

func normalizeCompressConfig(config CompressConfig) CompressConfig {
	defaults := DefaultCompressConfig()
	if config.Skipper == nil {
		config.Skipper = defaults.Skipper
	}
	if config.Level == 0 {
		config.Level = defaults.Level
	}
	config.Types = append([]string(nil), config.Types...)
	return config
}

type compressResponseWriter struct {
	http.ResponseWriter

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
		if compressor, err := newCompressor(w.encoding, w.level, w.ResponseWriter); err == nil {
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

	return w.ResponseWriter.Write(p)
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
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("credo/middleware: http.Hijacker is unavailable")
	}
	return hj.Hijack()
}

func (w *compressResponseWriter) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return errors.New("credo/middleware: http.Pusher is unavailable")
}

func (w *compressResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *compressResponseWriter) Close() error {
	if !w.enabled || w.compressor == nil {
		return nil
	}
	err := w.compressor.Close()
	// Return the compressor to its pool.
	switch c := w.compressor.(type) {
	case *gzip.Writer:
		getGzipPool(w.level).Put(c)
	case *flate.Writer:
		getFlatePool(w.level).Put(c)
	}
	w.compressor = nil
	return err
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

	for _, item := range strings.Split(acceptEncoding, ",") {
		name, q, ok := parseEncodingToken(item)
		if !ok {
			continue
		}

		switch name {
		case "gzip":
			if q > gzipQ {
				gzipQ = q
			}
		case "deflate":
			if q > deflateQ {
				deflateQ = q
			}
		case "*":
			if q > wildcardQ {
				wildcardQ = q
			}
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
		for _, part := range strings.Split(token[i+1:], ";") {
			part = strings.TrimSpace(part)
			if !strings.HasPrefix(part, "q=") {
				continue
			}

			value, err := strconv.ParseFloat(strings.TrimSpace(part[2:]), 64)
			if err != nil {
				return "", 0, false
			}
			if value < 0 {
				value = 0
			}
			if value > 1 {
				value = 1
			}
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
