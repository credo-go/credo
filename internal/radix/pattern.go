// Copyright (c) 2015-present Peter Kieltyka (https://github.com/pkieltyka), Google Inc.
// Originally derived from github.com/go-chi/chi (MIT License).

package radix

import "github.com/credo-go/credo/internal/pattern"

// NodeTyp classifies the type of a radix tree node. It is the route-pattern
// segment kind from internal/pattern, which owns the pattern grammar; the
// numeric order doubles as routing priority (static > regexp > param >
// catch-all).
type NodeTyp = pattern.Kind

const (
	NtStatic   = pattern.Static   // Static path segment (e.g., "/users")
	NtRegexp   = pattern.Regexp   // Regex-constrained parameter (e.g., "{id:[0-9]+}")
	NtParam    = pattern.Param    // Named parameter (e.g., "{id}")
	NtCatchAll = pattern.CatchAll // Catch-all parameter (e.g., "{path...}")
)

// PatternSegment is the parsed result of one route-pattern parameter.
type PatternSegment = pattern.Segment

// patNextSegment parses the next segment from a route pattern string.
func patNextSegment(p string) (PatternSegment, error) {
	return pattern.NextSegment(p)
}
