package credo_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

// registerIdle is a worker that runs until its context is cancelled.
var registerIdle = workerFunc(func(ctx context.Context) error {
	<-ctx.Done()
	return nil
})

// registerBuiltA and registerBuiltB are workers their constructors build.
type registerBuiltA struct{}

func (*registerBuiltA) Run(ctx context.Context) error { <-ctx.Done(); return nil }

type registerBuiltB struct{}

func (*registerBuiltB) Run(ctx context.Context) error { <-ctx.Done(); return nil }

func newRegisterBuiltA() *registerBuiltA { return &registerBuiltA{} }

func newRegisterBuiltB() *registerBuiltB { return &registerBuiltB{} }

// registerShutdowner is a component that is not a worker.
type registerShutdowner struct{}

func (*registerShutdowner) Shutdown(context.Context) error { return nil }

// registerComponent is a component of its own: unlike a pointer to a
// zero-size value, each one is a resource of its own.
type registerComponent struct{ _ byte }

func (*registerComponent) Shutdown(context.Context) error { return nil }

// registerWant fails the test unless msg is exactly want.
func registerWant(t *testing.T, msg, want string) {
	t.Helper()
	if !matchesWithSite(msg, want) {
		t.Errorf("message = %q\nwant      %q", msg, want)
	}
}

// siteWildcard stands, in an expected message, for a trigger's site and its
// callers, which finalize_trigger_test.go asserts exactly.
const siteWildcard = "<site>"

// matchesWithSite reports whether msg is want, each siteWildcard in want
// matching a non-empty run of text.
func matchesWithSite(msg, want string) bool {
	parts := strings.Split(want, siteWildcard)
	for i, part := range parts {
		parts[i] = regexp.QuoteMeta(part)
	}
	return regexp.MustCompile("^" + strings.Join(parts, ".+?") + "$").MatchString(msg)
}

// registerAfterFinalize is the container's reason for a registration after
// mustFinalize.
const registerAfterFinalize = "called after the container was finalized by Finalize at " + siteWildcard +
	"; register before that call"

