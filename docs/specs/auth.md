# Auth Spec

**Status:** Implemented (beta) **Package:** `auth/`; the user accessors are methods on the root `*credo.Context` **ADR:** [ADR-012](../adr/012-authentication-and-authorization.md) **Guide:** [Middleware guide](../guides/middleware.md#meta-driven-middleware)

## Overview

The `auth/` package provides authentication infrastructure for Credo applications. It does NOT provide a built-in `User` field on Context. Instead, the authenticated principal is reached through generic `*credo.Context` methods (`SetUser`/`GetUser`/`RequireUser`), and this package supplies the `Authenticator[T]` strategy interface, a middleware factory that populates them, and three built-in authenticators (JWT, API key, HTTP Basic). The JWT authenticator wraps `github.com/golang-jwt/jwt/v5` (pinned v5.3.1 in `go.mod`) and exposes it on purpose in its advanced tier. The rationale is in [ADR-012](../adr/012-authentication-and-authorization.md).

## API Surface

### User Accessors (generic Context methods)

The principal accessors are methods on `*credo.Context` (root package), not functions in `auth`:

```go
package credo

// SetUser stores the authenticated user on the request, keyed by type T.
// T is inferred, so the blessed call is ctx.SetUser(user).
func (c *Context) SetUser[T any](user T)

// GetUser returns the user stored under type T and whether one was present.
// The type argument is required: ctx.GetUser[*User]().
func (c *Context) GetUser[T any]() (T, bool)

// RequireUser is GetUser plus a handler-ready error: ErrUnauthorized
// wrapping ErrUserMissing when the user is absent.
func (c *Context) RequireUser[T any]() (T, error)

// ErrUserMissing is the cause RequireUser wraps; errors.Is reports it.
var ErrUserMissing error
```

**Internal implementation**: `SetUser` stores the user on the request's `context.Context` via `context.WithValue` with an unexported generic `userKey[T] struct{}` key (defined in the root package). The generic type parameter eliminates runtime type assertions at the call site, and because the key itself is parameterized by T, every user type gets its own slot — a JWT user and an API-key service account can coexist in one request. Storing the same type again replaces the previous value. Retrieve with the same T that was stored: a value stored under a concrete type is not visible through an interface type parameter. The Context struct has no field for the user; until `SetUser` is called nothing is stored, and each call adds a `context.WithValue` node and replaces the request with a shallow copy (`Request.WithContext`). The backing store is private; the only public access path is these methods (there is no `context.Context`-based accessor).

### Authenticator Interface (generic)

```go
// Authenticator validates a request and returns the authenticated user.
// T is the application's user type (e.g., *MyUser, Claims, etc.).
type Authenticator[T any] interface {
    Authenticate(r *http.Request) (T, error)
}
```

The interface is intentionally minimal — a single method that takes an `*http.Request` and returns the user or an error. This supports:

- JWT validation (read Authorization header, parse token)
- Session lookup (read cookie, query session store)
- API key check (read X-API-Key header, query database)
- mTLS (read TLS peer certificate)

### ErrorFunc (authentication failure callback)

```go
// ErrorFunc is called when authentication fails. It receives the
// error from the Authenticator and should return an appropriate
// HTTP error (or nil to use the default 401 response).
type ErrorFunc func(err error, ctx *credo.Context) error
```

An `ErrorFunc` returns the error to render; it may set response headers first, as `BasicErrorHandler` sets `WWW-Authenticate`. When `onError` is nil or returns nil, the middleware returns `credo.ErrUnauthorized` with the authenticator's error as its internal cause (logged, never rendered). An `ErrorFunc` must not write the response itself: the middleware still returns an error, and the error pipeline, finding the response committed, logs `credo: error after response committed` at Warn for every rejected request.

Every error from `Authenticate` takes this path, including an infrastructure failure: a database error that an API-key or Basic validator returns through its `err` result, or a JWKS outage that `Advanced.KeyFunc` returns. The built-in authenticators wrap it in their `…Invalid` sentinel, and the default `ErrUnauthorized` wraps that, so the client receives 401 — an explicit `HTTPError` wins classification even over a wrapped fault such as `store`'s unavailable kind ([ADR-009](../adr/009-handler-and-error-handling.md#an-explicit-status-wins)). An application that wants a 5xx for such a failure detects it in its `ErrorFunc` and returns an error with no `HTTPError` in its chain (the authenticator's error itself, for example), which the pipeline classifies like a handler error: a fault keeps its mapped status, a plain error is a 500.

