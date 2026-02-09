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
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/dasomel/nfs-quota-agent/internal/quota"
	"github.com/dasomel/nfs-quota-agent/internal/util"
)

// QuotaReport represents the full quota report
type QuotaReport struct {
	Timestamp  time.Time    `json:"timestamp" yaml:"timestamp"`
	Path       string       `json:"path" yaml:"path"`
	Filesystem string       `json:"filesystem" yaml:"filesystem"`
	Disk       DiskUsage    `json:"disk" yaml:"disk"`
	Quotas     []QuotaEntry `json:"quotas" yaml:"quotas"`
	Summary    QuotaSummary `json:"summary" yaml:"summary"`
}

// QuotaEntry represents a single quota entry
type QuotaEntry struct {
	Directory  string  `json:"directory" yaml:"directory"`
	Path       string  `json:"path" yaml:"path"`
	UsedBytes  uint64  `json:"used_bytes" yaml:"used_bytes"`
	Used       string  `json:"used" yaml:"used"`
	QuotaBytes uint64  `json:"quota_bytes" yaml:"quota_bytes"`
	Quota      string  `json:"quota" yaml:"quota"`
	UsedPct    float64 `json:"used_pct" yaml:"used_pct"`
	Status     string  `json:"status" yaml:"status"`
}

// QuotaSummary contains summary statistics
type QuotaSummary struct {
	TotalDirectories int    `json:"total_directories" yaml:"total_directories"`
	TotalUsedBytes   uint64 `json:"total_used_bytes" yaml:"total_used_bytes"`
	TotalUsed        string `json:"total_used" yaml:"total_used"`
	TotalQuotaBytes  uint64 `json:"total_quota_bytes" yaml:"total_quota_bytes"`
	TotalQuota       string `json:"total_quota" yaml:"total_quota"`
	WarningCount     int    `json:"warning_count" yaml:"warning_count"`
	ExceededCount    int    `json:"exceeded_count" yaml:"exceeded_count"`
}

// GenerateReport generates a quota report in various formats
func GenerateReport(basePath, format, outputFile string) error {
	fsType, err := quota.DetectFSType(basePath)
	if err != nil {
		return err
	}

	diskUsage, err := GetDiskUsage(basePath)
	if err != nil {
		return err
	}

	dirUsages, err := GetDirUsages(basePath, fsType)
	if err != nil {
		return err
	}

	// Sort by used space (descending)
	sort.Slice(dirUsages, func(i, j int) bool {
		return dirUsages[i].Used > dirUsages[j].Used
	})

	// Build report
	report := QuotaReport{
		Timestamp:  time.Now(),
		Path:       basePath,
		Filesystem: fsType,
		Disk: DiskUsage{
			Total:     diskUsage.Total,
			Used:      diskUsage.Used,
			Available: diskUsage.Available,
			UsedPct:   diskUsage.UsedPct,
		},
	}

	var totalUsed, totalQuota uint64
	var warningCount, exceededCount int

	for _, du := range dirUsages {
		st := "ok"
		if du.Quota > 0 {
			if du.QuotaPct >= 100 {
				st = "exceeded"
				exceededCount++
			} else if du.QuotaPct >= 90 {
				st = "warning"
				warningCount++
			}
		} else {
			st = "no_quota"
		}

		entry := QuotaEntry{
			Directory:  filepath.Base(du.Path),
			Path:       du.Path,
			UsedBytes:  du.Used,
			Used:       util.FormatBytes(int64(du.Used)),
			QuotaBytes: du.Quota,
			Quota:      util.FormatBytes(int64(du.Quota)),
			UsedPct:    du.QuotaPct,
			Status:     st,
		}
		report.Quotas = append(report.Quotas, entry)

		totalUsed += du.Used
		totalQuota += du.Quota
	}

	report.Summary = QuotaSummary{
		TotalDirectories: len(dirUsages),
		TotalUsedBytes:   totalUsed,
		TotalUsed:        util.FormatBytes(int64(totalUsed)),
		TotalQuotaBytes:  totalQuota,
		TotalQuota:       util.FormatBytes(int64(totalQuota)),
		WarningCount:     warningCount,
		ExceededCount:    exceededCount,
	}

	// Output
	var out *os.File
	if outputFile != "" {
		var err error
		out, err = os.Create(outputFile)
		if err != nil {
			return err
		}
		defer out.Close()
	} else {
		out = os.Stdout
	}

	switch format {
	case "json":
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)

	case "yaml":
		// Simple YAML output without external dependency
		return writeYAML(out, report)

	case "csv":
		return writeCSV(out, report)

	default: // table
		return writeTable(out, report)
	}
}

