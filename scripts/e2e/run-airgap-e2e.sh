#!/usr/bin/env bash
set -euo pipefail

# scripts/e2e/run-airgap-e2e.sh
# End-to-end air-gap verification workflow for nfs-quota-agent (Issue #5).
# Exercises:
#   Stage A: Host XFS filesystem setup with prjquota and NFS export
#   Stage B: Kind cluster setup with extraMounts and zero-egress prep
#   Stage C: Release bundle creation, offline verification, zero-egress install
#   Stage D: Quota proof (status annotations, host xfs_quota report, EDQUOT enforcement)
#   Stage E: Helm upgrade, rollback, and quota preservation check

CLUSTER_NAME="${CLUSTER_NAME:-airgap-e2e}"
EXPORT_DIR="${EXPORT_DIR:-/srv/nfs-export}"
BUNDLE_DIR="${BUNDLE_DIR:-/tmp/airgap-bundle}"
KIND_CONFIG="${KIND_CONFIG:-scripts/e2e/kind-config.yaml}"
MANIFESTS_DIR="${MANIFESTS_DIR:-scripts/e2e/manifests}"
STEP_SUMMARY="${GITHUB_STEP_SUMMARY:-}"

FS="${1:-${FS:-xfs}}"
echo "Selected filesystem backend for E2E: $FS"
if [[ "$FS" != "xfs" && "$FS" != "ext4" && "$FS" != "btrfs" ]]; then
  echo "FAIL: unsupported filesystem '$FS'; expected xfs, ext4, or btrfs" >&2
  exit 1
fi
PV_STORAGE_BYTES=104857600
if [ "$FS" = "btrfs" ]; then
  EXPECTED_ENFORCED_BYTES=$PV_STORAGE_BYTES
else
  # XFS and ext4 floor requested size to whole KB (quota.ExpectedEnforcedBytes semantics)
  EXPECTED_ENFORCED_BYTES=$(( (PV_STORAGE_BYTES / 1024) * 1024 ))
fi

SUDO=""
if [ "$(id -u)" -ne 0 ]; then
  SUDO="sudo"
fi

STAGE_A_STATUS="PENDING"
STAGE_A_DETAILS=""
STAGE_B_STATUS="PENDING"
STAGE_B_DETAILS=""
STAGE_C_STATUS="PENDING"
STAGE_C_DETAILS=""
STAGE_D_STATUS="PENDING"
STAGE_D_DETAILS=""
STAGE_E_STATUS="PENDING"
STAGE_E_DETAILS=""
AIRGAP_MARKER_TIME=""

resolve_project_quota_target() {
  PROJ_ID=$($SUDO lsattr -p -d "$EXPORT_DIR/pvc-e2e" 2>/dev/null | awk 'NR == 1 {print $1}' || true)
  if ! [[ "$PROJ_ID" =~ ^[0-9]+$ ]]; then
    echo "FAIL: could not resolve a numeric XFS project ID for $EXPORT_DIR/pvc-e2e" >&2
    exit 1
  fi
  echo "Resolved directory XFS project ID via lsattr: $PROJ_ID"

  # Optional cross-check: the DaemonSet template mounts /etc/projects and /etc/projid
  # as hostPath volumes from the node. In kind, the "node" is the kind container ($KIND_NODE);
  # kind-config.yaml also binds them to the runner host.
  # Cross-check the name<->id mapping if present.
  if [ -n "${KIND_NODE:-}" ]; then
    echo "Checking mapping files inside kind node $KIND_NODE..."
    local node_projid node_projects
    node_projid=$(docker exec "$KIND_NODE" cat /etc/projid 2>/dev/null || true)
    node_projects=$(docker exec "$KIND_NODE" cat /etc/projects 2>/dev/null || true)
    if [ -n "$node_projid" ]; then
      local node_id
      node_id=$(awk -F: -v name="pv_pv_e2e" '$1 == name {print $2; exit}' <<<"$node_projid" || true)
      if [ -n "$node_id" ] && [ "$node_id" -eq "$PROJ_ID" ]; then
        echo "OK: Kind node /etc/projid maps pv_pv_e2e to $PROJ_ID (matches lsattr id)."
      fi
    fi
    if [ -n "$node_projects" ]; then
      local node_path
      node_path=$(awk -F: -v id="$PROJ_ID" '$1 == id {print $2; exit}' <<<"$node_projects" || true)
      if [ -n "$node_path" ]; then
        echo "OK: Kind node /etc/projects maps id $PROJ_ID to $node_path."
      fi
    fi
  else
    # KIND_NODE not set; mapping inside kind node not cross-checked directly
    echo "Notice: KIND_NODE not set, skipping in-node mapping cross-check."
  fi

  # shellcheck disable=SC2016 # awk fields must remain literal for awk, not the shell.
  PROJ_NAME=$($SUDO awk -F: -v id="$PROJ_ID" '$2 == id {print $1; exit}' /etc/projid 2>/dev/null || true)
  if [ -n "$PROJ_NAME" ]; then
    if ! [[ "$PROJ_NAME" =~ ^[a-zA-Z0-9_-]+$ ]]; then
      echo "FAIL: resolved unsafe project name: $PROJ_NAME" >&2
      exit 1
    fi
    # If host /etc/projid contains the name mapping, xfs_quota report displays the name.
    # When no name mapping exists on the host, xfs_quota report displays #<id>.
    # Accept #<id> (primary anchor) or resolved project name.
    PROJECT_REPORT_SELECTOR="(#${PROJ_ID}|${PROJ_NAME})"
  else
    PROJECT_REPORT_SELECTOR="#${PROJ_ID}"
  fi

  echo "Resolved XFS project report selector: $PROJECT_REPORT_SELECTOR (id $PROJ_ID, name ${PROJ_NAME:-none})"
}

project_report_line() {
  local report="$1"
  # The selector is an exact first field matching #<id> or resolved project name.
  # Root project #0 cannot satisfy this gate.
  printf '%s\n' "$report" | grep -E "^[[:space:]]*${PROJECT_REPORT_SELECTOR}[[:space:]]+" | head -1 || true
}

