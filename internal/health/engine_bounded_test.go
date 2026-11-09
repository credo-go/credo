package health

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestEngine_ReadinessRunsNamedAndContributedInParallel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		engine := NewEngine(10 * time.Second)
		engine.AddReadiness("named", func(context.Context) error {
			time.Sleep(3 * time.Second)
			return nil
		}, false)

		contributed := ReadinessFunc(func() []ReadinessCheck {
			return []ReadinessCheck{
				{
					Name: "slow-component",
					Probe: NewProbe(func(context.Context) Result {
						time.Sleep(5 * time.Second)
						return SuccessResult()
					}),
				},
				{
					Name: "fast-component",
					Probe: NewProbe(func(context.Context) Result {
						time.Sleep(2 * time.Second)
						return SuccessResult()
					}),
				},
			}
		})

		start := time.Now()
		status, checks := engine.CheckReadiness(t.Context(), contributed)
		if elapsed := time.Since(start); elapsed != 5*time.Second {
			t.Fatalf("elapsed = %v, want 5s (named and contributed checks must share one parallel runner)", elapsed)
		}
		if status != "up" || len(checks) != 3 {
			t.Fatalf("result = (%q, %d checks), want (up, 3)", status, len(checks))
		}
	})
}

func TestEngine_ContributedPanicIsIsolated(t *testing.T) {
	engine := NewEngine(time.Second)
	contributed := ReadinessFunc(func() []ReadinessCheck {
		return []ReadinessCheck{
			{
				Name: "panicker",
				Probe: NewProbe(func(context.Context) Result {
					panic("component exploded")
				}),
			},
			{
				Name: "healthy",
				Probe: NewProbe(func(context.Context) Result {
					return SuccessResult()
				}),
			},
		}
	})

	status, checks := engine.CheckReadiness(t.Context(), contributed)
	if status != "down" {
		t.Fatalf("status = %q, want down", status)
	}
	byName := make(map[string]CheckResult, len(checks))
	for _, result := range checks {
		byName[result.Name] = result
	}
	if byName["healthy"].Status != "up" {
		t.Errorf("healthy sibling status = %q, want up", byName["healthy"].Status)
	}
	panicResult := byName["panicker"]
	if panicResult.Status != "down" || panicResult.Cause == nil ||
		!strings.Contains(panicResult.Cause.Error(), "component exploded") {
		t.Errorf("panic result = %#v, want named down result with panic cause", panicResult)
	}
}

func TestEngine_ContributedNameCollisionFailsClosed(t *testing.T) {
	engine := NewEngine(time.Second)
	engine.AddReadiness("database", func(context.Context) error { return nil }, false)
	contributed := ReadinessFunc(func() []ReadinessCheck {
		return []ReadinessCheck{{
			Name: "database",
			Probe: NewProbe(func(context.Context) Result {
				return SuccessResult()
			}),
		}}
	})

	status, checks := engine.CheckReadiness(t.Context(), contributed)
	if status != "down" {
		t.Fatalf("status = %q, want down", status)
	}
	found := false
	for _, check := range checks {
		if strings.HasPrefix(check.Name, "credo.contributed_name_conflict.") {
			found = check.Status == "down" && errors.Is(check.Cause, ErrCheckNameConflict)
		}
	}
	if !found {
		t.Fatalf("checks = %#v, want an explicit fail-closed collision result", checks)
	}
}
