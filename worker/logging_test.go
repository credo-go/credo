package worker

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// lifecycleLines returns the "worker started" and "worker stopped" records
// of the named worker.
func lifecycleLines(logs *logCapture, name string) (started, stopped []capturedLog) {
	for _, entry := range logs.all() {
		if entry.Attrs["worker"] != name {
			continue
		}
		switch entry.Message {
		case "worker started":
			started = append(started, entry)
		case "worker stopped":
			stopped = append(stopped, entry)
		}
	}
	return started, stopped
}

func TestLogging_OneStartAndOneStopPerExitPath(t *testing.T) {
	cases := []struct {
		name       string
		run        Func
		opts       []Option
		advance    time.Duration // virtual time before shutdown
		wantStatus Status
		wantReason string
	}{
		{"cancelled while waiting", scheduledOnce(), []Option{WithSchedule("@every 1h")},
			time.Minute, StatusStopped, "shutdown"},
		{"context error after cancellation", func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}, nil, time.Minute, StatusStopped, "shutdown"},
		{"nil after cancellation", blockingFunc(), nil,
			time.Minute, StatusStopped, "shutdown"},
		{"restart limit exhausted", func(context.Context) error { return errors.New("boom") },
			[]Option{WithMaxRestarts(1), WithRestartDelay(time.Second)}, time.Minute, StatusFailed, "failed"},
		{"failure limit exhausted", func(context.Context) error { return errors.New("boom") },
			[]Option{WithSchedule("@every 1m"), WithMaxConsecutiveFailures(2)}, 3 * time.Minute, StatusFailed, "failed"},
		{"no future activation", scheduledOnce(), []Option{WithSchedule("0 0 30 2 *")},
			time.Minute, StatusFailed, "failed"},
		{"panic during shutdown", func(ctx context.Context) error {
			<-ctx.Done()
			panic("kaboom")
		}, nil, time.Minute, StatusStopped, "shutdown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				logs := newLogCapture()
				p := newPool(logs.logger(), poolConfig{})
				startPool(t, p, mustDefinition(t, "w", tc.run, tc.opts...))
				time.Sleep(tc.advance)
				synctest.Wait()
				shutdownPool(t, p)

				started, stopped := lifecycleLines(logs, "w")
				if len(started) != 1 || len(stopped) != 1 {
					t.Fatalf("started = %d, stopped = %d lines, want exactly one each", len(started), len(stopped))
				}
				if started[0].Level != slog.LevelInfo || stopped[0].Level != slog.LevelInfo {
					t.Fatalf("lifecycle levels = %s/%s, want INFO/INFO", started[0].Level, stopped[0].Level)
				}
				if got := stopped[0].Attrs["status"]; got != string(tc.wantStatus) {
					t.Errorf("stopped status = %v, want %s", got, tc.wantStatus)
				}
				if got := stopped[0].Attrs["reason"]; got != tc.wantReason {
					t.Errorf("stopped reason = %v, want %s", got, tc.wantReason)
				}
				for _, removed := range []string{"worker stopped during scheduled run", "worker tick skipped"} {
					if n := len(logs.withMessage(removed)); n != 0 {
						t.Errorf("%d %q lines, want none", n, removed)
					}
				}
			})
		})
	}
}

func TestLogging_FailedStartLogsNothing(t *testing.T) {
	app := newTestApp(t)
	logs := newLogCapture()
	MustRegister(app, "healthy", blockingFunc())
	MustRegisterProvided[*stubWorker[kindA]](app, "missing")
	finalize(t, app)
	pool, err := app.Resolve[*Pool]()
	if err != nil {
		t.Fatal(err)
	}
	pool.logger = logs.logger()
	if err = pool.Start(t.Context()); err == nil {
		t.Fatal("Start() = nil, want a resolution error")
	}
	shutdownPool(t, pool)
	if entries := logs.all(); len(entries) != 0 {
		t.Fatalf("a failed Start logged %+v, want nothing: no worker ran", entries)
	}
}

