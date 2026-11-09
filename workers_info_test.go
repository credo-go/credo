package credo

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	internalworker "github.com/credo-go/credo/internal/worker"
)

// infoWorkerFunc adapts a function to Worker.
type infoWorkerFunc func(context.Context) error

func (f infoWorkerFunc) Run(ctx context.Context) error { return f(ctx) }

// infoBlocking is a continuous body that runs until its context ends.
func infoBlocking(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// infoProvided is a worker its constructor builds.
type infoProvided struct{}

func (*infoProvided) Run(ctx context.Context) error { return infoBlocking(ctx) }

// infoNewApp returns an App that logs nothing.
func infoNewApp(t *testing.T) *App {
	t.Helper()
	app, err := New(WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	return app
}

func TestWorkersSnapshot_ResolvedConfigurationBeforeAndAfterStart(t *testing.T) {
	app := infoNewApp(t)
	workers := app.Workers()
	workers.Continuous("consumer", infoWorkerFunc(infoBlocking), ContinuousConfig{
		Restart:           RestartPolicy{Limit: 4, MinDelay: 2 * time.Second},
		UnreadyWhenFailed: true,
	})
	workers.Scheduled("report", "@every 1h", infoWorkerFunc(func(context.Context) error { return nil }),
		ScheduledConfig{
			RunOnStart:               true,
			RunTimeout:               time.Minute,
			MaxConsecutiveFailures:   3,
			UnreadyUntilFirstSuccess: true,
			UnreadyAfterSuccessAge:   2 * time.Hour,
		})
	workers.Continuous("provided", func() *infoProvided { return &infoProvided{} },
		ContinuousConfig{Tier: TierIngress})
	workers.Continuous("plain", infoWorkerFunc(infoBlocking))

	want := []struct {
		name       string
		kind       WorkerKind
		schedule   string
		continuous *ContinuousConfig
		scheduled  *ScheduledConfig
	}{
		{"consumer", WorkerKindContinuous, "", &ContinuousConfig{
			Tier:              TierInternal,
			Restart:           RestartPolicy{Limit: 4, MinDelay: 2 * time.Second, MaxDelay: DefaultMaxRestartDelay},
			UnreadyWhenFailed: true,
		}, nil},
		{"report", WorkerKindScheduled, "@every 1h", nil, &ScheduledConfig{
			Tier:                     TierIngress,
			RunOnStart:               true,
			RunTimeout:               time.Minute,
			MaxConsecutiveFailures:   3,
			UnreadyUntilFirstSuccess: true,
			UnreadyAfterSuccessAge:   2 * time.Hour,
		}},
		{"provided", WorkerKindContinuous, "", &ContinuousConfig{
			Tier:    TierIngress,
			Restart: RestartPolicy{MinDelay: DefaultMinRestartDelay, MaxDelay: DefaultMaxRestartDelay},
		}, nil},
		{"plain", WorkerKindContinuous, "", &ContinuousConfig{
			Tier:    TierInternal,
			Restart: RestartPolicy{MinDelay: DefaultMinRestartDelay, MaxDelay: DefaultMaxRestartDelay},
		}, nil},
	}
	check := func(phase string, wantStatus WorkerStatus) {
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

	check("registered", WorkerStatusPending)
	if err := app.Finalize(); err != nil {
		t.Fatalf("Finalize() = %v", err)
	}
	check("finalized", WorkerStatusPending)

	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("App.Start() = %v", err)
	}
	check("started", "")
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("App.Shutdown() = %v", err)
	}
	check("shut down", WorkerStatusStopped)
}

func TestWorkersSnapshot_EachSnapshotOwnsItsConfiguration(t *testing.T) {
	s := infoNewApp(t).Workers()
	s.Continuous("consumer", infoWorkerFunc(infoBlocking), ContinuousConfig{Restart: RestartPolicy{Limit: 3}})
	s.Scheduled("report", "@every 1m", infoWorkerFunc(infoBlocking), ScheduledConfig{RunTimeout: time.Minute})

	first := s.Snapshot()
	first[0].Continuous.Restart.Limit = 99
	first[0].Continuous.Tier = TierIngress
	first[1].Scheduled.RunTimeout = time.Hour
	first[1].Scheduled.Tier = TierInternal

	second := s.Snapshot()
	looked, _ := s.Lookup("consumer")
	switch {
	case second[0].Continuous == first[0].Continuous || looked.Continuous == first[0].Continuous:
		t.Fatal("two snapshots share one *ContinuousConfig")
	case second[1].Scheduled == first[1].Scheduled:
		t.Fatal("two snapshots share one *ScheduledConfig")
	case second[0].Continuous.Restart.Limit != 3 || second[0].Continuous.Tier != TierInternal ||
		looked.Continuous.Restart.Limit != 3:
		t.Fatalf("a snapshot mutation reached the registry: %+v", *second[0].Continuous)
	case second[1].Scheduled.RunTimeout != time.Minute || second[1].Scheduled.Tier != TierIngress:
		t.Fatalf("a snapshot mutation reached the registry: %+v", *second[1].Scheduled)
	case s.workers[0].c.Definition().Continuous().Restart.Limit != 3:
		t.Fatal("a snapshot mutation reached the definition")
	}
}

func TestWorkersLookup_ExactName(t *testing.T) {
	s := infoNewApp(t).Workers()
	s.Continuous("consumer", infoWorkerFunc(infoBlocking))
	s.Scheduled("report", "@every 1m", infoWorkerFunc(infoBlocking))

	for _, name := range []string{"consumer", "report"} {
		info, ok := s.Lookup(name)
		if !ok || info.Name != name {
			t.Errorf("Lookup(%q) = %+v, %t, want the worker", name, info, ok)
		}
	}
	for _, name := range []string{"", "unknown", "Consumer", " consumer", "consumer ", "worker:consumer", "repor"} {
		info, ok := s.Lookup(name)
		if ok || !reflect.DeepEqual(info, WorkerInfo{}) {
			t.Errorf("Lookup(%q) = %+v, %t, want a zero WorkerInfo and false", name, info, ok)
		}
	}
}

// TestWorkerInfo_JSONShape pins the wire contract of Snapshot under Credo's
// response profile against the spec's example: snake_case names throughout,
// the tier as text, durations as integer nanoseconds, zero schedule,
// configuration, timestamps and errors omitted.
func TestWorkerInfo_JSONShape(t *testing.T) {
	s := infoNewApp(t).Workers()
	s.Continuous("consumer", infoWorkerFunc(infoBlocking), ContinuousConfig{
		Restart:           RestartPolicy{Limit: 5},
		UnreadyWhenFailed: true,
	})
	s.Scheduled("report", "@every 1m", infoWorkerFunc(infoBlocking), ScheduledConfig{
		RunTimeout:             15 * time.Second,
		MaxConsecutiveFailures: 3,
	})
	infos := s.Snapshot()
	// One failed run of report, as the engine reports it.
	report := s.workers[1].c.Snapshot()
	report.Status = internalworker.StatusWaiting
	report.ConsecutiveFailures = 1
	report.LastStartedAt = time.Date(2000, 1, 1, 0, 1, 0, 0, time.UTC)
	report.LastError = "boom"
	infos[1] = workerInfo(report, s.workers[1].tier)

	app := infoNewApp(t)
	app.GET("/workers", func(ctx *Context) error {
		return ctx.Response().JSON(http.StatusOK, infos)
	})
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
