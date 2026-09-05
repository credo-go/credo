// Copyright (c) 2015-present Peter Kieltyka (https://github.com/pkieltyka), Google Inc.
// Originally derived from github.com/go-chi/chi/middleware (MIT License).

package credo

import (
	"errors"
	"log/slog"
	"net/http"

	internalobserve "github.com/credo-go/credo/internal/observe"
)

// defaultRecoverStackSize bounds the stack trace captured during panic
// recovery when [RecoverConfig.StackSize] is zero. It prevents unbounded log
// growth from deep stacks during a panic storm.
const defaultRecoverStackSize = 8192

// RecoverConfig customizes the built-in panic recovery. Recovery is on by
// default and covers the whole request execution — feature selectors, user
// middleware, handlers, renderers and access-log filters. Configure it with
// [WithRecoverConfig]; disable it with [WithoutRecover], which wins regardless
// of option order. The zero value selects every default.
type RecoverConfig struct {
	// Logger receives the "panic recovered" record. nil logs through the
	// request-scoped logger (ctx.Logger()), which already carries the request
	// ID when that feature is installed; a dedicated logger gets the request
	// ID as an explicit attribute instead.
	Logger *slog.Logger

	// DisableStackTrace omits the stack trace from the panic record.
	DisableStackTrace bool

	// StackSize is the maximum number of bytes of stack trace captured.
	// Zero (or negative) selects 8192.
	StackSize int
}

// WithRecoverConfig customizes the built-in panic recovery: its logger,
// whether a stack trace is captured, and the stack size. It does not enable
// recovery — recovery is on by default — and it never re-enables it:
// [WithoutRecover] disables recovery regardless of the order the two options
// are given in.
func WithRecoverConfig(cfg RecoverConfig) Option {
	return func(o *appOptions) { o.recoverCfg = cfg }
}

// recoverFeature is the normalized recovery configuration. nil on the App
// means recovery is disabled.
type recoverFeature struct {
	logger    *slog.Logger
	stack     bool
	stackSize int
}

func newRecoverFeature(cfg RecoverConfig) *recoverFeature {
	f := &recoverFeature{
		logger:    cfg.Logger,
		stack:     !cfg.DisableStackTrace,
		stackSize: cfg.StackSize,
	}
	if f.stackSize <= 0 {
		f.stackSize = defaultRecoverStackSize
	}
	return f
}

// recoverPanic handles a panic recovered by the executor: it logs the panic
// with the configured stack policy and renders the 500 through the central
// error pipeline. http.ErrAbortHandler is re-panicked so the HTTP server
// aborts the connection as intended.
//
// Once the transport has actually been hijacked no response can be written;
// upgrade request headers alone are not transport state. A response that was
// already committed keeps its status and body: handleError only logs then.
func (app *App) recoverPanic(c *Context, rec *recoverFeature, rvr any) {
	if err, ok := rvr.(error); ok && errors.Is(err, http.ErrAbortHandler) {
		panic(rvr)
	}

	r := c.request.Request

	stack := ""
	if rec.stack {
		stack = internalobserve.StackTrace(rec.stackSize)
	}
	// Add request_id explicitly only when the logger does not already carry
	// it: a dedicated logger never does; ctx.Logger() does whenever a
	// request-scoped logger was set.
	requestID := ""
	if rec.logger != nil || !c.HasRequestLogger() {
		requestID = c.RequestID()
	}
	attrs := internalobserve.PanicAttrs(rvr, r.Method, r.URL.Path, requestID, stack)

	logger := rec.logger
	if logger == nil {
		logger = c.Logger()
	}
	logger.LogAttrs(r.Context(), slog.LevelError, "panic recovered", attrs...)

	// The response below is the request's final state: the access record
	// observes it rather than the escaped-panic fallback.
	c.exec.panicked = false

	if c.response.Hijacked() {
		return
	}
	app.handleError(ErrInternalServerError.WithInternal(internalobserve.PanicError(rvr)), c)
}
