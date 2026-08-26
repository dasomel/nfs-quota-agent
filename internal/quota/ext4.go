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
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
)

// CheckExt4QuotaAvailable checks if quota tools are available for ext4
func CheckExt4QuotaAvailable(quotaPath string) error {
	// Check if quotactl/setquota command is available
	if _, err := defaultRunner.Run("setquota", "-V"); err != nil {
		return fmt.Errorf("setquota command not found (install quota package): %w", err)
	}

	// Check if project quota is enabled by checking mount options
	output, err := defaultRunner.Run("findmnt", "-n", "-o", "OPTIONS", quotaPath)
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

// ApplyExt4Quota applies ext4 project quota
func ApplyExt4Quota(quotaPath, path, projectName string, projectID uint32, sizeBytes int64, projectsFile, projidFile string) error {
	if err := validateQuotaArg("path", path); err != nil {
		return err
	}
	if err := validateQuotaArg("projectName", projectName); err != nil {
		return err
	}

	// 1. Add project to projects file
	if err := AddProject(path, projectName, projectID, projectsFile, projidFile); err != nil {
		return fmt.Errorf("failed to add project: %w", err)
	}

	// 2. Set the project attribute on the directory using chattr
	// This associates the directory with the project ID. rootProjected
	// tracks whether path itself (not just some subdirectory) actually
	// got +P set -- previously this whole block swallowed every failure
	// unconditionally and always fell through to setquota, so setquota
	// could succeed (a real limit exists for projectID) while zero bytes
	// under path were actually accounted to it: usage would never count
	// against the limit at all. Since #10's read-back verification only
	// confirms the limit setquota reports, not the directory's actual
	// project binding, that combination has to be a hard error here --
	// otherwise "apply" can report success on a quota that structurally
	// can never be enforced.
	rootProjected := false
	if output, err := defaultRunner.Run("chattr", "-R", "+P", "-p", fmt.Sprintf("%d", projectID), path); err != nil {
		// Try alternative: use tune2fs project id setting
		slog.Debug("chattr failed, trying alternative method", "error", err, "output", string(output))

		// Use Go WalkDir instead of sh -c to avoid shell injection
		if walkErr := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // skip errors, continue walking
			}
			if d.IsDir() {
				if _, chErr := defaultRunner.Run("chattr", "+P", "-p", fmt.Sprintf("%d", projectID), p); chErr != nil {
					slog.Debug("chattr failed for entry", "path", p, "error", chErr)
				} else if p == path {
					rootProjected = true
				}
			}
			return nil
		}); walkErr != nil {
			slog.Warn("Failed to walk directory for chattr", "path", path, "error", walkErr)
		}
	} else {
		rootProjected = true
	}

	if !rootProjected {
		return fmt.Errorf("failed to associate project %d with directory %s via chattr (both the recursive attempt and the per-directory walk fallback failed on the target directory itself)", projectID, path)
	}

	// 3. Set the quota limit using setquota
	// Convert bytes to KB (setquota uses KB for block limits)
	sizeKB := sizeBytes / 1024
	if sizeKB == 0 {
		sizeKB = 1
	}

	// setquota -P <project_id> <block-softlimit> <block-hardlimit> <inode-softlimit> <inode-hardlimit> <filesystem>
	// We set block hard limit only (soft limit = 0 means no soft limit, inode limits = 0 means no inode limits)
	if output, err := defaultRunner.Run("setquota", "-P",
		fmt.Sprintf("%d", projectID),
		"0",                       // block soft limit (0 = no limit)
		fmt.Sprintf("%d", sizeKB), // block hard limit in KB
		"0",                       // inode soft limit
		"0",                       // inode hard limit
		quotaPath); err != nil {
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
