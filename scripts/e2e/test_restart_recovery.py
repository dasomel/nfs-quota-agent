"""Offline D3 decisions, correctness, result contract and failure cleanup."""

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
  "recovery", Path(__file__).with_name("measure-restart-recovery.py"))
recovery = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(recovery)


class RecoveryTests(unittest.TestCase):
  def test_kill_decision(self):
    self.assertEqual(recovery.kill_decision(9, 60), (False, False))
    self.assertEqual(recovery.kill_decision(10, 60), (True, True))
    self.assertEqual(recovery.kill_decision(60, 60), (True, False))
    self.assertEqual(recovery.kill_decision(10, 10), (True, False))

  def test_mapping_consistency(self):
    paths = ["/export/a", "/export/b"]
    self.assertEqual(recovery.check_mappings("1:/export/a\n2:/export/b", "a:1\nb:2", paths),
                     {"consistent": True, "kind": "projects/projid", "paths_checked": 2})
    for projects, projid in (
        ("1:/export/a\n1:/export/b", "a:1"),  # shared ID
        ("1:/export/a\n01:/export/b", "a:1"),  # same numeric ID
        ("1:/export/a\n2:/export/b", "a:1\nb:01\nc:2"),
        ("1:/export/a\n2:/export/b", "a:1\nb:1\nc:2"),  # duplicate ID
        ("1:/export/a", "a:1\nb:2"),  # missing path
        ("1:/export/a\n2:/export/b", "a:1"),  # missing projid
        ("1:/export/a\n1:/export/a\n2:/export/b", "a:1\nb:2"),
        ("1:/export/a\n3:/export/a\n2:/export/b", "a:1\nb:2\nc:3")):
      with self.subTest(projects=projects, projid=projid), self.assertRaises(RuntimeError):
        recovery.check_mappings(projects, projid, paths)

  def test_samples_are_five_distinct_including_frontier_and_endpoints(self):
    names = [str(i) for i in range(60)]
    for point in (0, 1, 30, 58, 59):
      indices = recovery.sample_indices(names, [names[point]])
      self.assertEqual(len(set(indices)), 5)
      self.assertTrue({0, 59, point}.issubset(indices))

  def exercise(self, fs="xfs", early=False, failure=None):
    calls, objects, polls = [], [], []
    killed = False
    process = Mock()
    process.poll.return_value = None if failure == "running-create" else 0
    pod = {"metadata": {"name": "old", "uid": "old-uid", "ownerReferences": [{"uid": "ds"}]},
           "status": {"conditions": [{"type": "Ready", "status": "True"}],
                      "containerStatuses": [{"name": "nfs-quota-agent", "restartCount": 0}]}}
    templates = {"items": [
      {"kind": "PersistentVolume", "metadata": {"annotations": {
        "pv.kubernetes.io/provisioned-by": "nfs.csi.k8s.io"}}, "spec": {"nfs": {}}},
      {"kind": "PersistentVolumeClaim", "metadata": {"namespace": "default"}, "spec": {}}]}

    def fake_run(command, **kwargs):
      nonlocal killed
      calls.append(command)
      if "daemonset/nfs-quota-agent" in command:
        return json.dumps({"metadata": {"uid": "ds"}})
      if "get" in command and "pods" in command:
        item = json.loads(json.dumps(pod))
        if killed:
          item["metadata"].update(name="new", uid="new-uid")
        if failure == "not-ready" and killed:
          item["status"]["conditions"][0]["status"] = "False"
        if failure == "same-uid":
          item["metadata"]["uid"] = "old-uid"
        return json.dumps({"items": [item]})
      if "--dry-run=client" in command:
        return json.dumps(templates)
      if "get" in command and "pvc" in command:
        return json.dumps({"items": [{"metadata": obj["metadata"], "status": {
          "phase": "Pending" if failure == "claim" else "Bound"}}
          for obj in objects if obj["kind"] == "PersistentVolumeClaim"]})
      if "get" in command and "pv" in command:
        if failure == "poll" and killed:
          raise subprocess.CalledProcessError(1, command)
        polls.append(command)
        n = 12 if early or killed and failure != "timeout" else 10
        items = []
        for source in objects:
          if source["kind"] != "PersistentVolume":
            continue
          pv = json.loads(json.dumps(source))
          pv["status"] = {"phase": "Bound"}
          if len(items) < n:
            pv["metadata"]["annotations"].update({"nfs.io/quota-status": "applied",
              "nfs.io/enforced-limit-bytes": "1" if failure == "limit" else
              ("100000000" if fs == "btrfs" else "99999744")})
          items.append(pv)
        return json.dumps({"items": items})
      if "delete" in command and "pod" in command:
        self.assertIn("--grace-period=0", command)
        self.assertIn("--force", command)
        self.assertIn("--wait=false", command)
        killed = True
      if command[-1] == "/etc/projects":
        return "\n".join(f"{i + 1}:{obj['spec']['nfs']['path']}" for i, obj in enumerate(
          o for o in objects if o["kind"] == "PersistentVolume"))
      if command[-1] == "/etc/projid":
        return "" if failure == "mapping" else "\n".join(f"name{i}:{i + 1}" for i in range(12))
      if failure == "cleanup" and "delete" in command and "pvc" in command:
        raise subprocess.CalledProcessError(1, command)
      return ""

    def popen(command, **kwargs):
      objects.extend(json.loads(Path(command[-1]).read_text())["items"])
      self.assertEqual(len(objects), 24)
      self.assertTrue(all(obj["metadata"].get("annotations", {}).get(
        "pv.kubernetes.io/provisioned-by") == "nfs.csi.k8s.io"
        for obj in objects if obj["kind"] == "PersistentVolume"))
      if failure == "create":
        raise OSError("create failed")
      return process

    output = io.StringIO()
    # Advance enough to exercise real timeout branches without waiting offline.
    clock = iter(range(0, 20000, 20))
    with tempfile.TemporaryDirectory() as tmp, \
         patch.dict("os.environ", {"E2E_RECOVERY_PVS": "12"}), \
         patch.object(recovery, "run", side_effect=fake_run), \
         patch.object(recovery.subprocess, "Popen", side_effect=popen), \
         patch.object(recovery.time, "sleep"), \
         patch.object(recovery.time, "monotonic", side_effect=lambda: next(clock)), \
         patch.object(recovery.latency, "verify_samples") as verify, \
         contextlib.redirect_stdout(output):
      if failure == "native":
        verify.side_effect = RuntimeError("wrong native hard limit")
      summary = Path(tmp, "summary")
      if failure:
        with self.assertRaises((RuntimeError, OSError, subprocess.CalledProcessError)):
          recovery.measure(fs, "/export", "gateway", "scripts/e2e/manifests", str(summary))
      else:
        recovery.measure(fs, "/export", "gateway", "scripts/e2e/manifests", str(summary))
        self.assertIn(f"| {fs} | 12 |", summary.read_text())
        self.assertEqual(verify.call_args_list[0].args[3], 100000000 if fs == "btrfs" else 99999744)
        self.assertEqual(len(verify.call_args_list[0].kwargs["indices"]), 5)
        if fs == "btrfs":
          self.assertEqual(list(verify.call_args_list[1].kwargs["indices"]), list(range(12)))
          self.assertFalse(any("/etc/projects" in c for c in calls))
    deletes = [c for c in calls if "delete" in c or "rmdir" in c]
    resources = [c for c in deletes if "pvc" in c or "pv" in c]
    self.assertEqual(len(resources), 2)
    self.assertIn("pvc", resources[0])
    self.assertIn("pv", resources[1])
    directories = [c for c in deletes if "rmdir" in c or "subvolume" in c]
    self.assertEqual(len(directories), 12)
    if failure == "running-create":
      process.kill.assert_called_once()
      process.wait.assert_called_once()
    if killed:
      self.assertIn("pods", calls[-1])  # final health check, even after cleanup failure
    if failure:
      self.assertNotIn("BEGIN E2E RESTART RECOVERY JSON", output.getvalue())
    return output.getvalue()

  def test_json_contract_and_all_applied_early(self):
    for fs in ("xfs", "ext4", "btrfs"):
      for early in (False, True):
        with self.subTest(fs=fs, early=early):
          output = self.exercise(fs, early)
          result = json.loads(output.split("BEGIN E2E RESTART RECOVERY JSON\n")[1].split(
            "END E2E RESTART RECOVERY JSON")[0])
          self.assertEqual(result["fs"], fs)
          self.assertEqual(result["N"], 12)
          self.assertEqual(result["applied_before_kill"], 12 if early else 10)
          self.assertEqual(result["killed_mid_burst"], not early)
          self.assertEqual(result["old_pod_uid"], "old-uid")
          self.assertEqual(result["new_pod_uid"], "new-uid")
          self.assertTrue(result["new_pod_ready"])
          self.assertEqual(result["mapping_consistency"]["paths_checked"], 12)
          for key in ("kill_to_ready_seconds", "kill_to_all_applied_seconds", "total_seconds"):
            self.assertIsInstance(result[key], (int, float))
          self.assertEqual(result["old_agent_container"], {"restart_count": 0, "termination_reason": None})
          self.assertIn("new_agent_container", result)

  def test_cleanup_on_failures(self):
    for failure in ("create", "poll", "limit", "native", "mapping", "cleanup", "not-ready", "same-uid", "timeout", "claim", "running-create"):
      with self.subTest(failure=failure):
        self.exercise(failure=failure)
    self.exercise(fs="btrfs", failure="poll")

  def test_btrfs_all_qgroups_reject_duplicate_missing_or_wrong_limit(self):
    names = [f"pv-{i}" for i in range(12)]
    for failure in ("duplicate", "missing", "wrong"):
      def fake_run(command):
        if command[:2] == ["btrfs", "subvolume"]:
          return f"Subvolume ID: {256 + int(command[-1].rsplit('-', 1)[1])}"
        rows = [f"0/{256 + i} 0 0 100000000" for i in range(12)]
        # Index 7 is outside the five samples at frontier 9; must still fail.
        if failure == "duplicate":
          rows.append(rows[7])
        elif failure == "missing":
          rows.pop(7)
        else:
          rows[7] = "0/263 0 0 1"
        return "\n".join(rows)
      with self.subTest(failure=failure), patch.object(recovery.latency, "run", side_effect=fake_run), \
           contextlib.redirect_stdout(io.StringIO()), self.assertRaises(RuntimeError):
        recovery.latency.verify_samples("btrfs", "/export", names, 100000000, [], indices=range(12))

  def test_termination_reason_and_readiness(self):
    self.assertEqual(recovery.container_status({"status": {"containerStatuses": [
      {"name": "nfs-quota-agent", "restartCount": 1,
       "lastState": {"terminated": {"reason": "OOMKilled"}}}]}}),
      {"restart_count": 1, "termination_reason": "OOMKilled"})
    self.assertFalse(recovery.ready({"metadata": {"deletionTimestamp": "now"}, "status": {
      "conditions": [{"type": "Ready", "status": "True"}]}}))


if __name__ == "__main__":
  unittest.main()
