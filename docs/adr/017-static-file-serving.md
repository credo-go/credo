# ADR-017: Static File Serving

**Status:** Accepted **Date:** 2026-03-11 **Depends on:** ADR-007, ADR-009

## Context

Web applications commonly serve static assets (CSS, JS, images, fonts) and single-page applications (SPAs) alongside dynamic API routes. Credo's router supports catch-all parameters (`{path...}`) and `Mount` for attaching `http.Handler` trees, but neither approach participates in Credo's middleware, error handling, or route meta system.

Go 1.24 introduced `os.Root`, which provides symlink-safe, sandboxed filesystem access via `Root.FS()`. Combined with `http.ServeContent` (range requests, conditional GETs), this gives a secure and complete foundation for static file serving.

Options considered:

1. **Document a `Mount` + `http.FileServerFS` recipe** — minimal framework work, but bypasses Credo's handler/error pipeline, middleware, and route meta. Users lose observability, auth integration, and consistent error responses.
2. **Middleware-based approach** (Fiber pattern) — flexible, but middleware that serves files short-circuits the handler chain, making route meta and per-route middleware semantics unclear.
3. **First-class `Static` / `File` methods on App and Group** — route-like registration that returns a chainable type, participates in all Credo systems (middleware, meta, error handling, access log), and follows the existing `GET`/`POST` registration pattern.

## Decision

Option 3. Static file serving is a first-class route registration feature. `Static` and `File` methods live on both `App` and `Group`, accept `fs.FS` (the stdlib interface), and produce Credo handlers that participate in the full middleware and error pipeline.

### `fs.FS` as the Source Interface

The methods accept `fs.FS` — not a string directory path. This keeps the filesystem source and lifecycle visible at the application boundary.

Supported sources:

- `credo.DirFS(dir)` — production, symlink-safe convenience; returns an `os.Root`-backed FS and is the recommended default for disk serving
- `os.Root.FS()` — production, symlink-safe with manual lifecycle control
- `embed.FS` via `fs.Sub()` — single-binary deployment
- `os.DirFS()` — development convenience (no symlink protection)
- Custom `fs.FS` — testing, CDN backends, etc.

`credo.DirFS` is the recommended convenience for disk serving: it opens an `os.Root` and returns its FS together with an `io.Closer` for the directory handle, so symlink escapes are refused without the caller wiring up `os.Root` by hand. Register the closer with `OnShutdown` to release the handle on graceful shutdown. The framework does not auto-manage the root — the caller still owns its lifecycle — keeping the security decision explicit; the framework never silently swaps in a sandbox behind the user's back.

### Static Method

```go
func (app *App) Static(prefix string, fsys fs.FS, cfgs ...StaticConfig) *StaticRoute
func (g *Group) Static(prefix string, fsys fs.FS, cfgs ...StaticConfig) *StaticRoute
```

Internally registers two routes:

- **Catch-all**: `prefix + "/{_static...}"` — serves files
- **Exact match**: `prefix` — serves the index file

Both routes share the same underlying handler. The `*StaticRoute` wrapper proxies fluent methods to both internal routes to ensure consistency.

Prefix validation: panics if prefix contains `{` or `}` (route parameters are not meaningful in a static prefix).

### StaticRoute Wrapper

`Static` returns `*StaticRoute` instead of `*Route` because it registers two internal routes. The wrapper ensures fluent operations apply consistently:

```go
type StaticRoute struct {
    primary *Route  // catch-all GET (HEAD twin auto-paired via Route.headTwin)
    index   *Route  // exact prefix GET (HEAD twin auto-paired via Route.headTwin)
}

func (sr *StaticRoute) Name(name string) *StaticRoute
func (sr *StaticRoute) SetMeta(key string, val any) *StaticRoute
func (sr *StaticRoute) Middleware(m ...Middleware) *StaticRoute
func (sr *StaticRoute) BuildURI(filePath ...string) string
```

- `Name` — applies to the primary (catch-all) route only, since route names must be unique and `BuildURI` on `Route` requires the catch-all parameter explicitly.
- `SetMeta` — applies to both GET routes. Each GET's auto-generated HEAD twin receives the value through `Route.SetMeta`'s headTwin propagation, so middleware reading meta sees identical values on GET and HEAD.
- `Middleware` — applies to both GET routes. Each GET's auto-generated HEAD twin receives the middleware through `Route.Middleware`'s headTwin propagation, so HEAD requests run the same chain as GET (no silent bypass of auth or rate limiting).
- `BuildURI` — convenience method that produces clean URIs without requiring the catch-all parameter name. `sr.BuildURI("css/app.css")` returns `"/static/css/app.css"`.

### HEAD Twin Propagation (universal)

