# ADR-021: JSON Output Profile

**Status:** Accepted **Date:** 2026-08-23 **Depends on:** ADR-008, ADR-009 **Specification:** [Context spec](../specs/context.md#json-output-profile) **Guide:** [Getting started](../guides/getting-started.md#writing-output)

## Context

Credo decodes request bodies with `encoding/json/v2` ([ADR-008](008-context-design.md)): strict single-value bodies, duplicate-member rejection, and typed `*BindError` reasons. Two generations of JSON behavior in one framework are a maintenance hazard, and v1 is the compatibility layer rather than the primary API, so the encode side — `Response.JSON`, renderer-returned bodies and the framework-owned error writers — uses json/v2 as well.

Encoding is not a like-for-like concern. Unlike the decode sites (config files, locale files, test helpers), `Response.JSON` encodes **application data**, so on every axis where v1 and v2 defaults differ the framework chooses what every Credo response looks like on the wire:

| Axis | v1 | v2 |
| --- | --- | --- |
| Map key order | always sorted | insertion/unspecified |
| nil slice / nil map | `null` | `[]` / `{}` |
| `omitempty` | Go zero value | JSON-empty (`""`, `null`, `[]`, `{}`); numbers and bools never omitted |
| `<`, `>`, `&` | escaped as `\u003c` … | emitted literally |
| Trailing newline | `Encoder.Encode` appends `\n` | `MarshalWrite` writes none |
| `time.Duration` | integer nanoseconds | no default representation — an error |

The Duration row is the hard constraint. Go issue 71631 was closed deliberately: json/v2 has _no_ default Duration representation. The experimental module's `format:` struct tag did **not ship in the standard-library release** (go.dev/issue/79071), pending the typed-struct-tags proposal (go.dev/issue/74472). In Go 1.27 there is therefore no tag an application can write to make a Duration field encode; without an explicit option, any response carrying a Duration fails to encode.

## Decision

### One profile per application, applied at every framework encode site

`Response.JSON`, renderer-returned JSON, and the default error writer encode through `jsonv2.MarshalWrite` with one application-level options value: json/v2's defaults plus `Deterministic(true)` (sorted map keys) and `FormatDurationAsNano(true)` (a Duration as integer nanoseconds).

The two adjustments are the axes where v2's default is either impossible (Duration) or a regression for a server framework (unstable map ordering breaks golden files, response diffs, and cache keys for no benefit — v1 sorted unconditionally, so the sort cost is not new).

The remaining four differences are adopted as the framework default for every Credo application:

- **`[]` and `{}` for nil slices and maps.** This is what API consumers want: no `null` guard before iterating. `null` for "empty list" is a Go-specific accident, not a JSON idiom.
- **JSON-empty `omitempty`.** The v2 rule matches what the tag name says; `omitzero` covers the v1 meaning explicitly.
- **No HTML escaping.** Credo responses are `application/json; charset=utf-8`; browsers do not sniff JSON served with that content type, and `nosniff` is available through `middleware.Secure`. Escaping costs bytes for a threat model that does not apply to an API response.
- **No trailing newline.** The newline was an artefact of v1's streaming `Encoder`, never part of the JSON value.

### `WithJSONOptions` is the escape hatch, not a per-call parameter

`credo.WithJSONOptions(...)` appends options after the framework profile, so each one overrides that axis (later options win) and leaves the rest intact; the [Context spec](../specs/context.md#json-output-profile) shows the per-axis overrides. The option is construction-time only and has no config-file key, because options are Go values rather than strings.

There is no per-call `JSONWith(...)` variant, mirroring the strict-bodies decision ([ADR-008](008-context-design.md)): one posture per application. An application that needs a different envelope has `UseSuccessRenderer`; a handler that needs different bytes entirely can marshal them itself and write through `Response.Blob`.

### Default error bodies always sort map keys

The [default error envelope](009-handler-and-error-handling.md#default-error-envelope) is a framework contract consumed by clients, translators, and tests, so `Deterministic(true)` is re-applied after the application profile even when the application turned it off. Custom renderer bodies, including the opt-in RFC 9457 renderer, use the application profile because the application selected that representation.

### Decoding policy stays separate

The profile governs encoding only. Request-body strictness is `WithStrictBodies` ([ADR-008](008-context-design.md)), and `BindBody` keeps its own option set — `MatchCaseInsensitiveNames` for client compatibility plus the same `FormatDurationAsNano`, so a Duration round-trips through Credo unchanged.

## Rejected Alternatives

| Alternative | Reason |
| --- | --- |
| No Duration default (error out, tell the developer to add a tag) | There is no tag to add in Go 1.27 — `format:` did not ship in the standard library. Every struct with a Duration field would simply fail to render |
| Credo-specific Duration marshaler (`WithMarshalers`, e.g. `"5s"`) | Invents a wire format the framework's own binder cannot read back, and user-supplied marshalers take precedence over any future `format:` tag, silently overriding a field the developer explicitly annotated |
| `DefaultOptionsV1()` as the framework default | Freezes Credo on the compatibility layer forever and forfeits the improvements (`[]` for nil slices, honest `omitempty`) that motivated json/v2 |
| Per-call encoding options (`JSONWith`) | Mixed posture inside one service; the response shape becomes a per-handler decision that no one can audit centrally |
| A config-file key for the profile | Options are Go values (`jsonv2.Options`), not strings; a stringly-typed subset would be a second, weaker API |

## Consequences

- Every v2 default is reversible per axis through `WithJSONOptions`; `DefaultOptionsV1()` restores v1 output at once except the trailing newline, which no option restores — it was never part of the value.
- Framework-owned JSON carries no Duration and no nil-versus-empty ambiguity: `ErrorBody.Violations` is `omitempty` on a slice, which behaves identically under both rules, and health output renders latencies as strings.
- `time.Time` keeps RFC 3339 (no `format:` tag means no `unix` or custom layouts; a named type with `MarshalText` covers the rest).
- If typed struct tags land in a future Go release with a Duration format mechanism, the integer-nanosecond Duration decision is worth revisiting — as a deliberate break, tracked in the [v1 gate](../../TODO.md#v1-gate).
