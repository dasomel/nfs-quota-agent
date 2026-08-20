# Security Threat Model and Privilege Analysis

This document provides a security threat model and minimum-privilege assessment for `nfs-quota-agent`. All statements and privilege requirements are grounded in empirical codebase analysis of the Helm chart and Go package sources.

## 1. Why Privileges Are Required

`nfs-quota-agent` operates on the NFS server node to manage XFS, ext4, and Btrfs filesystem project quotas for Kubernetes `PersistentVolume` resources. The table below lists every security privilege and host mount configured in [deployment.yaml](../charts/nfs-quota-agent/templates/deployment.yaml), the internal operations requiring it, and the operational impact if omitted.

| Privilege / Mount | Location in Code / Helm Chart | Specific Code Operation Requiring It | Consequence If Omitted / Disabled | Status / Verdict |
| :--- | :--- | :--- | :--- | :--- |
| `privileged: true` | [values.yaml](../charts/nfs-quota-agent/values.yaml)<br>[deployment.yaml](../charts/nfs-quota-agent/templates/deployment.yaml) | Issuing `quotactl`, `xfsctl`, `FS_IOC_SETFLAGS` (`chattr +P`), and Btrfs qgroup ioctls via external binaries (`xfs_quota`, `setquota`, `chattr`, `btrfs`). | Quota binaries fail with `EPERM` (Operation not permitted) or `EACCES` when attempting filesystem ioctls. | Required unless replaced by explicit capabilities. |
| `hostPID: true` | [deployment.yaml](../charts/nfs-quota-agent/templates/deployment.yaml) | **None.** Code inspection reveals zero references to `/proc`, host PIDs, process inspection, or `nsenter`. | None. No code path relies on the host PID namespace. | **UNNECESSARY. Candidate for removal.** |
| `/data` (`nfsExport.hostPath`) | [deployment.yaml](../charts/nfs-quota-agent/templates/deployment.yaml)<br>[deployment.yaml](../charts/nfs-quota-agent/templates/deployment.yaml) | `os.Stat` checks in [agent.go](../internal/agent/agent.go), `filepath.WalkDir` in [ext4.go](../internal/quota/ext4.go), and `GetDirSize` in [dir.go](../internal/status/dir.go). | Agent cannot stat PVC export paths or apply project directory attributes. | Required. |
| `/dev` hostPath mount | [deployment.yaml](../charts/nfs-quota-agent/templates/deployment.yaml)<br>[deployment.yaml](../charts/nfs-quota-agent/templates/deployment.yaml) | `xfs_quota`, `setquota`, `findmnt`, and `df` inspect block device nodes in `/dev` to identify backing filesystem devices. | `xfs_quota` and `setquota` fail to map mount paths to underlying block devices. | Required, but scope can be narrowed. |
| `/etc/projects` hostPath mount | [deployment.yaml](../charts/nfs-quota-agent/templates/deployment.yaml)<br>[deployment.yaml](../charts/nfs-quota-agent/templates/deployment.yaml) | `AddProject`, `RemoveLineFromFile`, and `ReadProjectsFile` in [project.go](../internal/quota/project.go) write/read `projectID:path` entries. | XFS/ext4 project quota mapping fails; host filesystem utilities lose project path metadata. | Required for XFS & ext4. |
| `/etc/projid` hostPath mount | [deployment.yaml](../charts/nfs-quota-agent/templates/deployment.yaml)<br>[deployment.yaml](../charts/nfs-quota-agent/templates/deployment.yaml) | `AddProject`, `RemoveLineFromFile`, `ReadProjidFile`, and `loadExistingProjectIDs` in [agent.go](../internal/agent/agent.go) write/read `projectName:projectID` entries. | Project ID generation and collision resolution against existing host IDs fail. | Required for XFS & ext4. |
| `/var/log/nfs-quota-agent` | [deployment.yaml](../charts/nfs-quota-agent/templates/deployment.yaml)<br>[deployment.yaml](../charts/nfs-quota-agent/templates/deployment.yaml) | Persists structured JSON audit logs emitted by `audit.Logger` in [logger.go](../internal/audit/logger.go). | Audit logs are lost when the container restarts. | Optional (enabled via `audit.enabled`). |
| `/var/lib/nfs-quota-agent` | [deployment.yaml](../charts/nfs-quota-agent/templates/deployment.yaml)<br>[deployment.yaml](../charts/nfs-quota-agent/templates/deployment.yaml) | Persists historical usage snapshots in `history.Store` in [store.go](../internal/history/store.go). | Usage history trend data is lost on container restart. | Optional (enabled via `history.enabled`). |

