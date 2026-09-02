package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	internalhealth "github.com/credo-go/credo/internal/health"
)

// ReadinessPolicy describes how one worker contributes to the application's
// readiness probe (see credo.App.UseHealth). Every condition is opt-in and the
// zero policy is rejected by [WithReadiness]; a worker with no policy never
// influences readiness, because not every failed worker should take an
// instance out of rotation.
//
// The contribution is reported as a readiness check named "worker:<name>",
// next to checks added with credo.App.AddReadinessCheck. It is evaluated
// in-memory from the worker's last known state and never blocks. Registration
// order relative to UseHealth does not matter.
type ReadinessPolicy struct {
	// RequireFirstSuccess keeps the instance unready until the worker's first
	// run has completed without error — a startup barrier for recovery or
	// warm-up jobs. Scheduled workers only; pair it with [WithStartImmediately]
	// unless waiting for the first cron activation is intended. Once satisfied
	// it stays satisfied, even if later runs fail.
	RequireFirstSuccess bool

	// FailWhenFailed reports the instance unready once the worker reaches
	// [StatusFailed], that is, once it exhausted [WithMaxRestarts] or
	// [WithMaxConsecutiveFailures]. Use it only for workers the instance cannot
	// serve without: every replica hitting the same persistent failure leaves
	// rotation together.
	FailWhenFailed bool

	// MaxSuccessAge reports the instance unready when the last successful run
	// finished longer ago than this duration. Scheduled workers only; zero
	// disables the check. Until the first success it is not applied — combine
	// it with RequireFirstSuccess to close that window.
	MaxSuccessAge time.Duration
}

// WithReadiness binds the worker to the readiness probe under policy.
// Registration fails when the policy has no condition, when MaxSuccessAge is
// negative, or when a scheduled-only condition is used on a continuous worker.
func WithReadiness(policy ReadinessPolicy) Option {
	return func(o *options) {
		o.hasReadiness = true
		o.readiness = policy
	}
}

func (policy ReadinessPolicy) validate(scheduled bool) error {
	if !policy.RequireFirstSuccess && !policy.FailWhenFailed && policy.MaxSuccessAge == 0 {
		return errors.New("worker: WithReadiness requires at least one condition")
	}
	if policy.MaxSuccessAge < 0 {
		return fmt.Errorf("worker: ReadinessPolicy.MaxSuccessAge must be >= 0, got %s", policy.MaxSuccessAge)
	}
	if !scheduled {
		if policy.RequireFirstSuccess {
			return errors.New("worker: ReadinessPolicy.RequireFirstSuccess is for scheduled workers")
		}
		if policy.MaxSuccessAge > 0 {
			return errors.New("worker: ReadinessPolicy.MaxSuccessAge is for scheduled workers")
		}
	}
	return nil
}

// readinessCheckName is the readiness-check name under which a worker's
// contribution is reported.
func readinessCheckName(workerName string) string {
	return "worker:" + workerName
}

// readinessChecks is the [internalhealth.ReadinessFunc] the pool provides into
// DI: a snapshot of stable probes, one per worker registered with
// [WithReadiness].
func (p *Pool) readinessChecks() []internalhealth.ReadinessCheck {
	p.mu.Lock()
	defer p.mu.Unlock()
	checks := make([]internalhealth.ReadinessCheck, 0, len(p.readiness))
	for _, check := range p.readiness {
		checks = append(checks, check)
	}
	return checks
}

// newReadinessProbe builds the stable probe for def. The probe reads the
// runner's last known state and never performs I/O.
func (p *Pool) newReadinessProbe(def *Definition) *internalhealth.Probe {
	return internalhealth.NewProbe(func(context.Context) internalhealth.Result {
		if err := p.evaluateReadiness(def); err != nil {
			return internalhealth.FailureResult(err)
		}
		return internalhealth.SuccessResult()
	})
}

// evaluateReadiness applies def's policy to the worker's current snapshot.
func (p *Pool) evaluateReadiness(def *Definition) error {
	policy := def.readiness
	if policy == nil {
		return nil
	}

	p.mu.Lock()
	var r *runner
	for _, candidate := range p.runners {
		if candidate.def == def {
			r = candidate
			break
		}
	}
	p.mu.Unlock()

	if r == nil {
		if policy.RequireFirstSuccess {
			return fmt.Errorf("worker %q has not started", def.name)
		}
		return nil
	}

	info := r.snapshot()
	if policy.FailWhenFailed && info.Status == StatusFailed {
		return fmt.Errorf("worker %q failed permanently: %s", def.name, info.LastError)
	}
	if policy.RequireFirstSuccess && info.LastSuccess.IsZero() {
		return fmt.Errorf("worker %q has no successful run yet", def.name)
	}
	if policy.MaxSuccessAge > 0 && !info.LastSuccess.IsZero() {
		if age := time.Since(info.LastSuccess); age > policy.MaxSuccessAge {
			return fmt.Errorf("worker %q last succeeded %s ago, limit %s",
				def.name, age.Round(time.Second), policy.MaxSuccessAge)
		}
	}
	return nil
}