func TestWorkersConfiguration_ContinuousResolution(t *testing.T) {
	defaults := credo.RestartPolicy{MinDelay: credo.DefaultMinRestartDelay, MaxDelay: credo.DefaultMaxRestartDelay}
	tests := []struct {
		name string
		cfg  []credo.ContinuousConfig
		want credo.ContinuousConfig
	}{
		{
			name: "no configuration",
			want: credo.ContinuousConfig{Tier: credo.TierInternal, Restart: defaults},
		},
		{
			name: "zero configuration",
			cfg:  []credo.ContinuousConfig{{}},
			want: credo.ContinuousConfig{Tier: credo.TierInternal, Restart: defaults},
		},
		{
			name: "ingress tier",
			cfg:  []credo.ContinuousConfig{{Tier: credo.TierIngress}},
			want: credo.ContinuousConfig{Tier: credo.TierIngress, Restart: defaults},
		},
		{
			name: "explicit internal tier",
			cfg:  []credo.ContinuousConfig{{Tier: credo.TierInternal}},
			want: credo.ContinuousConfig{Tier: credo.TierInternal, Restart: defaults},
		},
		{
			name: "MinDelay below the default cap keeps the cap",
			cfg:  []credo.ContinuousConfig{{Restart: credo.RestartPolicy{MinDelay: 5 * time.Second}}},
			want: credo.ContinuousConfig{
				Tier:    credo.TierInternal,
				Restart: credo.RestartPolicy{MinDelay: 5 * time.Second, MaxDelay: credo.DefaultMaxRestartDelay},
			},
		},
		{
			name: "MinDelay alone above the default cap is a fixed delay",
			cfg:  []credo.ContinuousConfig{{Restart: credo.RestartPolicy{MinDelay: 10 * time.Minute}}},
			want: credo.ContinuousConfig{
				Tier:    credo.TierInternal,
				Restart: credo.RestartPolicy{MinDelay: 10 * time.Minute, MaxDelay: 10 * time.Minute},
			},
		},
		{
			name: "MaxDelay alone above the default floor",
			cfg:  []credo.ContinuousConfig{{Restart: credo.RestartPolicy{MaxDelay: 2 * time.Minute}}},
			want: credo.ContinuousConfig{
				Tier:    credo.TierInternal,
				Restart: credo.RestartPolicy{MinDelay: credo.DefaultMinRestartDelay, MaxDelay: 2 * time.Minute},
			},
		},
		{
			name: "MaxDelay alone equal to the default floor",
			cfg:  []credo.ContinuousConfig{{Restart: credo.RestartPolicy{MaxDelay: credo.DefaultMinRestartDelay}}},
			want: credo.ContinuousConfig{
				Tier: credo.TierInternal,
				Restart: credo.RestartPolicy{
					MinDelay: credo.DefaultMinRestartDelay,
					MaxDelay: credo.DefaultMinRestartDelay,
				},
			},
		},
		{
			name: "equal delays",
			cfg: []credo.ContinuousConfig{{
				Restart: credo.RestartPolicy{MinDelay: 5 * time.Second, MaxDelay: 5 * time.Second},
			}},
			want: credo.ContinuousConfig{
				Tier:    credo.TierInternal,
				Restart: credo.RestartPolicy{MinDelay: 5 * time.Second, MaxDelay: 5 * time.Second},
			},
		},
		{
			name: "limit",
			cfg:  []credo.ContinuousConfig{{Restart: credo.RestartPolicy{Limit: 5}}},
			want: credo.ContinuousConfig{
				Tier: credo.TierInternal,
				Restart: credo.RestartPolicy{
					Limit:    5,
					MinDelay: credo.DefaultMinRestartDelay,
					MaxDelay: credo.DefaultMaxRestartDelay,
				},
			},
		},
		{
			name: "disabled keeps its delays zero",
			cfg:  []credo.ContinuousConfig{{Restart: credo.RestartPolicy{Disabled: true}}},
			want: credo.ContinuousConfig{Tier: credo.TierInternal, Restart: credo.RestartPolicy{Disabled: true}},
		},
		{
			name: "readiness condition",
			cfg:  []credo.ContinuousConfig{{UnreadyWhenFailed: true}},
			want: credo.ContinuousConfig{Tier: credo.TierInternal, Restart: defaults, UnreadyWhenFailed: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check := func(t *testing.T, s *credo.Workers) {
				t.Helper()
				info, ok := s.Lookup("w")
				if !ok {
					t.Fatal(`Lookup("w") = false`)
				}
				if info.Kind != credo.WorkerKindContinuous || info.Status != credo.WorkerStatusPending ||
					info.Schedule != "" {
					t.Errorf("WorkerInfo = %+v, want a pending continuous worker without a schedule", info)
				}
				if info.Scheduled != nil {
					t.Errorf("WorkerInfo.Scheduled = %+v, want nil", info.Scheduled)
				}
				if info.Continuous == nil || *info.Continuous != tt.want {
					t.Errorf("WorkerInfo.Continuous = %+v, want %+v", info.Continuous, tt.want)
				}
			}
			t.Run("Continuous", func(t *testing.T) {
				s := mustNew(t).Workers()
				s.Continuous("w", registerIdle, tt.cfg...)
				check(t, s)
			})
			t.Run("constructor", func(t *testing.T) {
				s := mustNew(t).Workers()
				s.Continuous("w", newRegisterBuiltA, tt.cfg...)
				check(t, s)
			})
		})
	}
}

