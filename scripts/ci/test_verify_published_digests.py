#!/usr/bin/env python3
"""Exercise the published-digest release gate with isolated registry inspectors."""

import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("verify-published-digests.sh").resolve()
REPOSITORY = "ghcr.io/dasomel/nfs-quota-agent"
RAW = json.dumps({"manifests": [
    {"platform": {"os": "linux", "architecture": "amd64"}},
    {"platform": {"os": "linux", "architecture": "arm64"}},
    {"platform": {"os": "linux", "architecture": "arm", "variant": "v7"}},
]}).encode()
DIGEST = "sha256:" + hashlib.sha256(RAW).hexdigest()

FAKE = r'''
import hashlib
import os
from pathlib import Path
import sys

args = sys.argv[1:]
docker = Path(sys.argv[0]).name == "docker"
if docker and args == ["buildx", "version"]:
    sys.exit(0)
if docker:
    assert args[:4] == ["buildx", "imagetools", "inspect", "--raw"], args
else:
    assert len(args) == 2 and args[0] in ("digest", "manifest"), args
ref = args[-1]
assert ref.startswith(os.environ["REPOSITORY"] + ":"), ref
mode = "equal" if ref.endswith(":" + os.environ["TAG"]) else os.environ["MODE"]
raw = Path(os.environ["INDEX"]).read_bytes()
if mode == "missing":
    error = ("ERROR: " + ref + ": not found" if docker else
             "Error: GET https://ghcr.io/v2/image/manifests/tag: MANIFEST_UNKNOWN: manifest unknown")
elif mode in ("equal", "different"):
    if mode == "different":
        raw += b" "
    if docker or args[0] == "manifest":
        sys.stdout.buffer.write(raw)
    else:
        print("sha256:" + hashlib.sha256(raw).hexdigest())
    sys.exit(0)
elif mode in ("empty", "empty_failure"):
    sys.exit(0 if mode == "empty" else 1)
elif mode == "wrong_ref":
    error = "ERROR: other.example/image:tag: not found"
elif mode == "generic_missing":
    error = "Error: not found"
else:
    error = mode
print(error, file=sys.stderr)
sys.exit(1)
'''


class VerifyPublishedDigestsTest(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        # An allowlist hides the installed crane/docker and prevents registry access.
        hash_tool = "sha256sum" if shutil.which("sha256sum") else "shasum"
        for tool in ("bash", "jq", "mktemp", "rm", "cat", "grep", "awk", hash_tool):
            source = shutil.which(tool)
            self.assertIsNotNone(source, tool + " is required")
            (self.bin / tool).symlink_to(source)
        (self.root / "index.json").write_bytes(RAW)

    def run_gate(self, inspector, mode, stable=False):
        fake = self.bin / inspector
        fake.write_text("#!" + sys.executable + "\n" + FAKE)
        fake.chmod(0o755)
        tag = "v0.6.0" if stable else "v0.6.0-rc2"
        manifest = self.root / "release-manifest.json"
        manifest.write_text(json.dumps({
            "tag": tag, "image": {"repository": REPOSITORY, "digest": DIGEST},
        }))
        env = dict(os.environ, PATH=str(self.bin), TAG=tag, MODE=mode,
                   MANIFEST=str(manifest), REPOSITORY=REPOSITORY,
                   INDEX=str(self.root / "index.json"))
        return subprocess.run([str(self.bin / "bash"), str(SCRIPT)], env=env,
                              capture_output=True, text=True, timeout=15)

    def test_rc_missing_crane(self):
        self.check_missing("crane")

    def test_rc_missing_docker(self):
        self.check_missing("docker")

    def check_missing(self, inspector):
        result = self.run_gate(inspector, "missing")
        self.assertEqual(result.returncode, 0, result.stderr)
        for tag in ("latest", "0.6", "0"):
            self.assertIn(f"floating tag {REPOSITORY}:{tag} does not exist yet (not moved)",
                          result.stdout)
        self.assertIn("MANIFEST_UNKNOWN" if inspector == "crane" else ": not found",
                      result.stderr)

    def test_rc_different_digest(self):
        for inspector in ("crane", "docker"):
            with self.subTest(inspector=inspector):
                self.assertEqual(self.run_gate(inspector, "different").returncode, 0)
                (self.bin / inspector).unlink()

    def test_rc_moved_digest(self):
        for inspector in ("crane", "docker"):
            with self.subTest(inspector=inspector):
                result = self.run_gate(inspector, "equal")
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("moved floating tag", result.stderr)
                (self.bin / inspector).unlink()

    def test_rc_other_errors_fail_closed(self):
        for inspector in ("crane", "docker"):
            for mode in ("connection refused", "TOOMANYREQUESTS", "unauthorized",
                         "context deadline exceeded", "empty", "empty_failure",
                         "unknown error", "wrong_ref", "generic_missing"):
                with self.subTest(inspector=inspector, mode=mode):
                    result = self.run_gate(inspector, mode)
                    self.assertNotEqual(result.returncode, 0, result.stdout)
                    self.assertNotIn("does not exist yet", result.stdout)
                    if mode not in ("empty", "empty_failure", "wrong_ref", "generic_missing"):
                        self.assertIn(mode, result.stderr)
                    (self.bin / inspector).unlink()

    def test_stable_missing_fails(self):
        for inspector in ("crane", "docker"):
            with self.subTest(inspector=inspector):
                result = self.run_gate(inspector, "missing", stable=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("could not resolve", result.stderr)
                (self.bin / inspector).unlink()

    def test_stable_all_equal(self):
        for inspector in ("crane", "docker"):
            with self.subTest(inspector=inspector):
                result = self.run_gate(inspector, "equal", stable=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn("OK: published digest verification passed", result.stdout)
                (self.bin / inspector).unlink()


if __name__ == "__main__":
    unittest.main()
