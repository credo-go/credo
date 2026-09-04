package sqldb

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// bunPrivateAccessFiles are the only non-test files allowed to import unsafe:
// the Bun compatibility boundary that reads and patches SelectQuery's private
// layout. Any other file reaching for unsafe widens the structurally pinned
// surface and must be reviewed under the Bun upgrade protocol documented in
// bun_select_clone.go.
var bunPrivateAccessFiles = []string{"bun_select_clone.go"}

// TestBunBoundary_UnsafeStaysInCompatibilityFiles keeps version-dependent
// structural access confined to the designated files, and keeps the allowlist
// honest by requiring each listed file to actually import unsafe.
func TestBunBoundary_UnsafeStaysInCompatibilityFiles(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var importers []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: unquote import %s: %v", name, imp.Path.Value, err)
			}
			if path == "unsafe" {
				importers = append(importers, name)
			}
		}
	}
	slices.Sort(importers)
	for _, name := range importers {
		if !slices.Contains(bunPrivateAccessFiles, name) {
			t.Errorf("%s imports unsafe outside the Bun compatibility boundary (allowed: %v)", name, bunPrivateAccessFiles)
		}
	}
	for _, name := range bunPrivateAccessFiles {
		if !slices.Contains(importers, name) {
			t.Errorf("%s is allowlisted for unsafe but no longer imports it; trim bunPrivateAccessFiles", name)
		}
	}
}
