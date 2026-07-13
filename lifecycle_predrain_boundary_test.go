package credo

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type delayedDeadlineContext struct {
	context.Context
	deadline time.Time
	done     <-chan struct{}
}

type messageCaptureHandler struct {
	message string
	records chan slog.Record
}

func (handler *messageCaptureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (handler *messageCaptureHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Message == handler.message {
		select {
		case handler.records <- record.Clone():
		default:
		}
	}
	return nil
}

func (handler *messageCaptureHandler) WithAttrs([]slog.Attr) slog.Handler { return handler }

func (handler *messageCaptureHandler) WithGroup(string) slog.Handler { return handler }

func (ctx delayedDeadlineContext) Deadline() (time.Time, bool) {
	return ctx.deadline, true
}

func (ctx delayedDeadlineContext) Done() <-chan struct{} {
	return ctx.done
}

func (delayedDeadlineContext) Err() error {
	return nil
}

func TestLifecycleManager_PreDrainCancellationBoundaryIsNeverMissed(t *testing.T) {
	const iterations = 256

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	app, err := New(WithLogger(logger), WithoutAccessLog())
	if err != nil {
		t.Fatal(err)
	}

	for iteration := range iterations {
		logs.Reset()
		started := make(chan struct{})
		app.lifecycle.onPreDrain = []drainHook{{
			index:  0,
			source: "boundary-test",
			fn: func(ctx context.Context) error {
				close(started)
				<-ctx.Done()
				return nil
			},
		}}

		ctx, cancel := context.WithCancel(t.Context())
		result := make(chan error, 1)
		go func() {
			result <- app.lifecycle.runPreDrainPhase(ctx)
		}()
		<-started
		cancel()

		err := <-result
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("iteration %d: pre-drain error = %v, want context.Canceled", iteration, err)
		}
		if got := strings.Count(err.Error(), "OnPreDrain hook [0]"); got != 1 {
			t.Fatalf("iteration %d: incomplete identity count = %d, want 1: %v", iteration, got, err)
		}
		if got := strings.Count(logs.String(), `"msg":"credo: drain task incomplete"`); got != 1 {
			t.Fatalf("iteration %d: incomplete log count = %d, want 1: %s", iteration, got, logs.String())
		}
		if got := strings.Count(logs.String(), `"hook_index":0`); got != 1 {
			t.Fatalf("iteration %d: hook identity log count = %d, want 1: %s", iteration, got, logs.String())
		}
	}
}

func TestLifecycleManager_PreDrainFastHookCompletesBeforeLiveBoundary(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	app, err := New(WithLogger(logger), WithoutAccessLog())
	if err != nil {
		t.Fatal(err)
	}
	app.lifecycle.onPreDrain = []drainHook{{
		index:  0,
		source: "fast-test",
		fn:     func(context.Context) error { return nil },
	}}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := app.lifecycle.runPreDrainPhase(ctx); err != nil {
		t.Fatalf("pre-drain error = %v, want nil", err)
	}
	if strings.Contains(logs.String(), "credo: drain task incomplete") {
		t.Fatalf("fast hook was reported incomplete: %s", logs.String())
	}
}

func TestLifecycleManager_PreDrainDeadlineTimestampDoesNotDependOnDelivery(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	app, err := New(WithLogger(logger), WithoutAccessLog())
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Millisecond)
	done := make(chan struct{})
	ctx := delayedDeadlineContext{Context: t.Context(), deadline: deadline, done: done}
	app.lifecycle.onPreDrain = []drainHook{{
		index:  0,
		source: "delayed-deadline-test",
		fn: func(context.Context) error {
			timer := time.NewTimer(time.Until(deadline) + time.Millisecond)
			defer timer.Stop()
			<-timer.C
			return nil
		},
	}}

	err = app.lifecycle.runPreDrainPhase(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pre-drain error = %v, want context.DeadlineExceeded", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("delayed context Err() = %v, want nil", ctx.Err())
	}
	select {
	case <-ctx.Done():
		t.Fatal("delayed context delivered Done unexpectedly")
	default:
	}
	if got := strings.Count(err.Error(), "OnPreDrain hook [0]"); got != 1 {
		t.Fatalf("incomplete identity count = %d, want 1: %v", got, err)
	}
	if got := strings.Count(logs.String(), `"msg":"credo: drain task incomplete"`); got != 1 {
		t.Fatalf("incomplete log count = %d, want 1: %s", got, logs.String())
	}
	if got := strings.Count(logs.String(), `"hook_index":0`); got != 1 {
		t.Fatalf("hook identity log count = %d, want 1: %s", got, logs.String())
	}
}

func TestPreDrainCompletionErrorUsesTimestampForDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(time.Hour))
	defer cancel()
	deadline, _ := ctx.Deadline()

	tests := []struct {
		name   string
		result drainResult
		want   error
	}{
		{
			name: "completion before deadline wins over late Err observation",
			result: drainResult{
				completedAt: deadline.Add(-time.Nanosecond),
				contextErr:  context.DeadlineExceeded,
			},
		},
		{
			name: "completion at deadline is incomplete",
			result: drainResult{
				completedAt: deadline,
			},
			want: context.DeadlineExceeded,
		},
		{
			name: "completion after deadline is incomplete",
			result: drainResult{
				completedAt: deadline.Add(time.Nanosecond),
			},
			want: context.DeadlineExceeded,
		},
		{
			name: "manual cancellation remains incomplete",
			result: drainResult{
				completedAt: deadline.Add(-time.Nanosecond),
				contextErr:  context.Canceled,
			},
			want: context.Canceled,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := preDrainCompletionError(ctx, test.result)
			if !errors.Is(got, test.want) || (test.want == nil && got != nil) {
				t.Fatalf("preDrainCompletionError() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestLifecycleManager_PreDrainLogsBoundaryBeforeHardBarrierReturns(t *testing.T) {
	capture := &messageCaptureHandler{
		message: preDrainBoundaryLogMessage,
		records: make(chan slog.Record, 1),
	}
	app, err := New(WithLogger(slog.New(capture)), WithoutAccessLog())
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Millisecond)
	ctx := delayedDeadlineContext{
		Context:  t.Context(),
		deadline: deadline,
		done:     make(chan struct{}),
	}
	release := make(chan struct{})
	app.lifecycle.onPreDrain = []drainHook{{
		index:  0,
		source: "boundary-log-test",
		fn: func(context.Context) error {
			<-release
			return nil
		},
	}}

	result := make(chan error, 1)
	go func() { result <- app.lifecycle.runPreDrainPhase(ctx) }()

	var record slog.Record
	select {
	case record = <-capture.records:
	case <-time.After(time.Second):
		t.Fatal("missing pre-drain boundary diagnostic")
	}
	var pendingCount int64
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "pending_count" {
			pendingCount = attr.Value.Int64()
		}
		return true
	})
	if pendingCount != 1 {
		t.Fatalf("boundary pending_count = %d, want 1", pendingCount)
	}
	select {
	case err := <-result:
		t.Fatalf("pre-drain returned before hard barrier release: %v", err)
	default:
	}

	close(release)
	err = <-result
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pre-drain error = %v, want context.DeadlineExceeded", err)
	}
}
