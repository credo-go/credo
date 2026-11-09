package credo_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
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

// registerProvidedA and registerProvidedB are the T of provided registrations.
type registerProvidedA struct{}

func (*registerProvidedA) Run(ctx context.Context) error { <-ctx.Done(); return nil }

type registerProvidedB struct{}

func (*registerProvidedB) Run(ctx context.Context) error { <-ctx.Done(); return nil }

// registerShutdowner is a component that is not a worker.
type registerShutdowner struct{}

func (*registerShutdowner) Shutdown(context.Context) error { return nil }

// registerWant fails the test unless msg is exactly want.
func registerWant(t *testing.T, msg, want string) {
	t.Helper()
	if msg != want {
		t.Errorf("message = %q\nwant      %q", msg, want)
	}
}

// registerProvidedCall is the panic prefix of a provided registration over T.
func registerProvidedCall[T credo.Worker](method, name string) string {
	return fmt.Sprintf("credo: Workers.%s[%s](%q)", method, reflect.TypeFor[T](), name)
}

// registerAfterFinalize is the container's reason for a registration after
// Finalize.
const registerAfterFinalize = "called after Finalize; make every registration before Finalize, " +
	"which Run and the first ServeHTTP call implicitly"

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
			t.Run("ContinuousProvided", func(t *testing.T) {
				s := mustNew(t).Workers()
				s.ContinuousProvided[*registerProvidedA]("w", tt.cfg...)
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
			t.Run("ScheduledProvided", func(t *testing.T) {
				s := mustNew(t).Workers()
				s.ScheduledProvided[*registerProvidedA]("w", "@every 1m", tt.cfg...)
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
			t.Run("ContinuousProvided", func(t *testing.T) {
				s := mustNew(t).Workers()
				msg := mustPanicMessage(t, func() {
					s.ContinuousProvided[*registerProvidedA]("order-consumer", tt.cfg...)
				})
				call := registerProvidedCall[*registerProvidedA]("ContinuousProvided", "order-consumer")
				registerWant(t, msg, call+": "+tt.want)
				if len(s.Snapshot()) != 0 {
					t.Errorf("Snapshot() = %+v after a refused registration, want empty", s.Snapshot())
				}
				// The refused registration reserved neither the name nor T.
				s.ContinuousProvided[*registerProvidedA]("order-consumer")
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
			t.Run("ScheduledProvided", func(t *testing.T) {
				s := mustNew(t).Workers()
				msg := mustPanicMessage(t, func() {
					s.ScheduledProvided[*registerProvidedA]("report", "@every 1m", tt.cfg...)
				})
				call := registerProvidedCall[*registerProvidedA]("ScheduledProvided", "report")
				registerWant(t, msg, call+": "+tt.want)
				if len(s.Snapshot()) != 0 {
					t.Errorf("Snapshot() = %+v after a refused registration, want empty", s.Snapshot())
				}
				s.ScheduledProvided[*registerProvidedA]("report", "@every 1m")
			})
		})
	}
}

func TestWorkersRegister_Misuse(t *testing.T) {
	var nilPointer *registerProvidedA
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
				return func() { s.ContinuousProvided[*registerProvidedA]("x\t") }
			},
			want: registerProvidedCall[*registerProvidedA]("ContinuousProvided", "x\t") +
				": the name has leading or trailing whitespace; names are never trimmed, so remove it",
		},
		{
			name: "control character",
			misuse: func(t *testing.T) func() {
				s := mustNew(t).Workers()
				return func() { s.ScheduledProvided[*registerProvidedA]("a\nb", "@every 1m") }
			},
			want: registerProvidedCall[*registerProvidedA]("ScheduledProvided", "a\nb") +
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
			name: "bad schedule on a provided worker",
			misuse: func(t *testing.T) func() {
				s := mustNew(t).Workers()
				return func() { s.ScheduledProvided[*registerProvidedA]("report", "not a cron") }
			},
			want: registerProvidedCall[*registerProvidedA]("ScheduledProvided", "report") + ": " + parseText + parseRemedy,
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
			name: "duplicate name, value then ContinuousProvided",
			misuse: func(t *testing.T) func() {
				s := mustNew(t).Workers()
				s.Continuous("dup", registerIdle)
				return func() { s.ContinuousProvided[*registerProvidedA]("dup") }
			},
			want: registerProvidedCall[*registerProvidedA]("ContinuousProvided", "dup") + duplicate,
		},
		{
			name: "duplicate name, provided then ScheduledProvided",
			misuse: func(t *testing.T) func() {
				s := mustNew(t).Workers()
				s.ContinuousProvided[*registerProvidedA]("dup")
				return func() { s.ScheduledProvided[*registerProvidedB]("dup", "@every 1m") }
			},
			want: registerProvidedCall[*registerProvidedB]("ScheduledProvided", "dup") + duplicate,
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
			name: "one T under two names",
			misuse: func(t *testing.T) func() {
				s := mustNew(t).Workers()
				s.ContinuousProvided[*registerProvidedA]("first")
				return func() { s.ScheduledProvided[*registerProvidedA]("second", "@every 1m") }
			},
			want: registerProvidedCall[*registerProvidedA]("ScheduledProvided", "second") +
				fmt.Sprintf(`: %s is already registered as worker "first"; the container hands every `+
					"registration the same instance, so register it once", reflect.TypeFor[*registerProvidedA]()),
		},
		{
			name: "one T under two names across two calls of App.Workers",
			misuse: func(t *testing.T) func() {
				app := mustNew(t)
				app.Workers().ContinuousProvided[*registerProvidedA]("first")
				return func() { app.Workers().ContinuousProvided[*registerProvidedA]("second") }
			},
			want: registerProvidedCall[*registerProvidedA]("ContinuousProvided", "second") +
				fmt.Sprintf(`: %s is already registered as worker "first"; the container hands every `+
					"registration the same instance, so register it once", reflect.TypeFor[*registerProvidedA]()),
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
				return func() { s.ContinuousProvided[*registerProvidedA]("x") }
			},
			want: registerProvidedCall[*registerProvidedA]("ContinuousProvided", "x") +
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
			name: "provided after Finalize",
			misuse: func(t *testing.T) func() {
				app := mustNew(t)
				s := app.Workers()
				mustFinalize(t, app)
				return func() { s.ContinuousProvided[*registerProvidedA]("consumer") }
			},
			want: registerProvidedCall[*registerProvidedA]("ContinuousProvided", "consumer") + ": " +
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
			want: `credo: Workers.Continuous("late"): ` + registerAfterFinalize,
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

// TestWorkersRegister_RefusedRegistrationLeavesTheProvidedTypeFree: a
// registration the App refuses records neither its name nor its T.
func TestWorkersRegister_RefusedRegistrationLeavesTheProvidedTypeFree(t *testing.T) {
	t.Run("refused by the App's component registry", func(t *testing.T) {
		app := mustNew(t)
		app.Manage(&registerShutdowner{}, credo.ComponentName("worker:x"))
		s := app.Workers()

		msg := mustPanicMessage(t, func() { s.ContinuousProvided[*registerProvidedA]("x") })
		if !strings.Contains(msg, `a component named "worker:x" is already registered`) {
			t.Fatalf("message = %q, want the App's refusal of worker:x", msg)
		}

		// T was not recorded under the refused name: the same T registers
		// under a free name.
		s.ContinuousProvided[*registerProvidedA]("y")
		infos := s.Snapshot()
		if len(infos) != 1 || infos[0].Name != "y" {
			t.Fatalf("Snapshot() = %+v, want only y", infos)
		}
	})
	t.Run("refused after Finalize", func(t *testing.T) {
		app := mustNew(t)
		s := app.Workers()
		mustFinalize(t, app)

		msg := mustPanicMessage(t, func() { s.ContinuousProvided[*registerProvidedA]("a") })
		registerWant(t, msg, registerProvidedCall[*registerProvidedA]("ContinuousProvided", "a")+": "+
			registerAfterFinalize)

		// A recorded refusal would report T as already registered as "a".
		msg = mustPanicMessage(t, func() { s.ScheduledProvided[*registerProvidedA]("b", "@every 1m") })
		registerWant(t, msg, registerProvidedCall[*registerProvidedA]("ScheduledProvided", "b")+": "+
			registerAfterFinalize)
		if len(s.Snapshot()) != 0 {
			t.Errorf("Snapshot() = %+v, want empty", s.Snapshot())
		}
	})
}

func TestWorkersRegister_ProvidedTypeIsPerApp(t *testing.T) {
	// The one-T rule is per App: two Apps each register the same T.
	mustNew(t).Workers().ContinuousProvided[*registerProvidedA]("consumer")
	mustNew(t).Workers().ContinuousProvided[*registerProvidedA]("consumer")
}

func TestWorkersRegister_SameValueUnderTwoNames(t *testing.T) {
	// A value registered twice runs on two independent loops; only a provided T is refused.
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
	app.Workers().ContinuousProvided[*registerProvidedA]("provided")

	want := []string{"consumer", "report", "provided"}
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
