# Pre-v1 Migration Guide

**Status:** The bootstrap/DI changes (DI minor), the router parameter-name change (router minor) and the built-in HTTP feature changes (HTTP minor) are implemented as of 2026-09-05; the [Bootstrap and DI](#bootstrap-and-di), [Built-in HTTP features](#built-in-http-features) and [Router](#router) sections below describe shipped behavior. The URL round-trip change (wire minor) is implemented as of 2026-09-05 and described under [Router](#router) as well. The accepted decisions are recorded in [ADR-022](../adr/022-bootstrap-and-di-ownership.md) (bootstrap and DI ownership), [ADR-007](../adr/007-router-and-routing.md#url-round-trip) (URL round trips) and [ADR-010](../adr/010-middleware-architecture.md#built-in-http-feature-configuration-criterion) (built-in HTTP features); [TODO](../../TODO.md#pre-v1-contract-migration) tracks progress. The worker contract of v0.20.0, the restart backoff of v0.21.0 and the v0.24.0 workers as components are described under [Workers](#workers); their decisions are recorded in [ADR-023](../adr/023-worker-system.md). The `store/sqldb` move to Bun v1.3.0 in v0.22.0 is described under [Data access](#data-access); [ADR-015](../adr/015-data-access.md) records the data-access decisions. The v0.23.0 router fixes are described under [Router](#router), and its YAML change under [Configuration](#configuration). The v0.24.0 sequential bootstrap is described under [Sequential bootstrap](#sequential-bootstrap), its lifecycle components under [Lifecycle components](#lifecycle-components), its stores, WebSocket server and i18n on the components under [Stores, WebSocket and i18n](#stores-websocket-and-i18n), its seven-method DI surface under [DI surface](#di-surface), and its workers as components under [Workers as components](#workers-as-components); [ADR-024](../adr/024-lifecycle-components.md) records the component decisions. Its error rules are described under [Error handling](#error-handling) and its `StatusHandler` check under [Router](#router). [v0.24.0 at a glance](#v0240-at-a-glance) lists first the v0.24.0 changes that compile unchanged, then what the release deletes, and ends with [what an application keeps](#what-an-application-keeps-v0240). [Unreleased](#unreleased), first, maps the changes on `main` since v0.26.0. [v0.26.0 at a glance](#v0260-at-a-glance), after it, opens with the v0.26.0 changes that compile unchanged, maps every row the release deletes or renames by area — errors and message keys, context values and meta, routing and middleware, configuration and reload, validation, authentication, store and `sqldb`, and the removed root APIs — and ends with [what an application keeps](#what-an-application-keeps-v0260). [v0.25.0 at a glance](#v0250-at-a-glance), after it, opens with the v0.25.0 changes that compile unchanged, maps every name v0.25.0 deletes or renames — the bound store, the first `Resolve` that finalizes, `Provide` without a type argument, the WebSocket server's declared tier, the workers in the root package, `middleware/ratelimit`, `credo.ComponentName` and `credo.CloseOnShutdown` — and ends with [what an application keeps](#what-an-application-keeps-v0250); the sections after it describe each earlier release with the calls of that release.

## Unreleased

These changes are on `main` and not yet in a release.

| v0.26 | Unreleased |
| --- | --- |
| A route with text after a catch-all (`GET /a/{p...}/x`), which never matched and answered 405 | Registration panics: end the pattern with the catch-all (`/a/{p...}`) and read the rest from its capture |
| A catch-all in a `Mount` prefix, or in a group prefix under `Static` | Registration panics: the mount or static route captures the path below its prefix itself |
| `middleware.ContractConfig{RequireContentType: true}`, or a `MetaAccept` contract that let a body without a `Content-Type` pass | Drop the field: the contract answers 415 to such a body by default, and `ContractConfig{AllowMissingContentType: true}` lets it pass |
| `UseHealth` with `Enabled` false, followed by a second `UseHealth` | The second call panics: call `UseHealth` once, with the `Enabled` value the deployment wants; checks registered after a disabled call are accepted and never run |

## v0.26.0 at a glance

v0.26.0 reshapes the contracts v1 will freeze, so that each has one way to do each thing:

- an error has one status channel;
- a request value or a route meta entry is a typed key;
- route meta is read after the match, by a routed middleware tier;
- configuration is read through typed section descriptors, decoded strictly, and live where it must change on reload;
- the access log is one observer of the final response;
- the Context is not pooled;
- validation names a field as the client sent it;
- JWT is in its own package;
- `store/sqldb` counts only what it can prove.

The release also removes the parallel APIs left beside each of these. The decisions are recorded in:

| Area | ADRs |
| --- | --- |
| Configuration | [ADR-005](../adr/005-configuration-architecture.md), [ADR-020](../adr/020-reload-and-partial-config-reload.md) |
| Routing and middleware | [ADR-007](../adr/007-router-and-routing.md), [ADR-010](../adr/010-middleware-architecture.md), [ADR-018](../adr/018-host-routing-and-rewrite.md) |
| Context and errors | [ADR-008](../adr/008-context-design.md), [ADR-009](../adr/009-handler-and-error-handling.md) |
| Validation | [ADR-011](../adr/011-validation-strategy.md) |
| Authentication | [ADR-012](../adr/012-authentication-and-authorization.md) |
| Observability | [ADR-014](../adr/014-observability.md) |
| Data access | [ADR-015](../adr/015-data-access.md) |
| Application modules | [ADR-003](../adr/003-application-architecture.md) |

Where an earlier section below names a call this release renames or deletes, this section gives the current one.

### Check first: changes that compile unchanged (v0.26.0)

The compiler finds every removed or renamed call ([below](#what-v0260-deletes-or-renames)). It does not find the changes below: code that builds as before and behaves differently. Look for them first.

- **Configuration decodes strictly.** An unknown key, or a value of another kind than its field (`port: true`, `name: 42`, `tags: "a"`), now fails the decode that reads it.
  - Under `server`, it fails `credo.New`.
  - Under an application section, it fails the `Decode`, `GetConfig` or `Unmarshal` that reads the section.
  - The error names the key path (`config: unknown key "server.prot"`) and `config.WithWeakDecoding()`, the opt-in that restores the previous decoding.
  - String overrides from the process environment and `.env` still decode into typed fields ([configuration guide](configuration.md)).
- **An error status comes from one channel.**
  - An error type that relied on `HTTPStatus() int` now renders 500.
  - Code that matches `interface{ HTTPStatus() int }` with `errors.As` still compiles, but no longer matches Credo's errors, `store`'s included.
- **Fault codes are their kind.** A store error, or another fault without a code of its own, renders its kind as its code:
  - `already_exists`, `constraint`, `serialization`, `deadlock`, `contention` and `conflict`, where every 409 rendered `conflict`;
  - `timeout`, where it rendered `gateway_timeout`;
  - `unavailable` and `read_only`, where they rendered `service_unavailable`.

  `not_found` and every status are unchanged. A client that matches `conflict` must match the kind's code.
- **A missing catalog key is never rendered as text.**
  - A message key no catalog defines falls back to the built-in message for the code, then to the status text. Debug mode logs a Warn naming the key, the code and the route.
  - `WithMessageKey("literal text")` compiles unchanged and now renders, for example, "Bad Request". Literal text is `WithMessage`.
  - `I18nConfig.ResolveMessageKey` now maps `middleware.Timeout`'s code `request_timeout`.
- **The framework's own rejections carry the client's value in `details`.** These render fixed messages, and the rejected value is in `error.details`, no longer in the message:
  - an unsupported content encoding (`encoding`) or content type (`content_type`);
  - `ContractGuard`'s rejected content type (`content_type`), header (`header`), query parameter (`query`) and scope (`scope`).
- **Two misuses are internal errors.** Both used to answer otherwise; the reason is now logged.
  - An unsupported bind field type answers 500 `invalid_bind_target`, where it answered 400.
  - `Response.Redirect` with a status outside 300..308 answers 500.
- **Validation names fields by their source.** A violation of `BindQuery`, or of a form or multipart `BindBody`, names its field by the `query` or `form` tag, the name the client sent. It used to use the `json` tag, or the Go field name where the struct had none. Rename those keys in `fields.json` too. A JSON body is unchanged, and an XML body keeps the `json` names.
- **Slices of structs and nested values are validated.**
  - A slice or array element that implements `validation.Validatable`, or a struct whose pointer does, is now validated, and its violations are named `items[0].name`. Such elements used to pass unless the field's rules said `Each`.
  - A field with rules now runs its value's `Validate` once those rules pass: `validation.Field(&r.Billing, validation.NotNil[Address]())` checks presence, then runs `Billing.Validate`. A field whose rules fail still reports their violations only.
  - The former pair `Field(&x, NotNil[T]())` + `Field(&x)` now runs `x.Validate` twice and reports each nested violation twice; keep the first `Field` only.
- **`ContractGuard` before routing fails closed.** A `ContractGuard` registered before routing (the former `GlobalMiddleware`, now `PreRouting`) answers 500 for every request and logs one Error naming the registration and the remedy. It used to let every request through. Register it with `app.Middleware`.
- **A mounted request carries its mount as its route.**
  - `HasRoute` is true for it.
  - App-level meta such as `MetaAccessLog` applies to it.
  - Its access record names the mount's prefix pattern.
  - For a mounted QUERY request without a Content-Type, the routed and pre-routing middleware now run before the 415, as they do for a route.
- **The request has one context.**
  - A user or value set inside `middleware.Timeout` or a wrapped stdlib middleware is now visible after the chain returns: to access-log filters, observers and error rendering.
  - A deadline or span that a stdlib middleware adds is visible to the handler through `c.Context()`.
  - A stdlib middleware that passes on a context not derived from the request's now hides the values set before it from the handlers after it.
  - Assigning `c.Request().Request` no longer changes `c.Context()`; `c.Within(ctx, next)` does. Debug mode logs a warning once per route.
  - The embedded request's own `c.Request().Request.Context()` does not carry values set with `Set` or `SetUser`.
  - A mounted `http.Handler` sees the values set before the mount.
- **A `*Context` kept past its request reads its own request.** The Context is no longer pooled. A `*Context` captured by a goroutine or stored by a hook keeps reading its own request's state; it no longer reads a later request's, another tenant's user included. It is still not safe for concurrent use.
- **`middleware.Rewrite` matches the wire form, as the router does.**
  - An encoded slash is data inside a segment: a rule that matched across `%2F` (`/old/{a}/{b}` against `/old/x%2Fy`) no longer does, and a captured `%2F` stays one segment in the target.
  - A `{name:regex}` parameter is one segment, and its constraint is checked on the decoded value: `{rest:.+}` no longer spans segments, and `%31` satisfies `[0-9]+`.
  - A `{name}` ends at the byte that follows it in the pattern: `/d/{a}-{b}` splits `x-y-z` at the first `-`.
  - A `To` with a literal `%` (`/new/100%`), or a `From` with text after its catch-all, now panics when the middleware is built ([routing guide](routing.md#encoded-paths)).
- **A static URL whose file path needs escaping is now escaped.** `StaticRoute.BuildURI` percent-encodes each segment of the file path (a space, `%`, `?`, `#`, `;`, non-ASCII), so the URL serves the file it names. Its new error result is a compile error at each call site.
- **Certificate files rotate when they change on disk.** With `WithTLSFiles` or `server.tls.*`, a changed pair is picked up at the next handshake, checked at most once a second, without a signal or a reload. A reload no longer moves the pair to new `server.tls.*` paths: a changed path is reported as needing a restart.
- **The restart report is computed against the start.** A reload's result names every key whose value differs from the start's outside a live section, on every reload until the key reverts. It used to name only the keys that reload changed and no subscriber covered.
- **`testutil.NewApp` without `WithConfig` pairs has no sections.** Decoding a key returns the not-found error a loaded configuration returns. It used to succeed and leave the target unchanged.
- **`sqldb` counts only a provable query.** `Count` and `Page` return `ErrUnsupportedCountQuery` before any I/O for these queries, which used to count through a derived table:
  - a query built with `Apply`, `Unwrap` or a `Relation` callback;
  - a raw `ColumnExpr` or `Having` without `GroupExpr` or `Distinct`;
  - a model that implements `bun.BeforeSelectHook`.

  Page them with `PageWithCount` ([data-access guide](data-access.md#paging-with-your-own-count)). The count is Bun's own statement (`SELECT count(*) …`, or a `_count_wrapper` CTE), not `… FROM (…) AS _credo_count_source`. It runs no model SELECT hook, so a query hook or log that matched the old SQL sees the new form.
- **Only the wrapper's `Conn` chooses a connection.** A `Conn` set inside `SelectQuery.Apply` is overridden by transaction injection. A typed terminal on a model-bound query reports `ErrTypedTerminalModel` before a Bun builder error recorded on the same query.
- **The access log is an observer.** Its records are unchanged byte for byte. It now runs among the observers, in the order `UseAccessLog` was called relative to `AddObserver`. Its `Skipper` skips the access record alone: a skipped request still reaches every other observer.

### What v0.26.0 deletes or renames

Each row below is a compile error, or a log line, message or code the compiler cannot see, with its replacement. The rows are grouped by area.

#### Errors and message keys

| v0.25 | v0.26.0 |
| --- | --- |
| An error type with `HTTPStatus() int` | Implement `fault.Provider`, return a `fault.Error`, or wrap it in `credo.NewHTTPError(status).WithInternal(err)` |
| `errors.As(err, &interface{ HTTPStatus() int })` | `ErrorInfo.Status` in a renderer; `errors.AsType[*credo.HTTPError](err)` and its `Status`; or `fault.KindOf(err)` |
| `WithMessageKey("literal text")` | `WithMessage("literal text")` |
| `WithMessageKey(catalogKey)` with a literal or a typed constant | Unchanged |
| `WithMessageKey(s)`, `RenderMessageKey(s)` or `ValidationError{MessageKey: s}` with a computed `string` | `credo.MessageKey(s)` |
| An `ErrorRenderer` or `SuccessRenderer` reading `MessageKey` as a `string` | It is a `credo.MessageKey`; use `string(info.MessageKey)` where a string is needed |
| `NewHTTPError(http.StatusNotFound, credo.MsgKeyNotFound)`, with a `MsgKey*` constant that is its status's own code | `NewHTTPError(http.StatusNotFound)` |
| `NewHTTPError(status, credo.MsgKeyX)` where X is not the status's code | `NewHTTPError(status, "x_code")` |
| `credo.MsgKeyValidationFailed`, `credo.MsgKeyBindFailed` | `credo.CodeValidationFailed`, `credo.CodeBindFailed` |
| A client matching `conflict` for a store or fault error | The kind's code: `already_exists`, `constraint`, `serialization`, `deadlock`, `contention` or `conflict`; `gateway_timeout` → `timeout`; `service_unavailable` → `unavailable` or `read_only` |
| A service returning `credo.NewHTTPError(409, "domain_taken")` | `fault.New(fault.KindAlreadyExists, "domain_taken")`. This is optional: the `HTTPError` still works |
| A client reading the rejected value from a 415, 400 or 403 message | `error.details`: `encoding`, `content_type`, `header`, `query`, `scope` |
| 400 `unsupported_bind_field_type` | 500 `invalid_bind_target`, with the reason logged |

A domain layer returns a `fault.Error`, and the handler returns it unchanged ([error-handling guide](error-handling.md)):

```go
var ErrDomainTaken = fault.New(fault.KindAlreadyExists, "domain_taken")

func (s *Service) Claim(ctx context.Context, domain string) error {
    if s.exists(ctx, domain) {
        return ErrDomainTaken // 409 {"code":"domain_taken"}
    }
    // …
}
```

#### Context, values and meta

| v0.25 | v0.26.0 |
| --- | --- |
| `c.Set("tenant", t)` / `c.Get("tenant").(T)` | `var tenantKey = credo.NewKey[T]("tenant")`; then `c.Set(tenantKey, t)` / `t, ok := c.Get(tenantKey)` |
| `r.SetMeta("permission", "admin")` / `r.LookupMeta("permission")` | `var permKey = credo.NewKey[string]("permission")`; then `r.SetMeta(permKey, "admin")` / `c.Route().GetMeta(permKey)` |
| `info.Meta["k"]` | `info.GetMeta(k)` |
| `r.SetMeta(credo.MetaAccessLog, false)` | Unchanged at the call site; the key is now a `Key[bool]` |
| `SetMeta(middleware.MetaScope, "read")`, or any `any` value | `SetMeta(middleware.MetaScope, []string{"read"})`; `MetaMaxBody` takes an `int64` |
| `SetMeta(middleware.MetaScope, string(scope))`, or a named type in a contract value | `SetMeta(middleware.MetaScope, []string{string(scope)})`: a `[]Scope` does not compile |
| `SetMeta(middleware.MetaRequireHeaders, nil)` to keep a contract (rejected before) | A `nil` list is an empty list and lifts the contract; `RemoveMeta` restores the group's |
| `c.Request().Request = r.WithContext(ctx); return next(c)` | `return c.Within(ctx, next)` |
| `x(c.Request().Request)`, handing the stdlib request onwards | `x(c.Request().Std())` |
| `c.Request().Request.Context()` | `c.Context()`, or `c.Request().Context()`, which now returns the same context |

```go
var tenantKey = credo.NewKey[*Tenant]("tenant")

func Tenant(next credo.Handler) credo.Handler {
    return func(c *credo.Context) error {
        c.Set(tenantKey, lookupTenant(c))
        return next(c)
    }
}

// In a handler, or in a repository given c.Context():
t, ok := c.Get(tenantKey) // *Tenant, no type assertion
```

#### Routing, middleware and observers

| v0.25 | v0.26.0 |
| --- | --- |
| `app.GlobalMiddleware(m...)` | `app.PreRouting(m...)` |
| A middleware on `GlobalMiddleware` that reads route meta (`ContractGuard`, authorization) | `app.Middleware(m...)`, the routed tier |
| `CORS`, `Rewrite`, `Secure` or `RateLimit` on `GlobalMiddleware` | `app.PreRouting(...)`: they stay before routing |
| Authentication repeated on every group, or registered as global middleware where it also covered 404s | `app.Middleware(auth.Middleware[...](...))`, which covers mounts too |
| `ContractGuard` repeated on every group | `app.Middleware(middleware.ContractGuard())` |
| A `Rewrite` rule that matched the decoded `URL.Path`, so `{a}/{b}` matched `x%2Fy` | Rules match the wire form, where an encoded slash is data inside one segment: match it with one `{name}`, or with `{name...}` |
| A `{name:regex}` in a `Rewrite` `From` that matched across segments (`{rest:.+}`) | It is one segment, as in a route: use `{rest...}`, or a `Regexp` rule, for a capture that spans segments |
| A `Rewrite` target that was a decoded path with `RawPath` cleared, so a captured `%2F` became a separator | `To` is a wire-form path: a captured `%2F` stays one segment, and a literal `%` is written `%25` |
| `ResultFilter: func(c *credo.Context, e credo.AccessLogEntry) bool { … e.Status … }` | `ResultFilter: func(ex *credo.Exchange) bool { … ex.Status() … }`; every entry field is a method of the same name |
| `c.GetUser[T]()` / `c.Get(key)` inside a `ResultFilter` | `ex.GetUser[T]()` / `ex.Get(key)`; `ex.Context()` for the request's context |
| A variable or parameter of type `credo.AccessLogEntry` | Copy the values you keep from the `*credo.Exchange`, which is valid only during the call |
| `credo.AccessLogResultFilter` | `func(*credo.Exchange) bool` |
| A middleware that observes the response to export metrics | An `Observer` added with `app.AddObserver`, which sees the final status, bytes and duration |

The user tiers run in the order PreRouting → routed → Group → Route:

```go
app.PreRouting(middleware.Secure(), middleware.CORS(corsCfg)) // before the match, 404 and 405 included
app.Middleware(auth.Middleware(authn, nil), middleware.ContractGuard()) // every route and mount, after the match
app.AddObserver(metrics) // sees each final response: ex.Status(), ex.Bytes(), ex.Duration(), ex.ErrorCode()
```

#### Configuration and reload

| v0.25 | v0.26.0 |
| --- | --- |
| `config.Load(config.WithStrictDecoding(), …)` | `config.Load(…)`: strict decoding is the default, and the option is gone |
| A configuration with keys no struct field decodes (a typo, a key another deployment reads, a stale section field), or a value of another kind than its field | Fix the configuration. To keep mapstructure's weak coercion and ignore unknown keys, load it with `config.Load(config.WithWeakDecoding())` and pass it through `credo.WithRawConfig` |
| A constructor that calls `rc.Unmarshal("app", &cfg)` and returns `cfg` | `app.Provide(config.Section[AppConfig]("app").Decode)`, or keep the constructor |
| `cfg.Get[T]("app")` | `config.Section[T]("app").Decode(cfg)` |
| `cfg.MustGet[T]("app")` | `config.Section[T]("app").Decode(cfg)`, with its error handled |
| `cfg.Get[T]("")` / `cfg.MustGet[T]("")`, the whole tree | `cfg.Unmarshal("", &v)`, since a section has no empty key. A strict whole-tree target must cover every `.env` entry and the `server` keys ([Strict Decoding](configuration.md#strict-decoding)) |
| `app.GetConfig[T]("app")` / `app.MustGetConfig[T]("app")` | Unchanged, since the untyped key converts. `app.GetConfig[T](key)` with a `string` variable becomes `app.GetConfig(config.Section[T](key))` |
| `app.GetConfig[T]("")` / `app.MustGetConfig[T]("")`, the whole tree | `Unmarshal("", &v)` on the `credo.RawConfig` a constructor takes, or on the one passed to `credo.WithRawConfig`, with the same caveat |
| A test that relied on `testutil.NewApp`'s empty configuration accepting any key | Provide the section with `testutil.WithConfig` |
| `app.OnConfigChange(key, fn)` with a holder the consumer reads | A live section: `var S = config.Section[T](key)`, `app.Provide(S.Live)`. The consumer takes `*config.Live[T]` and calls `Load()` where it read its holder. When `fn` derived state (a limiter, a `slog.LevelVar`), it becomes `live.OnChange(func(ctx, prev, next T) error)` |
| `app.OnReload(fn)` that refreshed derived state | The section's `OnChange` |
| `app.OnReload(fn)` that reopened a file or did other work unrelated to configuration | Its own SIGHUP handler with `signal.Notify`, which runs beside Credo's |
| A custom `RawConfig` that implemented `config.Stager` or `config.Reloader` | It no longer reloads. Load the configuration with `config.Load` (or `config.LoadBytes`) and pass it through `credo.WithRawConfig` |
| `cfg.Reload()` returning `config.Changes` | `cfg.Reload(ctx)` returning `config.ReloadResult`: `changes.Keys()` → `res.Changed`; `changes.Affects(p)` → a prefix test over `res.Changed`; `changes.Empty()` → `len(res.Changed) == 0` |
| `cfg.Stage()`, `Staged` and its `Commit` | No replacement: `Reload` validates every live section against the candidate before it publishes |
| `err := app.Reload(ctx)` | `res, err := app.Reload(ctx)`; `res` says what was applied and what needs a restart |
| A reload to move the served certificate to new `server.tls.*` paths | A restart. Rewriting the files at the same paths, or swapping the link they resolve through, rotates them without a reload |
| A reload (SIGHUP) to rotate certificate files rewritten in place | Nothing: the files are checked at most once a second, at a handshake |

A section is a snapshot unless it is declared live ([configuration guide](configuration.md)):

```go
var AppSection = config.Section[AppConfig]("app")      // static: read once
var LimitsSection = config.Section[Limits]("limits")   // live: replaced by a reload that changes it

app.Provide(AppSection.Decode)  // binds AppConfig
app.Provide(LimitsSection.Live) // binds *config.Live[Limits]

func NewLimiter(limits *config.Live[Limits]) *Limiter {
    l := &Limiter{}
    l.set(limits.Load())
    limits.OnChange(func(ctx context.Context, prev, next Limits) error {
        l.set(next)
        return nil
    })
    return l
}
```

#### Validation

| v0.25 | v0.26.0 |
| --- | --- |
| A client or test that matches a query or form violation's `field` by its `json` name (or Go field name) | Match the `query` or `form` tag name, and rename those keys in `fields.json` |
| `validation.Field(&x, validation.NotNil[T]())` and `validation.Field(&x)` for a required nested value | `validation.Field(&x, validation.NotNil[T]())` alone: it validates `x` once `NotNil` passes, and the pair now reports each violation twice |
| `validation.Each[T](validation.By(func(v T) error { return v.Validate() }))`, or similar, to validate a slice of structs | Drop it: the elements are validated once the field's rules pass |
| A slice of structs whose elements' `Validate` must not run on bind | Give the slice field a type with a `Validate` of its own, which replaces the element walk |

#### Authentication

| v0.25 | v0.26.0 |
| --- | --- |
| `import "github.com/credo-go/credo/auth"` for the JWT authenticator | `import "github.com/credo-go/credo/auth/jwtauth"`; `auth.Middleware` stays in `auth` |
| `auth.JWTConfig[T]`, `auth.JWTAdvanced[T]`, `auth.JWTClaims` | `jwtauth.Config[T]`, `jwtauth.Advanced[T]`, `jwtauth.Claims` |
| `auth.NewJWTAuthenticator[T](cfg)`, returning `*auth.JWTAuthenticator[T]` | `jwtauth.NewAuthenticator[T](cfg)`, returning `*jwtauth.Authenticator[T]` |
| `auth.ErrJWTMissing`, `auth.ErrJWTInvalid` (text `auth: jwt token is …`) | `jwtauth.ErrTokenMissing`, `jwtauth.ErrTokenInvalid` (text `jwtauth: token is …`) |
| Matching `auth: jwt signing key is required` | `jwtauth: Config has no signing key; set SigningKey, SigningKeys or Advanced.KeyFunc` |

An application that does not verify JWTs no longer compiles golang-jwt.

#### Store and sqldb

| v0.25 | v0.26.0 |
| --- | --- |
| `Count` or `Page` on a query built with `Apply`, `Unwrap` or a `Relation` callback | `PageWithCount[T](ctx, req, countQuery.Count)`, with the count built without the escape hatch (or a count of your own) |
| `Count` or `Page` on a raw `ColumnExpr` (or `Having`) without `GroupExpr` or `Distinct` | Add `GroupExpr` or `Distinct` if that is what the query means, else use `PageWithCount` |
| `Page[T]` where `T` implements `bun.BeforeSelectHook` | `PageWithCount[T](ctx, req, count)`, with the count applying the hook's predicate itself, for example through the same `ApplyFilter` function |
| `q.ApplyQueryBuilder(func(qb bun.QueryBuilder) bun.QueryBuilder { return qb.Where(...) })` | `q.ApplyFilter(func(f *sqldb.Filter) { f.Where(...) })`: the callback returns nothing, and `WhereGroup`'s callback takes a `*sqldb.Filter` |
| An `ApplyQueryBuilder` callback that needs more than predicates (`Unwrap`, other builder methods) | `Apply` on the query type; a select built with it pages with `PageWithCount` |
| `Apply(func(q *bun.SelectQuery) *bun.SelectQuery { return q.Conn(c) })` | `q.Conn(c)` |
| `store.WithTx[T](ctx, tx)`, `store.GetTx[T](ctx)`, `store.Conn[T](ctx, base)` | Make `scope := store.NewTxScope[T]()` once at wiring time, then call `scope.WithTx(ctx, tx)`, `scope.GetTx(ctx)`, `scope.Conn(ctx, base)` |
| `store.WithTxInScope(ctx, scope, tx)`, `GetTxInScope(ctx, scope)`, `RequireTxInScope(ctx, scope)`, `ConnInScope(ctx, scope, base)` | `scope.WithTx(ctx, tx)`, `scope.GetTx(ctx)`, `scope.RequireTx(ctx)`, `scope.Conn(ctx, base)` |
| `store.ErrDuplicate` | `store.ErrAlreadyExists` |
| `errors.Is(err, store.ErrConflict)` | A check of `store.ErrConstraint`, `ErrSerialization`, `ErrDeadlock` or `ErrContention`, whichever it meant |
| `sqldb.RunInTx(ctx, db, fn)` | `db.InTx(ctx, fn)` |
| `sqldb.RunInTxWith(ctx, db, opts, fn)` | `db.InTxWith(ctx, opts, fn)` |
| Matching `sqldb: unsupported Count/Page query shape` | `sqldb: unsupported Count/Page query`; `errors.Is` against `ErrUnsupportedCountQuery` is unchanged |

The usual shape is a cheap count beside the expensive data query, both scoped by one predicate ([data-access guide](data-access.md#paging-with-your-own-count)):

```go
scope := func(f *sqldb.Filter) { f.Where("tenant_id = ?", tenantID) }
page, err := db.Select((*Report)(nil)).ApplyFilter(scope).Apply(withJoins).
    PageWithCount[Report](ctx, req, db.Select((*Report)(nil)).ApplyFilter(scope).Count)
```

#### Removed root APIs

| v0.25 | v0.26.0 |
| --- | --- |
| `credo.RouteContext`, `resp.Reset(w)` | No replacement: nothing returned the one, and an application has nothing to reset with the other |
| `uri := sr.BuildURI(path)` on a `*StaticRoute` | `uri, err := sr.BuildURI(path)`: the file path is escaped per segment, and a path that is not valid UTF-8, or has a `.` or `..` segment, is an error |
| `sr.BuildURI()` | `sr.BuildURI("")`: the prefix with its trailing slash |
| `credo.Walk(app, fn)` / `credo.WalkRoutes(app, fn)` | `for _, ri := range app.Routes() { … }`. To keep `Walk`'s skip of mounts, add `if ri.Kind == credo.RouteKindMount { continue }` |
| `credo.NewRequest(r)` / `credo.NewResponse(w)` | `credo.NewContext(w, r).Request()` / `.Response()` |
| `return c.Rewrite(path)` | A `middleware.Rewrite` rule in `app.PreRouting`, static's SPA mode, or `return targetHandler(c)` ([routing guide](routing.md#conditional-forwarding)) |
| `if c.IsRewriting() { … }` in a middleware | Delete the check: middleware runs once per request |

### What an application keeps (v0.26.0)

- **Errors.** An error carries its status as an `*HTTPError` or a `fault.Error`. A message key is a catalog key; literal text is `WithMessage`.
- **Request values and meta.** Both use typed keys made with `credo.NewKey[T]`, and a middleware changes the request's context with `c.Within`.
- **Middleware tiers.** `PreRouting` holds what runs before the match; `app.Middleware` holds what reads route meta.
- **Configuration.**
  - It is read through `config.Section[T]` descriptors.
  - It decodes strictly unless it is loaded with `WithWeakDecoding()`.
  - A section that must change on reload is declared live.
- **Access log.** A `ResultFilter` reads the `*credo.Exchange`.
- **`store/sqldb`.** A query the wrapper cannot count pages with `PageWithCount`. A transaction runs through a `store.TxScope` and `db.InTx`.

New, and optional: an application split into parts that it registered in a hand-kept order can turn each part into a `credo.Module` (`Services` and `Handlers`) and pass them all to one `app.UseModules` call. Every module's services are then registered before any module's handlers resolve. Its tests use `testutil.WithModules` with `WithOverride` ([dependency-injection guide](dependency-injection.md)).

## v0.25.0 at a glance

v0.25.0 shortens the composition root to what only the application knows. A bound store is an ordinary component, the first `Resolve` finalizes the container, `Provide` takes its binding's type from the constructor, a WebSocket server declares its tier, workers are registered through `app.Workers()` in the root package, and rate limiting moves into `middleware/ratelimit`. The decisions are recorded in [ADR-004](../adr/004-dependency-injection-and-infra.md), [ADR-015](../adr/015-data-access.md), [ADR-016](../adr/016-health-checks.md), [ADR-022](../adr/022-bootstrap-and-di-ownership.md), [ADR-023](../adr/023-worker-system.md) and [ADR-024](../adr/024-lifecycle-components.md). Where a v0.24.0 section below names a call this release renames or deletes, this section gives the current one.

### Check first: changes that compile unchanged (v0.25.0)

The compiler finds every removed or renamed call ([below](#what-v0250-deletes-or-renames)). These changes it does not find — code that builds as before and behaves differently:

- **Every bound database gates readiness.** Every `*sqldb.DB` bound with `ProvideValue` or `Provide`, and every component whose binding's type shows `Ready`, is pinged at start and enters `/ready`, whether or not `store.Register` named it before; a borrowed database is no longer pinged, since its owner starts it. A dependency that must not gate readiness is bound with `credo.OptionalReadiness()`: while it fails, its entry shows `down` and `/ready` answers 200 with the status `degraded`. A database whose outage must not stop the start is opened with `sqldb.WithOptionalStart()` ([data-access guide](data-access.md#what-a-bound-database-does)).
- **The `/ready` body.** Every entry carries `latency`; the top-level status gains `degraded`; a database's entry is named `*sqldb.DB` or by `credo.ComponentName`; a resource held by two registrations — a raw handle and a wrapper that embeds it — reports one entry where two `store.Register` calls reported two; a store that reports DEGRADED, or up with a cause, is what its `Ready` says (`*sqldb.DB` reports `down` for any status but UP); and a failure is logged in the check shape, with its latency ([deployment guide](deployment.md#what-ready-reports)).
- **A WebSocket server is ingress without the option.** `websocket.Server` declares its tier, so a server registered without `credo.Ingress()` moves into the ingress tier, and an internal component that depends on it now fails `Finalize`, with the path and the two remedies.
- **Log keys.** A constructor's `Infra` logger and `app.NewInfra(name)` write `component` in place of `service`, with the binding's full name (`component=*users.Service`, where `service=Service` dropped the package and the pointer); a worker's records write `component=worker:<name>` in place of `worker=<name>`; the `http.Server.ErrorLog` bridge writes `module=net/http` in place of `component=net/http`; WebSocket connection logs gain `module=websocket`; and `sqldb`'s pool warning carries `module=sqldb` in place of `component=store` and `store=<name>`, with `component=<name>` when it goes to the App's logger rather than to one given with `sqldb.WithLogger`. Log queries, filters and alerts on the old keys must change.
- **One worker registry.** `Snapshot` and `Lookup` cover every worker of the App, and a duplicate worker name panics at the second registration whoever made the first, where two supervisors kept separate names.
- **`Resolve` before `Finalize` works.** The first `Resolve`, `ResolveAll`, `MustResolve` or `MustResolveAll` finalizes the container and resolves, where it panicked; a registration after it panics. A late registration's panic names what finalized the container, where, and the calls that led there ([dependency-injection guide](dependency-injection.md#finalize)).
- **A hidden capability is refused.** A binding under an interface that hides its value's `Start`, `Ready` or ingress tier is refused — `ProvideValue` at the call, a constructor's value at the start — where the method was skipped without a word, unless another registration of the same resource plans it or the binding is borrowed. A direct binding after an `Alias` of its type panics as a duplicate. `testutil.WithOverride` of an aliased interface replaces it, where it panicked, and the type the alias pointed at is no longer built when nothing else uses it.
- **A value a constructor builds claims nothing.** `Manage` of a constructor over a handle bound with `ProvideValue`, with or without `credo.Borrowed()`, now starts, where it failed the start. Such a value — from a constructor handed to `Manage`, or from a worker's constructor — no longer adds a readiness entry or a teardown of its own: `/ready` shows the binding's one entry, with its name and options, even when the constructor was registered first, and a `Manage` call of such a constructor with `credo.ComponentName`, `credo.Ingress()` or `credo.OptionalReadiness()` fails the start, naming the binding ([dependency-injection guide](dependency-injection.md#what-each-registration-form-claims)).
- **Registrations of one resource agree on its tier.** A raw handle bound internal and a wrapper that embeds it bound with `credo.Ingress()` are refused, naming both, where the first registered decided the teardown's tier without a word.
- **Two registrations of one worker constructor** no longer panic: each builds its own value.
- **Nested validation.** A nested `Validate` with a pointer receiver runs on the field itself, so what it normalizes is kept, and a nil nested pointer checked with `validation.Field(&r.Address)` and no rules is valid where it panicked and answered 500 ([validation spec](../specs/validation.md)).

### What v0.25.0 deletes or renames

Each row is a compile error, or a log key or message the compiler cannot see, with its replacement:

| v0.24 | v0.25.0 |
| --- | --- |
| `app.Provide[T](NewT)` | `app.Provide(NewT)`, when `T` is the constructor's result type |
| `app.Provide[I](NewConcrete)`, where `I` is an interface the result implements | `app.Provide(NewConcrete)` with `app.Alias[I, *Concrete]()`, or a constructor declared to return `I`. The concrete binding plans the `Start` and `Ready` the interface hid, so a component that ran unstarted starts |
| `app.ProvideValue[I](v)` whose `I` hides `v`'s `Start`, `Ready` or ingress tier | `app.ProvideValue(v)` with `app.Alias[I, *V]()`, or `credo.Borrowed()` when the caller starts and stops `v` |
| A direct binding after an `Alias` of its type | Add `credo.Override()`, which replaces the alias, or drop the alias. `Alias` takes `credo.Override()` to point an interface at another type |
| `credo.Named(name)` | `credo.ComponentName(name)`, which names a component and is not a DI key; it now also applies to `Provide` and `ProvideValue` |
| `credo.Closer()` | `credo.CloseOnShutdown()` |
| `app.Finalize()` before the first `Resolve`, required | Optional: the first `Resolve` finalizes; keep `Finalize` as an early check that returns the whole report as an error |
| Late registration: `called after Finalize; make every registration before Finalize, …` | `called after the container was finalized by <trigger> at <file:line> (called from …); register before that call` |
| `called before Finalize; call Finalize first …` | Gone: `Resolve` finalizes |
| `websocket.New(app.NewInfra("websocket"), cfg)` | `websocket.New(cfg)`: the server settles its logger in `Start` |
| A logger of the WebSocket server's own | `websocket.Config{Logger: l}` |
| `app.Manage(ws, credo.Ingress())`, `app.ProvideValue(ws, credo.Ingress())` | `app.Manage(ws)`, `app.ProvideValue(ws)`: the type declares the tier, and the option stays accepted |
| An internal component that depends on a WebSocket server registered without `credo.Ingress()` | Declare the dependent ingress with `credo.Ingress()`, or split it so that what internal components use is an internal part; `Finalize` names both |
| `service=Service`, written by a constructor's `Infra` logger | `component=*users.Service`, the binding's type as the lifecycle prints it, or its `credo.ComponentName` name |
| `service=name`, written by `app.NewInfra(name)` | `component=name` |
| `component=net/http`, written by the `http.Server.ErrorLog` bridge | `module=net/http` |
| WebSocket connection logs | Gain `module=websocket`; with `Config.Logger` they go there, with `connection_id` and `request_id` |
| `app.ProvideValue(db)` with `store.Register[*sqldb.DB](app)` | `app.ProvideValue(db)`: the binding pings the database at start, reports it in `/ready` and shuts it down after its consumers |
| `store.Register`, `store.RegisterOption` | Deleted: the binding alone does what they did |
| `store.WithName("primary")` | `credo.ComponentName("primary")` on the binding |
| `store.WithPingTimeout(d)` | `sqldb.Config{PingTimeout: d}`, the `ping_timeout` key when the configuration is decoded |
| `store.DefaultPingTimeout` | `sqldb.DefaultPingTimeout` |
| A database bound without `store.Register`, absent from `/ready` | It gates readiness; bind it with `credo.OptionalReadiness()` if it must not |
| A borrowed database pinged by `store.Register` | Pinged by its owner |
| `(*sqldb.DB).StoreRegistrationWarningCodes()` | Deleted: `Start` logs the code; a program that runs the database without an App calls `db.Start(ctx)` or reads `db.Stats().MaxOpenConnections` |
| Pool warning with `component=store` and `store=<name>` | `module=sqldb`, with `component=<name>` on the App's logger |
| A store adapter of the application's own implementing `store.Lifecycle` | It implements `Start`, `Ready` and `Shutdown`, as every component does; `store.Lifecycle` is deleted |
| `/ready` entry `credo.store_name_conflict.<i>` | `credo.contributed_name_conflict.<i>` |
| `import "github.com/credo-go/credo/worker"` | Deleted, with no aliases: the types live in the root package |
| `workers := worker.Use(app)` | `workers := app.Workers()`, the App's one registry, not bound in DI |
| A module that takes a `*worker.Supervisor` | It takes the `*credo.App` it already receives and calls `app.Workers()` |
| `worker.Supervisor` | `credo.Workers` |
| `worker.Worker` | `credo.Worker` |
| `worker.Func(fn)` | `fn`: a `func(context.Context) error` is a worker as it is |
| `worker.Info` | `credo.WorkerInfo`, whose JSON is unchanged |
| `worker.Kind`, `worker.KindContinuous`, `worker.KindScheduled` | `credo.WorkerKind`, `credo.WorkerKindContinuous`, `credo.WorkerKindScheduled` |
| `worker.Status`, `worker.StatusPending`, `StatusRunning`, `StatusBackoff`, `StatusWaiting`, `StatusStopped`, `StatusFailed` | `credo.WorkerStatus`, `credo.WorkerStatusPending`, `WorkerStatusRunning`, `WorkerStatusBackoff`, `WorkerStatusWaiting`, `WorkerStatusStopped`, `WorkerStatusFailed` |
| `worker.Restart{…}` | `credo.RestartPolicy{…}` |
| `worker.RunInfo`, `worker.CurrentRun(ctx)` | `credo.WorkerRun`, `credo.CurrentWorkerRun(ctx)` |
| `worker.ErrRunTimeout` | `credo.ErrWorkerRunTimeout`, with the same message |
| `worker.ContinuousConfig`, `worker.ScheduledConfig`, `worker.Schedule`, `worker.ParseSchedule`, `worker.DefaultMinRestartDelay`, `worker.DefaultMaxRestartDelay` | The same names in the root package: `credo.ContinuousConfig`, `credo.ScheduledConfig`, `credo.Schedule`, `credo.ParseSchedule`, `credo.DefaultMinRestartDelay`, `credo.DefaultMaxRestartDelay` |
| Two supervisors, each with its own `Snapshot` and `Lookup` | One registry: `Snapshot` and `Lookup` cover every worker of the App |
| Worker log key `worker=<name>` | `component=worker:<name>`; `module=worker`, `kind`, `status` and `reason` stay |
| Registration panic `worker: Continuous("x"): …` | `credo: Workers.Continuous("x"): …`, and likewise for `Scheduled` |
| `app.Provide(NewT)` with `workers.ContinuousProvided[T](name, cfg...)`, when nothing else resolves `T` | `app.Workers().Continuous(name, NewT, cfg...)`, and the `Provide` call goes, since kept beside the constructor it builds a second value; likewise `ScheduledProvided` becomes `Scheduled(name, expr, NewT, cfg...)` |
| A provided worker that other code also resolves | Keep `app.Provide(NewT)` and register the method expression of its `Run`: `app.Workers().Continuous(name, (*T).Run, cfg...)`. The binding keeps its options, and the loop's tier comes from the worker's configuration |
| Resolving a worker its constructor builds | Not injectable: bind it as above, or share a handle that the code handing it work also holds |
| `testutil.WithOverride` on a provided worker's type | Unchanged for a bound worker; for a worker its constructor builds, override its dependencies |
| A worker whose built value owns state and uses an ingress component | Split it: `Finalize` fails with the path |
| `app.Manage(NewT, credo.ComponentName(…))`, `credo.Ingress()` or `credo.OptionalReadiness()`, whose value holds a bound resource | Move the option to the binding, or hold the handle in a named field so that the value is a resource of its own |
| A raw handle and a wrapper that embeds it, registered in different tiers | One tier for both, or a wrapper that holds the handle in a named field |
| `middleware.NewRateLimiter(cfg).Middleware()`, `middleware.RateLimit(cfg)` | `ratelimit.Middleware(cfg)`, in `github.com/credo-go/credo/middleware/ratelimit`; one value attached to several routes spends one budget |
| `middleware.RateLimitConfig` | `ratelimit.Config`, whose `Skipper` keeps the type `middleware.Skipper` |
| `middleware.DefaultRateLimitConfig()` | `ratelimit.DefaultConfig()` |
| `app.Manage(limiter)` of a `*middleware.RateLimiter` | Nothing: delete the call with the variable it named; the rate limiter is not a component |

### What an application keeps (v0.25.0)

- a database is bound, not registered: `app.ProvideValue(db)`;
- a WebSocket server is handed over with `app.Manage(ws)`;
- workers are registered through `app.Workers()`, as a value, a run function or a constructor;
- `Provide` takes the constructor, and an interface is bound with `Alias`;
- `Finalize` is optional.

## v0.24.0 at a glance

### Check first: changes that compile unchanged

The compiler finds every removed or renamed call. These changes it does not find — code that builds as before and behaves differently — so look for them before anything else:

- **Rule errors are internal unless they are violations.** A validation rule that returns a plain error — `errors.New` in a `validation.By` function, a database error from a lookup — now yields a 500 with the text only in the log (a fault keeps its status, `store`'s unavailable kind a 503), not a 422 violation; a client message is `validation.NewError(code, message)`. An `HTTPError` you construct wins over validation errors it wraps: `NewHTTPError(409, …).WithInternal(vErrs)` renders 409. A `Validate` that returns one `*validation.ValidationError` renders 422 instead of 500 ([Error handling](#error-handling)).
- **Ignored registration errors are panics now.** `Provide`, `ProvideValue`, `Alias`, `BindMany` and `UseI18n` return nothing; a call whose error was discarded compiles and now panics on misuse at its line. `Resolve` before `Finalize` panics instead of returning an error ([Sequential bootstrap](#sequential-bootstrap)).
- **`OnStart`'s context ends with the hook.** A goroutine launched from a hook on its context stops as soon as the hook returns; background work becomes a component or a worker ([Lifecycle components](#lifecycle-components)).
- **A shutdown during startup interrupts the start.** The running `Start` sees its context cancelled, nothing further starts, what was built is rolled back, and `Run` returns nil after a clean rollback.
- **A value whose binding's type shows `Start` and `Shutdown` is started by the App.** An `OnStart` hook that also starts it makes it start twice; delete the hook. Behind an interface that hides either method, the hook alone starts it.
- **A resource several bindings hold is shut down once** — one pointer under several bindings, or a wrapper and the handle it embeds — through the holder registered first and after the consumers of all of them, where each binding used to shut it down, the first time before its consumers.
- **Drain order.** Every worker's context used to be cancelled as the drain began, before the HTTP drain. A continuous worker now stops after the HTTP drain and before the components it depends on; a scheduled worker stops beside the HTTP drain. The former `OnShutdown` hooks, now `OnStop`, run before their tier's components instead of after DI teardown ([Workers as components](#workers-as-components)).
- **Workers report differently.** The `worker` configuration section is no longer read; the snapshot's JSON fields, the statuses and the log messages changed, so dashboards, alerts and log queries must follow ([Workers as components](#workers-as-components)).
- **`ServeHTTP` refuses an App with anything to start until `App.Start` has succeeded.** An App served by an external `http.Server` or by `httptest` that has a component with `Start` or `Ready`, a start hook, a registered store, `UseI18n`, a WebSocket server or a worker panics on its first request until `app.Start(ctx)` — `testutil.Start(t, app)` in tests — has run.
- **Stores are pinged in the start phase.** A store's constructor and ping run in the start phase, not at registration; `/ready` has no store checks before the App has started; an override of a store's binding is what the App pings, reports and shuts down ([Stores, WebSocket and i18n](#stores-websocket-and-i18n)).
- **`StatusHandler` panics for any code but 404 and 405**, so a registration that never ran now fails at startup ([Router](#router)).
- **`testutil.WithOverride[T]` panics without an earlier binding of `T`**; adding a binding is `testutil.WithWiring`'s job.

### What v0.24.0 deletes

Each of these is a compile error, so the compiler finds every call site; the tables below give each replacement:

- the teardown mechanisms `OnPreDrain`, `OnDrain` and `OnShutdown`, `credo.Shutdowner` and `*credo.DIShutdownError` ([Lifecycle components](#lifecycle-components));
- `Replace` and `MustReplace`, `ProvideProtectedValue`, `ProtectBinding`, `AdoptValue`, `CanProvideValue`, and the registration twins `MustProvide`, `MustProvideValue`, `MustAlias` and `MustBindMany` ([DI surface](#di-surface));
- the error returns of every registration call ([Sequential bootstrap](#sequential-bootstrap));
- `store.Register`'s value argument, `store.Registry` with `HealthAll`, `store.WithLifecycle`, `store.WithCallerOwnedLifecycle`, `store.LifecycleIdentityProvider`, and `websocket.Use` ([Stores, WebSocket and i18n](#stores-websocket-and-i18n));
- `worker.Register`, `worker.RegisterProvided` and their `Must` forms, `worker.Pool`, `worker.Option`, `worker.Config`, `worker.ReadinessPolicy` and the run-context accessors `RunID`, `WorkerName` and `ScheduledAt` ([Workers as components](#workers-as-components)).

### What an application keeps (v0.24.0)

After the deletions, an application writes four things it did not write before:

- **Two calls for a store:** a binding where its ownership is decided — `app.ProvideValue(db)`, or `app.Provide[R](ctor)`, with `credo.Borrowed()` for a handle the caller shares — and `store.Register[R](app)`, which names that binding as a store.
- **A registration for a WebSocket server:** `app.Manage(websocket.New(app.NewInfra("websocket"), cfg), credo.Ingress())`, or a binding with `credo.Ingress()` when controllers take the server as a dependency.
- **The concrete type and an `Alias` for a value the App starts or checks:** `app.Provide[*KafkaPublisher](NewKafkaPublisher)` with `app.Alias[OrderEvents, *KafkaPublisher]()`, because `Start`, `Ready` and the tier are planned from the binding's type.
- **`App.Start` and the server's own drain for an App served by an external server:** `app.Start(ctx)` before the server accepts, `testutil.Start(t, app)` in tests, and the server's own `Shutdown` before `app.Shutdown`.

Everything else in v0.24.0 removes API, states the bootstrap order, or adds an option an application reaches for only when it needs one — `credo.Ingress()`, `credo.Borrowed()`, `credo.Closer()`, `credo.Override()`, `app.Manage`. The release is judged by this list as well as by what it deletes.

## Bootstrap and DI

> **v0.25.0:** the tables in this section and its subsections name the calls of the release that introduced them. v0.25.0 makes `Finalize` optional — the first `Resolve` finalizes, where it panicked — drops `Provide`'s type argument, renames `credo.Named` and `credo.Closer` to `credo.ComponentName` and `credo.CloseOnShutdown`, deletes `store.Register`, and lets `app.Manage(ws)` place a WebSocket server in the ingress tier without `credo.Ingress()`: [v0.25.0 at a glance](#v0250-at-a-glance) gives the current calls.

Complete dependency registrations, store/worker setup and overrides before an explicit, error-checked `app.Finalize()`. Then resolve controllers/services, capture hook dependencies and bind routes or DI-backed renderers. HTTP setup remains open until shared preparation or shutdown admission. Run's implicit Finalize remains an idempotent safeguard; it cannot precede a Resolve that the composition root has already executed.

| Before the DI minor | Now |
| --- | --- |
| Resolve before Run, relying on Run to Finalize | Add an error-checked Finalize after all DI writes and before the first Resolve; Resolve before Finalize panics ([Sequential bootstrap](#sequential-bootstrap)) |
| Resolve just to test optional registration | Use the non-resolving `Has[T]`; it is a snapshot, not a reservation or health check |
| ProvideFactory/MustProvideFactory | Use typed constructors with explicit dependency parameters |
| Preprovided Registry constructor adopted during registration | Nothing to provide: v0.24.0 removed `store.Registry` ([Stores, WebSocket and i18n](#stores-websocket-and-i18n)) |
| Replace / MustReplace | `credo.Override()` on `Provide` or `ProvideValue` before `Finalize`, or `testutil.WithOverride` in tests ([DI surface](#di-surface)) |
| Resolve from a drain hook | Resolve during bootstrap and capture the dependency in the hook closure |

Do not recover a constructor panic to retry resolution: the container stores a typed terminal failure. MustResolve still panics on error, with `*credo.DIPanicError` as its payload. Closing/closed resolution matches `credo.ErrDIClosed`; teardown failure is inspectable as `*credo.LifecycleError` (`*credo.DIShutdownError` before v0.24.0) through App-level error joins. Only construction finishing after the shutdown context ends gets the separate bounded late cleanup; ordinary `Shutdown` calls share the drain deadline.

Building-state Shutdown provides cleanup even after a failed Finalize. It does not drain an externally owned http.Server. Owners of such servers must stop admission and complete the HTTP drain before `App.Shutdown`. Stopped ServeHTTP returns the framework's default 503 envelope without custom renderers, i18n callbacks or DI access; it does not restart App.

### Sequential bootstrap

**Implemented (v0.24.0).** Bootstrap is sequential and every mistake has one phase: registration panics on misuse at the line that made it, and `Finalize` reports the whole graph at once. The [bootstrap spec](../specs/container.md#documented-order) gives the documented order.

| Before v0.24.0 | Now |
| --- | --- |
| `Provide`, `ProvideValue`, `Alias` and `BindMany` returned an error | They return nothing and panic on misuse — a constructor of the wrong shape, a duplicate binding, types that do not fit, a call after `Finalize` or after shutdown began — with a message naming the call and the remedy (`credo: App.Provide[*app.X]: *app.X is already registered; …`). Drop the error checks: `if err := app.Provide[T](ctor); err != nil` no longer compiles |
| `Resolve`, `ResolveAll` and their `Must` forms before `Finalize` returned a "not finalized" error | They panic (`credo: App.Resolve[T]: called before Finalize; call Finalize first …`); call `Finalize` first. After a failed `Finalize`, `Resolve` returns the `Finalize` error |
| Registration calls were safe for concurrent use | Registration comes from the goroutine that builds the App, before it runs, and is not safe for concurrent use; an application that registers from several goroutines serializes the calls itself |
| `Finalize` reported the first cycle it found and each missing dependency by its direct consumer (`di: Validate: … dependency … is not registered`) | It reports every missing dependency with its whole path from the registration that needs it (`di: missing dependency: *app.OrderService → *app.PaymentClient → *http.Client (not registered); provide *http.Client before Finalize`) and every cycle, joined in registration order with the same text on every run. Code or tests that match the old `di: Validate: …` text must change |

The `Must*` registration twins (`MustProvide`, `MustProvideValue`, `MustAlias`, `MustBindMany`) are removed, since the plain calls panic on misuse themselves ([DI surface](#di-surface)). A `Use*` call after preparation or shutdown panics as before; it no longer synchronizes with a preparation running on another goroutine.

### Lifecycle components

**Implemented (v0.24.0).** The App has one lifecycle abstraction, the component: a value with `Shutdown(ctx) error` whose teardown the App owns, started when its binding's type shows `Start`, asked for readiness when it shows `Ready`, and stopped in two tiers — ingress beside the HTTP drain, internal after it — after its consumers. Every former teardown mechanism became a component or a hook that is one. The [lifecycle spec](../specs/lifecycle.md) is the contract; the [dependency-injection guide](dependency-injection.md#shutdown-and-lifecycle) and the [deployment guide](deployment.md#shutdown-and-readiness) show the calls. The rows that compile unchanged come first; the last four are compile errors.

| Before v0.24.0 | Now |
| --- | --- |
| The `OnStart` context lived for the App and was cancelled at shutdown; goroutines launched from a hook watched it | The context ends when the hook returns, or earlier when a shutdown interrupts the start phase. Background work belongs to a component whose `Start` launches it on `context.WithoutCancel(ctx)` with its own cancel and whose `Shutdown` stops it, or to a worker |
| A shutdown requested during startup waited for every `OnStart` hook, then logged "server started" and drained; `Shutdown` refused a starting App | The first signal, the context of `RunContext` or `ServeContext`, or `Shutdown` — now accepted while starting — interrupts the start: the running `Start` sees its context cancelled, nothing further starts, what was built is rolled back, and `Run` returns nil after a clean rollback |
| A type that shows `Start` and `Shutdown`, started by an `OnStart` hook | The App starts it itself, so the hook would start it a second time: delete the hook. Behind an interface that hides either method, the hook alone starts it |
| `ServeHTTP` (an external `http.Server`, `httptest`) never ran `OnStart` hooks | An App with anything to start — a component with `Start` or `Ready`, a constructor handed to `Manage`, a start hook, workers — panics in `ServeHTTP` until `app.Start(ctx)` (`testutil.Start(t, app)` in tests) has succeeded. The server's owner still drains it before `app.Shutdown` |
| A Credo App mounted with `Mount` served on its own | Hand it to the parent with `parent.Manage(child)`, so it starts in the parent's start phase and stops after the parent's HTTP drain; otherwise its first request panics when it has anything to start |
| One pointer held by several bindings was shut down once per binding, the first time before its consumers | A resource — one pointer, or a wrapper and the handle it embeds — is shut down once, through the holder registered first and after the consumers of all of them. A wrapper that embeds a handle which identifies itself, such as `*sqldb.DB`, and releases state of its own in its own `Shutdown` holds the handle in a named field instead |
| `testutil.WithOverride[T]` added a binding when the wiring had none | It replaces through `credo.Override()` and panics without an earlier binding; add bindings with `testutil.WithWiring` |
| A value behind an interface that the App should start or ask for readiness | Provide the concrete type and `Alias` the interface: `Start`, `Ready` and the tier are planned from the binding's type |
| An internal component could depend on anything | An internal component that depends on an ingress one, directly or through bindings that are not components, fails `Finalize` with the path: declare it ingress, or split the ingress component so that what internal components use is an internal part |
| `credo.Shutdowner` | `credo.Component`, with the same method set: every `Shutdowner` is a component without a change. Rename the assertions (`var _ credo.Component = (*X)(nil)`) |
| `*credo.DIShutdownError` | `*credo.LifecycleError`, which covers a failed or interrupted start as well as the drain; each entry names the component, its tier, its phase and its outcome (`failed`, `panicked`, `abandoned`, `kept_open`) |
| `app.OnShutdown(fn)`, run LIFO after DI teardown | `app.OnStop(fn)`, which runs LIFO **before** its tier's components stop, so it can still use them. A hook that closed a resource becomes a component — a DI binding with `Shutdown` or with `credo.Closer()`, whose consumers' constructor parameters order its shutdown after theirs, or, for a value nothing in DI uses, a value handed to `app.Manage`, which has no dependency edges — or `app.OnStop(fn, credo.Ingress())` when it must run alongside the HTTP drain; cleanup that must outlive every component moves after `Run` returns |
| `app.OnPreDrain(fn)` / `app.OnDrain(fn)` | An ingress component (`credo.Ingress()` on its binding, or `app.Manage(v, credo.Ingress())`), which stops concurrently with the HTTP drain and before the internal components it uses, or `app.OnStop(fn, credo.Ingress())` |

New with it: the capabilities `credo.Starter`, `credo.Readier` and `credo.ResourceIdentifier`; `credo.Tier`; the registration options `credo.Ingress()`, `credo.Borrowed()`, `credo.Closer()`, `credo.Override()` and `credo.Named(name)`; `app.Manage`, `app.Start` and `testutil.Start`; the panic phase `credo.DIPanicStart`; and the per-component `Ready` aggregated by `/ready` from the values the start phase built.

### Stores, WebSocket and i18n

**Implemented (v0.24.0).** Stores, the WebSocket server and i18n run on the lifecycle components. A store is a DI binding that `store.Register` names and the start phase pings; the WebSocket server is an ingress component the application registers; `UseI18n` validates at the call and reads its locale files in the start phase. Registering any of them does no I/O and panics on misuse, and what touches the outside world fails the start instead. The [store spec](../specs/store.md#lifecycle-of-a-bound-database) is the contract; the [data-access guide](data-access.md#what-a-bound-database-does), the [WebSocket guide](websocket.md#registering-the-server) and the [localization guide](localization.md#enabling-i18n) show the calls. The rows that compile unchanged come first.

| Before v0.24.0 | Now |
| --- | --- |
| An App served through `ServeHTTP` that used stores, i18n or WebSocket needed no start | Each has start work — the store ping, the locale read, opening WebSocket admission — so the App is started with `app.Start(ctx)` (`testutil.Start(t, app)` in tests) before `ServeHTTP`, which panics until then |
| `/ready` resolved each store from DI per request | Each store's readiness probe is built once, when the start phase pings it; before the App has started there are no store checks |
| A registered store's binding could not be replaced | The binding is an ordinary one: `credo.Override()` or `testutil.WithOverride[R]` replaces it before `Finalize`, and the override is what the App pings, reports and shuts down |
| A second registration of one database — the raw `*sqldb.DB` and a wrapper that embeds it — was refused | Both registrations are accepted; each is pinged and reported under its own name, and the database is still shut down once, after the consumers of every holder |
| `store.Register[R](app, value, opts...) error`, which pinged the store at the call and published a protected binding | Bind the store where its ownership is decided — `app.ProvideValue(value)`, or `app.Provide[R](ctor)`, with `credo.Borrowed()` on `ProvideValue` for a handle the caller shares — and name that binding with `store.Register[R](app, opts...)`, which takes no value and returns nothing. Drop the error check and the cleanup on its failure: misuse panics at the call, a registration whose `R` has no binding fails `Finalize`, and the ping runs in the start phase, where a failure is a start failure that rolls back what was built. A store constructor now runs in the start phase, not at registration |
| `store.WithLifecycle(lc)` with `store.WithCallerOwnedLifecycle()` | A wrapper type that implements `store.Lifecycle` around the value, bound and registered by its type; caller ownership is `credo.Borrowed()` on the wrapper's `ProvideValue` |
| `store.LifecycleIdentityProvider` on a named-field wrapper | `credo.ResourceIdentifier`, with the same method `ResourceIdentity() any`; a wrapper that embeds `*sqldb.DB` inherits it |
| `store.Registry` and `Registry.HealthAll` | Removed, with no replacement API: `/ready` reports every registered store |
| `websocket.Use(app, cfg)` | `ws := websocket.New(app.NewInfra("websocket"), cfg)` and `app.Manage(ws, credo.Ingress())`, or a binding with `credo.Ingress()` when controllers take the server as a dependency. A server that was never started refuses every upgrade with a 500 and logs the missing registration |
| `if err := app.UseI18n(cfg); err != nil { … }` | `app.UseI18n(cfg)`: it returns nothing and panics on misuse — more than one config, `Dir` with `DirFS`, an `i18n` section that does not decode, an invalid `Default` or programmatic message, a second call. Locale files are read in the start phase, and a missing explicit source or a malformed file fails the start |

From the binding on, the App owns a store bound with `ProvideValue`: a composition root that fails before `Run` releases it with `app.Shutdown(ctx)`, which tears down an App that never ran, instead of closing the value itself.

### DI surface

**Implemented (v0.24.0).** The container's public surface is seven methods and `Finalize`: `Provide`, `ProvideValue`, `Alias`, `BindMany`, `Has`, `Resolve` and `ResolveAll`, with `MustResolve` and `MustResolveAll` kept as resolve conveniences. Nothing framework-owned is bound in the container any more — stores are named through the store registry and workers are components — so the methods that existed to defend framework values in it are removed, and so are the registration twins that duplicated calls which already panic. The [container spec](../specs/container.md) is the contract and the [dependency-injection guide](dependency-injection.md#replacing-a-binding) shows the calls.

Every change here is a compile error, so the compiler finds each call site:

| Before v0.24.0 | Now |
| --- | --- |
| `app.Replace[T](v)`, which returned the superseded instance and handed its shutdown to the caller, and `app.MustReplace[T](v)`, with its `credo: Replace superseded a component; the caller now owns its shutdown` Warn line | `app.ProvideValue[T](v, credo.Override())` (or `app.Provide[T](ctor, credo.Override())`) before `Finalize`, or `testutil.WithOverride[T](v)` in tests; both panic when there is no earlier binding of `T`. Nothing is handed back: an override's replaced value never becomes the App's, so the App neither starts nor shuts it down, and a value the caller built and then overrode is the caller's to release |
| A `Replace` after `Finalize`, which returned an error | No equivalent: after `Finalize` the graph is fixed. Move the override before `Finalize` |
| `app.ProvideProtectedValue[T](v)`, `app.ProtectBinding[T](...)`, `app.AdoptValue[T](validate)` | No successor: they existed for framework integrations that bound their infrastructure in the container, and nothing framework-owned is bound any more. An application that used them for its own values binds them with `app.ProvideValue[T](v)`; a binding is now always replaceable with `credo.Override()` |
| `app.CanProvideValue[T]()` | `app.Has[T]()` for the presence question — it never constructs and reserves nothing — or simply `app.ProvideValue[T](v)`, which panics on a duplicate |
| `app.MustProvide[T](ctor)`, `app.MustProvideValue[T](v)`, `app.MustAlias[I, T]()`, `app.MustBindMany[I, T]()` | `app.Provide[T](ctor)`, `app.ProvideValue[T](v)`, `app.Alias[I, T]()`, `app.BindMany[I, T]()`: the plain call panics on misuse |

## Error handling

**Implemented (v0.24.0).** Both changes compile unchanged and change the status a client receives, so look for them first. The rules are recorded in [ADR-011](../adr/011-validation-strategy.md#rule-errors-client-messages-and-internal-failures) and [ADR-009](../adr/009-handler-and-error-handling.md#an-explicit-status-wins); the [error-handling guide](error-handling.md#rule-errors-client-messages-and-internal-failures) shows them.

| Before v0.24.0 | Now |
| --- | --- |
| Any error a validation rule returned — `errors.New("must be a 2-letter code")` in a `validation.By` function, or a database error from a lookup inside a rule — became a 422 violation with code `invalid` and the error's text as its message, and validation went on with the other fields | Only a `*validation.ValidationError` or `validation.Errors` is a violation. Any other error is internal: validation stops, and the error leaves `Validate` and `BindBody`/`BindQuery` unchanged, so the pipeline renders a plain error as a 500 with the default message (its text is logged, never rendered) and a fault with its mapped status (`store`'s unavailable kind → 503). A rule whose message is meant for the client returns `validation.NewError(code, message)` |
| `credo.NewHTTPError(409, "tenant_conflict").WithInternal(vErrs)` rendered 422 `validation_failed`, because classification looked for validation errors anywhere in the chain first | An explicitly constructed `HTTPError` decides the status: it renders 409 `tenant_conflict`, and the wrapped errors stay in `ErrorInfo.Err`. Validation errors that no `HTTPError` wraps still render as 422 |
| A `Validate` method that returned a single `*validation.ValidationError` (not `validation.Errors`) rendered a 500 | It renders 422 `validation_failed` with one violation |

Migration: search for `validation.By(` and for `Validate(value` methods of custom rules, and turn each `errors.New`/`fmt.Errorf` that carries a message for the client into `validation.NewError("<code>", "<message>")`, choosing a lowercase snake-case code; keep returning a plain error where the failure is the server's. A client or test that expected 422 `invalid` for such a message now sees the new code. Search for `WithInternal(` around validation errors: the status you constructed is now the one sent.

## Built-in HTTP features

**Implemented (HTTP minor, 2026-09-05).** The [HTTP features spec](../specs/http-features.md) is the contract; the [middleware guide](middleware.md#framework-features-not-middleware) shows the new calls.

| Before the HTTP minor | Now |
| --- | --- |
| Recovery on; optional `middleware.Recover` configuration | `WithRecoverConfig(cfg)` configures default-on root recovery; `WithoutRecover()` wins |
| Default RequestID and `WithoutRequestID` | Explicit `app.UseRequestID(cfg...)`; omit it to disable |
| Default AccessLog, `WithoutAccessLog` and the `WithAccessLog*` field helpers | Explicit `app.UseAccessLog(cfg...)` with one `AccessLogConfig{Logger, MinLevel, Skipper, ResultFilter}` |
| `middleware.RequestID` / `middleware.AccessLog` / `middleware.GetRequestID` | Root `Use` registration and `ctx.RequestID()`; access policy through `MetaAccessLog` and config filters |
| `middleware.Compress` / `middleware.Decompress` | `app.UseCompress(cfg...)` / `app.UseDecompress(cfg...)` with root `CompressConfig` / `DecompressConfig` |
| `SetErrorRenderer` / `SetSuccessRenderer` | `app.UseErrorRenderer(renderer)` / `app.UseSuccessRenderer(renderer)`, one successful installation |
| `UseI18n` adding Global middleware | Keep `UseI18n`; custom `Detect` takes `*Context` instead of `*http.Request` and resolves lazily on first use |

Optional features have no parallel constructor/setter/Enabled route. Evaluate external enable flags in application bootstrap. Use-call order does not choose execution order. `WithoutRecover` wins over recovery configuration regardless of option order. Foundational logger, raw config, server, TLS and timeouts stay constructor settings.

AccessLog off does not disable framework or application diagnostics. Logger filtering and access record selection remain separate; WithDebug does not set the slog minimum level. To preserve request correlation and access records, explicitly enable both features. From v0.21.0 the `credo: server started` line lists the features in effect (`"features":["recover","request_id","access_log"]`); the reliable check is still a request to an ordinary route whose response carries a request ID and produces an access record with the same ID.

Scoped recovery is removed. Applications needing their own route policy can author ordinary middleware; Credo does not keep duplicate compatibility wrappers. CORS, CSRF, Secure, Timeout, RateLimit, Rewrite and ContractGuard retain their middleware APIs and ordering responsibilities.

Lazy locale: first Locale/translation access fixes the language and all translation paths share it. Custom Detect receives \*Context and can access the request or GetUser. It must handle missing auth data; Timeout/stdlib wrappers may restore a request without downstream auth data. To retain the authenticated language in later errors, call Locale after setting the user, before unwinding; an earlier read still wins. Empty/unresolvable detection uses the configured default. Recursive detection is misuse; panic/re-entry stores the fallback and never retries the detector. Recovery enabled uses the 500 path; disabled recovery propagates panic after cleanup.

A UseI18n that finds no conventional catalogs consumes registration as configured-but-inactive. Do not call it again as a fallback strategy. From v0.24.0 every call consumes the registration: misuse panics at the call, and an explicit source that is missing or invalid fails the start ([Stores, WebSocket and i18n](#stores-websocket-and-i18n)).

Decompress runs before Global middleware. Its Skipper uses the original request's method/path/ headers once, so raw-body webhook paths must be selected there, without route/auth dependencies. Rewrite does not repeat selection. Global body readers and binders see the same decoded stream under separate wire/decoded limits.

AccessLog bytes are post-compression accepted body bytes; headers/framing/TLS are excluded. Duration includes finalization and excludes the access filter/log write. Preserve actual committed status when transfer fails. With recovery enabled, an error-rendering failure falls back without callbacks; a post-response ResultFilter panic logs a diagnostic and skips that record. No failure after commitment appends a second JSON body; incomplete compressed output is aborted as required. These failure paths follow WithoutRecover for panics and always release request state.

## Router

**Implemented (router minor, 2026-09-05).** Path parameter names belong to the endpoint: `/customers/{id}` and `/customers/{customer_id}/timeline` coexist, and each handler reads its own names. Nothing needs to change in existing applications — every registration that was valid stays valid with the same captures — and routes that were previously split or renamed to satisfy the shared-name rule may now use their natural names. The `conflicting … parameter` registration panic no longer exists; the same method on the same name-stripped shape is a duplicate (`GET "/users/{name}" is already registered as "/users/{id}"`), and structural regex conflicts remain errors. `BuildURI` reads the selected route's names; host pattern semantics stay unchanged.

**Implemented (wire minor, 2026-09-05).** Route parameters are decoded once and reported in decoded form; constraints apply to the decoded value; parameters are single-segment; generation validates and escapes; malformed input has defined outcomes.

| Before the wire minor | Now |
| --- | --- |
| `RouteParam` returned the raw, still-encoded text: `%2F` stayed `%2F`, `%31` did not satisfy `[0-9]+` | Values are decoded once: `%2F` is `/` inside one segment, `%31` is `1` and satisfies numeric constraints, `%252F` is `%2F`, `+` stays `+`. A decoded value can contain `/` or `..`: check handlers that build file paths from parameters against [Route Parameters Are Not File Names](routing.md#route-parameters-are-not-file-names) |
| A regex could match a prefix of a segment, and a tail-bounded parameter could span a raw slash (`/{name}.json` matched `/a/b.json`) | Constraints apply to the whole decoded value and `{name}`/`{name:regex}` are single-segment; `{name...}` captures several segments |
| `BuildURI`/`BuildURL` pasted values verbatim | Values are validated against their constraints and percent-encoded per segment (`a/b` becomes `a%2Fb`); static text is written in its wire spelling; host labels are validated, never encoded; empty values, invalid UTF-8 and a value whose wire spelling shows its parameter's delimiter are errors |
| Static text with non-ASCII bytes (`/café`) was unreachable through `EscapedPath` matching | Static text and the request meet in one canonical form: every escape except those of reserved characters and `%` is decoded before matching, so any equivalent spelling routes alike, an encoded unreserved delimiter is the delimiter and an encoded reserved character (`%3B`) stays distinct from the literal |
| Invalid UTF-8 in a parameter was captured as bytes | 400 with the code `invalid_path_encoding` when no route matches |
| `ctx.Rewrite` took a decoded path | The target is a wire-form path; a malformed escape is an error |
| Mounted handlers received a decoded remainder with `RawPath` cleared | Mounted handlers receive `URL.Path` decoded plus `URL.RawPath` when the spellings differ |

Migration: remove any second `PathUnescape` of `RouteParam` values, pass raw values to `BuildURI`/`BuildURL` instead of pre-escaped ones, replace `{name:.+}` with `{name...}` where several segments were intended, and expect 400 rather than a captured byte sequence for invalid UTF-8. `OriginalPath` now reports the wire-form path. See [Encoded Parameter Values](../specs/router.md#encoded-parameter-values).

**Implemented (v0.23.0).** A catch-all matches an empty rest, and a pattern names each parameter once, as in chi and `net/http.ServeMux`.

| Before v0.23.0 | Now |
| --- | --- |
| `/files/{path...}` needed a non-empty rest: `/files/` answered 404, or a trailing-slash redirect to a sibling `/files` route | `/files/` reaches the catch-all with `path` = `""`; `/files` without a route of its own is redirected to `/files/` |
| A mount was reached through `/admin` only; `/admin/` was redirected to `/admin` (404 with `WithRedirectTrailingSlash(false)`), likewise `/t/acme/` under `Mount("/t/{tenant}", h)` | `/admin/` and `/t/acme/` reach the mounted handler as its root `/` |
| `Static("/static", fsys)` served `/static/` through a redirect to `/static` | `/static/` serves the index directly |
| `BuildURI("")` failed for a catch-all | It builds the prefix with the slash (`/files/`) |
| `/a/{id}/b/{id}` registered; `RouteParam("id")` returned the first capture, `RouteParams()` and `URLParam` the last | Registration panics with `duplicate parameter name "id"`, whether the repeat comes from the route, a group or mount prefix, a rewrite rule or a host pattern (`{a}.{a}.example.com`) |
| A host parameter without a name (`{}.example.com`) or with an empty constraint (`{org:}.example.com`) registered | `app.Host` panics |
| `Mount("/t/{_mount}", h)` registered and handed the child the rest of the path as `_mount`; under `Static`, a group prefix parameter `_static` was served as the file path | Both panic: `_mount` and `_static` are reserved for the captures `Mount` and `Static` add |

Migration: an application that registers both `GET /files` and `GET /files/{path...}` and relied on `/files/` being redirected to `/files` handles the empty capture in the catch-all handler, or registers `GET /files/` explicitly. A pattern that repeats a parameter name gives each capture its own name (`/a/{a_id}/b/{b_id}`).

**Implemented (v0.24.0).** `StatusHandler(code, h)` accepts 404 and 405 only, the two codes the router consults.

| Before v0.24.0 | Now |
| --- | --- |
| A handler for any other code — 403, 500 — was accepted and never called | Registration panics with `credo: App.StatusHandler(403): only 404 (http.StatusNotFound) and 405 (http.StatusMethodNotAllowed) are consulted; …`, so the App fails at startup |

Migration: delete such a registration rather than porting it — it never ran — and shape that response through `app.UseErrorRenderer`. A 404 or 405 handler that only returns the matching sentinel (`credo.ErrNotFound`, `credo.ErrMethodNotAllowed`) equals the default and can go in the same change. See [ADR-007](../adr/007-router-and-routing.md#status-handlers).

## Configuration

**Implemented (v0.23.0).** A YAML config file holds one document.

| Before v0.23.0 | Now |
| --- | --- |
| A file with a second document (`a: 1`, `---`, `b: 2`) loaded the first and dropped the rest without a word, a malformed second document included; `---`, `---`, `a: 1` loaded an empty config | The load fails with `yaml: a config file holds one document; found another after the first`, or with the second document's syntax error |
| A trailing `---` with nothing after it was ignored | It opens a second, empty document and fails the load |

Migration: merge a multi-document config into one mapping — the application ran with the first document only, so its values are the ones in effect — and delete a stray trailing `---`. A leading `---` and a closing `...` stay valid.

## Workers

> **v0.25.0:** the tables in this section name the calls of the `worker` package as v0.24.0 and earlier releases shipped them. v0.25.0 deletes the package: workers are registered through `app.Workers()` in the root package, `ContinuousProvided` and `ScheduledProvided` give way to a constructor or a run function passed to `Continuous` and `Scheduled`, `worker.Func(fn)` becomes `fn`, and the types are renamed (`worker.Info` → `credo.WorkerInfo`, `worker.Restart` → `credo.RestartPolicy`, …). [What v0.25.0 deletes or renames](#what-v0250-deletes-or-renames) maps every name.

### Workers as components

**Implemented (v0.24.0).** Each worker is a lifecycle component of the App. `worker.Use(app)` returns a supervisor — a registry and reporting object with no lifecycle of its own — whose registration method chooses the worker's kind, and each registration adds a component named `worker:<name>` that the App starts in the start phase and stops in its tier: a scheduled worker with the HTTP drain, a continuous worker after it and before the components it depends on. Configuration is a per-kind struct in code. The [worker spec](../specs/worker.md) is the contract and the [worker guide](worker.md) shows the calls.

None of these changes is a compile error, so check them first:

| Before v0.24.0 | Now |
| --- | --- |
| The `worker` configuration section (`worker.restart_delay`, `worker.max_restart_delay`) set the restart floor and cap of every continuous worker | No configuration section is read. A leftover `worker` section is ignored — it is an unknown key only when the application decodes the whole configuration tree into a struct under `WithStrictDecoding` — and every worker runs with the package defaults (3 s, 1 min) unless its registration sets `Restart{MinDelay, MaxDelay}`. Settings that must come from the environment move to a typed section the application reads itself ([worker guide](worker.md#configuration)) |
| Snapshot JSON: the configuration under `config` (`schedule`, `start_immediately`, `run_timeout`, `max_consecutive_failures`, `max_restarts`, `restart_delay`, `max_restart_delay`, `readiness`); `last_run`; `last_success` | `schedule` at the top level, and the resolved configuration under `continuous` (`tier`, `restart.disabled`, `restart.limit`, `restart.min_delay`, `restart.max_delay`, `unready_when_failed`) or `scheduled` (`tier`, `run_on_start`, `run_timeout`, `max_consecutive_failures`, `unready_when_failed`, `unready_until_first_success`, `unready_after_success_age`); `last_started_at`; `last_succeeded_at`. Dashboards and alert rules on the old field names must change |
| Statuses `idle`, and `waiting` for a continuous worker waiting to restart | `pending`, and `backoff` for a continuous worker waiting to restart; `waiting` belongs to a scheduled worker between activations |
| The log messages in the next table | Their new names; queries and alerts on the old messages must change |
| Registration misuse returned an error (`MustRegister` panicked), so a schedule read from configuration that did not parse was an error the composition root could handle | Every registration panics on misuse at the call, with a message naming the call and the remedy (`worker: Scheduled("report") after app.Finalize; register workers before Finalize`). Validate a schedule from configuration with `worker.ParseSchedule` first, which returns the error |
| Every worker's context was cancelled when the drain began, before the HTTP drain, and the pool was waited for beside it | A worker is stopped in its tier's turn. A scheduled worker stops beside the HTTP drain; a continuous worker now stops **after** it, once the handlers that enqueue its work have finished, and before the components it depends on. A continuous worker that consumes an external queue declares `Tier: credo.TierIngress` to stop with the listener |
| A `Run` that ignored cancellation past the drain deadline made the drain report a timeout, and the teardown went on to close the resources that `Run` still used | The worker's component is abandoned at the deadline: the components it depends on stay open, and `Run` (or `Shutdown`) returns a `*credo.LifecycleError` naming `worker:<name>` as abandoned |

| Log line before v0.24.0 | Now |
| --- | --- |
| `scheduled worker run failed` | `worker run failed` with `kind=scheduled`: one message for both kinds |
| `scheduled worker run completed` (Debug) | `worker run completed` (Debug), with `kind=scheduled` |
| `worker ticks skipped` | `worker activations skipped` |
| `worker exceeded max restarts` with `max_restarts` | `worker failed` with `reason=restart_limit` and `limit` |
| `worker exceeded max consecutive failures` with `max_consecutive_failures` | `worker failed` with `reason=failure_limit` and `limit` |
| `worker schedule has no future activation` | `worker failed` with `reason=schedule_exhausted` and `schedule` |
| `worker started` with `start_immediately=true` | `worker started` with `run_on_start=true` |

`worker failed` is now the single terminal line, also written with `reason=restart_disabled` for a worker registered with `Restart{Disabled: true}`. `worker started`, the continuous `worker run failed` line and `worker stopped` keep their messages and attributes.

The calls and types are renamed:

| Before v0.24.0 | Now |
| --- | --- |
| `worker.Register(app, name, w, opts...) error` / `worker.MustRegister(app, name, w, opts...)` | `workers := worker.Use(app)`, then `workers.Continuous(name, w, cfg)` or `workers.Scheduled(name, expr, w, cfg)`; both return nothing and panic on misuse |
| `worker.RegisterProvided[T](app, name, opts...) error` / `worker.MustRegisterProvided[T](app, name, opts...)` | `workers.ContinuousProvided[T](name, cfg)` or `workers.ScheduledProvided[T](name, expr, cfg)`: `T` is built in the start phase, after the components it depends on, and the worker stops before them. One `T` under two names panics |
| `WithSchedule(expr)` | the `expr` argument of `Scheduled` and `ScheduledProvided`; the method chooses the kind |
| `WithStartImmediately()` | `ScheduledConfig{RunOnStart: true}` |
| `WithRunTimeout(d)` | `ScheduledConfig{RunTimeout: d}` |
| `WithMaxConsecutiveFailures(n)` | `ScheduledConfig{MaxConsecutiveFailures: n}` |
| `WithMaxRestarts(n)` | `ContinuousConfig{Restart: worker.Restart{Limit: n}}` |
| `WithRestartDelay(d)` / `WithMaxRestartDelay(d)` | `worker.Restart{MinDelay: d}` / `worker.Restart{MaxDelay: d}` |
| `WithReadiness(worker.ReadinessPolicy{RequireFirstSuccess, FailWhenFailed, MaxSuccessAge})` | The configuration fields `UnreadyUntilFirstSuccess`, `UnreadyWhenFailed` and `UnreadyAfterSuccessAge`; `UnreadyWhenFailed` exists on both kinds, the other two on `ScheduledConfig` |
| `pool.Workers()` | `workers.Snapshot()`, or `workers.Lookup(name)` for one worker |
| `info.Config` | `info.Continuous` or `info.Scheduled`, the resolved configuration of the worker's kind (`info.Config.MaxRestarts` → `info.Continuous.Restart.Limit`, `info.Config.RestartDelay` → `info.Continuous.Restart.MinDelay`, `info.Config.StartImmediately` → `info.Scheduled.RunOnStart`, …); `info.Config.Schedule` → `info.Schedule` |
| `info.LastRun` / `info.LastSuccess` | `info.LastStartedAt` / `info.LastSucceededAt` |
| `worker.StatusIdle` | `worker.StatusPending`; `worker.StatusBackoff` is new |
| `worker.RunID(ctx)`, `worker.WorkerName(ctx)`, `worker.ScheduledAt(ctx)` | `run, ok := worker.CurrentRun(ctx)` with `run.ID`, `run.Worker` and `run.ScheduledAt` |
| `worker.DefaultRestartDelay` | `worker.DefaultMinRestartDelay` |
| `*worker.Pool` resolved from DI or taken as a constructor parameter | The supervisor `worker.Use` returns, passed on from the composition root; to inject it, bind it yourself with `app.ProvideValue(workers)` |
| `Pool.Start`, `Pool.Shutdown`, `worker.Option`, `worker.Config`, `worker.ReadinessPolicy` | Removed: the App starts and stops each worker's component, and the configuration is the two per-kind structs |

Before v0.24.0 the pool stopped every worker before DI teardown whatever the registration order, so a worker registered by value could use a database safely. Now a worker registered by value has no dependency edges — the App cannot see what a value uses — and takes its place in its tier by registration order alone. Register a worker that uses infrastructure in its provided form, so the App starts it after its dependencies and stops it before them. Workers also start as components now: in the start phase, before their tier's `OnStart` hooks, where the pool used to start inside an `OnStart` hook in registration order.

New with it: per-worker `Tier`, `Restart.Disabled` (the first failure is terminal), `Supervisor.Lookup`, `worker.CurrentRun` with `worker.RunInfo`, and the escalation of a terminal failure through a liveness check built on `Lookup` ([worker guide](worker.md#escalating-a-terminal-failure)).

### Worker contract and restart backoff

**Implemented (v0.20.0; restart backoff in v0.21.0).** The rows below name the v0.24.0 calls; [Workers as components](#workers-as-components) maps the v0.20.0 names to them, and [What v0.25.0 deletes or renames](#what-v0250-deletes-or-renames) maps them to the root package. One change compiles unchanged but behaves differently, so check it first:

> **A continuous worker whose `Run` returns nil while the application is running is restarted.** Before v0.20.0 it stopped silently. Now the early return is a failure: it is logged as `worker run failed` with `unexpected_exit=true` and the message `worker: Run returned nil before shutdown; a continuous worker must run until its context is cancelled`, and the worker is restarted with the restart backoff (3 s at first, up to a minute while failures repeat) — for ever, unless `Restart.Limit` is set. Before upgrading, look for continuous workers that return nil on purpose. Move finite work to `app.OnStart`, or end `Run` with `<-ctx.Done()` after the work is done. Returning nil after the context is cancelled remains a graceful stop.

| Before v0.20.0 | Now |
| --- | --- |
| `worker.Register(app, w, opts...)` | a name at registration: `workers.Continuous("name", w)` or `workers.Scheduled("name", expr, w)` |
| `worker.Func("name", fn)` | `worker.Func(fn)`; the name is passed at registration |
| `Name() string` on worker types | delete it (harmless if kept; it is no longer called) |
| constructing a worker by hand before `Finalize` because `Resolve` was unavailable | `app.Provide[T](constructor)` + `workers.ContinuousProvided[T]("name")` or `workers.ScheduledProvided[T]("name", expr)`; `T` is built in the start phase |
| continuous `Run` returns nil → the worker stops | restarted like a failure; see above |
| `WithMaxRestarts(N)`, `N > 0` → failed after N failures (N−1 restarts) | `Restart.Limit` N: the first run plus N restarts → failed after N+1 failures |
| `WithMaxRestarts(0)` → unlimited restarts | `Restart.Limit` zero: unchanged |
| `info.Kind == "scheduled"` | still compiles; prefer `worker.KindScheduled` |
| `info.Attempts` | `info.Restarts` (continuous) / `info.ConsecutiveFailures` (scheduled) |
| `worker.Attempt(ctx)` | removed; use `worker.CurrentRun(ctx)` for the run's identity, and the snapshot for counters |
| `LastSuccess` set when a continuous `Run` returned nil | `LastSucceededAt` is never set for continuous workers |
| a scheduled `Run` returning nil during shutdown stamps `LastSuccess` and resets `ConsecutiveFailures` | when shutdown cancellation came first, a graceful stop: `LastSucceededAt` and `ConsecutiveFailures` unchanged, status `stopped` |
| a graceful stop clears `LastError` | a graceful stop changes only the status; `LastError` keeps the most recent failure until a successful scheduled run clears it |
| a hand-rolled `context.WithTimeout` inside `Run` | `ScheduledConfig.RunTimeout`; a timed-out run is a failure even if it returns nil |
| `@every 0s`, a negative `@every` or `@every 1500ms` silently became one second | rejected: registration panics, and `worker.ParseSchedule` returns the error |
| `LastError` may contain a panic stack trace | never; the stack is the `stack` attribute of the failure log line |
| log `worker stopped during scheduled run`; continuous `worker stopped` only on some exit paths | exactly one `worker started` and one `worker stopped` (`reason=shutdown` or `reason=failed`) per worker |
| log `worker tick skipped`, one line per skipped activation | one `worker activations skipped` line per resumption with `skipped=N` |
| `restart` attribute of the continuous `worker run failed` line, counting failed runs (`1` on the first failure) | renamed `restarts`; equals `Info.Restarts` — restarts that actually started, so `0` on the first failure |
| names silently trimmed | surrounding whitespace and control characters are rejected |
| untagged (Go field name) JSON from the pool's snapshot | snake_case field names from `workers.Snapshot()`; empty `schedule`, `continuous`/`scheduled`, `last_started_at`, `last_succeeded_at` and `last_error` omitted |
| `worker.Definition` (exported, returned by no API) | removed; `info.Continuous` or `info.Scheduled` is the public view of a registration |
| a continuous worker reported `running` as soon as the pool started | `pending` until its first run is admitted |

New and unchanged behavior worth knowing while migrating: `RunTimeout` is scheduled-only; failure log lines carry `run_id` (equal to `worker.CurrentRun(ctx).ID`) and `duration`; a successful scheduled run logs `worker run completed` at Debug; the snapshot reports the resolved configuration, so a registration test can assert every worker's policy without running it.

v0.21.0 changes the wait between continuous restarts without a compile error. Both rows apply to workers registered without a restart policy too:

| Before v0.21.0 | Now |
| --- | --- |
| every restart waits the fixed restart delay (3 s by default) | the first restart waits `Restart.MinDelay`; repeated failures back off with jitter up to `Restart.MaxDelay` (1 min by default), and a run that lasted at least the cap resets the sequence. The same value in `MinDelay` and `MaxDelay` keeps a fixed delay |
| `WithMaxRestarts(N)` reaches `failed` after N fixed waits (15 s for N = 5 with the defaults) | the waits back off, so `failed` — and an `UnreadyWhenFailed` readiness drop — comes later: roughly 48–93 s for N = 5 with the defaults |

New with it: the restart cap (`Restart.MaxDelay`, `restart.max_delay` in JSON, default `DefaultMaxRestartDelay`) and the `next_restart_in` attribute of `worker run failed`. A `MinDelay` above one minute stays fixed unless a larger cap is set.

## Data access

**Implemented (v0.22.0).** `store/sqldb` requires Bun v1.3.0 (`github.com/uptrace/bun` and its three dialect modules) and pgx v5.11.0. Credo's curated API keeps its signatures: `SelectQuery.Limit`, `Offset` and `Page` take `int`, and `Count` returns `int`. What changes is visible where an application touches Bun directly — inside an `Apply` closure, through `Unwrap`, `Conn`, `RequireTx` or `Client()` — and in what the database accepts.

| Before v0.22.0 | Now |
| --- | --- |
| `bun.SelectQuery.Limit`/`Offset` take `int`; `Count` returns `int` | `int64`. An untyped constant (`q.Limit(10)`) compiles unchanged; an `int` variable needs `int64(n)`, and a `Count` result assigned to an `int` needs a conversion |
| curated `Limit`/`Offset` reject values outside the signed 32-bit range with `ErrInvalidLimitOffset`; `Page` rejects such windows with `ErrInvalidPageRequest` | every value reaches the database; `Page` executes any window whose offset fits in `int`. `ErrInvalidLimitOffset` is removed, so an `errors.Is` check against it stops compiling and can be deleted |
| a string argument carrying a NUL byte (`0x00`) is stored with the byte removed | on SQLite and PostgreSQL the statement fails with an error that `store.KindOf` leaves unmapped, and nothing is persisted; MySQL still strips the byte ([data-access guide](data-access.md#nul-bytes-in-strings)). Reject NUL at the validation boundary |
| two migration files with the same numeric prefix and different names: the later file silently replaced the earlier one's `Up`/`Down` | `migrate.Migrations.Discover` returns `migrate: duplicate migration ID …`, so `RegisterMigrations` is never reached |
| a top-level `WhereOr` on a soft-delete model (or next to `WherePK`) rendered `WHERE a OR b AND deleted_at IS NULL` | Bun parenthesizes the OR group: `WHERE (a OR b) AND deleted_at IS NULL`. Queries that relied on the old precedence change their result set |
| a `.tx.up.sql` migration whose COMMIT failed was marked applied | the COMMIT or ROLLBACK error reaches `Migrate` and the migration stays unapplied ([data-access guide](data-access.md#migrations)) |
| a `jsonb`, `bytea` or `numeric` column scanned into `map[string]any` on Go 1.27 with pgx deadlocked | scans normally (Bun v1.3.0 fix, pinned by a real-PostgreSQL subtest) |
| pgx returns a text-format `timestamptz` in a location that differs from the binary format's | both formats return `time.Local` (or the codec's `ScanLocation`); `date` rejects impossible dates such as `2024-02-30` instead of normalizing them |

Unchanged in v0.22.0: `SelectQuery.Clone` still restores the execution state Bun's `Clone` used to omit (v1.3.0 copies it itself; the compatibility layer is removed in a later minor), zero and negative `Limit`/`Offset` values still omit the clause, and standalone `Having` or a direct compound root still returns `ErrUnsupportedCountQuery` from `Count` and `Page`. v0.26.0 supersedes the `Clone` sentence: the compatibility layer is gone and `Clone` forwards to Bun's own, and `Count` and `Page` run only on a query the wrapper can prove ([v0.26.0 at a glance](#store-and-sqldb)).

## Examples and downstream impact

The [example migration map](../../examples/README.md) identifies runnable changes by release. SaaS finalizes before resolving TenantService (DI minor), enables RequestID/AccessLog explicitly and installs compression with `UseCompress` instead of global middleware (HTTP minor). For v0.24.0, SaaS follows the documented bootstrap order, with its feature mounts and satellite registrations between `Provide` and `Finalize`. For v0.25.0, SaaS binds its opened store stand-in with `ProvideValue` and `credo.ComponentName` alone, hands the WebSocket server to `app.Manage(ws)` without an option, registers its worker through its constructor on `app.Workers()`, and keeps `Finalize` as an optional early check. For v0.26.0, SaaS keeps Secure and CORS on `PreRouting`, verifies its tokens with `auth/jwtauth`, and declares its log level a live section whose `OnChange` sets the `slog.LevelVar`, while `app` stays static, so a reload that changes `app.name` reports a restart. Hello remains a minimal default-profile example and needed no change.

DI evidence comes from a 2026-09-05 scan of the maintainer's downstream applications: no factory/Replace calls, pre-Run resolution, and a worker-pool existence probe. The same applications install renderers once at bootstrap and use no scoped Recover, so their HTTP-minor migration is a rename of the renderer setters plus explicit `UseRequestID`/`UseAccessLog` calls.
