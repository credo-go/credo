# Contributing to Credo

Thank you for your interest in contributing to Credo! This guide will help you get started.

## Development Setup

1. **Fork and clone** the repository.
2. Ensure you have **Go 1.27+** installed.
3. Install `golangci-lint` and a race-enabled Go toolchain.
4. Run `make check` to vet, lint, and test both the root module and the
   `store/sqldb` submodule (the benchmark smoke step remains root-only).

## Branch Strategy

- `main` — Stable branch, always passes CI.
- `dev` — Integration branch for features.
- Feature branches: `feat/<name>`, `fix/<name>`, `docs/<name>`.

Always branch from `dev` for new work.

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

1. Create a feature branch from `dev`.
2. Write tests first (TDD is encouraged).
3. Ensure `make check` passes locally. If the platform cannot run `-race`, the
   required CI race jobs remain authoritative and must pass before merge.
4. Open a PR with a clear title and description explaining:
   - **What** changed
   - **Why** it changed
   - **How** to test it
5. At least one maintainer review is required before merge.

## Updating Bun

`store/sqldb` has a narrow compatibility layer for private `bun.SelectQuery`
state that Bun v1.2.18 does not copy. Update `github.com/uptrace/bun` and its
three dialect modules together to the same reviewed release.

Every Bun update must pass the full `Test (store/sqldb)` race job, including
`TestBunSelectCloneLayoutCompatibility` and the critical query-state,
pagination-count, and SQL-rendering contract tests. The normal build, tidy,
lint, and real PostgreSQL/MySQL jobs must pass as well.

If a Bun update changes private layout or SQL semantics, first evaluate an
upstream fix, removing or narrowing the compatibility layer, or narrowing the
Credo contract. Do not automatically expand unsafe private-field access or add
SQL parser logic merely to preserve the previous implementation.

Also re-check the migration finalizer limitation tracked in
[Bun #1389](https://github.com/uptrace/bun/issues/1389): remove the Bun v1.2.18
`.tx.up.sql` warning only after a conformance test proves that Commit/Rollback
errors reach the caller and can gate the applied marker.

## Releasing

Credo is a multi-module repository:

- `github.com/credo-go/credo` is the root framework module.
- `github.com/credo-go/credo/store/sqldb` is a submodule for the Bun SQL wrapper and its heavier database dependencies.

Before the first root tag exists, `store/sqldb/go.mod` uses a bootstrap `replace github.com/credo-go/credo => ../..` so the submodule can test against the in-tree root module. Do not commit `go.work`; it is for local development only and is ignored by Git. CI's `Release gate (replace-free consumer)` job creates temporary lockstep tags and builds an external consumer, so the publishable module graph is tested without changing this bootstrap state.

The modules use the following compatibility rule:

| `store/sqldb` version | Compatible root `credo` version |
| --- | --- |
| `vX.Y.Z` | exactly `vX.Y.Z` |

Release both modules in this order:

1. Ensure `main` is green and the working tree is clean.
2. Finalize `CHANGELOG.md` by replacing the version's `Unreleased` marker with the release date.
3. In `store/sqldb/go.mod`, require that exact root version and remove the bootstrap `replace`; then run `go mod tidy` inside `store/sqldb` and commit the release preparation.
4. Run `go run ./scripts/releasegate prepared v0.1.0` and `go run ./scripts/releasegate candidate v0.1.0`. The first command fails on dependency-version or `replace` drift; the second builds a temporary external consumer.
5. From the `main` branch, dispatch the `Release` GitHub Actions workflow with `version=v0.1.0`. It repeats the gates and atomically publishes `v0.1.0` plus `store/sqldb/v0.1.0`, so the nested tag is never visible without its required root tag.

The release notes/CHANGELOG entry must retain the compatibility table above (with `X.Y.Z` replaced by the released version) whenever the lockstep policy changes. The release workflow deliberately fails before tagging if the prepared `go.mod` version differs from its input or still contains the local replacement.

After the first release, local cross-module development can use an ignored workspace:

```sh
go work init . ./store/sqldb
```

## Coding Standards

- Format with `gofmt`.
- Lint with `golangci-lint` (see `.golangci.yml`).
- Every exported symbol must have a godoc comment.
- Table-driven tests with `t.Run()` sub-tests.
- Target 80%+ code coverage for core packages.
- Zero external dependencies in core packages (root).

## Reporting Issues

- Use the [Bug Report](.github/ISSUE_TEMPLATE/bug_report.md) template for bugs.
- Use the [Feature Request](.github/ISSUE_TEMPLATE/feature_request.md) template for ideas.

## Code of Conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md). Be respectful. We are building this together.
