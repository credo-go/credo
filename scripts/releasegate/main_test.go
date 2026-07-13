package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsCanonicalVersion(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    bool
	}{
		{name: "stable", version: "v0.1.0", want: true},
		{name: "prerelease", version: "v0.1.0-beta.1", want: true},
		{name: "default candidate", version: defaultCandidateVersion, want: true},
		{name: "numeric zero prerelease", version: "v1.2.3-0", want: true},
		{name: "alphanumeric leading zero", version: "v1.2.3-01alpha", want: true},
		{name: "build metadata", version: "v1.2.3+build.1", want: false},
		{name: "prerelease with build metadata", version: "v1.2.3-beta.1+build", want: false},
		{name: "empty prerelease", version: "v1.2.3-", want: false},
		{name: "empty prerelease identifier", version: "v1.2.3-beta..1", want: false},
		{name: "leading empty prerelease identifier", version: "v1.2.3-.beta", want: false},
		{name: "trailing empty prerelease identifier", version: "v1.2.3-beta.", want: false},
		{name: "numeric prerelease leading zero", version: "v1.2.3-beta.01", want: false},
		{name: "major leading zero", version: "v01.2.3", want: false},
		{name: "minor leading zero", version: "v1.02.3", want: false},
		{name: "patch leading zero", version: "v1.2.03", want: false},
		{name: "missing prefix", version: "1.2.3", want: false},
		{name: "extra core identifier", version: "v1.2.3.4", want: false},
		{name: "invalid prerelease character", version: "v1.2.3-beta_1", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCanonicalVersion(tt.version); got != tt.want {
				t.Fatalf("isCanonicalVersion(%q) = %t, want %t", tt.version, got, tt.want)
			}
		})
	}
}

func TestCommitStagedChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}

	repo := t.TempDir()
	mustCommand(t, repo, "git", "init", "--quiet")
	mustCommand(t, repo, "git", "config", "user.name", "Credo release gate test")
	mustCommand(t, repo, "git", "config", "user.email", "release-gate-test@credo.invalid")

	tracked := filepath.Join(repo, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("initial\n"), 0o644); err != nil {
		t.Fatalf("write initial file: %v", err)
	}
	mustCommand(t, repo, "git", "add", "tracked.txt")
	mustCommand(t, repo, "git", "commit", "--quiet", "-m", "initial")

	t.Run("no staged changes keeps HEAD", func(t *testing.T) {
		before := mustOutput(t, repo, "git", "rev-parse", "HEAD")

		committed, err := commitStagedChanges(repo, "synthetic release")
		if err != nil {
			t.Fatalf("commit staged changes: %v", err)
		}
		if committed {
			t.Fatal("commitStagedChanges reported a commit for a clean index")
		}
		after := mustOutput(t, repo, "git", "rev-parse", "HEAD")
		if after != before {
			t.Fatalf("HEAD changed from %s to %s", before, after)
		}
	})

	t.Run("staged changes create one commit", func(t *testing.T) {
		before := mustOutput(t, repo, "git", "rev-parse", "HEAD")
		if err := os.WriteFile(tracked, []byte("prepared\n"), 0o644); err != nil {
			t.Fatalf("write prepared file: %v", err)
		}
		mustCommand(t, repo, "git", "add", "tracked.txt")

		committed, err := commitStagedChanges(repo, "synthetic release")
		if err != nil {
			t.Fatalf("commit staged changes: %v", err)
		}
		if !committed {
			t.Fatal("commitStagedChanges did not report the staged commit")
		}
		after := mustOutput(t, repo, "git", "rev-parse", "HEAD")
		if after == before {
			t.Fatalf("HEAD remained at %s", before)
		}
		if status := mustOutput(t, repo, "git", "status", "--porcelain"); status != "" {
			t.Fatalf("repository is not clean after commit: %q", status)
		}
	})

	t.Run("git inspection errors are returned", func(t *testing.T) {
		committed, err := commitStagedChanges(t.TempDir(), "synthetic release")
		if err == nil {
			t.Fatal("commitStagedChanges succeeded outside a git repository")
		}
		if committed {
			t.Fatal("commitStagedChanges reported a commit after git inspection failed")
		}
		if !strings.Contains(err.Error(), "inspect staged release changes") {
			t.Fatalf("error = %q, want staged-change inspection context", err)
		}
	})
}

func TestCandidateEnvironmentIsolatesGoCaches(t *testing.T) {
	tmp := t.TempDir()
	globalModuleCache := filepath.Join(t.TempDir(), "global-modcache")
	globalBuildCache := filepath.Join(t.TempDir(), "global-gocache")
	t.Setenv("GOMODCACHE", globalModuleCache)
	t.Setenv("GOCACHE", globalBuildCache)
	t.Setenv("GOWORK", filepath.Join(t.TempDir(), "toxic-go.work"))
	t.Setenv("GOFLAGS", "-mod=vendor")

	env := candidateEnvironment(tmp, "file:///candidate")
	wantModuleCache := filepath.Join(tmp, "gomodcache")
	wantBuildCache := filepath.Join(tmp, "gocache")

	if got := mustOutputWithEnv(t, "", env, "go", "env", "GOMODCACHE"); got != wantModuleCache {
		t.Fatalf("GOMODCACHE = %q, want %q", got, wantModuleCache)
	}
	if got := mustOutputWithEnv(t, "", env, "go", "env", "GOCACHE"); got != wantBuildCache {
		t.Fatalf("GOCACHE = %q, want %q", got, wantBuildCache)
	}
	if got := mustOutputWithEnv(t, "", env, "go", "env", "GOWORK"); got != "off" {
		t.Fatalf("GOWORK = %q, want %q", got, "off")
	}
	if got := mustOutputWithEnv(t, "", env, "go", "env", "GOFLAGS"); got != "" {
		t.Fatalf("GOFLAGS = %q, want empty", got)
	}
}

func mustCommand(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %s: %v", name, args, raw, err)
	}
}

func mustOutput(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	return mustOutputWithEnv(t, dir, nil, name, args...)
}

func mustOutputWithEnv(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	got, err := output(dir, env, name, args...)
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return got
}
