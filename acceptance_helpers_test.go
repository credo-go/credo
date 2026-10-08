package credo_test

import (
	"slices"
	"sync"
	"testing"
)

// The lifecycle acceptance scenarios pin the component contract (ADR-024):
// a failed start rolls back what it built, and a drain delivers the last job
// an HTTP handler hands to a worker.

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
