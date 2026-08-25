package main

import (
	"encoding/json"
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

func TestRunArgumentValidation(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "no arguments", args: nil, wantErr: "usage:"},
		{name: "too many arguments", args: []string{"candidate", "v0.1.0", "extra"}, wantErr: "usage:"},
		{name: "unknown mode", args: []string{"frobnicate"}, wantErr: `unknown mode "frobnicate"`},
		{name: "prepared without version", args: []string{"prepared"}, wantErr: "prepared mode requires"},
		{name: "tidy with version", args: []string{"tidy", "v0.1.0"}, wantErr: "tidy mode does not accept"},
		{name: "workspace with version", args: []string{"workspace", "v0.1.0"}, wantErr: "workspace mode does not accept"},
		{name: "candidate without v prefix", args: []string{"candidate", "1.2.3"}, wantErr: "must be canonical semver"},
		{name: "candidate with build metadata", args: []string{"candidate", "v1.2.3+build.1"}, wantErr: "must be canonical semver"},
		{name: "prepared with leading zero", args: []string{"prepared", "v01.2.3"}, wantErr: "must be canonical semver"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := run(tt.args)
			if err == nil {
				t.Fatalf("run(%q) succeeded, want error containing %q", tt.args, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("run(%q) error = %q, want it to contain %q", tt.args, err, tt.wantErr)
			}
		})
	}
}

func TestCheckPreparedModule(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not available")
	}
	t.Setenv("GOWORK", filepath.Join(t.TempDir(), "toxic-go.work"))
	t.Setenv("GOFLAGS", "-mod=vendor")

	t.Run("prepared module passes", func(t *testing.T) {
		repo := writeTidyFixture(t, "v0.11.0", "", false)

		if err := checkPreparedModule(repo, "v0.11.0"); err != nil {
			t.Fatalf("check prepared module: %v", err)
		}
	})

	t.Run("version mismatch fails", func(t *testing.T) {
		repo := writeTidyFixture(t, "v0.10.0", "", false)

		err := checkPreparedModule(repo, "v0.11.0")
		if err == nil || !strings.Contains(err.Error(), "want v0.11.0") {
			t.Fatalf("error = %v, want root-requirement mismatch", err)
		}
	})

	t.Run("missing root requirement fails", func(t *testing.T) {
		repo := t.TempDir()
		sqldbDir := filepath.Join(repo, "store", "sqldb")
		if err := os.MkdirAll(sqldbDir, 0o755); err != nil {
			t.Fatalf("create store/sqldb: %v", err)
		}
		mustWriteFile(t, filepath.Join(sqldbDir, "go.mod"), "module "+sqldbModule+"\n\ngo 1.27\n")

		err := checkPreparedModule(repo, "v0.11.0")
		if err == nil || !strings.Contains(err.Error(), "want v0.11.0") {
			t.Fatalf("error = %v, want root-requirement mismatch", err)
		}
	})

	t.Run("leftover bootstrap replacement fails", func(t *testing.T) {
		repo := writeTidyFixture(t, "v0.11.0", "replace "+rootModule+" => ../..\n", false)

		err := checkPreparedModule(repo, "v0.11.0")
		if err == nil || !strings.Contains(err.Error(), "must not replace") {
			t.Fatalf("error = %v, want replacement rejection", err)
		}
	})

	t.Run("leftover foreign replacement fails", func(t *testing.T) {
		repo := writeTidyFixture(t, "v0.11.0", "replace "+rootModule+" v0.11.0 => "+rootModule+" v0.10.0\n", false)

		err := checkPreparedModule(repo, "v0.11.0")
		if err == nil || !strings.Contains(err.Error(), "must not replace") {
			t.Fatalf("error = %v, want replacement rejection", err)
		}
	})
}

