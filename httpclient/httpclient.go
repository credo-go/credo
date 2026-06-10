package httpclient

import (
	"log/slog"
	"net/http"
	"time"
)

// options holds the configuration assembled by New from its Options.
type options struct {
	timeout time.Duration
	base    http.RoundTripper
	retry   *RetryConfig
	logger  *slog.Logger
	trace   bool
}

// Option configures [New].
type Option func(*options)

// WithTimeout sets http.Client.Timeout — the total budget for a call,
// including all retry attempts and backoff waits. Per-call deadlines come
// from the request context.
func WithTimeout(d time.Duration) Option {
	return func(o *options) { o.timeout = d }
}

// WithBaseTransport sets the innermost transport that performs the actual
// HTTP exchange. Default: a clone of [http.DefaultTransport].
func WithBaseTransport(rt http.RoundTripper) Option {
	return func(o *options) { o.base = rt }
}

// WithRetry enables retry with full-jitter exponential backoff. Zero args
// use [DefaultRetryConfig]. See [NewRetryTransport] for the semantics.
func WithRetry(cfg ...RetryConfig) Option {
	config := DefaultRetryConfig()
	if len(cfg) > 0 {
		config = cfg[0]
	}
	return func(o *options) { o.retry = &config }
}

// WithLogging enables structured outbound logging through the given logger
// (in app code, typically infra.Logger). See [NewLoggingTransport] for the
// attributes and level mapping. Panics if logger is nil (config misuse).
func WithLogging(logger *slog.Logger) Option {
	if logger == nil {
		panic("httpclient: WithLogging called with nil logger")
	}
	return func(o *options) { o.logger = logger }
}

// WithTracePropagation enables W3C traceparent injection on outbound
// requests. See [NewTraceTransport] for the derivation rules and
// [TraceContextFromRequest] / [SetTraceContext] for the server-side wiring.
func WithTracePropagation() Option {
	return func(o *options) { o.trace = true }
}

// New returns a plain *http.Client with the configured RoundTripper chain —
// everything that accepts an http.Client works unchanged.
//
// Options compose in a canonical order regardless of the order they are
// passed:
//
//	http.Client.Timeout              ← total budget, incl. all retries
//	└── retry                        ← outermost transport: drives attempts
//	    └── logging                  ← one line per attempt
//	        └── trace                ← fresh traceparent per attempt
//	            └── base transport   ← cloned http.DefaultTransport (or WithBaseTransport)
//
// Retry sits outermost so each attempt re-enters logging and trace: every
// attempt gets its own log line and its own span ID.
//
// New() with no options is equivalent to an http.Client with a cloned
// default transport and no timeout.
func New(opts ...Option) *http.Client {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	rt := o.base
	if rt == nil {
		rt = defaultBaseTransport()
	}
	// Assemble the canonical chain inside-out: base ← trace ← logging ← retry.
	if o.trace {
		rt = NewTraceTransport(rt)
	}
	if o.logger != nil {
		rt = NewLoggingTransport(rt, o.logger)
	}
	if o.retry != nil {
		rt = NewRetryTransport(rt, *o.retry)
	}

	return &http.Client{
		Transport: rt,
		Timeout:   o.timeout,
	}
}

// defaultBaseTransport clones http.DefaultTransport so per-client
// connection pools and any later mutations stay isolated from the global.
func defaultBaseTransport() http.RoundTripper {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		return t.Clone()
	}
	// http.DefaultTransport was replaced with a custom RoundTripper —
	// use it as-is.
	return http.DefaultTransport
}
