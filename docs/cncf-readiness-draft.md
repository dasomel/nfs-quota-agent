<!-- markdownlint-disable MD013 -->
# CNCF Sandbox Readiness & Strategy Roadmap

This document outlines the strategic positioning, architectural capabilities, evidence-backed readiness status, and CNCF Sandbox roadmap for **nfs-quota-agent**.

---

## Phase 1: Project Identity & Core Positioning

### 1. Core Positioning Answer

**Question**: Does `nfs-quota-agent` solve a shared filesystem quota problem that Kubernetes `ResourceQuota` and CSI drivers do not solve, in an independent, reusable way?

**Answer: YES.**

#### Technical Reasoning

1. **Gap in Kubernetes `ResourceQuota` & `LimitRange`**:
   - `ResourceQuota` operates exclusively at the Kubernetes API admission layer. It caps the cumulative capacity requested by `PersistentVolumeClaim` (PVC) objects within a namespace.
   - `ResourceQuota` has **no mechanism or visibility** to enforce disk limits on the underlying filesystem. If a pod writes data beyond the PVC's requested capacity on an NFS share, `ResourceQuota` cannot detect, prevent, or stop the write operation.

2. **Gap in CSI Drivers & NFS Provisioners**:
   - NFS provisioners (e.g., `csi-driver-nfs` or `nfs-subdir-external-provisioner`) create subdirectories on an NFS export and bind Kubernetes `PersistentVolume` (PV) objects to them.
   - Standard NFS protocol mounts (NFSv3 / NFSv4) do **not** enforce capacity limits on subdirectories on the client side. NFS client mount options cannot set or manage server-side kernel quotas.
   - Consequently, a single application writing to an NFS PV can consume 100% of the NFS server's physical storage array, causing a noisy-neighbor Denial of Service (DoS) for all other workloads sharing that NFS export.

3. **Gap in Ad-hoc Quota Scripts**:
   - Unmanaged shell scripts, cron jobs, or manual `xfs_quota`/`setquota` invocations lack integration with the Kubernetes API Server.
   - They fail to provide real-time PV watch event reconciliation, declarative policy custom resources, status write-back to PV annotations (`nfs.io/quota-status`), Prometheus metrics, audit logging, or safety-guarded orphan directory cleanup.

4. **The `nfs-quota-agent` Solution**:
   - Deployed on the NFS server node (via hostPath access to the export and `/etc/projects`), `nfs-quota-agent` bridges Kubernetes PV lifecycle events with Linux kernel filesystem quota subsystems (XFS project quotas, ext4 project quotas, and Btrfs qgroups).
   - It automatically maps PVC requested storage to filesystem project IDs and enforces strict, kernel-level hard byte limits directly on the server host.

---

### 2. Multi-Tenancy & Problem Definition

In shared storage architectures, multiple Kubernetes namespaces and tenants store data on a single NFS filesystem export. Without server-side project quota enforcement, storage isolation relies on trust. `nfs-quota-agent` delivers true storage multi-tenancy at the kernel level for NFS-backed Kubernetes volumes.

---

### 3. Supported Filesystems & Scope Boundaries

- **XFS**: Enforces project quotas using `xfs_quota` (requires `prjquota` mount option). Verified on real kernel host.
- **ext4**: Enforces project quotas using `setquota` / `chattr` (requires `prjquota` mount option, Linux kernel 4.5+, and `quota_tree` kernel module).
- **Btrfs**: Enforces qgroup limits using `btrfs qgroup limit` (target directories must be subvolumes with `btrfs quota enable`).
- **Out of Scope**: Data replication, leader election for storage, volume fencing, cluster membership, and HA failover orchestration.
- **HA/DR Strategy**: High availability and failover management are explicitly delegated to external infrastructure tools (such as DRBD, Pacemaker, or storage array replication; see [`docs/ha-dr.md`](ha-dr.md)). The agent provides an active/standby mutation gate (`--ha-active-file`) to prevent concurrent quota mutations during split-brain scenarios without duplicating external HA capabilities.

---

### 4. Reusable Component Architecture

`nfs-quota-agent` is designed as a modular, reusable daemon running on the NFS server host (as a privileged Kubernetes DaemonSet or standalone systemd service):

