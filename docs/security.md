# Security Threat Model and Privilege Analysis

This document provides a security threat model and minimum-privilege assessment for `nfs-quota-agent`. All statements and privilege requirements are grounded in empirical codebase analysis of the Helm chart and Go package sources.

## 1. Why Privileges Are Required

`nfs-quota-agent` operates on the NFS server node to manage XFS, ext4, and Btrfs filesystem project quotas for Kubernetes `PersistentVolume` resources. The table below lists every security privilege and host mount configured in [daemonset.yaml](../charts/nfs-quota-agent/templates/daemonset.yaml), the internal operations requiring it, and the operational impact if omitted.

| Privilege / Mount | Location in Code / Helm Chart | Specific Code Operation Requiring It | Consequence If Omitted / Disabled | Status / Verdict |
| :--- | :--- | :--- | :--- | :--- |
| `privileged: true` | [values.yaml](../charts/nfs-quota-agent/values.yaml)<br>[daemonset.yaml](../charts/nfs-quota-agent/templates/daemonset.yaml) | Issuing `quotactl`, `xfsctl`, `FS_IOC_SETFLAGS` (`chattr +P`), and Btrfs qgroup ioctls via external binaries (`xfs_quota`, `setquota`, `chattr`, `btrfs`). | Quota binaries fail with `EPERM` (Operation not permitted) or `EACCES` when attempting filesystem ioctls. | Required unless replaced by explicit capabilities. |
| `hostPID: true` | [daemonset.yaml](../charts/nfs-quota-agent/templates/daemonset.yaml) | **None.** Code inspection reveals zero references to `/proc`, host PIDs, process inspection, or `nsenter`. | None. No code path relies on the host PID namespace. | **UNNECESSARY. Candidate for removal.** |
| `/data` (`nfsExport.hostPath`) | [daemonset.yaml](../charts/nfs-quota-agent/templates/daemonset.yaml)<br>[daemonset.yaml](../charts/nfs-quota-agent/templates/daemonset.yaml) | `os.Stat` checks in [agent.go](../internal/agent/agent.go), `filepath.WalkDir` in [ext4.go](../internal/quota/ext4.go), and `GetDirSize` in [dir.go](../internal/status/dir.go). | Agent cannot stat PVC export paths or apply project directory attributes. | Required. |
| `/dev` hostPath mount | [daemonset.yaml](../charts/nfs-quota-agent/templates/daemonset.yaml)<br>[daemonset.yaml](../charts/nfs-quota-agent/templates/daemonset.yaml) | `xfs_quota`, `setquota`, `findmnt`, and `df` inspect block device nodes in `/dev` to identify backing filesystem devices. | `xfs_quota` and `setquota` fail to map mount paths to underlying block devices. | Required, but scope can be narrowed. |
| `/etc/projects` hostPath mount | [daemonset.yaml](../charts/nfs-quota-agent/templates/daemonset.yaml)<br>[daemonset.yaml](../charts/nfs-quota-agent/templates/daemonset.yaml) | `AddProject`, `RemoveLineFromFile`, and `ReadProjectsFile` in [project.go](../internal/quota/project.go) write/read `projectID:path` entries. | XFS/ext4 project quota mapping fails; host filesystem utilities lose project path metadata. | Required for XFS & ext4. |
| `/etc/projid` hostPath mount | [daemonset.yaml](../charts/nfs-quota-agent/templates/daemonset.yaml)<br>[daemonset.yaml](../charts/nfs-quota-agent/templates/daemonset.yaml) | `AddProject`, `RemoveLineFromFile`, `ReadProjidFile`, and `loadExistingProjectIDs` in [agent.go](../internal/agent/agent.go) write/read `projectName:projectID` entries. | Project ID generation and collision resolution against existing host IDs fail. | Required for XFS & ext4. |
| `/var/log/nfs-quota-agent` | [daemonset.yaml](../charts/nfs-quota-agent/templates/daemonset.yaml)<br>[daemonset.yaml](../charts/nfs-quota-agent/templates/daemonset.yaml) | Persists structured JSON audit logs emitted by `audit.Logger` in [logger.go](../internal/audit/logger.go). | Audit logs are lost when the container restarts. | Optional (enabled via `audit.enabled`). |
| `/var/lib/nfs-quota-agent` (`state.hostPath`) | [daemonset.yaml](../charts/nfs-quota-agent/templates/daemonset.yaml)<br>[daemonset.yaml](../charts/nfs-quota-agent/templates/daemonset.yaml) | Two uses: (1) `RemoveLineFromFile` / `RecoverProjectFile` in [project.go](../internal/quota/project.go) keep a `<name>.bak` crash-recovery sidecar here for `/etc/projects` and `/etc/projid` before rewriting them in place — see `--state-dir` in [main.go](../cmd/nfs-quota-agent/main.go). (2) When enabled, `history.Store` in [store.go](../internal/history/store.go) persists historical usage snapshots here. | Without this mount, a crash mid-rewrite of `/etc/projects`/`/etc/projid` has no backup to recover from (the sidecar would otherwise land in the container's own ephemeral `/etc`, since only the individual files are bind-mounted, and be lost on restart). If `history.enabled`, usage history trend data is also lost on container restart. | **Required**, mounted unconditionally (not gated on `history.enabled`) precisely so the recovery sidecar has somewhere durable to land regardless of whether history collection is on. |

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
*Verdict*: **Not Load-Bearing.** `hostPID: true` is set on line 53 of [daemonset.yaml](../charts/nfs-quota-agent/templates/daemonset.yaml). A thorough search of all Go source files confirms zero interactions with host process tables, `/proc`, or process namespace operations. Removing `hostPID: true` reduces container blast radius with zero functional regression.

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
| `""` | `namespaces` | `get`, `list`, `watch` | `Namespaces().Get` / `.List` for policy annotations ([policy.go](../internal/policy/policy.go)). | Read-only. Used. |
| `""` | `limitranges` | `get`, `list`, `watch` | `LimitRanges().List` for default PVC storage limits ([policy.go](../internal/policy/policy.go)). | Read-only. Used. |
| `""` | `resourcequotas` | `get`, `list`, `watch` | `ResourceQuotas().List` for namespace storage caps ([policy.go](../internal/policy/policy.go)). | Read-only. Used. |

*Verdict*: The four remaining rules correspond exactly to the four resources the code actually calls — `PersistentVolumes`, `Namespaces`, `LimitRanges`, `ResourceQuotas`. No wildcard (`*`) verbs, no `delete`, and the only write access is `update`/`patch` on PVs for the status annotation.

Two rules were removed as verifiably unused: `storageclasses` (provisioner matching compares `pv.Spec.CSI.Driver` against the configured name directly, never consulting the StorageClass) and `persistentvolumeclaims` (claim identity comes from `pv.Spec.ClaimRef` on the PV object already retrieved). The latter mattered more than "unused" suggests — PVCs are namespaced, user-facing objects, and the grant was cluster-wide read, so a compromised agent could enumerate every claim in the cluster.

If you maintain a fork that added a consumer for either, re-add the corresponding rule; removing a grant the code never exercises cannot break upstream behavior.

---

## 5. Pod Security Admission (PSA) Compliance

Because the agent requires `hostPath` volumes (`/data`, `/dev`, `/etc/projects`, `/etc/projid`, `/var/lib/nfs-quota-agent`) and root/capability execution, the deployment **cannot** satisfy Kubernetes `restricted` or `baseline` Pod Security Admission standards.

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

0. **Single Instance Per NFS Server Node** (structural, not advisory):
   The agent owns host state — `/etc/projects` and `/etc/projid` — and has **no leader election and no file locking**. `AppendToFile` ([project.go](../internal/quota/project.go)) reads the file, checks whether the key is present, then appends; two instances racing that sequence both observe the key as absent and both append. Project-ID allocation reads the existing IDs for collision avoidance and is exposed to the same race, so two agents can map one ID to two different paths.

   The chart runs `nfs-quota-agent` as a `DaemonSet` ([daemonset.yaml](../charts/nfs-quota-agent/templates/daemonset.yaml)) rather than a `Deployment`, so Kubernetes itself guarantees at most one pod per matching node — there is no `replicaCount` knob to misconfigure into a race. `nodeSelector` is what decides *which* nodes get a pod, and now doubles as the isolation boundary between NFS server nodes and everything else: the chart rejects an empty `nodeSelector` at render time, since on a DaemonSet that would otherwise schedule the privileged agent onto every node in the cluster instead of just the intended NFS server node(s). To manage several NFS servers, label each of them with the matching selector (or run a separate release per node if their configs differ) — each pod only ever contends for the files on its own node.

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
