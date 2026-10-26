# ADR-008: Context Design

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** ADR-007 **Related:** ADR-009, ADR-011, ADR-012, ADR-013, ADR-021 **Specification:** [Context spec](../specs/context.md) **Guide:** [Getting started](../guides/getting-started.md#request-and-response), [Proxy trust](../guides/proxy-trust.md)

## Context

Every web framework needs a request-scoped object that provides access to the HTTP request, response writer, route information, and utilities. The design of this object significantly impacts API ergonomics and performance.

Go's stdlib uses the `(http.ResponseWriter, *http.Request)` pair. Echo uses a `Context` interface. Gin uses a `*gin.Context` struct. Each approach has trade-offs between flexibility, performance, and type safety.

The object exists once per request, so its allocation cost is paid on every request, and once it is reused a reference retained past its request observes a later one. Request input that is decoded but not validated leaves every handler to remember a second step. Client metadata derived from proxy headers drifts when each middleware derives it on its own.

## Decision

### Context as Struct in Root Package

`credo.Context` is a **struct** (not an interface) living in the root package. It is the single request-scoped object passed to all handlers. Its fields are unexported; the [Context spec](../specs/context.md#api-surface) lists its methods.

**Why struct, not interface:**

- Go generic methods need a concrete receiver: the typed user accessors (`ctx.SetUser`, `ctx.GetUser[T]`, `ctx.RequireUser[T]`, [ADR-012](012-authentication-and-authorization.md)) exist because Context is a struct.
- A new helper is a new method, not a change to an interface that every implementation and mock must follow.
- No user implementation can break framework invariants (pool reset, commit tracking, rewrite state).
- Calls are static, with no interface dispatch on every method call.

**Why root package:**

- Avoids `context/` name collision with stdlib
- Handler signature `func(*credo.Context) error` reads naturally
- No import cycle between router and context

### Request / Response Split

Input and output live on separate objects reached through `ctx.Request()` and `ctx.Response()`; Context itself stays a thin carrier of the matched route, the logger, the request-scoped store and the rewrite state. `Request` embeds `*http.Request` and adds route and query parameters, binding, and proxy-derived client metadata. `Response` wraps `http.ResponseWriter` with framework-owned commit tracking and the body helpers. A new response format or binding method lands on the side it belongs to without touching Context. The [Context spec](../specs/context.md#api-surface) holds both method sets.

`RouteParam(name)` is the preferred single-value accessor, named for symmetry with `QueryParam` and `RouteParams` rather than Echo's bare `Param`. Parameters are stored as parallel key/value slices, so `RouteParam` is an allocation-free scan; the slices are not exposed (no Echo-style `ParamNames`/`ParamValues`), and `RouteParams` materializes a read-only map only when it is called.

The plain-text helper is `Text`, not `String`, because `Response` implements `fmt.Stringer` for debugging. The original client path lives on Context, not Request: it is dispatch and rewrite state, and keeping it on Context avoids mutating the wrapped `*http.Request`.

### Parse, Don't Validate

`Request.BindBody()` and `Request.BindQuery()` decode and, when the target implements `validation.Validatable`, validate in the same call, so a handler holds either a decoded and validated value or an error for the pipeline. The [Context spec](../specs/context.md#bind-methods--parse-dont-validate) has the content types, the decoders and an example.

Binding uses reflection: `encoding/json/v2` for JSON bodies and cached struct-tag reading (`query`, `form`) for query and form values. This is the boundary of the "generics over reflection" principle, which covers DI resolution and rule execution, not decoding.

Request bodies are never decompressed implicitly: a non-identity `Content-Encoding` is rejected with 415 `unsupported_content_encoding` unless the application opted into `app.UseDecompress`, whose decompressed-size bound is what makes accepting compressed uploads safe.

JSON bodies are decoded under strict semantics: exactly one JSON value per body, and no duplicate object members, case-variant repeats included. These are the payloads two parsers can legitimately read differently, so a security layer and the application could act on different values. Member matching keeps v1's case-insensitive behavior for client compatibility. A decode failure is a typed `*credo.BindError` that the error pipeline renders as a 400 [default error envelope](009-handler-and-error-handling.md#default-error-envelope) with top-level `bind_failed` and the exact reason nested, so parse errors are as structured as validation errors, completing the "parse, don't validate" contract. The Context spec lists the [strictness rules](../specs/context.md#strict-json-bodies) and the [reason catalog](../specs/context.md#decode-error-model).

Unknown members are **ignored by default** and **rejected under an app-wide opt-in** (`credo.WithStrictBodies()` / `server.strict_bodies`, reason `unknown_field`). The lenient default stands because "must-ignore unknown fields" is the API-evolution norm for public and versioned APIs: clients deployed independently of the server must keep working when the server learns new fields, and the Go ecosystem default is lenient. The opt-in exists for the equally common shape where client and server ship together (one repository, one deploy): there an unknown member is almost always a misspelling or a stale client, and silently dropping it hides the bug until data goes missing. A Route-Meta override is deferred until a concrete need for mixed posture appears. Credo's always-on strictness still targets ambiguity (duplicates, trailing data) — payloads where two parsers could legitimately disagree; strict-unknown is a policy choice and therefore configurable, with the default favouring compatibility. Flipping the default at v1 is not planned.

### Proxy-Derived Client Metadata

`Request.Scheme()` and `Request.RealIP()` centralize reverse-proxy metadata. They are request-derived values, not response or routing state, and Request is the one place that Secure, RateLimit, the access log and application code all read, so they cannot disagree about the client. Forwarded headers are default-deny: they are considered only when the immediate peer `RemoteAddr` is in the app's trusted proxy list. Both values are cached for the request lifetime. The [Context spec](../specs/context.md#scheme-and-realip) gives the header precedence and the chain walk; the [proxy trust guide](../guides/proxy-trust.md) covers the deployment side.

### Logger

`ctx.Logger()` returns a request-scoped `*slog.Logger`. The request ID feature (`app.UseRequestID`) and `ctx.AddLogAttrs` enrich it. Fallback chain: request logger → app logger (from `WithLogger`) → the framework default logger (a text handler on stderr).

### Request Context Access

`credo.Context` deliberately does **not** implement `context.Context`. Because it is recycled through a `sync.Pool` (see below), a `*credo.Context` retained as a long-lived `context.Context` — handed to a goroutine or stored — would observe a _later_ request's state once the original request completed. To make that misuse impossible at the type level, `ctx.Context()` returns the underlying request's context explicitly.

Pass `ctx.Context()` to functions expecting `context.Context` (database queries and transactions, downstream requests). The returned context is canceled when the request completes; for work that must outlive the request, detach it with `context.WithoutCancel(ctx.Context())`.

### sync.Pool Recycling

Context instances are pooled per App, which removes the per-request allocation of the Context and its Request and Response. `reset()` clears the request state between requests. The Context, Request and Response keep a back-reference to their App, set once when the pool builds them and never cleared; through it request-time code reaches the installed i18n feature, the renderers, the JSON profile and the trusted proxies without the App being exposed on Context.

### Key-Value Store

`ctx.Set(key, val)` / `ctx.Get(key)` provides a simple request-scoped store for middleware-to-handler communication (the request ID feature stores its ID there).

## Rejected Alternatives

| Alternative | Reason |
| --- | --- |
| Context as interface | Generic methods need a concrete receiver; every new helper changes the interface; user implementations can break framework invariants |
| Separate `context/` package | Collides with stdlib `context` |
| `FormValue()` on Context | No shortcut that bypasses "parse, don't validate": `Bind*` is the supported path, while the embedded `*http.Request` still exposes raw form access |
| `SetRequest()` on Context | Breaks pool invariants, encourages unsafe patterns; middleware that must change the request context assigns the embedded field (`ctx.Request().Request = r.WithContext(newCtx)`) |
| `ParseBody`/`ParseQuery` names | `Parse` describes decode-and-validate better, but the ecosystem name `Bind*` (Echo, Gin, Fiber) reduces friction |
| Per-call `BindBody(target, opts...)` strict switch | Mixed posture inside one service makes the contract hard to reason about; JSON-only knob with no `BindQuery`/form counterpart — strict bodies is app-wide (`WithStrictBodies`) |

## Consequences

**Positive:**

- Pooling removes the per-request Context allocations
- Request/Response split provides clean API surface
- A `Validatable` target is validated in the same call that decodes it
- Centralized `Scheme()` and `RealIP()` prevent middleware-specific proxy trust drift
- Context() exposes the request's context.Context for stdlib interop without implementing it on the pooled struct
- Logger accessor enables structured, request-scoped logging

**Negative:**

- Struct cannot be extended by users (mitigate: `Set`/`Get` store)
- sync.Pool requires careful `reset()` to prevent data leaks
- `app` back-reference creates coupling (but needed for ErrorRenderer, i18n)