func TestWorkersConfiguration_ScheduledResolution(t *testing.T) {
	full := credo.ScheduledConfig{
		RunOnStart:               true,
		RunTimeout:               15 * time.Second,
		MaxConsecutiveFailures:   3,
		UnreadyWhenFailed:        true,
		UnreadyUntilFirstSuccess: true,
		UnreadyAfterSuccessAge:   15 * time.Minute,
	}
	fullIngress := full
	fullIngress.Tier = credo.TierIngress

	tests := []struct {
		name string
		cfg  []credo.ScheduledConfig
		want credo.ScheduledConfig
	}{
		{name: "no configuration", want: credo.ScheduledConfig{Tier: credo.TierIngress}},
		{
			name: "zero configuration",
			cfg:  []credo.ScheduledConfig{{}},
			want: credo.ScheduledConfig{Tier: credo.TierIngress},
		},
		{
			name: "internal tier",
			cfg:  []credo.ScheduledConfig{{Tier: credo.TierInternal}},
			want: credo.ScheduledConfig{Tier: credo.TierInternal},
		},
		{
			name: "explicit ingress tier",
			cfg:  []credo.ScheduledConfig{{Tier: credo.TierIngress}},
			want: credo.ScheduledConfig{Tier: credo.TierIngress},
		},
		{name: "every field set", cfg: []credo.ScheduledConfig{full}, want: fullIngress},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check := func(t *testing.T, s *credo.Workers) {
				t.Helper()
				info, ok := s.Lookup("w")
				if !ok {
					t.Fatal(`Lookup("w") = false`)
				}
				if info.Kind != credo.WorkerKindScheduled || info.Status != credo.WorkerStatusPending ||
					info.Schedule != "@every 1m" {
					t.Errorf("WorkerInfo = %+v, want a pending scheduled worker on @every 1m", info)
				}
				if info.Continuous != nil {
					t.Errorf("WorkerInfo.Continuous = %+v, want nil", info.Continuous)
				}
				if info.Scheduled == nil || *info.Scheduled != tt.want {
					t.Errorf("WorkerInfo.Scheduled = %+v, want %+v", info.Scheduled, tt.want)
				}
			}
			t.Run("Scheduled", func(t *testing.T) {
				s := mustNew(t).Workers()
				s.Scheduled("w", "@every 1m", registerIdle, tt.cfg...)
				check(t, s)
			})
			t.Run("constructor", func(t *testing.T) {
				s := mustNew(t).Workers()
				s.Scheduled("w", "@every 1m", newRegisterBuiltA, tt.cfg...)
				check(t, s)
			})
		})
	}
}

