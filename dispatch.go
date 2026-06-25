package credo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/credo-go/credo/internal/radix"
)

// routeHandler is the payload the radix tree stores for every endpoint.
// Credo routes carry route/handler/compiled; handlers attached via
// [App.Mount] carry only mounted and are dispatched as raw http.Handlers
// outside the per-route compiled chain.
type routeHandler struct {
	route    *Route
	handler  Handler
	compiled Handler // precompiled: group parent chain + route.middlewares + handler

	// mounted is the stdlib handler for Mount-registered endpoints.
	// nil for regular Credo routes; when non-nil all other fields are zero.
	mounted http.Handler
}

// compile builds the handler chain: globalMW[0] → ... → globalMW[n] → dispatch.
// It also precompiles per-route middleware chains and freezes the app.
func (app *App) compile() {
	// Sort host entries by specificity (most specific first).
	slices.SortStableFunc(app.hosts, compareHostEntries)

	// Precompile route chains for the default mux.
	app.compileRoutes(app.mux)

	// Precompile route chains for each host mux.
	for _, h := range app.hosts {
		app.compileRoutes(h.mux)
	}

	// Build global chain.
	var handler Handler = app.dispatch
	for i := len(app.globalMW) - 1; i >= 0; i-- {
		handler = app.globalMW[i](handler)
	}

	// Built-in error handler: catches handler errors, writes the error
	// response inline so that outer layers (access log) observe the final
	// response state (status, bytes, duration).
	handler = app.builtinErrorHandler(handler)

	// Built-in recovery: catches panics from handlers/middleware, writes
	// the 500 response via handleError. Placed inside builtinAccessLog so
	// the access log's defer fires AFTER the panic response is committed,
	// giving correct bytes/status/duration even on the panic path.
	if !app.disableRecover {
		handler = builtinRecover(handler)
	}

	// Built-in access logger (defer-based). Outermost observability layer
	// so its defer fires after both builtinErrorHandler and builtinRecover
	// have written the final response.
	if !app.disableAccessLog {
		handler = app.builtinAccessLog(handler)
	}

	// Built-in request ID (enriches ctx.Logger with request_id).
	// Outermost layer so that all inner layers (access log, recover)
	// benefit from the enriched logger.
	if !app.disableRequestID {
		handler = builtinRequestID(handler)
	}

	app.compiledHandler = handler
	app.frozen.Store(true)
}

// compileRoutes precompiles per-route middleware chains for a given mux.
// Each route handler is wrapped: leaf (errRewrite swallower) → route MW → group MW.
// Mount-registered payloads are skipped — they dispatch as raw http.Handlers.
func (app *App) compileRoutes(m *mux) {
	for _, rh := range m.handlers {
		if rh.mounted != nil {
			continue
		}
		// Capture the leaf handler BEFORE clearing rh.handler.
		leafHandler := rh.handler
		leaf := func(c *Context) error {
			err := leafHandler(c)
			if errors.Is(err, errRewrite) && c.rewriteRequested {
				return nil // swallow: dispatch loop handles it
			}
			return err
		}
		compiled := leaf
		for i := len(rh.route.middlewares) - 1; i >= 0; i-- {
			compiled = rh.route.middlewares[i](compiled)
		}
		// Collect group middleware from the parent chain at compile
		// time: the route's own group wraps innermost, the root group
		// outermost — so execution order is parent before child, the
		// same model LookupMeta uses for metadata.
		for g := rh.route.parent; g != nil; g = g.parent {
			for i := len(g.mws) - 1; i >= 0; i-- {
				compiled = g.mws[i](compiled)
			}
		}
		rh.compiled = compiled
		rh.handler = nil
	}
}

// dispatch is the rewrite-aware dispatch loop. It calls dispatchOnce and
// re-dispatches if the handler signaled an internal rewrite via ctx.Rewrite.
func (app *App) dispatch(c *Context) error {
	for {
		c.rewriteRequested = false
		err := app.dispatchOnce(c)
		if err != nil {
			return err
		}
		if !c.rewriteRequested {
			return nil
		}

		c.rewriteCount++
		if c.rewriteCount > maxRewrites {
			return fmt.Errorf("credo: rewrite loop after %d internal rewrites", maxRewrites)
		}

		target := c.rewriteTarget
		c.rewriteTarget = ""

		// Parse query from target.
		if i := strings.IndexByte(target, '?'); i >= 0 {
			c.request.URL.RawQuery = target[i+1:]
			target = target[:i]
		} else {
			c.request.URL.RawQuery = ""
		}
		c.request.URL.Path = target
		c.request.URL.RawPath = ""
		c.route = nil
		c.request.resetRouteParams()
		c.request.cachedQuery = nil // RawQuery changed above; drop stale query cache
	}
}

