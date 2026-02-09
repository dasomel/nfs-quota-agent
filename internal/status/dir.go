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
	"path/filepath"
	"strings"

	"github.com/dasomel/nfs-quota-agent/internal/quota"
)

// GetDirUsages returns usage information for all directories with quotas
func GetDirUsages(basePath, fsType string) ([]DirUsage, error) {
	var usages []DirUsage

	// Get quota report based on filesystem type
	quotaMap := make(map[string]uint64)
	usageMap := make(map[string]uint64)
	var err error

	switch fsType {
	case "xfs":
		quotaMap, usageMap, err = quota.GetXFSQuotaReport(basePath)
	case "ext4":
		quotaMap, usageMap, err = quota.GetExt4QuotaReport(basePath)
	}
	if err != nil {
		// Continue without quota info
		quotaMap = make(map[string]uint64)
		usageMap = make(map[string]uint64)
	}

	// Collect all directories that have quotas from quotaMap
	quotaDirs := make(map[string]bool)
	for path := range quotaMap {
		quotaDirs[path] = true
	}

	// Also scan directories up to 2 levels deep to find all potential PVC dirs
	// This handles both flat (pvc-xxx) and nested (namespace/pvc-name) patterns
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		name := entry.Name()
		if strings.HasPrefix(name, ".") || name == "projects" || name == "projid" {
			continue
		}

		dirPath := filepath.Join(basePath, name)

		// Check if this is a namespace directory (contains subdirectories)
		subEntries, err := os.ReadDir(dirPath)
		if err == nil {
			hasSubDirs := false
			for _, subEntry := range subEntries {
				if subEntry.IsDir() && !strings.HasPrefix(subEntry.Name(), ".") {
					hasSubDirs = true
					// Add nested directory
					subDirPath := filepath.Join(dirPath, subEntry.Name())
					quotaDirs[subDirPath] = true
				}
			}
			// If no subdirs, this might be a flat PVC directory
			if !hasSubDirs {
				quotaDirs[dirPath] = true
			}
		} else {
			quotaDirs[dirPath] = true
		}
	}

	// Build usage list from all discovered directories
	for dirPath := range quotaDirs {
		// Skip if directory doesn't exist
		if _, err := os.Stat(dirPath); os.IsNotExist(err) {
			continue
		}

		// Get directory size
		var used uint64
		if u, ok := usageMap[dirPath]; ok {
			used = u
		} else {
			used = GetDirSize(dirPath)
		}

		du := DirUsage{
			Path: dirPath,
			Used: used,
		}

		// Get quota if available
		if q, ok := quotaMap[dirPath]; ok {
			du.Quota = q
			if q > 0 {
				du.QuotaPct = float64(used) / float64(q) * 100
			}
		}

		usages = append(usages, du)
	}

	return usages, nil
}

// GetDirSize calculates directory size recursively
func GetDirSize(path string) uint64 {
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