func writeYAML(out *os.File, report QuotaReport) error {
	fmt.Fprintf(out, "timestamp: %s\n", report.Timestamp.Format(time.RFC3339))
	fmt.Fprintf(out, "path: %s\n", report.Path)
	fmt.Fprintf(out, "filesystem: %s\n", report.Filesystem)
	fmt.Fprintf(out, "disk:\n")
	fmt.Fprintf(out, "  total: %d\n", report.Disk.Total)
	fmt.Fprintf(out, "  used: %d\n", report.Disk.Used)
	fmt.Fprintf(out, "  available: %d\n", report.Disk.Available)
	fmt.Fprintf(out, "  used_pct: %.2f\n", report.Disk.UsedPct)
	fmt.Fprintf(out, "summary:\n")
	fmt.Fprintf(out, "  total_directories: %d\n", report.Summary.TotalDirectories)
	fmt.Fprintf(out, "  total_used: %s\n", report.Summary.TotalUsed)
	fmt.Fprintf(out, "  total_quota: %s\n", report.Summary.TotalQuota)
	fmt.Fprintf(out, "  warning_count: %d\n", report.Summary.WarningCount)
	fmt.Fprintf(out, "  exceeded_count: %d\n", report.Summary.ExceededCount)
	fmt.Fprintf(out, "quotas:\n")
	for _, q := range report.Quotas {
		fmt.Fprintf(out, "  - directory: %s\n", q.Directory)
		fmt.Fprintf(out, "    used: %s\n", q.Used)
		fmt.Fprintf(out, "    quota: %s\n", q.Quota)
		fmt.Fprintf(out, "    used_pct: %.2f\n", q.UsedPct)
		fmt.Fprintf(out, "    status: %s\n", q.Status)
	}
	return nil
}

func writeCSV(out *os.File, report QuotaReport) error {
	w := csv.NewWriter(out)
	defer w.Flush()

	// Header
	_ = w.Write([]string{"directory", "path", "used_bytes", "used", "quota_bytes", "quota", "used_pct", "status"})

	for _, q := range report.Quotas {
		_ = w.Write([]string{
			q.Directory,
			q.Path,
			fmt.Sprintf("%d", q.UsedBytes),
			q.Used,
			fmt.Sprintf("%d", q.QuotaBytes),
			q.Quota,
			fmt.Sprintf("%.2f", q.UsedPct),
			q.Status,
		})
	}

	return nil
}

func writeTable(out *os.File, report QuotaReport) error {
	fmt.Fprintf(out, "NFS Quota Report\n")
	fmt.Fprintf(out, "================\n\n")
	fmt.Fprintf(out, "Generated: %s\n", report.Timestamp.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(out, "Path:      %s\n", report.Path)
	fmt.Fprintf(out, "Filesystem: %s\n\n", report.Filesystem)

	fmt.Fprintf(out, "Disk Usage:\n")
	fmt.Fprintf(out, "  Total:     %s\n", util.FormatBytes(int64(report.Disk.Total)))
	fmt.Fprintf(out, "  Used:      %s (%.1f%%)\n", util.FormatBytes(int64(report.Disk.Used)), report.Disk.UsedPct)
	fmt.Fprintf(out, "  Available: %s\n\n", util.FormatBytes(int64(report.Disk.Available)))

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "DIRECTORY\tUSED\tQUOTA\tUSED%\tSTATUS")
	fmt.Fprintln(w, "---------\t----\t-----\t-----\t------")

	for _, q := range report.Quotas {
		dirName := q.Directory
		if len(dirName) > 40 {
			dirName = dirName[:37] + "..."
		}
		pctStr := "-"
		if q.QuotaBytes > 0 {
			pctStr = fmt.Sprintf("%.1f%%", q.UsedPct)
		}
		quotaStr := q.Quota
		if q.QuotaBytes == 0 {
			quotaStr = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", dirName, q.Used, quotaStr, pctStr, q.Status)
	}
	w.Flush()

	fmt.Fprintf(out, "\nSummary:\n")
	fmt.Fprintf(out, "  Total directories: %d\n", report.Summary.TotalDirectories)
	fmt.Fprintf(out, "  Total used:        %s\n", report.Summary.TotalUsed)
	fmt.Fprintf(out, "  Total quota:       %s\n", report.Summary.TotalQuota)
	if report.Summary.WarningCount > 0 {
		fmt.Fprintf(out, "  Warnings (>90%%):   %d\n", report.Summary.WarningCount)
	}
	if report.Summary.ExceededCount > 0 {
		fmt.Fprintf(out, "  Exceeded (>100%%):  %d\n", report.Summary.ExceededCount)
	}

	return nil
}
