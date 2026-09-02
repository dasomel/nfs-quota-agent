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
- Open the StepSecurity Harden Runner summary tab.
- Check the **Outbound traffic** section.
- Identify any endpoints flagged as unexpected or missing from the job's `allowed-endpoints`.

### 3. Reconcile Allowlist Deltas
- For any legitimate new endpoint (e.g., new CDN mirror for Alpine or Go proxy storage), update the job's allowlist in `.github/workflows/release.yaml`.
- For unexpected endpoints, investigate potential supply-chain or action drift.

### 4. Incremental Flip to `block` Mode (One Job per PR)
Jobs must be transitioned to `egress-policy: block` incrementally -- **one job per PR** -- to isolate issues and prevent partially failed releases.

> [!CRITICAL]
> **Publishing and signing jobs must be flipped last.**
> As noted in #26 architecture reviews: image tags and digests pushed to GitHub Packages / GHCR cannot be revoked or rolled back. If a missing endpoint (e.g., Fulcio, Rekor, TUF CDN, or GHCR container layers) blocks a signing step after the image build-and-push step has already pushed to the registry, the release enters a corrupted partial state (unsigned images, missing release assets).

### Recommended Phasing Order
1. **Phase 1 (Low risk / non-publishing)**:
   - `test`: standard Go test execution.
   - `changelog`: git history extraction with `git-cliff` and artifact upload.
   - `update-changelog`: commit and push changelog to `main`.
2. **Phase 2 (Image scanning)**:
   - `security-scan`: Trivy container scanning and SARIF upload.
3. **Phase 3 (Binary compilation & signing)**:
   - `release-binaries`: Go cross-compilation, SBOM generation, checksum signing, GitHub release creation.
4. **Phase 4 (Container image publishing & signing)**:
   - `build-and-push`: Buildx multi-arch compilation, GHCR push, keyless cosign signing.
5. **Phase 5 (Helm chart packaging & OCI publish)**:
   - `helm-release`: Helm packaging, OCI registry push to GHCR, chart signing.
6. **Phase 6 (Manifest publishing)**:
   - `release-manifest`: aggregates sha256 digests from upstream jobs, keyless signs `release-manifest.json`.
7. **Phase 7 (Offline install bundle)**:
   - `release-bundle`: apt-get installation of `skopeo`, release asset downloads, GHCR image export, bundle creation and signing.

---

## Job Checklist & Inventory Table

| Job | Current Mode | Endpoints Count | Key Hosts / Destinations | Target Phase | Status |
| :--- | :---: | :---: | :--- | :---: | :---: |
| `test` | `audit` | 6 | `github.com`, `api.github.com`, `*.githubusercontent.com`, `proxy.golang.org`, `sum.golang.org`, `storage.googleapis.com` | Phase 1 | Allowlist configured (Audit) |
| `changelog` | `audit` | 5 | `github.com`, `api.github.com`, `*.githubusercontent.com`, `results-receiver.actions.githubusercontent.com`, `*.blob.core.windows.net` | Phase 1 | Allowlist configured (Audit) |
| `update-changelog` | `audit` | 3 | `github.com`, `api.github.com`, `*.githubusercontent.com` | Phase 1 | Allowlist configured (Audit) |
| `security-scan` | `audit` | 7 | `github.com`, `api.github.com`, `*.githubusercontent.com`, `ghcr.io`, `pkg-containers.githubusercontent.com`, `mirror.gcr.io`, `check.trivy.dev` | Phase 2 | Allowlist configured (Audit) |
| `release-binaries` | `audit` | 14 | GitHub APIs/uploads, Go proxy/sumdb/storage, Sigstore (Fulcio, Rekor, TUF CDN, OAuth2), Actions artifacts | Phase 3 | Allowlist configured (Audit) |
| `build-and-push` | `audit` | 22 | GitHub APIs, Docker Hub & CloudFront/Cloudflare CDNs, Alpine apk mirror, Go proxy/storage, Sigstore, GHCR, GHA cache | Phase 4 | Allowlist configured (Audit) |
| `helm-release` | `audit` | 14 | GitHub APIs/uploads, `get.helm.sh`, GHCR, Sigstore (Fulcio, Rekor, TUF, OAuth2), Actions artifacts | Phase 5 | Allowlist configured (Audit) |
| `release-manifest` | `audit` | 11 | GitHub APIs/uploads, Sigstore (Fulcio, Rekor, TUF, OAuth2), Actions artifact downloads | Phase 6 | Allowlist configured (Audit) |
| `release-bundle` | `audit` | 17 | GitHub APIs/uploads, Ubuntu apt mirrors (ports 80 & 443), GHCR, Sigstore (Fulcio, Rekor, TUF, OAuth2) | Phase 7 | Allowlist configured (Audit) |
