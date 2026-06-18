# Static File Serving Spec

**Status**: Approved **Package**: Root (`github.com/credo-go/credo`) **Sources**: Original design (no external code adapted) **Depends on**: Root package (`App`, `Group`, `Route`, `Handler`, `Context`, `HTTPError`) **ADR**: [017-static-file-serving](../adr/017-static-file-serving.md)

---

## Overview

Credo provides first-class static file serving through `Static` and `File` methods on `App` and `Group`. Static routes are full participants in Credo's middleware, route meta, error handling, and access log systems.

**Design principles**:

- `fs.FS` as the source interface — works with `embed.FS`, `os.Root.FS()`, `os.DirFS`, custom implementations
- Visible ownership — user controls filesystem lifecycle, no hidden `os.Root` management
- Secure defaults — no directory listing, `nosniff` header, path sanitization
- SPA-aware — dot heuristic prevents assets from returning `index.html`

---

## API Surface

### StaticConfig

```go
type StaticCacheContext struct {
    RequestPath string // client path before internal rewrites
    FilePath    string // resolved fs.FS path; directory path for listings
    FileName    string // base file name or generated listing name
    IsHTML      bool   // true for HTML entry points and listings
}

type StaticConfig struct {
    // Index is the file served for directory requests.
    // Default: "index.html".
    Index string

    // Browse enables directory listing when a directory has no index file.
    // SPA takes precedence: if SPA is also true and the request qualifies
    // as an SPA candidate, the sibling-or-root fallback runs instead of
    // the directory listing.
    // Default: false (404 for directories without index).
    Browse bool

    // SPA enables single-page application mode for routes that target a
    // page rather than an asset (GET or HEAD with no dot in the last path
    // segment). For these requests, the resolver prefers, in order:
    //
    //   File branch (path does not exist):
    //     1. <path>.html sibling (e.g. /admin/users → admin/users.html)
    //     2. root index
    //
    //   Directory branch (path is an existing directory):
    //     1. <dir>/<Index> (the directory's own index file)
    //     2. <dir>.html sibling (e.g. /reports/ → reports.html)
    //     3. root index
    //
    // Requests with a dot in the last segment (app.js, style.css) skip
    // the fallback entirely and return 404 when missing.
    // Default: false.
    SPA bool

    // Download sets Content-Disposition to "attachment" on all responses,
    // prompting the browser to download instead of display inline.
    // Default: false.
    Download bool

    // CacheControl decides the Cache-Control header for each successfully
    // served response (status < 400). It receives the resolved cache
    // context and returns the full header value; "" writes no header.
    // Nil disables Cache-Control entirely. Called once per response
    // (including 206), never for error statuses.
    CacheControl func(StaticCacheContext) string
}

// Presets for the common policies:

// Every successful response: "public, max-age=N". maxAge floors to whole
// seconds (0 → no header); negative panics.
func StaticCacheMaxAge(maxAge time.Duration) func(StaticCacheContext) string

// Content-hashed builds: non-HTML assets "public, max-age=N, immutable",
// HTML responses (index files, SPA fallbacks, prerendered pages, listings)
// "no-cache, must-revalidate". Panics unless maxAge floors to ≥ 1s.
func StaticCacheImmutableAssets(maxAge time.Duration) func(StaticCacheContext) string
```

### Static (directory serving)

```go
func (app *App) Static(prefix string, fsys fs.FS, cfgs ...StaticConfig) *StaticRoute
func (g *Group) Static(prefix string, fsys fs.FS, cfgs ...StaticConfig) *StaticRoute
```

Registers routes that serve files from `fsys` under the given URL prefix.

**Registration-time panics:**

- `prefix` contains `{` or `}` (route parameters in static prefix)

(The cache presets panic at their own call site on invalid durations: `StaticCacheMaxAge` on negative, `StaticCacheImmutableAssets` on anything that floors below 1 second.)

### File (single file serving)

```go
func (app *App) File(path string, fsys fs.FS, name string, cfgs ...StaticConfig) *Route
func (g *Group) File(path string, fsys fs.FS, name string, cfgs ...StaticConfig) *Route
```

Registers a single GET route that serves one named file from `fsys`. Returns `*Route` for standard fluent chaining.

