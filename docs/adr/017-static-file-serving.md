# ADR-017: Static File Serving

**Status:** Accepted **Date:** 2026-03-11 **Depends on:** ADR-007, ADR-009 **Related:** ADR-010, ADR-024 **Specification:** [Static file serving spec](../specs/static.md) **Guide:** [Static file serving guide](../guides/static-files.md)

## Context

Web applications serve static assets (CSS, JS, images, fonts) and single-page applications (SPAs) beside their API routes. Static serving outside the route pipeline loses what every other route gets: route meta, route and group middleware such as auth and rate limiting, and the centralized error envelope. A hand-written catch-all handler keeps the pipeline but leaves every application to re-implement path sanitization, index resolution and SPA fallback.

The file path comes from the client, so the serving path is an attack surface. Traversal (`..`, its encoded and backslash forms, NUL bytes) must be refused before the filesystem sees the path, and a symlink inside the served directory must not lead outside it, which no lexical check can see. `os.Root` provides symlink-safe, sandboxed filesystem access through `Root.FS()`; combined with `http.ServeContent` (range requests, conditional GETs) it is a secure and complete foundation for static file serving.

SPAs add a routing force: a client-side route (`/dashboard`) must reach the application shell, while a missing asset (`/app.js`) must stay a 404 instead of answering with HTML that fails confusingly in the browser.

## Decision

Static file serving is a first-class route registration feature. `Static` and `File` live on both `App` and `Group`, follow the `GET`/`POST` registration pattern, accept `fs.FS` (the stdlib interface) and produce Credo handlers that run in the full middleware and error pipeline: a missing file or a rejected path is the standard `ErrNotFound` / `ErrBadRequest`, rendered like any other handler error. Signatures, configuration and the request flow are in the [static file serving spec](../specs/static.md).

### `fs.FS` as the Source Interface

The methods accept `fs.FS`, not a string directory path, so the filesystem source and its lifecycle stay visible at the application boundary. One interface covers single-binary deployment (`embed.FS` via `fs.Sub`), symlink-safe disk serving (`os.Root`), development convenience (`os.DirFS`, without symlink protection) and test doubles or remote backends.

`credo.DirFS` is the `os.Root`-backed default for disk serving. The caller closes it in an `OnStop` hook; an internal-tier stop hook runs after the HTTP drain, so no request still reads from it. Credo never manages the root or silently swaps in a sandbox, so the security choice stays at the call site.

### Static Method

`Static` registers two GET routes: the catch-all `prefix/{_static...}`, whose capture selects the file, and the exact `prefix`, which serves the index. Both serve through the same logic and configuration. `Static`'s own prefix is the literal location of the file tree, so a `{` or `}` in it panics; a parametric group prefix is accepted, with `_static` reserved for the capture.

### StaticRoute Wrapper