### Middleware Factory

```go
// Middleware creates a credo.Middleware that authenticates requests
// using the given Authenticator. On success, the user is stored on
// the request via SetUser. On failure, onError is called
// (or ErrUnauthorized if onError is nil / returns nil).
// Panics if a is nil.
func Middleware[T any](a Authenticator[T], onError ErrorFunc) credo.Middleware
```

**Returns**: `credo.Middleware` (`func(credo.Handler) credo.Handler`). A nil authenticator is a wiring error and panics at construction.

### Built-in Authenticators

Each constructor returns an error when its required input is missing; each `Authenticate` returns the package's sentinels, wrapping the cause where there is one.

| Constructor | Credentials, in order | Defaults | Constructor errors | Sentinels |
| --- | --- | --- | --- | --- |
| `NewJWTAuthenticator[T](JWTConfig[T])` | `Header` (after `Prefix`), `Query`, `Cookie` | `Header` = `Authorization` when no source is set; `Prefix` = `Bearer`; `SigningMethod` = HS256 | no `SigningKey`, `SigningKeys` or `Advanced.KeyFunc`; both `ParseClaims` and `Advanced.ParseToken` | `ErrJWTMissing`, `ErrJWTInvalid` |
| `NewAPIKeyAuthenticator[T](APIKeyConfig[T])` | `Header` (after optional `Prefix`), `Query` | `Header` = `X-API-Key` when `Header` and `Query` are empty | no `Validator` | `ErrAPIKeyMissing`, `ErrAPIKeyInvalid` |
| `NewBasicAuthenticator[T](BasicConfig[T])` | `Authorization: Basic` | `Realm` = `Restricted` | no `Validator` | `ErrBasicCredentialsMissing`, `ErrBasicCredentialsInvalid` |

- A `Prefix` matches case-insensitively and must be followed by a space or tab; every value of a repeated header is tried, and surrounding whitespace is trimmed.
- The API-key and Basic validators (`APIKeyValidator[T]`, `BasicValidator[T]`) return `(user T, ok bool, err error)`: `ok == false` yields the `…Invalid` sentinel, and a non-nil `err` yields the sentinel wrapping `err`.
- `NewBasicAuthenticator` sends no challenge by itself: pass `BasicErrorHandler(a.Realm())` as the middleware's `ErrorFunc` to set `WWW-Authenticate: Basic realm="…"` (`BasicChallenge(realm)` builds the value; an empty realm is `Restricted`).
- `SecureCompare(x, y string) bool` compares a received credential with a known plaintext secret in constant time, hiding both content and length. Stored passwords are verified with a password hash (bcrypt, argon2id) instead.

### JWT Configuration (two tiers)

`JWTConfig[T]` separates a Credo-typed simple tier from a golang-jwt escape hatch:

- **Simple tier** — extraction (`Header`/`Prefix`/`Query`/`Cookie`), key material (`SigningMethod`/`SigningKey`/`SigningKeys`), registered-claim validation (`Issuer`, `Audience`, `Leeway`, `RequireExpiry`), and user extraction via `ParseClaims func(JWTClaims) (T, error)`. `JWTClaims` is a read-only accessor view: registered claims come back with proper Go types (`ExpiresAt() time.Time` — never the raw float64 that JSON decoding produces), and `Get`/`GetString` read custom claims from the default map representation.
- **Advanced tier** — `Advanced JWTAdvanced[T]` exposes golang-jwt primitives directly: `KeyFunc` (e.g. JWKS), `NewClaims` (typed claims structs; default `jwt.MapClaims`), `ParseToken func(*jwt.Token) (T, error)`, and `ParserOptions` (appended after the options derived from the simple tier, so they win on conflict). Configuring both `ParseClaims` and `Advanced.ParseToken` is a constructor error. When neither is set, the raw claims value is type-asserted to `T`.

Validation semantics:

