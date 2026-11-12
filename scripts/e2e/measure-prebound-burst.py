#!/usr/bin/env python3
"""Prebind behind the provisioner gate, then release through normal PV WATCH."""

import copy
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import time
import uuid

from burst_diagnosis import BurstDiagnosis
from binding_diagnosis import POLL_SECONDS

SPEC = importlib.util.spec_from_file_location(
  "latency", Path(__file__).with_name("measure-enforcement-latency.py"))
latency = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(latency)
run = latency.run
DS = ["-n", "nfs-quota-agent", "daemonset/nfs-quota-agent"]
PROVISIONER = "pv.kubernetes.io/provisioned-by"


def patch_args(index, args):
  run(["kubectl", "--request-timeout=10s", "patch", *DS, "--type=json", "-p",
       json.dumps([{"op": "replace", "path": f"/spec/template/spec/containers/{index}/args",
                    "value": args}])])
  run(["kubectl", "--request-timeout=10s", "rollout", "status", *DS, "--timeout=40s"])


def list_pvs(selector):
  return json.loads(run(["kubectl", "--request-timeout=10s", "get", "pv", "-l", selector,
                         "-o", "json"]))["items"]


def agent_snapshot(ds_uid):
  pods = json.loads(run(["kubectl", "--request-timeout=10s", "get", "pods", "-n",
                         "nfs-quota-agent", "-o", "json"]))["items"]
  snapshot = {}
  for pod in pods:
    if not any(owner.get("uid") == ds_uid for owner in pod["metadata"].get("ownerReferences", [])):
      continue
    container = next((c for c in pod.get("status", {}).get("containerStatuses", [])
                      if c["name"] == "nfs-quota-agent"), None)
    if not container or not container.get("ready") or not container.get("containerID"):
      raise RuntimeError("Agent is not ready; startup sync may contaminate measurement")
    snapshot[pod["metadata"]["uid"]] = (container["containerID"], container["restartCount"])
  if not snapshot:
    raise RuntimeError("No ready agent pods found")
  return snapshot


def wait_command(process, completed):
  process.wait()
  completed.append(time.monotonic())


def result_for(fs, names, observations, total, command_seconds, diagnosis):
  values = [row["latency_ms"] for row in observations.values()]
  times = sorted(row["latency_ms"] / 1000 for row in observations.values())
  span = times[-1] - times[0]
  return {
    **{key: diagnosis.get(key) for key in
       ("phase_seconds_total", "phase_seconds_mean", "phase_time_share", "reconciles_counted")},
    "fs": fs, "path": "watch", "N": len(names), "total_seconds": round(total, 3),
    "observations_per_second": (len(names) - 1) / span if span > 0 else None,
    "mean_consecutive_observation_gap_seconds": span / (len(names) - 1),
    "p50_ms": latency.percentile(values, 50), "p90_ms": latency.percentile(values, 90),
    "p99_ms": latency.percentile(values, 99), "max_ms": max(values),
    "eligibility_command_seconds": round(command_seconds, 6),
    "effective_qps_burst_logged": diagnosis.get("effective_qps_burst_logged"),
    "agent_log_diagnosis": diagnosis, "pvs": observations,
    "measurement": "eligibility command start to first LIST response observing applied; monotonic client clock",
    "resolution": "0.5s polling plus LIST duration; command runs concurrently with polling",
    "percentile_method": "nearest rank",
    "total_definition": "eligibility command start to all applied observed and command completed",
    "rate_definition": "(N-1)/(last-first) client observation seconds (not agent apply rate); null if all in one poll",
    "limitations": "Includes kubectl annotation fan-out, API and queue overhead. Periodic sync delayed to 10m; "
                   "pod restarts invalidate the measurement; disconnected watches can delay applies. No timing thresholds.",
  }


