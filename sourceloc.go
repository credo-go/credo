package credo

import (
	"runtime"
	"strconv"
	"strings"
)

// sourceLocation is the file:line in user code where a route or mount was
// registered. It is captured once at registration time and surfaced via
// [RouteInfo.RegisteredAt] and in duplicate-route panics ("first registered
// at … → now registered at …").
type sourceLocation struct {
	file string
	line int
}

// String renders the location as "file:line", or "" when unknown.
func (s sourceLocation) String() string {
	if s.file == "" {
		return ""
	}
	return s.file + ":" + strconv.Itoa(s.line)
}

// callerLocation walks the call stack and returns the first frame outside the
// credo framework packages — the user's registration call site. Because it
// skips every framework frame rather than counting a fixed depth, it captures
// the correct location no matter how deep the internal registration funnel runs
// (App.GET → Group.GET → addGetRoute → addRoute, the auto HEAD twin, Static's
// catch-all + index pair, Mount's two registrations, host-scoped groups, …).
//
// Registration reached through a user helper (e.g. a controller's
// RegisterRoutes) is reported at that helper's call site, mirroring how the
// standard library attributes a logger's caller — documented behaviour, not a
// bug. A future "skip N frames" option can refine this if needed.
func callerLocation() sourceLocation {
	var pcs [32]uintptr
	// Skip runtime.Callers and callerLocation itself.
	n := runtime.Callers(2, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		if frame.Function != "" && !isFrameworkFrame(frame.Function) {
			return sourceLocation{file: frame.File, line: frame.Line}
		}
		if !more {
			return sourceLocation{}
		}
	}
}

// isFrameworkFrame reports whether fn — a fully-qualified function name from the
// runtime, e.g. "github.com/credo-go/credo.(*App).addRoute" — belongs to the
// credo framework's registration machinery: the root package or an internal
// subpackage. The trailing "." and "/internal/" guards keep sibling paths such
// as "github.com/credo-go/credo_test" (black-box tests) and all user code
// (including package main, reported as "main.*") classified as non-framework so
// their call sites are the ones captured.
func isFrameworkFrame(fn string) bool {
	const base = "github.com/credo-go/credo"
	return strings.HasPrefix(fn, base+".") || strings.HasPrefix(fn, base+"/internal/")
}
