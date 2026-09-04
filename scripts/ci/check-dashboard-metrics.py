#!/usr/bin/env python3
"""Validate that Prometheus metrics in Grafana dashboard JSON files are defined in internal/metrics/metrics.go.

Extracts all PromQL queries from 'expr' fields in dashboard panels and identifies metric names,
failing if any metric is not defined in the agent's metric collector.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path

# PromQL built-in aggregators, functions, keywords, and operators to ignore
# when identifying metric identifiers from expressions.
PROMQL_KEYWORDS = {
    # Aggregators
    "sum", "min", "max", "avg", "group", "stddev", "stdvar", "count", "count_values",
    "bottomk", "topk", "quantile",
    # Functions
    "rate", "irate", "increase", "resets", "changes", "deriv", "predict_linear",
    "histogram_quantile", "histogram_fraction",
    "absent", "absent_over_time",
    "ceil", "floor", "round", "clamp", "clamp_min", "clamp_max", "abs", "sqrt", "exp", "ln", "log2", "log10",
    "sgn", "sort", "sort_desc",
    "timestamp", "time",
    "label_replace", "label_join",
    "vector", "scalar",
    "day_of_month", "day_of_week", "day_of_year", "days_in_month", "hour", "minute", "month", "year",
    # Keywords / operators
    "by", "without", "on", "ignoring", "group_left", "group_right", "offset", "bool",
    "and", "or", "unless", "inf", "nan",
}


def find_repo_root(start_path: Path | None = None) -> Path:
    """Find repository root containing .git or return parent."""
    cur = (start_path or Path.cwd()).resolve()
    for p in [cur, *cur.parents]:
        if (p / ".git").exists():
            return p
    return Path(__file__).resolve().parent.parent.parent


def get_defined_metrics(metrics_file: Path) -> set[str]:
    """Parse Go source file and extract all defined Prometheus metric names from # HELP comments."""
    if not metrics_file.exists():
        raise FileNotFoundError(f"Metrics definition file not found: {metrics_file}")

    content = metrics_file.read_text(encoding="utf-8")
    # Metric names follow # HELP <name> or # TYPE <name>
    metrics = set(re.findall(r"# HELP\s+([a-zA-Z_][a-zA-Z0-9_:]*)", content))
    types = set(re.findall(r"# TYPE\s+([a-zA-Z_][a-zA-Z0-9_:]*)", content))
    return metrics | types


def extract_metrics_from_expr(expr: str) -> set[str]:
    """Extract metric names from a PromQL expression string."""
    metrics: set[str] = set()

    # 1. Strip string literals ("..." and '...')
    cleaned = re.sub(r'"(?:\\.|[^"\\])*"', " ", expr)
    cleaned = re.sub(r"'(?:\\.|[^'\\])*'", " ", cleaned)

    # 2. Extract metrics with label matchers: metric_name{...}
    for m in re.finditer(r"\b([a-zA-Z_][a-zA-Z0-9_:]*)\s*\{", cleaned):
        name = m.group(1)
        if name not in PROMQL_KEYWORDS:
            metrics.add(name)

    # 3. Extract metrics with range windows: metric_name[...]
    for m in re.finditer(r"\b([a-zA-Z_][a-zA-Z0-9_:]*)\s*\[", cleaned):
        name = m.group(1)
        if name not in PROMQL_KEYWORDS:
            metrics.add(name)

    # 4. Strip label selector blocks {...}, range blocks [...], and grouping clauses
    cleaned = re.sub(r"\{[^}]*\}", " ", cleaned)
    cleaned = re.sub(r"\[[^\]]*\]", " ", cleaned)
    cleaned = re.sub(r"\b(by|without|on|ignoring)\s*\([^)]*\)", " ", cleaned)

    # 5. Remaining identifiers (e.g. standalone metric in binary op or aggregation)
    for token in re.findall(r"\b([a-zA-Z_][a-zA-Z0-9_:]*)\b", cleaned):
        if token not in PROMQL_KEYWORDS and not token.isdigit():
            metrics.add(token)

    return metrics


def extract_dashboard_queries(dashboard_json: dict) -> list[tuple[str, str, str]]:
    """Extract all (panel_title, target_ref_id, expr) from dashboard JSON structure."""
    results: list[tuple[str, str, str]] = []

    def scan_panel(panel: dict, parent_title: str = ""):
        title = panel.get("title", parent_title or "<untitled>")
        for target in panel.get("targets", []):
            expr = target.get("expr")
            if expr and isinstance(expr, str) and expr.strip():
                ref_id = target.get("refId", "")
                results.append((title, ref_id, expr.strip()))
        # Check nested panels (rows in Grafana)
        for nested in panel.get("panels", []):
            scan_panel(nested, parent_title=title)

    for panel in dashboard_json.get("panels", []):
        scan_panel(panel)

    return results


def check_dashboard_file(dashboard_path: Path, defined_metrics: set[str]) -> tuple[list[str], int, int]:
    """Validate a single dashboard file.

    Returns:
        tuple of (failures list, total expressions checked, total unique metrics referenced)
    """
    failures: list[str] = []
    try:
        data = json.loads(dashboard_path.read_text(encoding="utf-8"))
    except Exception as exc:
        return [f"{dashboard_path}: failed to parse JSON: {exc}"], 0, 0

    queries = extract_dashboard_queries(data)
    all_referenced_metrics: set[str] = set()

    for title, ref_id, expr in queries:
        metrics = extract_metrics_from_expr(expr)
        all_referenced_metrics.update(metrics)
        unknown = metrics - defined_metrics
        if unknown:
            ref_str = f" [target {ref_id}]" if ref_id else ""
            unknown_str = ", ".join(sorted(unknown))
            failures.append(
                f"{dashboard_path}: panel '{title}'{ref_str} references undefined metric(s): {unknown_str}\n"
                f"  expr: {expr}"
            )

    return failures, len(queries), len(all_referenced_metrics)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "dashboards",
        nargs="*",
        type=Path,
        help="Path(s) to Grafana dashboard JSON file(s). Defaults to charts/nfs-quota-agent/dashboards/*.json",
    )
    parser.add_argument(
        "--metrics-file",
        type=Path,
        default=None,
        help="Path to internal/metrics/metrics.go (defaults to repo root relative path)",
    )
    parser.add_argument(
        "--root",
        type=Path,
        default=None,
        help="Repo root directory override",
    )
    args = parser.parse_args()

    repo_root = find_repo_root(args.root)
    metrics_file = args.metrics_file or (repo_root / "internal" / "metrics" / "metrics.go")

    try:
        defined_metrics = get_defined_metrics(metrics_file)
    except Exception as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 1

    dashboard_files = args.dashboards
    if not dashboard_files:
        dashboard_dir = repo_root / "charts" / "nfs-quota-agent" / "dashboards"
        dashboard_files = sorted(dashboard_dir.glob("*.json"))

    if not dashboard_files:
        print(f"ERROR: No dashboard files found to validate", file=sys.stderr)
        return 1

    total_failures: list[str] = []
    total_queries = 0
    total_metrics = 0

    for dash in dashboard_files:
        failures, queries_count, metrics_count = check_dashboard_file(dash, defined_metrics)
        total_failures.extend(failures)
        total_queries += queries_count
        total_metrics += metrics_count

    if total_failures:
        for f in total_failures:
            print(f"ERROR: {f}", file=sys.stderr)
        return 1

    print(
        f"SUCCESS: Verified {len(dashboard_files)} dashboard(s) ({total_queries} queries, "
        f"{total_metrics} unique metrics): all metrics defined in {metrics_file.relative_to(repo_root)}"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
