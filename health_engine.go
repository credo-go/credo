package credo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	internalhealth "github.com/credo-go/credo/internal/health"
)

var (
	errInvalidStoreHealthStatus = errors.New("credo: invalid store health status")
	errHealthCheckNameConflict  = errors.New("credo: readiness check name conflict")
)

// healthCheckResult holds the outcome of a named application health check.
// Cause is retained for structured operator logging; Error is the safe,
// explicitly selected text used only when ExposeErrors is enabled.
type healthCheckResult struct {
	Name   string
	Status string
	Error  string
	Cause  error
}

type namedHealthCheck struct {
	name  string
	probe *internalhealth.Probe
}

type healthCheckSource uint8

const (
	healthCheckSourceNamed healthCheckSource = iota
	healthCheckSourceStore
)

type scheduledHealthCheck struct {
	name   string
	source healthCheckSource
	probe  *internalhealth.Probe
}

type scheduledHealthResult struct {
	name   string
	source healthCheckSource
	result internalhealth.Result
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
	defer e.mu.Unlock()
	e.liveness = appendNamedHealthCheck(e.liveness, "liveness", name, fn)
}

// addReadiness registers a named readiness check.
func (e *healthEngine) addReadiness(name string, fn func(context.Context) error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.readiness = appendNamedHealthCheck(e.readiness, "readiness", name, fn)
}

func appendNamedHealthCheck(
	checks []namedHealthCheck,
	kind string,
	name string,
	fn func(context.Context) error,
) []namedHealthCheck {
	if err := validateHealthCheckName(name); err != nil {
		panic(fmt.Sprintf("credo: Add%sCheck: %v", titleWord(kind), err))
	}
	for _, check := range checks {
		if check.name == name {
			panic(fmt.Sprintf("credo: duplicate %s check name %q", kind, name))
		}
	}

	probe := internalhealth.NewProbe(func(ctx context.Context) internalhealth.Result {
		if err := fn(ctx); err != nil {
			return internalhealth.Result{Status: "down", Cause: err}
		}
		return internalhealth.Result{Status: "up"}
	})
	return append(checks, namedHealthCheck{name: name, probe: probe})
}

