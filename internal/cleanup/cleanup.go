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

// Package cleanup provides functions to identify and remove orphaned filesystem quotas
// that no longer have a corresponding Kubernetes PersistentVolume. It supports dry-run options,
// interactive confirmations, and CLI execution.
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

	"github.com/dasomel/nfs-quota-agent/internal/pvpath"
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

// SkippedQuota represents a project that was deliberately left alone
// instead of being classified orphaned, along with why.
type SkippedQuota struct {
	ProjectID   string
	ProjectName string
	Path        string
	Reason      string
}

// Result contains the cleanup operation results
type Result struct {
	ScannedCount          int
	ActiveCount           int
	OrphanedCount         int
	SkippedAmbiguousCount int
	CleanedCount          int
	FailedCount           int
	Orphans               []OrphanedQuota
	Skipped               []SkippedQuota
}

// activeSets holds, for a snapshot of PVs, the local paths we can confidently
// say are active and the local paths whose mapping is too ambiguous to trust
// either way (fallback-derived NFS path mapping, or two distinct PVs
// resolving to the same local path). Ambiguous paths are never treated as
// safe to delete, even though they are also not confirmed active.
type activeSets struct {
	active    map[string]bool
	ambiguous map[string]bool
}

// buildActiveSets lists the current PVs and classifies each into confidently
// active or ambiguous local paths using pvpath, the single source of truth
// also used by internal/agent and internal/ui.
func buildActiveSets(ctx context.Context, client kubernetes.Interface, nfsServerPath, basePath string) (*activeSets, error) {
	pvList, err := client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list PVs: %w", err)
	}

	sets := &activeSets{
		active:    make(map[string]bool),
		ambiguous: make(map[string]bool),
	}
	owner := make(map[string]string) // local path -> PV name that confidently claimed it

	for i := range pvList.Items {
		pv := &pvList.Items[i]
		nfsPath := pvpath.NFSPath(pv)
		if nfsPath == "" {
			continue
		}

		local := pvpath.ToLocal(nfsPath, nfsServerPath, basePath)
		localPath := filepath.Clean(local.Path)

		if local.Fallback {
			// Fallback-derived mapping: nfsPath didn't match nfsServerPath,
			// so localPath is only a basename guess. Multiple distinct NFS
			// paths could collide on it, so we can't trust it to prove
			// either liveness or orphan status for that path.
			sets.ambiguous[localPath] = true
			continue
		}

		if prevOwner, seen := owner[localPath]; seen && prevOwner != pv.Name {
			// Two distinct active PVs map to the same local path — a
			// configuration inconsistency we should not paper over by
			// guessing which one is "real".
			sets.ambiguous[localPath] = true
			delete(sets.active, localPath)
			continue
		}

		owner[localPath] = pv.Name
		sets.active[localPath] = true
	}

	// An ambiguous path can never simultaneously be treated as confidently
	// active, even if some other PV happened to map to it cleanly.
	for p := range sets.ambiguous {
		delete(sets.active, p)
	}

	return sets, nil
}

// classifyResult is what scanning one project entry from /etc/projects
// decided.
type classifyResult int

const (
	classifyActive classifyResult = iota
	classifyAmbiguous
	classifyOutsideRoot
	classifyOrphaned
)

// classifyProject decides what to do with one project path given the
// current active/ambiguous sets and the cleanup root. It never classifies a
// path as orphaned unless it is confidently outside both the active and
// ambiguous sets and confidently inside basePath.
func classifyProject(sets *activeSets, basePath, projectPath string) (classifyResult, string) {
	cleanPath := filepath.Clean(projectPath)

	if sets.active[cleanPath] {
		return classifyActive, ""
	}
	if sets.ambiguous[cleanPath] {
		return classifyAmbiguous, "path matches an ambiguous NFS-to-local mapping (fallback-derived or claimed by multiple PVs); refusing to guess"
	}
	if !pvpath.Contains(basePath, cleanPath) {
		return classifyOutsideRoot, fmt.Sprintf("path resolves outside the cleanup root %s; refusing to act on it", basePath)
	}
	return classifyOrphaned, ""
}

