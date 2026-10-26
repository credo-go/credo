# ADR-013: Internationalization

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** ADR-009, ADR-010, ADR-022, ADR-024 **Related:** ADR-011 **Specification:** [i18n spec](../specs/i18n.md) **Guide:** [Internationalization guide](../guides/localization.md)

## Context

Credo must localize application text, HTTP/domain errors, validation failures, bind failures, and optional field display names without making machine error codes presentation-dependent. Applications also need a safe default-language catalog that does not depend on deployment files.

The request's language may depend on state that exists only after routing or authentication, and a request that never translates should not pay for detecting it. A catalog that a deployment declares must not silently vanish, while a conventional directory that a deployment does not ship must not break the start.

## Decision

### Architecture and public API

The root package owns setup and request APIs (`app.UseI18n`, `ctx.Locale`, `ctx.T`, `ctx.TPlural`); `internal/i18n` is a root-independent message engine. i18n is a framework-owned HTTP feature installed through `UseI18n` ([ADR-010](010-middleware-architecture.md#built-in-http-feature-configuration-criterion)). The signatures and `I18nConfig` are in the [i18n spec](../specs/i18n.md#public-api).

### Locale detection

Detection is lazy and Context-based. `I18nConfig.Detect func(*Context) string` (`Accept-Language` by default) runs on the first `Locale` or translation access of a request, the error pipeline's automatic translation included, and its result is memoized for the request. A request that never translates pays nothing, and a detector that takes `*Context` sees the request as it stands at first use — the matched route once routing has run, the user once authentication has. A locale middleware is deliberately not used: it would detect on every request, before the route and the user are known. The [detector contract](../specs/i18n.md#detector-contract) — memo reset, re-entry, detector panics, principal lifetime — is in the spec.

### Two catalogs, not one namespace

`messages.json` contains message templates. `fields.json` maps exact technical field paths to display names. They stay separate because fields have different lookup/fallback semantics and should not require an artificial `field.` prefix.

`ValidationError.Field` stays the stable technical path on the wire. The display name is injected only as template data (`{{.field}}`). Lookup uses the exact full path (`address.city`, `items[0]`). If the selected locale lacks a field name, Credo uses the raw path; it does not borrow the default language's field name and produce a mixed-language sentence.

### Programmatic default-language base

`I18nConfig.Messages` and `Fields` represent only the effective `Default` language. They are copied and their templates compiled at the `UseI18n` call. Multi-language catalogs belong in `Dir`/`DirFS`; this avoids rebuilding Go code for translation work and avoids a second multi-language source of truth. Programmatic values are strings and populate the CLDR Other form; file-backed messages retain all plural forms. File catalogs layer over the programmatic base by canonical language tag and exact key, so programmatic-only keys survive ([sources and precedence](../specs/i18n.md#sources-and-precedence)).

### Source policy

An explicit source — `Dir`, `DirFS`, or a configured `i18n.dir` — fails the start when it is missing, unreadable, malformed or message-empty, even when programmatic maps could serve requests. Only conventional `./locales` discovery that finds nothing leaves i18n inactive, and programmatic maps without an explicit source turn that discovery off. The bundle is published whole or not at all, so a failed start exposes no partial catalog.

This distinguishes an optional convention from a declared deployment dependency. Programmatic fallback prevents raw keys on individual misses; it must not hide the loss of an explicitly configured source. Each condition and where it fails is in the [i18n spec](../specs/i18n.md#registration-and-start-phase).

### Registration and the start phase

`UseI18n(cfg ...I18nConfig)` returns nothing and panics on misuse, like every other `Use*`. Registration performs no I/O ([ADR-022](022-bootstrap-and-di-ownership.md)): misuse that needs no I/O panics at the call, with the call site in the panic, and the catalogs are read in the start phase by a framework start step named `i18n`, which runs before any component starts; a read failure is a start failure that rolls back like any other ([ADR-024](024-lifecycle-components.md)).

The first call consumes the registration, so a second call panics even when the start phase later finds no conventional catalogs and leaves i18n inactive: whether the App is configured does not depend on which deployment ships catalog files. An App that called `UseI18n` has something to start, so `ServeHTTP` refuses it until `App.Start` has succeeded ([ADR-024](024-lifecycle-components.md)); request handling performs no filesystem access.

Rejected: an error return. A registration that both returns errors and panics on misuse gives its caller two channels for one phase, and an error result implies a retry state that a single-use App has no use for. Rejected too: reading the catalogs at registration and panicking on a read failure — a missing deployment directory is an I/O failure, not a programming mistake, and registration performs no I/O.

### Exact keys and application-owned namespaces

Credo adds no namespace prefix to a code. A framework error flow looks up an exact key: an explicit value-level `MessageKey`, else the application's optional `ResolveMessageKey(MessageRef{Scope, Code})`, else the bare code or reason. `MessageScopeError`, `MessageScopeValidation`, and `MessageScopeBind` let an application apply namespaces without hidden string rules; prefixes are recommended for large catalogs but optional. The key a renderer sees as `ErrorInfo.MessageKey` is exactly the key that was looked up. The [resolution rules](../specs/i18n.md#message-key-resolution), with an example resolver and the fallbacks on a miss, are in the spec.

### Plurals and templates

Plural selection uses CLDR data from `golang.org/x/text/feature/plural` ([plurals and lookup](../specs/i18n.md#plurals-and-lookup)). Templates use `text/template`; locale sources are trusted application artifacts and must be reviewed like code. HTML escaping belongs at the HTML rendering boundary.

## Consequences

- Applications can ship a safe default-language catalog in code and layer real locale files over it.
- Message and field catalogs retain clear, independent responsibilities.
- Machine codes remain stable when presentation namespaces change.
- Strict explicit-source handling catches deployment mistakes at startup.
- Requests that never translate never run the detector.
- Programmatic multi-language and plural-form APIs (a public plural union or struct) are deliberately out of scope; `DirFS` covers those uses by embedding the catalog files in the binary.