func TestReadSQLDBModuleErrors(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not available")
	}

	t.Run("missing module file", func(t *testing.T) {
		_, err := readSQLDBModule(t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "read store/sqldb/go.mod") {
			t.Fatalf("error = %v, want read failure", err)
		}
	})

	t.Run("malformed module file", func(t *testing.T) {
		repo := t.TempDir()
		sqldbDir := filepath.Join(repo, "store", "sqldb")
		if err := os.MkdirAll(sqldbDir, 0o755); err != nil {
			t.Fatalf("create store/sqldb: %v", err)
		}
		mustWriteFile(t, filepath.Join(sqldbDir, "go.mod"), "not a module file\n")

		_, err := readSQLDBModule(repo)
		if err == nil || !strings.Contains(err.Error(), "read store/sqldb/go.mod") {
			t.Fatalf("error = %v, want read failure", err)
		}
	})
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

func TestCandidateTagsFollowHEAD(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}

	const version = "v0.11.0"
	tests := []struct {
		name        string
		prepareHEAD func(*testing.T, string) string
		wantCommit  bool
	}{
		{
			name: "synthetic commit",
			prepareHEAD: func(t *testing.T, repo string) string {
				mustWriteFile(t, filepath.Join(repo, "tracked.txt"), "synthetic\n")
				mustCommand(t, repo, "git", "add", "tracked.txt")
				return mustOutput(t, repo, "git", "rev-parse", "HEAD")
			},
			wantCommit: true,
		},
		{
			name: "no-op recovery",
			prepareHEAD: func(t *testing.T, repo string) string {
				mustWriteFile(t, filepath.Join(repo, "tracked.txt"), "prepared\n")
				mustCommand(t, repo, "git", "add", "tracked.txt")
				mustCommand(t, repo, "git", "commit", "--quiet", "-m", "prepared release")
				return mustOutput(t, repo, "git", "rev-parse", "HEAD")
			},
			wantCommit: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := writeTaggedGitFixture(t, version)
			beforeCommit := tt.prepareHEAD(t, repo)

			committed, err := commitStagedChanges(repo, "synthetic release")
			if err != nil {
				t.Fatalf("commit staged changes: %v", err)
			}
			if committed != tt.wantCommit {
				t.Fatalf("commitStagedChanges committed = %t, want %t", committed, tt.wantCommit)
			}
			head := mustOutput(t, repo, "git", "rev-parse", "HEAD")
			if tt.wantCommit && head == beforeCommit {
				t.Fatalf("synthetic commit left HEAD at %s", head)
			}
			if !tt.wantCommit && head != beforeCommit {
				t.Fatalf("no-op changed HEAD from %s to %s", beforeCommit, head)
			}

			if err := setCandidateTags(repo, version); err != nil {
				t.Fatalf("set candidate tags: %v", err)
			}
			assertTagCommit(t, repo, version, head)
			assertTagCommit(t, repo, "store/sqldb/"+version, head)

			if err := setCandidateTags(repo, version); err != nil {
				t.Fatalf("repeat set candidate tags: %v", err)
			}
			assertTagCommit(t, repo, version, head)
			assertTagCommit(t, repo, "store/sqldb/"+version, head)
		})
	}
}

func TestCandidateTagErrorContext(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}

	t.Run("root tag", func(t *testing.T) {
		err := setCandidateTags(t.TempDir(), "v0.11.0")
		if err == nil || !strings.Contains(err.Error(), "tag synthetic root module") {
			t.Fatalf("error = %v, want root tag context", err)
		}
	})

	t.Run("sqldb tag", func(t *testing.T) {
		repo := writeTaggedGitFixture(t, "store")
		err := setCandidateTags(repo, "v0.11.0")
		if err == nil || !strings.Contains(err.Error(), "tag synthetic sqldb module") {
			t.Fatalf("error = %v, want sqldb tag context", err)
		}
	})
}

func TestCheckCandidateRecoversExistingTags(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not available")
	}

	const version = "v0.11.0"
	repo := writeCandidateRecoveryFixture(t, version)
	if err := checkCandidate(repo, version); err != nil {
		t.Fatalf("check candidate with inherited release tags: %v", err)
	}
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