func TestWorkersConfiguration_ContinuousRejected(t *testing.T) {
	tests := []struct {
		name string
		cfg  []credo.ContinuousConfig
		want string // the reason, after the call
	}{
		{
			name: "two configurations",
			cfg:  []credo.ContinuousConfig{{}, {}},
			want: "2 configurations given; pass at most one credo.ContinuousConfig",
		},
		{
			name: "invalid tier",
			cfg:  []credo.ContinuousConfig{{Tier: credo.Tier(7)}},
			want: "Tier(7) is not a tier; use credo.TierIngress, credo.TierInternal, or zero for the default",
		},
		{
			name: "disabled beside limit",
			cfg:  []credo.ContinuousConfig{{Restart: credo.RestartPolicy{Disabled: true, Limit: 1}}},
			want: "Restart.Disabled beside Restart.Limit, MinDelay or MaxDelay; a worker without restarts " +
				"has no limit or delays, so set Disabled alone",
		},
		{
			name: "disabled beside MinDelay",
			cfg:  []credo.ContinuousConfig{{Restart: credo.RestartPolicy{Disabled: true, MinDelay: time.Second}}},
			want: "Restart.Disabled beside Restart.Limit, MinDelay or MaxDelay; a worker without restarts " +
				"has no limit or delays, so set Disabled alone",
		},
		{
			name: "disabled beside MaxDelay",
			cfg:  []credo.ContinuousConfig{{Restart: credo.RestartPolicy{Disabled: true, MaxDelay: time.Second}}},
			want: "Restart.Disabled beside Restart.Limit, MinDelay or MaxDelay; a worker without restarts " +
				"has no limit or delays, so set Disabled alone",
		},
		{
			name: "negative limit",
			cfg:  []credo.ContinuousConfig{{Restart: credo.RestartPolicy{Limit: -1}}},
			want: "Restart.Limit -1 is negative; use 0 for unlimited restarts or a positive limit",
		},
		{
			name: "negative MinDelay",
			cfg:  []credo.ContinuousConfig{{Restart: credo.RestartPolicy{MinDelay: -time.Second}}},
			want: "Restart.MinDelay -1s is negative; use 0 for the default 3s or a positive delay",
		},
		{
			name: "negative MaxDelay",
			cfg:  []credo.ContinuousConfig{{Restart: credo.RestartPolicy{MaxDelay: -time.Second}}},
			want: "Restart.MaxDelay -1s is negative; use 0 for the default or a positive cap",
		},
		{
			name: "MaxDelay below the default floor",
			cfg:  []credo.ContinuousConfig{{Restart: credo.RestartPolicy{MaxDelay: time.Second}}},
			want: "Restart.MaxDelay 1s is below MinDelay 3s (the default); set MinDelay to at most 1s, " +
				"or raise MaxDelay",
		},
		{
			name: "MaxDelay below an explicit MinDelay",
			cfg: []credo.ContinuousConfig{{
				Restart: credo.RestartPolicy{MinDelay: 10 * time.Second, MaxDelay: 5 * time.Second},
			}},
			want: "Restart.MaxDelay 5s is below MinDelay 10s; lower MinDelay to at most 5s, or raise MaxDelay",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Run("Continuous", func(t *testing.T) {
				s := mustNew(t).Workers()
				msg := mustPanicMessage(t, func() { s.Continuous("order-consumer", registerIdle, tt.cfg...) })
				registerWant(t, msg, `credo: Workers.Continuous("order-consumer"): `+tt.want)
				// A refused registration leaves nothing behind.
				if len(s.Snapshot()) != 0 {
					t.Errorf("Snapshot() = %+v after a refused registration, want empty", s.Snapshot())
				}
				s.Continuous("order-consumer", registerIdle)
			})
			t.Run("constructor", func(t *testing.T) {
				s := mustNew(t).Workers()
				msg := mustPanicMessage(t, func() {
					s.Continuous("order-consumer", newRegisterBuiltA, tt.cfg...)
				})
				call := `credo: Workers.Continuous("order-consumer")`
				registerWant(t, msg, call+": "+tt.want)
				if len(s.Snapshot()) != 0 {
					t.Errorf("Snapshot() = %+v after a refused registration, want empty", s.Snapshot())
				}
				// The refused registration reserved no name.
				s.Continuous("order-consumer", newRegisterBuiltA)
			})
		})
	}
}

func TestWorkersConfiguration_ScheduledRejected(t *testing.T) {
	tests := []struct {
		name string
		cfg  []credo.ScheduledConfig
		want string // the reason, after the call
	}{
		{
			name: "two configurations",
			cfg:  []credo.ScheduledConfig{{}, {}},
			want: "2 configurations given; pass at most one credo.ScheduledConfig",
		},
		{
			name: "invalid tier",
			cfg:  []credo.ScheduledConfig{{Tier: credo.Tier(7)}},
			want: "Tier(7) is not a tier; use credo.TierIngress, credo.TierInternal, or zero for the default",
		},
		{
			name: "negative RunTimeout",
			cfg:  []credo.ScheduledConfig{{RunTimeout: -time.Second}},
			want: "ScheduledConfig.RunTimeout -1s is negative; use 0 for no timeout or a positive budget",
		},
		{
			name: "negative MaxConsecutiveFailures",
			cfg:  []credo.ScheduledConfig{{MaxConsecutiveFailures: -1}},
			want: "ScheduledConfig.MaxConsecutiveFailures -1 is negative; use 0 for unlimited failures " +
				"or a positive limit",
		},
		{
			name: "negative UnreadyAfterSuccessAge",
			cfg:  []credo.ScheduledConfig{{UnreadyAfterSuccessAge: -time.Second}},
			want: "ScheduledConfig.UnreadyAfterSuccessAge -1s is negative; use 0 to turn the check off " +
				"or a positive age",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Run("Scheduled", func(t *testing.T) {
				s := mustNew(t).Workers()
				msg := mustPanicMessage(t, func() { s.Scheduled("report", "@every 1m", registerIdle, tt.cfg...) })
				registerWant(t, msg, `credo: Workers.Scheduled("report"): `+tt.want)
				if len(s.Snapshot()) != 0 {
					t.Errorf("Snapshot() = %+v after a refused registration, want empty", s.Snapshot())
				}
				s.Scheduled("report", "@every 1m", registerIdle)
			})
			t.Run("constructor", func(t *testing.T) {
				s := mustNew(t).Workers()
				msg := mustPanicMessage(t, func() {
					s.Scheduled("report", "@every 1m", newRegisterBuiltA, tt.cfg...)
				})
				call := `credo: Workers.Scheduled("report")`
				registerWant(t, msg, call+": "+tt.want)
				if len(s.Snapshot()) != 0 {
					t.Errorf("Snapshot() = %+v after a refused registration, want empty", s.Snapshot())
				}
				s.Scheduled("report", "@every 1m", newRegisterBuiltA)
			})
		})
	}
}

