package di

// ReadinessEntry is the one readiness entry of a resource: the Ready of its
// holder of record, under the resource's name, with its
// credo.OptionalReadiness() option.
type ReadinessEntry struct {
	// Unit is the holder whose Ready is asked.
	Unit *Unit
	// Name is the resource's name: a credo.ComponentName name one of its
	// registrations gives, else the holder's type.
	Name string
	// Optional reports the entry without letting it gate readiness.
	Optional bool
	// Value is the holder's built value.
	Value any
}

// ReadinessEntries returns one entry per resource whose holder plans a
// readiness entry and has been built: the first such holder in registration
// order, its holder of record. Holders of one resource agree on the options
// that shape the entry, so the first one's are the resource's. A unit whose
// value carries no identity is a resource of its own.
func (c *Container) ReadinessEntries() []ReadinessEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	seen := make(map[any]bool)
	var entries []ReadinessEntry
	for _, u := range c.units {
		if !u.readies {
			continue
		}
		v := c.valueOf(u)
		if v == nil || !u.RunsCapabilities(v) {
			continue
		}
		key := resourceKey(u)
		if seen[key] {
			continue
		}
		seen[key] = true
		entries = append(entries, ReadinessEntry{Unit: u, Name: u.name, Optional: u.optional, Value: v})
	}
	return entries
}
