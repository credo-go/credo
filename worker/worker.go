package worker

import (
	"context"
	"crypto/rand"
	"reflect"
	"time"
)

// Worker is a background task managed by the framework. The name that
// identifies it is given at registration ([Register], [RegisterProvided]), not
// by the worker itself.
//
// A continuous worker's Run must stay active until ctx is cancelled; a
// scheduled worker's Run performs one activation and returns.
type Worker interface {
	// Run executes the worker's logic.
	Run(ctx context.Context) error
}

// Func adapts a plain function into a Worker, in the style of
// [net/http.HandlerFunc].
type Func func(ctx context.Context) error

// Run calls f(ctx).
func (f Func) Run(ctx context.Context) error { return f(ctx) }

type workerNameKey struct{}
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

// WorkerName returns the registration name of the worker whose run ctx
// belongs to.
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

func enrichContext(parent context.Context, name string, scheduledAt time.Time, runID string) context.Context {
	ctx := context.WithValue(parent, workerNameKey{}, name)
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