This is not static-specific: every GET route (registered via `app.GET`, `group.GET`, `app.Static`, `app.File`, etc.) auto-generates a HEAD twin. The twin is stored on the GET route's unexported `headTwin` field. `Route.Middleware` and `Route.SetMeta` propagate to the twin so users cannot accidentally configure GET while leaving HEAD bypassed. If an explicit `app.HEAD(pattern, ...)` already exists for a pattern, `addHeadRoute` returns nil and the GET route's `headTwin` remains nil — the explicit HEAD route is left untouched and middleware on the GET route does not leak to it.

### File Method

```go
func (app *App) File(path string, fsys fs.FS, name string, cfgs ...StaticConfig) *Route
func (g *Group) File(path string, fsys fs.FS, name string, cfgs ...StaticConfig) *Route
```

Registers a single GET route that serves one file. Returns `*Route` (not `*StaticRoute`) since only one route is created.

Only `Download` and `CacheControl` fields of `StaticConfig` are supported. `Browse`, `SPA`, and `Index` panic at registration time if set — consistent with Credo's "fail fast on developer configuration" convention (`Name` duplicate, `checkFrozen`, prefix validation).

### StaticConfig

```go
type StaticConfig struct {
    Index    string // index file for directories; default "index.html"
    Browse   bool   // directory listing; default false
    SPA      bool   // single-page app fallback; default false
    Download bool   // Content-Disposition: attachment; default false

    // CacheControl decides the Cache-Control header for each successful
    // response; nil writes no header. See the Cache-Control section below.
    CacheControl func(StaticCacheContext) string
}
```

All boolean fields default to `false` — plain `bool` is used, not `*bool`. The `*bool` pattern is reserved for nil-means-true toggles (e.g., `HealthConfig.Enabled`); here, zero values are the desired defaults.

### Cache-Control

`Cache-Control` is decided entirely by the `CacheControl` hook. It receives a `StaticCacheContext` (resolved file path, file name, and whether the response is an HTML entry point) for each successful response — including 206 partial content — and never for error statuses. The returned string is written verbatim: `""` writes no header, and a nil hook disables `Cache-Control` entirely. Hooks should be pure and deterministic.

Two presets cover the common policies:

| Preset | Non-HTML asset | HTML entry point |
| --- | --- | --- |
| `StaticCacheMaxAge(d)` | `public, max-age=N` | `public, max-age=N` |
| `StaticCacheImmutableAssets(d)` | `public, max-age=N, immutable` | `no-cache, must-revalidate` |

`StaticCacheMaxAge` floors sub-second durations to whole seconds and emits no header when the floor is 0; both presets panic on negative durations, and `StaticCacheImmutableAssets` panics unless the floor is at least 1 second. Anything finer-grained — e.g. only one content-hashed asset directory should be immutable — is a few lines of custom hook over `StaticCacheContext`.

As defense in depth, 4xx/5xx responses rewrite any inherited `immutable` `Cache-Control` directive to `no-cache, must-revalidate`, so a missing hashed asset is not cached for a full immutable lifetime when headers were set before the final status was known. This does not cover reverse proxies or CDNs that add cache headers after Credo writes the response.

See [`docs/specs/static.md`](../specs/static.md) and the `StaticConfig`, `StaticCacheMaxAge`, and `StaticCacheImmutableAssets` godoc for the full hook contract and validation table.

### SPA Fallback Scope

SPA mode does NOT blindly serve `index.html` for all missing paths. It uses a dot-based heuristic to distinguish navigation requests from asset requests:

- Last path segment **has no dot** → SPA fallback (see preference order below)
- Last path segment **has a dot** → 404 (missing asset, e.g., `app.js`)

This prevents broken asset references from silently returning HTML, which causes confusing client-side errors. The heuristic matches the behavior of widely used SPA tools (Vite, create-react-app, connect-history-api-fallback).

SPA fallback is further restricted to **GET and HEAD** methods only. The static catch-all is registered as a GET route (with an auto-generated HEAD twin), so any other method against the prefix matches the route pattern but finds no handler for the verb — the router returns **405 Method Not Allowed**, not 404. The SPA branch is never reached for non-GET/HEAD requests.

#### SPA fallback preference order

When SPA mode is on and the request qualifies (GET/HEAD, no dot in last segment), Credo prefers prerendered HTML over the SPA shell. This supports the static-adapter outputs of SvelteKit, Next.js export, Astro static, Eleventy, and similar tools, which write each prerendered route as a sibling `<route>.html` file.

The fallback runs through one of two branches depending on what `fsys.Open(cleanPath)` returns:

**File branch** — `cleanPath` does not exist on the filesystem:

| Match attempt      | Description                                   |
| ------------------ | --------------------------------------------- |
| 1. Sibling `.html` | `<cleanPath>.html` exists (prerendered route) |
| 2. SPA shell       | Root `index.html` (or configured `cfg.Index`) |

**Directory branch** — `cleanPath` exists and is a directory:

| Match attempt | Description |
| --- | --- |
| 1. Directory's index | `<cleanPath>/index.html` exists |
| 2. Sibling `.html` | `<cleanPath>.html` exists (parent route, sibling file) |
| 3. SPA shell | Root `index.html` (or configured `cfg.Index`) |

(The "exact file exists, not a directory" case is the success path — it serves the file and never enters the SPA fallback.)

This split matters in two real cases:

- **File branch**: a request for `/admin/users` in a SvelteKit export serves the prerendered `admin/users.html` (fast, no client boot) rather than booting the SPA shell.
- **Directory branch**: a `/reports.html` parent route can coexist with a `/reports/` directory holding child routes; `/reports` resolves to the parent prerender (sibling), while `/reports/crm` resolves to the child prerender via the file branch. When a directory has its own `index.html`, that wins over the sibling — directories declare their own canonical entry point first.

The directory branch is also a small bug fix: before this preference order, a directory with no index file returned 404 even in SPA mode. It now falls through to the SPA shell, matching expectations.

#### SPA + Browse interaction

When both `cfg.SPA` and `cfg.Browse` are true and the request qualifies for SPA fallback (GET/HEAD, no dot in last segment), the SPA branch wins and the directory listing is **not** rendered. SPA navigation must not expose a browseable file index for routes the SPA owns. Browse listings are still rendered for non-SPA-candidate requests that reach the static handler (for example, GET/HEAD paths whose last segment has a dot). Non-GET/HEAD methods are rejected by routing with 405 before the static handler runs.

### Handler Behavior

The static handler is an `credo.Handler` (`func(*Context) error`):

1. Extract path from `RouteParams["_static"]` (or empty for exact match).
2. Decode the captured path with `url.PathUnescape`. Malformed escape sequences return 400 Bad Request.
3. Sanitize by rejecting null bytes, backslashes, and explicit `..` path segments (400 Bad Request), then normalize the remaining path via `path.Clean("/" + p)`.
4. Open file via `fsys.Open(cleanPath)`.
5. If not found:
   - SPA + GET/HEAD + no-dot heuristic: try sibling `<cleanPath>.html` (prerendered route); on miss, fall back to root index.
   - Otherwise: 404.
6. If directory:
   - Try `<cleanPath>/index.html` (or configured `cfg.Index`).
   - SPA + GET/HEAD + no-dot heuristic: try sibling `<cleanPath>.html` (parent route prerender); on miss, fall back to root index.
   - `cfg.Browse`: render directory listing.
   - Otherwise: 404.
7. Set headers: `X-Content-Type-Options: nosniff` (always), `Cache-Control` (from the `CacheControl` hook), `Content-Disposition` (if Download). Directory listings receive the same cache / download headers.
8. Serve via `http.ServeContent` (handles Range, If-Modified-Since, Content-Type detection). Fallback to `io.Copy` for non-seekable `fs.File` implementations.

Errors return `NewHTTPError(status, key)`, flowing through Credo's standard error pipeline (classification, logging, ErrorRenderer).

### Security Model

| Threat | Mitigation |
| --- | --- |
| Path traversal (`../`) | Explicit `..` segments are rejected with 400 Bad Request |
| Symlink escape | `os.Root.FS()` recommended; framework documents the risk of `os.DirFS` |
| Encoded path tricks (`%2e`, `%5c`, `%00`) | URL-decoded before sanitization, then validated |
| Backslash traversal | Rejected with 400 |
| Null byte injection | Rejected with 400 |
| MIME sniffing | `X-Content-Type-Options: nosniff` on all static responses |
| Directory listing leak | `Browse` defaults to false |
| Double encoding (`%252e`) | One decode pass runs before sanitization; still-normalized output cannot escape the FS root |

## Consequences

**Positive:**

- Static routes participate in all Credo systems: middleware, route meta, error handling, access log, request ID.
- `fs.FS` interface supports embed, `os.Root`, `os.DirFS`, and custom implementations — no framework lock-in to a specific filesystem.
- SPA dot heuristic prevents the common pitfall of assets returning HTML.
- `StaticRoute` wrapper ensures `SetMeta` and `Middleware` apply consistently to both internal routes.
- Security defaults are safe: no directory listing, nosniff header, path sanitization.

**Negative:**

- `*StaticRoute` is a new type distinct from `*Route`, adding API surface. Mitigated by keeping only 4 methods, all with familiar semantics.
- Dot heuristic for SPA has a theoretical false negative for paths like `/users/john.doe`. This is an uncommon URL pattern in SPAs, and users who need it can write a custom handler.
- No built-in compression or ETag generation. These are cross-cutting concerns handled by middleware (`middleware.Compress`) or `http.ServeContent` (Last-Modified).

**Risks:**

- Large files served without streaming could consume memory. Mitigated by `http.ServeContent` which uses `io.ReadSeeker` for range-based streaming when the `fs.File` supports it.
- Users may pass `os.DirFS` in production, which lacks symlink protection. Mitigated by clear documentation recommending `os.Root.FS()`.