- Only `SigningMethod` is accepted. The default key function checks the algorithm again, so a `ParserOptions` entry cannot widen it unless `Advanced.KeyFunc` replaces the key function.
- Key selection: when `SigningKeys` is set, a token with a `kid` header uses that entry and an unknown `kid` is rejected; a token without `kid` falls back to `SigningKey` when configured and is rejected otherwise. Without `SigningKeys`, `SigningKey` is used. `Advanced.KeyFunc` replaces this selection entirely.
- `Issuer` requires an exact `iss` match.
- `Audience` is **any-of** (RFC 7519 §4.1.3 validator practice): the token is accepted when its `aud` list contains at least one configured value.
- `Leeway` widens `exp`/`nbf`/`iat` checks in both directions to absorb clock skew between issuer and server.
- `RequireExpiry` rejects tokens without `exp`. The golang-jwt v5 default — pinned by tests — validates `exp` only when present, so an exp-less token never expires unless this is set.

## Usage Patterns

### Pattern 1: Middleware Factory (recommended)

```go
type MyAuthenticator struct {
    sessionStore *SessionStore
}

func (a *MyAuthenticator) Authenticate(r *http.Request) (*User, error) {
    token := r.Header.Get("Authorization")
    if token == "" {
        return nil, errors.New("missing token")
    }
    return a.sessionStore.Validate(token)
}

// Registration
api := app.Group("/api").Middleware(auth.Middleware[*User](myAuth, nil))
```

Register authentication on a group or route. Global middleware runs before routing, for requests that match no route too: global authentication answers an unauthenticated request for an unknown path with 401 instead of 404, and `ctx.Route()` is nil there ([middleware guide](../guides/middleware.md#rewrite-ordering-affects-auth-decisions)). A mounted handler bypasses group middleware and needs global authentication or its own.

### Pattern 2: Custom Middleware (full control)

```go
func MyAuthMiddleware(next credo.Handler) credo.Handler {
    return func(ctx *credo.Context) error {
        token := ctx.Request().Header.Get("Authorization")
        user, err := validateToken(token)
        if err != nil {
            return credo.ErrUnauthorized
        }
        // Attach the authenticated user to the request.
        ctx.SetUser(user)
        return next(ctx)
    }
}
```

### Pattern 3: Handler User Access

```go
func CreateOrder(ctx *credo.Context) error {
    // Type-safe — generic parameter, no assertion needed; 401 when absent
    user, err := ctx.RequireUser[*User]()
    if err != nil {
        return err
    }

    var input CreateOrderInput
    if err := ctx.Request().BindBody(&input); err != nil {
        return err
    }
    order, err := orderService.Create(ctx.Context(), user.TenantID, input)
    if err != nil {
        return err
    }
    return ctx.Response().JSON(201, order)
}
```

### Pattern 4: RBAC with Route Meta

Auth middleware sets the user, RBAC middleware reads meta and checks permissions:

```go
// Route registration: authentication first, then authorization
admin := app.Group("/v1/admin").
    Middleware(auth.Middleware[*User](myAuth, nil), RequirePermission)

admin.GET("/users", listUsers).SetMeta("permission", "admin.users.view")

// RequirePermission authorizes against the route's "permission" meta.
func RequirePermission(next credo.Handler) credo.Handler {
    return func(ctx *credo.Context) error {
        meta, ok := ctx.Route().LookupMeta("permission")
        if !ok {
            return next(ctx) // no permission required
        }
        perm, ok := meta.(string)
        if !ok || perm == "" {
            return credo.ErrForbidden // malformed meta fails closed
        }

        user, err := ctx.RequireUser[*User]()
        if err != nil {
            return err // 401
        }
        if !user.HasPermission(perm) {
            return credo.ErrForbidden
        }
        return next(ctx)
    }
}
```

### Pattern 5: Optional Authentication

Some routes need the user if present but don't require it:

```go
func ViewProduct(ctx *credo.Context) error {
    // User is optional — don't return error if missing
    user, authenticated := ctx.GetUser[*User]()

    product := getProduct(ctx)
    if authenticated {
        product.IsFavorite = checkFavorite(user.ID, product.ID)
    }
    return ctx.Response().JSON(200, product)
}
```

## Design Decisions

The rationale is in [ADR-012](../adr/012-authentication-and-authorization.md): generic accessors on `*credo.Context` with a per-type key and no `context.Context` accessor, the `Authenticator[T]` strategy interface and its optional use, the `ErrorFunc` callback, RBAC via route meta, the separate `auth` package, the two-tier `JWTConfig` and `SecureCompare`. Stdlib middleware adapted via `WrapStdMiddleware` cannot set or read the principal ([ADR-010](../adr/010-middleware-architecture.md#stdlib-adapter)).
