package di

// ReadinessEntry is the one readiness entry of a resource: the Ready of its
// owner, under the owner's name, with its credo.OptionalReadiness() option.
type ReadinessEntry struct {
	// Unit is the owner whose Ready is asked.
	Unit *Unit
	// Name is the owner's name: its credo.ComponentName name, else its type.
	Name string
	// Optional reports the entry without letting it gate readiness.
	Optional bool
	// Value is the owner's built value.
	Value any
}

// ReadinessEntries returns one entry per built unit whose type shows Ready
// and on whose value the App calls it (RunsCapabilities), in registration
// order. Those are owners alone: a view and a holder that claims nothing
// beside an owner plan no entry, so a resource that several registrations
// hold has the one entry of its owner.
func (c *Container) ReadinessEntries() []ReadinessEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var entries []ReadinessEntry
	for _, u := range c.units {
		v := c.valueOf(u)
		if v == nil || !u.readies || !u.RunsCapabilities(v) {
			continue
		}
		entries = append(entries, ReadinessEntry{Unit: u, Name: u.name, Optional: u.optional, Value: v})
	}
	return entries
}
