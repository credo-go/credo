package testutil_test

import (
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/credo-go/credo/testutil"
)

// recordTB records the failures a check reports instead of failing the test.
type recordTB struct {
	testing.TB
	errors []string
	fatal  string
}

func (r *recordTB) Helper() {}

func (r *recordTB) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recordTB) Fatalf(format string, args ...any) {
	r.fatal = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func runCheck(t *testing.T, catalog string, roots ...string) *recordTB {
	t.Helper()
	rec := &recordTB{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		testutil.CheckMessageKeys(rec, catalog, roots...)
	}()
	<-done
	return rec
}

func TestCheckMessageKeys_DefinedKeysPass(t *testing.T) {
	rec := runCheck(t, "testdata/messagekeys/locales", "testdata/messagekeys/clean")
	if len(rec.errors) != 0 || rec.fatal != "" {
		t.Fatalf("errors = %q, fatal = %q", rec.errors, rec.fatal)
	}
}

// TestCheckMessageKeys_Reports: an undefined key fails with its file and
// line, wherever it is written — WithMessageKey, RenderMessageKey, a
// violation's MessageKey field, or a typed constant declared in another
// package — and a key whose value is not a literal is reported. A key defined
// in any language passes.
func TestCheckMessageKeys_Reports(t *testing.T) {
	rec := runCheck(t, "testdata/messagekeys/locales", "testdata/messagekeys/src")
	if rec.fatal != "" {
		t.Fatalf("fatal = %q", rec.fatal)
	}
	app := filepath.Join("testdata", "messagekeys", "src", "app", "app.go")
	keys := filepath.Join("testdata", "messagekeys", "src", "keys", "keys.go")
	want := []string{
		app + `:14:45: message key "app.undefined" is not defined in the catalogs under testdata/messagekeys/locales`,
		app + `:16:48: message key "render.undefined" is not defined in the catalogs under testdata/messagekeys/locales`,
		app + `:18:72: message key "validation.undefined" is not defined in the catalogs under testdata/messagekeys/locales`,
		keys + `:9:31: message key "keys.gone" is not defined in the catalogs under testdata/messagekeys/locales`,
		app + `:15:62: message key expression "name" is not a literal, so the check cannot prove it is in the catalogs`,
	}
	slices.Sort(want)
	got := slices.Sorted(slices.Values(rec.errors))
	if !slices.Equal(got, want) {
		t.Fatalf("errors:\n%s\nwant:\n%s", fmt.Sprint(got), fmt.Sprint(want))
	}
}

func TestCheckMessageKeys_MissingCatalog(t *testing.T) {
	rec := runCheck(t, "testdata/messagekeys/none", "testdata/messagekeys/clean")
	if !strings.HasPrefix(rec.fatal, "testutil.CheckMessageKeys: open testdata/messagekeys/none: ") {
		t.Fatalf("fatal = %q, want the missing catalog directory", rec.fatal)
	}
	rec = runCheck(t, "testdata/messagekeys/src", "testdata/messagekeys/clean")
	if rec.fatal != "testutil.CheckMessageKeys: read testdata/messagekeys/src: file does not exist" {
		t.Fatalf("fatal = %q, want a catalog directory without messages.json", rec.fatal)
	}
}
