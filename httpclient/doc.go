// Package httpclient provides outbound HTTP for Credo applications:
// timeout, retry with backoff, structured logging, and W3C trace context
// propagation — composed as an http.RoundTripper chain on a plain
// *http.Client.
//
// The package is stdlib-only: it imports neither the root credo package nor
// any third-party library, so it can be used in any Go program. The logger
// is a plain *slog.Logger (in app code, typically infra.Logger).
//
// # Quick Start
//
//	client := httpclient.New(
//		httpclient.WithTimeout(10*time.Second),
//		httpclient.WithRetry(),
//		httpclient.WithLogging(infra.Logger),
//		httpclient.WithTracePropagation(),
//	)
//
// New returns a *http.Client — anything that accepts one (SDKs, generated
// API clients, stdlib helpers) works unchanged. Register it via plain DI:
//
//	app.ProvideValue(client)
//
// or wrap it in a named type when an application needs several clients.
//
// # Chain Order
//
// Options compose in a canonical order regardless of the order they are
// passed: Client.Timeout (total budget) → retry → logging → trace → base
// transport. Retry sits outermost, so every attempt gets its own log line
// and its own traceparent span ID. The individual transports are exported
// ([NewRetryTransport], [NewLoggingTransport], [NewTraceTransport]) for
// composing onto an existing client.
//
// # Safe-by-Default Retry
//
// [DefaultRetryIf] retries idempotent methods only (GET, HEAD, OPTIONS,
// TRACE, PUT, DELETE, QUERY) on transport errors and 5xx responses — POST is
// never retried by default, and a request body is only replayed when
// req.GetBody is set. See [NewRetryTransport].
//
// The client returned by [New] also preserves QUERY across 301/302 redirects
// when its body can be replayed, leaves 303 and 307/308 to net/http, and returns
// a non-replayable 301/302 response to the caller instead of silently changing
// QUERY to GET. Assigning Client.CheckRedirect after New replaces this policy.
//
// # Trace Propagation
//
// Tracing here is manual W3C traceparent header propagation — enough for
// log/trace correlation across services without an OTel SDK. Forward the
// inbound request's trace context to outbound calls with
// [TraceContextFromRequest] and [SetTraceContext]. OTel propagators, span
// creation, and request metrics land with the observability phase.
//
// Maturity: beta
package httpclient
