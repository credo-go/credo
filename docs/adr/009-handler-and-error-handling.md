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
    MessageKey string          // effective i18n key used to resolve Problem.Title
    Problem    *ProblemDetails // classified RFC 7807 Problem Details
}
```

A non-nil return is encoded with the application's JSON profile and written with `info.Problem.Status` (mutable before returning — the renderer's one status seam); returning nil keeps the default RFC 7807 body, so a renderer can also run purely for its side effects — setting `Retry-After` / `WWW-Authenticate` headers, reporting to Sentry — and let the default shape stand. The renderer is invoked for HEAD too (headers again), with the returned body discarded. For the rare non-JSON error response, the renderer may commit the response itself through the Context; once committed, the return value is ignored. That committed check also means a renderer cannot double-write by both committing and returning.

Splitting the pipeline this way keeps custom renderers total-function simple: a renderer maps `ErrorInfo` to a body value and never re-implements classification, logging, status writing, or the HEAD/committed guards, so those framework concerns cannot be accidentally omitted — or accidentally diverged, the failure mode of the earlier write-it-yourself contract, where every renderer repeated `WriteHeader` and encoding by hand and a missing write triggered a warn-and-fallback path. `ErrorInfo.Err` keeps the original error available for cross-cutting use; `ErrorInfo.MessageKey` carries the effective i18n key — the explicit `MessageKey` when one was attached, otherwise the classification key `errors.<code>` — for client-side i18n or telemetry grouping. The machine-readable `code` and structured `details` are read from `info.Problem.Code` / `info.Problem.Details` — `ErrorInfo` deliberately gains no duplicate fields, so the classified `ProblemDetails` stays the single source of truth for wire-facing values. A renderer that panics is caught and a minimal 500 emitted.

The renderer is the error-side half of the response-envelope story: paired with `SuccessRenderer` (ADR-008's `Context.Render` seam, `func(ctx, RenderInfo) any` — the same shape-only contract, with `RenderInfo` carrying status, data, and the optional message-key/meta side channels), an application defines one envelope for every response it produces without a single handler knowing the envelope exists.

### Where the Pipeline Begins

The pipeline covers every response the application produces, including the ones no handler produced: 404 and 405, `BindBody`/`BindQuery` failures, `max_body_bytes` overruns, and panics all reach `ErrorRenderer` as classified `ProblemDetails`, exactly like a returned error.

It cannot cover what `net/http` rejects before routing. A request whose headers exceed `max_header_bytes` or `max_header_value_count` (431), whose request line or Host is malformed (400), or whose transfer encoding is unsupported (501) is answered by the standard library writing directly to the connection, while no `http.Handler` has been invoked. Those responses are `text/plain`, carry no request ID, are not logged — not even through the `http.Server.ErrorLog` bridge — and `ErrorRenderer` is never called for them.

Credo does not try to hide this. Intercepting those responses would mean owning the connection loop instead of `net/http`'s, which trades the standard library's hardened HTTP/1.1 and HTTP/2 parsing for a bespoke one — a bad exchange for cosmetic uniformity on three malformed-request paths. The boundary is documented instead (see the [error-handling guide](../guides/error-handling.md)), so clients know to check `Content-Type` before parsing a non-2xx body as `application/problem+json`.

### Default Error Detection Order

```
1. Response already committed → no-op (response is in-flight)
2. validation.Errors → 422 Unprocessable Entity with field errors
3. *BindError → 400 Bad Request with a typed decode-reason errors entry
4. *HTTPError → status/code from the stored fields, title from MessageKey or the errors.<code> chain; invalid stored fields fail closed to a generic 500
5. fault.Provider → root default HTTP policy from the transport-neutral semantic kind
6. HTTPStatus() int interface → legacy or explicit transport status; out-of-domain statuses fail closed to a generic 500
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

