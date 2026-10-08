package worker

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/credo-go/credo"
)

// loggingLines returns the records of the named worker with message msg.
func loggingLines(logs *logCapture, name, msg string) []capturedLog {
	var out []capturedLog
	for _, entry := range logs.withMessage(msg) {
		if entry.Attrs["worker"] == name {
			out = append(out, entry)
		}
	}
	return out
}

// loggingOne returns the single record of the named worker with message msg,
// failing the test unless there is exactly one, at level.
func loggingOne(t *testing.T, logs *logCapture, name, msg string, level slog.Level) capturedLog {
	t.Helper()
	lines := loggingLines(logs, name, msg)
	if len(lines) != 1 {
		t.Fatalf("%s: %d %q lines, want 1: %+v", name, len(lines), msg, lines)
	}
	if lines[0].Level != level {
		t.Fatalf("%s: %q at %s, want %s", name, msg, lines[0].Level, level)
	}
	return lines[0]
}

// loggingRunIDs records the CurrentRun ID of every run of a worker, in order.
type loggingRunIDs struct {
	mu  sync.Mutex
	ids []string
}

func (r *loggingRunIDs) record(ctx context.Context) {
	run, _ := CurrentRun(ctx)
	r.mu.Lock()
	r.ids = append(r.ids, run.ID)
	r.mu.Unlock()
}

func (r *loggingRunIDs) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids...)
}

// loggingBlocking is a continuous body that runs until its context ends.
func loggingBlocking(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// loggingSucceed is a scheduled body that succeeds.
func loggingSucceed(context.Context) error { return nil }

// loggingFail is a body that fails with "boom".
func loggingFail(context.Context) error { return errors.New("boom") }

// loggingAssertAbsent fails the test for every key present in attrs.
func loggingAssertAbsent(t *testing.T, line capturedLog, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if v, ok := line.Attrs[key]; ok {
			t.Errorf("%q: %s = %v present, want absent", line.Message, key, v)
		}
	}
}

func TestLogging_OneStartAndOneStopPerExitPath(t *testing.T) {
	cases := []struct {
		name       string
		def        *definition
		run        Func
		advance    time.Duration // virtual time before shutdown
		wantStatus Status
		wantReason string
		wantFailed string // the reason of the "worker failed" line; empty for none
	}{
		{"scheduled cancelled while waiting", scheduledDef("w", "@every 1h"), loggingSucceed,
			time.Minute, StatusStopped, "shutdown", ""},
		{"continuous returns ctx.Err at shutdown", continuousDef("w"), func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}, time.Minute, StatusStopped, "shutdown", ""},
		{"continuous returns nil at shutdown", continuousDef("w"), loggingBlocking,
			time.Minute, StatusStopped, "shutdown", ""},
		{"continuous cancelled during backoff", continuousDef("w", ContinuousConfig{
			Restart: Restart{MinDelay: time.Hour},
		}), loggingFail, time.Minute, StatusStopped, "shutdown", ""},
		{"restart limit", continuousDef("w", ContinuousConfig{
			Restart: Restart{Limit: 1, MinDelay: time.Second},
		}), loggingFail, time.Minute, StatusFailed, "failed", "restart_limit"},
		{"restart disabled", continuousDef("w", ContinuousConfig{Restart: Restart{Disabled: true}}),
			loggingFail, time.Minute, StatusFailed, "failed", "restart_disabled"},
		{"restart disabled, failure during shutdown",
			continuousDef("w", ContinuousConfig{Restart: Restart{Disabled: true}}),
			func(ctx context.Context) error {
				<-ctx.Done()
				return errors.New("flush failed")
			}, time.Minute, StatusFailed, "failed", "restart_disabled"},
		{"failure limit", scheduledDef("w", "@every 1m", ScheduledConfig{MaxConsecutiveFailures: 2}),
			loggingFail, 3 * time.Minute, StatusFailed, "failed", "failure_limit"},
		{"schedule exhausted", scheduledDef("w", "0 0 30 2 *"), loggingSucceed,
			time.Minute, StatusFailed, "failed", "schedule_exhausted"},
		{"panic during shutdown", continuousDef("w"), func(ctx context.Context) error {
			<-ctx.Done()
			panic("kaboom")
		}, time.Minute, StatusStopped, "shutdown", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				logs := newLogCapture()
				s := newTestSupervisorWithLogger(logs.logger())
				c := addWorker(s, tc.def, tc.run)
				startWorker(t, c)
				time.Sleep(tc.advance)
				synctest.Wait()
				stopWorker(t, c)

				started := loggingOne(t, logs, "w", "worker started", slog.LevelInfo)
				stopped := loggingOne(t, logs, "w", "worker stopped", slog.LevelInfo)
				kind := string(tc.def.kind)
				if started.Attrs["kind"] != kind || stopped.Attrs["kind"] != kind {
					t.Errorf("kind = %v/%v, want %s on both lines", started.Attrs["kind"], stopped.Attrs["kind"], kind)
				}
				if got := stopped.Attrs["status"]; got != string(tc.wantStatus) {
					t.Errorf("stopped status = %v, want %s", got, tc.wantStatus)
				}
				if got := stopped.Attrs["reason"]; got != tc.wantReason {
					t.Errorf("stopped reason = %v, want %s", got, tc.wantReason)
				}

				failed := loggingLines(logs, "w", "worker failed")
				switch {
				case tc.wantFailed == "" && len(failed) != 0:
					t.Errorf("worker failed lines = %+v, want none", failed)
				case tc.wantFailed != "" && (len(failed) != 1 || failed[0].Attrs["reason"] != tc.wantFailed):
					t.Errorf("worker failed lines = %+v, want one with reason=%s", failed, tc.wantFailed)
				}

				entries := logs.all()
				if last := entries[len(entries)-1]; last.Message != "worker stopped" {
					t.Errorf("last line = %q, want worker stopped", last.Message)
				}
				for _, entry := range entries {
					if entry.Attrs["worker"] != "w" {
						t.Errorf("%q: worker = %v, want w", entry.Message, entry.Attrs["worker"])
					}
				}
			})
		})
	}
}

