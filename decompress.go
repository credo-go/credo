package credo

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/credo-go/credo/internal/httpheader"
)

// DefaultDecompressMaxBytes bounds the decompressed request body when
// [DecompressConfig.MaxBytes] is zero. It equals the server's default
// max_body_bytes (4 MiB); that server limit only bounds the compressed wire
// bytes, so the decompressed stream needs its own ceiling.
const DefaultDecompressMaxBytes int64 = 4 << 20

// DecompressConfig configures the request-body decompression feature
// installed by [App.UseDecompress]. The zero value selects every default.
type DecompressConfig struct {
	// Skipper excludes a request from decompression, leaving its body and
	// Content-Encoding untouched — the way to keep raw bodies for routes
	// such as signed webhooks. It is evaluated once, on the original request
	// as received (method, path and headers), before routing and before any
	// user middleware; route and authentication data are not available to
	// it, and a rewrite does not re-evaluate it. nil decompresses every
	// encoded request. A skipped encoded body is still rejected by BindBody
	// with 415.
	Skipper func(ctx *Context) bool

	// MaxBytes bounds the decompressed body in bytes. Reading past it fails
	// with the framework's 413 Request Entity Too Large classification, the
	// same way the server-wide max_body_bytes limit surfaces. Zero selects
	// [DefaultDecompressMaxBytes]; a negative value is a configuration error
	// and panics at registration.
	MaxBytes int64
}

// decompressFeature is the normalized decompression configuration.
type decompressFeature struct {
	skipper  func(*Context) bool
	maxBytes int64
}

// UseDecompress installs request-body decompression. Credo does not
// decompress request bodies by default: a Content-Encoding the application
// has not opted into is rejected by BindBody with 415 Unsupported Media Type.
// This feature is that opt-in. It understands gzip (also x-gzip) and deflate
// (zlib-wrapped per RFC 9110, with raw DEFLATE accepted for clients that omit
// the zlib header), all from the standard library.
//
// Decompression runs before Global middleware, so body-reading middleware and
// binders consume the same decoded stream. For a matching request it replaces
// the body with the decompressed stream bounded by MaxBytes (the server's
// max_body_bytes still bounds the compressed wire bytes independently), sets
// ContentLength to -1 (unknown), and removes the Content-Encoding header so
// downstream binding treats the body as plain. An unsupported coding or a
// coding list with more than one entry answers 415; a stream whose header is
// already corrupt answers 400 through the regular bind error pipeline; both
// are rendered without running any user middleware. Bodies declared empty
// pass through untouched.
//
// The feature is off by default. UseDecompress accepts zero configs for the
// defaults or one config; it panics for more than one config, for a negative
// MaxBytes, when called twice, or after the App was prepared or shut down.
func (app *App) UseDecompress(cfgs ...DecompressConfig) {
	cfg := oneConfig("App.UseDecompress", cfgs)
	if cfg.MaxBytes < 0 {
		panic(fmt.Sprintf("credo: App.UseDecompress: MaxBytes must be >= 0, got %d", cfg.MaxBytes))
	}
	f := &decompressFeature{skipper: cfg.Skipper, maxBytes: cfg.MaxBytes}
	if f.maxBytes == 0 {
		f.maxBytes = DefaultDecompressMaxBytes
	}
	app.installFeature("App.UseDecompress", func() {
		if app.decompress != nil {
			panic("credo: App.UseDecompress called twice")
		}
		app.decompress = f
	})
}

// apply selects decompression for the request once, on the original request,
// and installs the decoded body. A returned error is the response to send
// (415/400/413) instead of running the chain.
func (f *decompressFeature) apply(c *Context) error {
	if f.skipper != nil && f.skipper(c) {
		return nil
	}

	req := c.request
	coding := req.Header.Get("Content-Encoding")
	if coding == "" {
		return nil
	}

	open, ok := contentDecoder(coding)
	if !ok {
		if httpheader.IsIdentityContentCoding(coding) {
			return nil
		}
		return unsupportedContentEncoding(coding)
	}

	if req.Body == nil || req.Body == http.NoBody || req.ContentLength == 0 {
		// Nothing to decode: present an ordinary empty body downstream.
		req.Header.Del("Content-Encoding")
		return nil
	}

	decoded, err := open(req.Body)
	if err != nil {
		return decompressOpenError(err)
	}
	limited := http.MaxBytesReader(c.response.Unwrap(), decoded, f.maxBytes)

	req.Body = limited
	req.ContentLength = -1
	req.Header.Del("Content-Encoding")
	c.exec.body = limited
	return nil
}

// contentDecoder maps a single Content-Encoding token to its stream opener.
// Multi-coding lists ("gzip, br") are not supported and report false.
func contentDecoder(coding string) (func(io.ReadCloser) (io.ReadCloser, error), bool) {
	switch strings.ToLower(strings.TrimSpace(coding)) {
	case "gzip", "x-gzip":
		return openGzip, true
	case "deflate":
		return openDeflate, true
	}
	return nil, false
}

func unsupportedContentEncoding(coding string) error {
	return NewHTTPError(http.StatusUnsupportedMediaType, CodeUnsupportedContentEncoding).
		WithMessageKey("unsupported content encoding: " + coding)
}

// decompressOpenError classifies a failure to read the compressed stream's
// header: an exhausted server body limit keeps its 413 identity, an empty
// stream is an empty body, anything else is a syntax failure.
func decompressOpenError(err error) error {
	if mbe, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return NewHTTPError(http.StatusRequestEntityTooLarge).
			WithMessageKey("request body too large").
			WithInternal(mbe)
	}
	if errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return &BindError{Reason: BindReasonEmptyBody, Internal: err}
	}
	return &BindError{Reason: BindReasonSyntax, Internal: err}
}

// closeBoth closes the decoder and then the wrapped body.
type closeBoth struct {
	io.Reader
	decoder io.Closer
	body    io.Closer
}

func (c *closeBoth) Close() error {
	err := c.decoder.Close()
	if berr := c.body.Close(); err == nil {
		err = berr
	}
	return err
}

func openGzip(body io.ReadCloser) (io.ReadCloser, error) {
	gr, err := gzip.NewReader(body)
	if err != nil {
		return nil, err
	}
	// Reject a second gzip member that could follow the first; RFC 9110
	// clients send one. Multistream(false) makes the reader stop at the end
	// of the first member instead of silently concatenating.
	gr.Multistream(false)
	return &closeBoth{Reader: gr, decoder: gr, body: body}, nil
}

// openDeflate accepts RFC 9110 "deflate" (zlib-wrapped) and, for clients
// that send the bare DEFLATE stream Compress emits, raw DEFLATE. The two-byte
// zlib header (CM=8, FCHECK valid) decides.
func openDeflate(body io.ReadCloser) (io.ReadCloser, error) {
	br := bufio.NewReader(body)
	header, err := br.Peek(2)
	if err != nil {
		if errors.Is(err, io.EOF) && len(header) == 0 {
			return nil, io.EOF
		}
		if !errors.Is(err, io.EOF) {
			return nil, err
		}
	}
	if len(header) == 2 && header[0]&0x0f == 8 && (uint16(header[0])<<8|uint16(header[1]))%31 == 0 {
		zr, err := zlib.NewReader(br)
		if err != nil {
			return nil, err
		}
		return &closeBoth{Reader: zr, decoder: zr, body: body}, nil
	}
	fr := flate.NewReader(br)
	return &closeBoth{Reader: fr, decoder: fr, body: body}, nil
}
