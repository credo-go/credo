package credo

import (
	"context"
	"errors"
	"testing"
	"time"

	internalhealth "github.com/credo-go/credo/internal/health"
)

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
	e := newHealthEngine(5 * time.Second)

	// Each check sleeps 50ms. If run sequentially, 3 checks take >=150ms.
	// If concurrent, they take ~50ms.
	sleepDur := 50 * time.Millisecond
	for i := range 3 {
		name := string(rune('a' + i))
		e.addLiveness(name, func(context.Context) error {
			time.Sleep(sleepDur)
			return nil
		})
	}

	start := time.Now()
	status, _ := e.checkLiveness(t.Context())
	elapsed := time.Since(start)

	if status != "up" {
		t.Errorf("status = %q, want %q", status, "up")
	}
	// Should complete in well under 150ms if concurrent.
	if elapsed >= 140*time.Millisecond {
		t.Errorf("elapsed = %v, want < 140ms (checks should run concurrently)", elapsed)
	}
}

func TestHealthEngine_Timeout(t *testing.T) {
	e := newHealthEngine(50 * time.Millisecond)
	e.addLiveness("slow", func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return nil
		}
	})

	status, checks := e.checkLiveness(t.Context())
	if status != "down" {
		t.Errorf("status = %q, want %q", status, "down")
	}
	if len(checks) != 1 {
		t.Fatalf("checks = %d, want 1", len(checks))
	}
	if checks[0].Status != "down" {
		t.Errorf("check status = %q, want %q", checks[0].Status, "down")
	}
	if checks[0].Error == "" {
		t.Error("expected non-empty error for timed-out check")
	}
}

func TestHealthEngine_StoreFunc(t *testing.T) {
	e := newHealthEngine(5 * time.Second)
	storeFn := internalhealth.StoreFunc(func(context.Context) []internalhealth.StoreResult {
		return []internalhealth.StoreResult{
			{Name: "postgres", Status: "up", Latency: 2 * time.Millisecond},
			{Name: "redis", Status: "up", Latency: 1 * time.Millisecond},
		}
	})

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
	storeFn := internalhealth.StoreFunc(func(context.Context) []internalhealth.StoreResult {
		return []internalhealth.StoreResult{
			{Name: "postgres", Status: "down", Latency: 0},
		}
	})

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
