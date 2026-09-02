# CNCF Sandbox Readiness & Strategy Roadmap

This document outlines the strategic positioning, architectural capabilities, and CNCF Sandbox readiness roadmap for **nfs-quota-agent**.

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

### 3. Supported Filesystems & Scope

- **XFS**: Enforces project quotas using `xfs_quota` (requires `prjquota` mount option).
- **ext4**: Enforces project quotas using `setquota` / `chattr` (requires `prjquota` mount option, Linux kernel 4.5+, and `quota_tree` kernel module).
- **Btrfs**: Enforces qgroup limits using `btrfs qgroup limit` (target directories must be subvolumes with `btrfs quota enable`).

---

### 4. Non-Goals & Scope Boundaries

- **Out of Scope**: Data replication, leader election for storage, volume fencing, cluster membership, and HA failover orchestration.
- **HA/DR Strategy**: High availability and failover management are explicitly delegated to external infrastructure tools (such as DRBD, Pacemaker, or storage array replication; see [`docs/ha-dr.md`](ha-dr.md)). The agent provides an active/standby mutation gate (`--ha-active-file`) to prevent concurrent quota mutations during split-brain scenarios without duplicating external HA capabilities.

---

### 5. Reusable Component Architecture

`nfs-quota-agent` is designed as a modular, reusable daemon running on the NFS server host (as a privileged Kubernetes DaemonSet or standalone systemd service):

