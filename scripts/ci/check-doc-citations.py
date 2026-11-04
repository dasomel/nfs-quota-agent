#!/usr/bin/env python3
"""Scan markdown files for code/file citations and PR references.

Verifies:
1. Citations of the form `path:line` or `path:start-end`:
   - The cited file exists in the current tree (relative to repo root or doc dir).
   - The specified line or line range is within the file's line count (1 <= start <= end <= total).
   - The cited line(s) are not all blank (a range that merely starts or ends on a blank line is fine).
   - Comma lists such as `path:36,104-111` are checked part by part.
2. References of the form `PR #N`:
   - `gh pr view N --json state` reports state == "MERGED".
   - PR states are cached to avoid duplicate network queries.

Optionally (`--since <git-rev>`), warns when a line-numbered citation may have drifted
since <rev>: a `git diff -U0` hunk touches the cited range or changes the line count at or
above its last line. Warnings do not affect the exit code unless `--strict-drift` is given.

Exits 1 on any failure, printing every defect with the doc file and line number.
Exits 2 on usage/environment errors (missing doc, unknown rev, not a git checkout).
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
    r"(?<!http://)(?<!https://)(?<![\w/.-])([a-zA-Z0-9_./-]+\.[a-zA-Z0-9_-]+|[A-Z][A-Z0-9_]+):(\d+)(?:-(\d+))?((?:,\d+(?:-\d+)?)*)"
)
HUNK_RE = re.compile(r"^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@")
PR_REF_RE = re.compile(r"\bPR #(\d+)\b")


def find_repo_root(start_path: Path | None = None) -> Path:
    """Determine git repository root or fallback to script parent's grandparent."""
    cur = (start_path or Path.cwd()).resolve()
    for p in [cur, *cur.parents]:
        if (p / ".git").exists():
            return p
    return Path(__file__).resolve().parent.parent.parent


def _repo_relative(path: Path, repo_root: Path) -> str:
    """Repo-relative POSIX path for git, or the path unchanged when outside the repo."""
    try:
        return path.resolve().relative_to(repo_root.resolve()).as_posix()
    except ValueError:
        return path.as_posix()


def _git(repo_root: Path, *args: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        ["git", *args],
        cwd=str(repo_root),
        capture_output=True,
        text=True,
        check=False,
    )


def validate_rev(rev: str, repo_root: Path) -> str | None:
    """Return an error message if rev cannot be used for drift checks, else None."""
    try:
        if _git(repo_root, "rev-parse", "--git-dir").returncode != 0:
            return f"--since requires a git checkout (not a git repository: {repo_root})"
        if _git(repo_root, "rev-parse", "--verify", "--quiet", f"{rev}^{{commit}}").returncode != 0:
            return (
                f"unknown git revision '{rev}' for --since "
                "(shallow clone? use fetch-depth: 0)"
            )
    except OSError as exc:
        return f"unable to run git for --since: {exc}"
    return None


def hunk_drifts(old_start: int, old_count: int, new_count: int, start: int, end: int) -> bool:
    """True if a -U0 hunk touches the cited old-file range or shifts it via a line-count change."""
    if old_count == 0:
        # Pure insertion after old line old_start: shifts or splits the range if above its last line.
        return old_start < end
    if old_start > end:
        return False
    return old_start + old_count - 1 >= start or old_count != new_count


DELETED = "file deleted"


