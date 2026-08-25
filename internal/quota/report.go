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

package quota

import (
	"os"
	"strconv"
	"strings"

	"github.com/dasomel/nfs-quota-agent/internal/util"
)

// ExpectedEnforcedBytes returns the on-disk hard limit ApplyXFSQuota/
// ApplyExt4Quota/ApplyBtrfsQuota actually asks the kernel to enforce for
// sizeBytes, given fsType -- not necessarily sizeBytes itself. XFS and
// ext4 quota tooling both operate in whole KB (`bhard=%dk` / setquota's KB
// hard limit column), flooring sizeBytes to the nearest KB below it and
// enforcing a 1KB floor for any nonzero request smaller than that; btrfs
// qgroup limits are set in raw bytes with no such rounding. A caller
// verifying on-disk state after an apply (agent.go's verifyQuotaOnDisk,
// #10) must compare against this, not the raw requested sizeBytes -- doing
// otherwise makes every PV whose capacity isn't already a 1024-byte
// multiple (e.g. any decimal-SI `storage: 1G` = 1000000000 bytes) look
// like a permanent verification failure despite being applied correctly.
func ExpectedEnforcedBytes(fsType string, sizeBytes int64) int64 {
	switch fsType {
	case FSTypeXFS, FSTypeExt4:
		sizeKB := sizeBytes / 1024
		if sizeKB == 0 {
			sizeKB = 1
		}
		return sizeKB * 1024
	default:
		return sizeBytes
	}
}

// GetXFSQuotaReport parses xfs_quota report. projectsFile and projidFile
// are read directly (not through defaultRunner) to resolve project
// name/ID to filesystem path -- callers must pass the same paths the
// agent applies quotas against (a.projectsFile/a.projidFile), not assume
// the standard /etc/projects and /etc/projid; a caller with no
// configurable paths of its own (e.g. the status/UI reporting path) can
// pass those two literals directly.
func GetXFSQuotaReport(basePath, projectsFile, projidFile string) (map[string]uint64, map[string]uint64, error) {
	if err := validateQuotaArg("basePath", basePath); err != nil {
		return nil, nil, err
	}

	quotaMap := make(map[string]uint64)
	usageMap := make(map[string]uint64)

	output, err := defaultRunner.Run("xfs_quota", "-x", "-c", "report -p -b", basePath)
	if err != nil {
		return quotaMap, usageMap, err
	}

	// Parse projid file to get projectName -> projectID mapping
	projidMap := make(map[string]string) // projectName -> projectID
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
		if used, err := util.ParseSize(fields[1]); err == nil {
			usageMap[path] = used * 1024
		}
		// Hard limit is in KB
		if len(fields) >= 4 {
			if hard, err := util.ParseSize(fields[3]); err == nil && hard > 0 {
				quotaMap[path] = hard * 1024
			}
		}
	}

	return quotaMap, usageMap, nil
}

// GetExt4QuotaReport parses repquota output. projectsFile is read
// directly (not basePath) to resolve project ID to filesystem path -- see
// GetXFSQuotaReport's doc comment for why callers must pass the agent's
// configured path rather than assume /etc/projects.
func GetExt4QuotaReport(basePath, projectsFile string) (map[string]uint64, map[string]uint64, error) {
	if err := validateQuotaArg("basePath", basePath); err != nil {
		return nil, nil, err
	}

	quotaMap := make(map[string]uint64)
	usageMap := make(map[string]uint64)

	output, err := defaultRunner.Run("repquota", "-P", basePath)
	if err != nil {
		return quotaMap, usageMap, err
	}

	// Parse projects file (use projectsFile, not basePath)
	projectPaths := make(map[string]string)
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

		// Skip header/separator lines. Real repquota -P project rows
		// themselves start with "#<id>" (e.g. "#100      --     100 ..."),
		// so a bare HasPrefix(line, "#") here would drop every real data
		// row along with the header -- checked and fixed as part of #10:
		// this function had never actually resolved a real repquota row
		// to a path, since every row was silently skipped before
		// projectID was even computed.
		if fields[0] == "Project" || strings.HasPrefix(line, "-") {
			continue
		}

		projectID := strings.TrimPrefix(fields[0], "#")
		projectID = strings.TrimSuffix(projectID, "--")
		projectID = strings.TrimSuffix(projectID, "+-")
		projectID = strings.TrimSuffix(projectID, "-+")
		projectID = strings.TrimSuffix(projectID, "++")

		// The removed HasPrefix(line, "#") check above no longer filters
		// banner/comment lines out before they reach here -- make the
		// "this column must be a numeric project ID" assumption explicit
		// instead of relying entirely on an accidental map-lookup miss to
		// reject anything that isn't (unverified against every real
		// repquota banner variant; this guard is the cheap insurance).
		if _, err := strconv.ParseUint(projectID, 10, 32); err != nil {
			continue
		}

		if path, ok := projectPaths[projectID]; ok {
			// Used is in KB
			if used, err := util.ParseSize(fields[2]); err == nil {
				usageMap[path] = used * 1024
			}
			// Hard limit
			if len(fields) >= 5 {
				if hard, err := util.ParseSize(fields[4]); err == nil && hard > 0 {
					quotaMap[path] = hard * 1024
				}
			}
		}
	}

	return quotaMap, usageMap, nil
}
