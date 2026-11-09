package worker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// infoBlocking is a continuous body that runs until its context ends.
func infoBlocking(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// TestSnapshot_EachSnapshotOwnsItsConfiguration: a snapshot copies the
// resolved configuration, so mutating one reaches neither the next snapshot
// nor the definition.
func TestSnapshot_EachSnapshotOwnsItsConfiguration(t *testing.T) {
	consumer := NewComponent(newTestEngine().Engine,
		continuousDef("consumer", ContinuousConfig{Restart: Restart{Limit: 3}}), workerFunc(infoBlocking))
	report := NewComponent(newTestEngine().Engine,
		scheduledDef("report", "@every 1m", ScheduledConfig{RunTimeout: time.Minute}), workerFunc(infoBlocking))

	first := []Info{consumer.Snapshot(), report.Snapshot()}
	first[0].Continuous.Restart.Limit = 99
	first[1].Scheduled.RunTimeout = time.Hour

	second := []Info{consumer.Snapshot(), report.Snapshot()}
	switch {
	case second[0].Continuous == first[0].Continuous:
		t.Fatal("two snapshots share one *ContinuousConfig")
	case second[1].Scheduled == first[1].Scheduled:
		t.Fatal("two snapshots share one *ScheduledConfig")
	case second[0].Continuous.Restart.Limit != 3:
		t.Fatalf("a snapshot mutation reached the component: %+v", *second[0].Continuous)
	case second[1].Scheduled.RunTimeout != time.Minute:
		t.Fatalf("a snapshot mutation reached the component: %+v", *second[1].Scheduled)
	case consumer.Definition().Continuous().Restart.Limit != 3 || report.Definition().Scheduled().RunTimeout != time.Minute:
		t.Fatal("a snapshot mutation reached the definition")
	}
}

func TestSnapshot_CountersPerKind(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		s := newTestEngine()
		var continuousRuns, scheduledRuns atomic.Int32
		consumer := addWorker(s, continuousDef("consumer", ContinuousConfig{
			Restart: Restart{MinDelay: time.Second, MaxDelay: time.Second},
		}), workerFunc(func(ctx context.Context) error {
			if continuousRuns.Add(1) <= 2 {
				return errors.New("boom")
			}
			return infoBlocking(ctx)
		}))
		report := addWorker(s, scheduledDef("report", "@every 1m"), workerFunc(func(context.Context) error {
			if scheduledRuns.Add(1) <= 2 {
				return errors.New("boom")
			}
			return nil
		}))
		startWorker(t, consumer)
		startWorker(t, report)

		// +2m: the continuous worker is in its third run; the scheduled
		// worker has failed twice.
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		c, _ := s.Lookup("consumer")
		if c.Status != StatusRunning || c.Restarts != 2 || c.ConsecutiveFailures != 0 || c.LastError != "boom" ||
			!c.LastStartedAt.Equal(start.Add(2*time.Second)) || !c.LastSucceededAt.IsZero() {
			t.Errorf("consumer = %+v, want running, 2 restarts, no consecutive failures, "+
				"last error boom, started at +2s, never succeeded", c)
		}
		r, _ := s.Lookup("report")
		if r.Status != StatusWaiting || r.Restarts != 0 || r.ConsecutiveFailures != 2 || r.LastError != "boom" ||
			!r.LastStartedAt.Equal(start.Add(2*time.Minute)) || !r.LastSucceededAt.IsZero() {
			t.Errorf("report = %+v, want waiting, no restarts, 2 consecutive failures, "+
				"last error boom, started at +2m, never succeeded", r)
		}

		// +3m: the third scheduled run succeeds and resets the streak.
		time.Sleep(time.Minute)
		synctest.Wait()
		r, _ = s.Lookup("report")
		if r.Status != StatusWaiting || r.ConsecutiveFailures != 0 || r.LastError != "" ||
			!r.LastSucceededAt.Equal(start.Add(3*time.Minute)) {
			t.Errorf("report = %+v, want waiting, streak reset, error cleared, succeeded at +3m", r)
		}
	})
}
