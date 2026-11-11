# Error Handling

Credo handlers return errors and one framework pipeline classifies, logs, localizes, and renders them:

```go
app.GET("/users/{id}", func(c *credo.Context) error {
    user, err := service.Find(c.Context(), c.Request().RouteParam("id"))
    if errors.Is(err, store.ErrNotFound) {
        return credo.NewHTTPError(http.StatusNotFound, "user_not_found")
    }
    if err != nil {
        return err // generic 500; err text is not exposed
    }
    return c.Response().JSON(http.StatusOK, user)
})
```

## Default response

```json
{
  "success": false,
  "error": {
    "code": "user_not_found",
    "message": "Not Found"
  }
}
```

The HTTP status is carried by the status line. The top level of the default body holds only the `success:false` discriminator; the nested `error` object carries the stable `code`, resolved `message`, optional client-safe `details`, and optional `violations`. Its media type is `application/json`. The body is deterministically encoded.

`success:false` is an error discriminator. Ordinary successful calls remain plain payloads and do not automatically gain `success:true`. Use `SuccessRenderer` with `Context.Render` if your API requires a symmetric envelope.

## HTTPError

```go
return credo.NewHTTPError(http.StatusConflict, "email_exists").
    WithMessageKey("user.email_exists").
    WithDetails(map[string]string{"field": "email"}).
    WithInternal(err)
```

- `Status` is the HTTP status.
- `Code` is stable machine identity.
- `MessageKey` is an optional catalog key for the message and is never used to derive Code. Its type is `credo.MessageKey`, so a key computed as a `string` needs a conversion.
- `Message`, set with `WithMessage`, is optional literal client text, never looked up; it wins over a key.
- `Details` is client-safe structured data. Client input belongs here, never in a key or a message.
- `Internal` is never exposed. It is logged: at Error for a 5xx (`credo: server error`), at debug level for a 4xx (`credo: client error`), so a client error adds no line at the default log level.

Builders are copy-on-write, so shared sentinels such as `ErrNotFound` remain safe. `NewHTTPError` accepts zero or one code. A missing code comes from the frozen HTTP-status table. Codes use lowercase snake case. Invalid constructor input is programmer misuse and panics; request recovery converts it to a safe generic 500.

Hard-coded text is a message, not a key:

```go
return credo.NewHTTPError(http.StatusNotFound, "record_not_found").
    WithMessage("Kayıt bulunamadı")
```