`Static` returns `*StaticRoute` instead of `*Route` because it registers two routes, and a single `*Route` would apply `SetMeta` and `Middleware` to one of them only. The wrapper keeps four methods with `Route` semantics: `SetMeta` and `Middleware` reach both routes, `Name` names the catch-all only because route names are unique, and `BuildURI` builds a file URL without exposing the capture name ([spec](../specs/static.md#staticroute)).

Every auto-generated HEAD twin receives its GET route's middleware and meta, so a HEAD request cannot bypass auth or rate limiting that guards GET; an explicitly registered HEAD route keeps its own chain. This holds for every GET route, not only static ones.

### File Method

`File` registers one GET route that serves one named file and returns `*Route`. The `StaticConfig` fields that cannot apply to a single file (`Browse`, `SPA`, `Index`) panic at registration instead of being silently ignored, following the fail-fast convention for developer configuration ([spec](../specs/static.md#file-single-file-serving)).

### StaticConfig

Every boolean field defaults to `false` and is a plain `bool`, not `*bool`: the zero values are the desired defaults. The `*bool` pattern is reserved for nil-means-true toggles such as `HealthConfig.Enabled`.

### Cache-Control

Cache policy is one hook, `CacheControl func(StaticCacheContext) string`, plus two presets. Cache policy differs per application (one build marks every non-HTML asset immutable, another only one content-hashed directory), and a function over the resolved file expresses each policy in a few lines. A matrix of fields (a max-age, an immutable flag, a predicate selecting immutable assets, an override for HTML) was rejected: every combination needs its own validation rule, and the matrix still cannot express every policy.

The hook is status-aware: it runs for successful responses, including 206, and never for an error status, so a missing hashed asset never inherits a long-lived policy. `StaticCacheImmutableAssets` makes non-HTML assets immutable while HTML entry points get `no-cache, must-revalidate`, so the entry point that references the hashed assets stays refreshable after a deploy.

As defense in depth, an error status never carries an inherited `immutable` policy: `Response.WriteHeader` rewrites it to `no-cache, must-revalidate`, which covers cache headers that middleware set before the final status was known. Reverse proxies or CDNs that add cache headers after Credo are outside this guard. The hook contract, the presets and their duration rules are in the [spec](../specs/static.md#cache-control).

### SPA Fallback Scope

SPA mode does not serve `index.html` for every missing path. A dot heuristic separates navigation from asset requests: a missing path whose last segment has no dot falls back, one with a dot (`app.js`) answers 404. This keeps broken asset references from silently returning HTML, and it is the dot rule that Vite and connect-history-api-fallback apply. Detecting navigation from the `Accept` header was rejected: clients send `*/*`, which includes `text/html`, while the extension check is deterministic and easy to debug.

The fallback is limited to GET and HEAD. Static routes are GET routes with HEAD twins, so any other method gets the router's 405 Method Not Allowed and never reaches the fallback.

#### SPA fallback preference order

Credo prefers prerendered HTML over booting the SPA shell. Static-adapter outputs (SvelteKit, Next.js export, Astro static, Eleventy) write each prerendered route as a sibling `<route>.html`, and serving it directly is faster and paints without a client boot. A directory declares its own entry point first: its index wins over a sibling `<dir>.html`, which wins over the shell, so a `/reports.html` parent route can coexist with a `/reports/` directory of child routes. The branch order and examples are in the [spec](../specs/static.md#spa-fallback).

#### SPA + Browse interaction

When `SPA` and `Browse` are both on, an SPA candidate takes the SPA fallback and never a directory listing: SPA navigation must not expose a browseable file index for routes the SPA owns. Requests that are not SPA candidates still get listings.

### Security Model

Path safety has two layers with separate jobs. The sanitizer is lexical and runs on every request: it rejects NUL bytes, backslashes and explicit `..` segments with 400 instead of normalizing them, so a traversal attempt is refused rather than silently rewritten into an in-root path. Exactly one decode, the router's, runs before it, so a double-encoded `%252e%252e` stays the literal name `%2e%2e` and never becomes `..`. A lexical check cannot see symlinks, so confinement against a link that leads outside the directory is the filesystem's job: `credo.DirFS` (or `os.Root.FS()`) provides it, `os.DirFS` does not, which is one more reason the source stays visible at the call site. Served content carries `X-Content-Type-Options: nosniff`, and `Browse` is off by default, so a directory never exposes its listing unless asked. The threat table is in the [spec](../specs/static.md#security).

## Rejected Alternatives

- **Document a `Mount` + `http.FileServerFS` recipe.** Minimal framework work, but a mount carries no route meta and no route or group middleware, and the file server answers its own plain-text errors outside the error pipeline, so auth integration and consistent error responses are lost.
- **Middleware-based serving (the Fiber pattern).** Flexible, but middleware that serves files short-circuits the handler chain, which leaves the semantics of route meta and per-route middleware unclear.

## Consequences

**Positive:**

- Static routes participate in all Credo systems: middleware, route meta, error handling, access log, request ID.
- The `fs.FS` interface supports embed, `os.Root`, `os.DirFS` and custom implementations, with no lock-in to a specific filesystem.
- The SPA dot heuristic prevents the common pitfall of assets returning HTML.
- `StaticRoute` applies `SetMeta` and `Middleware` consistently to both internal routes.
- Security defaults are safe: no directory listing, `nosniff`, path sanitization.

**Negative:**

- `*StaticRoute` is a type distinct from `*Route`, adding API surface. Mitigated by keeping only four methods, all with familiar semantics.
- The dot heuristic has a false negative for page paths such as `/users/john.doe`. Such URLs are uncommon in SPAs; an application that needs them writes a custom handler.
- No static-specific compression or ETag generation. Compression is the cross-cutting `app.UseCompress` feature, and `http.ServeContent` answers conditional requests from `Last-Modified`.
- No configuration-file integration: static configuration is per route and several `Static` calls may differ, which global config keys do not model.
- The directory listing is a minimal HTML table with no template override; a custom listing is a regular handler.

**Risks:**

- A non-seekable `fs.File` is streamed with `io.Copy` and answers 200 only: no Range or conditional requests. `embed.FS`, `os.Root` and `os.DirFS` files are seekable and get the full `http.ServeContent` behavior.
- Users may pass `os.DirFS` in production, which lacks symlink protection. Mitigated by documentation recommending `credo.DirFS` (or `os.Root.FS()`).
