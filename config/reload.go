package config

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
)

// ReloadResult reports what a reload did: the keys it changed, the live
// sections it replaced, the keys whose change takes a restart and the file
// keys a later source keeps. It names keys, never values, so it is safe to log
// and to return to an operator; its fields encode to JSON in snake_case.
type ReloadResult struct {
	// Published reports whether the reload published its snapshot. It is false
	// when the sources failed to load or a live section failed to decode or
	// validate, in which case every live section keeps its value.
	Published bool `json:"published"`

	// Aborted says why nothing was published, naming the live sections that
	// failed and their errors; empty when Published is true.
	Aborted string `json:"aborted,omitempty"`

	// Changed lists the leaf keys whose value the reload changed, relative to
	// the snapshot current before it, in sorted order.
	Changed []string `json:"changed"`

	// Replaced lists the keys of the live sections whose value the reload
	// replaced, in the order the sections were declared live.
	Replaced []string `json:"replaced"`

	// RestartRequired lists, in sorted order, the leaf keys whose value
	// differs from the one the start read and that no live section covers,
	// a key of a section bound through Decode included: their change takes a
	// restart. A reload that changes nothing keeps reporting them, and one
	// that reverts a key to its start value clears it.
	RestartRequired []string `json:"restart_required"`

	// Overridden lists the keys whose file value differs from the one the
	// start read while a later source, .env or the process environment, sets
	// the key and so keeps its value.
	Overridden []OverriddenKey `json:"overridden"`
}

// OverriddenKey is a file key whose change a later source hides.
type OverriddenKey struct {
	// Key is the dotted key path whose file value changed.
	Key string `json:"key"`
	// Source names the later source that keeps the key's value: the .env
	// entry or the process environment variable.
	Source string `json:"source"`
}

