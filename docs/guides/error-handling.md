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
    Code       string // stable machine-readable code (RFC 7807 "code" extension), materialized at construction
    MessageKey string // optional presentation key or literal title; empty by default
    Details    any    // structured client-safe detail (RFC 7807 "details" extension)
    Internal   error  // underlying error (never exposed to client)
}
```

Create errors with `NewHTTPError(status, code...)` — the optional second argument is the **machine code**, the error's stable wire identity:

```go
// Frozen default code for the status
return credo.NewHTTPError(404) // Code = "not_found"

// Explicit machine code
return credo.NewHTTPError(409, "email_exists")

// With a presentation key (title only; the code is untouched)
return credo.NewHTTPError(404, "user_not_found").
    WithMessageKey("user.not_found")

// With internal error (logged, not exposed) and structured detail
return credo.NewHTTPError(409, "email_exists").
    WithDetails(map[string]string{"field": "email"}).
    WithInternal(err)
```

Codes obey the grammar `^[a-z0-9]+(_[a-z0-9]+)*$`. `NewHTTPError` validates strictly and **panics** on misuse — a status outside `100..999`, more than one code argument, or a malformed code (dots, spaces, uppercase, or empty). A panic during a request is caught by built-in recovery and rendered as a generic 500 that never publishes the invalid value; the loud first-execution failure is deliberate, because a malformed code constant is a bug, not a runtime condition.

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
// Client sees: 404 Not Found, code "not_found"
// Server logs: user 42 not in DB
```

The sentinels are shared package-level instances, like `io.EOF`: compare with `errors.Is` and treat them as immutable. Never assign to their fields — that would change the behavior of every handler in the process. `WithInternal`, `WithMessageKey`, and `WithDetails` all return copies, and `NewHTTPError` builds fresh instances for custom statuses or codes. A sentinel's stored fields are `Code` materialized (`ErrNotFound.Code == "not_found"`) and `MessageKey` empty — presentation keys are opt-in.

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

The constants remain as **explicit presentation keys** — attach one with `WithMessageKey` to keep a built-in title on a non-default status. Construction never auto-attaches a message key; default titles resolve through the `errors.<code>` chain below.

---

## Title Resolution

The pipeline resolves the human-readable title through one of two chains, selected by whether the error carries an explicit `MessageKey`:

**Explicit key** — the 3-level fallback:

1. **i18n bundle** — if `app.UseI18n()` is configured and the request locale has a translation for the key, use it
2. **builtInMessages** — built-in English defaults for standard HTTP error keys
3. **Key itself** — used as-is (works for literal messages and domain-organized locale keys)

**No key (the default)** — the effective lookup key is `errors.<code>`:

1. **i18n bundle** — locale lookup for `errors.<code>`
2. **`http.StatusText(status)`**
3. **`"HTTP <status>"`** when the status has no standard text

```
NewHTTPError(404)                                  // code "not_found", no key
  → i18n("tr", "errors.not_found") = "Bulunamadı"  ← used
  → http.StatusText(404) = "Not Found"
  → "HTTP 404"

NewHTTPError(409, "email_exists").WithMessageKey("user.email_exists")
  → i18n("tr", "user.email_exists") = "Bu e-posta zaten kayıtlı"  ← used
  → builtInMessages["user.email_exists"] = (not found)
  → "user.email_exists"  ← literal fallback without i18n
```

The `errors.` prefix exists because the locale bundle is one flat namespace shared by application, validation, bind, and error messages — a bare `conflict` lookup could be captured by an unrelated application message.

---

## Machine-Readable Codes and Details

Alongside the human-readable title, every problem response the default pipeline produces carries a machine-readable `code`, and optionally structured `details` — both RFC 7807 extension members clients can switch on without parsing text:

```json
{
    "type": "about:blank",
    "title": "user.email_exists",
    "status": 409,
    "code": "email_exists",
    "details": { "field": "email" }
}
```

