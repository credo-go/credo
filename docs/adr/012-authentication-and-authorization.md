# ADR-012: Authentication & Authorization

**Status:** Accepted
**Date:** 2026-03-01
**Depends on:** ADR-008

## Context

Enterprise applications (ADR-001) require authentication (who is the
user?) and authorization (what can they do?). Frameworks face a design
choice: embed a specific user model into the context, or provide generic
access patterns that work with any user type.

Embedding a `User` field into Context forces all applications to use the
same user representation. Generic accessors allow each application to
define its own user type while providing framework-level middleware
integration.

## Decision

### Generic User Accessors

The `auth` package provides generic context-based user accessors:

```go
auth.SetUser[T](ctx, user)        // store user in context
auth.GetUser[T](ctx) (T, bool)    // retrieve user from context
auth.RequireUser[T](ctx) (T, error) // retrieve or return ErrUserMissing
```

These work with any user type — the application defines what a "user" is:

```go
type User struct {
    ID    int
    Email string
    Roles []string
}

// In auth middleware
reqCtx := auth.SetUser[User](r.Context(), authenticatedUser)
r = r.WithContext(reqCtx)

// In handler
user, ok := auth.GetUser[User](ctx.Context())
```

### No Built-in User Type on Context

Context has no `User` field or `User()` method. User access is always
through the `auth` package. This avoids:
- Forcing a specific user model on all applications
- `any` type assertions (`ctx.User().(MyUser)`)
- Import cycles between root package and auth implementations

### Context Key

User is stored using `context.WithValue` with a package-private
`struct{}` key — allocation-free and collision-proof:

```go
type userKey struct{}
```

The accessors accept `context.Context`, not `*credo.Context`. Credo handlers
pass `ctx.Context()` explicitly; `*credo.Context` deliberately does not
implement `context.Context` because it is pooled and reused across requests
(ADR-008).

### Authenticator Interface

```go
type Authenticator[T any] interface {
    Authenticate(r *http.Request) (T, error)
}
```

Generic interface parameterized by user type. Implementations extract
credentials from the request (header, cookie, query) and return the
authenticated user or an error.

### Middleware Factory

```go
auth.Middleware[T](authenticator, onError) credo.Middleware
```

Creates middleware that:
1. Calls `authenticator.Authenticate(r)`
2. On success: stores user via `auth.SetUser[T]` in request context
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
        user, err := auth.RequireUser[User](ctx.Context())
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
|----------|-----------|
| Generic accessors, not Context field | Works with any user type, no forced model |
| `context.Context` param, not `*credo.Context` | Works outside Credo handlers; Credo handlers pass `ctx.Context()` |
| `struct{}` context key | Allocation-free, collision-proof |
| RBAC via Route Meta | Declarative, no separate system, inherits via `LookupMeta` |
| Default 401 on auth failure | `ErrUnauthorized.WithInternal(err)` — safe, logged |

## Consequences

**Positive:**
- Works with any user type — no framework-imposed model
- Type-safe via generics — no `any` assertions
- RBAC via existing Meta system — no new concepts
- Middleware factory is reusable across auth strategies (JWT, API key, etc.)
- `context.Context` param enables use outside Credo handlers

**Negative:**
- Callers must handle `ErrUserMissing` when auth middleware did not run or the user type mismatches
- Single user type per context — applications with multiple simultaneous
  user identities must define separate context keys
- RBAC is string-based (`"permission"` key) — no compile-time permission checking
