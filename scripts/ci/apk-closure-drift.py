#!/usr/bin/env python3
"""APK closure drift report generator.

Compares two Alpine apk closure manifests (baseline and current) generated
by `apk info -v | sort` (saved at /licenses/os-packages-manifest.txt in the container image).
Outputs a markdown table of added, removed, and version-changed packages and a JSON summary.

Decision record D5 (#26): apk packages resolve from the live index at build time
and are recorded, not pinned. This script provides visibility into package drift
between builds without blocking CI by default.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
from pathlib import Path
from typing import Any, Dict, List, Optional

# Regex to parse Alpine package lines from `apk info -v`:
# Format is `<pkg-name>-<pkg-version>-<pkg-release>`.
# In Alpine Linux naming conventions, package versions start with a digit
# following a hyphen: e.g. "alpine-baselayout-3.7.2-r1" -> name "alpine-baselayout", version "3.7.2-r1".
PKG_RE = re.compile(r"^(.+?)-(\d.*)$")


def parse_manifest(content: str) -> Dict[str, str]:
    """Parse manifest text into a mapping of package name -> version."""
    packages: Dict[str, str] = {}
    for raw_line in content.splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#"):
            continue
        match = PKG_RE.match(line)
        if match:
            name, version = match.groups()
            packages[name] = version
        else:
            # Fallback for non-standard package lines
            parts = line.rsplit("-", 2)
            if len(parts) == 3:
                packages[parts[0]] = f"{parts[1]}-{parts[2]}"
            elif len(parts) == 2:
                packages[parts[0]] = parts[1]
            else:
                packages[line] = "unknown"
    return packages


def compare_manifests(
    baseline: Dict[str, str], current: Dict[str, str]
) -> Dict[str, Any]:
    """Compare baseline and current package dictionaries."""
    added = {pkg: current[pkg] for pkg in sorted(current) if pkg not in baseline}
    removed = {pkg: baseline[pkg] for pkg in sorted(baseline) if pkg not in current}
    changed = {
        pkg: {"baseline": baseline[pkg], "current": current[pkg]}
        for pkg in sorted(baseline)
        if pkg in current and baseline[pkg] != current[pkg]
    }
    identical = {
        pkg: baseline[pkg]
        for pkg in sorted(baseline)
        if pkg in current and baseline[pkg] == current[pkg]
    }

    return {
        "added": added,
        "removed": removed,
        "changed": changed,
        "identical": identical,
    }


def format_markdown_table(comparison: Dict[str, Any], total_baseline: int, total_current: int) -> str:
    """Format comparison result as a markdown table or status message."""
    added = comparison["added"]
    removed = comparison["removed"]
    changed = comparison["changed"]
    identical = comparison["identical"]

    has_changes = bool(added or removed or changed)
    lines: List[str] = ["### APK Closure Drift Report", ""]

    if not has_changes:
        lines.append(f"No package drift detected against baseline ({len(identical)} packages unchanged).")
        return "\n".join(lines)

    lines.append("| Package | Change | Baseline Version | Current Version |")
    lines.append("|:---|:---|:---|:---|")

    all_changed_pkgs = sorted(set(added) | set(removed) | set(changed))
    for pkg in all_changed_pkgs:
        if pkg in added:
            lines.append(f"| `{pkg}` | added | - | `{added[pkg]}` |")
        elif pkg in removed:
            lines.append(f"| `{pkg}` | removed | `{removed[pkg]}` | - |")
        elif pkg in changed:
            b_ver = changed[pkg]["baseline"]
            c_ver = changed[pkg]["current"]
            lines.append(f"| `{pkg}` | changed | `{b_ver}` | `{c_ver}` |")

    lines.append("")
    lines.append(
        f"*Summary: {len(added)} added, {len(removed)} removed, {len(changed)} changed, "
        f"{len(identical)} unchanged (total: {total_current} current vs {total_baseline} baseline).*"
    )
    return "\n".join(lines)


def format_json_summary(
    comparison: Dict[str, Any], total_baseline: int, total_current: int
) -> Dict[str, Any]:
    """Generate structured JSON summary of package comparison."""
    added = comparison["added"]
    removed = comparison["removed"]
    changed = comparison["changed"]
    identical = comparison["identical"]

    has_changes = bool(added or removed or changed)
    return {
        "status": "drifted" if has_changes else "identical",
        "summary": {
            "added": len(added),
            "removed": len(removed),
            "changed": len(changed),
            "identical": len(identical),
            "total_baseline": total_baseline,
            "total_current": total_current,
        },
        "changes": {
            "added": [{"package": k, "current_version": v} for k, v in added.items()],
            "removed": [{"package": k, "baseline_version": v} for k, v in removed.items()],
            "changed": [
                {
                    "package": k,
                    "baseline_version": v["baseline"],
                    "current_version": v["current"],
                }
                for k, v in changed.items()
            ],
        },
    }


def parse_args(argv: Optional[List[str]] = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Compare Alpine apk closure manifests and report drift."
    )
    parser.add_argument(
        "baseline_pos",
        nargs="?",
        metavar="BASELINE",
        help="Path to baseline manifest (hack/os-packages-baseline.txt)",
    )
    parser.add_argument(
        "current_pos",
        nargs="?",
        metavar="CURRENT",
        help="Path to current manifest (/licenses/os-packages-manifest.txt)",
    )
    parser.add_argument(
        "--baseline",
        dest="baseline_flag",
        help="Path to baseline manifest (flag form)",
    )
    parser.add_argument(
        "--current",
        dest="current_flag",
        help="Path to current manifest (flag form)",
    )
    parser.add_argument(
        "--fail-on-change",
        action="store_true",
        help="Exit with non-zero status (1) if drift is detected",
    )
    parser.add_argument(
        "--output-markdown",
        "--markdown-out",
        dest="markdown_out",
        help="Write markdown table to this file path",
    )
    parser.add_argument(
        "--output-json",
        "--json-out",
        dest="json_out",
        help="Write JSON summary to this file path",
    )
    parser.add_argument(
        "--format",
        choices=["all", "markdown", "json"],
        default="all",
        help="Output format written to stdout (default: all)",
    )
    return parser.parse_args(argv)


def main(argv: Optional[List[str]] = None) -> int:
    args = parse_args(argv)

    baseline_path_str = args.baseline_flag or args.baseline_pos
    current_path_str = args.current_flag or args.current_pos

    # Check if baseline is absent or path doesn't exist
    if not baseline_path_str or not os.path.exists(baseline_path_str):
        print("no baseline")
        if args.markdown_out:
            Path(args.markdown_out).write_text("no baseline\n", encoding="utf-8")
        if args.json_out:
            no_baseline_json = {
                "status": "no baseline",
                "message": "no baseline",
                "summary": {
                    "added": 0,
                    "removed": 0,
                    "changed": 0,
                    "identical": 0,
                    "total_baseline": 0,
                    "total_current": 0,
                },
                "changes": {"added": [], "removed": [], "changed": []},
            }
            Path(args.json_out).write_text(
                json.dumps(no_baseline_json, indent=2) + "\n", encoding="utf-8"
            )
        return 0

    if not current_path_str:
        print("ERROR: current manifest path is required", file=sys.stderr)
        return 2

    if not os.path.exists(current_path_str):
        print(f"ERROR: current manifest file '{current_path_str}' not found", file=sys.stderr)
        return 2

    try:
        baseline_content = Path(baseline_path_str).read_text(encoding="utf-8")
        current_content = Path(current_path_str).read_text(encoding="utf-8")
    except OSError as e:
        print(f"ERROR: failed reading manifests: {e}", file=sys.stderr)
        return 2

    baseline_pkgs = parse_manifest(baseline_content)
    current_pkgs = parse_manifest(current_content)

    comparison = compare_manifests(baseline_pkgs, current_pkgs)
    total_baseline = len(baseline_pkgs)
    total_current = len(current_pkgs)

    markdown_report = format_markdown_table(comparison, total_baseline, total_current)
    json_summary = format_json_summary(comparison, total_baseline, total_current)
    json_str = json.dumps(json_summary, indent=2)

    # Write files if flags specified
    if args.markdown_out:
        Path(args.markdown_out).write_text(markdown_report + "\n", encoding="utf-8")
    if args.json_out:
        Path(args.json_out).write_text(json_str + "\n", encoding="utf-8")

    # Output to stdout according to --format
    if args.format == "markdown":
        print(markdown_report)
    elif args.format == "json":
        print(json_str)
    else:  # "all"
        print(markdown_report)
        print("")
        print("```json")
        print(json_str)
        print("```")

    has_changes = bool(
        comparison["added"] or comparison["removed"] or comparison["changed"]
    )
    if args.fail_on_change and has_changes:
        return 1

    return 0


if __name__ == "__main__":
    sys.exit(main())
