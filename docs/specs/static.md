# Static File Serving Spec

**Status:** Implemented (beta) **Package:** root package, `internal/static/` **ADR:** [ADR-017](../adr/017-static-file-serving.md) **Guide:** [Static file serving guide](../guides/static-files.md)

## Overview

Credo serves static files through `Static` and `File` on `App` and `Group`. Static routes are full participants in Credo's middleware, route meta, error handling and access log. This spec owns the serving contract: the API, the request flow, the SPA fallback, the headers and the security rules. [ADR-017](../adr/017-static-file-serving.md) holds the rationale and the rejected alternatives; the [static file serving guide](../guides/static-files.md) holds the recipes (embedded assets, disk serving, SPA builds, downloads, listings, single files, groups).

**Design principles**:

- `fs.FS` as the source interface: `credo.DirFS`, `embed.FS`, `os.Root.FS()`, `os.DirFS`, custom implementations
- Visible ownership: the caller owns the filesystem and its lifecycle; Credo never opens or closes one behind its back
- Secure defaults: no directory listing, `nosniff` on served content, path sanitization
- SPA-aware: a dot heuristic keeps assets from returning `index.html`

The public API, route registration, `DirFS`, the cache presets and the error mapping are in the root package (`static.go`); sanitization, index and SPA resolution, Range-aware delivery, status-aware `Cache-Control` and directory listings are in `internal/static` (`static.go`, `browse.go`).

## API Surface

### StaticConfig

```go
type StaticConfig struct {
    Index        string                          // file served for a directory; default "index.html"
    Browse       bool                            // HTML listing for a directory without an index; default false (404)
    SPA          bool                            // page-like GET/HEAD requests fall back to <path>.html, then the root index (SPA Fallback)
    Download     bool                            // Content-Disposition: attachment on served files and listings
    CacheControl func(StaticCacheContext) string // Cache-Control per successful response; nil writes none (Cache-Control)
}

type StaticCacheContext struct {
    RequestPath string // client path before internal rewrites (Context.OriginalPath)
    FilePath    string // resolved slash-separated path inside fsys; the directory for a listing
    FileName    string // base name of the served file, or the generated listing name
    IsHTML      bool   // an .html/.htm file, the configured index file, or a listing
}

func StaticCacheMaxAge(maxAge time.Duration) func(StaticCacheContext) string
func StaticCacheImmutableAssets(maxAge time.Duration) func(StaticCacheContext) string
```

