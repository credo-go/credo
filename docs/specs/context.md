# Context Spec

**Status**: Approved
**Package**: Root (`github.com/credo-go/credo`)
**Sources**: Echo (MIT)
**Depends on**: `internal/radix/`
**ADRs**: [011-validation-strategy](../adr/011-validation-strategy.md), [008-context-design](../adr/008-context-design.md), [018-host-routing-and-rewrite](../adr/018-host-routing-and-rewrite.md)

---

## Overview

`credo.Context` is a request-scoped **struct** (not interface) that holds the `*Request`, `*Response`, matched `*Route`, a logger, and a key-value store. It exposes the underlying `http.Request`'s `context.Context` via `Context()`; it deliberately does **not** itself implement `context.Context`, because the struct is pooled and reused across requests.

In addition to request/response accessors, Context tracks rewrite lifecycle:
the original client path, internal re-dispatch requests (`ctx.Rewrite()`),
and whether the current dispatch round is about to rewrite.

Input and output concerns are separated into `Request` and `Response` structs:

- **Request** — wraps `*http.Request`, provides route params, query shortcuts, and `Bind*` methods ("parse, don't validate").
- **Response** — wraps `http.ResponseWriter`, provides `JSON`, `Text`, `HTML`, `XML`, `NoContent`, `Redirect`, `Blob`, `Stream` helpers.

Defined in the **root package** (not a sub-package) to avoid collision with stdlib `context`.

---

## API Surface

### Context Struct

```go
type Context struct {
    // unexported fields — access via methods only (pool safety)
}

// Accessors
func (c *Context) Request() *Request
func (c *Context) Response() *Response
func (c *Context) Route() *Route            // nil when no route matched (404/405)
func (c *Context) HasRoute() bool           // true when a route matched; guard for Route() calls
func (c *Context) Logger() *slog.Logger    // c.logger → app.logger → slog.Default()
func (c *Context) SetLogger(*slog.Logger)  // replace logger wholesale; derive from Logger() or enrichment (request_id) is silently lost
func (c *Context) AddLogAttrs(args ...any) // add attrs, deriving from Logger() — preferred over SetLogger for enrichment
func (c *Context) HasRequestLogger() bool  // true once a request-scoped logger was set; does not inspect its attributes
func (c *Context) RequestID() string       // request ID set by built-in or middleware RequestID
func (c *Context) OriginalPath() string    // client path before any rewriting
func (c *Context) Rewrite(path string) error
func (c *Context) IsRewriting() bool

// Request-scoped store
func (c *Context) Set(key string, val any)
func (c *Context) Get(key string) any

// Request context access — the request's context.Context
func (c *Context) Context() context.Context
```

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
    http.ResponseWriter // swappable by writer-wrapping middleware (e.g., compress)
    // status, size, committed — framework-owned tracking state (unexported)
}

// State accessors (read-only — the framework owns the tracking state)
func (r *Response) Status() int     // status code written; 0 until committed
func (r *Response) Size() int64     // bytes written to the response body
func (r *Response) Committed() bool // true after WriteHeader has been called

// Existing methods
func (r *Response) WriteHeader(code int)
func (r *Response) Write(b []byte) (int, error)
func (r *Response) Flush()
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

---

## Rewrite Helpers

### OriginalPath

`OriginalPath()` returns the path captured in `reset()` before any middleware,
pre-dispatch rewrite, or handler-level re-dispatch runs.

```go
func (c *Context) OriginalPath() string
```

The value is immutable for the lifetime of the request. When the final served
path differs, built-in and configurable access logging include a
`path_original` attribute.

### Rewrite

`Rewrite(path)` triggers an internal re-dispatch to a new path without sending
an HTTP redirect to the client.

```go
func (c *Context) Rewrite(path string) error
```

Rules:

- Call it as the last statement in a handler: `return ctx.Rewrite("/new")`.
- The matched host scope does not change. Re-dispatch stays within the host
  mux selected for the original request.
- Route params are cleared between dispatch rounds and rebuilt from the target
  route.
- If the response is already committed, `Rewrite` returns an error.
- Credo enforces a hard limit of 10 internal rewrites per request. Exceeding the
  limit fails the request with a 500.

### IsRewriting

`IsRewriting()` reports whether the current handler chain has requested an
internal rewrite that the dispatch loop will process next.

