# Static File Serving Guide

This guide explains how to serve static files, single files, and single-page applications with Credo. For internal design rationale, see the [Static Spec](../specs/static.md) and [ADR-017](../adr/017-static-file-serving.md).

---

## Quick Start

```go
package main

import (
    "embed"
    "io/fs"
    "log"

    "github.com/credo-go/credo"
)

//go:embed public/*
var publicFS embed.FS

func main() {
    app, err := credo.New()
    if err != nil {
        log.Fatal(err)
    }

    sub, _ := fs.Sub(publicFS, "public")
    app.Static("/static", sub)

    if err := app.Run(); err != nil {
        log.Fatal(err)
    }
}
```

Requests to `/static/css/app.css` serve `public/css/app.css` from the embedded filesystem. `/static` (no trailing slash) serves the index file.

---

## Filesystem Sources

`Static` and `File` accept `fs.FS` — the standard library interface. You choose the source and manage its lifecycle.

### embed.FS (single-binary deployment)

```go
//go:embed assets/*
var assetsFS embed.FS

sub, _ := fs.Sub(assetsFS, "assets")
app.Static("/assets", sub)
```

### os.Root (production, symlink-safe)

```go
root, err := os.OpenRoot("./public")
if err != nil {
    log.Fatal(err)
}
app.OnShutdown(func(ctx context.Context) error {
    return root.Close()
})

app.Static("/static", root.FS())
```

`os.Root` prevents symlink-based path traversal. This is the recommended source for serving files from disk in production.

### os.DirFS (development convenience)

```go
app.Static("/static", os.DirFS("./public"))
```

`os.DirFS` does **not** prevent symlink escape. Use it only during development or when the directory is fully trusted.

### Custom fs.FS

Any type implementing `fs.FS` works — in-memory filesystems, CDN backends, testing doubles, etc.

---

## SPA Mode

Single-page applications (React, Vue, Angular) need a catch-all that serves `index.html` for client-side routes while still returning 404 for missing assets.

```go
sub, _ := fs.Sub(publicFS, "dist")
app.Static("/", sub, credo.StaticConfig{SPA: true})
```

Credo uses a **dot heuristic** to decide:

| Request path     | Has dot? | Result                      |
| ---------------- | -------- | --------------------------- |
| `/dashboard`     | no       | `index.html` (SPA fallback) |
| `/users/123`     | no       | `index.html` (SPA fallback) |
| `/app.js`        | yes      | 404 (missing asset)         |
| `/css/style.css` | yes      | 404 (missing asset)         |

SPA fallback only triggers for **GET and HEAD** requests. The static catch-all is registered as a GET route (with an auto-generated HEAD twin), so a POST to any path under the prefix matches the route pattern but finds no POST handler — the router returns **405 Method Not Allowed**, not 404.

### Prerendered routes (SvelteKit, Next.js export, Astro static)

Static-adapter frameworks emit each prerendered route as a sibling `<route>.html` file. When SPA mode is on, Credo prefers these files over the SPA shell so prerendered HTML is served directly (faster, no client boot, correct initial paint):

```
build/
├── index.html               ← SPA shell
├── admin/
│   └── users.html           ← prerendered route
├── reports.html             ← parent route prerender
└── reports/
    └── crm.html             ← child route prerender
```

| Request path   | Served file              | Notes                         |
| -------------- | ------------------------ | ----------------------------- |
| `/admin/users` | `admin/users.html`       | Sibling `.html` (file branch) |
| `/reports`     | `reports.html`           | Sibling `.html` (dir branch)  |
| `/reports/crm` | `reports/crm.html`       | Sibling `.html` (file branch) |
| `/missing`     | `index.html` (SPA shell) | No sibling, SPA fallback      |
| `/app.js`      | 404                      | Asset path (dot heuristic)    |

Both branches require the dot heuristic and method restriction — a request like `/admin/users.json` returns 404 instead of serving `admin/users.html`.

### SPA + Browse

When `cfg.SPA` and `cfg.Browse` are both enabled, the SPA branch wins for SPA-candidate paths (GET/HEAD + no dot in last segment). Directory listings are intentionally suppressed for those requests so SPA navigation cannot accidentally expose a browseable file index. Browse remains active for non-SPA-candidate requests handled by the same prefix.

