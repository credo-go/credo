# ADR-009: Handler & Error Handling

**Status:** Accepted
**Date:** 2026-03-01
**Depends on:** ADR-008

## Context

Go's stdlib handler signature `func(http.ResponseWriter, *http.Request)`
has no error return. Handlers must write error responses inline, leading
to scattered, inconsistent error handling across an application. Each
handler independently decides error format, status code, and logging.

Enterprise applications (ADR-001) need consistent, centralized error
handling: uniform error format, proper logging, and i18n support.

## Decision

### Handler Signature

```go
type Handler func(ctx *Context) error
```

Handlers return `error` instead of writing error responses directly.
This is the single most important API decision — it enables centralized
error handling.

### Centralized Error Handling

> **Note:** The original decision used `ErrorHandler`. This was replaced
> by `ErrorRenderer` with `ErrorInfo` — see the [amendment](#amendment-errorhandler---errorrenderer-2026-03-09) at the end.

```go
// Original design (superseded by amendment below)
type ErrorHandler func(err error, ctx *Context)
```

The `App.ErrorHandler` was called whenever a handler returned a non-nil
error. The default implementation (`DefaultErrorHandler`) wrote
RFC 7807 Problem Details JSON responses.

### Default Error Detection Order

```
1. Response already committed → no-op (response is in-flight)
2. validation.Errors → 422 Unprocessable Entity with field errors
3. *HTTPError → status from Code, title resolved from MessageKey
4. HTTPStatus() int interface → status from HTTPStatus() (e.g., store errors)
5. Any other error → 500 Internal Server Error (message NOT leaked)
```

Internal error details are never exposed to clients. Server errors
(5xx) and unhandled errors are logged via `slog`.

### RFC 7807 Problem Details

All error responses use the RFC 7807 Problem Details format:

```json
{
    "type": "about:blank",
    "title": "Not Found",
    "status": 404,
    "instance": "/api/users/999"
}
```

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
    Code       int    // HTTP status code
    MessageKey string // i18n message key or literal fallback message
    Internal   error  // underlying error (not exposed to client)
}
```

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

`WithInternal(err)` wraps an underlying error without exposing it:

```go
return credo.ErrNotFound.WithInternal(fmt.Errorf("user %d not found in DB", id))
// Client sees: 404 Not Found
// Server logs: user 42 not found in DB
```

### i18n Integration

`DefaultErrorHandler` resolves error messages at the consumption point
using a 3-level fallback:

1. **i18n bundle** — if configured via `app.UseI18n()` and the request
   locale has a translation for the MessageKey, use it
2. **builtInMessages** — built-in English defaults for standard HTTP
   error keys (e.g., `MsgKeyNotFound` → "Not Found")
3. **MessageKey itself** — used as-is (works for literal messages and
   custom domain error codes)

This replaces the previous `translateError()` pre-processing approach.

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

`NewHTTPError(code)` without an explicit message key auto-resolves to
the appropriate constant via `statusToKey`. Custom message keys are
supported: `NewHTTPError(404, "user.not_found")`.

### Panic Recovery

Panic recovery is built into the framework as the outermost handler
layer (applied in `compile()`). It catches panics from all middleware
and handlers, logs the stack trace via `ctx.Logger()`, and returns
`ErrInternalServerError`. `http.ErrAbortHandler` is re-panicked.

`WithoutRecover()` disables built-in recovery (e.g., for tests or
custom recovery mechanisms). `middleware.Recover(cfg ...RecoverConfig)`
remains available for per-group/route recovery with custom configuration
(custom logger, stack size control, disabled stack traces).

### Middleware Type

```go
type Middleware func(next Handler) Handler
```

Single middleware type for all tiers. Stdlib middleware adapted via
`WrapStdMiddleware`:

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

**Negative:**
- Every handler must return `error` (not just write response)
- Custom ErrorRenderer requires understanding `ErrorInfo` fields (`Err`, `MessageKey`, `Problem`)
- RFC 7807 JSON format may not suit all clients (mitigate: custom ErrorRenderer)

---

## ErrorRenderer

### Change

The `ErrorHandler` callback has been replaced by `ErrorRenderer`:

```go
// Before
type ErrorHandler func(err error, ctx *Context)

// After
type ErrorRenderer func(ctx *Context, info ErrorInfo)

type ErrorInfo struct {
    Err        error            // original error (for errors.As, Sentry, custom headers)
    MessageKey string           // i18n key used to resolve Problem.Title
    Problem    *ProblemDetails  // classified RFC 7807 Problem Details
}
```

- `app.ErrorHandler` field → `app.ErrorRenderer` field.
- `DefaultErrorHandler` (exported function) → internal `handleError` method
  (no longer exported).
- `ErrorInfo` carries the original error, message key, and classified
  `ProblemDetails` to the renderer.

### Rationale

The original `ErrorHandler` received a raw `error` and was responsible for
the full error-to-response pipeline: classifying the error, logging,
handling HEAD requests, guarding against committed responses, and
formatting the output. This meant custom implementations had to replicate
all framework logic just to change the response format.

The new design splits responsibilities:

1. **Framework internals** (`handleError` method) — error classification
   (validation errors, `*HTTPError`, `HTTPStatus()` interface, unknown
   errors), logging of 5xx/unhandled errors, HEAD response suppression,
   and committed-response guard. These concerns are handled once, correctly,
   by the framework.
2. **`ErrorRenderer`** — receives an `ErrorInfo` value containing the
   original error (`Err`), the i18n message key (`MessageKey`), and the
   classified `*ProblemDetails` (`Problem`). The renderer decides _format_
   and can use the original error for observability (Sentry, custom
   headers, telemetry grouping).

This separation means:
- Custom renderers are simpler (no error classification logic needed).
- Framework-level concerns (logging, HEAD, committed guard) cannot be
  accidentally omitted by a custom implementation.
- The original error is available for cross-cutting concerns (Sentry,
  audit logging, extracting metadata for custom headers like
  `Retry-After` or `WWW-Authenticate`).
- The i18n message key is preserved for client-side i18n, telemetry
  grouping, or custom error code mapping.
- A fallback safety net catches renderer panics/failures and writes a
  minimal 500 response.

### Migration

Replace:
```go
app.ErrorHandler = func(err error, ctx *credo.Context) { ... }
```

With:
```go
app.SetErrorRenderer(func(ctx *credo.Context, info credo.ErrorInfo) {
    // info.Err — original error (errors.As, Sentry, custom headers)
    // info.MessageKey — i18n key used to resolve Problem.Title
    // info.Problem — classified *ProblemDetails (Status, Title, Type, Instance, Errors)
    ctx.Response().JSON(info.Problem.Status, myFormat(info))
})
```
