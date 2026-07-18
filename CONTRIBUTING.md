# Contributing to nfs-quota-agent

Welcome! Thank you for helping improve the NFS Quota Agent. Please follow these guidelines to make sure your contributions can be reviewed and integrated smoothly.

## Development Setup

The project is built using **Go 1.26**.

We use a `Makefile` to automate common development workflows. The primary make targets are:
- `make build`         - Builds the binary into the `bin/` directory.
- `make test`          - Runs all unit tests.
- `make test-coverage` - Runs unit tests and generates an HTML coverage report.
- `make fmt`           - Formats code according to standards using `go fmt`.
- `make vet`           - Runs static analysis check via `go vet`.
- `make lint`          - Runs quality checks via `golangci-lint` (if installed).

## Testing Conventions

All unit tests must remain hermetic and should not depend on external infrastructure, system binaries, or local NFS mount configurations.
1. **Mocking External Commands**: Always stub system utility execution (like `xfs_quota`, `setquota`, or `df`) by providing a fake `CommandRunner` instance via `quota.SetCommandRunnerForTesting`. Ensure you invoke the returned restore function to prevent state leaks across tests:
   ```go
   restore := quota.SetCommandRunnerForTesting(fakeRunner)
   defer restore()
   ```
2. **Kubernetes API Calls**: Avoid using real API servers. Mock cluster operations using the standard `k8s.io/client-go/kubernetes/fake` client-set.

## Commit Message Guidelines

We use **Conventional Commits** to categorize modifications and automatically generate release notes using `git-cliff`. 

Format commit messages as:
```
<type>(<scope>): <description>

[optional body]
```
Common types: `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`.

## Pull Request Flow

1. Create a branch from `main` (e.g. `feat/my-awesome-feature`).
2. Implement your changes, adding clear comments and unit tests.
3. Validate your changes locally by running code style checks:
   ```bash
   make fmt && make vet && make test
   ```
4. Push your branch and open a Pull Request (PR) against the `main` branch.
5. All CI checks must pass, and the code must be reviewed by maintainers before merging.

## Release Process

We automate our releases through GitHub Actions. Pushing a SemVer-compliant Git tag (e.g., `v1.2.3`) to the repository triggers the release workflow, which compiles release binaries, packages Helm charts, builds multi-arch container images, and generates a changelog entry using `git-cliff`.
