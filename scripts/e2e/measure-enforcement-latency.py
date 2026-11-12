#!/usr/bin/env python3
"""CI-only real-filesystem burst measurement; invoked after Air-Gap Stage D."""

import copy
import datetime
import json
import math
import os
from pathlib import Path
import platform
import subprocess
import sys
import tempfile
import time
import uuid

from burst_diagnosis import BurstDiagnosis
from binding_diagnosis import POLL_SECONDS, record_bounds


def run(args, **kwargs):
  return subprocess.check_output(args, text=True, timeout=30, **kwargs).strip()


def percentile(values, percent):
  return sorted(values)[math.ceil(len(values) * percent / 100) - 1]


def parse_objects(text):
  # kubectl prints several objects as concatenated JSON documents (seen on the
  # CI runner: "Extra data" from json.loads), or as one List; accept both.
  decoder = json.JSONDecoder()
  objects, index = [], 0
  while index < len(text):
    if text[index].isspace():
      index += 1
      continue
    obj, index = decoder.raw_decode(text, index)
    objects.extend(obj["items"] if "items" in obj else [obj])
  return objects


def verify_samples(fs, export, names, expected, sudo):
  # D2: numeric project/qgroup IDs bind rows to actual directories, avoiding
  # host name-map ambiguity. Cost: lsattr/subvolume queries; escape: Stage D.
  if fs == "xfs":
    report = run(sudo + ["xfs_quota", "-x", "-c", "report -p -n", export])
  elif fs == "ext4":
    report = run(sudo + ["repquota", "-P", "-n", export])
  else:
    report = run(sudo + ["btrfs", "qgroup", "show", "-re", "--raw", export])
  print(report, flush=True)
  indices = sorted({round(i * (len(names) - 1) / 4) for i in range(5)})
  for index in indices:
    path = f"{export}/{names[index]}"
    if fs == "btrfs":
      info = run(sudo + ["btrfs", "subvolume", "show", path])
      subvol_id = next(line.split(":", 1)[1].strip() for line in info.splitlines()
                       if line.strip().startswith("Subvolume ID:"))
      selector = f"0/{subvol_id}"
    else:
      project_id = run(sudo + ["lsattr", "-p", "-d", path]).split()[0]
      if not project_id.isdigit() or int(project_id) == 0:
        raise RuntimeError(f"Invalid project ID for {path}: {project_id}")
      selector = f"#{project_id}"
    rows = [line.split() for line in report.splitlines()
            if line.split() and line.split()[0] == selector]
    if len(rows) != 1:
      raise RuntimeError(f"Missing/ambiguous quota row for {path}: {selector}")
    row = rows[0]
    hard_column = 4 if fs == "ext4" and row[1] in ("--", "+-", "-+", "++") else 3
    hard = int(row[hard_column])
    wanted = expected if fs == "btrfs" else expected // 1024
    if hard != wanted:
      raise RuntimeError(f"Wrong native hard limit for {path}: {hard} != {wanted}")
    print(f"OK: sample {index + 1}/{len(names)} {path}: hard={hard}", flush=True)


