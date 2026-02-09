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
	"os/exec"
	"strings"
)

const (
	// FSTypeXFS is the XFS filesystem type
	FSTypeXFS = "xfs"
	// FSTypeExt4 is the ext4 filesystem type
	FSTypeExt4 = "ext4"
)

// DetectFSType detects filesystem type using df -T
func DetectFSType(path string) (string, error) {
	cmd := exec.Command("df", "-T", path)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(output), "\n")
	if len(lines) < 2 {
		return "", fmt.Errorf("unexpected df output")
	}

	// Handle long device names that wrap to multiple lines
	// Join all data lines (skip header) and parse as single line
	dataLine := strings.Join(lines[1:], " ")
	fields := strings.Fields(dataLine)
	if len(fields) < 2 {
		return "", fmt.Errorf("unexpected df output format")
	}

	// Field order: Filesystem, Type, 1K-blocks, Used, Available, Use%, Mounted on
	// Type is always the second field
	return strings.ToLower(fields[1]), nil
}

// DetectFSTypeWithFindmnt detects filesystem type using findmnt (more reliable)
func DetectFSTypeWithFindmnt(path string) (string, error) {
	cmd := exec.Command("findmnt", "-n", "-o", "FSTYPE", path)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Fallback to df -T
		return DetectFSType(path)
	}

	fsType := strings.ToLower(strings.TrimSpace(string(output)))
	if fsType == "" {
		return DetectFSType(path)
	}

	return fsType, nil
}
