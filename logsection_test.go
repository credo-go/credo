package credo_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/config"
)

// logSectionApp builds an App on a YAML file with the given content and no
// WithLogger, with the section logger's output captured. The loader reads the
// file alone, so the test's own environment cannot reach the section.
func logSectionApp(t *testing.T, yaml string, opts ...credo.Option) (app *credo.App, out *syncBuffer, path string, err error) {
	t.Helper()
	out = &syncBuffer{}
	t.Cleanup(credo.SetLogSectionOutput(out))
	path = filepath.Join(t.TempDir(), "config.yaml")
	writeYAML(t, path, yaml)
	cfg, loadErr := config.Load(config.WithFiles(path), config.WithoutDotenv(), config.WithoutProcessEnv())
	if loadErr != nil {
		t.Fatalf("config.Load: %v", loadErr)
	}
	app, err = credo.New(append([]credo.Option{credo.WithRawConfig(cfg)}, opts...)...)
	return app, out, path, err
}

// logAllLevels writes one record at each level and returns the levels that
// reached the output, in order.
func logAllLevels(logger *slog.Logger, out *syncBuffer) []string {
	before := len(out.String())
	logger.Debug("probe")
	logger.Info("probe")
	logger.Warn("probe")
	logger.Error("probe")
	var got []string
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()[before:]), "\n") {
		for _, level := range []string{"DEBUG", "INFO", "WARN", "ERROR"} {
			if strings.Contains(line, `level=`+level) || strings.Contains(line, `"level":"`+level+`"`) {
				got = append(got, level)
			}
		}
	}
	return got
}

// isJSONLines reports whether every line of s is a JSON object.
func isJSONLines(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	for line := range strings.SplitSeq(s, "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			return false
		}
	}
	return true
}

// TestLogSection_LevelAndFormat: New builds the default logger from log.level
// and log.format, each optional and matched without regard to case.
func TestLogSection_LevelAndFormat(t *testing.T) {
	levels := []struct {
		value string
		want  []string
	}{
		{"debug", []string{"DEBUG", "INFO", "WARN", "ERROR"}},
		{"info", []string{"INFO", "WARN", "ERROR"}},
		{"warn", []string{"WARN", "ERROR"}},
		{"error", []string{"ERROR"}},
		{"", []string{"INFO", "WARN", "ERROR"}},
		{"WARN", []string{"WARN", "ERROR"}},
	}
	formats := []struct {
		value    string
		wantJSON bool
	}{
		{"text", false},
		{"json", true},
		{"", false},
		{"JSON", true},
	}
	for _, lv := range levels {
		for _, f := range formats {
			t.Run(fmt.Sprintf("level=%q/format=%q", lv.value, f.value), func(t *testing.T) {
				yaml := "log:\n"
				if lv.value != "" {
					yaml += "  level: " + lv.value + "\n"
				}
				if f.value != "" {
					yaml += "  format: " + f.value + "\n"
				}
				if lv.value == "" && f.value == "" {
					yaml = "log: {}\n"
				}
				app, out, _, err := logSectionApp(t, yaml)
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				if got := logAllLevels(app.Logger(), out); !slices.Equal(got, lv.want) {
					t.Errorf("levels written = %v, want %v", got, lv.want)
				}
				if got := isJSONLines(out.String()); got != f.wantJSON {
					t.Errorf("JSON output = %v, want %v:\n%s", got, f.wantJSON, out.String())
				}
			})
		}
	}
}

// TestLogSection_Absent: without the section the logger is the framework
// default, a text handler, and nothing is built from the configuration.
func TestLogSection_Absent(t *testing.T) {
	app, out, _, err := logSectionApp(t, "app:\n  name: demo\n")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := app.Logger().Handler().(*slog.TextHandler); !ok {
		t.Fatalf("logger handler = %T, want the framework default text handler", app.Logger().Handler())
	}
	if out.String() != "" {
		t.Fatalf("the section logger wrote without a section: %s", out.String())
	}
}

