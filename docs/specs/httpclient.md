# HTTP Client Spec

**Status**: Implemented
**Package**: `httpclient/`
**Sources**: Original (stdlib `net/http` composition)
**Roadmap**: TODO Phase 4.8

---

## Overview

The `httpclient/` package provides outbound HTTP for Credo applications:
timeout, retry with backoff, structured logging, and W3C trace context
propagation — the cross-cutting concerns that every enterprise service
re-implements around `http.Client`.

It is built as a **composable `http.RoundTripper` chain**, not a custom
client interface:

- `httpclient.New(opts...)` returns a plain `*http.Client` — everything
  that accepts an `http.Client` (SDKs, generated API clients, stdlib
  helpers) works unchanged.
- Each concern is an `http.RoundTripper` wrapper, individually exported
  so users can compose them onto an existing transport.
- The package is **stdlib-only**: it imports neither the root `credo`
  package nor any third-party library. The logger is a plain
  `*slog.Logger` (in app code, typically `infra.Logger`).

The **lean core ships independently of observability hooks**.
Tracing here is manual W3C `traceparent` header propagation — enough for
log/trace correlation across services without an OTel SDK. OTel
propagators, span creation, and request metrics land as hooks when 3.5
does.

## Goals

1. **One factory, stdlib shape**: `New(opts...) *http.Client`. No custom
   `Get`/`Post` wrapper methods, no new request type.
2. **Safe-by-default retry**: exponential backoff with full jitter,
   retrying only idempotent methods on transport errors and 5xx — POST is
   never retried by default.
3. **Structured outbound logging**: one `slog` line per attempt with
   method, URL (query stripped), status, duration, attempt number, and
   trace ID when available. Secrets never logged.
4. **Trace continuity before OTel**: forward the W3C `traceparent` from
   the inbound request to outbound calls, deriving a fresh span ID per
   attempt, so distributed traces stay connected even before Phase 3.5.
5. **Composability**: each transport is exported (`NewRetryTransport`,
   `NewLoggingTransport`, `NewTraceTransport`) for use on existing
   clients.

## Non-Goals (deliberate)

- **No OTel propagator / span creation / metrics** — Phase 3.5 hooks.
- **No circuit breaker** — deferred until demanded (decision §4).
- **No `app.HTTPClient()` sugar** — outbound clients sit at an application
  boundary, so the user constructs the client and registers it via plain DI.
- **No `Retry-After` honoring** — backoff is purely exponential+jitter in
  the lean core; revisit with real demand.
- **No request hedging, no per-attempt timeout option** —
  `http.Client.Timeout` bounds the *total* call including retries;
  per-call deadlines come from the request context.

---

## API

```go
// New returns a *http.Client with the configured RoundTripper chain.
func New(opts ...Option) *http.Client

type Option func(*options)

func WithTimeout(d time.Duration) Option          // http.Client.Timeout (total, incl. all retries)
func WithBaseTransport(rt http.RoundTripper) Option // innermost transport; default: cloned http.DefaultTransport
func WithRetry(cfg ...RetryConfig) Option         // zero args = DefaultRetryConfig
func WithLogging(logger *slog.Logger) Option      // per-attempt slog lines; nil logger panics
func WithTracePropagation() Option                // W3C traceparent injection

type RetryConfig struct {
    MaxAttempts int           // total attempts incl. the first; default 3
    MinDelay    time.Duration // first backoff ceiling; default 100ms
    MaxDelay    time.Duration // backoff cap; default 2s
    RetryIf     func(req *http.Request, resp *http.Response, err error) bool // default DefaultRetryIf
}

// DefaultRetryIf reports whether a request should be retried:
// idempotent method AND (transport error OR 5xx response),
// never after context cancellation/deadline.
func DefaultRetryIf(req *http.Request, resp *http.Response, err error) bool

// Composable transports (escape hatch for existing clients).
func NewRetryTransport(base http.RoundTripper, cfg ...RetryConfig) http.RoundTripper
func NewLoggingTransport(base http.RoundTripper, logger *slog.Logger) http.RoundTripper
func NewTraceTransport(base http.RoundTripper) http.RoundTripper

// W3C trace context carriage (server inbound → client outbound).
type TraceContext struct {
    TraceParent string // "00-<32hex trace-id>-<16hex span-id>-<2hex flags>"
    TraceState  string // optional vendor data; forwarded unchanged
}

func TraceContextFromRequest(r *http.Request) (TraceContext, bool) // read inbound headers
func SetTraceContext(ctx context.Context, tc TraceContext) context.Context
func GetTraceContext(ctx context.Context) (TraceContext, bool)
```