`code` and `details` are RFC 7807 extension members. `code` is the primary, stable machine identity of the error — never derived from presentation. It is materialized at construction: the explicit `NewHTTPError` code argument, or the frozen default for the status from Credo's committed, fixture-locked `statusToCode` table (`404` → `"not_found"`, `413` → `"request_entity_too_large"`); a status absent from the table yields the stable fallback `"http_<status>"` (`499` → `"http_499"`). Codes obey the grammar `^[a-z0-9]+(_[a-z0-9]+)*$`; an organization that standardizes on a different casing projects it in its `ErrorRenderer` by mutating `info.Problem.Code` and returning nil. Every response the default pipeline produces carries a non-empty code. Validation failures carry `"code": "validation_failed"`; bind failures carry the decode reason (`"syntax"`, `"type_mismatch"`, …) as the top-level code — the same value as their single `errors[]` entry. `details` carries `HTTPError.Details` verbatim (application JSON profile; client-safe data only).

**Considered and rejected — deriving `code` from the message key.** v0.10.0 shipped the inverse model: the wire code defaulted to the last dot segment of the i18n `MessageKey` (`"user.email_exists"` → `"email_exists"`), and a dotless key yielded no code. That coupling made the machine identity a by-product of human text — renaming a locale key silently renamed the wire code, a literal message produced no code at all, and the stability contract clients depend on rested on a naming habit. The identity now flows the other way: the stable code is authored first, and presentation (`MessageKey`, locale lookups) hangs off it.

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
    Code       string // stable machine-readable code, materialized at construction
    MessageKey string // optional presentation key (title only); empty by default
    Details    any    // structured client-safe detail (RFC 7807 "details" extension)
    Internal   error  // underlying error (not exposed to client)
}
```

`NewHTTPError(status int, code ...string)` validates strictly and panics on misuse: a status outside `100..999`, more than one code argument, or a code violating the grammar (including an explicitly empty one). This is a deliberate cause-based policy, not a phase-based one — a malformed code constant is a developer invariant violation even when the constructor happens to run during a request, so it fails loudly on first execution instead of publishing a malformed wire code. During a request, built-in recovery converts the panic into a generic 500 that exposes none of the invalid value. The same fail-closed rule guards the classification boundary for directly constructed `HTTPError` values and out-of-domain legacy `HTTPStatus()` results.

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

Sentinels are constructed through `NewHTTPError`, so their `Code` is materialized (`ErrNotFound.Code == "not_found"`) and their `MessageKey` is empty — presentation is opt-in, and direct readers or marshalers of an `HTTPError` observe exactly the stored fields.

`WithInternal(err)` wraps an underlying error without exposing it; `WithMessageKey` attaches a presentation key or literal title; `WithDetails` sets the structured wire extension. All three are copy-on-write, so sentinels stay immutable:

```go
return credo.ErrNotFound.WithInternal(fmt.Errorf("user %d not found in DB", id))
// Client sees: 404 Not Found, code "not_found"
// Server logs: user 42 not found in DB

return credo.NewHTTPError(409, "email_exists").
    WithMessageKey("user.email_exists").
    WithDetails(map[string]string{"field": "email"})
```

### i18n Integration

Titles are resolved at the consumption point through one of two chains, selected by whether the error carries an explicit `MessageKey`:

- **Explicit key** — the existing 3-level fallback: i18n bundle (via `app.UseI18n()` and the request locale) → `builtInMessages` English defaults → the key itself as a literal (works for literal messages and domain-organized locale keys).
- **No key (the default)** — the effective classification key is `errors.<code>`: locale bundle lookup for that key → `http.StatusText(status)` → `"HTTP <status>"` when the status has no standard text. `ErrorInfo.MessageKey` carries this effective key to the renderer.

The `errors.` prefix exists because the locale bundle is one flat namespace shared by application, validation, bind, and error messages — a bare `i18n["conflict"]` lookup could be captured by an unrelated application message. Resolving lazily at render time (rather than pre-translating when the error is created) keeps `HTTPError` values locale-independent and lets the request locale drive the final message.

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

The constants remain as explicit presentation keys — `NewHTTPError(503, "request_timeout").WithMessageKey(credo.MsgKeyRequestTimeout)` keeps a built-in title on a non-default status. Construction never auto-attaches a message key; default titles flow through the `errors.<code>` chain instead.

### Panic Recovery

Panic recovery is built into the framework as the outermost handler layer (applied in `compile()`). It catches panics from all middleware and handlers, logs the stack trace via `ctx.Logger()`, and returns `ErrInternalServerError`. `http.ErrAbortHandler` is re-panicked. Developer-misuse panics from strict constructors (`NewHTTPError` validation) land here too: the request renders a generic 500 and the invalid value never reaches the wire.

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
