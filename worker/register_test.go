package worker

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
var registerIdle = Func(func(ctx context.Context) error {
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

// registerContains fails the test unless msg contains every part of want.
func registerContains(t *testing.T, msg string, want ...string) {
	t.Helper()
	for _, part := range want {
		if !strings.Contains(msg, part) {
			t.Errorf("message = %q\nwant it to contain %q", msg, part)
		}
	}
}

// registerProvidedCall is the call text of a provided registration over T.
func registerProvidedCall[T Worker](method, name string) string {
	return fmt.Sprintf("%s[%s](%q)", method, reflect.TypeFor[T](), name)
}

func TestConfiguration_ContinuousResolution(t *testing.T) {
	tests := []struct {
		name string
		cfg  []ContinuousConfig
		want ContinuousConfig
	}{
		{
			name: "no configuration",
			want: ContinuousConfig{
				Tier:    credo.TierInternal,
				Restart: Restart{MinDelay: DefaultMinRestartDelay, MaxDelay: DefaultMaxRestartDelay},
			},
		},
		{
			name: "zero configuration",
			cfg:  []ContinuousConfig{{}},
			want: ContinuousConfig{
				Tier:    credo.TierInternal,
				Restart: Restart{MinDelay: DefaultMinRestartDelay, MaxDelay: DefaultMaxRestartDelay},
			},
		},
		{
			name: "ingress tier",
			cfg:  []ContinuousConfig{{Tier: credo.TierIngress}},
			want: ContinuousConfig{
				Tier:    credo.TierIngress,
				Restart: Restart{MinDelay: DefaultMinRestartDelay, MaxDelay: DefaultMaxRestartDelay},
			},
		},
		{
			name: "explicit internal tier",
			cfg:  []ContinuousConfig{{Tier: credo.TierInternal}},
			want: ContinuousConfig{
				Tier:    credo.TierInternal,
				Restart: Restart{MinDelay: DefaultMinRestartDelay, MaxDelay: DefaultMaxRestartDelay},
			},
		},
		{
			name: "MinDelay below the default cap keeps the cap",
			cfg:  []ContinuousConfig{{Restart: Restart{MinDelay: 5 * time.Second}}},
			want: ContinuousConfig{
				Tier:    credo.TierInternal,
				Restart: Restart{MinDelay: 5 * time.Second, MaxDelay: DefaultMaxRestartDelay},
			},
		},
		{
			name: "MinDelay alone above the default cap is a fixed delay",
			cfg:  []ContinuousConfig{{Restart: Restart{MinDelay: 10 * time.Minute}}},
			want: ContinuousConfig{
				Tier:    credo.TierInternal,
				Restart: Restart{MinDelay: 10 * time.Minute, MaxDelay: 10 * time.Minute},
			},
		},
		{
			name: "MaxDelay alone above the default floor",
			cfg:  []ContinuousConfig{{Restart: Restart{MaxDelay: 2 * time.Minute}}},
			want: ContinuousConfig{
				Tier:    credo.TierInternal,
				Restart: Restart{MinDelay: DefaultMinRestartDelay, MaxDelay: 2 * time.Minute},
			},
		},
		{
			name: "MaxDelay alone equal to the default floor",
			cfg:  []ContinuousConfig{{Restart: Restart{MaxDelay: DefaultMinRestartDelay}}},
			want: ContinuousConfig{
				Tier:    credo.TierInternal,
				Restart: Restart{MinDelay: DefaultMinRestartDelay, MaxDelay: DefaultMinRestartDelay},
			},
		},
		{
			name: "equal delays",
			cfg:  []ContinuousConfig{{Restart: Restart{MinDelay: 5 * time.Second, MaxDelay: 5 * time.Second}}},
			want: ContinuousConfig{
				Tier:    credo.TierInternal,
				Restart: Restart{MinDelay: 5 * time.Second, MaxDelay: 5 * time.Second},
			},
		},
		{
			name: "limit",
			cfg:  []ContinuousConfig{{Restart: Restart{Limit: 5}}},
			want: ContinuousConfig{
				Tier:    credo.TierInternal,
				Restart: Restart{Limit: 5, MinDelay: DefaultMinRestartDelay, MaxDelay: DefaultMaxRestartDelay},
			},
		},
		{
			name: "disabled keeps its delays zero",
			cfg:  []ContinuousConfig{{Restart: Restart{Disabled: true}}},
			want: ContinuousConfig{Tier: credo.TierInternal, Restart: Restart{Disabled: true}},
		},
		{
			name: "readiness condition",
			cfg:  []ContinuousConfig{{UnreadyWhenFailed: true}},
			want: ContinuousConfig{
				Tier:              credo.TierInternal,
				Restart:           Restart{MinDelay: DefaultMinRestartDelay, MaxDelay: DefaultMaxRestartDelay},
				UnreadyWhenFailed: true,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check := func(t *testing.T, s *Supervisor) {
				t.Helper()
				info, ok := s.Lookup("w")
				if !ok {
					t.Fatal(`Lookup("w") = false`)
				}
				if info.Kind != KindContinuous || info.Status != StatusPending || info.Schedule != "" {
					t.Errorf("Info = %+v, want a pending continuous worker without a schedule", info)
				}
				if info.Scheduled != nil {
					t.Errorf("Info.Scheduled = %+v, want nil", info.Scheduled)
				}
				if info.Continuous == nil || *info.Continuous != tt.want {
					t.Errorf("Info.Continuous = %+v, want %+v", info.Continuous, tt.want)
				}
			}
			t.Run("Continuous", func(t *testing.T) {
				s := Use(newTestApp(t))
				s.Continuous("w", registerIdle, tt.cfg...)
				check(t, s)
			})
			t.Run("ContinuousProvided", func(t *testing.T) {
				s := Use(newTestApp(t))
				s.ContinuousProvided[*registerProvidedA]("w", tt.cfg...)
				check(t, s)
			})
		})
	}
}

func TestConfiguration_ScheduledResolution(t *testing.T) {
	full := ScheduledConfig{
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
		cfg  []ScheduledConfig
		want ScheduledConfig
	}{
		{name: "no configuration", want: ScheduledConfig{Tier: credo.TierIngress}},
		{name: "zero configuration", cfg: []ScheduledConfig{{}}, want: ScheduledConfig{Tier: credo.TierIngress}},
		{
			name: "internal tier",
			cfg:  []ScheduledConfig{{Tier: credo.TierInternal}},
			want: ScheduledConfig{Tier: credo.TierInternal},
		},
		{
			name: "explicit ingress tier",
			cfg:  []ScheduledConfig{{Tier: credo.TierIngress}},
			want: ScheduledConfig{Tier: credo.TierIngress},
		},
		{name: "every field set", cfg: []ScheduledConfig{full}, want: fullIngress},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check := func(t *testing.T, s *Supervisor) {
				t.Helper()
				info, ok := s.Lookup("w")
				if !ok {
					t.Fatal(`Lookup("w") = false`)
				}
				if info.Kind != KindScheduled || info.Status != StatusPending || info.Schedule != "@every 1m" {
					t.Errorf("Info = %+v, want a pending scheduled worker on @every 1m", info)
				}
				if info.Continuous != nil {
					t.Errorf("Info.Continuous = %+v, want nil", info.Continuous)
				}
				if info.Scheduled == nil || *info.Scheduled != tt.want {
					t.Errorf("Info.Scheduled = %+v, want %+v", info.Scheduled, tt.want)
				}
			}
			t.Run("Scheduled", func(t *testing.T) {
				s := Use(newTestApp(t))
				s.Scheduled("w", "@every 1m", registerIdle, tt.cfg...)
				check(t, s)
			})
			t.Run("ScheduledProvided", func(t *testing.T) {
				s := Use(newTestApp(t))
				s.ScheduledProvided[*registerProvidedA]("w", "@every 1m", tt.cfg...)
				check(t, s)
			})
		})
	}
}

