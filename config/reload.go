package config

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// Reloader is implemented by configuration stores that can re-read their
// sources at runtime. *Config implements it; a custom RawConfig opts into the
// App-level reload (see the credo package and ADR-020) by implementing it too.
//
// Reload must be atomic from a reader's point of view — after it returns, a
// concurrent Unmarshal or Exists sees either the previous or the new snapshot,
// never a mix — and must leave the previous snapshot untouched on error.
type Reloader interface {
	Reload() (Changes, error)
}

// Compile-time interface satisfaction check.
var _ Reloader = (*Config)(nil)

// Changes is the set of leaf key paths whose value differs between two
// configuration snapshots — added, removed, or changed. It records key paths
// only, never values, so it is safe to log.
type Changes struct {
	keys []string // sorted, deduplicated dotted leaf paths
}

// Affects reports whether any changed key equals prefix or lies under it
// ("server" matches "server.port"). The empty prefix matches any change.
func (c Changes) Affects(prefix string) bool {
	if prefix == "" {
		return len(c.keys) > 0
	}
	for _, k := range c.keys {
		if k == prefix || strings.HasPrefix(k, prefix+keyDelim) {
			return true
		}
	}
	return false
}

// Keys returns a copy of the changed leaf key paths in sorted order.
func (c Changes) Keys() []string {
	return slices.Clone(c.keys)
}

// Empty reports whether nothing changed.
func (c Changes) Empty() bool {
	return len(c.keys) == 0
}

// Reload re-reads every source the Config was created from — the same files
// (or the same embedded document for [LoadBytes]), the same .env resolution,
// and the process environment — builds a fresh tree, and swaps it in
// atomically. It returns the leaf keys whose values differ from the previous
// snapshot.
//
// The effective CREDO_ENV is fixed at the first load: Reload does not switch
// environments, because that would swap entire file sets under sections that
// were never designed to change at runtime (ADR-020).
//
// On any read or parse error the previous snapshot stays current and the error
// is returned. Reload is safe to call concurrently with readers; concurrent
// Reload calls are serialized by the caller (the App does this), and the last
// one to finish wins.
func (c *Config) Reload() (Changes, error) {
	if c == nil || c.data == nil {
		return Changes{}, fmt.Errorf("config: instance not initialized")
	}
	fresh := &Config{data: make(map[string]any), opts: c.opts, src: c.src}
	dotenv, err := fresh.readDotenv()
	if err != nil {
		return Changes{}, fmt.Errorf("config: reload: load .env: %w", err)
	}
	if err := fresh.populate(dotenv); err != nil {
		return Changes{}, fmt.Errorf("config: reload: %w", err)
	}

	c.mu.Lock()
	previous := c.data
	c.data = fresh.data
	c.mu.Unlock()

	return diffTrees(previous, fresh.data), nil
}

// diffTrees returns the sorted symmetric difference of the leaf key paths of
// two trees: keys present in only one of them, plus keys whose values differ.
func diffTrees(previous, next map[string]any) Changes {
	before := flatten(previous)
	after := flatten(next)
	var keys []string
	for k, v := range before {
		if nv, ok := after[k]; !ok || !reflect.DeepEqual(v, nv) {
			keys = append(keys, k)
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return Changes{keys: keys}
}

// flatten maps every leaf of a nested tree to its dotted key path. Nested
// maps recurse; an empty map is itself a leaf so that an emptied or removed
// section still registers as a change.
func flatten(m map[string]any) map[string]any {
	out := make(map[string]any)
	flattenInto(out, "", m)
	return out
}

func flattenInto(out map[string]any, prefix string, m map[string]any) {
	for k, v := range m {
		path := k
		if prefix != "" {
			path = prefix + keyDelim + k
		}
		if sub, ok := v.(map[string]any); ok && len(sub) > 0 {
			flattenInto(out, path, sub)
			continue
		}
		out[path] = v
	}
}
