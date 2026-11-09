// Package credential extracts a credential from an HTTP request for the
// authenticators of auth and auth/jwtauth: from a header after an optional
// scheme prefix, a query parameter or a cookie.
package credential

import (
	"net/http"
	"strings"
)

// FromHeader returns the first value of header that carries a credential
// after prefix: the prefix matches case-insensitively and must be followed by
// a space or tab, every value of a repeated header is tried, and surrounding
// whitespace is trimmed. An empty prefix takes the whole trimmed value; an
// empty header name finds nothing.
func FromHeader(r *http.Request, header, prefix string) (string, bool) {
	if header == "" {
		return "", false
	}

	for _, value := range r.Header.Values(header) {
		if credential, ok := trimPrefix(value, prefix); ok {
			return credential, true
		}
	}

	return "", false
}

// FromQuery returns the trimmed value of the query parameter query, when it
// is not empty; an empty name finds nothing.
func FromQuery(r *http.Request, query string) (string, bool) {
	if query == "" {
		return "", false
	}

	if credential := strings.TrimSpace(r.URL.Query().Get(query)); credential != "" {
		return credential, true
	}

	return "", false
}

// FromCookie returns the trimmed value of the cookie named cookie, when it is
// not empty; an empty name finds nothing.
func FromCookie(r *http.Request, cookie string) (string, bool) {
	if cookie == "" {
		return "", false
	}

	c, err := r.Cookie(cookie)
	if err != nil {
		return "", false
	}

	if credential := strings.TrimSpace(c.Value); credential != "" {
		return credential, true
	}

	return "", false
}

func trimPrefix(value, prefix string) (string, bool) {
	v := strings.TrimSpace(value)
	if v == "" {
		return "", false
	}

	p := strings.TrimSpace(prefix)
	if p == "" {
		return v, true
	}

	if len(v) <= len(p) || !strings.EqualFold(v[:len(p)], p) {
		return "", false
	}

	sep := v[len(p)]
	if sep != ' ' && sep != '\t' {
		return "", false
	}

	credential := strings.TrimSpace(v[len(p):])
	if credential == "" {
		return "", false
	}

	return credential, true
}
