#!/usr/bin/env python3
"""Run a command and append one schema-conformant research evidence record."""

from __future__ import annotations

import argparse
import datetime as dt
import importlib.util
import json
import os
import pathlib
import platform
import re
import subprocess
import sys
import time


ROOT = pathlib.Path(__file__).resolve().parents[2]
CHECKER_PATH = pathlib.Path(__file__).with_name("check-research-evidence.py")
EVENT_TYPES = ("build", "test", "install", "deploy", "runtime", "recovery", "agent_task", "release", "benchmark")
SCALAR_METADATA = ("pass_count", "fail_count", "skip_count", "warning_count", "cpu_time_ms",
                   "peak_memory_mib", "storage_bytes", "artifact_digest", "failure_stage", "recovery_result")


def checker():
    spec = importlib.util.spec_from_file_location("research_checker", CHECKER_PATH)
    if spec is None or spec.loader is None:
        raise RuntimeError("research validator is missing")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def safe_label(value: str, *, task: bool = False) -> str:
    # D2: Normalize only new labels; never rewrite legacy evidence. Punctuation
    # is lost; a future schema version can widen the charset if needed.
    value = value.replace("#", "").replace(":", "-").replace("/", "-")
    if not task:
        value = re.sub(r"[^A-Za-z0-9_.-]", "-", value)
    else:
        value = re.sub(r"[^A-Za-z0-9_.:/() -]", "-", value)
    if not value or len(value) > (160 if task else 80):
        raise ValueError("normalized task/environment label is empty or too long")
    return value


def environment_label() -> str:
    if os.environ.get("GITHUB_ACTIONS") == "true":
        return safe_label("github-actions-" + os.environ.get("RUNNER_OS", "unknown") + "-" +
                          os.environ.get("RUNNER_ARCH", "unknown"))
    return safe_label("local-" + platform.system().lower() + "-" + platform.machine().lower())


def append_record(record: dict[str, object], path: pathlib.Path, root: pathlib.Path = ROOT) -> None:
    validation = checker()
    errors = validation.record_errors(record, root, validation.repository_slug(root))
    if errors:
        raise ValueError("; ".join(errors))
    path.parent.mkdir(parents=True, exist_ok=True)
    line = (json.dumps(record, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8")
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
    try:
        remaining = memoryview(line)
        while remaining:
            remaining = remaining[os.write(fd, remaining):]
    finally:
        os.close(fd)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--task", required=True, help="stable test/workload label")
    parser.add_argument("--event-type", required=True, choices=(*EVENT_TYPES, "verification"))
    parser.add_argument("--environment", help="environment label; normalized for the public schema")
    parser.add_argument("--attempt", type=int, default=1)
    parser.add_argument("--human-interventions", type=int)
    parser.add_argument("--review-corrections", type=int)
    parser.add_argument("--ci-retries", type=int, default=0)
    for name in SCALAR_METADATA[:7]:
        parser.add_argument("--" + name.replace("_", "-"), type=int)
    for name in SCALAR_METADATA[7:]:
        parser.add_argument("--" + name.replace("_", "-"))
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    if args.command and args.command[0] == "--":
        args.command = args.command[1:]
    if not args.command:
        parser.error("provide a command after --")
    # D5: Accept LDAPium's legacy CLI name while v1 stores test. The original
    # label is lost; callers can use test directly to avoid the alias.
    if args.event_type == "verification":
        args.event_type = "test"
    if args.event_type == "agent_task" and (args.human_interventions is None or args.review_corrections is None):
        parser.error("agent_task requires measured --human-interventions and --review-corrections")
    for name in ("attempt", "human_interventions", "review_corrections", "ci_retries", *SCALAR_METADATA[:7]):
        value = getattr(args, name)
        if value is not None and value < (1 if name == "attempt" else 0):
            parser.error(f"--{name.replace('_', '-')} must be non-negative (attempt starts at 1)")
    return args


def main() -> int:
    args = parse_args()
    validation = checker()
    try:
        slug = validation.repository_slug(ROOT)
        revision = validation.git_output(ROOT, "rev-parse", "HEAD")
        task = safe_label(args.task, task=True)
        environment = safe_label(args.environment) if args.environment else environment_label()
        if validation.SECRET_PATTERN.search(task) or validation.SECRET_PATTERN.search(environment):
            raise ValueError("task/environment contains a secret pattern")
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        print(f"research evidence setup failed: {error}", file=sys.stderr)
        return 2
    start = time.monotonic_ns()
    timestamp = dt.datetime.now(dt.timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")
    try:
        completed = subprocess.run(args.command, check=False)
        exit_code = completed.returncode
        result = "pass" if exit_code == 0 else "fail"
    except KeyboardInterrupt:
        exit_code = 130
        result = "cancelled"
    except OSError as error:
        print(f"recorded command could not start: {error}", file=sys.stderr)
        exit_code = 127
        result = "fail"
    duration_ms = (time.monotonic_ns() - start) // 1_000_000
    metadata: dict[str, object] = {
        "exit_code": exit_code,
        "working_tree_dirty": bool(validation.git_output(ROOT, "status", "--porcelain")),
    }
    for name in SCALAR_METADATA:
        value = getattr(args, name)
        if value is not None:
            # D3: Fixed scalar keys prevent raw log structures entering public
            # records. Nested metrics cost a schema update or safe flattening.
            metadata[name] = validation.SECRET_PATTERN.sub("[REDACTED]", value) if isinstance(value, str) else value
    record = {
        "schema_version": "1.0", "timestamp": timestamp, "repository": slug, "revision": revision,
        "event_type": args.event_type, "task_or_test": task, "result": result,
        "duration_ms": duration_ms, "environment": environment, "attempt": args.attempt,
        "human_interventions": args.human_interventions or 0,
        "review_corrections": args.review_corrections or 0,
        "ci_retries": args.ci_retries, "metadata": metadata,
    }
    path = pathlib.Path(os.environ.get("RESEARCH_EVIDENCE_DIR", ROOT / "research/evidence")) / f"{timestamp[:7]}.jsonl"
    try:
        append_record(record, path)
    except (OSError, ValueError) as error:
        print(f"research evidence rejected: {error}", file=sys.stderr)
        return 2
    print(f"research evidence: {path} ({result}, {duration_ms} ms)", file=sys.stderr)
    return exit_code


if __name__ == "__main__":
    raise SystemExit(main())