func TestWorkersRegister_Misuse(t *testing.T) {
	var nilPointer *registerBuiltA
	_, parseErr := credo.ParseSchedule("not a cron")
	if parseErr == nil {
		t.Fatal(`ParseSchedule("not a cron") = nil error`)
	}
	const parseText = `parse schedule "not a cron": expected exactly 5 fields, found 3: [not a cron]`
	// ParseSchedule returns the error under its own prefix; the panic carries
	// it under the call, without that prefix.
	registerWant(t, parseErr.Error(), "credo: "+parseText)
	const parseRemedy = "; validate a schedule from configuration with credo.ParseSchedule before registering it"
	const duplicate = `: a worker named "dup" is already registered; give each worker its own name`

	tests := []struct {
		name string
		// misuse performs the setup and returns the call that must panic.
		misuse func(t *testing.T) func()
		want   string
	}{
		{
			name: "nil worker",
			misuse: func(t *testing.T) func() {
				s := mustNew(t).Workers()
				return func() { s.Continuous("x", nil) }
			},
			want: `credo: Workers.Continuous("x"): the worker is nil; pass a value with a Run method`,
		},
		{
			name: "nil function",
			misuse: func(t *testing.T) func() {
				s := mustNew(t).Workers()
				return func() { s.Scheduled("x", "@every 1m", workerFunc(nil)) }
			},
			want: `credo: Workers.Scheduled("x"): the worker is nil; pass a value with a Run method`,
		},
		{
			name: "typed-nil pointer",
			misuse: func(t *testing.T) func() {
				s := mustNew(t).Workers()
				return func() { s.Continuous("x", nilPointer) }
			},
			want: `credo: Workers.Continuous("x"): the worker is nil; pass a value with a Run method`,
		},
		{
			name: "nil worker is checked before the name",
			misuse: func(t *testing.T) func() {
				s := mustNew(t).Workers()
				return func() { s.Continuous("", nil) }
			},
			want: `credo: Workers.Continuous(""): the worker is nil; pass a value with a Run method`,
		},
		{
			name: "empty name",
			misuse: func(t *testing.T) func() {
				s := mustNew(t).Workers()
				return func() { s.Continuous("", registerIdle) }
			},
			want: `credo: Workers.Continuous(""): the name is empty; name the worker`,
		},
		{
			name: "padded name",
			misuse: func(t *testing.T) func() {
				s := mustNew(t).Workers()
				return func() { s.Scheduled(" report", "@every 1m", registerIdle) }
			},
			want: `credo: Workers.Scheduled(" report"): the name has leading or trailing whitespace; ` +
				"names are never trimmed, so remove it",
		},
		{
			name: "trailing whitespace",
			misuse: func(t *testing.T) func() {
				s := mustNew(t).Workers()
				return func() { s.Continuous("x\t", newRegisterBuiltA) }
			},
			want: `credo: Workers.Continuous("x\t")` +
				": the name has leading or trailing whitespace; names are never trimmed, so remove it",
		},
		{
			name: "control character",
			misuse: func(t *testing.T) func() {
				s := mustNew(t).Workers()
				return func() { s.Scheduled("a\nb", "@every 1m", newRegisterBuiltA) }
			},
			want: `credo: Workers.Scheduled("a\nb")` +
				": the name contains control characters; remove them",
		},
		{
			name: "the name is checked before the configuration",
			misuse: func(t *testing.T) func() {
				s := mustNew(t).Workers()
				return func() {
					s.Continuous("", registerIdle, credo.ContinuousConfig{Restart: credo.RestartPolicy{Limit: -1}})
				}
			},
			want: `credo: Workers.Continuous(""): the name is empty; name the worker`,
		},
		{
			name: "the name is checked before the tier and the configuration count",
			misuse: func(t *testing.T) func() {
				s := mustNew(t).Workers()
				return func() {
					s.Scheduled("", "@every 1m", registerIdle, credo.ScheduledConfig{Tier: 7}, credo.ScheduledConfig{})
				}
			},
			want: `credo: Workers.Scheduled(""): the name is empty; name the worker`,
		},
		{
			name: "bad schedule",
			misuse: func(t *testing.T) func() {
				s := mustNew(t).Workers()
				return func() { s.Scheduled("report", "not a cron", registerIdle) }
			},
			want: `credo: Workers.Scheduled("report"): ` + parseText + parseRemedy,
		},
		{
			name: "bad schedule on a constructor",
			misuse: func(t *testing.T) func() {
				s := mustNew(t).Workers()
				return func() { s.Scheduled("report", "not a cron", newRegisterBuiltA) }
			},
			want: `credo: Workers.Scheduled("report")` + ": " + parseText + parseRemedy,
		},
		{
			name: "the configuration is checked before the schedule",
			misuse: func(t *testing.T) func() {
				s := mustNew(t).Workers()
				return func() {
					s.Scheduled("report", "not a cron", registerIdle, credo.ScheduledConfig{RunTimeout: -time.Second})
				}
			},
			want: `credo: Workers.Scheduled("report"): ScheduledConfig.RunTimeout -1s is negative; ` +
				"use 0 for no timeout or a positive budget",
		},
		{
			name: "the schedule is checked before the duplicate name",
			misuse: func(t *testing.T) func() {
				s := mustNew(t).Workers()
				s.Scheduled("report", "@every 1m", registerIdle)
				return func() { s.Scheduled("report", "not a cron", registerIdle) }
			},
			want: `credo: Workers.Scheduled("report"): ` + parseText + parseRemedy,
		},
		{
			name: "duplicate name, Continuous then Scheduled",
			misuse: func(t *testing.T) func() {
				s := mustNew(t).Workers()
				s.Continuous("dup", registerIdle)
				return func() { s.Scheduled("dup", "@every 1m", registerIdle) }
			},
			want: `credo: Workers.Scheduled("dup")` + duplicate,
		},
		{
			name: "duplicate name, Scheduled then Continuous",
			misuse: func(t *testing.T) func() {
				s := mustNew(t).Workers()
				s.Scheduled("dup", "@every 1m", registerIdle)
				return func() { s.Continuous("dup", registerIdle) }
			},
			want: `credo: Workers.Continuous("dup")` + duplicate,
		},
		{
			name: "duplicate name, value then constructor",
			misuse: func(t *testing.T) func() {
				s := mustNew(t).Workers()
				s.Continuous("dup", registerIdle)
				return func() { s.Continuous("dup", newRegisterBuiltA) }
			},
			want: `credo: Workers.Continuous("dup")` + duplicate,
		},
		{
			name: "duplicate name, constructor then Scheduled",
			misuse: func(t *testing.T) func() {
				s := mustNew(t).Workers()
				s.Continuous("dup", newRegisterBuiltA)
				return func() { s.Scheduled("dup", "@every 1m", newRegisterBuiltB) }
			},
			want: `credo: Workers.Scheduled("dup")` + duplicate,
		},
		{
			name: "duplicate name across two calls of App.Workers",
			misuse: func(t *testing.T) func() {
				app := mustNew(t)
				app.Workers().Continuous("dup", registerIdle)
				return func() { app.Workers().Scheduled("dup", "@every 1m", registerIdle) }
			},
			want: `credo: Workers.Scheduled("dup")` + duplicate,
		},
		{
			name: "name taken by a component managed with credo.ComponentName",
			misuse: func(t *testing.T) func() {
				app := mustNew(t)
				app.Manage(&registerShutdowner{}, credo.ComponentName("worker:x"))
				s := app.Workers()
				return func() { s.Continuous("x", registerIdle) }
			},
			want: `credo: Workers.Continuous("x"): a component named "worker:x" is already registered, ` +
				"as Manage(worker:x); give each component its own name with credo.ComponentName",
		},
		{
			name: "provided, name taken by a component managed with credo.ComponentName",
			misuse: func(t *testing.T) func() {
				app := mustNew(t)
				app.Manage(&registerShutdowner{}, credo.ComponentName("worker:x"))
				s := app.Workers()
				return func() { s.Continuous("x", newRegisterBuiltA) }
			},
			want: `credo: Workers.Continuous("x")` +
				`: a component named "worker:x" is already registered, as Manage(worker:x); ` +
				"give each component its own name with credo.ComponentName",
		},
		{
			name: "after Finalize",
			misuse: func(t *testing.T) func() {
				app := mustNew(t)
				s := app.Workers()
				mustFinalize(t, app)
				return func() { s.Scheduled("report", "@every 1m", registerIdle) }
			},
			want: `credo: Workers.Scheduled("report"): ` + registerAfterFinalize,
		},
		{
			name: "constructor after Finalize",
			misuse: func(t *testing.T) func() {
				app := mustNew(t)
				s := app.Workers()
				mustFinalize(t, app)
				return func() { s.Continuous("consumer", newRegisterBuiltA) }
			},
			want: `credo: Workers.Continuous("consumer")` + ": " +
				registerAfterFinalize,
		},
		{
			name: "after the App is prepared by ServeHTTP",
			misuse: func(t *testing.T) func() {
				app := mustNew(t)
				s := app.Workers()
				app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
				return func() { s.Continuous("late", registerIdle) }
			},
			want: `credo: Workers.Continuous("late"): called after the container was finalized by the first ` +
				"ServeHTTP call; register before the App serves",
		},
		{
			name: "after shutdown",
			misuse: func(t *testing.T) func() {
				app := mustNew(t)
				s := app.Workers()
				if err := app.Shutdown(t.Context()); err != nil {
					t.Fatalf("Shutdown() = %v", err)
				}
				return func() { s.Continuous("late", registerIdle) }
			},
			want: `credo: Workers.Continuous("late"): called after shutdown began; ` +
				"registration ends when the App shuts down",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fn := tt.misuse(t)
			registerWant(t, mustPanicMessage(t, fn), tt.want)
		})
	}
}