// TestLogging_StartedIsTheFirstLine pins that a worker's log opens with its
// one "worker started" line, also when the worker fails before any run.
func TestLogging_StartedIsTheFirstLine(t *testing.T) {
	cases := []struct {
		name string
		def  *definition
	}{
		{"continuous restart disabled", continuousDef("w", ContinuousConfig{Restart: Restart{Disabled: true}})},
		{"scheduled failure limit on start", scheduledDef("w", "@every 1h", ScheduledConfig{
			RunOnStart: true, MaxConsecutiveFailures: 1,
		})},
		{"schedule exhausted", scheduledDef("w", "0 0 30 2 *")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				logs := newLogCapture()
				s := newTestSupervisorWithLogger(logs.logger())
				c := addWorker(s, tc.def, Func(loggingFail))
				startWorker(t, c)
				synctest.Wait()
				stopWorker(t, c)

				entries := logs.all()
				if len(entries) == 0 || entries[0].Message != "worker started" {
					var got []string
					for _, entry := range entries {
						got = append(got, entry.Message)
					}
					t.Fatalf("lines = %q, want worker started first", got)
				}
			})
		})
	}
}

func TestLogging_NeverStartedComponentWritesNothing(t *testing.T) {
	logs := newLogCapture()
	s := newTestSupervisorWithLogger(logs.logger())
	c := addWorker(s, continuousDef("w"), Func(loggingBlocking))
	stopWorker(t, c)
	if entries := logs.all(); len(entries) != 0 {
		t.Fatalf("a never-started worker logged %+v, want nothing", entries)
	}
}

// loggingFailingStart is a component whose Start fails.
type loggingFailingStart struct{}

func (*loggingFailingStart) Start(context.Context) error { return errors.New("database unreachable") }

func (*loggingFailingStart) Shutdown(context.Context) error { return nil }

