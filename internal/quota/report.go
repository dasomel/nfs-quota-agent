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
	return getXFSQuotaReport(basePath, projectsFile, projidFile, false)
}

// GetXFSQuotaReportStrict is GetXFSQuotaReport for callers that cannot
// safely treat an unreadable project mapping as an empty quota report. It
// returns errors from both configured mapping files after the report command
// succeeds; the non-strict function keeps its best-effort reporting behavior.
func GetXFSQuotaReportStrict(basePath, projectsFile, projidFile string) (map[string]uint64, map[string]uint64, error) {
	return getXFSQuotaReport(basePath, projectsFile, projidFile, true)
}

func getXFSQuotaReport(basePath, projectsFile, projidFile string, strict bool) (map[string]uint64, map[string]uint64, error) {
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
	if data, err := os.ReadFile(projidFile); err != nil {
		if strict {
			return quotaMap, usageMap, fmt.Errorf("read projid file %q: %w", projidFile, err)
		}
	} else {
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
	if data, err := os.ReadFile(projectsFile); err != nil {
		if strict {
			return quotaMap, usageMap, fmt.Errorf("read projects file %q: %w", projectsFile, err)
		}
	} else {
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

	quotaMap, usageMap = parseXFSQuotaReportOutput(output, nameToPaths, projectPaths)
	return quotaMap, usageMap, nil
}

// parseXFSQuotaReportOutput parses `xfs_quota -x -c "report -p -b"`'s stdout
// into (quotaMap, usageMap) keyed by filesystem path, resolving each row's
// project name or ID against nameToPaths/projectPaths (built from
// projid/projects file content by GetXFSQuotaReport). Pure function, no
// I/O -- split out so it can be fuzzed directly against arbitrary tool
// output (#7), the same treatment PR #65 already gave
// parseBtrfsQgroupShow.
func parseXFSQuotaReportOutput(output []byte, nameToPaths, projectPaths map[string]string) (quotaMap, usageMap map[string]uint64) {
	quotaMap = make(map[string]uint64)
	usageMap = make(map[string]uint64)

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

	return quotaMap, usageMap
}

// GetExt4QuotaReport parses repquota output. projectsFile and projidFile
// are read directly (not basePath) to resolve each row to a filesystem
// path -- see GetXFSQuotaReport's doc comment for why callers must pass
// the agent's configured paths rather than assume /etc/projects/
// /etc/projid.
//
// projidFile matters here in a way it didn't before PR #155: real
// `repquota -P` prints a row's *name* column (resolved via /etc/projid),
// not "#<id>", whenever that project ID has a name registered -- and
// AddProject always registers one for every quota this agent applies. So
// in practice every real row for an agent-managed path is name-keyed, not
// "#<id>"-keyed. Confirmed against real `repquota -P` output on ext4
// (mkfs.ext4 -O project,quota, mount -o prjquota) on a real kernel (colima
// VM, aarch64 Ubuntu 24.04 -- the same environment CLAUDE.md's ext4
// kernel-module gotcha used): applying a project quota by name "pv-e2e"
// and then running `repquota -P` printed the row as
// "pv-e2e    --       4       0  102400 ...", never "#<id>". The prior
// version of this function only recognized "#<id>" rows, so it silently
// dropped every row an agent apply actually produces -- read-back
// verification (agent.go's verifyQuotaOnDisk) failed unconditionally for
// every ext4 PV, 100% reproducibly, which is exactly what PR #155's
// Air-Gap E2E ext4 job hit.
func GetExt4QuotaReport(basePath, projectsFile, projidFile string) (map[string]uint64, map[string]uint64, error) {
	return getExt4QuotaReport(basePath, projectsFile, projidFile, false)
}

// GetExt4QuotaReportStrict is GetExt4QuotaReport for callers that cannot
// safely treat an unreadable project mapping as an empty quota report. The
// non-strict function keeps its best-effort reporting behavior.
func GetExt4QuotaReportStrict(basePath, projectsFile, projidFile string) (map[string]uint64, map[string]uint64, error) {
	return getExt4QuotaReport(basePath, projectsFile, projidFile, true)
}

func getExt4QuotaReport(basePath, projectsFile, projidFile string, strict bool) (map[string]uint64, map[string]uint64, error) {
	if err := validateQuotaArg("basePath", basePath); err != nil {
		return nil, nil, err
	}

	quotaMap := make(map[string]uint64)
	usageMap := make(map[string]uint64)

	output, err := defaultRunner.Run("repquota", "-P", basePath)
	if err != nil {
		return quotaMap, usageMap, err
	}

	// Parse projects file (use projectsFile, not basePath): projectID -> path
	projectPaths := make(map[string]string)
	if data, err := os.ReadFile(projectsFile); err != nil {
		if strict {
			return quotaMap, usageMap, fmt.Errorf("read projects file %q: %w", projectsFile, err)
		}
	} else {
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

	// Parse projid file: projectName -> projectID -- needed to resolve the
	// name-keyed rows real repquota -P actually emits (see doc comment
	// above). Read the same way GetXFSQuotaReport reads it.
	projidMap := make(map[string]string) // name -> id
	if data, err := os.ReadFile(projidFile); err != nil {
		if strict {
			return quotaMap, usageMap, fmt.Errorf("read projid file %q: %w", projidFile, err)
		}
	} else {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				projidMap[parts[0]] = parts[1]
			}
		}
	}

	// Build projectName -> path, same construction as GetXFSQuotaReport's
	// nameToPaths.
	nameToPaths := make(map[string]string)
	for name, id := range projidMap {
		if path, ok := projectPaths[id]; ok {
			nameToPaths[name] = path
		}
	}

	quotaMap, usageMap = parseExt4RepquotaOutput(output, projectPaths, nameToPaths)
	return quotaMap, usageMap, nil
}

// parseExt4RepquotaOutput parses `repquota -P`'s stdout into
// (quotaMap, usageMap) keyed by filesystem path, resolving each row
// against projectPaths (id -> path) when the row is "#<id>"-keyed, or
// nameToPaths (name -> path, built from projid+projects content by
// GetExt4QuotaReport) when it's name-keyed -- see GetExt4QuotaReport's doc
// comment for why the name-keyed case is the one real repquota -P output
// actually produces for an agent-managed project. Pure function, no I/O --
// split out so it can be fuzzed directly against arbitrary tool output
// (#7), the same treatment PR #65 already gave parseBtrfsQgroupShow.
func parseExt4RepquotaOutput(output []byte, projectPaths, nameToPaths map[string]string) (quotaMap, usageMap map[string]uint64) {
	quotaMap = make(map[string]uint64)
	usageMap = make(map[string]uint64)

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}

		// Skip header/separator lines. Real repquota -P project rows
		// themselves start with either "#<id>" (e.g. "#0        -- ...",
		// used when the ID has no /etc/projid name) or the project's name
		// (e.g. "pv-e2e    --       4 ..."), so a bare HasPrefix(line, "#")
		// here would drop every "#<id>" data row along with the header --
		// checked and fixed as part of #10.
		if fields[0] == "Project" || strings.HasPrefix(line, "-") {
			continue
		}

		var path string
		if strings.HasPrefix(fields[0], "#") {
			projectID := strings.TrimPrefix(fields[0], "#")
			projectID = strings.TrimSuffix(projectID, "--")
			projectID = strings.TrimSuffix(projectID, "+-")
			projectID = strings.TrimSuffix(projectID, "-+")
			projectID = strings.TrimSuffix(projectID, "++")

			// Make the "this column must be a numeric project ID" assumption
			// explicit instead of relying entirely on an accidental
			// map-lookup miss to reject anything that isn't (unverified
			// against every real repquota banner variant; this guard is the
			// cheap insurance).
			if _, err := strconv.ParseUint(projectID, 10, 32); err != nil {
				continue
			}
			p, ok := projectPaths[projectID]
			if !ok {
				continue
			}
			path = p
		} else {
			// Real repquota -P prints the project's /etc/projid name here
			// instead of "#<id>" whenever one is registered -- which
			// AddProject always does for a quota this agent applied. See
			// GetExt4QuotaReport's doc comment for the real-kernel evidence.
			p, ok := nameToPaths[fields[0]]
			if !ok {
				continue
			}
			path = p
		}

		// Used is in KB
		if used, err := util.ParseSize(fields[2]); err == nil {
			usageMap[path] = used * 1024
		}
		// Hard limit
		if hard, err := util.ParseSize(fields[4]); err == nil && hard > 0 {
			quotaMap[path] = hard * 1024
		}
	}

	return quotaMap, usageMap
}
