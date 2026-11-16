#!/usr/bin/env bash
# Sourced by run-airgap-e2e.sh; all host mutations are confined to the CI runner.

setup_k3s_cluster() {
  if [[ "${GITHUB_ACTIONS:-}" != "true" || "$(uname -m)" != "x86_64" || "$FS" != "xfs" ]]; then
    echo "FAIL: k3s lane requires a GitHub Actions amd64 runner and xfs" >&2
    return 1
  fi
  # D1: official release pins, fetched via gh release download sha256sum-amd64.txt.
  # Cost: manual pin updates; escape: select kind (the default provider).
  local version='v1.34.1+k3s1'
  local binary_sha='ec8958fe1363a32d50e028b5e3411c04b0dbd0316fab33524e8094352b78f6a3'
  local images_sha='27355d2838c727180f0a8ded8aec0ac14e2186bd50487a3c3f29500884121aa7'
  local base="https://github.com/k3s-io/k3s/releases/download/${version/+/%2B}"
  local download_dir
  download_dir=$(mktemp -d)
  curl -fL --retry 3 "$base/k3s" -o "$download_dir/k3s"
  curl -fL --retry 3 "$base/k3s-airgap-images-amd64.tar.zst" -o "$download_dir/k3s-airgap-images-amd64.tar.zst"
  echo "$binary_sha  $download_dir/k3s" | sha256sum -c -
  echo "$images_sha  $download_dir/k3s-airgap-images-amd64.tar.zst" | sha256sum -c -
  $SUDO install -m 0755 "$download_dir/k3s" /usr/local/bin/k3s
  $SUDO mkdir -p /var/lib/rancher/k3s/agent/images /etc/rancher/k3s
  $SUDO install -m 0644 "$download_dir/k3s-airgap-images-amd64.tar.zst" /var/lib/rancher/k3s/agent/images/
  rm -rf "$download_dir"

  # D2: reject every CRI registry via a closed loopback mirror, including setup.
  # Cost: missing preloads fail startup; escape: fix the archive, never enable pulls.
  # https://docs.k3s.io/installation/private-registry (wildcard + disabled fallback).
  cat <<'YAML' | $SUDO tee /etc/rancher/k3s/registries.yaml >/dev/null
mirrors:
  "*":
    endpoint:
      - "http://127.0.0.1:1"
YAML
  cat <<'YAML' | $SUDO tee /etc/rancher/k3s/config.yaml >/dev/null
write-kubeconfig-mode: "0600"
disable:
  - traefik
  - servicelb
  - metrics-server
  - local-storage
disable-helm-controller: true
disable-default-registry-endpoint: true
node-name: airgap-e2e
node-label:
  - nfs-server=true
YAML
  # systemd owns the server and its containerd; journal is available on failures.
  $SUDO systemd-run --unit=k3s-e2e --property=Delegate=yes \
    --property=KillMode=process /usr/local/bin/k3s server --config /etc/rancher/k3s/config.yaml
  # Use the release's kubectl, avoiding hosted-runner client/server version skew.
  mkdir -p /tmp/k3s-e2e-bin
  printf '%s\n' '#!/bin/sh' 'exec /usr/local/bin/k3s kubectl "$@"' > /tmp/k3s-e2e-bin/kubectl
  chmod 0755 /tmp/k3s-e2e-bin/kubectl
  export PATH="/tmp/k3s-e2e-bin:$PATH"
  export KUBECONFIG="/tmp/k3s-e2e-kubeconfig"
  local ready=false
  for ((attempt=0; attempt<180; attempt++)); do
    if $SUDO test -s /etc/rancher/k3s/k3s.yaml; then
      $SUDO install -m 0600 -o "$(id -u)" /etc/rancher/k3s/k3s.yaml "$KUBECONFIG"
      if kubectl get node airgap-e2e >/dev/null 2>&1; then
        ready=true
        break
      fi
    fi
    sleep 2
  done
  if [[ "$ready" != true ]] || ! kubectl wait --for=condition=Ready node/airgap-e2e --timeout=180s; then
    $SUDO journalctl -u k3s-e2e --no-pager -n 150 >&2
    return 1
  fi
  kubectl version -o yaml
  $SUDO k3s --version
  # kubelet mounts NFS on the runner; pods reach this same routable node address
  # through flannel. No Docker bridge gateway exists in the k3s topology.
  GATEWAY_IP=$(kubectl get node airgap-e2e -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
  [[ "$GATEWAY_IP" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]
  echo "k3s runner InternalIP (NFS endpoint): $GATEWAY_IP"
  $SUDO showmount -e "$GATEWAY_IP"
  $SUDO mkdir -p /tmp/nfs-probe
  $SUDO mount -t nfs -o vers=4 "$GATEWAY_IP:$EXPORT_DIR/pvc-e2e" /tmp/nfs-probe
  findmnt -T /tmp/nfs-probe
  $SUDO umount /tmp/nfs-probe

  docker pull busybox:1.36
  docker save busybox:1.36 -o /tmp/k3s-busybox.tar
  $SUDO k3s ctr -n k8s.io images import /tmp/k3s-busybox.tar
  rm -f /tmp/k3s-busybox.tar
  assert_k3s_registry_block
}

assert_k3s_registry_block() {
  # Negative CRI probes bypass Kubernetes events and cannot be cached images.
  # These must fail at loopback; a missing image alone does not prove isolation.
  local registry output
  for registry in docker.io ghcr.io; do
    if output=$($SUDO timeout 30 k3s crictl pull "$registry/nfs-quota-e2e/blocked:never" 2>&1); then
      echo "FAIL: k3s unexpectedly pulled from $registry" >&2
      return 1
    fi
    echo "$output"
    if ! grep -q '127.0.0.1:1.*connect: connection refused' <<< "$output"; then
      echo "FAIL: CRI did not demonstrate closed-loopback rejection for $registry" >&2
      return 1
    fi
  done
  echo "OK: k3s CRI registry fallback disabled; loopback rejection observed."
}
