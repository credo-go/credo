# Error Handling

Credo handlers return errors and one framework pipeline classifies, logs,
localizes, and renders them:

```go
app.GET("/users/{id}", func(ctx *credo.Context) error {
    user, err := service.Find(ctx.Context(), ctx.Request().RouteParam("id"))
    if errors.Is(err, store.ErrNotFound) {
        return credo.NewHTTPError(http.StatusNotFound, "user_not_found")
    }
    if err != nil {
        return err // generic 500; err text is not exposed
    }
    return ctx.Response().JSON(http.StatusOK, user)
})
```

## Default response

```json
{
  "success": false,
  "code": "user_not_found",
  "message": "Not Found"
}
```

The HTTP status is carried by the status line. The default body contains
`success:false`, stable `code`, resolved `message`, optional client-safe
`details`, and optional field `errors`. Its media type is `application/json`.
The body is deterministically encoded.

`success:false` is an error discriminator. Ordinary successful calls remain
plain payloads and do not automatically gain `success:true`. Use
`SuccessRenderer` with `Context.Render` if your API requires a symmetric
envelope.

## HTTPError

```go
return credo.NewHTTPError(http.StatusConflict, "email_exists").
    WithMessageKey("user.email_exists").
    WithDetails(map[string]string{"field": "email"}).
    WithInternal(err)
```

- `Status` is the HTTP status.
- `Code` is stable machine identity.
- `MessageKey` is optional presentation identity and is never used to derive
  Code.
- `Details` is client-safe structured data.
- `Internal` is logged but never exposed.

Builders are copy-on-write, so shared sentinels such as `ErrNotFound` remain
safe. `NewHTTPError` accepts zero or one code. A missing code comes from the
frozen HTTP-status table. Codes use lowercase snake case. Invalid constructor
input is programmer misuse and panics; request recovery converts it to a safe
generic 500.

Hard-coded text works without enabling i18n because an explicit missing key is
also its literal fallback:

```go
return credo.NewHTTPError(http.StatusNotFound, "record_not_found").
    WithMessageKey("Kayıt bulunamadı")
```

## Validation and bind failures

Validation errors use 422 and top-level code `validation_failed`:

```json
{
  "success": false,
  "code": "validation_failed",
  "message": "Validation Failed",
  "errors": [
    {"field":"email","code":"required","message":"email is required"}
  ]
}
```

Bind/decode errors use 400 and top-level code `bind_failed`. The nested entry's
code is the exact reason (`syntax`, `type_mismatch`, `empty_body`,
`trailing_data`, `duplicate_field`, `unknown_field`, or `invalid_value`). This
lets clients switch on the broad class at the top and the actionable cause in
`errors[0].code`.

## Localization and message keys

Credo does not invent `errors.`, `v.`, or `bind.` prefixes. An exact message key
is selected in this order:

1. explicit `HTTPError.MessageKey` or `ValidationError.MessageKey`;
2. `I18nConfig.ResolveMessageKey`;
3. bare code/reason.

Large applications can opt into their own namespaces:

```go
ResolveMessageKey: func(ref credo.MessageRef) string {
    switch ref.Scope {
    case credo.MessageScopeValidation:
        return "validation." + ref.Code
    case credo.MessageScopeBind:
        return "request." + ref.Code
    default:
        return "problem." + ref.Code
    }
},
```

The resolver-produced key is never leaked after a miss. HTTP errors fall back
to built-in/HTTP status text, validation to the rule message, and bind errors
to their safe decode message. See the [localization guide](localization.md).

## Custom ErrorRenderer

Install a shape-only renderer before the app is finalized:

```go
app.SetErrorRenderer(func(ctx *credo.Context, info *credo.ErrorInfo) any {
    return map[string]any{
        "ok":      false,
        "error":   info.Code,
        "message": info.Message,
        "details": info.Details,
        "fields":  info.Errors,
    }
})
```

`ErrorInfo` contains the original error, effective status, code, exact message
key, already resolved message, details, and field errors. It is request-scoped;
do not retain it. The framework writes the returned body and owns JSON options,
HEAD/bodiless rules, and the status.

Returning nil keeps the default body, which is useful for side effects:

```go
app.SetErrorRenderer(func(ctx *credo.Context, info *credo.ErrorInfo) any {
    telemetry.Capture(info.Err)
    ctx.Response().Header().Set("X-Error-Code", info.Code)
    return nil
})
```

Change the outgoing status by mutating `info.Status` before return. An invalid
status fails closed to a generic 500. For a non-JSON body, write and commit
through `ctx.Response()`; after commit the framework ignores the return value.

## RFC 9457 Problem Details

Problem Details is a first-party opt-in:

```go
app.SetErrorRenderer(credo.RFC9457ErrorRenderer())
```

It writes `application/problem+json`, uses `about:blank` by default, and carries
Credo's `code`, `details`, and `errors` as extension members. To map codes to
problem-type URIs:

```go
app.SetErrorRenderer(credo.RFC9457ErrorRenderer(credo.RFC9457Config{
    ResolveType: func(info *credo.ErrorInfo) string {
        return "https://api.example.com/problems/" + info.Code
    },
}))
```

## Success and error envelopes

`ErrorRenderer` covers every error produced after a request reaches the app,
including 404/405, bind failures, body-limit failures, and panics.
`SuccessRenderer` is consulted only by `Context.Render`; raw Response helpers
remain escape hatches.

```go
app.SetSuccessRenderer(func(_ *credo.Context, info credo.RenderInfo) any {
    return map[string]any{"success": true, "data": info.Data, "meta": info.Meta}
})
app.SetErrorRenderer(func(_ *credo.Context, info *credo.ErrorInfo) any {
    return map[string]any{"success": false, "code": info.Code, "message": info.Message}
})

// Uses SuccessRenderer.
return ctx.Render(http.StatusOK, user)

// Deliberately bypasses it.
return ctx.Response().JSON(http.StatusOK, webhookPayload)
```

## What the pipeline cannot cover

`net/http` may reject a connection before invoking Credo: oversized headers
(431), malformed request/Host (400), and unsupported transfer encoding (501).
Those responses are standard-library plain text, carry no Credo request ID or
access log, and never call `ErrorRenderer`. Clients should inspect Content-Type,
or a front proxy should normalize these boundary errors.

## Panics and committed responses

Built-in recovery catches application panics and emits a safe generic 500.
`WithoutRecover` disables it; `middleware.Recover` provides scoped policy.
Errors returned after a response is committed or hijacked are logged and never
written over the in-flight response.
