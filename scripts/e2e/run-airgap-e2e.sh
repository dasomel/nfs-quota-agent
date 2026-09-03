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

resolve_project_quota_target() {
  PROJ_ID=$($SUDO lsattr -p -d "$EXPORT_DIR/pvc-e2e" 2>/dev/null | awk 'NR == 1 {print $1}' || true)
  if ! [[ "$PROJ_ID" =~ ^[0-9]+$ ]]; then
    echo "FAIL: could not resolve a numeric XFS project ID for $EXPORT_DIR/pvc-e2e" >&2
    exit 1
  fi

  # shellcheck disable=SC2016 # awk fields must remain literal for awk, not the shell.
  PROJ_NAME=$($SUDO awk -F: -v id="$PROJ_ID" '$2 == id {print $1; exit}' /etc/projid 2>/dev/null || true)
  if [ -n "$PROJ_NAME" ]; then
    if ! [[ "$PROJ_NAME" =~ ^[a-zA-Z0-9_-]+$ ]]; then
      echo "FAIL: resolved unsafe project name: $PROJ_NAME" >&2
      exit 1
    fi
    PROJECT_REPORT_SELECTOR="$PROJ_NAME"
  else
    PROJECT_REPORT_SELECTOR="#${PROJ_ID}"
  fi

  XFS_PROJECT_PATH=$($SUDO xfs_quota -x -c 'print' "$EXPORT_DIR" | awk -v id="$PROJ_ID" '$1 == "project" && $2 == "=" && $3 == id && $4 == ":" {print $5; exit}')
  if [ "$XFS_PROJECT_PATH" != "$EXPORT_DIR/pvc-e2e" ]; then
    echo "FAIL: XFS project $PROJECT_REPORT_SELECTOR (id $PROJ_ID) maps to ${XFS_PROJECT_PATH:-none}, not $EXPORT_DIR/pvc-e2e" >&2
    exit 1
  fi

  echo "Resolved XFS project: name=${PROJ_NAME:-none}, id=$PROJ_ID, path=$XFS_PROJECT_PATH"
}

project_report_line() {
  local report="$1"
  # The selector is an exact first field, so root project #0 cannot satisfy this gate.
  printf '%s\n' "$report" | grep -E "^[[:space:]]*${PROJECT_REPORT_SELECTOR}[[:space:]]+" | head -1 || true
}

assert_project_hard_limit() {
  local report="$1"
  local phase="$2"
  local line
  local used_kb
  local hard_kb

  line=$(project_report_line "$report")
  echo "Resolved $phase project quota line: ${line:-none}"
  if [ -z "$line" ]; then
    echo "FAIL: $phase XFS quota report has no line for $PROJECT_REPORT_SELECTOR" >&2
    exit 1
  fi
  used_kb=$(awk '{print $2}' <<<"$line")
  hard_kb=$(awk '{print $4}' <<<"$line")
  if ! [[ "$used_kb" =~ ^[0-9]+$ && "$hard_kb" =~ ^[0-9]+$ ]] || [ "$hard_kb" -ne 102400 ]; then
    echo "FAIL: $phase project quota line must have numeric used KiB and hard limit 102400 KiB: $line" >&2
    exit 1
  fi
}