func TestLogging_StartLineAttributes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		logs := newLogCapture()
		p := newPool(logs.logger(), poolConfig{})
		startPool(t, p,
			mustDefinition(t, "consumer", blockingFunc()),
			mustDefinition(t, "report", scheduledOnce(), WithSchedule("@every 1h")),
			mustDefinition(t, "warmup", scheduledOnce(), WithSchedule("@every 1h"), WithStartImmediately()),
		)
		synctest.Wait()

		for name, want := range map[string]map[string]any{
			"consumer": {"kind": "continuous"},
			"report":   {"kind": "scheduled", "schedule": "@every 1h", "next_run": start.Add(time.Hour)},
			"warmup":   {"kind": "scheduled", "schedule": "@every 1h", "start_immediately": true},
		} {
			started, _ := lifecycleLines(logs, name)
			if len(started) != 1 {
				t.Fatalf("%s: %d started lines, want 1", name, len(started))
			}
			attrs := started[0].Attrs
			for key, value := range want {
				got := attrs[key]
				if at, ok := value.(time.Time); ok {
					if gotTime, ok := got.(time.Time); !ok || !gotTime.Equal(at) {
						t.Errorf("%s: %s = %v, want %v", name, key, got, at)
					}
					continue
				}
				if got != value {
					t.Errorf("%s: %s = %v, want %v", name, key, got, value)
				}
			}
			if name == "report" {
				if _, ok := attrs["start_immediately"]; ok {
					t.Errorf("report: start_immediately present without WithStartImmediately")
				}
			}
		}
		shutdownPool(t, p)
	})
}

func TestLogging_RunLines(t *testing.T) {
	t.Run("success is debug only and carries the run id", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			logs := newLogCapture()
			p := newPool(logs.logger(), poolConfig{})
			var runID atomic.Value
			startPool(t, p, mustDefinition(t, "report", Func(func(ctx context.Context) error {
				runID.Store(RunID(ctx))
				time.Sleep(3 * time.Second)
				return nil
			}), WithSchedule("@every 1m")))
			time.Sleep(time.Minute + 5*time.Second)
			synctest.Wait()
			shutdownPool(t, p)

			done := logs.withMessage("scheduled worker run completed")
			if len(done) != 1 || done[0].Level != slog.LevelDebug {
				t.Fatalf("completion lines = %+v, want one at DEBUG", done)
			}
			if got := done[0].Attrs["run_id"]; got == "" || got != runID.Load() {
				t.Fatalf("run_id = %v, want the RunID the worker saw (%v)", got, runID.Load())
			}
			if got := done[0].Attrs["duration"]; got != 3*time.Second {
				t.Fatalf("duration = %v, want 3s", got)
			}
			for _, entry := range logs.all() {
				if entry.Level > slog.LevelDebug && entry.Message != "worker started" && entry.Message != "worker stopped" {
					t.Errorf("unexpected %s line %q for a successful run", entry.Level, entry.Message)
				}
			}
		})
	})

	failureLine := func(t *testing.T, logs *logCapture, msg string) capturedLog {
		t.Helper()
		lines := logs.withMessage(msg)
		if len(lines) == 0 {
			t.Fatalf("no %q line", msg)
		}
		if lines[0].Level != slog.LevelError {
			t.Fatalf("%q at %s, want ERROR", msg, lines[0].Level)
		}
		return lines[0]
	}

	t.Run("timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			logs := newLogCapture()
			p := newPool(logs.logger(), poolConfig{})
			var runID atomic.Value
			startPool(t, p, mustDefinition(t, "report", Func(func(ctx context.Context) error {
				runID.Store(RunID(ctx))
				<-ctx.Done()
				return nil
			}), WithSchedule("@every 1m"), WithRunTimeout(10*time.Second)))
			time.Sleep(time.Minute + 10*time.Second)
			synctest.Wait()
			shutdownPool(t, p)

			line := failureLine(t, logs, "scheduled worker run failed")
			if line.Attrs["timed_out"] != true || line.Attrs["duration"] != 10*time.Second || line.Attrs["run_id"] != runID.Load() {
				t.Fatalf("attrs = %v, want timed_out=true, duration=10s and the worker's run id", line.Attrs)
			}
			for _, absent := range []string{"unexpected_exit", "stack"} {
				if _, ok := line.Attrs[absent]; ok {
					t.Errorf("%s present on a timeout failure", absent)
				}
			}
		})
	})

	cases := []struct {
		name           string
		run            Func
		wantUnexpected bool
		wantStack      bool
	}{
		{"early nil return", func(context.Context) error { return nil }, true, false},
		{"error", func(context.Context) error { return errors.New("boom") }, false, false},
		{"panic", func(context.Context) error { panic("kaboom") }, false, true},
	}
	for _, tc := range cases {
		t.Run("continuous "+tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				logs := newLogCapture()
				p := newPool(logs.logger(), poolConfig{})
				startPool(t, p, mustDefinition(t, "consumer", tc.run, WithRestartDelay(time.Minute)))
				synctest.Wait()
				shutdownPool(t, p)

				line := failureLine(t, logs, "worker run failed")
				if _, ok := line.Attrs["run_id"].(string); !ok {
					t.Errorf("run_id = %v, want a string", line.Attrs["run_id"])
				}
				if _, ok := line.Attrs["duration"].(time.Duration); !ok {
					t.Errorf("duration = %v, want a time.Duration", line.Attrs["duration"])
				}
				_, unexpected := line.Attrs["unexpected_exit"]
				if unexpected != tc.wantUnexpected {
					t.Errorf("unexpected_exit present = %t, want %t", unexpected, tc.wantUnexpected)
				}
				stack, hasStack := line.Attrs["stack"].(string)
				if hasStack != tc.wantStack || (hasStack && !strings.Contains(stack, "goroutine")) {
					t.Errorf("stack = %q, want present=%t", stack, tc.wantStack)
				}
				if msg, _ := line.Attrs["error"].(error); msg != nil && strings.Contains(msg.Error(), "goroutine") {
					t.Errorf("error attribute carries a stack: %v", msg)
				}
				if _, ok := line.Attrs["timed_out"]; ok {
					t.Error("timed_out present on a continuous failure")
				}
			})
		})
	}
}