def measure(fs, export, gateway, manifests, summary):
  text = os.environ.get("E2E_LATENCY_PVS", "100")
  if not text.isascii() or not text.isdigit() or int(text) < 5:
    raise ValueError("E2E_LATENCY_PVS must be an integer >= 5")
  count = int(text)
  sudo = [] if os.geteuid() == 0 else ["sudo"]
  label = f"prebound-{uuid.uuid4().hex[:10]}"
  selector = f"e2e.nfs.io/prebound-run={label}"
  names = [f"{label}-{i:04d}" for i in range(count)]
  expected = 100000000 if fs == "btrfs" else 99999744
  ds = json.loads(run(["kubectl", "get", *DS, "-o", "json"]))
  containers = ds["spec"]["template"]["spec"]["containers"]
  index = next(i for i, c in enumerate(containers) if c["name"] == "nfs-quota-agent")
  original = containers[index]["args"]
  provisioner = next((arg.split("=", 1)[1] for arg in original
                      if arg.startswith("--provisioner-name=")), "nfs.csi.k8s.io")
  gated = [arg for arg in original if not arg.startswith(("--process-all-nfs", "--sync-interval"))]
  # D1: restore the native-NFS provisioner gate only for this phase. Cost: two
  # rollouts; escape: finally restores exact args. A 10m sync interval keeps
  # the normal Modified/WATCH queue as the intended measurement path.
  gated += ["--process-all-nfs=false", "--sync-interval=10m"]
  created_dirs, process, diagnosis = [], None, None
  restore = False
  try:
    restore = True  # patch/rollout can fail after changing the template
    patch_args(index, gated)
    # D4: exclude startup sync by checking pod identity/restarts across the window.
    # Cost: two pod LISTs; escape: fail this sample and rerun after stabilization.
    agents = agent_snapshot(ds["metadata"]["uid"])
    template = Path(manifests, "pvc-e2e.yaml").read_text().replace("__GATEWAY_IP__", gateway)
    templates = latency.parse_objects(run(
      ["kubectl", "create", "--dry-run=client", "-f", "-", "-o", "json"], input=template))
    objects = []
    for name in names:
      path = f"{export}/{name}"
      run(sudo + (["btrfs", "subvolume", "create", path] if fs == "btrfs" else ["mkdir", path]))
      created_dirs.append(path)
      run(sudo + ["chmod", "777", path])
      for source in templates:
        obj = copy.deepcopy(source)
        obj["metadata"]["name"] = name
        obj["metadata"].setdefault("labels", {})["e2e.nfs.io/prebound-run"] = label
        obj["metadata"].setdefault("annotations", {}).pop(PROVISIONER, None)
        if obj["kind"] == "PersistentVolume":
          obj["spec"]["nfs"]["path"] = path
        else:
          obj["spec"]["volumeName"] = name
        objects.append(obj)
    run(["kubectl", "--request-timeout=10s", "create", "-f", "-"],
        input=json.dumps({"apiVersion": "v1", "kind": "List", "items": objects}))
    deadline = time.monotonic() + 60
    while time.monotonic() < deadline:
      pvs = list_pvs(selector)
      if any(pv["metadata"].get("annotations", {}).get(PROVISIONER) == provisioner or
             pv["metadata"].get("annotations", {}).get("nfs.io/quota-status") == "applied"
             for pv in pvs):
        raise RuntimeError("PV became eligible/applied before prebinding completed")
      if {pv["metadata"]["name"] for pv in pvs if pv.get("status", {}).get("phase") == "Bound"} == set(names):
        claims = json.loads(run(["kubectl", "--request-timeout=10s", "get", "pvc", "-n", "default",
                                 "-l", selector, "-o", "json"]))["items"]
        if {claim["metadata"]["name"] for claim in claims
            if claim.get("status", {}).get("phase") == "Bound"} == set(names):
          break
      time.sleep(POLL_SECONDS)
    else:
      raise RuntimeError("Not all prebound PVs Bound within 60s")
    diagnosis = BurstDiagnosis(f"{fs}-prebound", names, run)
    diagnosis.fs = fs
    with tempfile.TemporaryDirectory(prefix="quota-prebound-") as tmp:
      with Path(tmp, "annotate.log").open("w+") as log:
        # D2: poll while kubectl fans out updates, preserving early applies.
        # Cost: LIST overhead and staggered eligibility; escape: report command
        # duration separately, never interpret the result as pure kernel rate.
        observations, command_seconds, completed = {}, None, []
        diagnosis.start = time.time()
        start = time.monotonic()
        process = subprocess.Popen(
          ["kubectl", "--request-timeout=10s", "annotate", "pv", "-l", selector,
           f"{PROVISIONER}={provisioner}", "--overwrite"], stdout=log, stderr=subprocess.STDOUT)
        waiter = threading.Thread(target=wait_command, args=(process, completed), daemon=True)
        waiter.start()
        deadline = start + 90
        while time.monotonic() < deadline:
          code = process.poll()
          if code not in (None, 0):
            log.seek(0)
            raise RuntimeError(f"Eligibility command failed: {log.read()}")
          if completed and code == 0:
            command_seconds = completed[0] - start
          pvs = list_pvs(selector)
          observed = time.monotonic()
          for pv in pvs:
            name = pv["metadata"]["name"]
            annotations = pv["metadata"].get("annotations", {})
            if name not in names or name in observations or annotations.get("nfs.io/quota-status") != "applied":
              continue
            if annotations.get("nfs.io/enforced-limit-bytes") != str(expected):
              raise RuntimeError(f"Wrong enforced-limit-bytes on {name}: {annotations}")
            observations[name] = {"latency_ms": (observed - start) * 1000,
                                  "observed_unix_seconds": time.time()}
          if len(observations) == count and command_seconds is not None:
            break
          time.sleep(POLL_SECONDS)
        else:
          raise RuntimeError(f"Not all {count} prebound PVs applied within 90s")
        total, end = time.monotonic() - start, time.time()
    if agent_snapshot(ds["metadata"]["uid"]) != agents:
      raise RuntimeError("Agent restarted/replaced during measurement; WATCH sample invalid")
    details = diagnosis.finish(end) or {}
    diagnosis = None
    latency.verify_samples(fs, export, names, expected, sudo)
    result = result_for(fs, names, observations, total, command_seconds, details)
    output = json.dumps(result, indent=2) + "\n"
    Path(f"/tmp/e2e-prebound-burst-{fs}.json").write_text(output)
    print("BEGIN E2E PREBOUND BURST JSON", flush=True)
    print(output, end="", flush=True)
    print("END E2E PREBOUND BURST JSON", flush=True)
    if summary:
      with open(summary, "a") as stream:
        stream.write("\n### Prebound quota burst (WATCH)\n\n"
                     "| FS | Path | N | Total s | Observations/s | Mean observation gap s | p50 ms | p90 ms | p99 ms | max ms | Eligibility command s |\n"
                     "|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n"
                     f"| {fs} | watch | {count} | {result['total_seconds']} | {result['observations_per_second']} | "
                     f"{result['mean_consecutive_observation_gap_seconds']} | {result['p50_ms']} | {result['p90_ms']} | "
                     f"{result['p99_ms']} | {result['max_ms']} | {result['eligibility_command_seconds']} |\n\n"
                     "0.5s polling plus LIST overhead; all Bound before annotation command; "
                     "five native hard limits verified. No timing thresholds. QPS/burst in JSON (null if logs absent).\n")
  finally:
    if process is not None and process.poll() is None:
      process.kill()
      process.wait()
    errors = []
    # D3: attempt every cleanup and restoration even after partial creation.
    # Cost: bounded API waits; escape: fail correctness and retain CI logs.
    commands = [
      ["kubectl", "--request-timeout=10s", "delete", "pvc", "-n", "default", "-l", selector,
       "--ignore-not-found", "--wait=true", "--timeout=20s"],
      ["kubectl", "--request-timeout=10s", "delete", "pv", "-l", selector,
       "--ignore-not-found", "--wait=true", "--timeout=20s"],
    ]
    for path in reversed(created_dirs):
      commands.append(sudo + (["btrfs", "subvolume", "delete", path] if fs == "btrfs" else ["rmdir", path]))
    for command in commands:
      # D5: retry transient host cleanup after the explicit API deletion wait.
      # Cost: at most two polling delays per path; escape: retain the final error.
      attempts = 1 if command[0] == "kubectl" else 3
      for attempt in range(attempts):
        try:
          run(command)
          break
        except (subprocess.SubprocessError, OSError) as error:
          if attempt + 1 == attempts:
            errors.append(str(error))
          else:
            time.sleep(POLL_SECONDS)
    if restore:
      try:
        patch_args(index, original)
      except (subprocess.SubprocessError, OSError) as error:
        errors.append(f"restore agent args: {error}")
    if errors:
      raise RuntimeError(f"Prebound cleanup failed: {errors}")


if __name__ == "__main__":
  measure(*sys.argv[1:])
