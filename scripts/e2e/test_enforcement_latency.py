"""Offline checks only; never contact a cluster or run host quota commands."""

import contextlib
import importlib.util
import io
import json
from pathlib import Path
import unittest
from unittest.mock import Mock, patch

SPEC = importlib.util.spec_from_file_location(
  "latency", Path(__file__).with_name("measure-enforcement-latency.py"))
latency = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(latency)


class NativeQuotaTests(unittest.TestCase):
  def check_backend(self, fs, bad_index=None, missing_index=None):
    names = [f"pv-{i}" for i in range(100)]
    rows = []
    for index in range(100):
      if index == missing_index:
        continue
      hard = 1 if index == bad_index else (100000000 if fs == "btrfs" else 97656)
      selector = f"0/{index + 256}" if fs == "btrfs" else f"#{index + 256}"
      flags = " --" if fs == "ext4" else ""
      rows.append(f"{selector}{flags} 0 0 {hard} 0 0")
    inspected = []

    def fake_run(command):
      if command[0] == "lsattr" or command[:2] == ["btrfs", "subvolume"]:
        index = int(command[-1].rsplit("-", 1)[1])
        inspected.append(index)
        if fs == "btrfs":
          return f"Name: pv-{index}\nSubvolume ID: {index + 256}"
        return f"{index + 256} ---------- {command[-1]}"
      return "\n".join(rows)

    with patch.object(latency, "run", side_effect=fake_run), contextlib.redirect_stdout(io.StringIO()):
      latency.verify_samples(fs, "/export", names, 100000000 if fs == "btrfs" else 99999744, [])
    self.assertEqual(inspected, [0, 25, 50, 74, 99])

  def test_first_last_and_five_distinct_native_samples(self):
    for fs in ("xfs", "ext4", "btrfs"):
      with self.subTest(fs=fs):
        self.check_backend(fs)

  def test_wrong_hard_limit_fails(self):
    for fs in ("xfs", "ext4", "btrfs"):
      with self.subTest(fs=fs), self.assertRaisesRegex(RuntimeError, "Wrong native hard limit"):
        self.check_backend(fs, bad_index=99)

  def test_missing_first_row_fails(self):
    for fs in ("xfs", "ext4", "btrfs"):
      with self.subTest(fs=fs), self.assertRaisesRegex(RuntimeError, "Missing/ambiguous"):
        self.check_backend(fs, missing_index=0)

  def test_nearest_rank_has_no_timing_threshold(self):
    values = list(range(100, 0, -1))
    self.assertEqual([latency.percentile(values, p) for p in (50, 90, 99)], [50, 90, 99])
    self.assertEqual(latency.percentile([999999999] * 5, 99), 999999999)

  def test_too_few_pvs_rejected_before_any_command(self):
    with patch.dict("os.environ", {"E2E_LATENCY_PVS": "4"}), patch.object(latency, "run") as command:
      with self.assertRaisesRegex(ValueError, ">= 5"):
        latency.measure("xfs", "/export", "gateway", "unused", "")
      command.assert_not_called()


class CleanupTests(unittest.TestCase):
  def test_partial_create_and_timeout_clean_claims_pvs_and_all_directories(self):
    templates = {"items": [
      {"kind": "PersistentVolume", "metadata": {}, "spec": {"nfs": {}}},
      {"kind": "PersistentVolumeClaim", "metadata": {}, "spec": {}},
    ]}
    for exit_code in (1, None):
      calls = []

      def fake_run(command, **kwargs):
        calls.append(command)
        if "--dry-run=client" in command:
          return json.dumps(templates)
        return ""

      process = Mock()
      process.poll.return_value = exit_code
      with self.subTest(exit_code=exit_code), patch.dict("os.environ", {"E2E_LATENCY_PVS": "5"}), \
           patch.object(latency, "run", side_effect=fake_run), \
           patch.object(latency.subprocess, "Popen", return_value=process), \
           patch.object(latency.time, "monotonic", side_effect=[0, 181] if exit_code is None else [0, 1]), \
           contextlib.redirect_stdout(io.StringIO()):
        with self.assertRaises(RuntimeError):
          latency.measure("btrfs", "/export", "gateway", "scripts/e2e/manifests", "")
      deletes = [command for command in calls if "delete" in command]
      self.assertEqual([command[3] for command in deletes[:2]], ["pvc", "pv"])
      self.assertEqual(len(deletes[2:]), 5)
      self.assertTrue(all("subvolume" in command for command in deletes[2:]))
      if exit_code is None:
        process.kill.assert_called_once()
        process.wait.assert_called_once()


