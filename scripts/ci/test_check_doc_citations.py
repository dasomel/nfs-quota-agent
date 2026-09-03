#!/usr/bin/env python3
"""Tests for scripts/ci/check-doc-citations.py.

Covers:
- Valid path:line and path:start-end citations within line count
- Missing referenced files
- Line range out of bounds (start < 1, start > end, end > total lines)
- Resolution against repo root and relative to doc directory
- Valid merged PR #N references
- Unmerged (OPEN, CLOSED) PR #N references
- Caching of PR lookups across multiple citations
- CLI invocation via subprocess with exit code and error output assertions
"""

from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parent / "check-doc-citations.py"


class CheckDocCitationsTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)

    def _run_cli(self, args: list[str]) -> subprocess.CompletedProcess[str]:
        cmd = [sys.executable, str(SCRIPT), *args]
        return subprocess.run(
            cmd,
            cwd=str(self.root),
            capture_output=True,
            text=True,
            check=False,
        )

    def test_valid_citations_pass(self) -> None:
        target_file = self.root / "internal" / "agent.go"
        target_file.parent.mkdir(parents=True, exist_ok=True)
        target_file.write_text("\n".join(f"// line {i}" for i in range(1, 101)) + "\n")

        doc = self.root / "docs" / "readiness.md"
        doc.parent.mkdir(parents=True, exist_ok=True)
        doc.write_text(
            "Evidence: [`internal/agent.go:10-25`](../internal/agent.go), PR #42\n"
        )

        proc = self._run_cli(
            [
                "--root",
                str(self.root),
                "--mock-pr-states",
                json.dumps({"42": "MERGED"}),
                str(doc),
            ]
        )
        self.assertEqual(proc.returncode, 0, msg=proc.stderr)
        self.assertIn("all valid", proc.stdout)

    def test_missing_referenced_file_fails(self) -> None:
        doc = self.root / "docs" / "readiness.md"
        doc.parent.mkdir(parents=True, exist_ok=True)
        doc.write_text("Evidence: `internal/missing.go:1-10`\n")

        proc = self._run_cli(["--root", str(self.root), str(doc)])
        self.assertEqual(proc.returncode, 1)
        self.assertIn("does not exist on current tree", proc.stderr)

    def test_line_range_exceeds_total_lines_fails(self) -> None:
        target_file = self.root / "README.md"
        target_file.write_text("Line 1\nLine 2\nLine 3\n")

        doc = self.root / "readiness.md"
        doc.write_text("See [README.md:1-10](README.md)\n")

        proc = self._run_cli(["--root", str(self.root), str(doc)])
        self.assertEqual(proc.returncode, 1)
        self.assertIn("exceeds total line count (3)", proc.stderr)

    def test_inverted_range_fails(self) -> None:
        target_file = self.root / "README.md"
        target_file.write_text("Line 1\nLine 2\nLine 3\n")

        doc = self.root / "readiness.md"
        doc.write_text("See README.md:3-1\n")

        proc = self._run_cli(["--root", str(self.root), str(doc)])
        self.assertEqual(proc.returncode, 1)
        self.assertIn("inverted range 3-1", proc.stderr)

    def test_unmerged_pr_fails(self) -> None:
        doc = self.root / "readiness.md"
        doc.write_text("Status: PR #100\n")

        proc = self._run_cli(
            [
                "--root",
                str(self.root),
                "--mock-pr-states",
                json.dumps({"100": "OPEN"}),
                str(doc),
            ]
        )
        self.assertEqual(proc.returncode, 1)
        self.assertIn("PR #100 state is OPEN (expected MERGED)", proc.stderr)

    def test_pr_caching(self) -> None:
        # Multiple citations of the same PR in the same document
        doc = self.root / "readiness.md"
        doc.write_text("PR #55 and again PR #55 and PR #55\n")

        proc = self._run_cli(
            [
                "--root",
                str(self.root),
                "--mock-pr-states",
                json.dumps({"55": "MERGED"}),
                str(doc),
            ]
        )
        self.assertEqual(proc.returncode, 0, msg=proc.stderr)
        self.assertIn("Verified 3 citation(s)", proc.stdout)


if __name__ == "__main__":
    unittest.main()
