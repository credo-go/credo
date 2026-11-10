package credo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Observer observes each request after its response is final. Observe
// receives one immutable record of the exchange, [Exchange]; the response can
// no longer change, so an observer exports, counts or logs what happened.
//
// Observers are added with [App.AddObserver] and run in registration order,
// after error rendering, panic recovery and response finalization, the
// compressor's trailer included. The access log installed by
// [App.UseAccessLog] is one of them. An observer may be called concurrently
// for multiple requests and must be concurrency-safe.
//
// With recovery enabled, a panic inside Observe is logged as a framework
// Error diagnostic and that observer is skipped for the exchange; the
// observers after it still run. http.ErrAbortHandler, and every panic under
// [WithoutRecover], propagate.
type Observer interface {
	Observe(ex *Exchange)
}

// Exchange is the immutable record of one request and its final response that
// an [Observer] receives. It is valid only during Observe: an observer copies
// what it keeps, apart from the request's context, which [Exchange.Context]
// returns and which may be kept.
//
// The status, the byte count and the duration are final. Bytes counts the
// bytes the underlying transport writer accepted — compressed output when
// response compression applied. Duration runs from executor entry to response
// finalization; the observers themselves are excluded. If recovery is
// disabled and a panic escaped, Status is the 500 fallback while Bytes
// reflects only what was written before the panic.
type Exchange struct {
	c        *Context
	status   int
	bytes    int64
	duration time.Duration
}

// Method returns the request method.
func (ex *Exchange) Method() string { return ex.c.request.Request.Method }

// Path returns the path the request was served under, after any rewrite.
func (ex *Exchange) Path() string { return ex.c.request.Request.URL.Path }

// OriginalPath returns the path the client sent, before any rewrite.
func (ex *Exchange) OriginalPath() string { return ex.c.OriginalPath() }

// Route returns the [RouteInfo] of the matched route, the view
// [Context.Route] returns: Pattern is the registered pattern, for example
// "/v1/jobs/{job_id}", which is the label of bounded cardinality a metric
// wants, and Name is the route's name. A request a mount matched has the
// mount's view, whose Methods slice is shared and must not be modified. When
// no route matched (404/405) it is the zero RouteInfo, whose Pattern and Name
// are "".
func (ex *Exchange) Route() RouteInfo { return ex.c.Route() }

// RemoteAddr returns the client address, as [Request.RealIP] reports it.
func (ex *Exchange) RemoteAddr() string { return ex.c.request.RealIP() }

// UserAgent returns the request's User-Agent header.
func (ex *Exchange) UserAgent() string { return ex.c.request.Request.UserAgent() }

// Status returns the final response status.
func (ex *Exchange) Status() int { return ex.status }

// Bytes returns the body bytes the underlying transport writer accepted,
// after compression.
func (ex *Exchange) Bytes() int64 { return ex.bytes }

// Duration returns the time from executor entry to response finalization.
func (ex *Exchange) Duration() time.Duration { return ex.duration }

// ErrorCode returns the error code the pipeline classified for the response,
// captured before the error renderer ran: for a recovered panic, the code of
// the 500 it rendered; after a fallback — an error renderer that panicked or
// returned an invalid status — the fallback's code. It is "" when the
// pipeline rendered nothing: no error, an error returned after the response
// was committed or hijacked, a handler that wrote its own error response, and
// an escaped panic with recovery disabled. It is never inferred from the
// status.
func (ex *Exchange) ErrorCode() string { return ex.c.exec.code }

// PanicRecovered reports whether recovery caught a panic during the request.
func (ex *Exchange) PanicRecovered() bool { return ex.c.exec.recovered }

// RequestID returns the request's ID, or "" when [App.UseRequestID] is not
// installed.
func (ex *Exchange) RequestID() string { return ex.c.RequestID() }

// Context returns the request's context as it stands when the response is
// final: the values set through [Context.Set] and the user, read with Value
// and their keys, and whatever a layer enclosing the observers put there,
// such as a span. Its nodes are never modified, so it may be kept beyond
// Observe; it is cancelled when the request ends.
func (ex *Exchange) Context() context.Context { return ex.c.Context() }

// Get returns the value set under key and reports whether one was set, as
// [Context.Get] does at the end of the request. A value set inside a scope is
// visible. Get panics for a zero Key.
func (ex *Exchange) Get[T any](key Key[T]) (T, bool) {
	key.mustID("Exchange.Get")
	v, ok := ex.c.lookup(key).(T)
	return v, ok
}

// GetUser returns the authenticated user stored under type T and reports
// whether one was present, as [Context.GetUser] does: ex.GetUser[*User]().
func (ex *Exchange) GetUser[T any]() (T, bool) {
	user, ok := ex.c.lookup(userKey[T]{}).(T)
	return user, ok
}

// AddObserver adds o to the observers of every request's final response.
// Observers run in registration order; [App.UseAccessLog] registers the
// access log in the same order. AddObserver panics for a nil observer and
// after the App was prepared or shut down.
func (app *App) AddObserver(o Observer) {
	if o == nil {
		panic("credo: App.AddObserver: nil observer")
	}
	app.checkFrozen("App.AddObserver")
	app.observers = append(app.observers, o)
}

// observe is the executor's observation stage. It runs after response
// finalization: it fixes the exchange's final status, byte count and duration
// and runs every observer on it. outBytes is the byte count taken at the
// output boundary when the response passed through a finalized writer
// (compression); counted reports that it applies, otherwise the plain
// Response size is the transport count.
func (app *App) observe(c *Context, outBytes int64, counted bool) {
	status := c.response.Status()
	if status == 0 {
		if c.exec.panicked {
			// Only reachable when recovery is disabled and a panic escaped:
			// the response is uncommitted and the client sees the server's
			// abort.
			status = http.StatusInternalServerError
		} else {
			status = http.StatusOK
		}
	}
	bytes := c.response.Size()
	if counted {
		bytes = outBytes
	}
	ex := &c.exec.exchange
	*ex = Exchange{c: c, status: status, bytes: bytes, duration: time.Since(c.exec.start)}
	for _, o := range app.observers {
		app.runObserver(o, ex)
	}
}

// runObserver invokes one observer. A panic inside it cannot change the
// response any more, so with recovery enabled it is logged as a framework
// diagnostic and the observer is skipped; http.ErrAbortHandler and, with
// recovery disabled, every panic propagate.
func (app *App) runObserver(o Observer, ex *Exchange) {
	if app.recover != nil {
		defer func() {
			if rvr := recover(); rvr != nil {
				app.observerPanicked(ex, "credo: observer panicked", rvr,
					slog.String("observer", fmt.Sprintf("%T", o)))
			}
		}()
	}
	o.Observe(ex)
}

// observerPanicked logs a panic recovered from a callback that runs after the
// final response — an observer, or one of the access log's callbacks — as a
// framework Error diagnostic. http.ErrAbortHandler is re-panicked.
func (app *App) observerPanicked(ex *Exchange, msg string, rvr any, extra ...slog.Attr) {
	if err, ok := rvr.(error); ok && errors.Is(err, http.ErrAbortHandler) {
		panic(rvr)
	}
	c := ex.c
	r := c.request.Request
	attrs := make([]slog.Attr, 0, 4+len(extra))
	attrs = append(attrs,
		slog.String("panic", fmt.Sprint(rvr)),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", ex.status),
	)
	attrs = append(attrs, extra...)
	c.Logger().LogAttrs(r.Context(), slog.LevelError, msg, attrs...)
}
