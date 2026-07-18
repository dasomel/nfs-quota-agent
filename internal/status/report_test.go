/*
Copyright 2024 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package status

import (
	"os"
	"strings"
	"testing"
	"time"
)

func sampleReport() QuotaReport {
	return QuotaReport{
		Timestamp:  time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Path:       "/data/nfs",
		Filesystem: "xfs",
		Disk:       DiskUsage{Total: 1000, Used: 400, Available: 600, UsedPct: 40},
		Quotas: []QuotaEntry{
			{Directory: "pvc-a", Path: "/data/nfs/pvc-a", UsedBytes: 900, Used: "900 B", QuotaBytes: 1000, Quota: "1000 B", UsedPct: 90, Status: "warning"},
			{Directory: "pvc-b", Path: "/data/nfs/pvc-b", UsedBytes: 0, Used: "0 B", QuotaBytes: 0, Quota: "0 B", UsedPct: 0, Status: "no_quota"},
		},
		Summary: QuotaSummary{
			TotalDirectories: 2, TotalUsedBytes: 900, TotalUsed: "900 B",
			TotalQuotaBytes: 1000, TotalQuota: "1000 B", WarningCount: 1, ExceededCount: 0,
		},
	}
}

// writeAndRead runs one of the unexported writeYAML/writeCSV/writeTable
// functions (they take *os.File, not io.Writer) against a temp file and
// returns what was written.
func writeAndRead(t *testing.T, fn func(*os.File, QuotaReport) error, report QuotaReport) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "report-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer f.Close()

	if err := fn(f, report); err != nil {
		t.Fatalf("write function returned error: %v", err)
	}

	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return string(data)
}

func TestWriteYAML(t *testing.T) {
	out := writeAndRead(t, writeYAML, sampleReport())
	for _, want := range []string{
		"path: /data/nfs",
		"filesystem: xfs",
		"total_directories: 2",
		"warning_count: 1",
		"- directory: pvc-a",
		"status: warning",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("YAML output missing %q\noutput:\n%s", want, out)
		}
	}
}

func TestWriteCSV(t *testing.T) {
	out := writeAndRead(t, writeCSV, sampleReport())
	lines := strings.Split(strings.TrimRight(out, "\r\n"), "\n")
	if len(lines) != 3 { // header + 2 rows
		t.Fatalf("expected 3 CSV lines, got %d:\n%s", len(lines), out)
	}
	wantHeader := "directory,path,used_bytes,used,quota_bytes,quota,used_pct,status"
	if strings.TrimRight(lines[0], "\r") != wantHeader {
		t.Errorf("unexpected CSV header: %q", lines[0])
	}
	if !strings.Contains(lines[1], "pvc-a,/data/nfs/pvc-a,900,900 B,1000,1000 B,90.00,warning") {
		t.Errorf("unexpected CSV row: %q", lines[1])
	}
}

func TestWriteTable(t *testing.T) {
	out := writeAndRead(t, writeTable, sampleReport())
	for _, want := range []string{
		"NFS Quota Report",
		"Path:      /data/nfs",
		"Filesystem: xfs",
		"pvc-a",
		"warning",
		"Total directories: 2",
		"Warnings (>90%):   1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q\noutput:\n%s", want, out)
		}
	}
}

// GenerateReport is not covered here: it starts by calling
// quota.DetectFSType(basePath), which (see display_test.go) cannot succeed
// against a real directory path on BSD/macOS df. The format-specific writers
// it delegates to (writeYAML/writeCSV/writeTable, tested above) hold all of
// the format-rendering logic that's independent of that platform quirk.
