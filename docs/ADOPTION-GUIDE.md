# NFS Quota Agent Adoption Guide

> First success is **filesystem quota enforcement**, not merely a running DaemonSet.

## 1. Preconditions

Use an NFS server node with a supported local filesystem and the quota feature enabled for that filesystem. Constrain the DaemonSet to the actual NFS server nodes; the project requires privileged host access and should not be scheduled broadly.

Implemented backends are summarized in the README's [Supported Filesystems](../README.md#supported-filesystems) table and kept current, per-backend, in `hack/compatibility-matrix.json`: XFS project quota, ext4 project quota, and Btrfs qgroup quota. Its shape is validated by `make compat-matrix-validate` (`hack/validate-compatibility-matrix.py` against `hack/compatibility-matrix.schema.json`).

## 2. Minimal adoption flow

1. Install/configure the Helm chart for the NFS server path and provisioner in use.
2. Confirm the agent is running only on the intended server node(s).
3. Create a test PVC/PV through the supported NFS path.
4. Confirm the PV receives quota status and the agent reports `applied` rather than only discovering the volume.
5. Write data beyond the requested capacity from a test workload.
6. Verify the filesystem refuses growth at the configured quota boundary.
7. Inspect Prometheus/audit evidence if enabled.

The test is incomplete if it stops at `kubectl get pvc` or pod readiness.

## 3. Evidence classes

Keep these claims separate:

- unit/stubbed command-runner tests;
- built-container capability checks for required filesystem commands;
- Kubernetes integration evidence;
- real quota-enabled filesystem enforcement E2E.

Only the last class proves actual quota enforcement.

## 4. Read next

- `README.md` ([Supported Filesystems](../README.md#supported-filesystems)) and `hack/compatibility-matrix.json` — implemented capabilities
- `docs/architecture.md` — architecture
- `docs/feature-guide.md` — features and configuration
- `docs/security.md` — privileged/runtime boundary
- `docs/ha-dr.md` — resilience
- `docs/quotapolicy-design.md` — advisory policy design

## 5. Known boundary

Namespace policy views are advisory and do not replace PV-capacity-to-filesystem-quota enforcement. Changes to host paths, privileged permissions, project-ID mapping, filesystem command construction, or exported APIs require stronger review and runtime evidence.