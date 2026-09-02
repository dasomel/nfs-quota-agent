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
  docker exec "$KIND_NODE" apt-get update
  docker exec "$KIND_NODE" apt-get install -y nfs-common
fi

# Preload busybox for write tests (setup phase)
echo "Preloading busybox:1.36 into kind cluster..."
docker pull busybox:1.36
kind load docker-image busybox:1.36 --name "$CLUSTER_NAME"

# Obtain Gateway IP for kind network
GATEWAY_IP=$(docker network inspect kind -f '{{(index .IPAM.Config 0).Gateway}}')
echo "Docker bridge gateway IP (NFS server endpoint): $GATEWAY_IP"

# Verify NFS connectivity from kind node
echo "Testing NFS connectivity from kind node..."
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
skopeo copy "oci-archive:$BUNDLE_DIR/images/nfs-quota-agent-image.tar" "docker-archive:/tmp/agent-docker.tar:ghcr.io/dasomel/nfs-quota-agent:e2e"
kind load image-archive /tmp/agent-docker.tar --name "$CLUSTER_NAME"
rm -f /tmp/agent-docker.tar

# Tag the image in containerd with digest-pinned reference
docker exec "$KIND_NODE" ctr -n k8s.io images tag "ghcr.io/dasomel/nfs-quota-agent:e2e" "ghcr.io/dasomel/nfs-quota-agent@$IMAGE_DIGEST"
echo "Container images on kind node:"
docker exec "$KIND_NODE" crictl images

# Install via Helm using chart from bundle only, pullPolicy=Never, digest-pinned
BUNDLED_CHART=$(find "$BUNDLE_DIR/chart" -maxdepth 1 -name "*.tgz" | head -1)
echo "Installing Helm chart from bundle: $BUNDLED_CHART"
helm install nfs-quota-agent "$BUNDLED_CHART" \
  --namespace nfs-quota-agent \
  --create-namespace \
  --set image.pullPolicy=Never \
  --set image.digest="$IMAGE_DIGEST" \
  --set config.nfsBasePath=/srv/nfs-export \
  --set config.nfsServerPath=/srv/nfs-export \
  --set nfsExport.hostPath=/srv/nfs-export \
  --set config.processAllNFS=true

# Deploy static PV and PVC
echo "Deploying static PV and PVC..."
sed "s|__GATEWAY_IP__|$GATEWAY_IP|g" "$MANIFESTS_DIR/pvc-e2e.yaml" | kubectl apply -f -

STAGE_C_STATUS="PASS"
STAGE_C_DETAILS="bundle verified; image loaded from OCI archive (digest: $IMAGE_DIGEST); helm installed with pullPolicy=Never"
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
# 100Mi = 102400 KiB blocks
if ! echo "$XFS_REPORT" | grep -E "(pvc-e2e|[0-9]+)[[:space:]]+[0-9]+[[:space:]]+0[[:space:]]+102400"; then
  echo "FAIL: xfs_quota report does not show expected 102400 KiB hard limit for project!" >&2
  exit 1
fi
echo "OK: Host XFS quota report matches expected hard limit (102400 KiB blocks = $EXPECTED_ENFORCED_BYTES bytes)"

# Deploy test-writer pod to test filesystem enforcement
echo "Deploying test-writer pod..."
kubectl apply -f "$MANIFESTS_DIR/test-writer.yaml"
kubectl wait --for=condition=Ready pod/test-writer --timeout=120s

echo "Writing 50 MiB to PVC (should succeed)..."
kubectl exec test-writer -- dd if=/dev/zero of=/mnt/nfs/test-50m.bin bs=1M count=50
echo "OK: 50 MiB write succeeded."

echo "Writing 120 MiB to PVC (100 MiB quota, must FAIL with EDQUOT)..."
set +e
WRITE_120M_OUT=$(kubectl exec test-writer -- dd if=/dev/zero of=/mnt/nfs/test-120m.bin bs=1M count=120 2>&1)
WRITE_120M_RC=$?
set -e

echo "Output of 120 MiB write (exit code $WRITE_120M_RC):"
echo "$WRITE_120M_OUT"

if [ "$WRITE_120M_RC" -eq 0 ]; then
  echo "FAIL: 120 MiB write unexpectedly succeeded despite 100 MiB quota limit!" >&2
  exit 1
fi

if ! echo "$WRITE_120M_OUT" | grep -qi "quota"; then
  echo "FAIL: 120 MiB write failed with error other than quota exceeded!" >&2
  exit 1
fi
echo "OK: Write failed with expected quota error (EDQUOT / Disk quota exceeded)"

echo "Cleaning up test-writer pod and test files..."
kubectl exec test-writer -- rm -f /mnt/nfs/test-50m.bin /mnt/nfs/test-120m.bin || true
kubectl delete pod test-writer --wait=true

STAGE_D_STATUS="PASS"
STAGE_D_DETAILS="PV status=applied, enforced-limit-bytes=104857600; xfs_quota hard limit=102400 KiB; EDQUOT observed"
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
if ! echo "$XFS_REPORT_UPGRADE" | grep -E "(pvc-e2e|[0-9]+)[[:space:]]+[0-9]+[[:space:]]+0[[:space:]]+102400"; then
  echo "FAIL: On-disk XFS quota report changed after upgrade!" >&2
  exit 1
fi
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
if ! echo "$XFS_REPORT_ROLLBACK" | grep -E "(pvc-e2e|[0-9]+)[[:space:]]+[0-9]+[[:space:]]+0[[:space:]]+102400"; then
  echo "FAIL: On-disk XFS quota report changed after rollback!" >&2
  exit 1
fi
echo "OK: Quota preserved across Helm rollback."

echo "Uninstalling Helm release..."
helm uninstall nfs-quota-agent -n nfs-quota-agent
kubectl wait --for=delete pod -l app.kubernetes.io/name=nfs-quota-agent -n nfs-quota-agent --timeout=60s || true

echo "Asserting XFS project quota still exists on host after agent uninstall..."
XFS_REPORT_UNINSTALL=$($SUDO xfs_quota -x -c 'report -p' "$EXPORT_DIR")
echo "$XFS_REPORT_UNINSTALL"
if ! echo "$XFS_REPORT_UNINSTALL" | grep -E "(pvc-e2e|[0-9]+)[[:space:]]+[0-9]+[[:space:]]+0[[:space:]]+102400"; then
  echo "FAIL: Quota was stripped from disk on uninstall!" >&2
  exit 1
fi
echo "OK: Quota correctly preserved on disk after agent uninstall."

STAGE_E_STATUS="PASS"
STAGE_E_DETAILS="Quota preserved across helm upgrade (rev 2), rollback (rev 1), and helm uninstall"
echo "STAGE E PASSED"

echo "=================================================================="
echo ">>> ALL AIR-GAP E2E STAGES PASSED SUCCESSFULLY"
echo "=================================================================="
