# Context Spec

**Status:** Implemented (beta) **Package:** root (`github.com/credo-go/credo`) **ADR:** [ADR-008](../adr/008-context-design.md), [ADR-009](../adr/009-handler-and-error-handling.md), [ADR-011](../adr/011-validation-strategy.md), [ADR-012](../adr/012-authentication-and-authorization.md), [ADR-013](../adr/013-internationalization.md), [ADR-018](../adr/018-host-routing-and-rewrite.md), [ADR-021](../adr/021-json-output-profile.md) **Guide:** [Getting started](../guides/getting-started.md#request-and-response), [Error handling](../guides/error-handling.md), [Routing](../guides/routing.md), [Proxy trust](../guides/proxy-trust.md) **Sources:** Echo (MIT)

## Overview

`credo.Context` is Credo's HTTP request context: a request-scoped **struct** (not interface) that holds the `*Request`, `*Response`, matched `*Route`, a logger, and a key-value store. It exposes the underlying `http.Request`'s `context.Context` via `Context()`; it deliberately does **not** itself implement `context.Context`, because the struct is pooled and reused across requests.

Lifecycle hooks such as `app.OnStart` and `app.OnStop`, and a component's `Start` and `Shutdown`, receive a standard `context.Context`, not `*credo.Context`. That context lives as long as its call and carries the cancellation and the deadline of the start phase or the drain; `credo.Context` exists only for an HTTP request.

In addition to request/response accessors, Context tracks rewrite lifecycle: the original client path, internal re-dispatch requests (`ctx.Rewrite()`), and whether the current dispatch round is about to rewrite.

Input and output concerns are separated into `Request` and `Response` structs:

- **Request** — wraps `*http.Request`, provides route params, query shortcuts, and `Bind*` methods ("parse, don't validate").
- **Response** — wraps `http.ResponseWriter`, provides `JSON`, `Text`, `HTML`, `XML`, `NoContent`, `Redirect`, `Blob`, `Stream` helpers.

Defined in the **root package** (not a sub-package) to avoid collision with stdlib `context`. The framework-owned HTTP features that run around the handler chain are specified in the [HTTP features spec](http-features.md), and locale detection in the [i18n spec](i18n.md#detector-contract).

## API Surface

### Context Struct

```go
type Context struct {
    // unexported fields — access via methods only (pool safety)
}

func NewContext(w http.ResponseWriter, r *http.Request) *Context // for testing handlers, renderers and middleware

// Accessors
func (c *Context) Request() *Request
func (c *Context) Response() *Response
func (c *Context) Route() *Route            // nil when no route matched (404/405)
func (c *Context) HasRoute() bool           // true when a route matched; guard for Route() calls
func (c *Context) Logger() *slog.Logger    // c.logger → app.logger → framework default (stderr)
func (c *Context) SetLogger(*slog.Logger)  // replace logger wholesale; derive from Logger() or enrichment (request_id) is silently lost
func (c *Context) AddLogAttrs(args ...any) // add attrs, deriving from Logger() — preferred over SetLogger for enrichment
func (c *Context) HasRequestLogger() bool  // true once a request-scoped logger was set; does not inspect its attributes
func (c *Context) RequestID() string       // request ID installed by App.UseRequestID; "" without it
func (c *Context) OriginalPath() string    // client path before any rewriting
func (c *Context) Rewrite(path string) error
func (c *Context) IsRewriting() bool

// Locale and translation — resolved on first use and memoized for the request
func (c *Context) Locale() string // "" without UseI18n
func (c *Context) T(key string, data ...map[string]any) string
func (c *Context) TPlural(key string, count any, data ...map[string]any) string

// Authenticated user — one slot per type T
func (c *Context) SetUser[T any](user T)
func (c *Context) GetUser[T any]() (T, bool)
func (c *Context) RequireUser[T any]() (T, error) // 401 wrapping ErrUserMissing when absent

// Request-scoped store
func (c *Context) Set(key string, val any)
func (c *Context) Get(key string) any

// Request context access — the request's context.Context
func (c *Context) Context() context.Context
```

`Locale`, `T` and `TPlural` follow the [i18n spec](i18n.md#detector-contract); the user accessors and `credo.ErrUserMissing` follow the [auth spec](auth.md#user-accessors-generic-context-methods).

### Request Struct

```go
type Request struct {
    *http.Request  // embedded — Cookie, Header, Method, URL all available
}

func NewRequest(r *http.Request) *Request
func (r *Request) RouteParam(name string) string  // single path/host param; "" when absent (preferred)
func (r *Request) RouteParams() map[string]string // all params; framework-owned map, do not retain
func (r *Request) PathValue(name string) string   // stdlib-shaped shadow → RouteParam, falls back to embedded request
func (r *Request) QueryParam(name string) string
func (r *Request) Scheme() string                 // "http" or "https"
func (r *Request) RealIP() string                 // original client IP with trusted proxy support

// Bind — "parse, don't validate" (decode + validate in one step)
func (r *Request) BindBody(target any) error
func (r *Request) BindQuery(target any) error
```

### Response Struct

```go
type Response struct {
    http.ResponseWriter // swappable by the compression feature or a wrapped stdlib middleware
    // status, size, committed, hijacked — framework-owned tracking state (unexported)
}

func NewResponse(w http.ResponseWriter) *Response

// State accessors (read-only — the framework owns the tracking state)
func (r *Response) Status() int     // status code written; 0 until committed
func (r *Response) Size() int64     // bytes written to the response body
func (r *Response) Committed() bool // true once the final status is written
func (r *Response) Hijacked() bool  // true once the connection was taken over

// http.ResponseWriter and optional interfaces
func (r *Response) WriteHeader(code int)
func (r *Response) Write(b []byte) (int, error)
func (r *Response) WriteString(s string) (int, error)     // io.StringWriter
func (r *Response) ReadFrom(src io.Reader) (int64, error) // io.ReaderFrom; see below
func (r *Response) Flush()            // http.Flusher; see FlushError
func (r *Response) FlushError() error // preferred by http.ResponseController
func (r *Response) Hijack() (net.Conn, *bufio.ReadWriter, error)
func (r *Response) Unwrap() http.ResponseWriter
func (r *Response) Reset(w http.ResponseWriter)
func (r *Response) String() string  // fmt.Stringer for debugging

// Response helpers
func (r *Response) JSON(code int, v any) error
func (r *Response) Text(code int, s string) error
func (r *Response) HTML(code int, html string) error
func (r *Response) XML(code int, v any) error
func (r *Response) NoContent(code int) error
func (r *Response) Redirect(code int, url string) error
func (r *Response) Blob(code int, contentType string, b []byte) error
func (r *Response) Stream(code int, contentType string, rd io.Reader) error
func (r *Response) SetCookie(cookie *http.Cookie)
```

`WriteHeader` writes the final status once; later calls are ignored. An informational status other than 101 — `WriteHeader(103)` for Early Hints — is sent at once with the current headers and commits nothing: `Status()` stays 0, `Committed()` false, and the final status follows, as with net/http. Under `UseCompress` the compression decision waits for that final status too. A code outside 100–999 panics, as net/http's own check does, before `Response` records anything, so recovery still answers with a 500. A final 4xx or 5xx status rewrites an inherited `Cache-Control` value that carries the `immutable` directive (matched case-insensitively) to `no-cache, must-revalidate`, static or not, so an error answered under a cache policy set before its status was known is never cached for an immutable lifetime ([static spec](static.md#cache-control)).

A flush commits the response, as net/http's does: status 200 is written first when no status was, so `Status()` reports 200, `Committed()` true, and an error the handler returns afterwards is logged (`credo: error after response committed`) instead of being appended to the flushed response. `FlushError` reports `http.ErrNotSupported` and commits nothing when no writer in the chain can flush (`http.ErrHijacked` after a hijack); `Flush` discards the result. Under `UseCompress` a flush before the first write makes the compression decision then, from the headers set so far, so the flushed header carries `Content-Encoding`.

Every body-writing helper (`JSON`, `Text`, `HTML`, `XML`, `Blob`, `Stream`) treats body-forbidding status codes — 1xx, 204 No Content, 304 Not Modified (RFC 9110) — as status-only: the body **and** the Content-Type header are skipped, the call returns nil, and `Stream`'s reader is never read. Without this, writing a body to such a status fails inside net/http after the header is committed, and the returned error can only surface as a spurious "error after response committed" warning; a 204 also must not advertise a `Content-Type` it does not carry. Handler-set headers (ETag, Cache-Control on a 304) are untouched.

`Response` implements `io.ReaderFrom`, so `Stream` and any `io.Copy` into the response copy a Reader-only source without allocating a buffer per call. On an uncommitted response the first 32 KiB of the source always pass through `Response.Write` from a framework-pooled buffer, so the commit is recorded by `Response` before anything else can fail. When the underlying `http.ResponseWriter` implements `io.ReaderFrom` (net/http's HTTP/1.1 writer, which keeps its sendfile/splice path for `*os.File` and socket sources on a plaintext TCP connection) the rest of a larger source — or the whole source of an already committed response — is delegated to the writer's `ReadFrom`; otherwise — a compressing or other wrapping writer, HTTP/2 — the copy runs through the pooled buffer and every chunk passes through `Response.Write`. A source implementing `io.WriterTo` writes itself on the pooled path. The bytes the writer accepted count toward `Size()`, a partial-write error is returned with the accepted count, a hijacked response answers `http.ErrHijacked`, and the commit follows `Write`: nothing is committed before the first byte (a source that fails up front leaves the response uncommitted so the handler's error still renders), and the first byte commits status 200 when none was written. A source that panics after its first bytes leaves the response committed: recovery logs the panic, the access record observes the 200 already on the wire, and no error envelope is appended. The delegated source is wrapped in a Response-owned `io.LimitedReader` (no allocation; the type net's sendfile and splice paths unwrap, so the zero-copy path survives) whose remaining count records what the writer consumed, so `Size()` — and the access record's bytes — stay equal to what the client received even when the source panics during delegation.

#### Render and the envelope seam

```go
// Method on *Context — the single call site that consults App.UseSuccessRenderer
func (c *Context) Render(status int, data any, opts ...RenderOption) error

// Side channels for the renderer's envelope; silently dropped with no renderer
func RenderMessageKey(key string) RenderOption
func RenderMeta(v any) RenderOption

// Renderers, each installed once with App.UseSuccessRenderer / App.UseErrorRenderer
type SuccessRenderer func(ctx *Context, info RenderInfo) any
type ErrorRenderer func(ctx *Context, info *ErrorInfo) any

type RenderInfo struct {
    Status     int
    Data       any
    MessageKey string // from RenderMessageKey
    Meta       any    // from RenderMeta
}

// Request-scoped; must not be retained after the renderer returns.
type ErrorInfo struct {
    Err        error                        // original error, for logging and errors.As
    Status     int                          // effective status; the renderer may change it
    Code       string                       // stable machine code
    MessageKey string                       // exact key before resolution
    Message    string                       // resolved message, including safe fallback
    Details    any                          // client-safe detail
    Violations []validation.ValidationError // validation or bind violations
}

// The default error envelope.
type ErrorResponse struct {
    Success bool      `json:"success"`
    Error   ErrorBody `json:"error"`
}

type ErrorBody struct {
    Code       string                       `json:"code"`
    Message    string                       `json:"message"`
    Details    any                          `json:"details,omitempty"`
    Violations []validation.ValidationError `json:"violations,omitempty"`
}
```

`Render` is the opt-in success-envelope seam: with a `SuccessRenderer` installed via `app.UseSuccessRenderer`, the renderer receives `RenderInfo` and returns the body shape; the framework owns the write — status, application JSON profile, and the bodiless-status rule apply centrally. A nil return writes `Data` plain; a renderer that commits the response itself keeps full control (return value ignored); a renderer panic hits built-in recovery like any handler panic. With no renderer installed, `Render` falls back to plain `Response.JSON` and imposes no envelope. The raw helpers above are never routed through the renderer, so webhooks, health probes, and third-party-dictated shapes always bypass it.

The error-side mirror is `ErrorRenderer` ([ADR-009](../adr/009-handler-and-error-handling.md)) with the identical shape-only contract: it receives the classified `ErrorInfo`, and a nil return writes the [default error envelope](../adr/009-handler-and-error-handling.md#default-error-envelope) (`ErrorResponse`, `Content-Type: application/json; charset=utf-8`). The renderer may set headers and may change `info.Status`; an invalid status fails closed to a generic 500 and discards the renderer body, and a renderer panic yields the same generic 500. HEAD and body-forbidding statuses render status-only. The pairing is documented in the [error-handling guide](../guides/error-handling.md#success-and-error-envelopes).

In debug mode, a handler that writes body-carrying JSON through the raw `Response.JSON` helper while a `SuccessRenderer` is installed triggers a `WARN` (envelope-bypass diagnostic); intentional raw routes silence it with the `credo.MetaRawResponse` route meta. Non-JSON writers are exempt by design.

#### JSON output profile

`Response.JSON` encodes with `encoding/json/v2` under an application-level profile ([ADR-021](../adr/021-json-output-profile.md) holds the rationale). Two axes are set by the framework; the rest are json/v2's defaults, which differ from `encoding/json` v1 on the wire:

| Axis | Credo output | v1 behavior | Override |
| --- | --- | --- | --- |
| Map key order | sorted (`Deterministic`) | sorted | `jsonv2.Deterministic(false)` (default error bodies stay sorted) |
| `time.Duration` | integer nanoseconds, as `BindBody` decodes | integer nanoseconds | none |
| nil slice / nil map | `[]` / `{}` | `null` | `jsonv2.FormatNilSliceAsNull(true)`, `jsonv2.FormatNilMapAsNull(true)` |
| `omitempty` | drops JSON-empty (`""`, `null`, `[]`, `{}`) | drops Go zero values | `jsonv1.OmitEmptyWithLegacySemantics(true)`, or `omitzero` on the field |
| `<` `>` `&` | literal | escaped (`\u003c`) | `jsontext.EscapeForHTML(true)` |
| Trailing newline | none | `\n` | none |

Override any axis for the whole application with `credo.WithJSONOptions(...)`; options are applied after the framework profile, so each one wins on its axis:

```go
credo.WithJSONOptions(jsonv2.FormatNilSliceAsNull(true))  // null for nil slices
credo.WithJSONOptions(jsontext.EscapeForHTML(true))       // escape < > &
credo.WithJSONOptions(jsonv1.DefaultOptionsV1())          // full legacy mode
```

There is no per-call variant: one posture per application. `Context.Render` inherits the profile: with a `SuccessRenderer` installed, its returned body is encoded under the same profile, and only a renderer that commits the response itself writes other bytes. Framework-owned default error bodies always sort map keys, even when the application disables `Deterministic`; custom error-renderer bodies use the application profile.

## Rewrite Helpers

### OriginalPath

`OriginalPath()` returns the wire-form path (`URL.EscapedPath()`, percent-encoding preserved) captured in `reset()` before any middleware, pre-dispatch rewrite, or handler-level re-dispatch runs.

```go
func (c *Context) OriginalPath() string
```

The value is immutable for the lifetime of the request. When the final served path differs, the access log includes a `path_original` attribute.

### Rewrite

`Rewrite(path)` triggers an internal re-dispatch to a new path without sending an HTTP redirect to the client.

```go
func (c *Context) Rewrite(path string) error
```

Rules:

- Call it as the last statement in a handler: `return ctx.Rewrite("/new")`.
- The target must begin with `/`. It is a wire-form path: percent-encoded values decode once during matching (`/files/a%2Fb` reaches `RouteParam` as `a/b`), and a malformed escape makes `Rewrite` return an error.
- The target's query string, or its absence, replaces the request's query.
- Before matching again, dispatch writes the target onto the request — `URL.Path` decoded, `URL.RawPath` set when the spellings differ — so the target route's middleware and handler see the rewritten URL while `OriginalPath()` keeps the client's.
- The matched host scope does not change. Re-dispatch stays within the host mux selected for the original request.
- Framework features and global middleware run once per request; group and route middleware run again for the target route.
- Route params are cleared between dispatch rounds and rebuilt from the target route.
- If the response is already committed or hijacked, `Rewrite` returns an error.
- Credo enforces a hard limit of 10 internal rewrites per request. Exceeding the limit fails the request with a 500.

### IsRewriting

`IsRewriting()` reports whether the current handler chain has requested an internal rewrite that the dispatch loop will process next.

```go
func (c *Context) IsRewriting() bool
```

This is primarily useful in middleware with `after` logic that should skip side effects when a handler forwards to another route.

### Params and Host Params

`Request.RouteParam(name)` returns a single parameter value ("" when absent) and is the preferred accessor — parameters are stored as parallel key/value slices, so the lookup is an allocation-free linear scan (routes carry a handful of params at most). `Request.RouteParams()` returns all params as one `map[string]string` — path params and, for host-scoped routes, host params. The map is a read-only view materialized lazily on first call (writes to it are not seen by `RouteParam`), owned by the framework and recycled after the request completes; do not retain it.

```go
tenant := ctx.Request().RouteParam("tenant")
id := ctx.Request().RouteParam("id")
```

Host and path params share one namespace, so a host-scoped route whose path params collide with its host params panics at registration ([router spec](router.md#host-based-route-groups)).

`Request.PathValue(name)` is a stdlib-shaped shadow over the embedded `*http.Request` method: it resolves route params first and falls back to the embedded request. The dispatcher deliberately does not populate stdlib path values with a route's parameters (an extra allocation per request for data `RouteParam` already serves), so without this shadow `ctx.Request().PathValue("id")` would silently return "", and the raw embedded request — as seen by middleware via `WrapStdMiddleware` — carries none of them. The fallback is also how an app mounted under a parametric prefix reads that prefix: `Mount("/t/{tenant}", child)` hands the child a request whose path values hold `tenant`, so in the child `PathValue("tenant")` returns it while `RouteParam`/`RouteParams` hold the child's own route parameters only ([router spec](router.md#sub-router-mounting)).

### Scheme and RealIP

`Request.Scheme()` and `Request.RealIP()` expose client metadata derived from the transport and, when configured, trusted reverse-proxy headers.

```go
scheme := ctx.Request().Scheme() // "http" or "https"
ip := ctx.Request().RealIP()     // IP-only string when parseable
```

Forwarded headers are default-deny. Unless the immediate peer `RemoteAddr` is inside `server.trusted_proxies` or configured via `WithTrustedProxies`, Credo ignores the RFC 7239 `Forwarded` header, `X-Forwarded-For`, `X-Forwarded-Proto`, `X-Forwarded-Ssl`, `Front-End-Https`, and `X-Real-IP`.

`Scheme()` returns `https` for a direct TLS connection; behind a trusted proxy it prefers the `Forwarded` header's `proto=` parameter, then `X-Forwarded-Proto`, `X-Forwarded-Ssl` and `Front-End-Https`, and returns only `http` or `https`.

`RealIP()` prefers the RFC 7239 `Forwarded` header (`for=` parameters), then walks `X-Forwarded-For` from right to left, skipping trusted proxy hops and returning the first untrusted address. If no usable value exists, it falls back to `X-Real-IP`, then the direct peer address. Each chain walk is limited to 32 hops as a DoS guard.

Both values are cached for the lifetime of the request and cleared when the pooled context is reset.

## Bind Methods — "Parse, Don't Validate"

The `Bind*` methods live on the `Request` struct and implement the "parse, don't validate" approach. Each method performs two steps atomically:

1. **Decode** — read data from the request source (body or query)
2. **Validate** — if the target implements [`validation.Validatable`](validation.md#philosophy-parse-dont-validate), call `Validate()`

In debug mode (`WithDebug()` or `server.debug: true`), a warning is logged when the target does not implement `Validatable`.

### BindBody

Reads request body. Content-Type determines decoder:

For ordinary methods, a missing `Content-Type` retains the JSON convenience default. A matched RFC 10008 QUERY route is stricter: routing requires a non-blank `Content-Type` before the handler, even for an empty body. Once present, `BindBody` uses the same decoder, validation, strict-body, and error contracts shown below; unsupported media types return 415.

`BindBody` never decompresses implicitly. A request whose `Content-Encoding` is anything other than `identity` is rejected with 415 and the code `unsupported_content_encoding` before any decoder runs, so a compressed body is reported as what it is instead of as a JSON syntax error. Applications that accept compressed bodies opt in with `app.UseDecompress`, which unwraps gzip/deflate under its own decompressed-size bound and removes the header before Global middleware runs (see the [HTTP features spec](http-features.md#locale-and-transport-features)).

A nil or non-pointer bind target (and a non-struct target for query/form binding) is a developer error: `BindBody`/`BindQuery` return a 500 with the code `invalid_bind_target`, the reason is logged as the error's internal cause, and nothing about it reaches the client.

| Content-Type                        | Decoder            |
| ----------------------------------- | ------------------ |
| `application/json`                  | `encoding/json/v2` |
| `application/xml`, `text/xml`       | `encoding/xml`     |
| `application/x-www-form-urlencoded` | Form decoder       |
| `multipart/form-data`               | Multipart decoder  |

**Multipart files live for the request.** File parts beyond the in-memory threshold (32 MiB per form) are spilled to temporary files. They are removed when the request ends — on normal completion, a returned error and a panic alike — whether the form was parsed by `BindBody` or by the handler itself (`ParseMultipartForm`, `FormFile`). That includes a form parsed on a copy of the request, which `net/http` does not clean up on its own: after `SetUser`, behind `middleware.Timeout` or a wrapped stdlib middleware that substitutes the request, and in a handler attached with `Mount` (removed when the mounted handler returns). A `*multipart.FileHeader` is therefore valid only until the handler returns; copy the content before handing it to a goroutine that outlives the request.

```go
type CreateUserInput struct {
    Name  string `json:"name"`
    Email string `json:"email"`
}

func (c *CreateUserInput) Validate() error {
    return validation.ValidateStruct(c,
        validation.Field(&c.Name, validation.Required[string](), validation.Length(2, 100)),
        validation.Field(&c.Email, validation.Required[string](), validation.Email()),
    )
}

// Handler — one call, guaranteed valid output
app.POST("/users", func(ctx *credo.Context) error {
    var input CreateUserInput
    if err := ctx.Request().BindBody(&input); err != nil {
        return err // decode OR validation error → default error envelope
    }
    // input is GUARANTEED valid here
    return ctx.Response().JSON(201, svc.CreateUser(input))
})
```

#### Strict JSON bodies

JSON bodies are decoded with `encoding/json/v2` under strict semantics:

- **Exactly one JSON value per request** — after the first value decodes, only whitespace may remain in the body. A second JSON document or trailing garbage fails the bind with reason `trailing_data`.
- **Object members must be unique** — a repeated member fails the bind with reason `duplicate_field`. Because member matching is case-insensitive (see below), a case-variant repeat (`{"name":…,"Name":…}`) is also rejected.
- **Member matching stays case-insensitive** — v1-compatible matching is preserved via `MatchCaseInsensitiveNames`, so existing clients sending `{"Name":…}` for a `name`-tagged field keep working (v2's case-sensitive default would silently leave the field empty).
- **`time.Duration` fields decode as integer nanoseconds** — `BindBody` enables `FormatDurationAsNano` (json/v2 has no default Duration representation; [ADR-021](../adr/021-json-output-profile.md) gives the background): `{"timeout": 5000000000}` binds 5s, a string fails with `type_mismatch`. For human-readable durations (`"5s"`) declare a named type implementing `encoding.TextUnmarshaler`.
- **Unknown members are ignored by default, rejected under strict bodies** — `credo.WithStrictBodies()` or `server.strict_bodies: true` makes a member that maps to no target field fail the bind with reason `unknown_field` and the member's path (`address.zipp`). The switch is app-wide (one posture per service; no per-call option) and affects JSON only — XML and form decoders keep ignoring extra input. Case-insensitive matching runs before the unknown decision, so `{"Name":…}` still binds to `name`; a repeat of a _known_ member is reported as `duplicate_field`, while a repeated _unknown_ member fails as `unknown_field` on its first occurrence.

Together these close the parser-discrepancy class of payloads (`{"a":1}{"a":2}`, `{"a":1,"a":2}`, `{"a":1}junk`) where a security layer and the application could read different values. XML and form bodies keep their single-pass decoder semantics.

#### Decode error model

Decode failures are typed. `BindBody`/`BindQuery` return `*credo.BindError` carrying a machine-readable `Reason`, the affected `Field` path (when known), the `Expected` type for mismatches, and the JSON byte `Offset`. The error pipeline classifies it as `400 Bad Request` with top-level code `bind_failed`, mirroring the validation output shape — a single `violations[]` entry whose `code` is the reason:

| Reason | Meaning |
| --- | --- |
| `syntax` | malformed or truncated payload (JSON, XML, form encoding) |
| `type_mismatch` | value type does not match the target field (`expected` param set, JSON offset known) |
| `invalid_value` | value of the right shape failed semantic conversion (`encoding.TextUnmarshaler`, time parse, numeric overflow) |
| `empty_body` | request body absent or empty |
| `trailing_data` | content after the first JSON value |
| `duplicate_field` | JSON object member appears more than once (including case-variant repeats) |
| `unknown_field` | JSON object member maps to no target field — strict bodies only (`WithStrictBodies` / `server.strict_bodies`) |

```json
{
  "success": false,
  "error": {
    "code": "bind_failed",
    "message": "Malformed Request",
    "violations": [
      {"field": "age", "code": "type_mismatch", "message": "must be of type integer", "params": {"expected": "integer", "offset": 12}}
    ]
  }
}
```

Handlers can branch on the typed error with `errors.AsType[*credo.BindError](err)`. The underlying decoder error is preserved as `BindError.Internal`, logged at debug level (`credo: client error`) and never exposed to the client; Go type names are likewise not leaked (`Expected` uses JSON terms — `string`, `integer`, `number`, `boolean`, `array`, `object`). Client messages use bind scope plus the exact reason: an optional resolver may namespace it, otherwise the bare reason is the key. Field display-name handling matches validation. Body-size overruns are not `BindError`s — they keep the dedicated `413 Request Entity Too Large` classification.

### BindQuery

Reads URL query parameters into a struct.

```go
type ListUsersQuery struct {
    Page    int    `query:"page"`
    PerPage int    `query:"per_page"`
    Sort    string `query:"sort"`
}

func (q *ListUsersQuery) Validate() error {
    return validation.ValidateStruct(q,
        validation.Field(&q.Page, validation.Min(1)),
        validation.Field(&q.PerPage, validation.Between(1, 100)),
        validation.Field(&q.Sort, validation.In("name", "created_at")),
    )
}

app.GET("/users", func(ctx *credo.Context) error {
    var q ListUsersQuery
    if err := ctx.Request().BindQuery(&q); err != nil {
        return err
    }
    return ctx.Response().JSON(200, svc.ListUsers(q))
})
```

**Supported field types**: `string`, `bool`, all integer types (`int`, `int8`–`int64`, `uint`–`uint64`), `float32`, `float64`, any type implementing `encoding.TextUnmarshaler` (`time.Time`, `netip.Addr`, custom types — takes precedence over native conversions, matching the config decoder), slices of all of these (`[]string`, `[]int`, `[]float64`, repeated params), and pointer variants of all scalar types (`*string`, `*int`, `*time.Time`, etc.). Pointer fields stay `nil` when the parameter is absent, enabling nil-vs-zero-value distinction (e.g., PATCH partial updates, optional filters). Embedded structs and pointer-to-embedded-structs are supported recursively. The same type support applies to `form:"..."` tags used by `BindBody` for form-urlencoded and multipart content types.

## Design Decisions

- A struct rather than an interface, the Request/Response split, the `RouteParam`, `Text` and `Bind*` names, the reflection boundary of binding, proxy metadata on Request and the original path on Context, and the rejected alternatives (`FormValue` and `SetRequest` on Context, a per-call strict-bodies switch): [ADR-008](../adr/008-context-design.md).
- The error-side choices (handler signature, classification, default envelope, renderer seam): [ADR-009](../adr/009-handler-and-error-handling.md).
- The JSON output profile: [ADR-021](../adr/021-json-output-profile.md).

## Pooling

Context structs, each with its Request and Response, are pooled per App in a `sync.Pool` built by `New`. Each request:

1. Passes the lifecycle admission gate in `ServeHTTP`, then takes a Context from the pool (a request answered with the [lifecycle 503](lifecycle.md#lifecycle-rejection-503) takes none)
2. Resets it with the new `ResponseWriter` and `*http.Request`
3. Runs the feature executor and the handler chain ([HTTP features spec](http-features.md#request-execution))
4. Returns the Context to the pool after finalization, the access record and multipart cleanup, on every path including a propagating panic

This removes the per-request Context, Request and Response allocations from the hot path.