---

## 2. Minimum-Privilege Alternatives

### Evaluation of Linux Capabilities vs. `privileged: true`
Full `privileged: true` grants all Linux capabilities and disables seccomp/AppArmor isolation. A least-privilege configuration should replace `privileged: true` with targeted Linux capabilities:

1. **`CAP_SYS_ADMIN`**: Required for executing `quotactl()` syscalls, XFS ioctls (`XFS_IOC_FSSETDM`), `chattr +P` (`FS_IOC_SETFLAGS`), and Btrfs qgroup ioctls.
2. **`CAP_DAC_OVERRIDE` & `CAP_FOWNER`**: Required to traverse, inspect, and set project attributes across directories owned by arbitrary UID/GID combinations on the exported NFS share.
3. **`CAP_SYS_RESOURCE`**: Required on certain kernel configurations to override system-wide quota resource limits.

#### Proposed Non-Privileged `securityContext`
```yaml
securityContext:
  privileged: false
  allowPrivilegeEscalation: false
  capabilities:
    drop:
      - ALL
    add:
      - SYS_ADMIN
      - DAC_OVERRIDE
      - FOWNER
      - SYS_RESOURCE
  readOnlyRootFilesystem: false  # Needs write access to /etc/projects, /etc/projid, and log paths
  runAsNonRoot: false            # Host quota ioctls and /etc file writes require UID 0
```

*Verdict*: Granular capability dropping is theoretically viable. However, external binaries such as `xfs_quota` and `btrfs` may perform device identification against host mount namespaces. If running in an unprivileged container namespace, `xfs_quota` may fail unless the container shares mount contexts or raw device access. Empirical host kernel testing is required before enforcing `privileged: false` in production releases.

### Analysis of `hostPID: true`
*Verdict*: **Not Load-Bearing.** `hostPID: true` is set on line 46 of [deployment.yaml](../charts/nfs-quota-agent/templates/deployment.yaml). A thorough search of all Go source files confirms zero interactions with host process tables, `/proc`, or process namespace operations. Removing `hostPID: true` reduces container blast radius with zero functional regression.

### Narrowing the `/dev` Mount
*Verdict*: **Viable.** Mounting the entire host `/dev` directory (`hostPath: /dev`) exposes all host block devices, serial ports, pseudo-terminals, and device nodes (e.g., `/dev/mem`, `/dev/kmem`). Where the NFS export resides on a known host disk (e.g., `/dev/sda1` or `/dev/nvme0n1p1`), the volume mount should be constrained to that explicit device node using `hostPath.type: BlockDevice`.

---

## 3. Threat Model

### Attack Surfaces and Blast Radius

```
+-----------------------------------------------------------------------------------+
| Kubernetes Control Plane / User Space                                            |
|                                                                                   |
|  [ Hostile User ] ---> Creates PV/PVC ---> [ PV Annotation: nfs.io/project-name ] |
|                                                              |                    |
+--------------------------------------------------------------|--------------------+
                                                               v
+-----------------------------------------------------------------------------------+
| Agent Container Boundary                                                          |
|                                                                                   |
|  [ validateQuotaArg ] ---> Cleans Input ---> [ CommandRunner: exec.Command ]      |
|                                                              |                    |
|  [ Host Path Mapping ] ---> pvpath.ToLocal                   v                    |
|                                                     [ Host Binaries ]             |
|                                                (xfs_quota / setquota / btrfs)     |
+--------------------------------------------------------------|--------------------+
                                                               v
+-----------------------------------------------------------------------------------+
| Host NFS Server Node Boundary                                                     |
|                                                                                   |
|  [ /data Mount ] ------------> /export Directory Tree                             |
|  [ /etc/projects, projid ] ---> Host Quota Filesystem Metadata                    |
|  [ Kernel Filesystem ] -------> Block Device & Quota Subsystems (ioctl)           |
+-----------------------------------------------------------------------------------+
```

