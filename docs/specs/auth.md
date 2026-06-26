# Auth Spec

**Status**: Approved **Package**: `auth/` **Sources**: Original design (no external code adapted) **Depends on**: Root package (`Context`, `Handler`, `Middleware`, `HTTPError`) **ADR**: [012-authentication-and-authorization](../adr/012-authentication-and-authorization.md)

---

## Overview

The `auth/` package provides authentication infrastructure for Credo applications. It does NOT provide a built-in `User` field on Context. Instead, the authenticated principal is reached through generic `*credo.Context` methods (`SetUser`/`GetUser`/`RequireUser`), and this package supplies the `Authenticator[T]` strategy interface and a middleware factory that populates them.

**Design principles**:

- Generic type parameter `T` — works with any user type
- Principal access is a `*credo.Context` method, not a `context.Context` helper — a first-class Context feature like `RequestID()`/`Route()`
- Per-type unexported key — collision-proof, and multiple identities coexist per request
- Zero cost when unused — no Context struct changes (the slot lives on the request's `context.Context`)

---

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
```

**Internal implementation**: `SetUser` stores the user on the request's `context.Context` via `context.WithValue` with an unexported generic `userKey[T] struct{}` key (defined in the root package). The generic type parameter eliminates runtime type assertions at the call site, and because the key itself is parameterized by T, every user type gets its own slot — a JWT user and an API-key service account can coexist in one request. Retrieve with the same T that was stored: a value stored under a concrete type is not visible through an interface type parameter. The backing store is private; the only public access path is these methods (there is no `context.Context`-based accessor).

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

When `nil` (or when it returns `nil`), the middleware returns `credo.ErrUnauthorized` by default.

### Middleware Factory

```go
// Middleware creates an credo.Middleware that authenticates requests
// using the given Authenticator. On success, the user is stored in
// the request context via SetUser. On failure, onError is called
// (or ErrUnauthorized if onError is nil / returns nil).
func Middleware[T any](a Authenticator[T], onError ErrorFunc) credo.Middleware
```

**Returns**: `credo.Middleware` (`func(credo.Handler) credo.Handler`).

### JWT Configuration (two tiers)

`JWTConfig[T]` separates a Credo-typed simple tier from a golang-jwt escape hatch:

- **Simple tier** — extraction (`Header`/`Prefix`/`Query`/`Cookie`), key material (`SigningMethod`/`SigningKey`/`SigningKeys`), registered-claim validation (`Issuer`, `Audience`, `Leeway`, `RequireExpiry`), and user extraction via `ParseClaims func(JWTClaims) (T, error)`. `JWTClaims` is a read-only accessor view: registered claims come back with proper Go types (`ExpiresAt() time.Time` — never the raw float64 that JSON decoding produces), and `Get`/`GetString` read custom claims from the default map representation.
- **Advanced tier** — `Advanced JWTAdvanced[T]` exposes golang-jwt primitives directly: `KeyFunc` (e.g. JWKS), `NewClaims` (typed claims structs), `ParseToken func(*jwt.Token) (T, error)`, and `ParserOptions` (appended after the options derived from the simple tier, so they win on conflict). Configuring both `ParseClaims` and `Advanced.ParseToken` is a constructor error. When neither is set, the raw claims value is type-asserted to `T`.

Validation semantics:

- `Audience` is **any-of** (RFC 7519 §4.1.3 validator practice): the token is accepted when its `aud` list contains at least one configured value.
- `Leeway` widens `exp`/`nbf`/`iat` checks in both directions to absorb clock skew between issuer and server.
- `RequireExpiry` rejects tokens without `exp`. The golang-jwt v5 default — pinned by tests — validates `exp` only when present, so an exp-less token never expires unless this is set.

---

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
app.GlobalMiddleware(auth.Middleware[*User](myAuth, nil))
```

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
    // Type-safe — generic parameter, no assertion needed
    user, ok := ctx.GetUser[*User]()
    if !ok {
        return credo.ErrUnauthorized
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
// Route registration
admin := app.Group("/v1/admin")
admin.Middleware(authMiddleware)
admin.Middleware(rbacMiddleware)

admin.GET("/users", listUsers).
    SetMeta("permission", "admin.users.view").
    SetMeta("scope", "TENANT")

// RBAC middleware
func RBACMiddleware(next credo.Handler) credo.Handler {
    return func(ctx *credo.Context) error {
        user, ok := ctx.GetUser[*User]()
        if !ok {
            return credo.ErrUnauthorized
        }

        perm, _ := ctx.Route().LookupMeta("permission")
        if perm != nil {
            if !user.HasPermission(perm.(string)) {
                return credo.ErrForbidden
            }
        }

        // Store scope info for downstream use
        scope, _ := ctx.Route().LookupMeta("scope")
        if scope != nil {
            ctx.Set("rbac.scope", scope)
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

---

## Package Structure

```
auth/
├── authenticator.go Authenticator[T] interface, ErrorFunc, Middleware[T]
├── basic.go         Basic Auth authenticator
├── apikey.go        API key authenticator
├── jwt.go           JWT authenticator
├── credentials.go   Shared credential extraction helpers
├── compare.go       SecureCompare (constant-time secret comparison)
├── doc.go           Package documentation
└── *_test.go        Tests
```

---

## Design Decisions

1. **No built-in User field** — Progressive disclosure, zero cost, type safety via generics. See [ADR-012](../adr/012-authentication-and-authorization.md).

2. **`context.WithValue` backing, exposed as a Context method** — The user is stored on the request's `context.Context` (so it rides along with the request and survives `ctx.Context()` hand-offs), but the only public access is `ctx.SetUser`/`GetUser`. Principal is a first-class Context feature, not a `context.Context` helper; stdlib middleware adapted via `WrapStdMiddleware` is intentionally second-class here and cannot set or read it (see [ADR-010](../adr/010-middleware-architecture.md)).

3. **Unexported generic key type** — `type userKey[T any] struct{}` (root-private) prevents key collisions (no other package can read or overwrite the user) and gives each user type its own slot, so multiple authentication identities can coexist per request.

4. **`Authenticator[T]` is optional** — The interface is a convenience, not a requirement. Users can write custom middleware and call `ctx.SetUser`/`ctx.GetUser` directly.

5. **`ErrorFunc` callback** — Different apps need different error responses (JSON, redirect to login, custom headers). The callback provides this flexibility without subclassing. If callback returns `nil`, middleware falls back to default `ErrUnauthorized` behavior.

6. **JWT key selection policy** — `SigningKeys` is selected by token `kid` when `kid` is present; unknown `kid` is rejected. If token has no `kid`, middleware falls back to `SigningKey` when configured.

7. **Separate from `middleware/`** — Auth is a cross-cutting concern with its own types (Authenticator, ErrorFunc) and strategy implementations. It deserves its own package rather than being scattered across `middleware/`. (The principal accessors themselves live on the root Context, since the user slot is a Context primitive.)

8. **`SecureCompare` helper** — Basic/API-key validators are user code, and the natural `==` comparison leaks timing information. The framework ships `SecureCompare(x, y string) bool` (hash-then-`subtle.ConstantTimeCompare`, masking both content and length) and the validator type godocs point to it. Password verification should instead use a dedicated password hash (bcrypt/argon2id); JWT signature comparison is already constant-time inside `golang-jwt`.

9. **Two-tier `JWTConfig`** — Credo does not hide golang-jwt, it integrates it (the same visibility policy as Bun in `store/sqldb`): the simple tier is fully Credo-typed for the common path, while the nested `Advanced JWTAdvanced[T]` struct keeps the library's full power one explicit step away. Structural nesting (rather than a doc-only convention) keeps the simple/advanced boundary visible in config literals and code review, and `_ = config.Advanced` usage is easy to grep for when auditing library coupling.

---

## Implementation Phase

- **Phase 3.2**: Generic user accessors + Authenticator interface + middleware factory
- **Phase 3.2** (also): JWT, API Key, Basic Auth implementations (these use the Authenticator interface)
