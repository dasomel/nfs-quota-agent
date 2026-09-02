#!/usr/bin/env python3
"""Tests for hack/update-chart-digest.py (#5).

Runs the script as a subprocess against fixture values.yaml files and a
fake PATH populated with stub crane/skopeo/docker executables, so these
tests exercise the real CLI contract (argv, exit code, stdout/stderr, and
the file actually written) rather than importing internals.
"""
import stat
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

HACK_DIR = Path(__file__).resolve().parent
SCRIPT = HACK_DIR / "update-chart-digest.py"

VALID_DIGEST = "sha256:" + "a" * 64
OTHER_VALID_DIGEST = "sha256:" + "b" * 64

FIXTURE_VALUES = """\
image:
  repository: ghcr.io/dasomel/nfs-quota-agent
  pullPolicy: IfNotPresent
  tag: ""  # Defaults to chart appVersion
  # a comment that must survive untouched
  digest: ""

imagePullSecrets: []
"""


def run(args, env=None):
    return subprocess.run(
        [sys.executable, str(SCRIPT)] + args,
        capture_output=True,
        text=True,
        env=env,
    )


def write_stub(bin_dir, name, script_body):
    """Writes an executable shell-script stub named `name` into bin_dir
    that runs `script_body` -- used to fake crane/skopeo/docker without
    touching a real registry."""
    path = Path(bin_dir) / name
    path.write_text(f"#!/bin/sh\n{script_body}\n")
    path.chmod(path.stat().st_mode | stat.S_IEXEC | stat.S_IXGRP | stat.S_IXOTH)
    return path


class UpdateChartDigestDirectTest(unittest.TestCase):
    """--digest path: no external tool involved at all."""

    def setUp(self):
        self.tmpdir = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmpdir.cleanup)
        self.values_path = Path(self.tmpdir.name) / "values.yaml"
        self.values_path.write_text(FIXTURE_VALUES)

    def test_writes_digest_and_preserves_rest_of_file(self):
        result = run(["--digest", VALID_DIGEST, str(self.values_path)])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("image.digest set to", result.stdout)

        new_text = self.values_path.read_text()
        self.assertIn(f'digest: "{VALID_DIGEST}"', new_text)
        self.assertIn("# a comment that must survive untouched", new_text)
        self.assertIn('tag: ""  # Defaults to chart appVersion', new_text)
        self.assertIn("imagePullSecrets: []", new_text)

    def test_idempotent_second_run_is_a_no_op(self):
        first = run(["--digest", VALID_DIGEST, str(self.values_path)])
        self.assertEqual(first.returncode, 0, first.stderr)
        text_after_first = self.values_path.read_text()

        second = run(["--digest", VALID_DIGEST, str(self.values_path)])
        self.assertEqual(second.returncode, 0, second.stderr)
        self.assertIn("no change", second.stdout)
        self.assertEqual(self.values_path.read_text(), text_after_first)

    def test_rewriting_with_a_different_digest_replaces_the_old_one(self):
        run(["--digest", VALID_DIGEST, str(self.values_path)])
        result = run(["--digest", OTHER_VALID_DIGEST, str(self.values_path)])
        self.assertEqual(result.returncode, 0, result.stderr)

        new_text = self.values_path.read_text()
        self.assertIn(f'digest: "{OTHER_VALID_DIGEST}"', new_text)
        self.assertNotIn(VALID_DIGEST, new_text)

    def test_invalid_digest_format_is_rejected_without_writing(self):
        original = self.values_path.read_text()
        result = run(["--digest", "sha256:not-hex", str(self.values_path)])
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("not a valid sha256 digest", result.stderr)
        self.assertEqual(self.values_path.read_text(), original)

    def test_missing_digest_key_in_values_file_errors_clearly(self):
        no_digest_key = Path(self.tmpdir.name) / "no-digest-key.yaml"
        no_digest_key.write_text("image:\n  repository: foo\n  tag: \"\"\n")
        result = run(["--digest", VALID_DIGEST, str(no_digest_key)])
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("no 'image.digest' key", result.stderr)

    def test_missing_values_file_errors_clearly(self):
        result = run(["--digest", VALID_DIGEST, str(Path(self.tmpdir.name) / "missing.yaml")])
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("not found", result.stderr)

    def test_digest_and_image_are_mutually_exclusive(self):
        result = run(["--digest", VALID_DIGEST, "--image", "example/foo:v1", str(self.values_path)])
        self.assertNotEqual(result.returncode, 0)

    def test_missing_source_argument_errors(self):
        result = run([str(self.values_path)])
        self.assertNotEqual(result.returncode, 0)


class UpdateChartDigestResolveTest(unittest.TestCase):
    """--image path: resolution via stub crane/skopeo/docker on a
    controlled PATH, so no real registry or network is touched."""

    def setUp(self):
        self.tmpdir = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmpdir.cleanup)
        self.values_path = Path(self.tmpdir.name) / "values.yaml"
        self.values_path.write_text(FIXTURE_VALUES)
        self.bin_dir = Path(self.tmpdir.name) / "bin"
        self.bin_dir.mkdir()

    def _env_with_stub_path(self):
        import os

        env = dict(os.environ)
        env["PATH"] = str(self.bin_dir)
        return env

    def test_resolves_via_crane_when_present(self):
        write_stub(self.bin_dir, "crane", f'echo "{VALID_DIGEST}"')
        result = run(["--image", "example/foo:v1", str(self.values_path)], env=self._env_with_stub_path())
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("resolved via crane", result.stdout)
        self.assertIn(f'digest: "{VALID_DIGEST}"', self.values_path.read_text())

    def test_falls_back_to_skopeo_when_crane_missing(self):
        write_stub(self.bin_dir, "skopeo", f'echo "{VALID_DIGEST}"')
        result = run(["--image", "example/foo:v1", str(self.values_path)], env=self._env_with_stub_path())
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("resolved via skopeo", result.stdout)

    def test_falls_back_to_docker_when_crane_and_skopeo_missing(self):
        write_stub(
            self.bin_dir,
            "docker",
            f'echo "Name:      example/foo"\necho "Digest:    {VALID_DIGEST}"',
        )
        result = run(["--image", "example/foo:v1", str(self.values_path)], env=self._env_with_stub_path())
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("resolved via docker", result.stdout)

    def test_prefers_crane_over_skopeo_and_docker(self):
        write_stub(self.bin_dir, "crane", f'echo "{VALID_DIGEST}"')
        write_stub(self.bin_dir, "skopeo", f'echo "{OTHER_VALID_DIGEST}"')
        result = run(["--image", "example/foo:v1", str(self.values_path)], env=self._env_with_stub_path())
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("resolved via crane", result.stdout)
        self.assertIn(f'digest: "{VALID_DIGEST}"', self.values_path.read_text())

    def test_no_tool_available_gives_a_clear_error(self):
        result = run(["--image", "example/foo:v1", str(self.values_path)], env=self._env_with_stub_path())
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("could not resolve a digest", result.stderr)
        self.assertIn("not found on PATH", result.stderr)

    def test_tool_failure_falls_through_to_the_next_one(self):
        write_stub(self.bin_dir, "crane", 'echo "no such image" >&2\nexit 1')
        write_stub(self.bin_dir, "skopeo", f'echo "{VALID_DIGEST}"')
        result = run(["--image", "example/foo:v1", str(self.values_path)], env=self._env_with_stub_path())
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("resolved via skopeo", result.stdout)


if __name__ == "__main__":
    unittest.main()
