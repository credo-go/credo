package credo

import (
	"context"
	"fmt"
	"sync"
	"time"

	internalhealth "github.com/credo-go/credo/internal/health"
)

// healthCheckResult holds the outcome of a single health check.
type healthCheckResult struct {
	Name   string
	Status string // "up" or "down"
	Error  string // non-empty when down
}

type namedHealthCheck struct {
	name string
	fn   func(context.Context) error
}

// healthEngine manages liveness and readiness health checks.
// It is safe for concurrent use.
type healthEngine struct {
	mu        sync.RWMutex
	liveness  []namedHealthCheck
	readiness []namedHealthCheck
	timeout   time.Duration
}

// newHealthEngine creates a new healthEngine with the given per-check timeout.
func newHealthEngine(timeout time.Duration) *healthEngine {
	return &healthEngine{timeout: timeout}
}

// addLiveness registers a named liveness check.
func (e *healthEngine) addLiveness(name string, fn func(context.Context) error) {
	e.mu.Lock()
	e.liveness = append(e.liveness, namedHealthCheck{name: name, fn: fn})
	e.mu.Unlock()
}

// addReadiness registers a named readiness check.
func (e *healthEngine) addReadiness(name string, fn func(context.Context) error) {
	e.mu.Lock()
	e.readiness = append(e.readiness, namedHealthCheck{name: name, fn: fn})
	e.mu.Unlock()
}

// checkLiveness runs all liveness checks concurrently and returns the
// aggregate status ("up" or "down") and individual results.
// No checks registered = "up" (the server responding proves it's alive).
func (e *healthEngine) checkLiveness(ctx context.Context) (string, []healthCheckResult) {
	e.mu.RLock()
	checks := make([]namedHealthCheck, len(e.liveness))
	copy(checks, e.liveness)
	e.mu.RUnlock()

	results := runHealthChecks(ctx, checks, e.timeout)
	return aggregateHealthStatus(results), results
}

// checkReadiness runs all readiness checks concurrently and returns the
// aggregate status, individual check results, and store health results.
// storeFn may be nil (no stores registered).
func (e *healthEngine) checkReadiness(ctx context.Context, storeFn internalhealth.StoreFunc) (string, []healthCheckResult, []internalhealth.StoreResult) {
	e.mu.RLock()
	checks := make([]namedHealthCheck, len(e.readiness))
	copy(checks, e.readiness)
	e.mu.RUnlock()

	results := runHealthChecks(ctx, checks, e.timeout)
	status := aggregateHealthStatus(results)

	var stores []internalhealth.StoreResult
	if storeFn != nil {
		stores = storeFn(ctx)
		for _, s := range stores {
			if s.Status != "up" {
				status = "down"
			}
		}
	}

	return status, results, stores
}

// runHealthChecks executes checks concurrently, each with its own timeout.
// Pre-allocates the result slice so each goroutine writes at its own index.
func runHealthChecks(ctx context.Context, checks []namedHealthCheck, timeout time.Duration) []healthCheckResult {
	if len(checks) == 0 {
		return nil
	}

	results := make([]healthCheckResult, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
		results[i].Name = c.name
		idx := i
		check := c
		wg.Go(func() {
			defer func() {
				if r := recover(); r != nil {
					results[idx].Status = "down"
					results[idx].Error = fmt.Sprintf("panic: %v", r)
				}
			}()
			checkCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			if err := check.fn(checkCtx); err != nil {
				results[idx].Status = "down"
				results[idx].Error = err.Error()
			} else {
				results[idx].Status = "up"
			}
		})
	}
	wg.Wait()
	return results
}

// aggregateHealthStatus returns "up" if all checks passed (or none registered),
// "down" if any check failed.
func aggregateHealthStatus(results []healthCheckResult) string {
	for _, r := range results {
		if r.Status != "up" {
			return "down"
		}
	}
	return "up"
}
