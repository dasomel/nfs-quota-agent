#!/usr/bin/env python3
"""Tests for check-agent-trace-requirement.py's dependabot pin-only exemption.

Builds tiny throwaway git repos on disk so `is_dependabot_action_pin_only`
exercises a real `git diff`, not a mock. Invokes the script as a subprocess
(its filename has hyphens, so it isn't import-able as a module) to exercise
the full CLI path including argument parsing and exit codes.
"""
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parent / "check-agent-trace-requirement.py"

# Minimal policy fixture, independent of the real .agents/evals/risk-policy.json,
# so this test does not couple to that file's content changing over time.
POLICY = {
    "schemaVersion": "openforge-agent-risk-policy/v1",
    "defaultRisk": "low",
    "traceRequiredAt": ["high"],
    "tracePathPrefix": ".agents/evals/traces/",
    "rules": [
        {"risk": "high", "pattern": ".github/workflows/**", "reason": "CI behavior"},
    ],
}

PIN_V3 = "uses: actions/checkout@1111111111111111111111111111111111111111 # v3.0.0"
PIN_V4 = "uses: actions/checkout@2222222222222222222222222222222222222222 # v4.0.0"
# owner/repo/subpath form used by e.g. github/codeql-action/upload-sarif - the
# real-world shape of dependabot PR #88, which the first regex draft missed
# because it only allowed a single "/" in the action reference.
SUBPATH_PIN_OLD = "uses: github/codeql-action/upload-sarif@3333333333333333333333333333333333333333 # v4.37.8"
SUBPATH_PIN_NEW = "uses: github/codeql-action/upload-sarif@4444444444444444444444444444444444444444 # v4.37.9"


def _run_git(args, cwd):
    result = subprocess.run(
        ["git", *args], cwd=cwd, capture_output=True, text=True, check=True
    )
    return result.stdout.strip()


def _init_repo(repo_dir):
    _run_git(["init"], repo_dir)
    _run_git(["config", "user.email", "test@example.com"], repo_dir)
    _run_git(["config", "user.name", "Test"], repo_dir)
    _run_git(["config", "commit.gpgsign", "false"], repo_dir)


def _write_workflow(repo_dir, lines):
    wf_dir = repo_dir / ".github" / "workflows"
    wf_dir.mkdir(parents=True, exist_ok=True)
    (wf_dir / "ci.yml").write_text("\n".join(lines) + "\n")


def _commit(repo_dir, message):
    _run_git(["add", "-A"], repo_dir)
    _run_git(["commit", "-m", message], repo_dir)
    return _run_git(["rev-parse", "HEAD"], repo_dir)


