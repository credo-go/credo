package di

import (
	"cmp"
	"slices"
)

// StartPlan returns the units of tier the start walk visits, dependencies
// first with registration order as the tie-break: every unit the App starts
// or asks for readiness, and every constructor handed to Manage, which the
// walk builds. A dependency reached through bindings that are not components
// counts as a direct one. The plan is taken from the registry as it stands;
// callers take it after Finalize.
func (c *Container) StartPlan(tier Tier) []*Unit {
	c.mu.RLock()
	defer c.mu.RUnlock()

	in := make(map[*Unit]bool)
	var plan []*Unit
	for _, u := range c.units {
		if u.Tier() != tier {
			continue
		}
		if u.starts || u.readies || (u.managed && u.entry.state != entryBuilt) {
			in[u] = true
			plan = append(plan, u)
		}
	}

	// Edges between planned units, through bindings that are not components.
	deps := make(map[*Unit][]*Unit, len(plan))
	waiting := make(map[*Unit]int, len(plan))
	dependents := make(map[*Unit][]*Unit, len(plan))
	for _, u := range plan {
		seen := map[*Unit]bool{u: true}
		var walk func(from *Unit)
		walk = func(from *Unit) {
			for _, d := range c.depUnitsLocked(from) {
				if seen[d] {
					continue
				}
				seen[d] = true
				switch {
				case in[d]:
					deps[u] = append(deps[u], d)
				case !d.component:
					walk(d)
				}
			}
		}
		walk(u)
		waiting[u] = len(deps[u])
		for _, d := range deps[u] {
			dependents[d] = append(dependents[d], u)
		}
	}

	// Kahn, the earliest registered ready unit first. Validation rejects
	// cycles, so every planned unit is placed.
	var ready, order []*Unit
	for _, u := range plan {
		if waiting[u] == 0 {
			ready = append(ready, u)
		}
	}
	for len(ready) > 0 {
		slices.SortFunc(ready, func(a, b *Unit) int { return cmp.Compare(a.index, b.index) })
		u := ready[0]
		ready = ready[1:]
		order = append(order, u)
		for _, d := range dependents[u] {
			waiting[d]--
			if waiting[d] == 0 {
				ready = append(ready, d)
			}
		}
	}
	return order
}
