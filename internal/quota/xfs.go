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
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

// CheckXFSQuotaAvailable checks if xfs_quota command is available
func CheckXFSQuotaAvailable(quotaPath string) error {
	cmd := exec.Command("xfs_quota", "-V")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("xfs_quota command not found: %w", err)
	}

	// Check if the filesystem supports project quotas
	cmd = exec.Command("xfs_quota", "-x", "-c", "state", quotaPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to check quota state: %w, output: %s", err, string(output))
	}

	if !strings.Contains(string(output), "Project quota state") {
		slog.Warn("Project quota may not be enabled", "output", string(output))
	}

	slog.Info("XFS quota is available")
	return nil
}

// ApplyXFSQuota applies XFS project quota
func ApplyXFSQuota(quotaPath, path, projectName string, projectID uint32, sizeBytes int64, projectsFile, projidFile string) error {
	// 1. Add project to projects file
	if err := AddProject(path, projectName, projectID, projectsFile, projidFile); err != nil {
		return fmt.Errorf("failed to add project: %w", err)
	}

	// 2. Initialize the project directory
	cmd := exec.Command("xfs_quota", "-x", "-c",
		fmt.Sprintf("project -s -p %s %d", path, projectID),
		quotaPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to initialize project: %w, output: %s", err, string(output))
	}

	// 3. Set the quota limit
	// Convert bytes to blocks (XFS uses 512-byte blocks for quota, but we'll use 1K blocks)
	sizeKB := sizeBytes / 1024
	if sizeKB == 0 {
		sizeKB = 1
	}

	cmd = exec.Command("xfs_quota", "-x", "-c",
		fmt.Sprintf("limit -p bhard=%dk %d", sizeKB, projectID),
		quotaPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to set quota limit: %w, output: %s", err, string(output))
	}

	slog.Debug("XFS quota applied",
		"path", path,
		"projectName", projectName,
		"projectID", projectID,
		"sizeKB", sizeKB,
	)

	return nil
}