- **Explicit**: the `NewHTTPError` code argument is the stable wire identity; `WithDetails(v)` attaches structured detail. `Details` is encoded with the application's JSON profile; put only client-safe data there.
- **Default**: with no explicit code, the frozen `statusToCode` table supplies one (`404` → `"not_found"`, `413` → `"request_entity_too_large"`). A status outside the table yields the stable fallback `"http_<status>"` (`499` → `"http_499"`). The table is committed source generated once from Go 1.27 `http.StatusText`; runtime code never derives a wire identity from `StatusText`, so a standard-library rewording can never rename your codes.
- **Validation and binding**: `validation.Errors` responses carry `"code": "validation_failed"`; a `BindBody`/`BindQuery` failure carries the bind reason (`"syntax"`, `"type_mismatch"`, …) as the top-level code, matching the `errors[]` entry.

The code is never derived from the message key: renaming a locale key can no longer rename a wire code. An organization that standardizes on a different code casing (for example AIP-193 `UPPER_SNAKE`) projects it in its `ErrorRenderer`:

```go
app.SetErrorRenderer(func(_ *credo.Context, info credo.ErrorInfo) any {
    info.Problem.Code = strings.ToUpper(info.Problem.Code)
    return nil // default RFC 7807 body with the projected code
})
```

---

## Internal Error Pipeline

The framework handles error classification, logging, status writing, HEAD handling, and committed-response guards internally. The `ErrorRenderer` receives an `ErrorInfo` (containing the original error, the i18n message key, and the classified `*ProblemDetails`) and returns the response body — or nil for the default. When no custom `ErrorRenderer` is set (or it returns nil), the framework writes RFC 7807 Problem Details JSON. Those bytes are deterministic by contract: map keys — a validation error's `params`, for instance — are always sorted, even when the application disabled deterministic encoding through `WithJSONOptions` ([ADR-021](../adr/021-json-output-profile.md)).

Detection order (handled internally, then passed to `ErrorRenderer`):

1. **Response committed** → no-op (guard)
2. **`validation.Errors`** → 422 with field-level errors
3. **`*HTTPError`** → status/code from the stored fields, title from `MessageKey` or the `errors.<code>` chain; invalid stored fields fail closed to a generic 500
4. **`fault.Provider`** → root default HTTP policy for the semantic kind
5. **`HTTPStatus() int`** → legacy or explicit transport status; out-of-domain statuses fail closed to a generic 500
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
- **`info.MessageKey`** — the effective i18n key used to resolve the title: the explicit `MessageKey` when one was attached, otherwise `errors.<code>` (for telemetry, client-side i18n)
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

`ErrorRenderer` is one half of a pair. Its success-side mirror is `SuccessRenderer`, installed with `app.SetSuccessRenderer` and consulted by exactly one call site: `ctx.Render(status, data, opts...)`. Both follow the same shape-only contract — the renderer returns the body, the framework owns the write (status, JSON profile, bodiless-status rule) — so installing both gives every response the application produces — success and failure, including the 404/405/panic/bind responses no handler produced — one envelope, while no handler ever constructs it:

```go
// The application's own envelope types. Credo ships no envelope shape;
// both renderers are opt-in and nil by default.
type Envelope[T any] struct {
    Data      T      `json:"data"`
    Message   string `json:"message,omitempty"`
    Meta      any    `json:"meta,omitempty"`
    RequestID string `json:"request_id,omitempty"`
}

app.SetSuccessRenderer(func(c *credo.Context, info credo.RenderInfo) any {
    msg := ""
    if info.MessageKey != "" {
        msg = c.T(info.MessageKey)
    }
    return Envelope[any]{Data: info.Data, Message: msg, Meta: info.Meta, RequestID: c.RequestID()}
})

app.SetErrorRenderer(func(c *credo.Context, info credo.ErrorInfo) any {
    return map[string]any{
        "error":      map[string]any{"code": info.Problem.Code, "message": info.Problem.Title, "fields": info.Problem.Errors},
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

app.POST("/users", func(c *credo.Context) error {
    // ... create u ...
    return c.Render(http.StatusCreated, u, credo.RenderMessageKey("user.created"))
})
```

`RenderInfo` carries the two side channels every envelope eventually needs — an optional message key (`credo.RenderMessageKey`, resolved by the renderer via `ctx.T`) and structured metadata such as pagination (`credo.RenderMeta`). With no renderer installed both are silently dropped: the side channels exist only for an envelope to consume.

