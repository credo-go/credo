package config

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
)

// Live is the handle of a live section: a section whose value each reload
// that changes it replaces whole. A consumer that takes a *Live[T] says in its
// signature that the value can change:
//
//	var Limits = config.Section[LimitsConfig]("limits")
//
//	app.Provide(Limits.Live)
//
//	func NewOrderService(limits *config.Live[LimitsConfig]) *OrderService
//
// [Live.Load] returns the current value with one atomic load. The type does
// not enforce two rules, which its consumers keep:
//
//   - Load once per operation, and use that value for the whole operation; a
//     value kept past it, or read in a constructor, is the snapshot a reload
//     does not change. Values that must agree within one operation belong to
//     one section, since two Load calls can straddle a reload.
//   - Read the value's slices and maps, never write them: every Load of one
//     reload shares them.
//
// State derived from the section — a limiter, a compiled pattern, a
// slog.LevelVar — is updated in an [Live.OnChange] callback, not on each Load.
//
// A Live is created by [Section.Live] over a *Config, or by [Fixed] for a
// consumer's unit test. A Live is safe for concurrent use.
type Live[T any] struct {
	state *liveState // nil for Fixed
	index int        // the handle's slot in each generation
	fixed T          // the value of a Fixed handle
}

// Load returns the section's current value with one atomic load. Once a Load
// returns a reload's value for one section, every later Load of a live
// section of the same configuration returns that reload's values or newer
// ones.
func (l *Live[T]) Load() T {
	if l.state == nil {
		return l.fixed
	}
	return l.state.gen.Load().vals[l.index].(T)
}

// OnChange registers fn for state derived from the section. fn runs after a
// reload that changes the section has published its new value, with the
// reload's context, the previous value and the new one. Callbacks of one
// configuration run in registration order, each once per reload that changes
// its section; an error or a recovered panic is joined into the reload's
// error, the later callbacks still run, and nothing rolls back. A callback
// still runs when the reload's context has been cancelled, since the next
// reload would not see the change again: it observes the cancellation
// through ctx.
//
// A [Fixed] handle never changes, so its callbacks never run. OnChange panics
// for a nil fn.
func (l *Live[T]) OnChange(fn func(ctx context.Context, prev, next T) error) {
	if fn == nil {
		panic("config: Live.OnChange: nil callback")
	}
	if l.state == nil {
		return
	}
	l.state.mu.Lock()
	defer l.state.mu.Unlock()
	l.state.callbacks = append(l.state.callbacks, liveCallback{
		index: l.index,
		run: func(ctx context.Context, prev, next any) error {
			return fn(ctx, prev.(T), next.(T))
		},
	})
}

// Fixed returns a handle whose value is v and never changes, for a unit test
// of a consumer that takes a *Live[T]. Its [Live.OnChange] callbacks never
// run.
func Fixed[T any](v T) *Live[T] {
	return &Live[T]{fixed: v}
}

// Live decodes and validates the section from src as [Section.Decode] does
// and returns its live handle, registered with the configuration it read:
// each reload of that configuration that changes a key under the section
// replaces the handle's value whole, once every live section the reload
// changes has validated. Its method value is the constructor that binds the
// handle:
//
//	app.Provide(Limits.Live) // a *config.Live[LimitsConfig] in DI
//
// src must be a *Config built by [Load] or [LoadBytes]; any other
// [RawConfig] cannot reload, and Live fails naming its type. A section is
// static or live within one configuration: Live fails when the configuration
// has read the section, a section nested in it, or one containing it as
// static (through [Section.Decode], credo.App.GetConfig or the framework's
// own "server" section), naming both reads.
func (s Section[T]) Live(src RawConfig) (*Live[T], error) {
	if s == "" {
		return nil, fmt.Errorf("config: Section.Live: %w", errEmptyKey)
	}
	c, ok := src.(*Config)
	if !ok || !c.initialized() {
		return nil, fmt.Errorf(
			"config: %s: the configuration is a %s, which cannot reload; load it with config.Load (or config.LoadBytes) and pass it to credo.WithRawConfig to declare a section live",
			s.describe("Live"), describeSource(src))
	}
	st := &c.live
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.checkRead(string(s), true, s.describe("Live")); err != nil {
		return nil, err
	}
	v, err := s.decode(c)
	if err != nil {
		return nil, err
	}
	st.recordRead(string(s), true, s.describe("Live"))
	index := st.add(string(s), v, func(fresh *Config) (any, error) { return s.decode(fresh) })
	return &Live[T]{state: st, index: index}, nil
}

