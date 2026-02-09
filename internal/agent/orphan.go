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

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dasomel/nfs-quota-agent/internal/quota"
	"github.com/dasomel/nfs-quota-agent/internal/status"
	"github.com/dasomel/nfs-quota-agent/internal/ui"
	"github.com/dasomel/nfs-quota-agent/internal/util"
)

// runAutoCleanup runs the automatic orphan cleanup loop
func (a *QuotaAgent) runAutoCleanup(ctx context.Context) {
	slog.Info("Starting auto-cleanup loop",
		"interval", a.cleanupInterval,
		"gracePeriod", a.orphanGracePeriod,
		"dryRun", a.cleanupDryRun,
	)

	ticker := time.NewTicker(a.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.cleanupOrphans(ctx)
		}
	}
}

// cleanupOrphans finds and removes orphaned directories
func (a *QuotaAgent) cleanupOrphans(ctx context.Context) {
	orphans := a.findOrphans(ctx)
	if len(orphans) == 0 {
		slog.Debug("No orphaned directories found")
		return
	}

	slog.Info("Found orphaned directories", "count", len(orphans))

	cleaned := 0
	for _, orphan := range orphans {
		if !orphan.CanDelete {
			slog.Debug("Orphan still in grace period",
				"path", orphan.Path,
				"age", orphan.Age,
				"gracePeriod", a.orphanGracePeriod,
			)
			continue
		}

		if a.cleanupDryRun {
			slog.Info("[DRY-RUN] Would delete orphan",
				"path", orphan.Path,
				"size", orphan.SizeStr,
				"age", orphan.Age,
			)
		} else {
			if err := a.RemoveOrphan(orphan); err != nil {
				slog.Error("Failed to remove orphan",
					"path", orphan.Path,
					"error", err,
				)
			} else {
				slog.Info("Removed orphan directory",
					"path", orphan.Path,
					"size", orphan.SizeStr,
				)
				cleaned++

				if a.auditLogger != nil {
					a.auditLogger.LogCleanup(orphan.Path, orphan.DirName, 0, nil)
				}
			}
		}
	}

	if cleaned > 0 {
		slog.Info("Cleanup completed", "removed", cleaned, "total", len(orphans))
	}
}

// findOrphans finds directories without matching PVs
func (a *QuotaAgent) findOrphans(ctx context.Context) []ui.OrphanInfo {
	var orphans []ui.OrphanInfo

	pvList, err := a.client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		slog.Error("Failed to list PVs for orphan detection", "error", err)
		return orphans
	}

	validPaths := make(map[string]bool)
	for _, pv := range pvList.Items {
		nfsPath := a.getNFSPath(&pv)
		if nfsPath != "" {
			localPath := a.nfsPathToLocal(nfsPath)
			validPaths[localPath] = true
		}
	}

	entries, err := os.ReadDir(a.nfsBasePath)
	if err != nil {
		slog.Error("Failed to read base path", "error", err)
		return orphans
	}

	a.orphanMu.Lock()
	defer a.orphanMu.Unlock()

	now := time.Now()

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		name := entry.Name()
		if strings.HasPrefix(name, ".") || name == "projects" || name == "projid" {
			continue
		}

		dirPath := filepath.Join(a.nfsBasePath, name)

		subEntries, err := os.ReadDir(dirPath)
		if err != nil {
			continue
		}

		for _, subEntry := range subEntries {
			if !subEntry.IsDir() || strings.HasPrefix(subEntry.Name(), ".") {
				continue
			}

			subDirPath := filepath.Join(dirPath, subEntry.Name())
			if !validPaths[subDirPath] {
				orphan := a.trackOrphan(subDirPath, subEntry.Name(), now)
				if orphan != nil {
					orphans = append(orphans, *orphan)
				}
			}
		}

		if !validPaths[dirPath] {
			hasSubDirs := false
			for _, sub := range subEntries {
				if sub.IsDir() && !strings.HasPrefix(sub.Name(), ".") {
					hasSubDirs = true
					break
				}
			}
			if !hasSubDirs {
				orphan := a.trackOrphan(dirPath, name, now)
				if orphan != nil {
					orphans = append(orphans, *orphan)
				}
			}
		}
	}

	for path := range a.orphanLastSeen {
		if validPaths[path] {
			delete(a.orphanLastSeen, path)
		}
	}

	return orphans
}

// trackOrphan tracks when an orphan was first seen
func (a *QuotaAgent) trackOrphan(path, dirName string, now time.Time) *ui.OrphanInfo {
	firstSeen, exists := a.orphanLastSeen[path]
	if !exists {
		a.orphanLastSeen[path] = now
		firstSeen = now
	}

	age := now.Sub(firstSeen)
	size := status.GetDirSize(path)

	return &ui.OrphanInfo{
		Path:      path,
		DirName:   dirName,
		Size:      size,
		SizeStr:   util.FormatBytes(int64(size)),
		FirstSeen: firstSeen,
		Age:       util.FormatDuration(age),
		CanDelete: age >= a.orphanGracePeriod,
	}
}

// RemoveOrphan removes an orphaned directory
func (a *QuotaAgent) RemoveOrphan(orphan ui.OrphanInfo) error {
	if a.fsType != "" {
		a.removeQuotaForPath(orphan.Path)
	}

	if err := os.RemoveAll(orphan.Path); err != nil {
		return fmt.Errorf("failed to remove directory: %w", err)
	}

	a.orphanMu.Lock()
	delete(a.orphanLastSeen, orphan.Path)
	a.orphanMu.Unlock()

	return nil
}

// removeQuotaForPath removes quota for a specific path
func (a *QuotaAgent) removeQuotaForPath(path string) {
	projectsData, err := os.ReadFile(a.projectsFile)
	if err != nil {
		return
	}

	var projectID string
	var projectName string

	for _, line := range strings.Split(string(projectsData), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 && parts[1] == path {
			projectID = parts[0]
			break
		}
	}

	if projectID == "" {
		return
	}

	projidData, err := os.ReadFile(a.projidFile)
	if err == nil {
		for _, line := range strings.Split(string(projidData), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 && parts[1] == projectID {
				projectName = parts[0]
				break
			}
		}
	}

	_ = quota.RemoveLineFromFile(a.projectsFile, projectID+":")

	if projectName != "" {
		_ = quota.RemoveLineFromFile(a.projidFile, projectName+":")
	}
}

// GetOrphans returns list of orphaned directories (for API)
func (a *QuotaAgent) GetOrphans(ctx context.Context) []ui.OrphanInfo {
	return a.findOrphans(ctx)
}
