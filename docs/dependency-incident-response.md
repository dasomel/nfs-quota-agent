# Dependency incident response

Written for #26 in response to the Rust supply-chain incident it references
(a malicious transitive crate executing arbitrary code via a `build.rs`
build script during `cargo build`, even when the application never calls
the compromised crate directly). This document covers what does and does
not apply to this repository's dependency graph, and the concrete steps to
take if a Go module (or a GitHub Actions/Docker input, already covered by
the SHA/digest pinning from PR #30/#48) is found to be compromised or
yanked.

## Why the referenced attack class doesn't transfer directly to Go modules

Cargo's `build.rs` (and npm's `postinstall`, and similar hooks in several
other ecosystems) is an arbitrary-code-execution hook the package manager
runs automatically during dependency resolution/build, before any of the
application's own code runs. **Go modules have no equivalent.**
`go mod download`, `go build`, and `go test` never execute code shipped by
a dependency at fetch or build time — a Go module is source code compiled
directly into the importing binary, with no install-time hook. Fetching
one, even a malicious one, cannot run code until something in this
repository's own source actually imports and calls it.

This repo's only place arbitrary generated code exists is `make generate`
(`internal/apis/quota/v1alpha1/zz_generated.deepcopy.go` and the CRD
manifest), which runs `go tool controller-gen` -- itself a pinned
dependency in `go.mod`'s `tool` block (`go.sum`-verified like any other
module), not a third-party download fetched and executed at generate time.
It is also never run automatically by CI's `build`/`test` jobs, only by a
developer (or CI's dedicated `Generate Check` job, which only diffs
already-committed output -- see `.github/workflows/ci.yaml`).

The attack surface this repo actually has is the one already worked on
across #26's earlier PRs: third-party **GitHub Actions** (arbitrary code
that runs as part of the workflow itself) and **container base images**
-- both already pinned to immutable digests, not floating tags.

## What Go's toolchain already verifies, and how to make it explicit

`GOSUMDB=sum.golang.org` (the default, unset/unoverridden by this repo's
CI -- confirmed via `go env GOSUMDB GOFLAGS`) means every `go mod
download` and `go build` already refuses to proceed if a module's content
hash doesn't match either `go.sum` or the public checksum transparency
log. This is not optional or best-effort; a mismatch is a hard build
failure. In that sense, "all release dependencies are integrity-verified"
already holds structurally for the Go module graph, as a side effect of
using Go's default toolchain configuration.

CI now makes this explicit rather than implicit: the `Test` job runs
`go mod verify` right after `go mod download`, so a hash mismatch shows up
as its own named, auditable CI step instead of being buried inside
whichever later step happened to trigger the download.

## Quarantine: blocking a specific compromised or yanked version

Go's mechanism for permanently refusing a specific module version, even
transitively, is `go.mod`'s `exclude` directive:

```bash
go mod edit -exclude=github.com/example/compromised-module@v1.2.3
go mod tidy
```

After this, `go build`/`go mod tidy` will refuse to select that version
for any import path, direct or transitive, and will instead resolve to
the next version that satisfies every other constraint (or fail loudly if
none exists) -- forcing an explicit, visible decision rather than silently
continuing to build against it.

## Rollback: recovering from a bad dependency bump already merged

1. Identify the commit that bumped the dependency (`git log -- go.mod
   go.sum`).
2. `git revert` that commit (preferred over hand-editing `go.mod`/`go.sum`
   back -- keeps the incident visible in history) or `go get
   module@<last-known-good-version>` followed by `go mod tidy`.
3. Regenerate `THIRD_PARTY_LICENSES.md` (`make license`) and the SBOM
   (`make sbom`) -- both are dependency-tree-derived and go stale the
   moment `go.sum` changes.
4. Confirm the compromised version no longer resolves anywhere in the
   graph: `go list -m all | grep <module>` should show nothing, or show
   only the rolled-back version.
5. `go mod verify` and the full CI suite (`Test`, `License Check`,
   `Security Scan`) must pass before merging the rollback.
6. If the bad version already shipped in a tagged release, cut a new
   patch release from the rollback commit -- do not attempt to mutate or
   re-point an already-published Git tag or container image tag.

## What this does not cover

- **Automated detection of a newly-compromised Go module**: `dependabot.yml`
  deliberately does not include the `gomod` ecosystem (see PR #26's first
  comment for why -- `THIRD_PARTY_LICENSES.md` staleness would fail every
  such PR without a follow-up regeneration step). `govulncheck` and Trivy
  (both already in CI) only catch dependencies with a *known, published*
  CVE -- neither detects a supply-chain compromise before it's been
  disclosed and added to a vulnerability database.
- **A negative test that simulates malicious build-time behavior**: given
  the structural point above (Go has no build-script hook to simulate),
  there is no equivalent of "prove a malicious `build.rs` gets caught" to
  write for this repo's own dependency-fetch path. The closest analogous
  risk -- a compromised GitHub Action executing during CI -- is mitigated
  by SHA-pinning (#26's earlier PRs), not by a test in this repository.
- **Egress blocking**, **offline release verification**, and **build-time
  dependency inventory in SBOM/provenance** remain open, tracked in #26's
  existing comment thread.