```go
func (c *Context) IsRewriting() bool
```

This is primarily useful in middleware with `after` logic that should skip
side effects when a handler forwards to another route.

### Params and Host Params

`Request.RouteParam(name)` returns a single parameter value ("" when absent)
and is the preferred accessor — parameters are stored as parallel
key/value slices, so the lookup is an allocation-free linear scan (routes
carry a handful of params at most). `Request.RouteParams()` returns all
params as one `map[string]string` — path params and, for host-scoped
routes, host params. The map is a read-only view materialized lazily on
first call (writes to it are not seen by `RouteParam`), owned by the
framework and recycled after the request completes; do not retain it.

```go
tenant := ctx.Request().RouteParam("tenant")
id := ctx.Request().RouteParam("id")
```

Host and path params share one namespace. Registering a host-scoped route whose
path params collide with host param names panics at registration time.

`Request.PathValue(name)` is a stdlib-shaped shadow over the embedded
`*http.Request` method: it resolves route params first and falls back to the
embedded request. Without it, `ctx.Request().PathValue("id")` would silently
return "" — the dispatcher deliberately does not populate stdlib path values
(an extra allocation per request for data `RouteParam` already serves). The
raw embedded request — as seen by stdlib handlers via `Mount` or middleware
via `WrapStdMiddleware` — still carries no path values.

### Scheme and RealIP

`Request.Scheme()` and `Request.RealIP()` expose client metadata derived from
the transport and, when configured, trusted reverse-proxy headers.

```go
scheme := ctx.Request().Scheme() // "http" or "https"
ip := ctx.Request().RealIP()     // IP-only string when parseable
```

Forwarded headers are default-deny. Unless the immediate peer `RemoteAddr` is
inside `server.trusted_proxies` or configured via `WithTrustedProxies`, Credo
ignores `X-Forwarded-For`, `X-Forwarded-Proto`, `X-Forwarded-Ssl`,
`Front-End-Https`, and `X-Real-IP`.

`RealIP()` walks `X-Forwarded-For` from right to left, skipping trusted proxy
hops and returning the first untrusted address. If no usable XFF value exists,
it falls back to `X-Real-IP`, then the direct peer address. The XFF walk is
limited to 32 hops as a DoS guard.

Both values are cached for the lifetime of the request and cleared when the
pooled context is reset.

---

## Bind Methods — "Parse, Don't Validate"

The `Bind*` methods live on the `Request` struct and implement Zod's "parse,
don't validate" philosophy in Go. Each method performs two steps atomically:

1. **Decode** — read data from the request source (body, query, headers)
2. **Validate** — if the target implements `Validatable`, call `Validate()`

In debug mode (`WithDebug()` or `server.debug: true`), a warning is logged
when the target does not implement `Validatable`.

```go
// Validatable interface — any struct can opt in
type Validatable interface {
    Validate() error
}
```

### BindBody

Reads request body. Content-Type determines decoder:

| Content-Type | Decoder | Status |
|---|---|---|
| `application/json` | `encoding/json` | **Implemented** |
| `application/xml` | `encoding/xml` | **Implemented** |
| `application/x-www-form-urlencoded` | Form decoder | **Implemented** |
| `multipart/form-data` | Multipart decoder | **Implemented** |

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
        return err // decode OR validation error → error pipeline → RFC 7807 (default)
    }
    // input is GUARANTEED valid here
    return ctx.Response().JSON(201, svc.CreateUser(input))
})
```

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

**Supported field types**: `string`, `bool`, all integer types (`int`,
`int8`–`int64`, `uint`–`uint64`), `float32`, `float64`, any type
implementing `encoding.TextUnmarshaler` (`time.Time`, `netip.Addr`,
custom types — takes precedence over native conversions, matching the
config decoder), slices of all of these (`[]string`, `[]int`,
`[]float64`, repeated params), and pointer variants of all scalar types
(`*string`, `*int`, `*time.Time`, etc.).
Pointer fields stay `nil` when the parameter is absent, enabling
nil-vs-zero-value distinction (e.g., PATCH partial updates, optional
filters). Embedded structs and pointer-to-embedded-structs are supported
recursively. The same type support applies to `form:"..."` tags used by
`BindBody` for form-urlencoded and multipart content types.

---

## Design Decisions

1. **Struct instead of interface** — Context was originally a 22-method
   interface. This caused interface bloat (every new helper broke the contract),
   made mocking difficult, and prevented future use of Go generic methods
   (which only work on concrete types). A struct with methods solves all three
   issues. Testing uses `testutil` helpers instead of interface mocking.
   See [ADR-008](../adr/008-context-design.md).

2. **Request/Response split** — Input (params, bind) and output (JSON, HTML)
   concerns are separated into `Request` and `Response` structs. The Context
   itself stays slim (a small method set plus the `Context()` accessor). New response formats
   or binding methods can be added without touching Context.

3. **`Text()` instead of `String()`** — The response helper for plain text
   is named `Text()` to avoid conflict with `fmt.Stringer`, which Response
   implements for debugging.

4. **Map-backed params + `RouteParam()` shortcut** — Parameters live in a
   single `map[string]string`, which is simpler and eliminates Echo's
   parallel-slices pattern (`ParamNames()`/`ParamValues()`). `RouteParam(name)`
   is the preferred single-value accessor (named for symmetry with
   `QueryParam`/`RouteParams`, unlike Echo's bare `Param`); `RouteParams()`
   exposes the full framework-owned map (recycled with the request — not safe
   to retain). Host-scoped routes reuse the same map for host params and path
   params.

5. **Proxy-derived metadata belongs on Request** — `Scheme()` and `RealIP()`
   are request-derived values, not response or routing state. They use the app's
   centralized trusted proxy list so Secure, RateLimit, access logs, and user
   code agree on client metadata.

6. **`FormValue()` removed** — `BindBody` covers form decoding. Direct
   `FormValue` access was removed as it bypasses the "parse, don't validate"
   pattern.

7. **`SetRequest()` removed** — Rare use case (middleware context modification).
   Middleware that needs to modify the request context can use
   `ctx.Request().Request = r.WithContext(newCtx)` via the embedded field.

8. **`Bind*` naming preserved** — While "Parse" better communicates the
   decode+validate philosophy, Go ecosystem universally uses "Bind" (Echo,
   Gin, Fiber). Using `BindBody`/`BindQuery` reduces friction.

9. **Auto-validation via `Validatable`** — If the target struct implements
   `Validate() error`, `Bind*` calls it automatically after decoding.
   No separate validation step needed in handlers.

10. **App back-reference** — Context holds an unexported `app *App` field set
   during pool construction (never changes, not touched by `reset()`). This
   enables the internal error handling pipeline to access `app.i18nBundle`
   for i18n translation without exposing the App publicly on Context.
   See [ADR-013](../adr/013-internationalization.md).

11. **Reflection boundaries** — `BindBody` uses `encoding/json` internally
    (stdlib reflection). `BindQuery` will use reflection for struct tag
    reading (`query:"name"`). This is standard Go practice and distinct
    from the "generics over reflection" principle, which applies to DI
    resolution and validation rule execution hot paths.

12. **Original path belongs on Context, not Request** — The original client
    path is framework lifecycle state, not raw HTTP state. Storing it on
    Context keeps it tied to dispatch/rewrite behavior and avoids mutating the
    wrapped `*http.Request`.

13. **Internal rewrite is a handler-level forward** — `ctx.Rewrite()` keeps the
    ergonomic `return ctx.Rewrite("/new")` API while dispatch owns the actual
    re-dispatch loop. Built-in/global middleware run once; group/route
    middleware re-run for the target route.

---

## Pooling

Context structs are pooled via `sync.Pool` (managed in root package `pool.go`).
Each request:

1. Gets a Context from the pool (contains pre-allocated *Request and *Response)
2. Resets it with the new `ResponseWriter` and `*http.Request`
3. Runs the handler chain
4. Returns the Context to the pool

This minimizes allocations on the hot path.

---

## File Layout

```
(root package)
├── context.go       Context struct, rewrite helpers, Context() accessor
├── request.go       Request struct (embeds *http.Request, Bind*, RouteParams, Scheme/RealIP)
├── response.go      Response struct (wraps http.ResponseWriter, JSON/Text/HTML helpers)
├── context_test.go  Context method tests

├── pool.go          Generic sync.Pool wrapper for Context reuse
```
