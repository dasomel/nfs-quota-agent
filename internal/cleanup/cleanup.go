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

package cleanup

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

	"github.com/dasomel/nfs-quota-agent/internal/quota"
	"github.com/dasomel/nfs-quota-agent/internal/status"
	"github.com/dasomel/nfs-quota-agent/internal/util"
)

// OrphanedQuota represents a quota without corresponding PV
type OrphanedQuota struct {
	ProjectID   string
	ProjectName string
	Path        string
	DirExists   bool
	DirSize     uint64
}

// Result contains the cleanup operation results
type Result struct {
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pvList, err := client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list PVs: %w", err)
	}

	validPaths := make(map[string]bool)
	for _, pv := range pvList.Items {
		if pv.Spec.NFS != nil {
			dirName := filepath.Base(pv.Spec.NFS.Path)
			validPaths[dirName] = true
		}
	}

	fmt.Printf("Found %d NFS PersistentVolumes in cluster\n\n", len(validPaths))

	projectsFile := filepath.Join(basePath, "projects")
	projidFile := filepath.Join(basePath, "projid")

	projects, err := quota.ReadProjectsFile(projectsFile)
	if err != nil {
		return fmt.Errorf("failed to read projects file: %w", err)
	}

	projids, err := quota.ReadProjidFile(projidFile)
	if err != nil {
		return fmt.Errorf("failed to read projid file: %w", err)
	}

	var orphans []OrphanedQuota
	for projectID, projectPath := range projects {
		dirName := filepath.Base(projectPath)

		if !validPaths[dirName] {
			dirExists := false
			var dirSize uint64

			if info, err := os.Stat(projectPath); err == nil && info.IsDir() {
				dirExists = true
				dirSize = status.GetDirSize(projectPath)
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

	result := Result{
		ScannedCount:  len(projects),
		OrphanedCount: len(orphans),
		Orphans:       orphans,
	}

	if len(orphans) == 0 {
		fmt.Println("No orphaned quotas found.")
		return nil
	}

	fmt.Printf("Found %d orphaned quotas:\n\n", len(orphans))
	fmt.Printf("%-12s %-25s %-40s %s\n", "PROJECT_ID", "PROJECT_NAME", "PATH", "STATUS")
	fmt.Printf("%s\n", strings.Repeat("-", 90))

	for _, o := range orphans {
		st := "dir missing"
		if o.DirExists {
			st = fmt.Sprintf("dir exists (%s)", util.FormatBytes(int64(o.DirSize)))
		}

		name := o.ProjectName
		if len(name) > 25 {
			name = name[:22] + "..."
		}

		path := o.Path
		if len(path) > 40 {
			path = "..." + path[len(path)-37:]
		}

		fmt.Printf("%-12s %-25s %-40s %s\n", o.ProjectID, name, path, st)
	}
	fmt.Println()

	if dryRun {
		fmt.Println("Dry-run mode: No changes made.")
		fmt.Println("Run with --force to remove orphaned quotas.")
		return nil
	}

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

	fmt.Println("\nCleaning up orphaned quotas...")

	fsType, err := quota.DetectFSType(basePath)
	if err != nil {
		return fmt.Errorf("failed to detect filesystem: %w", err)
	}

	cleaned := 0
	for _, o := range orphans {
		projectID := o.ProjectID

		if err := quota.RemoveQuotaByID(basePath, fsType, projectID); err != nil {
			fmt.Printf("  [ERROR] Failed to remove quota for %s: %v\n", projectID, err)
			continue
		}

		if err := quota.RemoveLineFromFile(projectsFile, projectID+":"); err != nil {
			fmt.Printf("  [WARN] Failed to update projects file: %v\n", err)
		}

		if err := quota.RemoveLineFromFile(projidFile, o.ProjectName+":"); err != nil {
			fmt.Printf("  [WARN] Failed to update projid file: %v\n", err)
		}

		fmt.Printf("  [OK] Removed quota for project %s (%s)\n", projectID, o.ProjectName)
		cleaned++
	}

	result.CleanedCount = cleaned

	fmt.Printf("\nCleanup complete: %d/%d orphaned quotas removed\n", cleaned, len(orphans))

	return nil
}
