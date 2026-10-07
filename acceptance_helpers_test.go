//go:build pending_w4 || pending_w6

package credo_test

import (
	"slices"
	"sync"
	"testing"
)

// The lifecycle acceptance scenarios are written against the v0.24.0
// component contract (ADR-024) before it is implemented, so they build only
// with the tag of the work item that makes them pass: pending_w4 for the
// failed start, pending_w6 for the drain through a worker. Each work item
// removes its tag when the scenario passes.

// acceptanceLog records lifecycle events in the order they happen.
type acceptanceLog struct {
	mu     sync.Mutex
	events []string
}

func (l *acceptanceLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *acceptanceLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

// assertBefore fails unless both events were recorded and first came before
// second.
func assertBefore(t *testing.T, events []string, first, second string) {
	t.Helper()
	i, j := slices.Index(events, first), slices.Index(events, second)
	if i < 0 || j < 0 || i > j {
		t.Errorf("want %q before %q; events: %q", first, second, events)
	}
}
