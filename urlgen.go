package credo

import (
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	internalpattern "github.com/credo-go/credo/internal/pattern"
	"github.com/credo-go/credo/internal/wirepath"
)

// pathTemplate is a route pattern parsed once for URL generation. The last
// segment is always Static and carries the pattern's trailing literal text.
type pathTemplate struct {
	segments []internalpattern.Segment
}

// parsePathTemplate splits pattern into its literal and parameter segments.
func parsePathTemplate(pattern string) (*pathTemplate, error) {
	t := &pathTemplate{}
	rest := pattern
	for {
		seg, err := internalpattern.NextSegment(rest)
		if err != nil {
			return nil, err
		}
		t.segments = append(t.segments, seg)
		if seg.Kind == internalpattern.Static {
			return t, nil
		}
		rest = seg.Suffix
	}
}

// build renders the pattern with the given decoded parameter values in order
// and reports how many values it consumed. Every value is validated against
// its constraint and percent-encoded per path segment, so the generated URI
// routes back to the same values: a single-segment parameter never gains a
// slash ("a/b" becomes "a%2Fb"), a catch-all keeps its slashes as separators
// and escapes each segment, "+" stays "+" and valid Unicode is encoded as
// UTF-8 octets; static pattern text is written in its wire spelling. A value
// that cannot round-trip is rejected: an empty value matches no parameter,
// invalid UTF-8 is refused by the router (400), and a value containing the
// byte that delimits its parameter in the pattern ("." for "{name}.json")
// would be cut at that byte when matched, since a percent-encoded delimiter
// is equivalent to the literal one.
func (t *pathTemplate) build(values []string) (string, int, error) {
	var b strings.Builder
	consumed := 0
	for _, seg := range t.segments {
		b.WriteString(wirepath.Escape(wirepath.Static(seg.Prefix)))
		if seg.Kind == internalpattern.Static {
			break
		}
		if consumed >= len(values) {
			return "", consumed, fmt.Errorf("missing parameter %q", seg.Name)
		}
		value := values[consumed]
		consumed++
		if value == "" {
			return "", consumed, fmt.Errorf("empty value for parameter %q", seg.Name)
		}
		if !utf8.ValidString(value) {
			return "", consumed, fmt.Errorf("value for parameter %q is not valid UTF-8", seg.Name)
		}
		if tail := seg.TailByte; tail != 0 && tail != '/' && strings.IndexByte(value, tail) >= 0 {
			return "", consumed, fmt.Errorf("value %q for parameter %q contains its delimiter %q", value, seg.Name, string(tail))
		}
		switch seg.Kind {
		case internalpattern.Regexp:
			if !seg.Regexp.MatchString(value) {
				return "", consumed, fmt.Errorf("value %q for parameter %q does not match constraint %q", value, seg.Name, seg.RegexpSource)
			}
			b.WriteString(url.PathEscape(value))
		case internalpattern.CatchAll:
			b.WriteString(escapeCatchAll(value))
		default:
			b.WriteString(url.PathEscape(value))
		}
	}
	return b.String(), consumed, nil
}

// escapeCatchAll percent-encodes a catch-all value segment by segment,
// preserving its slash separators.
func escapeCatchAll(value string) string {
	if strings.IndexByte(value, '/') < 0 {
		return url.PathEscape(value)
	}
	parts := strings.Split(value, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

// hostTemplate is a host pattern parsed once for URL generation, in label
// order (leftmost label first).
type hostTemplate struct {
	segments []hostSegment
}

// parseHostTemplate reuses the host-routing parser, whose segments are
// stored TLD-first, and restores label order.
func parseHostTemplate(pattern string) *hostTemplate {
	reversed, _ := parseHostPattern(pattern)
	segments := make([]hostSegment, len(reversed))
	for i, seg := range reversed {
		segments[len(reversed)-1-i] = seg
	}
	return &hostTemplate{segments: segments}
}

// build renders the host pattern with the given values in order and reports
// how many it consumed. Host labels are validated, never percent-encoded: a
// value fills exactly one label and must be a non-empty run of letters,
// digits, hyphens and underscores that satisfies the label's constraint.
func (t *hostTemplate) build(values []string) (string, int, error) {
	var b strings.Builder
	consumed := 0
	for i, seg := range t.segments {
		if i > 0 {
			b.WriteByte('.')
		}
		switch seg.typ {
		case hostSegStatic:
			b.WriteString(seg.value)
			continue
		case hostSegWildcard:
			return "", consumed, fmt.Errorf("wildcard host patterns cannot generate concrete URLs")
		}
		if consumed >= len(values) {
			return "", consumed, fmt.Errorf("missing parameter %q", seg.value)
		}
		value := values[consumed]
		consumed++
		if err := validateHostLabel(value); err != nil {
			return "", consumed, fmt.Errorf("parameter %q: %w", seg.value, err)
		}
		if seg.typ == hostSegRegexp && !seg.regexp.MatchString(value) {
			return "", consumed, fmt.Errorf("value %q for parameter %q does not match its host constraint", value, seg.value)
		}
		b.WriteString(value)
	}
	return b.String(), consumed, nil
}

// validateHostLabel rejects values that could not be one host label.
func validateHostLabel(value string) error {
	if value == "" {
		return fmt.Errorf("empty host label")
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '-', c == '_':
		default:
			return fmt.Errorf("invalid host label %q: only letters, digits, hyphens and underscores are allowed", value)
		}
	}
	return nil
}
