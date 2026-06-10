package credo

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	internalhost "github.com/credo-go/credo/internal/host"
)

// hostSegmentType represents the type of a host pattern segment.
type hostSegmentType uint8

const (
	hostSegStatic   hostSegmentType = iota // exact label: "api", "example", "com"
	hostSegParam                           // named param: "{tenant}"
	hostSegRegexp                          // regex param: "{org:[a-z]+}"
	hostSegWildcard                        // anonymous single-label wildcard: "*"
)

// hostSegment is a single dot-separated label in a host pattern.
type hostSegment struct {
	typ    hostSegmentType
	value  string         // static text or param name
	regexp *regexp.Regexp // compiled regex (nil for static/param/wildcard)
}

// hostEntry holds a host pattern and its dedicated routing infrastructure.
// The host's root *Group (returned by App.Host) is not retained here; routes
// reach it through Route.parent.
type hostEntry struct {
	pattern   string        // normalized: "{tenant}.myapp.com"
	segments  []hostSegment // parsed, reversed for matching (com, myapp, {tenant})
	paramKeys []string      // ["tenant"] — parameter names in order
	semantic  string        // canonical match semantics, ignoring param names
	mux       *mux          // dedicated radix tree
}

// normalizeHostPattern validates and normalizes a host pattern.
// Lowercases and trims trailing dot. Panics if the pattern contains a port.
func normalizeHostPattern(pattern string) string {
	if internalhost.PatternHasPort(pattern) {
		panic("credo: host patterns must not include a port")
	}
	return strings.TrimSuffix(strings.ToLower(pattern), ".")
}

// normalizeRequestHost lowercases the host, strips the port, and trims a
// trailing dot. Port stripping happens before dot trimming so that
// "example.com.:8080" becomes "example.com".
func normalizeRequestHost(host string) string {
	return internalhost.NormalizeRequest(host)
}

// parseHostPattern splits a normalized host pattern by dots, reverses the
// labels (TLD first), and classifies each as static, param, regexp, or wildcard.
// Returns the segments and parameter names in segment order.
func parseHostPattern(pattern string) ([]hostSegment, []string) {
	labels := strings.Split(pattern, ".")
	validateHostWildcard(pattern, labels)

	segments := make([]hostSegment, 0, len(labels))
	var paramKeys []string

	for i := len(labels) - 1; i >= 0; i-- { // reverse
		label := labels[i]
		if label == "*" {
			segments = append(segments, hostSegment{typ: hostSegWildcard})
		} else if len(label) > 1 && label[0] == '{' && label[len(label)-1] == '}' {
			inner := label[1 : len(label)-1]
			if name, reStr, ok := strings.Cut(inner, ":"); ok {
				// Regex constraint: {name:pattern}
				re := regexp.MustCompile("^(" + reStr + ")$")
				segments = append(segments, hostSegment{
					typ: hostSegRegexp, value: name, regexp: re,
				})
				paramKeys = append(paramKeys, name)
			} else {
				// Plain param: {name}
				segments = append(segments, hostSegment{
					typ: hostSegParam, value: inner,
				})
				paramKeys = append(paramKeys, inner)
			}
		} else {
			segments = append(segments, hostSegment{
				typ: hostSegStatic, value: label,
			})
		}
	}
	return segments, paramKeys
}

func validateHostWildcard(pattern string, labels []string) {
	wildcards := 0
	hasParam := false
	for i, label := range labels {
		if isHostParamLabel(label) {
			hasParam = true
			continue
		}
		if strings.Contains(label, "*") && label != "*" {
			panic(fmt.Sprintf("credo: invalid host pattern %q: wildcard * must be a complete label", pattern))
		}
		if label == "*" {
			wildcards++
			if i != 0 {
				panic(fmt.Sprintf("credo: invalid host pattern %q: wildcard * must be the leftmost label", pattern))
			}
		}
	}
	if wildcards > 1 {
		panic(fmt.Sprintf("credo: invalid host pattern %q: wildcard * may appear at most once", pattern))
	}
	if wildcards == 1 && hasParam {
		panic(fmt.Sprintf("credo: invalid host pattern %q: wildcard * cannot be mixed with host params", pattern))
	}
}

func isHostParamLabel(label string) bool {
	return len(label) > 1 && label[0] == '{' && label[len(label)-1] == '}'
}