// TestLogSection_InvalidFailsNew: a level or a format the section does not
// define, and a key it does not have, fail New. Each error names the key and
// the remedy; the unknown key's also says whose section "log" is, since the
// likeliest cause is an application section of that name.
func TestLogSection_InvalidFailsNew(t *testing.T) {
	const sectionRemedy = `"log" is the framework's section, with the keys log.level and log.format: rename an application section of that name, or pass the logger with credo.WithLogger, which leaves the section unread`
	tests := []struct {
		name string
		yaml string
		want []string
	}{
		{
			name: "unknown level",
			yaml: "log:\n  level: verbose\n",
			want: []string{`log.level "verbose" is not a log level`, "use debug, info, warn or error", sectionRemedy},
		},
		{
			name: "unknown format",
			yaml: "log:\n  format: xml\n",
			want: []string{`log.format "xml" is not a log format`, "use text or json", sectionRemedy},
		},
		{
			name: "an application section of the same name",
			yaml: "log:\n  level: info\n  file: /var/log/app.log\n",
			want: []string{`unknown key "log.file"`, sectionRemedy},
		},
		{
			name: "a scalar where the section is expected",
			yaml: "log: debug\n",
			want: []string{`"log"`, sectionRemedy},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, err := logSectionApp(t, tt.yaml)
			if err == nil {
				t.Fatal("New accepted the section")
			}
			if !strings.HasPrefix(err.Error(), "credo: invalid log config: ") {
				t.Errorf("error = %q, want it to open with what failed", err)
			}
			for _, part := range tt.want {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error does not say %q:\n%s", part, err)
				}
			}
		})
	}
}

// TestLogSection_WithLoggerWins: WithLogger is the whole-source override.
// The section is not read, so even one that would fail New is left alone,
// and the application may read a section of that name as its own.
func TestLogSection_WithLoggerWins(t *testing.T) {
	var own bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&own, nil))
	app, out, _, err := logSectionApp(t, "log:\n  level: verbose\n  file: /var/log/app.log\n", credo.WithLogger(logger))
	if err != nil {
		t.Fatalf("New with WithLogger read the log section: %v", err)
	}
	if app.Logger() != logger {
		t.Fatal("App.Logger is not the logger given with WithLogger")
	}
	app.Logger().Info("probe")
	if out.String() != "" || !strings.Contains(own.String(), "probe") {
		t.Fatalf("section output %q, own output %q: want the record in the application's logger only", out.String(), own.String())
	}
	type ownLog struct {
		Level string
		File  string
	}
	got, err := app.GetConfig(config.Section[ownLog]("log"))
	if err != nil || got.File != "/var/log/app.log" {
		t.Fatalf("GetConfig of the application's own log section = %+v, %v", got, err)
	}
}

