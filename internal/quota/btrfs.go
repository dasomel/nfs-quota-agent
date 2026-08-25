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
	"path/filepath"
	"strconv"
	"strings"
)

// CheckBtrfsQuotaAvailable checks if btrfs binary is available and quotas are enabled
func CheckBtrfsQuotaAvailable(quotaPath string) error {
	if _, err := defaultRunner.Run("btrfs", "--version"); err != nil {
		return fmt.Errorf("btrfs command not found: %w", err)
	}

	output, err := defaultRunner.Run("btrfs", "qgroup", "show", quotaPath)
	if err != nil {
		outStr := string(output)
		if strings.Contains(outStr, "quotas not enabled") {
			return fmt.Errorf("btrfs quotas not enabled; please run 'btrfs quota enable %s' to enable quotas", quotaPath)
		}
		return fmt.Errorf("failed to check btrfs quota state: %w, output: %s", err, outStr)
	}

	slog.Info("Btrfs quota is available")
	return nil
}

// ApplyBtrfsQuota applies btrfs subvolume quota
func ApplyBtrfsQuota(path string, sizeBytes int64) error {
	// Require target dir to be a subvolume
	output, err := defaultRunner.Run("btrfs", "subvolume", "show", path)
	if err != nil {
		outStr := string(output)
		if strings.Contains(outStr, "Not a Btrfs subvolume") || strings.Contains(outStr, "not a subvolume") {
			return fmt.Errorf("path %s is not a btrfs subvolume; btrfs quota requires the target directory to be a subvolume", path)
		}
		return fmt.Errorf("failed to verify if path %s is a subvolume: %w, output: %s", path, err, outStr)
	}

	// Apply limit using btrfs qgroup limit
	limitStr := fmt.Sprintf("%d", sizeBytes)
	if output, err = defaultRunner.Run("btrfs", "qgroup", "limit", limitStr, path); err != nil {
		return fmt.Errorf("failed to set btrfs quota limit: %w, output: %s", err, string(output))
	}

	slog.Debug("Btrfs quota applied",
		"path", path,
		"sizeBytes", sizeBytes,
	)
	return nil
}

// GetBtrfsQuotaReport parses btrfs qgroup show report
func GetBtrfsQuotaReport(basePath string) (map[string]uint64, map[string]uint64, error) {
	output, err := defaultRunner.Run("btrfs", "qgroup", "show", "-re", "--raw", basePath)
	if err != nil {
		return make(map[string]uint64), make(map[string]uint64), fmt.Errorf("failed to run btrfs qgroup show: %w, output: %s", err, string(output))
	}

	quotaMap, usageMap := parseBtrfsQgroupShow(string(output), basePath)
	return quotaMap, usageMap, nil
}

// parseBtrfsQgroupShow parses `btrfs qgroup show -re --raw`'s stdout, given
// the basePath it was run against (relative paths in the output are joined
// against it). Pulled out of GetBtrfsQuotaReport as a pure function -- no
// command execution, only string parsing -- so it can be fuzzed directly:
// see FuzzParseBtrfsQgroupShow (#7, following the same reasoning already
// applied to validateQuotaArg's fuzz coverage).
func parseBtrfsQgroupShow(output, basePath string) (map[string]uint64, map[string]uint64) {
	quotaMap := make(map[string]uint64)
	usageMap := make(map[string]uint64)

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}

		// Skip header lines (like qgroupid, rfer, excl, max_rfer, max_excl, path)
		if fields[0] == "qgroupid" || strings.HasPrefix(fields[0], "-") {
			continue
		}

		// fields order: qgroupid, rfer, excl, max_rfer, max_excl, [path...]
		if len(fields) < 6 {
			continue
		}

		pathVal := strings.Join(fields[5:], " ")
		if strings.HasPrefix(pathVal, "<") && strings.HasSuffix(pathVal, ">") {
			continue
		}

		// If pathVal is "none" or empty, skip
		if pathVal == "" || pathVal == "none" {
			continue
		}

		// Reconstruct full path. If pathVal is relative, join it with basePath
		var fullPath string
		if filepath.IsAbs(pathVal) {
			fullPath = pathVal
		} else {
			fullPath = filepath.Join(basePath, pathVal)
		}

		// parsed values
		rfer, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}

		var limit uint64
		if fields[3] != "none" {
			limit, err = strconv.ParseUint(fields[3], 10, 64)
			if err != nil {
				limit = 0
			}
		}

		usageMap[fullPath] = rfer
		if limit > 0 {
			quotaMap[fullPath] = limit
		}
	}

	return quotaMap, usageMap
}