// dispatchOnce performs a single radix tree lookup and executes the matched
// handler with group and route middleware.
func (app *App) dispatchOnce(c *Context) error {
	r := c.request.Request

	// Host matching: select the appropriate mux.
	entry, hostParams := app.matchHost(r.Host)
	m := app.mux
	if entry != nil {
		m = entry.mux
	}

	// Get or create RouteContext.
	rctx := getRouteContext(r)
	pooled := false
	if rctx == nil {
		rctx = m.pool.get()
		rctx.Reset()
		pooled = true
	}
	defer func() {
		if pooled {
			m.pool.put(rctx)
		}
	}()

	rctx.RouteMethod = r.Method

	// Use RoutePath if set (by mounted sub-routers), otherwise use URL path.
	path := rctx.RoutePath
	if path == "" {
		if r.URL.RawPath != "" {
			path = r.URL.RawPath
		} else {
			path = r.URL.Path
		}
	}

	// Look up the method's bit flag. An unknown method (not registered
	// in the radix method map) means no routes can possibly match,
	// so treat it as not found rather than method-not-allowed.
	method, ok := radix.LookupMethod(r.Method)
	if !ok {
		return app.resolveStatusHandler(c, http.StatusNotFound)
	}

	rh, found := m.tree.FindRoute(rctx, method, path)

	if found {
		if rh.mounted == nil {
			// Clear params for re-dispatch safety.
			c.request.resetRouteParams()

			// Copy params from radix RouteContext into credo Context.
			for i, key := range rctx.Params.Keys {
				if i < len(rctx.Params.Values) {
					c.request.addRouteParam(key, rctx.Params.Values[i])
				}
			}

			// Inject host parameters.
			for k, v := range hostParams {
				c.request.addRouteParam(k, v)
			}

			c.route = rh.route

			return rh.compiled(c)
		}

		// Mounted stdlib handler — dispatched raw, outside the compiled chain.
		if pooled {
			rh.mounted.ServeHTTP(c.response, withRouteContext(r, rctx))
		} else {
			rh.mounted.ServeHTTP(c.response, r)
		}
		return nil
	}

	if rctx.MethodNotAllowed {
		if allowed := rctx.MethodsAllowed(); allowed != 0 {
			methods := radix.MethodTypToString(allowed)
			c.response.Header().Set("Allow", strings.Join(methods, ", "))
		}
		return app.resolveStatusHandler(c, http.StatusMethodNotAllowed)
	}

	// Trailing slash redirect: probe the alternate path (slash toggled).
	if app.redirectTrailingSlash && r.Method != http.MethodConnect {
		altPath := trailingSlashAlternate(path)
		if altPath != "" {
			probeCtx := m.pool.get()
			probeCtx.Reset()
			_, altFound := m.tree.FindRoute(probeCtx, method, altPath)
			m.pool.put(probeCtx)
			if altFound {
				return app.redirectTrailingSlash301or308(c, r, altPath)
			}
		}
	}

	return app.resolveStatusHandler(c, http.StatusNotFound)
}

// trailingSlashAlternate returns the path with the trailing slash toggled.
// Returns "" when no meaningful alternate exists (the root path "/" cannot
// be stripped to an empty string).
func trailingSlashAlternate(path string) string {
	if path == "/" {
		return ""
	}
	if path[len(path)-1] == '/' {
		return path[:len(path)-1]
	}
	return path + "/"
}

// redirectTrailingSlash301or308 issues a redirect to altPath, preserving the
// query string. GET/HEAD → 301; other methods → 308 (method preserved).
func (app *App) redirectTrailingSlash301or308(c *Context, r *http.Request, altPath string) error {
	code := http.StatusMovedPermanently // 301
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		code = http.StatusPermanentRedirect // 308
	}
	loc := altPath
	if r.URL.RawQuery != "" {
		loc = altPath + "?" + r.URL.RawQuery
	}
	return c.response.Redirect(code, loc)
}

// resolveStatusHandler looks for a custom app-level status handler,
// falling back to sentinel errors.
func (app *App) resolveStatusHandler(c *Context, code int) error {
	if app.statusHandlers != nil {
		if h, ok := app.statusHandlers[code]; ok {
			return h(c)
		}
	}
	switch code {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusMethodNotAllowed:
		return ErrMethodNotAllowed
	default:
		return NewHTTPError(code)
	}
}

