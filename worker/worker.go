package worker

import (
	"context"
	"crypto/rand"
	"fmt"
	"reflect"
	"time"
)

// Worker defines a background task managed by the framework.
type Worker interface {
	// Name returns the worker's unique registration name.
	Name() string
	// Run executes the worker's logic.
	Run(ctx context.Context) error
}

type funcWorker struct {
	name string
	fn   func(ctx context.Context) error
}

// Func adapts a plain function into a Worker.
func Func(name string, fn func(ctx context.Context) error) Worker {
	return &funcWorker{name: name, fn: fn}
}

func (w *funcWorker) Name() string { return w.name }

func (w *funcWorker) Run(ctx context.Context) error {
	if w.fn == nil {
		return fmt.Errorf("worker: %q has nil function", w.name)
	}
	return w.fn(ctx)
}

type workerNameKey struct{}
type attemptKey struct{}
type scheduledAtKey struct{}
type runIDKey struct{}

// RunID returns the execution identifier stored in ctx.
func RunID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if id, ok := ctx.Value(runIDKey{}).(string); ok {
		return id
	}
	return ""
}

// Attempt returns the current worker attempt stored in ctx.
func Attempt(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	if attempt, ok := ctx.Value(attemptKey{}).(int); ok {
		return attempt
	}
	return 0
}

// WorkerName returns the worker name stored in ctx.
func WorkerName(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if name, ok := ctx.Value(workerNameKey{}).(string); ok {
		return name
	}
	return ""
}

// ScheduledAt returns the intended fire time for scheduled workers.
func ScheduledAt(ctx context.Context) time.Time {
	if ctx == nil {
		return time.Time{}
	}
	if scheduledAt, ok := ctx.Value(scheduledAtKey{}).(time.Time); ok {
		return scheduledAt
	}
	return time.Time{}
}

func enrichContext(parent context.Context, name string, attempt int, scheduledAt time.Time, runID string) context.Context {
	ctx := context.WithValue(parent, workerNameKey{}, name)
	ctx = context.WithValue(ctx, attemptKey{}, attempt)
	ctx = context.WithValue(ctx, scheduledAtKey{}, scheduledAt)
	ctx = context.WithValue(ctx, runIDKey{}, runID)
	return ctx
}

func newRunID() string {
	return rand.Text()
}

func isNilWorker(w Worker) bool {
	if w == nil {
		return true
	}
	v := reflect.ValueOf(w)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