class TraceRequirementCLITest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.repo_dir = Path(self.tmp.name) / "repo"
        self.repo_dir.mkdir()
        _init_repo(self.repo_dir)

        self.policy_path = Path(self.tmp.name) / "risk-policy.json"
        self.policy_path.write_text(json.dumps(POLICY))

        self.changed_files_path = Path(self.tmp.name) / "changed-files.txt"

    def _run_script(self, changed_files, extra_args=None):
        self.changed_files_path.write_text("\n".join(changed_files) + "\n")
        args = [
            sys.executable,
            str(SCRIPT),
            "--policy",
            str(self.policy_path),
            "--changed-files",
            str(self.changed_files_path),
        ]
        if extra_args:
            args.extend(extra_args)
        return subprocess.run(args, cwd=self.repo_dir, capture_output=True, text=True)

    def test_pure_dependabot_pin_only_bump_is_exempt(self):
        # Matches the repo's dominant workflow style: "- name: ..." followed
        # by "uses: ..." on its own line, not the combined "- uses: ..." form.
        _write_workflow(
            self.repo_dir,
            [
                "name: CI",
                "on: [push]",
                "jobs:",
                "  build:",
                "    runs-on: ubuntu-latest",
                "    steps:",
                "      - name: Checkout",
                f"        {PIN_V3}",
            ],
        )
        base_sha = _commit(self.repo_dir, "initial")
        _write_workflow(
            self.repo_dir,
            [
                "name: CI",
                "on: [push]",
                "jobs:",
                "  build:",
                "    runs-on: ubuntu-latest",
                "    steps:",
                "      - name: Checkout",
                f"        {PIN_V4}",
            ],
        )
        head_sha = _commit(self.repo_dir, "bump checkout")

        result = self._run_script(
            [".github/workflows/ci.yml"],
            [
                "--base-sha",
                base_sha,
                "--head-sha",
                head_sha,
                "--github-actor",
                "dependabot[bot]",
                "--pr-author",
                "dependabot[bot]",
            ],
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        report = json.loads(result.stdout)
        self.assertTrue(report["traceExempt"])
        self.assertIsNotNone(report["traceExemptReason"])

    def test_dependabot_pin_bump_with_subpath_action_is_exempt(self):
        # Regression test for the real PR #88 shape: owner/repo/subpath@sha,
        # not just owner/repo@sha.
        _write_workflow(
            self.repo_dir,
            [
                "name: CI",
                "on: [push]",
                "jobs:",
                "  build:",
                "    runs-on: ubuntu-latest",
                "    steps:",
                "      - name: Upload SARIF",
                f"        {SUBPATH_PIN_OLD}",
            ],
        )
        base_sha = _commit(self.repo_dir, "initial")
        _write_workflow(
            self.repo_dir,
            [
                "name: CI",
                "on: [push]",
                "jobs:",
                "  build:",
                "    runs-on: ubuntu-latest",
                "    steps:",
                "      - name: Upload SARIF",
                f"        {SUBPATH_PIN_NEW}",
            ],
        )
        head_sha = _commit(self.repo_dir, "bump codeql-action/upload-sarif")

        result = self._run_script(
            [".github/workflows/ci.yml"],
            [
                "--base-sha",
                base_sha,
                "--head-sha",
                head_sha,
                "--github-actor",
                "dependabot[bot]",
                "--pr-author",
                "dependabot[bot]",
            ],
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        report = json.loads(result.stdout)
        self.assertTrue(report["traceExempt"])

    def test_dependabot_diff_also_touching_run_line_is_not_exempt(self):
        _write_workflow(
            self.repo_dir,
            [
                "name: CI",
                "on: [push]",
                "jobs:",
                "  build:",
                "    runs-on: ubuntu-latest",
                "    steps:",
                "      - name: Checkout",
                f"        {PIN_V3}",
                "      - name: noop",
                '        run: echo "unrelated one"',
                '      - run: echo "hello"',
            ],
        )
        base_sha = _commit(self.repo_dir, "initial")
        _write_workflow(
            self.repo_dir,
            [
                "name: CI",
                "on: [push]",
                "jobs:",
                "  build:",
                "    runs-on: ubuntu-latest",
                "    steps:",
                "      - name: Checkout",
                f"        {PIN_V4}",
                "      - name: noop",
                '        run: echo "unrelated one"',
                '      - run: echo "goodbye"',
            ],
        )
        head_sha = _commit(self.repo_dir, "bump checkout and change run line")

        result = self._run_script(
            [".github/workflows/ci.yml"],
            [
                "--base-sha",
                base_sha,
                "--head-sha",
                head_sha,
                "--github-actor",
                "dependabot[bot]",
                "--pr-author",
                "dependabot[bot]",
            ],
        )
        self.assertEqual(result.returncode, 1, result.stdout)
        report = json.loads(result.stdout)
        self.assertFalse(report["traceExempt"])

    def test_identical_pin_diff_but_actor_not_dependabot_is_not_exempt(self):
        _write_workflow(
            self.repo_dir,
            [
                "name: CI",
                "on: [push]",
                "jobs:",
                "  build:",
                "    runs-on: ubuntu-latest",
                "    steps:",
                "      - name: Checkout",
                f"        {PIN_V3}",
            ],
        )
        base_sha = _commit(self.repo_dir, "initial")
        _write_workflow(
            self.repo_dir,
            [
                "name: CI",
                "on: [push]",
                "jobs:",
                "  build:",
                "    runs-on: ubuntu-latest",
                "    steps:",
                "      - name: Checkout",
                f"        {PIN_V4}",
            ],
        )
        head_sha = _commit(self.repo_dir, "bump checkout")

        result = self._run_script(
            [".github/workflows/ci.yml"],
            [
                "--base-sha",
                base_sha,
                "--head-sha",
                head_sha,
                "--github-actor",
                "some-human",
                "--pr-author",
                "dependabot[bot]",
            ],
        )
        self.assertEqual(result.returncode, 1, result.stdout)
        report = json.loads(result.stdout)
        self.assertFalse(report["traceExempt"])

    def test_pr_with_trace_file_passes_regardless(self):
        result = self._run_script(
            [
                ".github/workflows/ci.yml",
                ".agents/evals/traces/2026-08-something.json",
            ]
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        report = json.loads(result.stdout)
        self.assertTrue(report["traceRequired"])
        self.assertTrue(report["traceChanged"])
        self.assertFalse(report["traceExempt"])

    def test_low_risk_only_change_requires_no_trace(self):
        result = self._run_script(["README.md"])
        self.assertEqual(result.returncode, 0, result.stderr)
        report = json.loads(result.stdout)
        self.assertEqual(report["risk"], "low")
        self.assertFalse(report["traceRequired"])
        self.assertFalse(report["traceExempt"])


if __name__ == "__main__":
    unittest.main()