func TestConfiguration_ContinuousRejected(t *testing.T) {
	tests := []struct {
		name string
		cfg  []ContinuousConfig
		want []string
	}{
		{
			name: "two configurations",
			cfg:  []ContinuousConfig{{}, {}},
			want: []string{"2 configurations given; pass at most one worker.ContinuousConfig"},
		},
		{
			name: "invalid tier",
			cfg:  []ContinuousConfig{{Tier: credo.Tier(7)}},
			want: []string{
				"Tier(7) is not a tier",
				"use credo.TierIngress, credo.TierInternal, or zero for the default",
			},
		},
		{
			name: "disabled beside limit",
			cfg:  []ContinuousConfig{{Restart: Restart{Disabled: true, Limit: 1}}},
			want: []string{"Restart.Disabled beside Restart.Limit, MinDelay or MaxDelay", "set Disabled alone"},
		},
		{
			name: "disabled beside MinDelay",
			cfg:  []ContinuousConfig{{Restart: Restart{Disabled: true, MinDelay: time.Second}}},
			want: []string{"Restart.Disabled beside", "set Disabled alone"},
		},
		{
			name: "disabled beside MaxDelay",
			cfg:  []ContinuousConfig{{Restart: Restart{Disabled: true, MaxDelay: time.Second}}},
			want: []string{"Restart.Disabled beside", "set Disabled alone"},
		},
		{
			name: "negative limit",
			cfg:  []ContinuousConfig{{Restart: Restart{Limit: -1}}},
			want: []string{"Restart.Limit -1 is negative", "use 0 for unlimited restarts or a positive limit"},
		},
		{
			name: "negative MinDelay",
			cfg:  []ContinuousConfig{{Restart: Restart{MinDelay: -time.Second}}},
			want: []string{"Restart.MinDelay -1s is negative", "use 0 for the default 3s or a positive delay"},
		},
		{
			name: "negative MaxDelay",
			cfg:  []ContinuousConfig{{Restart: Restart{MaxDelay: -time.Second}}},
			want: []string{"Restart.MaxDelay -1s is negative", "use 0 for the default or a positive cap"},
		},
		{
			name: "MaxDelay below the default floor",
			cfg:  []ContinuousConfig{{Restart: Restart{MaxDelay: time.Second}}},
			want: []string{
				"Restart.MaxDelay 1s is below MinDelay 3s (the default)",
				"set MinDelay to at most 1s, or raise MaxDelay",
			},
		},
		{
			name: "MaxDelay below an explicit MinDelay",
			cfg:  []ContinuousConfig{{Restart: Restart{MinDelay: 10 * time.Second, MaxDelay: 5 * time.Second}}},
			want: []string{
				"Restart.MaxDelay 5s is below MinDelay 10s;",
				"lower MinDelay to at most 5s, or raise MaxDelay",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Run("Continuous", func(t *testing.T) {
				s := Use(newTestApp(t))
				msg := mustPanic(t, func() { s.Continuous("order-consumer", registerIdle, tt.cfg...) })
				registerContains(t, msg, append([]string{`worker: Continuous("order-consumer")`}, tt.want...)...)
				// A refused registration leaves nothing behind.
				if len(s.Snapshot()) != 0 {
					t.Errorf("Snapshot() = %+v after a refused registration, want empty", s.Snapshot())
				}
				s.Continuous("order-consumer", registerIdle)
			})
			t.Run("ContinuousProvided", func(t *testing.T) {
				s := Use(newTestApp(t))
				msg := mustPanic(t, func() { s.ContinuousProvided[*registerProvidedA]("order-consumer", tt.cfg...) })
				call := registerProvidedCall[*registerProvidedA]("worker: ContinuousProvided", "order-consumer")
				registerContains(t, msg, append([]string{call}, tt.want...)...)
				if len(s.Snapshot()) != 0 {
					t.Errorf("Snapshot() = %+v after a refused registration, want empty", s.Snapshot())
				}
				// The refused registration reserved neither the name nor T.
				s.ContinuousProvided[*registerProvidedA]("order-consumer")
			})
		})
	}
	// The default-floor message says the floor is the default; the explicit one does not.
	s := Use(newTestApp(t))
	msg := mustPanic(t, func() {
		s.Continuous("x", registerIdle,
			ContinuousConfig{Restart: Restart{MinDelay: 10 * time.Second, MaxDelay: time.Second}})
	})
	if strings.Contains(msg, "(the default)") {
		t.Errorf("message = %q, want no (the default) for an explicit MinDelay", msg)
	}
}

