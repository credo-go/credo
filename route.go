// Inspired by github.com/go-goyave/goyave (MIT License).

package credo

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// Route is the handle a registration returns: it names the route, sets its
// meta and adds its middleware, all before the App starts. It is the write
// side only. What a route is — its method, pattern, host, name and resolved
// meta — is read from its [RouteInfo], which [Context.Route],
// [Exchange.Route], [App.GetRoute] and [App.Routes] return.
type Route struct {
	// method is the HTTP method (e.g., "GET").
	method string

	// pattern is the registered URL pattern.
	pattern string

	// name is the optional route name for URL generation.
	name string

	// handler is the Credo handler function.
	handler Handler

	// middlewares are per-route middlewares, precompiled into the route's
	// chain when the app compiles.
	middlewares []Middleware

	// meta holds this route's own metadata, by key identity.
	meta map[*keyID]any

	// info is the route's read view, with its resolved metadata (route ←
	// groups ← App), built once when its chain is compiled. Every request
	// the route matches returns it by value, so reading the route allocates
	// nothing. A request reads it without synchronization: dispatch reaches a
	// route only through the published preparation, which is stored after
	// every route was sealed.
	info RouteInfo

	// sealed reports that info was built. It is what a reader outside a
	// request — App.GetRoute, App.Routes — loads before it touches info, so
	// that such a call is safe while another goroutine prepares the App.
	sealed atomic.Bool

	// srcLoc is the user call site that registered the route, for
	// RouteInfo.RegisteredAt.
	srcLoc sourceLocation

	// parent is the owning group: meta resolution and group-middleware
	// collection, both at compile time.
	parent *Group

	// registrar is the owning App's registration seam (freeze guard and
	// named-route index). Nil for routes built outside an App.
	registrar routeRegistrar

	// hostPattern is the normalized host pattern for host-scoped routes.
	// Empty for routes registered on the default mux.
	hostPattern string

	// mount marks the route of an [App.Mount]: its pattern is the mount's
	// prefix, it has no method, no name and no middleware of its own, and its
	// parent is the App's root group, so it reads the App's meta and runs
	// in the routed tier.
	mount bool

	// headTwin is the auto-generated HEAD route paired with this GET route.
	// Nil for non-GET routes and when an explicit HEAD route already exists
	// for the same pattern. When non-nil, Middleware and SetMeta propagate
	// to the twin so HEAD and GET share identical behavior — preventing
	// silent bypass of auth, rate-limiting, and meta-driven middleware.
	headTwin *Route

	// foreignRequestReported records that the debug report of a replaced
	// embedded request was made for this route (see noteForeignRequest).
	foreignRequestReported atomic.Bool

	// autoHead marks this route as an auto-generated HEAD twin of a GET route.
	// Surfaced via RouteInfo.AutoHead so introspection can distinguish it from
	// an explicitly registered HEAD route (which keeps autoHead false).
	autoHead bool

	// tmplOnce lazily parses pattern and hostPattern into the generation
	// templates used by buildURI/buildURL; parsing compiles the regex
	// constraints once per route instead of once per call.
	tmplOnce sync.Once
	tmpl     *pathTemplate
	hostTmpl *hostTemplate
	tmplErr  error
}

// Name sets the route name for URL generation.
// Route names must be unique within the App. Duplicate names panic.
// Must be called before the server starts; panics if called after compile.
// Returns the Route for fluent chaining.
func (r *Route) Name(name string) *Route {
	if r.registrar != nil {
		r.registrar.checkFrozen("Route.Name")
		if r.name == name {
			return r
		}

		// Register the new name first so a conflict panic leaves the existing
		// name mapping intact. Skip registration for empty names — Name("")
		// is treated as "clear the name" rather than "register empty key".
		if name != "" {
			r.registrar.registerName(name, r)
		}

		if r.name != "" {
			r.registrar.deregisterName(r.name)
		}
	}
	r.name = name
	return r
}

// SetMeta sets the route's value of a meta key, which overrides the value its
// groups and the App set. The value's type is the key's, checked at compile
// time:
//
//	var RequiredScopes = credo.NewKey[[]Scope]("auth.scope")
//
//	app.GET("/reports", h).SetMeta(RequiredScopes, []Scope{"reports:read"})
//
// Must be called before the server starts; panics if called after compile,
// and for a zero Key. Returns the Route for fluent chaining.
//
// For GET routes with an auto-generated HEAD twin, the value is also set
// on the twin so middleware reading meta sees identical values regardless
// of which method was used.
func (r *Route) SetMeta[T any](key Key[T], val T) *Route {
	id := key.mustID("Route.SetMeta")
	if r.registrar != nil {
		r.registrar.checkFrozen("Route.SetMeta")
	}
	r.setMeta(id, val)
	if r.headTwin != nil {
		r.headTwin.setMeta(id, val)
	}
	return r
}

