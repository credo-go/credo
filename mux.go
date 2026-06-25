// Copyright (c) 2015-present Peter Kieltyka (https://github.com/pkieltyka), Google Inc.
// Originally derived from github.com/go-chi/chi (MIT License).

package credo

import (
	"fmt"
	"sync"

	"github.com/credo-go/credo/internal/radix"
)

// Routes describes the interface for route introspection.
type Routes interface {
	// Routes returns a slice of RouteInfo for all registered routes.
	Routes() []RouteInfo
}

// RouteKind distinguishes a normal registered route from a mounted handler
// prefix in introspection output.
type RouteKind string

const (
	// RouteKindRoute is a normal route registered via GET/POST/Static and friends.
	RouteKindRoute RouteKind = "route"

	// RouteKindMount is a handler subtree attached via [App.Mount]. Only its
	// prefix and forwarded method set are exposed in introspection, never its
	// internal routes.
	RouteKindMount RouteKind = "mount"
)

// RouteInfo holds introspection data about a registered route or mount.
//
// A normal route (Kind == RouteKindRoute) carries a single Method and a nil
// Methods. A mount (Kind == RouteKindMount) leaves Method empty and lists its
// sorted forwarded method set in Methods, because one mount answers every
// forwarded method at its prefix.
//
// Meta is the fully resolved route metadata (route ← group ← app) as a shallow
// copy: callers may read it and add or delete keys without affecting framework
// state, but the values are read-only by convention — mutating a slice, map, or
// pointer stored in Meta mutates the live route metadata. Meta is nil when
// neither the route nor any ancestor group defines metadata.
type RouteInfo struct {
	// Method is the HTTP method of a normal route; empty for a mount (see Methods).
	Method string

	// Methods is the sorted set of forwarded HTTP methods for a mount
	// (CONNECT and TRACE excluded); nil for a normal route.
	Methods []string

	// Pattern is the user-facing URL pattern; for a mount it is the cleaned
	// mount prefix.
	Pattern string

	// Host is the host pattern, or empty for the default mux.
	Host string

	// Name is the route name, or empty if unnamed.
	Name string

	// Meta is the resolved, shallow-copied route metadata; nil if none.
	Meta map[string]any

	// Kind reports whether this entry describes a normal route or a mount.
	Kind RouteKind

	// AutoHead reports whether this is an auto-generated HEAD twin of a GET route.
	AutoHead bool
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
	cp := make([]routeEntry, len(rs.entries))
	copy(cp, rs.entries)
	return cp
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
		panic(fmt.Sprintf("credo: %v", err))
	}

	mx.handlers = append(mx.handlers, rh)
	mx.store.add(method, pattern, rh)
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

func routeInfoHost(rh *routeHandler) string {
	if rh.route != nil {
		return rh.route.hostPattern
	}
	return ""
}

// Routes returns introspection data for every normal route in this mux, in
// registration order. Mount payloads are skipped here — [App.Routes] surfaces
// a single RouteInfo per mount from a separate registry. The returned slice and
// each RouteInfo.Meta map are freshly allocated; the caller owns them.
//
// Routes reads live *Route fields (Name, metadata) without locking them, so it
// must not run concurrently with route registration or configuration (Name,
// SetMeta, Mount). Introspection is a post-wiring (or post-freeze) operation.
func (mx *mux) Routes() []RouteInfo {
	entries := mx.store.snapshot()
	out := make([]RouteInfo, 0, len(entries))
	for _, e := range entries {
		route := e.rh.route
		if route == nil {
			// Mounted handler — surfaced separately by App.Routes.
			continue
		}
		out = append(out, RouteInfo{
			Method:  e.method,
			Pattern: e.pattern,
			Host:    route.hostPattern,
			Name:    route.name,
			Meta:    route.resolveAllMeta(),
			Kind:    RouteKindRoute,
		})
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