// TestLogSection_Reload: a reload applies a new level and a new format, to
// the App's logger and to the loggers derived from it before the reload, and
// a reload with an invalid value is aborted and changes nothing.
func TestLogSection_Reload(t *testing.T) {
	app, out, path, err := logSectionApp(t, "log:\n  level: warn\n  format: text\n")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startServing(t, app)
	// Derived before the reload, as a component's logger is: attributes, and
	// a group opened after them.
	derived := app.Logger().With("component", "orders").WithGroup("job").With("id", 7)

	if got := logAllLevels(app.Logger(), out); !slices.Equal(got, []string{"WARN", "ERROR"}) {
		t.Fatalf("levels before the reload = %v", got)
	}
	if isJSONLines(out.String()) {
		t.Fatalf("format before the reload is JSON:\n%s", out.String())
	}

	writeYAML(t, path, "log:\n  level: debug\n  format: json\n")
	result, err := app.Reload(t.Context())
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !result.Published || !slices.Contains(result.Replaced, "log") || len(result.RestartRequired) != 0 {
		t.Fatalf("reload result = %+v, want log replaced and nothing needing a restart", result)
	}
	mark := len(out.String())
	if got := logAllLevels(app.Logger(), out); !slices.Equal(got, []string{"DEBUG", "INFO", "WARN", "ERROR"}) {
		t.Fatalf("levels after the reload = %v", got)
	}
	derived.Debug("derived probe", "n", 1)
	after := out.String()[mark:]
	if !isJSONLines(after) {
		t.Fatalf("format after the reload is not JSON:\n%s", after)
	}
	var rec map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(after), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["msg"] == "derived probe" {
			rec = m
		}
	}
	want := map[string]any{"id": float64(7), "n": float64(1)}
	if rec == nil || rec["component"] != "orders" || !reflect.DeepEqual(rec["job"], want) {
		t.Fatalf("derived logger's record after the reload = %v, want component=orders and the job group %v", rec, want)
	}

	// An invalid level aborts the reload; the logger keeps level and format.
	writeYAML(t, path, "log:\n  level: verbose\n  format: text\n")
	result, err = app.Reload(t.Context())
	if err == nil || result.Published {
		t.Fatalf("Reload accepted log.level verbose: %+v, %v", result, err)
	}
	for _, part := range []string{`live section "log"`, `log.level "verbose" is not a log level`, "use debug, info, warn or error"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("reload error does not say %q:\n%s", part, err)
		}
	}
	mark = len(out.String())
	if got := logAllLevels(app.Logger(), out); !slices.Equal(got, []string{"DEBUG", "INFO", "WARN", "ERROR"}) {
		t.Fatalf("levels after the aborted reload = %v", got)
	}
	if !isJSONLines(out.String()[mark:]) {
		t.Fatalf("format after the aborted reload is not JSON:\n%s", out.String()[mark:])
	}

	// A reload that changes the level alone keeps the handler.
	writeYAML(t, path, "log:\n  level: error\n  format: json\n")
	if _, err = app.Reload(t.Context()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := logAllLevels(derived, out); !slices.Equal(got, []string{"ERROR"}) {
		t.Fatalf("derived logger's levels after the second reload = %v", got)
	}
}

// TestLogSection_IsLive: the framework reads "log" as a live section, and a
// section has one nature, so an application's static read of it fails naming
// both reads.
func TestLogSection_IsLive(t *testing.T) {
	app, _, _, err := logSectionApp(t, "log:\n  level: info\n")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	type view struct{ Level string }
	_, err = app.GetConfig(config.Section[view]("log"))
	if err == nil {
		t.Fatal("a static read of the live log section was accepted")
	}
	for _, part := range []string{`section "log" is read both as static`, "and as live", `config.Section[credo.logConfig]("log").Live`} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("error does not say %q:\n%s", part, err)
		}
	}
}

// logOnlyRC is a RawConfig that is not a *config.Config: it cannot reload, so
// New decodes the "log" section from it once.
type logOnlyRC struct{ level, format string }

func (r logOnlyRC) Exists(key string) bool { return key == "log" }

func (r logOnlyRC) Unmarshal(key string, dst any) error {
	if key != "log" {
		return fmt.Errorf("key %q not found", key)
	}
	v := reflect.ValueOf(dst).Elem()
	v.FieldByName("Level").SetString(r.level)
	v.FieldByName("Format").SetString(r.format)
	return nil
}

// TestLogSection_RawConfigThatCannotReload: the section follows the server
// section's path. A RawConfig of another type gets it decoded once, and its
// values are checked by New, since such a RawConfig need not run Validate.
func TestLogSection_RawConfigThatCannotReload(t *testing.T) {
	out := &syncBuffer{}
	t.Cleanup(credo.SetLogSectionOutput(out))

	app, err := credo.New(credo.WithRawConfig(logOnlyRC{level: "error", format: "json"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := logAllLevels(app.Logger(), out); !slices.Equal(got, []string{"ERROR"}) {
		t.Errorf("levels written = %v, want ERROR alone", got)
	}
	if !isJSONLines(out.String()) {
		t.Errorf("output is not JSON:\n%s", out.String())
	}

	_, err = credo.New(credo.WithRawConfig(logOnlyRC{level: "loud"}))
	if err == nil {
		t.Fatal("New accepted log.level loud")
	}
	for _, part := range []string{"credo: invalid log config: ", `log.level "loud" is not a log level`, "use debug, info, warn or error"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("error does not say %q:\n%s", part, err)
		}
	}
}
