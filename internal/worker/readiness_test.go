package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// readinessComponent adds w under def to an engine without an App and
// returns the component the App would plan Ready from.
func readinessComponent(def *Definition, w Worker) *ReadyComponent {
	return &ReadyComponent{Component: addWorker(newTestEngine(), def, w)}
}

// readinessFailing is a worker whose every run fails with "boom".
var readinessFailing = workerFunc(func(context.Context) error { return errors.New("boom") })

// readinessWantErr fails the test unless err is non-nil and contains every part of want.
func readinessWantErr(t *testing.T, what string, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: Ready() = nil, want an error containing %q", what, want)
	}
	for _, part := range want {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("%s: Ready() = %q, want it to contain %q", what, err, part)
		}
	}
}

func readinessWantReady(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: Ready() = %v, want nil", what, err)
	}
}

func TestReadiness_ConditionsAreOptIn(t *testing.T) {
	tests := []struct {
		name string
		def  *Definition
		want bool
	}{
		{"continuous without a condition", continuousDef("w"), false},
		{"continuous with restarts limited", continuousDef("w", ContinuousConfig{Restart: Restart{Limit: 1}}), false},
		{"continuous UnreadyWhenFailed", continuousDef("w", ContinuousConfig{UnreadyWhenFailed: true}), true},
		{"scheduled without a condition", scheduledDef("w", "@every 1m"), false},
		{
			"scheduled with only non-readiness settings",
			scheduledDef("w", "@every 1m", ScheduledConfig{RunOnStart: true, MaxConsecutiveFailures: 3}),
			false,
		},
		{"scheduled UnreadyWhenFailed", scheduledDef("w", "@every 1m", ScheduledConfig{UnreadyWhenFailed: true}), true},
		{
			"scheduled UnreadyUntilFirstSuccess",
			scheduledDef("w", "@every 1m", ScheduledConfig{UnreadyUntilFirstSuccess: true}),
			true,
		},
		{
			"scheduled UnreadyAfterSuccessAge",
			scheduledDef("w", "@every 1m", ScheduledConfig{UnreadyAfterSuccessAge: time.Minute}),
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.def.Readies(); got != tt.want {
				t.Fatalf("readies() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReadiness_UnreadyWhenFailed_Continuous(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		def := continuousDef("critical", ContinuousConfig{Restart: Restart{Limit: 1}, UnreadyWhenFailed: true})
		c := readinessComponent(def, readinessFailing)
		readinessWantReady(t, "before Start", c.Ready(t.Context()))

		startWorker(t, c.Component)
		synctest.Wait()
		if got := c.r.status(); got != StatusBackoff {
			t.Fatalf("status after the first failure = %s, want backoff", got)
		}
		readinessWantReady(t, "in backoff", c.Ready(t.Context()))

		time.Sleep(DefaultMinRestartDelay)
		synctest.Wait()
		if got := c.r.status(); got != StatusFailed {
			t.Fatalf("status after the restart failed = %s, want failed", got)
		}
		readinessWantErr(t, "failed", c.Ready(t.Context()), `worker "critical" failed permanently: boom`)
	})
}

func TestReadiness_UnreadyWhenFailed_Scheduled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		def := scheduledDef("sync", "@every 1h", ScheduledConfig{
			RunOnStart:             true,
			MaxConsecutiveFailures: 2,
			UnreadyWhenFailed:      true,
		})
		c := readinessComponent(def, readinessFailing)
		readinessWantReady(t, "before Start", c.Ready(t.Context()))

		startWorker(t, c.Component)
		synctest.Wait()
		if info := c.r.snapshot(); info.Status != StatusWaiting || info.ConsecutiveFailures != 1 {
			t.Fatalf("after one failure: %+v, want waiting with one consecutive failure", info)
		}
		readinessWantReady(t, "failing but not failed", c.Ready(t.Context()))

		time.Sleep(time.Hour)
		synctest.Wait()
		if got := c.r.status(); got != StatusFailed {
			t.Fatalf("status after the second failure = %s, want failed", got)
		}
		readinessWantErr(t, "failed", c.Ready(t.Context()), `worker "sync" failed permanently: boom`)
	})
}

