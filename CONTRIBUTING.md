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
- `make license`       - Regenerates `THIRD_PARTY_LICENSES.md` from `go.mod`/`go.sum` and fails if any dependency's license isn't in `hack/allowed-licenses.txt`.
- `make sbom`          - Generates an SBOM (SPDX + CycloneDX) for the Go dependency tree via `trivy` (if installed).
- `make generate`      - Regenerates deepcopy code and the CRD manifest for `internal/apis/quota/v1alpha1` via `controller-gen`.
- `make compat-matrix`  - Validates `hack/compatibility-matrix.json` (the machine-readable filesystem/architecture/Kubernetes-version support matrix, #5) has the required shape.
- `make verify-release RELEASE_DIR=<dir>` - Offline-verifies a downloaded release bundle's binaries/chart/SBOM/compatibility matrix against `release-manifest.json`'s recorded sha256 digests (#16, #26). Defaults `RELEASE_DIR` to the current directory.

If a PR changes `go.mod` or `go.sum`, run `make license` and commit the regenerated `THIRD_PARTY_LICENSES.md` — CI fails the `License Check` job if it goes stale.

If a PR changes `internal/apis/quota/v1alpha1/types.go`, run `make generate` and commit the regenerated `zz_generated.deepcopy.go` and `charts/nfs-quota-agent/crds/quota.nfs.io_quotapolicies.yaml` — CI fails the `Generate Check` job if either goes stale. Never hand-edit either generated file; fix the kubebuilder markers in `types.go` and regenerate instead.

### Make target ↔ CI job mapping

Every job in [`.github/workflows/ci.yaml`](.github/workflows/ci.yaml) has a local equivalent you can run before pushing, except where noted:

| CI job | Local equivalent |
|---|---|
| `Test` | `make test-coverage` (CI additionally runs with `-race`: `go test -race ./...`) |
| `Lint` | `make lint` (CI pins `golangci-lint` to the version in `ci.yaml`'s `version:` field — install the same version locally with `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@<version>` to avoid false negatives/positives from a version drift) |
| `Build` | `make build-linux` (CI builds `linux/amd64` + `linux/arm64` only; `build-linux` additionally builds `linux/arm/v7`) |
| `License Check` | `make license` (regenerates `THIRD_PARTY_LICENSES.md` in place and checks every dependency's license against `hack/allowed-licenses.txt`; if the file changed, `git diff` shows what CI's separate staleness check would have failed on — commit the update) |
| `Generate Check` | `make generate`, then `git diff --exit-code` on `internal/apis/quota/v1alpha1/zz_generated.deepcopy.go` and `charts/nfs-quota-agent/crds/quota.nfs.io_quotapolicies.yaml` |
| `Helm Lint` | `make helm-lint`, then `helm template ./charts/nfs-quota-agent --set metrics.serviceMonitor.enabled=true --set metrics.prometheusRule.enabled=true --set podDisruptionBudget.enabled=true` for the same smoke-test coverage CI runs |
| `Compatibility Matrix` | `make compat-matrix` (shape-only check — every entry in `hack/compatibility-matrix.json` has a `status` and `evidence` field. It cannot verify the `status` values themselves are still true; keep them honest by hand when new evidence appears, same as `hack/allowed-licenses.txt`.) |
| `Security Scan` | **No local make target yet, and `govulncheck` is not a `go tool` dependency of this module** (unlike `go-licenses`/`controller-gen`). CI runs `aquasecurity/trivy-action` (filesystem scan) and `golang/govulncheck-action`; reproduce manually with `trivy fs .` (if `trivy` is installed) and `go run golang.org/x/vuln/cmd/govulncheck@latest ./...`. Tracked as a gap in #16. |

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

Every release publishes a `release-manifest.json` GitHub Release asset recording the source commit, workflow run, container image digest, and sha256 digests of the chart, binaries, SBOM, and `compatibility-matrix.json`. After downloading a release's assets into one directory, verify them offline against that manifest with:

```bash
make verify-release RELEASE_DIR=<download-dir>
```

This checks every file's digest against what the release pipeline actually produced; it does not verify the container image itself (that needs registry access — the command to run is printed if the manifest names an image digest). See `hack/verify-release.py` for the full logic.
