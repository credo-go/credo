# ADR-009: Handler and Error Handling

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** ADR-008 **Related:** ADR-010, ADR-011, ADR-013, ADR-021 **Specification:** [Context spec](../specs/context.md), [HTTP features spec](../specs/http-features.md) **Guide:** [Error handling](../guides/error-handling.md)

## Context

Go's `http.Handler` has no error return. Writing errors inside every handler duplicates classification, logging, localization, response shape, HEAD rules, and committed-response guards. Credo needs one pipeline while still allowing applications to own their public envelope.

Making one presentation standard the pipeline's internal model couples classification to it: with RFC 9457 Problem Details (which obsoletes RFC 7807) as the model, every custom envelope would work through an RFC-shaped intermediate value. Most applications also want a smaller default body than a problem document.

## Decision

### Handler and pipeline

```go
type Handler func(ctx *Context) error
```

A returned error is classified, localized, logged when appropriate, offered to the configured renderer, and written centrally. The same path handles 404, 405, bind failures, body-limit failures, and recovered panics. Recovery and final error rendering run in the framework-owned [request executor](../specs/http-features.md#request-execution); classification and rendering stay active when recovery is disabled.

The normalized model, `ErrorInfo`, is independent of any wire format. It carries the original error (`Err`, for telemetry and `errors.As`), the effective `Status`, the stable `Code`, the exact `MessageKey`, the already localized `Message`, client-safe `Details`, and the `Violations`. An `ErrorRenderer` (`func(*Context, *ErrorInfo) any`) returns only the body shape; the [Context spec](../specs/context.md#render-and-the-envelope-seam) lists the declarations. `ErrorInfo` is request-scoped and must not be retained. The framework still owns status writing, JSON encoding, HEAD and body-forbidding statuses, and committed/hijacked guards. The error and the success renderer are each installed once, with `UseErrorRenderer` and `UseSuccessRenderer`.

Returning nil keeps the default body. A renderer may set headers. It may also commit a response itself as the full-control escape; any returned value is then ignored. The renderer may change `info.Status` before returning. The framework reads the effective status afterward; an invalid override fails closed to a generic 500 and discards the renderer body. A renderer panic is recovered by the error-pipeline guard, whether or not request recovery is enabled, and also yields the minimal generic 500.

The [lifecycle 503](../specs/lifecycle.md#lifecycle-rejection-503) of an App that is stopped, or stopping without a prepared handler, invokes no application callback — renderer, message-key resolver, locale detector, JSON options, middleware or feature callback — because any of them may capture a component that has already shut down. When error rendering itself fails, the framework writes a default 500 without any custom renderer, resolver or encoder callback, so a failing extension is never invoked recursively ([HTTP features spec](../specs/http-features.md#callback-and-finalization-failures)).

### Default error envelope

The default error envelope (`ErrorResponse`) is JSON with `Content-Type: application/json; charset=utf-8`:

```json
{
  "success": false,
  "error": {
    "code": "user_not_found",
    "message": "Kayıt bulunamadı"
  }
}
```

The nested `error` object always has `code` and `message`, and carries `details` and `violations` when they are present. The top level carries only the `success` discriminator; everything about the error itself lives in the nested `error` object. This keeps envelope-level and error-level fields at separate altitudes and mirrors a symmetric success envelope pairing `success` with a data payload. Clients read `success`, then take `data` or `error` as one unit.

`success` is always present and false on the default error body. It is an error discriminator, not a promise that normal successes gain `success:true`. `Response.JSON` remains a raw payload API. Applications wanting a symmetric success envelope install `SuccessRenderer` and use `Context.Render`.

Validation uses top-level code `validation_failed`; bind/decode uses `bind_failed`. A bind entry in `violations[]` keeps its exact reason (`syntax`, `type_mismatch`, and so on). This deliberately separates the broad error class from the actionable per-violation cause.

The array is named `violations` because its entries are rule violations of two kinds: field-scoped validation failures and document-scoped bind (body-contract) failures whose `field` may be empty (a JSON syntax error concerns the whole body). The name is honest for both (compare Google AIP-193 `field_violations`).

The envelope's JSON is deterministic whatever the application's JSON profile ([ADR-021](021-json-output-profile.md#default-error-bodies-always-sort-map-keys)).

### Stable error codes

`HTTPError.Code` is the primary machine identity. `NewHTTPError(status, code...)` accepts zero or one code. With no explicit code, Credo uses the frozen status table and then `http_<status>` for unknown statuses. Codes obey `^[a-z0-9]+(_[a-z0-9]+)*$`, a deliberately opinionated wire convention.

Invalid status/code constructor input is developer misuse and panics even when the call happens during a request; recovery fails closed. Malformed directly constructed values and invalid `HTTPStatus()` results also classify as generic 500 without publishing their fields.

`MessageKey` is presentation identity and never determines `Code`. `WithMessageKey`, `WithDetails`, and `WithInternal` are copy-on-write so shared sentinels remain immutable.

### An explicit status wins

An explicitly constructed `HTTPError` wins over a validation error wrapped inside it. `NewHTTPError(409, "tenant_conflict").WithInternal(vErrs)` renders 409 with code `tenant_conflict`, not 422 `validation_failed`: the application stated the status, and a classification that searches the whole unwrap chain for `validation.Errors` first would overrule it with a cause the application chose to keep internal. The wrapped errors stay reachable through `ErrorInfo.Err` for logging and for a renderer that wants them. Validation errors that no `HTTPError` wraps — `validation.Errors`, or a single `*ValidationError` such as a `Validate` method may return from `validation.NewError` — still render as 422 `validation_failed`.

The companion rule is in [ADR-011](011-validation-strategy.md#rule-errors-client-messages-and-internal-failures): a validation rule error that is not a `*ValidationError` is internal, leaves `Validate` as it would leave a handler, and is classified here — a plain error is a 500 whose text is logged, not rendered, and a fault keeps its mapped status.

### Message-key resolution

Presentation keys are exact, and Credo adds no namespace prefix to a code; the [i18n spec](../specs/i18n.md#message-key-resolution) gives the selection order and the fallback on every kind of miss, and [ADR-013](013-internationalization.md#exact-keys-and-application-owned-namespaces) the rationale.

### RFC 9457 opt-in

RFC Problem Details remains first-party but is not the core model or default:

```go
app.UseErrorRenderer(credo.RFC9457ErrorRenderer())
```

The renderer writes `application/problem+json` and maps normalized information to `type`, `title`, `status`, `detail`, and `instance`, with `code`, `details`, and `violations` as extension members (the extension vocabulary matches the default error envelope). `about:blank` is the default type; `RFC9457Config.ResolveType` can supply application problem-type URIs. `ProblemDetails` is the adapter's body type.

### Where the pipeline begins

The pipeline starts only after `net/http` invokes the app. Standard-library rejections before routing—oversized headers (431), malformed request/Host (400), and unsupported transfer encoding (501)—are plain text written directly to the connection. They have no request ID or access log and never call `ErrorRenderer`. Clients must inspect Content-Type or use a front proxy when every error must share one envelope.

### Panic recovery

Built-in recovery is the outermost application layer and converts panics to a generic 500. `http.ErrAbortHandler` is re-panicked. `WithRecoverConfig` customizes its logger and stack capture; `WithoutRecover` disables it. There is no group/route recovery; scoped policy is ordinary middleware ([ADR-010](010-middleware-architecture.md)).

## Rejected Alternatives

| Alternative | Reason |
| --- | --- |
| RFC 9457 Problem Details as the default body | `ErrorInfo` is independent of both wire formats and each body is a renderer over it, so the flip has no engineering content and would break every client's wire contract; the default error envelope is the smaller body, and RFC 9457 stays a one-line opt-in |
| Another code casing, or a configurable one | The snake-case code is a deliberate, opinionated wire convention; an application that needs another casing projects it in an `ErrorRenderer` |
| Framework-generated message-key prefixes | A hidden string rule; a namespace is an application convention, applied through the optional resolver ([ADR-013](013-internationalization.md#exact-keys-and-application-owned-namespaces)) |
| `errors` or `fields` as the violations array name | `error.errors` stutters; `fields` misnames the document-scoped bind entries |

## Consequences

- Classification and rendering are independent of any wire standard.
- The default error envelope is smaller than a problem document, and a renderer reads the final localized message directly.
- RFC 9457 interoperability remains a one-line opt-in.
- Uniformity still stops at pre-routing standard-library rejections.