// Reload re-reads every source the Config was built from — the same files
// (or the same embedded document for [LoadBytes]), the same .env resolution
// and the process environment — and applies the result to its live sections
// (ADR-020). The steps are:
//
//  1. Re-read the sources into a candidate. A load error aborts the reload.
//  2. Decode and validate, against the candidate, every live section whose
//     keys the candidate changes. One failure aborts the reload: nothing is
//     published, every live section keeps its value, and the result's
//     Aborted names each failed section and its error.
//  3. Publish the candidate and the changed sections' values as one
//     generation: once a [Live.Load] returns this reload's value for one
//     section, every later Load returns this reload's values or newer ones.
//     No other value changes: a section read through [Section.Decode] keeps
//     its snapshot.
//  4. Run the [Live.OnChange] callbacks of the changed sections in
//     registration order, with ctx. An error or a recovered panic is joined
//     into the returned error, the later callbacks still run, and nothing
//     rolls back. No callback is skipped when ctx ends.
//  5. Return the result, whose RestartRequired lists every key whose value
//     differs from the start's and that no live section covers.
//
// The effective CREDO_ENV is fixed at the first load: Reload does not switch
// environments, because that would swap entire file sets under sections that
// were never designed to change at runtime. Concurrent calls are serialized,
// callbacks included.
func (c *Config) Reload(ctx context.Context) (ReloadResult, error) {
	if ctx == nil {
		return ReloadResult{}, errors.New("config: Reload: nil context")
	}
	if !c.initialized() {
		return ReloadResult{}, errors.New("config: Reload: instance not initialized; build the Config with config.Load or config.LoadBytes")
	}
	st := &c.live
	st.reloadMu.Lock()
	defer st.reloadMu.Unlock()

	st.mu.Lock()
	fresh, err := c.reread()
	if err != nil {
		st.mu.Unlock()
		err = fmt.Errorf("config: reload: %w; nothing was published", err)
		return ReloadResult{Aborted: err.Error()}, err
	}

	c.mu.RLock()
	changed := diffTrees(c.data, fresh.data)
	c.mu.RUnlock()

	prev := st.gen.Load()
	var (
		vals     []any
		replaced []int
		failed   []error
	)
	if prev != nil {
		vals = slices.Clone(prev.vals)
	}
	for i, e := range st.entries {
		if !affects(changed, e.key) {
			continue
		}
		v, err := e.decode(fresh)
		if err != nil {
			failed = append(failed, fmt.Errorf("live section %q: %w", e.key, err))
			continue
		}
		vals[i] = v
		replaced = append(replaced, i)
	}
	if len(failed) > 0 {
		st.mu.Unlock()
		err := fmt.Errorf("config: reload: %w; nothing was published, and every live section keeps its value",
			errors.Join(failed...))
		return ReloadResult{Aborted: err.Error(), Changed: changed}, err
	}

	c.mu.Lock()
	c.data, c.files, c.overrides = fresh.data, fresh.files, fresh.overrides
	if prev != nil {
		st.gen.Store(&generation{vals: vals})
	}
	c.mu.Unlock()

	result := ReloadResult{Published: true, Changed: changed, Replaced: []string{}}
	for _, i := range replaced {
		result.Replaced = append(result.Replaced, st.entries[i].key)
	}
	result.RestartRequired = []string{}
	for _, k := range diffTrees(c.start, fresh.data) {
		if !st.liveCovers(k) {
			result.RestartRequired = append(result.RestartRequired, k)
		}
	}
	result.Overridden = overridden(c.startFiles, fresh.files, fresh.overrides)

	var callbacks []liveCallback
	for _, cb := range st.callbacks {
		if slices.Contains(replaced, cb.index) {
			callbacks = append(callbacks, cb)
		}
	}
	keys := make([]string, len(st.entries))
	for i, e := range st.entries {
		keys[i] = e.key
	}
	st.mu.Unlock()

	var cbErrs []error
	for _, cb := range callbacks {
		if err := runCallback(ctx, cb, prev.vals[cb.index], vals[cb.index]); err != nil {
			cbErrs = append(cbErrs, fmt.Errorf("config: reload: OnChange callback of live section %q: %w", keys[cb.index], err))
		}
	}
	return result, errors.Join(cbErrs...)
}

// runCallback runs one OnChange callback, converting a panic into an error.
func runCallback(ctx context.Context, cb liveCallback, prev, next any) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("panic: %v", rec)
		}
	}()
	return cb.run(ctx, prev, next)
}

// reread builds a candidate from the recorded sources, with the same options.
func (c *Config) reread() (*Config, error) {
	fresh := &Config{configState: &configState{data: make(map[string]any), opts: c.opts, src: c.src}}
	dotenv, err := fresh.readDotenv()
	if err != nil {
		return nil, fmt.Errorf("load .env: %w", err)
	}
	if err := fresh.populate(dotenv); err != nil {
		return nil, err
	}
	return fresh, nil
}

// affects reports whether a changed leaf key lies under the section key, or
// the section lies under a changed leaf (a section removed or replaced by a
// scalar).
func affects(changed []string, key string) bool {
	for _, k := range changed {
		if covers(key, k) || covers(k, key) {
			return true
		}
	}
	return false
}

// overridden returns the file keys whose value differs from the start's while
// a later source sets the key, or a key containing it, with that source.
func overridden(startFiles, files map[string]any, overrides map[string]string) []OverriddenKey {
	out := []OverriddenKey{}
	for _, k := range diffTrees(startFiles, files) {
		for ok, source := range overrides {
			if covers(ok, k) {
				out = append(out, OverriddenKey{Key: k, Source: source})
				break
			}
		}
	}
	return out
}

// diffTrees returns the sorted symmetric difference of the leaf key paths of
// two trees: keys present in only one of them, plus keys whose values differ.
func diffTrees(previous, next map[string]any) []string {
	before := flatten(previous)
	after := flatten(next)
	keys := []string{}
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
	return keys
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
