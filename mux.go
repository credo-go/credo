// Copyright (c) 2015-present Peter Kieltyka (https://github.com/pkieltyka), Google Inc.
// Originally derived from github.com/go-chi/chi (MIT License).

package credo

import (
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/credo-go/credo/internal/radix"
)

// RouteKind distinguishes a normal registered route from a mounted handler
// prefix in introspection output.
type RouteKind string

const (
	// RouteKindRoute is a normal route registered via GET/POST/Static and friends.
	RouteKindRoute RouteKind = "route"

	// RouteKindMount is a handler subtree attached via [App.Mount]. Only its
	// prefix, forwarded method set and meta (the App's) are exposed in
	// introspection, never its internal routes.
	RouteKindMount RouteKind = "mount"
)

// RouteInfo is the read view of a registered route or mount: what it is, and
// the URLs it generates. There is one view of a route, and every reader gets
// it: [Context.Route] and [Exchange.Route] for the route a request matched,
// [App.GetRoute] for a named route, and [App.Routes] for all of them. Its
// counterpart is [*Route], the handle a registration returns, which
// configures the route and is not readable at request time.
//
// A normal route (Kind == RouteKindRoute) carries a single Method and a nil
// Methods. A mount (Kind == RouteKindMount) leaves Method empty and lists its
// sorted forwarded method set in Methods, because one mount answers every
// forwarded method at its prefix.
//
// The zero RouteInfo describes no route. It is what [Context.Route] and
// [Exchange.Route] return for a request no route matched: every field is
// empty, Kind included, GetMeta reports false, and BuildURI and BuildURL
// return an error.
//
// A RouteInfo is a value. The view of a prepared App is built once, when the
// route's chain is compiled, and copied to each reader. Its meta values are
// shared and read-only by convention — mutating a slice, map, or pointer read
// from GetMeta mutates the live route. The Methods slice of a view that
// [App.Routes] or [App.GetRoute] returned is the caller's; the one of a view
// that [Context.Route] or [Exchange.Route] returned is shared with every
// other request and must not be modified.
type RouteInfo struct {
	// Method is the HTTP method of a normal route; empty for a mount (see Methods).
	Method string

	// Methods is the sorted set of forwarded HTTP methods for a mount
	// (CONNECT and TRACE excluded); nil for a normal route. In a view read
	// from a request ([Context.Route], [Exchange.Route]) the slice is shared
	// and must not be modified.
	Methods []string

	// Pattern is the user-facing URL pattern; for a mount it is the cleaned
	// mount prefix.
	Pattern string

	// Host is the host pattern, or empty for the default mux.
	Host string

	// Name is the route name, or empty if unnamed.
	Name string

	// Kind reports whether this entry describes a normal route or a mount.
	// It is empty for the zero RouteInfo.
	Kind RouteKind

	// AutoHead reports whether this is an auto-generated HEAD twin of a GET route.
	AutoHead bool

	// RegisteredAt is the user call site ("file:line") that registered this
	// route or mount, or "" when the framework could not capture it. Intended
	// for startup route dumps and /debug/routes diagnostics.
	RegisteredAt string

	// meta is the resolved metadata GetMeta reads; nil if none.
	meta map[*keyID]any

	// route is the route the view describes, for URL generation; nil for the
	// zero RouteInfo.
	route *Route
}

