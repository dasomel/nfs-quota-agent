# Current Implementation Status

Last verified: 2026-09-03 against `main`.

This snapshot records capabilities that are implemented today. Planned CRDs, policy semantics, CNCF strategy and other future work remain tracked separately in issues/design documents.

## Core problem solved

Kubernetes PVC/PV capacity for NFS-backed storage does not itself enforce a filesystem limit on the NFS server. NFS Quota Agent closes that control gap by watching NFS PersistentVolumes and applying quota controls on the server-side filesystem.

## Implemented filesystems

- XFS project quota via `xfs_quota`
- ext4 project quota via project attributes / `setquota`
- Btrfs qgroup quota for subvolume targets

## Kubernetes integration

- NFS PV discovery and provisioner filtering
- CSI NFS and native NFS path mapping
- stable project-ID generation
- PV annotation status (`pending`, `applied`, `failed`) and the actual enforced hard limit (`nfs.io/enforced-limit-bytes`)
- DaemonSet deployment on NFS server nodes
- nodeSelector guardrails and RollingUpdate strategy

## Operational capabilities

- Prometheus metrics
- ServiceMonitor integration
- PrometheusRule alerts
- optional Web UI
- audit logging
- usage history
- orphan cleanup with dry-run-first operation
- advisory namespace quota policy views
- Helm chart configuration for server paths, provisioners and runtime settings

## QuotaPolicy status

`QuotaPolicy` is implemented, optional, and disabled by default. The
`quota.nfs.io/v1alpha1` API, generated CRD, reconciliation, status write-back,
and quota-bound resolution are present. Enable enforcement with
`quotaPolicy.enabled=true`; set `quotaPolicy.singleWriter=true` only when one
agent instance owns QuotaPolicy status updates. It is distinct from the older
advisory namespace policy views and does not replace filesystem enforcement.

## First verified success

A healthy DaemonSet is a prerequisite, not the outcome. First verified success
requires a real NFS-backed PV with `nfs.io/quota-status=applied`, a positive
`nfs.io/enforced-limit-bytes` value, a matching server-side quota report, and
an over-limit write rejected by the filesystem. Use a disposable PVC and follow
[`docs/ADOPTION-GUIDE.md`](ADOPTION-GUIDE.md); unit tests and pod readiness do
not establish this evidence class.

## Security / supply chain

The repository includes CI and supply-chain hardening for dependency integrity and network behavior. CI egress is explicitly controlled with an allowlist rather than left unrestricted, and the project has adopted the OpenForge documentation/security/GitHub baseline.

## Current operating boundary

Quota enforcement requires privileged access to the NFS server's local filesystem. The DaemonSet therefore must be constrained to actual NFS server nodes and the hostPath surface should remain limited to the export and quota-related paths.

The namespace policy feature is advisory; it does not silently replace the PV-capacity-to-filesystem-quota enforcement path.

## Related evidence

- `README.md`
- `docs/architecture.md`
- `docs/feature-guide.md`
- `docs/security.md`
- `docs/ha-dr.md`
- `docs/quotapolicy-design.md`
- `docs/ADOPTION-GUIDE.md`
- `charts/nfs-quota-agent/templates/NOTES.txt`
- Helm chart under `charts/nfs-quota-agent/`

This file should be refreshed when a capability changes from planned/experimental to implemented on `main`, or when the supported first-success path changes.
