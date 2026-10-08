package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/credo-go/credo"
)

// infoBlocking is a continuous body that runs until its context ends.
func infoBlocking(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// infoProvided is a worker the container provides.
type infoProvided struct{}

func (*infoProvided) Run(ctx context.Context) error { return infoBlocking(ctx) }

func TestSnapshot_ResolvedConfigurationBeforeAndAfterStart(t *testing.T) {
	app := newTestApp(t)
	workers := Use(app)
	workers.Continuous("consumer", Func(infoBlocking), ContinuousConfig{
		Restart:           Restart{Limit: 4, MinDelay: 2 * time.Second},
		UnreadyWhenFailed: true,
	})
	workers.Scheduled("report", "@every 1h", Func(func(context.Context) error { return nil }), ScheduledConfig{
		RunOnStart:               true,
		RunTimeout:               time.Minute,
		MaxConsecutiveFailures:   3,
		UnreadyUntilFirstSuccess: true,
		UnreadyAfterSuccessAge:   2 * time.Hour,
	})
	app.Provide[*infoProvided](func() *infoProvided { return &infoProvided{} })
	workers.ContinuousProvided[*infoProvided]("provided", ContinuousConfig{Tier: credo.TierIngress})
	workers.Continuous("plain", Func(infoBlocking))

	want := []struct {
		name       string
		kind       Kind
		schedule   string
		continuous *ContinuousConfig
		scheduled  *ScheduledConfig
	}{
		{"consumer", KindContinuous, "", &ContinuousConfig{
			Tier:              credo.TierInternal,
			Restart:           Restart{Limit: 4, MinDelay: 2 * time.Second, MaxDelay: DefaultMaxRestartDelay},
			UnreadyWhenFailed: true,
		}, nil},
		{"report", KindScheduled, "@every 1h", nil, &ScheduledConfig{
			Tier:                     credo.TierIngress,
			RunOnStart:               true,
			RunTimeout:               time.Minute,
			MaxConsecutiveFailures:   3,
			UnreadyUntilFirstSuccess: true,
			UnreadyAfterSuccessAge:   2 * time.Hour,
		}},
		{"provided", KindContinuous, "", &ContinuousConfig{
			Tier:    credo.TierIngress,
			Restart: Restart{MinDelay: DefaultMinRestartDelay, MaxDelay: DefaultMaxRestartDelay},
		}, nil},
		{"plain", KindContinuous, "", &ContinuousConfig{
			Tier:    credo.TierInternal,
			Restart: Restart{MinDelay: DefaultMinRestartDelay, MaxDelay: DefaultMaxRestartDelay},
		}, nil},
	}
	check := func(phase string, wantStatus Status) {
		t.Helper()
		infos := workers.Snapshot()
		if len(infos) != len(want) {
			t.Fatalf("%s: Snapshot() = %d entries, want %d", phase, len(infos), len(want))
		}
		for i, w := range want {
			got := infos[i]
			if got.Name != w.name || got.Kind != w.kind || got.Schedule != w.schedule {
				t.Errorf("%s: entry %d = %q/%s/%q, want %q/%s/%q",
					phase, i, got.Name, got.Kind, got.Schedule, w.name, w.kind, w.schedule)
			}
			if (got.Continuous == nil) == (got.Scheduled == nil) {
				t.Errorf("%s: %s: Continuous = %v, Scheduled = %v, want exactly one set",
					phase, w.name, got.Continuous, got.Scheduled)
			}
			if !reflect.DeepEqual(got.Continuous, w.continuous) || !reflect.DeepEqual(got.Scheduled, w.scheduled) {
				t.Errorf("%s: %s: configuration = %+v / %+v, want %+v / %+v",
					phase, w.name, got.Continuous, got.Scheduled, w.continuous, w.scheduled)
			}
			if wantStatus != "" && got.Status != wantStatus {
				t.Errorf("%s: %s: status = %q, want %q", phase, w.name, got.Status, wantStatus)
			}
		}
	}

	check("registered", StatusPending)
	finalize(t, app)
	check("finalized", StatusPending)

	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("App.Start() = %v", err)
	}
	check("started", "")
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("App.Shutdown() = %v", err)
	}
	check("shut down", StatusStopped)
}

func TestSnapshot_EachSnapshotOwnsItsConfiguration(t *testing.T) {
	s := newTestSupervisor()
	addWorker(s, continuousDef("consumer", ContinuousConfig{Restart: Restart{Limit: 3}}), Func(infoBlocking))
	addWorker(s, scheduledDef("report", "@every 1m", ScheduledConfig{RunTimeout: time.Minute}), Func(infoBlocking))

	first := s.Snapshot()
	first[0].Continuous.Restart.Limit = 99
	first[0].Continuous.Tier = credo.TierIngress
	first[1].Scheduled.RunTimeout = time.Hour

	second := s.Snapshot()
	looked, _ := s.Lookup("consumer")
	switch {
	case second[0].Continuous == first[0].Continuous || looked.Continuous == first[0].Continuous:
		t.Fatal("two snapshots share one *ContinuousConfig")
	case second[1].Scheduled == first[1].Scheduled:
		t.Fatal("two snapshots share one *ScheduledConfig")
	case second[0].Continuous.Restart.Limit != 3 || second[0].Continuous.Tier != credo.TierInternal ||
		looked.Continuous.Restart.Limit != 3:
		t.Fatalf("a snapshot mutation reached the supervisor: %+v", *second[0].Continuous)
	case second[1].Scheduled.RunTimeout != time.Minute:
		t.Fatalf("a snapshot mutation reached the supervisor: %+v", *second[1].Scheduled)
	case s.components[0].def.continuous.Restart.Limit != 3:
		t.Fatal("a snapshot mutation reached the definition")
	}
}

