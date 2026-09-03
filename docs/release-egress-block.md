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
| `build-and-push` | `audit` | 23 | GitHub APIs, Docker Hub & CloudFront/Cloudflare CDNs, Alpine apk mirror, Go proxy/storage, Sigstore, GHCR, GHA cache | Phase 3 | Allowlist configured (Audit) |
| `release-binaries` | `audit` | 16 | GitHub APIs/uploads, `raw/objects.githubusercontent.com`, `go.dev`, Go proxy/sumdb/storage, Sigstore (Fulcio, Rekor, TUF CDN, OAuth2), Actions artifacts | Phase 3 | Allowlist configured (Audit) |
| `helm-release` | `audit` | 15 | GitHub APIs/uploads, `raw/objects.githubusercontent.com`, `get.helm.sh`, GHCR, Sigstore (Fulcio, Rekor, TUF, OAuth2), Actions artifacts | Phase 3 | Allowlist configured (Audit) |
| `release-manifest` | `audit` | 12 | GitHub APIs/uploads, `raw/objects.githubusercontent.com`, Sigstore (Fulcio, Rekor, TUF, OAuth2), Actions artifact downloads | Phase 3 | Allowlist configured (Audit) |
| `release-bundle` | `audit` | 18 | GitHub APIs/uploads, `raw/objects.githubusercontent.com`, Ubuntu apt mirrors (ports 80 & 443), GHCR, Sigstore (Fulcio, Rekor, TUF, OAuth2) | Phase 3 | Allowlist configured (Audit) |
| `update-changelog` | `audit` | 4 | `github.com`, `api.github.com`, `raw.githubusercontent.com`, `objects.githubusercontent.com` | Phase 3 | Allowlist configured (Audit) |

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
