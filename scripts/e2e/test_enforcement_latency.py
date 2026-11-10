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
           patch.object(latency.time, "monotonic", side_effect=[0, 181] if exit_code is None else [0, 1]):
        with self.assertRaises(RuntimeError):
          latency.measure("btrfs", "/export", "gateway", "scripts/e2e/manifests", "")
      deletes = [command for command in calls if "delete" in command]
      self.assertEqual([command[3] for command in deletes[:2]], ["pvc", "pv"])
      self.assertEqual(len(deletes[2:]), 5)
      self.assertTrue(all("subvolume" in command for command in deletes[2:]))
      if exit_code is None:
        process.kill.assert_called_once()
        process.wait.assert_called_once()


class ParseObjectsTests(unittest.TestCase):
  def test_accepts_concatenated_documents_and_list(self):
    pv, pvc = {"kind": "PersistentVolume"}, {"kind": "PersistentVolumeClaim"}
    stream = json.dumps(pv, indent=4) + "\n" + json.dumps(pvc, indent=4) + "\n"
    self.assertEqual(latency.parse_objects(stream), [pv, pvc])
    as_list = json.dumps({"kind": "List", "items": [pv, pvc]})
    self.assertEqual(latency.parse_objects(as_list), [pv, pvc])


if __name__ == "__main__":
  unittest.main()