func TestConfiguration_ScheduledRejected(t *testing.T) {
	tests := []struct {
		name string
		cfg  []ScheduledConfig
		want []string
	}{
		{
			name: "two configurations",
			cfg:  []ScheduledConfig{{}, {}},
			want: []string{"2 configurations given; pass at most one worker.ScheduledConfig"},
		},
		{
			name: "invalid tier",
			cfg:  []ScheduledConfig{{Tier: credo.Tier(7)}},
			want: []string{
				"Tier(7) is not a tier",
				"use credo.TierIngress, credo.TierInternal, or zero for the default",
			},
		},
		{
			name: "negative RunTimeout",
			cfg:  []ScheduledConfig{{RunTimeout: -time.Second}},
			want: []string{"ScheduledConfig.RunTimeout -1s is negative", "use 0 for no timeout or a positive budget"},
		},
		{
			name: "negative MaxConsecutiveFailures",
			cfg:  []ScheduledConfig{{MaxConsecutiveFailures: -1}},
			want: []string{
				"ScheduledConfig.MaxConsecutiveFailures -1 is negative",
				"use 0 for unlimited failures or a positive limit",
			},
		},
		{
			name: "negative UnreadyAfterSuccessAge",
			cfg:  []ScheduledConfig{{UnreadyAfterSuccessAge: -time.Second}},
			want: []string{
				"ScheduledConfig.UnreadyAfterSuccessAge -1s is negative",
				"use 0 to turn the check off or a positive age",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Run("Scheduled", func(t *testing.T) {
				s := Use(newTestApp(t))
				msg := mustPanic(t, func() { s.Scheduled("report", "@every 1m", registerIdle, tt.cfg...) })
				registerContains(t, msg, append([]string{`worker: Scheduled("report")`}, tt.want...)...)
				if len(s.Snapshot()) != 0 {
					t.Errorf("Snapshot() = %+v after a refused registration, want empty", s.Snapshot())
				}
				s.Scheduled("report", "@every 1m", registerIdle)
			})
			t.Run("ScheduledProvided", func(t *testing.T) {
				s := Use(newTestApp(t))
				msg := mustPanic(t, func() {
					s.ScheduledProvided[*registerProvidedA]("report", "@every 1m", tt.cfg...)
				})
				call := registerProvidedCall[*registerProvidedA]("worker: ScheduledProvided", "report")
				registerContains(t, msg, append([]string{call}, tt.want...)...)
				if len(s.Snapshot()) != 0 {
					t.Errorf("Snapshot() = %+v after a refused registration, want empty", s.Snapshot())
				}
				s.ScheduledProvided[*registerProvidedA]("report", "@every 1m")
			})
		})
	}
}

