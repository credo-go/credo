# ADR-012: Authentication & Authorization

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** ADR-007, ADR-008 **Related:** ADR-001, ADR-009, ADR-010, ADR-014, ADR-015 **Specification:** [Auth spec](../specs/auth.md) **Guide:** [Middleware guide](../guides/middleware.md#meta-driven-middleware)

## Context

Enterprise applications (ADR-001) require authentication (who is the user?) and authorization (what can they do?). Frameworks face a design choice: embed a specific user model into the context, or provide generic access patterns that work with any user type.

Embedding a `User` field into Context forces all applications to use the same user representation. Generic accessors allow each application to define its own user type while providing framework-level middleware integration.

The principal is a fact about the request: a user an authentication middleware sets inside a timeout or a wrapped stdlib middleware must still be the request's user for the layers outside it, the error rendering and the access record, and storing it should not copy the request. Authorization policy read from route meta must not fail open on a value of an unexpected type, and two packages must not share one policy value by choosing one name.

## Decision

### Generic User Accessors on Context

The authenticated principal is reached through generic `*credo.Context` methods — `SetUser` (type inferred), `GetUser[T]` and `RequireUser[T]` — a first-class part of the Context model, alongside `RequestID()`, `Route()`, and `Logger()`. They work with any user type: the application defines what a "user" is. The signatures and the error `RequireUser` returns are in the [auth spec](../specs/auth.md#user-accessors-generic-context-methods).

The `auth` package holds the authentication _strategy_ (`Authenticator[T]`, `Middleware[T]`, `ErrorFunc`) and the built-in authenticators, in a package of its own rather than in the general-purpose `middleware/` package; only the typed user slot — the primitive — lives on the root Context. The dependency direction is `auth → root` (no cycle).

### No Built-in User Type on Context

Context has no concrete `User` field or `User()` method — only the generic `SetUser`/`GetUser`/`RequireUser` methods. This avoids:

- Forcing a specific user model on all applications
- `any` type assertions (`c.User().(MyUser)`)
- Coupling the root package to any concrete auth implementation

### Backing Store

`SetUser` stores the user on the request's `context.Context` under a root-private, zero-size key type parameterized by the user type. No other package can name the key, so the slot is collision-proof, and the type parameter gives every user type its own slot, so distinct principals (a JWT end-user and an API-key service account) coexist in one request.

The backing store is an implementation detail: the only public access path is the Context methods. There is deliberately no `context.Context`-based accessor (no `UserFrom(ctx)`-style helper in the root or the `auth` package) — the principal is an HTTP-boundary feature reached through `*credo.Context`, not a value read from the `context.Context` chain (contrast store's typed transaction scopes, which are; [ADR-015](015-data-access.md)).

`SetUser`, `GetUser[T]` and `RequireUser[T]` are shortcuts over a generic typed key of the root package's own, one per user type, so the user is a value on the Context's head like any other ([ADR-008](008-context-design.md#one-context-head-and-typed-keys)): `SetUser` adds one node and copies no request, a user set inside a scope outlives it, and the request a wrapped stdlib middleware passes on still carries the user. The key stays unexported, so the user keeps no `context.Context` accessor: stdlib code can neither read nor set the principal, and the typed keys add no reader of their own.

> **Pending v0.26.0 (R12):** not implemented yet; observers do not exist, and the access log's `ResultFilter` receives the `*Context`.

An observer of the final response reads the user through `Exchange.GetUser[T]`, as the Context's `GetUser` does, over the same unexported key ([ADR-014](014-observability.md)); no observer reads it through `Value`.

### Authenticator Interface

Authentication strategies implement `Authenticator[T]`, a single-method interface parameterized by the user type: it takes the `*http.Request`, extracts credentials (header, cookie, query, TLS peer certificate) and returns the authenticated user or an error.

### Middleware Factory

`auth.Middleware[T](authenticator, onError)` turns an authenticator into a `credo.Middleware`: on success it stores the user with `c.SetUser`, on failure it returns the error to render. Applications need different failure responses (another status or code, a `WWW-Authenticate` challenge), so the choice is an `ErrorFunc` callback rather than a subtype; without one, or when it returns nil, the failure is a 401 whose cause is logged, never rendered. The authenticator is a convenience, not a requirement: custom middleware can call `c.SetUser` directly. The [auth spec](../specs/auth.md#errorfunc-authentication-failure-callback) states the failure contract.

### Built-in Authenticators

`auth` ships JWT, API-key and HTTP Basic authenticators on the same interface. Two decisions shape them:

- **Two-tier JWT configuration.** Credo does not hide golang-jwt, it integrates it — the same visibility policy as Bun in `store/sqldb` ([ADR-015](015-data-access.md)). The simple tier of `JWTConfig[T]` is fully Credo-typed for the common path, while the nested `Advanced JWTAdvanced[T]` struct keeps the library's full power one explicit step away. Structural nesting, rather than a doc-only convention, keeps the simple/advanced boundary visible in config literals and code review, so coupling to the library is easy to audit.
- **Constant-time comparison is shipped.** Basic and API-key validators are application code, and the natural `==` comparison leaks timing information. The framework ships `SecureCompare` (hash, then `subtle.ConstantTimeCompare`, hiding both content and length), and the validator godocs point to it. Stored passwords are verified with a dedicated password hash (bcrypt, argon2id) instead; JWT signature comparison is already constant-time inside golang-jwt.

### JWT in Its Own Package

> **Pending v0.26.0 (R11):** not implemented yet; the JWT authenticator lives in `auth` (`auth.JWTConfig[T]`, `JWTAdvanced[T]`, `JWTClaims`, `JWTAuthenticator[T]`, `NewJWTAuthenticator[T]`, `ErrJWTMissing`, `ErrJWTInvalid`), so every application that imports `auth` compiles golang-jwt.

The JWT authenticator lives in `auth/jwtauth`, so that only an application that verifies JWTs compiles golang-jwt. The name is not `jwt`, golang-jwt's package name, which code that configures `Advanced` imports beside it; like `net/http/httptest`, it joins its subject to its parent's name. `auth.Middleware`, the `Authenticator[T]` interface and the API-key and Basic authenticators stay in `auth`. The types drop the `JWT` prefix the package now names — `Config[T]`, `Advanced[T]`, `Claims`, `Authenticator[T]`, `NewAuthenticator[T]` — and the errors take golang-jwt's `ErrToken` stem, `ErrTokenMissing` and `ErrTokenInvalid`, with `jwtauth:` as the prefix of their text. The package stays in the root module: a module of its own waits for a consumer that cannot resolve a version conflict or must keep golang-jwt out of its module graph.

**Problem.** An application that authenticates with API keys or HTTP Basic imports `auth` and compiles golang-jwt, a dependency it never calls.

**Removes.** `auth.JWTConfig[T]`, `JWTAdvanced[T]`, `JWTClaims`, `JWTAuthenticator[T]`, `NewJWTAuthenticator[T]`, `ErrJWTMissing` and `ErrJWTInvalid`, which move to `auth/jwtauth` under the names above.

### RBAC via Route Meta

Authorization uses route metadata ([ADR-007](007-router-and-routing.md#route-meta)) — no separate RBAC system. The permission's key is a typed key, so its value type is checked at compile time:

```go
var Permission = credo.NewKey[string]("permission")

app.GET("/admin/users", listUsers).SetMeta(Permission, "admin.users.read")
```

An authorization middleware registered on the group or route reads the value with `c.Route().GetMeta(Permission)`, which returns the typed value and inherits through the route, its groups and the App, and checks it against the user; the [auth spec](../specs/auth.md#pattern-4-rbac-with-route-meta) shows one.

## Consequences

**Positive:**

- Type-safe via generics — no `any` assertions
- Middleware factory is reusable across auth strategies (JWT, API key, etc.)

**Negative:**

- Callers must handle `ErrUserMissing` when auth middleware did not run or the user type mismatches
- The principal is reachable only where a `*credo.Context` exists (handlers, Credo middleware); stdlib middleware adapted via `WrapStdMiddleware` cannot read or set it ([ADR-010](010-middleware-architecture.md#stdlib-adapter))
- `SetUser` is not allocation-free: it adds one `context.WithValue` node over the request's context
- By default every authentication failure is a 401, including an infrastructure failure an authenticator reports (a validator's database error, a JWKS outage from `Advanced.KeyFunc`); an application that wants a 5xx maps it in its `ErrorFunc`
- Permission values are application strings under a typed key: the compiler checks the key's type, not the permission names