### Chain order (fixed)

Options compose in a **canonical order regardless of the order they are
passed** — there is exactly one sensible layering, so it is not
user-configurable:

```text
http.Client.Timeout                  ← total budget, incl. all retries
└── retry                            ← outermost transport: drives attempts
    └── logging                      ← one line per attempt
        └── trace                    ← fresh traceparent per attempt
            └── base transport       ← cloned http.DefaultTransport (or WithBaseTransport)
```

- Retry outermost: each attempt re-enters logging and trace, so every
  attempt gets its own log line and its own span ID.
- Logging above trace: the log line takes `trace_id` from the request
  context (`GetTraceContext`), not from the header, so ordering does not
  hide it.
- The retry transport records the attempt number in the request context;
  the logging transport reads it (`attempt` attribute) without coupling
  the two.

### Retry semantics

- **Attempts**: `MaxAttempts` total (default 3 = 1 initial + 2 retries).
- **Backoff**: full-jitter exponential — sleep is a uniformly random
  duration in `[0, min(MaxDelay, MinDelay·2^(attempt-1)))`. Waits abort
  immediately when the request context is done.
- **Default predicate** (`DefaultRetryIf`):
  - context canceled / deadline exceeded → never retry;
  - non-idempotent method (anything other than GET, HEAD, OPTIONS,
    TRACE, PUT, DELETE) → never retry;
  - transport error (`err != nil`) → retry;
  - `5xx` response → retry;
  - anything else (incl. `429`) → no retry. Override with `RetryIf`.
- **Body replay**: a request with a body is retried only when
  `req.GetBody` is set (stdlib sets it automatically for
  `bytes.Buffer`/`bytes.Reader`/`strings.Reader` bodies). Without
  `GetBody`, the first response/error is returned as-is — a request is
  never silently re-sent with a half-consumed body.
- **Exhaustion**: the *last* response (or error) is returned unchanged —
  a final 503 arrives as `(resp, nil)` exactly like stdlib; no custom
  error wrapping.
- Each attempt uses `req.Clone(ctx)` + a fresh `GetBody()` reader; the
  caller's request is never mutated. The response body of a *discarded*
  attempt is drained (up to a small cap) and closed so the underlying
  connection can be reused.

### Logging semantics

One line per attempt via the supplied `*slog.Logger`:

| Attr | Value |
|---|---|
| `method` | request method |
| `url` | `scheme://host/path` — **query string stripped** (may carry secrets) |
| `status` | response status code (omitted on transport error) |
| `error` | transport error (omitted on response) |
| `duration` | attempt duration |
| `attempt` | attempt number, when the retry transport is active |
| `trace_id` | 32-hex trace ID from `GetTraceContext`, when present |

Levels follow the access-log convention: `Error` for transport errors and
5xx, `Warn` for 4xx, `Info` otherwise. Headers and bodies are never
logged. `WithLogging(nil)` panics at construction (config misuse —
panic-vs-error policy).

### Trace propagation semantics

Manual W3C Trace Context (version `00`) — no OTel types:

