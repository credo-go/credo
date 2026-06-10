// Copyright (c) 2015-present Peter Kieltyka (https://github.com/pkieltyka), Google Inc.
// Originally derived from github.com/go-chi/chi (MIT License).

package radix

import (
	"maps"
	"net/http"
	"slices"
	"sync"
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

	mAny = MConnect | MDelete | MGet | MHead | MOptions | MPatch | MPost | MPut | MTrace
)

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
}

var (
	mu sync.RWMutex

	methodMap = map[string]MethodTyp{
		http.MethodConnect: MConnect,
		http.MethodDelete:  MDelete,
		http.MethodGet:     MGet,
		http.MethodHead:    MHead,
		http.MethodOptions: MOptions,
		http.MethodPatch:   MPatch,
		http.MethodPost:    MPost,
		http.MethodPut:     MPut,
		http.MethodTrace:   MTrace,
	}

	// nextMethodBit is the next unused bit position for custom methods.
	// MTrace occupies bit 9 (1<<9 = 512), so the next available is bit 10.
	nextMethodBit = uint(10)
)

// LookupMethod returns the MethodTyp for the given HTTP method string
// and a boolean indicating whether it was found. Standard HTTP methods
// are resolved via a lock-free switch for hot-path performance; only
// custom methods fall through to the locked map.
func LookupMethod(method string) (MethodTyp, bool) {
	// Fast path: standard methods (no lock needed).
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
	}

	// Slow path: custom methods (locked map lookup).
	mu.RLock()
	defer mu.RUnlock()
	mtyp, ok := methodMap[method]
	return mtyp, ok
}

// AllMethods returns a copy of all registered method string-to-MethodTyp pairs.
func AllMethods() map[string]MethodTyp {
	mu.RLock()
	defer mu.RUnlock()
	return maps.Clone(methodMap)
}

// registerMethod adds a custom HTTP method to the method map.
// Returns the assigned MethodTyp. Safe for concurrent use.
// Must be called before any routes using this method are registered.
func registerMethod(method string) MethodTyp {
	mu.Lock()
	defer mu.Unlock()

	if mtyp, ok := methodMap[method]; ok {
		return mtyp
	}
	if nextMethodBit >= 32 {
		panic("radix: too many custom HTTP methods registered (max 32)")
	}
	mtyp := MethodTyp(1 << nextMethodBit)
	nextMethodBit++
	methodMap[method] = mtyp
	return mtyp
}

// MethodTypToString converts a MethodTyp bitmask to a sorted slice
// of HTTP method strings. Only known (registered) methods are included.
func MethodTypToString(mtyp MethodTyp) []string {
	// Fast path: single standard method (no lock, no allocation)
	if name, ok := standardMethodNames[mtyp]; ok {
		return []string{name}
	}

	// Slow path: multi-bit or custom methods
	mu.RLock()
	defer mu.RUnlock()

	var methods []string
	for name, bit := range methodMap {
		if mtyp&bit != 0 {
			methods = append(methods, name)
		}
	}
	slices.Sort(methods)
	return methods
}
