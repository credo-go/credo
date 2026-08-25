# Internationalization Guide

This guide explains how to use Credo's built-in internationalization support in application code. For low-level design details, see the [i18n Spec](../specs/i18n.md) and [ADR-013](../adr/013-internationalization.md).

All locale examples in this guide use JSON.

---

## What Credo Gives You

Credo's i18n support is intentionally small from the application's point of view:

- `app.UseI18n(...)` initializes i18n during startup
- `ctx.Locale()` returns the resolved locale for the current request
- `ctx.T(key, data...)` translates application messages by key

When i18n is active, Credo also translates:

- validation field messages returned from `BindBody()` and `BindQuery()`
- `credo.HTTPError` messages such as `credo.ErrNotFound`
- semantic `fault.Provider` errors such as store errors, through root HTTP policy
- legacy/explicit errors that expose only an HTTP status via `HTTPStatus() int`

You do not import a public `i18n` package. The implementation stays internal.

---

## Quick Start

Create a `locales/` directory in your project:

```text
locales/
├── en/
│   └── messages.json
└── tr/
    └── messages.json
```

`locales/en/messages.json`:

```json
{
  "messages.welcome": "Welcome",
  "messages.hello_name": "Hello, {{.name}}",
  "not_found": "Not found",
  "internal_server_error": "Internal server error",
  "required": "is required",
  "email": "must be a valid email address"
}
```

`locales/tr/messages.json`:

```json
{
  "messages.welcome": "Hos geldiniz",
  "messages.hello_name": "Merhaba, {{.name}}",
  "not_found": "Bulunamadı",
  "internal_server_error": "Sunucu hatası",
  "required": "zorunludur",
  "email": "gecerli bir e-posta adresi olmalidir"
}
```

Enable i18n during startup:

```go
package main

import (
    "log"
    "net/http"

    "github.com/credo-go/credo"
)

func main() {
    app, err := credo.New()
    if err != nil {
        log.Fatal(err)
    }

    if err := app.UseI18n(); err != nil {
        log.Fatal(err)
    }

    app.GET("/hello", func(ctx *credo.Context) error {
        return ctx.Response().JSON(http.StatusOK, map[string]string{
            "locale":  ctx.Locale(),
            "welcome": ctx.T("messages.welcome"),
        })
    })

    if err := app.Run(); err != nil {
        log.Fatal(err)
    }
}
```

With no arguments, `UseI18n` reads from `RawConfig` if the `i18n` key exists. If that key is missing, Credo falls back to:

- `dir = "locales/"`
- `default = "en"`
- language detection from the `Accept-Language` header

### Programmatic default catalog

For a deployment-independent default language, provide `Messages` and optional
field display names in `Fields`:

```go
if err := app.UseI18n(credo.I18nConfig{
    Default: "tr",
    Messages: credo.I18nMessages{
        "not_found":         "Kayıt bulunamadı",
        "validation_failed": "Doğrulama başarısız",
        "required":          "{{.field}} zorunludur",
    },
    Fields: credo.I18nFields{
        "email": "e-posta adresi",
    },
}); err != nil {
    log.Fatal(err)
}
```

These maps represent only `Default`, are copied at setup, and may be the sole
source. Add an explicit `Dir` or `DirFS` to layer multi-language files over
them; file values override exact collisions and map-only keys remain. Supplying
maps alone intentionally does not probe `./locales`. Use `DirFS` rather than an
outer language map when translations must be embedded.

---

## Locale Directory Layout

Credo expects one directory per locale.

```text
locales/
├── en/
│   ├── messages.json
│   └── fields.json
└── tr/
    ├── messages.json
    └── fields.json
```

Files:

- `messages.json`: normal application messages plus validation and HTTP error keys
- `fields.json`: optional display names for field-aware validation messages

An absent conventional `locales/` directory discovered by zero-config setup is
an inactive warning. An explicitly configured `Dir`, RawConfig directory, or
`DirFS` is a declared deployment dependency: missing, unreadable, or empty
sources return an error.

---

## `messages.json`

`messages.json` is a flat key-value JSON object.

Recommended namespaces:

- `messages.*` for normal application messages
- `emails.*` for email copy
- `pages.*` for HTML/template copy
- `validation.*` for validation rules
- `request.*` for decode (bind) error reasons
- `problem.*` for HTTP and top-level error messages

Credo does not add these prefixes. Without a resolver, framework lookups use
bare codes such as `required`, `syntax`, `not_found`, `validation_failed`, and
`bind_failed`. To use the recommended namespaces, configure them explicitly:

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

Example:

```json
{
  "messages.welcome": "Welcome",
  "messages.hello_name": "Hello, {{.name}}",
  "messages.user_created": "User created",
  "validation.required": "is required",
  "validation.length": "must be between {{.min}} and {{.max}} characters",
  "validation.email": "must be a valid email address",
  "validation.min": "must be at least {{.min}}",
  "validation.max": "must be at most {{.max}}",
  "validation.between": "must be between {{.min}} and {{.max}}",
  "request.type_mismatch": "must be of type {{.expected}}",
  "request.trailing_data": "request body must contain a single JSON value",
  "problem.bad_request": "Bad request",
  "problem.unauthorized": "Unauthorized",
  "problem.forbidden": "Forbidden",
  "problem.not_found": "Not found",
  "problem.conflict": "Conflict",
  "problem.internal_server_error": "Internal server error",
  "problem.bind_failed": "Malformed request",
  "problem.validation_failed": "Validation failed"
}
```

Template variables use Go's `text/template` syntax:

```json
{
  "messages.hello_name": "Hello, {{.name}}"
}
```

```go
ctx.T("messages.hello_name", map[string]any{"name": "Ada"})
```

Template syntax is validated eagerly while locale files are loaded. Invalid templates fail startup instead of silently breaking later at request time.

For count-dependent messages, define CLDR plural forms as an object and use `ctx.TPlural` — it picks the right form for the detected locale and exposes the count as `{{.count}}`:

```json
{
  "messages.items": {"one": "{{.count}} item", "other": "{{.count}} items"}
}
```

```go
ctx.TPlural("messages.items", 1) // "1 item"
ctx.TPlural("messages.items", 5) // "5 items"
```

`ctx.T` always renders the `other` form; reach for `ctx.TPlural` whenever the text varies with a number. If the count cannot be interpreted as a number, `ctx.TPlural` renders the `other` form rather than failing — like the key fallback, the helpers degrade gracefully instead of surfacing i18n errors to end users.

> **Trust model.** Locale files are code: templates can call methods on the data you pass to `ctx.T`, so review translation files (including community-contributed ones) like any other code change. Messages render as plain text (`text/template`); when you embed them in HTML, escape at the rendering layer — `html/template` does this automatically for interpolated strings.

---

## `fields.json` (Optional)

`fields.json` is only needed when your validation messages reference a field label.

`locales/en/fields.json`:

```json
{
  "email": "email address",
  "first_name": "first name"
}
```

`locales/tr/fields.json`:

```json
{
  "email": "e-posta adresi",
  "first_name": "ad"
}
```

Then your validation message can use `{{.field}}`:

```json
{
  "validation.required": "{{.field}} zorunludur"
}
```

Important behavior:

- the response `field` value stays technical and stable, such as `"email"`
- only the translated `message` uses the localized display name
- if `fields.json` is missing, Credo falls back to the raw field name

---

## Enabling i18n

Call `UseI18n` during startup, before the first request and before `Run()`. Treat it as a one-time setup step.

### Zero-Config

```go
if err := app.UseI18n(); err != nil {
    log.Fatal(err)
}
```

This means:

1. if `RawConfig` contains `i18n`, Credo reads it
2. otherwise Credo uses `locales/` and `en`
3. if no locales are found, i18n stays inactive

### Explicit Directory

```go
if err := app.UseI18n(credo.I18nConfig{
    Dir:     "./resources/locales",
    Default: "tr",
}); err != nil {
    log.Fatal(err)
}
```

### Config-Driven Setup

`config.json`:

```json
{
  "i18n": {
    "dir": "locales/",
    "default": "en"
  }
}
```

`main.go`:

```go
if err := app.UseI18n(); err != nil {
    log.Fatal(err)
}
```

Only `dir` and `default` are config-driven. `DirFS` and `Detect` are code-only.

---

## Embedded Locales with `go:embed`

When shipping a single binary, embed the locale files and pass an `fs.FS` to `DirFS`.

```go
package main

import (
    "embed"
    "io/fs"
    "log"

    "github.com/credo-go/credo"
)

//go:embed locales/*
var localeFS embed.FS

func main() {
    app, err := credo.New()
    if err != nil {
        log.Fatal(err)
    }

    locales, err := fs.Sub(localeFS, "locales")
    if err != nil {
        log.Fatal(err)
    }

    if err := app.UseI18n(credo.I18nConfig{
        DirFS:   locales,
        Default: "en",
    }); err != nil {
        log.Fatal(err)
    }
}
```

`DirFS` is read from the root of the provided filesystem, so `fs.Sub(...)` is the usual choice when your embedded files live under a `locales/` subdirectory.

---

## Language Detection and Resolution

By default, Credo reads the `Accept-Language` header.

If the client sends:

```text
Accept-Language: tr-TR,tr;q=0.9,en;q=0.8
```

and you only provide `tr/` and `en/`, Credo resolves the request locale to `tr`. `ctx.Locale()` returns that resolved locale, not the raw header value.