assert_project_hard_limit() {
  local report="$1"
  local phase="$2"
  local line
  local selector_field
  local used_kb
  local hard_kb

  line=$(project_report_line "$report")
  echo "Resolved $phase project quota line: ${line:-none}"
  if [ -z "$line" ]; then
    echo "FAIL: $phase XFS quota report has no line for $PROJECT_REPORT_SELECTOR" >&2
    exit 1
  fi

  selector_field=$(awk '{print $1}' <<<"$line")
  if [[ "$selector_field" =~ ^#([0-9]+)$ ]]; then
    local rep_id="${BASH_REMATCH[1]}"
    if [ "$rep_id" -ne "$PROJ_ID" ]; then
      echo "FAIL: $phase report project id #$rep_id != lsattr directory project id $PROJ_ID" >&2
      exit 1
    fi
    echo "OK: $phase report line id #$rep_id matches directory lsattr project id $PROJ_ID"
  elif [ -n "${PROJ_NAME:-}" ] && [ "$selector_field" = "$PROJ_NAME" ]; then
    echo "OK: $phase report line name $selector_field matches resolved project name for lsattr id $PROJ_ID"
  fi

  used_kb=$(awk '{print $2}' <<<"$line")
  hard_kb=$(awk '{print $4}' <<<"$line")
  if ! [[ "$used_kb" =~ ^[0-9]+$ && "$hard_kb" =~ ^[0-9]+$ ]] || [ "$hard_kb" -ne 102400 ]; then
    echo "FAIL: $phase project quota line must have numeric used KiB and hard limit 102400 KiB: $line" >&2
    exit 1
  fi
}

assert_ext4_project_hard_limit() {
  local report="$1"
  local phase="$2"
  local line
  local selector_field
  local used_kb
  local hard_kb

  line=$(printf '%s\n' "$report" | grep -E "^[[:space:]]*(#${PROJ_ID}|${PROJ_NAME:-none})[[:space:]]+" | head -1 || true)
  echo "Resolved $phase ext4 project quota line: ${line:-none}"
  if [ -z "$line" ]; then
    echo "FAIL: $phase ext4 quota report has no line for project $PROJ_ID" >&2
    exit 1
  fi

  selector_field=$(awk '{print $1}' <<<"$line")
  local f2
  f2=$(awk '{print $2}' <<<"$line")
  if [[ "$f2" =~ ^[-+]{2}$ ]]; then
    used_kb=$(awk '{print $3}' <<<"$line")
    hard_kb=$(awk '{print $5}' <<<"$line")
  else
    used_kb=$(awk '{print $2}' <<<"$line")
    hard_kb=$(awk '{print $4}' <<<"$line")
  fi

  if ! [[ "$used_kb" =~ ^[0-9]+$ && "$hard_kb" =~ ^[0-9]+$ ]] || [ "$hard_kb" -ne 102400 ]; then
    echo "FAIL: $phase ext4 project quota line must have numeric used KiB and hard limit 102400 KiB: $line" >&2
    exit 1
  fi
  echo "OK: $phase ext4 report matches project $selector_field at 102400 KiB ($EXPECTED_ENFORCED_BYTES bytes)"
}

assert_btrfs_qgroup_limit() {
  local report="$1"
  local phase="$2"
  local line
  local used_bytes
  local hard_bytes

  line=$(printf '%s\n' "$report" | grep -E "(^|[[:space:]])(/srv/nfs-export/)?pvc-e2e([[:space:]]|$)" | head -1 || true)
  echo "Resolved $phase btrfs qgroup quota line: ${line:-none}"
  if [ -z "$line" ]; then
    echo "FAIL: $phase btrfs qgroup report has no line for subvolume pvc-e2e" >&2
    exit 1
  fi

  used_bytes=$(awk '{print $2}' <<<"$line")
  hard_bytes=$(awk '{print $4}' <<<"$line")

  if ! [[ "$used_bytes" =~ ^[0-9]+$ && "$hard_bytes" =~ ^[0-9]+$ ]] || [ "$hard_bytes" -ne 104857600 ]; then
    echo "FAIL: $phase btrfs qgroup line must have numeric used bytes and max_rfer 104857600 bytes: $line" >&2
    exit 1
  fi
  echo "OK: $phase btrfs report matches subvolume pvc-e2e at max_rfer 104857600 bytes (exact ExpectedEnforcedBytes)"
}

resolve_quota_target() {
  case "$FS" in
    xfs|ext4)
      resolve_project_quota_target
      ;;
    btrfs)
      echo "Resolved btrfs quota target: subvolume $EXPORT_DIR/pvc-e2e"
      ;;
  esac
}

get_native_quota_report() {
  case "$FS" in
    xfs)
      $SUDO xfs_quota -x -c 'report -p' "$EXPORT_DIR"
      ;;
    ext4)
      $SUDO repquota -P "$EXPORT_DIR"
      ;;
    btrfs)
      $SUDO btrfs qgroup show -re --raw "$EXPORT_DIR"
      ;;
  esac
}

assert_quota_hard_limit() {
  local report="$1"
  local phase="$2"
  case "$FS" in
    xfs)
      assert_project_hard_limit "$report" "$phase"
      ;;
    ext4)
      assert_ext4_project_hard_limit "$report" "$phase"
      ;;
    btrfs)
      assert_btrfs_qgroup_limit "$report" "$phase"
      ;;
  esac
}

check_post_write_usage() {
  local report="$1"
  case "$FS" in
    xfs)
      PROJECT_LINE=$(project_report_line "$report")
      echo "Resolved post-write project quota line: ${PROJECT_LINE:-none}"
      PROJ_USED_KB=""
      PROJ_HARD_KB=""
      if [ -n "$PROJECT_LINE" ]; then
        PROJ_USED_KB=$(echo "$PROJECT_LINE" | awk '{print $2}')
        PROJ_HARD_KB=$(echo "$PROJECT_LINE" | awk '{print $4}')
      fi
      echo "Project Used: ${PROJ_USED_KB:-unknown} KiB, Hard limit: ${PROJ_HARD_KB:-unknown} KiB"
      if ! [[ "$PROJ_USED_KB" =~ ^[0-9]+$ && "$PROJ_HARD_KB" =~ ^[0-9]+$ ]] || [ "$PROJ_HARD_KB" -ne 102400 ]; then
        echo "FAIL: post-write resolved project line must have hard limit 102400 KiB: ${PROJECT_LINE:-none}" >&2
        exit 1
      fi
      AT_HARD_LIMIT=false
      if [ "$PROJ_USED_KB" -ge "$PROJ_HARD_KB" ]; then
        AT_HARD_LIMIT=true
      fi
      ;;
    ext4)
      local line
      line=$(printf '%s\n' "$report" | grep -E "^[[:space:]]*(#${PROJ_ID}|${PROJ_NAME:-none})[[:space:]]+" | head -1 || true)
      echo "Resolved post-write ext4 project quota line: ${line:-none}"
      PROJ_USED_KB=""
      PROJ_HARD_KB=""
      if [ -n "$line" ]; then
        local f2
        f2=$(awk '{print $2}' <<<"$line")
        if [[ "$f2" =~ ^[-+]{2}$ ]]; then
          PROJ_USED_KB=$(awk '{print $3}' <<<"$line")
          PROJ_HARD_KB=$(awk '{print $5}' <<<"$line")
        else
          PROJ_USED_KB=$(awk '{print $2}' <<<"$line")
          PROJ_HARD_KB=$(awk '{print $4}' <<<"$line")
        fi
      fi
      echo "Project Used: ${PROJ_USED_KB:-unknown} KiB, Hard limit: ${PROJ_HARD_KB:-unknown} KiB"
      if ! [[ "$PROJ_USED_KB" =~ ^[0-9]+$ && "$PROJ_HARD_KB" =~ ^[0-9]+$ ]] || [ "$PROJ_HARD_KB" -ne 102400 ]; then
        echo "FAIL: post-write resolved ext4 project line must have hard limit 102400 KiB: ${line:-none}" >&2
        exit 1
      fi
      AT_HARD_LIMIT=false
      if [ "$PROJ_USED_KB" -ge "$PROJ_HARD_KB" ]; then
        AT_HARD_LIMIT=true
      fi
      ;;
    btrfs)
      local line
      line=$(printf '%s\n' "$report" | grep -E "(^|[[:space:]])(/srv/nfs-export/)?pvc-e2e([[:space:]]|$)" | head -1 || true)
      echo "Resolved post-write btrfs qgroup line: ${line:-none}"
      PROJ_USED_BYTES=""
      PROJ_HARD_BYTES=""
      if [ -n "$line" ]; then
        PROJ_USED_BYTES=$(awk '{print $2}' <<<"$line")
        PROJ_HARD_BYTES=$(awk '{print $4}' <<<"$line")
      fi
      echo "Subvolume Used: ${PROJ_USED_BYTES:-unknown} bytes, Hard limit: ${PROJ_HARD_BYTES:-unknown} bytes"
      if ! [[ "$PROJ_USED_BYTES" =~ ^[0-9]+$ && "$PROJ_HARD_BYTES" =~ ^[0-9]+$ ]] || [ "$PROJ_HARD_BYTES" -ne 104857600 ]; then
        echo "FAIL: post-write resolved btrfs qgroup line must have hard limit 104857600 bytes: ${line:-none}" >&2
        exit 1
      fi
      AT_HARD_LIMIT=false
      if [ "$PROJ_USED_BYTES" -ge "$PROJ_HARD_BYTES" ]; then
        AT_HARD_LIMIT=true
      fi
      PROJ_USED_KB=$(( PROJ_USED_BYTES / 1024 ))
      PROJ_HARD_KB=$(( PROJ_HARD_BYTES / 1024 ))
      ;;
  esac
}

