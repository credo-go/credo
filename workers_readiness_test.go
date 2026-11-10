package credo_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

// readinessFailing is a worker whose every run fails with "boom".
var readinessFailing = workerFunc(func(context.Context) error { return errors.New("boom") })

// readinessProbe serves GET /ready and returns the status code and the
// decoded body.
func readinessProbe(t *testing.T, app *credo.App) (int, readinessBody) {
	t.Helper()
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ready", nil))
	var body readinessBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("/ready body %q: %v", w.Body.String(), err)
	}
	return w.Code, body
}

type readinessBody struct {
	Status string `json:"status"`
	Checks map[string]struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	} `json:"checks"`
}

func TestWorkersReadiness_WorkerWithoutConditionContributesNothing(t *testing.T) {
	capture := newLogCapture()
	app := mustNew(t, credo.WithLogger(capture.logger()))
	app.UseHealth(credo.HealthConfig{ExposeErrors: true})
	failed := awaitWorkerLog(capture, "worker failed", "quiet")
	app.Workers().Continuous("quiet", readinessFailing,
		credo.ContinuousConfig{Restart: credo.RestartPolicy{Disabled: true}})
	startWorkersApp(t, app)

	workersAwait(t, failed, "the worker to fail")
	code, body := readinessProbe(t, app)
	if code != http.StatusOK {
		t.Fatalf("/ready = %d %+v, want 200: a failed worker without a condition is not a readiness check", code, body)
	}
	if _, ok := body.Checks["worker:quiet"]; ok {
		t.Fatalf("/ready checks = %+v, want no worker:quiet entry", body.Checks)
	}
}

func TestWorkersReadiness_ReportedOnReadyEndpoint(t *testing.T) {
	capture := newLogCapture()
	app := mustNew(t, credo.WithLogger(capture.logger()))
	app.UseHealth(credo.HealthConfig{ExposeErrors: true})
	succeeded := awaitWorkerLog(capture, "worker run completed", "warmup")

	// The first run waits for release, so the App is running before the
	// worker's first success.
	release := make(chan struct{})
	app.Workers().Scheduled("warmup", "@every 1h", workerFunc(func(ctx context.Context) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}), credo.ScheduledConfig{RunOnStart: true, UnreadyUntilFirstSuccess: true})
	startWorkersApp(t, app)

	code, body := readinessProbe(t, app)
	check, ok := body.Checks["worker:warmup"]
	if code != http.StatusServiceUnavailable || !ok || check.Status == "up" {
		t.Fatalf("before the first success: /ready = %d %+v, want 503 with worker:warmup down", code, body)
	}
	if !strings.Contains(check.Error, `worker "warmup" has no successful run yet`) {
		t.Errorf("worker:warmup error = %q, want the readiness failure text", check.Error)
	}

	close(release)
	workersAwait(t, succeeded, "the first success")
	code, body = readinessProbe(t, app)
	if check, ok := body.Checks["worker:warmup"]; code != http.StatusOK || !ok || check.Status != "up" {
		t.Fatalf("after the first success: /ready = %d %+v, want 200 with worker:warmup up", code, body)
	}
}

func TestWorkersReadiness_NameCollisionFailsClosed(t *testing.T) {
	app := mustNew(t, credo.WithLogger(slog.New(slog.DiscardHandler)))
	app.UseHealth()
	app.AddReadinessCheck("worker:dup", func(context.Context) error { return nil })
	app.Workers().Continuous("dup", workerFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	}), credo.ContinuousConfig{UnreadyWhenFailed: true})
	startWorkersApp(t, app)

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "contributed_name_conflict") {
		t.Fatalf("/ready = %d %s, want 503 with a contributed_name_conflict entry", w.Code, w.Body.String())
	}
}
