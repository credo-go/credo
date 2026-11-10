package middleware

// resolveConfig returns the config a constructor works with: the caller's, or
// the zero value when none is passed, run through normalize. The package has
// one rule for a config struct, and normalize is where each constructor
// applies it: a zero field selects its default, so a config that sets one
// field keeps the defaults of the others, and passing no config is the same
// as passing the zero value. Turning a default off takes an explicit value.
func resolveConfig[C any](cfg []C, normalize func(C) C) C {
	var c C
	if len(cfg) > 0 {
		c = cfg[0]
	}
	return normalize(c)
}
