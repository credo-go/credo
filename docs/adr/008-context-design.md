# ADR-008: Context Design

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** ADR-007

## Context

Every web framework needs a request-scoped object that provides access to the HTTP request, response writer, route information, and utilities. The design of this object significantly impacts API ergonomics and performance.

Go's stdlib uses the `(http.ResponseWriter, *http.Request)` pair. Echo uses a `Context` interface. Gin uses a `*gin.Context` struct. Each approach has trade-offs between flexibility, performance, and type safety.

## Decision

### Context as Struct in Root Package

`credo.Context` is a **struct** (not an interface) living in the root package. It is the single request-scoped object passed to all handlers:

```go
type Context struct {
    app      *App
    request  *Request
    response *Response
    route    *Route
    logger   *slog.Logger
    extra    map[string]any
}
```

**Why struct, not interface:**

- No vtable overhead on every method call
- Concrete type enables `sync.Pool` recycling
- Prevents user implementations that break framework invariants
- Compile-time field access instead of runtime method dispatch

**Why root package:**

- Avoids `context/` name collision with stdlib
- Handler signature `func(*credo.Context) error` reads naturally
- No import cycle between router and context

### Request / Response Split

Context exposes request and response through separate accessor objects:

```go
ctx.Request()   *Request    // wraps *http.Request
ctx.Response()  *Response   // wraps http.ResponseWriter
ctx.Route()     *Route      // matched route
```

**Request** wraps `*http.Request` and adds:

- `RouteParam(name) string` — single path/host parameter; preferred accessor (named for symmetry with `QueryParam` and `RouteParams`)
- `RouteParams() map[string]string` — all parameters; framework-owned map, recycled with the request — not safe to retain
- `QueryParam(name) string` — single query parameter
- `Scheme() string` — original client scheme with trusted proxy support
- `RealIP() string` — original client IP with trusted proxy support
- `BindBody(dst)` — decode + validate request body
- `BindQuery(dst)` — decode + validate query string

**Response** wraps `http.ResponseWriter` and adds:

- `JSON(code, v)` — JSON response
- `XML(code, v)` — XML response
- `HTML(code, html)` — HTML response
- `Text(code, s)` — plain text response
- `NoContent(code)` — empty response
- `Redirect(code, url)` — redirect response
- `Blob(code, contentType, b)` — binary response
- `Stream(code, contentType, r)` — streaming response
- `Status()`, `Size()`, `Committed()` — read-only accessors for the framework-owned tracking state (status code, body bytes, header-written flag)

### Parse, Don't Validate

`Request.BindBody()` and `Request.BindQuery()` combine decoding and validation in a single step. If the target struct implements `Validatable`, validation runs automatically after decode:

```go
type CreateUserRequest struct {
    Name  string `json:"name"`
    Email string `json:"email"`
}

func (r *CreateUserRequest) Validate() error {
    return validation.ValidateStruct(r,
        validation.Field(&r.Name, validation.Required[string]()),
        validation.Field(&r.Email, validation.Required[string](), validation.Email()),
    )
}

func createUser(ctx *credo.Context) error {
    var req CreateUserRequest
    if err := ctx.Request().BindBody(&req); err != nil {
        return err  // decode OR validation error → error handler
    }
    // req is decoded AND validated
}
```

Content-Type dispatch for `BindBody`: JSON (default), XML, form-urlencoded, multipart (including file upload binding).

JSON bodies are decoded with `encoding/json/v2` under strict semantics: exactly one JSON value is accepted (trailing content rejected) and duplicate object members are rejected — including case-variant repeats, since member matching keeps v1's case-insensitive behavior via `MatchCaseInsensitiveNames` for client compatibility. Decode failures return a typed `*credo.BindError` (reason, field path, expected type, byte offset) that the error pipeline renders as a 400 RFC 7807 response mirroring the validation `errors[]` shape — parse errors are as structured as validation errors, completing the "parse, don't validate" contract. See the [Context spec](../specs/context.md) for the reason catalog.