---

## Directory Browsing

```go
app.Static("/files", root.FS(), credo.StaticConfig{Browse: true})
```

When a directory has no index file, Credo renders a minimal HTML table with file name, size, and modification date. Browse is **off by default** — a directory without an index file returns 404 unless Browse is enabled.

---

## Single File Serving

```go
app.File("/favicon.ico", publicFS, "favicon.ico")
app.File("/robots.txt", os.DirFS("."), "robots.txt")
app.File("/report.pdf", uploadsFS, "reports/q1.pdf", credo.StaticConfig{
    Download: true,
})
```

`File` returns `*Route` (not `*StaticRoute`), so standard fluent chaining works. Only the `Download` and `CacheControl` config fields are supported — setting `Browse`, `SPA`, or `Index` panics at registration time.

---

## Caching

Cache policy is one hook: `CacheControl func(StaticCacheContext) string` decides the `Cache-Control` header per successfully served response. Two presets cover the common policies. The simplest — every response gets the same max-age:

```go
app.Static("/assets", sub, credo.StaticConfig{
    CacheControl: credo.StaticCacheMaxAge(24 * time.Hour),
})
```

| `StaticCacheMaxAge` input | Header                                       |
| ------------------------- | -------------------------------------------- |
| `24 * time.Hour`          | `Cache-Control: public, max-age=86400`       |
| `1500 * time.Millisecond` | `Cache-Control: public, max-age=1` (floored) |
| `500 * time.Millisecond`  | (none, floors to 0)                          |
| negative                  | panic at the preset call                     |

Leaving `CacheControl` nil writes no `Cache-Control` header at all.

For content-hashed SPA assets, use `StaticCacheImmutableAssets`:

```go
app.Static("/", dist, credo.StaticConfig{
    SPA:          true,
    CacheControl: credo.StaticCacheImmutableAssets(365 * 24 * time.Hour),
})
```

HTML responses (`index.html`, SPA fallback, prerendered `.html`, and directory listings) receive `no-cache, must-revalidate` from this preset, so SPA entry points stay refreshable while JS/CSS/image assets are cached with:

```http
Cache-Control: public, max-age=31536000, immutable
```

When one `Static()` tree mixes hashed and non-hashed files, write the policy as a custom hook. The context carries the resolved `FilePath`, so SPA fallback paths like `/dashboard` cannot accidentally be treated as assets:

```go
app.Static("/", dist, credo.StaticConfig{
    SPA: true,
    CacheControl: func(c credo.StaticCacheContext) string {
        if !c.IsHTML && strings.HasPrefix(c.FilePath, "_app/immutable/") {
            return "public, max-age=31536000, immutable"
        }
        return "no-cache, must-revalidate"
    },
})
```

The hook is called once per successful response — including 206 partial content — and never for error statuses; returning `""` writes no header. Keep hooks pure and deterministic; they run per request and should not depend on mutable counters, clocks, or random values.

If a request later becomes a 4xx/5xx error and an earlier middleware already set an `immutable` cache header, Credo rewrites it to:

```http
Cache-Control: no-cache, must-revalidate
```

This prevents missing hashed assets from being cached for the full immutable lifetime. The guard only covers headers set before Credo writes the response. If a reverse proxy or CDN adds `Cache-Control` after Credo, configure that layer separately.

---

## Download Mode

```go
app.Static("/downloads", root.FS(), credo.StaticConfig{
    Download:     true,
    CacheControl: credo.StaticCacheMaxAge(1 * time.Hour),
})
```

Sets `Content-Disposition: attachment` on all responses, prompting the browser to save instead of display inline.

---

## Middleware and Route Meta

Static routes are full participants in Credo's middleware and route meta systems.

```go
admin := app.Group("/admin")
admin.Static("/assets", adminFS).
    SetMeta("permission", "admin.assets").
    Middleware(auth.Middleware[*User](authenticator, nil))
```

`SetMeta` and `Middleware` apply to **both** internal GET routes (catch-all and exact match). Each GET route also has an auto-generated HEAD twin, and the underlying `Route.SetMeta` / `Route.Middleware` propagate to that twin — so HEAD requests run the same middleware chain and see the same route meta as GET. Auth, rate limiting, and other header-mutating middleware never get silently bypassed via HEAD.

