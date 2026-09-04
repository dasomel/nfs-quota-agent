#!/usr/bin/env bash
set -euo pipefail

# scripts/e2e/setup-fs-nfs.sh
# Sets up a 2 GiB filesystem (xfs, ext4, or btrfs) with quota support and exports it via nfs-kernel-server.
# Parameterized host setup for Issue #149 air-gap E2E matrix.

FS="${1:-${FS:-xfs}}"
EXPORT_DIR="${EXPORT_DIR:-/srv/nfs-export}"
IMG_FILE="${IMG_FILE:-/tmp/${FS}-nfs.img}"
IMG_SIZE="${IMG_SIZE:-2G}"

SUDO=""
if [ "$(id -u)" -ne 0 ]; then
  SUDO="sudo"
fi

echo "=== Stage A: Host Filesystem Setup ($FS + NFS) ==="

case "$FS" in
  xfs)
    # Keep XFS setup byte-identical via the established setup-xfs-nfs.sh
    exec bash "$(dirname "$0")/setup-xfs-nfs.sh"
    ;;

  ext4)
    # 1. Ensure required tools are installed on the host
    echo "Checking required ext4 host tools..."
    for tool in mkfs.ext4 repquota setquota chattr lsattr exportfs findmnt; do
      if ! command -v "$tool" >/dev/null 2>&1; then
        echo "Host tool $tool is missing. Installing nfs-kernel-server, nfs-common, e2fsprogs, quota..."
        $SUDO apt-get update
        $SUDO apt-get install -y nfs-kernel-server nfs-common e2fsprogs quota
        break
      fi
    done

    # 2. Ensure quota kernel modules are loaded (linux-modules-extra for quota_tree / quota_v2)
    echo "Ensuring ext4 quota kernel modules (quota_tree, quota_v2) are loaded..."
    if ! lsmod | grep -q quota_tree; then
      if command -v apt-get >/dev/null 2>&1; then
        echo "Installing linux-modules-extra for $(uname -r)..."
        $SUDO apt-get update || true
        $SUDO apt-get install -y --no-install-recommends "linux-modules-extra-$(uname -r)" || true
      fi
      $SUDO modprobe quota_tree || true
      $SUDO modprobe quota_v2 || true
    fi

    # 3. Create sparse file
    echo "Creating $IMG_SIZE sparse file at $IMG_FILE..."
    $SUDO rm -f "$IMG_FILE"
    $SUDO truncate -s "$IMG_SIZE" "$IMG_FILE"

    # 4. Format with ext4 with project and quota features enabled
    echo "Formatting $IMG_FILE as ext4 (-O project,quota)..."
    $SUDO mkfs.ext4 -F -O project,quota "$IMG_FILE"

    # 5. Mount with prjquota
    echo "Mounting $IMG_FILE at $EXPORT_DIR with -o prjquota..."
    $SUDO mkdir -p "$EXPORT_DIR"
    if findmnt -n "$EXPORT_DIR" >/dev/null 2>&1; then
      $SUDO umount "$EXPORT_DIR" || true
    fi

    if ! $SUDO mount -o loop,prjquota "$IMG_FILE" "$EXPORT_DIR"; then
      echo "FAIL: Failed to mount ext4 filesystem with prjquota!" >&2
      echo "Kernel log:" >&2
      $SUDO dmesg | tail -n 50 >&2 || true
      exit 1
    fi

    # 6. Verify mount options
    echo "Verifying mount options on $EXPORT_DIR..."
    MOUNT_OPTS=$($SUDO findmnt -n -o OPTIONS "$EXPORT_DIR" || true)
    echo "Mounted options: $MOUNT_OPTS"
    if [[ "$MOUNT_OPTS" != *"prjquota"* ]]; then
      echo "FAIL: prjquota is not present in mount options for $EXPORT_DIR!" >&2
      echo "Kernel log:" >&2
      $SUDO dmesg | tail -n 50 >&2 || true
      exit 1
    fi
    echo "OK: $EXPORT_DIR is mounted with prjquota"

    # 7. Prepare PVC directory and host project mapping files
    echo "Preparing $EXPORT_DIR/pvc-e2e directory..."
    $SUDO mkdir -p "$EXPORT_DIR/pvc-e2e"
    $SUDO chmod 777 "$EXPORT_DIR/pvc-e2e"

    echo "Ensuring /etc/projects and /etc/projid exist on host..."
    $SUDO touch /etc/projects /etc/projid
    $SUDO chmod 666 /etc/projects /etc/projid

    # 8. Configure and restart NFS kernel server and rpcbind
    echo "Configuring nfs-kernel-server export..."
    $SUDO mkdir -p /etc/exports.d
    EXPORTS_FILE="/etc/exports.d/nfs-quota-agent-e2e.exports"
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

    echo "Stage A ext4 setup complete."
    ;;

  btrfs)
    # 1. Ensure required tools are installed on the host
    echo "Checking required btrfs host tools..."
    for tool in mkfs.btrfs btrfs exportfs findmnt; do
      if ! command -v "$tool" >/dev/null 2>&1; then
        echo "Host tool $tool is missing. Installing nfs-kernel-server, nfs-common, btrfs-progs..."
        $SUDO apt-get update
        $SUDO apt-get install -y nfs-kernel-server nfs-common btrfs-progs
        break
      fi
    done

    # 2. Create sparse file
    echo "Creating $IMG_SIZE sparse file at $IMG_FILE..."
    $SUDO rm -f "$IMG_FILE"
    $SUDO truncate -s "$IMG_SIZE" "$IMG_FILE"

    # 3. Format with btrfs
    echo "Formatting $IMG_FILE as btrfs..."
    $SUDO mkfs.btrfs -f "$IMG_FILE"

    # 4. Mount btrfs filesystem
    echo "Mounting $IMG_FILE at $EXPORT_DIR..."
    $SUDO mkdir -p "$EXPORT_DIR"
    if findmnt -n "$EXPORT_DIR" >/dev/null 2>&1; then
      $SUDO umount "$EXPORT_DIR" || true
    fi

    if ! $SUDO mount -o loop "$IMG_FILE" "$EXPORT_DIR"; then
      echo "FAIL: Failed to mount btrfs filesystem!" >&2
      echo "Kernel log:" >&2
      $SUDO dmesg | tail -n 50 >&2 || true
      exit 1
    fi

    # 5. Enable quotas on btrfs filesystem
    echo "Enabling btrfs quotas on $EXPORT_DIR..."
    $SUDO btrfs quota enable "$EXPORT_DIR"
    $SUDO btrfs qgroup show "$EXPORT_DIR"

    # 6. Create PVC directory as a btrfs subvolume
    echo "Creating btrfs subvolume $EXPORT_DIR/pvc-e2e..."
    $SUDO btrfs subvolume create "$EXPORT_DIR/pvc-e2e"
    $SUDO chmod 777 "$EXPORT_DIR/pvc-e2e"

    # Ensure host mapping files exist for kind extraMounts
    echo "Ensuring /etc/projects and /etc/projid exist on host..."
    $SUDO touch /etc/projects /etc/projid
    $SUDO chmod 666 /etc/projects /etc/projid

    # 7. Configure and restart NFS kernel server and rpcbind
    echo "Configuring nfs-kernel-server export..."
    $SUDO mkdir -p /etc/exports.d
    EXPORTS_FILE="/etc/exports.d/nfs-quota-agent-e2e.exports"
    # crossmnt allows clients to cross into the subvolume, and direct export of pvc-e2e ensures direct mounting
    echo "$EXPORT_DIR *(rw,sync,no_root_squash,no_subtree_check,insecure,crossmnt)" | $SUDO tee "$EXPORTS_FILE" >/dev/null
    echo "$EXPORT_DIR/pvc-e2e *(rw,sync,no_root_squash,no_subtree_check,insecure)" | $SUDO tee -a "$EXPORTS_FILE" >/dev/null

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

    echo "Stage A btrfs setup complete."
    ;;

  *)
    echo "FAIL: unsupported filesystem type '$FS' (supported: xfs, ext4, btrfs)" >&2
    exit 1
    ;;
esac