def check_drift(
    citations: list[tuple[str, int, str, int, int]],
    rev: str,
    repo_root: Path,
) -> list[str]:
    """Warn for citations whose cited lines may have moved or changed since rev."""
    warnings: list[str] = []
    hunk_cache: dict[str, list[tuple[int, int, int, str]] | None] = {}
    for display_doc, lineno, rel, start, end in citations:
        if rel not in hunk_cache:
            if _git(repo_root, "cat-file", "-e", f"{rev}:{rel}").returncode != 0:
                hunk_cache[rel] = None  # absent at rev: no baseline to drift from
            elif not (repo_root / rel).is_file():
                hunk_cache[rel] = [(0, 0, 0, DELETED)]
            else:
                diff = _git(
                    repo_root, "diff", "-U0", "--no-color", "--no-ext-diff", rev, "--", rel
                )
                parsed = []
                for text in diff.stdout.splitlines():
                    m = HUNK_RE.match(text)
                    if m:
                        parsed.append(
                            (
                                int(m.group(1)),
                                int(m.group(2) if m.group(2) is not None else 1),
                                int(m.group(4) if m.group(4) is not None else 1),
                                text[: text.index("@@", 2) + 2],
                            )
                        )
                hunk_cache[rel] = parsed
        hunks = hunk_cache[rel]
        if not hunks:
            continue
        cited = f"{rel}:{start}" if start == end else f"{rel}:{start}-{end}"
        if hunks[0][3] == DELETED:
            warnings.append(
                f"{display_doc}:{lineno}: {cited} may have drifted since {rev} ({DELETED})"
            )
            continue
        for old_start, old_count, new_count, header in hunks:
            if hunk_drifts(old_start, old_count, new_count, start, end):
                warnings.append(
                    f"{display_doc}:{lineno}: {cited} may have drifted since {rev} (hunk {header})"
                )
                break
    return warnings


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
    lines_cache: dict[Path, list[str]] | None = None,
    citations_out: list[tuple[str, int, str, int, int]] | None = None,
) -> tuple[list[str], int]:
    """Scan a markdown document and verify all file line citations and PR references.

    Returns:
        tuple of (list of failure strings, total count of citations checked)

    If citations_out is given, every line-numbered citation part is appended to it as
    (display_doc, doc_lineno, repo_relative_path, start, end) for the drift check.
    """
    failures: list[str] = []
    total_citations = 0

    if pr_cache is None:
        pr_cache = {}
    if lines_cache is None:
        lines_cache = {}

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
            parts = [
                (int(path_match.group(2)), int(path_match.group(3) or path_match.group(2)))
            ]
            for extra in filter(None, path_match.group(4).split(",")):
                lo, _, hi = extra.partition("-")
                parts.append((int(lo), int(hi or lo)))

            # Resolve file path against repo_root first, then relative to doc directory
            target = repo_root / path_str
            if not target.exists():
                target = doc_path.parent / path_str

            if citations_out is not None:
                rel = _repo_relative(
                    target if target.exists() else repo_root / path_str, repo_root
                )
                citations_out.extend((str(display_doc), lineno, rel, s, e) for s, e in parts)

            if not target.exists() or not target.is_file():
                failures.append(
                    f"{display_doc}:{lineno}: referenced file '{path_str}' does not exist on current tree"
                )
                continue

            target_resolved = target.resolve()
            if target_resolved not in lines_cache:
                try:
                    lines_cache[target_resolved] = target_resolved.read_text(
                        encoding="utf-8"
                    ).splitlines()
                except OSError as exc:
                    failures.append(
                        f"{display_doc}:{lineno}: unable to read referenced file '{path_str}': {exc}"
                    )
                    continue

            file_lines = lines_cache[target_resolved]
            total_lines = len(file_lines)

            for start, end in parts:
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
                elif all(not text.strip() for text in file_lines[start - 1 : end]):
                    cited = f"{start}" if start == end else f"{start}-{end}"
                    failures.append(
                        f"{display_doc}:{lineno}: referenced file '{path_str}' cited line(s) {cited} are blank"
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
    parser.add_argument(
        "--since",
        type=str,
        default=None,
        metavar="REV",
        help="Warn about line-numbered citations that may have drifted since this git revision.",
    )
    parser.add_argument(
        "--strict-drift",
        action="store_true",
        help="Exit 1 when --since reports drift warnings.",
    )

    args = parser.parse_args(argv)

    if args.strict_drift and args.since is None:
        parser.error("--strict-drift requires --since")

    repo_root = args.root.resolve() if args.root else find_repo_root()

    if args.since is not None:
        err = validate_rev(args.since, repo_root)
        if err:
            print(f"Error: {err}", file=sys.stderr)
            return 2

    pr_getter = get_pr_state_gh
    if args.mock_pr_states:
        mock_data = json.loads(args.mock_pr_states)
        mock_map = {int(k): str(v) for k, v in mock_data.items()}

        def mock_getter(num: int) -> str:
            return mock_map.get(num, "NOT_FOUND")

        pr_getter = mock_getter

    pr_cache: dict[int, str] = {}
    lines_cache: dict[Path, list[str]] = {}
    citations: list[tuple[str, int, str, int, int]] = []
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
            lines_cache=lines_cache,
            citations_out=citations if args.since is not None else None,
        )
        all_failures.extend(failures)
        total_checked += count

    drift_warnings: list[str] = []
    if args.since is not None:
        drift_warnings = check_drift(citations, args.since, repo_root)
        for warn in drift_warnings:
            print(f"warning: {warn}", file=sys.stderr)

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
    if args.since is not None:
        print(f"{len(drift_warnings)} citation(s) may have drifted since {args.since}.")
        if drift_warnings and args.strict_drift:
            return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