# assert_root_writer_outcome checks the documented, filesystem-specific
# outcome of a 120 MiB write performed by a ROOT writer (test-writer-root.yaml)
# against the same 100 MiB quota the non-root writer above was correctly
# blocked by. ext4 project quota hard limits are not enforced for a writer
# holding CAP_SYS_RESOURCE (root): fs/quota/dquot.c's ignore_hardlimit()
# waives the hard limit outright, and knfsd performs the write with
# credentials mapped from the incoming NFS client, so a root pod over an
# export with no_root_squash writes as root on the server and inherits the
# bypass. XFS and btrfs enforce their hard limit regardless of the writer's
# privilege, so a root write must still fail there exactly like the
# non-root case. This function documents the bypass instead of hiding it:
# an unexpected result on any backend is a FAIL, not a soft warning.
assert_root_writer_outcome() {
  local report="$1"
  local rc="$2"
  case "$FS" in
    ext4)
      if [ "$rc" -ne 0 ]; then
        echo "FAIL: root writer's 120 MiB write unexpectedly FAILED (rc=$rc) -- ext4 project quota hard limits are documented to NOT be enforced for a CAP_SYS_RESOURCE (root) writer (fs/quota/dquot.c ignore_hardlimit()); either this kernel no longer has the bypass or the writer did not actually run as root over NFS." >&2
        exit 1
      fi
      local line f2
      line=$(printf '%s\n' "$report" | grep -E "^[[:space:]]*(#${PROJ_ID}|${PROJ_NAME:-none})[[:space:]]+" | head -1 || true)
      echo "Resolved root-writer ext4 project quota line: ${line:-none}"
      if [ -z "$line" ]; then
        echo "FAIL: root writer's ext4 quota report has no line for project $PROJ_ID" >&2
        exit 1
      fi
      f2=$(awk '{print $2}' <<<"$line")
      if [[ "$f2" != +* ]]; then
        echo "FAIL: root writer's write succeeded past the 100 MiB hard limit, but the ext4 quota report's block-status flag ($f2) does not show the expected over-quota '+' marker: $line" >&2
        exit 1
      fi
      ROOT_WRITER_OUTCOME="SUCCEEDED past the 100 MiB hard limit (repquota block-status flag '$f2' confirms over-quota) -- documents ext4's CAP_SYS_RESOURCE hard-limit bypass for a root/no_root_squash writer"
      echo "OK (documented bypass): $ROOT_WRITER_OUTCOME"
      ;;
    xfs|btrfs)
      if [ "$rc" -eq 0 ]; then
        echo "FAIL: root writer's 120 MiB write unexpectedly SUCCEEDED on $FS -- $FS project quotas are documented to enforce their hard limit even for a root/CAP_SYS_RESOURCE writer; this write should have failed with EDQUOT/ENOSPC exactly like the non-root case." >&2
        exit 1
      fi
      ROOT_WRITER_OUTCOME="FAILED as expected (rc=$rc) -- $FS enforces its hard limit even for a root/CAP_SYS_RESOURCE writer"
      echo "OK: $ROOT_WRITER_OUTCOME"
      ;;
  esac
}

assert_no_registry_pull_events() {
  local stage="$1"
  local raw_events

  if [ -z "${AIRGAP_MARKER_TIME:-}" ]; then
    echo "FAIL: AIRGAP_MARKER_TIME not set before calling assert_no_registry_pull_events" >&2
    exit 1
  fi

  raw_events=$(kubectl get events -A -o json)

  python3 -c '
import sys, json
from datetime import datetime

marker_str = sys.argv[1]
stage = sys.argv[2]
raw_json = sys.stdin.read()

def parse_ts(ts_str):
    if not ts_str:
        return None
    if ts_str.endswith("Z"):
        ts_str = ts_str[:-1] + "+00:00"
    try:
        return datetime.fromisoformat(ts_str)
    except Exception:
        return None

marker_dt = parse_ts(marker_str)
REGISTRY_PATTERNS = [
    "ghcr.io",
    "docker.io",
    "registry-1.docker.io",
    "index.docker.io",
    "quay.io",
    "gcr.io",
    "mcr.microsoft.com",
]

data = json.loads(raw_json)
offending = []

for item in data.get("items", []):
    reason = item.get("reason", "")
    message = item.get("message", "")

    if reason not in ("Pulling", "Pulled"):
        continue

    # Skip containerd local image hit event: "already present on machine"
    # indicates containerd used the preloaded local image without contacting a registry.
    if "already present on machine" in message.lower():
        continue

    event_ts = (
        parse_ts(item.get("eventTime")) or
        parse_ts(item.get("lastTimestamp")) or
        parse_ts(item.get("firstTimestamp")) or
        parse_ts(item.get("metadata", {}).get("creationTimestamp"))
    )
    if marker_dt and event_ts and event_ts <= marker_dt:
        continue

    msg_lower = message.lower()
    obj_str = str(item.get("involvedObject", {})).lower()
    if any(reg in msg_lower or reg in obj_str for reg in REGISTRY_PATTERNS):
        offending.append(item)

if offending:
    print(f"FAIL: registry-backed image pull observed by end of {stage}:", file=sys.stderr)
    for ev in offending:
        print(json.dumps(ev, indent=2), file=sys.stderr)
    sys.exit(1)

print(f"OK: 0 registry pulls after marker ({stage}).")
' "$AIRGAP_MARKER_TIME" "$stage" <<< "$raw_events"
}