// GetMeta returns the route's resolved value of a meta key — the route's own,
// else its groups', else the App's — and reports whether one was set; a mount
// reports the App's. The value has the key's type:
//
//	scopes, ok := c.Route().GetMeta(RequiredScopes) // scopes is a []Scope
//
// The zero RouteInfo reports false for every key, so a middleware that
// enforces route meta checks [Context.HasRoute] first. GetMeta panics for a
// zero Key.
func (ri RouteInfo) GetMeta[T any](key Key[T]) (T, bool) {
	v, ok := ri.meta[key.mustID("RouteInfo.GetMeta")]
	t, _ := v.(T)
	return t, ok
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
// parameter, fails its constraint, when too many values are provided, when
// the route pattern is malformed, or when the RouteInfo is the zero value. An
// empty catch-all value is valid: the catch-all matches an empty rest, so
// "/files/{path...}" with "" builds "/files/".
//
// The result is a path that stands alone as a reference, in a Location header
// or a link, so BuildURI also returns an error when the path it built begins
// with "//": a client reads such a reference as a host and a path, not as a
// path. Only a catch-all at the root given a value that begins with "/"
// ("/{path...}" with "/x") and a pattern that itself begins with "//" build
// one.
//
//	user, _ := app.GetRoute("user.show")
//	uri, err := user.BuildURI("42") // "/users/42"
func (ri RouteInfo) BuildURI(params ...string) (string, error) {
	if ri.route == nil {
		return "", errNoRouteToBuild("BuildURI")
	}
	return ri.route.buildURI("BuildURI", params...)
}

// BuildURL generates a reference to the route that carries its host: for a
// host-scoped route, the network-path reference "//host/path" of RFC 3986
// section 4.2, which is valid as it stands in a Location header or a link and
// takes the scheme of the page or request it is resolved against. A caller
// that needs an absolute URL prefixes the scheme it knows, "https:" + ref.
//
// Host pattern parameters are consumed first, then path parameters. A host
// value fills exactly one label: it must be non-empty, consist of letters,
// digits, hyphens and underscores, and satisfy the label's regex constraint;
// host labels are never percent-encoded. Path values follow
// [RouteInfo.BuildURI]. It returns an error when a value is missing or
// invalid, when too many values are provided, when either pattern is
// malformed, or when the RouteInfo is the zero value. Wildcard host patterns
// cannot generate concrete URLs and return an error.
//
// For host-scoped routes:
//
//	// Host: {tenant}.myapp.com, Path: /users/{id}
//	ref, err := user.BuildURL("acme", "42") // "//acme.myapp.com/users/42"
//
// A route without a host has no host to carry, and BuildURL returns what
// [RouteInfo.BuildURI] returns, with the same error for a path that begins
// with "//"; every error names BuildURL:
//
//	ref, err := user.BuildURL("42") // "/users/42"
func (ri RouteInfo) BuildURL(params ...string) (string, error) {
	if ri.route == nil {
		return "", errNoRouteToBuild("BuildURL")
	}
	return ri.route.buildURL(params...)
}

// errNoRouteToBuild is the error of URL generation on the zero RouteInfo.
func errNoRouteToBuild(method string) error {
	return fmt.Errorf("credo: %s: the RouteInfo describes no route, so there is no pattern to build from; "+
		"use one that App.GetRoute or App.Routes returned, or Context.Route of a request for which HasRoute is true", method)
}

// routeEntry is a stored route registration. RouteInfo is derived from it
// lazily at introspection time, so Name and Meta — configured on the live
// *Route after registration via the fluent API — are read at their final
// values rather than snapshotted empty at registration.
type routeEntry struct {
	method  string
	pattern string
	rh      *routeHandler
}

// routeStore is a shared registry for route introspection. It records one
// entry per registered method+pattern+host and derives RouteInfo on demand.
type routeStore struct {
	mu      sync.RWMutex
	entries []routeEntry
}

// add records a route registration, replacing any existing entry with the same
// method+pattern+host. The upsert is what lets an explicit HEAD registration
// override the auto-generated HEAD twin in introspection: the later
// registration's routeHandler wins.
func (rs *routeStore) add(method, pattern string, rh *routeHandler) {
	host := routeInfoHost(rh)
	rs.mu.Lock()
	for i := range rs.entries {
		e := &rs.entries[i]
		if e.method == method && e.pattern == pattern && routeInfoHost(e.rh) == host {
			e.rh = rh
			rs.mu.Unlock()
			return
		}
	}
	rs.entries = append(rs.entries, routeEntry{method: method, pattern: pattern, rh: rh})
	rs.mu.Unlock()
}

// snapshot returns a copy of the entry slice under the read lock. RouteInfo is
// derived outside the lock (see [mux.Routes]); the lock covers only the entry
// slice, not the live *Route fields the entries point to.
func (rs *routeStore) snapshot() []routeEntry {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return slices.Clone(rs.entries)
}

// mux is a radix-tree backed route storage. It handles insertion and
// introspection only — dispatching is done by App.dispatch.
type mux struct {
	// tree is the radix tree root node; payloads are *routeHandler.
	tree *radix.Node[*routeHandler]

	// pool reuses RouteContext instances.
	pool *pool[*radix.RouteContext]

	// store is the shared route registry for introspection.
	store *routeStore

	// handlers lists every inserted payload so compileRoutes can build
	// per-route chains without going through the introspection store.
	handlers []*routeHandler
}

// newMux creates a new mux with an empty radix tree.
func newMux() *mux {
	mx := &mux{
		tree:  radix.NewTree[*routeHandler](),
		store: &routeStore{},
	}
	mx.pool = newPool(func() *radix.RouteContext {
		rctx := &radix.RouteContext{}
		// Pre-allocate param slices to avoid repeated small allocations
		// during route matching. Most routes have 1-4 parameters.
		rctx.Params.Keys = make([]string, 0, 4)
		rctx.Params.Values = make([]string, 0, 4)
		return rctx
	})
	return mx
}

// insert registers a route payload for the given method and pattern in the
// radix tree. If autoGenerated is true, the endpoint is marked as
// auto-generated and can be overwritten by explicit registrations.
func (mx *mux) insert(method, pattern string, rh *routeHandler, autoGenerated ...bool) {
	mtyp, ok := radix.LookupMethod(method)
	if !ok {
		panic(fmt.Sprintf("credo: unknown method %q", method))
	}

	_, err := mx.tree.InsertRoute(mtyp, pattern, rh, autoGenerated...)
	if err != nil {
		if dup, ok := errors.AsType[*radix.DuplicateRouteError[*routeHandler]](err); ok {
			panic(duplicateRoutePanic(method, pattern, dup.ExistingPattern, rh, dup.Existing))
		}
		panic(fmt.Sprintf("credo: %v", err))
	}

	mx.handlers = append(mx.handlers, rh)
	mx.store.add(method, pattern, rh)
}

// duplicateRoutePanic formats the panic for a duplicate route registration,
// naming both the original and the conflicting call site when the framework
// captured them ("first registered at … → now registered at …"). incoming is
// the payload being inserted now; existing is the conflicting prior payload
// and existingPattern the pattern it was registered as — spelled out when it
// differs from pattern, since routes are identified by their name-stripped
// shape and "/users/{id}" and "/users/{name}" are the same route.
func duplicateRoutePanic(method, pattern, existingPattern string, incoming, existing *routeHandler) string {
	msg := fmt.Sprintf("credo: duplicate route: %s %q is already registered", method, pattern)
	if existingPattern != "" && existingPattern != pattern {
		msg += fmt.Sprintf(" as %q (parameter names do not distinguish routes)", existingPattern)
	}
	if existing != nil {
		if loc := existing.srcLoc.String(); loc != "" {
			msg += "\n  first registered at " + loc
		}
	}
	if incoming != nil {
		if loc := incoming.srcLoc.String(); loc != "" {
			msg += "\n  now registered at " + loc
		}
	}
	return msg
}

// tryInsert is like insert but returns false instead of panicking on duplicate
// route errors. Used by addHeadRoute to silently skip if an explicit HEAD
// route already exists.
func (mx *mux) tryInsert(method, pattern string, rh *routeHandler, autoGenerated bool) bool {
	mtyp, ok := radix.LookupMethod(method)
	if !ok {
		return false
	}

	_, err := mx.tree.InsertRoute(mtyp, pattern, rh, autoGenerated)
	if err != nil {
		return false
	}

	mx.handlers = append(mx.handlers, rh)
	mx.store.add(method, pattern, rh)
	return true
}

// wouldConflict reports whether inserting pattern for method would be rejected
// as a duplicate route — that is, an explicit (non-auto-generated) endpoint is
// already registered at that pattern's shape — and returns the conflicting
// payload and its registered pattern so the caller can build a descriptive
// panic. It is the read-only
// counterpart of the duplicate check in insert, used by [App.Mount] to
// preflight every registration before mutating the tree: the radix tree has no
// delete, so a duplicate panic partway through Mount's registrations would
// otherwise strand the earlier ones as orphan routes.
func (mx *mux) wouldConflict(method, pattern string) (existing *routeHandler, existingPattern string, ok bool) {
	mtyp, ok := radix.LookupMethod(method)
	if !ok {
		panic(fmt.Sprintf("credo: unknown method %q", method))
	}

	ep, ok := mx.tree.FindEndpoint(mtyp, pattern)
	if !ok || ep.AutoGenerated {
		return nil, "", false
	}
	return ep.Value, ep.Pattern, true
}

func routeInfoHost(rh *routeHandler) string {
	if rh.route != nil {
		return rh.route.hostPattern
	}
	return ""
}

// Routes returns the view of every normal route in this mux, in registration
// order. Mount payloads are skipped here — [App.Routes] surfaces one RouteInfo
// per mount from a separate registry. The returned slice is freshly
// allocated; the caller owns it.
//
// Routes is safe to call while another goroutine prepares the App. Before
// registration is frozen it reads live *Route fields (Name, metadata) without
// locking them, so it must not run concurrently with route registration or
// configuration (Name, SetMeta, Mount).
func (mx *mux) Routes() []RouteInfo {
	entries := mx.store.snapshot()
	out := make([]RouteInfo, 0, len(entries))
	for _, e := range entries {
		route := e.rh.route
		if route.mount {
			// Mounted handler — surfaced separately by App.Routes.
			continue
		}
		out = append(out, route.snapshot())
	}
	return out
}

// joinPath joins a prefix and pattern, ensuring exactly one slash between them.
func joinPath(prefix, pattern string) string {
	if prefix == "" {
		return pattern
	}
	if pattern == "" {
		return prefix
	}
	if prefix[len(prefix)-1] == '/' && pattern[0] == '/' {
		return prefix + pattern[1:]
	}
	if prefix[len(prefix)-1] != '/' && pattern[0] != '/' {
		return prefix + "/" + pattern
	}
	return prefix + pattern
}
