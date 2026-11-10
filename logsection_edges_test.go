package credo_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

// TestLogSection_AutoLoad: the section reaches the automatic load like any
// other key, an environment override included, and the logger it selects is
// the one the record of the automatic load is written to.
func TestLogSection_AutoLoad(t *testing.T) {
	out := &syncBuffer{}
	t.Cleanup(credo.SetLogSectionOutput(out))
	t.Chdir(t.TempDir())
	t.Setenv("CREDO_ENV", "")
	t.Setenv("CREDO_ENV_FILE", "")
	t.Setenv("CREDO_LOG__LEVEL", "DEBUG")
	t.Setenv("CREDO_LOG__FORMAT", "json")

	app, err := credo.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !isJSONLines(out.String()) || !strings.Contains(out.String(), `"msg":"credo: configuration loaded automatically"`) ||
		!strings.Contains(out.String(), `"level":"DEBUG"`) {
		t.Fatalf("the record of the automatic load is not in the section's logger, as JSON at Debug:\n%s", out.String())
	}
	if got := logAllLevels(app.Logger(), out); !slices.Equal(got, []string{"DEBUG", "INFO", "WARN", "ERROR"}) {
		t.Errorf("levels written = %v, want all four", got)
	}
}

// TestLogSection_ReloadEdges pins what a reload does when the section itself
// comes or goes, which follows from its being read like the server section
// (when present) and held like any live section.
func TestLogSection_ReloadEdges(t *testing.T) {
	t.Run("emptied: the defaults apply", func(t *testing.T) {
		app, out, path, err := logSectionApp(t, "log:\n  level: error\n  format: json\n")
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		startServing(t, app)
		writeYAML(t, path, "log:\n")
		result, err := app.Reload(t.Context())
		if err != nil || !slices.Contains(result.Replaced, "log") {
			t.Fatalf("Reload = %+v, %v, want log replaced", result, err)
		}
		mark := len(out.String())
		if got := logAllLevels(app.Logger(), out); !slices.Equal(got, []string{"INFO", "WARN", "ERROR"}) {
			t.Errorf("levels after the reload = %v, want the default, info", got)
		}
		if isJSONLines(out.String()[mark:]) {
			t.Errorf("format after the reload is JSON, want the default, text:\n%s", out.String()[mark:])
		}
	})

	t.Run("removed: the reload is aborted", func(t *testing.T) {
		app, out, path, err := logSectionApp(t, "log:\n  level: error\n")
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		startServing(t, app)
		writeYAML(t, path, "app:\n  name: demo\n")
		result, err := app.Reload(t.Context())
		if err == nil || result.Published {
			t.Fatalf("Reload = %+v, %v, want it aborted", result, err)
		}
		if !strings.Contains(err.Error(), `live section "log"`) || !strings.Contains(err.Error(), "not found") {
			t.Errorf("reload error does not name the missing live section:\n%s", err)
		}
		if got := logAllLevels(app.Logger(), out); !slices.Equal(got, []string{"ERROR"}) {
			t.Errorf("levels after the aborted reload = %v, want error kept", got)
		}
	})

	t.Run("added: it takes a restart", func(t *testing.T) {
		app, out, path, err := logSectionApp(t, "app:\n  name: demo\n")
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		startServing(t, app)
		writeYAML(t, path, "app:\n  name: demo\nlog:\n  level: debug\n")
		result, err := app.Reload(t.Context())
		if err != nil || !result.Published {
			t.Fatalf("Reload = %+v, %v", result, err)
		}
		if !slices.Contains(result.RestartRequired, "log.level") || slices.Contains(result.Replaced, "log") {
			t.Errorf("reload result = %+v, want log.level reported as needing a restart", result)
		}
		if out.String() != "" {
			t.Errorf("a logger was built from a section added by a reload:\n%s", out.String())
		}
	})
}