func TestRegister_Misuse(t *testing.T) {
	var nilPointer *registerProvidedA
	_, parseErr := ParseSchedule("not a cron")
	if parseErr == nil {
		t.Fatal(`ParseSchedule("not a cron") = nil error`)
	}
	// The panic carries the parse error under the call, without its own prefix.
	parseText := strings.TrimPrefix(parseErr.Error(), "worker: ")

	tests := []struct {
		name string
		// misuse performs the setup and returns the call that must panic.
		misuse func(t *testing.T) func()
		want   []string
	}{
		{
			name:   "Use with a nil app",
			misuse: func(*testing.T) func() { return func() { Use(nil) } },
			want:   []string{"worker: Use: app must not be nil", "pass the *credo.App the workers belong to"},
		},
		{
			name: "nil worker",
			misuse: func(t *testing.T) func() {
				s := Use(newTestApp(t))
				return func() { s.Continuous("x", nil) }
			},
			want: []string{`worker: Continuous("x"): the worker is nil`, "pass a value with a Run method"},
		},
		{
			name: "nil Func",
			misuse: func(t *testing.T) func() {
				s := Use(newTestApp(t))
				return func() { s.Scheduled("x", "@every 1m", Func(nil)) }
			},
			want: []string{`worker: Scheduled("x"): the worker is nil`, "pass a value with a Run method"},
		},
		{
			name: "typed-nil pointer",
			misuse: func(t *testing.T) func() {
				s := Use(newTestApp(t))
				return func() { s.Continuous("x", nilPointer) }
			},
			want: []string{`worker: Continuous("x"): the worker is nil`, "pass a value with a Run method"},
		},
		{
			name: "nil worker is checked before the name",
			misuse: func(t *testing.T) func() {
				s := Use(newTestApp(t))
				return func() { s.Continuous("", nil) }
			},
			want: []string{`worker: Continuous(""): the worker is nil`},
		},
		{
			name: "empty name",
			misuse: func(t *testing.T) func() {
				s := Use(newTestApp(t))
				return func() { s.Continuous("", registerIdle) }
			},
			want: []string{`worker: Continuous(""): the name is empty; name the worker`},
		},
		{
			name: "padded name",
			misuse: func(t *testing.T) func() {
				s := Use(newTestApp(t))
				return func() { s.Scheduled(" report", "@every 1m", registerIdle) }
			},
			want: []string{
				`worker: Scheduled(" report"): the name has leading or trailing whitespace`,
				"names are never trimmed, so remove it",
			},
		},
		{
			name: "trailing whitespace",
			misuse: func(t *testing.T) func() {
				s := Use(newTestApp(t))
				return func() { s.ContinuousProvided[*registerProvidedA]("x\t") }
			},
			want: []string{
				registerProvidedCall[*registerProvidedA]("ContinuousProvided", "x\t"),
				"leading or trailing whitespace",
			},
		},
		{
			name: "control character",
			misuse: func(t *testing.T) func() {
				s := Use(newTestApp(t))
				return func() { s.ScheduledProvided[*registerProvidedA]("a\nb", "@every 1m") }
			},
			want: []string{
				registerProvidedCall[*registerProvidedA]("ScheduledProvided", "a\nb"),
				"the name contains control characters; remove them",
			},
		},
		{
			name: "the name is checked before the configuration",
			misuse: func(t *testing.T) func() {
				s := Use(newTestApp(t))
				return func() { s.Continuous("", registerIdle, ContinuousConfig{Tier: credo.Tier(7)}) }
			},
			want: []string{"the name is empty"},
		},
		{
			name: "bad schedule",
			misuse: func(t *testing.T) func() {
				s := Use(newTestApp(t))
				return func() { s.Scheduled("report", "not a cron", registerIdle) }
			},
			want: []string{
				`worker: Scheduled("report"): ` + parseText,
				"validate a schedule from configuration with worker.ParseSchedule before registering it",
			},
		},
		{
			name: "bad schedule on a provided worker",
			misuse: func(t *testing.T) func() {
				s := Use(newTestApp(t))
				return func() { s.ScheduledProvided[*registerProvidedA]("report", "not a cron") }
			},
			want: []string{
				registerProvidedCall[*registerProvidedA]("worker: ScheduledProvided", "report"),
				parseText,
				"worker.ParseSchedule",
			},
		},
		{
			name: "the configuration is checked before the schedule",
			misuse: func(t *testing.T) func() {
				s := Use(newTestApp(t))
				return func() {
					s.Scheduled("report", "not a cron", registerIdle, ScheduledConfig{RunTimeout: -time.Second})
				}
			},
			want: []string{"ScheduledConfig.RunTimeout -1s is negative"},
		},
		{
			name: "the schedule is checked before the duplicate name",
			misuse: func(t *testing.T) func() {
				s := Use(newTestApp(t))
				s.Scheduled("report", "@every 1m", registerIdle)
				return func() { s.Scheduled("report", "not a cron", registerIdle) }
			},
			want: []string{parseText},
		},
		{
			name: "duplicate name, Continuous then Scheduled",
			misuse: func(t *testing.T) func() {
				s := Use(newTestApp(t))
				s.Continuous("dup", registerIdle)
				return func() { s.Scheduled("dup", "@every 1m", registerIdle) }
			},
			want: []string{`worker: Scheduled("dup"): duplicate worker name "dup"`, "give each worker its own name"},
		},
		{
			name: "duplicate name, Scheduled then Continuous",
			misuse: func(t *testing.T) func() {
				s := Use(newTestApp(t))
				s.Scheduled("dup", "@every 1m", registerIdle)
				return func() { s.Continuous("dup", registerIdle) }
			},
			want: []string{`worker: Continuous("dup"): duplicate worker name "dup"`, "give each worker its own name"},
		},
		{
			name: "duplicate name, value then ContinuousProvided",
			misuse: func(t *testing.T) func() {
				s := Use(newTestApp(t))
				s.Continuous("dup", registerIdle)
				return func() { s.ContinuousProvided[*registerProvidedA]("dup") }
			},
			want: []string{
				registerProvidedCall[*registerProvidedA]("ContinuousProvided", "dup"),
				`duplicate worker name "dup"`,
				"give each worker its own name",
			},
		},
		{
			name: "duplicate name, provided then ScheduledProvided",
			misuse: func(t *testing.T) func() {
				s := Use(newTestApp(t))
				s.ContinuousProvided[*registerProvidedA]("dup")
				return func() { s.ScheduledProvided[*registerProvidedB]("dup", "@every 1m") }
			},
			want: []string{
				registerProvidedCall[*registerProvidedB]("ScheduledProvided", "dup"),
				`duplicate worker name "dup"`,
			},
		},
		{
			name: "one T under two names in one supervisor",
			misuse: func(t *testing.T) func() {
				s := Use(newTestApp(t))
				s.ContinuousProvided[*registerProvidedA]("first")
				return func() { s.ScheduledProvided[*registerProvidedA]("second", "@every 1m") }
			},
			want: []string{
				registerProvidedCall[*registerProvidedA]("worker: ScheduledProvided", "second"),
				fmt.Sprintf(`%s is already registered as worker "first"`, reflect.TypeFor[*registerProvidedA]()),
				"so register it once",
			},
		},
		{
			name: "one T under two names in two supervisors of one App",
			misuse: func(t *testing.T) func() {
				app := newTestApp(t)
				Use(app).ContinuousProvided[*registerProvidedA]("first")
				s2 := Use(app)
				return func() { s2.ContinuousProvided[*registerProvidedA]("second") }
			},
			want: []string{
				registerProvidedCall[*registerProvidedA]("worker: ContinuousProvided", "second"),
				`is already registered as worker "first"`,
				"so register it once",
			},
		},
		{
			name: "same name in two supervisors of one App",
			misuse: func(t *testing.T) func() {
				app := newTestApp(t)
				Use(app).Continuous("x", registerIdle)
				s2 := Use(app)
				return func() { s2.Scheduled("x", "@every 1m", registerIdle) }
			},
			want: []string{
				`worker: Scheduled("x"): the App already has a component named "worker:x"`,
				"give each worker its own name",
			},
		},
		{
			name: "name taken by a component managed with credo.Named",
			misuse: func(t *testing.T) func() {
				app := newTestApp(t)
				app.Manage(&registerShutdowner{}, credo.Named("worker:x"))
				s := Use(app)
				return func() { s.Continuous("x", registerIdle) }
			},
			want: []string{
				`worker: Continuous("x"): the App already has a component named "worker:x"`,
				"give each worker its own name",
			},
		},
		{
			name: "after Finalize",
			misuse: func(t *testing.T) func() {
				app := newTestApp(t)
				s := Use(app)
				finalize(t, app)
				return func() { s.Scheduled("report", "@every 1m", registerIdle) }
			},
			want: []string{`worker: Scheduled("report") after app.Finalize; register workers before Finalize`},
		},
		{
			name: "provided after Finalize",
			misuse: func(t *testing.T) func() {
				app := newTestApp(t)
				s := Use(app)
				finalize(t, app)
				return func() { s.ContinuousProvided[*registerProvidedA]("consumer") }
			},
			want: []string{
				registerProvidedCall[*registerProvidedA]("worker: ContinuousProvided", "consumer") +
					" after app.Finalize; register workers before Finalize",
			},
		},
		{
			name: "after the App is prepared by ServeHTTP",
			misuse: func(t *testing.T) func() {
				app := newTestApp(t)
				s := Use(app)
				app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
				return func() { s.Continuous("late", registerIdle) }
			},
			want: []string{`worker: Continuous("late") after app.Finalize; register workers before Finalize`},
		},
		{
			name: "after shutdown",
			misuse: func(t *testing.T) func() {
				app := newTestApp(t)
				s := Use(app)
				if err := app.Shutdown(t.Context()); err != nil {
					t.Fatalf("Shutdown() = %v", err)
				}
				return func() { s.Continuous("late", registerIdle) }
			},
			want: []string{
				`worker: Continuous("late") after the App shut down`,
				"register workers before Finalize",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fn := tt.misuse(t)
			registerContains(t, mustPanic(t, fn), tt.want...)
		})
	}
}