write_summary() {
  if [ -n "$STEP_SUMMARY" ]; then
    cat <<EOF >> "$STEP_SUMMARY"
### Air-Gap E2E Quota Test Results (#5)
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
# STAGE A: Host Filesystem Setup (XFS prjquota + NFS export)
# ==============================================================================
echo "=================================================================="
echo ">>> STAGE A: Host Filesystem Setup"
echo "=================================================================="
bash scripts/e2e/setup-xfs-nfs.sh
STAGE_A_STATUS="PASS"
STAGE_A_DETAILS="2GiB XFS sparse file mounted at $EXPORT_DIR with prjquota; nfs-kernel-server active"
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
echo "Preloading busybox:1.36 into kind cluster..."
docker pull busybox:1.36
kind load docker-image busybox:1.36 --name "$CLUSTER_NAME"

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
  echo "$EXPORT_DIR *(rw,sync,no_root_squash,no_subtree_check,insecure,fsid=0)" | $SUDO tee "$EXPORTS_FILE" >/dev/null
  echo "$EXPORT_DIR $KIND_SUBNET(rw,sync,no_root_squash,no_subtree_check,insecure,fsid=0)" | $SUDO tee -a "$EXPORTS_FILE" >/dev/null
  $SUDO exportfs -ra
fi

echo "Host RPC services (rpcinfo -p):"
$SUDO rpcinfo -p || true

# Verify NFS connectivity from kind node
echo "Testing NFS connectivity from kind node to $GATEWAY_IP..."
docker exec "$KIND_NODE" showmount -e "$GATEWAY_IP"

STAGE_B_STATUS="PASS"
STAGE_B_DETAILS="kind cluster $CLUSTER_NAME running; node labeled nfs-server=true; gateway $GATEWAY_IP"
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

echo "Container images on kind node:"
docker exec "$KIND_NODE" crictl images

# Install via Helm using chart from bundle only, pullPolicy=Never, matching loaded image tag
BUNDLED_CHART=$(find "$BUNDLE_DIR/chart" -maxdepth 1 -name "*.tgz" | head -1)
echo "Installing Helm chart from bundle: $BUNDLED_CHART"
helm install nfs-quota-agent "$BUNDLED_CHART" \
  --namespace nfs-quota-agent \
  --create-namespace \
  --set image.pullPolicy=Never \
  --set image.tag=e2e \
  --set config.nfsBasePath=/srv/nfs-export \
  --set config.nfsServerPath=/srv/nfs-export \
  --set nfsExport.hostPath=/srv/nfs-export \
  --set config.processAllNFS=true

# Deploy static PV and PVC
echo "Deploying static PV and PVC..."
sed "s|__GATEWAY_IP__|$GATEWAY_IP|g" "$MANIFESTS_DIR/pvc-e2e.yaml" | kubectl apply -f -

STAGE_C_STATUS="PASS"
STAGE_C_DETAILS="bundle verified; image loaded from OCI archive (e2e, digest: $IMAGE_DIGEST); helm installed with pullPolicy=Never"
echo "STAGE C PASSED"

# ==============================================================================
# STAGE D: Proof of Real Quota Enforcement
# ==============================================================================
echo "=================================================================="
echo ">>> STAGE D: Real Quota Enforcement Proof"
echo "=================================================================="
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
  exit 1
fi

EXPECTED_ENFORCED_BYTES=104857600
echo "Observed PV annotations: nfs.io/quota-status=applied, nfs.io/enforced-limit-bytes=$ENFORCED_BYTES"
if [ "$ENFORCED_BYTES" -ne "$EXPECTED_ENFORCED_BYTES" ]; then
  echo "FAIL: enforced-limit-bytes $ENFORCED_BYTES != expected $EXPECTED_ENFORCED_BYTES" >&2
  exit 1
fi

echo "Inspecting host XFS quota report (xfs_quota -x -c 'report -p' $EXPORT_DIR)..."
XFS_REPORT=$($SUDO xfs_quota -x -c 'report -p' "$EXPORT_DIR")
echo "$XFS_REPORT"

# Resolve and bind the report to the actual project assigned to the PV directory.
resolve_project_quota_target
assert_project_hard_limit "$XFS_REPORT" "initial"
echo "OK: Host XFS quota report matches $PROJECT_REPORT_SELECTOR at 102400 KiB ($EXPECTED_ENFORCED_BYTES bytes)"

# Deploy test-writer pod to test filesystem enforcement
echo "Deploying test-writer pod..."
kubectl apply -f "$MANIFESTS_DIR/test-writer.yaml"
kubectl wait --for=condition=Ready pod/test-writer --timeout=120s

echo "Writer pod status and placement (kubectl get pod test-writer -o wide):"
kubectl get pod test-writer -o wide

echo "Writer pod description (kubectl describe pod test-writer):"
kubectl describe pod test-writer

echo "Writer pod mounts for /mnt/nfs (mount | grep /mnt/nfs):"
kubectl exec test-writer -- sh -c 'mount | grep /mnt/nfs || mount'

echo "Writer pod filesystem free space (df -h /mnt/nfs):"
kubectl exec test-writer -- df -h /mnt/nfs

echo "Writing 50 MiB to PVC (should succeed)..."
kubectl exec test-writer -- dd if=/dev/zero of=/mnt/nfs/test-50m.bin bs=1M count=50 conv=fsync
echo "OK: 50 MiB write succeeded."

echo "Host XFS quota report BEFORE 120 MiB write ($SUDO xfs_quota -x -c 'report -p' $EXPORT_DIR):"
XFS_REPORT_BEFORE=$($SUDO xfs_quota -x -c 'report -p' "$EXPORT_DIR")
echo "$XFS_REPORT_BEFORE"

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

echo "Host XFS quota report AFTER 120 MiB write ($SUDO xfs_quota -x -c 'report -p' $EXPORT_DIR):"
XFS_REPORT_AFTER=$($SUDO xfs_quota -x -c 'report -p' "$EXPORT_DIR")
echo "$XFS_REPORT_AFTER"

echo "Host filesystem free space on $EXPORT_DIR (df -h $EXPORT_DIR):"
df -h "$EXPORT_DIR"

echo "Kernel log messages for quota / XFS / EDQUOT / ENOSPC:"
$SUDO dmesg | grep -iE "quota|xfs|edquot|enospc" | tail -n 25 || true

if [ "$WRITE_120M_RC" -eq 0 ]; then
  echo "FAIL: 120 MiB write unexpectedly succeeded despite 100 MiB quota limit!" >&2
  exit 1
fi

# Extract used and hard-limit KiB only from the resolved PV project, never #0.
PROJECT_LINE=$(project_report_line "$XFS_REPORT_AFTER")
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

MATCHED_CASE=""
if echo "$WRITE_120M_OUT" | grep -qi "quota"; then
  MATCHED_CASE="EDQUOT (Disk quota exceeded)"
elif echo "$WRITE_120M_OUT" | grep -qi "No space left on device"; then
  # Only accept ENOSPC ("No space left on device") if the project quota is at its hard limit.
  # XFS project quotas simulate partition boundaries and intentionally return ENOSPC rather than EDQUOT.
  if [ -n "$PROJ_HARD_KB" ] && [ -n "$PROJ_USED_KB" ] && [ "$PROJ_USED_KB" -eq "$PROJ_USED_KB" ] 2>/dev/null && [ "$PROJ_HARD_KB" -eq "$PROJ_HARD_KB" ] 2>/dev/null && [ "$PROJ_USED_KB" -ge "$PROJ_HARD_KB" ]; then
    MATCHED_CASE="ENOSPC (No space left on device) with project at hard limit (${PROJ_USED_KB} KiB >= ${PROJ_HARD_KB} KiB)"
  else
    echo "FAIL: 120 MiB write failed with 'No space left on device', but project quota is not at hard limit (Used: ${PROJ_USED_KB:-unknown} KiB, Hard: ${PROJ_HARD_KB:-unknown} KiB)!" >&2
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

STAGE_D_STATUS="PASS"
STAGE_D_DETAILS="PV status=applied, enforced-limit-bytes=104857600; xfs_quota hard limit=102400 KiB; $MATCHED_CASE"
echo "STAGE D PASSED"

# ==============================================================================
# STAGE E: Upgrade, Rollback, and Quota Preservation
# ==============================================================================
echo "=================================================================="
echo ">>> STAGE E: Helm Upgrade, Rollback & Quota Preservation"
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

XFS_REPORT_UPGRADE=$($SUDO xfs_quota -x -c 'report -p' "$EXPORT_DIR")
echo "$XFS_REPORT_UPGRADE"
assert_project_hard_limit "$XFS_REPORT_UPGRADE" "post-upgrade"
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

XFS_REPORT_ROLLBACK=$($SUDO xfs_quota -x -c 'report -p' "$EXPORT_DIR")
echo "$XFS_REPORT_ROLLBACK"
assert_project_hard_limit "$XFS_REPORT_ROLLBACK" "post-rollback"
echo "OK: Quota preserved across Helm rollback."

echo "Uninstalling Helm release..."
helm uninstall nfs-quota-agent -n nfs-quota-agent
kubectl wait --for=delete pod -l app.kubernetes.io/name=nfs-quota-agent -n nfs-quota-agent --timeout=60s || true

echo "Asserting XFS project quota still exists on host after agent uninstall..."
XFS_REPORT_UNINSTALL=$($SUDO xfs_quota -x -c 'report -p' "$EXPORT_DIR")
echo "$XFS_REPORT_UNINSTALL"
assert_project_hard_limit "$XFS_REPORT_UNINSTALL" "post-uninstall"
echo "OK: Quota remains on disk after uninstall; this is a regression guard because the agent has no preStop quota-removal path."

STAGE_E_STATUS="PASS"
STAGE_E_DETAILS="Quota preserved across helm upgrade (rev 2), rollback (rev 1), and helm uninstall"
echo "STAGE E PASSED"

echo "=================================================================="
echo ">>> ALL AIR-GAP E2E STAGES PASSED SUCCESSFULLY"
echo "=================================================================="