The seam is deliberately narrow on the success side: only `Render` consults the `SuccessRenderer`. The raw `Response` helpers — `JSON`, `XML`, `Text`, `Blob`, the streaming writers — are never intercepted, so handlers serving webhooks, health probes, or third-party-dictated shapes bypass the envelope by calling them directly. With no `SuccessRenderer` installed, `Render` falls back to plain `Response.JSON` and imposes nothing, so `Render` is safe to use as the default success verb from day one and the envelope becomes a one-line decision later.

The contracts mirror each other precisely: a nil return from the `SuccessRenderer` writes `info.Data` plain (selective enveloping), just as a nil from the `ErrorRenderer` keeps the default RFC 7807 body; a renderer that commits the response itself keeps full control and its return value is ignored; and a `SuccessRenderer` panic is caught by the same built-in recovery layer as any handler panic.

### Detecting Envelope Bypass

The narrow seam has one failure mode: once a team adopts a house envelope, a handler that habitually calls `Response().JSON` instead of `Render` skips it silently — legal by design, but usually unintentional. This cannot be enforced at compile time, so Credo ships a leak detector instead of a hard rule: with a `SuccessRenderer` installed **and** debug mode on (`WithDebug()` / `server.debug`), a handler that writes a body-carrying JSON response outside `Render` triggers a `WARN "credo: response bypassed the success envelope"` carrying the route pattern and name.

Deliberate raw endpoints declare themselves with route meta, per route or inherited from a group (route value overrides group):

```go
app.POST("/webhooks/stripe", stripeHandler).SetMeta(credo.MetaRawResponse, true)
app.Group("/callbacks").SetMeta(credo.MetaRawResponse, true)
```

Framework-internal JSON writes (`Render` itself, the error pipeline) never trigger it, and the non-JSON writers — `Text`, `Blob`, `XML`, the streaming writers — are exempt by design: they are never envelope targets. Production (non-debug) runs never pay for or emit the diagnostic.

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
return credo.NewHTTPError(http.StatusUnprocessableEntity, "stock_conflict").
    WithMessageKey("order.stock_conflict").
    WithInternal(err)
```

`HTTPStatus() int` remains supported after semantic providers for legacy and
explicit transport-specific errors. Store sentinels retain it only as a
deprecated compatibility bridge.

---

## Domain Errors (Service Layer)

For service-layer sentinel errors, author the stable machine code first and attach a domain-organized locale key for presentation:

```go
var (
    ErrUserNotFound = credo.NewHTTPError(404, "user_not_found").
        WithMessageKey("user.not_found")
    ErrEmailExists = credo.NewHTTPError(409, "email_exists").
        WithMessageKey("user.email_exists")
    ErrRoleNotFound = credo.NewHTTPError(422, "role_not_found").
        WithMessageKey("user.role_not_found")
)
```

Wrap internal errors with `WithInternal`:

```go
func (s *UserService) Create(ctx context.Context, input CreateInput) (*User, error) {
    exists, err := s.repo.EmailExists(ctx, input.Email)
    if err != nil {
        return nil, credo.NewHTTPError(500, "user_create_failed").
            WithMessageKey("user.create_failed").WithInternal(err)
    }
    if exists {
        return nil, ErrEmailExists
    }
    // ...
}
```

Add translations in locale files — explicit keys keep their own names; default (key-less) errors are localized through `errors.<code>`:

```json
{
    "user.not_found": "User not found.",
    "user.email_exists": "This email address is already registered.",
    "user.create_failed": "An error occurred while creating the user.",
    "errors.not_found": "Not found."
}
```

---

## Best Practices

1. **Return errors, don't write them** — let the error pipeline decide format
2. **Use sentinel errors** for known domain conditions (4xx)
3. **Use `WithInternal`** for server errors (5xx) — separates client message from debug info
4. **Author machine codes deliberately** — they are the wire contract your clients switch on; declare domain errors as sentinels so each code is written once
5. **Add translations** for your explicit message keys, and `errors.<code>` entries for defaults you want localized
6. **Never leak internal errors** — `WithInternal` ensures they are logged but not sent to the client
7. **Never pass request-derived text as a code** — map it onto predeclared codes; a malformed code panics by design
