# App Split and Small Fixes

This plan covers two deliveries, in this order:

- **Part A**: four small fixes that change behaviour.
- **Part B**: the division of `App`'s implementation into internal parts, with no behaviour change.

Each part is its own pull request: B is a pure refactor whose evidence is that no test changes, so it never carries a behaviour change from A. Both land on `main` under the CHANGELOG's `[Unreleased]` heading; neither cuts a release ([CONTRIBUTING](../../CONTRIBUTING.md#releasing)). When B lands, what remains of this plan is folded into ADR-004, the specs and `TODO.md`, and this file is deleted.

Progress is tracked by the boxes below.

## Part A: Small fixes

One branch, one commit per item, one PR. Every item updates its spec and adds a CHANGELOG `[Unreleased]` entry in the short form ([CONTRIBUTING](../../CONTRIBUTING.md#changelog-and-release-notes)). A break also gets a row in the migration guide.

- [x] **A1. Text after a catch-all panics at registration** (`TODO.md`, Router Improvements).
  - **Problem:** the router accepts `GET /a/{p...}/x`, which can never match, and answers 405 for `/a/b/x`, `/a/b/c/x` and `/a/b`.
  - **Fix:** registration panics. `internal/pattern.NewMatcher` already rejects such a pattern for `middleware.Rewrite` with the message `pattern: text %q after the catch-all {%s...} in %q: a catch-all takes the rest of the path, so end the pattern with it`. The router parses patterns through the same package (`internal/radix/pattern.go`, `patNextSegment` in `internal/radix/tree.go`) but skips that check.
  - **Shape:** apply the check where the router registers a pattern, sharing the text with `internal/pattern` rather than copying it, so the router and `Rewrite` keep one message. The panic names the route as other registration panics do.
  - **Cover:** routes on the App, a group, a host group and `Static`. A mount prefix is not a pattern with a catch-all, so it is unaffected.
  - **Docs:** router spec and ADR-007, if either describes the catch-all's position; CHANGELOG as a **BREAKING (behavior)** entry, since a registration that used to succeed now panics.
- [x] **A2. Fields promoted from an unexported embedded struct** (`TODO.md`, 2.3 Validation).
  - **Problem:** such a field gets an empty field name in its violation, and a query or form bind never sets it. The cause is that both walks skip unexported fields, including unexported embedded structs: `validation.buildFieldMappingList` (`validation/rule.go`) and the bind's field walk (`decodeValues` in `bind.go` and the per-type field table behind it).
  - **Fix:** follow `encoding/json`, which already sets such fields for a JSON body. A non-pointer embedded struct is walked even when its type is unexported, and its exported fields are promoted. An embedded pointer to an unexported struct type is skipped, as `encoding/json` does, because the binder cannot allocate it.
  - **Check first:** for an embedded struct behind a pointer, `buildFieldMappingList` adds the embedded field's offset to the inner fields' offsets. Those inner fields live behind the pointer, not inside the parent, so a violation on one of them may be named wrongly. Write the test before changing the code. If the test confirms the bug, fix it in the same commit, mapping the pointee's fields by the pointee's own base.
  - **Docs:** validation spec (violation field names) and the binding guide; CHANGELOG under Fixed.
- [x] **A3. `ContractGuard` requires a Content-Type by default** (`TODO.md`, v1 Gate, deferred breaking changes, and 4.7).
  - **Problem:** `ContractConfig.RequireContentType` defaults to false, so a body-carrying request with no Content-Type passes a `MetaAccept` contract.
  - **Fix:** the default becomes "require". A bool cannot default to true, so the field becomes its opt-out: `ContractConfig.AllowMissingContentType bool`, whose zero value requires the header on a body-carrying request. `RequireContentType` is removed, so code that set it fails to compile.
  - **Docs:** the middleware spec (the `MetaAccept` row and the paragraph after it), the contract-guards guide if it shows the option, the migration guide (one row in a new `[Unreleased]` section), CHANGELOG as **BREAKING**. Tick both `TODO.md` boxes.
- [x] **A4. The bind failures that are not `bind_failed`** (the error-handling guide).
  - **Problem:** the error-handling guide's "Validation and bind failures" section lists only `bind_failed` reasons. Two neighbouring outcomes go unmentioned: the 415 `unsupported_content_encoding` (`credo.CodeUnsupportedContentEncoding`) for a non-identity `Content-Encoding` that `app.UseDecompress` did not unwrap, and the 500 `invalid_bind_target` (`credo.CodeInvalidBindTarget`) for a nil or non-pointer target or a field of a type the binder does not bind, whose reason is logged and never sent.
  - **Fix:** one short paragraph after the `bind_failed` reasons, with today's names. The local branch `docs/bind-error-codes` holds an older draft of it that says `middleware.Decompress` and omits the unsupported field type. Once this lands, that branch and its remote are deleted (the maintainer deletes the remote).

A5–A8 came after Part A merged: one more branch, one commit per item, one PR, under the same rules.

- [x] **A5. A whole-tree decode fails on `.env` entries.**
  - **Problem:** `config.Load()` reads `.env` entries without a prefix, and decoding is strict by default, so `Unmarshal("", &cfg)` fails on every `.env` entry and every `server.*` key the target does not cover. The Quick Start in `config/doc.go` decodes the whole tree.
  - **Fix:** the Quick Start reads its sections through `config.Section[T]`, and the README and guides are checked for other whole-tree examples. The config spec (strict decoding, next to the `.env` prefix policy) and the configuration guide state that a whole-tree decode sees every `.env` entry and the framework's `server` keys, with the remedies: decode sections, cover those keys, or load with `config.WithoutDotenv()`. The `.env` prefix policy stays.
  - **Error:** an unknown-key error names the source of a key that `.env` or the process environment set, from the record behind `ReloadResult.Overridden`: `config: unknown key "x_y" (from .env entry X_Y): …`.
  - **Docs:** CHANGELOG under Changed (the error text).
- [x] **A6. Migration rows for the whole-tree reads.**
  - **Problem:** the v0.26.0 migration rows map `cfg.Get[T]("app")` to a section, but not the whole-tree form with an empty key, which a section rejects.
  - **Fix:** rows map `cfg.Get[T]("")` and `cfg.MustGet[T]("")` to `cfg.Unmarshal("", &v)`, and `app.GetConfig[T]("")` and `app.MustGetConfig[T]("")` to `Unmarshal("", &v)` on the `RawConfig`, pointing to A5's caveat. The published v0.26.0 release notes stay as they are.
- [ ] **A7. A reload signal over a configuration that cannot reload is ignored silently.**
  - **Problem:** with a `RawConfig` other than `*config.Config`, SIGHUP logs that it was received, `app.Reload` returns its "cannot reload" error, and the handler logs that error only when the server is not running.
  - **Fix:** the handler decides before calling `Reload`, by the configuration's type, never by the error's text, and logs at Info `credo: reload signal ignored: the configuration cannot reload` with the signal and the configuration's type, in the shape of the `WithoutReloadSignals` line. The signal stays subscribed, so a routine SIGHUP (a log rotation, say) stays harmless.
  - **Docs:** one line in the lifecycle spec or the configuration guide; CHANGELOG under Changed (a log line).
- [ ] **A8. Input and access outcomes in `fault.Kind`** (`TODO.md` only).
  - `fault.Kind` covers data and concurrency outcomes but has no kind for invalid input, unauthenticated, permission denied or rate limited. One unchecked entry under 2.4 Error Handling records the design question, with gRPC's InvalidArgument, Unauthenticated, PermissionDenied and ResourceExhausted as the reference. No code.

## Part B: App's implementation in internal parts

One branch, opened after Part A has merged, one commit per step, one PR, and no API change. ADR-004 ("DI and Lifecycle Stay One Package") records the decision: the lifecycle manager's back-pointer becomes an explicit dependency, and the App's fields are grouped by subsystem, routing and HTTP and the server and TLS first.

**Rules for every step:**

- No test changes, except tests that read the moved fields. Those tests change only their field paths; `export_test.go` is the place for any accessor they need.
- The groups are struct types embedded in `App` by value, not by pointer, so the request path gains no indirection.
- Each group gets a short doc comment saying what it holds and who writes it: bootstrap writes, and the executor reads after preparation.
- A step that touches a field read on the request path carries benchstat evidence in the PR: `go test -run '^$' -bench 'ServeHTTP' -count=10 -benchmem` before and after ([CONTRIBUTING](../../CONTRIBUTING.md#performance-changes)).

**Steps:**

- [ ] **B1. Server and TLS.** Moves `serverCfg`, `tlsConfig`, `tlsConfigSet`, `tlsFilesSet`, `tlsFiles`, `httpRedirectAddr`, `configureServer` and `disableReloadSignals`.
- [ ] **B2. Routing.** Moves `mux`, `root`, `hosts`, `staticHosts`, `namedRoutes`, `mounts`, `statusHandlers`, `redirectTrailingSlash` and `preRoutingMW`.
- [ ] **B3. HTTP features and request settings.** Moves `recover`, `requestID`, `accessLog`, `compress`, `decompress`, `observers`, `errorRenderer`, `successRenderer`, `i18n` and `i18nRegistered`, and the request settings `debug`, `strictBodies`, `multipartMaxMemory`, `jsonOpts`, `errorJSONOpts`, `trustedProxies` and `foreignRequestReported`. The request path reads most of these, in `context.go`, `request.go`, `response.go`, `errors.go`, `accesslog.go` and `contexthead.go`, so this step needs the benchstat table.
- [ ] **B4. Health, DI and preparation.**
  - Health: `healthEngine`, `healthProbes`, `healthExposeErrors`.
  - DI: `container`, `rawConfig`, `modulesUsed`, `workers`.
  - Preparation: `prep`, `prepMu`, `frozen`.
  - What stays at the top of `App` is the logger, the lifecycle manager and the reload slot.
- [ ] **B5. The lifecycle manager's dependencies.**
  - **Problem:** `lifecycleManager` holds `app *App` and reaches back through it. In `lifecycle_manager.go` and `lifecycle_start.go` it uses the logger (`app.logger`, `app.Logger()`), the container, `serverCfg`, `reload`, `withComponentInfra` and `NewInfra`. In the files outside them, it uses the members the App's own methods call on its behalf.
  - **Fix:** replace the back-pointer with a dependencies struct passed at construction. The struct holds the logger, the container, the server group, the reload slot and the infra factory, and the preparation lock if the shutdown path needs it.
  - **Where the struct cannot hold a value:** where the manager calls back into App behaviour (preparation, the server build), take a function value rather than an interface with one implementation.
- [ ] **B6. Fold and delete.**
  - ADR-004: the App's implementation "is divided", with the groups named.
  - `TODO.md`: tick the Architecture Governance box "Divide `App`'s implementation into internal parts". The box "Exporting `App`'s parts" stays open as a separate decision.
  - CLAUDE.md is local; tell the maintainer what changed rather than editing it.
  - Delete this plan.

## Gates

**Before each PR:** `gofmt`, `go vet`, the shadow analyzer and golangci-lint (as CI builds it) on both modules; `go test -race ./...` on the root module; `store/sqldb` tested against the in-tree root (`go run ./scripts/releasegate workspace` creates the workspace); both examples build and vet.

**Push, PR and merge** each wait for the maintainer's approval.
