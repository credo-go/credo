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
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	}
	return strings.TrimSuffix(value, ".")
}

// PatternHasPort reports whether a host pattern contains a port separator.
// Colons inside braces, such as "{name:regex}", are treated as part of the
// pattern rather than as port separators.
func PatternHasPort(pattern string) bool {
	depth := 0
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '{':
			depth++
		case '}':
			depth--
		case ':':
			if depth == 0 {
				return true
			}
		}
	}
	return false
}
