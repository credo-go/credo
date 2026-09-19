# Restart Backoff and Startup Features — Delivery Plan

**Status:** Decisions accepted and promoted 2026-09-19; implementation pending. **Progress source:** [TODO.md](../../TODO.md#restart-backoff-and-startup-features). This plan defines scope, sequence and acceptance; progress checkboxes live only in TODO. When the last work item has shipped, fold what remains into the ADRs and specs, repoint every link to this file, and delete it.

## Contract map

| Work | Canonical decision | Detailed contract | Release |
| --- | --- | --- | --- |
| W1: static path decoded once (bug) | Router decode-once rule ([ADR-007](../adr/007-router-and-routing.md)) | [Router: encoded parameter values](../specs/router.md#encoded-parameter-values), [static-files guide: path sanitization](../guides/static-files.md#path-sanitization-and-security) | next patch |
| W2: startup `features` attribute | [ADR-010](../adr/010-middleware-architecture.md#startup-visibility-of-effective-features) | [HTTP features: startup visibility](../specs/http-features.md#startup-visibility), [lifecycle: startup record](../specs/lifecycle.md#startup-record) | next patch or minor (additive) |
| W3: continuous-worker restart backoff | [ADR-023](../adr/023-worker-system.md#restart-backoff) | [Worker spec: restart backoff](../specs/worker.md#restart-backoff) | v0.21.0, its own minor |

The documentation notes decided together with this work shipped with the promotion and need no release: the routing guide's [Route Parameters Are Not File Names](../guides/routing.md#route-parameters-are-not-file-names) section and corrected canonical-form sentence, the router spec's boundary statement, the static-files and migration-guide links to it, and the cron time-zone statements in the worker guide, worker spec and ADR-023.

Breaking changes are allowed before v1, and a behavioral theme takes its own minor. W3 changes behavior without a compile error, so it ships alone as v0.21.0. W1 restores documented behavior and W2 only adds a log attribute; both may ride a patch release. The implementation does not reopen the accepted decisions by adding options, modes or parallel APIs.

## Sequence

W1, W2 and W3 touch separate code and can be implemented in any order; the recommended order is W1 (a user-visible bug), then W2, then W3. Each work item is one content pull request with its own tests and documentation flip; releases follow the usual preparation pull request and `Release` dispatch in [CONTRIBUTING](../../CONTRIBUTING.md#releasing).

Every pull request runs `go vet`, the shadow analyzer, `gofmt` and `go test -race` over all packages except the ignored `tmp/` tree, and CI's required checks. Worker changes also run `go test ./worker/... -race -count=20`.

## W1: static serving decodes the captured path once

**Finding (2026-09-19, while promoting the file-path documentation).** Since the wire minor, `RouteParam` returns the catch-all value decoded once. `App.Static` passes that decoded value (`static.go`, `req.RouteParam("_static")`) to `internal/static`, whose `Serve` still documents its input as "still percent-encoded" and decodes it again with `url.PathUnescape`. Observed on the current tree:

| Request | File on disk | Today | Documented |
| --- | --- | --- | --- |
| `/static/100%25.txt` | `100%.txt` | 400 (second decode of `100%.txt` fails) | 200 |
| `/static/a%252Fb.txt` | `a%2Fb.txt` | decoded twice to `a/b.txt`: 404, or that other file's content when it exists | 200 with `a%2Fb.txt`'s content |
| `/static/%252e%252e/secret` | — | 400 (decoded twice to `..`) | 404, as the static-files guide's table states |

Directory listings are affected too: `Browse` renders the link for `100%.txt` correctly as `100%25.txt`, and following it answers 400. Sanitization still runs after the second decode, so no traversal is possible; this is a correctness bug against the router's "no second decoding" rule.

**Change.** `internal/static` treats its input as decoded: remove the second decode, keep the sanitization stages (null byte, backslash, explicit `..` segment, `path.Clean`) unchanged, and correct the `Serve` godoc. Audit the framework for any other `url.PathUnescape`/`QueryUnescape` applied to a router-captured value (mounts, proxy and rewrite helpers, WebSocket and file routes) and fix the same way.

**Tests.**

- The three rows above answer as documented.
- With both `a%2Fb.txt` and `a/b.txt` on disk, holding different content, assert the body, not only the status: `/static/a%252Fb.txt` returns the content of `a%2Fb.txt`, while `/static/a%2Fb.txt` and `/static/a/b.txt` return the content of `a/b.txt`.
- `/static/%2e%2e/secret` and `/static/..%2Fsecret` stay 400.
- Every link of a `Browse` listing that contains `100%.txt`, `my file.txt`, `test#1.txt` and `data?v=1.csv` round-trips to 200.
- A `ctx.Rewrite` to a static path with an encoded character serves the same file as the direct request.

Tests that encode the double-decode assumption are updated, not kept. `internal/static`'s direct `Serve(…, "%zz")` call expects `ErrBadRequest` today; under the corrected contract `%zz` is an already-decoded file name — served when such a file exists, `ErrNotFound` otherwise — so the test asserts both cases. An invalid URL escape is rejected at the HTTP boundary (net/http answers a request target containing `%zz` with 400 before Credo runs), not by static serving. The sanitization expectations — null byte, backslash, explicit `..` segment, `path.Clean` — stay unchanged.

**Documentation.** Rewrite the static-files guide's "1. Decode" step: the router has already decoded the captured path once, and static serving does not decode it again. Check the [static spec](../specs/static.md) for the same statement. CHANGELOG **Fixed**, stating that a URL which reached a file only through the second decode no longer does: `/static/sub%252Ffile.txt` serves a file literally named `sub%2Ffile.txt` when one exists and answers 404 otherwise, instead of serving `sub/file.txt`.

## W2: startup `features` attribute

**Change.** One private root-package helper returns the effective feature names as a non-nil `[]string` in the display order of the [spec table](../specs/http-features.md#startup-visibility), reading the App's fields after preparation: `recover` (nil under `WithoutRecover`), `requestID`, `accessLog`, `decompress`, `compress`, `i18n` (the active bundle — not `i18nRegistered`), `errorRenderer`, `successRenderer`, and `health` when at least one probe route was registered. `healthEngine != nil` is not that condition: `UseHealth` with `Liveness` and `Readiness` both disabled creates the engine and registers no route, so `UseHealth` records a registered probe in a private field of its own. The managed serve path adds the list as `"features"` to the existing `credo: server started` line. No registry, no bookkeeping in `installFeature`, no public API. Confirm that every field is published before preparation: `installFeature` publishes under `prepMu`, while `UseHealth` writes its state after its own `checkFrozen` guard.

**Tests.** Capture the start line through a **JSON** handler — only there does a nil slice appear as `null` — for:

- a default App: `"features":["recover"]`;
- `WithoutRecover` and nothing else: exactly `"features":[]`, and no Warn record during start;
- every feature installed, with the `Use*` calls in at least two different orders: the full list in display order;
- a successful `UseI18n` with no catalogs: `i18n` absent, and its existing Warn still written;
- `health`: absent without `UseHealth`, with `Enabled` false, and with `Liveness` and `Readiness` both false; listed with only liveness, with only readiness, and with the defaults;
- `ServeContext` on a test listener and `RunContext`, asserting `label` as well;
- a failed start: no start line.

**Documentation.** Remove the "pending" markers from the HTTP features spec, the lifecycle spec's startup record and ADR-010's amendment, naming the release. Add one sentence next to the v0.19.0 RequestID/AccessLog rows of the [pre-v1 migration guide](../guides/pre-v1-migration.md): the start line lists the features in effect, and the reliable check is a request to an ordinary route that returns a request ID and produces an access record with the same ID. CHANGELOG **Changed** (additive log attribute).

## W3: continuous-worker restart backoff

**Change**, following the [worker spec](../specs/worker.md#restart-backoff):

- `worker/definition.go`: `DefaultMaxRestartDelay`, `WithMaxRestartDelay` with a presence flag so that an explicit zero is distinguishable from an omitted option, and the cap in `restartPolicy`. Update the godocs of `WithRestartDelay` and `DefaultRestartDelay`: the value is now the first and minimum delay.
- `worker/pool.go`: `poolConfig.MaxRestartDelay` (`credo:"max_restart_delay"`), negative rejected when the pool is created (as `restart_delay` is) and zero meaning the default; option validation (negative value, any use on a scheduled worker — explicit zero included — with the existing cross-kind message pattern); three-way cap resolution where the pool's base is known, with an explicit positive cap below the effective base returned as a registration error.
- `worker/runner.go`: `continuousPolicy` (one per worker loop) keeps the current ceiling in one `ceiling` field — zero starts the sequence at `base`; otherwise the next ceiling is the cap when `ceiling > cap/2`, else `ceiling * 2`; `duration >= cap` resets it to zero first. `afterRun` decides whether a restart is planned (limit not exhausted, shutdown not observed) before writing `worker run failed`, adds `next_restart_in` only then, and keeps every existing line, count and status transition.
- Randomness sits behind a package-private seam: an unexported `jitter func(lo, hi time.Duration) time.Duration` on the pool, defaulting to a uniform pick from `math/rand/v2` and returning `lo` when `lo == hi`. Tests replace it with a lower-bound or upper-bound picker. No public clock or random-source abstraction.
- `worker/info.go`: `Config.MaxRestartDelay` (`json:"max_restart_delay"`), the effective cap for continuous workers and zero for scheduled ones; `RestartDelay` godoc updated to "base".

**Existing tests.** Six test files hold 15 `WithRestartDelay` uses, and several assume an exact cadence — `TestRunContinuous_MaxRestarts` expects runs at 0 s, 1 s, 2 s, 3 s and 4 s. Decide per test: a test about a fixed cadence sets both delay options to the same value; a test about growth pins the jitter seam. Do not loosen assertions to ranges.

**New tests.**

- Delay calculation, as a pure table: first delay equals `base`; windows for k = 1…8 with base 3 s and cap 1 min (`[3,3]`, `[3,6]`, `[6,12]`, `[12,24]`, `[24,48]`, `[30,60]`, …); saturation with a base near the largest duration and an unbounded sequence (no overflow, ceiling never above the cap); cap equal to base is fixed; reset at exactly `duration == cap` and not at `cap − 1ns`.
- Resolution, as a table next to the existing restart-delay one in `info_test.go`: omitted with no pool key (1 min); omitted with a pool cap (the pool cap); **explicit zero with a 5-minute pool cap (1 min — the pool is skipped)**; explicit positive overriding the pool cap; base 10 min above the default cap (cap 10 min, fixed); pool cap below one worker's base (raised, no error); negative pool cap (pool creation error); explicit positive cap below the base (registration error); negative explicit cap and `WithMaxRestartDelay(0)` on a scheduled worker (registration errors).
- Runtime, under `testing/synctest` with the seam pinned: restart times follow the lower and the upper bound sequences; the cap is reached and held; a long run resets the next wait to `base`; shutdown during a long wait ends in `stopped` without counting a restart; `Restarts` and the `WithMaxRestarts` budget are untouched by resets; an early nil return (W9 outcome 6) and a panic back off exactly like an error.
- Logging: `next_restart_in` equals the chosen delay on planned restarts and is absent on limit exhaustion and on failures during shutdown, while the number of `worker run failed` lines still equals the number of failed runs and `worker exceeded max restarts` still follows the last one.
- Snapshot: `Config.MaxRestartDelay` for both kinds and the golden JSON with `max_restart_delay`.
- Timing: with the defaults, `WithMaxRestarts(5)` reaches `failed` after 48 s with the lower-bound picker and 93 s with the upper-bound one.

**Documentation.**

- Worker spec: move the restart-backoff section's content into Continuous workers, Public API, Validation (cross-kind table row), Logging Contract, Snapshot (including the JSON example) and Configuration; remove the pending markers. Review Usage Examples 1 and 4, which set `WithRestartDelay`, for the changed meaning.
- ADR-023: status "implemented in v0.21.0"; the amendment paragraph becomes history-free current text.
- Worker guide: a "Restart backoff" section with the rate estimates, the recovery trade-off, the pool key, and the fixed-delay example — with the pool cap at its default, `WithRestartDelay(time.Minute)` alone is fixed, and with `worker.max_restart_delay: 5m` it grows from one minute to five; the options list, the Configuration section (both keys) and the readiness note on later `failed`.
- CHANGELOG **BREAKING (behavior)** and two migration rows, in the CHANGELOG, the [pre-v1 migration guide](../guides/pre-v1-migration.md#workers) and the v0.21.0 release notes: the fixed restart delay becomes a 3 s base doubling to a 1-minute cap with floor-preserving jitter (both options set to the same value keep a fixed delay); and `WithMaxRestarts(N)` reaches `failed` — and `FailWhenFailed` readiness drops — later.

**Release.** Content pull request, then `release/v0.21.0` preparation (CHANGELOG version header and compare links, `docs/releases/v0.21.0.md`, `store/sqldb/go.mod` requiring `credo v0.21.0`, `releasegate tidy|prepared|candidate`), then `Release` dispatch after CI and CodeQL pass on the preparation commit.

## Out of scope

The alternatives recorded in [ADR-023](../adr/023-worker-system.md#alternatives-considered) and [ADR-010](../adr/010-middleware-architecture.md#startup-visibility-of-effective-features) stay out: opt-in or pluggable backoff, full jitter, a circuit breaker or elapsed-time budget, per-error-class delays, backoff for scheduled workers, the backoff streak in `Info`, cron time-zone selection, a separate startup line, Warn lines for features that are off, and a public feature-introspection API.
