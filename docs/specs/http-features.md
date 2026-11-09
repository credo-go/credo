# Built-in HTTP Features

**Status:** Implemented (beta) **Package:** root package — `executor.go`, `features.go`, `recover.go`, `requestid.go`, `accesslog.go`, `compress.go`, `decompress.go`, `i18n.go` **ADR:** [ADR-010](../adr/010-middleware-architecture.md) **Guide:** [Middleware guide](../guides/middleware.md#framework-features-not-middleware) **Sources:** Chi (MIT), Echo (MIT)

> **Pending v0.26.0 (R14):** this spec is not yet split into its normative contract and current behaviour; until R14 ships, every section describes current behaviour.

Panic recovery, request IDs, access logging, response compression, request-body decompression, locale detection and the custom renderers are framework-owned HTTP features of the root package. They are not middleware: one internal request executor runs them in a fixed order around the user's PreRouting → routed → Group → Route chain ([middleware spec](middleware.md#tiers-prerouting-routed-group-route)), so they observe the final response — error envelopes and panic responses included — and cannot be scoped to a group or route. Timeout, CORS, CSRF, Secure, RateLimit, Rewrite and ContractGuard are user middleware, covered with the user chain by the [middleware spec](middleware.md); server body limits and trusted-proxy handling are core settings, not middleware. This document is the feature contract.

## Defaults and the sole public entry point

| Setting or feature | Default | Sole configuration/registration path |
| --- | --- | --- |
| Logger, raw config, server/TLS and timeout settings | Constructor defaults | Constructor options through `New(...Option)` |
| Recovery | On | `credo.WithRecoverConfig(cfg)` with `RecoverConfig{Logger, DisableStackTrace, StackSize}`; `WithoutRecover()` disables it regardless of option order |
| RequestID | Off | `app.UseRequestID(cfg...)` with `RequestIDConfig{Header, Generator, Limit}` |
| AccessLog | Off | `app.UseAccessLog(cfg...)` with `AccessLogConfig{Logger, MinLevel, Skipper, ResultFilter}` |
| Response compression | Off | `app.UseCompress(cfg...)` with `CompressConfig{Skipper, Level, Types}` |
| Request decompression | Off | `app.UseDecompress(cfg...)` with `DecompressConfig{Skipper, MaxBytes}` |
| i18n | Off | `app.UseI18n(cfg...)`; first-use detection through `I18nConfig.Detect func(*Context) string` |
| Custom error renderer | Default error rendering | `app.UseErrorRenderer(renderer)` |
| Custom success renderer | Plain JSON through `Context.Render` | `app.UseSuccessRenderer(renderer)` |

> **Pending v0.26.0 (R12):** until R12 ships, `AccessLogConfig` has no `Attrs` and no observer can be added; from R12 the table's AccessLog row lists `Attrs` beside `ResultFilter`, and `app.AddObserver(o)` adds an observer ([observers](#observers-of-the-final-response)).

- **Constructor settings.** Configuring recovery never re-enables it, and there is no separate activation helper.
- **Optional features.** Calling `Use*` installs the feature and leaving it out leaves the feature off; the configs carry no `Enabled` switch, so application bootstrap evaluates external enable flags before making the call ([rationale](../adr/010-middleware-architecture.md#built-in-http-feature-configuration-criterion)).
- **Renderers.** Renderers may be methods of objects resolved from DI after `Finalize`, so they are post-construction registrations. Omission selects the default. The renderer callbacks keep their nil-result/body-shaping semantics, and a success renderer is used only by `Context.Render`.

## Registration contract

- A config-based `Use*` method accepts zero configs for the defaults or one config for customization; more than one config panics (`credo: App.UseX accepts at most one config`). Renderer registrations take their renderer directly and panic for nil.
- Each feature and each renderer permits one successful registration; a second call panics (`credo: App.UseX called twice`). A later call never silently overwrites or merges configuration. A `UseI18n` whose conventional discovery finds no catalogs starts configured-but-inactive and still consumes the registration; when its catalogs are read and how their failures surface is in the [i18n spec](i18n.md#registration-and-start-phase).
- Configuration is validated and normalized off to the side, then published once: a rejected call (bad `Level`, negative `MaxBytes`, typed-nil `MinLevel`, a credential `RequestIDConfig.Header` (`Authorization`, `Proxy-Authorization` or `Cookie`), nil renderer, too many configs) leaves nothing installed. Caller-owned slices are copied at registration. Retained callbacks and logger/`Leveler` values keep their documented concurrency contracts. This is setup-time registration, not runtime reconfiguration.
- A `Use*` call after HTTP preparation or shutdown admission panics (`credo: App.UseX called after app was compiled or shut down`); DI `Finalize` alone leaves the HTTP registration window open, which is what allows DI-backed renderers and controllers ([registration guards](lifecycle.md#registration-guards)).
- `Use*` call order does not determine request execution order. The App stores each feature's normalized configuration and the executor applies them in one framework-defined plan; nothing is appended in call order. User middleware keeps its PreRouting/routed/Group/Route ordering rules.

## Request execution

`App.ServeHTTP` first applies the lifecycle admission gate: a request arriving after the App stopped, or while it is stopping without a prepared handler, takes the callback-free 503 branch and never reaches the executor. Admitted requests run through `App.execute`, which owns the following order:

1. **RequestID** (when installed): validate the incoming header value against the limit and the allowed character set, or generate one; store it on the Context, echo it on the response header and enrich the request-scoped logger lazily on first `Context.Logger` use.
2. **AccessLog selection** (when installed): the pre-dispatch `Skipper` decides whether the request is observed; the duration clock starts at executor entry.
3. **Decompress** (when installed and its `Skipper` returned false): present the decoded body to the chain, or answer 415 or 400 through central error rendering without running any user middleware ([rules](#locale-and-transport-features)).
4. **Compress** (when installed, its `Skipper` returned false and `Accept-Encoding` negotiates gzip or deflate): install the pooled compressing writer around the response writer. It stays installed through centralized error rendering, so error envelopes are compressed like handler output ([rules](#locale-and-transport-features)).

5. **User chain:** `PreRouting` middleware → dispatch → routed → Group → Route → handler; a mount's handler runs inside the routed tier. A returned error goes through centralized error classification and rendering (custom renderer, i18n translation); a custom status handler answers only the router's own 404 and 405.
6. **Recovery** (unless `WithoutRecover`): a panic anywhere in steps 1–5 is logged through `RecoverConfig` (request-scoped logger by default, optional dedicated logger, stack capture on by default, bounded by `StackSize`) and rendered as a generic 500 unless the response was already committed or hijacked. `http.ErrAbortHandler` always propagates. With recovery disabled the panic propagates after the cleanup below. Central error handling is the exception: it guards its own panics — the error renderer's included — with or without recovery, and a `ResultFilter` panic in step 8 has its own rule ([callback failures](#callback-and-finalization-failures)).
7. **Finalization:** close the compressor. A `Close` failure is a framework `Error` diagnostic; if no output was committed the request falls back to the central 500 with compression disabled, and if output had started the incomplete stream is aborted with `http.ErrAbortHandler` — never by appending a JSON body to a partial compressed stream. The pooled writer is released.

> **Current until v0.26.0 (R12):** step 8; from R12 this step runs the observers in registration order, the access log among them, on the `*Exchange` that replaces `AccessLogEntry` ([observers](#observers-of-the-final-response)).

8. **AccessLog observation** (when selected): `MetaAccessLog` silence → final status (an escaped panic with recovery disabled uses the 500 fallback) → `MinLevel` admission → `AccessLogEntry` snapshot → `ResultFilter` → emission through the selected logger (`internal/observe.EmitAccessLog`), where the logger's own level filtering still applies.

> **Current until v0.26.0 (R10):** step 9's release to the pool; from R10 the Context is not pooled.

9. **Cleanup:** the Context is released to the pool in a defer, so it runs on normal completion, fallback, escaping panic and abort alike.

Central error classification and rendering exist even when recovery and every optional feature are off. Preparation, Finalize and compilation failures are outside request recovery and follow the [stored preparation contract](lifecycle.md#preparation).

## Observability contract

- `Context.RequestID()` is the canonical accessor and returns `""` when no ID was installed. Without `UseRequestID` no ID is generated or echoed. RequestID and AccessLog are independent: access records carry an empty `request_id` when only access logging is installed, and enabling one never enables the other.
- **request_id sourcing rule** (shared by access logging and recovery): the `request_id` attribute is added explicitly only when the target logger does not already carry it — when a dedicated `Logger` was configured, or when no request-scoped logger was set (`Context.HasRequestLogger`). The rule is convention-based: `HasRequestLogger` reports that a request-scoped logger was set, not which attributes it carries, so middleware that replaces the logger without deriving from `ctx.Logger()` silently drops the ID. Enrich with `ctx.AddLogAttrs`; reserve `ctx.SetLogger` for wholesale replacement.

> **Current until v0.26.0 (R12):** in the next two items, `ResultFilter`'s pooled Context and every reference to `AccessLogEntry` and its fields; from R12 the filter takes the `*Exchange`, which carries the same fields ([observers](#observers-of-the-final-response)).

- **Access-log record.** Each admitted request yields one record with the message `request completed`, emitted by `internal/observe.EmitAccessLog` at the status-derived level. Status maps to the record level: `1xx/2xx/3xx → Info`, `4xx → Warn`, `5xx+ → Error`. `MinLevel` (`slog.Leveler`; nil means Info; a typed-nil provider panics at registration; a concurrency-safe `slog.LevelVar` supports runtime changes and is read once per eligible request) is an admission threshold, never a level rewrite. `Skipper` (`true = skip`) runs before dispatch and sees request data only. `MetaAccessLog: false` on the matched route, or inherited from a group, silences the record; a route-level value overrides a group-level one, and the key is a `credo.Key[bool]`, so a value of another type does not compile. `ResultFilter` (`true = emit`) runs after the final response with a pooled Context that must not be retained; it cannot restore an entry rejected by `MinLevel`. A dedicated `Logger` receives the standard fields and an explicit request ID but does not inherit arbitrary `ctx.AddLogAttrs` enrichment. `AccessLogEntry.RequestID` always holds the request's ID, whether or not the target logger already carries `request_id`; the duplicate check applies only to the emitted attribute.
- **Measurements.** `AccessLogEntry.Bytes` counts the body bytes the underlying transport writer accepted after compression, compressor trailers and partial writes included; headers, framing and TLS overhead are excluded and the count is not proof of client receipt. `Response.Size` keeps its own meaning (bytes handed to the response). `Duration` runs from executor entry through response and compressor finalization and excludes `ResultFilter`, log emission and Context release. `Route` is the matched route's registered pattern (`/v1/jobs/{job_id}`), emitted as `route` whenever a route matched; `path_original` is emitted when the served path differs from the client path; `RouteName` is filter metadata only. `RemoteAddr` comes from `Request.RealIP`.

- **A mount is a route.** A request a `Mount` matches carries a `*Route` of kind mount, so its record's `route` is the mount's prefix pattern and App-level meta such as `MetaAccessLog` applies to it.
- **AccessLog off does not silence framework logging.** Recovery, server errors, lifecycle, health and other framework diagnostics continue through their normal loggers and levels, as do application and `Infra` logs. `UseAccessLog` and `MinLevel` govern access records only. `WithLogger` does not activate access logging. `WithDebug` enables development diagnostics and does not change the logger's minimum level; some debug-mode diagnostics are Warn records.
- Health probes registered by `UseHealth` set `MetaAccessLog` to `HealthConfig.LogRequests` (default false), so probe traffic is silent even when access logging is installed.

## Observers of the final response

> **Pending v0.26.0 (R12):** not implemented yet; the access log is the executor's own step over an `AccessLogEntry`, its `ResultFilter` takes `(*Context, AccessLogEntry)`, and no other observer can be added.

- **Registration.** An `Observer` receives the `*Exchange` in its `Observe`. `app.AddObserver(o)` adds one; it panics on a nil observer and after preparation, as the other registrations do. Observers run at step 8 of [request execution](#request-execution), after finalization, in registration order. The access log is one of them, installed by `UseAccessLog`.
- **The record.** `*Exchange` is one immutable record of the final response: the method, the path and the original path, the route pattern and name, the remote address and the user agent, the final status, the bytes after compression, the duration, the classified error code, whether a panic was recovered, the request ID, and the request's values. It is valid only during `Observe`; an observer copies what it keeps, apart from the request's context.
- **The request's values.** `Exchange.Context()` returns the request's context head as it stands when the response is final: the values set through `c.Set`, read with `Value` and their keys, and a span that a wrap enclosing the observers put there, from which the OpenTelemetry SDK takes exemplars when a metric records through that context. The head is made of immutable nodes, so an observer may keep it beyond `Observe`; it is cancelled when the request ends. `Exchange.Get(key)` reads a typed key's value from the head, `(T, bool)` as `c.Get` returns it — the zero value and false for a key no layer set — and `Exchange.GetUser[T]` reads the user, whose key stays unexported ([ADR-014](../adr/014-observability.md#observers-read-the-requests-values)). A user or value set inside a scope, `middleware.Timeout` or a wrapped stdlib middleware included, is visible.
- **The classified error code** is the code the pipeline classified, captured before the renderer runs; for a recovered panic, the code of the 500 it rendered; after a fallback, the fallback's code; and empty for an error returned after the response was committed or hijacked, for a handler that wrote its own error response, and for an escaped panic with recovery disabled. It is never inferred from the status.
- **A panicking observer.** With recovery enabled, its panic is logged as a framework Error diagnostic and that observer is skipped for the exchange, while the observers after it still run; the response is already final. `http.ErrAbortHandler`, and every panic under `WithoutRecover`, propagate.
- **The access log as an observer.** `MinLevel`, `ResultFilter` and `Attrs` are options of the access-log observer, and its records stay byte for byte as above. `ResultFilter` is `func(ex *Exchange) bool` and reads from the `Exchange` each field `AccessLogEntry` carried, and the request's values through `ex.Get`, `ex.GetUser` and `ex.Context()`. `Attrs` receives the `*Exchange` and returns application attributes for a record that is a candidate for emission — after `MetaAccessLog`, `MinLevel` and `ResultFilter`, and only when the logger is enabled for the record's level — so it is not called for a skipped, silenced, filtered or disabled record. The attributes are emitted inside one `attrs` group, on both logger paths, so an attribute never shadows a framework attribute, present or future; `attrs` is a reserved key on the access record. A panic in `Attrs` is logged and the record is emitted without the group. The returned slice passes to the framework and must not be reused.

## Startup visibility

The managed start line, `credo: server started` ([lifecycle spec](lifecycle.md#startup-record)), carries a `features` attribute listing the built-in features in effect:

```json
{"level":"INFO","msg":"credo: server started","label":"Run","addr":"127.0.0.1:8080","features":["recover","request_id","access_log","compress"]}
```

| Name | Listed when |
| --- | --- |
| `recover` | recovery is on (not `WithoutRecover`) |
| `request_id` | `UseRequestID` installed it |
| `access_log` | `UseAccessLog` installed it |
| `decompress` | `UseDecompress` installed it |
| `compress` | `UseCompress` installed it |
| `i18n` | `UseI18n` installed an active bundle |
| `error_renderer` | `UseErrorRenderer` installed a custom renderer |
| `success_renderer` | `UseSuccessRenderer` installed a custom renderer |
| `health` | `UseHealth` registered at least one probe route (liveness or readiness) |

- **Order.** Names appear in the table's order, whatever the registration order. The order is for display; it is not the execution order of the request executor.
- **Effective state, not registration calls.** The list is read from the App after the start walk. A `UseI18n` whose conventional discovery found no catalogs consumes its registration but is not listed. Likewise, `UseHealth` with both `Liveness` and `Readiness` disabled registers no probe route and is not listed. A missing renderer name means the default rendering is in use, not that error rendering or `Context.Render` is off. `health` means at least one probe is mounted, not that the application is healthy.
- **Empty list.** A default App reports `["recover"]`. With every feature off, the attribute is an empty array — `[]` in JSON, never `null`.
- **No added diagnostics.** The feature summary adds no Warn or other record for features that are off. Installation diagnostics stay separate records, such as the start phase's Warn for a `UseI18n` whose discovery found no catalogs.
- **Scope.** Only managed serving writes the line (`Run`, `RunContext`, `ServeContext`). An application that only calls `ServeHTTP` from its own server gets no start line, and no line is added on the first request.
- **Installation, not delivery.** An installed access log can still drop records through `MinLevel`, `MetaAccessLog`, `Skipper` or `ResultFilter`, and health probes are silent by default. To verify an application's observability, send a request to an ordinary route and check that the response carries a request ID and that an access record with the same ID is written.

## Locale and transport features

- **i18n first-use detection.** `I18nConfig.Detect` runs on the request's first locale or translation use and is memoized for the request; the [detector contract](i18n.md#detector-contract) defines what it sees, its panic and re-entry handling and the inactive-bundle behavior.
- **Compress** keeps its writer valid through centralized error rendering and finalizes it at the response-completion boundary before the access record is observed. Supported codings are gzip and deflate, selected by `Accept-Encoding` quality values; `Level` is 1–9 (zero selects 5; other values panic at registration); `Types` accepts exact media types and `type/*` wildcards and defaults to the common textual types. Content-type selection and the `Content-Encoding`/`Vary` decision happen at first write. Responses that already carry a `Content-Encoding` are passed through. HEAD and bodiless responses, committed responses and hijacked connections keep their behavior; a `Flush` streams the compressed output, and a flush before the first write makes the compression decision then, so the flushed header carries `Content-Encoding` — an informational status other than 101 passes through and leaves the decision to the final status, and a body-forbidding final status (101, 204, 304) never starts a compressor, whatever its `Content-Type`, because even an empty compressed stream is a body net/http rejects. The selection is made once on the original request; a rewrite does not re-evaluate it.

> **Pending v0.26.0 (R2):** until R2 ships, an unsupported coding's 415 names the coding in its message key, and the decoded-size limit's 413 carries a literal key of its own.

- **Client values in `Details`.** An unsupported content encoding answers 415 `unsupported_content_encoding` with the built-in message and the coding in `Details`; the decoded-size limit answers the 413's built-in message. Neither carries a message key of its own ([ADR-009](../adr/009-handler-and-error-handling.md#one-error-channel-keys-messages-and-fault-codes)).
- **Decompress** runs before `PreRouting` middleware, so body-reading middleware and binders consume the same decoded stream; the body is processed as a stream, never buffered whole. An `identity` coding passes through and an empty body drops the header. Supported codings are `gzip`/`x-gzip` and `deflate` (zlib-wrapped per RFC 9110, raw DEFLATE tolerated); multiple gzip members are not concatenated. A supported coding replaces the body with the decoded stream, sets `ContentLength` to -1 and removes `Content-Encoding`; an unsupported coding or coding list answers 415 `unsupported_content_encoding` and a corrupt stream header answers 400 `bind_failed`. The server-wide `max_body_bytes` bounds the compressed wire bytes and `MaxBytes` (default `DefaultDecompressMaxBytes`, 4 MiB) bounds the decoded bytes; reading past it surfaces as the framework's regular 413. The transport `Skipper` is evaluated once on the original request's method, path and headers and is the way to keep raw bodies for routes such as signed webhooks; route and auth data are unavailable at selection and a rewrite does not reselect. Without `UseDecompress`, `BindBody` rejects a non-identity `Content-Encoding` with 415.

## Callback and finalization failures

Recovery configuration applies to the request-execution callbacks — feature selectors, locale detection, the success renderer and access filters — and with recovery disabled their panics propagate after mandatory cleanup. Central error handling guards itself instead: a panic while an error is classified, translated or rendered — in the error renderer, the message-key resolver, or a locale detection that error translation triggers — is logged (`credo: error pipeline panic`) and answered with the callback-free generic 500 unless the response was already committed or hijacked, whether or not recovery is enabled. Elsewhere, `http.ErrAbortHandler` always propagates as the transport-abort sentinel. Returned runtime errors use central error handling regardless of recovery.

| Failure with recovery enabled | Behavior |
| --- | --- |
| Feature/handler panic before commitment | Classified as 500 through request recovery; the failed callback is never retried |
| Error renderer or error-rendering extension fails before commitment | The callback-free framework default 500 body/encoder is used, honoring HEAD/body rules; the failing extension is not invoked recursively |
| Locale detector panics or re-enters | Default locale cached and the request takes the 500 path ([detector contract](i18n.md#detector-contract)) |
| `ResultFilter` panics after finalization | Completed response kept, framework `Error` diagnostic (`credo: access-log result filter panicked`) emitted, this access record skipped; the filter is never retried |
| Failure after commitment/hijack | Committed status/body preserved with the existing hijack/abort guards; no second error envelope |

Compression finalization errors are observed, not discarded: they carry their own framework `Error` diagnostic independently of access-log selection, the actual committed status and accepted byte count are preserved in the access record (a transmitted 200 never becomes a fictional 500), and started output is aborted rather than patched. The callback-free fallback is bounded and invokes no custom renderer, locale/message-key resolver or application encoder callback.