func TestLogging_SkippedActivationsCollapse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		logs := newLogCapture()
		p := newPool(logs.logger(), poolConfig{})
		var runs atomic.Int32
		startPool(t, p, mustDefinition(t, "ticker", Func(func(context.Context) error {
			if runs.Add(1) == 1 {
				time.Sleep(10*time.Second + 500*time.Millisecond)
			}
			return nil
		}), WithSchedule("@every 1s")))
		time.Sleep(15 * time.Second)
		synctest.Wait()
		shutdownPool(t, p)

		skips := logs.withMessage("worker ticks skipped")
		if len(skips) != 1 {
			t.Fatalf("%d skip lines, want 1 for the whole burst", len(skips))
		}
		attrs := skips[0].Attrs
		first, _ := attrs["first_scheduled_at"].(time.Time)
		last, _ := attrs["last_scheduled_at"].(time.Time)
		// The first run lasts 1s → 11.5s; the activations at 2s … 11s are
		// skipped and 12s runs next.
		if attrs["skipped"] != int64(10) || !first.Equal(start.Add(2*time.Second)) || !last.Equal(start.Add(11*time.Second)) {
			t.Fatalf("skip attrs = %v, want skipped=10 from +2s to +11s", attrs)
		}
		if skips[0].Level != slog.LevelWarn {
			t.Fatalf("skip line at %s, want WARN", skips[0].Level)
		}
	})
}

// TestPoolStart_PublishesContinuousRunnersIdle exercises the real Pool.Start
// path with an already-cancelled context: the published continuous runner is
// not Running when its loop logs "worker started", and the admission check
// then stops it without a run.
func TestPoolStart_PublishesContinuousRunnersIdle(t *testing.T) {
	logs := newLogCapture()
	p := newPool(logs.logger(), poolConfig{})
	var runs atomic.Int32
	if err := p.addDefinition(mustDefinition(t, "consumer", Func(func(ctx context.Context) error {
		runs.Add(1)
		<-ctx.Done()
		return nil
	}))); err != nil {
		t.Fatal(err)
	}

	statusAtStart := make(chan Status, 1)
	logs.setObserve(func(entry capturedLog) {
		if entry.Message == "worker started" {
			statusAtStart <- p.Workers()[0].Status
		}
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	shutdownPool(t, p)

	select {
	case status := <-statusAtStart:
		if status == StatusRunning {
			t.Fatalf("status at 'worker started' = %q: only run admission may set Running", status)
		}
	default:
		t.Fatal("no 'worker started' line")
	}
	info := p.Workers()[0]
	if runs.Load() != 0 || !info.LastRun.IsZero() || info.Status != StatusStopped {
		t.Fatalf("runs = %d, %+v, want no run, zero LastRun, stopped", runs.Load(), info)
	}
}