func TestLogging_FailedStartPhaseLogsNothingForUnstartedWorkers(t *testing.T) {
	logs := newLogCapture()
	app := newTestApp(t, credo.WithLogger(logs.logger()))
	app.Manage(&loggingFailingStart{}, credo.Named("database"))
	workers := Use(app)
	// Ingress: it would start after the internal tier, which fails first.
	workers.Scheduled("report", "@every 1h", Func(loggingSucceed), ScheduledConfig{RunOnStart: true})

	if err := app.Start(t.Context()); err == nil {
		t.Fatal("App.Start() = nil, want the database's start failure")
	}
	for _, entry := range logs.all() {
		if strings.HasPrefix(entry.Message, "worker ") || entry.Attrs["worker"] != nil {
			t.Errorf("unstarted worker logged %q %v", entry.Message, entry.Attrs)
		}
	}
	if info, _ := workers.Lookup("report"); info.Status != StatusPending {
		t.Errorf("status = %q, want pending", info.Status)
	}
}

func TestLogging_SupervisorLinesCarryModule(t *testing.T) {
	logs := newLogCapture()
	stopped := make(chan struct{})
	logs.setObserve(func(entry capturedLog) {
		if entry.Message == "worker stopped" && entry.Attrs["worker"] == "consumer" {
			close(stopped)
		}
	})
	app := newTestApp(t, credo.WithLogger(logs.logger()))
	Use(app).Continuous("consumer", Func(loggingFail), ContinuousConfig{Restart: Restart{Disabled: true}})
	startApp(t, app)

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("no worker stopped line")
	}
	want := []string{"worker started", "worker run failed", "worker failed", "worker stopped"}
	var got []string
	for _, entry := range logs.all() {
		if entry.Attrs["worker"] != "consumer" {
			continue
		}
		got = append(got, entry.Message)
		if entry.Attrs["module"] != "worker" {
			t.Errorf("%q: module = %v, want worker", entry.Message, entry.Attrs["module"])
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("lines = %q, want %q", got, want)
	}
}

func TestLogging_StartLineAttributes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		logs := newLogCapture()
		s := newTestSupervisorWithLogger(logs.logger())
		for _, c := range []*component{
			addWorker(s, continuousDef("consumer"), Func(loggingBlocking)),
			addWorker(s, scheduledDef("report", "@every 1h"), Func(loggingSucceed)),
			addWorker(s, scheduledDef("warmup", "@every 1h", ScheduledConfig{RunOnStart: true}), Func(loggingSucceed)),
		} {
			startWorker(t, c)
		}
		synctest.Wait()

		consumer := loggingOne(t, logs, "consumer", "worker started", slog.LevelInfo)
		if consumer.Attrs["kind"] != "continuous" {
			t.Errorf("consumer: kind = %v, want continuous", consumer.Attrs["kind"])
		}
		loggingAssertAbsent(t, consumer, "schedule", "run_on_start", "next_run")

		report := loggingOne(t, logs, "report", "worker started", slog.LevelInfo)
		next, _ := report.Attrs["next_run"].(time.Time)
		if report.Attrs["kind"] != "scheduled" || report.Attrs["schedule"] != "@every 1h" ||
			!next.Equal(start.Add(time.Hour)) {
			t.Errorf("report: attrs = %v, want kind=scheduled, schedule=@every 1h, next_run=+1h", report.Attrs)
		}
		loggingAssertAbsent(t, report, "run_on_start")

		warmup := loggingOne(t, logs, "warmup", "worker started", slog.LevelInfo)
		if warmup.Attrs["kind"] != "scheduled" || warmup.Attrs["schedule"] != "@every 1h" ||
			warmup.Attrs["run_on_start"] != true {
			t.Errorf("warmup: attrs = %v, want kind=scheduled, schedule=@every 1h, run_on_start=true", warmup.Attrs)
		}
		loggingAssertAbsent(t, warmup, "next_run")
	})
}