func hostPatternSemanticKey(segments []hostSegment) string {
	var b strings.Builder
	for _, seg := range segments {
		kind := ""
		value := ""
		switch seg.typ {
		case hostSegStatic:
			kind = "s"
			value = seg.value
		case hostSegParam, hostSegWildcard:
			kind = "x"
		case hostSegRegexp:
			kind = "r"
			value = seg.regexp.String()
		}
		b.WriteString(kind)
		b.WriteByte(':')
		b.WriteString(strconv.Itoa(len(value)))
		b.WriteByte(':')
		b.WriteString(value)
		b.WriteByte('|')
	}
	return b.String()
}

func hostPatternHasWildcard(pattern string) bool {
	for _, label := range strings.Split(pattern, ".") {
		if label == "*" {
			return true
		}
	}
	return false
}

// match checks if the reversed host labels match this entry's segments.
// Returns captured parameters and true on match. For static-only hosts,
// returns nil map as an optimization.
func (e *hostEntry) match(reversedLabels []string) (map[string]string, bool) {
	if len(reversedLabels) != len(e.segments) {
		return nil, false
	}

	var params map[string]string
	for i, seg := range e.segments {
		label := reversedLabels[i]
		switch seg.typ {
		case hostSegStatic:
			if !strings.EqualFold(seg.value, label) {
				return nil, false
			}
		case hostSegParam:
			if params == nil {
				params = make(map[string]string, len(e.paramKeys))
			}
			params[seg.value] = label
		case hostSegRegexp:
			if !seg.regexp.MatchString(label) {
				return nil, false
			}
			if params == nil {
				params = make(map[string]string, len(e.paramKeys))
			}
			params[seg.value] = label
		case hostSegWildcard:
			// Anonymous wildcard: consume exactly one host label without capturing it.
		}
	}
	return params, true
}

// matchHost finds the best matching hostEntry for the given request host.
// Returns nil if no host matches (caller falls back to default mux).
// Hosts are pre-sorted by specificity in compile().
func (app *App) matchHost(host string) (*hostEntry, map[string]string) {
	if host == "" {
		return nil, nil
	}
	host = normalizeRequestHost(host)
	if entry := app.staticHosts[host]; entry != nil {
		return entry, nil
	}

	labels := strings.Split(host, ".")

	// Reverse labels for matching against reversed segments.
	for i, j := 0, len(labels)-1; i < j; i, j = i+1, j-1 {
		labels[i], labels[j] = labels[j], labels[i]
	}

	for _, entry := range app.hosts {
		if params, ok := entry.match(labels); ok {
			return entry, params
		}
	}
	return nil, nil
}

func isStaticHostEntry(entry *hostEntry) bool {
	if len(entry.paramKeys) != 0 {
		return false
	}
	for _, seg := range entry.segments {
		if seg.typ != hostSegStatic {
			return false
		}
	}
	return true
}

// hasHostPattern returns true if a host entry with the normalized pattern
// is already registered. Used for duplicate detection.
func (app *App) hasHostPattern(normalized string) bool {
	for _, e := range app.hosts {
		if e.pattern == normalized {
			return true
		}
	}
	return false
}

func (app *App) hostSemanticConflict(semantic string) string {
	for _, e := range app.hosts {
		if e.semantic == semantic {
			return e.pattern
		}
	}
	return ""
}

func (app *App) hostParamKeys(hostPattern string) []string {
	for _, e := range app.hosts {
		if e.pattern == hostPattern {
			return e.paramKeys
		}
	}
	return nil
}

// compareHostEntries compares two host entries by specificity for sorting.
// Returns negative if a is more specific, positive if b is more specific.
// Segments are compared right-to-left (most-significant label first in the
// reversed segment slice). Static > regexp > param/wildcard.
func compareHostEntries(a, b *hostEntry) int {
	minLen := min(len(a.segments), len(b.segments))

	for i := 0; i < minLen; i++ {
		sa := hostSegmentSpecificity(a.segments[i])
		sb := hostSegmentSpecificity(b.segments[i])
		if sa != sb {
			return sb - sa // higher specificity first
		}
	}

	// More segments = more specific.
	return len(b.segments) - len(a.segments)
}

// hostSegmentSpecificity returns the specificity weight of a segment type.
// Static (3) > regexp (2) > param/wildcard (1).
func hostSegmentSpecificity(seg hostSegment) int {
	switch seg.typ {
	case hostSegStatic:
		return 3
	case hostSegRegexp:
		return 2
	case hostSegParam, hostSegWildcard:
		return 1
	default:
		return 0
	}
}