// addRoute creates a Route owned by group g and inserts a routeHandler
// into g's mux (the default app.mux when g.mux is nil). The pattern must
// already include the group prefix; callers join it. Group middleware is
// not captured here — compileRoutes collects it from the parent chain.
func (app *App) addRoute(method, pattern string, h Handler, g *Group) *Route {
	app.checkFrozen("route registration")
	if h == nil {
		panic(fmt.Sprintf("credo: nil handler for %s %s", method, pattern))
	}
	app.validateParamNamespace(g.hostPattern, pattern)
	route := &Route{
		method:      method,
		pattern:     pattern,
		handler:     h,
		parent:      g,
		app:         app,
		hostPattern: g.hostPattern,
	}

	rh := &routeHandler{route: route, handler: h}
	m := g.mux
	if m == nil {
		m = app.mux
	}
	m.insert(method, pattern, rh)
	return route
}

// addGetRoute registers a GET route plus its auto-generated HEAD twin.
// Every GET registration path (Group.GET — which App.GET delegates to —
// Static, File) funnels through here so the twin-wiring logic lives in
// exactly one place.
func (app *App) addGetRoute(pattern string, h Handler, g *Group) *Route {
	route := app.addRoute("GET", pattern, h, g)
	if headRoute := app.addHeadRoute(pattern, h, g); headRoute != nil {
		route.headTwin = headRoute
	}
	return route
}

// addHeadRoute registers an auto-generated HEAD handler for a GET route.
// The HEAD handler discards the response body. Returns the registered Route
// so the caller can attach it as the GET route's headTwin, which makes
// subsequent Middleware/SetMeta calls propagate to HEAD automatically.
//
// Returns nil when an explicit HEAD route already exists for this pattern;
// the conflict is silent (the explicit route wins). Callers must nil-check
// before assigning to headTwin.
//
// The pattern is not re-validated against host parameters because every
// caller registers the matching GET route via addRoute first, which already
// runs that check.
func (app *App) addHeadRoute(pattern string, h Handler, g *Group) *Route {
	headHandler := func(ctx *Context) error {
		orig := ctx.response.ResponseWriter
		ctx.response.ResponseWriter = &discardBodyWriter{ResponseWriter: orig}
		defer func() { ctx.response.ResponseWriter = orig }()
		return h(ctx)
	}

	route := &Route{
		method:      "HEAD",
		pattern:     pattern,
		handler:     headHandler,
		parent:      g,
		app:         app,
		hostPattern: g.hostPattern,
		autoHead:    true,
	}

	rh := &routeHandler{route: route, handler: headHandler}
	m := g.mux
	if m == nil {
		m = app.mux
	}
	if !m.tryInsert("HEAD", pattern, rh, true) {
		return nil
	}
	return route
}

func (app *App) validateParamNamespace(hostPattern, pattern string) {
	if hostPattern == "" {
		return
	}

	hostKeys := app.hostParamKeys(hostPattern)
	if len(hostKeys) == 0 {
		return
	}

	hostSet := make(map[string]struct{}, len(hostKeys))
	for _, key := range hostKeys {
		hostSet[key] = struct{}{}
	}

	for _, key := range routePatternParamKeys(pattern) {
		if _, ok := hostSet[key]; ok {
			panic(fmt.Sprintf("credo: route parameter %q conflicts with host parameter on host %q for pattern %q", key, hostPattern, pattern))
		}
	}
}

func routePatternParamKeys(pattern string) []string {
	var keys []string
	for offset := 0; offset < len(pattern); {
		start := strings.IndexByte(pattern[offset:], '{')
		if start < 0 {
			break
		}
		start += offset

		end := radix.FindMatchingBrace(pattern, start)
		if end < 0 {
			break
		}

		inner := pattern[start+1 : end]
		switch {
		case strings.HasSuffix(inner, "..."):
			keys = append(keys, inner[:len(inner)-3])
		case strings.Contains(inner, ":"):
			name, _, _ := strings.Cut(inner, ":")
			keys = append(keys, name)
		default:
			keys = append(keys, inner)
		}

		offset = end + 1
	}
	return keys
}

// discardBodyWriter wraps an http.ResponseWriter and discards Write calls.
// Used for HEAD auto-handling.
type discardBodyWriter struct {
	http.ResponseWriter
}

func (w *discardBodyWriter) Write(b []byte) (int, error) {
	return len(b), nil
}

