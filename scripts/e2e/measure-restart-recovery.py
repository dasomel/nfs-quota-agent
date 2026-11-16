#!/usr/bin/env python3
"""D3: force-delete the real agent during a D2-style bulk create."""

import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import uuid

from burst_resources import burst_objects
from binding_diagnosis import POLL_SECONDS

SPEC = importlib.util.spec_from_file_location(
  "latency", Path(__file__).with_name("measure-enforcement-latency.py"))
latency = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(latency)
run = latency.run
NAMESPACE = ["-n", "nfs-quota-agent"]


def kill_decision(applied, count):
  # D4: keep one PV pending even at N=10. Cost: earlier kill;
  # escape: fail an already-complete burst instead of claiming recovery.
  mid = min(10, count - 1) <= applied < count
  return mid, mid


def sample_indices(names, applied):
  # D1: sample actual observed kill frontier, not an assumed creation order.
  # Cost: five native queries; escape: all btrfs rows are checked separately.
  point = names.index(applied[-1]) if applied else 0
  candidates = [0, len(names) - 1, point - 1, point, point + 1, *range(len(names))]
  selected = []
  for index in candidates:
    if 0 <= index < len(names) and index not in selected:
      selected.append(index)
    if len(selected) == 5:
      return selected


def check_mappings(projects, projid, paths):
  entries = [line.strip().split(":", 1) for line in projects.splitlines()
             if line.strip() and not line.lstrip().startswith("#")]
  named_ids = [line.strip().split(":", 1) for line in projid.splitlines()
               if ":" in line and not line.lstrip().startswith("#")]
  named_ids = [(name, int(value) if value.isdigit() else None) for name, value in named_ids]
  ids = [value for _, value in named_ids]
  seen = set()
  for path in paths:
    matches = [row[0] for row in entries if len(row) == 2 and row[1] == path]
    if len(matches) != 1:
      raise RuntimeError(f"Mapping path must occur exactly once: {path}: {matches}")
    project = matches[0]
    project = int(project) if project.isdigit() else 0
    if project == 0 or project in seen or ids.count(project) != 1:
      raise RuntimeError(f"Missing/duplicate/shared project ID: {path}: {project}")
    # D5: these burst PV names equal path basenames; mirror getProjectName.
    # Cost: naming-rule coupling; escape: pass explicit names if fixtures change.
    name = "pv_" + Path(path).name.replace("-", "_")[:32]
    if [(key, value) for key, value in named_ids if key == name or value == project] != [(name, project)]:
      raise RuntimeError(f"Project name/ID mismatch: {path}: {name}: {project}")
    seen.add(project)
  return {"consistent": True, "kind": "projects/projid", "paths_checked": len(paths)}


def agent_pods(ds_uid):
  data = json.loads(run(["kubectl", "--request-timeout=10s", "get", "pods", *NAMESPACE, "-o", "json"]))
  return [pod for pod in data["items"] if any(
    owner.get("uid") == ds_uid for owner in pod["metadata"].get("ownerReferences", []))]


def ready(pod):
  return (not pod["metadata"].get("deletionTimestamp") and any(
    c.get("type") == "Ready" and c.get("status") == "True"
    for c in pod.get("status", {}).get("conditions", [])))


def container_status(pod):
  container = next((c for c in pod.get("status", {}).get("containerStatuses", [])
                    if c["name"] == "nfs-quota-agent"), {})
  return {"restart_count": container.get("restartCount"),
          "termination_reason": (container.get("state", {}).get("terminated") or
                                 container.get("lastState", {}).get("terminated") or {}).get("reason")}


def wait_replacement(ds_uid, old_uid, deadline):
  while time.monotonic() < deadline:
    for pod in agent_pods(ds_uid):
      if pod["metadata"]["uid"] != old_uid and ready(pod):
        return pod
    time.sleep(POLL_SECONDS)
  raise RuntimeError("No different-UID Ready replacement agent within recovery timeout")


