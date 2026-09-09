#!/usr/bin/env python3
"""Enforce the OpenForge skill-verification evidence contract for this repository.

A repository-local skill under .agents/skills/<name>/SKILL.md may only declare
`openforge-maturity: verified` (or `stable`) when a matching
.agents/skill-evals/<name>.json records a fresh-session replay using the
`openforge-agent-skill-verification/v1` schema.

The upstream portfolio auditor (dasomel/openforge templates/scripts/audit-agent-skills.py)
applies the same rules once a month against a clone of this repository; running them
here keeps a false `verified` claim from reaching main in the first place, and reuses
the upstream finding codes so both reports name the same defect. The one local-only
rule is SKILL-VERIFICATION-ORPHAN.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from datetime import date
from pathlib import Path

SCHEMA = "openforge-agent-skill-verification/v1"
EVIDENCE_DIR = Path(".agents/skill-evals")
# Every root the upstream auditor scans. A verified skill parked under any of them
# must carry evidence, or this gate would be looser than the audit it front-runs.
SKILL_ROOTS = (Path(".agents/skills"), Path(".claude/skills"), Path("skills"))
EVIDENCE_REQUIRED_MATURITY = {"verified", "stable"}
VALID_MATURITY = {"draft", "verified", "stable", "deprecated"}
PASS_STATUSES = {"pass", "passed", "success", "successful", "ok", "verified"}
DATE_RE = re.compile(r"^\d{4}-\d{2}-\d{2}$")


def find_repo_root(start_path: Path | None = None) -> Path:
    """Find repository root containing .git or return the script's grandparent."""
    cur = (start_path or Path.cwd()).resolve()
    for p in [cur, *cur.parents]:
        if (p / ".git").exists():
            return p
    return Path(__file__).resolve().parent.parent.parent


def read_text(path: Path) -> str:
    """Read a file the way the upstream auditor does, so odd bytes report rather than crash."""
    return path.read_text(encoding="utf-8", errors="replace")


def uses_symlink(root: Path, path: Path) -> bool:
    """True when path is reached through a symlinked directory under root."""
    current = path.parent
    while current != root and current != current.parent:
        if current.is_symlink():
            return True
        current = current.parent
    return False


def skill_files(root: Path) -> list[Path]:
    """Collect SKILL.md files across every discovery root, collapsing symlink aliases.

    `.agents/skills/<name>` is often a symlink to `.claude/skills/<name>` so both agent
    runtimes discover one physical skill. Auditing it twice would report every finding
    twice, so aliases resolve to the same target and the real path wins.
    """
    selected: dict[Path, tuple[tuple[int, int], Path]] = {}
    for order, skill_root in enumerate(SKILL_ROOTS):
        base = root / skill_root
        if not base.is_dir():
            continue
        for file in sorted(base.glob("*/SKILL.md")):
            target = file.resolve()
            rank = (1 if uses_symlink(root, file) else 0, order)
            current = selected.get(target)
            if current is None or rank < current[0]:
                selected[target] = (rank, file)
    return sorted(path for _, path in selected.values())


def parse_front_matter(text: str) -> dict[str, str]:
    """Extract flat `key: value` pairs from a SKILL.md YAML front matter block.

    Nested `metadata:` keys are flattened without their parent prefix; the
    OpenForge keys this checks (openforge-*) are unique across both levels.
    """
    if not text.startswith("---"):
        return {}
    lines = text.splitlines()
    fields: dict[str, str] = {}
    for line in lines[1:]:
        if line.strip() == "---":
            break
        match = re.match(r"^\s*([A-Za-z0-9_-]+):\s*(.*)$", line)
        if not match:
            continue
        key, value = match.group(1), match.group(2).strip()
        if value:
            fields[key] = value.strip('"').strip("'")
    return fields


def passed(value: object) -> bool:
    return str(value or "").strip().lower() in PASS_STATUSES


def check_stage(evidence: dict, key: str, code: str, rel: str, failures: list[str]) -> None:
    """Validate one replay stage (happyPath / edgeCase) has a pass plus evidence refs."""
    stage = evidence.get(key)
    if not isinstance(stage, dict) or not passed(stage.get("status")):
        failures.append(f"{rel}: SKILL-VERIFICATION-{code}: {key}.status must explicitly pass.")
        return
    refs = stage.get("evidence")
    if not isinstance(refs, list) or not refs:
        failures.append(
            f"{rel}: SKILL-VERIFICATION-{code}-EVIDENCE: {key}.evidence must list at least one reference."
        )


