package worker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/credo-go/credo"
)

// scheduledOnce is a scheduled worker body that succeeds.
func scheduledOnce() Func {
	return func(context.Context) error { return nil }
}

func TestPoolWorkers_ConfigRoundTripsBeforeAndAfterStart(t *testing.T) {
	app := newTestApp(t)
	MustRegister(app, "consumer", blockingFunc(),
		WithMaxRestarts(4),
		WithRestartDelay(2*time.Second),
		WithReadiness(ReadinessPolicy{FailWhenFailed: true}),
	)
	MustRegister(app, "report", scheduledOnce(),
		WithSchedule("@every 1h"),
		WithStartImmediately(),
		WithRunTimeout(time.Minute),
		WithMaxConsecutiveFailures(3),
		WithReadiness(ReadinessPolicy{RequireFirstSuccess: true, MaxSuccessAge: 2 * time.Hour}),
	)
	MustRegister(app, "plain", blockingFunc())
	finalize(t, app)
	pool, err := app.Resolve[*Pool]()
	if err != nil {
		t.Fatal(err)
	}

	want := []struct {
		name   string
		kind   Kind
		config Config
	}{
		{"consumer", KindContinuous, Config{
			MaxRestarts:     4,
			RestartDelay:    2 * time.Second,
			MaxRestartDelay: DefaultMaxRestartDelay,
			Readiness:       &ReadinessPolicy{FailWhenFailed: true},
		}},
		{"report", KindScheduled, Config{
			Schedule:               "@every 1h",
			StartImmediately:       true,
			RunTimeout:             time.Minute,
			MaxConsecutiveFailures: 3,
			Readiness:              &ReadinessPolicy{RequireFirstSuccess: true, MaxSuccessAge: 2 * time.Hour},
		}},
		{"plain", KindContinuous, Config{RestartDelay: DefaultRestartDelay, MaxRestartDelay: DefaultMaxRestartDelay}},
	}
	check := func(phase string, infos []Info) {
		t.Helper()
		if len(infos) != len(want) {
			t.Fatalf("%s: Workers() = %d entries, want %d", phase, len(infos), len(want))
		}
		for i, w := range want {
			got := infos[i]
			if got.Name != w.name || got.Kind != w.kind {
				t.Errorf("%s: worker %d = %s/%s, want %s/%s", phase, i, got.Name, got.Kind, w.name, w.kind)
			}
			if !reflect.DeepEqual(got.Config, w.config) {
				t.Errorf("%s: %s Config = %+v, want %+v", phase, w.name, got.Config, w.config)
			}
		}
	}

	before := pool.Workers()
	check("before Start", before)
	for _, info := range before {
		if info.Status != StatusIdle {
			t.Errorf("before Start: %s status = %q, want idle", info.Name, info.Status)
		}
	}

	if err = pool.Start(t.Context()); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	awaitCondition(t, "the startup run of report", func() bool {
		return !pool.Workers()[1].LastSuccess.IsZero()
	})
	check("after Start", pool.Workers())
	shutdownPool(t, pool)
	check("after Shutdown", pool.Workers())
}

func TestRegister_EffectiveRestartDelay(t *testing.T) {
	tests := []struct {
		name   string
		config *time.Duration // nil: no worker section
		opts   []Option
		want   time.Duration
	}{
		{"default", nil, nil, DefaultRestartDelay},
		{"pool config", new(7 * time.Second), nil, 7 * time.Second},
		{"option overrides config", new(7 * time.Second), []Option{WithRestartDelay(2 * time.Second)}, 2 * time.Second},
		{"explicit zero option means the default", new(7 * time.Second), []Option{WithRestartDelay(0)}, DefaultRestartDelay},
		{"zero pool config means the default", new(time.Duration(0)), nil, DefaultRestartDelay},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var opts []credo.Option
			if tt.config != nil {
				opts = append(opts, credo.WithRawConfig(fakeRawConfig{
					exists: true,
					worker: poolConfig{RestartDelay: *tt.config},
				}))
			}
			app := newTestApp(t, opts...)
			MustRegister(app, "job", blockingFunc(), tt.opts...)
			finalize(t, app)
			pool, err := app.Resolve[*Pool]()
			if err != nil {
				t.Fatal(err)
			}
			if got := pool.Workers()[0].Config.RestartDelay; got != tt.want {
				t.Fatalf("Config.RestartDelay = %s, want %s", got, tt.want)
			}
			if got := pool.definitions[0].restartPolicy.restartDelay; got != tt.want {
				t.Fatalf("runner restart delay = %s, want %s (Config must report what the runner uses)", got, tt.want)
			}
		})
	}
}

