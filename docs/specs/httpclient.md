# HTTP Client Spec

**Status:** Implemented (beta) **Package:** `httpclient/` **ADR:** [ADR-014](../adr/014-observability.md) **Sources:** Original (stdlib `net/http` composition)

## Overview

The `httpclient/` package provides outbound HTTP for Credo applications: timeout, retry with backoff, structured logging, and W3C trace context propagation — the cross-cutting concerns that every enterprise service re-implements around `http.Client`.

It is built as a **composable `http.RoundTripper` chain**, not a custom client interface:

- `httpclient.New(opts...)` returns a plain `*http.Client` — everything that accepts an `http.Client` (SDKs, generated API clients, stdlib helpers) works unchanged.
- Each concern is an `http.RoundTripper` wrapper, individually exported so users can compose them onto an existing transport.
- The package is **stdlib-only**: it imports neither the root `credo` package nor any third-party library. The logger is a plain `*slog.Logger` (in app code, typically `infra.Logger`).

[ADR-014](../adr/014-observability.md) records trace propagation and the planned OTel hooks; the package's own design rationale is in [Design Decisions](#design-decisions).

## Goals

1. **One factory, stdlib shape**: `New(opts...) *http.Client`. No custom `Get`/`Post` wrapper methods, no new request type.
2. **Safe-by-default retry**: full-jitter exponential backoff for idempotent methods on transport errors and 5xx ([retry semantics](#retry-semantics)).
3. **Structured outbound logging**: one `slog` line per attempt, with secrets redacted ([logging semantics](#logging-semantics)).
4. **Trace continuity without an OTel SDK**: the inbound W3C `traceparent` continues on outbound calls with a fresh span ID per attempt ([trace propagation semantics](#trace-propagation-semantics)).
5. **Composability**: each transport is exported (`NewRetryTransport`, `NewLoggingTransport`, `NewTraceTransport`) for use on existing clients.

## Non-Goals (deliberate)

- **No OTel propagator / span creation / metrics** — they land as hooks with the observability release ([ADR-014](../adr/014-observability.md)).
- **No circuit breaker** — deferred until an application needs one; the core stays lean.
- **No `app.HTTPClient()` sugar** — plain DI registration ([decision 7](#design-decisions)).
- **No `Retry-After` honoring** — backoff is purely exponential with jitter; revisit with real demand.
- **No request hedging, no per-attempt timeout option** — `http.Client.Timeout` bounds the _total_ call including retries; per-call deadlines come from the request context.

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

// DefaultRetryConfig returns a fresh copy of the defaults; zero-valued
// RetryConfig fields fall back to them.
func DefaultRetryConfig() RetryConfig

// DefaultRetryIf reports whether a request should be retried:
// idempotent method AND (transport error OR 5xx response),
// never after context cancellation/deadline.
func DefaultRetryIf(req *http.Request, resp *http.Response, err error) bool

// Composable transports (escape hatch for existing clients); a nil base is
// http.DefaultTransport.
func NewRetryTransport(base http.RoundTripper, cfg ...RetryConfig) http.RoundTripper
func NewLoggingTransport(base http.RoundTripper, logger *slog.Logger) http.RoundTripper // nil logger panics
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

Options compose in this **canonical order regardless of the order they are passed**:

```text
http.Client.Timeout                  ← total budget, incl. all retries
└── retry                            ← outermost transport: drives attempts
    └── logging                      ← one line per attempt
        └── trace                    ← fresh traceparent per attempt
            └── base transport       ← cloned http.DefaultTransport (or WithBaseTransport)
```

- Retry outermost: each attempt re-enters logging and trace, so every attempt gets its own log line and its own span ID.
- Logging above trace: the log line takes `trace_id` from the request context (`GetTraceContext`), not from the header, so ordering does not hide it.
- The retry transport records the attempt number in the request context; the logging transport reads it (`attempt` attribute) without coupling the two.

### Retry semantics

- **Attempts**: `MaxAttempts` total (default 3 = 1 initial + 2 retries).
- **Backoff**: full-jitter exponential — sleep is a uniformly random duration in `[0, min(MaxDelay, MinDelay·2^(attempt-1)))`. Waits abort immediately when the request context is done.
- **Default predicate** (`DefaultRetryIf`):
  - context canceled / deadline exceeded → never retry;
  - non-idempotent method (anything other than GET, HEAD, OPTIONS, TRACE, PUT, DELETE, QUERY) → never retry;
  - transport error (`err != nil`) → retry;
  - `5xx` response → retry;
  - anything else (incl. `429`) → no retry. Override with `RetryIf`.
- **Body replay**: a request with a body is retried only when `req.GetBody` is set (stdlib sets it automatically for `bytes.Buffer`/`bytes.Reader`/`strings.Reader` bodies). Without `GetBody`, the first response/error is returned as-is — a request is never silently re-sent with a half-consumed body.
- **Exhaustion**: the _last_ response (or error) is returned unchanged — a final 503 arrives as `(resp, nil)` exactly like stdlib; no custom error wrapping.
- Each attempt is a `req.Clone(ctx)`; retries take a fresh body from `GetBody()`. The caller's request is never mutated. The response body of a _discarded_ attempt is drained (up to a small cap) and closed so the underlying connection can be reused.

### QUERY redirect semantics

`httpclient.New` installs a `CheckRedirect` policy because Go's default client rewrites QUERY to GET on 301/302. The policy preserves the RFC 10008 contract while retaining the stdlib redirect engine for URL resolution, cookie-jar behavior, Referer handling, credential stripping, response draining, and hop execution:

- 301/302 preserve QUERY. A body is restored only through the original request's `GetBody`; without it the client returns the 3xx response unchanged via `http.ErrUseLastResponse` rather than silently sending GET.
- The body headers stripped by `net/http` during its provisional rewrite (`Content-Type`, `Content-Encoding`, `Content-Language`, `Content-Location`) are restored with all their values, including for a bodyless QUERY.
- Sensitive headers are never re-copied; the stdlib's cross-origin stripping remains authoritative.
- 303 remains GET without a body. A later redirect in that chain also stays GET.
- 307/308 remain under the stdlib behavior, which already preserves method and a replayable body.
- The standard ten-redirect limit applies to every method.

Each followed redirect hop traverses the configured transport chain, so retry, per-attempt logging, and trace propagation apply normally. Assigning `client.CheckRedirect` after `New` is the full-control escape hatch and replaces this QUERY policy entirely.

### Logging semantics

One line per attempt via the supplied `*slog.Logger`:

| Attr | Value |
| --- | --- |
| `method` | request method |
| `url` | `scheme://host/path` — **query string, userinfo and fragment stripped** |
| `status` | response status code (omitted on transport error) |
| `error` | transport error (omitted on response) |
| `duration` | attempt duration |
| `attempt` | attempt number, when the retry transport is active |
| `trace_id` | 32-hex trace ID from `GetTraceContext`, when present |

Levels follow the access-log convention: `Error` for transport errors and 5xx, `Warn` for 4xx, `Info` otherwise. Headers and bodies are never logged. `WithLogging(nil)` and `NewLoggingTransport(base, nil)` panic at construction (config misuse — panic-vs-error policy).

### Trace propagation semantics

Manual W3C Trace Context (version `00`) — no OTel types:

- **Extract** (server side, user wiring): `TraceContextFromRequest(r)` reads `traceparent`/`tracestate` from the inbound request, and `SetTraceContext(ctx, tc)` attaches them to the context used for outbound calls; the godoc of `TraceContextFromRequest` shows the wiring in a handler.
- **Inject** (transport): per attempt —
  - outgoing request already carries `traceparent` → left untouched;
  - context carries a _valid_ `TraceContext` → child derivation: same trace ID and flags, **new random 8-byte span ID** (`crypto/rand`), `tracestate` forwarded unchanged;
  - otherwise (absent or invalid inbound) → new root: random trace ID + span ID, flags `01` (sampled), no `tracestate`. Invalid inbound values are discarded per the W3C restart guidance.
- Derived span IDs do not correspond to recorded spans (there is no tracer yet) — the point is **trace-ID continuity** so logs and any downstream tracers correlate. With the observability release ([ADR-014](../adr/014-observability.md)), an OTel propagator supersedes this transport.

## Design Decisions

1. **`RoundTripper` chain over custom client interface** — stdlib-composable; any SDK accepting `*http.Client` benefits without adaptation. A custom interface would fragment the ecosystem for zero expressive gain.
2. **Fixed chain order** — retry→logging→trace→base is the only layering where per-attempt logs and per-attempt span IDs both fall out naturally. Making order configurable invites subtle misconfiguration (e.g. logging outside retry hides attempts).
3. **POST not retried by default** — non-idempotent retries cause duplicate side effects (double payments). Users who have idempotency keys can opt in via `RetryIf`.
4. **Returned response over wrapped error on exhaustion** — keeps stdlib semantics (`resp, nil` for HTTP-level failures); callers already must check status codes.
5. **Query string, userinfo and fragment stripped from logs** — they routinely carry secrets (`?api_key=`, signed URLs, basic-auth credentials). Scheme, host and path are enough for diagnostics.
6. **Stdlib-only, logger injected** — no `credo` import keeps the dependency direction clean (feature packages may import root, but this one needs nothing from it); `infra.Logger` is passed by the user at wiring time.
7. **Plain DI registration** — an outbound client sits at an application boundary, so the application constructs it and binds it: `app.ProvideValue[*http.Client]`, or a wrapper type per client when it needs several. No `app.HTTPClient()` sugar.
8. **QUERY redirect policy at the client seam** — retry without redirect correctness would still permit a silent method change. A small `CheckRedirect` policy restores only QUERY's method, replayable body, and body headers; a custom redirect transport would duplicate security-sensitive stdlib behavior.