func TestLogging_ScheduledRunLines(t *testing.T) {
	t.Run("success is debug only", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			logs := newLogCapture()
			s := newTestSupervisorWithLogger(logs.logger())
			var ids loggingRunIDs
			c := addWorker(s, scheduledDef("report", "@every 1m"), Func(func(ctx context.Context) error {
				ids.record(ctx)
				time.Sleep(3 * time.Second)
				return nil
			}))
			startWorker(t, c)
			time.Sleep(time.Minute + 5*time.Second)
			synctest.Wait()
			stopWorker(t, c)

			done := loggingOne(t, logs, "report", "worker run completed", slog.LevelDebug)
			at, _ := done.Attrs["scheduled_at"].(time.Time)
			if done.Attrs["kind"] != "scheduled" || done.Attrs["run_id"] != ids.all()[0] ||
				done.Attrs["duration"] != 3*time.Second || !at.Equal(start.Add(time.Minute)) {
				t.Fatalf("attrs = %v, want kind=scheduled, run_id=%s, duration=3s, scheduled_at=+1m",
					done.Attrs, ids.all()[0])
			}
			for _, entry := range logs.all() {
				lifecycle := entry.Message == "worker started" || entry.Message == "worker stopped"
				if entry.Level > slog.LevelDebug && !lifecycle {
					t.Errorf("unexpected %s line %q for a successful run", entry.Level, entry.Message)
				}
			}
		})
	})

	t.Run("failure", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			logs := newLogCapture()
			s := newTestSupervisorWithLogger(logs.logger())
			var ids loggingRunIDs
			c := addWorker(s, scheduledDef("report", "@every 1m"), Func(func(ctx context.Context) error {
				ids.record(ctx)
				return errors.New("boom")
			}))
			startWorker(t, c)
			time.Sleep(2*time.Minute + time.Second)
			synctest.Wait()
			stopWorker(t, c)

			lines := loggingLines(logs, "report", "worker run failed")
			runs := ids.all()
			if len(lines) != 2 || len(runs) != 2 {
				t.Fatalf("%d failure lines for %d runs, want 2 and 2", len(lines), len(runs))
			}
			for i, line := range lines {
				at, _ := line.Attrs["scheduled_at"].(time.Time)
				err, _ := line.Attrs["error"].(error)
				if line.Level != slog.LevelError || line.Attrs["kind"] != "scheduled" ||
					line.Attrs["run_id"] != runs[i] || line.Attrs["consecutive_failures"] != int64(i+1) || line.Attrs["duration"] != time.Duration(0) ||
					!at.Equal(start.Add(time.Duration(i+1)*time.Minute)) || err == nil || err.Error() != "boom" {
					t.Errorf("line %d = %s %v, want ERROR kind=scheduled run_id=%s consecutive_failures=%d "+
						"duration=0 scheduled_at=+%dm error=boom", i, line.Level, line.Attrs, runs[i], i+1, i+1)
				}
				loggingAssertAbsent(t, line, "timed_out", "stack", "unexpected_exit", "restarts", "next_restart_in")
			}
		})
	})

	t.Run("timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			logs := newLogCapture()
			s := newTestSupervisorWithLogger(logs.logger())
			var ids loggingRunIDs
			c := addWorker(s, scheduledDef("report", "@every 1m", ScheduledConfig{RunTimeout: 10 * time.Second}),
				Func(func(ctx context.Context) error {
					ids.record(ctx)
					<-ctx.Done()
					return nil
				}))
			startWorker(t, c)
			time.Sleep(time.Minute + 10*time.Second)
			synctest.Wait()
			stopWorker(t, c)

			line := loggingOne(t, logs, "report", "worker run failed", slog.LevelError)
			err, _ := line.Attrs["error"].(error)
			if line.Attrs["timed_out"] != true || line.Attrs["duration"] != 10*time.Second ||
				line.Attrs["run_id"] != ids.all()[0] || !errors.Is(err, ErrRunTimeout) {
				t.Fatalf("attrs = %v, want timed_out=true, duration=10s, the run's id and ErrRunTimeout", line.Attrs)
			}
			loggingAssertAbsent(t, line, "unexpected_exit", "stack")
		})
	})
}