A key is never rendered as text. A key your catalogs do not define falls back to the built-in message for the code, then to the status text, and with `WithDebug` the pipeline logs a warning naming the key, the code and the route. `testutil.CheckMessageKeys` fails a test for each key in your sources that the catalogs do not define, the keys you pass to `c.T` and `c.TPlural` included ([localization guide](localization.md#keys-are-typed-and-checked)):

```go
func TestMessageKeys(t *testing.T) {
    testutil.CheckMessageKeys(t, "locales", ".")
}
```

Declare key constants with their type, `const KeyEmailTaken credo.MessageKey = "user.email_exists"`, so the check finds them.

## Domain errors

A service reports what went wrong in its own terms, without naming an HTTP status: it returns a `fault.Error`, a kind and a code of its own.

```go
var ErrDomainTaken = fault.New(fault.KindAlreadyExists, "tenant_domain_taken")

func (s *TenantService) Create(ctx context.Context, in CreateTenant) (*Tenant, error) {
    t, err := s.repo.Insert(ctx, in)
    if errors.Is(err, store.ErrAlreadyExists) {
        return nil, ErrDomainTaken.WithDetails(map[string]any{"domain": in.Domain}).WithInternal(err)
    }
    return t, err
}
```

The handler returns it unchanged:

```go
func (h *TenantHandler) Create(c *credo.Context) error {
    var in CreateTenant
    if err := c.Request().BindBody(&in); err != nil {
        return err
    }
    t, err := h.svc.Create(c.Context(), in)
    if err != nil {
        return err
    }
    return c.Response().JSON(http.StatusCreated, t)
}
```

The response is 409, the status of `fault.KindAlreadyExists`, with the code `tenant_domain_taken` and the details; the store error is the internal cause, logged and never rendered. `fault.New(kind)` without a code takes the kind as the code, and a store error the service returns unwrapped does the same: `already_exists`, `serialization` or `deadlock` rather than one `conflict` for every 409, so a client tells a duplicate from a failure a retry may clear. `WithDetails` and `WithInternal` return copies, so `errors.Is(err, ErrDomainTaken)` still matches.

Keep `HTTPError` for what only the HTTP boundary knows: a status the handler chooses, a message, a header-driven rejection.

## Validation and bind failures

Validation errors use 422 and top-level code `validation_failed`:

```json
{
  "success": false,
  "error": {
    "code": "validation_failed",
    "message": "Validation Failed",
    "violations": [
      {"field":"email","code":"required","message":"email is required"}
    ]
  }
}
```

Bind/decode errors use 400 and top-level code `bind_failed`. The nested entry's code is the exact reason (`syntax`, `type_mismatch`, `empty_body`, `trailing_data`, `duplicate_field`, `unknown_field`, or `invalid_value`). This lets clients switch on the broad class in `error.code` and the actionable cause in `error.violations[0].code`.

Two neighbouring outcomes are not `bind_failed`. A body whose `Content-Encoding` is not `identity` and that `app.UseDecompress` has not unwrapped is a 415 with the code `unsupported_content_encoding` (`credo.CodeUnsupportedContentEncoding`) and the encoding in `details.encoding`, returned before any decoder runs. A bind target the binder cannot fill — nil, not a pointer, or with a field of a type it does not bind — is a developer error: a 500 with the code `invalid_bind_target` (`credo.CodeInvalidBindTarget`), whose reason is logged as the internal cause and never sent to the client.

The array is named `violations` because it carries two kinds of entries: field-scoped validation failures and document-scoped bind failures. A bind entry's `field` may be empty when the violation concerns the whole body — a JSON syntax error, for example.

### Rule errors: client messages and internal failures

A custom rule tells the client what is wrong with a `*validation.Violation`, which `validation.NewViolation` builds; `Field` fills in the field path:

```go
validation.Field(&in.Country, validation.By(func(code string) error {
    if len(code) != 2 {
        return validation.NewViolation("country_code", "must be a 2-letter code")
    }
    return nil
}))
```

Any other error a rule returns is internal. Validation stops, and the error leaves `Validate` and `BindBody`/`BindQuery` unchanged, so the pipeline classifies it like any error a handler returns: a plain error is a 500 with the default message and its text in the log, never in the body, and a fault keeps its status — `store`'s unavailable kind is a 503. A rule that could not reach its database therefore reports a server failure the client may retry, not a claim that the input was invalid.

### An explicit status wins

An `HTTPError` you construct decides the status even when it wraps validation errors:

```go
return credo.NewHTTPError(http.StatusConflict, "tenant_conflict").WithInternal(vErrs)
```

renders 409 `tenant_conflict`, not 422. The wrapped errors stay in `ErrorInfo.Err` for the log and for an `ErrorRenderer` that wants them. Validation errors that no `HTTPError` wraps still render as 422 `validation_failed`.

## Localization and message keys

Credo does not invent `errors.`, `v.`, or `bind.` prefixes. An exact message key is selected in this order:

1. explicit `HTTPError.MessageKey` or `Violation.MessageKey` (an `HTTPError` with a `Message` is not looked up);
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

No key is ever rendered after a miss, explicit or resolved. HTTP errors fall back to built-in/HTTP status text, validation to the rule message, and bind errors to their safe decode message. See the [localization guide](localization.md).

## Custom ErrorRenderer

Install a shape-only renderer once, before the app prepares to serve. The window stays open after the container is finalized — by the first `Resolve` or an explicit `app.Finalize()` — so the renderer may be a method of a DI-resolved service; a second `UseErrorRenderer` call panics.

```go
app.UseErrorRenderer(func(c *credo.Context, info *credo.ErrorInfo) any {
    return map[string]any{
        "ok":         false,
        "error":      info.Code,
        "message":    info.Message,
        "details":    info.Details,
        "violations": info.Violations,
    }
})
```

`ErrorInfo` contains the original error, effective status, code, exact message key, already resolved message, details, and violations. It is request-scoped; do not retain it. The framework writes the returned body and owns JSON options, HEAD/bodiless rules, and the status.

Returning nil keeps the default body, which is useful for side effects:

```go
app.UseErrorRenderer(func(c *credo.Context, info *credo.ErrorInfo) any {
    telemetry.Capture(info.Err)
    c.Response().Header().Set("X-Error-Code", info.Code)
    return nil
})
```

Change the outgoing status by mutating `info.Status` before return. An invalid status fails closed to a generic 500. For a non-JSON body, write and commit through `c.Response()`; after commit the framework ignores the return value.

## RFC 9457 Problem Details

Problem Details is a first-party opt-in:

```go
app.UseErrorRenderer(credo.RFC9457ErrorRenderer())
```

It writes `application/problem+json`, uses `about:blank` by default, and carries Credo's `code`, `details`, and `violations` as extension members. To map codes to problem-type URIs:

```go
app.UseErrorRenderer(credo.RFC9457ErrorRenderer(credo.RFC9457Config{
    ResolveType: func(info *credo.ErrorInfo) string {
        return "https://api.example.com/problems/" + info.Code
    },
}))
```

## Success and error envelopes

`ErrorRenderer` covers every error produced after a request reaches the app, including 404/405, bind failures, body-limit failures, and panics. `SuccessRenderer` is consulted only by `Context.Render`; raw Response helpers remain escape hatches.

```go
app.UseSuccessRenderer(func(_ *credo.Context, info credo.RenderInfo) any {
    return map[string]any{"success": true, "data": info.Data, "meta": info.Meta}
})
app.UseErrorRenderer(func(_ *credo.Context, info *credo.ErrorInfo) any {
    return map[string]any{"success": false, "error": map[string]any{"code": info.Code, "message": info.Message}}
})

// Uses SuccessRenderer.
return c.Render(http.StatusOK, user)

// Deliberately bypasses it.
return c.Response().JSON(http.StatusOK, webhookPayload)
```

## Custom 404 and 405 responses

`app.StatusHandler(code, h)` replaces the router's own answer when no route matches (404) or when routes match the path but none serves the method (405, with `Allow` already set). Only those two codes are consulted, so any other code panics at registration — a handler for 403 or 500 would never run. A status handler is not an error handler: an error a handler or middleware returns, `credo.ErrNotFound` included, goes through the pipeline above, and `UseErrorRenderer` shapes its body.

```go
app.StatusHandler(http.StatusNotFound, func(c *credo.Context) error {
    return c.Response().HTML(http.StatusNotFound, notFoundPage)
})
```

## What the pipeline cannot cover

`net/http` may reject a connection before invoking Credo: oversized headers (431), malformed request/Host (400), and unsupported transfer encoding (501). Those responses are standard-library plain text, carry no Credo request ID or access log, and never call `ErrorRenderer`. Clients should inspect Content-Type, or a front proxy should normalize these boundary errors.

## Panics and committed responses

Built-in recovery catches application panics and emits a safe generic 500. `WithRecoverConfig` customizes its logger and stack capture; `WithoutRecover` disables it. It covers renderers, locale detectors and access-log filters as well as handlers. Errors returned after a response is committed or hijacked are logged and never written over the in-flight response.
