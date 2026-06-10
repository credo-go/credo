// Copyright (c) 2015-present Peter Kieltyka (https://github.com/pkieltyka), Google Inc.
// Originally derived from github.com/go-chi/chi (MIT License).

package credo

import (
	"context"
	"net/http"

	"github.com/credo-go/credo/internal/radix"
)

// RouteContext is the routing context type used by the radix tree.
// Re-exported from internal/radix so consumers can reference this
// type without importing internal packages.
type RouteContext = radix.RouteContext

// routeCtxKey is the context key for storing the RouteContext.
// Using a pointer to a private struct guarantees uniqueness.
var routeCtxKey = &struct{ name string }{"RouteContext"}

// getRouteContext returns the RouteContext from the request context.
// Returns nil if no RouteContext is set.
func getRouteContext(r *http.Request) *RouteContext {
	val := r.Context().Value(routeCtxKey)
	if val == nil {
		return nil
	}
	rctx, _ := val.(*RouteContext)
	return rctx
}

// withRouteContext returns a new request with the given RouteContext
// stored in its context.
func withRouteContext(r *http.Request, rctx *RouteContext) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), routeCtxKey, rctx))
}

// URLParam returns the named URL parameter from the request's RouteContext.
// It is intended for use in stdlib handlers mounted via [App.Mount]; normal
// Credo handlers should use [Request.RouteParams] instead.
// Returns empty string if the parameter doesn't exist or no RouteContext is set.
func URLParam(r *http.Request, name string) string {
	rctx := getRouteContext(r)
	if rctx == nil {
		return ""
	}
	return rctx.URLParam(name)
}