// RemoveMeta removes the route's own value of a meta key, so that the value
// its groups or the App set shows through. Must be called before the server
// starts; panics if called after compile, and for a zero Key. Returns the
// Route for fluent chaining. A HEAD twin follows its GET route.
func (r *Route) RemoveMeta[T any](key Key[T]) *Route {
	id := key.mustID("Route.RemoveMeta")
	if r.registrar != nil {
		r.registrar.checkFrozen("Route.RemoveMeta")
	}
	delete(r.meta, id)
	if r.headTwin != nil {
		delete(r.headTwin.meta, id)
	}
	return r
}

func (r *Route) setMeta(id *keyID, val any) {
	if r.meta == nil {
		r.meta = make(map[*keyID]any)
	}
	r.meta[id] = val
}

// Middleware appends one or more per-route middlewares.
// Must be called before the server starts; panics if called after compile.
// Returns the Route for fluent chaining.
//
// For GET routes with an auto-generated HEAD twin, middleware is also
// appended to the twin so that HEAD requests run the same chain as GET —
// otherwise auth, rate limiting, and other middleware could be silently
// bypassed via HEAD.
func (r *Route) Middleware(m ...Middleware) *Route {
	if r.registrar != nil {
		r.registrar.checkFrozen("Route.Middleware")
	}
	r.middlewares = append(r.middlewares, m...)
	if r.headTwin != nil {
		r.headTwin.middlewares = append(r.headTwin.middlewares, m...)
	}
	return r
}

// GetMeta returns the value of a meta key for this route and reports whether
// one was set: the route's own value, else its group's, then the outer
// groups', then the App's. The value has the key's type:
//
//	reports := app.GET("/reports", h).SetMeta(RequiredScopes, []Scope{"reports:read"})
//	scopes, ok := reports.GetMeta(RequiredScopes) // scopes is a []Scope
//
// It reads what registration has set so far. A request reads the same value
// from the route's view, c.Route().GetMeta(key) ([RouteInfo.GetMeta]).
// GetMeta panics for a zero Key.
func (r *Route) GetMeta[T any](key Key[T]) (T, bool) {
	v, ok := r.lookupMeta(key.mustID("Route.GetMeta"))
	t, _ := v.(T)
	return t, ok
}

// lookupMeta walks the route, then its group parent chain.
func (r *Route) lookupMeta(id *keyID) (any, bool) {
	if val, ok := r.meta[id]; ok {
		return val, true
	}
	for g := r.parent; g != nil; g = g.parent {
		if val, ok := g.meta[id]; ok {
			return val, true
		}
	}
	return nil, false
}

// resolveAllMeta returns the route's fully resolved metadata, merging the
// group parent chain (route → group → ... → root) the same way lookupMeta
// resolves a single key: nearer scopes override farther ones, and the route's
// own meta wins over every group. The returned map is freshly allocated and
// owned by the caller.
//
// It returns nil when neither the route nor any ancestor group defines
// metadata; an empty (but non-nil) map is never returned.
//
// For any key, the presence and value of resolveAllMeta()[key] match
// lookupMeta(key): both walk the identical chain.
func (r *Route) resolveAllMeta() map[*keyID]any {
	// Collect the group parent chain, nearest first.
	var groups []*Group
	for g := r.parent; g != nil; g = g.parent {
		groups = append(groups, g)
	}

	var result map[*keyID]any

	// Merge farthest (root) → nearest group so nearer scopes override farther
	// ones, then apply the route's own meta last so it wins over every group.
	for i := len(groups) - 1; i >= 0; i-- {
		if len(groups[i].meta) == 0 {
			continue
		}
		if result == nil {
			result = make(map[*keyID]any)
		}
		maps.Copy(result, groups[i].meta)
	}
	if len(r.meta) > 0 {
		if result == nil {
			result = make(map[*keyID]any)
		}
		maps.Copy(result, r.meta)
	}

	return result
}

// view returns the read view of a route a request matched: the one built
// when its chain was compiled, copied, with no allocation and no
// synchronization (see the info field). Its Methods slice is the cached one.
func (r *Route) view() RouteInfo {
	return r.info
}

