#!/usr/bin/env python3
"""Backfill research evidence v1 records from real GitHub Actions run history."""

from __future__ import annotations

import argparse
import collections
import datetime as dt
import importlib.util
import json
import pathlib
import re
import subprocess
import sys


CHECKER_PATH = pathlib.Path(__file__).with_name("check-research-evidence.py")
RUN_FIELDS = ("id", "name", "path", "head_sha", "head_branch", "event", "status", "conclusion",
              "run_attempt", "run_started_at", "updated_at")
RESULTS = {"success": "pass", "failure": "fail", "timed_out": "fail", "startup_failure": "fail",
           "cancelled": "cancelled", "skipped": "skipped", "neutral": "skipped"}


def checker():
    spec = importlib.util.spec_from_file_location("research_checker", CHECKER_PATH)
    if spec is None or spec.loader is None:
        raise RuntimeError("research validator is missing")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def fetch_runs(slug: str) -> list[dict]:
    # D1: One gh call per page of 100 runs, no per-job calls (rate limits). The status=completed
    # query filter is NOT sent: GitHub caps filtered run listings at 1000 results, which would
    # silently truncate history. Completed runs are selected locally; cost is a few extra pages.
    # --jq keeps only whitelisted scalar fields, so actors/titles never enter this process.
    fields = ",".join(f"{name}:.{name}" for name in RUN_FIELDS)
    out = subprocess.run(["gh", "api", "--paginate", f"repos/{slug}/actions/runs?per_page=100",
                          "--jq", ".workflow_runs[] | {" + fields + "}"],
                         check=True, capture_output=True, text=True).stdout
    return [json.loads(line) for line in out.splitlines() if line.strip()]


def parse_time(value: object) -> dt.datetime | None:
    if not isinstance(value, str):
        return None
    try:
        return dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None


def event_type(run: dict) -> str:
    # D3: Heuristic from workflow file/name only; unknown workflows count as test. Misclassified
    # workflows cost label accuracy, not result accuracy; refine the keywords as needed.
    text = f"{run.get('path') or ''} {run.get('name') or ''}".lower()
    if "release" in text:
        return "release"
    if any(word in text for word in ("deploy", "pages", "publish")):
        return "deploy"
    if any(word in text for word in ("build", "image", "docker", "package")):
        return "build"
    return "test"


def task_label(run: dict) -> str:
    name = re.sub(r"[^A-Za-z0-9_.:/() -]", "-", str(run.get("name") or pathlib.PurePosixPath(run.get("path") or "").stem))
    return "gha:" + (name or "unknown")[:155]


def build_record(run: dict, slug: str, default_branch: str, backfilled_at: str) -> dict | None:
    # D3: duration_ms is run_started_at -> updated_at (queue time excluded, re-run wait included).
    result = RESULTS.get(run.get("conclusion"))
    started = parse_time(run.get("run_started_at"))
    attempt = run.get("run_attempt")
    if result is None or started is None or not isinstance(attempt, int) or attempt < 1:
        return None
    updated = parse_time(run.get("updated_at"))
    duration = int((updated - started).total_seconds() * 1000) if updated else None
    if duration is not None and duration < 0:
        duration = None
    return {
        "schema_version": "1.0", "timestamp": run["run_started_at"], "repository": slug,
        "revision": run["head_sha"], "event_type": event_type(run), "task_or_test": task_label(run),
        "result": result, "duration_ms": duration, "environment": "github-actions", "attempt": attempt,
        "human_interventions": 0, "review_corrections": 0, "ci_retries": attempt - 1,
        # D4: Scalars only; no titles, commit messages or actor names reach the public record.
        "metadata": {"source": "github-actions-backfill", "gh_run_id": run["id"], "gh_run_attempt": attempt,
                     "gh_event": run.get("event"), "workflow_path": run.get("path"),
                     "head_branch_is_default": run.get("head_branch") == default_branch,
                     "duration_source": "run_started_at_to_updated_at", "backfilled_at": backfilled_at,
                     # D8: v1 requires integer counters. Zero is a schema placeholder, not an
                     # observation; exclude these counters from analysis unless measured elsewhere.
                     # Cost: consumers must filter these flags; escape: a future nullable schema.
                     "human_interventions_observed": False, "review_corrections_observed": False,
                     "duration_semantics": "workflow_update_interval"},
    }


def existing_keys(*directories: pathlib.Path) -> set[tuple[int, int]]:
    # D5: A run is already backfilled when any evidence file holds its (run id, attempt).
    keys = set()
    for directory in directories:
        for path in directory.glob("*.jsonl"):
            for line in path.read_text(encoding="utf-8").splitlines():
                try:
                    meta = json.loads(line).get("metadata") or {}
                except (json.JSONDecodeError, AttributeError):
                    continue
                if "gh_run_id" in meta and "gh_run_attempt" in meta:
                    keys.add((meta["gh_run_id"], meta["gh_run_attempt"]))
    return keys