func TestSnapshot_CountersPerKind(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		s := newTestSupervisor()
		var continuousRuns, scheduledRuns atomic.Int32
		consumer := addWorker(s, continuousDef("consumer", ContinuousConfig{
			Restart: Restart{MinDelay: time.Second, MaxDelay: time.Second},
		}), Func(func(ctx context.Context) error {
			if continuousRuns.Add(1) <= 2 {
				return errors.New("boom")
			}
			return infoBlocking(ctx)
		}))
		report := addWorker(s, scheduledDef("report", "@every 1m"), Func(func(context.Context) error {
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

func TestLookup_ExactName(t *testing.T) {
	s := newTestSupervisor()
	addWorker(s, continuousDef("consumer"), Func(infoBlocking))
	addWorker(s, scheduledDef("report", "@every 1m"), Func(infoBlocking))

	for _, name := range []string{"consumer", "report"} {
		info, ok := s.Lookup(name)
		if !ok || info.Name != name {
			t.Errorf("Lookup(%q) = %+v, %t, want the worker", name, info, ok)
		}
	}
	for _, name := range []string{"", "unknown", "Consumer", " consumer", "consumer ", "worker:consumer", "repor"} {
		info, ok := s.Lookup(name)
		if ok || !reflect.DeepEqual(info, Info{}) {
			t.Errorf("Lookup(%q) = %+v, %t, want a zero Info and false", name, info, ok)
		}
	}
}

// TestInfo_JSONShape pins the wire contract of Snapshot under Credo's
// response profile against the spec's example: snake_case names throughout,
// the tier as text, durations as integer nanoseconds, zero schedule,
// configuration, timestamps and errors omitted.
func TestInfo_JSONShape(t *testing.T) {
	s := newTestSupervisor()
	addWorker(s, continuousDef("consumer", ContinuousConfig{
		Restart:           Restart{Limit: 5},
		UnreadyWhenFailed: true,
	}), Func(infoBlocking))
	report := addWorker(s, scheduledDef("report", "@every 1m", ScheduledConfig{
		RunTimeout:             15 * time.Second,
		MaxConsecutiveFailures: 3,
	}), Func(infoBlocking))
	// One failed run, as the runner records it.
	report.r.update(func(st *runState) {
		st.status = StatusWaiting
		st.consecutiveFailures = 1
		st.lastStartedAt = time.Date(2000, 1, 1, 0, 1, 0, 0, time.UTC)
		st.lastError = "boom"
	})

	app := newTestApp(t)
	app.GET("/workers", func(ctx *credo.Context) error {
		return ctx.Response().JSON(http.StatusOK, s.Snapshot())
	})
	startApp(t, app)
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/workers", nil))
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}

	const want = `[` +
		`{"name":"consumer","kind":"continuous",` +
		`"continuous":{"tier":"internal",` +
		`"restart":{"disabled":false,"limit":5,"min_delay":3000000000,"max_delay":60000000000},` +
		`"unready_when_failed":true},` +
		`"status":"pending","restarts":0,"consecutive_failures":0},` +
		`{"name":"report","kind":"scheduled","schedule":"@every 1m",` +
		`"scheduled":{"tier":"ingress","run_on_start":false,"run_timeout":15000000000,` +
		`"max_consecutive_failures":3,"unready_when_failed":false,"unready_until_first_success":false,` +
		`"unready_after_success_age":0},` +
		`"status":"waiting","restarts":0,"consecutive_failures":1,` +
		`"last_started_at":"2000-01-01T00:01:00Z","last_error":"boom"}` +
		`]`
	if got := string(body); got != want {
		t.Fatalf("JSON =\n%s\nwant\n%s", got, want)
	}

	// The same properties, independent of the golden's layout.
	var decoded []map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	consumer, scheduled := decoded[0], decoded[1]
	for _, key := range []string{"schedule", "scheduled", "last_started_at", "last_succeeded_at", "last_error"} {
		if _, ok := consumer[key]; ok {
			t.Errorf("consumer: %s present while zero", key)
		}
	}
	for _, key := range []string{"continuous", "last_succeeded_at"} {
		if _, ok := scheduled[key]; ok {
			t.Errorf("report: %s present while zero", key)
		}
	}
	if tier := consumer["continuous"].(map[string]any)["tier"]; tier != "internal" {
		t.Errorf("continuous tier = %v, want internal", tier)
	}
	scheduledCfg := scheduled["scheduled"].(map[string]any)
	if tier := scheduledCfg["tier"]; tier != "ingress" {
		t.Errorf("scheduled tier = %v, want ingress", tier)
	}
	if timeout := scheduledCfg["run_timeout"]; timeout != float64(15*time.Second) {
		t.Errorf("run_timeout = %v, want integer nanoseconds %d", timeout, 15*time.Second)
	}
}
