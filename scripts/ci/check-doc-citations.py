#!/usr/bin/env python3
"""Scan markdown files for code/file citations and PR references.

Verifies:
1. Citations of the form `path:line` or `path:start-end`:
   - The cited file exists in the current tree (relative to repo root or doc dir).
   - The specified line or line range is within the file's line count (1 <= start <= end <= total).
2. References of the form `PR #N`:
   - `gh pr view N --json state` reports state == "MERGED".
   - PR states are cached to avoid duplicate network queries.

Exits 1 on any failure, printing every defect with the doc file and line number.
"""

from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys
from pathlib import Path

# Match file:line or file:start-end citations.
# Preceding check avoids matching URLs (e.g. http://... or https://...).
# The path matches either files with extensions or all-caps file names (e.g. LICENSE, Makefile).
CITATION_PATH_RE = re.compile(
    r"(?<!http://)(?<!https://)(?<![\w/.-])([a-zA-Z0-9_./-]+\.[a-zA-Z0-9_-]+|[A-Z][A-Z0-9_]+):(\d+)(?:-(\d+))?"
)
PR_REF_RE = re.compile(r"\bPR #(\d+)\b")


def find_repo_root(start_path: Path | None = None) -> Path:
    """Determine git repository root or fallback to script parent's grandparent."""
    cur = (start_path or Path.cwd()).resolve()
    for p in [cur, *cur.parents]:
        if (p / ".git").exists():
            return p
    return Path(__file__).resolve().parent.parent.parent


def get_pr_state_gh(pr_num: int) -> str:
    """Fetch PR state via GitHub CLI."""
    try:
        proc = subprocess.run(
            ["gh", "pr", "view", str(pr_num), "--json", "state"],
            capture_output=True,
            text=True,
            check=False,
        )
        if proc.returncode != 0:
            return f"ERROR: gh pr view failed (exit code {proc.returncode}): {proc.stderr.strip()}"
        data = json.loads(proc.stdout)
        return str(data.get("state", "UNKNOWN"))
    except Exception as exc:  # pragma: no cover
        return f"ERROR: {exc}"


def check_citations(
    doc_path: Path,
    repo_root: Path,
    pr_state_getter=get_pr_state_gh,
    pr_cache: dict[int, str] | None = None,
    line_count_cache: dict[Path, int] | None = None,
) -> tuple[list[str], int]:
    """Scan a markdown document and verify all file line citations and PR references.

    Returns:
        tuple of (list of failure strings, total count of citations checked)
    """
    failures: list[str] = []
    total_citations = 0

    if pr_cache is None:
        pr_cache = {}
    if line_count_cache is None:
        line_count_cache = {}

    doc_path = doc_path.resolve()
    try:
        content = doc_path.read_text(encoding="utf-8")
    except OSError as exc:
        return [f"{doc_path}:0: unable to read file: {exc}"], 0

    try:
        display_doc = doc_path.relative_to(repo_root)
    except ValueError:
        display_doc = doc_path

    for lineno, line in enumerate(content.splitlines(), 1):
        # 1. Check PR #N citations
        for pr_match in PR_REF_RE.finditer(line):
            total_citations += 1
            pr_num = int(pr_match.group(1))
            if pr_num not in pr_cache:
                pr_cache[pr_num] = pr_state_getter(pr_num)

            state = pr_cache[pr_num]
            if state != "MERGED":
                failures.append(
                    f"{display_doc}:{lineno}: PR #{pr_num} state is {state} (expected MERGED)"
                )

        # 2. Check path:line or path:a-b citations
        for path_match in CITATION_PATH_RE.finditer(line):
            total_citations += 1
            path_str = path_match.group(1)
            start_str = path_match.group(2)
            end_str = path_match.group(3)

            start = int(start_str)
            end = int(end_str) if end_str is not None else start

            # Resolve file path against repo_root first, then relative to doc directory
            target = repo_root / path_str
            if not target.exists():
                target = doc_path.parent / path_str

            if not target.exists() or not target.is_file():
                failures.append(
                    f"{display_doc}:{lineno}: referenced file '{path_str}' does not exist on current tree"
                )
                continue

            target_resolved = target.resolve()
            if target_resolved not in line_count_cache:
                try:
                    lines = target_resolved.read_text(encoding="utf-8").splitlines()
                    line_count_cache[target_resolved] = len(lines)
                except OSError as exc:
                    failures.append(
                        f"{display_doc}:{lineno}: unable to read referenced file '{path_str}': {exc}"
                    )
                    continue

            total_lines = line_count_cache[target_resolved]

            if start < 1:
                failures.append(
                    f"{display_doc}:{lineno}: referenced file '{path_str}' has invalid start line {start} (< 1)"
                )
            elif start > end:
                failures.append(
                    f"{display_doc}:{lineno}: referenced file '{path_str}' has inverted range {start}-{end}"
                )
            elif end > total_lines:
                failures.append(
                    f"{display_doc}:{lineno}: referenced file '{path_str}' line range {start}-{end} exceeds total line count ({total_lines})"
                )

    return failures, total_citations


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Verify doc citations (path:lines and PR #N) in markdown files."
    )
    parser.add_argument(
        "files",
        nargs="+",
        type=Path,
        help="Markdown file(s) to verify citations in.",
    )
    parser.add_argument(
        "--root",
        type=Path,
        default=None,
        help="Repository root directory (defaults to git root).",
    )
    parser.add_argument(
        "--mock-pr-states",
        type=str,
        default=None,
        help="JSON string mapping PR number to state string (for testing without gh).",
    )

    args = parser.parse_args(argv)

    repo_root = args.root.resolve() if args.root else find_repo_root()

    pr_getter = get_pr_state_gh
    if args.mock_pr_states:
        mock_data = json.loads(args.mock_pr_states)
        mock_map = {int(k): str(v) for k, v in mock_data.items()}

        def mock_getter(num: int) -> str:
            return mock_map.get(num, "NOT_FOUND")

        pr_getter = mock_getter

    pr_cache: dict[int, str] = {}
    line_count_cache: dict[Path, int] = {}
    all_failures: list[str] = []
    total_checked = 0

    for file_arg in args.files:
        path = file_arg.resolve()
        if not path.exists():
            print(f"Error: markdown file '{file_arg}' not found", file=sys.stderr)
            return 2

        failures, count = check_citations(
            doc_path=path,
            repo_root=repo_root,
            pr_state_getter=pr_getter,
            pr_cache=pr_cache,
            line_count_cache=line_count_cache,
        )
        all_failures.extend(failures)
        total_checked += count

    if all_failures:
        print(
            f"Found {len(all_failures)} citation failure(s) out of {total_checked} checked citation(s):",
            file=sys.stderr,
        )
        for fail in all_failures:
            print(f"  {fail}", file=sys.stderr)
        return 1

    print(
        f"Verified {total_checked} citation(s) across {len(args.files)} file(s): all valid."
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
