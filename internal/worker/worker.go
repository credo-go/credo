package worker

import (
	"context"
	"crypto/rand"
	"reflect"
	"time"
)

// Worker is what the engine runs: the root package's credo.Worker has the
// same method set, so its values are Workers here without conversion.
type Worker interface {
	// Run executes the worker's logic.
	Run(ctx context.Context) error
}

// RunInfo is the execution metadata of one run, carried by the context
// passed to Run. The root package converts it to credo.WorkerRun.
type RunInfo struct {
	// Worker is the registration name.
	Worker string
	// ID is the run's identifier, equal to the run_id of the engine's log
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

// IsNilWorker reports whether w is nil or an interface holding a nil value of
// a nilable kind.
func IsNilWorker(w Worker) bool {
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