def measure(fs, export, gateway, manifests, summary):
  count_text = os.environ.get("E2E_LATENCY_PVS", "100")
  if not count_text.isascii() or not count_text.isdigit() or int(count_text) < 5:
    raise ValueError("E2E_LATENCY_PVS must be an integer >= 5 (five distinct samples)")
  count = int(count_text)
  sudo = [] if os.geteuid() == 0 else ["sudo"]
  label = f"latency-{uuid.uuid4().hex[:12]}"
  selector = f"e2e.nfs.io/latency-run={label}"
  names = [f"{label}-{i + 1:04d}" for i in range(count)]
  expected = 100000000 if fs == "btrfs" else (100000000 // 1024) * 1024
  template = Path(manifests, "pvc-e2e.yaml").read_text().replace("__GATEWAY_IP__", gateway)
  templates = parse_objects(run(["kubectl", "create", "--dry-run=client", "-f", "-", "-o", "json"],
                                input=template))
  objects = []
  for name in names:
    for original in templates:
      obj = copy.deepcopy(original)
      obj["metadata"]["name"] = name
      obj["metadata"].setdefault("labels", {})["e2e.nfs.io/latency-run"] = label
      if obj["kind"] == "PersistentVolume":
        obj["spec"]["nfs"]["path"] = f"{export}/{name}"
      else:
        obj["spec"]["volumeName"] = name
      objects.append(obj)
  created_dirs = []
  creator = None
  diagnosis = None
  burst_end = None
  try:
    for name in names:
      path = f"{export}/{name}"
      command = ["btrfs", "subvolume", "create", path] if fs == "btrfs" else ["mkdir", path]
      run(sudo + command)
      created_dirs.append(path)
      run(sudo + ["chmod", "777", path])
    with tempfile.TemporaryDirectory(prefix="quota-latency-") as tmp:
      manifest = Path(tmp, "burst.json")
      manifest.write_text(json.dumps({"apiVersion": "v1", "kind": "List", "items": objects}))
      with Path(tmp, "create.log").open("w+") as log:
        # D1: submit one bulk List while polling concurrently so early PVs do
        # not inherit the entire submit duration. Cost: polling/API overhead;
        # escape: tune N. PVC binding is required by shouldProcessPV.
        diagnosis = BurstDiagnosis(fs, names, run)
        start = time.monotonic()
        creator = subprocess.Popen(["kubectl", "--request-timeout=15s", "create", "-f", str(manifest)],
                                   stdout=log, stderr=subprocess.STDOUT)
        deadline = start + 180
        observations = {}
        bounds = {}
        while time.monotonic() < deadline:
          if creator.poll() not in (None, 0):
            log.seek(0)
            raise RuntimeError(f"Bulk create failed: {log.read()}")
          data = json.loads(run(["kubectl", "--request-timeout=10s", "get", "pv", "-l", selector,
                                 "-o", "json"]))
          observed = time.time()
          record_bounds(data["items"], bounds, observed)
          for pv in data["items"]:
            meta = pv["metadata"]
            name = meta["name"]
            annotations = meta.get("annotations", {})
            if annotations.get("nfs.io/quota-status") != "applied" or name in observations:
              continue
            if annotations.get("nfs.io/enforced-limit-bytes") != str(expected):
              raise RuntimeError(f"Wrong enforced-limit-bytes on {name}: {annotations}")
            created = datetime.datetime.fromisoformat(meta["creationTimestamp"].replace("Z", "+00:00"))
            observations[name] = {
              "creation_timestamp": meta["creationTimestamp"],
              "observed_unix_seconds": observed,
              "latency_ms": round((observed - created.timestamp()) * 1000, 3),
            }
          if len(observations) == count and creator.poll() == 0:
            break
          time.sleep(POLL_SECONDS)
        else:
          missing = sorted(set(names) - observations.keys())
          raise RuntimeError(f"Not all {count} PVs applied within 180s; missing={missing}")
        total = time.monotonic() - start
        burst_end = time.time()
        for name, row in observations.items():
          row["first_bound_observed_unix_seconds"] = bounds.get(name)
          row["apply_minus_bound_ms"] = (round((row["observed_unix_seconds"] - bounds[name]) * 1000, 3)
                                         if name in bounds else None)
        diagnosis.finish(burst_end, observations, bounds)
        diagnosis = None
      verify_samples(fs, export, names, expected, sudo)
      pods = json.loads(run(["kubectl", "get", "pods", "-n", "nfs-quota-agent",
                             "-l", "app.kubernetes.io/name=nfs-quota-agent", "-o", "json"]))
      containers = pods["items"][0]["status"]["containerStatuses"]
      cpu = next((line.split(":", 1)[1].strip() for line in Path("/proc/cpuinfo").read_text().splitlines()
                  if line.startswith("model name")), platform.machine())
      values = [entry["latency_ms"] for entry in observations.values()]
      result = {
        "fs": fs, "kernel": platform.release(), "N": count,
        "total_seconds": round(total, 3),
        "p50_ms": percentile(values, 50), "p90_ms": percentile(values, 90),
        "p99_ms": percentile(values, 99), "max_ms": max(values),
        "agent_image": containers[0]["image"], "agent_image_id": containers[0]["imageID"],
        "revision": run(["git", "rev-parse", "HEAD"]),
        "runner_cpu_model": cpu, "nproc": int(run(["nproc"])),
        "measurement": "PV creationTimestamp to first client LIST response observing quota-status=applied",
        "resolution": "API creationTimestamp: 1s; polling: 0.5s plus LIST duration; client clock vs API clock",
        "percentile_method": "nearest rank",
        "total_definition": "monotonic seconds before bulk create to all applied observed and create completed",
        "pvs": observations,
      }
      output = Path(f"/tmp/e2e-enforcement-latency-{fs}.json")
      output.write_text(json.dumps(result, indent=2) + "\n")
      print("BEGIN E2E ENFORCEMENT LATENCY JSON", flush=True)
      print(output.read_text(), end="", flush=True)
      print("END E2E ENFORCEMENT LATENCY JSON", flush=True)
      if summary:
        with open(summary, "a") as stream:
          stream.write("\n### Real-filesystem quota application latency\n\n"
                       "| FS | N | Total s | p50 ms | p90 ms | p99 ms | max ms |\n"
                       "|---|---:|---:|---:|---:|---:|---:|\n"
                       f"| {fs} | {count} | {total:.3f} | {result['p50_ms']} | {result['p90_ms']} | "
                       f"{result['p99_ms']} | {result['max_ms']} |\n\n"
                       "Creation-to-first-observed applied annotation; 1s creation timestamp, "
                       "0.5s polling plus LIST overhead. Includes binding and queueing; "
                       "sampled native hard limits verified. No latency thresholds.\n")
  finally:
    if creator is not None and creator.poll() is None:
      creator.kill()
      creator.wait()
    if diagnosis is not None:
      diagnosis.finish(burst_end or time.time())
    # D3: delete claims first to release PV protection, then PVs, even on a
    # partial create/timeout. Retain matches Stage D; leave host mapping files
    # to the agent. Cost: bounded cleanup; escape: ephemeral CI runner teardown.
    cleanup_errors = []
    commands = [
      ["kubectl", "--request-timeout=10s", "delete", "pvc", "-n", "default", "-l", selector,
       "--ignore-not-found", "--timeout=20s"],
      ["kubectl", "--request-timeout=10s", "delete", "pv", "-l", selector,
       "--ignore-not-found", "--timeout=20s"],
    ]
    for path in reversed(created_dirs):
      command = ["btrfs", "subvolume", "delete", path] if fs == "btrfs" else ["rmdir", path]
      commands.append(sudo + command)
    for command in commands:
      try:
        run(command)
      except (subprocess.SubprocessError, OSError) as error:
        cleanup_errors.append(str(error))
    if cleanup_errors:
      raise RuntimeError(f"Latency-stage cleanup failed: {cleanup_errors}")


if __name__ == "__main__":
  measure(*sys.argv[1:])
