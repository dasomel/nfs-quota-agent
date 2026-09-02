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
import tarfile
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


class VerifyReleaseBundleTest(unittest.TestCase):
    """Tests for --bundle (#5): verifying an offline install bundle built by
    `make release-bundle` against a release-manifest.json's optional
    "bundle" field, plus the image-digest and chart-sha256 cross-checks
    inside it. Builds a minimal but structurally real bundle (an OCI
    archive with an index.json, a chart .tgz) via tarfile directly, rather
    than shelling out to skopeo/docker, so these tests run without either
    installed."""

    def setUp(self):
        self.tmpdir = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmpdir.cleanup)
        self.work = Path(self.tmpdir.name)
        self.image_digest = "sha256:" + "b" * 64
        self.chart_content = b"fake-chart-content\n"
        self.bundle_path = self.work / "bundle.tar.gz"
        self._build_bundle(self.bundle_path)

    def _build_bundle(self, bundle_path, chart_content=None, image_digest=None):
        chart_content = self.chart_content if chart_content is None else chart_content
        image_digest = self.image_digest if image_digest is None else image_digest

        stage = self.work / "stage"
        if stage.exists():
            import shutil as _shutil
            _shutil.rmtree(stage)
        (stage / "images").mkdir(parents=True)
        (stage / "chart").mkdir(parents=True)
        (stage / "hack").mkdir(parents=True)

        # Minimal OCI archive: just enough for oci_archive_image_digest()
        # to find a manifests[0].digest.
        image_tar = stage / "images" / "nfs-quota-agent-image.tar"
        index = {
            "schemaVersion": 2,
            "manifests": [{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": image_digest, "size": 1}],
        }
        index_bytes = json.dumps(index).encode()
        with tarfile.open(image_tar, "w") as tf:
            info = tarfile.TarInfo("index.json")
            info.size = len(index_bytes)
            import io
            tf.addfile(info, io.BytesIO(index_bytes))

        (stage / "chart" / "nfs-quota-agent-0.1.0.tgz").write_bytes(chart_content)
        (stage / "BUNDLE-README.md").write_text("stub\n")

        with tarfile.open(bundle_path, "w:gz") as tf:
            for root, _, files in os.walk(stage):
                for name in files:
                    full = Path(root) / name
                    arcname = full.relative_to(stage)
                    tf.add(full, arcname=str(arcname))

        return sha256_bytes(bundle_path.read_bytes()), sha256_bytes(chart_content)

    def _write_manifest(self, bundle_sha, chart_sha, image_digest=None):
        image_digest = self.image_digest if image_digest is None else image_digest
        manifest = {
            "schemaVersion": 3,
            "tag": "v0.1.0-test",
            "sourceCommit": "deadbeef",
            "workflowRun": "123",
            "image": {"repository": "ghcr.io/dasomel/nfs-quota-agent", "digest": image_digest},
            "chart": {"file": "nfs-quota-agent-0.1.0.tgz", "sha256": chart_sha},
            "binaries": [{"file": "nfs-quota-agent-linux-amd64", "sha256": "0" * 64}],
            "bundle": {"file": self.bundle_path.name, "sha256": bundle_sha},
        }
        manifest_path = self.work / "release-manifest.json"
        manifest_path.write_text(json.dumps(manifest))
        return manifest_path

    def _empty_path_dir(self):
        """A PATH entry containing no `cosign` binary -- keeps these tests
        independent of whether cosign happens to be installed on the host
        running them."""
        d = self.work / "empty-path"
        d.mkdir(exist_ok=True)
        return str(d)

    def _failing_cosign_dir(self):
        """A PATH entry with a `cosign` stub that always exits nonzero,
        simulating an attacker who replaced the bundle/manifest and their
        sidecar checksums but does not hold the real signing key -- the
        checksums can be made to match a tampered artifact, but
        `cosign verify-blob` cannot."""
        d = self.work / "failing-cosign-bin"
        d.mkdir(exist_ok=True)
        stub = d / "cosign"
        stub.write_text("#!/bin/sh\necho 'stub cosign: signature verification failed' >&2\nexit 1\n")
        stub.chmod(stub.stat().st_mode | stat.S_IEXEC | stat.S_IXGRP | stat.S_IXOTH)
        return str(d)

    def _run(self, args, path_dirs=None):
        env = dict(os.environ)
        if path_dirs is not None:
            env["PATH"] = os.pathsep.join(path_dirs)
        else:
            # Default to no cosign on PATH so these tests don't depend on
            # whether the host running them happens to have it installed.
            env["PATH"] = self._empty_path_dir()
        return subprocess.run(
            [sys.executable, str(SCRIPT), *args],
            capture_output=True,
            text=True,
            env=env,
        )

    def test_bundle_matches_manifest_passes(self):
        bundle_sha = sha256_bytes(self.bundle_path.read_bytes())
        chart_sha = sha256_bytes(self.chart_content)
        manifest_path = self._write_manifest(bundle_sha, chart_sha)
        result = self._run(["--bundle", str(self.bundle_path), "--manifest", str(manifest_path)])
        self.assertEqual(result.returncode, 0, msg=result.stdout + result.stderr)
        self.assertIn("OK: bundle archive", result.stdout)
        self.assertIn("OK: bundle image digest", result.stdout)
        self.assertIn("OK: bundle chart", result.stdout)

    def test_tampered_chart_inside_bundle_fails(self):
        bundle_sha = sha256_bytes(self.bundle_path.read_bytes())
        chart_sha = sha256_bytes(self.chart_content)
        manifest_path = self._write_manifest(bundle_sha, chart_sha)

        # Tamper: rebuild a copy of the bundle with different chart bytes,
        # keeping the manifest pointed at the original (good) sha256s.
        tampered = self.work / "tampered.tar.gz"
        self._build_bundle(tampered, chart_content=b"tampered-chart-bytes\n")

        result = self._run(["--bundle", str(tampered), "--manifest", str(manifest_path)])
        self.assertNotEqual(result.returncode, 0, msg=result.stdout + result.stderr)
        combined = result.stdout + result.stderr
        self.assertIn("MISMATCH: bundle archive", combined)
        self.assertIn("MISMATCH: bundle chart", combined)

    def test_mismatched_image_digest_fails(self):
        bundle_sha = sha256_bytes(self.bundle_path.read_bytes())
        chart_sha = sha256_bytes(self.chart_content)
        manifest_path = self._write_manifest(bundle_sha, chart_sha, image_digest="sha256:" + "c" * 64)
        result = self._run(["--bundle", str(self.bundle_path), "--manifest", str(manifest_path)])
        self.assertNotEqual(result.returncode, 0, msg=result.stdout + result.stderr)
        self.assertIn("MISMATCH: bundle image digest", result.stdout + result.stderr)

    def test_missing_bundle_file_fails(self):
        result = self._run(["--bundle", str(self.work / "does-not-exist.tar.gz")])
        self.assertNotEqual(result.returncode, 0, msg=result.stdout + result.stderr)

    def test_bundle_sidecar_checksum_file_used_when_no_manifest_field(self):
        """A schemaVersion-3 release-manifest.json published before the
        release-bundle job appends has no 'bundle' field (see verify_bundle's
        docstring) -- a sidecar <bundle>.sha256 file is the real-world
        source of truth in that case."""
        bundle_sha = sha256_bytes(self.bundle_path.read_bytes())
        chart_sha = sha256_bytes(self.chart_content)
        manifest = {
            "schemaVersion": 3,
            "tag": "v0.1.0-test",
            "sourceCommit": "deadbeef",
            "workflowRun": "123",
            "image": {"repository": "ghcr.io/dasomel/nfs-quota-agent", "digest": self.image_digest},
            "chart": {"file": "nfs-quota-agent-0.1.0.tgz", "sha256": chart_sha},
            "binaries": [{"file": "nfs-quota-agent-linux-amd64", "sha256": "0" * 64}],
        }
        manifest_path = self.work / "release-manifest.json"
        manifest_path.write_text(json.dumps(manifest))
        sidecar = Path(str(self.bundle_path) + ".sha256")
        sidecar.write_text(f"{bundle_sha}  {self.bundle_path.name}\n")

        result = self._run(["--bundle", str(self.bundle_path), "--manifest", str(manifest_path)])
        self.assertEqual(result.returncode, 0, msg=result.stdout + result.stderr)
        self.assertIn("OK: bundle archive", result.stdout)
        self.assertIn(".sha256", result.stdout)

        # Tamper the sidecar -- must now fail.
        sidecar.write_text(f"{'0' * 64}  {self.bundle_path.name}\n")
        result = self._run(["--bundle", str(self.bundle_path), "--manifest", str(manifest_path)])
        self.assertNotEqual(result.returncode, 0, msg=result.stdout + result.stderr)
        self.assertIn("MISMATCH: bundle archive", result.stdout + result.stderr)

    def test_bundle_without_manifest_reports_notes_not_failures(self):
        """No --manifest and no release-manifest.json alongside the bundle:
        every cross-check degenerates to a SKIP/NOTE rather than a failure,
        since there is nothing to compare against yet the bundle itself is
        still structurally readable."""
        result = self._run(["--bundle", str(self.bundle_path)])
        self.assertEqual(result.returncode, 0, msg=result.stdout + result.stderr)
        self.assertIn("SKIP: bundle archive sha256", result.stdout)
        self.assertIn("NOTE: bundle image digest", result.stdout)
        self.assertIn("NOTE: bundle chart", result.stdout)

    def test_require_signatures_without_any_signature_files_fails(self):
        """CRITICAL-1 (independent review of #117): before this fix, bundle
        mode never checked a cosign signature at all and ignored
        --require-signatures entirely -- a bundle with no signature files
        would print OK. Now: no release-manifest.json.bundle and no
        <bundle>.bundle present, --require-signatures must FAIL and name
        both missing signatures."""
        bundle_sha = sha256_bytes(self.bundle_path.read_bytes())
        chart_sha = sha256_bytes(self.chart_content)
        manifest_path = self._write_manifest(bundle_sha, chart_sha)
        result = self._run(
            ["--bundle", str(self.bundle_path), "--manifest", str(manifest_path), "--require-signatures"],
            [self._failing_cosign_dir()],  # cosign present but stubbed -- exercises the "present but fails" path too
        )
        self.assertNotEqual(result.returncode, 0, msg=result.stdout + result.stderr)
        combined = result.stdout + result.stderr
        self.assertIn("FAIL: release-manifest.json signature", combined)
        self.assertIn("FAIL: bundle signature", combined)

    def test_require_signatures_without_cosign_on_path_fails(self):
        bundle_sha = sha256_bytes(self.bundle_path.read_bytes())
        chart_sha = sha256_bytes(self.chart_content)
        manifest_path = self._write_manifest(bundle_sha, chart_sha)
        result = self._run(
            ["--bundle", str(self.bundle_path), "--manifest", str(manifest_path), "--require-signatures"],
            [self._empty_path_dir()],
        )
        self.assertNotEqual(result.returncode, 0, msg=result.stdout + result.stderr)
        combined = result.stdout + result.stderr
        self.assertIn("FAIL:", combined)
        self.assertIn("cosign", combined.lower())

    def test_tampered_bundle_with_matching_sidecar_but_bad_signature_fails(self):
        """The core CRITICAL-1 scenario: an attacker who can replace release
        assets regenerates a matching <bundle>.sha256 sidecar for their
        tampered bundle (so the sha256 check alone would say OK) but cannot
        produce a cosign signature that verifies, since they don't hold the
        signing key. A stubbed `cosign` that always fails stands in for
        "the real Sigstore verification would reject this.\""""
        bundle_sha = sha256_bytes(self.bundle_path.read_bytes())
        chart_sha = sha256_bytes(self.chart_content)
        manifest_path = self._write_manifest(bundle_sha, chart_sha)
        # Sidecar checksum "attacker-regenerated" to match the (unmodified,
        # for simplicity) bundle -- the point is that a matching checksum
        # must not be sufficient on its own once a signature is present to
        # check.
        sidecar = Path(str(self.bundle_path) + ".sha256")
        sidecar.write_text(f"{bundle_sha}  {self.bundle_path.name}\n")
        bogus_bundle_sig = Path(str(self.bundle_path) + ".bundle")
        bogus_bundle_sig.write_text('{"attacker":"forged, not a real cosign bundle"}\n')

        result = self._run(
            ["--bundle", str(self.bundle_path), "--manifest", str(manifest_path), "--require-signatures"],
            [self._failing_cosign_dir()],
        )
        self.assertNotEqual(result.returncode, 0, msg=result.stdout + result.stderr)
        combined = result.stdout + result.stderr
        self.assertIn("MISMATCH: bundle signature", combined)

    def test_symlink_escape_member_is_rejected(self):
        """CRITICAL-2 (independent review of #117): a tar member that is a
        symlink pointing outside the extraction directory, followed by a
        member that writes through it, could previously escape a purely
        pre-extraction realpath check (the symlink target doesn't exist
        yet when the later member's path is resolved). Every
        symlink/hardlink/device member must now be rejected outright."""
        evil_bundle = self.work / "evil.tar.gz"
        with tarfile.open(evil_bundle, "w:gz") as tf:
            outside_marker = self.work / "outside-marker.txt"
            outside_marker.write_text("should never be written by extraction\n")

            symlink_info = tarfile.TarInfo("images")
            symlink_info.type = tarfile.SYMTYPE
            symlink_info.linkname = str(self.work)  # escapes the extraction tmpdir
            tf.addfile(symlink_info)

            escape_content = b"pwned\n"
            escape_info = tarfile.TarInfo("images/pwned.txt")
            escape_info.size = len(escape_content)
            import io
            tf.addfile(escape_info, io.BytesIO(escape_content))

        result = self._run(["--bundle", str(evil_bundle)])
        self.assertNotEqual(result.returncode, 0, msg=result.stdout + result.stderr)
        combined = result.stdout + result.stderr
        self.assertIn("FAIL: bundle contents", combined)
        self.assertIn("symlink", combined.lower())
        # The escape must not actually have happened.
        self.assertFalse((self.work / "pwned.txt").exists())


if __name__ == "__main__":
    unittest.main()
