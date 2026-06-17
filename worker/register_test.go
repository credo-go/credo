package worker

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

func requireErrContaining(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want message containing %q", err, want)
	}
}

// mustPanicContaining asserts fn panics with an error or string containing want.
func mustPanicContaining(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic containing %q", want)
		}
		if !strings.Contains(fmt.Sprint(r), want) {
			t.Fatalf("panic = %v, want message containing %q", r, want)
		}
	}()
	fn()
}

func TestRegister_RejectsCrossModeOptions(t *testing.T) {
	app := newTestApp(t)

	err := Register(app, Func("scheduled", func(context.Context) error { return nil }),
		WithSchedule("@every 1m"),
		WithRestartDelay(time.Second),
	)
	requireErrContaining(t, err, "WithRestartDelay is for continuous workers")

	err = Register(app, Func("continuous", func(context.Context) error { return nil }), WithStartImmediately())
	requireErrContaining(t, err, "WithStartImmediately is for scheduled workers")
}

func TestMustRegister_PanicsOnError(t *testing.T) {
	app := newTestApp(t)

	mustPanicContaining(t, "WithRestartDelay is for continuous workers", func() {
		MustRegister(app, Func("scheduled", func(context.Context) error { return nil }),
			WithSchedule("@every 1m"),
			WithRestartDelay(time.Second),
		)
	})
}

func TestRegister_DuplicateName(t *testing.T) {
	app := newTestApp(t)

	if err := Register(app, Func("dup", func(context.Context) error { return nil })); err != nil {
		t.Fatalf("Register() = %v", err)
	}
	err := Register(app, Func("dup", func(context.Context) error { return nil }))
	requireErrContaining(t, err, "duplicate worker name")

	mustPanicContaining(t, "duplicate worker name", func() {
		MustRegister(app, Func("dup", func(context.Context) error { return nil }))
	})
}

func TestRegister_UsesConfiguredRestartDelay(t *testing.T) {
	app := newTestApp(t, credo.WithRawConfig(fakeRawConfig{
		exists: true,
		worker: poolConfig{RestartDelay: 7 * time.Second},
	}))

	if err := Register(app, Func("job", func(context.Context) error { return nil })); err != nil {
		t.Fatalf("Register() = %v", err)
	}

	pool, err := credo.Resolve[*Pool](app)
	if err != nil {
		t.Fatalf("Resolve[*Pool]() = %v", err)
	}
	if pool.defaultRestartDelay != 7*time.Second {
		t.Fatalf("default restart delay = %s, want 7s", pool.defaultRestartDelay)
	}
	if got := pool.definitions[0].restartPolicy.restartDelay; got != 7*time.Second {
		t.Fatalf("definition restart delay = %s, want 7s", got)
	}
}

func TestPoolWorkers_BeforeStartReturnsIdleSnapshot(t *testing.T) {
	app := newTestApp(t)
	if err := Register(app, Func("idle", func(context.Context) error { return nil })); err != nil {
		t.Fatalf("Register() = %v", err)
	}

	pool, err := credo.Resolve[*Pool](app)
	if err != nil {
		t.Fatalf("Resolve[*Pool]() = %v", err)
	}

	workers := pool.Workers()
	if len(workers) != 1 {
		t.Fatalf("Workers() len = %d, want 1", len(workers))
	}
	if workers[0].Status != StatusIdle {
		t.Fatalf("status = %q, want %q", workers[0].Status, StatusIdle)
	}
	if workers[0].Kind != kindContinuous {
		t.Fatalf("kind = %q, want %q", workers[0].Kind, kindContinuous)
	}
}
