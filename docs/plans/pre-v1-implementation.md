# Pre-v1 Contract Implementation Plan

**Status:** G1–G4 decisions accepted; P1–P3 (the DI minor) and P4 (the router minor) implemented 2026-09-05; P8, P5 and the performance follow-ups are pending. **Progress source:** [TODO.md](../../TODO.md#pre-v1-contract-migration). This plan defines sequence, scope and acceptance; progress checkboxes live only in TODO.

## Contract map

| Work | Canonical decision | Detailed contract |
| --- | --- | --- |
| P1–P3: bootstrap, DI ownership and teardown | [ADR-022](../adr/022-bootstrap-and-di-ownership.md) | [Bootstrap and DI lifecycle](../specs/bootstrap-and-di-lifecycle.md) |
| P4: endpoint-owned path parameter names (implemented) | [ADR-007 radix tree](../adr/007-router-and-routing.md#radix-tree) | [Router URL parameters](../specs/router.md#url-parameters) |
| P5: escaped URL round trips | [ADR-007 URL amendment](../adr/007-router-and-routing.md#pre-v1-url-round-trip-amendment) | [Router URL contract](../specs/router.md#pre-v1-url-round-trip-contract) |
| P8: built-in HTTP features | [ADR-010](../adr/010-middleware-architecture.md#built-in-http-feature-configuration-criterion) | [HTTP features](../specs/http-features.md) |
| A/B: measured performance changes | Evidence required per change | [Wire hot-path plan](wire-hot-paths.md) |

Breaking changes are allowed before v1. Each behavioral/wire theme gets its own minor; no advance minor announcement or deferral to the v1.0.0 batch is required for this work. The existing v1 batch in TODO retains its existing scope. Do not choose release numbers until preparing the release.

## Decision gates

**Closed by user acceptance on 2026-09-05.** The gate identifiers below remain as traceability labels for implementation and tests. No G1–G4 design approval remains outstanding. The P1 runtime preparation/shutdown gate is implemented and tested, so P8 may start.

| Gate | Accepted decision | Implementation scope |
| --- | --- | --- |
| G1: registration access | Reject Registry constructors without execution; AdoptValue validates a prebuilt binding and atomically protects it; failed/raced adoption stays repairable | P1/P2 |
| G2: DI diagnostics and late cleanup | ErrDIClosed, DIShutdownError, DIPanicError; one fixed five-second wait only for late-construction cleanup | P3 |
| G3: URL semantics | Keep raw segment boundaries, decode captures once, validate regex on decoded values; malformed encoding/UTF-8 is 400, regex mismatch is no match | P5 |
| G4a: HTTP names/installation | WithRecoverConfig plus WithoutRecover precedence; successful inactive UseI18n consumes registration, genuine setup errors do not | P8 registration |
| G4b: lazy locale | One Detect func(*Context) string; first-use memoization, current auth/request lifetime, default fallback and no recursive/repeated detection after failure | P8 locale |
| G4c: transport/observation | Decompress before Global with original-request selection; post-compression body bytes and duration through finalization; recovery-aware callback failures and no second response | P8 executor/transport |

AdoptValue and the DI diagnostic types are callable; the new HTTP names remain target APIs pending implementation. Lazy locale preserves first-access data: changing the detector signature does not extend principal lifetime through middleware request restoration. Applications can resolve Locale after auth to retain that language for later errors; earlier locale reads still win. The spec defines the full failure and measurement contracts; implementation does not reopen these decisions by adding parallel APIs or re-detection.

## Delivery sequence

P1–P3 shipped together in one DI minor and P4 in its own router minor (2026-09-05); steps 2–5 stay as the acceptance record until the plan is deleted with the last pending phase. P8 follows the DI minor; the shared P1 HTTP gate it builds on (`app.frozen` set at preparation/shutdown admission, `checkFrozen`) is implemented and tested, and P8 must use it rather than swap guards later. P6/P7 never block P8.

### 1. Baseline and contract tests

- Record the implementation base and working-tree state. Confirm that the wire benchmark suite (`wire_benchmark_test.go` and the i18n benchmarks) is present before using it.
- Land this documentation promotion as a docs-only pull request branched from `main`. Merge the wire benchmark suite through its own pull request first; plans reference it by file name, not by commit, because squash merges do not preserve branch commit ids. Pre-existing documents had drifted from the repository paragraph-mode formatting; commit the prettier reflow of unchanged content separately from the promotion so reviewers see only substantive changes in the second commit.
- Implement positive G1/G2 contract tests from the accepted spec (done with the DI minor). Do not keep obsolete repro tests whose expected outcome is the defect itself.
- Verify any copied upstream source at a pinned revision and preserve license headers. A reviewed reference tag is not proof of the original adaptation revision; do not invent NOTICES provenance.

### 2. P1: shared bootstrap and HTTP preparation gate

**Done 2026-09-05.** Areas: App lifecycle/server preparation, DI write freeze, shared registration guards.

- Implement DI-only Finalize and one stored Finalize → compile → publish result for all serve paths.
- Coordinate managed start, building Shutdown, direct ServeHTTP preparation and publication. Roll back managed starting on every preparation failure without retrying a frozen failed plan.
- Freeze HTTP registrations at preparation/shutdown admission; permit DI-backed route/renderer composition after explicit Finalize. Add callback-free lifecycle 503 with stopped precedence.
- Enable bootstrap/serverless Shutdown without successful Seal; retain external-server ownership.

Acceptance: direct readiness without explicit Finalize; repeated preparation errors; compile panic rollback; startup/shutdown/publication races; cached handler/error followed by stopped; HEAD and disabled-recovery 503; zero DI/user callback activity on rejection; prepared stopping health/drain. The gate is complete only when all HTTP write surfaces use it and late publication is tested.

### 3. P2 and phase APIs: migrate integrations atomically

**Done 2026-09-05.** Areas: public/internal DI registration APIs, `store.Register`/Registry, worker Pool, health seams, testutil, SaaS composition root and DI examples.

- Add non-resolving Has and shared AdoptValue; reject Registry constructor adoption without running it. Validate ready values and compare-and-protect only the unchanged accepted binding.
- Return old-instance information from Replace/MustReplace and transfer cleanup only on success.
- Remove ProvideFactory/MustProvideFactory; update constructor registrations and phase boundaries.
- Preserve reservations, resource identity, protected publication, invalid-value repair, managed Pool adoption, seam rewiring and registration-window rejection. Avoid a new public API solely to support a duplicate internal probe.
- Move all dependency writes before Finalize and all constructor-backed resolves after it.

Acceptance: Has never constructs/protects; Registry constructors are rejected without invocation; failed/raced AdoptValue never protects or returns a stale binding; invalid Registry repair; protected Replace rejection; old value ownership and unbuilt-constructor boolean; no stale adopter; worker registration ordering in both directions; testutil overrides precede Finalize.

### 4. P3: completion, graph scheduler and shutdown report

**Done 2026-09-05.** Areas: `internal/di` entry completion, canonical graph extraction, lifecycle teardown and diagnostics.

- Record success/error/panic as one terminal construction result and wake all waiters.
- Atomically coordinate closing, resolution admission, delivery and cleanup ownership. Extract teardown graphs even after missing/failed Seal; include visible value and collection edges.
- Implement reverse-registration-priority Kahn traversal, pending builds and non-Shutdowner retirement. Bound sequential normal calls with the shared context; isolate panics in helpers.
- Add the separate one-attempt five-second late-construction path. Expose ErrDIClosed, DIShutdownError and DIPanicError with immutable snapshots and unwrapped causes.
- Document dependency capture in hooks and log late completions without mutating returned reports.

Acceptance: consumers before dependencies independent of registration order; aliases close once; non-Shutdowner intermediate edges; pending success/failure and independent cleanup; failed Seal bootstrap; constructor panic shared by all callers; cached-delivery/closing race; hung Shutdowner and blocked dependencies; no retry or second budget; late cleanup success/error/panic/timeout; report determinism, elapsed-versus-completed timing, error traversal and snapshot race safety.

### 5. P4: endpoint-owned path parameters (implemented 2026-09-05)

Areas: radix endpoint keys and positional captures, root dispatch/mux tests, router spec/guide.

- Remove node-owned names and name-mismatch rejection; resolve capture names from the matched endpoint. Keep duplicate method+shape and structural conflict checks.
- Cover shared-prefix names, different methods, regex/catch-all captures, backtracking, automatic HEAD, mounted dispatch and host-scoped path routing. Host-pattern matching is outside this change.
- Verify BuildURI still reads route-pattern names and path dispatch retains zero-allocation cases.

### 6. P8: optional HTTP features and one setup API

Start only after the implemented P1 gate passes. G4a–G4c are accepted; ship one HTTP minor.

1. Install feature configs once through Use APIs, with atomic failed-setup rollback and immutable snapshots. Keep recovery default-on through WithRecoverConfig and WithoutRecover precedence. Successful inactive i18n setup consumes its registration; genuine setup errors remain retryable.
2. Build the request executor around the three user middleware tiers. Integrate independent RequestID, recovery/error rendering, finalization and final-response AccessLog.
3. Integrate first-use Detect(*Context), memoized failure/default state and pool reset. Install Decompress before Global with original-request selection; retain both byte limits. Measure output body bytes after compression and duration through finalization. Apply recovery-aware callback fallback, post-response filter isolation and abort rules; preserve streaming/HEAD/hijack cleanup.
4. Migrate root/package tests, benchmarks, testutil, WebSocket compression tests, README, guides, package docs and examples. Remove old wrappers, options, setters, unused helpers and duplicate tests in the same change; preserve real protocol and failure contracts.

Acceptance: default App enables recovery alone; independent RequestID/AccessLog; framework logs retain level filtering; equivalent Use-call permutations; DI-backed single renderer registration; prepared/stopped boundaries; successful-inactive i18n duplicate rejection and failed-setup retry; zero/one locale detections, auth/request restoration, recursive/panicking detectors and pool reuse; compressed error responses and exact final access measurements; original-body selection and limits; renderer/filter/finalization failures with recovery enabled/disabled and immutable committed status; all meaningful contracts in the [HTTP spec](../specs/http-features.md#acceptance-checks-for-the-implementation).

### 7. P5 and performance follow-ups

P5 implements the accepted G3/ADR-007 amendment in a separate wire-contract minor. Escape generation by segment, decode captures once and test regex against decoded values without changing raw route boundaries. Verify the published %2F/%252F/%31/+/Unicode table, malformed encoding/UTF-8 400, regex mismatch/no-match behavior and generation errors. P4 shipped separately; P5 builds on its endpoint-owned names without reopening them.

Performance A1 → A2 → A3 and B follow the [measurement plan](wire-hot-paths.md). They do not require the DI minor. A2's Bundle cache can precede lazy locale; comparisons must use equivalent feature sets and distinguish unused, first-use and cached translations. B needs real HTTP/1.1, TLS and HTTP/2 validation. Removing wrappers or default features is not a demonstrated hot-path gain.

## Verification and release completion

Run focused regression tests per slice, then the repository's required checks at each complete delivery boundary. Use the supported Go version and the pinned lint setup in CONTRIBUTING/CI.

| Scope | Required evidence |
| --- | --- |
| Root module, including worker/WebSocket | `go test -race -count=1 ./...`, `go vet ./...`, build and required lint |
| `store/sqldb` | In-tree module workspace via the documented releasegate flow; race, vet and build against the changed root |
| `examples/hello`, `examples/saas` | Each module builds, vets and stays tidy; startup smoke plus graceful signal exit as in the existing CI Examples job |
| Lifecycle signals | Required Unix signal CI cases, plus portable fake-channel concurrency tests |
| Public module boundary | Existing replace-free release gate for root/nested-module compatibility |
| Documentation | Valid local links; no private-document links; current/target status labels; complete migration/removal notes |

Do not treat `.gitignore` as a Go package exclusion: ignored scratch Go files can still enter `./...`. Use a clean verification checkout without disposable repro packages; do not hide required tests with a blanket package filter. A root-only run does not test nested modules. Report any platform-limited check as pending CI instead of claiming it passed locally.

When a minor lands, fold its accepted target amendment into the current docs, update godoc and examples to callable APIs, remove superseded wording and add actual Changed/Removed migration entries to CHANGELOG. Keep documentation-only promotion distinct from runtime completion. Update TODO only after the implementation and its required checks are complete.

Documents under `docs/plans/` are delivery aids, not permanent references. When a minor lands, fold anything still relevant into ADR/spec and delete the plan; a plan never outlives the work it schedules.

## Backlog and exclusions

- P6: a compiled route model/read-only runtime view requires maintenance evidence. The candidate compiles GET/HEAD relations, effective middleware and meta from one definition; dispatch and introspection share it, and atomic tree publication replaces Mount's multi-insert preflight. Context.Route would expose a read-only view. A smaller alternative lets the HEAD twin delegate middleware/meta to its primary at compile time, preserving RouteInfo.AutoHead. Neither is required by P4/P8; do not turn this maintenance candidate into an unmeasured router rewrite.
- P7: read-only DI explanation may expose registered type, deps/dependents, state, captured source locations and first-build duration as detached snapshots. It must not resolve/adopt/probe services. Capture the registration call site once and the constructor location with runtime.FuncForPC; duration belongs to the first build, not cached resolves. This borrows do v2.1.0's ExplainService idea without its invocation frame bookkeeping. A String rendering is diagnostic, not stable output; no per-invocation frame map or web UI.
- OTel/Prometheus remain Phase 3.5, with no technical dependency on these changes. No new scope, transient lifetime, generic lifecycle-service taxonomy or public request-stage/plugin API.