- **Extract** (server side, user wiring): `TraceContextFromRequest(r)`
  reads `traceparent`/`tracestate` from the inbound request;
  `SetTraceContext(ctx, tc)` attaches it to the context used for
  outbound calls:

  ```go
  func handler(ctx *credo.Context) error {
      callCtx := ctx.Context()
      if tc, ok := httpclient.TraceContextFromRequest(ctx.Request().Request); ok {
          callCtx = httpclient.SetTraceContext(callCtx, tc)
      }
      req, _ := http.NewRequestWithContext(callCtx, http.MethodGet, url, nil)
      resp, err := client.Do(req)
      // ...
  }
  ```

- **Inject** (transport): per attempt —
  - outgoing request already carries `traceparent` → left untouched;
  - context carries a *valid* `TraceContext` → child derivation: same
    trace ID and flags, **new random 8-byte span ID** (`crypto/rand`),
    `tracestate` forwarded unchanged;
  - otherwise (absent or invalid inbound) → new root: random trace ID +
    span ID, flags `01` (sampled), no `tracestate`. Invalid inbound
    values are discarded per the W3C restart guidance.
- Derived span IDs do not correspond to recorded spans (there is no
  tracer yet) — the point is **trace-ID continuity** so logs and any
  downstream tracers correlate. When Phase 3.5 lands, the OTel
  propagator supersedes this transport.

---

## Design Decisions

1. **`RoundTripper` chain over custom client interface** (decision §4) —
   stdlib-composable; any SDK accepting `*http.Client` benefits without
   adaptation. A custom interface would fragment the ecosystem for zero
   expressive gain.
2. **Fixed chain order** — retry→logging→trace→base is the only layering
   where per-attempt logs and per-attempt span IDs both fall out
   naturally. Making order configurable invites subtle misconfiguration
   (e.g. logging outside retry hides attempts).
3. **POST not retried by default** — non-idempotent retries cause
   duplicate side effects (double payments). Users who have idempotency
   keys can opt in via `RetryIf`.
4. **Returned response over wrapped error on exhaustion** — keeps stdlib
   semantics (`resp, nil` for HTTP-level failures); callers already must
   check status codes.
5. **Query strings stripped from logs** — URLs routinely carry tokens
   (`?api_key=`, signed URLs). Path+host is enough for diagnostics.
6. **Stdlib-only, logger injected** — no `credo` import keeps the
   dependency direction clean (feature packages may import root, but
   this one needs nothing from it); `infra.Logger` is passed by the user
   at wiring time.
7. **Plain DI registration** — `credo.ProvideValue[*http.Client]` or a
   named wrapper type for multiple clients; no framework sugar.

---

## File Layout

```text
httpclient/
├── doc.go            ← package documentation
├── httpclient.go     ← New, Option, options struct, chain assembly
├── retry.go          ← retry transport, RetryConfig, DefaultRetryIf, backoff
├── logging.go        ← logging transport
├── trace.go          ← TraceContext, parse/derive/inject, context accessors
├── httpclient_test.go
├── retry_test.go
├── logging_test.go
└── trace_test.go
```

---

## Test Checklist

- `New()` returns a working client with cloned default transport
- `WithTimeout` bounds the total call including retries
- Retry: 5xx then 200 succeeds for GET; attempt count correct
- Retry: transport error then success
- Retry: POST with 5xx is **not** retried by default
- Retry: `RetryIf` override enables POST retry
- Retry: body replay via `GetBody`; no retry when `GetBody` is nil
- Retry: context cancellation aborts the backoff wait
- Retry: exhaustion returns the last response unmodified
- Logging: per-attempt lines, attrs (method/url/status/duration/attempt),
  query string stripped, level mapping (5xx → Error, 4xx → Warn)
- Trace: valid inbound → child (same trace ID, new span ID, flags and
  tracestate preserved)
- Trace: absent/invalid inbound → new root traceparent
- Trace: pre-set `traceparent` header is not overwritten
- Trace: `TraceContextFromRequest`/`Set`/`Get` round-trip
- Chain: canonical order regardless of option order (logging sees
  `attempt`, trace sets header per attempt)
