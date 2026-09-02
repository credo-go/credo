package credo_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

const modulePath = "github.com/credo-go/credo"

// importPolicy describes what a package inside the root module may import
// beyond the standard library. Every non-test source file of the package is
// checked; test files are exempt because they may pull in third-party
// libraries to exercise adapters (jwt, go-limiter, coder/websocket).
type importPolicy struct {
	// credo lists allowed in-module packages as paths relative to modulePath.
	// "" is the root package. "internal/*" allows every internal package.
	credo []string
	// external lists allowed non-stdlib, non-module import path prefixes.
	// A nil list means the package must stay stdlib-only.
	external []string
}

// modulePolicies is the dependency matrix documented in CLAUDE.md
// ("Dependencies" and "Internal Packages"). Each row is an explicit
// allowance; anything not listed is a violation. The table is also checked
// in the other direction: a package directory without a row, or a row without
// a directory, fails the test so the matrix cannot silently drift.
var modulePolicies = map[string]importPolicy{
	// Root package: adapted core, zero external runtime dependencies. The
	// three feature imports are documented exceptions (RawConfig alias,
	// transport-neutral fault contract, parse-don't-validate error types).
	"": {credo: []string{"config", "fault", "validation", "internal/*"}},

	"auth":       {credo: []string{""}, external: []string{"github.com/golang-jwt/jwt/v5"}},
	"config":     {external: []string{"github.com/go-viper/mapstructure/v2", "gopkg.in/yaml.v3"}},
	"fault":      {},
	"httpclient": {}, // stdlib-only AND must not import credo (documented in its spec)
	"middleware": {
		credo: []string{
			"", "validation",
			"internal/host", "internal/httpheader", "internal/httpwriter",
			"internal/observe", "internal/requestid",
			// Rewrite reuses the route-pattern grammar. Temporary: the
			// pattern primitive moves to a shared internal package (arch
			// refactor A4) and this allowance goes with it.
			"internal/radix",
		},
		external: []string{"github.com/sethvargo/go-limiter"},
	},
	"pagination": {},
	"store":      {credo: []string{"", "fault", "internal/health", "internal/resourceid"}},
	"testutil":   {credo: []string{"", "config"}},
	"validation": {},
	"websocket":  {credo: []string{"", "internal/httpwriter"}, external: []string{"github.com/coder/websocket"}},
	"worker":     {credo: []string{""}},

	"internal/di":          {},
	"internal/faultstatus": {credo: []string{"fault"}},
	"internal/health":      {},
	"internal/host":        {},
	"internal/httpheader":  {},
	"internal/httpwriter":  {},
	"internal/i18n":        {external: []string{"golang.org/x/text"}},
	"internal/observe":     {credo: []string{"fault", "internal/faultstatus"}},
	"internal/proxy":       {},
	"internal/radix":       {},
	"internal/requestid":   {},
	"internal/resourceid":  {},

	"scripts/releasegate": {}, // release tooling, stdlib-only
}

// TestImportBoundary_ModulePolicy is an architectural fitness test: it parses
// the import lists of every non-test Go file in the root module and checks
// them against modulePolicies. Nested modules (examples/*, store/sqldb) have
// their own go.mod and are skipped; the scratch directory is ignored.
func TestImportBoundary_ModulePolicy(t *testing.T) {
	packages := collectModulePackages(t)

	var violations []string
	seen := make(map[string]bool, len(packages))

	for _, pkg := range packages {
		seen[pkg.rel] = true
		policy, ok := modulePolicies[pkg.rel]
		if !ok {
			violations = append(violations, pkg.rel+": no import policy row in modulePolicies (add one)")
			continue
		}
		for _, imp := range pkg.imports {
			if err := checkImport(pkg.rel, imp, policy); err != "" {
				violations = append(violations, err)
			}
		}
	}

	for rel := range modulePolicies {
		if !seen[rel] {
			violations = append(violations, rel+": policy row exists but no such package directory (remove the stale row)")
		}
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Errorf("module import policy violations:\n  %s", strings.Join(violations, "\n  "))
	}
}

// checkImport returns a violation message, or "" when imp is allowed for pkg.
func checkImport(pkg, imp string, policy importPolicy) string {
	display := pkg
	if display == "" {
		display = "(root)"
	}

	if imp == modulePath || strings.HasPrefix(imp, modulePath+"/") {
		sub := strings.TrimPrefix(strings.TrimPrefix(imp, modulePath), "/")
		if sub == pkg {
			return "" // a package never imports itself; tolerate for symmetry
		}
		for _, allowed := range policy.credo {
			if allowed == sub {
				return ""
			}
			if strings.HasSuffix(allowed, "/*") && strings.HasPrefix(sub, strings.TrimSuffix(allowed, "*")) {
				return ""
			}
		}
		return display + ": imports " + imp + " (not in policy.credo)"
	}

	if isStdlib(imp) {
		return ""
	}
	for _, allowed := range policy.external {
		if imp == allowed || strings.HasPrefix(imp, allowed+"/") {
			return ""
		}
	}
	return display + ": imports " + imp + " (not in policy.external)"
}

// isStdlib reports whether an import path belongs to the standard library:
// the first path element carries no dot (no host name).
func isStdlib(imp string) bool {
	first, _, _ := strings.Cut(imp, "/")
	return !strings.Contains(first, ".")
}

type modulePackage struct {
	rel     string   // directory relative to the module root, "" for root
	imports []string // sorted, unique import paths of non-test files
}

// collectModulePackages walks the root module and returns one entry per
// directory that holds at least one non-test Go file.
func collectModulePackages(t *testing.T) []modulePackage {
	t.Helper()

	fset := token.NewFileSet()
	byDir := make(map[string]map[string]bool)

	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == "." {
				return nil
			}
			name := d.Name()
			if strings.HasPrefix(name, ".") || name == "tmp" || name == "testdata" || name == "docs" {
				return filepath.SkipDir
			}
			if _, statErr := os.Stat(filepath.Join(p, "go.mod")); statErr == nil {
				return filepath.SkipDir // nested module
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, parseErr := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if parseErr != nil {
			t.Errorf("parse %s: %v", p, parseErr)
			return nil
		}
		dir := filepath.ToSlash(filepath.Dir(p))
		if dir == "." {
			dir = ""
		}
		set := byDir[dir]
		if set == nil {
			set = make(map[string]bool)
			byDir[dir] = set
		}
		for _, imp := range f.Imports {
			set[strings.Trim(imp.Path.Value, `"`)] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	out := make([]modulePackage, 0, len(byDir))
	for dir, set := range byDir {
		imports := make([]string, 0, len(set))
		for imp := range set {
			imports = append(imports, imp)
		}
		slices.Sort(imports)
		out = append(out, modulePackage{rel: path.Clean(dir), imports: imports})
	}
	slices.SortFunc(out, func(a, b modulePackage) int { return strings.Compare(a.rel, b.rel) })
	for i := range out {
		if out[i].rel == "." {
			out[i].rel = ""
		}
	}
	return out
}
