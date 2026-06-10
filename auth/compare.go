package auth

import (
	"crypto/sha256"
	"crypto/subtle"
)

// SecureCompare reports whether x and y are equal, in time independent of
// their contents. Use it inside [BasicValidator] and [APIKeyValidator]
// implementations when comparing a received credential against a known
// secret — a plain == comparison returns early on the first differing byte,
// leaking how much of the secret matched through response timing.
//
// Both inputs are hashed before comparison, so the timing reveals neither
// the contents nor the lengths of the inputs.
//
// SecureCompare is for comparing against plaintext secrets (API keys,
// tokens). Stored passwords should instead be verified with a dedicated
// password hash such as bcrypt or argon2id, whose comparison functions are
// already timing-safe.
func SecureCompare(x, y string) bool {
	xs := sha256.Sum256([]byte(x))
	ys := sha256.Sum256([]byte(y))
	return subtle.ConstantTimeCompare(xs[:], ys[:]) == 1
}
