package health

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

// contributedFunc returns one contributed check per error: nil is up.
func contributedFunc(optional bool, errs map[string]error) ReadinessFunc {
	checks := make([]ReadinessCheck, 0, len(errs))
	for name, err := range errs {
		checks = append(checks, ReadinessCheck{
			Name:     name,
			Optional: optional,
			Probe: NewProbe(func(context.Context) Result {
				if err != nil {
					return FailureResult(err)
				}
				return SuccessResult()
			}),
		})
	}
	return func() []ReadinessCheck { return slices.Clone(checks) }
}

func TestEngine_NoChecks_LivenessUp(t *testing.T) {
	e := NewEngine(5 * time.Second)
	status, checks := e.CheckLiveness(t.Context())
	if status != "up" {
		t.Errorf("status = %q, want %q", status, "up")
	}
	if len(checks) != 0 {
		t.Errorf("checks = %d, want 0", len(checks))
	}
}

func TestEngine_LivenessPass(t *testing.T) {
	e := NewEngine(5 * time.Second)
	e.AddLiveness("ok1", func(context.Context) error { return nil })
	e.AddLiveness("ok2", func(context.Context) error { return nil })

	status, checks := e.CheckLiveness(t.Context())
	if status != "up" {
		t.Errorf("status = %q, want %q", status, "up")
	}
	if len(checks) != 2 {
		t.Fatalf("checks = %d, want 2", len(checks))
	}
	for _, c := range checks {
		if c.Status != "up" {
			t.Errorf("check %q status = %q, want %q", c.Name, c.Status, "up")
		}
		if c.Error != "" {
			t.Errorf("check %q error = %q, want empty", c.Name, c.Error)
		}
	}
}

func TestEngine_LivenessFail(t *testing.T) {
	e := NewEngine(5 * time.Second)
	e.AddLiveness("ok", func(context.Context) error { return nil })
	e.AddLiveness("bad", func(context.Context) error { return errors.New("boom") })

	status, checks := e.CheckLiveness(t.Context())
	if status != "down" {
		t.Errorf("status = %q, want %q", status, "down")
	}
	if len(checks) != 2 {
		t.Fatalf("checks = %d, want 2", len(checks))
	}
	// Find the failing check.
	var found bool
	for _, c := range checks {
		if c.Name == "bad" {
			found = true
			if c.Status != "down" {
				t.Errorf("check %q status = %q, want %q", c.Name, c.Status, "down")
			}
			if c.Error != "boom" {
				t.Errorf("check %q error = %q, want %q", c.Name, c.Error, "boom")
			}
		}
	}
	if !found {
		t.Error("did not find check named 'bad'")
	}
}

func TestEngine_ReadinessPass(t *testing.T) {
	e := NewEngine(5 * time.Second)
	e.AddReadiness("dep1", func(context.Context) error { return nil }, false)

	status, checks := e.CheckReadiness(t.Context(), nil)
	if status != "up" {
		t.Errorf("status = %q, want %q", status, "up")
	}
	if len(checks) != 1 {
		t.Fatalf("checks = %d, want 1", len(checks))
	}
	if checks[0].Status != "up" {
		t.Errorf("check status = %q, want %q", checks[0].Status, "up")
	}
}

func TestEngine_ReadinessFail(t *testing.T) {
	e := NewEngine(5 * time.Second)
	e.AddReadiness("ok", func(context.Context) error { return nil }, false)
	e.AddReadiness("bad", func(context.Context) error { return errors.New("not ready") }, false)

	status, _ := e.CheckReadiness(t.Context(), nil)
	if status != "down" {
		t.Errorf("status = %q, want %q", status, "down")
	}
}

func TestEngine_ConcurrentExecution(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(10 * time.Second)
		for index, delay := range []time.Duration{2 * time.Second, 5 * time.Second, 3 * time.Second} {
			name := string(rune('a' + index))
			e.AddLiveness(name, func(context.Context) error {
				time.Sleep(delay)
				return nil
			})
		}

		start := time.Now()
		status, _ := e.CheckLiveness(t.Context())
		if elapsed := time.Since(start); elapsed != 5*time.Second {
			t.Errorf("elapsed = %v, want 5s (maximum check duration)", elapsed)
		}
		if status != "up" {
			t.Errorf("status = %q, want %q", status, "up")
		}
	})
}