```text
┌─────────────────┐     ┌─────────────────────────────────────────────────┐
│   Kubernetes    │     │              NFS Server Node                    │
│    API Server   │     │  ┌─────────────────────────────────────────────┐│
│                 │     │  │           nfs-quota-agent                   ││
│  ┌───────────┐  │     │  │  ┌───────────┐    ┌─────────────────────┐   ││
│  │    PV     │◄─┼─────┼──┼──│  Watcher  │    │ Filesystem Quota    │   ││
│  │ (NFS type)│  │     │  │  └───────────┘    │ Engine (xfs/ext4/   │   ││
│  └───────────┘  │     │  │         │          │ btrfs)              │   ││
│                 │     │  │         ▼          └─────────────────────┘   ││
└─────────────────┘     │  │  ┌─────────────────────────────────────┐    ││
                        │  │  │ CommandRunner (xfs_quota/setquota) │    ││
                        │  │  └─────────────────────────────────────┘    ││
                        │  └─────────────────────────────────────────────┘│
                        │                      │                          │
                        │                      ▼                          │
                        │  ┌──────────────────────────────────────────┐   │
                        │  │      NFS Export Filesystem (/data)       │   │
                        │  └──────────────────────────────────────────┘   │
                        └─────────────────────────────────────────────────┘
```

---

## Readiness Status Table by Phase

