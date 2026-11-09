package credo_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

// loggingSucceed is a scheduled body that succeeds.
func loggingSucceed(context.Context) error { return nil }

// loggingFail is a body that fails with "boom".
func loggingFail(context.Context) error { return errors.New("boom") }

// loggingFailingStart is a component whose Start fails.
type loggingFailingStart struct{}

func (*loggingFailingStart) Start(context.Context) error { return errors.New("database unreachable") }

func (*loggingFailingStart) Shutdown(context.Context) error { return nil }

func TestWorkersLogging_FailedStartPhaseLogsNothingForUnstartedWorkers(t *testing.T) {
	logs := newLogCapture()
	app := mustNew(t, credo.WithLogger(logs.logger()))
	app.Manage(&loggingFailingStart{}, credo.ComponentName("database"))
	workers := app.Workers()
	// Ingress: it would start after the internal tier, which fails first.
	workers.Scheduled("report", "@every 1h", workerFunc(loggingSucceed), credo.ScheduledConfig{RunOnStart: true})

	if err := app.Start(t.Context()); err == nil {
		t.Fatal("App.Start() = nil, want the database's start failure")
	}
	// The App's own records may name worker:report; the worker's loop, whose
	// records carry module=worker, writes none.
	for _, entry := range logs.all() {
		if strings.HasPrefix(entry.Message, "worker ") || entry.Attrs["module"] == "worker" ||
			entry.Attrs["worker"] != nil {
			t.Errorf("unstarted worker logged %q %v", entry.Message, entry.Attrs)
		}
	}
	if info, _ := workers.Lookup("report"); info.Status != credo.WorkerStatusPending {
		t.Errorf("status = %q, want pending", info.Status)
	}
}

// TestWorkersLogging_RecordsCarryComponentAndModule: the records of a worker
// the App starts carry component=worker:<name> from the worker's component
// and module=worker from the registry's logger, and no worker key.
func TestWorkersLogging_RecordsCarryComponentAndModule(t *testing.T) {
	logs := newLogCapture()
	stopped := awaitWorkerLog(logs, "worker stopped", "consumer")
	app := mustNew(t, credo.WithLogger(logs.logger()))
	app.Workers().Continuous("consumer", workerFunc(loggingFail),
		credo.ContinuousConfig{Restart: credo.RestartPolicy{Disabled: true}})
	startWorkersApp(t, app)
	workersAwait(t, stopped, "the worker stopped line")

	want := []string{"worker started", "worker run failed", "worker failed", "worker stopped"}
	var got []string
	for _, entry := range logs.all() {
		if !strings.HasPrefix(entry.Message, "worker ") {
			continue
		}
		got = append(got, entry.Message)
		if c := entry.Attrs["component"]; c != "worker:consumer" {
			t.Errorf("%q: component = %v, want worker:consumer", entry.Message, c)
		}
		if m := entry.Attrs["module"]; m != "worker" {
			t.Errorf("%q: module = %v, want worker", entry.Message, m)
		}
		if v, ok := entry.Attrs["worker"]; ok {
			t.Errorf("%q: worker = %v present, want no worker key", entry.Message, v)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("lines = %q, want %q", got, want)
	}
}
