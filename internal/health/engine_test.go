package health

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

func staticStoreFunc(results ...StoreResult) StoreFunc {
	checks := make([]StoreCheck, 0, len(results))
	for _, storeResult := range results {
		checks = append(checks, StoreCheck{
			Name: storeResult.Name,
			Probe: NewProbe(func(context.Context) Result {
				return Result{
					Status:  storeResult.Status,
					Latency: storeResult.Latency,
					Cause:   storeResult.Cause,
				}
			}),
		})
	}
	return func() []StoreCheck {
		return slices.Clone(checks)
	}
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
	e.AddReadiness("dep1", func(context.Context) error { return nil })

	status, checks, stores := e.CheckReadiness(t.Context(), nil, nil)
	if status != "up" {
		t.Errorf("status = %q, want %q", status, "up")
	}
	if len(checks) != 1 {
		t.Fatalf("checks = %d, want 1", len(checks))
	}
	if checks[0].Status != "up" {
		t.Errorf("check status = %q, want %q", checks[0].Status, "up")
	}
	if len(stores) != 0 {
		t.Errorf("stores = %d, want 0", len(stores))
	}
}

func TestEngine_ReadinessFail(t *testing.T) {
	e := NewEngine(5 * time.Second)
	e.AddReadiness("ok", func(context.Context) error { return nil })
	e.AddReadiness("bad", func(context.Context) error { return errors.New("not ready") })

	status, _, _ := e.CheckReadiness(t.Context(), nil, nil)
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

func TestEngine_StoreFunc(t *testing.T) {
	e := NewEngine(5 * time.Second)
	storeFn := staticStoreFunc(
		StoreResult{Name: "postgres", Status: "up", Latency: 2 * time.Millisecond},
		StoreResult{Name: "redis", Status: "up", Latency: time.Millisecond},
	)

	status, _, stores := e.CheckReadiness(t.Context(), storeFn, nil)
	if status != "up" {
		t.Errorf("status = %q, want %q", status, "up")
	}
	if len(stores) != 2 {
		t.Fatalf("stores = %d, want 2", len(stores))
	}
}

func TestEngine_StoreFunc_Down(t *testing.T) {
	e := NewEngine(5 * time.Second)
	storeFn := staticStoreFunc(StoreResult{Name: "postgres", Status: "down"})

	status, _, _ := e.CheckReadiness(t.Context(), storeFn, nil)
	if status != "down" {
		t.Errorf("status = %q, want %q", status, "down")
	}
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