func TestRegister_RefusedRegistrationReleasesProvidedType(t *testing.T) {
	t.Run("refused by the App's component registry", func(t *testing.T) {
		app := newTestApp(t)
		Use(app).Continuous("x", registerIdle)
		s := Use(app)

		msg := mustPanic(t, func() { s.ContinuousProvided[*registerProvidedA]("x") })
		registerContains(t, msg, `the App already has a component named "worker:x"`)

		// T's reservation was released with the refused component: the same T
		// registers under a free name.
		s.ContinuousProvided[*registerProvidedA]("y")
		infos := s.Snapshot()
		if len(infos) != 1 || infos[0].Name != "y" {
			t.Fatalf("Snapshot() = %+v, want only y", infos)
		}
	})
	t.Run("refused after Finalize", func(t *testing.T) {
		app := newTestApp(t)
		s := Use(app)
		finalize(t, app)

		msg := mustPanic(t, func() { s.ContinuousProvided[*registerProvidedA]("a") })
		registerContains(t, msg, "after app.Finalize")

		// A lingering reservation would report T as already registered as "a".
		msg = mustPanic(t, func() { s.ScheduledProvided[*registerProvidedA]("b", "@every 1m") })
		registerContains(t, msg, `ScheduledProvided`, `("b") after app.Finalize`)
		if strings.Contains(msg, "already registered") {
			t.Errorf("message = %q, want no lingering reservation of the refused registration", msg)
		}
		if len(s.Snapshot()) != 0 {
			t.Errorf("Snapshot() = %+v, want empty", s.Snapshot())
		}
	})
}