This propagation is universal across the framework: `app.GET(...)`, `group.GET(...)`, `app.File(...)` and `group.File(...)` all create a HEAD twin that inherits configuration through the same mechanism. If you explicitly register a HEAD route for the same pattern, the auto-twin is silently skipped and middleware on the GET route does **not** leak to the explicit HEAD route — register middleware on each route as needed.

---

## URL Generation

```go
sr := app.Static("/assets", sub).Name("assets")

sr.BuildURI("css/app.css")  // "/assets/css/app.css"
sr.BuildURI("")             // "/assets/"
sr.BuildURI()               // "/assets/"
```

Use `StaticRoute.BuildURI` for clean URLs. `Route.BuildURI` on the underlying catch-all route requires the `_static` parameter explicitly.

---

## Path Sanitization and Security

Credo sanitizes every incoming static file path **before** it reaches the filesystem. The process has two stages:

### 1. Decode

The captured path is URL-decoded with `url.PathUnescape`. Malformed percent-encoding sequences (e.g., `%ZZ`) return **400 Bad Request**.

### 2. Sanitize

```
Input (decoded)
  │
  ├── contains \x00 (null byte)?  → 400 Bad Request
  ├── contains \ (backslash)?     → 400 Bad Request
  ├── contains .. path segment?   → 400 Bad Request
  └── path.Clean("/" + p)         → normalized, relative path
```

- **Null bytes** are rejected because they can truncate paths in C-backed filesystem implementations.
- **Backslashes** are rejected because they are not valid in URL paths and can be used for path traversal on Windows (`..\..\etc`).
- **Explicit `..` path segments** are rejected before normalization. Credo does not silently rewrite traversal attempts into in-root paths.
- **`path.Clean`** (not `filepath.Clean`) is used for platform-independent normalization of the remaining safe path. It collapses `./` and double slashes regardless of the host OS.

After cleaning, the leading `/` is stripped to produce a relative path for `fs.FS` (which expects relative paths from its root).

### What this means in practice

| Request | After sanitization | Result |
| --- | --- | --- |
| `/static/css/app.css` | `css/app.css` | Served normally |
| `/static/../../../etc/passwd` | — | 400 (`..` rejected) |
| `/static/foo%2e%2e/bar` | `foo../bar` | Normalized by path.Clean (segment is `foo..`, not `..`) |
| `/static/foo\bar` | — | 400 (backslash rejected) |
| `/static/foo%00bar` | — | 400 (null byte rejected) |
| `/static/%2e%2e/%2e%2e/secret` | — | 400 (`%2e%2e` decodes to `..`, rejected before path.Clean) |
| `/static/%252e%252e/secret` | `%2e%2e/secret` | 404 (one decode pass yields literal `%2e%2e`, no such file) |

### Defense in depth

The sanitization layer works **in front of** `fs.FS`. Even if you use `os.DirFS` (which does not prevent symlink escape), `../` traversal is neutralized before the path reaches the filesystem.

For production disk serving, use `os.Root.FS()` for full symlink protection as an additional layer:

```go
// Two layers of protection:
// 1. Credo sanitizes the path (no ../, no \, no \x00)
// 2. os.Root prevents symlink escape
root, _ := os.OpenRoot("./public")
app.Static("/static", root.FS())
```

### Security headers

All static responses include `X-Content-Type-Options: nosniff` — regular files, index files, SPA fallback, and directory listings. This prevents browsers from MIME-sniffing responses into executable content types.

---

## Group Integration

Static routes work inside groups and inherit group middleware:

```go
api := app.Group("/api")
api.Middleware(rateLimiter)

docs := api.Group("/docs")
docs.Static("/assets", docsFS)
// Serves: /api/docs/assets/...
// Inherits: rateLimiter middleware
```

---

## Related Documents

- [Static Spec](../specs/static.md) — full API contracts, request flow, design decisions
- [ADR-017](../adr/017-static-file-serving.md) — architecture decision
- [Routing Guide](routing.md) — path parameters, groups, host routing
- [Middleware Guide](middleware.md) — middleware tiers, meta-driven behavior
