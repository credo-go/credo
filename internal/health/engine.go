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

var (
	// ErrInvalidStoreStatus marks a store probe that reported a status outside
	// the allowlist (or "up" together with a failure cause). Such results fail
	// closed as "down".
	ErrInvalidStoreStatus = errors.New("credo: invalid store health status")

	// ErrCheckNameConflict marks a store check whose name duplicates another
	// store check or a named readiness check.
	ErrCheckNameConflict = errors.New("credo: readiness check name conflict")
)

// CheckResult holds the outcome of a named application health check.
// Cause is retained for structured operator logging; Error is the safe,
// explicitly selected text used only when the root opts into exposing errors.
type CheckResult struct {
	Name   string
	Status string
	Error  string
	Cause  error
}

type namedCheck struct {
	name  string
	probe *Probe
}

type checkSource uint8

const (
	sourceNamed checkSource = iota
	sourceStore
)

type scheduledCheck struct {
	name   string
	source checkSource
	probe  *Probe
}

type scheduledResult struct {
	name   string
	source checkSource
	result Result
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

// AddLiveness registers a named liveness check. It panics on an invalid or
// duplicate name; the root registration API documents these panics.
func (e *Engine) AddLiveness(name string, fn func(context.Context) error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.liveness = appendNamedCheck(e.liveness, "liveness", name, fn)
}

// AddReadiness registers a named readiness check. It panics on an invalid or
// duplicate name; the root registration API documents these panics.
func (e *Engine) AddReadiness(name string, fn func(context.Context) error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.readiness = appendNamedCheck(e.readiness, "readiness", name, fn)
}

func appendNamedCheck(
	checks []namedCheck,
	kind string,
	name string,
	fn func(context.Context) error,
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
	return append(checks, namedCheck{name: name, probe: probe})
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
	named := e.snapshotLiveness()
	scheduled := scheduleNamedChecks(named)
	runResults := runChecks(ctx, scheduled, e.timeout)
	results := namedResults(runResults)
	return aggregateStatus(results), results
}

// CheckReadiness runs named, contributed, and store checks through the same
// bounded parallel runner. storeFn returns stable per-store probes and
// contributedFn stable per-contributor probes (reported among the named
// checks); either may be nil.
func (e *Engine) CheckReadiness(
	ctx context.Context,
	storeFn StoreFunc,
	contributedFn ReadinessFunc,
) (string, []CheckResult, []StoreResult) {
	named := e.snapshotReadiness()
	scheduled := scheduleNamedChecks(named)
	namedNames := make(map[string]struct{}, len(named))
	for _, check := range named {
		namedNames[check.name] = struct{}{}
	}

	storeChecks, storeSnapshotErr := snapshotStoreChecks(storeFn)
	configFailures := make([]CheckResult, 0)
	if storeSnapshotErr != nil {
		configFailures = append(configFailures, failedResult(
			"credo.store_registry",
			storeSnapshotErr,
		))
	}

	seenStores := make(map[string]struct{}, len(storeChecks))
	for index, storeCheck := range storeChecks {
		if err := validateStoreCheckDescriptor(storeCheck); err != nil {
			configFailures = append(configFailures, failedResult(
				configurationFailureName("store_descriptor", index),
				err,
			))
			continue
		}
		if _, duplicate := seenStores[storeCheck.Name]; duplicate {
			configFailures = append(configFailures, failedResult(
				configurationFailureName("store_name_conflict", index),
				fmt.Errorf("%w: duplicate store check %q", ErrCheckNameConflict, storeCheck.Name),
			))
			continue
		}
		seenStores[storeCheck.Name] = struct{}{}
		if _, collision := namedNames[storeCheck.Name]; collision {
			configFailures = append(configFailures, failedResult(
				configurationFailureName("store_name_conflict", index),
				fmt.Errorf("%w: named and store checks both use %q", ErrCheckNameConflict, storeCheck.Name),
			))
			continue
		}
		scheduled = append(scheduled, scheduledCheck{
			name:   storeCheck.Name,
			source: sourceStore,
			probe:  storeCheck.Probe,
		})
	}

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
		_, storeCollision := seenStores[check.Name]
		if duplicate || namedCollision || storeCollision {
			configFailures = append(configFailures, failedResult(
				configurationFailureName("contributed_name_conflict", index),
				fmt.Errorf("%w: contributed readiness check %q is already registered", ErrCheckNameConflict, check.Name),
			))
			continue
		}
		seenContributed[check.Name] = struct{}{}
		scheduled = append(scheduled, scheduledCheck{
			name:   check.Name,
			source: sourceNamed,
			probe:  check.Probe,
		})
	}

	runResults := runChecks(ctx, scheduled, e.timeout)
	checks := make([]CheckResult, 0, len(named)+len(configFailures))
	stores := make([]StoreResult, 0, len(storeChecks))
	for _, runResult := range runResults {
		switch runResult.source {
		case sourceStore:
			stores = append(stores, normalizeStoreResult(runResult.name, runResult.result))
		default:
			checks = append(checks, namedResult(runResult))
		}
	}
	checks = append(checks, configFailures...)

	status := aggregateStatus(checks)
	if status == "up" {
		for _, storeResult := range stores {
			// All stores remain critical in this delivery. "degraded" is
			// explicitly readiness-blocking until optional-store policy lands.
			if storeResult.Status != "up" {
				status = "down"
				break
			}
		}
	}
	return status, checks, stores
}

func (e *Engine) snapshotLiveness() []namedCheck {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return slices.Clone(e.liveness)
}

func (e *Engine) snapshotReadiness() []namedCheck {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return slices.Clone(e.readiness)
}

func scheduleNamedChecks(checks []namedCheck) []scheduledCheck {
	scheduled := make([]scheduledCheck, 0, len(checks))
	for _, check := range checks {
		scheduled = append(scheduled, scheduledCheck{
			name:   check.name,
			source: sourceNamed,
			probe:  check.probe,
		})
	}
	return scheduled
}

func snapshotStoreChecks(storeFn StoreFunc) (checks []StoreCheck, err error) {
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

func validateStoreCheckDescriptor(check StoreCheck) error {
	if err := ValidateName(check.Name); err != nil {
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

// runChecks executes every named and store probe concurrently. Workers publish
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
				name:   check.name,
				source: check.source,
				result: check.probe.Run(ctx, timeout),
			}
		})
	}
	wait.Wait()
	return results
}

func namedResults(runResults []scheduledResult) []CheckResult {
	results := make([]CheckResult, 0, len(runResults))
	for _, result := range runResults {
		results = append(results, namedResult(result))
	}
	return results
}

func namedResult(result scheduledResult) CheckResult {
	status := result.result.Status
	if status != "up" {
		status = "down"
	}
	return CheckResult{
		Name:   result.name,
		Status: status,
		Error:  result.result.Error,
		Cause:  result.result.Cause,
	}
}

func failedResult(name string, cause error) CheckResult {
	return CheckResult{
		Name:   name,
		Status: "down",
		Error:  cause.Error(),
		Cause:  cause,
	}
}

func normalizeStoreResult(name string, result Result) StoreResult {
	status := strings.ToLower(result.Status)
	cause := result.Cause
	causeText := result.Error
	latency := max(result.Latency, 0)

	switch status {
	case "up":
		if cause != nil || causeText != "" {
			status = "down"
			cause = errors.Join(ErrInvalidStoreStatus, cause)
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
		cause = errors.Join(ErrInvalidStoreStatus, cause)
		causeText = appendSafeCauseText(
			fmt.Sprintf("store %q reported unsupported status %q", name, result.Status),
			causeText,
		)
	}

	return StoreResult{
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
