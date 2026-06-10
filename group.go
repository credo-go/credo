// Inspired by github.com/go-goyave/goyave (MIT License).

package credo

// Group is the user-facing route group for configuring routes with a
// shared prefix, middleware, and metadata. It is also the node of the
// group parent chain: route metadata (LookupMeta) and group middleware
// are both resolved by walking this chain — metadata dynamically at
// request time, middleware once at compile time.
type Group struct {
	app         *App
	prefix      string
	parent      *Group // nil for the root group
	meta        map[string]any
	mws         []Middleware
	mux         *mux   // nil → app.mux, non-nil → host-specific mux
	hostPattern string // "" for default scope, non-empty for host-scoped groups
}

// lookupMeta searches the group's own meta, then traverses up the
// parent chain.
func (g *Group) lookupMeta(key string) (any, bool) {
	if g.meta != nil {
		if val, ok := g.meta[key]; ok {
			return val, true
		}
	}
	if g.parent != nil {
		return g.parent.lookupMeta(key)
	}
	return nil, false
}

// GET registers a GET route in this group. A matching HEAD handler is
// registered automatically; subsequent calls to [Route.Middleware] and
// [Route.SetMeta] on the returned route apply to HEAD as well, so HEAD
// requests can never silently bypass auth, rate limiting, or meta-driven
// middleware.
// Panics under the same conditions as [App.GET].
func (g *Group) GET(pattern string, h Handler) *Route {
	return g.app.addGetRoute(joinPath(g.prefix, pattern), h, g)
}

// POST registers a POST route in this group.
// Panics under the same conditions as [App.GET].
func (g *Group) POST(pattern string, h Handler) *Route {
	return g.app.addRoute("POST", joinPath(g.prefix, pattern), h, g)
}

// PUT registers a PUT route in this group.
// Panics under the same conditions as [App.GET].
func (g *Group) PUT(pattern string, h Handler) *Route {
	return g.app.addRoute("PUT", joinPath(g.prefix, pattern), h, g)
}

// DELETE registers a DELETE route in this group.
// Panics under the same conditions as [App.GET].
func (g *Group) DELETE(pattern string, h Handler) *Route {
	return g.app.addRoute("DELETE", joinPath(g.prefix, pattern), h, g)
}

// PATCH registers a PATCH route in this group.
// Panics under the same conditions as [App.GET].
func (g *Group) PATCH(pattern string, h Handler) *Route {
	return g.app.addRoute("PATCH", joinPath(g.prefix, pattern), h, g)
}

// HEAD registers a HEAD route in this group.
// Panics under the same conditions as [App.GET].
func (g *Group) HEAD(pattern string, h Handler) *Route {
	return g.app.addRoute("HEAD", joinPath(g.prefix, pattern), h, g)
}

// OPTIONS registers an OPTIONS route in this group.
// Panics under the same conditions as [App.GET].
func (g *Group) OPTIONS(pattern string, h Handler) *Route {
	return g.app.addRoute("OPTIONS", joinPath(g.prefix, pattern), h, g)
}

// Middleware appends group-level middlewares. They apply to every route
// of this group and its sub-groups, regardless of whether the route was
// registered before or after this call: per-route chains are assembled
// from the group parent chain when the app compiles (first request or
// Run), mirroring how route metadata is resolved via [Route.LookupMeta].
// Must be called before the server starts; panics if called after
// compile. Returns *Group for chaining.
func (g *Group) Middleware(middlewares ...Middleware) *Group {
	g.app.checkFrozen("Group.Middleware")
	g.mws = append(g.mws, middlewares...)
	return g
}

// SetMeta sets a metadata key-value pair for this group.
// All routes within the group inherit this metadata.
// Must be called before the server starts; panics if called after compile.
func (g *Group) SetMeta(key string, val any) {
	g.app.checkFrozen("Group.SetMeta")
	if g.meta == nil {
		g.meta = make(map[string]any)
	}
	g.meta[key] = val
}

// RemoveMeta removes a metadata key from this group.
// Must be called before the server starts; panics if called after compile.
func (g *Group) RemoveMeta(key string) {
	g.app.checkFrozen("Group.RemoveMeta")
	if g.meta != nil {
		delete(g.meta, key)
	}
}

// Group creates a nested sub-group with the given prefix. The sub-group
// inherits this group's metadata and middleware through the parent chain.
func (g *Group) Group(prefix string) *Group {
	return &Group{
		app:         g.app,
		prefix:      joinPath(g.prefix, prefix),
		parent:      g,
		mux:         g.mux,
		hostPattern: g.hostPattern,
	}
}
