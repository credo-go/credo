// Inspired by github.com/go-goyave/goyave (MIT License).

package credo

import (
	"fmt"
	"maps"
	"strings"

	"github.com/credo-go/credo/internal/radix"
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

	// middlewares are per-route middlewares applied lazily at request time.
	middlewares []Middleware

	// meta holds key-value metadata for this route.
	meta map[string]any

	// parent is the owning group: meta chain traversal at request time,
	// group-middleware collection at compile time.
	parent *Group

	// app is the owning App (for name registration).
	app *App

	// hostPattern is the normalized host pattern for host-scoped routes.
	// Empty for routes registered on the default mux.
	hostPattern string

	// headTwin is the auto-generated HEAD route paired with this GET route.
	// Nil for non-GET routes and when an explicit HEAD route already exists
	// for the same pattern. When non-nil, Middleware and SetMeta propagate
	// to the twin so HEAD and GET share identical behavior — preventing
	// silent bypass of auth, rate-limiting, and meta-driven middleware.
	headTwin *Route

	// autoHead marks this route as an auto-generated HEAD twin of a GET route.
	// Surfaced via RouteInfo.AutoHead so introspection can distinguish it from
	// an explicitly registered HEAD route (which keeps autoHead false).
	autoHead bool
}

// Name sets the route name for URL generation.
// Route names must be unique within the App. Duplicate names panic.
// Must be called before the server starts; panics if called after compile.
// Returns the Route for fluent chaining.
func (r *Route) Name(name string) *Route {
	if r.app != nil {
		r.app.checkFrozen("Route.Name")
		if r.name == name {
			return r
		}

		// Register the new name first so a conflict panic leaves the existing
		// name mapping intact. Skip registration for empty names — Name("")
		// is treated as "clear the name" rather than "register empty key".
		if name != "" {
			r.app.registerName(name, r)
		}

		if r.name != "" {
			r.app.deregisterName(r.name)
		}
	}
	r.name = name
	return r
}

// SetMeta attaches a key-value metadata pair to this route.
// Must be called before the server starts; panics if called after compile.
// Returns the Route for fluent chaining.
//
// For GET routes with an auto-generated HEAD twin, the value is also set
// on the twin so middleware reading meta sees identical values regardless
// of which method was used.
func (r *Route) SetMeta(key string, val any) *Route {
	if r.app != nil {
		r.app.checkFrozen("Route.SetMeta")
	}
	if r.meta == nil {
		r.meta = make(map[string]any)
	}
	r.meta[key] = val
	if r.headTwin != nil {
		if r.headTwin.meta == nil {
			r.headTwin.meta = make(map[string]any)
		}
		r.headTwin.meta[key] = val
	}
	return r
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
	if r.app != nil {
		r.app.checkFrozen("Route.Middleware")
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

// GetMethod returns the HTTP method.
func (r *Route) GetMethod() string {
	return r.method
}

// GetPattern returns the URL pattern.
func (r *Route) GetPattern() string {
	return r.pattern
}

// GetHost returns the host pattern for host-scoped routes.
// Returns an empty string for routes on the default mux.
func (r *Route) GetHost() string {
	return r.hostPattern
}

// LookupMeta searches for a metadata value by key, traversing the parent
// chain (route → group → parent group → ...) until found.
func (r *Route) LookupMeta(key string) (any, bool) {
	if r.meta != nil {
		if val, ok := r.meta[key]; ok {
			return val, true
		}
	}
	if r.parent != nil {
		return r.parent.lookupMeta(key)
	}
	return nil, false
}

// resolveAllMeta returns the route's fully resolved metadata, merging the
// group parent chain (route → group → ... → root) the same way LookupMeta
// resolves a single key: nearer scopes override farther ones, and the route's
// own meta wins over every group. The returned map is freshly allocated and
// owned by the caller.
//
// It returns nil when neither the route nor any ancestor group defines
// metadata, mirroring the "nil if none" contract of [RouteInfo.Meta] — an
// empty (but non-nil) map is never returned.
//
// For any key, the presence and value of resolveAllMeta()[key] match
// LookupMeta(key): both walk the identical chain.
func (r *Route) resolveAllMeta() map[string]any {
	// Collect the group parent chain, nearest first.
	var groups []*Group
	for g := r.parent; g != nil; g = g.parent {
		groups = append(groups, g)
	}

	var result map[string]any

	// Merge farthest (root) → nearest group so nearer scopes override farther
	// ones, then apply the route's own meta last so it wins over every group.
	for i := len(groups) - 1; i >= 0; i-- {
		if len(groups[i].meta) == 0 {
			continue
		}
		if result == nil {
			result = make(map[string]any)
		}
		maps.Copy(result, groups[i].meta)
	}
	if len(r.meta) > 0 {
		if result == nil {
			result = make(map[string]any)
		}
		maps.Copy(result, r.meta)
	}

	return result
}

// BuildURI generates a URI from the route pattern by replacing named
// parameters with the provided values in order. It returns an error when a
// parameter is missing, when too many values are provided, or when the route
// pattern is malformed. Uses brace-depth-aware parsing to correctly handle
// regex quantifiers like {id:[0-9]{2,4}}.
//
//	uri, err := route.BuildURI("42") // "/users/42"
func (r *Route) BuildURI(params ...string) (string, error) {
	uri, consumed, err := replaceParams(r.pattern, params)
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
// Host pattern parameters are consumed first, then path parameters. It returns
// an error when a parameter is missing, when too many values are provided, or
// when either pattern is malformed. Wildcard host patterns cannot generate
// concrete URLs and return an error.
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
	host, consumed, err := replaceParams(r.hostPattern, params)
	if err != nil {
		return "", fmt.Errorf("credo: BuildURL host %q: %w", r.hostPattern, err)
	}
	uri, pathConsumed, err := replaceParams(r.pattern, params[consumed:])
	if err != nil {
		return "", fmt.Errorf("credo: BuildURL path %q: %w", r.pattern, err)
	}
	consumed += pathConsumed
	if consumed < len(params) {
		return "", fmt.Errorf("credo: BuildURL %q%s: %d extra parameter(s)", r.hostPattern, r.pattern, len(params)-consumed)
	}
	return host + uri, nil
}

// replaceParams replaces {placeholder} tokens in s with the provided values
// in order. Returns the resulting string and the number of params consumed.
// Uses brace-depth-aware parsing to correctly handle regex quantifiers
// like {name:[0-9]{2,4}}.
func replaceParams(s string, params []string) (string, int, error) {
	var b strings.Builder
	last := 0
	consumed := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}

		end := radix.FindMatchingBrace(s, i)
		if end < 0 {
			return "", consumed, fmt.Errorf("missing closing brace at byte %d", i)
		}
		if consumed >= len(params) {
			return "", consumed, fmt.Errorf("missing parameter %q", routeParamName(s[i+1:end]))
		}

		b.WriteString(s[last:i])
		b.WriteString(params[consumed])
		consumed++
		i = end
		last = end + 1
	}
	if consumed == 0 {
		return s, 0, nil
	}
	b.WriteString(s[last:])
	return b.String(), consumed, nil
}

func routeParamName(token string) string {
	if name, _, ok := strings.Cut(token, ":"); ok {
		return name
	}
	return strings.TrimSuffix(token, "...")
}