// TestWorkersRegister_RefusedConstructorAddsNothing: a constructor
// registration the App refuses adds neither its loop nor its built value.
func TestWorkersRegister_RefusedConstructorAddsNothing(t *testing.T) {
	t.Run("refused by the App's component registry", func(t *testing.T) {
		app := mustNew(t)
		app.Manage(&registerShutdowner{}, credo.ComponentName("worker:x"))
		s := app.Workers()

		msg := mustPanicMessage(t, func() { s.Continuous("x", newRegisterBuiltA) })
		registerWant(t, msg, `credo: Workers.Continuous("x"): a component named "worker:x" is already `+
			"registered, as Manage(worker:x); give each component its own name with credo.ComponentName")

		// Neither name was taken: the built value's name is free, and the
		// constructor registers under another name.
		app.Manage(&registerComponent{}, credo.ComponentName("worker-value:x"))
		s.Continuous("y", newRegisterBuiltA)
		infos := s.Snapshot()
		if len(infos) != 1 || infos[0].Name != "y" {
			t.Fatalf("Snapshot() = %+v, want only y", infos)
		}
	})
	t.Run("the built value's name taken", func(t *testing.T) {
		app := mustNew(t)
		app.Manage(&registerComponent{}, credo.ComponentName("worker-value:x"))
		s := app.Workers()
		msg := mustPanicMessage(t, func() { s.Continuous("x", newRegisterBuiltA) })
		registerWant(t, msg, `credo: Workers.Continuous("x"): a component named "worker-value:x" is already `+
			"registered, as Manage(worker-value:x); give each component its own name with credo.ComponentName")
		// The loop was not added either: a worker value registers under x.
		s.Continuous("x", registerIdle)
	})
	t.Run("refused after Finalize", func(t *testing.T) {
		app := mustNew(t)
		s := app.Workers()
		mustFinalize(t, app)

		msg := mustPanicMessage(t, func() { s.Continuous("a", newRegisterBuiltA) })
		registerWant(t, msg, `credo: Workers.Continuous("a"): `+registerAfterFinalize)
		msg = mustPanicMessage(t, func() { s.Scheduled("b", "@every 1m", newRegisterBuiltA) })
		registerWant(t, msg, `credo: Workers.Scheduled("b"): `+registerAfterFinalize)
		if len(s.Snapshot()) != 0 {
			t.Errorf("Snapshot() = %+v, want empty", s.Snapshot())
		}
	})
}

