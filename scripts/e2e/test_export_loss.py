"""Offline checks for D4 outage ordering, quota proof and restoration."""

import contextlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import unittest
from unittest.mock import Mock, patch

SPEC = importlib.util.spec_from_file_location(
  "export_loss", Path(__file__).with_name("measure-export-loss.py"))
export_loss = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(export_loss)


class ExportLossTests(unittest.TestCase):
  def exercise(self, fs="xfs", cluster="kind", server="kernel", failure=None):
    calls, pvs = [], []
    listening = True
    starts = 0
    ended = False
    expected = "100000000" if fs == "btrfs" else "99999744"

    def pv(name):
      return {"metadata": {"name": name, "annotations": {
        "nfs.io/quota-status": "applied", "nfs.io/enforced-limit-bytes": expected}},
        "status": {"phase": "Bound"}}

    def fake_run(command, **kwargs):
      nonlocal listening, starts, ended
      calls.append(command)
      if "stop" in command:
        self.assertTrue(listening)
        listening = False
        if failure == "stop":
          raise subprocess.CalledProcessError(1, command)
      if "start" in command:
        starts += 1
        if failure == "first-start" and starts == 1:
          raise subprocess.CalledProcessError(1, command)
        listening = ended = True
      if "daemonset/nfs-quota-agent" in command:
        return '{"metadata":{"uid":"ds"}}'
      if "ss" in command and failure == "listener":
        return "listener"
      if "--dry-run=client" in command:
        return json.dumps({"items": [
          {"kind": "PersistentVolume", "metadata": {}, "spec": {"nfs": {}}},
          {"kind": "PersistentVolumeClaim", "metadata": {}, "spec": {}}]})
      if "create" in command and command[-1] == "-":
        self.assertFalse(listening)
        if failure == "create":
          raise subprocess.CalledProcessError(1, command)
        if failure == "sigterm":
          export_loss.terminate(15, None)
        items = json.loads(kwargs["input"])["items"]
        pvs.extend(i["metadata"]["name"] for i in items if i["kind"] == "PersistentVolume")
        self.assertEqual(len(items), 6)
        self.assertEqual(len(set(pvs)), 3)
        self.assertEqual([i["spec"]["volumeName"] for i in items
                          if i["kind"] == "PersistentVolumeClaim"], pvs)
      if "get" in command and "pv" in command:
        if "-l" not in command:
          return json.dumps(pv("pv-e2e"))
        items = [] if failure == "pending" or failure == "lost-after" and ended else [pv(n) for n in pvs]
        return json.dumps({"items": items})
      if command[-1] == "/etc/projects":
        return "\n".join(f"{i}:/export/{name}" for i, name in enumerate(["pvc-e2e", *pvs], 1))
      if command[-1] == "/etc/projid":
        return "\n".join(f"pv_{name.replace('-', '_')}:{i}"
                         for i, name in enumerate(["pv-e2e", *pvs], 1))
      if command[-1] == "/proc/self/mountinfo":
        self.assertTrue(listening and ended)
        return "1 0 0:1 / /mnt/nfs rw - nfs4 gateway:/export/pvc-e2e rw"
      if "sh" in command:
        return {"enospc": "No space left on device\nrc=1",
                "early-enospc": "No space left on device\nrc=1",
                "write-succeeded": "rc=0"}.get(failure, "Disk quota exceeded\nrc=1")
      return ""

    def native(_fs, _path, names, _expected, _sudo, indices=None):
      if failure == "native" and not listening:
        raise RuntimeError("wrong native limit")
      used = 99_500_000 if failure == "enospc" else 0
      marker = "0/" if fs == "btrfs" else "#"
      samples = {name: (f"{marker}{i}", used) for i, name in enumerate(names, 1)}
      if (failure == "mapping" and not listening or failure == "qgroup-changed" and ended
          and len(names) > 1):
        samples[names[-1]] = (f"{marker}999", used)
      return samples

    pod = {"metadata": {"name": "agent", "uid": "uid"}, "status": {
      "conditions": [{"type": "Ready", "status": "True"}],
      "containerStatuses": [{"name": "nfs-quota-agent", "restartCount": 0}]}}
    changed = {**pod, "metadata": {"name": "new-agent", "uid": "new-uid"}}
    agent_pods = [[pod], [changed if failure == "agent-restarted" else pod]]
    clock = iter(range(0, 5000, 15))
    output = io.StringIO()
    errors = (RuntimeError, subprocess.CalledProcessError, SystemExit)
    with patch.object(export_loss, "run", side_effect=fake_run), \
         patch.object(export_loss, "nfs_port_open", side_effect=lambda _gateway: listening), \
         patch.object(export_loss.recovery, "agent_pods", side_effect=agent_pods), \
         patch.object(export_loss.recovery, "cleanup") as cleanup, \
         patch.object(export_loss.recovery.latency, "verify_samples", side_effect=native), \
         patch.object(export_loss.time, "monotonic", side_effect=lambda: next(clock)), \
         patch.object(export_loss.time, "sleep"), contextlib.redirect_stdout(output):
      args = (fs, "/export", "gateway", "scripts/e2e/manifests", cluster, server)
      if failure and failure != "enospc":
        with self.assertRaises(errors):
          export_loss.measure(*args)
        self.assertNotIn("BEGIN E2E EXPORT LOSS JSON", output.getvalue())
      else:
        export_loss.measure(*args)
      self.assertTrue(listening)
      cleanup.assert_called_once()
      self.assertEqual(len(cleanup.call_args.args[2]), 3)
    service = "nfs-ganesha.service" if server == "ganesha" else "nfs-server.service"
    self.assertTrue(any(cmd[-2:] == ["stop", service] for cmd in calls))
    self.assertEqual(starts, 2 if failure == "first-start" else 1)
    if not failure or failure == "enospc":
      self.assertTrue(any("delete" in cmd and "test-writer" in cmd for cmd in calls))
      return json.loads(output.getvalue().split("BEGIN E2E EXPORT LOSS JSON\n")[1].split(
        "END E2E EXPORT LOSS JSON")[0])

  def test_all_ci_legs(self):
    for fs, cluster, server in (("xfs", "kind", "kernel"), ("ext4", "kind", "kernel"),
                                ("btrfs", "kind", "kernel"), ("xfs", "k3s", "kernel"),
                                ("xfs", "kind", "ganesha")):
      with self.subTest(fs=fs, cluster=cluster, server=server):
        result = self.exercise(fs, cluster, server)
        self.assertEqual((result["fs"], result["cluster"], result["nfs_server"]),
                         (fs, cluster, server))
        self.assertEqual(result["pvs_created_during_outage"], 3)
        self.assertEqual(result["new_pvs_applied_during_outage"], 3)
        self.assertEqual(result["applied_before_outage"], 1)
        self.assertEqual(result["applied_after_recovery"], 4)
        self.assertGreater(result["outage_seconds"], 0)
        self.assertLess(result["recovery_to_all_applied_seconds"], 0)
        self.assertGreater(result["recovery_to_all_applied_recheck_seconds"], 0)
        self.assertEqual(result["mapping_consistency"]["paths_checked"], 4)
        self.assertEqual(result["post_recovery_nfs_write"]["over_quota_result"], "EDQUOT")

  def test_restoration_and_failed_assertions(self):
    for failure in ("stop", "listener", "create", "sigterm", "pending", "native", "mapping",
                    "first-start", "lost-after", "write-succeeded", "early-enospc", "agent-restarted"):
      with self.subTest(failure=failure):
        self.exercise(failure=failure)
    self.exercise(fs="btrfs", failure="qgroup-changed")

  def test_enospc_requires_usage_at_quota_limit(self):
    result = self.exercise(failure="enospc")
    self.assertEqual(result["post_recovery_nfs_write"]["over_quota_result"],
                     "ENOSPC at quota hard limit")
    self.exercise(failure="early-enospc")

  def test_native_usage_is_reported_in_bytes(self):
    for fs, selector, row, used in (
        ("xfs", "#123", "#123 97280 0 97656", 97280 * 1024),
        ("ext4", "#123", "#123 -- 97280 0 97656", 97280 * 1024),
        ("btrfs", "0/123", "0/123 99614720 0 100000000", 99614720)):
      raw_id = "Subvolume ID: 123" if fs == "btrfs" else "123 --- /export/pvc-e2e"
      expected = 100000000 if fs == "btrfs" else 99999744
      with self.subTest(fs=fs), \
           patch.object(export_loss.recovery.latency, "run", side_effect=[row, raw_id]), \
           contextlib.redirect_stdout(io.StringIO()):
        samples = export_loss.recovery.latency.verify_samples(fs, "/export", ["pvc-e2e"], expected, [])
        self.assertEqual(samples, {"pvc-e2e": (selector, used)})

  def test_kernel_service_alias_fallback(self):
    first = subprocess.CalledProcessError(3, ["systemctl", "is-active", "nfs-server.service"])
    with patch.object(export_loss, "run", Mock(side_effect=[first, ""])):
      self.assertEqual(export_loss.active_service("kernel", []), "nfs-kernel-server.service")


if __name__ == "__main__":
  unittest.main()
