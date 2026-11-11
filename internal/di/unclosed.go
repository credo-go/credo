package di

// UnclosedBinding is a binding whose value has a Close the App does not
// call: its type shows one of the three Close shapes, it claims nothing — no
// Shutdown on its value, no credo.CloseOnShutdown(), no credo.Borrowed() —
// it is not a view, and no registration owns its resource.
type UnclosedBinding struct {
	// Name is the binding's unit name: its type as the lifecycle prints it.
	Name string
	// Registration names the binding in the application's terms: "the
	// Provide binding of T" or "the ProvideValue binding of T".
	Registration string
	// Close is the Close shape its type shows: "Close() error", "Close()" or
	// "Close(ctx) error".
	Close string
	// ValueBinding reports a ProvideValue binding, which credo.Borrowed()
	// may leave to its caller.
	ValueBinding bool
}

// UnclosedBindings returns, in registration order, the bindings whose value
// the container holds and whose Close the App never calls. It reads the
// values and the owners as they stand, so the App asks once the drain has
// begun, when both are final: a binding beside a registration that owns its
// resource is a plain holder and is not returned, whichever was built first,
// and a binding never built holds nothing open. A binding an override
// replaced is no longer a unit, so it is never returned.
func (c *Container) UnclosedBindings() []UnclosedBinding {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var unclosed []UnclosedBinding
	for _, u := range c.units {
		if u.managed || u.view {
			continue
		}
		shape := closeShape(u.t)
		if shape == closeNone {
			continue
		}
		v := c.valueOf(u)
		if v == nil || u.claims(v) {
			continue
		}
		if _, own := u.key.(*Unit); u.key != nil && !own && c.owners[u.key] != nil {
			continue
		}
		what, _ := u.registrationPhrase()
		unclosed = append(unclosed, UnclosedBinding{Name: u.Name(), Registration: what, Close: shape.String(),
			ValueBinding: u.valueBinding})
	}
	return unclosed
}

// String names the Close shape as a type shows it.
func (k closeKind) String() string {
	switch k {
	case closeErr:
		return "Close() error"
	case closeVoid:
		return "Close()"
	case closeCtx:
		return "Close(ctx) error"
	}
	return ""
}
