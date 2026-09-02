package middleware

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/credo-go/credo"
)

// DefaultDecompressMaxBytes bounds the decompressed request body when
// [DecompressConfig.MaxBytes] is zero. It equals the server's default
// max_body_bytes (4 MiB); that server limit only bounds the compressed wire
// bytes, so the decompressed stream needs its own ceiling.
const DefaultDecompressMaxBytes int64 = 4 << 20

// DecompressConfig defines configuration for the Decompress middleware.
type DecompressConfig struct {
	// Skipper defines a function to skip middleware.
	Skipper Skipper

	// MaxBytes bounds the decompressed body in bytes. Reading past it fails
	// with the framework's 413 Request Entity Too Large classification, the
	// same way the server-wide max_body_bytes limit surfaces. Zero selects
	// [DefaultDecompressMaxBytes]; a negative value is a configuration error
	// and panics at construction.
	MaxBytes int64
}

// DefaultDecompressConfig returns the default Decompress middleware config.
// Each call returns a fresh value, so callers cannot mutate the package-wide
// defaults.
func DefaultDecompressConfig() DecompressConfig {
	return DecompressConfig{
		Skipper:  DefaultSkipper,
		MaxBytes: DefaultDecompressMaxBytes,
	}
}

// Decompress returns request-body decompression middleware.
//
// Credo does not decompress request bodies by default: a Content-Encoding the
// application has not opted into is rejected by BindBody with 415 Unsupported
// Media Type. This middleware is that opt-in. It understands gzip (also
// x-gzip) and deflate (zlib-wrapped per RFC 9110, with raw DEFLATE accepted
// for clients that omit the zlib header), all from the standard library.
//
// For a matching request it replaces the body with the decompressed stream
// bounded by [DecompressConfig.MaxBytes], sets ContentLength to -1 (unknown),
// and removes the Content-Encoding header so downstream binding treats the
// body as plain. An unsupported coding or a coding list with more than one
// entry returns 415; a stream whose header is already corrupt returns 400
// through the regular bind error pipeline. Bodies declared empty pass
// through untouched.
func Decompress(cfg ...DecompressConfig) credo.Middleware {
	config := resolveConfig(cfg, DefaultDecompressConfig(), normalizeDecompressConfig)

	return func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			if config.Skipper(ctx) {
				return next(ctx)
			}

			req := ctx.Request()
			coding := req.Header.Get("Content-Encoding")
			if coding == "" {
				return next(ctx)
			}

			open, ok := contentDecoder(coding)
			if !ok {
				if isIdentityContentCoding(coding) {
					return next(ctx)
				}
				return unsupportedContentEncoding(coding)
			}

			if req.Body == nil || req.Body == http.NoBody || req.ContentLength == 0 {
				// Nothing to decode: present an ordinary empty body downstream.
				req.Header.Del("Content-Encoding")
				return next(ctx)
			}

			decoded, err := open(req.Body)
			if err != nil {
				return decompressOpenError(err)
			}
			limited := http.MaxBytesReader(ctx.Response().Unwrap(), decoded, config.MaxBytes)
			defer limited.Close()

			req.Body = limited
			req.ContentLength = -1
			req.Header.Del("Content-Encoding")

			return next(ctx)
		}
	}
}

func normalizeDecompressConfig(config DecompressConfig) DecompressConfig {
	defaults := DefaultDecompressConfig()
	if config.Skipper == nil {
		config.Skipper = defaults.Skipper
	}
	if config.MaxBytes < 0 {
		panic("credo: middleware.Decompress: MaxBytes must be >= 0")
	}
	if config.MaxBytes == 0 {
		config.MaxBytes = defaults.MaxBytes
	}
	return config
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

// isIdentityContentCoding reports whether every token in a Content-Encoding
// value is "identity" (or the value is empty), meaning no transformation.
func isIdentityContentCoding(value string) bool {
	for token := range strings.SplitSeq(value, ",") {
		if t := strings.ToLower(strings.TrimSpace(token)); t != "" && t != "identity" {
			return false
		}
	}
	return true
}

func unsupportedContentEncoding(coding string) error {
	return credo.NewHTTPError(http.StatusUnsupportedMediaType, credo.CodeUnsupportedContentEncoding).
		WithMessageKey("unsupported content encoding: " + coding)
}

// decompressOpenError classifies a failure to read the compressed stream's
// header: an exhausted server body limit keeps its 413 identity, an empty
// stream is an empty body, anything else is a syntax failure.
func decompressOpenError(err error) error {
	if mbe, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return credo.NewHTTPError(http.StatusRequestEntityTooLarge).
			WithMessageKey("request body too large").
			WithInternal(mbe)
	}
	if errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return &credo.BindError{Reason: credo.BindReasonEmptyBody, Internal: err}
	}
	return &credo.BindError{Reason: credo.BindReasonSyntax, Internal: err}
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
