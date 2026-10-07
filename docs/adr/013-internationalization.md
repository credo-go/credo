# ADR-013: Internationalization

**Status:** Accepted; v0.24.0 decisions accepted, pending implementation ([plan](../plans/components-and-sequential-bootstrap.md)) **Date:** 2026-03-01 **Last revised:** 2026-09-05 **Depends on:** ADR-009, ADR-010

## HTTP integration amendment

**2026-09-05 (HTTP minor):** i18n remains explicitly installed through `UseI18n` and is a framework-owned HTTP feature under [ADR-010](010-middleware-architecture.md#built-in-http-feature-configuration-criterion); its former hidden `GlobalMiddleware` registration is gone. Catalog/source validation, exact keys, field catalogs and plural semantics are unchanged.

**Detector contract:** `I18nConfig.Detect func(*Context) string` is the sole callback. It resolves on the first `Locale`/translation access, including automatic error/field translation, and memoizes one result per request in state that is reset with the pooled Context. The default detector reads Accept-Language; empty/unresolvable results use the configured default. Inactive or unused i18n does not call the detector. Recursive `Locale`/translation from `Detect` is a programming panic; a detector panic retains the default fallback and never detects again for that request, so error rendering uses the cached default without recursing. Recovery-enabled requests take the 500 path; disabled recovery propagates the panic after cleanup.

First access fixes the language using the data visible then. Context-based detection permits `GetUser` but does not extend the principal's lifetime through Timeout/stdlib request restoration; applications needing the authenticated language in later errors resolve `Locale` after setting the user, before unwinding, and earlier reads still win. No detector runs on the terminal lifecycle 503.

A successful `UseI18n` with missing/empty conventional discovery records configured-but-inactive state and consumes the sole registration; a second call is duplicate misuse. Real source/load/validation errors leave the slot free for repair before preparation, so successful configuration is independent of which deployment has conventional catalog files. The [HTTP feature contract](../specs/http-features.md#locale-and-transport-features) carries the full rules.

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

`Accept-Language` detection is the default. `I18nConfig.Detect func(*Context) string` may replace it. Detection is lazy: it runs on the first `Locale`/translation access of a request (the error pipeline's automatic translation included), the selected canonical tag is memoized on the request Context, and requests that never touch locale or translation do not invoke the detector.

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
    Detect   func(*Context) string
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
- The complete bundle is published only after all sources validate, so a failed setup exposes no partial catalog and leaves the registration free for repair; a successful setup — a conventional discovery that found nothing included — consumes the single `UseI18n` registration.

This distinguishes an optional convention from a declared deployment dependency. Programmatic fallback prevents raw keys on individual misses; it must not hide the loss of an explicitly configured source.

### Registration and the start phase

**Accepted, pending implementation (v0.24.0, W5).** When it ships, this section replaces the amendment's paragraph on how a successful or failed `UseI18n` consumes its registration, and the last bullet of the source policy above.

`UseI18n(cfg ...I18nConfig)` returns nothing and panics on misuse, like every other `Use*`. Registration performs no I/O ([ADR-022](022-bootstrap-and-di-ownership.md)); the catalogs are read in the start phase, and a read failure is a start failure that rolls back like any other ([ADR-024](024-lifecycle-components.md)).

- **At the call**, with the call site in the panic: more than one config, `Dir` together with `DirFS`, a second call, a call after the App is prepared or shut down, and every rule decided without reading a source — the `Default` tag, the programmatic `Messages` and `Fields` (empty keys or values, templates that do not compile), `Fields` with no `Messages` and no file source, and an `i18n` RawConfig section that does not decode.
- **In the start phase**, as errors: conventional `./locales` discovery, the reads of `Dir`, `DirFS` or a configured `i18n.dir`, malformed or read-denied files, and an explicit source that is missing or contains no messages. The source policy above is unchanged — an explicit source still fails loud, and only absent conventional discovery is an inactive warning, now logged by the start phase. The bundle is published whole, after every source has validated and before the listener accepts; a failure publishes nothing.
- **No repair state.** The first `UseI18n` call consumes the registration. A source error no longer leaves the slot free, since it surfaces as a start failure of a single-use App.
- **Serving waits for the catalogs.** An App that called `UseI18n` has something to start, so `ServeHTTP` refuses it until `App.Start` has succeeded, as for any start work ([ADR-024](024-lifecycle-components.md)). Request handling still performs no filesystem access, and the detector contract is unchanged.

Rejected: keeping the error return. `UseI18n` returned an error only because it read files at the call; with the reads in the start phase that reason is gone, and a registration that both returns errors and panics on misuse gives its caller two channels for one phase. Rejected too: reading the catalogs at registration and only changing the return to a panic — a missing deployment directory is an I/O failure, not a programming mistake, and registration no longer performs I/O.

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
