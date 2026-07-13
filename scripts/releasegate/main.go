// Command releasegate verifies Credo's lockstep multi-module release contract.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	rootModule              = "github.com/credo-go/credo"
	sqldbModule             = rootModule + "/store/sqldb"
	defaultCandidateVersion = "v0.0.0-releasegate.1"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "release gate:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: go run ./scripts/releasegate (candidate [vX.Y.Z] | prepared vX.Y.Z)")
	}

	mode := args[0]
	version := ""
	if len(args) == 2 {
		version = args[1]
	}

	switch mode {
	case "candidate":
		if version == "" {
			version = defaultCandidateVersion
		}
	case "prepared":
		if version == "" {
			return errors.New("prepared mode requires vX.Y.Z")
		}
	default:
		return fmt.Errorf("unknown mode %q", mode)
	}
	if !isCanonicalVersion(version) {
		return fmt.Errorf("version must be canonical semver with a v prefix and no build metadata (got %q)", version)
	}

	repoRoot, err := output("", nil, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("find git checkout: %w", err)
	}

	if mode == "prepared" {
		if err := checkPreparedModule(repoRoot, version); err != nil {
			return err
		}
		fmt.Printf("release gate: store/sqldb is prepared for %s\n", version)
		return nil
	}

	return checkCandidate(repoRoot, version)
}

func isCanonicalVersion(version string) bool {
	if !strings.HasPrefix(version, "v") || strings.Contains(version, "+") {
		return false
	}

	parts := strings.SplitN(strings.TrimPrefix(version, "v"), "-", 2)
	core := strings.Split(parts[0], ".")
	if len(core) != 3 {
		return false
	}
	for _, identifier := range core {
		if !isCanonicalNumericIdentifier(identifier) {
			return false
		}
	}

	if len(parts) == 1 {
		return true
	}
	for _, identifier := range strings.Split(parts[1], ".") {
		if identifier == "" || !isPrereleaseIdentifier(identifier) {
			return false
		}
		if isNumeric(identifier) && !isCanonicalNumericIdentifier(identifier) {
			return false
		}
	}
	return true
}

func isCanonicalNumericIdentifier(identifier string) bool {
	return isNumeric(identifier) && (identifier == "0" || identifier[0] != '0')
}

