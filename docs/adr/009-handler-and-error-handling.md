# ADR-009: Handler and Error Handling

**Status:** Accepted **Date:** 2026-03-01 **Last revised:** 2026-08-26 **Depends on:** ADR-008, ADR-013

## Accepted pre-v1 amendments

**2026-09-05; implementation pending.** The [bootstrap contract](../specs/bootstrap-and-di-lifecycle.md) adds a lifecycle-admission rejection path: HTTP 503, machine code `service_unavailable`, default message/envelope/encoder and HEAD/body rules, with no preparation, DI or application callbacks. Custom renderers, message-key resolvers, JSON callbacks, middleware and HTTP feature callbacks may depend on closed resources, so this terminal response bypasses them. Stopped admission takes precedence over a cached handler or preparation failure; an admitted preparation failure retains the repeatable developer-error behavior. Prepared stopping requests still use ordinary drain.

The [HTTP contract](../specs/http-features.md) moves request recovery and final error rendering into framework-owned execution. Error and success renderer registration become single-install UseErrorRenderer/UseSuccessRenderer; body-shaping/nil-result and committed-response semantics stay intact. Recovery is default-on with WithRecoverConfig and WithoutRecover; error classification/rendering remain active independently. Preparation failures and other goroutines remain outside request recovery.

G4c is accepted: with recovery enabled, pre-commit callback panics use 500; failure in error rendering falls back to the callback-free default encoder/body. Detector failures retain the cached default language and are never retried. A post-response ResultFilter panic preserves the response, emits a framework diagnostic and drops that access record. With recovery disabled, callback panics propagate after cleanup. ErrAbortHandler always propagates.

