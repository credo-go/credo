# Contributing to Credo

Thank you for your interest in contributing to Credo! This guide will help you
get started.

## Development Setup

1. **Fork and clone** the repository.
2. Ensure you have **Go 1.26+** installed.
3. Run `make test` to verify your setup.
4. Run `make lint` to check code style.

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
3. Ensure `make lint` and `make test` pass locally.
4. Open a PR with a clear title and description explaining:
   - **What** changed
   - **Why** it changed
   - **How** to test it
5. At least one maintainer review is required before merge.

## Releasing

Credo is a multi-module repository:

- `github.com/credo-go/credo` is the root framework module.
- `github.com/credo-go/credo/store/sqldb` is a submodule for the Bun SQL wrapper
  and its heavier database dependencies.

Before the first root tag exists, `store/sqldb/go.mod` uses a bootstrap
`replace github.com/credo-go/credo => ../..` so the submodule can test against
the in-tree root module. Do not commit `go.work`; it is for local development
only and is ignored by Git.

Release both modules in this order:

1. Ensure `main` is green and the working tree is clean.
2. Finalize `CHANGELOG.md` by replacing the version's `Unreleased` marker with
   the release date.
3. Tag and push the root module, for example `git tag v0.1.0`.
4. In `store/sqldb/go.mod`, require the published root version and remove the
   bootstrap `replace`; then run `go mod tidy` inside `store/sqldb` and commit.
5. Tag and push the submodule with its path prefix, for example
   `git tag store/sqldb/v0.1.0`.

After the first release, local cross-module development can use an ignored
workspace:

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

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md).
Be respectful. We are building this together.