```
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

## Phase 2: Kubernetes-Native Architecture Status

| Capability / Requirement | Status | Current Implementation Details | Sourced Context / Files |
| :--- | :---: | :--- | :--- |
| **PV Watcher & Reconciliation** | **Implemented** | Watches `VolumeBound` PVs, filters provisioners (`nfs.csi.k8s.io`, `nfs-client`, static NFS), resolves paths. | [`internal/agent`](../internal/agent), [`docs/architecture.md`](architecture.md) |
| **PV Annotation Status** | **Implemented** | Updates `nfs.io/quota-status` (`pending`, `applied`, `failed`) on target PVs. | [`README.md`](../README.md#pv-annotations) |
| **QuotaPolicy CRD** | **Implemented** | `quota.nfs.io/v1alpha1` CRD for declarative quota bounding (`defaultQuota`, `minQuota`, `maxQuota`, `enforceMax`); opt-in behind the `--enable-quota-policy` agent flag / chart value `quotaPolicy.enabled` (`false` by default). Answers the Phase 2 "is a CRD/controller needed" question from issue #81: yes, and it's built. | [`docs/quotapolicy-design.md`](quotapolicy-design.md), [`internal/apis/quota/v1alpha1`](../internal/apis/quota/v1alpha1), [`charts/nfs-quota-agent/values.yaml`](../charts/nfs-quota-agent/values.yaml) |
| **CEL Spec Validation** | **Implemented** | API server CEL validation prevents inverted bounds (`minQuota <= maxQuota`) using `quantity()`. | [`docs/quotapolicy-design.md`](quotapolicy-design.md#3-precedence) |
| **ResourceQuota / LimitRange Precedence** | **Open / Gap** | `QuotaPolicy` is a filesystem-enforcement layer, not a Kubernetes allocation layer, and deterministic precedence with `ResourceQuota`/`LimitRange`/StorageClass plus an admission-time preflight are not yet designed; no admission-webhook infrastructure exists in this repo today. | [dasomel/nfs-quota-agent#14](https://github.com/dasomel/nfs-quota-agent/issues/14) |
| **Drift Detection (`Drifted`)** | **Implemented** | Independent read-back (`xfs_quota report`, `repquota`, `btrfs qgroup show`) checks disk state vs. spec. | [`docs/quotapolicy-design.md`](quotapolicy-design.md#drifted-independent-read-back-not-the-enforcement-cache) |
| **Unsafe Shrink Guard** | **Implemented** | Rejects policy reductions below active directory storage usage (`UnsafeShrinkRejected`). | [`docs/quotapolicy-design.md`](quotapolicy-design.md#shrink-guard-refusing-a-decrease-below-current-usage) |
| **Prometheus Observability** | **Implemented** | `/metrics` endpoint, `ServiceMonitor`, and `PrometheusRule` manifests. | [`internal/metrics`](../internal/metrics), [`README.md`](../README.md#prometheus-metrics) |
| **Advisory Policy Views** | **Implemented** | Web UI surfaces informational namespace policies and policy violations. | [`docs/feature-guide.md`](feature-guide.md#4-policies-tab) |
| **Leased Leader Election** | **Open / Gap** | Status writing on multi-node deployments currently relies on `quotaPolicy.singleWriter=true`. K8s Lease leader election is planned. | [`docs/quotapolicy-design.md`](quotapolicy-design.md#multi-writer-status-this-charts-daemonset-can-run-on-several-nodes) |
| **Informer Workqueue** | **Open / Gap** | `QuotaPolicy` updates reconcile on periodic sync interval rather than an instant watch workqueue. | [`docs/quotapolicy-design.md`](quotapolicy-design.md#reconcile-cadence-and-why-the-watch-path-resolves-against-a-cache) |

---

## Phase 3: Storage Portability Status

| Capability / Requirement | Status | Current Implementation Details | Sourced Context / Files |
| :--- | :---: | :--- | :--- |
| **XFS Backend** | **Implemented** | Manages project quotas via `xfs_quota` and `/etc/projects`. | [`internal/quota/xfs.go`](../internal/quota) |
| **ext4 Backend** | **Implemented** | Manages directory project quotas via `chattr +P` and `setquota`. | [`internal/quota/ext4.go`](../internal/quota) |
| **Btrfs Backend** | **Implemented** | Manages `btrfs qgroup` limits on subvolume targets. | [`internal/quota/btrfs.go`](../internal/quota) |
| **KB Flooring Calculation** | **Implemented** | `ExpectedEnforcedBytes` accounts for XFS/ext4 1024-byte downward flooring. | [`CLAUDE.md`](../CLAUDE.md#gotchas) |
| **CSI & Native NFS Paths** | **Implemented** | Maps paths for native `pv.Spec.NFS` and CSI `pv.Spec.CSI` attributes. | [`README.md`](../README.md#how-it-works) |
| **Real Host Kernel E2E CI** | **Open / Gap** | Unit tests use stubbed `quota.CommandRunner`. End-to-end kernel verification requires a real `prjquota` host. | [`CLAUDE.md`](../CLAUDE.md#gotchas), [`docs/IMPLEMENTATION-STATUS.md`](IMPLEMENTATION-STATUS.md) |
| **ZFS / Other Filesystems** | **Open / Gap** | ZFS dataset quota support is currently not implemented. | [`docs/IMPLEMENTATION-STATUS.md`](IMPLEMENTATION-STATUS.md) |

---

## Phase 4: Security & Supply Chain Status

| Capability / Requirement | Status | Current Implementation Details | Sourced Context / Files |
| :--- | :---: | :--- | :--- |
| **Scoped RBAC** | **Implemented** | Least-privilege ClusterRole rules gated behind feature flags (`quotaPolicy.enabled`). | [`docs/quotapolicy-design.md`](quotapolicy-design.md#rbac-two-new-grants-beyond-the-crd-only-clusterrole-both-gated-on-quotapolicyenabled) |
| **CI Egress Control** | **Implemented** | Network egress restricted via StepSecurity `harden-runner`. | [`docs/security.md`](security.md), [`docs/IMPLEMENTATION-STATUS.md`](IMPLEMENTATION-STATUS.md) |
| **Dependency Integrity** | **Implemented** | Automated `dependency-review` action and SBOM generation. | [`docs/IMPLEMENTATION-STATUS.md`](IMPLEMENTATION-STATUS.md) |
| **Container Image Pinning** | **Implemented** | SHA256 digest pinning for base images and GitHub Actions. | [`docs/security.md`](security.md) |
| **Image Signing (Cosign)** | **In Progress** | Cosign image signing pipeline is currently being added in parallel effort. | Issue #81 prompt reference |
| **SLSA Provenance** | **Open / Gap** | SLSA Level 3 build provenance attestation is planned for formal release pipelines. | Strategy Roadmap |

---

## Strategic CNCF Sandbox Readiness Action Plan

1. **Finalize Security & Supply Chain Baseline**:
   - Complete Cosign container image signing and release provenance generation.
2. **Enhance Multi-Node Governance**:
   - Implement `coordination.k8s.io` Lease leader election for DaemonSet pods running `QuotaPolicy` status write-back.
3. **Submit CNCF Sandbox Application**:
   - Position `nfs-quota-agent` as the lightweight, specialized Kubernetes storage control plane component filling the critical NFS server-side project quota enforcement gap.
