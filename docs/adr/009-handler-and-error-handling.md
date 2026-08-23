# ADR-009: Handler & Error Handling

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** ADR-008

## Context

Go's stdlib handler signature `func(http.ResponseWriter, *http.Request)` has no error return. Handlers must write error responses inline, leading to scattered, inconsistent error handling across an application. Each handler independently decides error format, status code, and logging.

Enterprise applications (ADR-001) need consistent, centralized error handling: uniform error format, proper logging, and i18n support.

## Decision

### Handler Signature

```go
type Handler func(ctx *Context) error
```

Handlers return `error` instead of writing error responses directly. This is the single most important API decision — it enables centralized error handling.

### Centralized Error Handling

A returned error flows through the framework's internal error pipeline, which separates two concerns:

1. **Framework internals** (the unexported `handleError` method) classify the error, log server faults (5xx and unhandled), write the response with the classified status and a JSON Content-Type, suppress bodies on HEAD requests, and guard against writing to an already-committed response. These run once, correctly, for every error.
2. **`ErrorRenderer`** decides only the response body's _shape_. It receives an already-classified `ErrorInfo` and returns the body to encode; it is pluggable via `App.SetErrorRenderer`, and the default renders RFC 7807 Problem Details JSON.

```go
type ErrorRenderer func(ctx *Context, info ErrorInfo) any

type ErrorInfo struct {
    Err        error           // original error (errors.As, Sentry, custom headers)
    MessageKey string          // i18n key used to resolve Problem.Title
    Problem    *ProblemDetails // classified RFC 7807 Problem Details
}
```

A non-nil return is encoded with the application's JSON profile and written with `info.Problem.Status` (mutable before returning — the renderer's one status seam); returning nil keeps the default RFC 7807 body, so a renderer can also run purely for its side effects — setting `Retry-After` / `WWW-Authenticate` headers, reporting to Sentry — and let the default shape stand. The renderer is invoked for HEAD too (headers again), with the returned body discarded. For the rare non-JSON error response, the renderer may commit the response itself through the Context; once committed, the return value is ignored. That committed check also means a renderer cannot double-write by both committing and returning.

Splitting the pipeline this way keeps custom renderers total-function simple: a renderer maps `ErrorInfo` to a body value and never re-implements classification, logging, status writing, or the HEAD/committed guards, so those framework concerns cannot be accidentally omitted — or accidentally diverged, the failure mode of the earlier write-it-yourself contract, where every renderer repeated `WriteHeader` and encoding by hand and a missing write triggered a warn-and-fallback path. `ErrorInfo.Err` keeps the original error available for cross-cutting use; `ErrorInfo.MessageKey` preserves the raw i18n key for client-side i18n, telemetry grouping, or custom error-code mapping. The machine-readable `code` and structured `details` are read from `info.Problem.Code` / `info.Problem.Details` — `ErrorInfo` deliberately gains no duplicate fields, so the classified `ProblemDetails` stays the single source of truth for wire-facing values. A renderer that panics is caught and a minimal 500 emitted.

