package credo

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
)

// StaticCacheContext describes a successfully resolved static response for
// cache policy decisions.
type StaticCacheContext struct {
	// RequestPath is the path received from the client before internal rewrites.
	RequestPath string

	// FilePath is the resolved slash-separated path inside the fs.FS.
	// Directory listings use the resolved directory path.
	FilePath string

	// FileName is the base name of the served file, or the generated listing name.
	FileName string

	// IsHTML reports whether the response is an HTML entry point or listing.
	IsHTML bool
}

// StaticConfig configures static file serving for [App.Static], [Group.Static],
// [App.File], and [Group.File].
//
// For [App.File] and [Group.File], only Download and CacheControl apply.
// Setting Browse, SPA, or a non-empty Index panics at registration time.
//
// Credo can only guard headers written before its Response.WriteHeader call.
// Reverse proxies or CDNs that add Cache-Control after Credo must be configured
// separately.
type StaticConfig struct {
	// Index is the file served for directory requests.
	// Default: "index.html".
	Index string

	// Browse enables directory listing when a directory has no index file.
	// SPA takes precedence: if SPA is also true and the request qualifies
	// as an SPA candidate, the sibling-or-root fallback runs instead of
	// the directory listing.
	// Default: false (returns 404 for directories without index).
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
	// served response (status below 400). It receives the resolved cache
	// context and returns the full header value; returning "" writes no
	// header. Nil disables Cache-Control entirely.
	//
	// The hook runs once per response — including 206 partial-content
	// responses — and never for error statuses. It should be pure and
	// deterministic.
	//
	// Presets cover the common policies:
	//
	//	credo.StaticConfig{CacheControl: credo.StaticCacheMaxAge(24 * time.Hour)}
	//	credo.StaticConfig{CacheControl: credo.StaticCacheImmutableAssets(365 * 24 * time.Hour)}
	//
	// Anything else is a few lines of code — e.g. SvelteKit-style builds
	// where only one directory holds content-hashed assets:
	//
	//	CacheControl: func(c credo.StaticCacheContext) string {
	//		if strings.HasPrefix(c.FilePath, "_app/immutable/") {
	//			return "public, max-age=31536000, immutable"
	//		}
	//		return "no-cache, must-revalidate"
	//	}
	CacheControl func(StaticCacheContext) string
}

// StaticCacheMaxAge returns a [StaticConfig.CacheControl] hook that serves
// every successful response with "public, max-age=N". Sub-second durations
// are floored to whole seconds; if the floored result is 0 the hook writes
// no header. Panics if maxAge is negative.
func StaticCacheMaxAge(maxAge time.Duration) func(StaticCacheContext) string {
	validateMaxAge(maxAge)
	secs := resolveMaxAge(maxAge)
	if secs == 0 {
		return func(StaticCacheContext) string { return "" }
	}
	value := fmt.Sprintf("public, max-age=%d", secs)
	return func(StaticCacheContext) string { return value }
}

// StaticCacheImmutableAssets returns a [StaticConfig.CacheControl] hook for
// content-hashed builds: non-HTML assets get "public, max-age=N, immutable",
// while HTML responses (index files, SPA fallbacks, prerendered pages, and
// directory listings) get "no-cache, must-revalidate" so entry points stay
// refreshable. Panics if maxAge is negative or floors below 1 second.
func StaticCacheImmutableAssets(maxAge time.Duration) func(StaticCacheContext) string {
	validateMaxAge(maxAge)
	secs := resolveMaxAge(maxAge)
	if secs == 0 {
		panic("credo: StaticCacheImmutableAssets requires a maxAge of at least 1 second")
	}
	assetValue := fmt.Sprintf("public, max-age=%d, immutable", secs)
	return func(c StaticCacheContext) string {
		if c.IsHTML {
			return cacheControlNoCacheMustRevalidate
		}
		return assetValue
	}
}

// indexName returns the configured index file name, defaulting to "index.html".
func (cfg *StaticConfig) indexName() string {
	if cfg.Index != "" {
		return cfg.Index
	}
	return "index.html"
}

