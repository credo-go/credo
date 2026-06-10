package requestid

import "crypto/rand"

const (
	// Key is the request-scoped store key used for the current request ID.
	Key = "credo.requestid"

	// Header is the default HTTP header used to read and write the request ID.
	Header = "X-Request-Id"

	// DefaultLimit is the default maximum accepted request ID length.
	DefaultLimit = 64
)

// Resolve returns the incoming request ID when it is valid, or a generated
// replacement otherwise.
func Resolve(value string, limit int, generator func() string) string {
	if limit <= 0 {
		limit = DefaultLimit
	}
	if generator == nil {
		generator = Generate
	}
	if value == "" || len(value) > limit || !IsValid(value) {
		return generator()
	}
	return value
}

// IsValid reports whether id contains only safe header/log characters.
func IsValid(id string) bool {
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.':
		default:
			return false
		}
	}
	return true
}

// Generate creates a 128-bit cryptographically random base32 request ID.
func Generate() string {
	return rand.Text()
}
