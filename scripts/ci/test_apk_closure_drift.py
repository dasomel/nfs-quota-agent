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
        self.assertIn("### APK Closure Drift Report", res.stdout)
        self.assertIn("| Package | Change | Baseline Version | Current Version |", res.stdout)
        self.assertIn("| *(all packages)* | unchanged: 3 | - | - |", res.stdout)
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


EXTRACT_SCRIPT = Path(__file__).resolve().parent / "extract-oci-manifest.py"


class ExtractOCIManifestTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.tmp_path = Path(self.tmp.name)

    def _create_mock_oci_archive(self, oci_tar_path: Path, manifest_content: str) -> None:
        import hashlib
        import io
        import tarfile

        # Create layer tarball
        layer_buf = io.BytesIO()
        with tarfile.open(fileobj=layer_buf, mode="w:gz") as ltar:
            data = manifest_content.encode("utf-8")
            ti = tarfile.TarInfo("licenses/os-packages-manifest.txt")
            ti.size = len(data)
            ltar.addfile(ti, io.BytesIO(data))
        layer_bytes = layer_buf.getvalue()
        layer_hash = hashlib.sha256(layer_bytes).hexdigest()

        # Create config
        config_bytes = json.dumps({"architecture": "amd64", "os": "linux"}).encode("utf-8")
        config_hash = hashlib.sha256(config_bytes).hexdigest()

        # Create amd64 manifest
        manifest_obj = {
            "schemaVersion": 2,
            "mediaType": "application/vnd.oci.image.manifest.v1+json",
            "config": {
                "mediaType": "application/vnd.oci.image.config.v1+json",
                "digest": f"sha256:{config_hash}",
                "size": len(config_bytes),
            },
            "layers": [
                {
                    "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
                    "digest": f"sha256:{layer_hash}",
                    "size": len(layer_bytes),
                }
            ],
        }
        manifest_bytes = json.dumps(manifest_obj).encode("utf-8")
        manifest_hash = hashlib.sha256(manifest_bytes).hexdigest()

        # Create multi-arch index
        index_list_obj = {
            "schemaVersion": 2,
            "mediaType": "application/vnd.oci.image.index.v1+json",
            "manifests": [
                {
                    "mediaType": "application/vnd.oci.image.manifest.v1+json",
                    "digest": f"sha256:{manifest_hash}",
                    "size": len(manifest_bytes),
                    "platform": {"architecture": "amd64", "os": "linux"},
                }
            ],
        }
        index_list_bytes = json.dumps(index_list_obj).encode("utf-8")
        index_list_hash = hashlib.sha256(index_list_bytes).hexdigest()

        # Root index.json
        root_index = {
            "schemaVersion": 2,
            "mediaType": "application/vnd.oci.image.index.v1+json",
            "manifests": [
                {
                    "mediaType": "application/vnd.oci.image.index.v1+json",
                    "digest": f"sha256:{index_list_hash}",
                    "size": len(index_list_bytes),
                }
            ],
        }
        root_index_bytes = json.dumps(root_index).encode("utf-8")

        with tarfile.open(oci_tar_path, mode="w") as otar:
            # add index.json
            ti = tarfile.TarInfo("index.json")
            ti.size = len(root_index_bytes)
            otar.addfile(ti, io.BytesIO(root_index_bytes))

            # add blobs
            for h, b in [
                (index_list_hash, index_list_bytes),
                (manifest_hash, manifest_bytes),
                (config_hash, config_bytes),
                (layer_hash, layer_bytes),
            ]:
                ti = tarfile.TarInfo(f"blobs/sha256/{h}")
                ti.size = len(b)
                otar.addfile(ti, io.BytesIO(b))

    def test_extract_manifest_success(self) -> None:
        oci_path = self.tmp_path / "image.tar"
        out_path = self.tmp_path / "extracted.txt"
        content = "test-pkg-1.0.0-r0\n"
        self._create_mock_oci_archive(oci_path, content)

        cmd = [sys.executable, str(EXTRACT_SCRIPT), str(oci_path), str(out_path)]
        res = subprocess.run(cmd, capture_output=True, text=True)
        self.assertEqual(res.returncode, 0)
        self.assertEqual(out_path.read_text(encoding="utf-8"), content)

    def test_extract_manifest_missing_archive_fails(self) -> None:
        oci_path = self.tmp_path / "non_existent.tar"
        out_path = self.tmp_path / "extracted.txt"

        cmd = [sys.executable, str(EXTRACT_SCRIPT), str(oci_path), str(out_path)]
        res = subprocess.run(cmd, capture_output=True, text=True)
        self.assertNotEqual(res.returncode, 0)
        self.assertIn("ERROR:", res.stderr)

    def test_extract_manifest_wrong_arch_fails(self) -> None:
        oci_path = self.tmp_path / "image.tar"
        out_path = self.tmp_path / "extracted.txt"
        self._create_mock_oci_archive(oci_path, "foo-1.0\n")

        cmd = [
            sys.executable,
            str(EXTRACT_SCRIPT),
            str(oci_path),
            str(out_path),
            "--arch",
            "riscv64",
        ]
        res = subprocess.run(cmd, capture_output=True, text=True)
        self.assertNotEqual(res.returncode, 0)
        self.assertIn("ERROR:", res.stderr)


if __name__ == "__main__":
    unittest.main()