#### Vector 1: Compromised Agent Container
* **Attacker Capability**: Execution of arbitrary shell commands or code inside the `nfs-quota-agent` container.
* **Blast Radius under Current Config (`privileged: true`, `hostPID: true`, `hostPath: /dev`)**: High. The attacker can access all host block devices in `/dev`, inspect host processes via `hostPID`, and execute raw kernel exploits to achieve full host node compromise.
* **Blast Radius under Hardened Config**: Medium-Low. With `hostPID: false`, narrowed `/dev`, and capability dropping, attacker reach is confined to modifying files on the mounted `/data` NFS export and altering host `/etc/projects` and `/etc/projid` files.

#### Vector 2: Malicious PV Annotations (`nfs.io/project-name`) Reaching Binary `argv`
* **Attacker Input Source**: Untrusted metadata in `pv.Annotations["nfs.io/project-name"]` parsed in [agent.go](../internal/agent/agent.go).
* **Validation Defense**: Inputs are checked by `validateQuotaArg()` in [validate.go](../internal/quota/validate.go):
  ```go
  func validateQuotaArg(kind, value string) error {
      if value == "" { return fmt.Errorf(...) }
      for _, r := range value {
          if unicode.IsSpace(r) || r == '\'' || r == '"' || unicode.IsControl(r) {
              return fmt.Errorf(...)
          }
      }
      return nil
  }
  ```
* **Execution Safety**: All binary invocations pass through `CommandRunner` in [runner.go](../internal/quota/runner.go) using `exec.Command(name, args...)`. Arguments are passed as discrete strings to the `execve()` system call without subshell invocation (`sh -c`).
* **Security Assessment**: Robust against command injection, shell metacharacter manipulation, and argument splitting.

#### Vector 3: Malicious PersistentVolume Path Traversal
* **Attacker Input Source**: Malicious `spec.nfs.path` or CSI `subDir` attribute containing relative path components (e.g., `/data/../../etc`).
* **Path Conversion Logic**: [pvpath.go](../internal/pvpath/pvpath.go) converts NFS paths via `filepath.Join`.
* **Identified Vulnerability Gap**:
  While `cleanup.go:L162` explicitly enforces root containment using `pvpath.Contains(basePath, cleanPath)`, [agent.go](../internal/agent/agent.go) calls `a.nfsPathToLocal(nfsPath)` without checking `pvpath.Contains`. If a crafted PV path evaluates to `/etc`, `ensureQuota` will attempt to stat and apply project quota logic to host directories outside the intended `/export` root.
  *Remediation*: Add `if !pvpath.Contains(a.nfsBasePath, localPath)` guard in `agent.go:ensureQuota`.

---

## 4. RBAC Surface Analysis

The agent uses a cluster-scoped `ClusterRole` defined in [clusterrole.yaml](../charts/nfs-quota-agent/templates/clusterrole.yaml).

| API Group | Resource | Verbs | Justification in Code | RBAC Scope Assessment |
| :--- | :--- | :--- | :--- | :--- |
| `""` | `persistentvolumes` | `get`, `list`, `watch`, `update`, `patch` | `syncAllQuotas` lists PVs ([agent.go](../internal/agent/agent.go)); `updateQuotaStatus` updates `nfs.io/quota-status` annotation ([agent.go](../internal/agent/agent.go)). | Tightly scoped. `update`/`patch` is strictly required for writing quota status annotations. |
| `""` | `persistentvolumeclaims` | `get`, `list`, `watch` | **None found.** No `PersistentVolumeClaims(...)` client call exists anywhere in `internal/` or `cmd/`. Claim identity is read from `pv.Spec.ClaimRef`, which is part of the PV object already retrieved. | **Unused — candidate for removal.** |
| `storage.k8s.io` | `storageclasses` | `get`, `list`, `watch` | **None found.** No `StorageClasses(...)` client call and no reference to `storageClassName` exists. Provisioner matching compares `pv.Spec.CSI.Driver` against the configured name directly ([agent.go](../internal/agent/agent.go)), never consulting the StorageClass. | **Unused — candidate for removal.** |
| `""` | `namespaces` | `get`, `list`, `watch` | `Namespaces().Get` / `.List` for policy annotations ([policy.go](../internal/policy/policy.go), [policy.go](../internal/policy/policy.go)). | Read-only. Used. |
| `""` | `limitranges` | `get`, `list`, `watch` | `LimitRanges().List` for default PVC storage limits ([policy.go](../internal/policy/policy.go), [policy.go](../internal/policy/policy.go)). | Read-only. Used. |
| `""` | `resourcequotas` | `get`, `list`, `watch` | `ResourceQuotas().List` for namespace storage caps ([policy.go](../internal/policy/policy.go), [policy.go](../internal/policy/policy.go)). | Read-only. Used. |