func TestTidySQLDBModule(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not available")
	}
	t.Setenv("GOWORK", filepath.Join(t.TempDir(), "toxic-go.work"))
	t.Setenv("GOFLAGS", "-mod=vendor")

	t.Run("adds and removes a temporary replacement", func(t *testing.T) {
		repo := writeTidyFixture(t, "v0.2.0", "", false)

		if err := tidySQLDBModule(repo); err != nil {
			t.Fatalf("tidy store/sqldb: %v", err)
		}
		assertRootReplacement(t, repo, "")
	})

	t.Run("preserves the bootstrap replacement", func(t *testing.T) {
		repo := writeTidyFixture(t, "v0.0.0", "replace "+rootModule+" => ../..\n", false)

		if err := tidySQLDBModule(repo); err != nil {
			t.Fatalf("tidy store/sqldb: %v", err)
		}
		assertRootReplacement(t, repo, "../..")
	})

	t.Run("rejects an unexpected replacement", func(t *testing.T) {
		repo := writeTidyFixture(t, "v0.2.0", "replace "+rootModule+" => ../unexpected\n", false)

		err := tidySQLDBModule(repo)
		if err == nil || !strings.Contains(err.Error(), "unexpected replacement") {
			t.Fatalf("error = %v, want unexpected replacement", err)
		}
	})

	t.Run("removes the temporary replacement after tidy fails", func(t *testing.T) {
		repo := writeTidyFixture(t, "v0.2.0", "", true)

		err := tidySQLDBModule(repo)
		if err == nil || !strings.Contains(err.Error(), "tidy store/sqldb") {
			t.Fatalf("error = %v, want tidy failure", err)
		}
		assertRootReplacement(t, repo, "")
	})
}

func TestCreateInTreeWorkspace(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not available")
	}
	t.Setenv("GOWORK", filepath.Join(t.TempDir(), "toxic-go.work"))
	t.Setenv("GOFLAGS", "-mod=vendor")

	tests := []struct {
		name        string
		rootVersion string
		replacement string
	}{
		{name: "prepared module", rootVersion: "v0.2.0"},
		{name: "bootstrap module", rootVersion: "v0.0.0", replacement: "replace " + rootModule + " => ../..\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := writeTidyFixture(t, tt.rootVersion, tt.replacement, false)
			if err := createInTreeWorkspace(repo); err != nil {
				t.Fatalf("create workspace: %v", err)
			}

			workFile := filepath.Join(repo, "go.work")
			workspaceEnvironment := []string{"GOWORK=" + workFile, "GOFLAGS="}
			modules := mustOutputWithEnv(t, filepath.Join(repo, "store", "sqldb"), workspaceEnvironment, "go", "list", "-m", "all")
			if !strings.Contains(modules, rootModule) || !strings.Contains(modules, sqldbModule) {
				t.Fatalf("workspace module list does not contain both Credo modules:\n%s", modules)
			}

			raw := mustOutputWithEnv(t, repo, workspaceEnvironment, "go", "work", "edit", "-json")
			var work struct {
				Replace []struct {
					Old struct {
						Path    string
						Version string
					}
					New struct {
						Path string
					}
				}
			}
			if err := json.Unmarshal([]byte(raw), &work); err != nil {
				t.Fatalf("decode go.work: %v", err)
			}
			if len(work.Replace) != 1 ||
				work.Replace[0].Old.Path != rootModule ||
				work.Replace[0].Old.Version != tt.rootVersion ||
				work.Replace[0].New.Path != "." {
				t.Fatalf("workspace replacements = %#v, want %s %s => .", work.Replace, rootModule, tt.rootVersion)
			}

			if err := createInTreeWorkspace(repo); err == nil || !strings.Contains(err.Error(), "go.work already exists") {
				t.Fatalf("second create error = %v, want existing-workspace refusal", err)
			}
		})
	}
}

func writeTidyFixture(t *testing.T, rootVersion, replacement string, brokenImport bool) string {
	t.Helper()

	repo := t.TempDir()
	sqldbDir := filepath.Join(repo, "store", "sqldb")
	if err := os.MkdirAll(sqldbDir, 0o755); err != nil {
		t.Fatalf("create store/sqldb: %v", err)
	}

	mustWriteFile(t, filepath.Join(repo, "go.mod"), "module "+rootModule+"\n\ngo 1.27\n")
	mustWriteFile(t, filepath.Join(repo, "credo.go"), "package credo\n\nconst Version = \"fixture\"\n")
	mustWriteFile(t, filepath.Join(sqldbDir, "go.mod"), "module "+sqldbModule+"\n\ngo 1.27\n\nrequire "+rootModule+" "+rootVersion+"\n\n"+replacement)

	importPath := rootModule
	if brokenImport {
		importPath += "/missing"
	}
	mustWriteFile(t, filepath.Join(sqldbDir, "sqldb.go"), "package sqldb\n\nimport credo \""+importPath+"\"\n\nvar _ = credo.Version\n")
	return repo
}

