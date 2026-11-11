package testutil_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
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
// violation's MessageKey field, a typed constant declared in another package,
// the first argument of T or TPlural, or a concatenation of constants. A key
// built at run time is a family: one whose prefix the catalogs have a key
// under passes, and one without fails and names the prefix. A value with no
// literal prefix that ends in "." is reported with the remedy. A key defined
// in any language passes.
func TestCheckMessageKeys_Reports(t *testing.T) {
	rec := runCheck(t, "testdata/messagekeys/locales", "testdata/messagekeys/src")
	if rec.fatal != "" {
		t.Fatalf("fatal = %q", rec.fatal)
	}
	app := filepath.Join("testdata", "messagekeys", "src", "app", "app.go")
	keys := filepath.Join("testdata", "messagekeys", "src", "keys", "keys.go")
	const dir = "testdata/messagekeys/locales"
	undefined := func(pos, key string) string {
		return pos + `: message key "` + key + `" is not defined in the catalogs under ` + dir
	}
	family := func(pos, prefix, expr string) string {
		return pos + `: message key family "` + prefix + `", the literal prefix of ` + strconv.Quote(expr) +
			`, has no key in the catalogs under ` + dir +
			`; add a key that begins with "` + prefix + `" to a catalog, or correct the prefix`
	}
	unreadable := func(pos, expr string) string {
		return pos + `: message key expression ` + strconv.Quote(expr) +
			` is not a literal and does not begin with a literal or a string constant that ends in ".", so the check cannot prove it is in the catalogs` +
			`; write the key as a literal or a constant of type MessageKey, or build it from a literal prefix that ends in "." and the computed part`
	}
	want := []string{
		undefined(app+":14:45", "app.undefined"),
		unreadable(app+":15:62", `name`),
		undefined(app+":16:48", "render.undefined"),
		undefined(app+":18:72", "validation.undefined"),
		undefined(keys+":9:31", "keys.gone"),
		// T and TPlural: line 21 is a defined literal and passes.
		undefined(app+":22:10", "t.undefined"),
		undefined(app+":23:16", "plural.undefined"),
		// Families: lines 24 and 25 ("status." as a literal and as a constant
		// of another package) have catalog keys under them and pass.
		family(app+":26:27", "status.sub.", `keys.StatusPrefix + "sub." + name`),
		family(app+":27:27", "orphan.", `"orphan." + name`),
		family(app+":28:33", "orphan.", `keys.OrphanPrefix + name`),
		// No literal prefix, and a prefix that does not end in ".".
		unreadable(app+":29:27", `name + ".title"`),
		unreadable(app+":30:27", `"status" + name`),
		// A concatenation of constants is one key: line 31 is defined.
		undefined(app+":32:27", "status.gone"),
	}
	slices.Sort(want)
	got := slices.Sorted(slices.Values(rec.errors))
	if !slices.Equal(got, want) {
		t.Fatalf("errors:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// writeSource writes a file of a source tree a test builds under root.
func writeSource(t *testing.T, root, name, src string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCheckMessageKeys_ConstantsAreReadPerPackage: a family's prefix is read
// from the constant the name stands for where it is written — one declared
// in the function, else the package's, else the imported package's — and not
// from a constant of the same name elsewhere. A name the function declares as
// a variable or a parameter is not a constant, whatever the package declares
// under that name; nor is a name the sources do not declare, or declare with
// two values in one package.
func TestCheckMessageKeys_ConstantsAreReadPerPackage(t *testing.T) {
	write := writeSource
	const handler = `package app

import (
	"github.com/credo-go/credo"

	IMPORT
)

const prefix = "known."

const other = "missing."

func lookup() string { return "computed." }

func unrelated() { other := lookup(); _ = other }

func handler(c *credo.Context, code string) string {
	BODY
	return c.T(credo.MessageKey(EXPR))
}

func byParameter(c *credo.Context, prefix, code string) string {
	return c.T(credo.MessageKey(PARAM))
}
`
	tests := []struct {
		name     string
		imports  string
		body     string
		expr     string
		param    string // the key byParameter builds; a literal when the case is not about it
		wantPass bool
		wantText string
	}{
		{name: "the package's constant", imports: `_ "example.com/app/a"`, expr: `prefix + code`, wantPass: true},
		{name: "a local variable named as the package's constant", imports: `_ "example.com/app/a"`, body: `prefix := lookup()`, expr: `prefix + code`, wantText: `message key expression "prefix + code" is not a literal`},
		{name: "a variable declared with var", imports: `_ "example.com/app/a"`, body: `var prefix = lookup()`, expr: `prefix + code`, wantText: `message key expression "prefix + code" is not a literal`},
		{name: "a variable declared in an inner block", imports: `_ "example.com/app/a"`, body: `for _, prefix := range []string{lookup()} { _ = prefix }`, expr: `prefix + code`, wantText: `message key expression "prefix + code" is not a literal`},
		{name: "a parameter named as the package's constant", imports: `_ "example.com/app/a"`, expr: `"known." + code`, param: `prefix + code`, wantText: `message key expression "prefix + code" is not a literal`},
		{name: "a variable of that name in another function", imports: `_ "example.com/app/a"`, expr: `other + code`, wantText: `message key family "missing."`},
		{name: "a local constant hides the package's", imports: `_ "example.com/app/a"`, body: `const prefix = "missing."`, expr: `prefix + code`, wantText: `message key family "missing."`},
		{name: "a local constant built from the package's", imports: `_ "example.com/app/a"`, body: `const nested = prefix + "nested."`, expr: `nested + code`, wantText: `message key family "known.nested."`},
		{name: "a local variable named as the import", imports: `"example.com/app/a"`, body: `a := struct{ Prefix string }{lookup()}`, expr: `a.Prefix + code`, wantText: `message key expression "a.Prefix + code" is not a literal`},
		{name: "the imported package's constant", imports: `"example.com/app/a"`, expr: `a.Prefix + code`, wantPass: true},
		{name: "an aliased import", imports: `first "example.com/app/a"`, expr: `first.Prefix + code`, wantPass: true},
		{name: "the same name in another package", imports: `"example.com/app/b"`, expr: `b.Prefix + code`, wantText: `message key family "missing."`},
		{name: "a constant declared in the function", imports: `_ "example.com/app/a"`, body: `const local = "known."`, expr: `local + code`, wantPass: true},
		{name: "a constant built from another", imports: `"example.com/app/a"`, expr: `a.Nested + code`, wantText: `message key family "known.nested."`},
		{name: "a package outside the sources", imports: `"example.com/elsewhere/z"`, expr: `z.Prefix + code`, wantText: `message key expression "z.Prefix + code" is not a literal`},
		{name: "a variable", imports: `"example.com/app/a"`, expr: `a.Variable + code`, wantText: `message key expression "a.Variable + code" is not a literal`},
		{name: "a name with two values in one package", imports: `"example.com/app/b"`, expr: `b.Twice + code`, wantText: `message key expression "b.Twice + code" is not a literal`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, "locales/en/messages.json", `{"known.one": "One."}`)
			write(t, root, "src/a/a.go", "package a\n\nconst Prefix = \"known.\"\n\nconst Nested = Prefix + \"nested.\"\n\nvar Variable = \"known.\"\n")
			write(t, root, "src/b/b.go", "package b\n\nconst Prefix = \"missing.\"\n\nconst Twice = \"known.\"\n")
			write(t, root, "src/b/b_other.go", "//go:build other\n\npackage b\n\nconst Twice = \"missing.\"\n")
			param := tt.param
			if param == "" {
				param = `"known.one"`
			}
			src := strings.NewReplacer("IMPORT", tt.imports, "BODY", tt.body, "EXPR", tt.expr, "PARAM", param).Replace(handler)
			write(t, root, "src/app/app.go", src)

			rec := runCheck(t, filepath.Join(root, "locales"), filepath.Join(root, "src"))
			if rec.fatal != "" {
				t.Fatalf("fatal = %q", rec.fatal)
			}
			if tt.wantPass {
				if len(rec.errors) != 0 {
					t.Fatalf("errors = %q, want none", rec.errors)
				}
				return
			}
			if len(rec.errors) != 1 || !strings.Contains(rec.errors[0], tt.wantText) {
				t.Fatalf("errors = %q, want one that contains %q", rec.errors, tt.wantText)
			}
		})
	}
}

// TestCheckMessageKeys_ImportedConstantIsTheSameEveryRun: two directories
// declare a constant of one name, and the one whose path matches the import
// path best is read every time, whether or not the other can be read. The
// constants are kept in a map, so a lookup that depended on the order it
// met them in would differ from run to run; 200 runs caught it reliably.
func TestCheckMessageKeys_ImportedConstantIsTheSameEveryRun(t *testing.T) {
	root := t.TempDir()
	writeSource(t, root, "locales/en/messages.json", `{"status.ok": "OK"}`)
	writeSource(t, root, "src/a/keys/keys.go", "package keys\n\nconst Prefix = \"status.\"\n")
	writeSource(t, root, "src/b/keys/keys.go", "package keys\n\nconst Prefix = 5\n")
	writeSource(t, root, "src/app/app.go", `package app

import (
	"github.com/credo-go/credo"

	"example.com/m/a/keys"
)

func handler(c *credo.Context, code string) string {
	return c.T(credo.MessageKey(keys.Prefix + code))
}
`)
	for run := range 200 {
		rec := runCheck(t, filepath.Join(root, "locales"), filepath.Join(root, "src"))
		if rec.fatal != "" || len(rec.errors) != 0 {
			t.Fatalf("run %d: errors = %q, fatal = %q; want the family status. to pass every run", run, rec.errors, rec.fatal)
		}
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
