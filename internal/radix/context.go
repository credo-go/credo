// Copyright (c) 2015-present Peter Kieltyka (https://github.com/pkieltyka), Google Inc.
// Originally derived from github.com/go-chi/chi (MIT License).

package radix

// RouteParams holds key-value pairs of URL parameters extracted
// from the matched route pattern.
type RouteParams struct {
	Keys   []string
	Values []string
}

// Add appends a key-value pair to the route parameters.
func (p *RouteParams) Add(key, value string) {
	p.Keys = append(p.Keys, key)
	p.Values = append(p.Values, value)
}

// RouteContext holds routing state for a single request.
// It is stored in the request context and populated by the router
// during route matching.
type RouteContext struct {
	// RoutePath is the path to match against. Updated during sub-router mounting.
	RoutePath string

	// RouteMethod is the HTTP method of the request.
	RouteMethod string

	// RoutePatterns collects path patterns matched along the route chain.
	RoutePatterns []string

	// Params holds the URL parameter key-value pairs.
	Params RouteParams

	// MethodNotAllowed indicates that the path matched but not the method.
	MethodNotAllowed bool

	// methodsAllowed tracks which methods ARE allowed (for 405 Allow header).
	methodsAllowed MethodTyp
}

// Reset clears the RouteContext for reuse.
func (rc *RouteContext) Reset() {
	rc.RoutePath = ""
	rc.RouteMethod = ""
	// Clear slice elements to release string references for GC,
	// then truncate to zero length while retaining capacity.
	clear(rc.RoutePatterns)
	rc.RoutePatterns = rc.RoutePatterns[:0]
	clear(rc.Params.Keys)
	rc.Params.Keys = rc.Params.Keys[:0]
	clear(rc.Params.Values)
	rc.Params.Values = rc.Params.Values[:0]
	rc.MethodNotAllowed = false
	rc.methodsAllowed = 0
}

// URLParam returns the value of a URL parameter by name.
// Returns empty string if the parameter doesn't exist.
// Iterates in reverse so that the last-added value wins (supports
// nested routes with shadowed parameters).
func (rc *RouteContext) URLParam(name string) string {
	for i := len(rc.Params.Keys) - 1; i >= 0; i-- {
		if rc.Params.Keys[i] == name {
			return rc.Params.Values[i]
		}
	}
	return ""
}

// MethodsAllowed returns the set of methods allowed for the matched path.
func (rc *RouteContext) MethodsAllowed() MethodTyp {
	return rc.methodsAllowed
}

// setMethodsAllowed sets the allowed methods for the matched path.
func (rc *RouteContext) setMethodsAllowed(m MethodTyp) {
	rc.methodsAllowed = m
}