// snapshot returns the route's read view to a caller outside a request, at
// any time relative to preparation: the sealed view once there is one, and
// before that a view built from the route's live fields, which registration
// can still change. Neither branch reads a field that compile writes without
// the sealed load ordering the two, so the call is safe while another
// goroutine prepares the App. The caller owns the Methods slice.
func (r *Route) snapshot() RouteInfo {
	if !r.sealed.Load() {
		return r.buildView()
	}
	ri := r.info
	ri.Methods = slices.Clone(ri.Methods)
	return ri
}

// buildView builds the route's read view from its fields, resolving its
// meta through its groups to the App.
func (r *Route) buildView() RouteInfo {
	ri := RouteInfo{
		Pattern:      r.pattern,
		Host:         r.hostPattern,
		Kind:         RouteKindRoute,
		RegisteredAt: r.srcLoc.String(),
		route:        r,
	}
	if r.mount {
		ri.Kind = RouteKindMount
		ri.Methods = mountForwardedMethods()
	} else {
		ri.Method = r.method
		ri.Name = r.name
		ri.AutoHead = r.autoHead
	}
	ri.meta = r.resolveAllMeta()
	return ri
}

// seal builds the route's read view, with its meta snapshot. It runs when
// the route's chain is compiled, after registration was frozen.
func (r *Route) seal() {
	r.info = r.buildView()
	r.sealed.Store(true)
}

// buildURI is [RouteInfo.BuildURI], and [RouteInfo.BuildURL] of a route
// without a host; method is the one the caller called, which every error
// names.
//
// The result stands alone as a reference, so it must not begin with "//": a
// client reads such a reference as a host and a path (RFC 3986, section 4.2),
// which is the form BuildURL returns for a host-scoped route. A catch-all at
// the root given a value that begins with "/", and a pattern that itself
// begins with "//", would build one; both are an error.
func (r *Route) buildURI(method string, params ...string) (string, error) {
	tmpl, _, err := r.templates()
	if err != nil {
		return "", fmt.Errorf("credo: %s %q: %w", method, r.pattern, err)
	}
	uri, consumed, err := tmpl.build(params)
	if err != nil {
		return "", fmt.Errorf("credo: %s %q: %w", method, r.pattern, err)
	}
	if consumed < len(params) {
		return "", fmt.Errorf("credo: %s %q: %d extra parameter(s)", method, r.pattern, len(params)-consumed)
	}
	if strings.HasPrefix(uri, "//") {
		return "", fmt.Errorf("credo: %s %q: the path built, %q, begins with \"//\", and a reference that begins with \"//\" "+
			"is read as a host, not as a path (RFC 3986, section 4.2); pass a value that does not begin with \"/\" "+
			"for a catch-all at the root — a path that begins with \"//\" cannot be written as a reference without a scheme and a host",
			method, r.pattern, uri)
	}
	return uri, nil
}

// buildURL is [RouteInfo.BuildURL].
func (r *Route) buildURL(params ...string) (string, error) {
	if r.hostPattern == "" {
		return r.buildURI("BuildURL", params...)
	}
	if hostPatternHasWildcard(r.hostPattern) {
		return "", fmt.Errorf("credo: BuildURL host %q: wildcard host patterns cannot generate concrete URLs", r.hostPattern)
	}
	tmpl, hostTmpl, err := r.templates()
	if err != nil {
		return "", fmt.Errorf("credo: BuildURL %q%s: %w", r.hostPattern, r.pattern, err)
	}
	host, consumed, err := hostTmpl.build(params)
	if err != nil {
		return "", fmt.Errorf("credo: BuildURL host %q: %w", r.hostPattern, err)
	}
	uri, pathConsumed, err := tmpl.build(params[consumed:])
	if err != nil {
		return "", fmt.Errorf("credo: BuildURL path %q: %w", r.pattern, err)
	}
	consumed += pathConsumed
	if consumed < len(params) {
		return "", fmt.Errorf("credo: BuildURL %q%s: %d extra parameter(s)", r.hostPattern, r.pattern, len(params)-consumed)
	}
	// The host is written, so a path that begins with "//" stays a path:
	// "//host" + "//x" is the host "host" and the path "//x".
	return "//" + host + uri, nil
}

// templates returns the route's cached generation templates, parsing the
// path and host patterns on first use.
func (r *Route) templates() (*pathTemplate, *hostTemplate, error) {
	r.tmplOnce.Do(func() {
		r.tmpl, r.tmplErr = parsePathTemplate(r.pattern)
		if r.tmplErr == nil && r.hostPattern != "" {
			r.hostTmpl = parseHostTemplate(r.hostPattern)
		}
	})
	return r.tmpl, r.hostTmpl, r.tmplErr
}
