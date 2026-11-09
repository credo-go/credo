# ADR-008: Context Design

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** ADR-007 **Related:** ADR-003, ADR-009, ADR-010, ADR-011, ADR-012, ADR-013, ADR-018, ADR-021 **Specification:** [Context spec](../specs/context.md) **Guide:** [Getting started](../guides/getting-started.md#request-and-response), [Proxy trust](../guides/proxy-trust.md)

## Context

Every web framework needs a request-scoped object that provides access to the HTTP request, response writer, route information, and utilities. The design of this object significantly impacts API ergonomics and performance.

Go's stdlib uses the `(http.ResponseWriter, *http.Request)` pair. Echo uses a `Context` interface. Gin uses a `*gin.Context` struct. Each approach has trade-offs between flexibility, performance, and type safety.

The object exists once per request, so its allocation cost is paid on every request, and once it is reused a reference retained past its request observes a later one.

The request's values and its `context.Context` must be one thing. A deadline a timeout middleware sets or a span a stdlib middleware adds changes the request's context; a value a middleware records — a tenant, the user — must reach a repository given that context and a mounted stdlib handler, and must survive the end of the layer that set it, or the layers outside it lose the user. Values copied into a request per write cost an allocation each, and a key that binds no type to its name lets two packages share one value by accident and makes every read a type assertion. Request input that is decoded but not validated leaves every handler to remember a second step. Client metadata derived from proxy headers drifts when each middleware derives it on its own.

## Decision

### Context as Struct in Root Package

`credo.Context` is a **struct** (not an interface) living in the root package. It is the single request-scoped object passed to all handlers. Its fields are unexported; the [Context spec](../specs/context.md#api-surface) lists its methods.

**Why struct, not interface:**

- Go generic methods need a concrete receiver: the typed user accessors (`c.SetUser`, `c.GetUser[T]`, `c.RequireUser[T]`, [ADR-012](012-authentication-and-authorization.md)) exist because Context is a struct.
- A new helper is a new method, not a change to an interface that every implementation and mock must follow.
- No user implementation can break framework invariants (pool reset, commit tracking, rewrite state).
- Calls are static, with no interface dispatch on every method call.

**Why root package:**

- Avoids `context/` name collision with stdlib
- Handler signature `func(*credo.Context) error` reads naturally
- No import cycle between router and context

### Request / Response Split

Input and output live on separate objects reached through `c.Request()` and `c.Response()`; Context itself stays a thin carrier of the matched route, the logger, the request's context head and the rewrite state. `Request` embeds `*http.Request` and adds route and query parameters, binding, and proxy-derived client metadata. `Response` wraps `http.ResponseWriter` with framework-owned commit tracking and the body helpers. A new response format or binding method lands on the side it belongs to without touching Context. The [Context spec](../specs/context.md#api-surface) holds both method sets.

`RouteParam(name)` is the preferred single-value accessor, named for symmetry with `QueryParam` and `RouteParams` rather than Echo's bare `Param`. Parameters are stored as parallel key/value slices, so `RouteParam` is an allocation-free scan; the slices are not exposed (no Echo-style `ParamNames`/`ParamValues`), and `RouteParams` materializes a read-only map only when it is called.

The plain-text helper is `Text`, not `String`, because `Response` implements `fmt.Stringer` for debugging. The original client path lives on Context, not Request: it is dispatch and rewrite state, and keeping it on Context avoids mutating the wrapped `*http.Request`.

### Route Parameters Are Not Stdlib Path Values

The dispatcher does not set the standard request's path values for a route. Credo code reads parameters through `Request.RouteParam` and `Request.PathValue`, the stdlib-shaped shadow that resolves route parameters first and falls back to the embedded request, so stdlib code handed the embedded request sees none of them. A handler mounted under a parametric prefix keeps receiving the prefix's parameters as path values: the mount hands it a clone of the request whose path values hold them ([router spec](../specs/router.md#sub-router-mounting)).

Rejected: populating them on every request, which would allocate a map per request outside a `ServeMux` pattern and share that map between a request and its `WithContext` copy, so that a value set on one shows through the other.

### Parse, Don't Validate

`Request.BindBody()` and `Request.BindQuery()` decode and, when the target implements `validation.Validatable`, validate in the same call, so a handler holds either a decoded and validated value or an error for the pipeline. The [Context spec](../specs/context.md#bind-methods--parse-dont-validate) has the content types, the decoders and an example.

Binding uses reflection: `encoding/json/v2` for JSON bodies and cached struct-tag reading (`query`, `form`) for query and form values. This is the boundary of the "generics over reflection" principle, which covers DI resolution and rule execution, not decoding.

Request bodies are never decompressed implicitly: a non-identity `Content-Encoding` is rejected with 415 `unsupported_content_encoding` unless the application opted into `app.UseDecompress`, whose decompressed-size bound is what makes accepting compressed uploads safe.

JSON bodies are decoded under strict semantics: exactly one JSON value per body, and no duplicate object members, case-variant repeats included. These are the payloads two parsers can legitimately read differently, so a security layer and the application could act on different values. Member matching keeps v1's case-insensitive behavior for client compatibility. A decode failure is a typed `*credo.BindError` that the error pipeline renders as a 400 [default error envelope](009-handler-and-error-handling.md#default-error-envelope) with top-level `bind_failed` and the exact reason nested, so parse errors are as structured as validation errors, completing the "parse, don't validate" contract. The Context spec lists the [strictness rules](../specs/context.md#strict-json-bodies) and the [reason catalog](../specs/context.md#decode-error-model).

Unknown members are **ignored by default** and **rejected under an app-wide opt-in** (`credo.WithStrictBodies()` / `server.strict_bodies`, reason `unknown_field`). The lenient default stands because "must-ignore unknown fields" is the API-evolution norm for public and versioned APIs: clients deployed independently of the server must keep working when the server learns new fields, and the Go ecosystem default is lenient. The opt-in exists for the equally common shape where client and server ship together (one repository, one deploy): there an unknown member is almost always a misspelling or a stale client, and silently dropping it hides the bug until data goes missing. A Route-Meta override is deferred until a concrete need for mixed posture appears. Credo's always-on strictness still targets ambiguity (duplicates, trailing data) — payloads where two parsers could legitimately disagree; strict-unknown is a policy choice and therefore configurable, with the default favouring compatibility. Flipping the default at v1 is not planned.

### Proxy-Derived Client Metadata

`Request.Scheme()` and `Request.RealIP()` centralize reverse-proxy metadata. They are request-derived values, not response or routing state, and Request is the one place that Secure, RateLimit, the access log and application code all read, so they cannot disagree about the client. Forwarded headers are default-deny: they are considered only when the immediate peer `RemoteAddr` is in the app's trusted proxy list. Both values are cached for the request lifetime. The [Context spec](../specs/context.md#scheme-and-realip) gives the header precedence and the chain walk; the [proxy trust guide](../guides/proxy-trust.md) covers the deployment side.

### Logger

`c.Logger()` returns a request-scoped `*slog.Logger`. The request ID feature (`app.UseRequestID`) and `c.AddLogAttrs` enrich it. Fallback chain: request logger → app logger (from `WithLogger`) → the framework default logger (a text handler on stderr).

### Request Context Access

> **Current until v0.26.0 (R10):** the next paragraph gives the pool as the reason the Context does not implement `context.Context`.

`credo.Context` deliberately does **not** implement `context.Context`. Because it is recycled through a `sync.Pool` (see below), a `*credo.Context` retained as a long-lived `context.Context` — handed to a goroutine or stored — would observe a _later_ request's state once the original request completed. To make that misuse impossible at the type level, `c.Context()` returns the underlying request's context explicitly.

> **Pending v0.26.0 (R10):** the next paragraph replaces the paragraph above when R10 ships; until then the Context is pooled.

`credo.Context` deliberately does **not** implement `context.Context`. Its head moves as scopes open and return ([below](#one-context-head-and-typed-keys)), so a `*Context` held as a `context.Context` — handed to a goroutine or stored — would not be one fixed context, and a `*Context` is not safe for concurrent use. `c.Context()` returns the head as it stands: a fixed node, whose values do not change after it is handed out.

Pass `c.Context()` to functions expecting `context.Context` (database queries and transactions, downstream requests). The returned context is canceled when the request completes; for work that must outlive the request, detach it with `context.WithoutCancel(c.Context())`.

### One Context Head and Typed Keys

The Context holds the request's current `context.Context` as its head. `c.Context()` and `c.Request().Context()` return it. Request values are typed keys over the head:

```go
var Tenant = credo.NewKey[*TenantInfo]("tenant")

c.Set(Tenant, t)       // a value of another type does not compile
t, ok := c.Get(Tenant) // t is a *TenantInfo
```

`c.Set` replaces the head with a `context.WithValue` node over it, so no request is copied for a value and no custom `Value` implementation exists; stdlib code that holds only a `context.Context` reads a value with the standard `Value` and its key, which is the context key, and Credo adds no reader of its own. One key type, `Key[T]`, serves request values and route meta ([ADR-007](007-router-and-routing.md#route-meta)). Its name is for diagnostics; identity is the key value, so the keys of two packages never collide: two `NewKey` calls with one name are two keys, where two message keys with one text are one catalog key ([ADR-009](009-handler-and-error-handling.md)). A zero `Key[T]`, declared without `NewKey`, panics in every method that takes a key, since all zero keys of one type would be one context key. The user accessors stay as shortcuts over a generic key whose type stays unexported, so the user has no `context.Context` accessor ([ADR-012](012-authentication-and-authorization.md#backing-store)).

**Scopes.** A layer that derives a context derives it from the head and makes the result the head of the inner chain, so `middleware.Timeout`'s deadline and a span a wrapped stdlib middleware adds are visible through `c.Context()`. `c.Within(ctx, next)` is the one way a layer opens a scope: it makes `ctx` the head of the inner chain, keeps the embedded request in step with one copy per scope, runs `next`, and reverts when `next` returns or panics. The deadline or span ends with its scope, while the values set through `c.Set` inside it are applied again over the outer head, in the order of their `Set` calls, so a user set inside a scope before a panic is still the request's user for the error rendering, the observers and the access record: a user or a tenant is a fact about the request, not about one layer. The scope's end, which runs deferred, only marks those values pending; the first read of the head after it — `c.Context()`, `Std()`, a `Set`, a new scope — builds their nodes, so nested scopes that end in turn build them once, and `c.Get` and `GetUser` read a pending value from the record of `Set` calls without building any. A value the scope owns, such as a transaction or a span, goes into the context given to `Within` and ends with it. A key set again and again does not lengthen the chain without bound: a node built over the Context's own node for that key is built over its parent, and once the Context's nodes over a head number twice the keys set there, they are rebuilt from the record of `Set` calls; neither modifies a node, so a context handed out before keeps its values. The [Context spec](../specs/context.md#context-head-and-typed-keys) states the edges: a retrying middleware, a stdlib middleware that runs the next handler on another goroutine or passes on an unrelated context, and a mounted Credo App.

**The stdlib boundary.** `c.Request().Std()` is the one accessor for the standard request: the embedded `*http.Request` when it already carries the head, otherwise a copy with the head that replaces the embedded request. `WrapStdMiddleware` and `Mount` hand the request on through it and take the context of the request the stdlib code passes on as the head of the inner chain. `c.Request().Request` stays the embedded field; its `Context()` lags the head after a `Set`, and in debug mode the Context reports, once per route, an embedded request it did not install.

**Cost.** On a nested chain — a span a wrapped stdlib middleware adds, `middleware.Timeout`, an authentication middleware that sets the user, a handler that sets a value it reads from the request, and an access-log filter that reads the user after the response — the head costs no time against a request copy per value and a string store, which lose the user (793.9 ns against 797.6 ns, p=0.436, `-count=10 -benchmem`), allocates 17% fewer bytes (1.196 KiB against 1.447 KiB), and makes the same 15 allocations. A constant set through the generic `Set[T]` costs one allocation and about 1% time (+1.2%, p=0.019) that an untyped `Set` avoids by boxing the constant statically; a value computed per request is boxed either way.

Rejected:

- `WithContext` as the scope method's name: `With` names construction options in Credo, and the standard library's `WithContext` returns a copy where this runs a chain.
- Adopting a replaced request's context as the head, which no scope would ever revert.
- A mutable store behind `Value`, which does not conform to the `context` contract however it is documented.
- A chain of the Context's own, rooted once at the request's context, which a deadline or span added later never reaches.
- Values that end with their scope, which loses a user set inside `middleware.Timeout` or a wrapped stdlib middleware.
- Values in a table of the Context's own, which a repository given `c.Context()`, a mounted stdlib handler and a wrapped stdlib middleware would no longer reach.
- Keeping the embedded request in step on every `Set`, a request copy per value.
- Reverting a retry attempt's values when `next` returns an error, since the Context cannot tell a retry from a middleware that handles the error and goes on, and a user set before the failure is still the request's user.

Rejected for the keys:

- String keys with a type argument at each call, which bind no type to the name and let two packages share one value.
- Methods on the key beside the Context's, a second path to one value.
- Converting between `T` and `*T` on a mismatch, which loses the caller's writes and which a typed key cannot produce.
- A typed reader for a `context.Context`, a second path for code that holds the Context, and its user form, the accessor ADR-012 refuses.
- `NewValue` with `SetValue`/`GetValue`, whose constructor makes a key, not a value.

### One Allocation per Request

> **Current until v0.26.0 (R10):** the next paragraph describes the per-App pool.

Context instances are pooled per App, which removes the per-request allocation of the Context and its Request and Response. `reset()` clears the request state between requests. The Context, Request and Response keep a back-reference to their App, set once when the pool builds them and never cleared; through it request-time code reaches the installed i18n feature, the renderers, the JSON profile and the trusted proxies without the App being exposed on Context.

> **Pending v0.26.0 (R10):** not implemented yet; the Context, its Request and Response and the two parameter arrays are pooled per App and reset between requests, so a `*Context` kept past its request reads a later request's state.

The Context is not pooled. Each request allocates one Context, which embeds its `Request` and `Response` by value and keeps the first parameters in fixed arrays, folding the five pooled allocations — the Context, its `Request` and `Response`, and the two parameter arrays — into one. A `*Context` kept past its request reads that request's state, `GetUser` included, never another request's. What remains of the old caveats is stated where it applies: a `*Context` is not safe for concurrent use, since its head moves as scopes return, and its response cannot be written after the handler returns, as `net/http` forbids for every `ResponseWriter`. The Context, Request and Response keep a back-reference to their App, set when the Context is built; through it request-time code reaches the installed i18n feature, the renderers, the JSON profile and the trusted proxies without the App being exposed on Context.

**Measurement.** The pooled build is measured against the unpooled one, after the typed keys have settled the Context's layout, with a CPU-bound realistic handler such as a small JSON encode, at saturation — requests per second, CPU and allocations per request — and p99 at a fixed load; a handler that waits on I/O would hide the difference by construction. The measurement prices the change and does not decide it. Expected in advance: a cost well inside 5%, since one allocation is small against the ones `net/http` makes per request; a larger cost is answered in the layout, not by keeping the pool. The numbers are recorded here when the change ships.

Rejected: keeping the pool when the unpooled build is more than 5% worse, which would price a read of another tenant's user against throughput; and detecting use after release instead, which a counter inside the object cannot do, since a retained bare pointer reads the reused object's new counter.

**Problem.** A pooled Context is reused by a later request, so a `*Context` kept past its request — captured by a goroutine, stored by a hook — reads another request's state, another tenant's user included. The "must not be retained" caveats in the hook godocs warn about it but cannot prevent it, and nothing can detect it after the fact.

**Removes.** The per-App pool and the reset between requests, and with them the "must not be retained" caveats that the pool was the reason for; `Response.Reset` loses its one caller ([below](#pruned-context-apis)).

### Pruned Context APIs

> **Pending v0.26.0 (R14):** not implemented yet; `Response.Reset`, the `RouteContext` alias, `NewRequest`, `NewResponse`, `Context.Rewrite` and `IsRewriting` are public.

Three leftovers of earlier shapes and the Context's parallel APIs leave the public surface before v1 freezes it:

- `Response.Reset`: only the Context pool resets a response, the pool is gone, and an application has none to reset.
- The `RouteContext` alias of the internal radix type: no exported function returns one, so it only exposes the fields.
- `NewRequest` and `NewResponse`: a test that exercises a `Request` or a `Response` takes it from a Context built with `NewContext`, and a handler test runs through `testutil.NewApp` and `ServeHTTP` ([ADR-003](003-application-architecture.md)).
- `Context.Rewrite` and `IsRewriting`: the common cases have their own path, static's SPA mode and `middleware.Rewrite` before routing, while the post-match re-dispatch would freeze at v1 with its contract and adding it back later is additive ([ADR-018](018-host-routing-and-rewrite.md)).

Kept: `NewContext`, with which the hooks that take a `*Context` — a locale detector, a `Skipper`, an `ErrorRenderer`, an `Authenticator` — are unit-tested, and a `Request` or a `Response` with them; its godoc names what a Context built without an App lacks.

**Problem.** Each of these freezes at v1 with a contract nothing needs: a reset with no pool, a type alias that exposes internal fields, a second way to build a `Request` or a `Response` beside the Context that owns them, and a re-dispatch whose pending signal, rewrite limit and second run of group and route middleware no common case uses.

**Removes.** `Response.Reset` and `credo.RouteContext`, with no replacement; `credo.NewRequest(r)` and `credo.NewResponse(w)`, which become `credo.NewContext(w, r).Request()` and `.Response()`; `Context.Rewrite` and `IsRewriting`, which become a `middleware.Rewrite` rule, static's SPA mode, or a handler that calls the target's handler itself.

## Rejected Alternatives

| Alternative | Reason |
| --- | --- |
| Context as interface | Generic methods need a concrete receiver; every new helper changes the interface; user implementations can break framework invariants |
| Separate `context/` package | Collides with stdlib `context` |
| `FormValue()` on Context | No shortcut that bypasses "parse, don't validate": `Bind*` is the supported path, while the embedded `*http.Request` still exposes raw form access |
| `SetRequest()` on Context | Breaks pool invariants, encourages unsafe patterns; middleware that must change the request's context opens a scope with `c.Within(ctx, next)` |
| `ParseBody`/`ParseQuery` names | `Parse` describes decode-and-validate better, but the ecosystem name `Bind*` (Echo, Gin, Fiber) reduces friction |
| Per-call `BindBody(target, opts...)` strict switch | Mixed posture inside one service makes the contract hard to reason about; JSON-only knob with no `BindQuery`/form counterpart — strict bodies is app-wide (`WithStrictBodies`) |

## Consequences

> **Current until v0.26.0 (R10):** the bullets on pooling (the positive one on the removed Context allocations and the negative one on `reset()`), and the phrase "on the pooled struct"; with R10 the Context is one allocation per request and a `*Context` kept past its request reads only that request.

**Positive:**

- Pooling removes the per-request Context allocations
- Request/Response split provides clean API surface
- A `Validatable` target is validated in the same call that decodes it
- Centralized `Scheme()` and `RealIP()` prevent middleware-specific proxy trust drift
- Context() exposes the request's context.Context for stdlib interop without implementing it on the pooled struct
- Logger accessor enables structured, request-scoped logging

**Negative:**

- Struct cannot be extended by users (mitigate: typed keys with `c.Set`/`c.Get`)
- sync.Pool requires careful `reset()` to prevent data leaks
- `app` back-reference creates coupling (but needed for ErrorRenderer, i18n)
