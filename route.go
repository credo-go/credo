// Inspired by github.com/go-goyave/goyave (MIT License).

package credo

import (
	"fmt"
	"maps"
	"sync"
	"sync/atomic"
)

// Route represents a registered route with its handler, metadata,
// and fluent configuration methods.
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

	// metaSnap is the route's resolved metadata (route ← groups ← App),
	// taken when its chain is compiled; metaResolved reports that it was, so
	// a request-time GetMeta reads the snapshot instead of walking the chain.
	metaSnap     map[*keyID]any
	metaResolved bool

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
	// templates used by BuildURI/BuildURL; parsing compiles the regex
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

// GetName returns the route name.
func (r *Route) GetName() string {
	return r.name
}

// GetMethod returns the HTTP method. It is empty for a mount's route, which
// answers every forwarded method.
func (r *Route) GetMethod() string {
	return r.method
}

// GetPattern returns the URL pattern; for a mount's route, the mount's
// prefix.
func (r *Route) GetPattern() string {
	return r.pattern
}

// GetHost returns the host pattern for host-scoped routes.
// Returns an empty string for routes on the default mux.
func (r *Route) GetHost() string {
	return r.hostPattern
}

// GetMeta returns the value of a meta key for this route and reports whether
// one was set: the route's own value, else its group's, then the outer
// groups', then the App's. The value has the key's type:
//
//	scopes, ok := c.Route().GetMeta(RequiredScopes) // scopes is a []Scope
//
// Once the App is prepared, the route's resolved meta is a snapshot taken
// when its chain was compiled, so a request-time lookup does not walk the
// groups. GetMeta panics for a zero Key.
func (r *Route) GetMeta[T any](key Key[T]) (T, bool) {
	id := key.mustID("Route.GetMeta")
	var (
		v  any
		ok bool
	)
	if r.metaResolved {
		v, ok = r.metaSnap[id]
	} else {
		v, ok = r.lookupMeta(id)
	}
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

// resolveMeta takes the route's meta snapshot. It runs when the route's
// chain is compiled, after registration was frozen.
func (r *Route) resolveMeta() {
	r.metaSnap = r.resolveAllMeta()
	r.metaResolved = true
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

// BuildURI generates a URI from the route pattern by replacing named
// parameters with the provided values in order. Values are decoded parameter
// values, exactly what [Request.RouteParam] reports for the generated URI:
// each is checked against its regex constraint and percent-encoded for one
// path segment with [url.PathEscape], so "a/b" for a single-segment parameter
// yields "a%2Fb" while a catch-all value keeps its slashes as separators and
// escapes each segment. Static text of the pattern is written verbatim.
//
// It returns an error when a value is missing, empty for a single-segment
// parameter, fails its constraint, when too many values are provided, or when
// the route pattern is malformed. An empty catch-all value is valid: the
// catch-all matches an empty rest, so "/files/{path...}" with "" builds
// "/files/".
//
//	uri, err := route.BuildURI("42") // "/users/42"
func (r *Route) BuildURI(params ...string) (string, error) {
	tmpl, _, err := r.templates()
	if err != nil {
		return "", fmt.Errorf("credo: BuildURI %q: %w", r.pattern, err)
	}
	uri, consumed, err := tmpl.build(params)
	if err != nil {
		return "", fmt.Errorf("credo: BuildURI %q: %w", r.pattern, err)
	}
	if consumed < len(params) {
		return "", fmt.Errorf("credo: BuildURI %q: %d extra parameter(s)", r.pattern, len(params)-consumed)
	}
	return uri, nil
}

// BuildURL generates a full URL by combining the route's host pattern and
// path pattern, replacing parameters with the provided values in order.
// Host pattern parameters are consumed first, then path parameters. A host
// value fills exactly one label: it must be non-empty, consist of letters,
// digits, hyphens and underscores, and satisfy the label's regex constraint;
// host labels are never percent-encoded. Path values follow [Route.BuildURI].
// It returns an error when a value is missing or invalid, when too many values
// are provided, or when either pattern is malformed. Wildcard host patterns
// cannot generate concrete URLs and return an error.
//
// For host-scoped routes:
//
//	// Host: {tenant}.myapp.com, Path: /users/{id}
//	url, err := route.BuildURL("acme", "42") // "acme.myapp.com/users/42"
//
// For default (non-host-scoped) routes, BuildURL is equivalent to [Route.BuildURI]:
//
//	url, err := route.BuildURL("42") // "/users/42"
func (r *Route) BuildURL(params ...string) (string, error) {
	if r.hostPattern == "" {
		return r.BuildURI(params...)
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
	return host + uri, nil
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