**Supported config fields:** `Download`, `CacheControl`.

**Registration-time panics:**

- `cfg.Browse` is `true`
- `cfg.SPA` is `true`
- `cfg.Index` is not empty

### StaticRoute

```go
type StaticRoute struct {
    // unexported: primary *Route (catch-all GET), index *Route (exact GET).
    // Each GET's auto-generated HEAD twin lives on Route.headTwin —
    // SetMeta and Middleware propagate to those twins automatically.
}
```

Wraps the two internal GET routes created by `Static` (catch-all + exact match) and proxies fluent operations to both.

```go
// Name sets the route name on the primary (catch-all) route only.
// The exact-match route is unnamed. Use BuildURI for URL generation.
func (sr *StaticRoute) Name(name string) *StaticRoute

// SetMeta sets metadata on both GET routes. Each GET's auto-generated
// HEAD twin receives the value via Route.SetMeta's headTwin propagation,
// so middleware reading meta sees identical values on GET and HEAD.
func (sr *StaticRoute) SetMeta(key string, val any) *StaticRoute

// Middleware appends middleware to both GET routes. Each GET's auto-
// generated HEAD twin receives the middleware via Route.Middleware's
// headTwin propagation, so HEAD requests run the same chain as GET.
func (sr *StaticRoute) Middleware(m ...Middleware) *StaticRoute

// BuildURI returns the URL path for a file within this static endpoint.
//
//   sr.BuildURI("css/app.css")  → "/static/css/app.css"
//   sr.BuildURI("")             → "/static/"
//   sr.BuildURI()               → "/static/"
func (sr *StaticRoute) BuildURI(filePath ...string) string
```

---

## Usage Patterns

### Pattern 1: Embedded Assets (production)

```go
//go:embed public/*
var publicFS embed.FS

func main() {
    app, _ := credo.New()
    sub, _ := fs.Sub(publicFS, "public")
    app.Static("/static", sub)
    app.Run()
}
```

### Pattern 2: Disk Serving with os.Root (secure)

```go
func main() {
    app, _ := credo.New()

    root, err := os.OpenRoot("./public")
    if err != nil {
        log.Fatal(err)
    }
    app.OnShutdown(func(ctx context.Context) error {
        return root.Close()
    })

    app.Static("/static", root.FS())
    app.Run()
}
```

### Pattern 3: SPA (React, Vue, Angular)

```go
sub, _ := fs.Sub(publicFS, "dist")
app.Static("/", sub, credo.StaticConfig{SPA: true})
```

SPA fallback behavior (CSR shell only — no prerender output):

| Request path     | Has dot? | Result                      |
| ---------------- | -------- | --------------------------- |
| `/dashboard`     | no       | `index.html` (SPA fallback) |
| `/users/123`     | no       | `index.html` (SPA fallback) |
| `/app.js`        | yes      | 404 (missing asset)         |
| `/css/style.css` | yes      | 404 (missing asset)         |

When the build output also contains prerendered HTML (SvelteKit static adapter, Next.js export, Astro static, Eleventy, Hugo, etc.), Credo prefers the prerender over the SPA shell. Sibling `<route>.html` files take precedence in both the missing-file branch and the directory branch:

| Request path | Filesystem entry served | Reason |
| --- | --- | --- |
| `/admin/users` | `admin/users.html` | Sibling `.html` (prerendered route) |
| `/reports` | `reports.html` | Sibling `.html` parent route, dir lacks `index` |
| `/reports/crm` | `reports/crm.html` | Sibling `.html` child route |
| `/missing` | `index.html` (SPA shell) | No sibling, SPA fallback |

The sibling check is gated on the same dot heuristic and method restriction (GET/HEAD only) as the SPA shell fallback.

### Pattern 4: Download Zone

```go
app.Static("/downloads", root.FS(), credo.StaticConfig{
    Download:     true,
    CacheControl: credo.StaticCacheMaxAge(1 * time.Hour),
})
```

### Pattern 5: Directory Browsing

```go
app.Static("/files", root.FS(), credo.StaticConfig{Browse: true})
```

Renders a minimal HTML directory listing with file name, size, and modification date.

### Pattern 6: Single Files

