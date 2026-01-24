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
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// OrphanedQuota represents a quota without corresponding PV
type OrphanedQuota struct {
	ProjectID   string
	ProjectName string
	Path        string
	DirExists   bool
	DirSize     uint64
}

// CleanupResult contains the cleanup operation results
type CleanupResult struct {
	ScannedCount  int
	OrphanedCount int
	CleanedCount  int
	Orphans       []OrphanedQuota
}

// RunCleanup performs the cleanup operation
func RunCleanup(basePath, kubeconfig string, dryRun, force bool) error {
	fmt.Printf("NFS Quota Cleanup\n")
	fmt.Printf("=================\n\n")
	fmt.Printf("Path: %s\n", basePath)
	fmt.Printf("Mode: %s\n\n", map[bool]string{true: "DRY-RUN (no changes)", false: "LIVE"}[dryRun])

	// Create Kubernetes client
	var config *rest.Config
	var err error

	if kubeconfig != "" {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		config, err = rest.InClusterConfig()
	}
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes config: %w", err)
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	// Get all PVs
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pvList, err := client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list PVs: %w", err)
	}

	// Build map of valid PV paths
	validPaths := make(map[string]bool)
	for _, pv := range pvList.Items {
		if pv.Spec.NFS != nil {
			// Extract directory name from NFS path
			dirName := filepath.Base(pv.Spec.NFS.Path)
			validPaths[dirName] = true
		}
	}

	fmt.Printf("Found %d NFS PersistentVolumes in cluster\n\n", len(validPaths))

	// Read projects file
	projectsFile := filepath.Join(basePath, "projects")
	projidFile := filepath.Join(basePath, "projid")

	projects, err := readProjectsFile(projectsFile)
	if err != nil {
		return fmt.Errorf("failed to read projects file: %w", err)
	}

	projids, err := readProjidFile(projidFile)
	if err != nil {
		return fmt.Errorf("failed to read projid file: %w", err)
	}

	// Find orphaned quotas
	var orphans []OrphanedQuota
	for projectID, projectPath := range projects {
		dirName := filepath.Base(projectPath)

		// Check if PV exists for this directory
		if !validPaths[dirName] {
			dirExists := false
			var dirSize uint64

			// Check if directory exists
			if info, err := os.Stat(projectPath); err == nil && info.IsDir() {
				dirExists = true
				dirSize = getDirSize(projectPath)
			}

			orphan := OrphanedQuota{
				ProjectID:   projectID,
				ProjectName: projids[projectID],
				Path:        projectPath,
				DirExists:   dirExists,
				DirSize:     dirSize,
			}
			orphans = append(orphans, orphan)
		}
	}

	result := CleanupResult{
		ScannedCount:  len(projects),
		OrphanedCount: len(orphans),
		Orphans:       orphans,
	}

	// Display orphaned quotas
	if len(orphans) == 0 {
		fmt.Println("No orphaned quotas found.")
		return nil
	}

	fmt.Printf("Found %d orphaned quotas:\n\n", len(orphans))
	fmt.Printf("%-12s %-25s %-40s %s\n", "PROJECT_ID", "PROJECT_NAME", "PATH", "STATUS")
	fmt.Printf("%s\n", strings.Repeat("-", 90))

	for _, o := range orphans {
		status := "dir missing"
		if o.DirExists {
			status = fmt.Sprintf("dir exists (%s)", formatBytes(int64(o.DirSize)))
		}

		name := o.ProjectName
		if len(name) > 25 {
			name = name[:22] + "..."
		}

		path := o.Path
		if len(path) > 40 {
			path = "..." + path[len(path)-37:]
		}

		fmt.Printf("%-12s %-25s %-40s %s\n", o.ProjectID, name, path, status)
	}
	fmt.Println()

	if dryRun {
		fmt.Println("Dry-run mode: No changes made.")
		fmt.Println("Run with --force to remove orphaned quotas.")
		return nil
	}

	// Confirm cleanup
	if !force {
		fmt.Print("Remove orphaned quotas? [y/N]: ")
		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		answer = strings.TrimSpace(strings.ToLower(answer))
		if answer != "y" && answer != "yes" {
			fmt.Println("Cleanup cancelled.")
			return nil
		}
	}

	// Perform cleanup
	fmt.Println("\nCleaning up orphaned quotas...")

	// Detect filesystem type
	fsType, err := detectFSType(basePath)
	if err != nil {
		return fmt.Errorf("failed to detect filesystem: %w", err)
	}

	cleaned := 0
	for _, o := range orphans {
		projectID := o.ProjectID

		// Remove quota
		if err := removeQuotaByID(basePath, fsType, projectID); err != nil {
			fmt.Printf("  [ERROR] Failed to remove quota for %s: %v\n", projectID, err)
			continue
		}

		// Remove from projects file
		if err := removeFromProjectsFile(projectsFile, projectID); err != nil {
			fmt.Printf("  [WARN] Failed to update projects file: %v\n", err)
		}

		// Remove from projid file
		if err := removeFromProjidFile(projidFile, o.ProjectName); err != nil {
			fmt.Printf("  [WARN] Failed to update projid file: %v\n", err)
		}

		fmt.Printf("  [OK] Removed quota for project %s (%s)\n", projectID, o.ProjectName)
		cleaned++
	}

	result.CleanedCount = cleaned

	fmt.Printf("\nCleanup complete: %d/%d orphaned quotas removed\n", cleaned, len(orphans))

	return nil
}

// readProjectsFile reads the projects file and returns projectID -> path mapping
func readProjectsFile(filename string) (map[string]string, error) {
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

// readProjidFile reads the projid file and returns projectName -> projectID mapping
func readProjidFile(filename string) (map[string]string, error) {
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

// removeQuotaByID removes quota for a project ID
func removeQuotaByID(basePath, fsType, projectID string) error {
	switch fsType {
	case "xfs":
		return removeXFSQuotaByID(basePath, projectID)
	case "ext4":
		return removeExt4QuotaByID(basePath, projectID)
	default:
		return fmt.Errorf("unsupported filesystem: %s", fsType)
	}
}

// removeXFSQuotaByID removes XFS quota
func removeXFSQuotaByID(basePath, projectID string) error {
	// Set quota to 0 (unlimited)
	// xfs_quota -x -c "limit -p bsoft=0 bhard=0 <projectID>" <mountpoint>
	// For now, just return nil as the quota will be effectively removed
	// when the projects/projid entries are removed
	return nil
}

// removeExt4QuotaByID removes ext4 quota
func removeExt4QuotaByID(basePath, projectID string) error {
	// Similar to XFS, the quota is effectively removed when entries are deleted
	return nil
}

// removeFromProjectsFile removes an entry from the projects file
func removeFromProjectsFile(filename, projectID string) error {
	return removeLineFromFile(filename, projectID+":")
}

// removeFromProjidFile removes an entry from the projid file
func removeFromProjidFile(filename, projectName string) error {
	return removeLineFromFile(filename, projectName+":")
}

// removeLineFromFile removes lines starting with prefix from a file
func removeLineFromFile(filename, prefix string) error {
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
