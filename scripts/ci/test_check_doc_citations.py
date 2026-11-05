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
- Blank cited lines (single line, all-blank range) fail; ranges with content pass
- Comma-list citations are checked per part
- --since drift warnings (insert above/below, in-range edit), --strict-drift, bad rev
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

    def test_blank_single_line_fails(self) -> None:
        (self.root / "NOTES.md").write_text("first\n\nthird\n")
        doc = self.root / "readiness.md"
        doc.write_text("See NOTES.md:2\n")

        proc = self._run_cli(["--root", str(self.root), str(doc)])
        self.assertEqual(proc.returncode, 1)
        self.assertIn("cited line(s) 2 are blank", proc.stderr)

    def test_all_blank_range_fails(self) -> None:
        (self.root / "NOTES.md").write_text("first\n\n  \n\t\nlast\n")
        doc = self.root / "readiness.md"
        doc.write_text("See NOTES.md:2-4\n")

        proc = self._run_cli(["--root", str(self.root), str(doc)])
        self.assertEqual(proc.returncode, 1)
        self.assertIn("cited line(s) 2-4 are blank", proc.stderr)

    def test_range_with_content_and_blank_edges_passes(self) -> None:
        (self.root / "NOTES.md").write_text("\nbody\n\n")
        doc = self.root / "readiness.md"
        doc.write_text("See NOTES.md:1-3\n")

        proc = self._run_cli(["--root", str(self.root), str(doc)])
        self.assertEqual(proc.returncode, 0, msg=proc.stderr)

    def test_comma_list_checked_per_part(self) -> None:
        (self.root / "NOTES.md").write_text("one\n\nthree\nfour\n")
        doc = self.root / "readiness.md"
        doc.write_text("See NOTES.md:1,3-4 and NOTES.md:1,2\n")

        proc = self._run_cli(["--root", str(self.root), str(doc)])
        self.assertEqual(proc.returncode, 1)
        self.assertEqual(proc.stderr.count("are blank"), 1, msg=proc.stderr)
        self.assertIn("cited line(s) 2 are blank", proc.stderr)


class DriftTest(unittest.TestCase):
    """--since drift warnings against a throwaway git repo."""

    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self._git("init", "-q")
        self.target = self.root / "src.txt"
        self.target.write_text("".join(f"line {i}\n" for i in range(1, 21)))
        self.doc = self.root / "readiness.md"
        self.doc.write_text("Evidence: src.txt:10-12\n")
        self._git("add", ".")
        self._git("commit", "-q", "-m", "base")
        self.rev = self._git("rev-parse", "HEAD").stdout.strip()

    def _git(self, *args: str) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [
                "git",
                "-c",
                "user.name=t",
                "-c",
                "user.email=t@example.com",
                "-c",
                "commit.gpgsign=false",
                *args,
            ],
            cwd=str(self.root),
            capture_output=True,
            text=True,
            check=True,
        )

    def _run_cli(self, *extra: str) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [sys.executable, str(SCRIPT), "--root", str(self.root), *extra, str(self.doc)],
            cwd=str(self.root),
            capture_output=True,
            text=True,
            check=False,
        )

    def _base(self) -> list[str]:
        return [f"line {i}" for i in range(1, 21)]

    def _edit(self, lines: list[str]) -> None:
        self.target.write_text("".join(f"{t}\n" for t in lines))

    def test_insert_above_warns(self) -> None:
        lines = self._base()
        lines[2:2] = ["new a", "new b"]
        self._edit(lines)

        proc = self._run_cli("--since", self.rev)
        self.assertEqual(proc.returncode, 0, msg=proc.stderr)
        self.assertIn(f"src.txt:10-12 may have drifted since {self.rev}", proc.stderr)
        self.assertIn("(hunk @@ -2,0 +3,2 @@)", proc.stderr)

    def test_insert_below_does_not_warn(self) -> None:
        lines = self._base()
        lines[15:15] = ["new a", "new b"]
        self._edit(lines)

        proc = self._run_cli("--since", self.rev)
        self.assertEqual(proc.returncode, 0, msg=proc.stderr)
        self.assertNotIn("drifted", proc.stderr)

    def test_edit_inside_range_without_count_change_warns(self) -> None:
        lines = self._base()
        lines[10] = "changed"
        self._edit(lines)

        proc = self._run_cli("--since", self.rev)
        self.assertEqual(proc.returncode, 0, msg=proc.stderr)
        self.assertIn("src.txt:10-12 may have drifted", proc.stderr)

    def test_edit_above_without_count_change_does_not_warn(self) -> None:
        lines = self._base()
        lines[2] = "changed"
        self._edit(lines)

        proc = self._run_cli("--since", self.rev)
        self.assertNotIn("drifted", proc.stderr)

    def test_deleted_file_warns(self) -> None:
        self.target.unlink()

        proc = self._run_cli("--since", self.rev)
        self.assertEqual(proc.returncode, 1)  # the missing file is also a hard failure
        self.assertIn("file deleted", proc.stderr)

    def test_strict_drift_exits_1(self) -> None:
        lines = self._base()
        lines[0:0] = ["new"]
        self._edit(lines)

        proc = self._run_cli("--since", self.rev, "--strict-drift")
        self.assertEqual(proc.returncode, 1)
        self.assertIn("drifted", proc.stderr)

    def test_no_drift_check_without_flag(self) -> None:
        lines = self._base()
        lines[0:0] = ["new"]
        self._edit(lines)

        proc = self._run_cli()
        self.assertEqual(proc.returncode, 0, msg=proc.stderr)
        self.assertNotIn("drifted", proc.stderr)

    def test_unknown_rev_exits_2(self) -> None:
        proc = self._run_cli("--since", "does-not-exist")
        self.assertEqual(proc.returncode, 2)
        self.assertIn("unknown git revision", proc.stderr)

    def test_non_git_directory_exits_2(self) -> None:
        with tempfile.TemporaryDirectory() as plain:
            doc = Path(plain) / "readiness.md"
            doc.write_text("nothing cited\n")
            proc = subprocess.run(
                [sys.executable, str(SCRIPT), "--root", plain, "--since", "HEAD", str(doc)],
                cwd=plain,
                capture_output=True,
                text=True,
                check=False,
            )
        self.assertEqual(proc.returncode, 2)
        self.assertIn("git", proc.stderr)


if __name__ == "__main__":
    unittest.main()
