// Copyright (c) 2015-present Peter Kieltyka (https://github.com/pkieltyka), Google Inc.
// Originally derived from github.com/go-chi/chi (MIT License).

package radix

import (
	"maps"
	"net/http"
	"slices"
)

// MethodTyp is a bitmask representing one or more HTTP methods.
type MethodTyp uint

const (
	mStub    MethodTyp = 1 << iota // Stub — placeholder, not a real method
	MConnect                       // CONNECT
	MDelete                        // DELETE
	MGet                           // GET
	MHead                          // HEAD
	MOptions                       // OPTIONS
	MPatch                         // PATCH
	MPost                          // POST
	MPut                           // PUT
	MTrace                         // TRACE
	MQuery                         // QUERY

	mAny = MConnect | MDelete | MGet | MHead | MOptions | MPatch | MPost | MPut | MTrace | MQuery
)

const methodQuery = "QUERY"

// standardMethodNames caches the reverse mapping from single-method bitmask
// to HTTP method string for fast MethodTypToString on standard methods.
var standardMethodNames = map[MethodTyp]string{
	MConnect: http.MethodConnect,
	MDelete:  http.MethodDelete,
	MGet:     http.MethodGet,
	MHead:    http.MethodHead,
	MOptions: http.MethodOptions,
	MPatch:   http.MethodPatch,
	MPost:    http.MethodPost,
	MPut:     http.MethodPut,
	MTrace:   http.MethodTrace,
	MQuery:   methodQuery,
}

// methodMap is the immutable method string-to-MethodTyp table. The method
// set is fixed at compile time; there is no runtime registration.
var methodMap = map[string]MethodTyp{
	http.MethodConnect: MConnect,
	http.MethodDelete:  MDelete,
	http.MethodGet:     MGet,
	http.MethodHead:    MHead,
	http.MethodOptions: MOptions,
	http.MethodPatch:   MPatch,
	http.MethodPost:    MPost,
	http.MethodPut:     MPut,
	http.MethodTrace:   MTrace,
	methodQuery:        MQuery,
}

// LookupMethod returns the MethodTyp for the given HTTP method string
// and a boolean indicating whether it is a known method.
func LookupMethod(method string) (MethodTyp, bool) {
	switch method {
	case http.MethodConnect:
		return MConnect, true
	case http.MethodDelete:
		return MDelete, true
	case http.MethodGet:
		return MGet, true
	case http.MethodHead:
		return MHead, true
	case http.MethodOptions:
		return MOptions, true
	case http.MethodPatch:
		return MPatch, true
	case http.MethodPost:
		return MPost, true
	case http.MethodPut:
		return MPut, true
	case http.MethodTrace:
		return MTrace, true
	case methodQuery:
		return MQuery, true
	}
	return 0, false
}

// AllMethods returns a copy of all known method string-to-MethodTyp pairs.
func AllMethods() map[string]MethodTyp {
	return maps.Clone(methodMap)
}

// MethodTypToString converts a MethodTyp bitmask to a sorted slice
// of HTTP method strings. Only known methods are included.
func MethodTypToString(mtyp MethodTyp) []string {
	// Fast path: single method (no allocation beyond the result)
	if name, ok := standardMethodNames[mtyp]; ok {
		return []string{name}
	}

	var methods []string
	for name, bit := range methodMap {
		if mtyp&bit != 0 {
			methods = append(methods, name)
		}
	}
	slices.Sort(methods)
	return methods
}