func TestLogging_ContinuousRunFailedLine(t *testing.T) {
	cases := []struct {
		name           string
		run            func(context.Context) error
		wantUnexpected bool
		wantStack      bool
		wantError      string
	}{
		{"early nil return", func(context.Context) error { return nil }, true, false, errUnexpectedExit.Error()},
		{"error", loggingFail, false, false, "boom"},
		{"panic", func(context.Context) error { panic("kaboom") }, false, true, "worker: run panicked: kaboom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				logs := newLogCapture()
				s := newTestSupervisorWithLogger(logs.logger())
				var ids loggingRunIDs
				c := addWorker(s, continuousDef("consumer", ContinuousConfig{
					Restart: Restart{MinDelay: time.Minute, MaxDelay: time.Minute},
				}), Func(func(ctx context.Context) error {
					ids.record(ctx)
					return tc.run(ctx)
				}))
				startWorker(t, c)
				time.Sleep(time.Minute) // the first restart starts and fails at +1m
				synctest.Wait()
				stopWorker(t, c)

				lines := loggingLines(logs, "consumer", "worker run failed")
				runs := ids.all()
				if len(lines) != 2 || len(runs) != 2 {
					t.Fatalf("%d failure lines for %d runs, want 2 and 2", len(lines), len(runs))
				}
				for i, line := range lines {
					err, _ := line.Attrs["error"].(error)
					if line.Level != slog.LevelError || line.Attrs["kind"] != "continuous" ||
						line.Attrs["run_id"] != runs[i] || line.Attrs["restarts"] != int64(i) ||
						line.Attrs["next_restart_in"] != time.Minute || err == nil || err.Error() != tc.wantError {
						t.Errorf("line %d = %s %v, want ERROR kind=continuous run_id=%s restarts=%d "+
							"next_restart_in=1m error=%q", i, line.Level, line.Attrs, runs[i], i, tc.wantError)
					}
					if _, ok := line.Attrs["duration"].(time.Duration); !ok {
						t.Errorf("duration = %v, want a time.Duration", line.Attrs["duration"])
					}
					if got, ok := line.Attrs["unexpected_exit"]; ok != tc.wantUnexpected || (ok && got != true) {
						t.Errorf("unexpected_exit = %v (present %t), want present=%t", got, ok, tc.wantUnexpected)
					}
					stack, hasStack := line.Attrs["stack"].(string)
					if hasStack != tc.wantStack || (hasStack && !strings.Contains(stack, "goroutine")) {
						t.Errorf("stack = %q, want present=%t", stack, tc.wantStack)
					}
					if err != nil && strings.Contains(err.Error(), "goroutine") {
						t.Errorf("error attribute carries a stack: %v", err)
					}
					loggingAssertAbsent(t, line, "timed_out", "scheduled_at", "consecutive_failures")
				}
			})
		})
	}
}

func TestLogging_NextRestartInOmittedWithoutRestart(t *testing.T) {
	cases := []struct {
		name  string
		cfg   ContinuousConfig
		run   func(context.Context) error
		lines int // failure lines expected
	}{
		{"restart disabled", ContinuousConfig{Restart: Restart{Disabled: true}}, loggingFail, 1},
		{"restart limit reached", ContinuousConfig{Restart: Restart{Limit: 1, MinDelay: time.Second}}, loggingFail, 2},
		{"shutdown already observed", ContinuousConfig{}, func(ctx context.Context) error {
			<-ctx.Done()
			return errors.New("flush failed")
		}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				logs := newLogCapture()
				s := newTestSupervisorWithLogger(logs.logger())
				c := addWorker(s, continuousDef("consumer", tc.cfg), Func(tc.run))
				startWorker(t, c)
				time.Sleep(time.Minute)
				synctest.Wait()
				stopWorker(t, c)

				lines := loggingLines(logs, "consumer", "worker run failed")
				if len(lines) != tc.lines {
					t.Fatalf("%d failure lines, want %d", len(lines), tc.lines)
				}
				// Only the last failure is the one with no restart planned.
				loggingAssertAbsent(t, lines[len(lines)-1], "next_restart_in")
			})
		})
	}
}