def cleanup(fs, selector, paths, sudo):
  errors = []
  commands = [
    ["kubectl", "--request-timeout=10s", "delete", "pvc", "-n", "default", "-l", selector,
     "--ignore-not-found", "--wait=true", "--timeout=20s"],
    ["kubectl", "--request-timeout=10s", "delete", "pv", "-l", selector,
     "--ignore-not-found", "--wait=true", "--timeout=20s"],
  ]
  commands += [sudo + (["btrfs", "subvolume", "delete", path] if fs == "btrfs" else ["rmdir", path])
               for path in reversed(paths)]
  # D2: attempt all deletions even after failure. Cost: bounded API waits;
  # escape: fail the stage and retain logs, never edit host mapping files.
  for command in commands:
    for attempt in range(3 if command[0] != "kubectl" else 1):
      try:
        run(command)
        break
      except (subprocess.SubprocessError, OSError) as error:
        if command[0] == "kubectl" or attempt == 2:
          errors.append(str(error))
        else:
          time.sleep(POLL_SECONDS)
  if errors:
    raise RuntimeError(f"Recovery cleanup failed: {errors}")


def measure(fs, export, gateway, manifests, summary):
  text = os.environ.get("E2E_RECOVERY_PVS", "60")
  if not text.isascii() or not text.isdigit() or int(text) < 10:
    raise ValueError("E2E_RECOVERY_PVS must be an integer >= 10")
  count = int(text)
  sudo = [] if os.geteuid() == 0 else ["sudo"]
  label = f"recovery-{uuid.uuid4().hex[:10]}"
  selector = f"e2e.nfs.io/recovery-run={label}"
  names = [f"{label}-{i + 1:04d}" for i in range(count)]
  expected = 100000000 if fs == "btrfs" else 99999744
  ds = json.loads(run(["kubectl", "get", *NAMESPACE, "daemonset/nfs-quota-agent", "-o", "json"]))
  ds_uid = ds["metadata"]["uid"]
  pods = agent_pods(ds_uid)
  if len(pods) != 1 or not ready(pods[0]):
    raise RuntimeError("D3 requires exactly one Ready agent (single-node kind)")
  old = pods[0]
  old_uid = old["metadata"]["uid"]
  template = Path(manifests, "pvc-e2e.yaml").read_text().replace("__GATEWAY_IP__", gateway)
  templates = latency.parse_objects(run(
    ["kubectl", "create", "--dry-run=client", "-f", "-", "-o", "json"], input=template))
  objects = burst_objects(templates, names, export, "e2e.nfs.io/recovery-run", label)
  paths, creator, killed, replacement = [], None, None, None
  start = time.monotonic()
  try:
    for name in names:
      path = f"{export}/{name}"
      run(sudo + (["btrfs", "subvolume", "create", path] if fs == "btrfs" else ["mkdir", path]))
      paths.append(path)
      run(sudo + ["chmod", "777", path])
    with tempfile.TemporaryDirectory(prefix="quota-recovery-") as tmp:
      manifest = Path(tmp, "burst.json")
      manifest.write_text(json.dumps({"apiVersion": "v1", "kind": "List", "items": objects}))
      with Path(tmp, "create.log").open("w+") as log:
        creator = subprocess.Popen(["kubectl", "--request-timeout=15s", "create", "-f", str(manifest)],
                                   stdout=log, stderr=subprocess.STDOUT)
        deadline = time.monotonic() + 300
        applied_before, mid_burst, ready_seconds, applied_seconds = None, False, None, None
        while time.monotonic() < deadline:
          if creator.poll() not in (None, 0):
            log.seek(0)
            raise RuntimeError(f"Bulk create failed: {log.read()}")
          pvs = json.loads(run(["kubectl", "--request-timeout=10s", "get", "pv", "-l", selector,
                                "-o", "json"]))["items"]
          applied = []
          for pv in pvs:
            annotations = pv["metadata"].get("annotations", {})
            if pv["metadata"]["name"] not in names or annotations.get("nfs.io/quota-status") != "applied":
              continue
            if annotations.get("nfs.io/enforced-limit-bytes") != str(expected):
              raise RuntimeError(f"Wrong enforced-limit-bytes: {pv['metadata']['name']}")
            if pv.get("status", {}).get("phase") == "Bound":
              applied.append(pv["metadata"]["name"])
          should_kill, mid = kill_decision(len(applied), count)
          if killed is None and len(applied) == count:
            raise RuntimeError("Burst completed before agent kill; retry with more PVs")
          if killed is None and should_kill:
            applied_before, mid_burst = len(applied), mid
            samples = sample_indices(names, applied)
            killed = time.monotonic()
            deadline = killed + 300
            run(["kubectl", "--request-timeout=10s", "delete", "pod", old["metadata"]["name"],
                 *NAMESPACE, "--grace-period=0", "--force", "--wait=false"])
          if killed is not None:
            if applied_seconds is None and len(applied) == count:
              applied_seconds = time.monotonic() - killed
            if replacement is None:
              replacement = next((pod for pod in agent_pods(ds_uid)
                                  if pod["metadata"]["uid"] != old_uid and ready(pod)), None)
              if replacement:
                ready_seconds = time.monotonic() - killed
            if replacement and len(applied) == count and creator.poll() == 0:
              break
          time.sleep(POLL_SECONDS)
        else:
          raise RuntimeError(f"Recovery incomplete within 300s: applied={len(applied)}/{count}")
    claims = json.loads(run(["kubectl", "--request-timeout=10s", "get", "pvc", "-n", "default",
                             "-l", selector, "-o", "json"]))["items"]
    if {claim["metadata"]["name"] for claim in claims
        if claim.get("status", {}).get("phase") == "Bound"} != set(names):
      raise RuntimeError("Not all recovery PVCs are Bound")
    latency.verify_samples(fs, export, names, expected, sudo, indices=samples)
    if fs == "btrfs":
      latency.verify_samples(fs, export, names, expected, sudo, indices=range(count))
      mapping = {"consistent": True, "kind": "qgroup", "paths_checked": count}
    else:
      # D3: chart mounts kind-node /etc files, kind extraMounts bind host /etc.
      # Read the actual new pod mounts (no local /etc access). Cost: two execs;
      # escape: fail if those mappings disagree with the N exported paths.
      prefix = ["kubectl", "--request-timeout=10s", "exec", *NAMESPACE,
                replacement["metadata"]["name"], "-c", "nfs-quota-agent", "--", "cat"]
      mapping = check_mappings(run(prefix + ["/etc/projects"]), run(prefix + ["/etc/projid"]), paths)
    result = {"fs": fs, "N": count, "applied_before_kill": applied_before,
              "killed_mid_burst": mid_burst, "old_pod_uid": old_uid,
              "new_pod_uid": replacement["metadata"]["uid"], "new_pod_ready": True,
              "kill_to_ready_seconds": round(ready_seconds, 3),
              "kill_to_all_applied_seconds": round(applied_seconds, 3),
              "total_seconds": round(time.monotonic() - start, 3),
              "mapping_consistency": mapping, "old_agent_container": container_status(old),
              "new_agent_container": container_status(replacement)}
  finally:
    if creator is not None and creator.poll() is None:
      creator.kill()
      creator.wait()
    try:
      cleanup(fs, selector, paths, sudo)
    finally:
      if killed is not None:
        wait_replacement(ds_uid, old_uid, time.monotonic() + 300)
  output = json.dumps(result, indent=2) + "\n"
  Path(f"/tmp/e2e-restart-recovery-{fs}.json").write_text(output)
  print("BEGIN E2E RESTART RECOVERY JSON", flush=True)
  print(output, end="", flush=True)
  print("END E2E RESTART RECOVERY JSON", flush=True)
  if summary:
    with open(summary, "a") as stream:
      stream.write("\n### D3: Agent restart during a burst\n\n"
                   "| FS | N | Applied before kill | Mid burst | Ready s | All applied s | Total s | Mapping |\n"
                   "|---|---:|---:|---|---:|---:|---:|---|\n"
                   f"| {fs} | {count} | {applied_before} | {mid_burst} | {ready_seconds:.3f} | "
                   f"{applied_seconds:.3f} | {result['total_seconds']} | consistent |\n\n"
                   "Force-deletes a pod; does not reboot a node or drop an export. "
                   "0.5s polling plus API overhead; timing is descriptive, 300s recovery timeout. "
                   "Total includes directory setup and checks, excludes cleanup.\n")


if __name__ == "__main__":
  measure(*sys.argv[1:])
