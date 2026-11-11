# Contributing to Credo

Thank you for your interest in contributing to Credo! This guide will help you get started.

## Development Setup

1. **Fork and clone** the repository.
2. Ensure you have **Go 1.27+** installed.
3. Install `golangci-lint` and a race-enabled Go toolchain.
4. Run `make check` to vet, lint, and test both the root module and the `store/sqldb` submodule (the benchmark smoke step remains root-only).

## Branch Strategy

- `main` — Stable, protected branch: changes land only through pull requests whose required checks pass, and it always passes CI.
- Feature branches: `feat/<name>`, `fix/<name>`, `docs/<name>`, created from `main`.
- Release preparation branches: `release/vX.Y.Z` (see [Releasing](#releasing)).

Branch from `main` for new work and open the pull request back into `main`; pull requests are squash-merged. There is no separate integration branch.

## Commit Messages

We follow [Conventional Commits](https://www.conventionalcommits.org/):

```
type(scope): description

feat(router): add wildcard parameter support
fix(container): resolve singleton race condition
docs(readme): add quick start example
test(validation): add email rule edge cases
refactor(middleware): simplify chain composition
```

**Types:** `feat`, `fix`, `docs`, `style`, `refactor`, `test`, `chore`, `perf`, `ci`

**Scope:** Package name (e.g., `router`, `container`, `middleware`).

## Pull Request Process

1. Create a feature branch from `main`.
2. Write tests first (TDD is encouraged).
3. Ensure `make check` passes locally. If the platform cannot run `-race`, the required CI race jobs remain authoritative and must pass before merge.
4. Open a PR with a clear title and description explaining:
   - **What** changed
   - **Why** it changed
   - **How** to test it
5. At least one maintainer review and green required checks are needed before the squash-merge into `main`.

## Performance Changes

A change made for performance carries its evidence. Run the relevant benchmarks on the base commit and on the change with `go test -run '^$' -bench <name> -count=10 -benchmem`, compare the two outputs with `benchstat` (`go run golang.org/x/perf/cmd/benchstat@latest old.txt new.txt`) and put the table in the PR body. There is no CI benchmark job because shared runners are too noisy; the PR table is the record. Existing tests stay unchanged unless one pins the old cost.

## Wrapped Dependency Upgrades

An upgrade to a wrapped protocol or infrastructure dependency must identify the candidate tag and commit, review its release/security notes, and pass the full root test, race, vet, build, tidy, and pinned-lint jobs. For `github.com/coder/websocket`, the root test job is also the executable upgrade gate: do not skip or weaken `websocket/upstream_*_conformance_test.go` or the origin, handshake, real-network, and lifecycle suites when updating the pin. Record the upstream tag's Autobahn/conformance evidence in the PR; Credo does not require a separate Autobahn run on every commit.

### Updating Bun

`store/sqldb` uses Bun through its exported API only: no file imports `unsafe` (`TestBunBoundary_NoUnsafe`), and no wrapper reads or restores a private Bun field. Update `github.com/uptrace/bun` and its three dialect modules together to the same reviewed release.

Every Bun update must pass the full `Test (store/sqldb)` race job, including the count conformance tests: `TestCountConformance_SQLite`, and the "count conformance" and "count runs on the query's own connection" subtests of `TestRealDB_Contracts` on PostgreSQL and MySQL. They check Bun's `Count` against the rows of each query shape the wrapper counts, that every count drops ORDER BY, LIMIT, OFFSET and FOR, the `BeforeAppendModel` facts the automatic total relies on, and the connection a count runs on. The query-state, pagination and SQL-rendering contract tests, and the normal build, tidy, lint, and real PostgreSQL/MySQL jobs must pass as well.

If a Bun update changes what its `Count` returns for a shape the wrapper counts, first evaluate an upstream fix, or narrowing the shapes `Count` and `Page` accept so such a query pages with `PageWithCount`. Do not read Bun's private state or add SQL parser logic to preserve the previous behavior.

The `.tx.up.sql` finalizer contract — a COMMIT or ROLLBACK error reaches `Migrate` and leaves the migration unapplied, fixed upstream in Bun v1.3.0 ([Bun #1389](https://github.com/uptrace/bun/issues/1389)) — is pinned by `TestMigrate_TxSQLCommitErrorReachesCaller` on SQLite and by the "transactional SQL migration commit error" subtest of `TestRealDB_Contracts` on PostgreSQL. A Bun update that fails them reinstates the documented warning instead of weakening the tests.

## Releasing

Work accumulates on `main` under the CHANGELOG's `[Unreleased]` heading. A release is cut for a security fix (a patch, at once), for a consumer that needs a tag, or at a milestone the maintainer names — not because a plan or a work item finished. Work done together ships as one release, whatever version its plans named. A consumer that needs unreleased work tracks `main` with a pseudo-version (`go get github.com/credo-go/credo@main`, and the same for `store/sqldb`).

Credo is a multi-module repository:

- `github.com/credo-go/credo` is the root framework module.
- `github.com/credo-go/credo/store/sqldb` is a submodule for the Bun SQL wrapper and its heavier database dependencies.

Before the first root tag exists, `store/sqldb/go.mod` uses a bootstrap `replace github.com/credo-go/credo => ../..` so the submodule can test against the in-tree root module. Do not commit `go.work`; it is for local development only and is ignored by Git. CI's `Release gate (replace-free consumer)` job creates temporary lockstep tags and builds an external consumer, so the publishable module graph is tested without changing this bootstrap state.

The modules use the following compatibility rule:

| `store/sqldb` version | Compatible root `credo` version |
| --------------------- | ------------------------------- |
| `vX.Y.Z`              | exactly `vX.Y.Z`                |

Release both modules in this order:

1. Ensure `main` is green and the working tree is clean.
2. Finalize `CHANGELOG.md` and add reviewed, user-facing notes at `docs/releases/<version>.md`, both in the [short form](#changelog-and-release-notes) below.
3. In `store/sqldb/go.mod`, require that exact root version. Run `go run ./scripts/releasegate tidy`; it preserves the canonical bootstrap `replace` when present, or adds and removes a temporary in-tree replacement when absent. Remove any root-module `replace` before committing the release preparation.
4. Run `go run ./scripts/releasegate prepared vX.Y.Z` and `go run ./scripts/releasegate candidate vX.Y.Z`. The first command fails on dependency-version or `replace` drift; the second builds a temporary external consumer with isolated module and build caches. It fetches Credo's modules from a local clone and every other module from `proxy.golang.org`, checked against `sum.golang.org`, whatever `GOPROXY` and `GOSUMDB` the shell or `go env -w` sets.
5. From the `main` branch, dispatch the `Release` GitHub Actions workflow with the exact version. It repeats the gates, atomically publishes the root and `store/sqldb/` tags, and creates one root GitHub Release. Stable `vX.Y.Z` releases are marked Latest; a prerelease semver such as `v0.3.0-beta.1` creates a GitHub pre-release instead.

The release notes/CHANGELOG entry must retain the compatibility table above (with `X.Y.Z` replaced by the released version) whenever the lockstep policy changes. The release workflow waits for successful `CI` and `CodeQL` push runs on the exact release commit, then fails before tagging if the prepared `go.mod` version differs from its input or still contains the local replacement. Re-running the workflow is safe even after `main` advances: when both tags already exist, it checks out their shared commit, re-verifies it, and creates a missing GitHub Release. A partial tag pair or tags pointing at different commits fail loud.

### CHANGELOG and release notes

Each fact is written once. Why a change was made belongs in its ADR, and how to migrate belongs in the migration guide. The CHANGELOG and the release notes only point to them.

- **CHANGELOG entry**: one bullet per user-visible change, in one or two sentences: what changed and, for a break, its replacement. Mark breaks `**BREAKING**`. Leave out rationale, examples, measurements and sub-lists. Leave out internal refactors and test-only changes. Documentation changes get one line at most.
- **Release notes** (`docs/releases/<version>.md`, the GitHub Release body), about one screen:
  1. a summary of two to four sentences;
  2. the module versions and the `go get` lines;
  3. **Check this first**: only the changes that compile unchanged and behave differently, one table row each, saying what changes and what to do;
  4. links to the CHANGELOG section and the migration guide.

  Release notes have no per-theme sections, no internal changes, no list of what stayed unchanged, and do not repeat the CHANGELOG.

After the first release, local cross-module development can use an ignored workspace:

```sh
go run ./scripts/releasegate workspace
```

## Coding Standards

- Format with `gofmt`.
- Lint with `golangci-lint` (see `.golangci.yml`).
- Every exported symbol must have a godoc comment.
- Table-driven tests with `t.Run()` sub-tests.
- An error or panic message is part of the API: it names what failed, the phase and the remedy, and the test that triggers it asserts those parts, so a message changes only on purpose ([ADR-022](docs/adr/022-bootstrap-and-di-ownership.md)).
- Target 80%+ code coverage for core packages.
- Dependencies are an allowlist, enforced by `architecture_test.go`: what each package may import directly (the root package: the standard library and in-module packages only) and which modules the root package links in through them (`go.yaml.in/yaml/v3`, `github.com/go-viper/mapstructure/v2`, `golang.org/x/text`). Adding a dependency means changing that file in the same PR. The allowlist and the rule that decides what lives in the root package are recorded in [ADR-002](docs/adr/002-code-acquisition-strategy.md#what-lives-in-the-root-package).

## Reporting Issues

- Use the [Bug Report](.github/ISSUE_TEMPLATE/bug_report.md) template for bugs.
- Use the [Feature Request](.github/ISSUE_TEMPLATE/feature_request.md) template for ideas.

## Code of Conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md). Be respectful. We are building this together.