func TestWorkersRegister_SameValueUnderTwoNames(t *testing.T) {
	// A value registered twice runs on two independent loops.
	s := mustNew(t).Workers()
	s.Continuous("a", registerIdle)
	s.Continuous("b", registerIdle)
	if got := len(s.Snapshot()); got != 2 {
		t.Fatalf("len(Snapshot()) = %d, want 2", got)
	}
}

func TestWorkers_OneRegistryPerApp(t *testing.T) {
	app := mustNew(t)
	first := app.Workers()
	if first == nil {
		t.Fatal("App.Workers() = nil")
	}
	for i := range 3 {
		if got := app.Workers(); got != first {
			t.Fatalf("App.Workers() call %d = %p, want the first call's %p", i+2, got, first)
		}
	}
	if other := mustNew(t).Workers(); other == first {
		t.Fatal("two Apps share one worker registry")
	}
}

func TestWorkers_RegistersNothing(t *testing.T) {
	app := mustNew(t)
	s := app.Workers()
	app.Workers()
	app.GET("/ping", func(ctx *credo.Context) error { return ctx.Response().Text(http.StatusOK, "pong") })

	if app.Has[*credo.Workers]() {
		t.Error("App.Workers bound the registry into the container")
	}
	if got := s.Snapshot(); len(got) != 0 {
		t.Errorf("Snapshot() = %+v, want empty", got)
	}
	if _, ok := s.Lookup("anything"); ok {
		t.Error(`Lookup("anything") = true on an empty registry`)
	}

	// No start work: the App serves through ServeHTTP without App.Start.
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ping", nil))
	if w.Code != http.StatusOK || w.Body.String() != "pong" {
		t.Fatalf("GET /ping = %d %q, want 200 pong", w.Code, w.Body.String())
	}
}