func TestPoolWorkers_ConfigReadinessIsACopy(t *testing.T) {
	app := newTestApp(t)
	MustRegister(app, "critical", blockingFunc(), WithReadiness(ReadinessPolicy{FailWhenFailed: true}))
	finalize(t, app)
	pool, err := app.Resolve[*Pool]()
	if err != nil {
		t.Fatal(err)
	}

	first := pool.Workers()[0].Config.Readiness
	first.FailWhenFailed = false
	first.MaxSuccessAge = time.Hour

	second := pool.Workers()[0].Config.Readiness
	if second == first {
		t.Fatal("two snapshots share one *ReadinessPolicy")
	}
	if !second.FailWhenFailed || second.MaxSuccessAge != 0 {
		t.Fatalf("snapshot mutation reached the definition: %+v", *second)
	}
	if !pool.definitions[0].readiness.FailWhenFailed {
		t.Fatal("snapshot mutation reached the definition's policy")
	}
}

// TestInfo_JSONShape pins the wire contract of Pool.Workers under Credo's
// response profile: snake_case names throughout, zero timestamps, errors and
// readiness omitted, durations as integer nanoseconds.
func TestInfo_JSONShape(t *testing.T) {
	app := newTestApp(t)
	MustRegister(app, "consumer", blockingFunc(),
		WithMaxRestarts(5),
		WithReadiness(ReadinessPolicy{FailWhenFailed: true}),
	)
	finalize(t, app)
	pool, err := app.Resolve[*Pool]()
	if err != nil {
		t.Fatal(err)
	}
	idle := pool.Workers()[0]

	var failed Info
	synctest.Test(t, func(t *testing.T) {
		p := newTestPool()
		o, schedule, err := validateOptions([]Option{
			WithSchedule("@every 1m"), WithMaxConsecutiveFailures(3), WithRunTimeout(15 * time.Second),
		})
		if err != nil {
			t.Fatal(err)
		}
		def, err := buildDefinition("report", o, schedule, poolConfig{})
		if err != nil {
			t.Fatal(err)
		}
		def.resolve = instance(Func(func(context.Context) error { return errors.New("boom") }))
		if err := p.addDefinition(def); err != nil {
			t.Fatal(err)
		}
		if err := p.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		failed = p.Workers()[0]
		shutdownPool(t, p)
	})
	// The bubble's clock starts at 2000-01-01 00:00 UTC; normalize the zone
	// so the golden does not depend on the machine's local time zone.
	failed.LastRun = failed.LastRun.UTC()

	encoder := newTestApp(t)
	encoder.GET("/workers", func(ctx *credo.Context) error {
		return ctx.Response().JSON(http.StatusOK, []Info{idle, failed})
	})
	rec := httptest.NewRecorder()
	encoder.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/workers", nil))

	const want = `[` +
		`{"name":"consumer","kind":"continuous","config":{"schedule":"","start_immediately":false,` +
		`"run_timeout":0,"max_consecutive_failures":0,"max_restarts":5,"restart_delay":3000000000,` +
		`"max_restart_delay":60000000000,` +
		`"readiness":{"require_first_success":false,"fail_when_failed":true,"max_success_age":0}},` +
		`"status":"idle","restarts":0,"consecutive_failures":0},` +
		`{"name":"report","kind":"scheduled","config":{"schedule":"@every 1m","start_immediately":false,` +
		`"run_timeout":15000000000,"max_consecutive_failures":3,"max_restarts":0,"restart_delay":0,` +
		`"max_restart_delay":0},` +
		`"status":"waiting","restarts":0,"consecutive_failures":1,` +
		`"last_run":"2000-01-01T00:01:00Z","last_error":"boom"}` +
		`]`
	if got := rec.Body.String(); got != want {
		t.Fatalf("JSON =\n%s\nwant\n%s", got, want)
	}
}