// StaticRoute represents a static file serving endpoint. It wraps the two
// internal GET routes created by [App.Static] (catch-all + exact prefix
// match) and proxies fluent configuration to both. HEAD requests
// automatically observe the same middleware and route metadata as GET,
// so calling [StaticRoute.Middleware] or [StaticRoute.SetMeta] keeps
// the two methods in sync without any extra bookkeeping.
type StaticRoute struct {
	primary *Route // catch-all GET: /prefix/{_static...}
	index   *Route // exact GET:     /prefix
	prefix  string // cleaned prefix (no trailing slash)
}

// Name sets the route name on the primary (catch-all) route only.
// Route names must be unique; the exact-match route remains unnamed.
// Use [StaticRoute.BuildURI] for URL generation instead of
// [Route.BuildURI], which requires the catch-all parameter explicitly.
//
// Must be called before the server starts; panics if called after compile.
func (sr *StaticRoute) Name(name string) *StaticRoute {
	sr.primary.Name(name)
	return sr
}

// SetMeta sets metadata on both the catch-all and exact-match GET routes.
// HEAD requests automatically see the same metadata as GET, so middleware
// reading route meta returns consistent values for both methods and both
// registration patterns.
//
// Must be called before the server starts; panics if called after compile.
func (sr *StaticRoute) SetMeta(key string, val any) *StaticRoute {
	sr.primary.SetMeta(key, val)
	sr.index.SetMeta(key, val)
	return sr
}

// Middleware appends middleware to both the catch-all and exact-match GET
// routes. HEAD requests automatically run the same chain as GET, so auth,
// rate limiting, and other header-mutating middleware can never be
// silently bypassed via a HEAD request.
//
// Must be called before the server starts; panics if called after compile.
func (sr *StaticRoute) Middleware(m ...Middleware) *StaticRoute {
	sr.primary.Middleware(m...)
	sr.index.Middleware(m...)
	return sr
}

// BuildURI returns the URL path for a file within this static endpoint.
//
//	sr.BuildURI("css/app.css")  → "/static/css/app.css"
//	sr.BuildURI("")             → "/static/"
//	sr.BuildURI()               → "/static/"
func (sr *StaticRoute) BuildURI(filePath ...string) string {
	p := ""
	if len(filePath) > 0 {
		p = strings.TrimLeft(filePath[0], "/")
	}
	if p == "" {
		if sr.prefix == "/" {
			return "/"
		}
		return sr.prefix + "/"
	}
	if sr.prefix == "/" {
		return "/" + p
	}
	return sr.prefix + "/" + p
}

// DirFS returns a traversal-safe [fs.FS] rooted at dir, together with an
// [io.Closer] that releases the underlying directory handle.
//
// Unlike [os.DirFS], it is backed by [os.Root], so symlinks that resolve outside
// dir are refused — closing a common path-traversal hole when serving files from
// disk. Prefer it over os.DirFS for disk-backed static serving.
//
// The FS holds an open directory handle until the closer is called. Register the
// closer for graceful shutdown so the handle is released cleanly:
//
//	fsys, closer, err := credo.DirFS("./public")
//	if err != nil {
//		return err
//	}
//	app.Static("/assets", fsys)
//	app.OnShutdown(func(context.Context) error { return closer.Close() })
//
// It returns an error if dir cannot be opened.
func DirFS(dir string) (fs.FS, io.Closer, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("credo: DirFS(%q): %w", dir, err)
	}
	return root.FS(), root, nil
}

