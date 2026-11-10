# Context Spec

**Status:** Implemented (beta) **Package:** root (`github.com/credo-go/credo`) **ADR:** [ADR-008](../adr/008-context-design.md), [ADR-009](../adr/009-handler-and-error-handling.md), [ADR-011](../adr/011-validation-strategy.md), [ADR-012](../adr/012-authentication-and-authorization.md), [ADR-013](../adr/013-internationalization.md), [ADR-018](../adr/018-host-routing-and-rewrite.md), [ADR-021](../adr/021-json-output-profile.md) **Guide:** [Getting started](../guides/getting-started.md#request-and-response), [Error handling](../guides/error-handling.md), [Routing](../guides/routing.md), [Proxy trust](../guides/proxy-trust.md) **Sources:** Echo (MIT)

> **Contract.** Every section of this spec is normative — what a client sees on the wire or application code can observe, the error contract included — except [Current Behaviour](#current-behaviour), which describes the data structures and mechanisms behind that contract as they are now: not guaranteed, they may change in any release. [Design Decisions](#design-decisions) link the reasons. The examples run as `Example*` tests in [`example_context_test.go`](../../example_context_test.go), so a drift fails the build.

## Overview

`credo.Context` is Credo's HTTP request context: a request-scoped **struct** (not interface) that holds the `*Request`, `*Response`, matched `*Route`, a logger, and the request's current `context.Context`, its head, over which typed keys carry the request's values ([Context head and typed keys](#context-head-and-typed-keys)). `Context()` returns the head; the struct deliberately does **not** itself implement `context.Context`, because its head moves as scopes open and return. Documentation and examples name the parameter `c *credo.Context`, since `ctx` conventionally names a `context.Context`.

A `*Context` kept past its request reads that request's state, never another request's ([Allocation](#allocation)).

Lifecycle hooks such as `app.OnStart` and `app.OnStop`, and a component's `Start` and `Shutdown`, receive a standard `context.Context`, not `*credo.Context`. That context lives as long as its call and carries the cancellation and the deadline of the start phase or the drain; `credo.Context` exists only for an HTTP request.

Input and output concerns are separated into `Request` and `Response` structs:

- **Request** — wraps `*http.Request`, provides route params, query shortcuts, and `Bind*` methods ("parse, don't validate").
- **Response** — wraps `http.ResponseWriter`, provides `JSON`, `Text`, `HTML`, `XML`, `NoContent`, `Redirect`, `Blob`, `Stream` helpers.

Defined in the **root package** (not a sub-package) to avoid collision with stdlib `context`. The framework-owned HTTP features that run around the handler chain are specified in the [HTTP features spec](http-features.md), and locale detection in the [i18n spec](i18n.md#detector-contract).

## API Surface

### Context Struct

```go
type Context struct {
    // unexported fields — access via methods only
}

func NewContext(w http.ResponseWriter, r *http.Request) *Context // without an App: for unit tests of hooks, a Request or a Response

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

// Locale and translation — resolved on first use and memoized for the request
func (c *Context) Locale() string // "" without UseI18n
func (c *Context) T(key string, data ...map[string]any) string
func (c *Context) TPlural(key string, count any, data ...map[string]any) string

// Authenticated user — one slot per type T
func (c *Context) SetUser[T any](user T)
func (c *Context) GetUser[T any]() (T, bool)
func (c *Context) RequireUser[T any]() (T, error) // 401 wrapping ErrUserMissing when absent


// Request context access — the request's context.Context
func (c *Context) Context() context.Context
```

```go
// Typed keys — one key type for request values and route meta
type Key[T any] struct {
    // unexported; build one with NewKey
}

func NewKey[T any](name string) Key[T] // the name is for diagnostics; identity is the key value

// Request values — over the Context's head
func (c *Context) Set[T any](key Key[T], val T)    // a value of another type does not compile
func (c *Context) Get[T any](key Key[T]) (T, bool) // the zero value and false when no layer set it

// Key methods
func (k Key[T]) String() string // the name given to NewKey

// Scopes — ctx, derived from c.Context(), is the head of the inner chain until next returns or panics
func (c *Context) Within(ctx context.Context, next Handler) error
```

`Locale`, `T` and `TPlural` follow the [i18n spec](i18n.md#detector-contract); the user accessors and `credo.ErrUserMissing` follow the [auth spec](auth.md#user-accessors-generic-context-methods).

### Request Struct

```go
type Request struct {
    *http.Request  // embedded — Cookie, Header, Method, URL all available
}

func (r *Request) RouteParam(name string) string  // single path/host param; "" when absent (preferred)
func (r *Request) RouteParams() map[string]string // all params; a read-only view
func (r *Request) PathValue(name string) string   // stdlib-shaped shadow → RouteParam, falls back to embedded request
func (r *Request) QueryParam(name string) string
func (r *Request) Scheme() string                 // "http" or "https"
func (r *Request) RealIP() string                 // original client IP with trusted proxy support

// Bind — "parse, don't validate" (decode + validate in one step)
func (r *Request) BindBody(target any) error
func (r *Request) BindQuery(target any) error
```

```go
// Std returns the standard request carrying the Context's head. The call may replace the
// embedded request: when it does not already carry the head, it is replaced by a copy with it.
func (r *Request) Std() *http.Request

// Context returns the request's current context, the head c.Context() returns.
// It shadows the embedded request's Context, which lags the head after a Set.
func (r *Request) Context() context.Context
```

`Std()` is the one accessor for the standard request, used wherever stdlib code takes one ([stdlib boundary](#the-stdlib-boundary)). `Request.Context()` returns the head, as `c.Context()` does. The embedded `Request.Request` stays the field it is; its own `Context()`, `c.Request().Request.Context()`, is the context it was built with, which lags the head after a `Set`.

### Response Struct

```go
type Response struct {
    http.ResponseWriter // swappable by the compression feature or a wrapped stdlib middleware
    // status, size, committed, hijacked — framework-owned tracking state (unexported)
}

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

`Response` implements `io.ReaderFrom`, so `Stream` and any `io.Copy` into the response go through it ([how it copies](#response-copying)). The bytes the writer accepted count toward `Size()`, a partial-write error is returned with the accepted count, a hijacked response answers `http.ErrHijacked`, and the commit follows `Write`: nothing is committed before the first byte (a source that fails up front leaves the response uncommitted so the handler's error still renders), and the first byte commits status 200 when none was written. A source that panics after its first bytes leaves the response committed: recovery logs the panic, the access record observes the 200 already on the wire, and no error envelope is appended. `Size()` — and the access record's bytes — stay equal to what the client received, even when the source panics mid-copy.

#### Render and the envelope seam

```go
// Method on *Context — the single call site that consults App.UseSuccessRenderer
func (c *Context) Render(status int, data any, opts ...RenderOption) error

// Side channels for the renderer's envelope; silently dropped with no renderer
func RenderMessageKey(key MessageKey) RenderOption
func RenderMeta(v any) RenderOption

// Renderers, each installed once with App.UseSuccessRenderer / App.UseErrorRenderer
type SuccessRenderer func(ctx *Context, info RenderInfo) any
type ErrorRenderer func(ctx *Context, info *ErrorInfo) any

type RenderInfo struct {
    Status     int
    Data       any
    MessageKey MessageKey // from RenderMessageKey
    Meta       any        // from RenderMeta
}

// Request-scoped; must not be retained after the renderer returns.
type ErrorInfo struct {
    Err        error                        // original error, for logging and errors.As
    Status     int                          // effective status; the renderer may change it
    Code       string                       // stable machine code
    MessageKey MessageKey                   // catalog key before resolution; empty for a literal message
    Message    string                       // resolved message, including safe fallback
    Details    any                          // client-safe detail
    Violations []validation.Violation // validation or bind violations
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
    Violations []validation.Violation `json:"violations,omitempty"`
}
```

A message key has its own named string type, `credo.MessageKey`, an alias of `validation.MessageKey`, so a violation's key has it too: `RenderMessageKey(key MessageKey)`, `RenderInfo.MessageKey` and `ErrorInfo.MessageKey` take it, as `HTTPError.WithMessageKey` does. A key is a catalog key only, never rendered as text and never carrying client input: a key the catalog lacks falls back to the built-in message for the code and then to the status text, and in debug mode the pipeline warns with the key, the code and the route. Literal client text is set with `HTTPError.WithMessage`, which is never looked up and wins over a key; `ErrorInfo.MessageKey` is then empty. A key computed as a `string` converts to `MessageKey`; a `string` variable passed without a conversion does not compile ([ADR-009](../adr/009-handler-and-error-handling.md)).

`Render` is the opt-in success-envelope seam: with a `SuccessRenderer` installed via `app.UseSuccessRenderer`, the renderer receives `RenderInfo` and returns the body shape; the framework owns the write — status, application JSON profile, and the bodiless-status rule apply centrally. A nil return writes `Data` plain; a renderer that commits the response itself keeps full control (return value ignored); a renderer panic hits built-in recovery like any handler panic. With no renderer installed, `Render` falls back to plain `Response.JSON` and imposes no envelope. The raw helpers above are never routed through the renderer, so webhooks, health probes, and third-party-dictated shapes always bypass it.

The error-side mirror is `ErrorRenderer` ([ADR-009](../adr/009-handler-and-error-handling.md)) with the identical shape-only contract: it receives the classified `ErrorInfo`, and a nil return writes the [default error envelope](../adr/009-handler-and-error-handling.md#default-error-envelope) (`ErrorResponse`, `Content-Type: application/json; charset=utf-8`). The renderer may set headers and may change `info.Status`; an invalid status fails closed to a generic 500 and discards the renderer body, and a renderer panic yields the same generic 500. HEAD and body-forbidding statuses render status-only. The pairing is documented in the [error-handling guide](../guides/error-handling.md#success-and-error-envelopes).

In debug mode, a handler that writes body-carrying JSON through the raw `Response.JSON` helper while a `SuccessRenderer` is installed triggers a `WARN` (envelope-bypass diagnostic); intentional raw routes silence it by setting the `credo.MetaRawResponse` route meta, a `credo.Key[bool]`, to `true`. Non-JSON writers are exempt by design.

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

There is no per-call variant: one posture per application. Runnable as `ExampleWithJSONOptions`; the bodiless-status rule as `ExampleResponse_JSON`, and `Render` with a success renderer as `ExampleContext_Render`. `Context.Render` inherits the profile: with a `SuccessRenderer` installed, its returned body is encoded under the same profile, and only a renderer that commits the response itself writes other bytes. Framework-owned default error bodies always sort map keys, even when the application disables `Deterministic`; custom error-renderer bodies use the application profile.

## Context Head and Typed Keys

### Typed keys

```go
var Tenant = credo.NewKey[*TenantInfo]("tenant")

c.Set(Tenant, t)       // a value of another type does not compile
t, ok := c.Get(Tenant) // t is a *TenantInfo

var RequiredScopes = credo.NewKey[[]Scope]("auth.scope")
route.SetMeta(RequiredScopes, []Scope{"admin"})  // the value type is checked at compile time
scopes, ok := c.Route().GetMeta(RequiredScopes) // the route, then its groups, then the App
```

Runnable as `ExampleContext_Set`.

One key type, `Key[T]`, serves request values and [route meta](router.md#route-meta-system-goyave-inspired). The name passed to `NewKey` is for diagnostics; identity is the key value, so the keys of two packages never collide. Two `NewKey` calls with one name are two keys, while two `MessageKey` values with one text are one catalog key; both godocs say so. A zero `Key[T]`, declared without `NewKey`, panics in every method that takes a key, naming the method, since all zero keys of one type would be one context key.

`Set`, `Get`, `SetMeta` and `RemoveMeta` take a key, as generic methods whose type parameter is inferred from it; `GetMeta` returns `(T, bool)`, as `Get` and `GetUser` do. Credo reads and writes values only through these methods. Code that holds only a `context.Context` reads a value with the standard `Value` and its key, which is the context key; Credo adds no reader of its own. The framework's own keys are typed too: `middleware.MetaAccept`, `MetaRequireHeaders`, `MetaRequireQuery`, `MetaAPIVersion` and `MetaScope` are `Key[[]string]`, `middleware.MetaMaxBody` is a `Key[int64]`, and `credo.MetaAccessLog` and `credo.MetaRawResponse` are `Key[bool]`. The request ID's typed key stays unexported: `c.RequestID()` reads it.

### The head

The Context holds the request's current `context.Context`, its head; `c.Context()` and `c.Request().Context()` return it. `c.Set` makes a new head that carries the value; no request is copied for a value. A context handed to a goroutine is fixed, so successive `Value` calls on it agree, and no later `Set` changes what it holds. A key set again and again, as a server-sent-events or WebSocket loop may set one, does not grow the head without bound ([how the head is built](#the-heads-nodes)).

### Scopes

A layer that derives a context derives it from the head and makes the result the head of the inner chain: `middleware.Timeout`'s deadline and a span a stdlib middleware adds through `WrapStdMiddleware` are visible through `c.Context()`. When the layer returns or panics, the head reverts to the outer one, so a deadline or span ends with its scope, and the values set through `c.Set` inside it are applied again over the outer head: a user or a tenant is a fact about the request, not about one layer. The Context keeps the order of its `Set` calls for that, and the scope's end runs in a deferred call, so when the handler panics, a user set inside the scope is still the request's user for the error rendering, the observers and the access record.

`c.Within(ctx, next)` opens a scope: it makes `ctx` the head of the inner chain, keeps the embedded request in step with one copy per scope, runs `next`, and reverts as above when `next` returns or panics. `ctx` is derived from `c.Context()`; one that is not is an unrelated context ([edges](#edges-of-the-head)). A value the scope owns, such as a transaction or a span, goes into the context given to `Within` and ends with it, while `c.Set` records a fact about the request, which outlives the scope: a transaction stored with `c.Set` would still be found after its commit.

```go
func Deadline(d time.Duration) credo.Middleware {
    return func(next credo.Handler) credo.Handler {
        return func(c *credo.Context) error {
            ctx, cancel := context.WithTimeout(c.Context(), d)
            defer cancel()
            return c.Within(ctx, next)
        }
    }
}
```

Runnable as `ExampleContext_Within`; a wrapped stdlib middleware's context as `ExampleWrapStdMiddleware`.

Assigning `c.Request().Request` a request with a new context still compiles and no longer moves the head: the next `Std()` hands stdlib code a copy of the assigned request with the head. In debug mode the Context logs one warning per route for an embedded request it did not install, naming the route and the remedies, `c.Within(ctx, next)` and `c.Request().Std()`.

### The stdlib boundary

`c.Request().Std()` is the one accessor for the standard request: the embedded `*http.Request` when it already carries the head, otherwise a copy with the head that replaces the embedded request, so a second call without a new `Set` copies nothing, and a multipart form that stdlib code parses on the copy stays under the request-end cleanup. `WrapStdMiddleware` and `Mount` hand the request on through it, and take the context of the request the stdlib code passes on as the head of the inner chain. `c.Request().Request` stays the embedded field, and its own `Context()` is the context it was built with, which lags the head after a `Set`; `Request.Context()` shadows it and returns the head. `Std()`'s godoc says in its first sentence that the call may replace the embedded request, and the `Request` type's godoc points to `Request.Context` and `Std` for the head.

### Edges of the head

- **A retry.** A Credo middleware that calls `next` again runs the second attempt over the head the first left: the values the failed attempt set are visible to the second and, unless it sets them again, to the observers. The Context cannot tell a retry from a middleware that handles the error and goes on, and a user set before the failure is still the request's user.
- **Another goroutine.** A stdlib middleware that runs the next handler on another goroutine, as `http.TimeoutHandler` does, is outside `WrapStdMiddleware`'s contract: the Context is not safe for concurrent use, and the goroutine would move the head while the outer layer reverts it. `middleware.Timeout` sets its deadline on the request's own goroutine.
- **An unrelated context.** A stdlib middleware that passes on a context not derived from the request's, which drops the request's cancellation too, cuts the handlers after it off from the values set before it, through `c.Get` as through `Value`.
- **A mounted Credo App** has a Context of its own. The mount hands it the request through `Std()`, so it reads what was set before the mount, the user included; what it sets stays in its own Context, and the parent's observers and access record do not see it.

### User accessors

`SetUser`, `GetUser[T]` and `RequireUser[T]` stay, as shortcuts over a generic key whose type is unexported, so the user has no `context.Context` accessor ([auth spec](auth.md#user-accessors-generic-context-methods)). `SetUser` copies no request.

## Original Client Path

### OriginalPath

`OriginalPath()` returns the wire-form path (`URL.EscapedPath()`, percent-encoding preserved) captured when the Context is built, before any middleware or pre-dispatch rewrite runs.

```go
func (c *Context) OriginalPath() string
```

The value is immutable for the lifetime of the request. When the final served path differs, the access log includes a `path_original` attribute.

### Params and Host Params

`Request.RouteParam(name)` returns a single parameter value ("" when absent) and is the preferred accessor. `Request.RouteParams()` returns all params as one `map[string]string` — path params and, for host-scoped routes, host params. The map is a read-only view: writes to it are not seen by `RouteParam`.

```go
tenant := c.Request().RouteParam("tenant")
id := c.Request().RouteParam("id")
```

The map `RouteParams` returns is never handed to another request.

Host and path params share one namespace, so a host-scoped route whose path params collide with its host params panics at registration ([router spec](router.md#host-based-route-groups)).

`Request.PathValue(name)` is a stdlib-shaped shadow over the embedded `*http.Request` method: it resolves route params first and falls back to the embedded request. The dispatcher deliberately does not populate stdlib path values with a route's parameters: doing so would allocate a map per request outside a `ServeMux` pattern, for data `RouteParam` already serves, and share that map between a request and its `WithContext` copy, so that a value set on one would show through the other ([ADR-008](../adr/008-context-design.md#route-parameters-are-not-stdlib-path-values)). Without this shadow `c.Request().PathValue("id")` would silently return "", and the raw embedded request — as seen by middleware via `WrapStdMiddleware` — carries none of them. The fallback is also how an app mounted under a parametric prefix reads that prefix: `Mount("/t/{tenant}", child)` hands the child a request whose path values hold `tenant`, so in the child `PathValue("tenant")` returns it while `RouteParam`/`RouteParams` hold the child's own route parameters only ([router spec](router.md#sub-router-mounting)).

### Scheme and RealIP

`Request.Scheme()` and `Request.RealIP()` expose client metadata derived from the transport and, when configured, trusted reverse-proxy headers.

```go
scheme := c.Request().Scheme() // "http" or "https"
ip := c.Request().RealIP()     // IP-only string when parseable
```

Forwarded headers are default-deny. Unless the immediate peer `RemoteAddr` is inside `server.trusted_proxies` or configured via `WithTrustedProxies`, Credo ignores the RFC 7239 `Forwarded` header, `X-Forwarded-For`, `X-Forwarded-Proto`, `X-Forwarded-Ssl`, `Front-End-Https`, and `X-Real-IP`.

`Scheme()` returns `https` for a direct TLS connection; behind a trusted proxy it prefers the `Forwarded` header's `proto=` parameter, then `X-Forwarded-Proto`, `X-Forwarded-Ssl` and `Front-End-Https`, and returns only `http` or `https`.

`RealIP()` prefers the RFC 7239 `Forwarded` header (`for=` parameters), then walks `X-Forwarded-For` from right to left, skipping trusted proxy hops and returning the first untrusted address. If no usable value exists, it falls back to `X-Real-IP`, then the direct peer address. Each chain walk is limited to 32 hops as a DoS guard.

Both values are cached for the lifetime of the request.

## Bind Methods — "Parse, Don't Validate"

The `Bind*` methods live on the `Request` struct and implement the "parse, don't validate" approach. Each method performs two steps atomically:

1. **Decode** — read data from the request source (body or query)
2. **Validate** — if the target implements [`validation.Validatable`](validation.md#philosophy-parse-dont-validate), call `Validate()`

In debug mode (`WithDebug()` or `server.debug: true`), a warning is logged when the target does not implement `Validatable`.

### BindBody

Reads request body. Content-Type determines decoder:

For ordinary methods, a missing `Content-Type` retains the JSON convenience default. A matched RFC 10008 QUERY route is stricter: routing requires a non-blank `Content-Type` before the handler, even for an empty body. Once present, `BindBody` uses the same decoder, validation, strict-body, and error contracts shown below; unsupported media types return 415.

`BindBody` never decompresses implicitly. A request whose `Content-Encoding` is anything other than `identity` is rejected with 415 and the code `unsupported_content_encoding` before any decoder runs, so a compressed body is reported as what it is instead of as a JSON syntax error. Applications that accept compressed bodies opt in with `app.UseDecompress`, which unwraps gzip/deflate under its own decompressed-size bound and removes the header before `PreRouting` middleware runs (see the [HTTP features spec](http-features.md#locale-and-transport-features)).

An unsupported content encoding and an unsupported content type carry the rejected value in the error's `Details`, under `encoding` and `content_type`, never in its message key or its message: the error sets no key, so its message is resolved from its code.

A nil or non-pointer bind target (and a non-struct target for query/form binding) is a developer error: `BindBody`/`BindQuery` return a 500 with the code `invalid_bind_target`, the reason is logged as the error's internal cause, and nothing about it reaches the client.

| Content-Type                        | Decoder            |
| ----------------------------------- | ------------------ |
| `application/json`                  | `encoding/json/v2` |
| `application/xml`, `text/xml`       | `encoding/xml`     |
| `application/x-www-form-urlencoded` | Form decoder       |
| `multipart/form-data`               | Multipart decoder  |

**Multipart files live for the request.** File parts beyond the in-memory threshold (32 MiB per form) are spilled to temporary files. They are removed when the request ends — on normal completion, a returned error and a panic alike — whether the form was parsed by `BindBody` or by the handler itself (`ParseMultipartForm`, `FormFile`). That includes a form parsed on a copy of the request, which `net/http` does not clean up on its own: the copy `Request.Std()` hands to stdlib code, the copy a scope opened with `Within` installs, a request a wrapped stdlib middleware passes on, and a handler attached with `Mount` (removed when the mounted handler returns). `SetUser` and `Set` copy no request. A `*multipart.FileHeader` is therefore valid only until the handler returns; copy the content before handing it to a goroutine that outlives the request.

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
app.POST("/users", func(c *credo.Context) error {
    var input CreateUserInput
    if err := c.Request().BindBody(&input); err != nil {
        return err // decode OR validation error → default error envelope
    }
    // input is GUARANTEED valid here
    return c.Response().JSON(201, svc.CreateUser(input))
})
```

Runnable as `ExampleRequest_BindBody`.

#### Strict JSON bodies

JSON bodies are decoded with `encoding/json/v2` under strict semantics:

- **Exactly one JSON value per request** — after the first value decodes, only whitespace may remain in the body. A second JSON document or trailing garbage fails the bind with reason `trailing_data`.
- **Object members must be unique** — a repeated member fails the bind with reason `duplicate_field`. Because member matching is case-insensitive (see below), a case-variant repeat (`{"name":…,"Name":…}`) is also rejected.
- **Member matching stays case-insensitive** — v1-compatible matching is preserved via `MatchCaseInsensitiveNames`, so existing clients sending `{"Name":…}` for a `name`-tagged field keep working (v2's case-sensitive default would silently leave the field empty).
- **`time.Duration` fields decode as integer nanoseconds** — `BindBody` enables `FormatDurationAsNano` (json/v2 has no default Duration representation; [ADR-021](../adr/021-json-output-profile.md) gives the background): `{"timeout": 5000000000}` binds 5s, a string fails with `type_mismatch`. For human-readable durations (`"5s"`) declare a named type implementing `encoding.TextUnmarshaler`.
- **Unknown members are ignored by default, rejected under strict bodies** — `credo.WithStrictBodies()` or `server.strict_bodies: true` makes a member that maps to no target field fail the bind with reason `unknown_field` and the member's path (`address.zipp`). The switch is app-wide (one posture per service; no per-call option) and affects JSON only — XML and form decoders keep ignoring extra input ([XML bodies](#xml-bodies)). Case-insensitive matching runs before the unknown decision, so `{"Name":…}` still binds to `name`; a repeat of a _known_ member is reported as `duplicate_field`, while a repeated _unknown_ member fails as `unknown_field` on its first occurrence.

Together these close the parser-discrepancy class of payloads (`{"a":1}{"a":2}`, `{"a":1,"a":2}`, `{"a":1}junk`) where a security layer and the application could read different values. Form bodies keep their single-pass decoder semantics; an XML body's contract is below.

#### XML bodies

An `application/xml` or `text/xml` body is decoded with `encoding/xml` into the target's `xml` struct tags. The binder decides three things the decoder leaves open:

- **Exactly one document per request** — after the root element only white space (space, tab, carriage return, line feed), comments and processing instructions may remain, which is what XML's grammar allows there. Anything else fails the bind with reason `trailing_data`: a second element, text, a directive such as `<!DOCTYPE …>`, or markup that does not parse. This is the XML form of the one-value rule above, and it closes the same class of payload (`<order>…</order><order>…</order>`).
- **A repeated element keeps `encoding/xml`'s meaning** — repetition is how XML spells a list, so it is never a `duplicate_field` failure. Every occurrence is appended to a slice field, and for a field of another kind the last occurrence sets the value.
- **Unknown input is ignored in both modes** — an element or attribute the target has no field for is skipped, under `WithStrictBodies` too. Strict bodies decide JSON members only.

The prolog (`<?xml version="1.0"?>`) and anything else before the root element are read as `encoding/xml` reads them. An empty body fails with `empty_body`, and a document that does not parse, or whose root element is not the one the target names, fails with `syntax`. A body that passes the size limit after its root element is a 413, as everywhere else.

#### Decode error model

Decode failures are typed. `BindBody`/`BindQuery` return `*credo.BindError` carrying a machine-readable `Reason`, the affected `Field` path (when known), the `Expected` type for mismatches, and the JSON byte `Offset`. The error pipeline classifies it as `400 Bad Request` with top-level code `bind_failed`, mirroring the validation output shape — a single `violations[]` entry whose `code` is the reason:

| Reason | Meaning |
| --- | --- |
| `syntax` | malformed or truncated payload (JSON, XML, form encoding) |
| `type_mismatch` | value type does not match the target field (`expected` param set, JSON offset known) |
| `invalid_value` | value of the right shape failed semantic conversion (`encoding.TextUnmarshaler`, time parse, numeric overflow) |
| `empty_body` | request body absent or empty |
| `trailing_data` | content after the first JSON value, or after an XML body's root element (white space, comments and processing instructions excepted) |
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

The decode error above runs as `ExampleBindError`. Handlers can branch on the typed error with `errors.AsType[*credo.BindError](err)`. The underlying decoder error is preserved as `BindError.Internal`, logged at debug level (`credo: client error`) and never exposed to the client; Go type names are likewise not leaked (`Expected` uses JSON terms — `string`, `integer`, `number`, `boolean`, `array`, `object`). Client messages use bind scope plus the exact reason: an optional resolver may namespace it, otherwise the bare reason is the key. Field display-name handling matches validation. Body-size overruns are not `BindError`s — they keep the dedicated `413 Request Entity Too Large` classification.

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

app.GET("/users", func(c *credo.Context) error {
    var q ListUsersQuery
    if err := c.Request().BindQuery(&q); err != nil {
        return err
    }
    return c.Response().JSON(200, svc.ListUsers(q))
})
```

Runnable as `ExampleRequest_BindQuery`.

**Supported field types**: `string`, `bool`, all integer types (`int`, `int8`–`int64`, `uint`–`uint64`), `float32`, `float64`, any type implementing `encoding.TextUnmarshaler` (`time.Time`, `netip.Addr`, custom types — takes precedence over native conversions, matching the config decoder), slices of all of these (`[]string`, `[]int`, `[]float64`, repeated params), and pointer variants of all scalar types (`*string`, `*int`, `*time.Time`, etc.). Pointer fields stay `nil` when the parameter is absent, enabling nil-vs-zero-value distinction (e.g., PATCH partial updates, optional filters). Embedded structs are walked recursively as `encoding/json` walks them for a JSON body: an embedded struct promotes its exported fields even when its type is unexported, an embedded pointer to an exported struct type is allocated when one of its fields is bound, and an embedded pointer to an unexported struct type is skipped, since the binder cannot set it. The same type support applies to `form:"..."` tags used by `BindBody` for form-urlencoded and multipart content types.

A field of a type outside this list is a developer error: the bind returns a 500 with the code `invalid_bind_target`, the reason is logged as the error's internal cause, as for an invalid bind target, and nothing about it reaches the client.

## Design Decisions

- A struct rather than an interface, the Request/Response split, the `RouteParam`, `Text` and `Bind*` names, the reflection boundary of binding, proxy metadata on Request and the original path on Context, and the rejected alternatives (`FormValue` and `SetRequest` on Context, a per-call strict-bodies switch): [ADR-008](../adr/008-context-design.md).
- The error-side choices (handler signature, classification, default envelope, renderer seam): [ADR-009](../adr/009-handler-and-error-handling.md).
- The JSON output profile: [ADR-021](../adr/021-json-output-profile.md).
- Route parameters are not stdlib path values: [ADR-008](../adr/008-context-design.md#route-parameters-are-not-stdlib-path-values).
- The context head, the typed keys, `Within`, `Std()` and their rejected alternatives: [ADR-008](../adr/008-context-design.md#one-context-head-and-typed-keys).
- One allocation per request, without a pool, and its measurement: [ADR-008](../adr/008-context-design.md#one-allocation-per-request).
- The pruned APIs — `Response.Reset`, the `RouteContext` alias, `NewRequest`, `NewResponse`, `Context.Rewrite` and `IsRewriting` — and `NewContext`, which stays: [ADR-008](../adr/008-context-design.md#pruned-context-apis).

## Allocation

Each request has a Context of its own, which no other request reuses. A `*Context` kept past its request reads that request's values, `GetUser` included, after the next request on the same connection has run. What remains is stated where it applies: a `*Context` is not safe for concurrent use, since its head moves as scopes return, and its response cannot be written after the handler returns, as `net/http` forbids for every `ResponseWriter`. How a request's Context is allocated is [current behaviour](#one-allocation-per-request).

## Current Behaviour

Not guaranteed: the data structures and mechanisms behind the contract above, as they are now. A release may change them without notice, provided the contract holds.

### One allocation per request

The Context is not pooled. Each request:

1. Passes the lifecycle admission gate in `ServeHTTP`, then allocates one Context (a request answered with the [lifecycle 503](lifecycle.md#lifecycle-rejection-503) allocates none)
2. Runs the feature executor and the handler chain ([HTTP features spec](http-features.md#request-execution))
3. Finalizes, emits the access record and cleans up multipart files, on every path including a propagating panic

The one allocation folds the Context, its `Request` and `Response`, embedded by value, and the first four route parameters and the first two values, kept in fixed arrays. The measurement is recorded in [ADR-008](../adr/008-context-design.md#one-allocation-per-request).

### Route parameters

Parameters are stored as parallel key/value slices, so `RouteParam` is an allocation-free linear scan (routes carry a handful of params at most). The map `RouteParams` returns is materialized lazily on its first call.

### The head's nodes

`c.Set` replaces the head with a `context.WithValue` node over it; no custom `Value` implementation exists, and `SetUser` adds a node too. When the head is the node the Context built for a key set again, the new node is built over that node's parent. Keys set in turn add a node each, so once the nodes the Context built over the request's context, or over a scope's, number twice the keys set there, it rebuilds them from the record of `Set` calls, which keeps one entry per key. Neither modifies a node, so a context handed out before keeps its values.

The reapplication at a scope's end is lazy. A scope that ends marks the values set inside it as pending; the first read of the head after it — `c.Context()`, `Request.Context()`, `Std()`, a `Set`, a new scope — builds their nodes over the outer head. Nested scopes that end in turn with no read between them build the nodes once, and `c.Get` and `GetUser` read a pending value from the record of `Set` calls without building any, so an observer that reads only the user builds no node.

### Response copying

`Response.ReadFrom` copies a Reader-only source without allocating a buffer per call. On an uncommitted response the first 32 KiB of the source always pass through `Response.Write` from a framework-pooled buffer, so the commit is recorded by `Response` before anything else can fail. When the underlying `http.ResponseWriter` implements `io.ReaderFrom` (net/http's HTTP/1.1 writer, which keeps its sendfile/splice path for `*os.File` and socket sources on a plaintext TCP connection) the rest of a larger source — or the whole source of an already committed response — is delegated to the writer's `ReadFrom`; otherwise — a compressing or other wrapping writer, HTTP/2 — the copy runs through the pooled buffer and every chunk passes through `Response.Write`. A source implementing `io.WriterTo` writes itself on the pooled path. The delegated source is wrapped in a Response-owned `io.LimitedReader` (no allocation; the type net's sendfile and splice paths unwrap, so the zero-copy path survives) whose remaining count records what the writer consumed, which is how `Size()` stays equal to what the client received.
