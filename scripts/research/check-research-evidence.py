#!/usr/bin/env python3
"""Validate append-only OpenForge research evidence in the current repository."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import pathlib
import re
import subprocess
import sys

from jsonschema import Draft202012Validator, FormatChecker


# D1: Keep the v1 contract local so copies work offline; compare it with the
# canonical OpenForge schema in tests. Cost: copies need updating on a version bump.
SCHEMA = {
    "$schema": "https://json-schema.org/draft/2020-12/schema",
    "$id": "https://github.com/dasomel/openforge/schemas/research-evidence-v1.schema.json",
    "title": "OpenForge Public Research Evidence v1",
    "type": "object",
    "additionalProperties": False,
    "required": ["schema_version", "timestamp", "repository", "revision", "event_type",
                 "task_or_test", "result", "duration_ms", "environment", "attempt",
                 "human_interventions", "review_corrections", "ci_retries", "metadata"],
    "properties": {
        "schema_version": {"const": "1.0"},
        "timestamp": {"type": "string", "format": "date-time"},
        "repository": {"type": "string", "pattern": "^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$", "maxLength": 200},
        "revision": {"type": "string", "pattern": "^[0-9a-f]{7,40}$"},
        "event_type": {"enum": ["build", "test", "install", "deploy", "runtime", "recovery", "agent_task", "release", "benchmark"]},
        "task_or_test": {"type": "string", "minLength": 1, "maxLength": 160, "pattern": "^[A-Za-z0-9_.:/() -]+$"},
        "result": {"enum": ["pass", "fail", "partial", "cancelled", "skipped"]},
        "duration_ms": {"type": ["integer", "null"], "minimum": 0},
        "environment": {"type": "string", "minLength": 1, "maxLength": 80, "pattern": "^[A-Za-z0-9_.-]+$"},
        "attempt": {"type": "integer", "minimum": 0},
        "human_interventions": {"type": "integer", "minimum": 0},
        "review_corrections": {"type": "integer", "minimum": 0},
        "ci_retries": {"type": "integer", "minimum": 0},
        "metadata": {"type": "object", "additionalProperties": {"type": ["string", "number", "integer", "boolean", "null"]},
                     "propertyNames": {"pattern": "^[A-Za-z0-9_.-]{1,80}$"}, "maxProperties": 40},
    },
}

SECRET_PATTERN = re.compile(
    r"(?i)(?:bearer\s+[a-z0-9._~+/-]{12,}|gh[pousr]_[a-z0-9]{20,}|"
    r"(?:password|token|secret|api[_-]?key|authorization|cookie)[\"']?\s*[:=]\s*[\"']?[^\s,\"']{8,}|"
    r"-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----)"
)
FORMAT_CHECKER = FormatChecker()


@FORMAT_CHECKER.checks("date-time")
def utc_datetime(value: object) -> bool:
    if not isinstance(value, str) or not re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z", value):
        return False
    try:
        dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return False
    return True


VALIDATOR = Draft202012Validator(SCHEMA, format_checker=FORMAT_CHECKER)


def git_output(root: pathlib.Path, *args: str) -> str:
    return subprocess.run(["git", *args], cwd=root, check=True, capture_output=True, text=True).stdout.strip()


def repository_slug(root: pathlib.Path) -> str:
    remote = git_output(root, "remote", "get-url", "origin")
    match = re.search(r"(?:github\.com[:/])([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+?)(?:\.git)?/?$", remote)
    if not match:
        raise ValueError("origin must be a GitHub owner/repository URL")
    return match.group(1)


def record_errors(record: object, root: pathlib.Path, slug: str) -> list[str]:
    errors = [f"schema {'.'.join(map(str, error.path)) or '<root>'}: {error.message}"
              for error in sorted(VALIDATOR.iter_errors(record), key=lambda error: tuple(map(str, error.path)))]
    if SECRET_PATTERN.search(json.dumps(record, ensure_ascii=False)):
        errors.append("secret-pattern match")
    if isinstance(record, dict):
        if record.get("repository") != slug:
            errors.append("repository does not match origin")
        revision = record.get("revision")
        if isinstance(revision, str) and re.fullmatch(r"[0-9a-f]{7,40}", revision):
            if subprocess.run(["git", "cat-file", "-e", f"{revision}^{{commit}}"], cwd=root,
                              capture_output=True).returncode:
                errors.append("revision does not exist as a local commit")
    return errors


def check(root: pathlib.Path) -> list[str]:
    slug = repository_slug(root)
    evidence = root / "research/evidence"
    if not evidence.is_dir():
        return ["research/evidence directory is missing"]
    allowlist_path = evidence / "known-invalid.json"
    allowlist = json.loads(allowlist_path.read_text(encoding="utf-8")) if allowlist_path.exists() else []
    if not isinstance(allowlist, list):
        return ["known-invalid.json must be an array"]
    allowed: dict[tuple[str, int], dict] = {}
    problems = []
    for entry in allowlist:
        if not isinstance(entry, dict) or set(entry) != {"file", "line", "sha256", "reason"} or \
                not isinstance(entry["file"], str) or not re.fullmatch(r"\d{4}-\d{2}\.jsonl", entry["file"]) or \
                not isinstance(entry["line"], int) or isinstance(entry["line"], bool) or entry["line"] < 1 or \
                not isinstance(entry["sha256"], str) or not re.fullmatch(r"[0-9a-f]{64}", entry["sha256"]) or \
                not isinstance(entry["reason"], str) or not entry["reason"].strip():
            problems.append("invalid known-invalid.json entry")
            continue
        key = (entry["file"], entry["line"])
        if key in allowed:
            problems.append(f"duplicate allowlist entry: {key}")
        allowed[key] = entry
    seen = set()
    for path in sorted(evidence.glob("*.jsonl")):
        for line_number, raw in enumerate(path.read_bytes().splitlines(keepends=True), 1):
            key = (path.name, line_number)
            if key in allowed:
                # D1: Preserve historical bytes. Line moves cost a manual re-pin;
                # remove the exception only after a linked correction record.
                seen.add(key)
                if hashlib.sha256(raw).hexdigest() != allowed[key]["sha256"]:
                    problems.append(f"{path.name}:{line_number}: allowlisted line hash mismatch")
                continue
            try:
                record = json.loads(raw)
            except (UnicodeDecodeError, json.JSONDecodeError) as error:
                problems.append(f"{path.name}:{line_number}: invalid JSON: {error}")
                continue
            for reason in record_errors(record, root, slug):
                problems.append(f"{path.name}:{line_number}: {reason}")
    for file, line in sorted(allowed.keys() - seen):
        problems.append(f"{file}:{line}: allowlisted line is missing")
    return problems


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=pathlib.Path, default=pathlib.Path.cwd())
    args = parser.parse_args()
    try:
        problems = check(args.root.resolve())
    except (OSError, ValueError, subprocess.CalledProcessError, json.JSONDecodeError) as error:
        problems = [str(error)]
    for problem in problems:
        print(problem, file=sys.stderr)
    if problems:
        return 1
    print("research evidence: OK")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