block_kind_node_registry_egress() {
  # The job's harden-runner allowlist is host-wide; block HTTPS from the kind
  # node after all setup pulls so CRI cannot reach a registry in Stages C--E.
  if docker exec "$KIND_NODE" iptables -C OUTPUT -p tcp --dport 443 -j REJECT 2>/dev/null; then
    echo "kind-node TCP/443 REJECT rule already installed."
    return 0
  fi
  docker exec "$KIND_NODE" iptables -I OUTPUT -p tcp --dport 443 -j REJECT
  if ! docker exec "$KIND_NODE" iptables -C OUTPUT -p tcp --dport 443 -j REJECT; then
    echo "FAIL: kind-node HTTPS egress block was not installed" >&2
    exit 1
  fi
  echo "Installed and verified kind-node TCP/443 REJECT rule for Stages C--E."
}

write_summary() {
  if [ -n "$STEP_SUMMARY" ]; then
    cat <<EOF >> "$STEP_SUMMARY"
### Air-Gap E2E Quota Test Results (#149 - $FS)
| Stage | Status | Details |
|---|---|---|
| Stage A: Host Filesystem | $STAGE_A_STATUS | $STAGE_A_DETAILS |
| Stage B: Cluster Setup | $STAGE_B_STATUS | $STAGE_B_DETAILS |
| Stage C: Offline Bundle Install | $STAGE_C_STATUS | $STAGE_C_DETAILS |
| Stage D: Real Quota Proof | $STAGE_D_STATUS | $STAGE_D_DETAILS |
| Stage E: Upgrade, Rollback & Preservation | $STAGE_E_STATUS | $STAGE_E_DETAILS |
EOF
  fi
}

trap 'write_summary' EXIT

# ==============================================================================
# STAGE A: Host Filesystem Setup ($FS + NFS export)
# ==============================================================================
echo "=================================================================="
echo ">>> STAGE A: Host Filesystem Setup ($FS)"
echo "=================================================================="
bash scripts/e2e/setup-fs-nfs.sh "$FS"
STAGE_A_STATUS="PASS"
STAGE_A_DETAILS="2GiB $FS sparse file mounted at $EXPORT_DIR; nfs-kernel-server active"
echo "STAGE A PASSED"

# ==============================================================================
# STAGE B: Cluster Setup (kind with extraMounts)
# ==============================================================================
echo "=================================================================="
echo ">>> STAGE B: Cluster Setup"
echo "=================================================================="
if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}\$"; then
  echo "Deleting existing kind cluster $CLUSTER_NAME..."
  kind delete cluster --name "$CLUSTER_NAME"
fi

echo "Creating kind cluster $CLUSTER_NAME..."
kind create cluster --config "$KIND_CONFIG" --name "$CLUSTER_NAME"

echo "Labeling control-plane node with nfs-server=true..."
kubectl label node -l node-role.kubernetes.io/control-plane nfs-server=true --overwrite

KIND_NODE=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
echo "Kind control-plane node: $KIND_NODE"

# Ensure nfs-common is installed in the kind node so kubelet can mount NFS volumes
if ! docker exec "$KIND_NODE" which mount.nfs >/dev/null 2>&1; then
  echo "Installing nfs-common inside kind node $KIND_NODE..."
  docker exec "$KIND_NODE" apt-get update -o Acquire::Retries=3
  docker exec "$KIND_NODE" apt-get install -y --no-install-recommends nfs-common
fi

# Preload busybox for write tests (setup phase)
BUSYBOX_TAG="busybox:1.36"
echo "Pulling $BUSYBOX_TAG on host to resolve digest..."
docker pull "$BUSYBOX_TAG"
BUSYBOX_DIGEST=$(docker inspect --format='{{index .RepoDigests 0}}' "$BUSYBOX_TAG" 2>/dev/null || true)
if [ -z "$BUSYBOX_DIGEST" ]; then
  BUSYBOX_DIGEST="busybox@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662"
fi
echo "Pulling busybox by digest: $BUSYBOX_DIGEST..."
docker pull "$BUSYBOX_DIGEST"
echo "Preloading $BUSYBOX_DIGEST into kind cluster..."
kind load docker-image "$BUSYBOX_DIGEST" --name "$CLUSTER_NAME" || true
echo "Preloading $BUSYBOX_TAG into kind cluster..."
kind load docker-image "$BUSYBOX_TAG" --name "$CLUSTER_NAME"

