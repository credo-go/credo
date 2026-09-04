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

// IsIdentityContentCoding reports whether every token of a Content-Encoding
// value is "identity" (or the value is empty), meaning the body carries no
// transformation.
func IsIdentityContentCoding(value string) bool {
	for token := range strings.SplitSeq(value, ",") {
		if t := strings.ToLower(strings.TrimSpace(token)); t != "" && t != "identity" {
			return false
		}
	}
	return true
}

// AddToken appends token to the comma-separated header unless it is
// already present.
func AddToken(h http.Header, key, token string) {
	if HasToken(h, key, token) {
		return
	}
	h.Add(key, token)
}
