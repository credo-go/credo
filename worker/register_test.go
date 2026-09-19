package worker

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
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

	err := Register(app, "scheduled", Func(func(context.Context) error { return nil }),
		WithSchedule("@every 1m"),
		WithRestartDelay(time.Second),
	)
	requireErrContaining(t, err, "WithRestartDelay is for continuous workers")

	err = Register(app, "continuous", Func(func(context.Context) error { return nil }), WithStartImmediately())
	requireErrContaining(t, err, "WithStartImmediately is for scheduled workers")
}

func TestMustRegister_PanicsOnError(t *testing.T) {
	app := newTestApp(t)

	mustPanicContaining(t, "WithRestartDelay is for continuous workers", func() {
		MustRegister(app, "scheduled", Func(func(context.Context) error { return nil }),
			WithSchedule("@every 1m"),
			WithRestartDelay(time.Second),
		)
	})
}

func TestRegister_DuplicateName(t *testing.T) {
	app := newTestApp(t)

	if err := Register(app, "dup", Func(func(context.Context) error { return nil })); err != nil {
		t.Fatalf("Register() = %v", err)
	}
	err := Register(app, "dup", Func(func(context.Context) error { return nil }))
	requireErrContaining(t, err, "duplicate worker name")

	mustPanicContaining(t, "duplicate worker name", func() {
		MustRegister(app, "dup", Func(func(context.Context) error { return nil }))
	})
}