```go
app.File("/favicon.ico", publicFS, "favicon.ico")
app.File("/robots.txt", os.DirFS("."), "robots.txt")
app.File("/report.pdf", uploadsFS, "reports/q1.pdf", credo.StaticConfig{
    Download: true,
})
```

### Pattern 7: Group with Middleware

```go
admin := app.Group("/admin")
admin.Static("/assets", adminFS).
    Middleware(auth.Middleware[*User](authenticator, nil))
```

### Pattern 8: Cached Assets with Naming

```go
app.Static("/assets", sub, credo.StaticConfig{
    CacheControl: credo.StaticCacheImmutableAssets(365 * 24 * time.Hour),
}).Name("assets")

// URL generation
url := app.GetRoute("assets") // *Route (catch-all)
// Prefer StaticRoute.BuildURI for clean URLs:
// sr.BuildURI("css/app.css") → "/assets/css/app.css"
```

---

## Internal Behavior

### Route Registration

`Static("/static", fsys)` registers:

```
GET /static/{_static...}   → staticHandler(fsys, cfg)   [catch-all]
GET /static                → staticIndexHandler(fsys, cfg) [exact match]
```

Both routes share the same serving logic. HEAD routes are auto-registered by the existing `addHeadRoute` mechanism and stored on each GET's `Route.headTwin`. `StaticRoute.SetMeta` and `StaticRoute.Middleware` delegate to `Route.SetMeta` / `Route.Middleware`, which propagate to the HEAD twins automatically — there are no separate HEAD references inside `StaticRoute`.

### Request Flow

```
1. Extract      RouteParams["_static"] (empty for exact match)
2. Decode       url.PathUnescape(p); malformed escape → 400
3. Sanitize     reject \x00, \, and explicit .. segments → 400;
                path.Clean("/" + p) for remaining normalization
4. Open         fsys.Open(cleanPath)
5. Not found?
   ├─ SPA + GET/HEAD + no dot → try <cleanPath>.html sibling, else root index
   └─ else                    → NewHTTPError(404, MsgKeyNotFound)
6. Directory?
   ├─ index file exists       → serve index
   ├─ SPA + GET/HEAD + no dot → try <cleanPath>.html sibling, else root index
   ├─ Browse enabled          → serve directory listing
   └─ else                    → NewHTTPError(404, MsgKeyNotFound)
   (When SPA branch matches a candidate request, Browse listing is
    intentionally skipped — SPA navigation should not expose a browseable
    file index for routes the SPA owns.)
7. Headers      X-Content-Type-Options: nosniff (always)
                Cache-Control: whatever the CacheControl hook returns
                ("" or nil hook → no header; hook runs only for status < 400)
                Content-Disposition: attachment (if Download)
8. Serve        http.ServeContent (Range, If-Modified-Since, Content-Type)
                Fallback: io.Copy for non-seekable fs.File
```

### Path Sanitization

```go
func sanitizeStaticPath(p string) (string, error) {
    // Input has already been decoded with url.PathUnescape.
    // 1. Reject null bytes → ErrBadRequest
    // 2. Reject backslashes → ErrBadRequest
    // 3. Reject explicit ".." path segments → ErrBadRequest
    // 4. path.Clean("/" + p) — platform-independent normalization
    // 5. Strip leading "/"
    // 6. Empty → "." (root directory for fsys.Open)
}
```

Uses `path.Clean` (not `filepath.Clean`) for platform-independent behavior.

### Headers

All static responses include `X-Content-Type-Options: nosniff` without exception — regular files, index files, SPA fallback, and directory listings. `Content-Disposition` is applied to directory listings when configured.

`Cache-Control` is decided entirely by the `CacheControl` hook. It is called once per successfully served response (status below 400, including 206 partial content) with the resolved `StaticCacheContext` — request path, resolved file path, file name, and whether the response is an HTML entry point or listing. The returned string is written verbatim; `""` writes no header; a nil hook disables `Cache-Control` entirely. Error responses never consult the hook, so a missing hashed asset cannot inherit a long-lived cache policy.

Hooks run per request and should be pure and deterministic. They must not depend on mutable counters, clocks, random values, or other side effects; otherwise identical assets may receive inconsistent cache policies.

The presets encode the two common policies:

| Preset | Successful non-HTML response | Successful HTML response / listing |
| --- | --- | --- |
| `StaticCacheMaxAge(d)` | `public, max-age=N` | `public, max-age=N` |
| `StaticCacheImmutableAssets(d)` | `public, max-age=N, immutable` | `no-cache, must-revalidate` |

Anything finer-grained — e.g. marking only `_app/immutable/` as immutable — is a custom hook over `StaticCacheContext.FilePath`.

As a global defense-in-depth rule, Credo rewrites inherited `immutable` `Cache-Control` directives on 4xx/5xx responses to `no-cache, must-revalidate`. This protects missing hashed assets from being cached for a full immutable lifetime if an application middleware set cache headers before the final status was known. This guard only covers headers present before Credo writes the response. Reverse proxies or CDNs that add `Cache-Control` after Credo must be configured separately.

### Preset Duration Resolution

Both presets floor the duration to whole seconds at construction time. `StaticCacheMaxAge` panics on negative durations and emits no header when the floor is 0; `StaticCacheImmutableAssets` panics unless the floor is at least 1 second. Invalid cache configuration is a programmer error, caught at the preset call site (which is the registration site).

| Input                     | Seconds | `StaticCacheMaxAge` header |
| ------------------------- | ------- | -------------------------- |
| `0`                       | 0       | (none)                     |
| `24 * time.Hour`          | 86400   | `max-age=86400`            |
| `1500 * time.Millisecond` | 1       | `max-age=1`                |
| `500 * time.Millisecond`  | 0       | (none)                     |
| `-1 * time.Second`        | —       | panic                      |

---

## Security

| Threat | Mitigation |
| --- | --- |
| Path traversal (`../`) | Explicit `..` segments rejected with 400 Bad Request |
| Symlink escape | `os.Root.FS()` recommended; `os.DirFS` risk documented |
| Encoded path tricks (`%2e`, `%5c`, `%00`) | URL-decoded before sanitization, then validated |
| Backslash traversal | Rejected with 400 Bad Request |
| Null byte injection | Rejected with 400 Bad Request |
| MIME sniffing | `X-Content-Type-Options: nosniff` on all static responses |
| Directory listing leak | `Browse` defaults to false |
| Double encoding (`%252e`) | One decode pass runs before sanitization; still-normalized output cannot escape the FS root |

**Production recommendation** (documented in godoc):

> Use `os.Root.FS()` for disk serving in production. `os.DirFS` does not prevent symlink-based path traversal. See the Go blog post "Traversal-Resistant File APIs" for details.

---

## Package Structure

```
(root package)
├── static.go         Static(), File(), StaticRoute, StaticConfig
├── static_handler.go static handler logic, sanitization, SPA fallback
├── static_browse.go  directory listing renderer
└── static_test.go    tests
```

No `internal/` package needed — the implementation is small and lives in the root package alongside the existing handler/route/context code.

---

## Design Decisions

1. **`fs.FS` not string path** — user controls filesystem source and lifecycle. Supports embed, `os.Root`, `os.DirFS`, test doubles. See [ADR-017](../adr/017-static-file-serving.md).

2. **`*StaticRoute` wrapper, not `*Route`** — `Static` registers two internal routes (catch-all + exact). A single `*Route` return would make `SetMeta` and `Middleware` inconsistent. The wrapper proxies to both, ensuring uniform behavior.

3. **`File` panics on unsupported config** — Silent ignore of `Browse`, `SPA`, `Index` hides configuration mistakes. Registration-time panic matches existing conventions (`Name` duplicate, `checkFrozen`).

4. **SPA dot heuristic** — Accept-header detection is unreliable (`*/*` includes `text/html`). File extension check is deterministic, debug-friendly, and used by industry-standard tools (Vite, CRA).

5. **No built-in compression** — `middleware.Compress()` handles this as a cross-cutting concern. Static-specific compression would duplicate functionality.

6. **No config file integration** — Static config is per-route. Multiple `Static()` calls may have different configs. Global config file keys don't map to this pattern.

7. **Directory listing is minimal** — Framework provides a simple HTML table. Custom listings are built with regular handlers. No template override mechanism (YAGNI).

---

## Implementation Phase

- **Phase 3+**: `Static`, `File`, `StaticRoute`, `StaticConfig`, path sanitization, SPA fallback, directory listing, security headers.
