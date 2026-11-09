// Package machinecode holds the machine-code grammar that the root's
// HTTPError and fault.Error share: fault cannot import the root.
package machinecode

// Valid reports whether code satisfies Credo's machine-code grammar: one or
// more lower-snake-case segments of [a-z0-9], joined by single underscores —
// ^[a-z0-9]+(_[a-z0-9]+)*$. Dots, spaces, hyphens, uppercase, and
// leading/trailing/doubled underscores are all rejected.
func Valid(code string) bool {
	if code == "" {
		return false
	}
	previousUnderscore := true // a leading underscore is invalid
	for i := range len(code) {
		c := code[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			previousUnderscore = false
		case c == '_':
			if previousUnderscore {
				return false
			}
			previousUnderscore = true
		default:
			return false
		}
	}
	return !previousUnderscore // a trailing underscore is invalid
}
