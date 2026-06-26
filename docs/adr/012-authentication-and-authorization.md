# ADR-012: Authentication & Authorization

**Status:** Accepted **Date:** 2026-03-01 **Depends on:** ADR-008

## Context

Enterprise applications (ADR-001) require authentication (who is the user?) and authorization (what can they do?). Frameworks face a design choice: embed a specific user model into the context, or provide generic access patterns that work with any user type.

Embedding a `User` field into Context forces all applications to use the same user representation. Generic accessors allow each application to define its own user type while providing framework-level middleware integration.

## Decision

### Generic User Accessors on Context

The authenticated principal is accessed through generic `*credo.Context`
methods — a first-class part of the Context model, alongside `RequestID()`,
`Route()`, and `Logger()`:

```go
ctx.SetUser(user)                  // store user (T inferred — blessed form)
ctx.GetUser[T]() (T, bool)         // retrieve user (explicit [T] required)
ctx.RequireUser[T]() (T, error)    // retrieve or return ErrUnauthorized/ErrUserMissing
```

These work with any user type — the application defines what a "user" is:

```go
type User struct {
    ID    int
    Email string
    Roles []string
}

// In auth middleware (auth.Middleware calls this for you)
ctx.SetUser(authenticatedUser)

// In handler
user, ok := ctx.GetUser[User]()
```

The `auth` package keeps the authentication *strategy* (`Authenticator[T]`,
`Middleware[T]`, `ErrorFunc`); only the typed user slot — the primitive — lives
on the root Context. The dependency direction is `auth → root` (no cycle).

### No Built-in User Type on Context

Context has no concrete `User` field or `User()` method — only the generic `SetUser`/`GetUser`/`RequireUser` methods. This avoids:

- Forcing a specific user model on all applications
- `any` type assertions (`ctx.User().(MyUser)`)
- Coupling the root package to any concrete auth implementation

### Backing Store

`SetUser` stores the user on the request's `context.Context` using a root-private, generic key — allocation-free and collision-proof. The type parameter gives every user type its own slot, so distinct principals (a JWT end-user and an API-key service account) coexist in one request:

```go
type userKey[T any] struct{}
```

The backing store is an implementation detail: the only public access path is the Context methods. There is deliberately no `context.Context`-based accessor (no `credo.UserFrom`, no `auth.GetUser(ctx)`) — the principal is an HTTP-boundary feature reached through `*credo.Context`, not a value propagated down the `context.Context` chain (contrast `store.GetTx`, which is).

### Authenticator Interface

```go
type Authenticator[T any] interface {
    Authenticate(r *http.Request) (T, error)
}
```

Generic interface parameterized by user type. Implementations extract credentials from the request (header, cookie, query) and return the authenticated user or an error.

### Middleware Factory

```go
auth.Middleware[T](authenticator, onError) credo.Middleware
```

Creates middleware that:

1. Calls `authenticator.Authenticate(r)`
2. On success: stores the user via `ctx.SetUser` on the request
3. On failure: calls `onError` (or defaults to 401 Unauthorized)

```go
app.GlobalMiddleware(auth.Middleware[User](jwtAuth, nil))
```

### RBAC via Route Meta

Authorization uses route metadata (ADR-007) — no separate RBAC system:

```go
// Registration
app.GET("/admin/users", listUsers).SetMeta("permission", "admin.users.read")

// Authorization middleware reads meta
func RBAC(next credo.Handler) credo.Handler {
    return func(ctx *credo.Context) error {
        perm, _ := ctx.Route().LookupMeta("permission").(string)
        if perm == "" {
            return next(ctx) // no permission required
        }
        user, err := ctx.RequireUser[User]()
        if err != nil {
            return credo.ErrUnauthorized
        }
        if !hasPermission(user, perm) {
            return credo.ErrForbidden
        }
        return next(ctx)
    }
}
```

### Design Decisions

| Decision | Rationale |
| --- | --- |
| Generic accessors, not Context field | Works with any user type, no forced model |
| `*credo.Context` methods, not a `context.Context` helper | Principal is an HTTP-boundary feature, like `RequestID()`/`Route()`; no `xFromContext` ceremony |
| Per-type generic `userKey[T]` | Allocation-free, collision-proof, multiple identities coexist |
| RBAC via Route Meta | Declarative, no separate system, inherits via `LookupMeta` |
| Default 401 on auth failure | `ErrUnauthorized.WithInternal(err)` — safe, logged |

## Consequences

**Positive:**

- Works with any user type — no framework-imposed model
- Type-safe via generics — no `any` assertions
- RBAC via existing Meta system — no new concepts
- Middleware factory is reusable across auth strategies (JWT, API key, etc.)
- Principal access is a first-class Context method — no `ctx.Context()` ceremony

**Negative:**

- Callers must handle `ErrUserMissing` when auth middleware did not run or the user type mismatches
- The principal is reachable only where a `*credo.Context` exists (handlers, Credo middleware); stdlib middleware adapted via `WrapStdMiddleware` cannot read or set it
- RBAC is string-based (`"permission"` key) — no compile-time permission checking
