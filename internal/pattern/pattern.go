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
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/credo-go/credo/internal/wirepath"
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

	// Regexp is the constraint compiled as "^(?:regex)$" for Regexp segments:
	// a constraint always applies to a whole decoded parameter value, never to
	// a prefix of it.
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
		re, err := regexp.Compile("^(?:" + reStr + ")$")
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

// CheckNames reports an error when pattern uses a parameter name more than
// once. A name is how a capture is read; with a name used twice, one capture
// is unreadable and lookups that scan from different ends answer with
// different captures. Parsing stops at the first malformed parameter, which
// NextSegment reports.
func CheckNames(pattern string) error {
	names := ParamNames(pattern)
	for i, name := range names {
		if slices.Contains(names[:i], name) {
			return fmt.Errorf("pattern: duplicate parameter name %q in %q", name, pattern)
		}
	}
	return nil
}

// Matcher matches a canonical wire path (see [wirepath.Canonical]) against
// one pattern the way the router matches a route: literal text in canonical
// form (see [wirepath.Static]); a {name} or {name:regex} candidate that ends
// at the parameter's tail byte or the next slash, whichever comes first, with
// every escape one unit (see [wirepath.CandidateEnd]), so it never spans a
// raw slash and an escaped slash ("%2F") is data; a constraint checked on the
// whole decoded value; and a {name...} catch-all that takes the rest of the
// path, an empty rest included. A capture that decodes to invalid UTF-8 is
// no match. Each capture is reported in canonical wire form, which decodes
// once to the value a route parameter would have.
type Matcher struct {
	params []matcherParam
	tail   string // canonical literal text after the last parameter
	names  []string
}

type matcherParam struct {
	prefix string         // canonical literal text before the parameter
	kind   Kind           // Param, Regexp or CatchAll
	re     *regexp.Regexp // the constraint, anchored, for Regexp
	tail   byte           // the pattern byte after the closing brace
}

// NewMatcher parses pattern into a [Matcher]. A malformed parameter, a
// repeated parameter name (see [CheckNames]) and literal text after a
// catch-all, which takes the rest of the path, are errors.
func NewMatcher(pattern string) (*Matcher, error) {
	if err := CheckNames(pattern); err != nil {
		return nil, err
	}
	m := &Matcher{}
	rest := pattern
	for {
		seg, err := NextSegment(rest)
		if err != nil {
			return nil, err
		}
		if seg.Kind == Static {
			m.tail = wirepath.Static(seg.Prefix)
			return m, nil
		}
		if seg.Kind == CatchAll && seg.Suffix != "" {
			return nil, fmt.Errorf("pattern: text %q after the catch-all {%s...} in %q: a catch-all takes the rest of the path, so end the pattern with it", seg.Suffix, seg.Name, pattern)
		}
		m.params = append(m.params, matcherParam{
			prefix: wirepath.Static(seg.Prefix),
			kind:   seg.Kind,
			re:     seg.Regexp,
			tail:   seg.TailByte,
		})
		m.names = append(m.names, seg.Name)
		rest = seg.Suffix
	}
}

// Names returns the parameter names in pattern order, aligned with the
// captures [Matcher.Match] returns.
func (m *Matcher) Names() []string { return m.names }

// Match reports whether the canonical wire path matches the pattern and
// returns one canonical capture per parameter.
func (m *Matcher) Match(path string) ([]string, bool) {
	captures := make([]string, 0, len(m.params))
	rest := path
	for _, p := range m.params {
		after, ok := strings.CutPrefix(rest, p.prefix)
		if !ok {
			return nil, false
		}
		rest = after
		if p.kind == CatchAll {
			if _, valid := decode(rest); !valid {
				return nil, false
			}
			captures = append(captures, rest)
			rest = ""
			continue
		}
		end := wirepath.CandidateEnd(rest, p.tail)
		if end == 0 {
			return nil, false // a parameter never matches an empty segment
		}
		value, valid := decode(rest[:end])
		if !valid || (p.re != nil && !p.re.MatchString(value)) {
			return nil, false
		}
		captures = append(captures, rest[:end])
		rest = rest[end:]
	}
	if rest != m.tail {
		return nil, false
	}
	return captures, true
}

// decode returns the value of a canonical capture, decoded once, and whether
// it is valid UTF-8.
func decode(raw string) (string, bool) {
	value := raw
	if strings.IndexByte(raw, '%') >= 0 {
		var err error
		if value, err = url.PathUnescape(raw); err != nil {
			return "", false
		}
	}
	return value, utf8.ValidString(value)
}