def on_main(root: pathlib.Path, sha: str, cache: dict[str, bool]) -> bool:
    # D2: Only commits reachable from origin/main, so the validator's local commit check passes in CI.
    if sha not in cache:
        result = subprocess.run(["git", "merge-base", "--is-ancestor", sha, "origin/main"], cwd=root,
                                capture_output=True)
        # Missing PR commits are expected; a missing main ref is checked before conversion.
        cache[sha] = result.returncode == 0
    return cache[sha]


def convert(runs: list[dict], root: pathlib.Path, slug: str, since: dt.date | None, known: set,
            default_branch: str = "main", backfilled_at: str | None = None, exclude_paths: tuple[str, ...] = ()):
    backfilled_at = backfilled_at or dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    stats = collections.Counter()
    # D9: Fail on a missing baseline instead of reporting a successful empty export.
    # Cost: one local Git call; escape: fetch origin/main and retry.
    subprocess.run(["git", "rev-parse", "--verify", "origin/main^{commit}"], cwd=root,
                   check=True, capture_output=True)
    known = set(known)
    cache: dict[str, bool] = {}
    records = []
    for run in runs:
        if run.get("status") != "completed":
            continue
        stats["fetched"] += 1
        started = parse_time(run.get("run_started_at"))
        if since and started and started.date() < since:
            stats["excluded_since"] += 1
            continue
        # D11: Workflow files named here are skipped so a scheduled backfill does not record its own
        # runs (each week's run would otherwise add a record about the previous run). Cost: those
        # runs are absent from the dataset by design; the count is reported as excluded_workflow.
        if run.get("path") in exclude_paths:
            stats["excluded_workflow"] += 1
            continue
        record = build_record(run, slug, default_branch, backfilled_at)
        if record is None:
            stats["excluded_other"] += 1
        elif (run["id"], run["run_attempt"]) in known:
            stats["skipped_existing"] += 1
        elif not on_main(root, run["head_sha"], cache):
            stats["excluded_not_on_main"] += 1
            stats["not_on_main_pull_request" if run.get("event") == "pull_request" else "not_on_main_other_event"] += 1
        else:
            records.append(record)
            known.add((run["id"], run["run_attempt"]))
    records.sort(key=lambda item: item["timestamp"])
    stats["included"] = len(records)
    return records, stats


def write_records(records: list[dict], output_dir: pathlib.Path) -> None:
    # D6: Append-only, grouped by timestamp month; existing lines are never rewritten.
    output_dir.mkdir(parents=True, exist_ok=True)
    months = collections.defaultdict(list)
    for record in records:
        months[record["timestamp"][:7]].append(record)
    # D10: Reject an unterminated historical line before any append. Adding a newline would
    # change its allowlist hash; cost: explicit repair needed, escape: a separate output directory.
    for month in months:
        path = output_dir / f"{month}.jsonl"
        if path.exists() and path.stat().st_size:
            with path.open("rb") as stream:
                stream.seek(-1, 2)
                if stream.read(1) != b"\n":
                    raise ValueError(f"{path}: existing evidence must end with a newline")
    for month, items in sorted(months.items()):
        with (output_dir / f"{month}.jsonl").open("a", encoding="utf-8") as stream:
            for item in items:
                stream.write(json.dumps(item, sort_keys=True, separators=(",", ":")) + "\n")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo-root", type=pathlib.Path, default=pathlib.Path.cwd())
    parser.add_argument("--output-dir", type=pathlib.Path, help="default: <repo-root>/research/evidence")
    parser.add_argument("--since", type=dt.date.fromisoformat, help="only runs started on/after YYYY-MM-DD")
    parser.add_argument("--exclude-path", action="append", default=[], metavar="WORKFLOW_PATH",
                        help="skip runs of this workflow file, e.g. .github/workflows/x.yml (repeatable)")
    parser.add_argument("--dry-run", action="store_true", help="print counts only")
    args = parser.parse_args()
    root = args.repo_root.resolve()
    output_dir = (args.output_dir or root / "research/evidence").resolve()
    validation = checker()
    try:
        slug = validation.repository_slug(root)
        runs = fetch_runs(slug)
        known = existing_keys(*(path for path in (root / "research/evidence", output_dir) if path.is_dir()))
        records, stats = convert(runs, root, slug, args.since, known, exclude_paths=tuple(args.exclude_path))
        # D7: Every record is validated before anything is written; one invalid record aborts the run.
        for record in records:
            errors = validation.record_errors(record, root, slug)
            if errors:
                raise ValueError(f"run {record['metadata']['gh_run_id']}: " + "; ".join(errors))
        if not args.dry_run:
            write_records(records, output_dir)
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        detail = getattr(error, "stderr", "") or ""
        print(f"backfill failed: {error} {detail}".strip(), file=sys.stderr)
        return 2
    per_month = collections.Counter(record["timestamp"][:7] for record in records)
    print(json.dumps({"repository": slug, **stats, "per_month": dict(sorted(per_month.items()))}, sort_keys=True))
    if not args.dry_run:
        print(f"backfill: wrote {len(records)} records to {output_dir}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
