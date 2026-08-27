package host

import (
	"net"
	"strings"
)

// NormalizeRequest lowercases a request host, strips an explicit port, and
// trims a trailing dot.
func NormalizeRequest(value string) string {
	if value == "" {
		return ""
	}
	value = strings.ToLower(value)
	// Only attempt port stripping when a colon is present: a portless host is
	// the common case, and net.SplitHostPort allocates an error for it.
	if strings.IndexByte(value, ':') >= 0 {
		if host, _, err := net.SplitHostPort(value); err == nil {
			value = host
		}
	}
	return strings.TrimSuffix(value, ".")
}

// PatternHasPort reports whether a host pattern contains a port separator.
// Colons inside braces, such as "{name:regex}", are treated as part of the
// pattern, and colons inside brackets, such as an IPv6 literal "[::1]", are
// treated as part of the address — neither counts as a port separator.
func PatternHasPort(pattern string) bool {
	depth := 0
	inBracket := false
	for i := range len(pattern) {
		switch pattern[i] {
		case '{':
			depth++
		case '}':
			depth--
		case '[':
			inBracket = true
		case ']':
			inBracket = false
		case ':':
			if depth == 0 && !inBracket {
				return true
			}
		}
	}
	return false
}