The renderer is the error-side half of the response-envelope story: paired with `SuccessRenderer` (ADR-008's `Context.Render` seam), an application defines one envelope for every response it produces without a single handler knowing the envelope exists.

### Where the Pipeline Begins

The pipeline covers every response the application produces, including the ones no handler produced: 404 and 405, `BindBody`/`BindQuery` failures, `max_body_bytes` overruns, and panics all reach `ErrorRenderer` as classified `ProblemDetails`, exactly like a returned error.

It cannot cover what `net/http` rejects before routing. A request whose headers exceed `max_header_bytes` or `max_header_value_count` (431), whose request line or Host is malformed (400), or whose transfer encoding is unsupported (501) is answered by the standard library writing directly to the connection, while no `http.Handler` has been invoked. Those responses are `text/plain`, carry no request ID, are not logged — not even through the `http.Server.ErrorLog` bridge — and `ErrorRenderer` is never called for them.

Credo does not try to hide this. Intercepting those responses would mean owning the connection loop instead of `net/http`'s, which trades the standard library's hardened HTTP/1.1 and HTTP/2 parsing for a bespoke one — a bad exchange for cosmetic uniformity on three malformed-request paths. The boundary is documented instead (see the [error-handling guide](../guides/error-handling.md)), so clients know to check `Content-Type` before parsing a non-2xx body as `application/problem+json`.

### Default Error Detection Order

```
1. Response already committed → no-op (response is in-flight)
2. validation.Errors → 422 Unprocessable Entity with field errors
3. *BindError → 400 Bad Request with a typed decode-reason errors entry
4. *HTTPError → status from Status, title resolved from MessageKey
5. fault.Provider → root default HTTP policy from the transport-neutral semantic kind
6. HTTPStatus() int interface → legacy or explicit transport status
7. Any other error → 500 Internal Server Error (message NOT leaked)
```

`*BindError` is the typed decode failure returned by `BindBody`/`BindQuery` (see the [Context spec](../specs/context.md)). It renders like a one-entry validation failure — `type: "https://credo.dev/errors/binding"`, the machine-readable reason as the entry's `code` — so clients consume decode and validation failures through one shape while the status codes (400 vs 422) keep the parse/validate distinction.

Internal error details are never exposed to clients. Server errors (5xx) and unhandled errors are logged via `slog`.

### RFC 7807 Problem Details

All error responses use the RFC 7807 Problem Details format:

```json
{
    "type": "about:blank",
    "title": "Not Found",
    "status": 404,
    "instance": "/api/users/999",
    "code": "not_found"
}
```

`code` and `details` are RFC 7807 extension members. `code` is the machine-readable twin of the human-readable `title`: taken from `HTTPError.Code` when set explicitly (`WithCode`), otherwise derived from the message key as the segment after the last dot (`"user.email_exists"` → `"email_exists"`); a dotless key is a literal human message and yields no code. This turns the dotted i18n key convention into a framework guarantee instead of an undocumented habit consumers reverse-engineered from `ErrorInfo.MessageKey`. Validation failures carry `"code": "validation_failed"`; bind failures carry the decode reason (`"syntax"`, `"type_mismatch"`, …) as the top-level code — the same value as their single `errors[]` entry, chosen over a `"bind."`-prefixed form for consistency with the last-segment rule. `details` carries `HTTPError.Details` verbatim (application JSON profile; client-safe data only).

Validation errors include field-level details:

```json
{
    "type": "https://credo.dev/errors/validation",
    "title": "Validation Failed",
    "status": 422,
    "instance": "/api/users",
    "errors": [
        {"field": "email", "code": "required", "message": "is required"},
        {"field": "name", "code": "length", "message": "must be 2-100 characters"}
    ]
}
```

### HTTPError Type

```go
type HTTPError struct {
    Status     int    // HTTP status code
    Code       string // machine-readable error code; derived from MessageKey when empty
    MessageKey string // i18n message key or literal fallback message
    Details    any    // structured client-safe detail (RFC 7807 "details" extension)
    Internal   error  // underlying error (not exposed to client)
}
```

The status field is named `Status`, not `Code` (renamed pre-1.0): "code" is the industry-standard name for the machine-readable string code in error envelopes, so the HTTP status keeping that name would have forced the string code into an awkward synonym forever. The rename changes the field's type as well as its name, so no consumer breaks silently — `.Code` expecting an int fails to compile.

Sentinel errors for common conditions:

```go
var (
    ErrNotFound            = NewHTTPError(404)
    ErrBadRequest          = NewHTTPError(400)
    ErrUnauthorized        = NewHTTPError(401)
    ErrForbidden           = NewHTTPError(403)
    ErrInternalServerError = NewHTTPError(500)
    // ...
)
```

`WithInternal(err)` wraps an underlying error without exposing it; `WithCode` and `WithDetails` set the wire extensions. All three are copy-on-write, so sentinels stay immutable:

```go
return credo.ErrNotFound.WithInternal(fmt.Errorf("user %d not found in DB", id))
// Client sees: 404 Not Found
// Server logs: user 42 not found in DB

return credo.NewHTTPError(409, "user.email_exists").
    WithCode("dup_email").WithDetails(map[string]string{"field": "email"})
```

### i18n Integration

Error messages are resolved at the consumption point (`resolveMessage`) using a 3-level fallback:

1. **i18n bundle** — if configured via `app.UseI18n()` and the request locale has a translation for the MessageKey, use it
2. **builtInMessages** — built-in English defaults for standard HTTP error keys (e.g., `MsgKeyNotFound` → "Not Found")
3. **MessageKey itself** — used as-is (works for literal messages and custom domain error codes)

Resolving lazily at render time (rather than pre-translating when the error is created) keeps `HTTPError` values locale-independent and lets the request locale drive the final message.

### MsgKey Constants

Standard HTTP error keys are defined as constants:

```go
const (
    MsgKeyBadRequest   = "http.bad_request"
    MsgKeyNotFound     = "http.not_found"
    MsgKeyInternalError = "http.internal_server_error"
    // ... (14 total)
)
```

`NewHTTPError(code)` without an explicit message key auto-resolves to the appropriate constant via `statusToKey`. Custom message keys are supported: `NewHTTPError(404, "user.not_found")`.

### Panic Recovery

Panic recovery is built into the framework as the outermost handler layer (applied in `compile()`). It catches panics from all middleware and handlers, logs the stack trace via `ctx.Logger()`, and returns `ErrInternalServerError`. `http.ErrAbortHandler` is re-panicked.

`WithoutRecover()` disables built-in recovery (e.g., for tests or custom recovery mechanisms). `middleware.Recover(cfg ...RecoverConfig)` remains available for per-group/route recovery with custom configuration (custom logger, stack size control, disabled stack traces).

### Middleware Type

```go
type Middleware func(next Handler) Handler
```

Single middleware type for all tiers. Stdlib middleware adapted via `WrapStdMiddleware`:

```go
app.GlobalMiddleware(credo.WrapStdMiddleware(corsMiddleware))
```

## Consequences

**Positive:**

- Consistent error format across the entire application
- Internal errors never leak to clients
- RFC 7807 is machine-readable and standards-compliant
- Centralized logging of server errors
- i18n-aware error translation built-in
- `WithInternal` pattern separates client message from debug info
- Custom renderers stay small — a renderer only returns a body shape; classification, logging, status, encoding, and the HEAD/committed guards are handled by the framework

**Negative:**

- Every handler must return `error` (not just write response)
- A custom `ErrorRenderer` requires understanding `ErrorInfo` fields (`Err`, `MessageKey`, `Problem`)
- RFC 7807 JSON format may not suit all clients (mitigate: custom ErrorRenderer)
- Uniformity stops at the standard library's own rejections: 431, malformed-request 400, and unsupported-transfer-encoding 501 are plain text written straight to the connection, unreachable by `ErrorRenderer` and absent from the logs