Unknown members are **ignored by default** and **rejected under an app-wide opt-in** (`credo.WithStrictBodies()` / `server.strict_bodies`, reason `unknown_field`). The lenient default stands because "must-ignore unknown fields" is the API-evolution norm for public and versioned APIs: clients deployed independently of the server must keep working when the server learns new fields, and the Go ecosystem default is lenient. The opt-in exists for the equally common shape where client and server ship together (one repository, one deploy): there an unknown member is almost always a misspelling or a stale client, and silently dropping it hides the bug until data goes missing. Strict mode is a single posture per application rather than a per-call `BindBody` option — mixed posture within one service makes the contract hard to reason about, and `BindQuery`/form binding have no equivalent switch, so a per-call knob would apply to JSON only. A Route-Meta override is deferred until a concrete need for mixed posture appears. Credo's always-on strictness still targets ambiguity (duplicates, trailing data) — payloads where two parsers could legitimately disagree; strict-unknown is a policy choice and therefore configurable, with the default favouring compatibility. Flipping the default at v1 is not planned.

### Proxy-Derived Client Metadata

`Request.Scheme()` and `Request.RealIP()` centralize reverse-proxy metadata. Forwarded headers are default-deny: `X-Forwarded-For`, `X-Forwarded-Proto`, `X-Forwarded-Ssl`, `Front-End-Https`, and `X-Real-IP` are considered only when the immediate peer `RemoteAddr` is in the app's trusted proxy CIDR list.

`RealIP()` walks `X-Forwarded-For` right-to-left, skipping trusted proxy hops and returning the first untrusted address. Both helpers cache their result for the request lifetime and reset with the pooled `Context`.

### Logger

`ctx.Logger()` returns a request-scoped `*slog.Logger`. Middleware (e.g., RequestID) enriches this logger with request attributes. Fallback chain: request logger → app logger (from `WithLogger`) → the framework default logger (a text handler on stderr).

### Request Context Access

`credo.Context` deliberately does **not** implement `context.Context`. Because it is recycled through a `sync.Pool` (see below), a `*credo.Context` retained as a long-lived `context.Context` — handed to a goroutine or stored — would observe a _later_ request's state once the original request completed. To make that misuse impossible at the type level, the Context exposes the underlying request's context explicitly:

```go
func (c *Context) Context() context.Context { return c.request.Context() }
```

Pass `ctx.Context()` to functions expecting `context.Context` (database queries, auth accessors, downstream requests). The returned context is canceled when the request completes; for work that must outlive the request, detach it with `context.WithoutCancel(ctx.Context())`.

### sync.Pool Recycling

Context instances are pooled for zero-allocation request handling. `reset()` clears all fields between requests. The `app` back-reference is set once in the pool constructor and never cleared.

### Key-Value Store

`ctx.Set(key, val)` / `ctx.Get(key)` provides a simple request-scoped store for middleware-to-handler communication (e.g., locale detection).

### Rejected Alternatives

| Alternative | Reason |
| --- | --- |
| Context as interface | vtable overhead, can't pool, user impls break invariants |
| Separate `context/` package | stdlib name collision |
| `FormValue()` on Context | Bypasses "parse, don't validate" — raw access encourages skipping validation |
| `SetRequest()` on Context | Breaks pool invariants, encourages unsafe patterns |
| Per-call `BindBody(target, opts...)` strict switch | Mixed posture inside one service; JSON-only knob with no `BindQuery`/form counterpart — strict bodies is app-wide (`WithStrictBodies`) |

## Consequences

**Positive:**

- Struct enables sync.Pool — zero-allocation request handling
- Request/Response split provides clean API surface
- Parse-don't-validate ensures data is always validated
- Centralized `Scheme()` and `RealIP()` prevent middleware-specific proxy trust drift
- Context() exposes the request's context.Context for stdlib interop without implementing it on the pooled struct
- Logger accessor enables structured, request-scoped logging

**Negative:**

- Struct cannot be extended by users (mitigate: `Set`/`Get` store)
- sync.Pool requires careful `reset()` to prevent data leaks
- `app` back-reference creates coupling (but needed for ErrorRenderer, i18n)
