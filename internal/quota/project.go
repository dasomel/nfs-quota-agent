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

// Package quota implements commands to configure, update, and remove project quotas
// across various file systems like XFS, ext4, and Btrfs. It manages configurations
// in projects and projid files and invokes command line utilities to enforce limits.
package quota

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// AddProject adds a project to the projects and projid files
func AddProject(path, projectName string, projectID uint32, projectsFile, projidFile string) error {
	// Add to projid file: projectName:projectID
	projidEntry := fmt.Sprintf("%s:%d\n", projectName, projectID)
	if err := AppendToFile(projidFile, projidEntry, projectName); err != nil {
		return err
	}

	// Add to projects file: projectID:path
	projectsEntry := fmt.Sprintf("%d:%s\n", projectID, path)
	if err := AppendToFile(projectsFile, projectsEntry, strconv.FormatUint(uint64(projectID), 10)); err != nil {
		return err
	}

	return nil
}

// AppendToFile appends an entry to a file if it doesn't already exist
func AppendToFile(filename, entry, searchKey string) (err error) {
	// Read existing content
	data, err := os.ReadFile(filename)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	// Check if entry already exists by looking for searchKey at the start of any line
	prefix := searchKey + ":"
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) || line == searchKey {
			return nil // Already exists
		}
	}

	// Append entry
	f, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()

	_, err = f.WriteString(entry)
	return err
}

// RemoveLineFromFile removes lines starting with prefix from a file
func RemoveLineFromFile(filename, prefix string) error {
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}

	var newLines []string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, prefix) {
			newLines = append(newLines, line)
		}
	}

	return os.WriteFile(filename, []byte(strings.Join(newLines, "\n")), 0644)
}

// ReadProjectsFile reads the projects file and returns projectID -> path mapping
func ReadProjectsFile(filename string) (map[string]string, error) {
	result := make(map[string]string)

	data, err := os.ReadFile(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return nil, err
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			result[parts[0]] = parts[1]
		}
	}

	return result, nil
}

// ReadProjidFile reads the projid file and returns projectID -> projectName mapping
func ReadProjidFile(filename string) (map[string]string, error) {
	result := make(map[string]string)

	data, err := os.ReadFile(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return nil, err
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			// projectName:projectID -> projectID:projectName
			result[parts[1]] = parts[0]
		}
	}

	return result, nil
}

// RemoveQuotaByID removes quota for a project ID
func RemoveQuotaByID(basePath, fsType, projectID string) error {
	if err := validateQuotaArg("basePath", basePath); err != nil {
		return err
	}

	switch fsType {
	case FSTypeXFS:
		// Set hard block limit to 0 (unlimited), effectively removing the quota
		if output, err := defaultRunner.Run("xfs_quota", "-x", "-c",
			fmt.Sprintf("limit -p bhard=0 %s", projectID),
			basePath); err != nil {
			return fmt.Errorf("failed to remove XFS quota for project %s: %w, output: %s", projectID, err, string(output))
		}
		return nil
	case FSTypeExt4:
		// Remove ext4 project quota by setting all limits to 0
		if output, err := defaultRunner.Run("setquota", "-P", projectID, "0", "0", "0", "0", basePath); err != nil {
			return fmt.Errorf("failed to remove ext4 quota for project %s: %w, output: %s", projectID, err, string(output))
		}
		return nil
	default:
		return fmt.Errorf("unsupported filesystem: %s", fsType)
	}
}
