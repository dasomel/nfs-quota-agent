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
	"strings"

	"github.com/dasomel/nfs-quota-agent/internal/quota"
)

// GetDirUsages returns usage information for all directories with quotas.
// projectsFile/projidFile must be the same paths the agent applying quotas
// is actually configured with (a.ProjectsFile()/a.ProjidFile()) -- pass the
// literal "/etc/projects"/"/etc/projid" only from a genuinely standalone
// caller with no agent instance to read the real configured paths from
// (the `nfs-quota-agent status` CLI path). Passing the standard-path
// literals from a caller that does have an agent in scope silently shows
// empty/wrong usage under a non-default --projects-file/--projid-file --
// exactly the gap this parameterization exists to close; see the
// CLAUDE.md gotcha on this.
func GetDirUsages(basePath, fsType, projectsFile, projidFile string) ([]DirUsage, error) {
	var usages []DirUsage

	// Get quota report based on filesystem type
	quotaMap := make(map[string]uint64)
	usageMap := make(map[string]uint64)
	var err error

	switch fsType {
	case "xfs":
		quotaMap, usageMap, err = quota.GetXFSQuotaReport(basePath, projectsFile, projidFile)
	case "ext4":
		quotaMap, usageMap, err = quota.GetExt4QuotaReport(basePath, projectsFile, projidFile)
	case "btrfs":
		quotaMap, usageMap, err = quota.GetBtrfsQuotaReport(basePath)
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

// GetReportedUsage returns the fsType-specific quota report's usage map
// (path -> bytes used) exactly as the report command states it, with none
// of GetDirUsages' tolerance: a report failure is returned to the caller
// instead of being swallowed into an empty map, and there is no
// filepath.Walk apparent-size fallback for a path the report has no entry
// for. GetDirUsages' tolerance is correct for its own callers (metrics, the
// web UI: "show something rather than nothing"), but it makes "usage is
// truly zero" and "we couldn't find out" indistinguishable, which is
// exactly wrong for a caller that must fail closed on the latter -- see
// CLAUDE.md's high-risk-path note on apply/verify comparison logic and the
// agent's ensureQuota shrink guard, the intended caller.
//
// Same basePath/projectsFile/projidFile caveats as GetDirUsages apply here.
func GetReportedUsage(basePath, fsType, projectsFile, projidFile string) (map[string]uint64, error) {
	switch fsType {
	case "xfs":
		_, usageMap, err := quota.GetXFSQuotaReportStrict(basePath, projectsFile, projidFile)
		return usageMap, err
	case "ext4":
		_, usageMap, err := quota.GetExt4QuotaReportStrict(basePath, projectsFile, projidFile)
		return usageMap, err
	case "btrfs":
		_, usageMap, err := quota.GetBtrfsQuotaReport(basePath)
		return usageMap, err
	default:
		return nil, fmt.Errorf("unsupported filesystem type: %s", fsType)
	}
}

// GetDirUsagesStrict is GetDirUsages' sibling for the shrink guard's
// brownfield snapshot ONLY (internal/agent's
// primeAppliedQuotasFromDiskOnce) -- no other caller should use it. Same
// directory-discovery walk as GetDirUsages, but "strict" in two ways:
//
//  1. A quota-report failure is returned to the caller instead of being
//     swallowed into an empty quotaMap/usageMap -- see GetReportedUsage's
//     doc comment for why a caller that must fail closed on "we don't
//     know" can't tell that apart from "usage is genuinely zero" once the
//     error is discarded.
//  2. A path absent from usageMap falls back to GetDirAllocatedSize
//     (actual disk blocks, quota accounting's own unit) instead of
//     GetDirSize's apparent-size filepath.Walk -- see GetDirAllocatedSize's
//     doc comment (#94) for why apparent size is a biased proxy, in both
//     directions, for what a real backend charges.
//
// Same basePath/projectsFile/projidFile caveats as GetDirUsages apply here.
func GetDirUsagesStrict(basePath, fsType, projectsFile, projidFile string) ([]DirUsage, error) {
	var usages []DirUsage

	quotaMap := make(map[string]uint64)
	usageMap := make(map[string]uint64)
	var err error

	switch fsType {
	case "xfs":
		quotaMap, usageMap, err = quota.GetXFSQuotaReportStrict(basePath, projectsFile, projidFile)
	case "ext4":
		quotaMap, usageMap, err = quota.GetExt4QuotaReportStrict(basePath, projectsFile, projidFile)
	case "btrfs":
		quotaMap, usageMap, err = quota.GetBtrfsQuotaReport(basePath)
	}
	if err != nil {
		return nil, err
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

		// Get directory usage: prefer the report's own number; fall back to
		// an allocated-size walk (not GetDirSize's apparent size) for a
		// path the report has no entry for -- see doc comment above.
		var used uint64
		if u, ok := usageMap[dirPath]; ok {
			used = u
		} else {
			used = GetDirAllocatedSize(dirPath)
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
