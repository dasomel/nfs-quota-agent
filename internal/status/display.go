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
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/dasomel/nfs-quota-agent/internal/quota"
	"github.com/dasomel/nfs-quota-agent/internal/util"
)

// ShowStatus displays the current quota status
func ShowStatus(basePath string, showAll bool) error {
	// Detect filesystem type
	fsType, err := quota.DetectFSType(basePath)
	if err != nil {
		return fmt.Errorf("failed to detect filesystem: %w", err)
	}

	// Get overall disk usage
	diskUsage, err := GetDiskUsage(basePath)
	if err != nil {
		return fmt.Errorf("failed to get disk usage: %w", err)
	}

	// Print header
	fmt.Printf("NFS Quota Status\n")
	fmt.Printf("================\n\n")
	fmt.Printf("Path:       %s\n", basePath)
	fmt.Printf("Filesystem: %s\n", fsType)
	fmt.Printf("Total:      %s\n", util.FormatBytes(int64(diskUsage.Total)))
	fmt.Printf("Used:       %s (%.1f%%)\n", util.FormatBytes(int64(diskUsage.Used)), diskUsage.UsedPct)
	fmt.Printf("Available:  %s\n\n", util.FormatBytes(int64(diskUsage.Available)))

	// Get directory quotas
	dirUsages, err := GetDirUsages(basePath, fsType)
	if err != nil {
		return fmt.Errorf("failed to get directory usages: %w", err)
	}

	if len(dirUsages) == 0 {
		fmt.Println("No project quotas configured.")
		return nil
	}

	// Sort by used space (descending)
	sort.Slice(dirUsages, func(i, j int) bool {
		return dirUsages[i].Used > dirUsages[j].Used
	})

	// Print directory table
	fmt.Printf("Directory Quotas (%d total)\n", len(dirUsages))
	fmt.Println(strings.Repeat("-", 80))

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "DIRECTORY\tUSED\tQUOTA\tUSED%\tSTATUS")

	displayCount := len(dirUsages)
	if !showAll && displayCount > 20 {
		displayCount = 20
	}

	for i := 0; i < displayCount; i++ {
		du := dirUsages[i]
		dirName := filepath.Base(du.Path)
		if len(dirName) > 40 {
			dirName = dirName[:37] + "..."
		}

		usedStr := util.FormatBytes(int64(du.Used))
		quotaStr := "-"
		pctStr := "-"
		st := "no quota"

		if du.Quota > 0 {
			quotaStr = util.FormatBytes(int64(du.Quota))
			pctStr = fmt.Sprintf("%.1f%%", du.QuotaPct)
			if du.QuotaPct >= 90 {
				st = "WARNING"
			} else if du.QuotaPct >= 100 {
				st = "EXCEEDED"
			} else {
				st = "OK"
			}
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", dirName, usedStr, quotaStr, pctStr, st)
	}
	w.Flush()

	if !showAll && len(dirUsages) > 20 {
		fmt.Printf("\n... and %d more directories (use --all to show all)\n", len(dirUsages)-20)
	}

	// Summary
	var totalUsed, totalQuota uint64
	warningCount, exceededCount := 0, 0
	for _, du := range dirUsages {
		totalUsed += du.Used
		totalQuota += du.Quota
		if du.Quota > 0 {
			if du.QuotaPct >= 100 {
				exceededCount++
			} else if du.QuotaPct >= 90 {
				warningCount++
			}
		}
	}

	fmt.Printf("\nSummary:\n")
	fmt.Printf("  Total directories: %d\n", len(dirUsages))
	fmt.Printf("  Total used:        %s\n", util.FormatBytes(int64(totalUsed)))
	fmt.Printf("  Total quota:       %s\n", util.FormatBytes(int64(totalQuota)))
	if warningCount > 0 || exceededCount > 0 {
		fmt.Printf("  Warnings:          %d (>90%% used)\n", warningCount)
		fmt.Printf("  Exceeded:          %d (>100%% used)\n", exceededCount)
	}

	return nil
}

// ShowTop displays top directories by usage
func ShowTop(basePath string, count int, watch bool) error {
	showOnce := func() error {
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

		// Clear screen in watch mode
		if watch {
			fmt.Print("\033[H\033[2J")
		}

		// Print header
		fmt.Printf("NFS Quota Top - %s\n", time.Now().Format("15:04:05"))
		fmt.Printf("Path: %s | Total: %s | Used: %s (%.1f%%) | Free: %s\n\n",
			basePath,
			util.FormatBytes(int64(diskUsage.Total)),
			util.FormatBytes(int64(diskUsage.Used)),
			diskUsage.UsedPct,
			util.FormatBytes(int64(diskUsage.Available)),
		)

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "#\tDIRECTORY\tUSED\tQUOTA\tUSED%\tBAR")

		displayCount := count
		if displayCount > len(dirUsages) {
			displayCount = len(dirUsages)
		}

		for i := 0; i < displayCount; i++ {
			du := dirUsages[i]
			dirName := filepath.Base(du.Path)
			if len(dirName) > 35 {
				dirName = dirName[:32] + "..."
			}

			usedStr := util.FormatBytes(int64(du.Used))
			quotaStr := "-"
			pctStr := "-"
			bar := ""

			if du.Quota > 0 {
				quotaStr = util.FormatBytes(int64(du.Quota))
				pctStr = fmt.Sprintf("%.1f%%", du.QuotaPct)
				bar = MakeProgressBar(du.QuotaPct, 20)
			}

			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n",
				i+1, dirName, usedStr, quotaStr, pctStr, bar)
		}
		w.Flush()

		if watch {
			fmt.Println("\nPress Ctrl+C to exit")
		}

		return nil
	}

	if watch {
		for {
			if err := showOnce(); err != nil {
				return err
			}
			time.Sleep(5 * time.Second)
		}
	}

	return showOnce()
}

// MakeProgressBar creates a text progress bar
func MakeProgressBar(pct float64, width int) string {
	if pct > 100 {
		pct = 100
	}
	filled := int(pct / 100 * float64(width))
	if filled > width {
		filled = width
	}

	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)

	if pct >= 90 {
		return fmt.Sprintf("[%s]!", bar)
	}
	return fmt.Sprintf("[%s]", bar)
}
