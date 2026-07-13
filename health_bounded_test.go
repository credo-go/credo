package credo

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	internalhealth "github.com/credo-go/credo/internal/health"
)

func TestHealthEngine_ReadinessRunsNamedAndStoresInParallel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		engine := newHealthEngine(10 * time.Second)
		engine.addReadiness("named", func(context.Context) error {
			time.Sleep(3 * time.Second)
			return nil
		})

		storeFn := internalhealth.StoreFunc(func() []internalhealth.StoreCheck {
			return []internalhealth.StoreCheck{
				{
					Name: "slow-store",
					Probe: internalhealth.NewProbe(func(context.Context) internalhealth.Result {
						time.Sleep(5 * time.Second)
						return internalhealth.Result{Status: "up"}
					}),
				},
				{
					Name: "fast-store",
					Probe: internalhealth.NewProbe(func(context.Context) internalhealth.Result {
						time.Sleep(2 * time.Second)
						return internalhealth.Result{Status: "up"}
					}),
				},
			}
		})

		start := time.Now()
		status, checks, stores := engine.checkReadiness(t.Context(), storeFn)
		if elapsed := time.Since(start); elapsed != 5*time.Second {
			t.Fatalf("elapsed = %v, want 5s (named and store checks must share one parallel runner)", elapsed)
		}
		if status != "up" || len(checks) != 1 || len(stores) != 2 {
			t.Fatalf("result = (%q, %d checks, %d stores), want (up, 1, 2)",
				status, len(checks), len(stores))
		}
	})
}

func TestHealthEngine_StorePanicIsIsolated(t *testing.T) {
	engine := newHealthEngine(time.Second)
	storeFn := internalhealth.StoreFunc(func() []internalhealth.StoreCheck {
		return []internalhealth.StoreCheck{
			{
				Name: "panicker",
				Probe: internalhealth.NewProbe(func(context.Context) internalhealth.Result {
					panic("store exploded")
				}),
			},
			{
				Name: "healthy",
				Probe: internalhealth.NewProbe(func(context.Context) internalhealth.Result {
					return internalhealth.Result{Status: "up"}
				}),
			},
		}
	})

	status, _, stores := engine.checkReadiness(t.Context(), storeFn)
	if status != "down" {
		t.Fatalf("status = %q, want down", status)
	}
	byName := make(map[string]internalhealth.StoreResult, len(stores))
	for _, result := range stores {
		byName[result.Name] = result
	}
	if byName["healthy"].Status != "up" {
		t.Errorf("healthy sibling status = %q, want up", byName["healthy"].Status)
	}
	panicResult := byName["panicker"]
	if panicResult.Status != "down" || panicResult.Cause == nil ||
		!strings.Contains(panicResult.Cause.Error(), "store exploded") {
		t.Errorf("panic result = %#v, want named down result with panic cause", panicResult)
	}
}

func TestHealthEngine_InvalidStoreStatusFailsClosed(t *testing.T) {
	secret := "dial tcp 10.0.1.5:5432"
	engine := newHealthEngine(time.Second)
	storeFn := internalhealth.StoreFunc(func() []internalhealth.StoreCheck {
		return []internalhealth.StoreCheck{{
			Name: "bad-adapter",
			Probe: internalhealth.NewProbe(func(context.Context) internalhealth.Result {
				return internalhealth.Result{Status: secret}
			}),
		}}
	})

	status, _, stores := engine.checkReadiness(t.Context(), storeFn)
	if status != "down" || len(stores) != 1 || stores[0].Status != "down" {
		t.Fatalf("result = (%q, %#v), want fail-closed down", status, stores)
	}
	if stores[0].Cause == nil || !errors.Is(stores[0].Cause, errInvalidStoreHealthStatus) {
		t.Fatalf("cause = %v, want errInvalidStoreHealthStatus", stores[0].Cause)
	}
}

func TestHealthEngine_DegradedStoreIsReadinessBlocking(t *testing.T) {
	engine := newHealthEngine(time.Second)
	storeFn := internalhealth.StoreFunc(func() []internalhealth.StoreCheck {
		return []internalhealth.StoreCheck{{
			Name: "replica",
			Probe: internalhealth.NewProbe(func(context.Context) internalhealth.Result {
				return internalhealth.Result{Status: "degraded"}
			}),
		}}
	})

	status, _, stores := engine.checkReadiness(t.Context(), storeFn)
	if status != "down" || len(stores) != 1 || stores[0].Status != "degraded" {
		t.Fatalf("result = (%q, %#v), want top-level down with visible degraded store", status, stores)
	}
}

func TestHealthEngine_CustomStoreNameCollisionFailsClosed(t *testing.T) {
	engine := newHealthEngine(time.Second)
	engine.addReadiness("database", func(context.Context) error { return nil })
	storeFn := internalhealth.StoreFunc(func() []internalhealth.StoreCheck {
		return []internalhealth.StoreCheck{{
			Name: "database",
			Probe: internalhealth.NewProbe(func(context.Context) internalhealth.Result {
				return internalhealth.Result{Status: "up"}
			}),
		}}
	})

	status, checks, stores := engine.checkReadiness(t.Context(), storeFn)
	if status != "down" {
		t.Fatalf("status = %q, want down", status)
	}
	if len(stores) != 0 {
		t.Fatalf("colliding stores = %#v, want omitted rather than overwriting the named result", stores)
	}
	found := false
	for _, check := range checks {
		if strings.HasPrefix(check.Name, "credo.store_name_conflict.") {
			found = check.Status == "down" && errors.Is(check.Cause, errHealthCheckNameConflict)
		}
	}
	if !found {
		t.Fatalf("checks = %#v, want an explicit fail-closed collision result", checks)
	}
}
