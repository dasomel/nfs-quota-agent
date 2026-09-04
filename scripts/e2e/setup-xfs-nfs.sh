#!/usr/bin/env bash
set -euo pipefail

# scripts/e2e/setup-xfs-nfs.sh
# Sets up a 2 GiB XFS filesystem with prjquota and exports it via nfs-kernel-server.
# Stage A of Issue #5 air-gap E2E testing.

EXPORT_DIR="${EXPORT_DIR:-/srv/nfs-export}"
IMG_FILE="${IMG_FILE:-/tmp/xfs-nfs.img}"
IMG_SIZE="${IMG_SIZE:-2G}"

SUDO=""
if [ "$(id -u)" -ne 0 ]; then
  SUDO="sudo"
fi

echo "=== Stage A: Host Filesystem Setup (XFS prjquota + NFS) ==="

# 1. Ensure required tools are installed on the host
echo "Checking required host tools..."
for tool in mkfs.xfs xfs_quota exportfs findmnt; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "Host tool $tool is missing. Installing nfs-kernel-server, nfs-common, and xfsprogs..."
    $SUDO apt-get update
    $SUDO apt-get install -y nfs-kernel-server nfs-common xfsprogs
    break
  fi
done

# 2. Create sparse file
echo "Creating $IMG_SIZE sparse file at $IMG_FILE..."
$SUDO rm -f "$IMG_FILE"
$SUDO truncate -s "$IMG_SIZE" "$IMG_FILE"

# 3. Format with XFS
echo "Formatting $IMG_FILE as XFS..."
$SUDO mkfs.xfs -f "$IMG_FILE"

# 4. Mount with prjquota / pquota
echo "Mounting $IMG_FILE at $EXPORT_DIR with -o pquota..."
$SUDO mkdir -p "$EXPORT_DIR"
if findmnt -n "$EXPORT_DIR" >/dev/null 2>&1; then
  $SUDO umount "$EXPORT_DIR" || true
fi

if ! $SUDO mount -o loop,pquota "$IMG_FILE" "$EXPORT_DIR" && ! $SUDO mount -o loop,prjquota "$IMG_FILE" "$EXPORT_DIR"; then
  echo "FAIL: Failed to mount XFS filesystem with pquota/prjquota!" >&2
  echo "Kernel log:" >&2
  dmesg | tail -n 50 >&2
  exit 1
fi

# 5. Verify mount options
echo "Verifying mount options on $EXPORT_DIR..."
MOUNT_OPTS=$($SUDO findmnt -n -o OPTIONS "$EXPORT_DIR" || true)
echo "Mounted options: $MOUNT_OPTS"
if [[ "$MOUNT_OPTS" != *"prjquota"* && "$MOUNT_OPTS" != *"pquota"* ]]; then
  echo "FAIL: pquota/prjquota is not present in mount options for $EXPORT_DIR!" >&2
  echo "Kernel log:" >&2
  dmesg | tail -n 50 >&2
  exit 1
fi
echo "OK: $EXPORT_DIR is mounted with pquota/prjquota"

# 6. Prepare PVC directory and host project mapping files
echo "Preparing $EXPORT_DIR/pvc-e2e directory..."
$SUDO mkdir -p "$EXPORT_DIR/pvc-e2e"
$SUDO chmod 777 "$EXPORT_DIR/pvc-e2e"

echo "Ensuring /etc/projects and /etc/projid exist on host..."
$SUDO touch /etc/projects /etc/projid
$SUDO chmod 666 /etc/projects /etc/projid

# 7. Configure and restart NFS kernel server and rpcbind
echo "Configuring nfs-kernel-server export..."
$SUDO mkdir -p /etc/exports.d
EXPORTS_FILE="/etc/exports.d/nfs-quota-agent-e2e.exports"
# fsid=0 would make $EXPORT_DIR itself the NFSv4 pseudo-root, so a v4 client
# asking for the real path ($EXPORT_DIR/pvc-e2e) would only find it at the
# pseudo-root-relative path (/pvc-e2e) and the mount would fail. Modern
# nfs-kernel-server auto-creates the v4 pseudo-root from real paths, so omit
# fsid=0 and export the real path for both v3 and v4 clients.
echo "$EXPORT_DIR *(rw,sync,no_root_squash,no_subtree_check,insecure)" | $SUDO tee "$EXPORTS_FILE" >/dev/null

echo "Starting rpcbind and nfs services..."
if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet systemd 2>/dev/null; then
  $SUDO systemctl enable --now rpcbind || true
  $SUDO systemctl restart rpcbind || true
  $SUDO systemctl enable --now nfs-server || $SUDO systemctl enable --now nfs-kernel-server || true
  $SUDO systemctl restart nfs-server || $SUDO systemctl restart nfs-kernel-server || true
else
  $SUDO service rpcbind restart || true
  $SUDO service nfs-kernel-server restart || true
fi

echo "Exporting filesystems..."
$SUDO exportfs -ra

echo "Active NFS exports (exportfs -v):"
$SUDO exportfs -v

echo "Stage A setup complete."