Compression finalization errors are logged. After commitment, never append a second JSON body; abort incomplete transfer where required and preserve the actual committed status in logs. The [HTTP failure contract](../specs/http-features.md#callback-and-finalization-failures) defines these boundaries. The sections below describe the current implementation.

## Context

Go's `http.Handler` has no error return. Writing errors inside every handler duplicates classification, logging, localization, response shape, HEAD rules, and committed-response guards. Credo needs one pipeline while still allowing applications to own their public envelope.

RFC 7807 was previously the pipeline's internal and default representation. That coupled classification to one presentation standard and made custom envelopes work through an RFC-shaped intermediate value. RFC 9457 now obsoletes RFC 7807, and most Credo applications need a smaller default body.

## Decision

### Handler and pipeline

```go
type Handler func(ctx *Context) error
```

A returned error is classified, localized, logged when appropriate, offered to the configured renderer, and written centrally. The same path handles 404, 405, bind failures, body-limit failures, and recovered panics.

The normalized model is independent of any wire format:

```go
type ErrorInfo struct {
    Err        error
    Status     int
    Code       string
    MessageKey string
    Message    string
    Details    any
    Errors     []validation.ValidationError
}

type ErrorRenderer func(ctx *Context, info *ErrorInfo) any
```

`ErrorInfo` is request-scoped and must not be retained. `Err` is the original error for telemetry and `errors.As`; `Message` is already localized; the other fields are normalized client-safe values. A renderer returns only the body shape. The framework still owns status writing, JSON encoding, HEAD and body-forbidding statuses, and committed/hijacked guards.

Returning nil keeps the default body. A renderer may set headers. It may also commit a response itself as the full-control escape; any returned value is then ignored. The renderer may change `info.Status` before returning. The framework reads the effective status afterward; an invalid override fails closed to a generic 500 and discards the renderer body. A renderer panic is recovered by the error-pipeline guard and also yields the minimal generic 500.

### Default Credo error envelope

The default response is JSON with `Content-Type: application/json; charset=utf-8`:

```go
type ErrorResponse struct {
    Success bool      `json:"success"`
    Error   ErrorBody `json:"error"`
}

type ErrorBody struct {
    Code       string                       `json:"code"`
    Message    string                       `json:"message"`
    Details    any                          `json:"details,omitempty"`
    Violations []validation.ValidationError `json:"violations,omitempty"`
}
```

```json
{
  "success": false,
  "error": {
    "code": "user_not_found",
    "message": "Kayıt bulunamadı"
  }
}
```

The top level carries only the `success` discriminator; everything about the error itself lives in the nested `error` object. This keeps envelope-level and error-level fields at separate altitudes and mirrors a symmetric success envelope pairing `success` with a data payload. Clients read `success`, then take `data` or `error` as one unit.

`success` is always present and false on the default error body. It is an error discriminator, not a promise that normal successes gain `success:true`. `Response.JSON` remains a raw payload API. Applications wanting a symmetric success envelope install `SuccessRenderer` and use `Context.Render`.

Validation uses top-level code `validation_failed`; bind/decode uses `bind_failed`. A bind entry in `violations[]` keeps its exact reason (`syntax`, `type_mismatch`, and so on). This deliberately separates the broad error class from the actionable per-violation cause.

The array is named `violations`, not `errors` or `fields`, because its entries are rule violations of two kinds: field-scoped validation failures and document-scoped bind (body-contract) failures whose `field` may be empty (a JSON syntax error concerns the whole body). `fields` would misname the document-scoped entries, and `error.errors` stutters; `violations` is honest for both (compare Google AIP-193 `field_violations`).

Framework-owned error JSON is deterministic even if the application disables deterministic map ordering for ordinary responses.

### Stable error codes

`HTTPError.Code` is the primary machine identity. `NewHTTPError(status, code...)` accepts zero or one code. With no explicit code, Credo uses the frozen status table and then `http_<status>` for unknown statuses. Codes obey `^[a-z0-9]+(_[a-z0-9]+)*$`.

Invalid status/code constructor input is developer misuse and panics even when the call happens during a request; recovery fails closed. Malformed directly constructed values and invalid legacy `HTTPStatus()` results also classify as generic 500 without publishing their fields. This is a deliberate opinionated wire convention. Applications requiring another casing can project it in an `ErrorRenderer`.

`MessageKey` is presentation identity and never determines `Code`. `WithMessageKey`, `WithDetails`, and `WithInternal` are copy-on-write so shared sentinels remain immutable.

### Message-key resolution

Credo never prepends `errors.`, `http.`, `v.`, or `bind.`. Resolution is:

1. an explicit `MessageKey`, used as an exact key;
2. `I18nConfig.ResolveMessageKey(MessageRef{Scope, Code})`, if configured;
3. the bare code/reason.

Scopes distinguish error, validation, and bind lookups. A resolver returning an empty key is misuse and fails closed through recovery. Namespace prefixes remain a recommended application convention, not a framework requirement.

For HTTP/domain errors, an explicit key miss falls back to the literal key so hard-coded messages work without i18n. An implicit-key miss falls back to a built-in English message, `http.StatusText`, then `HTTP <status>`; resolver keys therefore never leak. Validation misses retain the rule's safe message. Bind misses retain the safe built-in decode message.

### RFC 9457 opt-in

RFC Problem Details remains first-party but is not the core model or default:

```go
app.SetErrorRenderer(credo.RFC9457ErrorRenderer())
```

The renderer writes `application/problem+json` and maps normalized information to `type`, `title`, `status`, `detail`, and `instance`, with `code`, `details`, and `violations` as extension members (the extension vocabulary matches the default envelope). `about:blank` is the default type; `RFC9457Config.ResolveType` can supply application problem-type URIs. The public `ProblemDetails` helper type is retained for this adapter.

### Where the pipeline begins

The pipeline starts only after `net/http` invokes the app. Standard-library rejections before routing—oversized headers (431), malformed request/Host (400), and unsupported transfer encoding (501)—are plain text written directly to the connection. They have no request ID or access log and never call `ErrorRenderer`. Clients must inspect Content-Type or use a front proxy when every error must share one envelope.

### Panic recovery

Built-in recovery is the outermost application layer and converts panics to a generic 500. `http.ErrAbortHandler` is re-panicked. `WithoutRecover` disables the built-in; `middleware.Recover` remains available for group/route policy.

## Consequences

- Classification and rendering no longer depend on RFC fields.
- The common default envelope is smaller and applications can read the final localized message directly in a renderer.
- RFC 9457 interoperability remains a one-line opt-in.
- Changing from Problem Details to `ErrorResponse`, changing the default media type, changing `ErrorRenderer` to `*ErrorInfo`, changing bind's top-level code, and nesting the envelope (flat fields → `error` object, `errors[]` → `violations[]`) are pre-v1 breaking migrations and must be called out separately.
- Uniformity still stops at pre-routing standard-library rejections.