func TestWorkers_RegisteredWorkerIsStartWork(t *testing.T) {
	// The contrast: a registered worker is a component, so ServeHTTP refuses
	// to serve before App.Start.
	app := mustNew(t)
	app.Workers().Continuous("x", registerIdle)
	app.GET("/ping", func(ctx *credo.Context) error { return ctx.Response().Text(http.StatusOK, "pong") })

	msg := mustPanicMessage(t, func() {
		app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ping", nil))
	})
	for _, want := range []string{"has not been started", "call App.Start"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message = %q\nwant it to contain %q", msg, want)
		}
	}
}

// TestWorkers_TwoCallsShareOneSnapshot: workers registered through separate
// calls of App.Workers are one registry's, in registration order.
func TestWorkers_TwoCallsShareOneSnapshot(t *testing.T) {
	app := mustNew(t)
	app.Workers().Continuous("consumer", registerIdle)
	app.Workers().Scheduled("report", "@every 1h", registerIdle)
	app.Workers().Continuous("built", newRegisterBuiltA)

	want := []string{"consumer", "report", "built"}
	for i, s := range []*credo.Workers{app.Workers(), app.Workers()} {
		var got []string
		for _, info := range s.Snapshot() {
			got = append(got, info.Name)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("call %d: Snapshot() names = %q, want %q", i+1, got, want)
		}
		for _, name := range want {
			if _, ok := s.Lookup(name); !ok {
				t.Errorf("call %d: Lookup(%q) = false", i+1, name)
			}
		}
	}
}
