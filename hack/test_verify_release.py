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


class VerifyReleaseSignatureEntriesTest(unittest.TestCase):
    """A schemaVersion-3 manifest whose "signatures" object is missing,
    empty, or only partially populated must not let --require-signatures
    slide by with only the entries that happen to be present -- release.yaml
    always signs both checksums.txt and the chart (see
    EXPECTED_SIGNATURE_ENTRIES in verify-release.py), so a manifest lacking
    either is not a legitimate "nothing to check" case."""

    def setUp(self):
        self.tmpdir = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmpdir.cleanup)
        self.release_dir = Path(self.tmpdir.name) / "release"
        self.release_dir.mkdir()

    def _write(self, name, content: bytes) -> Path:
        path = self.release_dir / name
        path.write_bytes(content)
        return path

    def _build_fixture(self, signatures):
        """A minimal schemaVersion-3 manifest whose non-signature checks all
        pass, with `signatures` set to whatever the caller wants to probe
        (a dict, an empty dict, or omitted entirely via None)."""
        binary_content = b"fake-binary-content\n"
        chart_content = b"fake-chart-content\n"
        self._write("nfs-quota-agent-linux-amd64", binary_content)
        self._write("nfs-quota-agent-0.1.0.tgz", chart_content)

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
        }
        if signatures is not None:
            manifest["signatures"] = signatures
        (self.release_dir / "release-manifest.json").write_text(json.dumps(manifest, indent=2))

    def _empty_path_dir(self) -> str:
        d = Path(self.tmpdir.name) / "empty-path"
        d.mkdir(exist_ok=True)
        return str(d)

    def _run(self, extra_args):
        env = dict(os.environ)
        env["PATH"] = self._empty_path_dir()
        return subprocess.run(
            [sys.executable, str(SCRIPT), str(self.release_dir), *extra_args],
            capture_output=True,
            text=True,
            env=env,
        )

    def test_strict_empty_signatures_object_fails_naming_missing_entries(self):
        self._build_fixture({})
        result = self._run(["--require-signatures"])
        self.assertNotEqual(result.returncode, 0, msg=result.stdout + result.stderr)
        self.assertIn(
            "FAIL: checksums signature entry missing from release-manifest.json",
            result.stdout,
        )
        self.assertIn(
            "FAIL: chart signature entry missing from release-manifest.json",
            result.stdout,
        )

    def test_strict_partial_signatures_fails_naming_missing_entry(self):
        checksums_bundle_content = b'{"fake":"checksums-bundle"}\n'
        self._write("checksums.txt.bundle", checksums_bundle_content)
        self._build_fixture(
            {
                "checksums": {
                    "file": "checksums.txt.bundle",
                    "sha256": sha256_bytes(checksums_bundle_content),
                }
                # "chart" entry deliberately omitted.
            }
        )
        result = self._run(["--require-signatures"])
        self.assertNotEqual(result.returncode, 0, msg=result.stdout + result.stderr)
        self.assertNotIn(
            "FAIL: checksums signature entry missing from release-manifest.json",
            result.stdout,
        )
        self.assertIn(
            "FAIL: chart signature entry missing from release-manifest.json",
            result.stdout,
        )

    def test_strict_missing_signatures_key_fails(self):
        self._build_fixture(None)
        result = self._run(["--require-signatures"])
        self.assertNotEqual(result.returncode, 0, msg=result.stdout + result.stderr)
        combined = result.stdout + result.stderr
        self.assertIn("FAIL:", combined)

    def test_default_mode_with_partial_manifests_is_unchanged(self):
        """Documents current (pre- and post-fix, since the new check is
        gated entirely on --require-signatures) default-mode behavior: an
        empty or absent "signatures" object on a schemaVersion-3 manifest
        produces no signature-related output at all and exits 0 -- exactly
        as it did before this change, for either shape."""
        for signatures in ({}, None):
            with self.subTest(signatures=signatures):
                self._build_fixture(signatures)
                result = self._run([])
                self.assertEqual(result.returncode, 0, msg=result.stdout + result.stderr)
                self.assertNotIn("FAIL:", result.stdout)
                self.assertNotIn("SKIP:", result.stdout)
                self.assertNotIn("signature", result.stdout)


if __name__ == "__main__":
    unittest.main()
