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
	"log/slog"
	"os/exec"
	"strings"
)

// checkExt4QuotaAvailable checks if quota tools are available for ext4
func (a *QuotaAgent) checkExt4QuotaAvailable() error {
	// Check if quotactl/setquota command is available
	cmd := exec.Command("setquota", "-V")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("setquota command not found (install quota package): %w", err)
	}

	// Check if project quota is enabled by checking mount options
	cmd = exec.Command("findmnt", "-n", "-o", "OPTIONS", a.quotaPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		slog.Warn("Failed to check mount options", "error", err)
	} else {
		mountOpts := string(output)
		if !strings.Contains(mountOpts, "prjquota") {
			slog.Warn("Project quota may not be enabled (prjquota mount option not found)", "mountOpts", mountOpts)
		}
	}

	slog.Info("ext4 quota tools available")
	return nil
}

// applyExt4Quota applies ext4 project quota
func (a *QuotaAgent) applyExt4Quota(path, projectName string, projectID uint32, sizeBytes int64) error {
	// 1. Add project to projects file
	if err := a.addProject(path, projectName, projectID); err != nil {
		return fmt.Errorf("failed to add project: %w", err)
	}

	// 2. Set the project attribute on the directory using chattr
	// This associates the directory with the project ID
	cmd := exec.Command("chattr", "-R", "+P", fmt.Sprintf("-p %d", projectID), path)
	if output, err := cmd.CombinedOutput(); err != nil {
		// Try alternative: use tune2fs project id setting
		slog.Debug("chattr failed, trying alternative method", "error", err, "output", string(output))

		// Use e4defrag or similar to set project ID - fallback to quota tool
		cmd = exec.Command("sh", "-c",
			fmt.Sprintf("find %s -exec chattr +P -p %d {} \\; 2>/dev/null || true", path, projectID))
		if _, err := cmd.CombinedOutput(); err != nil {
			slog.Warn("Failed to set project attribute", "path", path, "error", err)
		}
	}

	// 3. Set the quota limit using setquota
	// Convert bytes to KB (setquota uses KB for block limits)
	sizeKB := sizeBytes / 1024
	if sizeKB == 0 {
		sizeKB = 1
	}

	// setquota -P <project_id> <block-softlimit> <block-hardlimit> <inode-softlimit> <inode-hardlimit> <filesystem>
	// We set block hard limit only (soft limit = 0 means no soft limit, inode limits = 0 means no inode limits)
	cmd = exec.Command("setquota", "-P",
		fmt.Sprintf("%d", projectID),
		"0",                       // block soft limit (0 = no limit)
		fmt.Sprintf("%d", sizeKB), // block hard limit in KB
		"0",                       // inode soft limit
		"0",                       // inode hard limit
		a.quotaPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to set quota limit: %w, output: %s", err, string(output))
	}

	slog.Debug("ext4 quota applied",
		"path", path,
		"projectName", projectName,
		"projectID", projectID,
		"sizeKB", sizeKB,
	)

	return nil
}
