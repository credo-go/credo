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

// ReadinessEntries returns one entry per built resource that a holder taking
// part in it plans an entry for: the first registration of the resource —
// a binding, or a value handed to Manage — whose type shows Ready, in
// registration order. A value a constructor that is not a binding builds
// defers to such a registration and adds no entry of its own; it gives the
// entry only of a resource no registration holds, as its holder of record.
// The holders that take part agree on the options that shape the entry, so
// the chosen one's are the resource's. A unit whose value carries no
// identity is a resource of its own.
func (c *Container) ReadinessEntries() []ReadinessEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	groups := make(map[any][]*Unit)
	var keys []any
	for _, u := range c.units {
		if c.valueOf(u) == nil {
			continue
		}
		key := resourceKey(u)
		if _, seen := groups[key]; !seen {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], u)
	}
	var entries []ReadinessEntry
	for _, key := range keys {
		holders := groups[key]
		for _, u := range holders {
			v := c.valueOf(u)
			if !takesPart(u, holders) || !u.readies || !u.RunsCapabilities(v) {
				continue
			}
			entries = append(entries, ReadinessEntry{Unit: u, Name: u.name, Optional: u.optional, Value: v})
			break
		}
	}
	return entries
}