# Obtain IPv4 Gateway IP and Subnet for kind network
GATEWAY_IP=$(docker network inspect kind | python3 -c '
import json, sys
data = json.load(sys.stdin)
for c in data[0].get("IPAM", {}).get("Config", []):
    gw = c.get("Gateway", "")
    if "." in gw:
        print(gw)
        break
')
if [ -z "$GATEWAY_IP" ]; then
  GATEWAY_IP=$(docker exec "$KIND_NODE" ip route | awk '/default/ {print $3}' | head -1)
fi
echo "Docker bridge gateway IP (NFS server endpoint): $GATEWAY_IP"

KIND_SUBNET=$(docker network inspect kind | python3 -c '
import json, sys
data = json.load(sys.stdin)
for c in data[0].get("IPAM", {}).get("Config", []):
    sub = c.get("Subnet", "")
    if "." in sub:
        print(sub)
        break
')
echo "Docker kind network subnet: $KIND_SUBNET"

if [ -n "$KIND_SUBNET" ]; then
  echo "Allowing kind subnet $KIND_SUBNET in NFS exports..."
  EXPORTS_FILE="/etc/exports.d/nfs-quota-agent-e2e.exports"
  # No fsid=0 here either (see setup-xfs-nfs.sh): fsid=0 turns $EXPORT_DIR into
  # the NFSv4 pseudo-root, which breaks a vers=4 client mounting the real path
  # $EXPORT_DIR/pvc-e2e (it would only be reachable at /pvc-e2e under the
  # pseudo-root). Export the real path for both v3 and v4 clients.
  if [ "$FS" = "btrfs" ]; then
    echo "$EXPORT_DIR *(rw,sync,no_root_squash,no_subtree_check,insecure,crossmnt)" | $SUDO tee "$EXPORTS_FILE" >/dev/null
    echo "$EXPORT_DIR $KIND_SUBNET(rw,sync,no_root_squash,no_subtree_check,insecure,crossmnt)" | $SUDO tee -a "$EXPORTS_FILE" >/dev/null
    echo "$EXPORT_DIR/pvc-e2e *(rw,sync,no_root_squash,no_subtree_check,insecure)" | $SUDO tee -a "$EXPORTS_FILE" >/dev/null
    echo "$EXPORT_DIR/pvc-e2e $KIND_SUBNET(rw,sync,no_root_squash,no_subtree_check,insecure)" | $SUDO tee -a "$EXPORTS_FILE" >/dev/null
  else
    echo "$EXPORT_DIR *(rw,sync,no_root_squash,no_subtree_check,insecure)" | $SUDO tee "$EXPORTS_FILE" >/dev/null
    echo "$EXPORT_DIR $KIND_SUBNET(rw,sync,no_root_squash,no_subtree_check,insecure)" | $SUDO tee -a "$EXPORTS_FILE" >/dev/null
  fi
  $SUDO exportfs -ra
fi

echo "Host RPC services (rpcinfo -p):"
$SUDO rpcinfo -p || true

# Verify NFS connectivity from kind node
echo "Testing NFS connectivity from kind node to $GATEWAY_IP..."
docker exec "$KIND_NODE" showmount -e "$GATEWAY_IP"

# showmount -e only speaks the NFSv3 MOUNT protocol, so it cannot catch a v4
# pseudo-root mismatch (e.g. an fsid=0 export hiding the real path from a v4
# client). Do a real NFSv4 mount of the exact PV path from inside the kind
# node, matching the PV's mountOptions: [vers=4], and fail Stage B here
# instead of failing opaquely in Stage D's kubectl wait.
echo "Testing NFSv4 mount of $GATEWAY_IP:$EXPORT_DIR/pvc-e2e from kind node..."
if ! docker exec "$KIND_NODE" sh -c "mkdir -p /tmp/nfs-probe && mount -t nfs -o vers=4 $GATEWAY_IP:$EXPORT_DIR/pvc-e2e /tmp/nfs-probe && findmnt -T /tmp/nfs-probe && umount /tmp/nfs-probe"; then
  echo "FAIL: NFSv4 mount of $GATEWAY_IP:$EXPORT_DIR/pvc-e2e failed from kind node $KIND_NODE!" >&2
  echo "Host NFS exports (exportfs -v):" >&2
  $SUDO exportfs -v >&2
  exit 1
fi
echo "OK: NFSv4 mount probe of $GATEWAY_IP:$EXPORT_DIR/pvc-e2e succeeded from kind node"

# Record marker timestamp at end of setup to scope zero-egress assertions
sleep 2
AIRGAP_MARKER_TIME=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
echo "Air-gap window marker recorded at end of setup: $AIRGAP_MARKER_TIME"

STAGE_B_STATUS="PASS"
STAGE_B_DETAILS="kind cluster $CLUSTER_NAME running; node labeled nfs-server=true; gateway $GATEWAY_IP; marker $AIRGAP_MARKER_TIME"
echo "STAGE B PASSED"

# ==============================================================================
# STAGE C: Bundle Build, Verification, and Air-Gap Install
# ==============================================================================
echo "=================================================================="
echo ">>> STAGE C: Release Bundle & Air-Gap Install"
echo "=================================================================="
echo "Building local container image (host arch)..."
make docker-build VERSION=e2e
IMAGE_REF="ghcr.io/dasomel/nfs-quota-agent:e2e"

echo "Packaging Helm chart..."
make helm-package
CHART_TGZ=$(find .helm-releases -maxdepth 1 -name "nfs-quota-agent-*.tgz" | head -1)
echo "Packaged chart: $CHART_TGZ"

BUNDLE_FILE="nfs-quota-agent-e2e-offline.tar.gz"
echo "Building release bundle $BUNDLE_FILE..."
make release-bundle IMAGE_REF="$IMAGE_REF" CHART_TGZ="$CHART_TGZ" BUNDLE_FILE="$BUNDLE_FILE"

# Extract OCI archive image digest from inside the bundle
IMAGE_DIGEST=$(python3 -c "
import tarfile, json
with tarfile.open('$BUNDLE_FILE', 'r:gz') as btf:
    with tarfile.open(fileobj=btf.extractfile('images/nfs-quota-agent-image.tar')) as itf:
        index = json.load(itf.extractfile('index.json'))
        print(index['manifests'][0]['digest'])
")
echo "Extracted image digest from bundle: $IMAGE_DIGEST"

# Generate local release manifest matching this image digest and chart
echo "Generating local release manifest for verification..."
make release-manifest-local IMAGE_DIGEST="$IMAGE_DIGEST"
sha256sum "$BUNDLE_FILE" > "$BUNDLE_FILE.sha256"

# Verify offline bundle with hack/verify-release.py
echo "Running offline bundle verification..."
echo "INFO: Synthetic local release — cosign signatures unverified; verifying bundle archive sha256, image digest, and chart sha256."
python3 hack/verify-release.py --bundle "$BUNDLE_FILE" --manifest .release-manifest-local/release-manifest.json

# Extract bundle into air-gapped staging directory
echo "Extracting bundle into isolated directory $BUNDLE_DIR..."
rm -rf "$BUNDLE_DIR"
mkdir -p "$BUNDLE_DIR"
tar xzf "$BUNDLE_FILE" -C "$BUNDLE_DIR"

# Zero-egress enforcement: load image strictly from the bundle's OCI archive
echo "Loading image into kind exclusively from extracted bundle archive..."
# sudo is required so skopeo untar has permission to restore archive uid/gid 0
$SUDO skopeo copy "oci-archive:$BUNDLE_DIR/images/nfs-quota-agent-image.tar" "docker-archive:/tmp/agent-docker.tar:ghcr.io/dasomel/nfs-quota-agent:e2e"
$SUDO chmod 644 /tmp/agent-docker.tar
kind load image-archive /tmp/agent-docker.tar --name "$CLUSTER_NAME"
$SUDO rm -f /tmp/agent-docker.tar
block_kind_node_registry_egress

echo "Container images on kind node:"
docker exec "$KIND_NODE" crictl images

# Prefer the digest that containerd associates with the loaded archive. Some kind/containerd
# versions retain only the tag for `kind load image-archive`; log that exact inspection and
# use the tag only in that documented fallback case.
RUNTIME_IMAGE_INSPECT=""
RUNTIME_REPO_DIGEST=""
if RUNTIME_IMAGE_INSPECT=$(docker exec "$KIND_NODE" crictl inspecti "$IMAGE_REF" 2>&1); then
  RUNTIME_REPO_DIGEST=$(printf '%s\n' "$RUNTIME_IMAGE_INSPECT" | jq -r --arg image "$IMAGE_REF" \
    '.status.repoDigests[]? | select(startswith($image + "@sha256:"))' | head -1)
else
  echo "Containerd did not inspect loaded image $IMAGE_REF; retaining tag fallback. inspecti output:"
  echo "$RUNTIME_IMAGE_INSPECT"
fi

HELM_IMAGE_ARGS=(--set image.tag=e2e)
IMAGE_INSTALL_MODE="tag fallback"
if [[ "$RUNTIME_REPO_DIGEST" == "$IMAGE_REF@sha256:"* ]] && [[ "${RUNTIME_REPO_DIGEST#*@}" =~ ^sha256:[a-f0-9]{64}$ ]]; then
  RUNTIME_IMAGE_DIGEST="${RUNTIME_REPO_DIGEST#*@}"
  HELM_IMAGE_ARGS=(--set "image.digest=$RUNTIME_IMAGE_DIGEST")
  IMAGE_INSTALL_MODE="containerd repoDigest $RUNTIME_IMAGE_DIGEST"
  echo "Containerd repoDigest for loaded image: $RUNTIME_REPO_DIGEST"
else
  echo "Containerd exposes no usable repoDigest for loaded archive; using image.tag=e2e. inspecti output:"
  echo "$RUNTIME_IMAGE_INSPECT"
fi

# Install via Helm using chart from bundle only, pullPolicy=Never, and the loaded digest when available.
BUNDLED_CHART=$(find "$BUNDLE_DIR/chart" -maxdepth 1 -name "*.tgz" | head -1)
echo "Installing Helm chart from bundle: $BUNDLED_CHART"
helm install nfs-quota-agent "$BUNDLED_CHART" \
  --namespace nfs-quota-agent \
  --create-namespace \
  --set image.pullPolicy=Never \
  "${HELM_IMAGE_ARGS[@]}" \
  --set config.nfsBasePath=/srv/nfs-export \
  --set config.nfsServerPath=/srv/nfs-export \
  --set nfsExport.hostPath=/srv/nfs-export \
  --set config.processAllNFS=true

# Deploy static PV and PVC
echo "Deploying static PV and PVC..."
sed "s|__GATEWAY_IP__|$GATEWAY_IP|g" "$MANIFESTS_DIR/pvc-e2e.yaml" | kubectl apply -f -
assert_no_registry_pull_events "Stage C"

STAGE_C_STATUS="PASS"
STAGE_C_DETAILS="bundle verified; image loaded from OCI archive (bundle digest: $IMAGE_DIGEST; install: $IMAGE_INSTALL_MODE); helm installed with pullPolicy=Never"
echo "STAGE C PASSED"

# ==============================================================================
# STAGE D: Proof of Real Quota Enforcement
# ==============================================================================
echo "=================================================================="
echo ">>> STAGE D: Real Quota Enforcement Proof"
echo "=================================================================="
echo "TRACE: writer pod mounts PVC via NFS; quota enforcement crosses the NFS wire path."
echo "Waiting for nfs-quota-agent DaemonSet rollout..."
kubectl rollout status daemonset/nfs-quota-agent -n nfs-quota-agent --timeout=120s

echo "Waiting for PV quota status annotation and enforced limit bytes..."
START_TIME=$(date +%s)
QUOTA_APPLIED=false
ENFORCED_BYTES=""
while [ $(( $(date +%s) - START_TIME )) -lt 120 ]; do
  STATUS=$(kubectl get pv pv-e2e -o jsonpath='{.metadata.annotations.nfs\.io/quota-status}' 2>/dev/null || true)
  ENFORCED_BYTES=$(kubectl get pv pv-e2e -o jsonpath='{.metadata.annotations.nfs\.io/enforced-limit-bytes}' 2>/dev/null || true)
  if [ "$STATUS" = "applied" ] && [ -n "$ENFORCED_BYTES" ] && [ "$ENFORCED_BYTES" -gt 0 ] 2>/dev/null; then
    QUOTA_APPLIED=true
    break
  fi
  sleep 2
done

if [ "$QUOTA_APPLIED" != "true" ]; then
  echo "FAIL: PV quota was not applied within 120s!" >&2
  kubectl describe pv pv-e2e >&2 || true
  kubectl logs -n nfs-quota-agent daemonset/nfs-quota-agent --all-containers=true --tail=100 >&2 || true

  # Self-diagnosing failure: dump the ext4 quota state from both the host's
  # and the agent container's point of view, since "PV quota was not
  # applied" alone can't distinguish an apply failure from a read-back
  # verification failure (agent.go's verifyQuotaOnDisk) -- see PR #155/#149.
  if [ "$FS" = "ext4" ]; then
    echo "--- ext4 diagnostics (host side) ---" >&2
    $SUDO repquota -P -a -v >&2 || true
    $SUDO repquota -P "$EXPORT_DIR" >&2 || true
    echo "host /etc/projects:" >&2
    cat /etc/projects >&2 || true
    echo "host /etc/projid:" >&2
    cat /etc/projid >&2 || true
    echo "host lsattr -p -d $EXPORT_DIR/pvc-e2e:" >&2
    lsattr -p -d "$EXPORT_DIR/pvc-e2e" >&2 || true

    echo "--- ext4 diagnostics (agent pod's view) ---" >&2
    AGENT_POD=$(kubectl get pods -n nfs-quota-agent -l app.kubernetes.io/name=nfs-quota-agent -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    if [ -n "$AGENT_POD" ]; then
      kubectl exec -n nfs-quota-agent "$AGENT_POD" -- repquota -P -a -v >&2 || true
      kubectl exec -n nfs-quota-agent "$AGENT_POD" -- repquota -P "$EXPORT_DIR" >&2 || true
      echo "agent pod /etc/projects:" >&2
      kubectl exec -n nfs-quota-agent "$AGENT_POD" -- cat /etc/projects >&2 || true
      echo "agent pod /etc/projid:" >&2
      kubectl exec -n nfs-quota-agent "$AGENT_POD" -- cat /etc/projid >&2 || true
      echo "agent pod lsattr -p -d $EXPORT_DIR/pvc-e2e:" >&2
      kubectl exec -n nfs-quota-agent "$AGENT_POD" -- lsattr -p -d "$EXPORT_DIR/pvc-e2e" >&2 || true
    else
      echo "WARN: could not resolve agent pod name for in-container ext4 diagnostics" >&2
    fi
  fi
  exit 1
fi

EXPECTED_ENFORCED_BYTES=104857600
echo "Observed PV annotations: nfs.io/quota-status=applied, nfs.io/enforced-limit-bytes=$ENFORCED_BYTES"
if [ "$ENFORCED_BYTES" -ne "$EXPECTED_ENFORCED_BYTES" ]; then
  echo "FAIL: enforced-limit-bytes $ENFORCED_BYTES != expected $EXPECTED_ENFORCED_BYTES" >&2
  exit 1
fi

echo "Inspecting host $FS native quota report..."
FS_REPORT_INITIAL=$(get_native_quota_report)
echo "$FS_REPORT_INITIAL"

# Resolve and bind the report to the actual project/subvolume assigned to the PV directory.
resolve_quota_target
assert_quota_hard_limit "$FS_REPORT_INITIAL" "initial"
echo "OK: Host $FS native quota report matches target at $EXPECTED_ENFORCED_BYTES bytes"

# Deploy test-writer pod to test filesystem enforcement
echo "Deploying test-writer pod..."
kubectl apply -f "$MANIFESTS_DIR/test-writer.yaml"
if ! kubectl wait --for=condition=Ready pod/test-writer --timeout=120s; then
  echo "FAIL: test-writer pod did not become Ready in time!" >&2
  echo "kubectl describe pod test-writer:" >&2
  kubectl describe pod test-writer >&2 || true
  echo "kubectl get events (last 30, by lastTimestamp):" >&2
  kubectl get events --sort-by=.lastTimestamp >&2 | tail -30 || true
  echo "kubectl get pvc,pv:" >&2
  kubectl get pvc,pv >&2 || true
  echo "dmesg on kind node (nfs lines):" >&2
  docker exec "$KIND_NODE" dmesg 2>/dev/null | grep -i nfs | tail -20 >&2 || true
  exit 1
fi

echo "Writer pod status and placement (kubectl get pod test-writer -o wide):"
kubectl get pod test-writer -o wide

echo "Writer pod description (kubectl describe pod test-writer):"
kubectl describe pod test-writer

# Assert the writer's /mnt/nfs is a real NFS mount, not a hostPath.
echo "Asserting writer mount at /mnt/nfs is NFS type..."
MOUNT_LINE=$(kubectl exec test-writer -- sh -c 'mount | grep " /mnt/nfs "' || true)
echo "Writer mount line: ${MOUNT_LINE:-none}"
if ! echo "$MOUNT_LINE" | grep -qE 'type nfs4?[, ]|type nfs '; then
  echo "FAIL: /mnt/nfs is not mounted as NFS inside the writer pod!" >&2
  echo "findmnt output from writer pod:" >&2
  kubectl exec test-writer -- sh -c 'findmnt /mnt/nfs 2>/dev/null || mount' >&2
  exit 1
fi
echo "OK: Writer pod /mnt/nfs is an NFS mount ($MOUNT_LINE)"

echo "Writer pod mounts for /mnt/nfs (mount | grep /mnt/nfs):"
kubectl exec test-writer -- sh -c 'mount | grep /mnt/nfs || mount'

echo "Writer pod filesystem free space (df -h /mnt/nfs):"
kubectl exec test-writer -- df -h /mnt/nfs

echo "Writing 50 MiB to PVC (should succeed)..."
kubectl exec test-writer -- dd if=/dev/zero of=/mnt/nfs/test-50m.bin bs=1M count=50 conv=fsync
echo "OK: 50 MiB write succeeded."

echo "Host $FS native quota report BEFORE 120 MiB write:"
FS_REPORT_BEFORE=$(get_native_quota_report)
echo "$FS_REPORT_BEFORE"

echo "Writing 120 MiB to PVC (100 MiB quota, must FAIL with EDQUOT or ENOSPC at quota limit)..."
set +e
WRITE_120M_EXEC=$(kubectl exec test-writer -- sh -c 'dd if=/dev/zero of=/mnt/nfs/test-120m.bin bs=1M count=120 conv=fsync 2>&1; echo rc=$?')
EXEC_RC=$?
set -e

echo "Full writer command output (including rc marker):"
echo "$WRITE_120M_EXEC"

WRITE_120M_RC=$(echo "$WRITE_120M_EXEC" | awk -F'rc=' '/rc=/{print $2}' | tail -1)
WRITE_120M_OUT=$(echo "$WRITE_120M_EXEC" | sed '/rc=[0-9]*/d')
if [ -z "$WRITE_120M_RC" ]; then
  WRITE_120M_RC=$EXEC_RC
fi
echo "Writer command exit code: $WRITE_120M_RC"
echo "Writer command stderr/stdout:"
echo "$WRITE_120M_OUT"

echo "Host $FS native quota report AFTER 120 MiB write:"
FS_REPORT_AFTER=$(get_native_quota_report)
echo "$FS_REPORT_AFTER"

echo "Host filesystem free space on $EXPORT_DIR (df -h $EXPORT_DIR):"
df -h "$EXPORT_DIR"

echo "Kernel log messages for quota / $FS / EDQUOT / ENOSPC:"
$SUDO dmesg | grep -iE "quota|xfs|ext4|btrfs|edquot|enospc" | tail -n 25 || true

if [ "$WRITE_120M_RC" -eq 0 ]; then
  echo "FAIL: 120 MiB write unexpectedly succeeded despite 100 MiB quota limit!" >&2
  exit 1
fi

check_post_write_usage "$FS_REPORT_AFTER"

MATCHED_CASE=""
if echo "$WRITE_120M_OUT" | grep -qi "quota"; then
  MATCHED_CASE="EDQUOT (Disk quota exceeded)"
elif echo "$WRITE_120M_OUT" | grep -qi "No space left on device"; then
  # Only accept ENOSPC ("No space left on device") if the project quota is at its hard limit.
  # XFS project quotas simulate partition boundaries and intentionally return ENOSPC rather than EDQUOT.
  if [ "$AT_HARD_LIMIT" = "true" ]; then
    MATCHED_CASE="ENOSPC (No space left on device) with quota at hard limit (${PROJ_USED_KB} KiB >= ${PROJ_HARD_KB} KiB)"
  else
    echo "FAIL: 120 MiB write failed with 'No space left on device', but quota is not at hard limit (Used: ${PROJ_USED_KB:-unknown} KiB, Hard: ${PROJ_HARD_KB:-unknown} KiB)!" >&2
    exit 1
  fi
else
  echo "FAIL: 120 MiB write failed with unexpected error other than quota exceeded or ENOSPC at quota limit!" >&2
  exit 1
fi

echo "OK: Write failed with expected quota error. Matched case: $MATCHED_CASE"

echo "Cleaning up test-writer pod and test files..."
kubectl exec test-writer -- rm -f /mnt/nfs/test-50m.bin /mnt/nfs/test-120m.bin || true
$SUDO rm -f "$EXPORT_DIR/pvc-e2e/test-50m.bin" "$EXPORT_DIR/pvc-e2e/test-120m.bin" || true
kubectl delete pod test-writer --wait=true

# --------------------------------------------------------------------------
# Supplementary: document (not hide) the root-writer / CAP_SYS_RESOURCE
# hard-limit bypass. ext4 project quota hard limits are not enforced for a
# writer holding CAP_SYS_RESOURCE (fs/quota/dquot.c ignore_hardlimit()), and
# knfsd performs the write with credentials mapped from the incoming NFS
# client, so a root pod over an export with no_root_squash writes as root on
# the server and inherits the bypass. Repeat the 120 MiB write with a root
# writer on all three backends: ext4 must SUCCEED with the over-quota flag
# set in repquota (the bypass, documented); xfs and btrfs must still FAIL
# exactly like the non-root case (their project quotas/qgroups enforce
# regardless of privilege).
# --------------------------------------------------------------------------
echo "Deploying test-writer-root pod (root writer, documents the CAP_SYS_RESOURCE hard-limit bypass)..."
kubectl apply -f "$MANIFESTS_DIR/test-writer-root.yaml"
if ! kubectl wait --for=condition=Ready pod/test-writer-root --timeout=120s; then
  echo "FAIL: test-writer-root pod did not become Ready in time!" >&2
  kubectl describe pod test-writer-root >&2 || true
  exit 1
fi

echo "Writing 120 MiB to PVC as root (100 MiB quota; ext4 expected to SUCCEED via the documented bypass, $FS expected to FAIL)..."
set +e
WRITE_120M_ROOT_EXEC=$(kubectl exec test-writer-root -- sh -c 'dd if=/dev/zero of=/mnt/nfs/test-120m-root.bin bs=1M count=120 conv=fsync 2>&1; echo rc=$?')
set -e
echo "Full root-writer command output (including rc marker):"
echo "$WRITE_120M_ROOT_EXEC"
WRITE_120M_ROOT_RC=$(echo "$WRITE_120M_ROOT_EXEC" | awk -F'rc=' '/rc=/{print $2}' | tail -1)

echo "Host $FS native quota report AFTER root-writer 120 MiB write:"
FS_REPORT_ROOT_WRITE=$(get_native_quota_report)
echo "$FS_REPORT_ROOT_WRITE"

assert_root_writer_outcome "$FS_REPORT_ROOT_WRITE" "$WRITE_120M_ROOT_RC"

echo "Cleaning up test-writer-root pod and test files..."
kubectl exec test-writer-root -- rm -f /mnt/nfs/test-120m-root.bin || true
$SUDO rm -f "$EXPORT_DIR/pvc-e2e/test-120m-root.bin" || true
kubectl delete pod test-writer-root --wait=true

assert_no_registry_pull_events "Stage D"

echo "=================================================================="
echo "Stage D outcome summary ($FS):"
echo "  1. Non-root writer, 50 MiB write (under 100 MiB quota): SUCCEEDED (expected)"
echo "  2. Non-root writer, 120 MiB write (over 100 MiB quota): FAILED -- $MATCHED_CASE (expected: quota enforced)"
echo "  3. Root writer, 120 MiB write (over 100 MiB quota): $ROOT_WRITER_OUTCOME"
echo "=================================================================="

STAGE_D_STATUS="PASS"
STAGE_D_DETAILS="PV status=applied, enforced-limit-bytes=$EXPECTED_ENFORCED_BYTES; $FS native quota hard limit verified; writer via NFS mount; non-root 120MiB: $MATCHED_CASE; root 120MiB: $ROOT_WRITER_OUTCOME"
echo "STAGE D PASSED"

# ==============================================================================
# STAGE E: Upgrade, Rollback, and Quota Preservation
# ==============================================================================
echo "=================================================================="
echo ">>> STAGE E: Helm Upgrade, Rollback & Quota Preservation ($FS)"
echo "=================================================================="
echo "Upgrading Helm release with harmless podAnnotation (trigger rolling update)..."
helm upgrade nfs-quota-agent "$BUNDLED_CHART" \
  --namespace nfs-quota-agent \
  --reuse-values \
  --set podAnnotations.e2e=upgraded

kubectl rollout status daemonset/nfs-quota-agent -n nfs-quota-agent --timeout=120s
sleep 5

echo "Re-asserting quota preservation after upgrade..."
STATUS_UPGRADE=$(kubectl get pv pv-e2e -o jsonpath='{.metadata.annotations.nfs\.io/quota-status}')
LIMIT_UPGRADE=$(kubectl get pv pv-e2e -o jsonpath='{.metadata.annotations.nfs\.io/enforced-limit-bytes}')
if [ "$STATUS_UPGRADE" != "applied" ] || [ "$LIMIT_UPGRADE" != "$EXPECTED_ENFORCED_BYTES" ]; then
  echo "FAIL: Quota annotations changed after upgrade: status=$STATUS_UPGRADE, limit=$LIMIT_UPGRADE" >&2
  exit 1
fi

FS_REPORT_UPGRADE=$(get_native_quota_report)
echo "$FS_REPORT_UPGRADE"
assert_quota_hard_limit "$FS_REPORT_UPGRADE" "post-upgrade"
echo "OK: Quota preserved across Helm upgrade."

echo "Rolling back Helm release to revision 1..."
helm rollback nfs-quota-agent 1 -n nfs-quota-agent
kubectl rollout status daemonset/nfs-quota-agent -n nfs-quota-agent --timeout=120s
sleep 5

echo "Re-asserting quota preservation after rollback..."
STATUS_ROLLBACK=$(kubectl get pv pv-e2e -o jsonpath='{.metadata.annotations.nfs\.io/quota-status}')
LIMIT_ROLLBACK=$(kubectl get pv pv-e2e -o jsonpath='{.metadata.annotations.nfs\.io/enforced-limit-bytes}')
if [ "$STATUS_ROLLBACK" != "applied" ] || [ "$LIMIT_ROLLBACK" != "$EXPECTED_ENFORCED_BYTES" ]; then
  echo "FAIL: Quota annotations changed after rollback: status=$STATUS_ROLLBACK, limit=$LIMIT_ROLLBACK" >&2
  exit 1
fi

FS_REPORT_ROLLBACK=$(get_native_quota_report)
echo "$FS_REPORT_ROLLBACK"
assert_quota_hard_limit "$FS_REPORT_ROLLBACK" "post-rollback"
echo "OK: Quota preserved across Helm rollback."

echo "Uninstalling Helm release..."
helm uninstall nfs-quota-agent -n nfs-quota-agent
kubectl wait --for=delete pod -l app.kubernetes.io/name=nfs-quota-agent -n nfs-quota-agent --timeout=60s || true

echo "Asserting $FS quota still exists on host after agent uninstall..."
FS_REPORT_UNINSTALL=$(get_native_quota_report)
echo "$FS_REPORT_UNINSTALL"
assert_quota_hard_limit "$FS_REPORT_UNINSTALL" "post-uninstall"
echo "OK: Quota remains on disk after uninstall; this is a regression guard because the agent has no preStop quota-removal path."
assert_no_registry_pull_events "Stage E"

STAGE_E_STATUS="PASS"
STAGE_E_DETAILS="Quota preserved across helm upgrade (rev 2), rollback (rev 1), and helm uninstall on $FS"
echo "STAGE E PASSED"

echo "=================================================================="
echo ">>> ALL AIR-GAP E2E STAGES PASSED SUCCESSFULLY"
echo "=================================================================="
