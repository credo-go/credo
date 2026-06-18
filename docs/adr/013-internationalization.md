# ADR-013: Internationalization

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** ADR-009

## Context

Enterprise applications (ADR-001) serving international users need error messages and validation feedback in the user's language. The i18n system must integrate with the framework's internal error handler (ADR-009) without coupling the root package to i18n implementation details.

## Decision

### Architecture

i18n implementation lives in `internal/i18n/` — a pure message lookup engine with zero root package imports. The public API is exposed via the root package:

```go
app.UseI18n(...)                                  // setup
ctx.Locale()                                     // detected language
ctx.T("v.required")                              // translate
```

Errors carry their own translation key rather than being matched by type. `HTTPError` holds a `MessageKey` field (e.g. `"http.not_found"`); the error pipeline (ADR-009) resolves it against the bundle. The same key doubles as the literal fallback, so a message survives even with no bundle configured.

> **Scope**: only errors that already carry HTTP response semantics get a translated title — `*HTTPError` (via `MessageKey`) and errors exposing `HTTPStatus() int` (mapped to a key through `statusToKey`). A plain error is classified as 500 and its message is never leaked to the client.

### Language Detection

`UseI18n` adds a global middleware that detects the user's language. Default: reads `Accept-Language` header. Custom: via `I18nConfig.Detect` function.

The detected locale is stored directly on the `Context` struct as the `locale` field, accessed via `ctx.Locale()`.

### Message Bundles

Locale files are JSON, organized by directory-per-locale:

```
locales/
  en/
    messages.json     # error messages
    fields.json       # field name translations (opt-in)
  tr/
    messages.json
    fields.json
```

`Bundle` loads messages from the filesystem. String-based APIs (`TranslateForLang`, `FieldNameForLang`) bridge the internal/root boundary without exposing `language.Tag`.

### CLDR Plural Rules

Plural form selection is delegated to `golang.org/x/text/feature/plural` (CLDR data maintained upstream). `internal/i18n/plural.go` only decomposes the count into CLDR operands (derived from go-i18n). Six plural forms: zero, one, two, few, many, other. Public surface: `ctx.TPlural(key, count, data...)`; `ctx.T` always renders the Other form. `ctx.TPlural` is tolerant — an uninterpretable count renders the Other form; the strict, error-returning path lives in the internal `Localizer`.

### Error Translation

The error pipeline (ADR-009) resolves messages at render time; i18n plugs in at two points:

1. **Validation errors** (`validation.Errors`) — `translateValidationErrors` translates each field error under the `"v." + code` key (e.g. `"v.required"`), injecting the translated field name when `fields.json` is present.
2. **HTTP error titles** — `*HTTPError` (via its `MessageKey`) and errors exposing `HTTPStatus() int` (mapped to a key such as `"http.not_found"` via `statusToKey`) are resolved by `resolveMessage`, a 3-level fallback: bundle → built-in English default → the key itself.

With no bundle configured both paths fall through to the built-in/literal text, so translation is purely additive.

### Two-Mode Field Translation

1. **Field-agnostic** (default): Only error messages are translated. Field names appear as-is from the struct.
2. **Field-aware** (opt-in via `fields.json`): Field names are also translated (`"email"` → `"E-posta adresi"`).

### Code Prefix Convention

Validation rule codes are bare identifiers (`"required"`, `"length"`). Locale keys use a `"v."` prefix (`"v.required"`, `"v.length"`). This separates validation codes from other message namespaces.

### Design Decisions

| Decision | Rationale |
| --- | --- |
| JSON-only locale files | Zero dependency, industry standard, no YAML/TOML parser needed |
| `internal/i18n/` engine | Pure message lookup, zero root imports, no circular deps |
| Key-based message lookup | Errors carry a `MessageKey` resolved by `resolveMessage` — no type-dispatching error translator, and i18n stays out of the error's type identity |
| `UseI18n` on App | Single setup method, frozen-guarded, zero-config defaults |
| `ctx.locale` field | Direct struct field, no context key lookup overhead |
| CLDR plurals via `x/text/feature/plural` | Battle-tested, 200+ languages, Unicode updates maintained upstream (originally adapted from go-i18n, replaced to drop ~1k generated lines) |
| `golang.org/x/text/language` internal only | Never leaks into root API |
| No template delimiter customization | `{{` `}}` is sufficient, reduces API surface |
| `text/template`, not `html/template` | Messages are plain text rendered into JSON bodies and logs; unconditional HTML escaping would corrupt them. Matches go-i18n upstream. **Trust model:** locale files are developer-controlled code artifacts — review them like code (templates can call methods on the data passed to `T`); HTML escaping belongs to the HTML rendering layer (`html/template` escapes interpolated i18n strings automatically) |

## Consequences

**Positive:**

- Error messages in user's language with proper pluralization
- Zero cost when not configured (nil bundle check in error handler)
- Minimal public API (3 methods + 2 types)
- No circular imports — internal engine has zero root dependencies
- Pure function translation — no hidden state, easy to test
- 200+ languages supported via CLDR

**Negative:**

- JSON-only limits to simple key-value messages (no ICU MessageFormat)
- Field-aware translation requires maintaining `fields.json` per locale
- `golang.org/x/text` (language + feature/plural) as internal dependency
