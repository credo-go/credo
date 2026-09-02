package credo_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// maturityByPackage is the single source of truth for per-package maturity
// labels. Keys are package directories relative to the module root ("" is
// the root package); values are the labels that README's "Maturity by Area"
// table and each package's doc.go must repeat verbatim. Internal packages,
// examples, and tooling under scripts/ carry no label.
var maturityByPackage = map[string]string{
	"":            "beta",
	"auth":        "beta",
	"config":      "beta",
	"fault":       "beta",
	"httpclient":  "beta",
	"middleware":  "beta",
	"pagination":  "beta",
	"store":       "beta",
	"store/sqldb": "beta",
	"testutil":    "beta",
	"validation":  "beta",
	"websocket":   "beta",
	"worker":      "beta",
}

// readmeAreas maps README table rows whose area text does not name the
// package in backticks to the package directories they describe.
var readmeAreas = map[string][]string{
	"Routing, Context, Handlers, Middleware": {"", "middleware"},
	"Validation":                             {"validation"},
	"Authentication":                         {"auth"},
	"Pagination":                             {"pagination"},
}

var (
	maturityLabels   = map[string]bool{"experimental": true, "beta": true, "stable": true}
	maturityLineRE   = regexp.MustCompile(`^// Maturity: (\S+)$`)
	readmeBacktickRE = regexp.MustCompile("`([^`]+)`")
)

// TestMaturityLabels_DocGo checks that every importable package ends its
// package documentation with exactly one "// Maturity: <label>" line and that
// the label matches maturityByPackage.
func TestMaturityLabels_DocGo(t *testing.T) {
	seen := make(map[string]bool)
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == "." {
				return nil
			}
			switch name := d.Name(); {
			case strings.HasPrefix(name, "."), name == "tmp", name == "testdata", name == "docs",
				name == "examples", name == "internal", name == "scripts":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		dir := filepath.ToSlash(filepath.Dir(p))
		if dir == "." {
			dir = ""
		}
		seen[dir] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	dirs := make([]string, 0, len(seen))
	for dir := range seen {
		dirs = append(dirs, dir)
	}
	slices.Sort(dirs)
	for _, dir := range dirs {
		want, ok := maturityByPackage[dir]
		if !ok {
			t.Errorf("%s: no maturity row in maturityByPackage (add one)", displayDir(dir))
			continue
		}
		got, err := readMaturityLabel(filepath.Join(dir, "doc.go"))
		if err != nil {
			t.Errorf("%s: %v", displayDir(dir), err)
			continue
		}
		if got != want {
			t.Errorf("%s/doc.go: Maturity: %s, maturityByPackage says %s", displayDir(dir), got, want)
		}
	}
	for dir := range maturityByPackage {
		if !seen[dir] {
			t.Errorf("maturityByPackage lists %q but no package exists there", dir)
		}
	}
}

// TestMaturityLabels_README checks that README's "Maturity by Area" table
// agrees with maturityByPackage for every row that names a labelled package.
func TestMaturityLabels_README(t *testing.T) {
	data, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	start := strings.Index(text, "## Maturity by Area")
	if start < 0 {
		t.Fatal(`README.md: "## Maturity by Area" section not found`)
	}
	section := text[start+len("## Maturity by Area"):]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}

	checked := make(map[string]bool)
	for line := range strings.SplitSeq(section, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") || strings.HasPrefix(line, "| ---") || strings.HasPrefix(line, "| Area") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 4 {
			t.Errorf("README.md: malformed maturity row %q", line)
			continue
		}
		area := strings.TrimSpace(cells[1])
		status := strings.ToLower(strings.TrimSpace(cells[2]))

		var dirs []string
		for _, m := range readmeBacktickRE.FindAllStringSubmatch(area, -1) {
			if _, ok := maturityByPackage[m[1]]; ok {
				dirs = append(dirs, m[1])
			}
		}
		for prefix, mapped := range readmeAreas {
			if strings.HasPrefix(area, prefix) {
				dirs = append(dirs, mapped...)
			}
		}
		for _, dir := range dirs {
			checked[dir] = true
			if want := maturityByPackage[dir]; want != status {
				t.Errorf("README.md row %q says %s, but %s is labelled %s", area, status, displayDir(dir), want)
			}
		}
	}
	if len(checked) == 0 {
		t.Fatal("README.md: no maturity row matched a labelled package")
	}
}

// readMaturityLabel returns the label from the single "// Maturity: <label>"
// line in docPath, which must be the last line of the package doc comment
// (immediately before the package clause).
func readMaturityLabel(docPath string) (string, error) {
	data, err := os.ReadFile(docPath)
	if err != nil {
		return "", fmt.Errorf("missing package documentation file: %w", err)
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	var labels []string
	for i, line := range lines {
		m := maturityLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		labels = append(labels, m[1])
		if i+1 >= len(lines) || !strings.HasPrefix(lines[i+1], "package ") {
			return "", fmt.Errorf("%s: the Maturity line must be the last line of the package doc comment, directly before the package clause", docPath)
		}
	}
	if len(labels) != 1 {
		return "", fmt.Errorf("%s: expected exactly one \"// Maturity: <label>\" line, found %d", docPath, len(labels))
	}
	if !maturityLabels[labels[0]] {
		return "", fmt.Errorf("%s: unknown maturity label %q (want experimental, beta, or stable)", docPath, labels[0])
	}
	return labels[0], nil
}

func displayDir(dir string) string {
	if dir == "" {
		return "root package"
	}
	return dir
}