def validate_evidence(path: Path, rel: str, skill_name: str, skill_version: str) -> list[str]:
    failures: list[str] = []
    try:
        evidence = json.loads(read_text(path))
    except (OSError, ValueError) as exc:
        return [f"{rel}: SKILL-VERIFICATION-JSON: invalid verification evidence: {exc}"]
    if not isinstance(evidence, dict):
        return [f"{rel}: SKILL-VERIFICATION-JSON: verification evidence must be a JSON object."]

    if evidence.get("schemaVersion") != SCHEMA:
        failures.append(f"{rel}: SKILL-VERIFICATION-SCHEMA: schemaVersion must be {SCHEMA}.")
    if evidence.get("skill") != skill_name:
        failures.append(f"{rel}: SKILL-VERIFICATION-NAME: evidence skill must match SKILL.md name.")
    if str(evidence.get("skillVersion", "")) != skill_version:
        failures.append(
            f"{rel}: SKILL-VERIFICATION-VERSION: evidence skillVersion must match metadata.openforge-version."
        )
    if evidence.get("freshSession") is not True:
        failures.append(f"{rel}: SKILL-VERIFICATION-FRESH: evidence must record freshSession=true.")

    runtime = evidence.get("agentRuntime")
    if not isinstance(runtime, str) or not runtime.strip():
        failures.append(f"{rel}: SKILL-VERIFICATION-RUNTIME: agentRuntime must identify the replay runtime.")

    check_stage(evidence, "happyPath", "HAPPY", rel, failures)
    check_stage(evidence, "edgeCase", "EDGE", rel, failures)

    checks = evidence.get("deterministicChecks")
    if not isinstance(checks, list) or not checks:
        failures.append(
            f"{rel}: SKILL-VERIFICATION-CHECKS: deterministicChecks must list at least one repository-owned check."
        )
    else:
        for index, check in enumerate(checks):
            command = check.get("command") if isinstance(check, dict) else None
            if not isinstance(check, dict) or not isinstance(command, str) or not command.strip() or not passed(check.get("status")):
                failures.append(
                    f"{rel}: SKILL-VERIFICATION-CHECK: deterministicChecks[{index}] requires a command "
                    "and an explicit passing status."
                )

    unverified = evidence.get("unverified")
    if unverified is not None and not isinstance(unverified, list):
        failures.append(f"{rel}: SKILL-VERIFICATION-UNVERIFIED: unverified must be a JSON array.")

    # date.today() is the runner's local date, which is UTC in CI. An artifact
    # written from a UTC+n timezone can therefore be a day ahead of the runner
    # and fail here; record the UTC date of the replay.
    verified_at = str(evidence.get("verifiedAt", ""))
    if not DATE_RE.match(verified_at):
        failures.append(f"{rel}: SKILL-VERIFICATION-DATE: verifiedAt must use YYYY-MM-DD.")
    else:
        try:
            if date.fromisoformat(verified_at) > date.today():
                failures.append(f"{rel}: SKILL-VERIFICATION-FUTURE: verifiedAt cannot be in the future.")
        except ValueError:
            failures.append(f"{rel}: SKILL-VERIFICATION-DATE: verifiedAt is not a valid date.")

    return failures


def audit(root: Path) -> tuple[list[str], int]:
    failures: list[str] = []
    skills = skill_files(root)
    owning_names: set[str] = set()
    for skill_path in skills:
        rel_skill = str(skill_path.relative_to(root))
        fields = parse_front_matter(read_text(skill_path))
        name = fields.get("name", "")
        maturity = fields.get("openforge-maturity", "")
        version = fields.get("openforge-version", "")

        if not name:
            failures.append(f"{rel_skill}: SKILL-NAME: front matter must declare name.")
            continue
        owning_names.add(name)
        if name != skill_path.parent.name:
            failures.append(
                f"{rel_skill}: SKILL-DIR-MISMATCH: name '{name}' must match directory '{skill_path.parent.name}'."
            )
        if maturity and maturity not in VALID_MATURITY:
            failures.append(f"{rel_skill}: SKILL-MATURITY: unknown openforge-maturity '{maturity}'.")
        if maturity not in EVIDENCE_REQUIRED_MATURITY:
            continue

        evidence_path = root / EVIDENCE_DIR / f"{name}.json"
        rel_evidence = str(evidence_path.relative_to(root))
        if not evidence_path.is_file():
            failures.append(
                f"{rel_skill}: SKILL-VERIFICATION-EVIDENCE: {maturity} skill requires {rel_evidence}."
            )
            continue
        failures.extend(validate_evidence(evidence_path, rel_evidence, name, version))

    # An evidence file with no owning skill is stale governance state, not proof.
    # Local-only rule: upstream ignores orphans, and a stale one would quietly outlive
    # the skill whose promotion it justified.
    evidence_root = root / EVIDENCE_DIR
    for evidence_path in sorted(evidence_root.glob("*.json")) if evidence_root.is_dir() else []:
        if evidence_path.stem not in owning_names:
            failures.append(
                f"{evidence_path.relative_to(root)}: SKILL-VERIFICATION-ORPHAN: "
                "no SKILL.md under any discovery root owns this evidence."
            )

    return failures, len(skills)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", default=None, help="Repository root (defaults to the enclosing git repo)")
    args = parser.parse_args()
    root = Path(args.root).resolve() if args.root else find_repo_root()

    if not any((root / skill_root).is_dir() for skill_root in SKILL_ROOTS):
        roots = ", ".join(str(skill_root) for skill_root in SKILL_ROOTS)
        print(f"ERROR: no skill discovery root ({roots}) found under {root}", file=sys.stderr)
        return 2

    failures, scanned = audit(root)
    for failure in failures:
        print(f"FAIL {failure}")
    if failures:
        print(f"\n{len(failures)} skill evidence defect(s) across {scanned} skill(s).", file=sys.stderr)
        return 1
    print(f"OK {scanned} skill(s) satisfy the {SCHEMA} evidence contract.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
