# Error Handling

This guide covers how Credo handles errors returned from handlers. For validation-specific errors, see the [Validation Spec](../specs/validation.md). For i18n integration, see the [Localization Guide](localization.md).

---

## Handler Signature

Every Credo handler returns `error`:

```go
type Handler func(ctx *credo.Context) error
```

Return `nil` for success. Return any `error` to trigger the internal error handling pipeline.

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
    Status     int    // HTTP status code
    Code       string // machine-readable error code (RFC 7807 "code" extension); derived from MessageKey when empty
    MessageKey string // i18n message key or literal fallback
    Details    any    // structured client-safe detail (RFC 7807 "details" extension)
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

// With an explicit machine-readable code and structured detail
return credo.NewHTTPError(409, "user.email_exists").
    WithCode("dup_email").
    WithDetails(map[string]string{"field": "email"})
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

The sentinels are shared package-level instances, like `io.EOF`: compare with `errors.Is` and treat them as immutable. Never assign to their fields — that would change the behavior of every handler in the process. `WithInternal`, `WithCode`, and `WithDetails` all return copies, and `NewHTTPError` builds fresh instances for custom statuses or message keys.

---

## MsgKey Constants

Standard HTTP error keys:

| Constant | Value | Default Message |
| --- | --- | --- |
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

The internal error handling pipeline resolves `MessageKey` to a human-readable string using a 3-level fallback:

1. **i18n bundle** — if `app.UseI18n()` is configured and the request locale has a translation for the key, use it
2. **builtInMessages** — built-in English defaults for standard HTTP error keys
3. **Key itself** — used as-is (works for literal messages and custom domain error codes)

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

## Machine-Readable Codes and Details

Alongside the human-readable title, every problem response can carry a machine-readable `code` and structured `details` — both RFC 7807 extension members clients can switch on without parsing text:

```json
{
    "type": "about:blank",
    "title": "user.email_exists",
    "status": 409,
    "code": "dup_email",
    "details": { "field": "email" }
}
```

- **Explicit**: `WithCode("dup_email")` and `WithDetails(v)` on `HTTPError` set them directly. `Details` is encoded with the application's JSON profile; put only client-safe data there.
- **Derived**: when `Code` is empty, the pipeline derives it from the message key — the segment after the last dot (`"user.email_exists"` → `"email_exists"`, `MsgKeyNotFound` = `"http.not_found"` → `"not_found"`). A key without a dot is a literal human message and yields no code, so the member is omitted.
- **Validation and binding**: `validation.Errors` responses carry `"code": "validation_failed"`; a `BindBody`/`BindQuery` failure carries the bind reason (`"syntax"`, `"type_mismatch"`, …) as the top-level code, matching the `errors[]` entry.

The dotted-key convention (`domain.snake_case_code`) is thereby a framework guarantee: name your i18n message keys that way and the wire code comes for free; `WithCode` is the override for when the two must diverge.

---

## Internal Error Pipeline

The framework handles error classification, logging, status writing, HEAD handling, and committed-response guards internally. The `ErrorRenderer` receives an `ErrorInfo` (containing the original error, the i18n message key, and the classified `*ProblemDetails`) and returns the response body — or nil for the default. When no custom `ErrorRenderer` is set (or it returns nil), the framework writes RFC 7807 Problem Details JSON. Those bytes are deterministic by contract: map keys — a validation error's `params`, for instance — are always sorted, even when the application disabled deterministic encoding through `WithJSONOptions` ([ADR-021](../adr/021-json-output-profile.md)).

Detection order (handled internally, then passed to `ErrorRenderer`):

1. **Response committed** → no-op (guard)
2. **`validation.Errors`** → 422 with field-level errors
3. **`*HTTPError`** → status from `Status`, title resolved from `MessageKey`
4. **`fault.Provider`** → root default HTTP policy for the semantic kind
5. **`HTTPStatus() int`** → legacy or explicit transport status
6. **Any other error** → 500 (message never leaked)

```json
{
    "type": "about:blank",
    "title": "Not Found",
    "status": 404,
    "instance": "/api/users/999",
    "code": "not_found"
}
```

Server errors (5xx) and unhandled errors are logged via `slog`. Internal error messages are never exposed to the client.

---

## What the Pipeline Does Not Cover

The pipeline starts when a request reaches the application. Everything that gets that far is uniform — including the responses no handler produced:

| Response | Produced by | Shape |
| --- | --- | --- |
| Handler's returned error | error pipeline | RFC 7807, `ErrorRenderer` |
| Panic (built-in recovery) | error pipeline | RFC 7807, `ErrorRenderer` |
| 404 / 405 | error pipeline | RFC 7807, `ErrorRenderer` |
| 400 from `BindBody` / `BindQuery` | error pipeline | RFC 7807, `ErrorRenderer` |
| 413 over `max_body_bytes` | error pipeline | RFC 7807, `ErrorRenderer` |
| **431** over `max_header_bytes` / `max_header_value_count` | `net/http`, direct to the connection | `text/plain`, one line |
| **400** on a malformed request line or Host | `net/http`, direct to the connection | `text/plain`, one line |
| **501** on an unsupported transfer encoding | `net/http`, direct to the connection | `text/plain`, one line |

The bottom three never enter the pipeline, because `net/http` writes them before any `http.Handler` runs — it could not parse the request well enough to route it. They carry no `X-Request-Id`, they are not logged (not even through the `http.Server.ErrorLog` bridge), and `ErrorRenderer` is never called: installing a custom renderer does not change those bytes. This is a property of the standard library's server, not a Credo policy, and no Go framework can intercept them.

The practical consequence is for clients: a consumer that parses every non-2xx body as `application/problem+json` will fail on these three. Check the `Content-Type` before parsing, or terminate at a proxy that normalises error bodies. To keep them rare, size `max_header_bytes` and `max_header_value_count` for your real traffic; to count them, install a `ConnState` hook through `WithHTTPServer` — you can observe the connections, but not rewrite the response.

---

## Custom ErrorRenderer

Replace the default body shape with `app.SetErrorRenderer` when your clients expect a different error format (it must be set before the server starts). The `ErrorRenderer` receives an `ErrorInfo` containing:

- **`info.Err`** — the original error (for `errors.As`/`errors.Is`, Sentry, etc.)
- **`info.MessageKey`** — the i18n key used to resolve the title (for telemetry, client-side i18n)
- **`info.Problem`** — the classified `*ProblemDetails` (status, title, instance, validation errors)

and returns the body to send. The framework does the rest: classification and logging have already happened, and the status code (`info.Problem.Status`), the `Content-Type`, and the actual write stay framework-owned. A non-nil return is encoded as JSON with the application's JSON profile:

```go
app.SetErrorRenderer(func(ctx *credo.Context, info credo.ErrorInfo) any {
    return map[string]any{
        "success": false,
        "code":    info.MessageKey,
        "message": info.Problem.Title,
        "fields":  info.Problem.Errors,
    }
})
```

Returning nil keeps the default RFC 7807 body, which makes side-effect-only renderers natural — report to Sentry, set headers, keep the standard shape:

```go
app.SetErrorRenderer(func(ctx *credo.Context, info credo.ErrorInfo) any {
    // Report to Sentry
    if info.Problem.Status >= 500 {
        sentry.CaptureException(info.Err)
    }

    // Set custom headers from error metadata
    if rl, ok := errors.AsType[*ratelimit.Error](info.Err); ok {
        ctx.Response().Header().Set("Retry-After", rl.RetryAfter())
    }

    return nil // default RFC 7807 body, decorated with the headers above
})
```

To change the status code, mutate `info.Problem.Status` before returning — it is the classified status the framework writes for both body shapes. The renderer is also called for HEAD requests so it can set headers; the returned body is discarded and a status-only response goes out.

Two guarantees close the contract. If the renderer panics, the framework recovers and writes a minimal 500. And for the rare error response that is not JSON at all, the renderer may commit the response itself through the `Context` — exactly as a handler could — after which the return value is ignored:

```go
app.SetErrorRenderer(func(ctx *credo.Context, info credo.ErrorInfo) any {
    if wantsPlainText(ctx) {
        _ = ctx.Response().Text(info.Problem.Status, info.Problem.Title)
        return nil // already committed; the framework writes nothing more
    }
    return myEnvelope(info)
})
```

---

## Response Envelopes

`ErrorRenderer` is one half of a pair. Its success-side mirror is `SuccessRenderer`, installed with `app.SetSuccessRenderer` and consulted by exactly one call site: `ctx.Render(status, data)`. Installing both gives every response the application produces — success and failure, including the 404/405/panic/bind responses no handler produced — one envelope, while no handler ever constructs it:

```go
// The application's own envelope types. Credo ships no envelope shape;
// both renderers are opt-in and nil by default.
type Envelope[T any] struct {
    Data      T      `json:"data"`
    RequestID string `json:"request_id,omitempty"`
}

app.SetSuccessRenderer(func(c *credo.Context, status int, data any) error {
    return c.Response().JSON(status, Envelope[any]{Data: data, RequestID: c.RequestID()})
})

app.SetErrorRenderer(func(c *credo.Context, info credo.ErrorInfo) any {
    return map[string]any{
        "error":      map[string]any{"code": info.MessageKey, "message": info.Problem.Title, "fields": info.Problem.Errors},
        "request_id": c.RequestID(),
    }
})

app.GET("/users/{id}", func(c *credo.Context) error {
    u, err := svc.Get(c.Context(), c.Request().RouteParam("id"))
    if err != nil {
        return err                        // error envelope, applied by the pipeline
    }
    return c.Render(http.StatusOK, u)     // success envelope, applied by Render
})
```

The seam is deliberately narrow on the success side: only `Render` consults the `SuccessRenderer`. The raw `Response` helpers — `JSON`, `XML`, `Text`, `Blob`, the streaming writers — are never intercepted, so handlers serving webhooks, health probes, or third-party-dictated shapes bypass the envelope by calling them directly. With no `SuccessRenderer` installed, `Render` falls back to plain `Response.JSON` and imposes nothing, so `Render` is safe to use as the default success verb from day one and the envelope becomes a one-line decision later.

Note the signature asymmetry: a `SuccessRenderer` owns the write (it decides status, encoding, everything — its error return flows into the error pipeline like any handler error), while an `ErrorRenderer` only returns a shape. The error side runs inside the framework's pipeline, which must keep classification, logging, status, and HEAD semantics correct regardless of what the renderer does; the success side is ordinary handler code where full control is harmless.

---

## Semantic Fault Provider

Transport-neutral packages expose a semantic kind through the stdlib-only
`fault` leaf package:

```go
type Provider interface {
    error
    FaultKind() fault.Kind
}
```

The root pipeline maps known kinds to its default HTTP status/title without
importing the feature package. `store.Error`, for example, carries a semantic
kind plus driver code/constraint/resource/cause metadata; only the kind affects
the default Problem Details response.

```go
if kind, ok := store.KindOf(err); ok {
    switch kind {
    case store.KindNotFound:
        // domain-specific handling
    case store.KindSerialization, store.KindDeadlock:
        // transient condition; not automatic retry permission
    }
}
```

An outer `*HTTPError` is checked first, so the service layer can override the
default transport meaning while retaining the store cause:

```go
return credo.NewHTTPError(http.StatusUnprocessableEntity, "order.stock_conflict").
    WithInternal(err)
```

`HTTPStatus() int` remains supported after semantic providers for legacy and
explicit transport-specific errors. Store sentinels retain it only as a
deprecated compatibility bridge.

---

## Domain Errors (Service Layer)

For service-layer sentinel errors, use `NewHTTPError` with domain-specific message keys. The dotted `domain.snake_case` form doubles as the wire `code` (last segment, derived automatically):

```go
var (
    ErrUserNotFound  = credo.NewHTTPError(404, "user.not_found")   // code: "not_found"
    ErrEmailExists   = credo.NewHTTPError(409, "user.email_exists") // code: "email_exists"
    ErrRoleNotFound  = credo.NewHTTPError(422, "user.role_not_found")
)
```

Wrap internal errors with `WithInternal`:

```go
func (s *UserService) Create(ctx context.Context, input CreateInput) (*User, error) {
    exists, err := s.repo.EmailExists(ctx, input.Email)
    if err != nil {
        return nil, credo.NewHTTPError(500, "user.create_failed").WithInternal(err)
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
    "user.not_found": "User not found.",
    "user.email_exists": "This email address is already registered.",
    "user.create_failed": "An error occurred while creating the user."
}
```

---

## Best Practices

1. **Return errors, don't write them** — let the error pipeline decide format
2. **Use sentinel errors** for known domain conditions (4xx)
3. **Use `WithInternal`** for server errors (5xx) — separates client message from debug info
4. **Define MessageKeys as constants** in your `types` package for consistency
5. **Add translations** for all MessageKeys in your locale files
6. **Never leak internal errors** — `WithInternal` ensures they are logged but not sent to the client
