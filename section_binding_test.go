package credo_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/config"
	"github.com/credo-go/credo/testutil"
)

// limitsConfig is a section type that validates itself.
type limitsConfig struct {
	RPS int
}

func (l limitsConfig) Validate() error {
	if l.RPS <= 0 {
		return errors.New("rps must be positive")
	}
	return nil
}

var limitsSection = config.Section[limitsConfig]("limits")

// limitsService consumes the bound section, as a business service would.
type limitsService struct{ cfg limitsConfig }

func newLimitsService(cfg limitsConfig) *limitsService { return &limitsService{cfg: cfg} }

// sectionApp builds an App over doc with the section bound through Provide
// and a service that depends on it.
func sectionApp(t *testing.T, doc string) *credo.App {
	t.Helper()
	rc, err := config.LoadBytes([]byte(doc), config.FormatJSON, config.WithoutProcessEnv(), config.WithoutDotenv())
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	app := mustNew(t, credo.WithRawConfig(rc))
	app.Provide(limitsSection.Decode)
	app.Provide(newLimitsService)
	return app
}

func TestSection_ProvideDecode(t *testing.T) {
	t.Run("resolves the decoded section", func(t *testing.T) {
		app := sectionApp(t, `{"limits":{"rps":50}}`)
		svc := app.MustResolve[*limitsService]()
		if svc.cfg.RPS != 50 {
			t.Errorf("RPS = %d, want 50", svc.cfg.RPS)
		}
	})

	for _, tt := range []struct{ name, doc, part string }{
		{name: "missing section", doc: `{}`, part: "not found"},
		{name: "invalid section", doc: `{"limits":{"rps":0}}`, part: "rps must be positive"},
		{name: "undecodable section", doc: `{"limits":{"rps":1,"burst":2}}`, part: `unknown key "limits.burst"`},
	} {
		t.Run(tt.name+" fails its constructor", func(t *testing.T) {
			app := sectionApp(t, tt.doc)
			_, err := app.Resolve[*limitsService]()
			if err == nil {
				t.Fatal("Resolve: expected the section's constructor error, got nil")
			}
			for _, part := range []string{`section "limits"`, tt.part} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("Resolve error %q does not contain %q", err, part)
				}
			}
		})
	}

	t.Run("a second binding of one section type panics", func(t *testing.T) {
		app := mustNew(t)
		app.Provide(limitsSection.Decode)
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("second Provide of one section type: expected a panic")
			}
			if msg := fmt.Sprint(r); !strings.Contains(msg, "limitsConfig") {
				t.Errorf("panic %q does not name the bound type", msg)
			}
		}()
		app.Provide(config.Section[limitsConfig]("other_limits").Decode)
	})
}

// TestSection_StartFailsOnInvalidSection pins that a section a started
// component depends on fails the start, naming the key and the error.
func TestSection_StartFailsOnInvalidSection(t *testing.T) {
	rc, err := config.LoadBytes([]byte(`{"limits":{"rps":-1}}`), config.FormatJSON,
		config.WithoutProcessEnv(), config.WithoutDotenv())
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	app := mustNew(t, credo.WithRawConfig(rc))
	app.Provide(limitsSection.Decode)
	app.Provide(func(cfg limitsConfig) *limitsComponent { return &limitsComponent{cfg: cfg} })
	err = app.Start(t.Context())
	t.Cleanup(func() { _ = app.Shutdown(t.Context()) })
	if err == nil {
		t.Fatal("Start: expected the section's error, got nil")
	}
	for _, part := range []string{`section "limits"`, "rps must be positive"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("Start error %q does not contain %q", err, part)
		}
	}
}

// TestApp_ConfigExists: the App probes an optional section through the
// descriptor GetConfig reads it with, or through a literal key with the type
// given, and the probe decodes nothing.
func TestApp_ConfigExists(t *testing.T) {
	// The limits section is present and fails its validation.
	app := sectionApp(t, `{"limits":{"rps":0}}`)

	tests := []struct {
		name string
		got  bool
		want bool
	}{
		{"the descriptor of a present section", app.ConfigExists(limitsSection), true},
		{"a literal key with the type given", app.ConfigExists[limitsConfig]("limits"), true},
		{"a present leaf", app.ConfigExists[int]("limits.rps"), true},
		{"a missing section", app.ConfigExists[limitsConfig]("cache"), false},
		{"an empty key", app.ConfigExists[limitsConfig](""), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("ConfigExists = %v, want %v", tt.got, tt.want)
			}
		})
	}

	// The probe did not decode: the read that follows reports the invalid
	// section, and the container is still open for it.
	if _, err := app.GetConfig(limitsSection); err == nil || !strings.Contains(err.Error(), "rps must be positive") {
		t.Errorf("GetConfig after ConfigExists = %v, want the validation error", err)
	}
}

// limitsComponent has start work, so the start phase constructs it.
type limitsComponent struct{ cfg limitsConfig }

func (*limitsComponent) Start(context.Context) error { return nil }

// TestSection_TestutilEmptyConfig pins that testutil's empty configuration
// honours the missing-key contract: Exists is false and a decode fails with
// the not-found error a loaded *config.Config returns.
func TestSection_TestutilEmptyConfig(t *testing.T) {
	app := testutil.NewApp(t)
	if app.ConfigExists(limitsSection) {
		t.Error("ConfigExists(limitsSection) = true on the empty configuration")
	}
	_, err := app.GetConfig(limitsSection)
	if err == nil || !strings.Contains(err.Error(), `config: key "limits" not found`) {
		t.Errorf("GetConfig on the empty configuration = %v, want the not-found error", err)
	}
}