func titleWord(value string) string {
	if value == "" {
		return value
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func validateHealthCheckName(name string) error {
	return internalhealth.ValidateName(name)
}

// checkLiveness runs all liveness checks concurrently and returns the
// aggregate status ("up" or "down") and individual results.
// No checks registered = "up" (the server responding proves it is alive).
func (e *healthEngine) checkLiveness(ctx context.Context) (string, []healthCheckResult) {
	named := e.snapshotLiveness()
	scheduled := scheduleNamedChecks(named)
	runResults := runHealthChecks(ctx, scheduled, e.timeout)
	results := namedResults(runResults)
	return aggregateHealthStatus(results), results
}

// checkReadiness runs named and store checks through the same bounded parallel
// runner. storeFn returns stable per-store probes and may be nil.
func (e *healthEngine) checkReadiness(
	ctx context.Context,
	storeFn internalhealth.StoreFunc,
) (string, []healthCheckResult, []internalhealth.StoreResult) {
	named := e.snapshotReadiness()
	scheduled := scheduleNamedChecks(named)
	namedNames := make(map[string]struct{}, len(named))
	for _, check := range named {
		namedNames[check.name] = struct{}{}
	}

	storeChecks, storeSnapshotErr := snapshotStoreChecks(storeFn)
	configFailures := make([]healthCheckResult, 0)
	if storeSnapshotErr != nil {
		configFailures = append(configFailures, failedHealthCheckResult(
			"credo.store_registry",
			storeSnapshotErr,
		))
	}

	seenStores := make(map[string]struct{}, len(storeChecks))
	for index, storeCheck := range storeChecks {
		if err := validateStoreCheckDescriptor(storeCheck); err != nil {
			configFailures = append(configFailures, failedHealthCheckResult(
				configurationFailureName("store_descriptor", index),
				err,
			))
			continue
		}
		if _, duplicate := seenStores[storeCheck.Name]; duplicate {
			configFailures = append(configFailures, failedHealthCheckResult(
				configurationFailureName("store_name_conflict", index),
				fmt.Errorf("%w: duplicate store check %q", errHealthCheckNameConflict, storeCheck.Name),
			))
			continue
		}
		seenStores[storeCheck.Name] = struct{}{}
		if _, collision := namedNames[storeCheck.Name]; collision {
			configFailures = append(configFailures, failedHealthCheckResult(
				configurationFailureName("store_name_conflict", index),
				fmt.Errorf("%w: named and store checks both use %q", errHealthCheckNameConflict, storeCheck.Name),
			))
			continue
		}
		scheduled = append(scheduled, scheduledHealthCheck{
			name:   storeCheck.Name,
			source: healthCheckSourceStore,
			probe:  storeCheck.Probe,
		})
	}

	runResults := runHealthChecks(ctx, scheduled, e.timeout)
	checks := make([]healthCheckResult, 0, len(named)+len(configFailures))
	stores := make([]internalhealth.StoreResult, 0, len(storeChecks))
	for _, runResult := range runResults {
		switch runResult.source {
		case healthCheckSourceStore:
			stores = append(stores, normalizeStoreResult(runResult.name, runResult.result))
		default:
			checks = append(checks, namedResult(runResult))
		}
	}
	checks = append(checks, configFailures...)

	status := aggregateHealthStatus(checks)
	if status == "up" {
		for _, storeResult := range stores {
			// All stores remain critical in this delivery. StatusDegraded is
			// explicitly readiness-blocking until optional-store policy lands.
			if storeResult.Status != "up" {
				status = "down"
				break
			}
		}
	}
	return status, checks, stores
}

func (e *healthEngine) snapshotLiveness() []namedHealthCheck {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return append([]namedHealthCheck(nil), e.liveness...)
}

func (e *healthEngine) snapshotReadiness() []namedHealthCheck {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return append([]namedHealthCheck(nil), e.readiness...)
}

func scheduleNamedChecks(checks []namedHealthCheck) []scheduledHealthCheck {
	scheduled := make([]scheduledHealthCheck, 0, len(checks))
	for _, check := range checks {
		scheduled = append(scheduled, scheduledHealthCheck{
			name:   check.name,
			source: healthCheckSourceNamed,
			probe:  check.probe,
		})
	}
	return scheduled
}

func snapshotStoreChecks(storeFn internalhealth.StoreFunc) (checks []internalhealth.StoreCheck, err error) {
	if storeFn == nil {
		return nil, nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			checks = nil
			err = fmt.Errorf("credo: store health registry panic: %v", recovered)
		}
	}()
	return storeFn(), nil
}

func validateStoreCheckDescriptor(check internalhealth.StoreCheck) error {
	if err := validateHealthCheckName(check.Name); err != nil {
		return fmt.Errorf("credo: invalid store check: %w", err)
	}
	if check.Probe == nil {
		return fmt.Errorf("credo: store check %q has a nil probe", check.Name)
	}
	return nil
}

func configurationFailureName(kind string, index int) string {
	return fmt.Sprintf("credo.%s.%d", kind, index)
}

// runHealthChecks executes every named and store probe concurrently. Workers
// publish immutable Probe results; only these bounded collector goroutines
// write their dedicated result indexes, so a late callback cannot race with or
// mutate a response that already timed out.
func runHealthChecks(
	ctx context.Context,
	checks []scheduledHealthCheck,
	timeout time.Duration,
) []scheduledHealthResult {
	if len(checks) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	results := make([]scheduledHealthResult, len(checks))
	var wait sync.WaitGroup
	for index, check := range checks {
		idx := index
		current := check
		wait.Go(func() {
			results[idx] = scheduledHealthResult{
				name:   current.name,
				source: current.source,
				result: current.probe.Run(ctx, timeout),
			}
		})
	}
	wait.Wait()
	return results
}

func namedResults(runResults []scheduledHealthResult) []healthCheckResult {
	results := make([]healthCheckResult, 0, len(runResults))
	for _, result := range runResults {
		results = append(results, namedResult(result))
	}
	return results
}

func namedResult(result scheduledHealthResult) healthCheckResult {
	status := result.result.Status
	if status != "up" {
		status = "down"
	}
	return healthCheckResult{
		Name:   result.name,
		Status: status,
		Error:  result.result.Error,
		Cause:  result.result.Cause,
	}
}

func failedHealthCheckResult(name string, cause error) healthCheckResult {
	return healthCheckResult{
		Name:   name,
		Status: "down",
		Error:  cause.Error(),
		Cause:  cause,
	}
}

func normalizeStoreResult(name string, result internalhealth.Result) internalhealth.StoreResult {
	status := strings.ToLower(result.Status)
	cause := result.Cause
	causeText := result.Error
	latency := result.Latency
	if latency < 0 {
		latency = 0
	}

	switch status {
	case "up":
		if cause != nil || causeText != "" {
			status = "down"
			cause = errors.Join(errInvalidStoreHealthStatus, cause)
			causeText = appendSafeCauseText(
				fmt.Sprintf("store %q reported up with a failure cause", name),
				causeText,
			)
		}
	case "down", "degraded":
		if cause == nil {
			if causeText == "" {
				causeText = fmt.Sprintf("store %q reported status %q", name, status)
			}
			cause = errors.New(causeText)
		} else if causeText == "" {
			causeText = fmt.Sprintf("store %q reported status %q", name, status)
		}
	default:
		status = "down"
		cause = errors.Join(errInvalidStoreHealthStatus, cause)
		causeText = appendSafeCauseText(
			fmt.Sprintf("store %q reported unsupported status %q", name, result.Status),
			causeText,
		)
	}

	return internalhealth.StoreResult{
		Name:    name,
		Status:  status,
		Latency: latency,
		Cause:   cause,
		Error:   causeText,
	}
}

func appendSafeCauseText(prefix string, causeText string) string {
	if causeText == "" {
		return prefix
	}
	return prefix + ": " + causeText
}

// aggregateHealthStatus returns "up" if every check passed (or none were
// registered), and "down" if any check failed.
func aggregateHealthStatus(results []healthCheckResult) string {
	for _, result := range results {
		if result.Status != "up" {
			return "down"
		}
	}
	return "up"
}
