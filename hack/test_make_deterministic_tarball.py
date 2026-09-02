#!/usr/bin/env python3
"""Tests for hack/make-deterministic-tarball.py (#5).

Stdlib-only (unittest), runs the script as a subprocess against a tiny
staging directory with a couple of files, and checks: (1) two independent
runs over identical inputs produce byte-identical output (the actual
determinism property `make release-bundle` relies on), (2) a changed mtime
on an input file does not change the output digest (metadata is
normalized away), (3) both .tar.gz and plain .tar output modes work.
"""
import hashlib
import os
import subprocess
import sys
import tarfile
import tempfile
import time
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parent / "make-deterministic-tarball.py"


def sha256_file(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


class MakeDeterministicTarballTest(unittest.TestCase):
    def setUp(self):
        self.tmpdir = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmpdir.cleanup)
        self.work = Path(self.tmpdir.name)
        self.stage = self.work / "stage"
        (self.stage / "sub").mkdir(parents=True)
        (self.stage / "a.txt").write_text("a\n")
        (self.stage / "sub" / "b.txt").write_text("b\n")

    def _run(self, staging_dir, output_path, extra_args=()):
        result = subprocess.run(
            [sys.executable, str(SCRIPT), str(staging_dir), str(output_path), *extra_args],
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 0, msg=result.stdout + result.stderr)
        return result

    def test_two_runs_over_identical_input_are_byte_identical(self):
        out1 = self.work / "out1.tar.gz"
        out2 = self.work / "out2.tar.gz"
        self._run(self.stage, out1, ["--mtime", "1700000000"])
        self._run(self.stage, out2, ["--mtime", "1700000000"])
        self.assertEqual(sha256_file(out1), sha256_file(out2))

    def test_changed_input_mtime_does_not_change_output_digest(self):
        out1 = self.work / "out1.tar.gz"
        self._run(self.stage, out1, ["--mtime", "1700000000"])
        digest1 = sha256_file(out1)

        # Touch a file to a very different mtime -- output must be unaffected.
        os.utime(self.stage / "a.txt", (1000000000, 1000000000))
        out2 = self.work / "out2.tar.gz"
        self._run(self.stage, out2, ["--mtime", "1700000000"])
        self.assertEqual(digest1, sha256_file(out2))

    def test_plain_tar_output_extension_is_uncompressed(self):
        out = self.work / "out.tar"
        self._run(self.stage, out, ["--mtime", "0"])
        # A gzip file starts with the magic bytes 1f 8b; a plain tar does not.
        with open(out, "rb") as f:
            magic = f.read(2)
        self.assertNotEqual(magic, b"\x1f\x8b")
        self.assertTrue(tarfile.is_tarfile(out))

    def test_gzip_output_contains_expected_members(self):
        out = self.work / "out.tar.gz"
        self._run(self.stage, out, ["--mtime", "0"])
        with tarfile.open(out, "r:gz") as tf:
            names = sorted(tf.getnames())
        self.assertEqual(names, ["a.txt", "sub/b.txt"])

    def test_nonexistent_staging_dir_fails(self):
        result = subprocess.run(
            [sys.executable, str(SCRIPT), str(self.work / "does-not-exist"), str(self.work / "out.tar.gz")],
            capture_output=True,
            text=True,
        )
        self.assertNotEqual(result.returncode, 0)


if __name__ == "__main__":
    unittest.main()
