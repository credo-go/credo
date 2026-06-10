# Error Handling

This guide covers how Credo handles errors returned from handlers.
For validation-specific errors, see the [Validation Spec](../specs/validation.md).
For i18n integration, see the [Localization Guide](localization.md).

---

## Handler Signature

Every Credo handler returns `error`:

```go
type Handler func(ctx *credo.Context) error
```

Return `nil` for success. Return any `error` to trigger the internal
error handling pipeline.

```go
app.GET("/users/{id}", func(ctx *credo.Context) error {
    user, err := svc.FindByID(ctx.Context(), ctx.Request().RouteParam("id"))
    if err != nil {
        return err // handled by internal error pipeline
    }
    return ctx.Response().JSON(200, user)
})
```

---

## HTTPError

`HTTPError` is the primary error type for HTTP responses:

```go
type HTTPError struct {
    Code       int    // HTTP status code
    MessageKey string // i18n message key or literal fallback
    Internal   error  // underlying error (never exposed to client)
}
```

Create errors with `NewHTTPError`:

```go
// Auto-resolves MessageKey from status code
return credo.NewHTTPError(404) // MessageKey = "http.not_found"

// Custom MessageKey
return credo.NewHTTPError(404, "user.not_found")

// With internal error (logged, not exposed)
return credo.NewHTTPError(500, "db.query_failed").WithInternal(err)
```

---

## Sentinel Errors

Common HTTP errors are pre-defined:

```go
return credo.ErrNotFound            // 404
return credo.ErrBadRequest          // 400
return credo.ErrUnauthorized        // 401
return credo.ErrForbidden           // 403
return credo.ErrConflict            // 409
return credo.ErrInternalServerError // 500
```

Wrap with internal context:

```go
return credo.ErrNotFound.WithInternal(fmt.Errorf("user %s not in DB", id))
// Client sees: 404 Not Found
// Server logs: user 42 not in DB
```

The sentinels are shared package-level instances, like `io.EOF`: compare
with `errors.Is` and treat them as immutable. Never assign to their
fields — that would change the behavior of every handler in the process.
`WithInternal` already returns a copy, and `NewHTTPError` builds fresh
instances for custom statuses or message keys.

---

## MsgKey Constants

Standard HTTP error keys:

| Constant | Value | Default Message |
|----------|-------|-----------------|
| `MsgKeyBadRequest` | `http.bad_request` | Bad Request |
| `MsgKeyUnauthorized` | `http.unauthorized` | Unauthorized |
| `MsgKeyForbidden` | `http.forbidden` | Forbidden |
| `MsgKeyNotFound` | `http.not_found` | Not Found |
| `MsgKeyMethodNotAllowed` | `http.method_not_allowed` | Method Not Allowed |
| `MsgKeyConflict` | `http.conflict` | Conflict |
| `MsgKeyUnprocessableEntity` | `http.unprocessable_entity` | Unprocessable Entity |
| `MsgKeyUnsupportedMedia` | `http.unsupported_media_type` | Unsupported Media Type |
| `MsgKeyInternalError` | `http.internal_server_error` | Internal Server Error |
| `MsgKeyTooManyRequests` | `http.too_many_requests` | Too Many Requests |
| `MsgKeyServiceUnavailable` | `http.service_unavailable` | Service Unavailable |
| `MsgKeyGatewayTimeout` | `http.gateway_timeout` | Gateway Timeout |
| `MsgKeyRequestTimeout` | `http.request_timeout` | Request Timeout |
| `MsgKeyValidationFailed` | `http.validation_failed` | Validation Failed |

`NewHTTPError(code)` without an explicit key auto-resolves via `statusToKey`.

---

## Message Resolution

The internal error handling pipeline resolves `MessageKey` to a
human-readable string using a 3-level fallback:

1. **i18n bundle** — if `app.UseI18n()` is configured and the request
   locale has a translation for the key, use it
2. **builtInMessages** — built-in English defaults for standard HTTP
   error keys
3. **Key itself** — used as-is (works for literal messages and custom
   domain error codes)

```
MessageKey = "http.not_found"
  → i18n("tr", "http.not_found") = "Bulunamadı"  ← used
  → builtInMessages["http.not_found"] = "Not Found"
  → "http.not_found"

MessageKey = "user.email_exists"
  → i18n("tr", "user.email_exists") = "Bu e-posta zaten kayıtlı"  ← used
  → builtInMessages["user.email_exists"] = (not found)
  → "user.email_exists"

MessageKey = "user.email_exists" (no i18n)
  → builtInMessages["user.email_exists"] = (not found)
  → "user.email_exists"  ← used as-is
```

---

## Internal Error Pipeline

The framework handles error classification, logging, and
committed-response guards internally. The `ErrorRenderer` receives an
`ErrorInfo` (containing the original error, the i18n message key, and
the classified `*ProblemDetails`) and is responsible for writing the
response. When no custom `ErrorRenderer` is set, the default renderer
writes RFC 7807 Problem Details JSON.