The presets and their duration rules are in [Cache-Control](#cache-control).

### DirFS

```go
func DirFS(dir string) (fs.FS, io.Closer, error)
```

`DirFS` opens `dir` as an `os.Root` and returns its `fs.FS` with an `io.Closer` for the directory handle. Symlinks that resolve outside `dir` are refused. It returns an error when `dir` cannot be opened. The handle stays open until the caller closes it; Credo does not. Close it in an `OnStop` hook: an internal stop hook runs after the HTTP drain ([lifecycle spec](lifecycle.md#shutdown-in-tiers)), so no request still reads from it.

```go
fsys, closer, err := credo.DirFS("./public")
if err != nil {
    return err
}
app.Static("/static", fsys)
app.OnStop(func(context.Context) error { return closer.Close() })
```

### Static (directory serving)

```go
func (app *App) Static(prefix string, fsys fs.FS, cfgs ...StaticConfig) *StaticRoute
func (g *Group) Static(prefix string, fsys fs.FS, cfgs ...StaticConfig) *StaticRoute
```

Registers a catch-all and an exact GET route that serve files from `fsys` under the given URL prefix ([Route Registration](#route-registration)). A trailing slash on the prefix is dropped.

**Registration-time panics:**

- `prefix` contains `{` or `}`: `Static`'s own prefix takes no route parameters (a parametric group prefix is accepted)
- the group's prefix names a parameter `_static`, the name the file path is captured under, or uses a parameter name twice (`credo: Static "/t/{_static}/files": the parameter name "_static" is reserved`)
- the App is already prepared ([registration guards](lifecycle.md#registration-guards))

### File (single file serving)

```go
func (app *App) File(urlPath string, fsys fs.FS, name string, cfgs ...StaticConfig) *Route
func (g *Group) File(urlPath string, fsys fs.FS, name string, cfgs ...StaticConfig) *Route
```

Registers a single GET route, with the router's HEAD twin, that serves the file `name` from `fsys`. Returns `*Route` for standard fluent chaining. `name` is normalized with `path.Clean` once at registration; a missing file or a directory answers `ErrNotFound` (404).

**Supported config fields:** `Download`, `CacheControl`.

**Registration-time panics:**

- `cfg.Browse` is `true`
- `cfg.SPA` is `true`
- `cfg.Index` is not empty
- the App is already prepared ([registration guards](lifecycle.md#registration-guards))

### StaticRoute

```go
func (sr *StaticRoute) Name(name string) *StaticRoute
func (sr *StaticRoute) SetMeta(key string, val any) *StaticRoute
func (sr *StaticRoute) Middleware(m ...Middleware) *StaticRoute
func (sr *StaticRoute) BuildURI(filePath ...string) string
```

`*StaticRoute` wraps the two GET routes `Static` registers and proxies fluent configuration to them:

| Method | Applies to | Effect |
| --- | --- | --- |
| `Name` | the catch-all route only | route names are unique; the exact-prefix route stays unnamed |
| `SetMeta` | both GET routes and their HEAD twins | middleware reading meta sees the same value for both methods and both patterns |
| `Middleware` | both GET routes and their HEAD twins | HEAD runs the same chain as GET |
| `BuildURI` | the registered prefix | the URL path of a file under it (below) |

`Name`, `SetMeta` and `Middleware` panic once the App is prepared, like their `Route` counterparts.

```go
sr := app.Static("/static", fsys)
sr.BuildURI("css/app.css") // "/static/css/app.css"
sr.BuildURI("")            // "/static/"
sr.BuildURI()              // "/static/"
```

Leading slashes of the file path are dropped; under the root prefix `"/"` the results are `/css/app.css` and `/`. `BuildURI` joins the prefix as registered and the file path verbatim: it percent-encodes nothing and substitutes no parameters. Under a parametric group prefix it therefore returns pattern text (`/t/{tenant}/files/css/app.css`), not a routable URL. Name the static route and build from the catch-all instead, which fills the parameters in order and escapes each segment:

```go
app.Group("/t/{tenant}").Static("/files", fsys).Name("files")
uri, err := app.GetRoute("files").BuildURI("acme", "css/app.css") // "/t/acme/files/css/app.css"
```

## Internal Behavior

### Route Registration

`Static("/static", fsys)` registers:

```
GET /static/{_static...}   catch-all: the captured remainder selects the file
GET /static                exact prefix: serves the index file
```

Under the root prefix the patterns are `/{_static...}` and `/`. `/static/` reaches the catch-all with an empty capture and serves the index as well. Both routes serve through the same logic and configuration. Each GET route gets the router's automatic HEAD twin, which shares its chain and meta ([router spec](router.md#head-auto-handling)).

### Request Flow

```
1. Extract   RouteParam("_static"); "" for the exact prefix and for "prefix/"
2. Sanitize  NUL byte, backslash or explicit ".." segment → ErrBadRequest (400);
             otherwise path.Clean (see Path Sanitization) → cleanPath
3. Open      fsys.Open(cleanPath)
4. Not found?
   ├─ SPA candidate → <cleanPath>.html sibling, else root index (see SPA Fallback)
   └─ else          → ErrNotFound (404)
5. Directory?
   ├─ <cleanPath>/<Index> exists → serve it
   ├─ SPA candidate              → <cleanPath>.html sibling, else root index
   ├─ Browse enabled             → directory listing
   └─ else                       → ErrNotFound (404)
6. Headers   X-Content-Type-Options: nosniff
             Content-Disposition: attachment (if Download)
             Cache-Control from the hook, at WriteHeader time, status < 400 only
7. Serve     http.ServeContent (Range, conditional requests, Content-Type)
             non-seekable fs.File: io.Copy, always 200, no Range
```

`ErrNotFound` and `ErrBadRequest` are the framework's standard sentinels: they render through the error pipeline like any handler error, not through a `StatusHandler`, which serves only the router's own 404 and 405. A method other than GET or HEAD never reaches the handler: the router answers 405.

### Path Sanitization

The captured value was decoded exactly once by the router, like every route parameter ([router spec](router.md#encoded-parameter-values)); static serving never decodes it again. `/static/100%25.txt` reaches the file `100%.txt`, and `/static/a%252Fb.txt` a file literally named `a%2Fb.txt`, never `a/b.txt`. A malformed escape such as `%zz` does not reach Credo: net/http rejects the request target with 400.

Sanitization is lexical and rejects rather than rewrites: a NUL byte, a backslash or an explicit `..` segment answers `ErrBadRequest` (400). The remaining path is normalized with `path.Clean("/" + p)` (platform-independent, unlike `filepath.Clean`), the leading `/` is stripped, and an empty result becomes `.`, the root of `fsys`. Symlinks are the filesystem's concern ([DirFS](#dirfs)).

### SPA Fallback

With `SPA` on, a request is an SPA candidate when its method is GET or HEAD and the last segment of the captured path has no dot. Only candidates fall back; a missing path with a dot in its last segment (`app.js`, `css/style.css`) answers 404. The root index is the `Index` file (default `index.html`) at the root of `fsys`; when it is missing too, the request answers 404.

| `cleanPath` | Tried in order |
| --- | --- |
| does not exist | 1. `<cleanPath>.html` (prerendered route) 2. root index |
| is a directory | 1. `<cleanPath>/<Index>` (the directory's own index, tried with or without SPA) 2. `<cleanPath>.html` (parent route prerender) 3. root index |

With a build that holds a CSR shell (`index.html`) and prerendered routes (`admin/users.html`, `reports.html`, `reports/crm.html`):

| Request path | Has dot? | Served | Branch |
| --- | --- | --- | --- |
| `/dashboard` | no | `index.html` (SPA shell) | missing path, no sibling |
| `/users/123` | no | `index.html` (SPA shell) | missing path, no sibling |
| `/admin/users` | no | `admin/users.html` | missing path, sibling |
| `/reports` | no | `reports.html` | directory without an index, sibling |
| `/reports/crm` | no | `reports/crm.html` | missing path, sibling |
| `/app.js` | yes | 404 | not a candidate |
| `/admin/users.json` | yes | 404, never `admin/users.html` | not a candidate |

When `Browse` is also on, an SPA candidate takes the SPA fallback and never gets a directory listing; requests that are not candidates still get listings.

### Headers

Every served file (a regular file, an index file, an SPA fallback or a prerendered sibling) and every directory listing carries `X-Content-Type-Options: nosniff`. The 404 and 400 answers come from the error pipeline, which does not add it.

With `Download`, served files and directory listings carry `Content-Disposition: attachment` with the file name (encoded with `mime.FormatMediaType`, so quotes and non-ASCII names are safe); a listing is named after its directory (`<dir>.html`, or `listing.html` at the root).

### Cache-Control

`Cache-Control` is decided entirely by the `CacheControl` hook. It is called once per successfully served response (status below 400, including 206 partial content and 304) with the resolved `StaticCacheContext`. The returned string is written verbatim; `""` writes no header; a nil hook disables `Cache-Control` entirely. The hook is applied at `WriteHeader` time, so an error status, including one `http.ServeContent` produces itself (412, 416), never consults it, and a missing hashed asset cannot inherit a long-lived cache policy.

Hooks run per request and should be pure and deterministic. They must not depend on mutable counters, clocks, random values, or other side effects; otherwise identical assets may receive inconsistent cache policies.

The presets encode the two common policies:

| Preset | Successful non-HTML response | Successful HTML response / listing |
| --- | --- | --- |
| `StaticCacheMaxAge(d)` | `public, max-age=N` | `public, max-age=N` |
| `StaticCacheImmutableAssets(d)` | `public, max-age=N, immutable` | `no-cache, must-revalidate` |

Anything finer-grained, for example marking only `_app/immutable/` as immutable, is a custom hook over `StaticCacheContext.FilePath`.

Error statuses never carry an inherited `immutable` policy: on every 4xx/5xx response, static or not, `Response.WriteHeader` rewrites it to `no-cache, must-revalidate` ([context spec](context.md#response-struct)), while a `Cache-Control` that a reverse proxy or CDN adds after Credo is outside this guard.

#### Preset Duration Resolution

Both presets floor the duration to whole seconds when they are called. `StaticCacheMaxAge` panics on a negative duration and writes no header when the floor is 0; `StaticCacheImmutableAssets` panics on a negative duration or one that floors below 1 second. Invalid cache configuration is a programmer error, caught at the preset call site, which is the registration site.

| Input                     | Seconds | `StaticCacheMaxAge` header |
| ------------------------- | ------- | -------------------------- |
| `0`                       | 0       | (none)                     |
| `24 * time.Hour`          | 86400   | `public, max-age=86400`    |
| `1500 * time.Millisecond` | 1       | `public, max-age=1`        |
| `500 * time.Millisecond`  | 0       | (none)                     |
| `-1 * time.Second`        | —       | panic                      |

## Security

| Threat | Mitigation |
| --- | --- |
| Path traversal (`../`) | Explicit `..` segments rejected with 400 Bad Request |
| Symlink escape | Confined by the filesystem: `credo.DirFS` or `os.Root.FS()` for disk serving; `os.DirFS` does not prevent it |
| Encoded path tricks (`%2e`, `%5c`, `%00`) | Decoded once by the router, then validated |
| Backslash traversal | Rejected with 400 Bad Request |
| Null byte injection | Rejected with 400 Bad Request |
| MIME sniffing | `X-Content-Type-Options: nosniff` on every served file and listing |
| Directory listing leak | `Browse` defaults to false |
| Double encoding (`%252e`) | Exactly one decode pass (the router's) runs before sanitization: `%252e%252e` is the literal name `%2e%2e`, never `..` |

## Design Decisions

Rationale and rejected alternatives are in [ADR-017](../adr/017-static-file-serving.md):

- `fs.FS`, not a string path, with `credo.DirFS` as the disk default: [ADR-017](../adr/017-static-file-serving.md#fsfs-as-the-source-interface)
- `*StaticRoute`, not `*Route`: [ADR-017](../adr/017-static-file-serving.md#staticroute-wrapper)
- `File` panics on fields it cannot apply: [ADR-017](../adr/017-static-file-serving.md#file-method)
- Cache policy as one hook plus presets: [ADR-017](../adr/017-static-file-serving.md#cache-control)
- A dot heuristic for SPA candidates, not `Accept` detection, and prerendered HTML before the shell: [ADR-017](../adr/017-static-file-serving.md#spa-fallback-scope)
- No static-specific compression or ETag, no config-file integration, a minimal listing: [ADR-017](../adr/017-static-file-serving.md#consequences)