All 10 phases from strategic issue [#81](https://github.com/dasomel/nfs-quota-agent/issues/81) are tracked below with verified evidence citations pointing to source paths, commit shas, or merged PRs in the repository.

| Phase | Item | Status | Evidence | Next Step |
| :--- | :--- | :---: | :--- | :--- |
| **Phase 1** | Define shared storage quota problem | **DONE** | [`docs/cncf-readiness-draft.md:16-33`](cncf-readiness-draft.md), [`docs/IMPLEMENTATION-STATUS.md:7-9`](IMPLEMENTATION-STATUS.md) | Maintain alignment with Kubernetes SIG Storage updates |
| **Phase 1** | Document operational pain points | **DONE** | [`docs/cncf-readiness-draft.md:21-25`](cncf-readiness-draft.md), [`docs/ADOPTION-GUIDE.md:3-32`](ADOPTION-GUIDE.md) | Gather additional enterprise workload case studies |
| **Phase 1** | Contrast with ResourceQuota | **DONE** | [`docs/cncf-readiness-draft.md:17-20`](cncf-readiness-draft.md), [`docs/quotapolicy-design.md:14-22`](quotapolicy-design.md) | Track Kubernetes API ResourceQuota capacity proposals |
| **Phase 1** | Contrast with CSI driver quotas | **DONE** | [`docs/cncf-readiness-draft.md:21-25`](cncf-readiness-draft.md), [`docs/architecture.md:22-48`](architecture.md) | Monitor CSI VolumeLimits / VolumeStats API enhancements |
| **Phase 1** | Contrast with ad-hoc quota scripts | **DONE** | [`docs/cncf-readiness-draft.md:26-33`](cncf-readiness-draft.md), [`docs/architecture.md:5-20`](architecture.md) | Emphasize real-time reconciliation in user guides |
| **Phase 1** | Connect XFS project quota to K8s | **DONE** | [`internal/quota/xfs.go:20-65`](../internal/quota/xfs.go), [`docs/architecture.md:85-115`](architecture.md) | Track newer `xfsprogs` release command compatibility |
| **Phase 1** | Document ext4/Btrfs scope & differences | **DONE** | [`CLAUDE.md:38-48`](../CLAUDE.md), [`hack/compatibility-matrix.json:11-22`](../hack/compatibility-matrix.json) | Complete automated real-kernel ext4 E2E validation |
| **Phase 1** | Define project scope & non-goals | **DONE** | [`docs/cncf-readiness-draft.md:48-55`](cncf-readiness-draft.md), [`docs/ha-dr.md:5-30`](ha-dr.md) | Re-evaluate non-goals if storage clustering is considered |
| **Phase 1** | Reusable component architecture doc | **DONE** | [`docs/architecture.md:1-120`](architecture.md), [`docs/cncf-readiness-draft.md:57-81`](cncf-readiness-draft.md) | Present architecture overview to CNCF Storage TAG |
| **Phase 2** | PV/PVC to quota mapping structure | **DONE** | [`internal/agent/agent.go:1731-1745`](../internal/agent/agent.go), [`internal/agent/watch.go:120-180`](../internal/agent/watch.go), PR #107 | Hard-reject fallback path on StorageClass boundary mismatch |
| **Phase 2** | Namespace/workload policy model | **DONE** | [`docs/quotapolicy-design.md:1-120`](quotapolicy-design.md), [`internal/apis/quota/v1alpha1/types.go:200-247`](../internal/apis/quota/v1alpha1/types.go), PR #13 | Evaluate pod/workload selector policies beyond namespace scope |
| **Phase 2** | Declarative API/CRD quota management | **DONE** | [`internal/apis/quota/v1alpha1/types.go:20-80`](../internal/apis/quota/v1alpha1/types.go), [`charts/nfs-quota-agent/crds/`](../charts/nfs-quota-agent/crds/) | Graduate `v1alpha1` to `v1beta1` following multi-cluster tests |
| **Phase 2** | Controller/operator architecture | **DONE** | [`internal/quotapolicy/policy.go:1-200`](../internal/quotapolicy/policy.go), [`docs/IMPLEMENTATION-STATUS.md:38-46`](IMPLEMENTATION-STATUS.md) | Maintain controller decoupled inside DaemonSet architecture |
| **Phase 2** | Dynamic provisioning / CSI integration | **DONE** | [`internal/agent/agent.go:960-1010`](../internal/agent/agent.go), [`docs/architecture.md:35-50`](architecture.md) | Test interoperability with external out-of-tree CSI provisioners |
| **Phase 2** | Multi-NFS server / backend support | **PARTIAL** | [`docs/architecture.md:65-80`](architecture.md), [`internal/agent/agent.go:1020-1060`](../internal/agent/agent.go) | Support per-export server identities and heterogeneous backends |
| **Phase 2** | HA & failover strategy | **PARTIAL** | [`docs/ha-dr.md:1-65`](ha-dr.md), [`docs/quotapolicy-design.md:180-210`](quotapolicy-design.md) | Implement `coordination.k8s.io` Lease leader election for writers |
| **Phase 2** | Idempotent reconciliation guarantee | **DONE** | [`internal/agent/agent.go:1144-1230`](../internal/agent/agent.go), [`docs/quotapolicy-design.md:150-180`](quotapolicy-design.md), PR #13 | Maintain zero-mutation verification across periodic sync loops |
| **Phase 2** | Quota drift detection & reconcile | **DONE** | [`internal/quotapolicy/status.go:261`](../internal/quotapolicy/status.go), [`docs/quotapolicy-design.md:160-195`](quotapolicy-design.md), PR #13 | Add automated alerting trigger when `Drifted` is observed |
| **Phase 2** | Large-scale PV/PVC scalability test | **OPEN** | Currently unexercised at synthetic scale (>1,000 PVs) | Develop synthetic scalability test harness for 1,000+ PVs |
| **Phase 3** | XFS project quota production test | **PARTIAL** | Manual host test in Issue #4 noted in [`hack/compatibility-matrix.json:5-10`](../hack/compatibility-matrix.json); automated CI test pending in PR #126 | Run tests across multiple enterprise Linux distributions |
| **Phase 3** | ext4 quota support verification | **PARTIAL** | [`internal/quota/ext4.go:1-120`](../internal/quota/ext4.go), [`CLAUDE.md:43-48`](../CLAUDE.md), [`hack/compatibility-matrix.json:11-16`](../hack/compatibility-matrix.json) | Run automated E2E with host `quota_tree` kernel module loaded |
| **Phase 3** | Btrfs support verification & status | **PARTIAL** | [`internal/quota/btrfs.go:1-95`](../internal/quota/btrfs.go), [`hack/compatibility-matrix.json:17-22`](../hack/compatibility-matrix.json), PR #86 | Implement automated Btrfs subvolume qgroup E2E test |
| **Phase 3** | Per-filesystem capability detection | **DONE** | [`internal/quota/detect.go:62-80`](../internal/quota/detect.go), [`internal/agent/agent.go:699-733`](../internal/agent/agent.go) | Add pre-apply kernel probe checking filesystem mount flags |
| **Phase 3** | Safe failure for unsupported fs | **DONE** | [`internal/agent/agent.go:1060-1070`](../internal/agent/agent.go), [`docs/IMPLEMENTATION-STATUS.md:11-16`](IMPLEMENTATION-STATUS.md) | Surface unsupported filesystem errors in Kubernetes Events |
| **Phase 3** | NFS server compatibility testing | **PARTIAL** | Linux `nfs-kernel-server` verified in Issue #4 manual session; automated workflow pending in PR #126; user-space unverified | Test interoperability against NFS-Ganesha |
| **Phase 3** | Diverse K8s distribution verification | **PARTIAL** | [`hack/compatibility-matrix.json:41-54`](../hack/compatibility-matrix.json) (k3s v1.35/1.36 in Issue #4 manual session; Kind test pending in PR #126) | Verify on vanilla upstream kubeadm, OpenShift, and managed K8s |
| **Phase 3** | Bare-metal / VM / cloud portability | **PARTIAL** | Colima VM arm64 verified in Issue #4 manual session; GitHub Actions runner pending in PR #126 | Document reference validation on dedicated bare-metal storage host |
| **Phase 4** | Review and strengthen SECURITY.md | **DONE** | [`SECURITY.md:1-25`](../SECURITY.md), [`SECURITY-ko.md:1-25`](../SECURITY-ko.md), OpenForge standard | Establish dedicated security vulnerability reporting alias |
| **Phase 4** | Least-privilege RBAC review | **DONE** | [`charts/nfs-quota-agent/templates/clusterrole.yaml:1-60`](../charts/nfs-quota-agent/templates/clusterrole.yaml), PR #14 review | Maintain read-only scope for tenant PVCs |
| **Phase 4** | Minimize and document privileged needs | **DONE** | [`CLAUDE.md:52-54`](../CLAUDE.md), [`SECURITY.md:11-18`](../SECURITY.md), [`docs/architecture.md:95-110`](architecture.md) | Investigate rootless/userns execution boundaries on Linux 6.x |
| **Phase 4** | HostPath access threat model | **DONE** | [`docs/security.md:20-60`](security.md), [`docs/ADOPTION-GUIDE.md:65-80`](ADOPTION-GUIDE.md) | Publish formal threat modeling diagram following STRIDE |
| **Phase 4** | Container image vulnerability scan | **PARTIAL** | [`.github/workflows/ci.yaml:433-445`](../.github/workflows/ci.yaml) runs Trivy filesystem scanning and uploads SARIF; no built-image scan is configured | Add a Trivy image scan of the built OCI image and alert on newly reported upstream CVEs |
| **Phase 4** | Dependency vulnerability scan | **DONE** | [`.github/workflows/ci.yaml:447-451`](../.github/workflows/ci.yaml) (`govulncheck`), PR #108, PR #114 | Maintain automated weekly Dependabot scans |
| **Phase 4** | SBOM generation & release attachment | **DONE** | [`.github/workflows/release.yaml:215-245`](../.github/workflows/release.yaml) (`anchore/sbom-action`), PR #62 | Verify SBOM package URLs against compiled binary hashes |
| **Phase 4** | Container image signing (Cosign) | **PARTIAL** | [`.github/workflows/release.yaml:179-188`](../.github/workflows/release.yaml), [`hack/verify-release.py:110-160`](../hack/verify-release.py), PR #102 (workflow configured, live tag signature pending) | Validate keyless signature on first live `v*` tag trigger |
| **Phase 4** | SLSA provenance & build manifest v4 | **DONE** | Commit `037b3f5` (`git log`), `release-manifest.json` v4 (PR #120), `buildx mode=max` | Transition to formal SLSA Level 3 builder action |
| **Phase 4** | OpenSSF Scorecard activation | **PARTIAL** | CI egress block (PR #123), dependency pinning (PR #104), branch protection active | Check in `.github/workflows/scorecard.yml` workflow |
| **Phase 4** | OpenSSF Best Practices Badge | **OPEN** | Criteria reviewed in issue #81; badge not yet formally requested | Submit application to OpenSSF Best Practices Badge program |
| **Phase 5** | Unit test coverage expansion | **DONE** | [`internal/quota/report_test.go`](../internal/quota/report_test.go), [`hack/test_verify_release.py`](../hack/test_verify_release.py) (343 `Test*` functions: `go test -list . ./... | awk '/^Test/ { count++ } END { print count+0 }'`) | Track and enforce CI line coverage threshold |
| **Phase 5** | Integration test automation | **PARTIAL** | [`.github/workflows/ci.yaml:43-90`](../.github/workflows/ci.yaml) runs hermetic unit/race tests; [`internal/agent/watch_test.go`](../internal/agent/watch_test.go) exercises the watch path without a real NFS server | Implement mock NFS RPC server integration tests |
| **Phase 5** | Kubernetes E2E test infrastructure | **PARTIAL** | Manual host test record in Issue #4; automated workflow pending in PR #126 ([`.github/workflows/e2e-airgap.yaml`](../.github/workflows/)) | Merge PR #126 to automate E2E on every main commit |
| **Phase 5** | Real NFS server quota E2E test | **PARTIAL** | Verified on host in Issue #4 manual session; automated workflow pending in PR #126 (`mkfs.xfs` + EDQUOT assertion) | Land PR #126 and observe in GitHub Actions CI |
| **Phase 5** | Per-filesystem regression tests | **PARTIAL** | Unit regressions cover XFS/ext4/Btrfs; real-kernel CI covers XFS pending in PR #126 | Add ext4 and Btrfs runner environments to CI matrix |
| **Phase 5** | K8s version compatibility matrix | **DONE** | [`hack/compatibility-matrix.json:1-63`](../hack/compatibility-matrix.json), [`hack/validate-compatibility-matrix.py`](../hack/validate-compatibility-matrix.py), PR #101 | Re-run quarterly compatibility validation |
| **Phase 5** | Upgrade / downgrade test automation | **PARTIAL** | Verified manually in Issue #4 session; automated test pending in PR #126 Stage E (Helm upgrade/rollback) | Merge PR #126 to enforce quota preservation in CI |
| **Phase 5** | Failure and recovery testing | **PARTIAL** | `--ha-active-file` gate tested; pod crash restart verified manually in Issue #4 session | Add automated chaos test simulating abrupt node reboot |
| **Phase 5** | Concurrent quota operation test | **DONE** | [`internal/agent/agent.go:1144-1230`](../internal/agent/agent.go), PR #13, [`internal/agent/reconcile_queue_test.go`](../internal/agent/reconcile_queue_test.go), PR #106 | Add high-concurrency burst PVC creation stress test |
| **Phase 5** | Quota exhaustion scenario test | **PARTIAL** | Verified manually on host in Issue #4 session; automated CI test pending in PR #126 Stage D (120MiB write on 100MiB quota fails with EDQUOT) | Ensure metrics and alerts emit on quota exhaustion |
| **Phase 5** | Scalability & performance benchmark | **OPEN** | No published benchmark measuring enforcement latency under load | Run and publish benchmarks for 100 to 1,000 PV reconciliations |
| **Phase 6** | Standardized Prometheus metrics | **DONE** | [`internal/metrics/metrics.go:130-162`](../internal/metrics/metrics.go), [`docs/IMPLEMENTATION-STATUS.md:28-36`](IMPLEMENTATION-STATUS.md) | Ensure OpenMetrics specification compliance |
| **Phase 6** | Usage / limit / violation metrics | **DONE** | [`internal/metrics/metrics.go:171-223`](../internal/metrics/metrics.go) (`nfs_quota_used_bytes`, `nfs_quota_limit_bytes`, `nfs_quota_warning_count`) | Introduce cardinality limits for project ID labels |
| **Phase 6** | Controller reconciliation metrics | **DONE** | [`internal/metrics/metrics.go:232-262`](../internal/metrics/metrics.go) (`reconcile_queue_depth`, `reconcile_total`, `reconcile_duration_seconds_sum`), [`internal/agent/agent.go:431-450`](../internal/agent/agent.go) | Add QuotaPolicy CRD reconciliation duration histograms |
| **Phase 6** | Error and retry metrics | **PARTIAL** | [`internal/metrics/metrics.go:244-255`](../internal/metrics/metrics.go) exposes `reconcile_errors_total` and `verification_failures_total`; retry-specific metrics are not defined | Add retry metrics and distinguish transient IO errors from kernel quota errors |
| **Phase 6** | Structured logging standardization | **DONE** | [`cmd/nfs-quota-agent/main.go:40-75`](../cmd/nfs-quota-agent/main.go), [`internal/audit/logger.go:1-90`](../internal/audit/logger.go), PR #111 | Verify structured JSON formatting with log collectors |
| **Phase 6** | Kubernetes Events integration | **PARTIAL** | PV annotations implemented (`nfs.io/quota-status`); `corev1.Event` broadcaster pending | Implement `k8s.io/client-go/tools/record` EventRecorder |
| **Phase 6** | Grafana dashboard provision | **PARTIAL** | ServiceMonitor provided in chart; curated Grafana JSON dashboard pending | Commit production Grafana JSON template to repository |
| **Phase 6** | Troubleshooting guide | **DONE** | [`README.md:936-991`](../README.md#troubleshooting), [`CLAUDE.md:13-50`](../CLAUDE.md) (ext4 `quota_tree`, KB-flooring) | Extract dedicated standalone `docs/TROUBLESHOOTING.md` |
| **Phase 6** | Production operations guide | **DONE** | [`docs/ADOPTION-GUIDE.md:1-120`](ADOPTION-GUIDE.md), [`docs/ha-dr.md:1-80`](ha-dr.md), [`docs/IMPLEMENTATION-STATUS.md:47-55`](IMPLEMENTATION-STATUS.md) | Document disaster recovery runbook for NFS export failure |
| **Phase 7** | Apache-2.0 CNCF-compatible license | **DONE** | [`LICENSE:1-201`](../LICENSE), PR #99, PR #108 (license check automation) | Continuously verify new dependencies via `dependency-review` |
| **Phase 7** | Strengthened CONTRIBUTING.md | **DONE** | [`CONTRIBUTING.md:1-250`](../CONTRIBUTING.md), commit `887fac0` (DCO sign-off convention) | Solicit feedback from new external contributors |
| **Phase 7** | Confirmed CODE_OF_CONDUCT.md | **DONE** | [`CODE_OF_CONDUCT.md:1-60`](../CODE_OF_CONDUCT.md) (Contributor Covenant v2.1), [`CODE_OF_CONDUCT-ko.md`](../CODE_OF_CONDUCT-ko.md) | Ensure moderation contact address is tested |
| **Phase 7** | Documented MAINTAINERS.md | **DONE** | [`MAINTAINERS.md:1-20`](../MAINTAINERS.md), [`MAINTAINERS-ko.md:1-20`](../MAINTAINERS-ko.md), commit `887fac0` | Add co-maintainers as contributor base grows |
| **Phase 7** | Adopted GOVERNANCE.md | **DONE** | [`GOVERNANCE.md:1-74`](../GOVERNANCE.md), [`GOVERNANCE-ko.md:1-74`](../GOVERNANCE-ko.md), commit `887fac0` | Transition to 3+ maintainer voting model when eligible |
| **Phase 7** | Configured CODEOWNERS | **DONE** | [`.github/CODEOWNERS:1-7`](../.github/CODEOWNERS) (reviews routed to `@dasomel`) | Update CODEOWNERS upon onboarding additional maintainers |
| **Phase 7** | DCO convention documented | **DONE** | [`CONTRIBUTING.md:120-155`](../CONTRIBUTING.md), commit `887fac0` | Consider adding automated DCO check bot |
| **Phase 7** | Issue & PR templates | **DONE** | [`.github/ISSUE_TEMPLATE/`](../.github/ISSUE_TEMPLATE/), [`.github/PULL_REQUEST_TEMPLATE.md`](../.github/PULL_REQUEST_TEMPLATE.md) | Add specialized bug template for filesystem quota errors |
| **Phase 7** | Documented release policy | **DONE** | [`GOVERNANCE.md:57-63`](../GOVERNANCE.md), [`docs/release-egress-block.md:1-70`](release-egress-block.md) | Document end-of-life and backport policies |
| **Phase 7** | Semantic versioning policy | **DONE** | [`GOVERNANCE.md:61-62`](../GOVERNANCE.md) (SemVer 2.0.0 compliance specified) | Enforce API compatibility checking in CI |
| **Phase 7** | Maintained public roadmap | **DONE** | [`docs/cncf-readiness-draft.md`](cncf-readiness-draft.md), Issue #81 | Review and update roadmap quarterly |
| **Phase 7** | External contributor onboarding | **PARTIAL** | [`CONTRIBUTING.md`](../CONTRIBUTING.md), [`Makefile`](../Makefile), [`docs/ADOPTION-GUIDE.md`](ADOPTION-GUIDE.md) | Curate and label issues with `good first issue` |
| **Phase 8** | Minimal external install path | **DONE** | [`charts/nfs-quota-agent/`](../charts/nfs-quota-agent/), [`docs/ADOPTION-GUIDE.md:35-80`](ADOPTION-GUIDE.md) | Test quick-install script on lightweight distributions |
| **Phase 8** | Stable Helm chart & packaging | **DONE** | [`charts/nfs-quota-agent/Chart.yaml`](../charts/nfs-quota-agent/Chart.yaml), PR #110 (digest pinning), PR #117 | Publish Helm chart to Artifact Hub |
| **Phase 8** | 10-15 minute Quick Start guide | **DONE** | [`docs/ADOPTION-GUIDE.md:35-75`](ADOPTION-GUIDE.md), [`README.md:45-90`](../README.md) | Validate guide with operators unfamiliar with the project |
| **Phase 8** | Collection of real-world use cases | **PARTIAL** | Multi-tenant isolation use case documented in [`docs/ADOPTION-GUIDE.md`](ADOPTION-GUIDE.md) | Publish formal `ADOPTERS.md` directory |
| **Phase 8** | External user feedback collection | **PARTIAL** | Feedback tracked in GitHub Issues (#13, #14, #26, #5, #81) | Open GitHub Discussions for broader user Q&A |
| **Phase 8** | Expansion of external contributors | **PARTIAL** | Automated bot contributions, multi-model review passes active | Present project at Kubernetes SIG Storage / CNCF meetups |
| **Phase 8** | Tracking adoption & release metrics | **PARTIAL** | GitHub release asset and GHCR pull telemetry available | Publish quarterly pull counts in periodic review report |
| **Phase 8** | Public organization adoption evidence | **OPEN** | No formal organization adoption case study published yet | Solicit public testimonials from enterprise cluster operators |
| **Phase 8** | Presentations, blog posts & demos | **OPEN** | Technical design documented; external blog posts pending | Draft technical blog post on Linux project quotas in K8s |
| **Phase 9** | Review CNCF Sandbox criteria | **DONE** | Documented in [`docs/cncf-readiness-draft.md`](cncf-readiness-draft.md) and Issue #81 | Review CNCF Sandbox guidelines quarterly |
| **Phase 9** | CNCF Storage TAG engagement | **PARTIAL** | Documented shared storage gap in [`docs/cncf-readiness-draft.md:7-40`](cncf-readiness-draft.md) | Prepare project overview deck for CNCF Storage TAG |
| **Phase 9** | Relationship with K8s SIG Storage | **DONE** | Analyzed in [`docs/cncf-readiness-draft.md:17-25`](cncf-readiness-draft.md), [`docs/quotapolicy-design.md:14-25`](quotapolicy-design.md) | Engage SIG Storage during quota semantic reviews |
| **Phase 9** | Overlap check with CNCF projects | **DONE** | [`docs/cncf-readiness-draft.md:21-25`](cncf-readiness-draft.md) (differentiated from Ceph/Rook, Longhorn, CSI) | Track emerging shared filesystem projects in CNCF Landscape |
| **Phase 9** | Comparison with Sandbox projects | **DONE** | [`docs/cncf-readiness-draft.md:11-34`](cncf-readiness-draft.md) (unique node-level filesystem quota daemon) | Monitor lifecycle of specialized storage node agents |
| **Phase 9** | CNCF Landscape registration | **OPEN** | Requires broader adoption or initial Sandbox review | Submit pull request to `cncf/landscape` under Storage |
| **Phase 9** | Vendor-neutral positioning | **DONE** | [`GOVERNANCE.md:17-25`](../GOVERNANCE.md) (independent majority-vote pathway), [`LICENSE`](../LICENSE) (Apache-2.0) | Ensure all branding and governance remain neutral |
| **Phase 9** | Neutral org transition scenario | **DONE** | [`GOVERNANCE.md:71-74`](../GOVERNANCE.md) (preparatory governance steps documented) | Plan GitHub organization transition (`nfs-quota-agent`) |
| **Phase 9** | CNCF presentation materials | **OPEN** | Technical justification ready in docs; slide deck not yet created | Produce formal slide deck following CNCF Sandbox template |
| **Phase 10** | Clear problem definition & differentiation | **DONE** | [`docs/cncf-readiness-draft.md:7-40`](cncf-readiness-draft.md), [`docs/IMPLEMENTATION-STATUS.md:7-10`](IMPLEMENTATION-STATUS.md) | Keep differentiation current as CSI drivers evolve |
| **Phase 10** | Independent reusable cloud component | **DONE** | [`docs/cncf-readiness-draft.md:57-81`](cncf-readiness-draft.md), [`docs/architecture.md:1-50`](architecture.md) | Maintain zero hard dependencies on platform distributions |
| **Phase 10** | Mature K8s-native API & architecture | **DONE** | [`internal/apis/quota/v1alpha1/types.go`](../internal/apis/quota/v1alpha1/types.go), PR #13, PR #111, PR #125 | Resolve pending maintainer decisions (StorageClass, persistence) |
| **Phase 10** | Verified across 2+ K8s environments | **PARTIAL** | [`hack/compatibility-matrix.json:41-54`](../hack/compatibility-matrix.json) (k3s v1.35/1.36 in Issue #4 manual session; Kind pending in PR #126) | Add automated verification record on cloud-managed K8s |
| **Phase 10** | Stable release & versioning system | **DONE** | `release-manifest.json` v4 (PR #120), offline bundle (PR #117), strict verifier (PR #102), SemVer in [`GOVERNANCE.md:61-63`](../GOVERNANCE.md) | Execute next live `v*` tag release |
| **Phase 10** | Security & supply-chain requirements | **DONE** | [`SECURITY.md`](../SECURITY.md), CI egress block (`ci.yaml:16`), release egress allowlists (PR #123), Cosign | Complete incremental release egress flip from audit to block |
| **Phase 10** | Public governance & contribution process | **DONE** | [`GOVERNANCE.md`](../GOVERNANCE.md), [`CONTRIBUTING.md`](../CONTRIBUTING.md), [`.github/CODEOWNERS`](../.github/CODEOWNERS), commit `887fac0` | Encourage external contributor participation |
| **Phase 10** | Direction to improve bus factor | **DONE** | [`GOVERNANCE.md:17-25`](../GOVERNANCE.md), [`MAINTAINERS.md:13-16`](../MAINTAINERS.md) (formal transition to 3+ maintainers) | Onboard additional project co-maintainers |
| **Phase 10** | External user / contributor evidence | **PARTIAL** | Multi-party issues, automated tooling, independent reviews active; enterprise directory pending | Create `ADOPTERS.md` to record production users |
| **Phase 10** | Meaningful differentiation in CNCF | **DONE** | [`docs/cncf-readiness-draft.md:11-34`](cncf-readiness-draft.md) (fills the unaddressed server-side shared NFS quota gap) | Highlight complementary operation alongside CSI provisioners |
| **Phase 10** | Mutual benefit for CNCF ecosystem | **DONE** | [`docs/cncf-readiness-draft.md:1-40`](cncf-readiness-draft.md), Issue #81 strategy | Present at CNCF Storage TAG to validate community interest |

---

## Periodic Evidence Review

The following 10-line checklist must be executed once each quarter by maintainers to maintain evidence integrity and detect regressions across dependencies, compatibility, security, and supply-chain controls:

1. **Compatibility Matrix Validation**: Verify that the support matrix matches JSON schema constraints.

   ```bash
   make compat-matrix
   ```

2. **Offline Release Bundle Verification**: Confirm deterministic packaging and offline integrity tests.

   ```bash
   python3 -m pytest hack/test_release_bundle_makefile.py hack/test_make_deterministic_tarball.py -q
   ```

3. **Release Verifier Test Suite**: Ensure offline signature verification and schema parsing pass.

   ```bash
   python3 -m pytest hack/test_verify_release.py -q
   ```

4. **Release Egress Block Phasing**: Inspect StepSecurity egress policies across all 9 release jobs per [`docs/release-egress-block.md`](release-egress-block.md).

   ```bash
   grep -E "(name:|egress-policy:)" .github/workflows/release.yaml
   ```

5. **CI Egress Block Integrity**: Confirm all CI jobs strictly enforce `egress-policy: block`.

   ```bash
   grep "egress-policy:" .github/workflows/ci.yaml | sort | uniq -c
   ```

6. **Dependabot & Dependency Health**: Check open automated dependency updates and security alerts.

   ```bash
   gh pr list --search "author:app/dependabot" --state open
   ```

7. **Go Vulnerability Check**: Scan codebase for known Go dependency vulnerabilities.

   ```bash
   govulncheck ./...
   ```

8. **Static Container & Filesystem Vulnerability Scan**: Run Trivy security scanner against the repository.

   ```bash
   trivy fs --severity HIGH,CRITICAL .
   ```

9. **Real-Filesystem E2E Status**: Check status and run records for the automated air-gap XFS quota E2E workflow.

   ```bash
   gh run list --workflow e2e-airgap.yaml -L 5
   ```

10. **Full Unit & Race Detection Test Suite**: Execute all Go tests with data race detector enabled.

    ```bash
    go test -v -race ./...
    ```

---

## Decisions Pending Maintainers

The following four strategic and architectural decisions are pending resolution by project maintainers (copied from maintainer reviews on issues [#14](https://github.com/dasomel/nfs-quota-agent/issues/14), [#26](https://github.com/dasomel/nfs-quota-agent/issues/26), and [#5](https://github.com/dasomel/nfs-quota-agent/issues/5)):

1. **StorageClass CRD Field + RBAC** (Issue [#14](https://github.com/dasomel/nfs-quota-agent/issues/14)):
   - *Context*: Deciding whether to introduce a `storageClassName` or selector field into `QuotaPolicy` spec and grant `storageclasses` read RBAC to the agent DaemonSet.
   - *Hazard / Constraint*: StorageClass binding is not proof of filesystem ownership (`nfsServerIdentity` is an operator string, and `nfsPathToLocal` in [`internal/agent/agent.go:1734`](../internal/agent/agent.go) falls back to `filepath.Base` on mismatch). Any StorageClass binding gate must hard-reject fallback paths to prevent cross-namespace directory takeover, while strictly avoiding cluster-wide tenant PVC update/patch permissions.

2. **Admission Correlation Persistence** (Issue [#14](https://github.com/dasomel/nfs-quota-agent/issues/14)):
   - *Context*: Deciding where to persist the admission decision / correlation ID across the admission-to-reconciliation lifecycle.
   - *Hazard / Constraint*: Persisting decision IDs on tenant PVC annotations would require granting the privileged host DaemonSet cluster-wide tenant PVC update/patch permissions, and tenant PVC annotations are untrusted client input. The correlation ID is currently logged via `slog` and `internal/audit.Entry` (PR #111); maintainers must decide whether to record it in PV annotations (`nfs.io/correlation-id`), Kubernetes Events (requiring an EventRecorder), or retain it within agent-local audit history.

3. **Air-gap Registry Credential Model** (Issue [#5](https://github.com/dasomel/nfs-quota-agent/issues/5), PR #117, PR #126):
   - *Context*: Standardizing the credential and image distribution pattern for private container registries in air-gapped enterprise environments.
   - *Hazard / Constraint*: The offline bundle provides an OCI archive with `pullPolicy: Never` (PR #117, PR #126 D2). In enterprise environments with private mirror registries, operators require either `imagePullSecrets` or pre-loaded node images; maintainers must specify the standardized pattern and Helm chart configuration for private registry credentials without weakening zero-egress guarantees.

4. **Next `v*` Tagged Release Execution** (Issue [#26](https://github.com/dasomel/nfs-quota-agent/issues/26), Issue [#5](https://github.com/dasomel/nfs-quota-agent/issues/5), [`docs/release-egress-block.md`](release-egress-block.md)):
   - *Context*: Scheduling and triggering the next formal release tag (`v*`).
   - *Hazard / Constraint*: Keyless Sigstore Cosign signing and OIDC provenance generation cannot execute locally and only run during real GitHub Actions tag triggers. Furthermore, per [`docs/release-egress-block.md`](release-egress-block.md), transitioning the 9 release jobs from `audit` to `block` mode requires observing allowlist traffic during at least one live tagged release run before flipping each job incrementally.
