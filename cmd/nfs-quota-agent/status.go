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

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
)

// DiskUsage represents disk usage information
type DiskUsage struct {
	Total     uint64
	Used      uint64
	Available uint64
	UsedPct   float64
}

// DirUsage represents directory usage information
type DirUsage struct {
	Path      string
	Used      uint64
	Quota     uint64 // 0 if no quota
	UsedPct   float64
	QuotaPct  float64 // percentage of quota used
	ProjectID uint32
}

// ShowStatus displays the current quota status
func ShowStatus(basePath string, showAll bool) error {
	// Detect filesystem type
	fsType, err := detectFSType(basePath)
	if err != nil {
		return fmt.Errorf("failed to detect filesystem: %w", err)
	}

	// Get overall disk usage
	diskUsage, err := getDiskUsage(basePath)
	if err != nil {
		return fmt.Errorf("failed to get disk usage: %w", err)
	}

	// Print header
	fmt.Printf("NFS Quota Status\n")
	fmt.Printf("================\n\n")
	fmt.Printf("Path:       %s\n", basePath)
	fmt.Printf("Filesystem: %s\n", fsType)
	fmt.Printf("Total:      %s\n", formatBytes(int64(diskUsage.Total)))
	fmt.Printf("Used:       %s (%.1f%%)\n", formatBytes(int64(diskUsage.Used)), diskUsage.UsedPct)
	fmt.Printf("Available:  %s\n\n", formatBytes(int64(diskUsage.Available)))

	// Get directory quotas
	dirUsages, err := getDirUsages(basePath, fsType)
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

		usedStr := formatBytes(int64(du.Used))
		quotaStr := "-"
		pctStr := "-"
		status := "no quota"

		if du.Quota > 0 {
			quotaStr = formatBytes(int64(du.Quota))
			pctStr = fmt.Sprintf("%.1f%%", du.QuotaPct)
			if du.QuotaPct >= 90 {
				status = "WARNING"
			} else if du.QuotaPct >= 100 {
				status = "EXCEEDED"
			} else {
				status = "OK"
			}
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", dirName, usedStr, quotaStr, pctStr, status)
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
	fmt.Printf("  Total used:        %s\n", formatBytes(int64(totalUsed)))
	fmt.Printf("  Total quota:       %s\n", formatBytes(int64(totalQuota)))
	if warningCount > 0 || exceededCount > 0 {
		fmt.Printf("  Warnings:          %d (>90%% used)\n", warningCount)
		fmt.Printf("  Exceeded:          %d (>100%% used)\n", exceededCount)
	}

	return nil
}

// detectFSType detects filesystem type
func detectFSType(path string) (string, error) {
	cmd := exec.Command("df", "-T", path)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(output), "\n")
	if len(lines) < 2 {
		return "", fmt.Errorf("unexpected df output")
	}

	fields := strings.Fields(lines[1])
	if len(fields) < 2 {
		return "", fmt.Errorf("unexpected df output format")
	}

	return strings.ToLower(fields[1]), nil
}

// getDiskUsage returns overall disk usage for the path
func getDiskUsage(path string) (*DiskUsage, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return nil, err
	}

	total := stat.Blocks * uint64(stat.Bsize)
	free := stat.Bfree * uint64(stat.Bsize)
	used := total - free
	usedPct := float64(used) / float64(total) * 100

	return &DiskUsage{
		Total:     total,
		Used:      used,
		Available: free,
		UsedPct:   usedPct,
	}, nil
}

// getDirUsages returns usage information for all directories with quotas
func getDirUsages(basePath, fsType string) ([]DirUsage, error) {
	var usages []DirUsage

	// Read directories
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return nil, err
	}

	// Get quota report based on filesystem type
	quotaMap := make(map[string]uint64)
	usageMap := make(map[string]uint64)

	switch fsType {
	case "xfs":
		quotaMap, usageMap, err = getXFSQuotaReport(basePath)
	case "ext4":
		quotaMap, usageMap, err = getExt4QuotaReport(basePath)
	}
	if err != nil {
		// Continue without quota info
		quotaMap = make(map[string]uint64)
		usageMap = make(map[string]uint64)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		// Skip hidden directories and special files
		name := entry.Name()
		if strings.HasPrefix(name, ".") || name == "projects" || name == "projid" {
			continue
		}

		dirPath := filepath.Join(basePath, name)

		// Get directory size
		var used uint64
		if u, ok := usageMap[dirPath]; ok {
			used = u
		} else {
			used = getDirSize(dirPath)
		}

		du := DirUsage{
			Path: dirPath,
			Used: used,
		}

		// Get quota if available
		if quota, ok := quotaMap[dirPath]; ok {
			du.Quota = quota
			if quota > 0 {
				du.QuotaPct = float64(used) / float64(quota) * 100
			}
		}

		usages = append(usages, du)
	}

	return usages, nil
}