func TestLogging_WorkerFailedLine(t *testing.T) {
	cases := []struct {
		name      string
		def       *definition
		advance   time.Duration
		wantAttrs map[string]any
		absent    []string
	}{
		{"restart_limit", continuousDef("w", ContinuousConfig{Restart: Restart{Limit: 2, MinDelay: time.Second}}),
			time.Minute, map[string]any{"kind": "continuous", "reason": "restart_limit", "limit": int64(2)},
			[]string{"schedule"}},
		{"restart_disabled", continuousDef("w", ContinuousConfig{Restart: Restart{Disabled: true}}),
			time.Minute, map[string]any{"kind": "continuous", "reason": "restart_disabled"},
			[]string{"limit", "schedule"}},
		{"failure_limit", scheduledDef("w", "@every 1m", ScheduledConfig{MaxConsecutiveFailures: 3}),
			5 * time.Minute, map[string]any{"kind": "scheduled", "reason": "failure_limit", "limit": int64(3)},
			[]string{"schedule"}},
		{"schedule_exhausted", scheduledDef("w", "0 0 30 2 *"),
			time.Minute, map[string]any{"kind": "scheduled", "reason": "schedule_exhausted", "schedule": "0 0 30 2 *"},
			[]string{"limit"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				logs := newLogCapture()
				s := newTestSupervisorWithLogger(logs.logger())
				c := addWorker(s, tc.def, Func(loggingFail))
				startWorker(t, c)
				time.Sleep(tc.advance)
				synctest.Wait()
				stopWorker(t, c)

				line := loggingOne(t, logs, "w", "worker failed", slog.LevelError)
				for key, want := range tc.wantAttrs {
					if got := line.Attrs[key]; got != want {
						t.Errorf("%s = %v (%T), want %v (%T)", key, got, got, want, want)
					}
				}
				loggingAssertAbsent(t, line, tc.absent...)

				// The terminal line precedes the stop line and, when a run ended
				// the worker, follows that run's failure line.
				var order []string
				for _, entry := range logs.all() {
					if entry.Level >= slog.LevelInfo {
						order = append(order, entry.Message)
					}
				}
				// (Where "worker started" sits is TestLogging_StartedIsTheFirstLine's.)
				order = slices.DeleteFunc(order, func(msg string) bool { return msg == "worker started" })
				n := len(order)
				if n < 2 || order[n-2] != "worker failed" || order[n-1] != "worker stopped" ||
					(tc.name != "schedule_exhausted" && (n < 3 || order[n-3] != "worker run failed")) {
					t.Errorf("line order = %q, want ... [worker run failed,] worker failed, worker stopped", order)
				}
				if c.r.status() != StatusFailed {
					t.Errorf("status = %q, want failed", c.r.status())
				}
			})
		})
	}
}

func TestLogging_SkippedActivationsCollapse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		logs := newLogCapture()
		s := newTestSupervisorWithLogger(logs.logger())
		var runs atomic.Int32
		c := addWorker(s, scheduledDef("ticker", "@every 1s"), Func(func(context.Context) error {
			switch runs.Add(1) {
			case 1: // +1s → +11.5s: +2s … +11s are skipped, +12s runs next
				time.Sleep(10*time.Second + 500*time.Millisecond)
			case 3: // +13s → +16.5s: +14s … +16s are skipped, +17s runs next
				time.Sleep(3*time.Second + 500*time.Millisecond)
			}
			return nil
		}))
		startWorker(t, c)
		time.Sleep(20 * time.Second)
		synctest.Wait()
		stopWorker(t, c)

		skips := loggingLines(logs, "ticker", "worker activations skipped")
		want := []struct {
			skipped     int64
			first, last time.Duration
		}{
			{10, 2 * time.Second, 11 * time.Second},
			{3, 14 * time.Second, 16 * time.Second},
		}
		if len(skips) != len(want) {
			t.Fatalf("%d skip lines, want one per resumption (%d)", len(skips), len(want))
		}
		for i, w := range want {
			attrs := skips[i].Attrs
			first, _ := attrs["first_scheduled_at"].(time.Time)
			last, _ := attrs["last_scheduled_at"].(time.Time)
			if skips[i].Level != slog.LevelWarn || attrs["skipped"] != w.skipped ||
				!first.Equal(start.Add(w.first)) || !last.Equal(start.Add(w.last)) {
				t.Errorf("skip line %d = %s %v, want WARN skipped=%d from +%s to +%s",
					i, skips[i].Level, attrs, w.skipped, w.first, w.last)
			}
		}
	})
}
