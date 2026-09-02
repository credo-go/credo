// Copyright (c) 2015-present Peter Kieltyka (https://github.com/pkieltyka), Google Inc.
// Originally derived from github.com/go-chi/chi (MIT License).

// Package pattern owns Credo's route-pattern grammar: {name} matches one
// path segment, {name:regex} constrains it, and {name...} captures the rest
// of the path. The radix tree, route URL building, host/route parameter
// checks, and the Rewrite middleware all parse patterns through this package
// so the grammar has exactly one implementation.
package pattern

import (
	"fmt"
	"regexp"
	"strings"
)

// Kind classifies a parsed pattern segment. The numeric order is the routing
// priority the radix tree relies on (static before regexp before param before
// catch-all), so the values must not be reordered.
type Kind uint8

const (
	Static   Kind = iota // literal path text (e.g. "/users")
	Regexp               // regex-constrained parameter (e.g. "{id:[0-9]+}")
	Param                // named parameter (e.g. "{id}")
	CatchAll             // catch-all parameter (e.g. "{path...}")
)

// Segment is the parsed result of the next parameter in a pattern.
type Segment struct {
	// Kind is the segment kind.
	Kind Kind

	// Prefix is the literal text before the parameter (the whole pattern for
	// Static).
	Prefix string

	// Name is the parameter name without braces, "..." or the constraint.
	Name string

	// Regexp is the constraint compiled as "^(regex)" for Regexp segments.
	Regexp *regexp.Regexp

	// RegexpSource is the raw constraint text for Regexp segments.
	RegexpSource string

	// Suffix is the remaining pattern after the closing brace.
	Suffix string

	// TailByte is the first byte of Suffix (0 when Suffix is empty); the tree
	// uses it to bound parameter scanning.
	TailByte byte
}

// NextSegment parses the next segment of pattern. A pattern without an
// opening brace, or with a trailing "{" at its very end, is one Static
// segment. Malformed parameters (unclosed brace, empty name, empty or invalid
// regex) return an error.
func NextSegment(pattern string) (Segment, error) {
	seg := Segment{}

	openIdx := strings.IndexByte(pattern, '{')
	if openIdx < 0 || openIdx == len(pattern)-1 {
		seg.Kind = Static
		seg.Prefix = pattern
		return seg, nil
	}

	// A plain IndexByte('}') would break on regex quantifiers like {id:[0-9]{2,4}}.
	closeIdx := FindMatchingBrace(pattern, openIdx)
	if closeIdx < 0 {
		return seg, fmt.Errorf("pattern: unclosed parameter brace in %q", pattern)
	}

	seg.Prefix = pattern[:openIdx]
	inner := pattern[openIdx+1 : closeIdx]
	seg.Suffix = pattern[closeIdx+1:]
	if len(seg.Suffix) > 0 {
		seg.TailByte = seg.Suffix[0]
	}

	if inner == "" {
		return seg, fmt.Errorf("pattern: empty parameter name in %q", pattern)
	}

	if name, ok := strings.CutSuffix(inner, "..."); ok {
		seg.Kind = CatchAll
		seg.Name = name
		seg.TailByte = 0
		if name == "" {
			return seg, fmt.Errorf("pattern: empty catch-all parameter name in %q", pattern)
		}
		return seg, nil
	}

	if name, reStr, ok := strings.Cut(inner, ":"); ok {
		seg.Kind = Regexp
		seg.Name = name
		seg.RegexpSource = reStr
		if name == "" {
			return seg, fmt.Errorf("pattern: empty regex parameter name in %q", pattern)
		}
		if reStr == "" {
			return seg, fmt.Errorf("pattern: empty regex in %q", pattern)
		}
		re, err := regexp.Compile("^(" + reStr + ")")
		if err != nil {
			return seg, fmt.Errorf("pattern: invalid regex %q in %q: %w", reStr, pattern, err)
		}
		seg.Regexp = re
		return seg, nil
	}

	seg.Kind = Param
	seg.Name = inner
	return seg, nil
}

// FindMatchingBrace returns the index of the closing '}' that matches the
// opening '{' at openIdx, accounting for nested braces in regex quantifiers
// like {id:[0-9]{2,4}}, escaped braces (\{, \}), and character classes
// ([...]). It returns -1 when no matching brace exists.
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

// ParamName returns the parameter name of the text between one pair of
// braces: "id" for "id", "id:[0-9]+" and "id...".
func ParamName(inner string) string {
	if name, _, ok := strings.Cut(inner, ":"); ok {
		return name
	}
	return strings.TrimSuffix(inner, "...")
}

// ParamNames returns every parameter name in pattern in order. Parsing stops
// at the first malformed parameter, mirroring how registration reports the
// error separately; the names collected so far are returned.
func ParamNames(pattern string) []string {
	var names []string
	rest := pattern
	for {
		seg, err := NextSegment(rest)
		if err != nil || seg.Kind == Static {
			return names
		}
		names = append(names, seg.Name)
		rest = seg.Suffix
	}
}

// ToRegexp converts a whole pattern into an anchored regexp with one capture
// group per parameter: {name} → ([^/]+), {name...} → (.*), {name:re} → (re).
// names[0] is "" so the slice aligns with regexp.FindStringSubmatch.
func ToRegexp(pattern string) (*regexp.Regexp, []string, error) {
	var b strings.Builder
	names := []string{""}
	b.WriteString("^")

	rest := pattern
	for {
		seg, err := NextSegment(rest)
		if err != nil {
			return nil, nil, err
		}
		b.WriteString(regexp.QuoteMeta(seg.Prefix))
		switch seg.Kind {
		case Static:
			b.WriteString("$")
			re, err := regexp.Compile(b.String())
			if err != nil {
				return nil, nil, fmt.Errorf("pattern: compile %q: %w", pattern, err)
			}
			return re, names, nil
		case Param:
			b.WriteString("([^/]+)")
		case CatchAll:
			b.WriteString("(.*)")
		case Regexp:
			b.WriteString("(")
			b.WriteString(seg.RegexpSource)
			b.WriteString(")")
		}
		names = append(names, seg.Name)
		rest = seg.Suffix
	}
}