// Unwrap returns the underlying ResponseWriter so [http.ResponseController]
// can reach optional interfaces (Flusher, Hijacker) through the wrapper.
func (w *discardBodyWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// Mount attaches another http.Handler as a sub-router under the given pattern.
// The sub-router receives the remainder of the URL path.
//
// Method scope: the mounted handler is registered for all standard HTTP
// methods except CONNECT and TRACE, which are excluded deliberately
// (CONNECT is a proxy mechanism; TRACE enables cross-site tracing).
// Requests using them receive 405 Method Not Allowed.
//
// Middleware scope: mounted handlers receive only built-in and global middleware.
// Group-level and route-level middleware do not apply because mounted handlers
// are plain [http.Handler] instances dispatched outside the per-route compiled
// chain. If the mounted sub-application requires authentication or other
// protections, it must enforce them internally or the protections must be
// registered as global middleware.
//
// The parent's RouteContext (which may contain internal params like _mount) is
// stripped before calling the child handler, so the child dispatch creates its
// own fresh RouteContext. This prevents internal routing state from leaking
// across mount boundaries.
//
// Must be called before the server starts; panics if called after compile.
func (app *App) Mount(pattern string, handler http.Handler) {
	app.checkFrozen("Mount")
	if handler == nil {
		panic(fmt.Sprintf("credo: nil handler for Mount %s", pattern))
	}
	// Compute the cleaned prefix once and reuse it for both registrations and
	// introspection, so dispatch and Routes() can never disagree. The exact
	// match is registered on the cleaned prefix itself ("/admin", or "/" for a
	// root mount); the catch-all carries the "/{_mount...}" suffix. A root mount
	// is special-cased so the catch-all stays "/{_mount...}" instead of the
	// invalid "//{_mount...}".
	exact := cleanMountPrefix(pattern)
	catchAll := exact + "/{_mount...}"
	if exact == "/" {
		catchAll = "/{_mount...}"
	}

	mountHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rctx := getRouteContext(r)
		remaining := "/"
		if rctx != nil {
			remaining = "/" + rctx.URLParam("_mount")
		}
		handler.ServeHTTP(w, mountChildRequest(r, remaining))
	})

	app.mountRoutes(catchAll, mountHandler)

	// Also handle exact pattern match (without trailing path)
	exactHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, mountChildRequest(r, "/"))
	})

	app.mountRoutes(exact, exactHandler)

	// Record the mount for introspection only after both registrations
	// succeed, so a duplicate or conflicting Mount panic leaves no stale entry.
	// The prefix is the same cleaned value the exact match was registered on.
	app.mounts = append(app.mounts, mountInfo{prefix: exact})
}

// mountChildRequest creates a child request for a mounted sub-handler.
// It rewrites the URL path and strips the parent's RouteContext so the
// child dispatch creates a fresh one — preventing internal params (e.g.
// _mount) from leaking to child handlers.
func mountChildRequest(r *http.Request, newPath string) *http.Request {
	r2 := rewriteRequest(r, newPath)
	return r2.WithContext(context.WithValue(r2.Context(), routeCtxKey, (*RouteContext)(nil)))
}

// rewriteRequest returns a shallow copy of r with URL.Path set to newPath
// and URL.RawPath cleared. Used by Mount to adjust the path for sub-routers.
func rewriteRequest(r *http.Request, newPath string) *http.Request {
	r2 := new(http.Request)
	*r2 = *r
	r2.URL = new(url.URL)
	*r2.URL = *r.URL
	r2.URL.Path = newPath
	r2.URL.RawPath = ""
	return r2
}

// mountInfo records a mounted prefix for route introspection. [App.Mount] adds
// an entry only after the radix registration succeeds, so a registration panic
// (duplicate or conflicting mount) leaves no stale introspection entry. There
// is no host field: Group.Mount does not exist, so mounts are always
// default-scope.
type mountInfo struct {
	prefix string
}

// mountForwardedMethods returns the sorted set of HTTP methods a mount answers:
// every standard method except CONNECT and TRACE. CONNECT is reserved for proxy
// tunnels and TRACE echoes requests back (cross-site tracing exposure) —
// neither should reach a mounted sub-handler implicitly. A fresh slice is
// returned on each call, and it is the single source of truth shared by
// mountRoutes (registration) and [App.Routes] (introspection), so the two can
// never drift.
func mountForwardedMethods() []string {
	return []string{
		http.MethodDelete,
		http.MethodGet,
		http.MethodHead,
		http.MethodOptions,
		http.MethodPatch,
		http.MethodPost,
		http.MethodPut,
	}
}

// cleanMountPrefix normalizes a mount pattern to its user-facing prefix for
// introspection: a trailing slash is trimmed, but the root mount "/" is
// preserved rather than collapsing to "". So "/admin" and "/admin/" both yield
// "/admin", and "/" yields "/".
func cleanMountPrefix(pattern string) string {
	s := strings.TrimSuffix(pattern, "/")
	if s == "" {
		return "/"
	}
	return s
}

// mountRoutes registers a handler on the given pattern for every method in
// mountForwardedMethods.
func (app *App) mountRoutes(pattern string, handler http.Handler) {
	rh := &routeHandler{mounted: handler}
	for _, method := range mountForwardedMethods() {
		app.mux.insert(method, pattern, rh)
	}
}
