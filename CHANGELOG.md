# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Credo is pre-1.0: minor (`0.x`) releases may contain breaking changes; when they do, the break is called out explicitly under **Changed** or **Removed**.

The `store/sqldb` submodule is versioned in lockstep with the root module (path-prefixed `store/sqldb/vX.Y.Z` tags — see [CONTRIBUTING.md#releasing](CONTRIBUTING.md#releasing)); its changes are recorded here.

The `v0.1.0` section records the initial public development baseline; it was not published as a Git tag. Tagged root and `store/sqldb` releases begin with `v0.2.0`.

| Release module | Compatible root module |
| --- | --- |
| `github.com/credo-go/credo/store/sqldb vX.Y.Z` | `github.com/credo-go/credo vX.Y.Z` |

## [Unreleased]

## [0.11.0] - 2026-08-24

### Changed

- **Breaking:** the machine-readable `Code` is now the primary stable identity of `HTTPError`, and `NewHTTPError`'s optional second argument is that code — not a message key: `NewHTTPError(status int, code ...string)`. The constructor is strict: it panics on a status outside `100..999`, on more than one code argument, and on a code that does not match `^[a-z0-9]+(_[a-z0-9]+)*$` (registration-time misuse fails fast; a panic reached at request time is caught by built-in recovery and rendered as a fail-closed generic 500 that publishes none of the invalid fields). A codeless call materializes `Code` from a frozen, committed `statusToCode` table generated once from Go 1.27's `http.StatusText` (62 entries, fixture-locked; unknown status → `http_<status>`) — the table never tracks the live standard library, so codes cannot silently rename. `WithCode` and message-key last-dot-segment code derivation are removed; message keys become optional presentation attached with the new copy-on-write `WithMessageKey`. The positional argument keeps its Go type while changing meaning, so old keyed calls may compile — the strict grammar turns dotted keys and literal messages into loud panics under test; see the migration table in [docs/releases/v0.11.0.md](docs/releases/v0.11.0.md). See [ADR-009](docs/adr/009-handler-and-error-handling.md).
- **Breaking:** sentinel field state changed: sentinels now materialize their code and carry no stored key — `ErrNotFound.Code == "not_found"`, `ErrNotFound.MessageKey == ""`. `ErrorInfo.MessageKey` continues to carry the effective classification key (now `errors.<code>`) to renderers. Default locale keys for built-in HTTP titles move from `http.*` to `errors.*` (`http.not_found` → `errors.not_found`); `http.validation_failed` and `http.request_timeout` are kept because framework code references them as explicit keys. Bundles using the old defaults must rename.
- **Breaking:** `HTTPError.Error()` now prints machine identity first — `credo: http error: status=…, code=…, key=…, internal=…` — omitting empty segments. String assertions on the old format break.
- Classification is fail-closed at the boundary: a directly constructed `HTTPError` struct with an out-of-domain status or grammar-violating code, and a legacy `HTTPStatus() int` provider returning a value outside `100..999`, are classified as a generic 500 and none of the invalid fields reach the wire.

### Added

- `HTTPError.WithMessageKey(key string)` — copy-on-write presentation-key builder, the successor to passing a key positionally. Title resolution is now two explicit chains: an explicit `MessageKey` resolves i18n bundle → built-in English default → the key itself as literal text; without a key the title resolves bundle(`errors.<code>`) → `http.StatusText` → `"HTTP <status>"`.
- Every classified error now carries a `code` member on the wire: previously code-less bodies (sentinels without derivable keys, unknown statuses, generic 500s) gain the frozen default — a strictly additive RFC 7807 change. Errors whose code was previously derived from a built-in key keep byte-identical bodies.
- An informational drift canary logs (never fails) when Go's `http.StatusText` diverges from the frozen table, so deliberate table updates remain a reviewed decision.

## [0.10.0] - 2026-08-24

### Changed

- **Breaking:** `credo.SuccessRenderer` is now shape-only, completing the symmetry with v0.9.0's `ErrorRenderer`: `func(ctx *Context, info RenderInfo) any`. The renderer receives `RenderInfo{Status, Data, MessageKey, Meta}` and returns the body; the framework owns the write — status, the application JSON profile, and the bodiless-status rule apply centrally. Returning nil writes `Data` plain (selective enveloping); a renderer that commits the response itself keeps full control and its return value is ignored; a renderer panic is caught by built-in recovery like any handler panic. `Context.Render` gains variadic `RenderOption`s — `credo.RenderMessageKey(key)` and `credo.RenderMeta(v)` — carrying the two side channels every envelope needs (resolved message, pagination-style metadata); with no renderer installed they are silently dropped. Migration: change the signature from `func(c, status, data) error` to `func(c, info) any`, build the envelope and `return` it instead of writing; a renderer that wrote its own non-JSON response keeps the write and returns nil only after committing. See [ADR-009](docs/adr/009-handler-and-error-handling.md).
- **Breaking:** `HTTPError.Code` (the HTTP status, an int) is renamed to `HTTPError.Status`. The field's type changes with its name, so nothing breaks silently — code reading `.Code` as an int fails to compile; replace it with `.Status` (or the unchanged `HTTPStatus()` method). `NewHTTPError`'s signature and the sentinels are unaffected. The rename frees the `Code` name for the machine-readable string code below.

### Added

- First-class machine-readable error codes and structured details on the RFC 7807 wire, as extension members: `HTTPError` gains `Code string` and `Details any` with copy-on-write `WithCode(string)` / `WithDetails(any)` builders, and `ProblemDetails` gains matching `code` / `details` members (`omitempty`). When `Code` is unset it is derived from the message key — the segment after the last dot (`"user.email_exists"` → `"email_exists"`, built-in `http.not_found` → `"not_found"`); a dotless literal message yields no code. Validation failures carry `"code": "validation_failed"`, bind failures the decode reason (`"syntax"`, `"type_mismatch"`, …) as the top-level code. Renderers read both off `info.Problem`; `ErrorInfo` gains no duplicate fields. See [ADR-009](docs/adr/009-handler-and-error-handling.md).

- Debug-mode envelope-bypass diagnostic: with a `SuccessRenderer` installed and debug mode on, a handler that writes a body-carrying JSON response through the raw `Response.JSON` helper instead of `Context.Render` triggers `WARN "credo: response bypassed the success envelope"` with the route pattern and name. Deliberate raw endpoints (webhooks, third-party shapes) silence it with the new `credo.MetaRawResponse` route meta (group-inheritable, route overrides group). Framework-internal writes (`Render`, the error pipeline) and the non-JSON writers (`Text`, `Blob`, `XML`, streaming) never trigger it; non-debug runs pay nothing.

### Fixed

- Body-writing response helpers (`Response.JSON`, `Text`, `HTML`, `XML`, `Blob`, `Stream`) now treat body-forbidding status codes — 1xx, 204 No Content, 304 Not Modified — as status-only: the body and the Content-Type header are skipped and the call returns nil (`Stream`'s reader is never read). Previously `JSON(204, body)` failed inside net/http after the header was committed, surfacing as a spurious `"credo: error after response committed"` warning and a misleading `Content-Type: application/json` on a bodiless response; handlers no longer need to special-case 204 themselves. Handler-set headers such as ETag and Cache-Control on a 304 are preserved.

## [0.9.0] - 2026-08-24

### Changed

- **Breaking:** `credo.ErrorRenderer` is now shape-only: `func(ctx *Context, info ErrorInfo) any`. The renderer returns the response body instead of writing it — a non-nil value is encoded with the application's JSON profile and written with `info.Problem.Status` (mutate it before returning to change the status); nil keeps the default RFC 7807 body, which turns headers-only and side-effect-only renderers (Sentry, `Retry-After`) into a plain `return nil`. Classification, logging, Content-Type, HEAD handling, and committed-response guards stay framework-owned; a renderer that commits the response itself keeps full control, and its return value is ignored. The previous warn-and-fallback path for a renderer that wrote nothing is gone — nil is the documented signal now. Migration: add `any` to the signature, replace the final write with `return body` (or `return nil` after a self-commit). See [ADR-009](docs/adr/009-handler-and-error-handling.md).

### Documentation

- The response-envelope story is now actually documented: the error-handling guide gained a "Response Envelopes" section pairing `ErrorRenderer` with the long-shipped but under-documented `SuccessRenderer`/`Context.Render` seam, the context spec documents `Render`, and ADR-009 records the shape-only renderer contract.
- Where the error pipeline begins is documented: 404/405, bind failures, 413, and panics all render as RFC 7807 through `ErrorRenderer`, while `net/http`'s pre-routing rejections (431, malformed-request 400, unsupported-transfer-encoding 501) are plain text written straight to the connection — see the error-handling guide's "What the Pipeline Does Not Cover".

### Fixed

- `examples/saas` failed at startup with a config-key mismatch; it is repaired, each example settles on a single config format (hello = JSON, saas = YAML), and `examples/hello` no longer reports a graceful shutdown as exit 1. CI now runs every example end to end — startup, a live request, and a clean SIGTERM — instead of only compiling it.

## [0.8.0] - 2026-08-23

### Added

- `credo.WithHTTPServer(fn func(*http.Server))`: a callback that receives the `*http.Server` the framework built, keeping the whole `net/http` surface reachable — `Protocols` (including H2C), `HTTP2`, `ConnState`, `BaseContext`, `ConnContext`, `DisableClientPriority` — without an option per stdlib field. It runs after every framework-set field, so it wins on all of them, config keys included; `Handler`, `Addr`, and `TLSConfig` are re-imposed afterwards (TLS is configured through `WithTLSFiles`/`WithTLSConfig`, never here). The `WithHTTPRedirect` listener is excluded, and everything set in the callback is restart-only. See [ADR-006](docs/adr/006-application-lifecycle.md).
- `server.max_header_value_count` config key: caps the number of header lines per request (Go 1.27's `http.Server.MaxHeaderValueCount`). Zero keeps net/http's own default of 500; a negative value is rejected by `credo.New`. Requests over the limit receive `431`, written straight to the connection by `net/http` and therefore never logged.

### Fixed

- `App.ServeContext`'s documentation listed H2C among the listeners it enables. It supplies the listener only — the server still came from the framework, so `Protocols` was unreachable and H2C could not actually be served. The documentation now points at `WithHTTPServer`, which makes it work.

## [0.7.0] - 2026-08-23

### Added

- `credo.WithJSONOptions(opts ...jsonv2.Options)`: overrides Credo's JSON response encoding profile per axis (for example `jsonv2.FormatNilSliceAsNull(true)`, `jsontext.EscapeForHTML(true)`, or `jsonv1.DefaultOptionsV1()` for full legacy output). Construction-time only; see [ADR-021](docs/adr/021-json-output-profile.md).

### Changed

- **Response encoding moved to `encoding/json/v2`** ([ADR-021](docs/adr/021-json-output-profile.md)). `Response.JSON` (and therefore `Context.Render`'s fallback) plus both RFC 7807 Problem Details writers now encode with a documented profile: sorted map keys and nanosecond `time.Duration`s as before, and four visible wire changes — nil slices and maps render as `[]` and `{}` instead of `null`, `omitempty` drops JSON-empty values rather than Go zero values (numbers and bools are no longer omitted; use `omitzero` for the old meaning), `<`, `>`, and `&` are no longer escaped, and no trailing newline is written. Each axis is opt-out through `WithJSONOptions`. Problem Details always sort map keys regardless of the application profile.
- `net/http` server diagnostics — TLS handshake failures, listener accept errors, panics that escape the framework recovery, superfluous `WriteHeader` calls — now go through the application logger at `ERROR` with `component=net/http` instead of the standard `log` package's stderr output. The stdlib message text is unchanged. Header-limit rejections (`431`) are written directly to the connection by `net/http` and are still not logged.

## [0.6.0] - 2026-08-23

### Changed

- Internal JSON decoding (JSON config files, i18n locale files, `testutil` log assertions, the release gate) moved from `encoding/json` to `encoding/json/v2`. JSON config and locale files now reject duplicate object members and invalid UTF-8 instead of taking the last value / substituting U+FFFD. `validation.Errors` additionally implements the v2 `MarshalJSONTo` interface; `MarshalJSON` is unchanged.

### Fixed

- `Request.BindBody` could not decode a target with a `time.Duration` field: every payload failed with 400 `invalid_value` because json/v2 has no default Duration representation (and Go 1.27 ships without the `format:` tag). Duration fields now decode as integer nanoseconds, matching `encoding/json` v1.

### Added

- `middleware.ContractConfig.RequireContentType`: when set, a request that carries a body (positive or unknown `Content-Length`) but no or an empty `Content-Type` header fails a route's `MetaAccept` contract with 415 instead of passing. Default off; bodiless requests and routes without `MetaAccept` are unaffected. Flipping the default is a v1 item.
- Strict request bodies: `credo.WithStrictBodies()` and the `server.strict_bodies` config key make `Request.BindBody` reject JSON object members that map to no target field with a 400 `BindError` of reason `unknown_field` (new `BindReasonUnknownField`, i18n key `bind.unknown_field`). The default stays lenient (unknown members ignored); the switch is app-wide and affects JSON only. ADR-008 revised accordingly.

## [0.5.0] - 2026-08-23

### Added

- **Reload: `app.Reload`, `SIGHUP`, `OnReload`, `OnConfigChange[T]`** — a trigger-driven partial reload ([ADR-020](docs/adr/020-reload-and-partial-config-reload.md)). `app.Reload(ctx)` (running-only, serialized) stages the configuration through `config.Stager`, decodes and validates every affected `app.OnConfigChange[T](key, fn)` subscriber's `T` against the candidate before publishing (a failure aborts with the old snapshot untouched), then runs framework participants, affected subscribers in registration order, and `app.OnReload` hooks FIFO; errors and recovered panics are joined and returned, never rolled back, and never stop the process. Changed keys with no subscriber are logged once at `WARN` as `restart required` (key paths only). Under `app.Run()` on Unix, `SIGHUP` now triggers `Reload` within `WithReloadTimeout` / `server.reload_timeout` (default 30s), coalescing signals that arrive mid-reload; `RunContext`/`ServeContext` stay signal-free. `OnConfigChange` panics at registration when the `RawConfig` cannot reload.
- **`config`: `Reloader`, `Stager`, `Staged`, `Changes`** — `*config.Config` re-runs its captured load pipeline (`Reload()` one-shot, or `Stage()` → inspect → `Commit()`) with an atomic snapshot swap; `Changes` is the sorted symmetric difference of leaf key paths with `Affects`/`Keys`/`Empty`. `CREDO_ENV` is fixed at first load.
- **File-based TLS certificate rotation** — `WithTLSFiles` and `server.tls.*` serve the key pair through `GetCertificate` backed by an atomic pointer and re-read it on every reload (in-place rotation needs no config change; changed `server.tls.*` paths are followed). A pair that fails to load keeps the current certificate and surfaces through the reload error. `WithTLSConfig` is untouched.
- **Deployment guide** — [`docs/guides/deployment.md`](docs/guides/deployment.md): signal table, systemd unit with `ExecReload`, `TimeoutStopSec` vs `WithShutdownTimeout`, `EnvironmentFile=` caveat, container/Kubernetes idioms, certbot deploy hook, admin-endpoint reload. The configuration guide gained "Reloading Configuration" and a **Reloadable** column.

### Changed

- **Go 1.27 GA toolchain** — modules, CI, and CodeQL now select the toolchain from `go.mod`; the `go1.27rc2` pin is gone. Dependency bumps: go-limiter v1.2.0, `golang.org/x/text` v0.41.0, modernc.org/sqlite v1.56.0.
- **`SIGHUP` under `app.Run()`** no longer terminates the process (Go's default); it reloads. Supervisors that used `SIGHUP` to stop a Credo service should send `SIGTERM`.
- **`WithTLSFiles` / `server.tls.*`** no longer populate `tls.Config.Certificates`; the pair is served through `GetCertificate`. Observable only to tests that inspected the resolved config.

## [0.4.1] - 2026-08-09

### Fixed

- **`store/sqldb`: ncruces/go-sqlite3 error classification** — the SQLite classifier now recognizes the [ncruces/go-sqlite3](https://github.com/ncruces/go-sqlite3) driver's typed error codes (`Code() ErrorCode`, `ExtendedCode() ExtendedErrorCode`), which previously satisfied neither the modernc `Code() int` interface nor the mattn field shape and passed through unclassified. Unique/constraint, busy/locked, and read-only failures from that driver now map to the same `store.Err*` sentinels as modernc and mattn. Recognition is structural (reflection over package path and method shape), so no SQLite driver becomes a Credo dependency. The [data access guide](docs/guides/data-access.md) now documents per-family driver recognition and the pass-through behavior of unrecognized drivers.

## [0.4.0] - 2026-07-23

### Added

- **Typed bind errors** — `BindBody`/`BindQuery` decode failures now return `*credo.BindError` carrying a machine-readable `Reason` (`syntax`, `type_mismatch`, `invalid_value`, `empty_body`, `trailing_data`, `duplicate_field`), the affected field path, the expected type, and the JSON byte offset. The error pipeline classifies it as `400 Bad Request` with `type: "https://credo.dev/errors/binding"` and a single validation-shaped `errors[]` entry (`code` = reason), localizable via `bind.<reason>` i18n keys with the `http.bind_failed` title key. The underlying decoder error stays server-side (`Internal`); Go type names are not leaked (`expected` uses JSON terms). Body-size overruns keep their dedicated 413 classification. See [ADR-009](docs/adr/009-handler-and-error-handling.md) and the [Context spec](docs/specs/context.md).

### Changed

- **Breaking (behavioral): strict JSON bodies** — `BindBody` now decodes JSON with `encoding/json/v2` (Go 1.27) under strict semantics. Exactly one JSON value is accepted per body: content after the first value (a second document, or trailing garbage) is rejected with reason `trailing_data` instead of being silently ignored (trailing whitespace remains accepted). Duplicate object members — previously last-value-wins — are rejected with reason `duplicate_field`, including case-variant repeats. Member-name matching against struct fields stays case-insensitive (v1-compatible), and unknown members remain accepted. Decode-error responses gained structured `errors[]` detail (previously a generic `invalid JSON body`-style title with no field information); empty-body, scalar-conversion, and JSON decode error messages changed accordingly.

## [0.3.0] - 2026-07-22

### Added

- **AccessLog policy controls** — the authoritative built-in access logger now supports a dedicated sink (`WithAccessLogLogger`), a dynamic status-derived threshold (`WithAccessLogMinLevel(slog.Leveler)`), and a positive post-response filter (`WithAccessLogResultFilter`). `AccessLogEntry` gives filters an immutable request/result snapshot while preserving the existing emitted attribute schema; `middleware.AccessLogConfig` exposes matching `MinLevel` and `ResultFilter` fields for route/group policies. Defaults remain unchanged (`Info`, all status classes), `Skipper` remains the pre-dispatch package convention, and `slog.LevelVar` can change the threshold at runtime. The built-in observes final error-renderer status/bytes/duration; configurable middleware retains its earlier observation boundary. See [ADR-010](docs/adr/010-middleware-architecture.md).

## [0.2.0] - 2026-07-13

### Added

- **Semantic faults** — the new stdlib-only `fault` leaf package defines transport-neutral `Kind` values and a `Provider` seam. Credo's root error pipeline applies its default HTTP policy from the semantic kind before consulting the legacy `HTTPStatus()` interface, while an outer `*HTTPError` still overrides that default. Error logging consumes the already-classified response status instead of independently re-deriving policy.
- **Data access (`store`)** — structured semantic errors: `store.Error` carries `Kind`, optional operation/resource/constraint/driver-code metadata, `Transient`, and the original cause. New exact sentinels distinguish `ErrAlreadyExists`, `ErrConstraint`, `ErrSerialization`, `ErrDeadlock`, `ErrContention`, and `ErrUnavailable`; `KindOf` and `IsTransient` expose classification without transport coupling. `ErrDuplicate` remains an alias of `ErrAlreadyExists`, and specific constraint/concurrency errors continue to match the deprecated `ErrConflict` umbrella.
- **Data access lifecycle ownership** — `store.WithCallerOwnedLifecycle()` is
  the explicit shutdown opt-out for a value that uses a separate
  `WithLifecycle(lc)` health handle. Direct `Lifecycle` values remain
  framework-owned after successful registration. The optional
  `store.LifecycleIdentityProvider` extension lets a semantic wrapper return
  the underlying resource pointer (or another stable identity token); an
  embedded `*sqldb.DB` promotes the adapter's implementation automatically.
  `App.CanProvideValue[T]()` provides the store registration path (and other
  composition helpers) a non-mutating frozen/direct-duplicate preflight; it is
  point-in-time only and does not reserve T or guarantee that a later
  normal/protected publication succeeds.
- **Data access pool diagnostics** — `sqldb.Config.MaxIdleTime` now wires
  `database/sql.SetConnMaxIdleTime`; `(*sqldb.DB).Stats()` exposes the complete
  `sql.DBStats` snapshot, and SQL health details include cumulative wait and
  idle/lifetime closure counters. A successfully registered unlimited pool
  emits one structured `sqldb.pool.max_open_unlimited` warning through the app
  logger. Standalone users can inspect the same secret-free signal through
  `(*sqldb.DB).StoreRegistrationWarningCodes()`.
- **Protected DI values** — `App.ProvideProtectedValue[T]` publishes a
  pre-built singleton that `App.Replace[T]` cannot overwrite;
  `App.ProtectBinding[T](expected ...T)` adds the same protection to an existing
  direct binding before Finalize. With no expected value it is idempotent and
  does not resolve T. With one expected value it performs an atomic CAS-style
  compare-and-protect against `Replace`: the singleton must already be resolved,
  comparable, and unchanged, otherwise no protection is added. These are
  low-level integration primitives for values coupled to external lifecycle or
  health state; ordinary application bindings should remain
  override-friendly with `ProvideValue`.
- **WebSocket server adapter** — new `websocket` package wrapping exact-pinned `coder/websocket v1.8.15` behind Credo-owned `Server`, `Conn`, message/close/compression types, secure same-origin and bounded-read defaults, subprotocol policy, RFC 7807 pre-upgrade errors, secret-safe lifecycle logging, and managed/manual graceful shutdown. Canonical registration is `ws := websocket.Use(app, cfg)` plus `app.GET(path, ws.Handler(handler))`; hubs, clients, heartbeat policy, and RFC 8441 remain deferred. See [ADR-019](docs/adr/019-websocket-integration-and-drain.md), the [spec](docs/specs/websocket.md), and the [guide](docs/guides/websocket.md).
- **Early pre-cancellation drain** — `app.OnPreDrain(func(context.Context) error)` runs unordered, panic-isolated hooks after readiness is withdrawn but before lifecycle cancellation. It is the narrow seam for work that must finish while lifecycle workers and DI infrastructure are still live; most subsystems should continue to use `OnDrain`. Hooks also run on failed-startup teardown. Deadline-ignoring work emits an immediate waiting diagnostic but remains a hard teardown barrier: lifecycle cancellation and infrastructure teardown wait for every hook to return, then the completion timestamp determines the final identified incomplete error and later phases continue with the same possibly-expired context. See [ADR-006](docs/adr/006-application-lifecycle.md).
- **Pre-infrastructure subsystem drain** — `app.OnDrain(func(context.Context) error)` runs unordered subsystem hooks concurrently with HTTP drain after lifecycle cancellation and before DI teardown. Deadline/panic failures are identified and joined; deadline-ignoring work is reported incomplete while teardown continues with the same absolute context. WebSocket is the first consumer: a completed drain proves active handlers finish before their repositories close, while an incomplete drain is reported explicitly. See [ADR-006](docs/adr/006-application-lifecycle.md).
- **Access logging** — `WithAccessLogSkipper(func(*credo.Context) bool)` installs a pre-dispatch predicate that excludes matching requests from the built-in access log without disabling it. The new `credo.MetaAccessLog` route meta (`route.SetMeta(credo.MetaAccessLog, false)`) silences a single route or, by `LookupMeta` inheritance, a whole group; a route-level value overrides its group's, and `middleware.AccessLog` honours the same meta. See [ADR-010](docs/adr/010-middleware-architecture.md).
- **Health checks** — `HealthConfig.LogRequests` (default `false`) keeps `/health` and `/ready` probe requests out of the access log; set it to `true` to log them. Because the meta is applied per route, `true` re-enables logging even under a group that silenced access logging. See [ADR-016](docs/adr/016-health-checks.md).
- **TLS** — `WithTLSFiles(certFile, keyFile)` and `WithTLSConfig(*tls.Config)` configure HTTPS, as do the `server.tls.cert_file` / `server.tls.key_file` config keys. Sources resolve by precedence (`WithTLSConfig` > `WithTLSFiles` > `server.tls.*` > plaintext; whole-source override, never a conflict error). `Run` and `RunContext` serve HTTPS automatically when TLS is configured; the certificate is validated once at preflight, so a bad cert — or an explicitly-set-but-empty/nil source (`WithTLSConfig(nil)`, `WithTLSFiles` with an empty path) — fails fast and rolls the state back to `building` rather than silently downgrading to a lower-precedence source or plaintext. `ServeContext` is TLS-exempt — wrap the listener with `tls.NewListener` for HTTPS. See [ADR-006](docs/adr/006-application-lifecycle.md).
- **HTTP→HTTPS redirect** — `WithHTTPRedirect(addr)` runs a second, plaintext listener that permanently redirects every request to its HTTPS equivalent (301 for GET/HEAD, 308 for other methods). It requires TLS (preflight fails fast otherwise) and binds, serves, and drains with the main server — closing before the main server on drain, and tearing the app down if the redirect listener fails at runtime, so a requested redirect never silently dies. `ServeContext` ignores it. See [ADR-006](docs/adr/006-application-lifecycle.md).
- **Configuration** — typed-snapshot getters over `Unmarshal`: `config.(*Config).Get[T](key) (T, error)` plus `MustGet[T]`, and `(*credo.App).GetConfig[T](key) (T, error)` plus `MustGetConfig[T]`. Each decodes a config section into a value of `T` in one call (the `Must` forms panic, matching the `MustProvide`/`MustResolve` family); there is deliberately no `MustLoad`. They are composition-root sugar — a handler has no `App` accessor, so typed config still flows to services via DI. See [ADR-005](docs/adr/005-configuration-architecture.md).
- **Data access (`store/sqldb`)** — typed terminal methods on `*SelectQuery` (Go 1.27 concrete-type generic methods): `One[T](ctx) (T, error)` and `All[T](ctx) ([]T, error)` return the queried type directly instead of scanning into a caller-supplied destination. `T` drives both the table and the destination, so the query is built model-less (`db.Select().Where(...).One[User](ctx)`). `One` applies `LIMIT 1` (first row; multiple matches are not an error — add `OrderExpr` for determinism) and maps no-row to `store.ErrNotFound`; `All` returns a non-nil empty slice with a nil error when nothing matches. Both use internal execution snapshots and inject the ambient transaction exactly like `Scan`. See [ADR-015](docs/adr/015-data-access.md).
- **Data access (`store/sqldb`)** — `(*SelectQuery).Page[T](ctx, req) (*pagination.Page[T], error)`, a typed pagination terminal completing the `One`/`All`/`Page` family. It runs COUNT + a LIMIT/OFFSET SELECT and returns a ready `*pagination.Page[T]`; `T` drives the table, so the query is built model-less (`db.Select().Where(...).OrderExpr(...).Page[User](ctx, req)`). `BindQuery` still applies `PageRequest.Validate`'s forgiving defaults/clamp policy, while the terminal copies the request and independently enforces strict execution invariants before COUNT. Nil, non-positive, native-offset-overflow, and Bun v1.2.18 signed-int32 LIMIT/OFFSET violations wrap `pagination.ErrInvalidPageRequest`; the caller's request is never mutated and valid custom `PerPage` values above 50 remain honoured. On zero rows SELECT is skipped and the page preserves the snapshot's page/per-page with a non-nil empty slice. COUNT and SELECT use internal execution snapshots and both join the ambient transaction, but a normal Read Committed transaction does not make the two statements share a snapshot. See [ADR-015](docs/adr/015-data-access.md).
- **Data access pagination count contract** — `Page.Total` is the cardinality of
  the complete logical projection before ordering and the page window: an
  ungrouped aggregate normally contributes one result row, `Distinct` counts
  selected projection tuples, `Group` counts groups, and `Group` + `Having`
  counts the groups left after `Having`. Credo now counts a universal outer
  `_credo_count_source` derived table after stripping root ORDER/LIMIT/OFFSET/FOR;
  its behavior is pinned by conformance tests. `Count` and `Page` reject
  standalone `Having` and direct `UNION`/`INTERSECT`/`EXCEPT` shapes with
  `sqldb.ErrUnsupportedCountQuery` before database I/O; restructure those
  shapes behind an outer derived table/CTE and compose an explicit count query,
  data query, and `pagination.NewPage`. The logical projection is evaluated by
  the count source; expensive, volatile, or set-returning projections should
  use that explicit custom composition. A first-class custom-count strategy is
  deferred until two real consumers need the same abstraction. `Page` retains
  exact-total metadata; total-free offset windows reserve `Slice[T]`, while
  keyset pagination reserves the distinct `CursorPage[T]`, rather than
  overloading `Page` with unknown totals.
  Logical COUNT also runs the model SELECT hook lifecycle on its private source
  (`BeforeSelect`, `BeforeAppendModel`, and successful-query `AfterSelect`), so
  hook-added filters/projections contribute to `Total`; a `Page` that reaches
  its data SELECT runs the lifecycle once for COUNT and once for SELECT. Count
  does not scan or mutate a bound model, and nondeterministic hooks/volatile SQL
  can still differ across the two executions regardless of isolation. MySQL is
  now the oracle for its unique derived-column-name rule: wildcard and
  implicit/unaliased projections pass when the server derives unique names,
  while logical-count `ER_DUP_FIELDNAME` (1060) is wrapped with
  `ErrUnsupportedCountQuery` after I/O and retains the driver cause. Raw and
  other non-count 1060 errors remain unchanged. Real tests cover normal mode and
  `NO_BACKSLASH_ESCAPES`. The outer count
  query preserves `QueryEvent.Model` for observability while soft-delete policy
  remains solely on the inner source.
- **Pagination snapshot guidance** — `Page` does not start an implicit
  transaction. PostgreSQL Read Committed can observe different statement
  snapshots; an outer read-only Repeatable Read transaction is the documented
  PostgreSQL/MySQL path when one snapshot is required. MySQL's guarantee is
  limited to InnoDB ordinary nonlocking reads and configured isolation. SQLite
  uses the first-read snapshot of an explicit transaction; WAL permits a
  concurrent writer while rollback-journal mode may serialize it. The pinned
  modernc SQLite driver does not reliably enforce `sql.TxOptions.Isolation` or
  `ReadOnly`, so SQLite callers use plain `InTx`; driver-capability validation is
  deferred as a fail-loud follow-up.
- **Data access (`store`)** — typed transaction scopes: `NewTxScope[T]()` fixes the transaction contract once, and `scope.WithTx`, `GetTx`, `RequireTx`, and `Conn` all use the same `T`. This prevents a concrete transaction from being stored and then silently missed through an interface-typed read. `WithTx` rejects nil and typed-nil transaction handles; `RequireTx` and its free-function counterpart return the new `store.ErrTxMissing` instead of falling back. Distinct scopes isolate multiple logical connections with the same transaction type. See [ADR-015](docs/adr/015-data-access.md).
- **Data access (`store/sqldb`)** — `DB.Conn(ctx) bun.IDB` exposes the active per-DB transaction, or the base DB outside a transaction, for advanced native Bun operations; `DB.RequireTx(ctx)` returns `store.ErrTxMissing` rather than falling back. Native executions participate in the selected transaction but still bypass Credo error mapping. See [ADR-015](docs/adr/015-data-access.md).
- **Data access (`store/sqldb`)** — `WithTxCleanupTimeout(d)` configures how long Credo waits for each nested savepoint creation/release/rollback and fail-safe ambient abort (default five seconds, positive values only). Callback duration is unaffected. Uncertain nested operations synchronously mark shared state rollback-only; an outer callback that swallows the inner error returns the new `ErrTxRollbackOnly` rather than committing.
- **Pagination** — `(*pagination.Page[T]).Map[U](fn func(T) U) *Page[U]` (a Go 1.27 generic method) returns a new page with each record transformed by `fn`, carrying the pagination metadata (`Total`, `Page`, `PerPage`, `TotalPages`) over unchanged. It is the canonical model→DTO step: build `Page[Model]` from `SelectQuery.Page` and `Map` it to `Page[DTO]`, so an intermediate `Page[Model]` no longer means hand-copying metadata. `fn` must be pure — a nil `fn` panics, even for an empty page; for a fallible conversion, map in the service and build with `NewPage`. See the [pagination spec](docs/specs/pagination.md).

### Changed

- **Data access migration cleanup** — `DB.Migrate` now attempts Unlock with a fresh cancellation-detached five-second budget and bounds caller wait even when a driver ignores context. Migration and unlock errors remain joined. An unlock timeout is explicitly an uncertain outcome and is not automatically retried. Production guidance now prefers one deadline-bounded pre-deploy job for multi-replica releases; `app.OnStart(db.Migrate)` remains the dev/single-replica convenience, and mark-on-success is documented as at-least-once bookkeeping that still requires transactional or replay-safe migrations.
- **BREAKING — generated `sqldb` DSNs now fail loud on ambiguous or invalid
  connection configuration.** Driver-family inference uses exact aliases
  instead of substring matches; generated PostgreSQL/MySQL DSNs require a
  non-zero port and serialize IPv6 hosts correctly; positive fractional
  PostgreSQL connect timeouts round up to one-second units. Conflicting
  structured fields/driver options and explicit nil `WithDialect` or
  `WithConnector` values now return secret-safe startup errors, as do known
  driver/dialect family mismatches. Raw `Config.DSN` and non-nil custom
  connectors remain the driver-native escape hatches.
- **BREAKING — `sqldb.Config.MaxIdle` is now `*int`.** `nil` makes Credo leave
  the idle setter untouched (the effective stdlib default remains subject to
  `MaxOpen`), `new(0)` explicitly disables idle retention, and a positive value
  is applied exactly. Migrate
  `MaxIdle: 10` to `MaxIdle: new(10)`. With finite `MaxOpen`, an explicit
  `MaxIdle > MaxOpen` now fails `Open` instead of being silently clamped.
  `MaxOpen=0` remains unlimited; Credo does not impose a workload-independent
  finite default. `MaxIdleTime=0` and `MaxLifetime=0` disable their respective
  expiry policies. The added `MaxIdleTime` field also changes positional
  `sqldb.Config` literals: migrate them to keyed fields, which is the supported
  form for this evolving beta config struct.
- **Cursor/keyset pagination design gate** — the reserved first result is a
  forward-only `CursorPage[T]`; future total-free offset pagination keeps the
  separate working name `Slice[T]` pending its own design gate. Cursor execution owns a stable non-null keyset with
  an explicit unique tie-breaker, uses `per_page + 1`, performs no COUNT, and
  requires an explicit scope-bound HMAC keyring for public HTTP tokens. No
  cursor symbols ship yet: implementation remains gated on a concrete
  consumer, a fail-loud Bun hook boundary for terminal-owned order/window
  state, invalid-argument transport mapping, canonical wire vectors, and real
  PostgreSQL/MySQL/SQLite conformance. See
  [ADR-015](docs/adr/015-data-access.md).
- **BREAKING — `SelectQuery.Count` now counts complete logical projection rows.**
  It wraps the projection as `_credo_count_source` after removing root
  ORDER/LIMIT/OFFSET/FOR, so ungrouped aggregate, distinct, and grouped queries
  return their result-row cardinality instead of Bun's replacement-projection
  count. The projection can now be evaluated during COUNT. Standalone `Having`
  and direct compound roots, which previously could error or return a misleading
  count, fail before I/O with `ErrUnsupportedCountQuery`; use an explicit
  derived-table count/data composition for those or for expensive/volatile
  projections. MySQL now decides derived-source output validity at execution:
  logical-count 1060 is wrapped with `ErrUnsupportedCountQuery` and preserves
  its cause, while former wildcard/implicit-expression false positives and
  non-count 1060 are no longer narrowed by a local parser.
- **BREAKING — `pagination.PageRequest.Offset` now returns `(int, error)`.** Migrate `offset := req.Offset()` to `offset, err := req.Offset()` and handle `pagination.ErrInvalidPageRequest` with `errors.Is`. Unlike `Normalize`/`Validate`, `Offset` is strict and non-mutating: it rejects non-positive values and native `int` multiplication overflow instead of silently producing an unsafe offset. `SelectQuery.Page` adds the narrower Bun v1.2.18 signed-int32 LIMIT/OFFSET check and rejects every invalid request before COUNT; it does not clamp valid custom page sizes.
- **Data access (`store/sqldb`)** — curated `SelectQuery.Limit` and `Offset` now reject values outside Bun v1.2.18's signed-int32 storage range with `sqldb.ErrInvalidLimitOffset`; the builder records the error and its terminal returns before database execution instead of allowing an `int`→`int32` narrowing. Values inside the range, including zero and negative values, retain Bun semantics. Raw Bun reached through `Apply` or `Unwrap` remains an explicit escape hatch and is not covered by the curated-method guard.
- **BREAKING — store registration ownership and publication are now explicit.**
  `WithLifecycle(lc)` no longer succeeds with a warning when the registered
  value cannot be shut down by DI; pair it with
  `WithCallerOwnedLifecycle()` and arrange shutdown yourself, or make the
  registered value implement the complete `store.Lifecycle` contract. A value
  that already implements `Lifecycle` may no longer supply a second lifecycle
  or caller-owned opt-out, and a Shutdowner-only value cannot split Shutdown
  from Ping/Health. Ownership transfers only when `Register` succeeds; every
  failure remains caller-owned. For a framework-owned value DI is the sole
  framework shutdown owner: a teardown makes at most one attempt if its live
  deadline reaches the registration, and may make none if the deadline expires
  first. Migrate embedded wrappers by removing their redundant `WithLifecycle`
  option. Migrate named-field wrappers by delegating all Lifecycle methods
  (framework-owned) or adding the explicit caller-owned option and an
  `OnShutdown` hook.
- **BREAKING — store names, types, and declared resource identities are unique.**
  Explicit empty, padded, control-character, and reserved `credo.` names are
  rejected rather than normalized; omitted names use the pointer-unwrapped,
  package-qualified named type and unnamed types require `WithName`. Register
  privately reserves name, DI type, and resource identity, keeps pending
  entries invisible, and commits Registry visibility only after Ping and
  authoritative DI publication both succeed. Identity defaults to the
  top-level Lifecycle value itself; pointer-backed implementations are
  recommended. A semantic/named wrapper must implement
  `ResourceIdentity() any` explicitly and return its underlying pointer; Credo
  does not scan struct fields. Tokens must be non-nil, comparable, reflexively
  equal, and stable; non-comparable values and NaN-like tokens fail before Ping.
  Within the `store.Register` ledger, a repeated identity under another type or
  ownership mode is rejected. Migrate a second interface view to
  `app.Alias[I, T]()` instead of re-registering it. Raw `app.Provide`,
  `app.ProvideFactory`, `app.ProvideValue`, `app.ProvideProtectedValue`, or
  `app.Replace` calls are outside this ledger and must not publish the same
  lifecycle again; a caller-owned handle must not also be registered in DI as a
  Shutdowner.
  Concurrent external DI mutation can still win after the point-in-time
  preflight; final publication remains authoritative.
- **BREAKING — successful store and Registry DI bindings are protected.**
  `store.Register[R]` publishes `R` with `ProvideProtectedValue`, and the
  Registry binding is created protected or, when supplied by the composition
  root, resolved and validated before its pointer is atomically
  compare-and-protected against `Replace`. A raced replacement, invalid nil, or
  failing Registry binding remains unprotected and replaceable before Finalize
  so composition can repair it and retry. After adoption, later
  `app.Replace[R]` and `app.Replace[*store.Registry]` calls return an error
  instead of detaching DI from lifecycle/readiness state. Install
  test/application substitutes before registration or register the intended
  lifecycle value on a fresh App.
- **Data access health compatibility** — `store.Health` adds a JSON-excluded typed `Cause error` field, and `store/sqldb.Health` now places ping failures there instead of copying their text into `Details["error"]`. Keyed struct literals and `Lifecycle.Health(ctx) Health` implementations remain source-compatible; positional `store.Health{...}` literals must add the new field or migrate to keyed literals.
- **Data access (`store/sqldb`)** — driver error normalization is now context- and driver-family-aware. PostgreSQL SQLSTATE, strict MySQL error envelopes, and SQLite numeric codes preserve the original cause and driver code in `*store.Error`; constraint, serialization, deadlock, contention, timeout, unavailable, and read-only conditions are distinct. PostgreSQL `57014` consults cancellation/deadline state and maps an active request only when statement timeout is verified. MySQL 1205 and SQLite busy/locked map to contention rather than HTTP timeout; MySQL 1290 is no longer assumed read-only. Loose message matching was removed so domain/hook text such as “duplicate key validation” passes through unchanged.
- **BREAKING — `config.Load` and `config.LoadBytes` now return `*config.Config` instead of `credo.RawConfig`.** The concrete `*config.Config` still satisfies `RawConfig`, so passing the result to `credo.WithRawConfig` or storing it in a `RawConfig`/`credo.RawConfig` variable keeps compiling unchanged; only code that depends on the exact interface return type (for example assigning `config.Load` to a `func(...) (credo.RawConfig, error)` value) needs adjusting. The concrete return type is what carries the new `Get[T]`/`MustGet[T]` methods. See [ADR-005](docs/adr/005-configuration-architecture.md).
- **Lifecycle** — a failed startup (an `OnStart` hook returning an error) or a non-graceful `Serve` failure after the server reached `running` now runs the full teardown chain (readiness withdrawal → `OnPreDrain` → lifecycle cancellation → HTTP/`OnDrain` → DI container shutdown → `OnShutdown`) and ends in the terminal `stopped` state, instead of rolling back to `building`. This attempts to release resources an earlier `OnStart` hook started (workers, locks, connections) instead of leaking them. `OnShutdown` hooks consequently run on every teardown, including a failed startup, so they must be idempotent and must not assume any particular `OnStart` hook completed. Pre-session failures (TLS preflight, listener bind) still roll back to `building` and remain retryable. See [ADR-006](docs/adr/006-application-lifecycle.md).
- **Access logging** — the built-in access logger and `middleware.AccessLog` now share a single emit core (`internal/observe.EmitAccessLog`), keeping their attribute set, `"request completed"` message, and status→level mapping identical. No behavior change for existing callers.
- **TLS** — `Run` and `RunContext` now serve HTTPS when TLS is configured (previously plaintext-only); see **Added** and **Removed**.
- **BREAKING — typed Select terminals now require a model-less query.** `One[T]`, `All[T]`, and `Page[T]` no longer silently replace a model bound through `Select`, `Model`, or `Apply`; they return `sqldb.ErrTypedTerminalModel` before executing. Use `Model(&dest).Relation(...).Scan(ctx)` for bound models and relation loading, and use explicit-destination `Scan` for projections.
- **Data access (`store/sqldb`)** — `DB.Select`, `Insert`, `Update`, and `Delete` now accept at most one optional model argument. Supplying more causes the builder to record `sqldb: <Op> accepts at most one model, got N`; the terminal returns that error without executing instead of silently ignoring extra arguments.
- **BREAKING — `store.TxScope` is now `store.TxScope[T]`.** Construct it with `store.NewTxScope[T]()`; its methods no longer take their own type argument (`scope.WithTx[T](ctx, tx)` becomes `scope.WithTx(ctx, tx)`). The matching `*InScope` free functions now accept `*TxScope[T]`. The old unscoped `store.WithTx` / `GetTx` / `Conn` remain temporarily available but are deprecated because they cannot prevent cross-call concrete/interface mismatch or isolate same-type logical connections.
- **Data access (`store/sqldb`)** — transaction callbacks now have explicit failure semantics. After a successful rollback the exact callback error is returned without SQL mapping; callback and rollback failures retain both causes while only the rollback side is normalized. Nil callbacks return `ErrNilTxCallback` before BEGIN. Nested transactions accept nil/zero options but return `ErrNestedTxOptions` for non-default isolation/read-only options that Bun savepoints cannot apply. Savepoint creation is cancellation-aware; release/rollback remains usable after callback-context cancellation; a nil result after cancellation rolls back. Uncertain nested operations poison the ambient transaction before its bounded abort, preventing scheduler-dependent outer commits. Commit errors remain mapped but are documented as outcome-ambiguous, not unconditional retry signals.

### Removed

- **BREAKING — `(*store.Registry).Add` is removed.** Registry is now a
  read-only health view; only `store.Register` can create entries. This prevents
  health-only entries from bypassing startup Ping, DI publication, and shutdown
  ownership. Migrate direct `Registry.Add(name, lc)` calls to a typed
  `store.Register[R](app, value, ...)` registration.
- **BREAKING — `App.RunTLS` and `App.RunTLSContext` are removed.** TLS is now server configuration rather than a serve-method variant. Migrate by configuring TLS at construction and calling the plain entry points: `app.RunTLS(cert, key)` → `credo.New(credo.WithTLSFiles(cert, key))` then `app.Run()`; `app.RunTLSContext(ctx, cert, key)` → `WithTLSFiles` then `app.RunContext(ctx)`. For full `crypto/tls` control use `WithTLSConfig`. See [ADR-006](docs/adr/006-application-lifecycle.md).
- **BREAKING — `auth.SetUser` / `auth.GetUser` / `auth.RequireUser` are removed.** The authenticated principal is now reached through generic `*credo.Context` methods instead of `context.Context` helpers: `ctx.SetUser(user)` (T inferred), `ctx.GetUser[T]()`, and `ctx.RequireUser[T]()` (returns `credo.ErrUnauthorized` wrapping the new `credo.ErrUserMissing`). `auth.Middleware` is unchanged at the call site — it now stores the user via `ctx.SetUser`. Migrate handler reads: `auth.GetUser[T](ctx.Context())` → `ctx.GetUser[T]()`. See [ADR-012](docs/adr/012-authentication-and-authorization.md).
- **BREAKING — `sqldb.Paginate` and `sqldb.PaginateRequest` are removed.** The typed `(*SelectQuery).Page[T]` terminal replaces `PaginateRequest` one-for-one: `sqldb.PaginateRequest[User](ctx, q, req)` → `q.Page[User](ctx, req)`. For a fallible model→DTO conversion, fetch a model-less `Page[Model]`, map `modelPage.Records` with ordinary error handling, and construct `pagination.NewPage(dtos, modelPage.Total, modelPage.Page, modelPage.PerPage)`. The ORM-agnostic pagination data shapes remain; the `PageRequest.Offset` signature change is described above. See [ADR-015](docs/adr/015-data-access.md).

### Fixed

- **Pagination** — `pagination.NewPage` now calculates ceiling division as quotient plus remainder, avoiding the `total + perPage - 1` overflow that could produce an invalid `TotalPages` near `math.MaxInt64`.
- **Health/readiness** — named and store checks now share one bounded parallel runner. Per-check deadlines are enforced even when callbacks ignore cancellation; overlapping requests join a stable in-flight probe so a hung dependency cannot accumulate one goroutine per Kubernetes poll. Store checks are independently timeout- and panic-isolated, typed `store.Health.Cause` values are logged and masked by default, arbitrary adapter statuses fail closed, and a custom/store name collision produces an explicit 503 instead of silently overwriting a response entry. All stores remain critical; `DEGRADED` is explicitly readiness-blocking. Optional/critical policy and bounded low-cardinality tags remain separate follow-up decisions. See [ADR-016](docs/adr/016-health-checks.md).
- **Data access (`store/sqldb`)** — Select terminal snapshots and public `SelectQuery.Clone` now preserve the execution fields Bun v1.2.18 omits: explicit connection, builder error, `WherePK`, soft-delete flags, and CTE materialization flags. Credo retains Bun's clone behavior for the rest. Public `Clone` is a top-level builder fork, not a recursive object-graph copy: a bound destination and nested CTE/relation query values may remain shared, so source and clone must not mutate or scan shared values concurrently.
- **Routing** — `App.Mount` is now atomic: a mount that conflicts with an existing explicit route registers nothing. `Mount` makes fourteen radix registrations (every forwarded method on both the exact prefix and the catch-all), and the radix tree has no delete, so a conflict discovered partway through previously stranded the earlier registrations as orphan routes — reachable by dispatch yet hidden from introspection. `Mount` now preflights every method/pattern pair and panics before mutating the tree if any explicit route already occupies one, leaving the router exactly as it was. See [ADR-007](docs/adr/007-router-and-routing.md).
- **Docs** — `WithLogger`'s godoc no longer claims a "nop logger" is used when it is left unset; the framework default logger (a text handler on stderr) is, so access and request logging are on by default with no configuration.

## [0.1.0] - 2026-06-10 — development baseline (untagged)

Initial public development baseline.

### Added

- **Routing** — radix-tree router (adapted from Chi) with `{param}`, regex constraints, and `{path...}` catch-all; named routes with `BuildURI`/`BuildURL`; route Meta with parent-chain lookup; host routing; app-level 404/405/5xx status handlers; trailing-slash redirect; automatic HEAD handling.
- **Context** — pooled `credo.Context` with a `Request`/`Response` split, one-step bind-and-validate (`BindBody`/`BindQuery`), internal rewrites (`Rewrite`/`OriginalPath`), and `Context()` for context-taking APIs.
- **Error handling** — `func(*credo.Context) error` handler signature, RFC 7807 Problem Details responses, pluggable `ErrorRenderer`, built-in panic recovery.
- **Middleware** — four-tier execution (built-in → global → group → route); built-ins on by default: Recover, RequestID, AccessLog (opt-out via `Without*`). Suite: CORS, CSRF (stdlib `CrossOriginProtection`), Secure, Compress, Timeout, RateLimit, Rewrite, ContractGuard.
- **Static files** — `os.Root`-sandboxed `app.Static`/`app.File`.
- **Dependency injection** — generics-based container (`Provide[T]`/`Resolve[T]`/`ProvideFactory`/`Alias`/`Replace`), validated graph freeze (`Finalize`), reverse-order shutdown, and the `credo.Infra` carrier (per-service logger; tracer/metrics with noop fallbacks).
- **Configuration** — koanf-adapted loader (`config.Load`), env-specific file derivation, `.env` support, typed config snapshots via `RawConfig.Unmarshal`.
- **Validation** — programmatic generic rules (`Rule[T]`, ozzo-style field refs), file-upload rules, RFC 7807 error output; no struct tags.
- **Authentication** — generic `auth.SetUser[T]`/`GetUser[T]`, JWT / Basic / API-key authenticators, middleware factory, RBAC-via-route-Meta pattern.
- **Internationalization** — JSON locale files, `ctx.T()`/`ctx.Locale()`, key-based error translation with three-level fallback.
- **Data access** — `store` contracts (universal errors, context-based transaction scope) and the `store/sqldb` Bun wrapper submodule: curated query-builder proxies, `db.InTx`/`RunInTx`, and migrations via a thin `bun/migrate` wrapper (`app.OnStart(db.Migrate)`, mark-applied-on-success default).
- **Background workers** — continuous and cron-scheduled workers (`worker.Register`, parser adapted from robfig/cron v3), panic recovery, graceful shutdown.
- **Health checks** — `/health` + `/ready` (Kubernetes-compatible), store-registry auto-integration, readiness errors masked by default.
- **Outbound HTTP** — `httpclient` package: `*http.Client` factory with a fixed RoundTripper chain (timeout → retry → logging → trace), safe-by-default retry, per-attempt structured logging, manual W3C trace context propagation.
- **Pagination** — query-param parsing and normalization, with `store`-integrated `Paginate` in `store/sqldb`.
- **Observability baseline** — structured logging (slog) wired by default with request IDs and access logs; OpenTelemetry tracing and Prometheus metrics are experimental with noop fallbacks.
- **testutil** — hermetic `testutil.NewApp` with DI overrides (`WithOverride`/`WithConfig`/`WithWiring`) and `LogBuffer` log assertions.

Adapted open-source code is attributed in [NOTICES](NOTICES); the per-component acquisition strategy is documented in [docs/adr/002-code-acquisition-strategy.md](docs/adr/002-code-acquisition-strategy.md).

[Unreleased]: https://github.com/credo-go/credo/compare/v0.11.0...HEAD
[0.11.0]: https://github.com/credo-go/credo/compare/v0.10.0...v0.11.0
[0.10.0]: https://github.com/credo-go/credo/compare/v0.9.0...v0.10.0
[0.9.0]: https://github.com/credo-go/credo/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/credo-go/credo/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/credo-go/credo/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/credo-go/credo/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/credo-go/credo/compare/v0.4.1...v0.5.0
[0.4.1]: https://github.com/credo-go/credo/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/credo-go/credo/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/credo-go/credo/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/credo-go/credo/compare/cdb0643f6b6b006d7c5d2d81c916b3942874e6c6...v0.2.0
[0.1.0]: https://github.com/credo-go/credo/commit/cdb0643f6b6b006d7c5d2d81c916b3942874e6c6
