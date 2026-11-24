#!/usr/bin/env python3
"""D4a: take NFS offline while the agent applies quotas to local directories."""

import importlib.util
import json
import os
from pathlib import Path
import re
import signal
import socket
import subprocess
import sys
import time
import uuid

from burst_resources import burst_objects

SPEC = importlib.util.spec_from_file_location(
  "recovery", Path(__file__).with_name("measure-restart-recovery.py"))
recovery = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(recovery)
run = recovery.run
KUBECTL = ["kubectl", "--request-timeout=10s"]
BASE_PV = "pv-e2e"
BASE_DIR = "pvc-e2e"
WRITER = "test-writer"
BLOCK_BYTES = 1024 * 1024


def active_service(server, sudo):
  candidates = (["nfs-ganesha.service"] if server == "ganesha" else
                ["nfs-server.service", "nfs-kernel-server.service"])
  for service in candidates:
    try:
      run(sudo + ["systemctl", "is-active", "--quiet", service])
    except subprocess.CalledProcessError:
      continue
    return service
  raise RuntimeError(f"No active NFS service among {candidates}")


def nfs_port_open(gateway):
  try:
    with socket.create_connection((gateway, 2049), timeout=1):
      return True
  except ConnectionRefusedError:
    return False


def wait_for_nfs(gateway):
  deadline = time.monotonic() + 120
  while time.monotonic() < deadline:
    try:
      if nfs_port_open(gateway):
        return time.monotonic()
    except TimeoutError:
      pass
    time.sleep(recovery.POLL_SECONDS)
  raise RuntimeError(f"NFS listener on {gateway}:2049 did not recover")


def restore_server(sudo, service, gateway):
  run(sudo + ["systemctl", "start", service], timeout=120)
  return wait_for_nfs(gateway)


def applied_pvs(selector, expected):
  command = (["get", "pv", "-l", selector] if selector else ["get", "pv", BASE_PV])
  response = json.loads(run(KUBECTL + command + ["-o", "json"]))
  pvs = response["items"] if selector else [response]
  applied = set()
  for pv in pvs:
    metadata = pv["metadata"]
    annotations = metadata.get("annotations", {})
    if pv.get("status", {}).get("phase") != "Bound" or annotations.get("nfs.io/quota-status") != "applied":
      continue
    if annotations.get("nfs.io/enforced-limit-bytes") != str(expected):
      raise RuntimeError(f"Wrong enforced limit for {metadata['name']}")
    applied.add(metadata["name"])
  return applied


def wait_applied(selector, names, expected):
  deadline = time.monotonic() + 120
  while time.monotonic() < deadline:
    if applied_pvs(selector, expected) == set(names):
      return time.monotonic()
    time.sleep(recovery.POLL_SECONDS)
  raise RuntimeError(f"PV quotas were not all applied while NFS was stopped: {names}")


def agent_identity(ds_uid):
  pods = recovery.agent_pods(ds_uid)
  if len(pods) != 1 or not recovery.ready(pods[0]):
    raise RuntimeError("D4 requires one Ready agent")
  pod = pods[0]
  restart_count = recovery.container_status(pod)["restart_count"]
  if restart_count is None:
    raise RuntimeError("Agent restart count unavailable")
  return pod["metadata"]["name"], pod["metadata"]["uid"], restart_count


def native_snapshot(fs, export, names, expected, sudo, pod):
  samples = recovery.latency.verify_samples(fs, export, names, expected, sudo, indices=range(len(names)))
  ids = {name: value[0] for name, value in samples.items()}
  if len(set(ids.values())) != len(names):
    raise RuntimeError(f"Shared project or qgroup ID: {ids}")
  if fs != "btrfs":
    prefix = KUBECTL + ["exec", *recovery.NAMESPACE, pod, "-c", "nfs-quota-agent", "--", "cat"]
    projects = run(prefix + ["/etc/projects"])
    projid = run(prefix + ["/etc/projid"])
    paths = [f"{export}/{name}" for name in names]
    recovery.check_mappings(projects, projid, paths,
                            [BASE_PV if name == BASE_DIR else name for name in names])
    mapped = {fields[1]: int(fields[0]) for line in projects.splitlines()
              if (fields := line.strip().split(":", 1))[0].isdigit() and len(fields) == 2}
    if any(ids[name] != f"#{mapped[path]}" for name, path in zip(names, paths)):
      raise RuntimeError("Project mappings disagree with on-disk directory IDs")
  return ids


def prove_nfs_write(fs, export, expected, sudo, manifests):
  # D2: reuse Stage D's non-root writer and directly exported baseline PVC.
  # Cost: one fresh mount; per-PV native read-back covers the outage cohort.
  prefix = ["kubectl", "exec", WRITER, "--"]
  filename = "export-loss-write.bin"
  remote = f"/mnt/nfs/{filename}"
  try:
    run(KUBECTL + ["create", "-f", str(Path(manifests, "test-writer.yaml"))])
    run(["kubectl", "wait", "--for=condition=Ready", f"pod/{WRITER}", "--timeout=120s"],
        timeout=130)
    mounts = run(prefix + ["cat", "/proc/self/mountinfo"])
    if not re.search(r" /mnt/nfs .* - nfs4? ", mounts):
      raise RuntimeError("Post-recovery writer has no NFS mount")
    run(prefix + ["dd", "if=/dev/zero", f"of={remote}", "bs=1M", "count=1", "conv=fsync"],
        timeout=120)
    output = run(prefix + ["sh", "-c",
                 'dd if=/dev/zero "of=$1" bs=1M count=120 conv=fsync 2>&1; echo rc=$?',
                 "sh", remote], timeout=120)
    print(output, flush=True)
    if not re.search(r"^rc=[1-9][0-9]*$", output, re.MULTILINE):
      raise RuntimeError("Post-recovery over-quota write did not fail")
    samples = recovery.latency.verify_samples(fs, export, [BASE_DIR], expected, sudo)
    used = samples[BASE_DIR][1]
    if re.search(r"disk quota exceeded", output, re.IGNORECASE):
      return "EDQUOT"
    if "No space left on device" in output and used <= expected < used + BLOCK_BYTES:
      return "ENOSPC at quota hard limit"
    raise RuntimeError(f"Unexpected over-quota write failure: used={used}, hard={expected}")
  finally:
    # D3: test-writer runs `sleep` as PID 1, which ignores SIGTERM, so deletion
    # always waits the full 30s grace period. Cost: slower cleanup; escape: none.
    try:
      run(["kubectl", "delete", "pod", WRITER, "--ignore-not-found", "--timeout=120s"], timeout=130)
    finally:
      run(sudo + ["rm", "-f", f"{export}/{BASE_DIR}/{filename}"])


