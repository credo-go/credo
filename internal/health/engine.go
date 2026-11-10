package health

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// ErrCheckNameConflict marks a contributed readiness check whose name
// duplicates another contributed check or a named readiness check.
var ErrCheckNameConflict = errors.New("credo: readiness check name conflict")

// CheckResult holds the outcome of one health check. Cause is retained for
// structured operator logging; Error is the safe, explicitly selected text
// used only when the root opts into exposing errors. Latency is the time the
// check's run took, or the time until its deadline. An Optional check is
// reported without gating readiness.
type CheckResult struct {
	Name     string
	Status   string
	Error    string
	Cause    error
	Latency  time.Duration
	Optional bool
}

type namedCheck struct {
	name     string
	probe    *Probe
	optional bool
}

type scheduledCheck struct {
	name     string
	probe    *Probe
	optional bool
}

type scheduledResult struct {
	name     string
	optional bool
	result   Result
}

// Engine manages liveness and readiness health checks and runs them through
// one bounded parallel runner. It is safe for concurrent use.
type Engine struct {
	mu        sync.RWMutex
	liveness  []namedCheck
	readiness []namedCheck
	timeout   time.Duration
}

// NewEngine creates an Engine with the given per-check timeout.
func NewEngine(timeout time.Duration) *Engine {
	return &Engine{timeout: timeout}
}

// SetTimeout replaces the per-check timeout. The root sets it when the
// endpoints are mounted, which may come after the first check is added.
func (e *Engine) SetTimeout(timeout time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.timeout = timeout
}

// Counts returns the number of registered liveness and readiness checks.
func (e *Engine) Counts() (liveness, readiness int) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.liveness), len(e.readiness)
}

// AddLiveness registers a named liveness check. It panics on an invalid or
// duplicate name; the root registration API documents these panics.
func (e *Engine) AddLiveness(name string, fn func(context.Context) error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.liveness = appendNamedCheck(e.liveness, "liveness", name, fn, false)
}

// AddReadiness registers a named readiness check; an optional one is
// reported without gating readiness. It panics on an invalid or duplicate
// name; the root registration API documents these panics.
func (e *Engine) AddReadiness(name string, fn func(context.Context) error, optional bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.readiness = appendNamedCheck(e.readiness, "readiness", name, fn, optional)
}

func appendNamedCheck(
	checks []namedCheck,
	kind string,
	name string,
	fn func(context.Context) error,
	optional bool,
) []namedCheck {
	if err := ValidateName(name); err != nil {
		panic(fmt.Sprintf("credo: Add%sCheck: %v", titleWord(kind), err))
	}
	for _, check := range checks {
		if check.name == name {
			panic(fmt.Sprintf("credo: duplicate %s check name %q", kind, name))
		}
	}

	probe := NewProbe(func(ctx context.Context) Result {
		if err := fn(ctx); err != nil {
			return Result{Status: "down", Cause: err}
		}
		return Result{Status: "up"}
	})
	return append(checks, namedCheck{name: name, probe: probe, optional: optional})
}

func titleWord(value string) string {
	if value == "" {
		return value
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

// CheckLiveness runs all liveness checks concurrently and returns the
// aggregate status ("up" or "down") and individual results.
// No checks registered = "up" (the server responding proves it is alive).
func (e *Engine) CheckLiveness(ctx context.Context) (string, []CheckResult) {
	named, timeout := e.snapshotLiveness()
	scheduled := scheduleNamedChecks(named)
	results := checkResults(runChecks(ctx, scheduled, timeout))
	return aggregateStatus(results), results
}

// CheckReadiness runs the named and the contributed checks through the same
// bounded parallel runner. contributedFn returns stable per-contributor
// probes, reported among the named checks; it may be nil. The status is
// "down" when a gating check fails, else "degraded" when an optional one
// does, else "up".
func (e *Engine) CheckReadiness(ctx context.Context, contributedFn ReadinessFunc) (string, []CheckResult) {
	named, timeout := e.snapshotReadiness()
	scheduled := scheduleNamedChecks(named)
	namedNames := make(map[string]struct{}, len(named))
	for _, check := range named {
		namedNames[check.name] = struct{}{}
	}

	configFailures := make([]CheckResult, 0)
	contributed, contributedSnapshotErr := snapshotContributedChecks(contributedFn)
	if contributedSnapshotErr != nil {
		configFailures = append(configFailures, failedResult(
			"credo.readiness_contributors",
			contributedSnapshotErr,
		))
	}
	seenContributed := make(map[string]struct{}, len(contributed))
	for index, check := range contributed {
		if err := validateContributedCheck(check); err != nil {
			configFailures = append(configFailures, failedResult(
				configurationFailureName("contributed_descriptor", index),
				err,
			))
			continue
		}
		_, duplicate := seenContributed[check.Name]
		_, namedCollision := namedNames[check.Name]
		if duplicate || namedCollision {
			configFailures = append(configFailures, failedResult(
				configurationFailureName("contributed_name_conflict", index),
				fmt.Errorf("%w: contributed readiness check %q is already registered", ErrCheckNameConflict, check.Name),
			))
			continue
		}
		seenContributed[check.Name] = struct{}{}
		scheduled = append(scheduled, scheduledCheck{
			name:     check.Name,
			probe:    check.Probe,
			optional: check.Optional,
		})
	}

	checks := checkResults(runChecks(ctx, scheduled, timeout))
	checks = append(checks, configFailures...)
	return readinessStatus(checks), checks
}

func (e *Engine) snapshotLiveness() ([]namedCheck, time.Duration) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return slices.Clone(e.liveness), e.timeout
}

func (e *Engine) snapshotReadiness() ([]namedCheck, time.Duration) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return slices.Clone(e.readiness), e.timeout
}

