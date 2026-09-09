#!/usr/bin/env python3
"""Tests for scripts/ci/check-agent-skill-evidence.py.

Covers:
- A draft skill needs no evidence file; a verified one does
- A complete verified skill plus matching evidence passes
- Each schema rule (schema, name, version, freshSession, runtime, stages, checks, date) fails closed
- Orphan evidence files and name/directory mismatches are reported
"""

from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import unittest
from datetime import date, timedelta
from pathlib import Path

SCRIPT = Path(__file__).resolve().parent / "check-agent-skill-evidence.py"

SKILL_TEMPLATE = """---
name: {name}
description: Example project skill used by the evidence contract tests.
metadata:
  openforge-scope: project
  openforge-owner: dasomel/nfs-quota-agent
  openforge-maturity: {maturity}
  openforge-version: "{version}"
---

# Body
"""


def valid_evidence(name: str = "sample-skill", version: str = "1") -> dict:
    return {
        "schemaVersion": "openforge-agent-skill-verification/v1",
        "skill": name,
        "skillVersion": version,
        "freshSession": True,
        "agentRuntime": "test-runtime",
        "happyPath": {"status": "passed", "scenario": "happy", "evidence": ["artifact:happy"]},
        "edgeCase": {"status": "passed", "scenario": "edge", "evidence": ["artifact:edge"]},
        "deterministicChecks": [{"command": "make test", "status": "passed", "scope": "repo"}],
        "runtimeEvidence": [],
        "unverified": [],
        "verifiedAt": date.today().isoformat(),
    }


class CheckAgentSkillEvidenceTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        (self.root / ".git").mkdir()
        (self.root / ".agents" / "skills").mkdir(parents=True)
        (self.root / ".agents" / "skill-evals").mkdir(parents=True)

    def write_skill(self, name: str = "sample-skill", maturity: str = "verified", version: str = "1") -> None:
        skill_dir = self.root / ".agents" / "skills" / name
        skill_dir.mkdir(parents=True, exist_ok=True)
        (skill_dir / "SKILL.md").write_text(
            SKILL_TEMPLATE.format(name=name, maturity=maturity, version=version), encoding="utf-8"
        )

    def write_evidence(self, payload: dict, name: str = "sample-skill") -> None:
        (self.root / ".agents" / "skill-evals" / f"{name}.json").write_text(
            json.dumps(payload, indent=2), encoding="utf-8"
        )

    def run_check(self) -> subprocess.CompletedProcess:
        return subprocess.run(
            [sys.executable, str(SCRIPT), "--root", str(self.root)],
            capture_output=True,
            text=True,
        )

    def assert_fails_with(self, code: str) -> None:
        result = self.run_check()
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn(code, result.stdout)

    def test_draft_skill_needs_no_evidence(self) -> None:
        self.write_skill(maturity="draft")
        result = self.run_check()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_verified_skill_with_valid_evidence_passes(self) -> None:
        self.write_skill()
        self.write_evidence(valid_evidence())
        result = self.run_check()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_stable_skill_also_requires_evidence(self) -> None:
        self.write_skill(maturity="stable")
        self.assert_fails_with("SKILL-VERIFICATION-EVIDENCE")

    def test_verified_skill_without_evidence_fails(self) -> None:
        self.write_skill()
        self.assert_fails_with("SKILL-VERIFICATION-EVIDENCE")

    def test_wrong_schema_version_fails(self) -> None:
        self.write_skill()
        payload = valid_evidence()
        payload["schemaVersion"] = "openforge-agent-skill-verification/v0"
        self.write_evidence(payload)
        self.assert_fails_with("SKILL-VERIFICATION-SCHEMA")

    def test_skill_name_mismatch_fails(self) -> None:
        self.write_skill()
        payload = valid_evidence()
        payload["skill"] = "other-skill"
        self.write_evidence(payload)
        self.assert_fails_with("SKILL-VERIFICATION-NAME")

    def test_skill_version_mismatch_fails(self) -> None:
        self.write_skill(version="2")
        self.write_evidence(valid_evidence(version="1"))
        self.assert_fails_with("SKILL-VERIFICATION-VERSION")

    def test_fresh_session_false_fails(self) -> None:
        self.write_skill()
        payload = valid_evidence()
        payload["freshSession"] = False
        self.write_evidence(payload)
        self.assert_fails_with("SKILL-VERIFICATION-FRESH")

    def test_blank_runtime_fails(self) -> None:
        self.write_skill()
        payload = valid_evidence()
        payload["agentRuntime"] = "   "
        self.write_evidence(payload)
        self.assert_fails_with("SKILL-VERIFICATION-RUNTIME")

    def test_unpassed_happy_path_fails(self) -> None:
        self.write_skill()
        payload = valid_evidence()
        payload["happyPath"]["status"] = "pending"
        self.write_evidence(payload)
        self.assert_fails_with("SKILL-VERIFICATION-HAPPY")

    def test_happy_path_without_evidence_refs_fails(self) -> None:
        self.write_skill()
        payload = valid_evidence()
        payload["happyPath"]["evidence"] = []
        self.write_evidence(payload)
        self.assert_fails_with("SKILL-VERIFICATION-HAPPY-EVIDENCE")

    def test_unpassed_edge_case_fails(self) -> None:
        self.write_skill()
        payload = valid_evidence()
        payload["edgeCase"]["status"] = "failed"
        self.write_evidence(payload)
        self.assert_fails_with("SKILL-VERIFICATION-EDGE")

    def test_edge_case_without_evidence_refs_fails(self) -> None:
        self.write_skill()
        payload = valid_evidence()
        del payload["edgeCase"]["evidence"]
        self.write_evidence(payload)
        self.assert_fails_with("SKILL-VERIFICATION-EDGE-EVIDENCE")

    def test_empty_deterministic_checks_fails(self) -> None:
        self.write_skill()
        payload = valid_evidence()
        payload["deterministicChecks"] = []
        self.write_evidence(payload)
        self.assert_fails_with("SKILL-VERIFICATION-CHECKS")

    def test_deterministic_check_without_passing_status_fails(self) -> None:
        self.write_skill()
        payload = valid_evidence()
        payload["deterministicChecks"] = [{"command": "make test", "status": "skipped"}]
        self.write_evidence(payload)
        self.assert_fails_with("SKILL-VERIFICATION-CHECK")

    def test_unverified_must_be_a_list(self) -> None:
        self.write_skill()
        payload = valid_evidence()
        payload["unverified"] = "nothing"
        self.write_evidence(payload)
        self.assert_fails_with("SKILL-VERIFICATION-UNVERIFIED")

    def test_bad_date_format_fails(self) -> None:
        self.write_skill()
        payload = valid_evidence()
        payload["verifiedAt"] = "09/09/2026"
        self.write_evidence(payload)
        self.assert_fails_with("SKILL-VERIFICATION-DATE")

    def test_future_date_fails(self) -> None:
        self.write_skill()
        payload = valid_evidence()
        payload["verifiedAt"] = (date.today() + timedelta(days=1)).isoformat()
        self.write_evidence(payload)
        self.assert_fails_with("SKILL-VERIFICATION-FUTURE")

    def test_malformed_json_fails(self) -> None:
        self.write_skill()
        (self.root / ".agents" / "skill-evals" / "sample-skill.json").write_text("{ not json", encoding="utf-8")
        self.assert_fails_with("SKILL-VERIFICATION-JSON")

    def test_orphan_evidence_fails(self) -> None:
        self.write_skill(maturity="draft")
        self.write_evidence(valid_evidence(name="ghost-skill"), name="ghost-skill")
        self.assert_fails_with("SKILL-VERIFICATION-ORPHAN")

    def test_name_directory_mismatch_fails(self) -> None:
        skill_dir = self.root / ".agents" / "skills" / "sample-skill"
        skill_dir.mkdir(parents=True)
        (skill_dir / "SKILL.md").write_text(
            SKILL_TEMPLATE.format(name="different-name", maturity="draft", version="1"), encoding="utf-8"
        )
        self.assert_fails_with("SKILL-DIR-MISMATCH")

    def test_missing_name_fails(self) -> None:
        skill_dir = self.root / ".agents" / "skills" / "sample-skill"
        skill_dir.mkdir(parents=True)
        (skill_dir / "SKILL.md").write_text(
            "---\ndescription: no name here\nmetadata:\n  openforge-maturity: verified\n---\n", encoding="utf-8"
        )
        self.assert_fails_with("SKILL-NAME")

    def test_unknown_maturity_fails(self) -> None:
        self.write_skill(maturity="production")
        self.assert_fails_with("SKILL-MATURITY")

    def test_evidence_top_level_must_be_object(self) -> None:
        self.write_skill()
        (self.root / ".agents" / "skill-evals" / "sample-skill.json").write_text("[1, 2, 3]", encoding="utf-8")
        self.assert_fails_with("SKILL-VERIFICATION-JSON")

    def test_non_dict_happy_path_fails(self) -> None:
        self.write_skill()
        payload = valid_evidence()
        payload["happyPath"] = "passed"
        self.write_evidence(payload)
        self.assert_fails_with("SKILL-VERIFICATION-HAPPY")

    def test_missing_edge_case_fails(self) -> None:
        self.write_skill()
        payload = valid_evidence()
        del payload["edgeCase"]
        self.write_evidence(payload)
        self.assert_fails_with("SKILL-VERIFICATION-EDGE")

    def test_deterministic_checks_not_a_list_fails(self) -> None:
        self.write_skill()
        payload = valid_evidence()
        payload["deterministicChecks"] = {"command": "make test", "status": "passed"}
        self.write_evidence(payload)
        self.assert_fails_with("SKILL-VERIFICATION-CHECKS")

    def test_non_dict_deterministic_check_entry_fails(self) -> None:
        self.write_skill()
        payload = valid_evidence()
        payload["deterministicChecks"] = ["make test"]
        self.write_evidence(payload)
        self.assert_fails_with("SKILL-VERIFICATION-CHECK")

    def test_blank_command_fails(self) -> None:
        self.write_skill()
        payload = valid_evidence()
        payload["deterministicChecks"] = [{"command": "   ", "status": "passed"}]
        self.write_evidence(payload)
        self.assert_fails_with("SKILL-VERIFICATION-CHECK")

    def test_null_command_fails(self) -> None:
        """A JSON null must not stringify to the non-empty literal "None" and pass."""
        self.write_skill()
        payload = valid_evidence()
        payload["deterministicChecks"] = [{"command": None, "status": "passed"}]
        self.write_evidence(payload)
        self.assert_fails_with("SKILL-VERIFICATION-CHECK")

    def test_calendar_invalid_date_fails(self) -> None:
        """2026-02-31 matches the YYYY-MM-DD shape but is not a real date."""
        self.write_skill()
        payload = valid_evidence()
        payload["verifiedAt"] = "2026-02-31"
        self.write_evidence(payload)
        self.assert_fails_with("SKILL-VERIFICATION-DATE")

    def test_verified_skill_under_claude_skills_also_needs_evidence(self) -> None:
        """The gate must scan every root the upstream auditor scans, not just .agents/skills."""
        skill_dir = self.root / ".claude" / "skills" / "claude-only-skill"
        skill_dir.mkdir(parents=True)
        (skill_dir / "SKILL.md").write_text(
            SKILL_TEMPLATE.format(name="claude-only-skill", maturity="verified", version="1"), encoding="utf-8"
        )
        self.assert_fails_with("SKILL-VERIFICATION-EVIDENCE")

    def test_symlink_alias_is_audited_once(self) -> None:
        """.agents/skills/<name> -> ../../.claude/skills/<name> is one skill, not two."""
        real = self.root / ".claude" / "skills" / "aliased-skill"
        real.mkdir(parents=True)
        (real / "SKILL.md").write_text(
            SKILL_TEMPLATE.format(name="aliased-skill", maturity="draft", version="1"), encoding="utf-8"
        )
        (self.root / ".agents" / "skills" / "aliased-skill").symlink_to("../../.claude/skills/aliased-skill")
        result = self.run_check()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("OK 1 skill(s)", result.stdout)

    def test_block_scalar_cannot_override_metadata_maturity(self) -> None:
        """A description quoting a metadata key must not win over the real declaration.

        Flattening every indented line would read `draft` from the block scalar and
        skip the evidence requirement, passing here while the upstream auditor still
        reads `verified` and fails -- the exact parity gap this gate exists to close.
        """
        skill_dir = self.root / ".agents" / "skills" / "blocky"
        skill_dir.mkdir(parents=True)
        (skill_dir / "SKILL.md").write_text(
            "---\n"
            "name: blocky\n"
            "metadata:\n"
            "  openforge-maturity: verified\n"
            '  openforge-version: "1"\n'
            "description: >\n"
            "  Documents the front matter contract. Example of a parked skill:\n"
            "  openforge-maturity: draft\n"
            "---\n\n# Body\n",
            encoding="utf-8",
        )
        self.assert_fails_with("SKILL-VERIFICATION-EVIDENCE")

    def test_top_level_key_does_not_satisfy_metadata_maturity(self) -> None:
        """openforge-maturity declared at top level is not the metadata declaration."""
        skill_dir = self.root / ".agents" / "skills" / "toplevel"
        skill_dir.mkdir(parents=True)
        (skill_dir / "SKILL.md").write_text(
            "---\nname: toplevel\nopenforge-maturity: verified\n---\n\n# Body\n", encoding="utf-8"
        )
        result = self.run_check()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_no_skills_found_is_an_error_not_a_pass(self) -> None:
        """A root that exists but holds no SKILL.md must not report compliance."""
        result = self.run_check()
        self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
        self.assertIn("no SKILL.md found", result.stderr)

    def test_non_utf8_skill_does_not_crash(self) -> None:
        skill_dir = self.root / ".agents" / "skills" / "sample-skill"
        skill_dir.mkdir(parents=True)
        (skill_dir / "SKILL.md").write_bytes(
            SKILL_TEMPLATE.format(name="sample-skill", maturity="draft", version="1").encode("utf-8")
            + b"\xff\xfe binary tail\n"
        )
        result = self.run_check()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
