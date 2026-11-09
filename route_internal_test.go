package credo

import (
	"maps"
	"reflect"
	"testing"
)

// metaIDs names the key identities of the table cases below, so a resolved
// map can be compared by name.
var metaIDs = map[string]*keyID{}

func metaID(name string) *keyID {
	if id, ok := metaIDs[name]; ok {
		return id
	}
	id := &keyID{name: name}
	metaIDs[name] = id
	return id
}

// metaOf builds a meta table from name/value pairs.
func metaOf(kv ...any) map[*keyID]any {
	m := make(map[*keyID]any, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		m[metaID(kv[i].(string))] = kv[i+1]
	}
	return m
}

// byName turns a resolved meta table into a name-keyed map.
func byName(m map[*keyID]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for id, v := range m {
		out[id.name] = v
	}
	return out
}

// assertMeta compares a resolved table with name/value pairs. The table cases
// hold only comparable values (primitives), so maps.Equal is safe.
func assertMeta(t *testing.T, got map[*keyID]any, want map[string]any) {
	t.Helper()
	if g := byName(got); !maps.Equal(g, want) {
		t.Errorf("resolveAllMeta() = %v, want %v", g, want)
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
		r := &Route{parent: &Group{}, meta: metaOf("auth", true)}
		assertMeta(t, r.resolveAllMeta(), map[string]any{"auth": true})
	})

	t.Run("group-only meta inherited by route", func(t *testing.T) {
		g := &Group{meta: metaOf("group", "x")}
		r := &Route{parent: g}
		assertMeta(t, r.resolveAllMeta(), map[string]any{"group": "x"})
	})

	t.Run("route meta overrides group meta", func(t *testing.T) {
		g := &Group{meta: metaOf("k", "group", "g", 1)}
		r := &Route{parent: g, meta: metaOf("k", "route")}
		assertMeta(t, r.resolveAllMeta(), map[string]any{"k": "route", "g": 1})
	})

	t.Run("nearest group wins across nested groups", func(t *testing.T) {
		root := &Group{meta: metaOf("k", "root", "r", true)}
		mid := &Group{parent: root, meta: metaOf("k", "mid", "m", true)}
		leaf := &Group{parent: mid, meta: metaOf("k", "leaf")}
		r := &Route{parent: leaf}
		assertMeta(t, r.resolveAllMeta(), map[string]any{"k": "leaf", "m": true, "r": true})
	})

	t.Run("route wins over all nested groups", func(t *testing.T) {
		root := &Group{meta: metaOf("k", "root")}
		mid := &Group{parent: root, meta: metaOf("k", "mid")}
		r := &Route{parent: mid, meta: metaOf("k", "route")}
		assertMeta(t, r.resolveAllMeta(), map[string]any{"k": "route"})
	})

	t.Run("root-level meta inherited several levels down", func(t *testing.T) {
		// App.SetMeta writes to the root group; a route many levels down inherits it.
		root := &Group{meta: metaOf("app", "root")}
		mid := &Group{parent: root}
		leaf := &Group{parent: mid}
		r := &Route{parent: leaf}
		assertMeta(t, r.resolveAllMeta(), map[string]any{"app": "root"})
	})

	t.Run("empty non-nil meta maps yield nil", func(t *testing.T) {
		// e.g. RemoveMeta deleted the last key, leaving an empty (non-nil) map.
		g := &Group{meta: metaOf()}
		r := &Route{parent: g, meta: metaOf()}
		if got := r.resolveAllMeta(); got != nil {
			t.Errorf("resolveAllMeta() = %v, want nil for empty maps", got)
		}
	})

	t.Run("no parent group", func(t *testing.T) {
		// Defensive: a route with no parent chain still resolves its own meta.
		r := &Route{meta: metaOf("solo", 1)}
		assertMeta(t, r.resolveAllMeta(), map[string]any{"solo": 1})
	})
}

// TestRoute_resolveAllMeta_MatchesLookup asserts the invariant that the
// snapshot and the walk it replaces agree: every key in the resolved map is
// found by lookupMeta, with the same value for comparable values (slices/maps/
// pointers are checked for presence and shape only — a generalized == would
// panic on non-comparable dynamic types), and no key the walk finds is
// missing from the snapshot.
func TestRoute_resolveAllMeta_MatchesLookup(t *testing.T) {
	root := &Group{meta: metaOf("a", "root-a", "shared", "root")}
	mid := &Group{parent: root, meta: metaOf("b", 2, "shared", "mid")}
	r := &Route{
		parent: mid,
		meta:   metaOf("c", true, "shared", "route", "perms", []string{"read", "write"}),
	}

	resolved := r.resolveAllMeta()

	for id, v := range resolved {
		lv, ok := r.lookupMeta(id)
		if !ok {
			t.Errorf("key %q present in resolveAllMeta but missing from lookupMeta", id.name)
			continue
		}
		if isComparable(v) && lv != v {
			t.Errorf("key %q: resolveAllMeta=%v, lookupMeta=%v", id.name, v, lv)
		}
	}
	for _, name := range []string{"a", "b", "c", "shared", "perms"} {
		if _, ok := resolved[metaID(name)]; !ok {
			t.Errorf("key %q found by lookupMeta but missing from the snapshot", name)
		}
	}

	// Nearest scope wins: route overrides mid overrides root for "shared".
	if got := resolved[metaID("shared")]; got != "route" {
		t.Errorf("shared = %v, want %q", got, "route")
	}

	// Non-comparable value survives with correct shape and identity.
	perms, ok := resolved[metaID("perms")].([]string)
	if !ok || len(perms) != 2 || perms[0] != "read" || perms[1] != "write" {
		t.Errorf("perms = %v, want []string{read write}", resolved[metaID("perms")])
	}
}

// isComparable reports whether v can be compared with == without panicking.
func isComparable(v any) bool {
	return v == nil || reflect.TypeOf(v).Comparable()
}

// TestRoute_resolveAllMeta_RealApp exercises the genuine wiring path:
// App.SetMeta (root group), Group.SetMeta, and Route.SetMeta all feed the
// parent chain resolveAllMeta walks.
func TestRoute_resolveAllMeta_RealApp(t *testing.T) {
	app, err := New()
	if err != nil {
		t.Fatal(err)
	}
	appKey := NewKey[string]("app")
	groupKey := NewKey[string]("group")
	routeKey := NewKey[string]("route")

	app.SetMeta(appKey, "A") // root group
	g := app.Group("/api")   // child of root
	g.SetMeta(groupKey, "G").SetMeta(appKey, "G-override")
	r := g.GET("/users", func(*Context) error { return nil }).SetMeta(routeKey, "R")

	assertMeta(t, r.resolveAllMeta(), map[string]any{
		"app":   "G-override",
		"group": "G",
		"route": "R",
	})
}
