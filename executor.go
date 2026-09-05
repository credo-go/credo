package credo

import (
	"io"
	"log/slog"
	"net/http"
	"time"
)

// execState is the per-request bookkeeping of the executor. It lives inside
// the pooled Context so a request allocates nothing for it.
type execState struct {
	// start is the executor entry time; access-log duration is measured from
	// it through response finalization.
	start time.Time

	// logging records that the access-log feature selected this request
	// (installed and not skipped), so finishRequest observes it.
	logging bool

	// panicked is true while the request runs and cleared when the chain (or
	// recovery) completed normally. It survives only when a panic escaped
	// with recovery disabled, or an abort is propagating.
	panicked bool

	// compress is the response compression writer installed for this
	// request, nil when compression was not selected.
	compress *compressResponseWriter

	// body is the decoded request body installed by decompression; it is
	// closed at finalization.
	body io.Closer
}

func (s *execState) reset() {
	*s = execState{}
}

// execute is the request executor: the single owner of request
// initialization, the user middleware/handler chain, centralized error and
// panic handling, response finalization, access-log observation and Context
// release. chain is the prepared global middleware → dispatch handler.
//
// The stages run in one framework-defined order regardless of the order the
// features were installed in:
//
//  1. request ID (when installed): resolve or generate, publish on the
//     context store and the response header, defer logger enrichment;
//  2. access-log selection (when installed): the pre-dispatch Skipper;
//  3. request decompression (when installed): selected once on the original
//     request, before any user middleware, so Global body readers and
//     binders see the same decoded stream;
//  4. response compression (when installed): selected once on the original
//     request; the writer stays installed through error rendering;
//  5. the chain; a returned error goes through the central error pipeline;
//  6. recovery (unless WithoutRecover): a panic in any stage above, feature
//     selectors included, is logged and rendered as 500 through the same
//     pipeline; http.ErrAbortHandler always propagates;
//  7. finalization: the compressor is closed before anything observes the
//     response, so the access record sees the bytes the transport accepted;
//  8. access-log observation (when selected), after the final response;
//  9. Context release — on normal completion, on a propagating panic and on
//     a transport abort alike.
//
// Lifecycle-rejected requests never reach execute: ServeHTTP answers them
// with the callback-free 503 first.
func (app *App) execute(c *Context, chain Handler) {
	c.exec.reset()
	if app.accessLog != nil {
		c.exec.start = time.Now()
	}
	defer app.finishRequest(c)
	app.runRequest(c, chain)
}

// runRequest is the recovered region of the executor: everything that may
// invoke application code — feature selectors, the chain and error rendering
// — runs here, so a panic anywhere inside is handled by one recovery.
func (app *App) runRequest(c *Context, chain Handler) {
	if rec := app.recover; rec != nil {
		defer func() {
			if rvr := recover(); rvr != nil {
				app.recoverPanic(c, rec, rvr)
			}
		}()
	}
	c.exec.panicked = true

	if f := app.requestID; f != nil {
		f.apply(c)
	}
	if f := app.accessLog; f != nil && (f.skipper == nil || !f.skipper(c)) {
		c.exec.logging = true
	}
	if f := app.decompress; f != nil {
		if err := f.apply(c); err != nil {
			// The body could not be presented to the chain: answer through
			// the central pipeline without running any user middleware.
			app.handleError(err, c)
			c.exec.panicked = false
			return
		}
	}
	if f := app.compress; f != nil {
		f.apply(c)
	}

	if err := chain(c); err != nil {
		app.handleError(err, c)
	}
	app.warnEnvelopeBypass(c)
	c.exec.panicked = false
}

// finishRequest runs after the recovered region on every path — normal
// completion, a propagating panic and a transport abort. It finalizes the
// response (closing the compressor), observes the access record and releases
// the Context to the pool.
func (app *App) finishRequest(c *Context) {
	defer app.ctxPool.put(c)

	var (
		outBytes int64
		counted  bool
		abort    bool
	)
	if cw := c.exec.compress; cw != nil {
		c.exec.compress = nil
		var closeErr error
		if c.response.Hijacked() {
			// The connection belongs to the application now; nothing may be
			// written to it, so the compressor is abandoned, not flushed.
			cw.abandon()
		} else {
			closeErr = cw.Close()
		}
		c.response.ResponseWriter = cw.ResponseWriter
		outBytes, counted = cw.out.n, true
		releaseCompressResponseWriter(cw)
		if closeErr != nil {
			abort = app.compressionFailed(c, closeErr)
		}
	}
	if body := c.exec.body; body != nil {
		c.exec.body = nil
		_ = body.Close()
	}
	if c.exec.logging {
		app.observeAccess(c, outBytes, counted)
	}
	if abort {
		// Output has started and the compressed stream is incomplete: no
		// second body may follow, so the connection is aborted the way
		// net/http expects.
		panic(http.ErrAbortHandler)
	}
}

// compressionFailed reports a compressor finalization failure. It is always
// logged as a framework Error diagnostic, independently of access logging.
// When output has started the committed status and bytes are preserved and
// the caller aborts the connection; otherwise the failure is rendered through
// the central pipeline with compression already removed from the writer.
func (app *App) compressionFailed(c *Context, err error) (abort bool) {
	r := c.request.Request
	c.Logger().LogAttrs(r.Context(), slog.LevelError, "credo: response compression failed",
		slog.Any("error", err),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", c.response.Status()),
	)
	if c.response.Committed() || c.response.Hijacked() {
		return true
	}
	app.handleError(ErrInternalServerError.WithInternal(err), c)
	return false
}