*Verdict*: No wildcard (`*`) verbs and no `delete` on cluster resources, and the only write access is `update`/`patch` on PVs for the status annotation — so the footprint is narrow in kind. It is **not** minimal in extent: two of the six rules (`persistentvolumeclaims`, `storageclasses`) grant cluster-wide read access that no code path exercises. Removing them costs nothing today.

Note that `persistentvolumeclaims` read access is cluster-wide and PVCs are namespaced user-facing objects, so this grant is more meaningful than its unused status suggests — it lets a compromised agent enumerate every claim in the cluster. Verify against your own fork before removing, in case a downstream change added a consumer.

---

## 5. Pod Security Admission (PSA) Compliance

Because the agent requires `hostPath` volumes (`/data`, `/dev`, `/etc/projects`, `/etc/projid`) and root/capability execution, the deployment **cannot** satisfy Kubernetes `restricted` or `baseline` Pod Security Admission standards.

It requires the **`privileged`** PSA profile.

### Recommended Namespace Security Policy Manifest
Cluster operators should deploy the agent into a dedicated namespace labeled with `privileged` PSA enforcement:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: nfs-quota-agent
  labels:
    pod-security.kubernetes.io/enforce: privileged
    pod-security.kubernetes.io/enforce-version: latest
    pod-security.kubernetes.io/audit: privileged
    pod-security.kubernetes.io/warn: privileged
```

### Risk Isolation for Cluster Operators
Granting a `privileged` namespace exception exposes only the designated NFS server node to risk, provided `nodeSelector` pinning is configured:
* The agent is constrained to NFS nodes via `nodeSelector: nfs-server: "true"` ([values.yaml](../charts/nfs-quota-agent/values.yaml)).
* General application workloads running on worker nodes cannot leverage the agent's privilege context.

---

## 6. Operational Hardening Guidance

1. **Dedicated Node Isolation**:
   Always run `nfs-quota-agent` on dedicated NFS server nodes. Apply node taints (e.g., `nfs-server=true:NoSchedule`) and matching tolerations to prevent tenant workloads from co-locating on the NFS node.
2. **Restrict PV Creation Access**:
   Ensure Kubernetes RBAC prevents regular users from creating or patching `PersistentVolume` objects directly. PV creation must be restricted to automated CSI provisioners.
3. **Path Traversal Sanity Gate**:
   Implement an admission webhook or Kyverno rule validating that `spec.nfs.path` and CSI `subDir` attributes do not contain `..` directory traversal sequences.
4. **Audit Logging & Monitoring**:
   * Enable agent audit logging via `--enable-audit` ([values.yaml](../charts/nfs-quota-agent/values.yaml)).
   * Monitor Prometheus metrics for `QuotaStatusFailed` conditions.
   * Configure host-level `auditd` monitoring on `/etc/projects` and `/etc/projid` to detect unauthorized out-of-band modifications.

---

## 7. Unverified / Needs Confirmation

1. **Empirical Capability Testing**: Verification whether `privileged: false` with `SYS_ADMIN`, `DAC_OVERRIDE`, `FOWNER`, `SYS_RESOURCE` fully succeeds for `xfs_quota` and `btrfs` without host mount namespace access requires testing on live RHEL/XFS and Ubuntu/ext4 kernel nodes.
2. **`/dev` Granular Device Mount**: Confirming whether `xfs_quota` operates correctly when mounted with only specific block device nodes (e.g., `/dev/sda1`) rather than host `/dev` depends on system `libxcmd` storage device enumeration behaviors.