func writeTaggedGitFixture(t *testing.T, version string) string {
	t.Helper()

	repo := t.TempDir()
	mustCommand(t, repo, "git", "init", "--quiet")
	mustCommand(t, repo, "git", "config", "user.name", "Credo release gate test")
	mustCommand(t, repo, "git", "config", "user.email", "release-gate-test@credo.invalid")
	mustWriteFile(t, filepath.Join(repo, "tracked.txt"), "initial\n")
	mustCommand(t, repo, "git", "add", "tracked.txt")
	mustCommand(t, repo, "git", "commit", "--quiet", "-m", "initial")
	mustCommand(t, repo, "git", "tag", "-a", version, "-m", "stale root release")
	if version != "store" {
		mustCommand(t, repo, "git", "tag", "-a", "store/sqldb/"+version, "-m", "stale sqldb release")
	}
	return repo
}

func writeCandidateRecoveryFixture(t *testing.T, version string) string {
	t.Helper()

	repo := t.TempDir()
	sqldbDir := filepath.Join(repo, "store", "sqldb")
	if err := os.MkdirAll(sqldbDir, 0o755); err != nil {
		t.Fatalf("create store/sqldb: %v", err)
	}
	mustCommand(t, repo, "git", "init", "--quiet")
	mustCommand(t, repo, "git", "config", "user.name", "Credo release gate test")
	mustCommand(t, repo, "git", "config", "user.email", "release-gate-test@credo.invalid")

	mustWriteFile(t, filepath.Join(repo, "go.mod"), "module "+rootModule+"\n\ngo 1.27\n")
	mustWriteFile(t, filepath.Join(repo, "credo.go"), "package credo\n\nconst Version = \"fixture\"\n")
	mustWriteFile(t, filepath.Join(sqldbDir, "go.mod"), "module "+sqldbModule+"\n\ngo 1.27\n\nrequire "+rootModule+" "+version+"\n")
	mustWriteFile(t, filepath.Join(sqldbDir, "sqldb.go"), "package sqldb\n\nconst Stale = true\n")
	mustCommand(t, repo, "git", "add", ".")
	mustCommand(t, repo, "git", "commit", "--quiet", "-m", "stale release")
	mustCommand(t, repo, "git", "tag", "-a", version, "-m", "stale root release")
	mustCommand(t, repo, "git", "tag", "-a", "store/sqldb/"+version, "-m", "stale sqldb release")

	mustWriteFile(t, filepath.Join(sqldbDir, "sqldb.go"), "package sqldb\n\nvar ErrUnsupportedCountQuery = struct{}{}\n")
	mustCommand(t, repo, "git", "add", "store/sqldb/sqldb.go")
	mustCommand(t, repo, "git", "commit", "--quiet", "-m", "prepare release")
	return repo
}

func assertTagCommit(t *testing.T, repo, tag, want string) {
	t.Helper()
	if got := mustOutput(t, repo, "git", "rev-parse", tag+"^{commit}"); got != want {
		t.Fatalf("tag %s points to %s, want %s", tag, got, want)
	}
}

func assertRootReplacement(t *testing.T, repo, wantPath string) {
	t.Helper()

	raw := mustOutputWithEnv(t, filepath.Join(repo, "store", "sqldb"), isolatedGoEnvironment, "go", "mod", "edit", "-json")
	var mod moduleFile
	if err := json.Unmarshal([]byte(raw), &mod); err != nil {
		t.Fatalf("decode go.mod: %v", err)
	}

	gotPath := ""
	for _, replacement := range mod.Replace {
		if replacement.Old.Path == rootModule {
			gotPath = replacement.New.Path
		}
	}
	if gotPath != wantPath {
		t.Fatalf("root replacement path = %q, want %q", gotPath, wantPath)
	}
}

func mustWriteFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
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