// getXFSQuotaReport parses xfs_quota report
func getXFSQuotaReport(basePath string) (map[string]uint64, map[string]uint64, error) {
	quotaMap := make(map[string]uint64)
	usageMap := make(map[string]uint64)

	cmd := exec.Command("xfs_quota", "-x", "-c", "report -p -b", basePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return quotaMap, usageMap, err
	}

	// Parse projid file to get projectName -> projectID mapping
	projidMap := make(map[string]string) // projectName -> projectID
	projidFile := "/etc/projid"
	if data, err := os.ReadFile(projidFile); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				projidMap[parts[0]] = parts[1] // name -> id
			}
		}
	}

	// Parse projects file to get projectID -> path mapping
	projectPaths := make(map[string]string) // projectID -> path
	projectsFile := "/etc/projects"
	if data, err := os.ReadFile(projectsFile); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				projectPaths[parts[0]] = parts[1] // id -> path
			}
		}
	}

	// Build projectName -> path mapping
	nameToPaths := make(map[string]string)
	for name, id := range projidMap {
		if path, ok := projectPaths[id]; ok {
			nameToPaths[name] = path
		}
	}

	// Parse xfs_quota output
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}

		// Skip header lines
		if fields[0] == "Project" || strings.HasPrefix(fields[0], "-") {
			continue
		}

		projectName := strings.TrimPrefix(fields[0], "#")
		// Try to find path by project name first, then by project ID
		var path string
		if p, ok := nameToPaths[projectName]; ok {
			path = p
		} else if p, ok := projectPaths[projectName]; ok {
			path = p
		} else {
			continue
		}

		// Used is in KB, convert to bytes
		if used, err := parseSize(fields[1]); err == nil {
			usageMap[path] = used * 1024
		}
		// Hard limit is in KB
		if len(fields) >= 4 {
			if hard, err := parseSize(fields[3]); err == nil && hard > 0 {
				quotaMap[path] = hard * 1024
			}
		}
	}

	return quotaMap, usageMap, nil
}

// getExt4QuotaReport parses repquota output
func getExt4QuotaReport(basePath string) (map[string]uint64, map[string]uint64, error) {
	quotaMap := make(map[string]uint64)
	usageMap := make(map[string]uint64)

	cmd := exec.Command("repquota", "-P", basePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return quotaMap, usageMap, err
	}

	// Parse projects file (use /etc/projects, not basePath)
	projectPaths := make(map[string]string)
	projectsFile := "/etc/projects"
	if data, err := os.ReadFile(projectsFile); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				projectPaths[parts[0]] = parts[1]
			}
		}
	}

	// Parse repquota output
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}

		// Skip header
		if fields[0] == "Project" || strings.HasPrefix(line, "-") || strings.HasPrefix(line, "#") {
			continue
		}

		projectID := strings.TrimSuffix(fields[0], "--")
		projectID = strings.TrimSuffix(projectID, "+-")
		projectID = strings.TrimSuffix(projectID, "-+")
		projectID = strings.TrimSuffix(projectID, "++")

		if path, ok := projectPaths[projectID]; ok {
			// Used is in KB
			if used, err := parseSize(fields[2]); err == nil {
				usageMap[path] = used * 1024
			}
			// Hard limit
			if len(fields) >= 5 {
				if hard, err := parseSize(fields[4]); err == nil && hard > 0 {
					quotaMap[path] = hard * 1024
				}
			}
		}
	}

	return quotaMap, usageMap, nil
}

// getDirSize calculates directory size recursively
func getDirSize(path string) uint64 {
	var size uint64
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			size += uint64(info.Size())
		}
		return nil
	})
	return size
}

// parseSize parses size string (handles K, M, G suffixes)
func parseSize(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}

	var multiplier uint64 = 1
	if strings.HasSuffix(s, "K") || strings.HasSuffix(s, "k") {
		multiplier = 1
		s = s[:len(s)-1]
	} else if strings.HasSuffix(s, "M") || strings.HasSuffix(s, "m") {
		multiplier = 1024
		s = s[:len(s)-1]
	} else if strings.HasSuffix(s, "G") || strings.HasSuffix(s, "g") {
		multiplier = 1024 * 1024
		s = s[:len(s)-1]
	}

	var value uint64
	_, err := fmt.Sscanf(s, "%d", &value)
	if err != nil {
		return 0, err
	}

	return value * multiplier, nil
}
