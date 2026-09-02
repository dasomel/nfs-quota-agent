#!/usr/bin/env python3
"""Tests for hack/verify-release.py's --require-signatures flag (#26).

Stdlib-only (unittest), no pytest -- runs the script as a subprocess against
a minimal fake release-dir fixture (release-manifest.json + the handful of
artifact files it references) with a controlled PATH, so it exercises the
real CLI surface rather than importing internals.
"""
import hashlib
import json
import os
import stat
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parent / "verify-release.py"


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


class VerifyReleaseRequireSignaturesTest(unittest.TestCase):
    def setUp(self):
        self.tmpdir = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmpdir.cleanup)
        self.release_dir = Path(self.tmpdir.name) / "release"
        self.release_dir.mkdir()
        self._build_fixture()

    def _write(self, name, content: bytes) -> Path:
        path = self.release_dir / name
        path.write_bytes(content)
        return path

    def _build_fixture(self):
        """Smallest release-manifest.json (schemaVersion 3, with a
        signatures block) plus the binary/chart/signature-bundle files it
        points at, so every non-signature check passes and the only thing
        left to observe is how the script handles the signature checks."""
        binary_content = b"fake-binary-content\n"
        chart_content = b"fake-chart-content\n"
        checksums_bundle_content = b'{"fake":"checksums-bundle"}\n'
        chart_bundle_content = b'{"fake":"chart-bundle"}\n'

        self._write("nfs-quota-agent-linux-amd64", binary_content)
        self._write("nfs-quota-agent-0.1.0.tgz", chart_content)
        self._write("checksums.txt.bundle", checksums_bundle_content)
        self._write("nfs-quota-agent-0.1.0.tgz.bundle", chart_bundle_content)

        manifest = {
            "schemaVersion": 3,
            "tag": "v0.1.0-test",
            "sourceCommit": "deadbeefcafefeed",
            "workflowRun": "123456789",
            "image": {
                "repository": "ghcr.io/dasomel/nfs-quota-agent",
                "digest": "sha256:" + "a" * 64,
            },
            "chart": {
                "file": "nfs-quota-agent-0.1.0.tgz",
                "sha256": sha256_bytes(chart_content),
            },
            "binaries": [
                {
                    "file": "nfs-quota-agent-linux-amd64",
                    "sha256": sha256_bytes(binary_content),
                }
            ],
            "signatures": {
                "checksums": {
                    "file": "checksums.txt.bundle",
                    "sha256": sha256_bytes(checksums_bundle_content),
                },
                "chart": {
                    "file": "nfs-quota-agent-0.1.0.tgz.bundle",
                    "sha256": sha256_bytes(chart_bundle_content),
                },
            },
        }
        manifest_path = self.release_dir / "release-manifest.json"
        manifest_path.write_text(json.dumps(manifest, indent=2))

    def _empty_path_dir(self) -> str:
        """A PATH entry containing no `cosign` binary."""
        d = Path(self.tmpdir.name) / "empty-path"
        d.mkdir(exist_ok=True)
        return str(d)

    def _fake_cosign_dir(self) -> str:
        """A PATH entry with a `cosign` stub so shutil.which finds it. The
        stub must never actually run in these tests -- the trusted-root
        check short-circuits before any subprocess call reaches it."""
        d = Path(self.tmpdir.name) / "fake-bin"
        d.mkdir(exist_ok=True)
        stub = d / "cosign"
        stub.write_text("#!/bin/sh\necho 'fake cosign should not have run' >&2\nexit 1\n")
        mode = stub.stat().st_mode
        stub.chmod(mode | stat.S_IEXEC | stat.S_IXGRP | stat.S_IXOTH)
        return str(d)

    def _run(self, extra_args, path_dirs):
        env = dict(os.environ)
        env["PATH"] = os.pathsep.join(path_dirs)
        return subprocess.run(
            [sys.executable, str(SCRIPT), str(self.release_dir), *extra_args],
            capture_output=True,
            text=True,
            env=env,
        )

    def test_default_without_require_signatures_skips_and_exits_zero(self):
        result = self._run([], [self._empty_path_dir()])
        self.assertEqual(result.returncode, 0, msg=result.stdout + result.stderr)
        self.assertIn(
            "SKIP: checksums.txt signature (cosign not found on PATH)", result.stdout
        )
        self.assertNotIn("FAIL:", result.stdout)

    def test_require_signatures_without_cosign_fails(self):
        result = self._run(["--require-signatures"], [self._empty_path_dir()])
        self.assertNotEqual(result.returncode, 0, msg=result.stdout + result.stderr)
        combined = result.stdout + result.stderr
        self.assertIn("FAIL:", combined)
        self.assertIn("cosign", combined.lower())

    def test_require_signatures_with_missing_trusted_root_fails(self):
        missing_root = str(Path(self.tmpdir.name) / "no-such-trusted-root.json")
        result = self._run(
            ["--require-signatures", "--trusted-root", missing_root],
            [self._fake_cosign_dir()],
        )
        self.assertNotEqual(result.returncode, 0, msg=result.stdout + result.stderr)
        combined = result.stdout + result.stderr
        self.assertIn("FAIL:", combined)
        self.assertIn("trusted root", combined.lower())


if __name__ == "__main__":
    unittest.main()
