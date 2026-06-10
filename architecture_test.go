package credo_test

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// allowedRootImports lists feature packages that the root package is
// permitted to import. Each entry must have a documented justification.
var allowedRootImports = map[string]bool{
	"config":     true, // RawConfig type alias (avoid circular import: config/ ↔ root)
	"validation": true, // error handling + parse-don't-validate (documented exception)
}

// TestImportBoundary_RootPackage verifies that the root package does not
// import feature packages (middleware, container, config, etc.) except
// for explicitly allowed exceptions and internal/ packages.
//
// This is an architectural fitness test — it catches accidental coupling
// between the root package and feature packages at test time.
func TestImportBoundary_RootPackage(t *testing.T) {
	const modulePrefix = "github.com/credo-go/credo/"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	var violations []string

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Errorf("parse %s: %v", name, err)
			continue
		}

		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if !strings.HasPrefix(path, modulePrefix) {
				continue
			}

			// Extract the sub-package name (first segment after module prefix).
			sub := strings.TrimPrefix(path, modulePrefix)

			// internal/ packages are always allowed.
			if strings.HasPrefix(sub, "internal/") {
				continue
			}

			// Check the top-level package name (e.g. "container" from "container/scope").
			pkg := sub
			if idx := strings.IndexByte(sub, '/'); idx > 0 {
				pkg = sub[:idx]
			}

			if allowedRootImports[pkg] {
				continue
			}

			violations = append(violations, name+": imports "+path)
		}
	}

	if len(violations) > 0 {
		t.Errorf("root package import boundary violations:\n  %s\n\nAllowed feature imports: %v",
			strings.Join(violations, "\n  "),
			allowedRootImports)
	}
}