// Static registers routes that serve files from fsys under the given URL prefix.
// Returns a [*StaticRoute] for fluent configuration (Name, SetMeta, Middleware).
//
// Internally registers two routes: a catch-all for file paths and an exact
// match for the prefix itself (serves the index file). Both share the same
// handler and receive fluent configuration uniformly via StaticRoute.
//
// Panics if prefix contains { or } (route parameters in a static prefix are
// not meaningful) or if called after compile. The cache presets
// ([StaticCacheMaxAge], [StaticCacheImmutableAssets]) panic at their own
// call site on invalid durations.
//
// Production recommendation: use [DirFS] (or [os.Root].FS()) for symlink-safe
// disk serving. [os.DirFS] does not prevent symlink-based path traversal.
func (app *App) Static(prefix string, fsys fs.FS, cfgs ...StaticConfig) *StaticRoute {
	return app.root.Static(prefix, fsys, cfgs...)
}

// File registers a single GET route that serves one named file from fsys.
// Returns [*Route] for standard fluent chaining (Name, SetMeta, Middleware).
//
// Only the Download and CacheControl fields of [StaticConfig] are supported.
// Setting Browse, SPA, or a non-empty Index panics at registration time.
//
// Panics if called after compile.
func (app *App) File(urlPath string, fsys fs.FS, name string, cfgs ...StaticConfig) *Route {
	return app.root.File(urlPath, fsys, name, cfgs...)
}

// Static registers routes that serve files from fsys under the given URL prefix
// within this group. See [App.Static] for full documentation.
func (g *Group) Static(prefix string, fsys fs.FS, cfgs ...StaticConfig) *StaticRoute {
	g.app.checkFrozen("Static")
	validateStaticPrefix(prefix)

	cfg := StaticConfig{}
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	}

	fullPrefix := joinPath(g.prefix, prefix)
	// Normalize: strip trailing slash for consistent path building,
	// but preserve "/" for the root prefix case.
	cleanPrefix := strings.TrimRight(fullPrefix, "/")
	if cleanPrefix == "" {
		cleanPrefix = "/"
	}

	h := newStaticHandler(fsys, cfg)

	// Catch-all route: /prefix/{_static...}
	var catchAllPattern string
	if cleanPrefix == "/" {
		catchAllPattern = "/{_static...}"
	} else {
		catchAllPattern = cleanPrefix + "/{_static...}"
	}
	primary := g.app.addGetRoute(catchAllPattern, h, g)

	// Exact match route: /prefix (serves index)
	indexHandler := newStaticIndexHandler(fsys, cfg)
	index := g.app.addGetRoute(cleanPrefix, indexHandler, g)

	return &StaticRoute{
		primary: primary,
		index:   index,
		prefix:  cleanPrefix,
	}
}

// File registers a single GET route that serves one named file from fsys
// within this group. See [App.File] for full documentation.
func (g *Group) File(urlPath string, fsys fs.FS, name string, cfgs ...StaticConfig) *Route {
	g.app.checkFrozen("File")

	cfg := StaticConfig{}
	if len(cfgs) > 0 {
		cfg = cfgs[0]
		validateFileConfig(cfg)
	}

	fullPath := joinPath(g.prefix, urlPath)
	h := newFileHandler(fsys, name, cfg)
	return g.app.addGetRoute(fullPath, h, g)
}

// validateStaticPrefix panics if the prefix contains route parameter characters.
func validateStaticPrefix(prefix string) {
	if strings.ContainsAny(prefix, "{}") {
		panic(fmt.Sprintf("credo: Static prefix must not contain route parameters: %q", prefix))
	}
}

// validateFileConfig panics if unsupported StaticConfig fields are set for File().
func validateFileConfig(cfg StaticConfig) {
	if cfg.Browse {
		panic("credo: StaticConfig.Browse is not supported by File()")
	}
	if cfg.SPA {
		panic("credo: StaticConfig.SPA is not supported by File()")
	}
	if cfg.Index != "" {
		panic("credo: StaticConfig.Index is not supported by File()")
	}
}

// validateMaxAge panics if duration is negative.
func validateMaxAge(d time.Duration) {
	if d < 0 {
		panic("credo: static cache maxAge must not be negative")
	}
}

// resolveMaxAge converts a duration to Cache-Control max-age seconds.
// Sub-second durations are floored. Returns 0 when no header should be written.
func resolveMaxAge(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int(d.Seconds())
}