Detection order (handled internally, then passed to `ErrorRenderer`):

1. **Response committed** → no-op (guard)
2. **`validation.Errors`** → 422 with field-level errors
3. **`*HTTPError`** → status from `Code`, title resolved from `MessageKey`
4. **`HTTPStatus() int`** → status from interface (e.g., store errors)
5. **Any other error** → 500 (message never leaked)

```json
{
    "type": "about:blank",
    "title": "Not Found",
    "status": 404,
    "instance": "/api/users/999"
}
```

Server errors (5xx) and unhandled errors are logged via `slog`.
Internal error messages are never exposed to the client.

---

## Custom ErrorRenderer

Replace the default renderer with `app.SetErrorRenderer` when you need a
different response format (it must be set before the server starts).
The `ErrorRenderer` receives an `ErrorInfo` containing:

- **`info.Err`** — the original error (for `errors.As`/`errors.Is`, Sentry, etc.)
- **`info.MessageKey`** — the i18n key used to resolve the title (for telemetry, client-side i18n)
- **`info.Problem`** — the classified `*ProblemDetails` (status, title, instance, validation errors)

Error classification, logging, and the committed-response guard are all
performed by the framework before the renderer is called. The renderer
is called for all HTTP methods including HEAD, so it can set response
headers (e.g., `Retry-After`, `WWW-Authenticate`). For HEAD requests
where the renderer does not commit the response, the framework sends a
status-only response (no body).

```go
app.SetErrorRenderer(func(ctx *credo.Context, info credo.ErrorInfo) {
    ctx.Response().JSON(info.Problem.Status, map[string]any{
        "success": false,
        "code":    info.MessageKey,
        "message": info.Problem.Title,
        "status":  info.Problem.Status,
    })
})
```

Access the original error for observability integrations:

```go
app.SetErrorRenderer(func(ctx *credo.Context, info credo.ErrorInfo) {
    // Report to Sentry
    if info.Problem.Status >= 500 {
        sentry.CaptureException(info.Err)
    }

    // Set custom headers from error metadata
    if rl, ok := errors.AsType[*ratelimit.Error](info.Err); ok {
        ctx.Response().Header().Set("Retry-After", rl.RetryAfter())
    }

    // Don't write body → fallback renders default RFC 7807 JSON
    // (headers set above are preserved)
})
```

> **Fallback safety:** If the `ErrorRenderer` panics, the framework
> recovers and writes a 500 response. If the renderer returns without
> committing the response (i.e., without calling `WriteHeader` or
> writing a body), the framework logs a warning and falls back to the
> default RFC 7807 JSON renderer. Setting headers without writing a body
> is a valid pattern — the fallback renderer will include those headers.

---

## httpStatusProvider Interface

Errors from packages like `store/` implement `HTTPStatus() int`:

```go
type httpStatusProvider interface {
    HTTPStatus() int
}
```

The internal error handling pipeline detects this via `errors.As` without
importing the package that defines the error. This enables clean dependency
boundaries between the error handler and data access layers.

```go
// store/errors.go
var ErrNotFound = &StoreError{status: 404, msg: "not found"}

func (e *StoreError) HTTPStatus() int { return e.status }
```

---

## Domain Errors (Service Layer)

For service-layer sentinel errors, use `NewHTTPError` with domain-specific
message keys:

```go
var (
    ErrUserNotFound  = credo.NewHTTPError(404, "USER_NOT_FOUND")
    ErrEmailExists   = credo.NewHTTPError(409, "EMAIL_EXISTS")
    ErrRoleNotFound  = credo.NewHTTPError(422, "ROLE_NOT_FOUND")
)
```

Wrap internal errors with `WithInternal`:

```go
func (s *UserService) Create(ctx context.Context, input CreateInput) (*User, error) {
    exists, err := s.repo.EmailExists(ctx, input.Email)
    if err != nil {
        return nil, credo.NewHTTPError(500, "USER_CREATE_FAILED").WithInternal(err)
    }
    if exists {
        return nil, ErrEmailExists
    }
    // ...
}
```

Add translations in locale files:

```json
{
    "USER_NOT_FOUND": "User not found.",
    "EMAIL_EXISTS": "This email address is already registered.",
    "USER_CREATE_FAILED": "An error occurred while creating the user."
}
```

---

## Best Practices

1. **Return errors, don't write them** — let the error pipeline decide format
2. **Use sentinel errors** for known domain conditions (4xx)
3. **Use `WithInternal`** for server errors (5xx) — separates client message
   from debug info
4. **Define MessageKeys as constants** in your `types` package for consistency
5. **Add translations** for all MessageKeys in your locale files
6. **Never leak internal errors** — `WithInternal` ensures they are logged
   but not sent to the client