func TestRegister_ProvidedTypeIsPerApp(t *testing.T) {
	// The one-T rule is per App: two Apps each register the same T.
	Use(newTestApp(t)).ContinuousProvided[*registerProvidedA]("consumer")
	Use(newTestApp(t)).ContinuousProvided[*registerProvidedA]("consumer")
}

func TestRegister_SameValueUnderTwoNames(t *testing.T) {
	// A value registered twice runs on two independent loops; only a provided T is refused.
	s := Use(newTestApp(t))
	s.Continuous("a", registerIdle)
	s.Continuous("b", registerIdle)
	if got := len(s.Snapshot()); got != 2 {
		t.Fatalf("len(Snapshot()) = %d, want 2", got)
	}
}

func TestUse_RegistersNothing(t *testing.T) {
	app := newTestApp(t)
	s := Use(app)
	Use(app) // each call is an independent registry
	app.GET("/ping", func(ctx *credo.Context) error { return ctx.Response().Text(http.StatusOK, "pong") })

	if app.Has[*Supervisor]() {
		t.Error("Use bound the supervisor into the container")
	}
	if got := s.Snapshot(); len(got) != 0 {
		t.Errorf("Snapshot() = %+v, want empty", got)
	}
	if _, ok := s.Lookup("anything"); ok {
		t.Error(`Lookup("anything") = true on an empty supervisor`)
	}

	// No start work: the App serves through ServeHTTP without App.Start.
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ping", nil))
	if w.Code != http.StatusOK || w.Body.String() != "pong" {
		t.Fatalf("GET /ping = %d %q, want 200 pong", w.Code, w.Body.String())
	}
}

func TestUse_RegisteredWorkerIsStartWork(t *testing.T) {
	// The contrast: a registered worker is a component, so ServeHTTP refuses
	// to serve before App.Start.
	app := newTestApp(t)
	Use(app).Continuous("x", registerIdle)
	app.GET("/ping", func(ctx *credo.Context) error { return ctx.Response().Text(http.StatusOK, "pong") })

	msg := mustPanic(t, func() {
		app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ping", nil))
	})
	registerContains(t, msg, "has not been started", "call App.Start")
}
