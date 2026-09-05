# ADR-013: Internationalization

**Status:** Accepted **Date:** 2026-03-01 **Last revised:** 2026-08-26 **Depends on:** ADR-009

## Pre-v1 HTTP integration amendment

**Accepted 2026-09-05; implementation pending:** i18n remains explicitly installed through UseI18n and becomes a framework-owned HTTP feature. Remove its hidden GlobalMiddleware registration; preserve catalog/source validation, exact keys, field catalogs and plural semantics.

**Accepted detector contract (G4b):** `I18nConfig.Detect func(*Context) string` is the sole callback. Resolve on first Locale/translation access, including automatic error/field translation, and memoize one result per request. The default detector reads Accept-Language; empty/unresolvable results use the configured default. Inactive/unused i18n does not call the detector.

Separate memo state is reset with the pooled Context. Recursive Locale/translation from Detect is a programming panic. On detector panic/re-entry, retain a default fallback and never detect again for that request. Recovery-enabled requests use the 500 path; disabled recovery propagates panic. Error rendering can use the cached default without recursing into the detector.

First access fixes the language using currently visible data. Context-based detection permits GetUser but does not extend the principal's lifetime through Timeout/stdlib request restoration. Applications needing the authenticated language in later errors resolve Locale after setting the user, before unwinding; earlier reads still win. No detector runs on terminal lifecycle 503.

A successful UseI18n with missing/empty conventional discovery records configured-but-inactive state and consumes the sole registration. A second call is duplicate misuse. Real source/load/ validation errors leave the slot free for repair before preparation. This makes successful configuration independent of which deployment has conventional catalog files.

The [HTTP feature contract](../specs/http-features.md#locale-and-transport-features) carries the full target. The eager request-stage detection and current callback below remain descriptions of the existing implementation until the HTTP minor lands.

## Context

Credo must localize application text, HTTP/domain errors, validation failures, bind failures, and optional field display names without making machine error codes presentation-dependent. Applications also need a safe default-language catalog that does not depend on deployment files.

## Decision

### Architecture and public API

The root package owns setup and request APIs; `internal/i18n` remains a root- independent message engine:

```go
app.UseI18n(credo.I18nConfig{...})
ctx.Locale()
ctx.T("welcome", data)
ctx.TPlural("items", count, data)
```

`Accept-Language` detection is the default. `I18nConfig.Detect` may replace it. The selected canonical tag is stored on the request Context.

### Two catalogs, not one namespace

`messages.json` contains message templates. `fields.json` maps exact technical field paths to display names. They stay separate because fields have different lookup/fallback semantics and should not require an artificial `field.` prefix.

```text
locales/
  en/messages.json
  en/fields.json
  tr/messages.json
  tr/fields.json
```

`ValidationError.Field` remains the stable technical path on the wire. The display name is injected only as template data (`{{.field}}`). Lookup uses the exact full path (`address.city`, `items[0]`). If the selected locale lacks a field name, Credo uses the raw path; it does not borrow the default language's field name and produce a mixed-language sentence.

### Programmatic default-language base

```go
type I18nMessages map[string]string
type I18nFields map[string]string

type I18nConfig struct {
    Dir      string
    DirFS    fs.FS
    Default  string
    Detect   func(*http.Request) string
    Messages I18nMessages
    Fields   I18nFields
    ResolveMessageKey MessageKeyResolver
}
```

`Messages` and `Fields` represent only the effective `Default` language. They are copied and templates are compiled during setup. Multi-language catalogs belong in `Dir`/`DirFS`; this avoids rebuilding Go code for translation work and avoids a second multi-language source of truth.

Programmatic values are strings and populate the CLDR Other form. File-backed messages retain all plural forms. A public plural union/struct is deferred.

Load order is programmatic base first, external source second. Both messages and fields merge by canonical language tag and exact key; external values override collisions while programmatic-only keys remain. No caller map is retained.

### Source policy

- `Messages` alone activates map-only i18n; `Messages + Fields` is field-aware.
- `Fields` without any message source is an error.
- Supplying maps without `Dir`/`DirFS` disables implicit `./locales` discovery.
- Explicit `Dir` or `DirFS` is strict: missing, unreadable, malformed, or message-empty is a setup error even when `Messages` could serve requests.
- A RawConfig `i18n.dir` is explicit and follows the same fail-loud rule.
- Only absent conventional `./locales` discovery from zero-config setup is an inactive warning.
- `Dir` and `DirFS` are mutually exclusive.
- The complete bundle and middleware are published only after all sources validate, so a failed setup exposes no partial catalog.

This distinguishes an optional convention from a declared deployment dependency. Programmatic fallback prevents raw keys on individual misses; it must not hide the loss of an explicitly configured source.

### Exact keys and application-owned namespaces

Credo never generates prefixes such as `errors.`, `http.`, `v.`, or `bind.`. For framework error flows, key selection is:

1. an explicit value-level `MessageKey` (exact);
2. optional `ResolveMessageKey(MessageRef{Scope, Code})`;
3. bare code/reason.

`MessageScopeError`, `MessageScopeValidation`, and `MessageScopeBind` let an application apply namespaces without hidden string rules:

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

Prefixes are recommended for large catalogs but optional. Explicit `HTTPError.MessageKey` and `ValidationError.MessageKey` bypass the resolver. The selected exact key is visible to `ErrorRenderer` as `ErrorInfo.MessageKey`; resolved text is `ErrorInfo.Message`.

### Plurals and templates

Plural selection uses `golang.org/x/text/feature/plural` CLDR data. `ctx.T` always renders Other. `ctx.TPlural` selects zero/one/two/few/many/other for file catalogs and uses Other for programmatic strings. Templates use `text/template`; locale sources are trusted application artifacts and must be reviewed like code. HTML escaping belongs at the HTML rendering boundary.

## Consequences

- Applications can ship a safe default-language catalog in code and layer real locale files over it.
- Message and field catalogs retain clear, independent responsibilities.
- Machine codes remain stable when presentation namespaces change.
- Strict explicit-source handling catches deployment mistakes at startup.
- Programmatic multi-language and plural-form APIs remain intentionally out of scope; `DirFS` covers those uses.