func TestEngine_Timeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(time.Second)
		e.AddLiveness("slow", func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})

		start := time.Now()
		status, checks := e.CheckLiveness(t.Context())
		if elapsed := time.Since(start); elapsed != time.Second {
			t.Errorf("elapsed = %v, want 1s", elapsed)
		}
		if status != "down" {
			t.Errorf("status = %q, want %q", status, "down")
		}
		if len(checks) != 1 || checks[0].Status != "down" || checks[0].Error == "" {
			t.Fatalf("checks = %#v, want one timed-out down result", checks)
		}
	})
}

// TestEngine_OptionalReadiness: a failing optional check, named or
// contributed, is reported down while readiness is "degraded"; a failing
// gating check beside it makes readiness "down".
func TestEngine_OptionalReadiness(t *testing.T) {
	for _, tc := range []struct {
		name    string
		gating  error
		optName string
		want    string
	}{
		{"named optional fails", nil, "cache", "degraded"},
		{"contributed optional fails", nil, "*app.Cache", "degraded"},
		{"gating fails too", errors.New("db down"), "cache", "down"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEngine(5 * time.Second)
			e.AddReadiness("db", func(context.Context) error { return tc.gating }, false)
			var contributed ReadinessFunc
			if tc.optName == "cache" {
				e.AddReadiness("cache", func(context.Context) error { return errors.New("cache down") }, true)
			} else {
				contributed = contributedFunc(true, map[string]error{tc.optName: errors.New("cache down")})
			}
			status, checks := e.CheckReadiness(t.Context(), contributed)
			if status != tc.want {
				t.Errorf("status = %q, want %q", status, tc.want)
			}
			for _, c := range checks {
				if c.Name == tc.optName && (c.Status != "down" || !c.Optional || c.Error != "cache down") {
					t.Errorf("optional entry = %+v, want down, optional, with its error", c)
				}
			}
		})
	}
	t.Run("optional recovers", func(t *testing.T) {
		e := NewEngine(5 * time.Second)
		e.AddReadiness("cache", func(context.Context) error { return nil }, true)
		if status, _ := e.CheckReadiness(t.Context(), nil); status != "up" {
			t.Errorf("status = %q, want up", status)
		}
	})
}

// TestEngine_EveryResultCarriesItsLatency: a check's result carries the time
// its run took, and a check that outlives its deadline the time until the
// deadline; a contributed check's included.
func TestEngine_EveryResultCarriesItsLatency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := NewEngine(time.Second)
		e.AddReadiness("quick", func(context.Context) error {
			time.Sleep(200 * time.Millisecond)
			return nil
		}, false)
		e.AddReadiness("slow", func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}, false)
		contributed := func() []ReadinessCheck {
			return []ReadinessCheck{{Name: "component", Probe: NewProbe(func(context.Context) Result {
				time.Sleep(300 * time.Millisecond)
				return SuccessResult()
			})}}
		}
		_, checks := e.CheckReadiness(t.Context(), contributed)
		want := map[string]time.Duration{"quick": 200 * time.Millisecond, "slow": time.Second,
			"component": 300 * time.Millisecond}
		for _, c := range checks {
			if c.Latency != want[c.Name] {
				t.Errorf("%s: latency = %v, want %v", c.Name, c.Latency, want[c.Name])
			}
		}
		if len(checks) != 3 {
			t.Fatalf("checks = %+v, want three", checks)
		}
	})
}

func TestEngine_CheckPanic(t *testing.T) {
	e := NewEngine(5 * time.Second)
	e.AddLiveness("ok", func(context.Context) error { return nil })
	e.AddLiveness("panicker", func(context.Context) error {
		panic("segfault simulation")
	})

	status, checks := e.CheckLiveness(t.Context())
	if status != "down" {
		t.Errorf("status = %q, want %q", status, "down")
	}
	if len(checks) != 2 {
		t.Fatalf("checks = %d, want 2", len(checks))
	}
	for _, c := range checks {
		if c.Name == "panicker" {
			if c.Status != "down" {
				t.Errorf("panicker status = %q, want %q", c.Status, "down")
			}
			if c.Error != "panic: segfault simulation" {
				t.Errorf("panicker error = %q, want %q", c.Error, "panic: segfault simulation")
			}
		}
	}
}

func TestEngine_ContextCancellation(t *testing.T) {
	e := NewEngine(5 * time.Second)
	e.AddLiveness("ctx-check", func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return nil
		}
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel immediately

	status, checks := e.CheckLiveness(ctx)
	if status != "down" {
		t.Errorf("status = %q, want %q", status, "down")
	}
	if len(checks) != 1 {
		t.Fatalf("checks = %d, want 1", len(checks))
	}
	if checks[0].Status != "down" {
		t.Errorf("check status = %q, want %q", checks[0].Status, "down")
	}
}
