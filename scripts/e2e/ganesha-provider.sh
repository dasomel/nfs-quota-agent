#!/usr/bin/env bash
# Sourced by run-airgap-e2e.sh; host mutations run only in the E2E runner.

ganesha_diagnostics() {
  $SUDO cat /etc/ganesha/ganesha.conf
  $SUDO ss -ltnp 'sport = :2049'
  $SUDO systemctl status nfs-ganesha --no-pager || true
  $SUDO journalctl -u nfs-ganesha --no-pager -n 100 || true
  $SUDO tail -n 100 /var/log/ganesha/ganesha.log || true
}

assert_ganesha_server() {
  local listeners listener pid threads
  if [ -e /proc/fs/nfsd/threads ]; then
    threads=$($SUDO cat /proc/fs/nfsd/threads)
    if [[ "$threads" != 0 ]]; then
      echo "FAIL: kernel nfsd threads still running: $threads" >&2
      return 1
    fi
  fi
  if pgrep -x nfsd >/dev/null; then
    echo "FAIL: kernel nfsd process still present" >&2
    return 1
  fi
  listeners=$($SUDO ss -H -ltnp 'sport = :2049')
  if [ -z "$listeners" ]; then
    echo "FAIL: no TCP listener on NFS port 2049" >&2
    return 1
  fi
  # D2: inspect every listener's executable, not just service status/RPC version.
  # Cost: root /proc access; escape: kernel lane, never relax server identity.
  while IFS= read -r listener; do
    if [[ "$listener" != *'("ganesha.nfsd",'* ]]; then
      echo "FAIL: port 2049 listener is not ganesha.nfsd: $listener" >&2
      return 1
    fi
    if ! [[ "$listener" =~ pid=[0-9]+ ]]; then
      echo "FAIL: port 2049 process ownership unavailable" >&2
      return 1
    fi
    while IFS= read -r pid; do
      if [[ "$($SUDO readlink "/proc/$pid/exe")" != */ganesha.nfsd ]]; then
        echo "FAIL: port 2049 PID $pid does not execute ganesha.nfsd" >&2
        return 1
      fi
    done < <(grep -oE 'pid=[0-9]+' <<< "$listener" | cut -d= -f2)
  done <<< "$listeners"
  echo "$listeners"
  echo "OK: port 2049 owned by ganesha.nfsd; kernel nfsd absent."
}

setup_ganesha_server() {
  dpkg-query -W -f='${Package} ${Version}\n' nfs-ganesha nfs-ganesha-vfs
  # Stop and mask both Ubuntu names so later setup cannot reactivate knfsd.
  $SUDO systemctl disable --now nfs-kernel-server.service
  $SUDO systemctl mask --now nfs-server.service nfs-kernel-server.service
  $SUDO systemctl start rpcbind
  $SUDO mkdir -p /etc/ganesha
  # D3: match setup-xfs-nfs.sh's *(rw,sync,no_root_squash,...,insecure).
  # Pseudo equals the real path so existing vers=4 PV mounts stay unchanged.
  # Cost: CI-only unrestricted export; escape: remove this optional provider.
  # Stable writes/COMMIT use FSAL_VFS defaults; no Async or privileged-port gate.
  cat <<EOF | $SUDO tee /etc/ganesha/ganesha.conf >/dev/null
NFS_CORE_PARAM {
  Protocols = 4;
  NFS_Port = 2049;
  Enable_UDP = false;
}
EXPORT {
  Export_Id = 1;
  Path = "$EXPORT_DIR";
  Pseudo = "$EXPORT_DIR";
  Access_Type = RW;
  Squash = No_Root_Squash;
  SecType = sys;
  Protocols = 4;
  Transports = TCP;
  PrivilegedPort = false;
  FSAL { Name = VFS; }
}
EOF
  $SUDO systemctl restart nfs-ganesha
  local attempt
  for ((attempt=0; attempt<60; attempt++)); do
    if $SUDO timeout 5 rpcinfo -T tcp 127.0.0.1 100003 4 >/dev/null 2>&1; then
      assert_ganesha_server
      return
    fi
    sleep 2
  done
  echo "FAIL: Ganesha did not serve NFSv4 before timeout" >&2
  ganesha_diagnostics >&2
  return 1
}
