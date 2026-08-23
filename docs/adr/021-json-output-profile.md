# ADR-021: JSON Output Profile

**Status:** Accepted (implementation tracked in [TODO.md](../../TODO.md)) **Date:** 2026-08-23 **Depends on:** ADR-008, ADR-009 **Related:** ADR-011, ADR-014

## Context

Credo decodes request bodies with `encoding/json/v2` ([ADR-008](008-context-design.md)): strict single-value bodies, duplicate-member rejection, and typed `*BindError` reasons. Encoding stayed on `encoding/json` v1 — `Response.JSON` used a `json.Encoder`, and so did both RFC 7807 Problem Details writers. Two decoders' worth of behavior in one framework is a maintenance hazard, and v1 is now the compatibility layer rather than the primary API.

Moving the encode side is not a like-for-like swap. Unlike the decode sites (config files, locale files, test helpers), `Response.JSON` encodes **application data**, so every v1→v2 default difference is a visible wire change for every Credo application:

| Axis | v1 | v2 |
| --- | --- | --- |
| Map key order | always sorted | insertion/unspecified |
| nil slice / nil map | `null` | `[]` / `{}` |
| `omitempty` | Go zero value | JSON-empty (`""`, `null`, `[]`, `{}`); numbers and bools never omitted |
| `<`, `>`, `&` | escaped as `<` … | emitted literally |
| Trailing newline | `Encoder.Encode` appends `\n` | `MarshalWrite` writes none |
| `time.Duration` | integer nanoseconds | no default representation — an error |

The Duration row is the hard constraint. Go issue 71631 was closed deliberately: json/v2 has *no* default Duration representation. The `format:` struct tag that the experimental module used to provide was **removed before the standard-library release** (go.dev/issue/79071), pending the typed-struct-tags proposal (go.dev/issue/74472). In Go 1.27 there is therefore no tag an application can write to make a Duration field encode; without an explicit option, any response carrying a Duration fails to encode.

## Decision

### One profile per application, applied at every framework encode site

`Response.JSON` and both Problem Details writers encode through `jsonv2.MarshalWrite` with an application-level options value:

```go
var defaultJSONOptions = jsonv2.JoinOptions(
    jsonv2.Deterministic(true),          // sorted map keys
    jsonv1.FormatDurationAsNano(true),   // Duration as integer nanoseconds
)
```

Everything else is json/v2's own default. The two adjustments are the axes where v2's default is either impossible (Duration) or a regression for a server framework (unstable map ordering breaks golden files, response diffs, and cache keys for no benefit — v1 sorted unconditionally, so the sort cost is not new).

The remaining four differences are adopted as the framework default for every Credo application:

- **`[]` and `{}` for nil slices and maps.** This is what API consumers want: no `null` guard before iterating. `null` for "empty list" is a Go-specific accident, not a JSON idiom.
- **JSON-empty `omitempty`.** The v2 rule matches what the tag name says; `omitzero` covers the v1 meaning explicitly.
- **No HTML escaping.** Credo responses are `application/json; charset=utf-8`; browsers do not sniff JSON served with that content type, and `nosniff` is available through `middleware.Secure`. Escaping cost bytes for a threat model that does not apply to an API response.
- **No trailing newline.** The newline was an artefact of v1's streaming `Encoder`, never part of the JSON value.

### `WithJSONOptions` is the escape hatch, not a per-call parameter

```go
credo.WithJSONOptions(jsonv2.FormatNilSliceAsNull(true))  // one axis back to v1
credo.WithJSONOptions(jsonv1.DefaultOptionsV1())          // full legacy mode
```

Options are appended after the framework profile, so each one overrides that axis (later options win) and leaves the rest intact. The option is construction-time only and has no config-file key, because options are Go values rather than strings.

There is no per-call `JSONWith(...)` variant, mirroring the strict-bodies decision ([ADR-008](008-context-design.md)): one posture per application. An application that needs a different envelope has `SetSuccessRenderer`; a handler that needs different bytes entirely can marshal them itself and write through `Response.Blob`.

### Problem Details always sort map keys

RFC 7807 bodies are a framework contract consumed by clients, translators, and tests, so `Deterministic(true)` is re-applied after the application profile even when the application turned it off. Error bodies stay byte-stable across applications.

### Decoding policy stays separate

The profile governs encoding only. Request-body strictness is `WithStrictBodies` ([ADR-008](008-context-design.md)), and `BindBody` keeps its own option set — `MatchCaseInsensitiveNames` for client compatibility plus the same `FormatDurationAsNano`, so a Duration round-trips through Credo unchanged.

## Rejected Alternatives

| Alternative | Reason |
| --- | --- |
| No Duration default (error out, tell the developer to add a tag) | There is no tag to add in Go 1.27 — `format:` was removed before release. Every struct with a Duration field would simply fail to render |
| Credo-specific Duration marshaler (`WithMarshalers`, e.g. `"5s"`) | Invents a wire format the framework's own binder cannot read back, and user-supplied marshalers take precedence over any future `format:` tag, silently overriding a field the developer explicitly annotated |
| `DefaultOptionsV1()` as the framework default | Freezes Credo on the compatibility layer forever and forfeits the improvements (`[]` for nil slices, honest `omitempty`) that motivated the move |
| Per-call encoding options (`JSONWith`) | Mixed posture inside one service; the response shape becomes a per-handler decision that no one can audit centrally |
| A config-file key for the profile | Options are Go values (`jsonv2.Options`), not strings; a stringly-typed subset would be a second, weaker API |

## Consequences

- Applications upgrading see four wire changes: `[]`/`{}` instead of `null`, JSON-empty `omitempty`, unescaped `<>&`, and no trailing newline. Each is opt-out per axis through `WithJSONOptions`; `DefaultOptionsV1()` restores all of them at once (except the trailing newline, which no option restores — it was never part of the value).
- Response snapshot tests that hard-code `null` for empty arrays or `<` escapes need updating; that is the intended, visible cost of the change.
- Framework-owned JSON is unaffected in shape: `ProblemDetails.Errors` is `omitempty` on a slice, which behaves identically under both rules, and health output renders durations as strings.
- `time.Time` keeps RFC 3339 (no `format:` tag means no `unix` or custom layouts; a named type with `MarshalText` covers the rest).
- If typed struct tags land in a future Go release with a Duration format mechanism, the P1 decision is worth revisiting — as a deliberate break, tracked in the v1 gate.
