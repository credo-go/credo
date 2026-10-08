package credo_test

import (
	"slices"
	"sync"
	"testing"
)

// The lifecycle acceptance scenarios pin the component contract (ADR-024).
// A scenario written before the work item that makes it pass builds only with
// that work item's tag — pending_w6 for the drain through a worker — and the
// work item removes the tag when the scenario passes.

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
