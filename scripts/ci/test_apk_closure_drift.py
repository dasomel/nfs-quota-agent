#!/usr/bin/env python3
"""Tests for scripts/ci/apk-closure-drift.py.

Covers:
- added packages
- removed packages
- version-changed packages
- identical manifests
- missing baseline (absent file)
- --fail-on-change behavior (change vs identical vs missing baseline)
- flag forms (--baseline, --current) and positional arguments
- output formatting (--format markdown, --format json, --output-markdown, --output-json)
"""

from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parent / "apk-closure-drift.py"


class APKClosureDriftCLITest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.tmp_path = Path(self.tmp.name)

    def _run_script(self, args: list[str]) -> subprocess.CompletedProcess[str]:
        cmd = [sys.executable, str(SCRIPT), *args]
        return subprocess.run(
            cmd, cwd=self.tmp_path, capture_output=True, text=True
        )

    def test_identical_manifests(self) -> None:
        manifest_content = (
            "alpine-baselayout-3.7.2-r1\n"
            "btrfs-progs-6.17.1-r1\n"
            "quota-tools-4.11-r0\n"
        )
        baseline = self.tmp_path / "baseline.txt"
        current = self.tmp_path / "current.txt"
        baseline.write_text(manifest_content, encoding="utf-8")
        current.write_text(manifest_content, encoding="utf-8")

        res = self._run_script([str(baseline), str(current)])
        self.assertEqual(res.returncode, 0)
        self.assertIn("No package drift detected against baseline", res.stdout)
        self.assertIn('"status": "identical"', res.stdout)
        self.assertIn('"identical": 3', res.stdout)
        self.assertIn('"added": 0', res.stdout)
        self.assertIn('"removed": 0', res.stdout)
        self.assertIn('"changed": 0', res.stdout)

    def test_added_package(self) -> None:
        baseline = self.tmp_path / "baseline.txt"
        current = self.tmp_path / "current.txt"
        baseline.write_text("pkg-a-1.0.0-r0\n", encoding="utf-8")
        current.write_text("pkg-a-1.0.0-r0\npkg-b-2.0.0-r0\n", encoding="utf-8")

        res = self._run_script([str(baseline), str(current)])
        self.assertEqual(res.returncode, 0)
        self.assertIn("| `pkg-b` | added | - | `2.0.0-r0` |", res.stdout)
        self.assertIn('"status": "drifted"', res.stdout)
        self.assertIn('"added": 1', res.stdout)

    def test_removed_package(self) -> None:
        baseline = self.tmp_path / "baseline.txt"
        current = self.tmp_path / "current.txt"
        baseline.write_text("pkg-a-1.0.0-r0\npkg-b-2.0.0-r0\n", encoding="utf-8")
        current.write_text("pkg-a-1.0.0-r0\n", encoding="utf-8")

        res = self._run_script([str(baseline), str(current)])
        self.assertEqual(res.returncode, 0)
        self.assertIn("| `pkg-b` | removed | `2.0.0-r0` | - |", res.stdout)
        self.assertIn('"status": "drifted"', res.stdout)
        self.assertIn('"removed": 1', res.stdout)

    def test_version_changed_package(self) -> None:
        baseline = self.tmp_path / "baseline.txt"
        current = self.tmp_path / "current.txt"
        baseline.write_text("pcre2-10.47-r1\n", encoding="utf-8")
        current.write_text("pcre2-10.48-r0\n", encoding="utf-8")

        res = self._run_script([str(baseline), str(current)])
        self.assertEqual(res.returncode, 0)
        self.assertIn("| `pcre2` | changed | `10.47-r1` | `10.48-r0` |", res.stdout)
        self.assertIn('"status": "drifted"', res.stdout)
        self.assertIn('"changed": 1', res.stdout)

    def test_missing_baseline_exits_zero_with_no_baseline(self) -> None:
        current = self.tmp_path / "current.txt"
        current.write_text("pkg-a-1.0.0-r0\n", encoding="utf-8")
        non_existent = self.tmp_path / "non_existent_baseline.txt"

        res = self._run_script([str(non_existent), str(current)])
        self.assertEqual(res.returncode, 0)
        self.assertEqual(res.stdout.strip(), "no baseline")

    def test_missing_baseline_with_fail_on_change_still_exits_zero(self) -> None:
        current = self.tmp_path / "current.txt"
        current.write_text("pkg-a-1.0.0-r0\n", encoding="utf-8")
        non_existent = self.tmp_path / "absent_baseline.txt"

        res = self._run_script([str(non_existent), str(current), "--fail-on-change"])
        self.assertEqual(res.returncode, 0)
        self.assertEqual(res.stdout.strip(), "no baseline")

    def test_fail_on_change_flag_behavior(self) -> None:
        baseline = self.tmp_path / "baseline.txt"
        current_changed = self.tmp_path / "current_changed.txt"
        current_same = self.tmp_path / "current_same.txt"

        baseline.write_text("pkg-1.0.0-r0\n", encoding="utf-8")
        current_changed.write_text("pkg-1.0.1-r0\n", encoding="utf-8")
        current_same.write_text("pkg-1.0.0-r0\n", encoding="utf-8")

        # Without --fail-on-change: exits 0 even when drifted
        res_drift = self._run_script([str(baseline), str(current_changed)])
        self.assertEqual(res_drift.returncode, 0)

        # With --fail-on-change: exits 1 when drifted
        res_fail = self._run_script([str(baseline), str(current_changed), "--fail-on-change"])
        self.assertEqual(res_fail.returncode, 1)

        # With --fail-on-change: exits 0 when identical
        res_ok = self._run_script([str(baseline), str(current_same), "--fail-on-change"])
        self.assertEqual(res_ok.returncode, 0)

    def test_flag_arguments_and_outputs(self) -> None:
        baseline = self.tmp_path / "baseline.txt"
        current = self.tmp_path / "current.txt"
        md_out = self.tmp_path / "report.md"
        json_out = self.tmp_path / "report.json"

        baseline.write_text("foo-1.0-r0\n", encoding="utf-8")
        current.write_text("foo-1.1-r0\nbar-2.0-r0\n", encoding="utf-8")

        res = self._run_script([
            "--baseline", str(baseline),
            "--current", str(current),
            "--markdown-out", str(md_out),
            "--json-out", str(json_out),
            "--format", "json",
        ])
        self.assertEqual(res.returncode, 0)

        # --format json should print raw JSON to stdout
        stdout_json = json.loads(res.stdout)
        self.assertEqual(stdout_json["status"], "drifted")
        self.assertEqual(stdout_json["summary"]["added"], 1)
        self.assertEqual(stdout_json["summary"]["changed"], 1)

        # md_out should contain markdown table
        md_content = md_out.read_text(encoding="utf-8")
        self.assertIn("### APK Closure Drift Report", md_content)
        self.assertIn("| `foo` | changed | `1.0-r0` | `1.1-r0` |", md_content)
        self.assertIn("| `bar` | added | - | `2.0-r0` |", md_content)

        # json_out should match stdout_json
        file_json = json.loads(json_out.read_text(encoding="utf-8"))
        self.assertEqual(file_json, stdout_json)


if __name__ == "__main__":
    unittest.main()