// describe names the read for diagnostics: config.Section[T]("key").method.
func (s Section[T]) describe(method string) string {
	return fmt.Sprintf("config.Section[%s](%q).%s", reflect.TypeFor[T]().String(), string(s), method)
}

// describeSource names a RawConfig's type, or nil.
func describeSource(src RawConfig) string {
	if src == nil {
		return "nil RawConfig"
	}
	if c, ok := src.(*Config); ok && !c.initialized() {
		return "*config.Config that Load did not build"
	}
	return reflect.TypeOf(src).String()
}

// liveState is the part of a Config that live sections share: the current
// generation of their values, the registered handles and callbacks, and the
// record of how each section was read.
type liveState struct {
	// reloadMu serializes Reload, callbacks included.
	reloadMu sync.Mutex
	// mu guards entries, callbacks and reads, and is held by Reload from the
	// re-read to the publish, so a handle registered meanwhile is decoded from
	// the snapshot the reload diffs against.
	mu        sync.Mutex
	gen       atomic.Pointer[generation]
	entries   []liveEntry
	callbacks []liveCallback
	reads     []sectionRead
}

// generation holds the value of every live handle as one reload published
// them, indexed by the handle's slot. A generation is never modified.
type generation struct {
	vals []any
}

// liveEntry is one registered handle: its section's key and how to decode the
// section from a reload's candidate.
type liveEntry struct {
	key    string
	decode func(fresh *Config) (any, error)
}

// liveCallback is one OnChange registration.
type liveCallback struct {
	index int
	run   func(ctx context.Context, prev, next any) error
}

// sectionRead records that a section was read through its descriptor, and
// whether as static or live.
type sectionRead struct {
	key  string
	live bool
	by   string
}

// add registers a handle with its first value and returns its slot. The
// caller holds mu.
func (st *liveState) add(key string, v any, decode func(*Config) (any, error)) int {
	var vals []any
	if g := st.gen.Load(); g != nil {
		vals = append(vals, g.vals...)
	}
	vals = append(vals, v)
	st.entries = append(st.entries, liveEntry{key: key, decode: decode})
	st.gen.Store(&generation{vals: vals})
	return len(vals) - 1
}

// checkRead fails when a read of the other nature covers key: the same
// section, one nested in it, or one containing it. The caller holds mu.
func (st *liveState) checkRead(key string, live bool, by string) error {
	for _, r := range st.reads {
		if r.live == live || (!covers(r.key, key) && !covers(key, r.key)) {
			continue
		}
		staticBy, liveBy := by, r.by
		if live {
			staticBy, liveBy = r.by, by
		}
		return fmt.Errorf(
			"config: section %q is read both as static, by %s, and as live, by %s; a section is static or live within one configuration: read it one way, and put the keys that need a restart and the keys a reload applies in separate sections",
			key, staticBy, liveBy)
	}
	return nil
}

// recordRead records a read that checkRead accepted. The caller holds mu.
func (st *liveState) recordRead(key string, live bool, by string) {
	for _, r := range st.reads {
		if r.key == key && r.live == live && r.by == by {
			return
		}
	}
	st.reads = append(st.reads, sectionRead{key: key, live: live, by: by})
}

// recordStatic records a static read of the section through its descriptor,
// failing when the configuration reads it as live.
func (c *Config) recordStatic(key, by string) error {
	st := &c.live
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.checkRead(key, false, by); err != nil {
		return err
	}
	st.recordRead(key, false, by)
	return nil
}

// liveCovers reports whether a registered live section covers key. The caller
// holds mu.
func (st *liveState) liveCovers(key string) bool {
	for _, e := range st.entries {
		if covers(e.key, key) {
			return true
		}
	}
	return false
}

// covers reports whether key equals prefix or lies under it, segment-wise.
func covers(prefix, key string) bool {
	return key == prefix || strings.HasPrefix(key, prefix+keyDelim)
}

// errEmptyKey is wrapped by the descriptor methods for an empty key.
var errEmptyKey = errors.New(`empty section key; declare the section with its dotted key, as config.Section[T]("app") does`)
