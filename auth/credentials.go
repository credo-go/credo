package auth

import (
	"net/http"
	"strings"
)

func extractHeaderCredential(r *http.Request, header, prefix string) (string, bool) {
	if header == "" {
		return "", false
	}

	for _, value := range r.Header.Values(header) {
		if credential, ok := trimCredentialPrefix(value, prefix); ok {
			return credential, true
		}
	}

	return "", false
}

func extractQueryCredential(r *http.Request, query string) (string, bool) {
	if query == "" {
		return "", false
	}

	if credential := strings.TrimSpace(r.URL.Query().Get(query)); credential != "" {
		return credential, true
	}

	return "", false
}

func extractCookieCredential(r *http.Request, cookie string) (string, bool) {
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

func trimCredentialPrefix(value, prefix string) (string, bool) {
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