class PollingTests(unittest.TestCase):
  def test_existing_list_records_bound_before_applied_without_extra_poll_requests(self):
    calls, polls = [], []
    templates = {"items": [
      {"kind": "PersistentVolume", "metadata": {}, "spec": {"nfs": {}}},
      {"kind": "PersistentVolumeClaim", "metadata": {}, "spec": {}},
    ]}

    def fake_run(command, **kwargs):
      calls.append(command)
      if "--dry-run=client" in command:
        return json.dumps(templates)
      if "get" in command and "pv" in command:
        polls.append(command)
        applied = len(polls) == 2
        return json.dumps({"items": [{
          "metadata": {"name": f"latency-aaaaaaaaaaaa-{i:04d}",
                       "creationTimestamp": "1970-01-01T00:00:00Z",
                       "annotations": {"nfs.io/quota-status": "applied" if applied else "pending",
                                       "nfs.io/enforced-limit-bytes": "99999744"}},
          "status": {"phase": "Bound"}} for i in range(1, 6)]})
      if "get" in command and "pods" in command:
        return json.dumps({"items": [{"status": {"containerStatuses": [
          {"image": "test", "imageID": "test-id"}]}}]})
      return "1" if command == ["nproc"] else ""

    read_text = Path.read_text
    process, diagnosis = Mock(), Mock()
    process.poll.return_value = 0
    output = io.StringIO()
    with patch.dict("os.environ", {"E2E_LATENCY_PVS": "5"}), \
         patch.object(latency, "run", side_effect=fake_run), \
         patch.object(latency.uuid, "uuid4", return_value=Mock(hex="a" * 32)), \
         patch.object(latency.subprocess, "Popen", return_value=process), \
         patch.object(latency, "BurstDiagnosis", return_value=diagnosis), \
         patch.object(Path, "read_text", autospec=True, side_effect=lambda path, **kw:
                      "model name: test CPU" if str(path) == "/proc/cpuinfo" else read_text(path, **kw)), \
         patch.object(latency, "verify_samples") as verify, \
         patch.object(latency.time, "time", side_effect=[1, 2, 2]), \
         patch.object(latency.time, "sleep") as sleep, \
         contextlib.redirect_stdout(output):
      latency.measure("xfs", "/export", "gateway", "scripts/e2e/manifests", "")
    self.assertEqual(len(polls), 2)
    sleep.assert_called_once_with(.5)
    verify.assert_called_once()
    end, observations, bounds = diagnosis.finish.call_args.args
    self.assertEqual(end, 2)
    self.assertEqual(len(bounds), 5)
    for row in observations.values():
      self.assertEqual(row["first_bound_observed_unix_seconds"], 1)
      self.assertEqual(row["observed_unix_seconds"], 2)
      self.assertEqual(row["latency_ms"], 2000)
      self.assertEqual(row["apply_minus_bound_ms"], 1000)
    self.assertIn("BEGIN E2E ENFORCEMENT LATENCY JSON", output.getvalue())
    self.assertIn("END E2E ENFORCEMENT LATENCY JSON", output.getvalue())


class ParseObjectsTests(unittest.TestCase):
  def test_accepts_concatenated_documents_and_list(self):
    pv, pvc = {"kind": "PersistentVolume"}, {"kind": "PersistentVolumeClaim"}
    stream = json.dumps(pv, indent=4) + "\n" + json.dumps(pvc, indent=4) + "\n"
    self.assertEqual(latency.parse_objects(stream), [pv, pvc])
    as_list = json.dumps({"kind": "List", "items": [pv, pvc]})
    self.assertEqual(latency.parse_objects(as_list), [pv, pvc])


if __name__ == "__main__":
  unittest.main()