// RunCleanup performs the cleanup operation. It returns a Result summarizing
// what was scanned, found active/orphaned/skipped, and (if not a dry-run)
// actually removed.
func RunCleanup(basePath, nfsServerPath, kubeconfig string, dryRun, force bool) (*Result, error) {
	fmt.Printf("NFS Quota Cleanup\n")
	fmt.Printf("=================\n\n")
	fmt.Printf("Path: %s\n", basePath)
	fmt.Printf("NFS server path: %s\n", nfsServerPath)
	fmt.Printf("Mode: %s\n\n", map[bool]string{true: "DRY-RUN (no changes)", false: "LIVE"}[dryRun])

	fsType, err := quota.DetectFSType(basePath)
	if err == nil && fsType == "btrfs" {
		fmt.Println("Btrfs uses qgroup quotas and does not use projects/projid files. Auto-cleanup is not supported for Btrfs currently.")
		return &Result{}, nil
	}

	var config *rest.Config

	if kubeconfig != "" {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		config, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes config: %w", err)
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	basePath = filepath.Clean(basePath)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sets, err := buildActiveSets(ctx, client, nfsServerPath, basePath)
	if err != nil {
		// Fail-closed: if we can't confirm what's active, we make no
		// removal decisions at all.
		return nil, err
	}

	fmt.Printf("Found %d confidently active NFS PersistentVolume path(s), %d ambiguous\n\n", len(sets.active), len(sets.ambiguous))

	projectsFile := filepath.Join(basePath, "projects")
	projidFile := filepath.Join(basePath, "projid")

	projects, err := quota.ReadProjectsFile(projectsFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read projects file: %w", err)
	}

	projids, err := quota.ReadProjidFile(projidFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read projid file: %w", err)
	}

	var orphans []OrphanedQuota
	var skipped []SkippedQuota

	for projectID, projectPath := range projects {
		decision, reason := classifyProject(sets, basePath, projectPath)
		switch decision {
		case classifyActive:
			continue
		case classifyAmbiguous, classifyOutsideRoot:
			skipped = append(skipped, SkippedQuota{
				ProjectID:   projectID,
				ProjectName: projids[projectID],
				Path:        projectPath,
				Reason:      reason,
			})
			continue
		}

		dirExists := false
		var dirSize uint64
		if info, err := os.Stat(projectPath); err == nil && info.IsDir() {
			dirExists = true
			dirSize = status.GetDirSize(projectPath)
		}

		orphans = append(orphans, OrphanedQuota{
			ProjectID:   projectID,
			ProjectName: projids[projectID],
			Path:        projectPath,
			DirExists:   dirExists,
			DirSize:     dirSize,
		})
	}

	result := Result{
		ScannedCount:          len(projects),
		ActiveCount:           len(sets.active),
		OrphanedCount:         len(orphans),
		SkippedAmbiguousCount: len(skipped),
		Orphans:               orphans,
		Skipped:               skipped,
	}

	if len(skipped) > 0 {
		fmt.Printf("Skipped %d project(s) instead of guessing:\n\n", len(skipped))
		for _, s := range skipped {
			fmt.Printf("  [SKIP] %s (%s) %s — %s\n", s.ProjectID, s.ProjectName, s.Path, s.Reason)
		}
		fmt.Println()
	}

	if len(orphans) == 0 {
		fmt.Println("No orphaned quotas found.")
		return &result, nil
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
		return &result, nil
	}

	if !force {
		fmt.Print("Remove orphaned quotas? [y/N]: ")
		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		answer = strings.TrimSpace(strings.ToLower(answer))
		if answer != "y" && answer != "yes" {
			fmt.Println("Cleanup cancelled.")
			return &result, nil
		}
	}

	fmt.Println("\nCleaning up orphaned quotas...")

	fsType, err = quota.DetectFSType(basePath)
	if err != nil {
		return nil, fmt.Errorf("failed to detect filesystem: %w", err)
	}

	// Final pre-delete validation: re-list PVs right before actually
	// touching anything, even in force mode. PV state can change between
	// the scan above and now, and a project that was orphaned a moment ago
	// may have become active in the meantime.
	revalidated, err := buildActiveSets(ctx, client, nfsServerPath, basePath)
	if err != nil {
		fmt.Printf("  [ERROR] Final pre-delete validation failed (%v); skipping all removals.\n", err)
		for _, o := range orphans {
			skipped = append(skipped, SkippedQuota{
				ProjectID:   o.ProjectID,
				ProjectName: o.ProjectName,
				Path:        o.Path,
				Reason:      fmt.Sprintf("final pre-delete validation failed: %v", err),
			})
		}
		result.SkippedAmbiguousCount = len(skipped)
		result.Skipped = skipped
		return &result, nil
	}

	cleaned := 0
	failed := 0
	for _, o := range orphans {
		decision, reason := classifyProject(revalidated, basePath, o.Path)
		if decision != classifyOrphaned {
			if reason == "" {
				reason = "path became active since the initial scan"
			}
			fmt.Printf("  [SKIP] %s (%s) %s — %s\n", o.ProjectID, o.ProjectName, o.Path, reason)
			skipped = append(skipped, SkippedQuota{
				ProjectID:   o.ProjectID,
				ProjectName: o.ProjectName,
				Path:        o.Path,
				Reason:      reason,
			})
			continue
		}

		projectID := o.ProjectID

		if err := quota.RemoveQuotaByID(basePath, fsType, projectID); err != nil {
			fmt.Printf("  [ERROR] Failed to remove quota for %s: %v\n", projectID, err)
			failed++
			continue
		}

		if err := quota.RemoveLineFromFile(projectsFile, projectID+":"); err != nil {
			fmt.Printf("  [WARN] Failed to update projects file: %v\n", err)
		}

		if err := quota.RemoveLineFromFile(projidFile, o.ProjectName+":"); err != nil {
			fmt.Printf("  [WARN] Failed to update projid file: %v\n", err)
		}

		fmt.Printf("  [OK] Removed quota for project %s (%s) — confirmed still orphaned at delete time\n", projectID, o.ProjectName)
		cleaned++
	}

	skippedAtDelete := len(orphans) - cleaned - failed

	result.CleanedCount = cleaned
	result.FailedCount = failed
	result.SkippedAmbiguousCount = len(skipped)
	result.Skipped = skipped

	fmt.Printf("\nCleanup complete: %d/%d orphaned quotas removed (%d skipped at delete time, %d failed)\n", cleaned, len(orphans), skippedAtDelete, failed)

	return &result, nil
}