### Custom Detection

You can override detection completely:

```go
if err := app.UseI18n(credo.I18nConfig{
    Dir:     "locales/",
    Default: "en",
    Detect: func(r *http.Request) string {
        if lang := r.URL.Query().Get("lang"); lang != "" {
            return lang
        }
        return r.Header.Get("X-Language")
    },
}); err != nil {
    log.Fatal(err)
}
```

Your detector may return:

- a simple BCP 47 tag such as `en` or `tr`
- a more specific tag such as `en-US`
- a raw `Accept-Language` header value

Credo resolves that input against the locales you actually loaded.

---

## Translating Normal Application Messages

Use `ctx.T()` anywhere you build the response body.

```go
app.GET("/dashboard", func(ctx *credo.Context) error {
    return ctx.Response().JSON(http.StatusOK, map[string]any{
        "locale":  ctx.Locale(),
        "title":   ctx.T("messages.dashboard_title"),
        "welcome": ctx.T("messages.hello_name", map[string]any{"name": "Ada"}),
    })
})
```

Behavior summary:

- Credo first tries the resolved request locale, then the default locale
- if the key is still missing, Credo returns the key itself
- if i18n is inactive, Credo returns the key itself

That fallback makes it safe to adopt i18n incrementally.

---

## Automatic Validation Translation

Credo translates field-level validation messages automatically when a handler returns `validation.Errors`. The most common way this happens is through `BindBody()` or `BindQuery()`.

```go
package main

import (
    "net/http"

    "github.com/credo-go/credo"
    "github.com/credo-go/credo/validation"
)

type CreateUserInput struct {
    Name  string `json:"name"`
    Email string `json:"email"`
}

func (in *CreateUserInput) Validate() error {
    return validation.ValidateStruct(in,
        validation.Field(&in.Name, validation.Required[string](), validation.Length(2, 100)),
        validation.Field(&in.Email, validation.Required[string](), validation.Email()),
    )
}

func createUser(ctx *credo.Context) error {
    var input CreateUserInput
    if err := ctx.Request().BindBody(&input); err != nil {
        return err
    }

    return ctx.Response().JSON(http.StatusCreated, map[string]string{
        "message": ctx.T("messages.user_created"),
    })
}
```

With the namespace resolver shown above, Credo translates entries such as:

- `validation.required`
- `validation.length`
- `validation.email`
- `validation.min`
- `validation.max`
- `validation.between`

Decode failures use bind scope and exact reasons. With that resolver their keys
are `request.syntax`, `request.type_mismatch`, `request.invalid_value`, and so
on. `{{.field}}`, `{{.expected}}`, and `{{.offset}}` are available where
applicable. The top-level message uses error scope + `bind_failed`.

Example validation response:

```json
{
  "success": false,
  "code": "validation_failed",
  "message": "Validation Failed",
  "errors": [
    {
      "field": "email",
      "code": "required",
      "message": "e-posta adresi zorunludur"
    }
  ]
}
```

Notes:

- the field list stays machine-friendly
- each field message is localized
- Credo first tries the request locale, then the default locale, then the original validation message
- the top-level response uses Credo's default error envelope

---

## Automatic HTTP Error Translation

Credo localizes error messages automatically. An explicit `MessageKey` is an
exact key and falls back to its literal value. Otherwise Credo invokes the
optional scope-aware resolver, then uses the bare code. An implicit lookup miss
falls back to built-in/HTTP status text, never to the resolver-generated key.

```go
app.GET("/users/{id}", func(ctx *credo.Context) error {
    id := ctx.Request().RouteParam("id")
    if id == "missing" {
        return credo.ErrNotFound // code "not_found", no explicit key
    }
    return ctx.Response().JSON(http.StatusOK, map[string]string{"id": id})
})
```

If `locales/tr/messages.json` contains:

```json
{
  "not_found": "Bulunamadı"
}
```

then the default envelope message becomes `Bulunamadı` for Turkish requests.

Default keys are bare frozen status codes—`bad_request`, `unauthorized`,
`forbidden`, `not_found`, `internal_server_error`, and so on. Unknown statuses
use `http_<status>`. Validation and bind top-level keys are
`validation_failed` and `bind_failed`. A resolver may namespace all error-scope
keys consistently.

Explicit presentation keys are attached with `WithMessageKey`:

```go
return credo.NewHTTPError(http.StatusConflict, "email_exists").
    WithMessageKey("user.email_exists")
```

If no translation is found, the explicit key itself (`"user.email_exists"`) is
used as the message. It may therefore be a literal hard-coded message.

---

## Recommended Pattern in Handlers

A clea Credo handler usually looks like this:

```go
func (c *UserController) Create(ctx *credo.Context) error {
    var input CreateUserInput
    if err := ctx.Request().BindBody(&input); err != nil {
        return err
    }

    user, err := c.svc.Create(ctx.Context(), input)
    if err != nil {
        return err
    }

    return ctx.Response().JSON(http.StatusCreated, map[string]any{
        "message": ctx.T("messages.user_created"),
        "user":    user,
    })
}
```

The key idea is:

- use `ctx.T(...)` for success payloads and application copy
- return errors normally
- let Credo's error handler perform automatic translation for errors

---

## Inactive Mode

If `UseI18n` finds no usable locale files, Credo treats i18n as inactive.

That is not an error.

Inactive behavior is intentionally cheap:

- `UseI18n(...)` returns `nil`
- no locale-detection middleware is added
- `ctx.Locale()` returns `""`
- `ctx.T("messages.welcome")` returns `"messages.welcome"`
- automatic error translation does not run

This is useful when:

- a test binary does not ship locale files
- a small prototype is not ready for translations yet
- you want one code path for both translated and untranslated environments

---

## Startup Errors

`UseI18n` returns an error for real configuration or data problems, for example:

- invalid default language tag such as `"english"`
- malformed JSON in `messages.json` or `fields.json`
- invalid template syntax such as an unclosed `{{.name`
- invalid `RawConfig` data under the `i18n` key

This is deliberate: broken locale assets should fail fast at startup.

---

## Troubleshooting

### `ctx.T()` Returns the Key Instead of a Translation

Check the following:

1. `UseI18n(...)` was called during startup
2. locale files actually exist under the directory you configured
3. the key exists in `messages.json`
4. the request resolved to the locale you expect
5. for `DirFS`, you passed the correct filesystem root, often via `fs.Sub(...)`

### `ctx.Locale()` Is Empty

This means i18n is inactive. Common causes:

- the locale directory does not exist
- the directory exists but contains no valid locale folders
- locale files were not bundled into the binary

### `UseI18n()` Fails at Startup

Typical causes:

- invalid JSON
- invalid template syntax
- invalid default locale string
- bad config decode from `RawConfig`

### Validation Messages Stay in English

Check that your locale file contains the matching validation keys, for example:

- bare `required`, `length`, `email`, `min`, and `max`, or the exact keys
  produced by your configured resolver

If a key is missing, Credo keeps the original default message.

---

## Practical Recommendations

- keep keys stable; do not use raw English sentences as keys
- always define `http.*` keys for the statuses your API returns often
- add validation and bind keys early so `BindBody()` and `BindQuery()` become useful immediately; use a resolver if those keys are namespaced
- use `fields.json` only when server-generated messages need human field labels
- prefer `ctx.T(...)` for response copy, not for control flow
- when embedding locales, use `fs.Sub(...)` so `DirFS` points at the locale root

---

## Complete Example

Complete English and Turkish starter catalogs are available under
[`examples/references/locales`](../../examples/references/locales/). The files
are versioned copyable references, not catalogs embedded or loaded
automatically by the framework.

`config.json`:

```json
{
  "i18n": {
    "dir": "locales/",
    "default": "en"
  }
}
```

`locales/en/messages.json`:

```json
{
  "messages.user_created": "User created",
  "required": "{{.field}} is required",
  "email": "{{.field}} must be a valid email address",
  "not_found": "Not found",
  "internal_server_error": "Internal server error"
}
```

`locales/en/fields.json`:

```json
{
  "email": "email address"
}
```

`main.go`:

```go
package main

import (
    "log"
    "net/http"

    "github.com/credo-go/credo"
    "github.com/credo-go/credo/validation"
)

type CreateUserInput struct {
    Email string `json:"email"`
}

func (in *CreateUserInput) Validate() error {
    return validation.ValidateStruct(in,
        validation.Field(&in.Email, validation.Required[string](), validation.Email()),
    )
}

func main() {
    app, err := credo.New()
    if err != nil {
        log.Fatal(err)
    }

    if err := app.UseI18n(); err != nil {
        log.Fatal(err)
    }

    app.POST("/users", func(ctx *credo.Context) error {
        var input CreateUserInput
        if err := ctx.Request().BindBody(&input); err != nil {
            return err
        }

        return ctx.Response().JSON(http.StatusCreated, map[string]string{
            "message": ctx.T("messages.user_created"),
            "locale":  ctx.Locale(),
        })
    })

    app.GET("/users/{id}", func(ctx *credo.Context) error {
        return credo.ErrNotFound
    })

    if err := app.Run(); err != nil {
        log.Fatal(err)
    }
}
```

This gives you:

- translated success messages through `ctx.T(...)`
- translated validation field messages from `BindBody()`
- translated HTTP error titles from `credo.ErrNotFound`
- resolved per-request locale via `ctx.Locale()`