func isNumeric(identifier string) bool {
	if identifier == "" {
		return false
	}
	for _, char := range identifier {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func isPrereleaseIdentifier(identifier string) bool {
	for _, char := range identifier {
		if (char < '0' || char > '9') &&
			(char < 'A' || char > 'Z') &&
			(char < 'a' || char > 'z') &&
			char != '-' {
			return false
		}
	}
	return true
}

type moduleFile struct {
	Require []struct {
		Path    string
		Version string
	}
	Replace []struct {
		Old struct {
			Path string
		}
	}
}

func checkPreparedModule(repoRoot, version string) error {
	raw, err := output(repoRoot, nil, "go", "mod", "edit", "-json", "store/sqldb/go.mod")
	if err != nil {
		return fmt.Errorf("read store/sqldb/go.mod: %w", err)
	}

	var mod moduleFile
	if err := json.Unmarshal([]byte(raw), &mod); err != nil {
		return fmt.Errorf("decode store/sqldb/go.mod: %w", err)
	}

	rootVersion := ""
	for _, requirement := range mod.Require {
		if requirement.Path == rootModule {
			rootVersion = requirement.Version
			break
		}
	}
	if rootVersion != version {
		return fmt.Errorf("store/sqldb/go.mod requires %s %s, want %s", rootModule, rootVersion, version)
	}
	for _, replacement := range mod.Replace {
		if replacement.Old.Path == rootModule {
			return fmt.Errorf("store/sqldb/go.mod must not replace %s for a release", rootModule)
		}
	}
	return nil
}

func checkCandidate(repoRoot, version string) error {
	tmp, tempErr := os.MkdirTemp("", "credo-release-gate-")
	if tempErr != nil {
		return fmt.Errorf("create temporary directory: %w", tempErr)
	}
	defer os.RemoveAll(tmp)

	repo := filepath.Join(tmp, "credo")
	consumer := filepath.Join(tmp, "consumer")
	if err := os.MkdirAll(consumer, 0o755); err != nil {
		return fmt.Errorf("create consumer directory: %w", err)
	}

	sourceHead, headErr := output(repoRoot, nil, "git", "rev-parse", "HEAD")
	if headErr != nil {
		return fmt.Errorf("resolve candidate HEAD: %w", headErr)
	}
	if err := command("", nil, "git", "clone", "--quiet", "--no-hardlinks", "--no-checkout", repoRoot, repo); err != nil {
		return fmt.Errorf("clone candidate HEAD: %w", err)
	}
	if err := command(repo, nil, "git", "checkout", "--quiet", "--detach", sourceHead); err != nil {
		return fmt.Errorf("check out candidate HEAD: %w", err)
	}
	if err := command(repo, nil, "git", "config", "user.name", "Credo release gate"); err != nil {
		return err
	}
	if err := command(repo, nil, "git", "config", "user.email", "release-gate@credo.invalid"); err != nil {
		return err
	}

	sqldbDir := filepath.Join(repo, "store", "sqldb")
	if err := command(sqldbDir, nil, "go", "mod", "edit", "-require="+rootModule+"@"+version); err != nil {
		return fmt.Errorf("set candidate root requirement: %w", err)
	}
	if err := command(sqldbDir, nil, "go", "mod", "edit", "-dropreplace="+rootModule); err != nil {
		return fmt.Errorf("drop candidate root replacement: %w", err)
	}
	if err := command(repo, nil, "git", "add", "store/sqldb/go.mod"); err != nil {
		return err
	}
	if _, err := commitStagedChanges(repo, "chore: prepare synthetic release "+version); err != nil {
		return err
	}
	if err := command(repo, nil, "git", "tag", version); err != nil {
		return fmt.Errorf("tag synthetic root module: %w", err)
	}
	if err := command(repo, nil, "git", "tag", "store/sqldb/"+version); err != nil {
		return fmt.Errorf("tag synthetic sqldb module: %w", err)
	}

	consumerMod := fmt.Sprintf("module credo.release.gate/consumer\n\ngo 1.27\n\nrequire %s %s\n", sqldbModule, version)
	consumerMain := "package main\n\nimport \"github.com/credo-go/credo/store/sqldb\"\n\nfunc main() {\n\t_ = sqldb.ErrUnsupportedCountQuery\n}\n"
	if err := os.WriteFile(filepath.Join(consumer, "go.mod"), []byte(consumerMod), 0o644); err != nil {
		return fmt.Errorf("write consumer go.mod: %w", err)
	}
	if err := os.WriteFile(filepath.Join(consumer, "main.go"), []byte(consumerMain), 0o644); err != nil {
		return fmt.Errorf("write consumer main.go: %w", err)
	}

	repoURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(repo)}).String()
	env := candidateEnvironment(tmp, repoURL)
	if err := command(consumer, env, "go", "mod", "tidy"); err != nil {
		return fmt.Errorf("resolve replace-free consumer: %w", err)
	}
	if err := command(consumer, env, "go", "build", "./..."); err != nil {
		return fmt.Errorf("build replace-free consumer: %w", err)
	}

	rootState, err := output(consumer, env, "go", "list", "-m", "-f", "{{.Version}} {{if .Replace}}replace{{end}}", rootModule)
	if err != nil {
		return fmt.Errorf("inspect resolved root module: %w", err)
	}
	sqldbState, err := output(consumer, env, "go", "list", "-m", "-f", "{{.Version}} {{if .Replace}}replace{{end}}", sqldbModule)
	if err != nil {
		return fmt.Errorf("inspect resolved sqldb module: %w", err)
	}
	if rootState != version {
		return fmt.Errorf("consumer resolved %s as %q, want %q without replace", rootModule, rootState, version)
	}
	if sqldbState != version {
		return fmt.Errorf("consumer resolved %s as %q, want %q without replace", sqldbModule, sqldbState, version)
	}

	fmt.Printf("release gate: external consumer built %s %s without replace\n", sqldbModule, version)
	return nil
}

func commitStagedChanges(repo, message string) (bool, error) {
	cmd := exec.Command("git", "diff", "--cached", "--quiet")
	cmd.Dir = repo
	raw, err := cmd.CombinedOutput()
	if err == nil {
		return false, nil
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		detail := strings.TrimSpace(string(raw))
		if detail != "" {
			return false, fmt.Errorf("inspect staged release changes: %s: %w", detail, err)
		}
		return false, fmt.Errorf("inspect staged release changes: %w", err)
	}

	if err := command(repo, nil, "git", "commit", "--quiet", "-m", message); err != nil {
		return false, fmt.Errorf("commit synthetic release: %w", err)
	}
	return true, nil
}

func candidateEnvironment(tmp, repoURL string) []string {
	return []string{
		"GOPROXY=direct",
		"GONOSUMDB=github.com/credo-go/credo*",
		"GOMODCACHE=" + filepath.Join(tmp, "gomodcache"),
		"GOCACHE=" + filepath.Join(tmp, "gocache"),
		"GOWORK=off",
		"GOFLAGS=",
		"GIT_ALLOW_PROTOCOL=file:https",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=url." + repoURL + ".insteadOf",
		"GIT_CONFIG_VALUE_0=https://github.com/credo-go/credo",
	}
}

func command(dir string, extraEnv []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func output(dir string, extraEnv []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	raw, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w", strings.TrimSpace(string(raw)), err)
	}
	return strings.TrimSpace(string(raw)), nil
}
