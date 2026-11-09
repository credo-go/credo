package credo_test

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestImportBoundary_MiddlewareGraphLinksNoGoLimiter keeps rate limiting in
// its own package: an application that imports middleware, for CORS say, must
// not link go-limiter, which only middleware/ratelimit may bring in.
func TestImportBoundary_MiddlewareGraphLinksNoGoLimiter(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the go tool is needed to list the middleware package's dependencies: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), goTool, "list", "-deps",
		"-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", modulePath+"/middleware")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, stderr.String())
	}
	if !strings.Contains(string(out), modulePath+"/middleware") {
		t.Fatalf("go list -deps did not report the middleware package itself; output:\n%s", out)
	}

	for line := range strings.Lines(string(out)) {
		importPath := strings.TrimRight(line, "\r\n")
		if importPath == "github.com/sethvargo/go-limiter" ||
			strings.HasPrefix(importPath, "github.com/sethvargo/go-limiter/") ||
			importPath == modulePath+"/middleware/ratelimit" {
			t.Errorf("the middleware package links %s; rate limiting and go-limiter belong to middleware/ratelimit only", importPath)
		}
	}
}
