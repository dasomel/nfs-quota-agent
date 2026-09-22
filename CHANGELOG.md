# Changelog

All notable changes to this project will be documented in this file.

## [0.5.0] - 2026-09-22

### Bug Fixes

- **chart:** Correct dashboard panel metric attribution and node label by @dasomel
- **quota:** Resolve name-keyed repquota -P rows for ext4 read-back verification (#149) by @dasomel
- **quota:** Reject repquota-sentinel project names and document projid mismatch hazard (#149) by @dasomel
- **e2e:** Run Stage D's primary writer non-root; document ext4's root quota bypass (#149) by @dasomel
- **e2e:** Fail closed when the root writer's exit code is missing; derive limits from ExpectedEnforcedBytes (#149) by @dasomel
- **e2e:** Treat "less than one write block remains" as at-hard-limit for ENOSPC (#149) by @dasomel
- **events:** Drop deprecated EventBroadcasterAdapter and fix E2E Event query (#152) by @dasomel
- **events:** Emit PolicyClamped only on mutation, size dedupe window above the sync tick, evict per-PV dedupe state (#152) by @dasomel
- **events:** Forget pruned/pathless PVs, dedup on message, cover real recorder RBAC by @dasomel
- **ci:** Never serve the runtime stage's apk upgrade from the BuildKit cache by @dasomel
- **agent:** Hydrate trace before evidence correlation by @dasomel
- **agent:** Record the replay date in UTC, not the author's local date by @dasomel
- **agent:** Parse skill front matter the way the auditor does by @dasomel
- **ci:** Keep the egress note four lines so doc citations do not move by @dasomel
- Harden file permissions and HTTP server timeouts flagged by gosec by @dasomel
- **ci:** Tolerate already-deleted refs in merged-branch cleanup by @dasomel
- **agent:** Keep change workflow outside skill registry by @dasomel
- **agent:** Avoid unverified portable skill registration by @dasomel

### Build

- **image:** Apk upgrade the runtime base packages so security fixes from the Alpine index reach the image (#150) by @dasomel
- Bump golang.org/x/time in the go-dependencies group by @dependabot[bot]

### CI/CD

- **security:** Scan the built image with Trivy and add the OpenSSF Scorecard workflow (#150) by @dasomel
- **scorecard:** Allow the Sigstore hosts the publish step signs results with (#150) by @dasomel
- **e2e:** Add ext4 and btrfs real-kernel quota enforcement to the Air-Gap E2E matrix (#149) by @dasomel
- **e2e:** Anchor the ext4 repquota project selector and keep ext4/btrfs matrix rows pending until CI proves them (#149) by @dasomel
- Retrigger workflows for c833020 (no Actions runs were created for the last two pushes) by @dasomel
- **e2e:** Use a decimal 100M PV capacity so the KB-flooring assertion is real (#149) by @dasomel
- **security:** Default workflows to a read-only token and bump x/mod for CVE-2026-56864/56865 by @dasomel
- Bump docker/setup-qemu-action from 4.2.0 to 4.3.0 by @dependabot[bot]
- Bump orhun/git-cliff-action from 4.8.0 to 4.9.0 by @dependabot[bot]
- Add OpenForge status publisher by @dasomel
- **agent:** Record status publisher trace by @dasomel
- **agent:** Align publisher trace with behavior gate by @dasomel
- **agent:** Canary OpenForge reusable gate by @dasomel
- Bump github/codeql-action/upload-sarif from 4.37.9 to 4.38.0 by @dependabot[bot]
- Add CodeQL analysis, gosec lint, and weekly E2E schedule by @dasomel
- Scope gosec exclusions per file and harden CodeQL egress by @dasomel
- Bump docker/build-push-action from 7.3.0 to 7.4.0 by @dependabot[bot]
- Bump codecov/codecov-action from 7.0.0 to 7.1.1 by @dependabot[bot]
- Bump github/codeql-action/analyze from 4.38.0 to 4.38.1 by @dependabot[bot]
- Bump github/codeql-action/autobuild from 4.38.0 to 4.38.1 by @dependabot[bot]
- Bump github/codeql-action/init from 4.38.0 to 4.38.1 by @dependabot[bot]

### Documentation

- Update CHANGELOG.md for v0.4.3 by @github-actions[bot]
- **adr:** Options and recommendation for Kubernetes Events and retry metrics (#152) by @dasomel
- **adr:** Fix reconcileResult citation and document C's namespace-default RBAC gap (#152) by @dasomel
- **adr:** Accept option D for Kubernetes Events and retry metrics per maintainer decision (#152) by @dasomel
- **readme:** Document the reserved nfs.io/project-name shapes rejected since #149 by @dasomel
- **skills:** Mirror the verification skill into .agents/skills for Codex and point it at AGENTS.md by @dasomel
- **security:** Define reduced OpenForge execution profile by @dasomel
- **agent:** Route Claude to canonical verification skill by @dasomel
- **agent:** Narrow verification skill activation by @dasomel
- **agent:** Make quota guidance task-relevant and risk-scoped by @dasomel
- Add research evidence collection guidance by @dasomel
- Catalog legacy evidence during development by @dasomel
- Refresh architecture views by @dasomel
- Audit and refresh project documentation by @dasomel
- **cncf:** Refresh the #81 readiness evidence against 60aa69a by @dasomel
- **cncf:** Refresh ci.yaml citations after #175 landed by @dasomel
- Document missing CLI flags and retire stale projectsFile gotcha by @dasomel
- Declare Beta project status in README by @dasomel
- **research:** Link AGENTS.md to the evidence standard, seed first batch by @dasomel
- **readiness:** Update retry_metrics.go citation range after array refactor by @dasomel
- **agent:** Document Claude adapter retention by @dasomel
- **agent:** Add risk-scaled change package workflow by @dasomel
- **change:** Add short-lived change package templates by @dasomel
- **agent:** Require risk-scaled change packages by @dasomel

### Features

- **chart:** Ship a Grafana dashboard and a metric-name lint for it (#151) by @dasomel
- **agent:** Emit events.k8s.io/v1 Events and retry metrics behind events.enabled (#152) by @dasomel
- **agent:** Add canonical nfs quota verification skill by @dasomel
- **agent:** Gate the verified skill claim on fresh-session evidence by @dasomel
- **chart:** Add opt-in NetworkPolicy template by @dasomel

### Miscellaneous

- **license:** Regenerate THIRD_PARTY_LICENSES.md after x/net and x/text bump by @dasomel
- **agent:** Require fresh replay before verified by @dasomel
- **agent:** Adopt OpenForge validation standards by @dasomel
- **ci:** Run the CI governance self-tests and add a verify entry point by @dasomel
- **ci:** Clean merged branches by @dasomel
- **ci:** Record branch cleanup behavior evidence by @dasomel
- **ci:** Bind typed branch-hygiene evidence by @dasomel
- **ci:** Sweep safely proven merged branches by @dasomel
- Regenerate THIRD_PARTY_LICENSES.md for dependency update by @github-actions[bot]
- **agent:** Record cleanup-branch fix as a bug-fix trace by @dasomel
- **agent:** Fix duplicate trace event id by @dasomel

### Refactoring

- **agent:** Turn legacy verification skill into adapter by @dasomel
- Consolidate duplicated parsing, dedup and histogram code since v0.4.3 by @dasomel
- **agent,events:** Apply deferred review findings; emit rejection Events at the guard sites by @dasomel

### Testing

- **agent:** Add operational trace for the events/retry-metrics change (#152) by @dasomel
- **agent:** Bind reusable gate rollout evidence by @dasomel
- **agent:** Add skill canonicalization trace by @dasomel
- **agent:** Record maturity evidence alignment by @dasomel

## [0.4.3] - 2026-09-04

### Documentation

- Update CHANGELOG.md for v0.4.3-rc2 by @github-actions[bot]

### Release

- **chart:** Align Chart.yaml version and appVersion to 0.4.3 for the stable release by @dasomel

## [0.4.3-rc2] - 2026-09-04

### Bug Fixes

- **ci:** Hash the raw index in the docker fallback of verify-published-digests instead of piping into an early-exiting awk (#5) by @dasomel

### Documentation

- Update CHANGELOG.md for v0.4.3-rc1 by @github-actions[bot]

### Release

- **chart:** Align Chart.yaml version and appVersion to 0.4.3-rc2 for the second rc verification tag by @dasomel

## [0.4.3-rc1] - 2026-09-04

### Bug Fixes

- **e2e:** Drop fsid=0 pseudo-root export breaking the NFSv4 wire path (#5) by @dasomel

### CI/CD

- **e2e:** Route the Stage D writer through the NFS wire path (#5) by @dasomel
- **release:** Verify published image digests against release manifest by @dasomel
- **release:** Classify rc tags by the pipeline's -rc rule and document post-publish failure handling (#5) by @dasomel

### Documentation

- Update CHANGELOG.md for v0.4.2 by @github-actions[bot]
- **adr:** Record QuotaPolicy admission options by @dasomel
- **adr:** Tighten webhook failure and trust boundaries by @dasomel
- **adr:** Name the bind-to-ensureQuota window as a limit of reconcile-time clamping (#132) by @dasomel
- **adr:** Accept option A (no admission webhook) per maintainer decision (#132) by @dasomel
- **readiness:** Refresh #81 evidence for v0.4.2, closed #26 and merged E2E (#81) by @dasomel
- **readiness:** Fix Cosign citation, false E2E trigger claim, and pasted-not-paraphrased run record (#81) by @dasomel
- **readiness:** Record real-NFS E2E evidence ahead of PR #142 merge (#81) by @dasomel
- **readiness:** Cite merged PR #142 for the NFS wire path E2E (#81) by @dasomel

### Testing

- **e2e:** Add behavior-contract trace for the NFS wire path fix (#5) by @dasomel

### Release

- **chart:** Align Chart.yaml version and appVersion to 0.4.3-rc1 for the rc verification tag by @dasomel

## [0.4.2] - 2026-09-03

### Documentation

- Update CHANGELOG.md for v0.4.2-rc1 by @github-actions[bot]
- **release:** Record that all release jobs run in egress block mode, verified on v0.4.1 and v0.4.2-rc1 (#26) by @dasomel

### Release

- **chart:** Align Chart.yaml version and appVersion to 0.4.2 for the stable release by @dasomel

## [0.4.2-rc1] - 2026-09-03

### CI/CD

- **release:** Block egress for the publishing and signing jobs using v0.4.1 audit evidence (#26) by @dasomel
- **release:** Keep rc tags off latest and mark rc releases as pre-release (#26) by @dasomel

### Documentation

- Update CHANGELOG.md for v0.4.1 by @github-actions[bot]
- **release:** Document Phase 3 egress block and rc verification policy (#26) by @dasomel
- **release:** Document the suffix-match wildcard exceptions as an accepted decision (#26) by @dasomel

### Release

- **chart:** Align Chart.yaml version and appVersion to 0.4.2-rc1 for the rc verification tag by @dasomel

## [0.4.1] - 2026-09-03

### Bug Fixes

- **release:** Log cosign in to GHCR before signing the Helm chart OCI artifact (#26) by @dasomel

### CI/CD

- **release:** Block egress for the read-only release jobs using v0.4.0 audit evidence (#26) by @dasomel

### Documentation

- Update CHANGELOG.md for v0.4.0 by @github-actions[bot]
- **release:** Document Phase 1 egress block rationale and add audit trace (#26) by @dasomel

### Release

- **chart:** Align Chart.yaml version and appVersion to 0.4.1 for the v0.4.1 tag by @dasomel

## [0.4.0] - 2026-09-03

### Bug Fixes

- **ci:** Modernize golangci-lint for Go 1.26 by @dasomel
- **lint:** Resolve golangci-lint v2 findings and add lint configuration by @dasomel
- **cleanup:** Make orphan detection CSI-aware and fail-closed by @dasomel
- **license:** Harden license tooling against silent failure, add GPL-2.0 written offer by @dasomel
- **chart:** Enforce single instance, drop anti-affinity and unused RBAC by @dasomel
- **agent:** Keep the applied-quota cache consistent with what is on disk by @dasomel
- **quota:** Fsync host project files and make a mid-rewrite crash recoverable by @dasomel
- **quota:** Reject conflicting and malformed project identities by @dasomel
- **lint:** Use ​ escape instead of a literal zero-width space (ST1018) by @dasomel
- **lint:** Drop unnecessary blank-identifier assignment (staticcheck S1005) by @dasomel
- **agent:** Remove --default-quota / --enforce-max-quota, dead since inception by @dasomel
- **docs:** Correct remaining references missed by the dead-flag removal by @dasomel
- **audit:** Log project ID allocation failures, not just apply failures by @dasomel
- **quota:** Reject a path claimed under a second project ID by @dasomel
- **agent:** Stop discarding orphan metadata-cleanup errors by @dasomel
- **quotapolicy:** Refuse a QuotaPolicy quota decrease below current usage by @dasomel
- **status:** Thread projectsFile/projidFile through GetDirUsages by @dasomel
- **agent:** Close shrink guard bypass on process restart by @dasomel
- **quota:** ValidateQuotaArg two more argv-bound paths an independent review caught by @dasomel
- **release:** Harden verify-release.py against a crafted manifest by @dasomel
- **quota:** ValidateQuotaArg the remaining unvalidated argv paths in the package by @dasomel
- **supply-chain:** Detect latest image tags correctly by @dasomel
- **agent:** Ignore unrelated historical traces by @dasomel
- **runtime:** Ship btrfs tooling required by backend by @dasomel
- **legal:** Correct btrfs-progs license to GPL-2.0-or-later by @dasomel
- **docs:** Point adoption guide to the doc that actually exists by @dasomel
- **ci:** Exempt dependabot action-pin-only bumps from the trace gate by @dasomel
- **agent:** Close three unsafe-shrink guard bypasses (#90) by @dasomel
- **ui:** Keyboard-operable expandable rows, alert role, invalid-end test by @dasomel
- **ui:** Require path on /api/history and add /api/history/stats (#96) by @dasomel
- **status:** Add allocated-size directory walk for the brownfield snapshot (#94) by @dasomel
- **agent:** Retry the brownfield prime until it succeeds and expose its state (#93) by @dasomel
- **agent:** Memoize usage-report absence per sync pass and rate-limit rejection logs (#92) by @dasomel
- **agent:** Keep shrink guard unprimed on mapping read failure by @dasomel
- **release:** Add --require-signatures to verify-release.py so skipped signatures fail (#26) by @dasomel
- **release:** Require every expected signature entry under --require-signatures (#26) by @dasomel
- **release:** Reject unsupported schema keywords in the compat-matrix validator (#5) by @dasomel
- **agents:** Use typed evidence prefixes in the gomod trace by @dasomel
- **dependabot:** Address security review findings on license regen workflow by @dasomel
- **dependabot:** Allow artifact-service egress and observe it on this PR by @dasomel
- **audit:** Address review findings on correlation/enforced-bytes PR by @dasomel
- **ci:** Require Dockerfile repository match in dependabot pin-only exemption by @dasomel
- **release:** Address independent review CRITICALs on offline bundle (#5) by @dasomel
- **release:** Address Codex critic + opus findings on offline bundle (#5) by @dasomel
- **release:** Address Codex final verification on offline bundle (#5) by @dasomel
- **release:** Decision D on manifest auto-discovery visibility (#5) by @dasomel
- **release:** Fail closed on incomplete v4 provenance, verify source commit/tree by @dasomel
- **release:** Reject concatenated JSON documents in provenance-meta.json by @dasomel
- **release:** Use CI-recognized evidence-reference prefixes in the Go 1.27 trace by @dasomel
- **quotapolicy:** Report LimitRange minimum conflicts in the LimitRangeConflict condition (#14) by @dasomel
- **quotapolicy:** Aggregate every LimitRange for conflict detection and state the real effect (#14) by @dasomel
- **quotapolicy:** Finish StorageClass binding review items (#14) by @dasomel
- **agent:** Refresh policy decision annotations on cache hits and harden the status signature (#14) by @dasomel
- **agent:** Record policy decisions only after the PV status write succeeds (#14) by @dasomel

### Build

- **docker:** Pin base images to digests, add dependabot docker ecosystem by @dasomel
- Bump alpine from 3.21 to 3.24 by @dependabot[bot]
- Bump golang from 1.26-alpine to 1.27-alpine by @dependabot[bot]
- **docker:** Align builder to Go 1.26 and pin apk packages (#26) by @dasomel
- **docker:** Pin the full apk closure in both Dockerfile stages (#26) by @dasomel
- **docker:** Refresh stale builder pcre2 pin by @dasomel
- **docker:** Record the apk closure instead of pinning it (#26) by @dasomel
- **docker:** Cross-compile natively instead of emulating the Go toolchain (#26) by @dasomel
- Bump golang from `b6890e3` to `ce864e7` by @dependabot[bot]
- **deps:** Bump the k8s.io module set to v0.36.4 and resolve the genproto split by @dasomel
- **go:** Move the toolchain to Go 1.27 everywhere it is pinned (supersedes #112) by @dasomel

### CI/CD

- Pin GitHub Actions to commit SHAs, add dependabot for action updates by @dasomel
- Bump docker/build-push-action from 6.19.2 to 7.3.0 by @dependabot[bot]
- Bump actions/checkout from 4.4.0 to 7.0.1 by @dependabot[bot]
- Bump actions/setup-go from 5.6.0 to 7.0.0 by @dependabot[bot]
- Bump codecov/codecov-action from 4.6.0 to 7.0.0 by @dependabot[bot]
- Bump actions/upload-artifact from 4.6.2 to 7.0.1 by @dependabot[bot]
- **license:** Enforce an explicit dependency license allowlist by @dasomel
- Bump azure/setup-helm from 4.3.1 to 5.0.1 by @dependabot[bot]
- Bump softprops/action-gh-release from 2.6.2 to 3.0.2 by @dependabot[bot]
- Bump actions/download-artifact from 4.3.0 to 8.0.1 by @dependabot[bot]
- Bump github/codeql-action/upload-sarif from 3.37.8 to 4.37.8 by @dependabot[bot]
- Bump docker/login-action from 3.7.0 to 4.6.0 by @dependabot[bot]
- Bump docker/setup-buildx-action from 3.12.0 to 4.3.0 by @dependabot[bot]
- Bump aquasecurity/trivy-action from 0.35.0 to 0.36.0 by @dependabot[bot]
- Bump docker/metadata-action from 5.10.0 to 6.2.0 by @dependabot[bot]
- Bump docker/setup-qemu-action from 3.7.0 to 4.2.0 by @dependabot[bot]
- **security:** Egress auditing on every job, pin floating Helm version by @dasomel
- **release:** Publish chart as a release asset and a signed-source release manifest by @dasomel
- **agent:** Gate operational traces against trusted baseline by @dasomel
- **supply-chain:** Gate mutable release inputs by @dasomel
- **agent:** Require traces for high-risk quota-agent changes by @dasomel
- **agent:** Correlate high-risk changes with trace evidence by @dasomel
- **agent:** Bind live filesystem runtime evidence by @dasomel
- Bump anchore/sbom-action from 0.24.0 to 0.24.2 by @dependabot[bot]
- Bump github/codeql-action/upload-sarif from 4.37.8 to 4.37.9 by @dependabot[bot]
- **license:** Fail an egress-blocked license-URL lookup as such, not as stale (#95) by @dasomel
- **license:** Validate GO_LICENSES_ATTEMPTS and test each discovery signal alone by @dasomel
- **build:** Gate the Dockerfile build on every PR before release (#26) by @dasomel
- **build:** Pin QEMU image and allow Docker Hub CDN by @dasomel
- **build:** Pin the buildx buildkit image to a digest (#26) by @dasomel
- **security:** Add positive-control negative egress check (#26) by @dasomel
- **egress:** Retry the positive control, never the negative one by @dasomel
- **dependabot:** Enable gomod ecosystem with license regen automation by @dasomel
- Bump softprops/action-gh-release from 3.0.2 to 3.0.3 by @dependabot[bot]
- Bump actions/dependency-review-action from 4.9.0 to 5.0.0 by @dependabot[bot]
- Bump step-security/harden-runner from 2.21.0 to 2.21.1 by @dependabot[bot]
- **dependabot:** Ignore uncoordinated k8s/docker bumps, exempt Dockerfile pin-only PRs by @dasomel
- **release:** Declare per-job egress allowlists ahead of the audit→block flip (#26) by @dasomel
- **image:** Report apk closure drift against a committed baseline (#26 D5) by @dasomel
- **image:** Read the apk closure from the same buildx output the job publishes (#26) by @dasomel
- **release:** Guard chart metadata before publishing by @dasomel
- **release:** Pin and log skopeo candidate by @dasomel
- **release:** Record preflight guard evidence by @dasomel
- **docs:** Add script and tests to check doc citations by @dasomel
- **docs:** Gate readiness doc citations and record the trace (#81) by @dasomel

### Documentation

- Update CHANGELOG.md for v0.3.0 by @github-actions[bot]
- Add Rust adoption assessment by @dasomel
- Describe Rust adoption candidate by @dasomel
- Capture Rust adoption scope by @dasomel
- Add temporary backlog review note by @dasomel
- Remove temporary backlog review note by @dasomel
- Temporary by @dasomel
- Replace AGENT.md with CLAUDE.md and refresh agent context by @dasomel
- Trim CLAUDE.md to non-derivable gotchas, extract verification skill by @dasomel
- **api:** Design the QuotaPolicy custom resource and define its types by @dasomel
- **contributing:** Add make target <-> CI job mapping table by @dasomel
- **chart:** Document the DaemonSet PDB scale-subresource limitation (#4) by @dasomel
- **security:** Dependency incident-response procedure + explicit go mod verify (#26) by @dasomel
- **claude:** Refresh CLAUDE.md gotchas from this session's findings by @dasomel
- **readme:** Add QuotaPolicy usage examples by @dasomel
- Add cross-agent engineering contract by @dasomel
- Add current implementation status snapshot by @dasomel
- **agent:** Correlate maintenance trace with changed paths by @dasomel
- **agent:** Mark maintenance trace strict and verified by @dasomel
- **compat:** Record repaired btrfs runtime dependency by @dasomel
- **legal:** Cover btrfs runtime package and pinned Alpine base by @dasomel
- Add quota first-success journey by @dasomel
- **readme:** Fix broken Korean README link by @dasomel
- **governance:** Add GOVERNANCE.md, MAINTAINERS.md, and a DCO convention by @dasomel
- **claude-md:** Correct btrfs gotcha, add ext4 kernel-module gotcha by @dasomel
- Add CNCF Sandbox readiness draft (#81) by @dasomel
- **chart:** Clarify ha.activeFile must be the container-side path by @dasomel
- **governance:** Add CODEOWNERS and refresh CNCF readiness draft (#81) by @dasomel
- **quotapolicy:** Document deterministic precedence with ResourceQuota/LimitRange/StorageClass/resize (#14) by @dasomel
- **trace:** State the exact helm-template diff for #110 by @dasomel
- **agent:** State slog's actual rendering of an empty correlation_id by @dasomel
- **dependabot:** State that ignore also suppresses security PRs by @dasomel
- **dependabot:** State the real coordinated k8s bump procedure by @dasomel
- Refresh implementation status and operator success guidance (rebased from docs/adoption-refresh) by @dasomel
- **release:** Order the egress flip by blast radius and fix allowlist gaps (#26) by @dasomel
- **trace:** Record a reproducible deliberate-breakage for watch-path provenance (#14) by @dasomel
- **release:** Fix air-gap verification command by @dasomel
- **quotapolicy:** Document StorageClass selector safety (#14) by @dasomel
- **trace:** Record the passing air-gap E2E run (#5) by @dasomel
- **trace:** Record Opus review fixes and deliberate breakage verification (#14) by @dasomel
- **quotapolicy:** Document admission-time correlation ID as open (#14) by @dasomel
- **cncf:** Evidence-backed readiness status and periodic review checklist (#81) by @dasomel
- **cncf:** Replace nonexistent PR #4 citations and mark pending E2E rows (#81) by @dasomel
- **cncf:** Correct readiness evidence (#81) by @dasomel
- **cncf:** Fix readiness table lint (#81) by @dasomel
- **cncf:** Correct issue-number evidence (#81) by @dasomel
- **cncf:** Mark NFS E2E evidence pending (#81) by @dasomel
- **cncf:** Cite privilege analysis source (#81) by @dasomel
- **cncf:** Add vulnerability check install step (#81) by @dasomel
- **cncf:** Make every citation verifiable (#81) by @dasomel
- **cncf:** Sync counts after citation pass (#81) by @dasomel
- **cncf:** Relevance pass rows 1–15 (#81) by @dasomel
- **cncf:** Relevance pass rows 16–30 (#81) by @dasomel
- **cncf:** Relevance pass rows 31–45 (#81) by @dasomel
- **cncf:** Relevance pass rows 46–55 (#81) by @dasomel
- **cncf:** Relevance pass rows 56–63 (#81) by @dasomel
- **cncf:** Complete evidence relevance pass (#81) by @dasomel

### Features

- **production:** Real health semantics, probes, HA defaults, drop hostPID by @dasomel
- **chart:** Run as a DaemonSet, reject surge upgrades and empty nodeSelector by @dasomel
- **api:** Generate QuotaPolicy deepcopy and CRD, ship it in the chart by @dasomel
- **metrics:** Add a critical PrometheusRule alert for quota exhaustion by @dasomel
- **quotapolicy:** Reconcile QuotaPolicy CRD into filesystem quota enforcement by @dasomel
- **agent:** ResourceVersion-aware List-then-Watch for the PV watch loop by @dasomel
- **agent:** Per-PV reconcile work queue with bounded workers, metrics, drain by @dasomel
- **agent:** HA active/standby quota mutation gate by @dasomel
- **ui:** Read-only REST facade for QuotaPolicy (/api/quota-policies) by @dasomel
- **agent:** Expose enforced quota limit distinctly from requested capacity by @dasomel
- **release:** Add machine-readable filesystem/arch/k8s-version compatibility matrix by @dasomel
- **quota:** Read back and verify filesystem quota state after apply (#10) by @dasomel
- **quotapolicy:** Implement Drifted condition via independent read-back by @dasomel
- **release:** Offline-verify a downloaded release bundle against release-manifest.json by @dasomel
- **agent:** Adopt OpenForge behavior and trace evals by @dasomel
- **agent:** Align evaluator with canonical OpenForge schema by @dasomel
- **agent:** Align reference trace with canonical schema by @dasomel
- **agent:** Add incremental trace recorder by @dasomel
- **agent:** Add trusted baseline regression gate by @dasomel
- **agent:** Add trusted behavior baseline by @dasomel
- **supply-chain:** Add mutable input regression guard by @dasomel
- **agent:** Define nfs-quota-agent high-risk trace policy by @dasomel
- **agent:** Add risk-based trace requirement gate by @dasomel
- **agent:** Add trace evidence correlation gate by @dasomel
- **agent:** Enforce strict outcome evidence consistency by @dasomel
- **agent:** Require strict passed evidence for high-risk traces by @dasomel
- **agent:** Add live verification binder by @dasomel
- **api:** Register QuotaPolicy GroupVersion/SchemeBuilder (#13) by @dasomel
- **ui:** Declare OpenForge archetype and close design-system a11y gaps by @dasomel
- **ui:** Custom trends range, QuotaPolicy tab, HA standby badge, contrast fixes by @dasomel
- **release:** Add JSON Schema and validator for the compatibility matrix (#5) by @dasomel
- **chart:** Pin the agent image by digest for air-gap installs (#5) by @dasomel
- **audit:** Add correlation ID, enforced-bytes, and policy provenance by @dasomel
- **release:** Add offline air-gap install bundle (part of #5) by @dasomel
- **release:** Bind build-input provenance into release-manifest.json (schemaVersion 4) by @dasomel
- **agent:** Record QuotaPolicy provenance on watch-path audit entries (#14) by @dasomel
- **quotapolicy:** Add storageClassNames selector and regenerate CRD (#14) by @dasomel
- **quotapolicy:** Match PVs by StorageClass and fail closed on path fallback (#14) by @dasomel
- **agent:** Stable QuotaPolicy decision IDs on PV annotations and audit (#14) by @dasomel

### Miscellaneous

- Remove temporary CI note by @dasomel
- **license:** Generate third-party licenses, SBOM, and GPL compliance notices by @dasomel
- **license:** Regenerate third-party inventory after the controller-gen tool by @dasomel
- **chart:** Reconcile appVersion with the last actual release tag by @dasomel
- Retrigger CI after GitHub Actions outage by @dasomel
- Retrigger CI now that Actions is operational by @dasomel
- **openforge:** Standardize Korean documentation, adopt security policy, and add GitHub templates by @dasomel
- **agent:** Protect CI governance scripts as high risk by @dasomel
- **agents:** Add operational trace for the shrink-guard fix (#90) by @dasomel
- **agents:** Add operational trace for the OpenForge design-system PR (#97) by @dasomel
- **agents:** Add operational trace for the /api/history path fix (#96) by @dasomel
- **agents:** Add operational trace for the flaky reconcile-queue test fix (#103) by @dasomel
- **agents:** Add operational trace for the brownfield-guard follow-ups (#92 #93 #94) by @dasomel
- **agents:** Record the mapping-file and race fixes in the brownfield-guard trace (#92 #93 #94) by @dasomel
- **agents:** Add operational trace for verify-release strict mode (#26) by @dasomel
- **agents:** Add operational trace for the License Check egress fix (#95) by @dasomel
- **agents:** Add operational trace for the compat-matrix validator (#5) by @dasomel
- **agents:** Record the validation gap as a bug fix in the compat-matrix trace (#5) by @dasomel
- **agents:** Add operational trace for the toolchain/apk pin change (#26) by @dasomel
- **agents:** Fold the bug-fix triad into the toolchain/apk trace (#26) by @dasomel
- **agents:** Add real CI evidence for the negative-egress positive control by @dasomel
- **agents:** Record the exact CI run confirming the retry change by @dasomel
- **evals:** Trace StorageClass binding safeguards (#14) by @dasomel

### Refactoring

- **agent:** Dedup expectedEnforcedBytes into quota.ExpectedEnforcedBytes by @dasomel

### Testing

- **quota:** Add a native fuzz test for validateQuotaArg by @dasomel
- **quota:** Add fuzz coverage for the btrfs qgroup show output parser by @dasomel
- **agent:** Verify a Kubernetes API list failure never manufactures orphans by @dasomel
- **agent:** Verify a process restart converges drifted quota to desired state by @dasomel
- **quota:** Fuzz the xfs/ext4 quota report parsers by @dasomel
- **agent:** Add first operational pilot trace by @dasomel
- **agent:** Record mutable input guard maintenance trace by @dasomel
- **runtime:** Verify filesystem tools in shipped image by @dasomel
- **agent:** Add filesystem runtime regression trace by @dasomel
- **agent:** Make the delete-after-in-flight reconcile test condition-based (#103) by @dasomel
- **quotapolicy:** Cover StorageClass binding resolution and fallback rejection (#14) by @dasomel
- **e2e:** Air-gap install, quota enforcement, upgrade and rollback on XFS prjquota (#5) by @dasomel
- **e2e:** Pin kind by its published checksum and harden host setup (#5) by @dasomel
- **e2e:** Resolve kind docker bridge IPv4 gateway and export subnet (#5) by @dasomel
- **e2e:** Fix oci untar permissions, image tag reference, and quota assertions (#5) by @dasomel
- **e2e:** Match resolved project name in host xfs_quota report (#5) by @dasomel
- **e2e:** Capture writer diagnostics and accept ENOSPC at XFS quota limit (#5) by @dasomel
- **e2e:** Record successful air-gap quota run (#5) by @dasomel
- **e2e:** Bind quota proof to resolved project (#5) by @dasomel
- **e2e:** Pin loaded image digest when available (#5) by @dasomel
- **e2e:** Enforce zero egress during air-gap stages (#5) by @dasomel
- **e2e:** State hostpath coverage boundary (#5) by @dasomel
- **e2e:** Preload test images and scope the zero-egress assertion to the air-gap window (#5) by @dasomel
- **e2e:** Bind the quota proof to the directory's project id via lsattr (#5) by @dasomel
- **agent:** Verify policy decision cache-hit refresh, unchanged no-op, and nil attempt preservation (#14) by @dasomel

### Release

- **chart:** Align Chart.yaml version and appVersion to 0.4.0 for the v0.4.0 tag by @dasomel

### Security

- **ci:** Switch harden-runner from audit to block with per-job allowlist by @dasomel
- **ci:** Review dependency changes in pull requests by @dasomel
- **release:** Sign release artifacts with cosign, publish chart via OCI by @dasomel

## [0.3.0] - 2026-07-19

### Documentation

- Update CHANGELOG.md for v0.2.2 by @github-actions[bot]

### Features

- OSS modernization — Go 1.26, tests, btrfs, docs/Helm, UI overhaul (#2) by @dasomel

## [0.2.2] - 2026-03-23

### Bug Fixes

- Correct trivy-action version tag (no v prefix) by @dasomel
- Address all 16 code review issues by @dasomel
- **deps:** Upgrade vulnerable indirect dependencies by @dasomel
- **lint:** Remove unused save() method in history store by @dasomel
- **ci:** Update trivy-action to v0.35.0 by @dasomel
- **ci:** Upgrade Go 1.24 → 1.25 to fix stdlib CVEs by @dasomel
- **ci:** Use goinstall mode for golangci-lint to support Go 1.25 by @dasomel
- **ci:** Update trivy-action in release.yaml to v0.35.0 by @dasomel
- **docker:** Upgrade base image golang:1.24 → 1.25 by @dasomel

### Documentation

- Update CHANGELOG.md for v0.2.1 by @github-actions[bot]
- Update CHANGELOG.md for v0.2.2 by @github-actions[bot]

### Miscellaneous

- Add .bkit/ and .omc/ to .gitignore and untrack state files by @dasomel

### Refactoring

- Apply simplify cleanup across agent, history, quota packages by @dasomel

### Security

- V0.2.2 by @dasomel

## [0.2.1] - 2026-02-09

### Documentation

- Add feature guide with screenshots (EN/KO) by @dasomel

### Refactoring

- Reorganize into internal packages and clean up project by @dasomel

## [0.2.0] - 2026-02-08

### Bug Fixes

- Add required volume mounts for xfs_quota operation by @dasomel
- Add CSI NFS PV support for quota sync by @dasomel
- Fix Web UI quota display by parsing /etc/projid and /etc/projects by @dasomel
- Handle long LVM device names in filesystem detection by @dasomel
- Support subDir parameter with capital D in CSI volumeAttributes by @dasomel

### Documentation

- Update CHANGELOG.md for v0.1.3 by @github-actions[bot]
- Update CHANGELOG.md for v0.1.4 by @github-actions[bot]
- Update CHANGELOG.md for v0.1.5 by @github-actions[bot]
- Add required volume mounts section to README by @dasomel
- Update CHANGELOG.md for v0.1.6 by @github-actions[bot]
- Update CHANGELOG.md for v0.1.8 by @github-actions[bot]
- Update README with CSI NFS PV support and bump chart to 0.1.8 by @dasomel
- Update CHANGELOG.md for v0.1.9 by @github-actions[bot]
- Update CHANGELOG.md for v0.1.10 by @github-actions[bot]
- Update CHANGELOG.md for v0.1.11 by @github-actions[bot]
- Update CHANGELOG.md for v0.1.12 by @github-actions[bot]
- Add namespace/PVC name directory pattern example by @dasomel
- Add recommended NFS mount options by @dasomel
- Update CHANGELOG.md for v0.1.15 by @github-actions[bot]
- Update CHANGELOG.md for v0.1.16 by @github-actions[bot]

### Features

- Add web UI as optional flag in run command by @dasomel
- Add PV/PVC status display and refresh button to Web UI by @dasomel
- Add audit logging support with Helm chart option by @dasomel
- Improve Web UI with sorting, nested dirs, and conditional audit tab by @dasomel
- Add actor and provisioner to audit logs by @dasomel

### Miscellaneous

- Bump Helm chart version to 0.1.6 by @dasomel
- Bump version to 0.1.10 by @dasomel
- Bump version to 0.1.11 by @dasomel

### Release

- V0.2.0 by @dasomel

## [0.1.3] - 2026-02-08

### Bug Fixes

- Use xfsprogs-extra for xfs_quota command by @dasomel

### Documentation

- Update CHANGELOG.md for v0.1.2 by @github-actions[bot]

## [0.1.2] - 2026-02-05

### Documentation

- Update CHANGELOG.md for v0.1.1 by @github-actions[bot]

### Features

- Add metrics service configuration to Helm chart by @dasomel

## [0.1.1] - 2026-02-05

### Bug Fixes

- Add util-linux for findmnt command in container by @dasomel

### Documentation

- Update CHANGELOG.md for v0.1.0 by @github-actions[bot]

## [0.1.0] - 2026-02-04

### Bug Fixes

- Update Go version to 1.22 for CI compatibility by @dasomel
- Use stable Go 1.22 and k8s.io/client-go v0.29.0 by @dasomel
- Resolve golangci-lint errors by @dasomel
- Update Go version to 1.22 in CI/CD workflows and Dockerfile by @dasomel
- Upgrade to Go 1.23 and fix remaining lint issues by @dasomel
- Update govulncheck to Go 1.23 and increase lint timeout by @dasomel
- Upgrade to Go 1.24 and update dependencies for security fixes by @dasomel
- Update govulncheck to Go 1.24 by @dasomel
- Add ref tag to Docker image metadata for security scan by @dasomel
- Handle long LVM device names in filesystem detection by @dasomel

### Documentation

- Update CHANGELOG.md for v0.1.0 by @github-actions[bot]


