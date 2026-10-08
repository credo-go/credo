package worker

import (
	"context"
	"crypto/rand"
	"reflect"
	"time"
)

// Worker is a background task run by the framework. The name that identifies
// it is given at registration ([Supervisor.Continuous], [Supervisor.Scheduled]
// and their provided forms), not by the worker itself.
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

// RunInfo is the execution metadata of one run, carried by the context
// passed to Run.
type RunInfo struct {
	// Worker is the registration name.
	Worker string
	// ID is the run's identifier, equal to the run_id of the framework's log
	// lines for that run.
	ID string
	// ScheduledAt is the intended activation of a scheduled run; zero for
	// continuous runs and for the RunOnStart run.
	ScheduledAt time.Time
}

type runInfoKey struct{}

// CurrentRun returns the metadata of the run that ctx belongs to, and false
// with a zero RunInfo for a context that is not a run context.
func CurrentRun(ctx context.Context) (RunInfo, bool) {
	if ctx == nil {
		return RunInfo{}, false
	}
	info, ok := ctx.Value(runInfoKey{}).(RunInfo)
	return info, ok
}

func withRunInfo(parent context.Context, info RunInfo) context.Context {
	return context.WithValue(parent, runInfoKey{}, info)
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
