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
//
// A store that also implements [Stager] lets the App validate the new
// snapshot before it is published; a Reloader-only store is reloaded first and
// validated afterwards, so a bad value reaches readers before it is reported.
type Reloader interface {
	Reload() (Changes, error)
}

// Stager is the two-phase form of [Reloader]: Stage re-reads the sources into
// a candidate snapshot that can be inspected through the [Staged] handle
// without affecting readers, and Staged.Commit publishes it. The App uses this
// form when available so that typed subscribers are decoded and validated
// against the candidate and a failure leaves the previous snapshot current.
//
// *Config implements Stager; [Config.Reload] is Stage followed by Commit.
type Stager interface {
	Stage() (Staged, error)
}

// Staged is a candidate snapshot produced by [Stager.Stage]. Its RawConfig
// methods read the candidate, Changes reports how it differs from the snapshot
// that was current when it was staged, and Commit publishes it atomically.
// A Staged that is never committed is simply discarded.
type Staged interface {
	RawConfig
	Changes() Changes
	Commit()
}

// Compile-time interface satisfaction checks.
var (
	_ Reloader = (*Config)(nil)
	_ Stager   = (*Config)(nil)
	_ Staged   = (*staged)(nil)
)

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
	s, err := c.Stage()
	if err != nil {
		return Changes{}, err
	}
	s.Commit()
	return s.Changes(), nil
}

// Stage re-reads every source exactly as [Config.Reload] does, but returns the
// candidate as a [Staged] handle instead of publishing it. Readers keep seeing
// the current snapshot until Commit. Changes are computed against the snapshot
// current at Stage time; callers that stage concurrently must serialize
// themselves (the App does), as the last Commit wins.
func (c *Config) Stage() (Staged, error) {
	if !c.initialized() {
		return nil, fmt.Errorf("config: instance not initialized")
	}
	fresh := &Config{configState: &configState{data: make(map[string]any), opts: c.opts, src: c.src}}
	dotenv, err := fresh.readDotenv()
	if err != nil {
		return nil, fmt.Errorf("config: reload: load .env: %w", err)
	}
	if err := fresh.populate(dotenv); err != nil {
		return nil, fmt.Errorf("config: reload: %w", err)
	}

	c.mu.RLock()
	changes := diffTrees(c.data, fresh.data)
	c.mu.RUnlock()

	return &staged{parent: c, fresh: fresh, changes: changes}, nil
}

// staged is the Staged handle for *Config: a fully built candidate tree plus
// the diff against the parent's snapshot at Stage time.
type staged struct {
	parent  *Config
	fresh   *Config
	changes Changes
}

// Unmarshal decodes from the candidate snapshot.
func (s *staged) Unmarshal(key string, dst any) error { return s.fresh.Unmarshal(key, dst) }

// Exists reports presence in the candidate snapshot.
func (s *staged) Exists(key string) bool { return s.fresh.Exists(key) }

// Changes reports how the candidate differs from the snapshot that was current
// when it was staged.
func (s *staged) Changes() Changes { return s.changes }

// Commit publishes the candidate atomically.
func (s *staged) Commit() {
	s.parent.mu.Lock()
	s.parent.data = s.fresh.data
	s.parent.mu.Unlock()
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
