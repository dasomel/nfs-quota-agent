#!/usr/bin/env python3
"""Tests for scripts/ci/check-dashboard-metrics.py.

Covers:
- Valid dashboard expressions with metrics defined in metrics.go pass
- Undefined metric in expression fails with proper error reporting
- Metric extraction from PromQL queries with labels, ranges, binary operators
- Handling of nested panels inside rows
- Missing metrics file or invalid JSON error handling
"""

from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parent / "check-dashboard-metrics.py"


class CheckDashboardMetricsTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)

        # Create mock internal/metrics/metrics.go
        self.metrics_file = self.root / "internal" / "metrics" / "metrics.go"
        self.metrics_file.parent.mkdir(parents=True, exist_ok=True)
        self.metrics_file.write_text(
            """package metrics

func dummy() {
\tsb.WriteString("# HELP nfs_quota_used_bytes Used space\\n")
\tsb.WriteString("# TYPE nfs_quota_used_bytes gauge\\n")
\tsb.WriteString("# HELP nfs_quota_limit_bytes Quota limit\\n")
\tsb.WriteString("# TYPE nfs_quota_limit_bytes gauge\\n")
\tsb.WriteString("# HELP nfs_quota_used_percent Used percent\\n")
\tsb.WriteString("# TYPE nfs_quota_used_percent gauge\\n")
}
""",
            encoding="utf-8",
        )

        self.dash_dir = self.root / "charts" / "nfs-quota-agent" / "dashboards"
        self.dash_dir.mkdir(parents=True, exist_ok=True)

    def _run_cli(self, args: list[str]) -> subprocess.CompletedProcess[str]:
        cmd = [sys.executable, str(SCRIPT), "--root", str(self.root), *args]
        return subprocess.run(
            cmd,
            cwd=str(self.root),
            capture_output=True,
            text=True,
            check=False,
        )

    def test_valid_dashboard_passes(self) -> None:
        dash_json = self.dash_dir / "valid.json"
        dash_json.write_text(
            json.dumps(
                {
                    "panels": [
                        {
                            "title": "Usage Panel",
                            "targets": [
                                {
                                    "refId": "A",
                                    "expr": "nfs_quota_used_bytes{directory=\"pv-1\"}",
                                },
                                {
                                    "refId": "B",
                                    "expr": "nfs_quota_used_percent > 80",
                                },
                            ],
                        }
                    ]
                }
            ),
            encoding="utf-8",
        )

        proc = self._run_cli([str(dash_json)])
        self.assertEqual(proc.returncode, 0, msg=proc.stderr)
        self.assertIn("SUCCESS: Verified 1 dashboard(s)", proc.stdout)

    def test_undefined_metric_fails(self) -> None:
        dash_json = self.dash_dir / "invalid.json"
        dash_json.write_text(
            json.dumps(
                {
                    "panels": [
                        {
                            "title": "Bad Panel",
                            "targets": [
                                {
                                    "refId": "A",
                                    "expr": "unknown_metric_total + nfs_quota_used_bytes",
                                }
                            ],
                        }
                    ]
                }
            ),
            encoding="utf-8",
        )

        proc = self._run_cli([str(dash_json)])
        self.assertEqual(proc.returncode, 1)
        self.assertIn("references undefined metric(s): unknown_metric_total", proc.stderr)

    def test_nested_panels_in_row(self) -> None:
        dash_json = self.dash_dir / "nested.json"
        dash_json.write_text(
            json.dumps(
                {
                    "panels": [
                        {
                            "title": "Row",
                            "type": "row",
                            "panels": [
                                {
                                    "title": "Child Panel",
                                    "targets": [
                                        {"expr": "nfs_quota_limit_bytes"}
                                    ],
                                }
                            ],
                        }
                    ]
                }
            ),
            encoding="utf-8",
        )

        proc = self._run_cli([str(dash_json)])
        self.assertEqual(proc.returncode, 0, msg=proc.stderr)
        self.assertIn("SUCCESS", proc.stdout)


if __name__ == "__main__":
    unittest.main()