func scheduleNamedChecks(checks []namedCheck) []scheduledCheck {
	scheduled := make([]scheduledCheck, 0, len(checks))
	for _, check := range checks {
		scheduled = append(scheduled, scheduledCheck{
			name:     check.name,
			probe:    check.probe,
			optional: check.optional,
		})
	}
	return scheduled
}

func snapshotContributedChecks(fn ReadinessFunc) (checks []ReadinessCheck, err error) {
	if fn == nil {
		return nil, nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			checks = nil
			err = fmt.Errorf("credo: contributed readiness checks panic: %v", recovered)
		}
	}()
	return fn(), nil
}

func validateContributedCheck(check ReadinessCheck) error {
	if err := ValidateName(check.Name); err != nil {
		return fmt.Errorf("credo: invalid contributed readiness check: %w", err)
	}
	if check.Probe == nil {
		return fmt.Errorf("credo: contributed readiness check %q has a nil probe", check.Name)
	}
	return nil
}

func configurationFailureName(kind string, index int) string {
	return fmt.Sprintf("credo.%s.%d", kind, index)
}

// runChecks executes every scheduled probe concurrently. Workers publish
// immutable Probe results; only these bounded collector goroutines write their
// dedicated result indexes, so a late callback cannot race with or mutate a
// response that already timed out.
func runChecks(
	ctx context.Context,
	checks []scheduledCheck,
	timeout time.Duration,
) []scheduledResult {
	if len(checks) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	results := make([]scheduledResult, len(checks))
	var wait sync.WaitGroup
	for index, check := range checks {
		wait.Go(func() {
			results[index] = scheduledResult{
				name:     check.name,
				optional: check.optional,
				result:   check.probe.Run(ctx, timeout),
			}
		})
	}
	wait.Wait()
	return results
}

func checkResults(runResults []scheduledResult) []CheckResult {
	results := make([]CheckResult, 0, len(runResults))
	for _, result := range runResults {
		status := result.result.Status
		if status != "up" {
			status = "down"
		}
		results = append(results, CheckResult{
			Name:     result.name,
			Status:   status,
			Error:    result.result.Error,
			Cause:    result.result.Cause,
			Latency:  max(result.result.Latency, 0),
			Optional: result.optional,
		})
	}
	return results
}

func failedResult(name string, cause error) CheckResult {
	return CheckResult{
		Name:   name,
		Status: "down",
		Error:  cause.Error(),
		Cause:  cause,
	}
}

// aggregateStatus returns "up" if every check passed (or none were
// registered), and "down" if any check failed.
func aggregateStatus(results []CheckResult) string {
	for _, result := range results {
		if result.Status != "up" {
			return "down"
		}
	}
	return "up"
}

// readinessStatus returns "down" if a gating check failed, "degraded" if
// only optional checks did, and "up" if every check passed or none was
// registered.
func readinessStatus(results []CheckResult) string {
	status := "up"
	for _, result := range results {
		switch {
		case result.Status == "up":
		case !result.Optional:
			return "down"
		default:
			status = "degraded"
		}
	}
	return status
}
