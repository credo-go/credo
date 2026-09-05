# Wire Hot-Path Performance Plan

**Status:** Measurement-backed candidates; implementation not started. Promoted 2026-09-05. **Progress:** [TODO.md](../../TODO.md#pre-v1-contract-migration).

Source: the `200ms × 3` wire/i18n benchmark run on the v0.18.0 tree plus the benchmark suite, and the code assessment of 2026-09-05. These are historical measurements, not fresh results from documentation promotion. Performance changes need before/after evidence; none of the projected gains is guaranteed.

## Baseline (v0.18.0 tree plus benchmark suite, no-op body writer, io.Discard logger)

| Path                                | Observed                       |
| ----------------------------------- | ------------------------------ |
| Core NoContent                      | 51–53 ns, 0 allocs             |
| Error JSON option assembly          | 107–136 ns, 112 B / 1 alloc    |
| Core / StreamReaderOnly, 4 KiB body | 4.6–5.6 µs, ~32 KiB / 3 allocs |
| i18n / two-field validation error   | 5.2–5.5 µs, 55 allocs          |
| AccessLog enabled, RequestID off    | 1.07–1.21 µs, 2 allocs         |
| AccessLog, handler-level filtered   | 131–137 ns, 1 alloc            |
| AccessLog, MinLevel filtered        | 60–63 ns, 0 allocs             |
| Bundle.TranslateForLang("tr")       | 355–383 ns, 6 allocs           |
| Bundle.translateForTag(Turkish)     | 34 ns, 0 allocs                |

Measurement tools: `wire_benchmark_test.go` (root, internal package), `internal/i18n/benchmark_test.go`, `i18n_benchmark_test.go`, `benchmark_test.go` (`benchExpect` guards each benchmark request once outside the loop).

## Acceptance criterion (applies to every item)

Same inputs, `go test -run '^$' -bench <name> -count=10 -benchmem`, compared with `benchstat` before/after; PR body carries the table. No CI benchmark job (shared-runner noise). Behaviour preserved: existing tests unchanged except where a test pins the old cost. Record the outcome as a short performance note under CHANGELOG `Changed`. Add the `-count=10` + `benchstat` rule to CONTRIBUTING when the first perf PR lands.

The built-in HTTP feature work in [the HTTP feature contract](../specs/http-features.md) changes defaults and specifies lazy locale detection. Those behavior/API changes belong to a separate HTTP minor. Keep feature activation and locale-access patterns equivalent in perf comparisons; the baseline above describes the v0.18.0 tree, not a measurement of the new default profile.

## Package A — `perf/wire-hot-paths` (one PR, three commits)

### A1. Precompute error JSON options at construction — high confidence, low risk

- `errorJSONOptions` (`json.go`) calls `jsonv2.JoinOptions(app.jsonOptions(), Deterministic(true))` on every error response; `New` already fixes `jsonOpts` at construction.
- Change: add `app.errorJSONOpts jsonv2.Options`, computed next to `jsonOpts`; package-level `defaultErrorJSONOptions` for the nil-App fallback (tests using `NewResponse`). `errorJSONOptions()` returns the field.
- Only caller: the default error body writer in `errors.go`.
- Proof: `BenchmarkWireJSONOptions/ErrorOptions` must match `PrecomputedErrorOptions` (0 allocs).

### A2. Skip language re-resolution for canonical locale strings — high yield, medium risk, contained in `internal/i18n`

- Current source of repeated work: after the `UseI18n` locale middleware has run, `ctx.locale` contains `bundle.MatchLangString(lang)` or `cfg.Default`. Later `TranslateForLang`, `TranslatePluralForLang` and `FieldNameForLang` calls re-run parsing/matching on the same string. The validation path resolves twice per violation (field name + message).
- Revised invariant for P8's accepted lazy contract: locale may be unresolved until the first `Locale()` or translation access. One request-owned resolver must initialize and memoize it before any of those lookups, including error/bind/validation and field-name translation. Only the resolved locale has the canonical/default-string fast-path property. Do not use an initially empty `ctx.locale` as proof that i18n is inactive; reset separate memoization state with the pooled Context. P8 owns this behavioral change and its ordering/failure contract; A2 owns the Bundle fast path.
- Change: in `Bundle.rebuildMatcher` build `canonical map[string]language.Tag` keyed by `tag.String()` for every registered tag plus `defaultLang.String()`; also add the raw `cfg.Default` string if it differs from `defaultLang.String()` (e.g. `en-us` vs `en-US`). `resolveTag` checks the map first; miss → existing Accept-Language path. Read-only after build, no lock (Bundle is not mutated after `UseI18n`; i18n is not a reload participant — verify before relying on it).
- Not doing (yet): carrying the resolved `language.Tag` on `Context`. Would pull an `x/text` type into the root package; the table gives the same effect without touching public API. Revisit only if the table leaves measurable cost.
- Proof: `BenchmarkBundleTranslate/CanonicalString` approaches `ResolvedTag` (34 ns, 0 allocs); `BenchmarkUseI18n_ValidationError` allocs drop by roughly 2 × fields × (parse + match).
- If P8 has landed, add equivalent no-locale-use, first-resolution and repeated-translation cases. First use still pays detector/header resolution; subsequent lookups should use the cached request locale and Bundle fast path. Assert no detector calls for unused i18n and at most one for used requests, including early error translation. Do not credit skipped work to canonical lookup cost. The Bundle-only cache can land before or after P8; its public string-input behavior stays the same.
- Risk to watch: `MatchLangString` for a raw header like `tr` already returns `tr`; ensure the table never shadows a header that happens to equal a canonical string but should match differently (it cannot: exact canonical string → that tag is the correct match by definition).

### A3. Access log: check the effective logger's level before building the entry — measured, narrow

- `builtinAccessLog` (`accesslog.go`): current order is MinLevel → entry construction (`RealIP`, User-Agent, Route, RequestID) → `ResultFilter` → logger selection → `EmitAccessLog` → `logger.Enabled`.
- Logger selection depends only on `configuredLogger` / `ctx.logger` / base logger; the level only on status. Neither needs the entry.
- Change: when `filter == nil`, select the logger and call `logger.Enabled(r.Context(), level)` before constructing `AccessLogEntry`; return early when disabled. When `filter != nil`, keep the existing order so the filter still observes every response (documented contract). `EmitAccessLog` keeps its own `Enabled` check for the other producers.
- Ceiling: handler-filtered 131 ns / 1 alloc → ~60 ns / 0 allocs (the MinLevel figure).
- Proof: `BenchmarkWireObservability/AccessLog/HandlerFiltered` vs `MinLevelFiltered`.

## Package B — `perf/response-readfrom` (separate PR, needs live-server verification)

### B1. `Response.ReadFrom` with delegation and a pooled fallback — high yield where used, medium risk

- `Response.Stream` does `io.Copy(r, rd)`; `*Response` has no `ReadFrom`, so the underlying writer's `io.ReaderFrom` is never consulted and a Reader-only source costs a 32 KiB `io.Copy` buffer per call. (`wireBenchmarkWriter.ReadFrom` is currently unreachable for the same reason.)
- Change: implement `(*Response).ReadFrom(src io.Reader) (int64, error)`: hijacked → `0, http.ErrHijacked`; commit the header if needed; if `r.ResponseWriter` implements `io.ReaderFrom` delegate (net/http's `response.ReadFrom` uses a pooled copy buffer and sendfile when applicable; TLS and HTTP/2 writers fall back inside net/http or lack `ReaderFrom` altogether); else `io.CopyBuffer` with a `sync.Pool` 32 KiB buffer. Add the returned count to `r.size` on both branches. The type assertion at call time means compress and other wrapping writers naturally take the pooled branch.
- Preserve: byte counter semantics (`Size()`), partial-write error propagation, hijack refusal, `WriteHeader(200)` on first write, middleware writer chain.
- Verify on real HTTP/1.1 plaintext, TLS and HTTP/2 listeners, not only the benchmark writer. Add a unit test that a Reader-only source of N bytes reports `Size() == N` on both branches.
- Proof: `BenchmarkWireSuccess/*/StreamReaderOnly` allocs ~32 KiB → ~0 with the benchmark writer (delegation branch) and a wrapped-writer variant for the pooled branch.

## Deferred (recorded, not scheduled)

- Request ID dedicated field instead of the `map[string]any` context store read by `Context.RequestID`: 16 B / 1 alloc. Compatible route is special-casing `requestIDKey` in `Context.Set`/`Get` to redirect to a private field, at the cost of a string compare on every `Set`/`Get`. Small gain; after A and B.
- Total-free `Slice`/cursor response next to `Page` (COUNT + SELECT): already listed as deferred in CLAUDE.md; opt-in, `Page` contract unchanged. Application-level: watch `DB.Stats().WaitCount/WaitDuration`, query plans, N+1.
- PGO: application `main` package concern (`default.pgo` from representative production profile). The framework ships no profile; never derive one from the wire microbenchmarks.

## Order

1. Ensure the wire benchmark suite (`wire_benchmark_test.go` and the i18n benchmarks) is on the implementation base; if already merged, do not duplicate it. Resolve its current branch/merge state before starting.
2. Package A as `perf/wire-hot-paths`: A1 → A2 → A3, one commit each, benchstat in the PR body.
3. Package B as `perf/response-readfrom`.
4. Deferred items only with a new measurement that changes the picture.
