package credo

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	internalhealth "github.com/credo-go/credo/internal/health"
)

func staticStoreFunc(results ...internalhealth.StoreResult) internalhealth.StoreFunc {
	checks := make([]internalhealth.StoreCheck, 0, len(results))
	for _, storeResult := range results {
		checks = append(checks, internalhealth.StoreCheck{
			Name: storeResult.Name,
			Probe: internalhealth.NewProbe(func(context.Context) internalhealth.Result {
				return internalhealth.Result{
					Status:  storeResult.Status,
					Latency: storeResult.Latency,
					Cause:   storeResult.Cause,
				}
			}),
		})
	}
	return func() []internalhealth.StoreCheck {
		return slices.Clone(checks)
	}
}

func TestHealthEngine_NoChecks_LivenessUp(t *testing.T) {
	e := newHealthEngine(5 * time.Second)
	status, checks := e.checkLiveness(t.Context())
	if status != "up" {
		t.Errorf("status = %q, want %q", status, "up")
	}
	if len(checks) != 0 {
		t.Errorf("checks = %d, want 0", len(checks))
	}
}

func TestHealthEngine_LivenessPass(t *testing.T) {
	e := newHealthEngine(5 * time.Second)
	e.addLiveness("ok1", func(context.Context) error { return nil })
	e.addLiveness("ok2", func(context.Context) error { return nil })

	status, checks := e.checkLiveness(t.Context())
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

func TestHealthEngine_LivenessFail(t *testing.T) {
	e := newHealthEngine(5 * time.Second)
	e.addLiveness("ok", func(context.Context) error { return nil })
	e.addLiveness("bad", func(context.Context) error { return errors.New("boom") })

	status, checks := e.checkLiveness(t.Context())
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

func TestHealthEngine_ReadinessPass(t *testing.T) {
	e := newHealthEngine(5 * time.Second)
	e.addReadiness("dep1", func(context.Context) error { return nil })

	status, checks, stores := e.checkReadiness(t.Context(), nil)
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

func TestHealthEngine_ReadinessFail(t *testing.T) {
	e := newHealthEngine(5 * time.Second)
	e.addReadiness("ok", func(context.Context) error { return nil })
	e.addReadiness("bad", func(context.Context) error { return errors.New("not ready") })

	status, _, _ := e.checkReadiness(t.Context(), nil)
	if status != "down" {
		t.Errorf("status = %q, want %q", status, "down")
	}
}

func TestHealthEngine_ConcurrentExecution(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newHealthEngine(10 * time.Second)
		for index, delay := range []time.Duration{2 * time.Second, 5 * time.Second, 3 * time.Second} {
			name := string(rune('a' + index))
			e.addLiveness(name, func(context.Context) error {
				time.Sleep(delay)
				return nil
			})
		}

		start := time.Now()
		status, _ := e.checkLiveness(t.Context())
		if elapsed := time.Since(start); elapsed != 5*time.Second {
			t.Errorf("elapsed = %v, want 5s (maximum check duration)", elapsed)
		}
		if status != "up" {
			t.Errorf("status = %q, want %q", status, "up")
		}
	})
}

func TestHealthEngine_Timeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newHealthEngine(time.Second)
		e.addLiveness("slow", func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})

		start := time.Now()
		status, checks := e.checkLiveness(t.Context())
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

func TestHealthEngine_StoreFunc(t *testing.T) {
	e := newHealthEngine(5 * time.Second)
	storeFn := staticStoreFunc(
		internalhealth.StoreResult{Name: "postgres", Status: "up", Latency: 2 * time.Millisecond},
		internalhealth.StoreResult{Name: "redis", Status: "up", Latency: time.Millisecond},
	)

	status, _, stores := e.checkReadiness(t.Context(), storeFn)
	if status != "up" {
		t.Errorf("status = %q, want %q", status, "up")
	}
	if len(stores) != 2 {
		t.Fatalf("stores = %d, want 2", len(stores))
	}
}

func TestHealthEngine_StoreFunc_Down(t *testing.T) {
	e := newHealthEngine(5 * time.Second)
	storeFn := staticStoreFunc(internalhealth.StoreResult{Name: "postgres", Status: "down"})

	status, _, _ := e.checkReadiness(t.Context(), storeFn)
	if status != "down" {
		t.Errorf("status = %q, want %q", status, "down")
	}
}

func TestHealthEngine_CheckPanic(t *testing.T) {
	e := newHealthEngine(5 * time.Second)
	e.addLiveness("ok", func(context.Context) error { return nil })
	e.addLiveness("panicker", func(context.Context) error {
		panic("segfault simulation")
	})

	status, checks := e.checkLiveness(t.Context())
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

func TestHealthEngine_ContextCancellation(t *testing.T) {
	e := newHealthEngine(5 * time.Second)
	e.addLiveness("ctx-check", func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return nil
		}
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel immediately

	status, checks := e.checkLiveness(ctx)
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
