# Transitioning Release Workflow Egress from Audit to Block (#26)

This document outlines the operational procedure and rollout schedule for transitioning `.github/workflows/release.yaml` from StepSecurity Harden Runner's `egress-policy: audit` mode to `egress-policy: block` mode.

## Background & Strategy

In CI (`.github/workflows/ci.yaml`), all jobs run with `egress-policy: block` and explicit `allowed-endpoints` lists. However, `.github/workflows/release.yaml` initially ran in `egress-policy: audit` mode without declared endpoints because its historical runs predated several recently added jobs and tools (such as offline bundle packaging with `skopeo`, `git-cliff`, and provenance signing).

In PR [#26](https://github.com/dasomel/nfs-quota-agent/issues/26), all 9 jobs in `release.yaml` were given explicit, job-specific `allowed-endpoints:` while keeping `egress-policy: audit`.
Under `audit` mode with declared allowlists:
1. **Zero runtime breakage risk**: Network calls outside the list are not blocked.
2. **Actionable delta reporting**: Harden Runner's run summary highlights only endpoints contacted that are *not* in the declared list.
3. **Evidence-based blocking**: Operators can observe exactly what telemetry or dependency traffic occurs during a real release before enforcing strict blocks.

## Rollout Procedure

### 1. Execute One `v*` Tagged Release
Push a standard release tag (e.g., `v1.2.0`). The workflow runs in `audit` mode.

### 2. Inspect Harden Runner Audit Summaries
For each of the 9 jobs in the workflow run:
- Navigate to the GitHub Actions workflow run page.
- In the job's Markdown summary, click the "View Full Report" link (also printed in the harden-runner step log as an Insights link) to open StepSecurity Insights.
- In StepSecurity Insights, review the **Summary**, **Network Events**, and **Recommendations** sections to view observed endpoints (cite: https://docs.stepsecurity.io/harden-runner).
- Identify any endpoints flagged as unexpected or missing from the job's `allowed-endpoints`.

### 3. Reconcile Allowlist Deltas
- For any legitimate new endpoint (e.g., new CDN mirror for Alpine or Go proxy storage), update the job's allowlist in `.github/workflows/release.yaml`.
- For unexpected endpoints, investigate potential supply-chain or action drift.

### 4. Incremental Flip to `block` Mode

For publishing and signing jobs, jobs must be transitioned to `egress-policy: block` incrementally -- **one job per PR** -- to isolate issues and prevent partially failed releases.

However, the four **Phase 1 read-only jobs** (`release-preflight`, `test`, `changelog`, `security-scan`) were grouped into a single PR (for v0.4.1). Rationale: they are all read-only and every publishing job `needs:` them (or in `security-scan`'s case, it only publishes internal SARIF reports to GitHub Security and produces no external release artifacts), so a wrongly blocked host fails the release BEFORE anything is published — no partial or corrupted release is possible from this phase.

> [!CRITICAL]
> **Publishing and signing jobs must be flipped last.**
> As noted in #26 architecture reviews: image tags and digests pushed to GitHub Packages / GHCR cannot be revoked or rolled back. If a missing endpoint (e.g., Fulcio, Rekor, TUF CDN, or GHCR container layers) blocks a signing step after the image build-and-push step has already pushed to the registry, the release enters a corrupted partial state (unsigned images, missing release assets).

### Recommended Phasing Order

1. **Phase 1 (Read-only jobs)** (Transitioned to `block` in v0.4.1):
   - `release-preflight`: verifies tag and Chart.yaml version match.
   - `test`: standard Go test execution.
   - `changelog`: git history extraction with `git-cliff` and artifact upload.
   - `security-scan`: Trivy container scanning and SARIF upload.
2. **Phase 2 (Jobs that produce but do not publish)**:
   - Intermediate jobs that produce artifacts locally without publishing or pushing to external registries/remotes (none currently in `release.yaml`; reserved for future decoupled build/staging steps).
3. **Phase 3 (Publishing, uploading, and signing jobs)**:
   - Every job that pushes, uploads, signs, or publishes:
     - `build-and-push`: Buildx multi-arch compilation, GHCR push, keyless cosign signing.
     - `release-binaries`: Go cross-compilation, SBOM generation, checksum signing, GitHub release creation.
     - `helm-release`: Helm packaging, OCI registry push to GHCR, chart signing.
     - `release-manifest`: aggregates sha256 digests from upstream jobs, keyless signs `release-manifest.json`.
     - `release-bundle`: apt-get installation of `skopeo`, release asset downloads, GHCR image export, bundle creation and signing.
     - `update-changelog`: commit and push changelog to `main`.
   - **Rollout requirement**: Flipped last, **one PR each**, and only after Phases 1–2 have each survived one real tag release in block mode.

---

## Job Checklist & Inventory Table

| Job | Current Mode | Endpoints Count | Key Hosts / Destinations | Target Phase | Status |
| :--- | :---: | :---: | :--- | :---: | :---: |
| `release-preflight` | `block` | 4 | `github.com`, `api.github.com`, `raw.githubusercontent.com`, `objects.githubusercontent.com` | Phase 1 | block (since v0.4.1) |
| `test` | `block` | 11 | `github.com`, `api.github.com`, `raw/objects.githubusercontent.com`, `release-assets.githubusercontent.com`, `go.dev`, Go proxy/sumdb/storage, `results-receiver`, `*.blob.core.windows.net` | Phase 1 | block (since v0.4.1) |
| `changelog` | `block` | 7 | `github.com`, `api.github.com`, `raw/objects.githubusercontent.com`, `release-assets.githubusercontent.com`, `results-receiver`, `*.blob.core.windows.net` | Phase 1 | block (since v0.4.1) |
| `security-scan` | `block` | 10 | `github.com`, `api.github.com`, `raw/objects.githubusercontent.com`, `ghcr.io`, `pkg-containers.githubusercontent.com`, `mirror.gcr.io`, `check.trivy.dev`, `results-receiver`, `*.blob.core.windows.net` | Phase 1 | block (since v0.4.1) |
| `build-and-push` | `block` | 26 | GitHub APIs, `release-assets`, `*.actions.githubusercontent.com`, Docker Hub & CDNs, Alpine apk, Go proxy, Sigstore (incl. `timestamp`), GHCR, `*.blob.core.windows.net` | Phase 3 | block (pending verification on the next rc tag) |
| `release-binaries` | `block` | 19 | GitHub APIs/uploads, `release-assets`, `*.actions.githubusercontent.com`, `go.dev`, Go proxy/sumdb/storage, Sigstore (incl. `timestamp`), `*.blob.core.windows.net` | Phase 3 | block (pending verification on the next rc tag) |
| `helm-release` | `block` | 18 | GitHub APIs/uploads, `release-assets`, `*.actions.githubusercontent.com`, `get.helm.sh`, GHCR, Sigstore (incl. `timestamp`), `*.blob.core.windows.net` | Phase 3 | block (pending verification on the next rc tag) |
| `release-manifest` | `block` | 15 | GitHub APIs/uploads, `release-assets`, `*.actions.githubusercontent.com`, Sigstore (incl. `timestamp`), `*.blob.core.windows.net` | Phase 3 | block (pending verification on the next rc tag) |
| `release-bundle` | `block` | 24 | GitHub APIs/uploads, `release-assets`, `*.actions.githubusercontent.com`, Ubuntu apt mirrors (ports 80 & 443; archive, esm, motd, packages.microsoft.com), GHCR, Sigstore (incl. `timestamp`) | Phase 3 | block (pending verification on the next rc tag) |
| `update-changelog` | `block` | 5 | `github.com`, `api.github.com`, `raw/objects.githubusercontent.com`, `release-assets.githubusercontent.com` | Phase 3 | block (pending verification on the next rc tag) |

### Phase 1 Audit Evidence & Deltas (Evidence Run ID: 33769700751, v0.4.0)

Audit logs from the v0.4.0 release run ([33769700751](https://github.com/dasomel/nfs-quota-agent/actions/runs/33769700751)) were analyzed to reconcile allowed endpoints prior to enforcing `block` mode. All observed endpoints contacted port 443 (HTTPS):

1. **`release-preflight`** (databaseId `100696461422`):
   - Observed (1): `github.com` (port 443).
   - Observed-but-missing: None.
   - Declared-but-unused: `api.github.com`, `raw.githubusercontent.com`, `objects.githubusercontent.com`.
   - Reconciled count: 4 (kept defensive checkout endpoints).

2. **`test`** (databaseId `100696462321`):
   - Observed (5): `api.github.com`, `github.com`, `productionresultssa7.blob.core.windows.net` (actions/setup-go cache storage), `release-assets.githubusercontent.com`, `results-receiver.actions.githubusercontent.com`.
   - Observed-but-missing: `release-assets.githubusercontent.com`, `results-receiver.actions.githubusercontent.com`, `*.blob.core.windows.net` (all port 443, for actions/setup-go cache).
   - Declared-but-unused: `raw.githubusercontent.com`, `objects.githubusercontent.com`, `go.dev`, `proxy.golang.org`, `sum.golang.org`, `storage.googleapis.com`.
   - Reconciled count: 11.

3. **`changelog`** (databaseId `100696461145`):
   - Observed (5): `api.github.com`, `github.com`, `productionresultssa18.blob.core.windows.net`, `release-assets.githubusercontent.com`, `results-receiver.actions.githubusercontent.com`.
   - Observed-but-missing: `release-assets.githubusercontent.com` (port 443, git-cliff binary download by `orhun/git-cliff-action`).
   - Declared-but-unused: `raw.githubusercontent.com`, `objects.githubusercontent.com`.
   - Reconciled count: 7.

4. **`security-scan`** (databaseId `100697441268`):
   - Observed (8): `api.github.com`, `check.trivy.dev`, `ghcr.io`, `mirror.gcr.io`, `pkg-containers.githubusercontent.com`, `productionresultssa11.blob.core.windows.net`, `productionresultssa3.blob.core.windows.net`, `results-receiver.actions.githubusercontent.com`.
   - Observed-but-missing: `results-receiver.actions.githubusercontent.com`, `*.blob.core.windows.net` (all port 443, for `github/codeql-action/upload-sarif` artifact upload).
   - Declared-but-unused: `github.com`, `raw.githubusercontent.com`, `objects.githubusercontent.com`.
   - Reconciled count: 10.

### Phase 3 Audit Evidence & Deltas (Evidence Run ID: 33772050837, v0.4.1)

Audit logs from the v0.4.1 release run ([33772050837](https://github.com/dasomel/nfs-quota-agent/actions/runs/33772050837)) were analyzed to reconcile allowed endpoints across the six publishing and signing jobs before flipping them to `block` mode:

1. **`build-and-push`**:
   - Observed (26): `api.github.com`, `auth.docker.io`, `fulcio.sigstore.dev`, `ghcr.io`, `github.com`, `production.cloudfront.docker.com`, `productionresultssa1..19.blob.core.windows.net` (13 distinct accounts, matching `*.blob.core.windows.net`), `registry-1.docker.io`, `rekor.sigstore.dev`, `release-assets.githubusercontent.com`, `results-receiver.actions.githubusercontent.com`, `run-actions-2-azure-eastus.actions.githubusercontent.com`, `timestamp.sigstore.dev`, `tuf-repo-cdn.sigstore.dev` (all port 443).
   - Observed-but-missing: `release-assets.githubusercontent.com`, `timestamp.sigstore.dev`, and `run-actions-2-azure-eastus.actions.githubusercontent.com` (normalised to `*.actions.githubusercontent.com` port 443 per D2).
   - Declared-but-unused: `raw.githubusercontent.com`, `objects.githubusercontent.com`, `token.actions.githubusercontent.com`, `pkg-containers.githubusercontent.com`, `docker.io`, `index.docker.io`, `production.cloudflare.docker.com`, `dl-cdn.alpinelinux.org`, `proxy.golang.org`, `sum.golang.org`, `storage.googleapis.com`, `oauth2.sigstore.dev` (retained defensively for buildx caching, APK mirrors, and Go modules).
   - Reconciled count: 26.

2. **`release-binaries`**:
   - Observed (13): `api.github.com`, `fulcio.sigstore.dev`, `github.com`, `productionresultssa6..7.blob.core.windows.net`, `raw.githubusercontent.com`, `rekor.sigstore.dev`, `release-assets.githubusercontent.com`, `results-receiver.actions.githubusercontent.com`, `run-actions-2-azure-eastus.actions.githubusercontent.com`, `timestamp.sigstore.dev`, `tuf-repo-cdn.sigstore.dev`, `uploads.github.com` (all port 443).
   - Observed-but-missing: `release-assets.githubusercontent.com`, `timestamp.sigstore.dev`, `run-actions-2-azure-eastus.actions.githubusercontent.com` (normalised to `*.actions.githubusercontent.com` port 443 per D2).
   - Declared-but-unused: `objects.githubusercontent.com`, `token.actions.githubusercontent.com`, `go.dev`, `proxy.golang.org`, `sum.golang.org`, `storage.googleapis.com`, `oauth2.sigstore.dev`.
   - Reconciled count: 19.

3. **`helm-release`**:
   - Observed (13): `api.github.com`, `fulcio.sigstore.dev`, `get.helm.sh`, `ghcr.io`, `github.com`, `productionresultssa6.blob.core.windows.net`, `rekor.sigstore.dev`, `release-assets.githubusercontent.com`, `results-receiver.actions.githubusercontent.com`, `run-actions-2-azure-eastus.actions.githubusercontent.com`, `timestamp.sigstore.dev`, `tuf-repo-cdn.sigstore.dev`, `uploads.github.com` (all port 443).
   - Observed-but-missing: `release-assets.githubusercontent.com`, `timestamp.sigstore.dev`, `run-actions-2-azure-eastus.actions.githubusercontent.com` (normalised to `*.actions.githubusercontent.com` port 443 per D2). (`get.helm.sh` confirmed in audit and retained).
   - Declared-but-unused: `raw.githubusercontent.com`, `objects.githubusercontent.com`, `token.actions.githubusercontent.com`, `pkg-containers.githubusercontent.com`, `oauth2.sigstore.dev`.
   - Reconciled count: 18.

4. **`release-manifest`**:
   - Observed (11): `api.github.com`, `fulcio.sigstore.dev`, `github.com`, `productionresultssa6.blob.core.windows.net`, `rekor.sigstore.dev`, `release-assets.githubusercontent.com`, `results-receiver.actions.githubusercontent.com`, `run-actions-2-azure-eastus.actions.githubusercontent.com`, `timestamp.sigstore.dev`, `tuf-repo-cdn.sigstore.dev`, `uploads.github.com` (all port 443).
   - Observed-but-missing: `release-assets.githubusercontent.com`, `timestamp.sigstore.dev`, `run-actions-2-azure-eastus.actions.githubusercontent.com` (normalised to `*.actions.githubusercontent.com` port 443 per D2).
   - Declared-but-unused: `raw.githubusercontent.com`, `objects.githubusercontent.com`, `token.actions.githubusercontent.com`, `oauth2.sigstore.dev`.
   - Reconciled count: 15.

5. **`release-bundle`**:
   - Observed (16): `api.github.com` (port 443), `azure.archive.ubuntu.com` (port 80), `dl.google.com` (port 443; runner Chrome apt repo, excluded per D3), `esm.ubuntu.com` (port 443), `fulcio.sigstore.dev` (port 443), `ghcr.io` (port 443), `github.com` (port 443), `motd.ubuntu.com` (port 443), `packages.microsoft.com` (port 443), `pkg-containers.githubusercontent.com` (port 443), `rekor.sigstore.dev` (port 443), `release-assets.githubusercontent.com` (port 443), `run-actions-2-azure-eastus.actions.githubusercontent.com` (port 443; normalised to `*.actions.githubusercontent.com` per D2), `timestamp.sigstore.dev` (port 443), `tuf-repo-cdn.sigstore.dev` (port 443), `uploads.github.com` (port 443).
   - Observed-but-missing: `esm.ubuntu.com`, `motd.ubuntu.com`, `packages.microsoft.com`, `release-assets.githubusercontent.com`, `timestamp.sigstore.dev`, `run-actions-2-azure-eastus.actions.githubusercontent.com` (all port 443).
   - Normalisation: `dl.google.com` is explicitly NOT allowlisted. Instead, the runner's preinstalled Google Chrome apt sources are removed before `apt-get update` (matching `.github/workflows/e2e-airgap.yaml` pattern), eliminating unwanted third-party browser traffic.
   - Declared-but-unused: `raw.githubusercontent.com`, `objects.githubusercontent.com`, `token.actions.githubusercontent.com`, `archive.ubuntu.com` (ports 80 & 443), `security.ubuntu.com` (ports 80 & 443), `azure.archive.ubuntu.com` (port 443), `oauth2.sigstore.dev`.
   - Reconciled count: 24.

6. **`update-changelog`**:
   - Observed (3): `api.github.com`, `github.com`, `release-assets.githubusercontent.com` (all port 443).
   - Observed-but-missing: `release-assets.githubusercontent.com` (binary download by `orhun/git-cliff-action`).
   - Declared-but-unused: `raw.githubusercontent.com`, `objects.githubusercontent.com`.
   - Reconciled count: 5.

### Normalisation Decisions (D1–D3)

- **D1 (Azure Blob Wildcard)**: Use `*.blob.core.windows.net` (port 443) rather than pinning individual `productionresultssa<N>` accounts, as runner artifact and cache storage account names vary arbitrarily by region and run.
- **D2 (Actions Runner Backend Wildcard)**: Use `*.actions.githubusercontent.com` (port 443) and drop specific regional hostnames like `run-actions-2-azure-eastus.actions.githubusercontent.com` because runner backend assignments depend on dynamic runner pool routing.
- **D3 (Foreign APT Repository Removal)**: In `release-bundle`, remove Google Chrome apt source files (`sudo rm -f /etc/apt/sources.list.d/*chrome*`) prior to `apt-get update` instead of adding `dl.google.com` to `allowed-endpoints`, adhering to the principle of least privilege.

### RC Verification Procedure

Because Phase 3 involves irrevocable actions (pushing container tags to GHCR, publishing GitHub release assets), verifying the transition to `block` mode is decoupled from production releases via release candidate tags (e.g. `v0.4.2-rc1`):

1. **Preflight Guard**: `release-preflight` and `make release-preflight` support SemVer prereleases (`vX.Y.Z-rcN`) matching Chart.yaml `version: X.Y.Z-rcN` and `appVersion: "X.Y.Z-rcN"`.
2. **Release Marking**: All `softprops/action-gh-release` steps configure `prerelease: ${{ contains(github.ref_name, '-rc') }}` and `make_latest: ${{ contains(github.ref_name, '-rc') && 'false' || 'true' }}`, ensuring RC runs never become the repository's "Latest" release.
3. **Floating Tag Protection**: `docker/metadata-action` gates `latest`, `{{major}}.{{minor}}` (`0.4`), and `{{major}}` (`0`) on `!contains(github.ref_name, '-rc')`, guaranteeing that only the specific RC tag (`v0.4.2-rc1`, `0.4.2-rc1`) and sha tags are published.

## Wildcard exceptions (accepted, D4)

harden-runner's allowlist wildcards match by **suffix** (step-security/agent v0.16.3 `dnsproxy.go`), not by a single DNS label. Two wildcards are kept on purpose:

- `*.blob.core.windows.net:443` — the Actions artifact storage account name varies per run (`productionresultssa1..19` observed on run 33772050837).
- `*.actions.githubusercontent.com:443` — the Actions results host is region-specific (`run-actions-2-azure-eastus.actions.githubusercontent.com` observed on run 33772050837); enumerating it would fail releases scheduled onto another region.

Both admit deeper subdomains than intended. They are limited to GitHub- and Azure-operated infrastructure and were reviewed as an accepted exception (Codex critic review of PR #137). Revisit if harden-runner adds single-label wildcard semantics.
