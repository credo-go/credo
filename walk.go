// Copyright (c) 2015-present Peter Kieltyka (https://github.com/pkieltyka), Google Inc.
// Originally derived from github.com/go-chi/chi (MIT License).

package credo

// WalkFunc is the callback for Walk. It receives the method and pattern
// of each registered route. Return a non-nil error to stop walking.
type WalkFunc func(method, pattern string) error

// Walk iterates over all registered routes, calling fn for each one.
func Walk(r Routes, fn WalkFunc) error {
	for _, ri := range r.Routes() {
		if err := fn(ri.Method, ri.Pattern); err != nil {
			return err
		}
	}
	return nil
}

// WalkRoutesFunc is the callback for WalkRoutes. It receives the full
// RouteInfo including the host pattern. Return a non-nil error to stop walking.
type WalkRoutesFunc func(ri RouteInfo) error

// WalkRoutes iterates over all registered routes with full RouteInfo,
// calling fn for each one.
func WalkRoutes(r Routes, fn WalkRoutesFunc) error {
	for _, ri := range r.Routes() {
		if err := fn(ri); err != nil {
			return err
		}
	}
	return nil
}
