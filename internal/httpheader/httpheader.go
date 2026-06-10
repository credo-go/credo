package httpheader

import (
	"net/http"
	"strings"
)

// HasToken reports whether a comma-separated header contains token.
// Comparisons are case-insensitive and ignore surrounding spaces.
func HasToken(h http.Header, key, token string) bool {
	for _, v := range h.Values(key) {
		remaining := v
		for remaining != "" {
			var part string
			part, remaining, _ = strings.Cut(remaining, ",")
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// AddToken appends token to the comma-separated header unless it is
// already present.
func AddToken(h http.Header, key, token string) {
	if HasToken(h, key, token) {
		return
	}
	h.Add(key, token)
}
