# NFS Quota Agent

English | [한국어](README_ko.md)

A Kubernetes agent that automatically enforces filesystem project quotas for NFS-based PersistentVolumes. This agent runs on NFS server nodes and ensures storage limits are enforced at the filesystem level. Supports both **XFS** and **ext4** filesystems.

## Overview

When using NFS-based storage in Kubernetes (such as with [csi-driver-nfs](https://github.com/kubernetes-csi/csi-driver-nfs) or [nfs-subdir-external-provisioner](https://github.com/kubernetes-sigs/nfs-subdir-external-provisioner)), storage quotas defined in PersistentVolumeClaims are not enforced at the filesystem level. This agent solves that problem by:

1. Watching for NFS PersistentVolumes in your cluster
2. Automatically applying project quotas based on PV capacity (supports XFS and ext4)
3. Tracking quota status via PV annotations

## Prerequisites

- Kubernetes cluster (v1.20+)
- NFS server with **XFS** or **ext4** filesystem
- Project quota enabled on the NFS export filesystem
- The agent must run on the NFS server node

### Supported Filesystems

| Filesystem | Quota Tool | Mount Option | Min Kernel |
|------------|------------|--------------|------------|
| XFS | `xfs_quota` | `prjquota` | 2.6+ |
| ext4 | `setquota` | `prjquota` | 4.5+ |

### Enabling Project Quota

#### XFS Filesystem

```bash
# Check current mount options
mount | grep xfs

# Remount with project quota (or add to /etc/fstab)
mount -o remount,prjquota /data
```

Add to `/etc/fstab` for persistent configuration:
```
/dev/sdb1  /data  xfs  defaults,prjquota  0 0
```

#### ext4 Filesystem

```bash
# Enable project quota feature (one-time, requires unmount)
umount /data
tune2fs -O project,quota /dev/sdb1
mount /data

# Or remount with prjquota option
mount -o remount,prjquota /data
```

Add to `/etc/fstab` for persistent configuration:
```
/dev/sdb1  /data  ext4  defaults,prjquota  0 0
```

**Note:** ext4 project quota requires Linux kernel 4.5+ and e2fsprogs 1.43+.

## Installation

### 1. Label your NFS server node

```bash
kubectl label node <nfs-server-node> nfs-server=true
```

### 2. Build and push the container image

```bash
# Build locally
make build

# Build and push Docker image
make docker-build docker-push REGISTRY=your-registry.io VERSION=v1.0.0

# Or build multi-arch image
make docker-buildx REGISTRY=your-registry.io VERSION=v1.0.0
```

### 3. Update deployment configuration

Edit `deploy/deployment.yaml` to match your environment:

```yaml
args:
  - --nfs-base-path=/export          # Local mount path in container
  - --nfs-server-path=/data          # NFS server's export path
  - --provisioner-name=cluster.local/nfs-subdir-external-provisioner
  - --sync-interval=30s
volumes:
  - name: nfs-export
    hostPath:
      path: /data                    # Your NFS export path on the server
```

### 4. Deploy to Kubernetes

#### Using Kustomize

```bash
kubectl apply -k deploy/

# Or using make
make deploy
```

#### Using Helm Chart

```bash
# Install with default values
helm install nfs-quota-agent ./charts/nfs-quota-agent \
  --namespace nfs-quota-agent \
  --create-namespace

# Install with custom values
helm install nfs-quota-agent ./charts/nfs-quota-agent \
  --namespace nfs-quota-agent \
  --create-namespace \
  --set config.nfsBasePath=/export \
  --set config.nfsServerPath=/data \
  --set nfsExport.hostPath=/data \
  --set config.provisionerName=cluster.local/nfs-subdir-external-provisioner

# Install with custom values file
helm install nfs-quota-agent ./charts/nfs-quota-agent \
  --namespace nfs-quota-agent \
  --create-namespace \
  -f my-values.yaml

# Upgrade
helm upgrade nfs-quota-agent ./charts/nfs-quota-agent \
  --namespace nfs-quota-agent

# Uninstall
helm uninstall nfs-quota-agent -n nfs-quota-agent
```

#### Helm Chart Values

| Key | Default | Description |
|-----|---------|-------------|
| `image.repository` | `ghcr.io/dasomel/nfs-quota-agent` | Image repository |
| `image.tag` | `""` (appVersion) | Image tag |
| `config.nfsBasePath` | `/export` | Mount path in container |
| `config.nfsServerPath` | `/data` | NFS server export path |
| `config.provisionerName` | `nfs.csi.k8s.io` | Provisioner to filter |
| `config.processAllNFS` | `false` | Process all NFS PVs |
| `config.syncInterval` | `30s` | Sync interval |
| `config.metricsAddr` | `:9090` | Metrics server address |
| `webUI.enabled` | `false` | Enable web UI dashboard |
| `webUI.addr` | `:8080` | Web UI listen address |
| `audit.enabled` | `false` | Enable audit logging |
| `audit.logPath` | `/var/log/nfs-quota-agent/audit.log` | Audit log file path |
| `cleanup.enabled` | `false` | Enable auto orphan cleanup |
| `cleanup.interval` | `1h` | Cleanup run interval |
| `cleanup.gracePeriod` | `24h` | Grace period before deletion |
| `cleanup.dryRun` | `true` | Dry-run mode (no deletion) |
| `history.enabled` | `false` | Enable usage history tracking |
| `history.path` | `/var/lib/nfs-quota-agent/history.json` | History file path |
| `history.interval` | `5m` | History snapshot interval |
| `history.retention` | `720h` | History retention (30 days) |
| `policy.enabled` | `false` | Enable namespace quota policy |
| `policy.defaultQuota` | `1Gi` | Global default quota |
| `policy.enforceMaxQuota` | `false` | Enforce max quota |
| `nfsExport.hostPath` | `/data` | Host path to NFS export |
| `nodeSelector` | `nfs-server: "true"` | Node selector |
| `service.enabled` | `true` | Enable metrics service |
| `service.type` | `ClusterIP` | Service type |
| `service.port` | `9090` | Service port |
| `resources.limits.memory` | `128Mi` | Memory limit |
| `resources.limits.cpu` | `100m` | CPU limit |

## Configuration

### Command Line Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--kubeconfig` | (in-cluster) | Path to kubeconfig file |
| `--nfs-base-path` | `/export` | Local path where NFS export is mounted in container |
| `--nfs-server-path` | `/data` | NFS server's export path |
| `--provisioner-name` | `cluster.local/nfs-subdir-external-provisioner` | Provisioner name to filter PVs (`nfs.csi.k8s.io` for csi-driver-nfs) |
| `--process-all-nfs` | `false` | Process all NFS PVs regardless of provisioner |
| `--sync-interval` | `30s` | Interval between quota synchronization |
| `--metrics-addr` | `:9090` | Address for Prometheus metrics endpoint |
| `--enable-ui` | `false` | Enable integrated web UI dashboard |
| `--ui-addr` | `:8080` | Web UI listen address |
| `--enable-audit` | `false` | Enable audit logging |
| `--audit-log-path` | `/var/log/nfs-quota-agent/audit.log` | Audit log file path |
| `--enable-auto-cleanup` | `false` | Enable automatic orphan directory cleanup |
| `--cleanup-interval` | `1h` | Interval between cleanup runs |
| `--orphan-grace-period` | `24h` | Grace period before deleting orphans |
| `--cleanup-dry-run` | `true` | Dry-run mode (no actual deletion) |
| `--enable-history` | `false` | Enable usage history collection |
| `--history-path` | `/var/lib/nfs-quota-agent/history.json` | Path to store usage history |
| `--history-interval` | `5m` | Interval between history snapshots |
| `--history-retention` | `720h` | How long to keep history data (30 days) |
| `--enable-policy` | `false` | Enable namespace quota policy |
| `--default-quota` | `1Gi` | Global default quota for namespaces |
| `--enforce-max-quota` | `false` | Enforce maximum quota from namespace annotation |

### PV Annotations

The agent uses the following annotations on PersistentVolumes:

| Annotation | Description |
|------------|-------------|
| `nfs.io/project-name` | Custom project name for XFS quota (auto-generated if not set) |
| `nfs.io/quota-status` | Quota status: `pending`, `applied`, or `failed` |

### Namespace Quota Policy

When policy feature is enabled, the agent reads quota limits from Kubernetes native resources with the following priority:

**Priority: LimitRange > Namespace Annotation > Global Default**

#### 1. LimitRange (Recommended)

```yaml
apiVersion: v1
kind: LimitRange
metadata:
  name: storage-limits
  namespace: team-a
spec:
  limits:
  - type: PersistentVolumeClaim
    max:
      storage: 50Gi      # Maximum PVC size
    min:
      storage: 1Gi       # Minimum PVC size
    default:
      storage: 5Gi       # Default PVC size
    defaultRequest:
      storage: 1Gi       # Default request size
```

#### 2. ResourceQuota (Namespace Total)

```yaml
apiVersion: v1
kind: ResourceQuota
metadata:
  name: storage-quota
  namespace: team-a
spec:
  hard:
    requests.storage: 100Gi       # Total storage limit for namespace
    persistentvolumeclaims: 10    # Max number of PVCs
```

#### 3. Namespace Annotations (Fallback)

If no LimitRange is defined, the agent falls back to namespace annotations:

| Annotation | Description |
|------------|-------------|
| `nfs.io/default-quota` | Default quota for PVCs in this namespace (e.g., `10Gi`) |
| `nfs.io/max-quota` | Maximum allowed quota for PVCs in this namespace (e.g., `100Gi`) |

## How It Works

1. **Filesystem Detection**: The agent automatically detects the filesystem type (XFS or ext4) at startup

2. **PV Detection**: The agent watches for NFS PersistentVolumes that are:
   - In `Bound` state
   - Provisioned by the configured provisioner (or all NFS PVs if `--process-all-nfs` is set)
   - Supports both **native NFS PVs** (`pv.Spec.NFS`) and **CSI-based NFS PVs** (`nfs.csi.k8s.io` driver)

3. **Path Mapping**: Converts NFS server paths to local paths:
   - **Native NFS**: Uses `pv.Spec.NFS.Path`
   - **CSI NFS**: Uses `pv.Spec.CSI.VolumeAttributes["share"]` + `["subdir"]`
   - Example: `/data/namespace-pvc-xxx` → `/export/namespace-pvc-xxx`

4. **Project ID Generation**: Creates unique project IDs from PV names using FNV hash

5. **Quota Application**:
   - **XFS**: Uses `xfs_quota` to initialize projects and set block limits
   - **ext4**: Uses `chattr` to set project attributes and `setquota` for limits
   - Creates project entries in `projects` and `projid` files

6. **Status Tracking**: Updates PV annotations to reflect quota status

## Why Run on NFS Server Node?

The agent **must** run on the NFS server node. This is not optional.

### Reason

```
┌─────────────────────────────────────────────────────────────┐
│                     NFS Server Node                         │
│  ┌────────────────────────────────────────────────────────┐ │
│  │              nfs-quota-agent (Pod)                     │ │
│  │                                                        │ │
│  │   xfs_quota / setquota commands                        │ │
│  │              ↓                                         │ │
│  │   hostPath: /data  →  container: /export               │ │
│  └────────────────────────────────────────────────────────┘ │
│                          ↓                                  │
│  ┌────────────────────────────────────────────────────────┐ │
│  │          XFS/ext4 Filesystem (/data)                   │ │
│  │   Project quota can ONLY be set on local filesystem    │ │
│  └────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────┘
```

| Constraint | Description |
|------------|-------------|
| **Quota commands are local-only** | `xfs_quota` and `setquota` only work on local filesystems |
| **NFS client cannot set quotas** | Quota commands do not work over NFS mounts |
| **Direct filesystem access required** | The agent needs hostPath access to the actual disk |

### Configuration (NFS server in cluster)

If your NFS server is a Kubernetes node:

```yaml
# Label your NFS server node
kubectl label node <nfs-server-node> nfs-server=true

# Deployment uses nodeSelector
nodeSelector:
  nfs-server: "true"

# Volume mounts the actual filesystem
volumes:
  - name: nfs-export
    hostPath:
      path: /data  # The actual NFS export directory on the server
```

### Required Volume Mounts

The agent requires specific volume mounts and host access to execute quota commands properly:

| Volume/Setting | Path | Type | Description |
|----------------|------|------|-------------|
| `hostPID` | - | `true` | Required for quota commands to access process information |
| `nfs-export` | Host NFS path | Directory | The actual NFS export directory |
| `dev` | `/dev` | Directory | Access to block devices for quota commands |
| `etc-projects` | `/etc/projects` | FileOrCreate | Project ID to path mapping file |
| `etc-projid` | `/etc/projid` | FileOrCreate | Project name to ID mapping file |

The Helm chart automatically configures these mounts. For manual deployment:

```yaml
spec:
  hostPID: true
  containers:
    - name: nfs-quota-agent
      volumeMounts:
        - name: nfs-export
          mountPath: /export
        - name: dev
          mountPath: /dev
        - name: etc-projects
          mountPath: /etc/projects
        - name: etc-projid
          mountPath: /etc/projid
  volumes:
    - name: nfs-export
      hostPath:
        path: /data  # Your NFS export path
        type: Directory
    - name: dev
      hostPath:
        path: /dev
        type: Directory
    - name: etc-projects
      hostPath:
        path: /etc/projects
        type: FileOrCreate
    - name: etc-projid
      hostPath:
        path: /etc/projid
        type: FileOrCreate
```

**Note:** The `/etc/projects` and `/etc/projid` files are used by XFS and ext4 project quota systems to track project IDs and their associated paths.

### External NFS Server (outside cluster)

If your NFS server is **not** part of the Kubernetes cluster (e.g., NAS appliance, external VM):

#### Option 1: Run agent directly on NFS server (Recommended)

```bash
# On the NFS server (not in Kubernetes)

# Download and run the binary
./nfs-quota-agent \
  --kubeconfig=/path/to/kubeconfig \
  --nfs-base-path=/data \
  --nfs-server-path=/data \
  --sync-interval=30s

# Or run as systemd service
cat <<EOF | sudo tee /etc/systemd/system/nfs-quota-agent.service
[Unit]
Description=NFS Quota Agent
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/nfs-quota-agent \
  --kubeconfig=/etc/kubernetes/admin.conf \
  --nfs-base-path=/data \
  --nfs-server-path=/data
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl enable --now nfs-quota-agent
```

#### Option 2: Run as Docker container on NFS server

```bash
# On the NFS server
docker run -d \
  --name nfs-quota-agent \
  --privileged \
  -v /data:/export \
  -v /path/to/kubeconfig:/kubeconfig:ro \
  nfs-quota-agent:latest \
  --kubeconfig=/kubeconfig \
  --nfs-base-path=/export \
  --nfs-server-path=/data
```

#### Option 3: Add NFS server to cluster

Add the NFS server as a Kubernetes node (worker), then use `nodeSelector`.

```bash
# On NFS server - join the cluster
kubeadm join <control-plane>:6443 --token <token> ...

# Label the node
kubectl label node <nfs-server> nfs-server=true
```

## Architecture

```
┌─────────────────┐     ┌─────────────────────────────────────────────────┐
│   Kubernetes    │     │              NFS Server Node                    │
│    API Server   │     │  ┌─────────────────────────────────────────────┐│
│                 │     │  │           nfs-quota-agent                   ││
│  ┌───────────┐  │     │  │  ┌───────────┐    ┌─────────────────────┐   ││
│  │    PV     │◄─┼─────┼──┼──│  Watcher  │    │  XFS Quota Manager  │   ││
│  │ (NFS type)│  │     │  │  └───────────┘    └─────────────────────┘   ││
│  └───────────┘  │     │  │         │                    │              ││
│                 │     │  │         ▼                    ▼              ││
└─────────────────┘     │  │  ┌─────────────────────────────────────┐    ││
                        │  │  │           xfs_quota                 │    ││
                        │  │  └─────────────────────────────────────┘    ││
                        │  └─────────────────────────────────────────────┘│
                        │                      │                          │
                        │                      ▼                          │
                        │  ┌──────────────────────────────────────────┐   │
                        │  │      XFS Filesystem (/data)              │   │
                        │  │  ┌──────────┐ ┌──────────┐ ┌──────────┐  │   │
                        │  │  │ ns-pvc-1 │ │ ns-pvc-2 │ │ ns-pvc-3 │  │   │
                        │  │  │ quota:1G │ │ quota:5G │ │quota:10G │  │   │
                        │  │  └──────────┘ └──────────┘ └──────────┘  │   │
                        │  └──────────────────────────────────────────┘   │
                        └─────────────────────────────────────────────────┘
```

## CLI Commands

The agent provides several management commands:

```bash
# Run quota enforcement agent (default)
nfs-quota-agent run --nfs-base-path=/export --provisioner-name=nfs.csi.k8s.io

# Show quota status and disk usage
nfs-quota-agent status --path=/data

# Show top directories by usage
nfs-quota-agent top --path=/data -n 10

# Watch mode (refresh every 5s)
nfs-quota-agent top --path=/data --watch

# Generate report in different formats
nfs-quota-agent report --path=/data --format=json
nfs-quota-agent report --path=/data --format=yaml --output=report.yaml
nfs-quota-agent report --path=/data --format=csv --output=quotas.csv

# Cleanup orphaned quotas (dry-run by default)
nfs-quota-agent cleanup --path=/data --kubeconfig=~/.kube/config

# Actually remove orphaned quotas
nfs-quota-agent cleanup --path=/data --kubeconfig=~/.kube/config --dry-run=false

# Force remove without confirmation
nfs-quota-agent cleanup --path=/data --kubeconfig=~/.kube/config --dry-run=false --force

# Start web UI dashboard
nfs-quota-agent ui --path=/data --addr=:8080
```

### Web UI Dashboard

Start the web UI for visual monitoring:

```bash
nfs-quota-agent ui --path=/data --addr=:8080
```

Then open http://localhost:8080 in your browser.

![Dashboard Screenshot](docs/dashboard.png)

Features:
- Real-time disk usage overview with visual progress bars
- PV/PVC binding status display
- Expandable file browser (click rows to view directory contents)
- Orphan directory management with immediate deletion
- Usage trends and history tracking
- Namespace quota policy display
- Audit log viewer
- Search, filter, and sortable tables
- Auto-refresh every 10 seconds

### Example Output

**status command:**
```
NFS Quota Status
================

Path:       /data
Filesystem: xfs
Total:      1.0 TiB
Used:       650.5 GiB (63.5%)
Available:  373.5 GiB

Directory Quotas (45 total)
--------------------------------------------------------------------------------
DIRECTORY                  USED      QUOTA     USED%   STATUS
default-pvc-abc123         8.5 GiB   10 GiB    85.0%   OK
prod-data-xyz789           9.8 GiB   10 GiB    98.0%   WARNING
dev-logs-def456            5.2 GiB   5 GiB     104.0%  EXCEEDED
...
```

**cleanup command:**
```
NFS Quota Cleanup
=================

Path: /data
Mode: DRY-RUN (no changes)

Found 25 NFS PersistentVolumes in cluster

Found 3 orphaned quotas:

PROJECT_ID   PROJECT_NAME              PATH                                     STATUS
------------------------------------------------------------------------------------------
1234567890   pv_old_namespace_pvc_xxx  /data/old-namespace-pvc-xxx              dir missing
2345678901   pv_deleted_app_data       /data/deleted-app-data                   dir exists (2.5 GiB)
3456789012   pv_test_volume            /data/test-volume                        dir missing

Dry-run mode: No changes made.
Run with --dry-run=false to remove orphaned quotas.
```

**top command:**
```
NFS Quota Top - 15:04:05
Path: /data | Total: 1.0 TiB | Used: 650.5 GiB (63.5%) | Free: 373.5 GiB

#   DIRECTORY              USED      QUOTA     USED%   BAR
1   prod-data-xyz789       9.8 GiB   10 GiB    98.0%   [██████████████████░░]!
2   default-pvc-abc123     8.5 GiB   10 GiB    85.0%   [█████████████████░░░]
3   dev-logs-def456        5.2 GiB   5 GiB     104.0%  [████████████████████]!
```

### Prometheus Metrics

The agent exposes metrics on `:9090/metrics`:

```
# Disk metrics
nfs_disk_total_bytes{path="/data"} 1099511627776
nfs_disk_used_bytes{path="/data"} 698488954880
nfs_disk_available_bytes{path="/data"} 401022672896

# Per-directory quota metrics
nfs_quota_used_bytes{directory="prod-data-xyz789"} 10523566080
nfs_quota_limit_bytes{directory="prod-data-xyz789"} 10737418240
nfs_quota_used_percent{directory="prod-data-xyz789"} 98.01

# Summary metrics
nfs_quota_directories_total 45
nfs_quota_warning_count 3
nfs_quota_exceeded_count 1
```

## Usage Examples

### With csi-driver-nfs (Recommended)

[csi-driver-nfs](https://github.com/kubernetes-csi/csi-driver-nfs) is the recommended NFS provisioner for Kubernetes. It follows the CSI standard and supports VolumeSnapshots.

```bash
# Configure agent for csi-driver-nfs
helm install nfs-quota-agent ./charts/nfs-quota-agent \
  --namespace nfs-quota-agent \
  --create-namespace \
  --set config.provisionerName=nfs.csi.k8s.io
```

```yaml
# StorageClass for csi-driver-nfs
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: nfs-csi
provisioner: nfs.csi.k8s.io
parameters:
  server: nfs-server.example.com
  share: /data
reclaimPolicy: Delete
volumeBindingMode: Immediate
mountOptions:
  - nfsvers=4.1
  - hard                    # Retry indefinitely on failure (data safety)
  - noatime                 # Disable access time updates (performance)
  - rsize=1048576           # Read block size 1MB (performance)
  - wsize=1048576           # Write block size 1MB (performance)
---
# PVC using csi-driver-nfs StorageClass
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-data
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: nfs-csi
  resources:
    requests:
      storage: 10Gi  # Quota will be enforced at 10Gi
```

#### Namespace/PVC Name Directory Pattern

The agent supports nested directory structures using namespace and PVC name. This is useful for organizing PV directories by namespace:

```yaml
# StorageClass with namespace/pvc-name subdirectory pattern
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: nfs-csi-organized
provisioner: nfs.csi.k8s.io
parameters:
  server: nfs-server.example.com
  share: /data
  # Creates directories like: /data/default/my-pvc, /data/production/app-data
  subDir: ${pvc.metadata.namespace}/${pvc.metadata.name}
reclaimPolicy: Delete
volumeBindingMode: Immediate
mountOptions:
  - nfsvers=4.1
  - hard
  - noatime
  - rsize=1048576
  - wsize=1048576
```

With this pattern:
- PVC `my-pvc` in namespace `default` → `/data/default/my-pvc`
- PVC `app-data` in namespace `production` → `/data/production/app-data`

The agent automatically handles path conversion:
- NFS server path: `/data/default/my-pvc`
- Local mount path: `/export/default/my-pvc` (with `nfsBasePath=/export`)

#### Recommended Mount Options

| Option | Description |
|--------|-------------|
| `nfsvers=4.1` | Use NFS v4.1 protocol (recommended for stability and features) |
| `hard` | Retry NFS requests indefinitely on failure (ensures data safety) |
| `noatime` | Disable access time updates (improves performance) |
| `rsize=1048576` | Read block size 1MB (improves throughput for large files) |
| `wsize=1048576` | Write block size 1MB (improves throughput for large files) |

**Optional options:**

| Option | Description |
|--------|-------------|
| `soft` | Return error after retries (use instead of `hard` for faster failure) |
| `timeo=600` | Timeout in deciseconds (60 seconds) |
| `retrans=2` | Number of retries before returning error (with `soft`) |
| `nolock` | Disable NFS locking (for legacy applications) |

### With nfs-subdir-external-provisioner (Legacy)

```yaml
# PVC using nfs-subdir-external-provisioner StorageClass
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-data
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: nfs-client
  resources:
    requests:
      storage: 10Gi  # Quota will be enforced at 10Gi
```

### With static NFS PVs

Enable `--process-all-nfs` flag to process manually created NFS PVs:

```yaml
# Static NFS PV
apiVersion: v1
kind: PersistentVolume
metadata:
  name: my-nfs-pv
spec:
  capacity:
    storage: 50Gi  # Quota will be enforced at 50Gi
  accessModes:
    - ReadWriteMany
  nfs:
    server: nfs-server.example.com
    path: /data/my-volume
```

### Verify quota status

```bash
# Check PV annotation
kubectl get pv <pv-name> -o jsonpath='{.metadata.annotations.nfs\.io/quota-status}'

# Check quota on NFS server (XFS)
xfs_quota -x -c "report -p -h" /data

# Check quota on NFS server (ext4)
repquota -P -s /data
```

### Quota exceeded errors

When a Pod exceeds its storage quota, the following errors occur at the filesystem level:

**XFS:**
```bash
$ dd if=/dev/zero of=/data/testfile bs=1M count=2000
dd: error writing '/data/testfile': No space left on device
```

**ext4:**
```bash
$ dd if=/dev/zero of=/data/testfile bs=1M count=2000
dd: error writing '/data/testfile': Disk quota exceeded
```

**In application logs:**
```
Error: write /data/output.log: disk quota exceeded
```

| Situation | errno | Message |
|-----------|-------|---------|
| Block quota exceeded | `EDQUOT` (122) | Disk quota exceeded |
| No space (XFS) | `ENOSPC` (28) | No space left on device |

**Testing quota enforcement:**
```bash
# From inside the Pod (assuming 10Gi PVC)
kubectl exec -it my-pod -- dd if=/dev/zero of=/data/test bs=1M count=15000
# Error occurs after ~10GB written
```

## Development

### Build

```bash
# Build binary
make build

# Build for Linux (multiple architectures)
make build-linux

# Run tests
make test

# Run tests with coverage
make test-coverage

# Format code
make fmt

# Run go vet
make vet

# Run linter (requires golangci-lint)
make lint
```

### Local Testing

```bash
# Run locally with kubeconfig
./bin/nfs-quota-agent \
  --kubeconfig=$HOME/.kube/config \
  --nfs-base-path=/mnt/nfs \
  --nfs-server-path=/data \
  --sync-interval=10s
```

## Troubleshooting

### Quota not being applied

1. Check filesystem type and quota state:
   ```bash
   # For XFS
   xfs_quota -x -c "state" /data

   # For ext4
   quotaon -p /data
   tune2fs -l /dev/sdb1 | grep -i project
   ```

2. Verify agent logs:
   ```bash
   kubectl logs -n nfs-quota-agent deployment/nfs-quota-agent
   ```

3. Check PV annotations for errors:
   ```bash
   kubectl get pv -o jsonpath='{range .items[*]}{.metadata.name}: {.metadata.annotations.nfs\.io/quota-status}{"\n"}{end}'
   ```

### Permission denied errors

The agent requires privileged access to run quota commands. Ensure:
- The container has `privileged: true` security context
- The NFS export directory is correctly mounted

### ext4-specific issues

1. Ensure project quota feature is enabled:
   ```bash
   tune2fs -l /dev/sdb1 | grep "Filesystem features"
   # Should include: project
   ```

2. Check if quota is mounted:
   ```bash
   mount | grep prjquota
   ```

3. Verify e2fsprogs version (requires 1.43+):
   ```bash
   tune2fs -V
   ```

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) for details.

## Contributing

Contributions are welcome! Please feel free to submit issues and pull requests.