func TestReadiness_UnreadyUntilFirstSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var calls int
		w := workerFunc(func(ctx context.Context) error {
			calls++
			if calls > 1 {
				return errors.New("boom")
			}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		def := scheduledDef("recover", "@every 1h", ScheduledConfig{RunOnStart: true, UnreadyUntilFirstSuccess: true})
		c := readinessComponent(def, w)
		readinessWantErr(t, "before Start", c.Ready(t.Context()), `worker "recover" has not started`)

		startWorker(t, c.Component)
		synctest.Wait()
		readinessWantErr(t, "first run in flight", c.Ready(t.Context()), `worker "recover" has no successful run yet`)

		close(release)
		synctest.Wait()
		readinessWantReady(t, "after the first success", c.Ready(t.Context()))

		// Once met, the condition stays met: a later failure does not revert it.
		time.Sleep(time.Hour)
		synctest.Wait()
		if info := c.r.snapshot(); info.ConsecutiveFailures != 1 || info.LastError != "boom" {
			t.Fatalf("after the second run: %+v, want one consecutive failure", info)
		}
		readinessWantReady(t, "failure after the first success", c.Ready(t.Context()))
	})
}

func TestReadiness_NeverStartedComponent(t *testing.T) {
	tests := []struct {
		name    string
		def     *Definition
		wantErr string
	}{
		{
			"UnreadyUntilFirstSuccess",
			scheduledDef("warmup", "@every 1m", ScheduledConfig{UnreadyUntilFirstSuccess: true}),
			`worker "warmup" has not started`,
		},
		{"continuous UnreadyWhenFailed", continuousDef("warmup", ContinuousConfig{UnreadyWhenFailed: true}), ""},
		{
			"scheduled UnreadyWhenFailed",
			scheduledDef("warmup", "@every 1m", ScheduledConfig{UnreadyWhenFailed: true}),
			"",
		},
		{
			"UnreadyAfterSuccessAge",
			scheduledDef("warmup", "@every 1m", ScheduledConfig{UnreadyAfterSuccessAge: time.Minute}),
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := readinessComponent(tt.def, readinessFailing)
			err := c.Ready(t.Context())
			if tt.wantErr == "" {
				readinessWantReady(t, "never started", err)
				return
			}
			readinessWantErr(t, "never started", err, tt.wantErr)
		})
	}
}

func TestReadiness_UnreadyAfterSuccessAge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		def := scheduledDef("sync", "@every 24h", ScheduledConfig{UnreadyAfterSuccessAge: time.Hour})
		c := readinessComponent(def, workerFunc(func(context.Context) error { return nil }))

		startWorker(t, c.Component)
		synctest.Wait()
		// The age is not applied before the first success.
		readinessWantReady(t, "before the first success", c.Ready(t.Context()))

		time.Sleep(24 * time.Hour)
		synctest.Wait()
		if c.r.snapshot().LastSucceededAt.IsZero() {
			t.Fatal("LastSucceededAt is zero after the first activation")
		}
		readinessWantReady(t, "fresh success", c.Ready(t.Context()))

		time.Sleep(time.Hour)
		readinessWantReady(t, "success exactly at the age", c.Ready(t.Context()))

		time.Sleep(time.Second)
		readinessWantErr(t, "stale success", c.Ready(t.Context()),
			`worker "sync" last succeeded 1h0m1s ago, limit 1h0m0s`)

		// The next success makes it fresh again.
		time.Sleep(23*time.Hour - time.Second)
		synctest.Wait()
		readinessWantReady(t, "after the next success", c.Ready(t.Context()))
	})
}
