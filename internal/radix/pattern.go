// Copyright (c) 2015-present Peter Kieltyka (https://github.com/pkieltyka), Google Inc.
// Originally derived from github.com/go-chi/chi (MIT License).

package radix

import (
	"fmt"
	"regexp"
	"strings"
)

// NodeTyp classifies the type of a radix tree node.
type NodeTyp uint8

const (
	NtStatic   NodeTyp = iota // Static path segment (e.g., "/users")
	NtRegexp                  // Regex-constrained parameter (e.g., "{id:[0-9]+}")
	NtParam                   // Named parameter (e.g., "{id}")
	NtCatchAll                // Catch-all parameter (e.g., "{path...}")
)

// PatternSegment holds the parsed result from patNextSegment.
type PatternSegment struct {
	// Typ is the type of this segment.
	Typ NodeTyp

	// Prefix is the static string before the parameter.
	Prefix string

	// ParamKey is the parameter name (without braces).
	ParamKey string

	// Regexp is the compiled regex for NtRegexp segments.
	Regexp *regexp.Regexp

	// Suffix is the remaining pattern after this segment.
	Suffix string

	// TailByte is the first byte after the parameter
	// (used for fast scanning during match).
	TailByte byte
}

// patNextSegment parses the next segment from a route pattern string.
//
// Pattern syntax:
//   - {name}      → named parameter (NtParam)
//   - {name:re}   → regex-constrained parameter (NtRegexp)
//   - {name...}   → catch-all parameter (NtCatchAll)
//   - anything else → static segment (NtStatic)
//
// Returns a PatternSegment and an error if the pattern is malformed.
func patNextSegment(pattern string) (PatternSegment, error) {
	ps := PatternSegment{}

	// Find opening brace
	openIdx := strings.IndexByte(pattern, '{')
	if openIdx < 0 || openIdx == len(pattern)-1 {
		// No parameter — entire segment is static
		ps.Typ = NtStatic
		ps.Prefix = pattern
		return ps, nil
	}

	// Find matching closing brace (brace-depth aware).
	// A simple IndexByte('}') would break regex quantifiers like {id:[0-9]{2,4}}.
	closeIdx := FindMatchingBrace(pattern, openIdx)
	if closeIdx < 0 {
		return ps, fmt.Errorf("radix: unclosed parameter brace in %q", pattern)
	}

	// Static prefix before the parameter
	ps.Prefix = pattern[:openIdx]

	// Content inside braces
	inner := pattern[openIdx+1 : closeIdx]

	// Suffix after closing brace
	ps.Suffix = pattern[closeIdx+1:]

	if inner == "" {
		return ps, fmt.Errorf("radix: empty parameter name in %q", pattern)
	}

	// Check for catch-all: {name...}
	if strings.HasSuffix(inner, "...") {
		ps.Typ = NtCatchAll
		ps.ParamKey = inner[:len(inner)-3]
		if ps.ParamKey == "" {
			return ps, fmt.Errorf("radix: empty catch-all parameter name in %q", pattern)
		}
		return ps, nil
	}

	// Check for regex: {name:regex}
	if paramKey, reStr, ok := strings.Cut(inner, ":"); ok {
		ps.Typ = NtRegexp
		ps.ParamKey = paramKey
		if ps.ParamKey == "" {
			return ps, fmt.Errorf("radix: empty regex parameter name in %q", pattern)
		}
		if reStr == "" {
			return ps, fmt.Errorf("radix: empty regex in %q", pattern)
		}
		re, err := regexp.Compile("^(" + reStr + ")")
		if err != nil {
			return ps, fmt.Errorf("radix: invalid regex %q in %q: %w", reStr, pattern, err)
		}
		ps.Regexp = re

		// Determine tail byte for scanning
		if len(ps.Suffix) > 0 {
			ps.TailByte = ps.Suffix[0]
		}
		return ps, nil
	}

	// Simple named parameter: {name}
	ps.Typ = NtParam
	ps.ParamKey = inner

	// Determine tail byte
	if len(ps.Suffix) > 0 {
		ps.TailByte = ps.Suffix[0]
	}

	return ps, nil
}

// FindMatchingBrace returns the index of the closing '}' that matches the
// opening '{' at position openIdx, accounting for nested braces in regex
// quantifiers like {id:[0-9]{2,4}}, escaped braces (\{, \}), and
// character classes ([...]). Returns -1 if no matching brace is found.
func FindMatchingBrace(pattern string, openIdx int) int {
	depth := 0
	escaped := false
	inClass := false

	for i := openIdx; i < len(pattern); i++ {
		if escaped {
			escaped = false
			continue
		}

		if pattern[i] == '\\' {
			escaped = true
			continue
		}

		if inClass {
			if pattern[i] == ']' {
				inClass = false
			}
			continue
		}

		switch pattern[i] {
		case '[':
			inClass = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}
