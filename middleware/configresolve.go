package middleware

// resolveConfig returns the caller-supplied config (or the package default when
// none is given), run through normalize. It collapses the
// default-or-override-then-normalize boilerplate that every middleware
// constructor taking an optional Config would otherwise repeat.
func resolveConfig[C any](cfg []C, def C, normalize func(C) C) C {
	c := def
	if len(cfg) > 0 {
		c = cfg[0]
	}
	return normalize(c)
}
