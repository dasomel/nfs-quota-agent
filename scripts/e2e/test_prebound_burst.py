"""Offline prebinding lifecycle and metric contract checks."""

import contextlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, patch

SPEC = importlib.util.spec_from_file_location(
  "prebound", Path(__file__).with_name("measure-prebound-burst.py"))
prebound = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(prebound)


class PreboundTests(unittest.TestCase):
  def exercise(self, failure=None, fs="xfs"):
    calls, objects, polls = [], [], []
    args = ["--process-all-nfs", "--sync-interval=30s", "--provisioner-name=nfs.csi.k8s.io"]
    gated = False
    eligible = False
    claim_polls = []
    process = Mock()
    process.poll.return_value = 0
    diagnosis = Mock()
    diagnosis.finish.return_value = {"effective_qps_burst_logged": [{"qps": 50, "burst": 100}]}
    templates = {"items": [
      {"kind": "PersistentVolume", "metadata": {"annotations": {prebound.PROVISIONER: "nfs.csi.k8s.io"}},
       "spec": {"nfs": {}}},
      {"kind": "PersistentVolumeClaim", "metadata": {}, "spec": {}},
    ]}

    def fake_run(command, **kwargs):
      nonlocal gated
      calls.append(command)
      if "patch" in command:
        new = json.loads(command[-1])[0]["value"]
        gated = "--process-all-nfs=false" in new
        return ""
      if "get" in command and "daemonset/nfs-quota-agent" in command:
        return json.dumps({"spec": {"template": {"spec": {"containers": [
          {"name": "nfs-quota-agent", "args": args}]}}}})
      if "--dry-run=client" in command:
        return json.dumps(templates)
      if "create" in command and "-f" in command:
        objects.extend(json.loads(kwargs["input"])["items"])
        self.assertTrue(gated)
        self.assertTrue(all(prebound.PROVISIONER not in obj["metadata"]["annotations"] for obj in objects))
        if failure == "create":
          raise subprocess.CalledProcessError(1, command)
      if "get" in command and "pvc" in command:
        claim_polls.append(command)
        return json.dumps({"items": [{"metadata": obj["metadata"], "status": {
          "phase": "Pending" if len(claim_polls) == 1 else "Bound"}}
                                     for obj in objects if obj["kind"] == "PersistentVolumeClaim"]})
      if "get" in command and "pv" in command:
        polls.append(command)
        pvs = [obj for obj in objects if obj["kind"] == "PersistentVolume"]
        for i, pv in enumerate(pvs):
          pv["status"] = {"phase": "Available" if len(polls) == 1 and i == 0 else "Bound"}
          if eligible and failure != "timeout":
            pv["metadata"]["annotations"].update({"nfs.io/quota-status": "applied",
              "nfs.io/enforced-limit-bytes": "1" if failure == "limit" else
              ("100000000" if fs == "btrfs" else "99999744")})
        if failure == "bound":
          raise subprocess.CalledProcessError(1, command)
        return json.dumps({"items": pvs})
      if failure == "cleanup" and "delete" in command and "pvc" in command:
        raise subprocess.CalledProcessError(1, command)
      return ""

    def popen(command, **kwargs):
      nonlocal eligible
      self.assertEqual(len(polls), 3)
      self.assertEqual(len(claim_polls), 2)
      self.assertTrue(all(obj.get("status", {}).get("phase") == "Bound"
                          for obj in objects if obj["kind"] == "PersistentVolume"))
      self.assertIn("annotate", command)
      self.assertIn("-l", command)
      eligible = True
      if failure == "annotate":
        process.poll.return_value = 1
      elif failure == "timeout":
        process.poll.return_value = None
      return process

    def start_waiter(**kwargs):
      prebound.wait_command(*kwargs["args"])
      return Mock(start=Mock())

    output = io.StringIO()
    clock = iter(range(0, 10000, 10))
    with tempfile.TemporaryDirectory() as tmp, \
         patch.dict("os.environ", {"E2E_LATENCY_PVS": "5"}), \
         patch.object(prebound, "run", side_effect=fake_run), \
         patch.object(prebound.subprocess, "Popen", side_effect=popen), \
         patch.object(prebound.threading, "Thread", side_effect=start_waiter), \
         patch.object(prebound, "BurstDiagnosis", return_value=diagnosis), \
         patch.object(prebound.time, "sleep"), \
         patch.object(prebound.time, "monotonic", side_effect=lambda: next(clock)), \
         patch.object(prebound.latency, "verify_samples") as verify, \
         contextlib.redirect_stdout(output):
      if failure:
        with self.assertRaises((RuntimeError, subprocess.CalledProcessError)):
          prebound.measure(fs, "/export", "gateway", "scripts/e2e/manifests", "")
      else:
        summary = Path(tmp, "summary.md")
        prebound.measure(fs, "/export", "gateway", "scripts/e2e/manifests", str(summary))
        self.assertIn("| watch | 5 |", summary.read_text())
        verify.assert_called_once()
    patches = [json.loads(c[-1])[0]["value"] for c in calls if "patch" in c]
    self.assertEqual(patches[-1], args)
    deletes = [c for c in calls if "delete" in c or "rmdir" in c]
    self.assertEqual([c[3] for c in deletes[:2]], ["pvc", "pv"])
    self.assertEqual(len(deletes[2:]), 5)
    if failure == "timeout":
      process.kill.assert_called_once()
    return output.getvalue()

  def test_bound_barrier_and_json_fields(self):
    for fs in ("xfs", "ext4", "btrfs"):
      output = self.exercise(fs=fs)
      result = json.loads(output.split("BEGIN E2E PREBOUND BURST JSON\n")[1].split("END E2E PREBOUND BURST JSON")[0])
      self.assertEqual(result["path"], "watch")
      self.assertEqual(result["N"], 5)
      for key in ("total_seconds", "applies_per_second", "mean_consecutive_apply_gap_seconds",
                  "p50_ms", "p90_ms", "p99_ms", "max_ms", "eligibility_command_seconds", "pvs"):
        self.assertIn(key, result)
      self.assertEqual(result["effective_qps_burst_logged"], [{"qps": 50, "burst": 100}])

  def test_partial_failures_restore_args_and_clean_every_resource(self):
    for failure in ("create", "bound", "annotate", "limit", "cleanup", "timeout"):
      with self.subTest(failure=failure):
        self.exercise(failure, fs="btrfs")

  def test_rate_and_single_poll_resolution(self):
    rows = {str(i): {"latency_ms": i * 500} for i in range(5)}
    result = prebound.result_for("xfs", list(rows), rows, 3, .1, {})
    self.assertEqual(result["applies_per_second"], 2)
    self.assertEqual(result["mean_consecutive_apply_gap_seconds"], .5)
    rows = {str(i): {"latency_ms": 1000} for i in range(5)}
    self.assertIsNone(prebound.result_for("xfs", list(rows), rows, 2, .1, {})["applies_per_second"])


if __name__ == "__main__":
  unittest.main()
