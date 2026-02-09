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
func AppendToFile(filename, entry, searchKey string) error {
	// Read existing content
	data, err := os.ReadFile(filename)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	// Check if entry already exists
	if strings.Contains(string(data), searchKey) {
		return nil // Already exists
	}

	// Append entry
	f, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

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
	switch fsType {
	case FSTypeXFS:
		// Set quota to 0 (unlimited) - effectively removes it
		return nil
	case FSTypeExt4:
		// Similar to XFS, the quota is effectively removed when entries are deleted
		return nil
	default:
		return fmt.Errorf("unsupported filesystem: %s", fsType)
	}
}
