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
	"strings"

	"github.com/dasomel/nfs-quota-agent/internal/util"
)

// GetXFSQuotaReport parses xfs_quota report
func GetXFSQuotaReport(basePath string) (map[string]uint64, map[string]uint64, error) {
	quotaMap := make(map[string]uint64)
	usageMap := make(map[string]uint64)

	cmd := xfsQuotaReportCommand(basePath)
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

// GetExt4QuotaReport parses repquota output
func GetExt4QuotaReport(basePath string) (map[string]uint64, map[string]uint64, error) {
	quotaMap := make(map[string]uint64)
	usageMap := make(map[string]uint64)

	cmd := ext4QuotaReportCommand(basePath)
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