def measure(fs, export, gateway, manifests, cluster, server):
  sudo = [] if os.geteuid() == 0 else ["sudo"]
  service = active_service(server, sudo)
  if not nfs_port_open(gateway):
    raise RuntimeError("NFS listener unavailable before D4")
  label = f"export-loss-{uuid.uuid4().hex[:10]}"
  key = "e2e.nfs.io/export-loss-run"
  selector = f"{key}={label}"
  names = [f"{label}-{index}" for index in range(1, 4)]
  expected = 100000000 if fs == "btrfs" else 99999744
  ds = json.loads(run(KUBECTL + ["get", *recovery.NAMESPACE, "daemonset/nfs-quota-agent", "-o", "json"]))
  ds_uid = ds["metadata"]["uid"]
  identity = agent_identity(ds_uid)
  pod = identity[0]
  applied_before = applied_pvs(None, expected)
  if applied_before != {BASE_PV}:
    raise RuntimeError("Baseline PV is not applied before outage")
  before = native_snapshot(fs, export, [BASE_DIR], expected, sudo, pod)
  template = Path(manifests, "pvc-e2e.yaml").read_text().replace("__GATEWAY_IP__", gateway)
  templates = recovery.latency.parse_objects(run(
    KUBECTL + ["create", "--dry-run=client", "-f", "-", "-o", "json"], input=template))
  manifest = json.dumps({"apiVersion": "v1", "kind": "List",
                         "items": burst_objects(templates, names, export, key, label)})
  paths = []
  try:
    for name in names:
      path = f"{export}/{name}"
      run(sudo + (["btrfs", "subvolume", "create", path] if fs == "btrfs" else ["mkdir", path]))
      paths.append(path)
    # D1: stop only NFS; both cluster setups bind the mounted filesystem into
    # the agent. Cost: service restart; escape: always restore on stage exit.
    restored = False
    try:
      run(sudo + ["systemctl", "stop", service], timeout=120)
      if run(sudo + ["ss", "-H", "-ltn", "sport = :2049"]) or nfs_port_open(gateway):
        raise RuntimeError("NFS still listening after service stop")
      outage_start = time.monotonic()
      run(KUBECTL + ["create", "-f", "-"], input=manifest)
      all_applied = wait_applied(selector, names, expected)
      during = native_snapshot(fs, export, [BASE_DIR, *names], expected, sudo, pod)
      if during[BASE_DIR] != before[BASE_DIR] or nfs_port_open(gateway):
        raise RuntimeError("Baseline mapping changed or NFS came back during the outage")
      recovered = restore_server(sudo, service, gateway)
      restored = True
    finally:
      if not restored:
        restore_server(sudo, service, gateway)
    after = applied_pvs(None, expected) | applied_pvs(selector, expected)
    rechecked = time.monotonic()
    if after != {BASE_PV, *names}:
      raise RuntimeError(f"Applied PVs changed after recovery: {after}")
    if native_snapshot(fs, export, [BASE_DIR, *names], expected, sudo, pod) != during:
      raise RuntimeError("Native quota IDs changed across NFS recovery")
    write_result = prove_nfs_write(fs, export, expected, sudo, manifests)
    if agent_identity(ds_uid) != identity:
      raise RuntimeError("Agent was replaced or restarted during the scenario")
  finally:
    recovery.cleanup(fs, selector, paths, sudo)

  result = {
    "fs": fs, "cluster": cluster, "nfs_server": server, "service": service,
    "outage_seconds": round(recovered - outage_start, 3),
    "pvs_created_during_outage": len(names),
    "applied_before_outage": len(applied_before),
    "new_pvs_applied_during_outage": len(names),
    "applied_after_recovery": len(after),
    "recovery_to_all_applied_seconds": round(all_applied - recovered, 3),
    "recovery_to_all_applied_recheck_seconds": round(rechecked - recovered, 3),
    "mapping_consistency": {"consistent": True, "paths_checked": len(after),
                            "kind": "qgroup" if fs == "btrfs" else "projects/projid"},
    "post_recovery_nfs_write": {"pv": BASE_PV, "over_quota_result": write_result},
  }
  # A negative recovery-to-apply offset records application during the outage.
  print("BEGIN E2E EXPORT LOSS JSON", flush=True)
  print(json.dumps(result, indent=2), flush=True)
  print("END E2E EXPORT LOSS JSON", flush=True)


def terminate(signum, _frame):
  raise SystemExit(128 + signum)


if __name__ == "__main__":
  signal.signal(signal.SIGTERM, terminate)
  measure(*sys.argv[1:])
