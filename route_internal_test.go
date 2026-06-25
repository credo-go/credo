package credo

import (
	"maps"
	"reflect"
	"testing"
)

// assertMeta compares two metadata maps. Both must hold only comparable
// values (the table cases below use primitives), so maps.Equal is safe.
func assertMeta(t *testing.T, got, want map[string]any) {
	t.Helper()
	if !maps.Equal(got, want) {
		t.Errorf("resolveAllMeta() = %v, want %v", got, want)
	}
}

func TestRoute_resolveAllMeta(t *testing.T) {
	t.Run("nil when no meta anywhere", func(t *testing.T) {
		r := &Route{parent: &Group{}}
		if got := r.resolveAllMeta(); got != nil {
			t.Errorf("resolveAllMeta() = %v, want nil", got)
		}
	})

	t.Run("route-only meta", func(t *testing.T) {
		r := &Route{
			parent: &Group{},
			meta:   map[string]any{"auth": true},
		}
		assertMeta(t, r.resolveAllMeta(), map[string]any{"auth": true})
	})

	t.Run("group-only meta inherited by route", func(t *testing.T) {
		g := &Group{meta: map[string]any{"group": "x"}}
		r := &Route{parent: g}
		assertMeta(t, r.resolveAllMeta(), map[string]any{"group": "x"})
	})

	t.Run("route meta overrides group meta", func(t *testing.T) {
		g := &Group{meta: map[string]any{"k": "group", "g": 1}}
		r := &Route{parent: g, meta: map[string]any{"k": "route"}}
		assertMeta(t, r.resolveAllMeta(), map[string]any{"k": "route", "g": 1})
	})

	t.Run("nearest group wins across nested groups", func(t *testing.T) {
		root := &Group{meta: map[string]any{"k": "root", "r": true}}
		mid := &Group{parent: root, meta: map[string]any{"k": "mid", "m": true}}
		leaf := &Group{parent: mid, meta: map[string]any{"k": "leaf"}}
		r := &Route{parent: leaf}
		assertMeta(t, r.resolveAllMeta(), map[string]any{"k": "leaf", "m": true, "r": true})
	})

	t.Run("route wins over all nested groups", func(t *testing.T) {
		root := &Group{meta: map[string]any{"k": "root"}}
		mid := &Group{parent: root, meta: map[string]any{"k": "mid"}}
		r := &Route{parent: mid, meta: map[string]any{"k": "route"}}
		assertMeta(t, r.resolveAllMeta(), map[string]any{"k": "route"})
	})

	t.Run("root-level meta inherited several levels down", func(t *testing.T) {
		// App.SetMeta writes to the root group; a route many levels down inherits it.
		root := &Group{meta: map[string]any{"app": "root"}}
		mid := &Group{parent: root}
		leaf := &Group{parent: mid}
		r := &Route{parent: leaf}
		assertMeta(t, r.resolveAllMeta(), map[string]any{"app": "root"})
	})

	t.Run("empty non-nil meta maps yield nil", func(t *testing.T) {
		// e.g. RemoveMeta deleted the last key, leaving an empty (non-nil) map.
		g := &Group{meta: map[string]any{}}
		r := &Route{parent: g, meta: map[string]any{}}
		if got := r.resolveAllMeta(); got != nil {
			t.Errorf("resolveAllMeta() = %v, want nil for empty maps", got)
		}
	})

	t.Run("no parent group", func(t *testing.T) {
		// Defensive: a route with no parent chain still resolves its own meta.
		r := &Route{meta: map[string]any{"solo": 1}}
		assertMeta(t, r.resolveAllMeta(), map[string]any{"solo": 1})
	})
}

// TestRoute_resolveAllMeta_MatchesLookupMeta asserts the invariant that
// resolveAllMeta and LookupMeta agree: every key in the resolved map is found
// by LookupMeta, with the same value for comparable values (slices/maps/
// pointers are checked for presence and shape only — a generalized == would
// panic on non-comparable dynamic types).
func TestRoute_resolveAllMeta_MatchesLookupMeta(t *testing.T) {
	root := &Group{meta: map[string]any{"a": "root-a", "shared": "root"}}
	mid := &Group{parent: root, meta: map[string]any{"b": 2, "shared": "mid"}}
	r := &Route{
		parent: mid,
		meta:   map[string]any{"c": true, "shared": "route", "perms": []string{"read", "write"}},
	}

	resolved := r.resolveAllMeta()

	for k, v := range resolved {
		lv, ok := r.LookupMeta(k)
		if !ok {
			t.Errorf("key %q present in resolveAllMeta but missing from LookupMeta", k)
			continue
		}
		if isComparable(v) && lv != v {
			t.Errorf("key %q: resolveAllMeta=%v, LookupMeta=%v", k, v, lv)
		}
	}

	// Nearest scope wins: route overrides mid overrides root for "shared".
	if resolved["shared"] != "route" {
		t.Errorf("shared = %v, want %q", resolved["shared"], "route")
	}

	// Non-comparable value survives with correct shape and identity.
	perms, ok := resolved["perms"].([]string)
	if !ok || len(perms) != 2 || perms[0] != "read" || perms[1] != "write" {
		t.Errorf("perms = %v, want []string{read write}", resolved["perms"])
	}
}

// isComparable reports whether v can be compared with == without panicking.
func isComparable(v any) bool {
	return v == nil || reflect.TypeOf(v).Comparable()
}

// TestRoute_resolveAllMeta_RealApp exercises the genuine wiring path that Faz 3
// introspection will rely on: App.SetMeta (root group), Group.SetMeta, and
// Route.SetMeta all feed the parent chain resolveAllMeta walks.
func TestRoute_resolveAllMeta_RealApp(t *testing.T) {
	app, err := New()
	if err != nil {
		t.Fatal(err)
	}

	app.SetMeta("app", "A")        // root group
	g := app.Group("/api")         // child of root
	g.SetMeta("group", "G")        // group scope
	g.SetMeta("app", "G-override") // nearer scope overrides root

	r := g.GET("/users", func(ctx *Context) error { return nil })
	r.SetMeta("route", "R")

	assertMeta(t, r.resolveAllMeta(), map[string]any{
		"app":   "G-override",
		"group": "G",
		"route": "R",
	})
}
