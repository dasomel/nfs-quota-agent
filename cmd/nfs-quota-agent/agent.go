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
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

const (
	// Annotation keys
	annotationProjectName = "nfs.io/project-name"
	annotationQuotaStatus = "nfs.io/quota-status"

	// Quota status values
	quotaStatusPending = "pending"
	quotaStatusApplied = "applied"
	quotaStatusFailed  = "failed"

	// Filesystem types
	fsTypeXFS  = "xfs"
	fsTypeExt4 = "ext4"
)

// QuotaAgent manages filesystem quotas for NFS PVs
type QuotaAgent struct {
	client          kubernetes.Interface
	nfsBasePath     string           // Base path where NFS is mounted (e.g., /export)
	nfsServerPath   string           // NFS server's base path (e.g., /data)
	provisionerName string           // Filter PVs by this provisioner
	processAllNFS   bool             // Process all NFS PVs regardless of provisioner
	quotaPath       string           // Mount point for quota commands
	fsType          string           // Filesystem type (xfs or ext4)
	projectsFile    string           // Path to projects file
	projidFile      string           // Path to projid file
	syncInterval    time.Duration    // How often to sync quotas
	mu              sync.Mutex       // Protects quota operations
	appliedQuotas   map[string]int64 // Track applied quotas: path -> bytes
}

// NewQuotaAgent creates a new QuotaAgent
func NewQuotaAgent(client kubernetes.Interface, nfsBasePath, nfsServerPath, provisionerName string) *QuotaAgent {
	return &QuotaAgent{
		client:          client,
		nfsBasePath:     nfsBasePath,
		nfsServerPath:   nfsServerPath,
		provisionerName: provisionerName,
		quotaPath:       nfsBasePath,
		projectsFile:    filepath.Join(nfsBasePath, "projects"),
		projidFile:      filepath.Join(nfsBasePath, "projid"),
		syncInterval:    30 * time.Second,
		appliedQuotas:   make(map[string]int64),
	}
}

// Run starts the quota agent
func (a *QuotaAgent) Run(ctx context.Context) error {
	// Detect filesystem type
	if err := a.detectFilesystemType(); err != nil {
		return fmt.Errorf("failed to detect filesystem type: %w", err)
	}

	slog.Info("Starting NFS Quota Agent",
		"nfsBasePath", a.nfsBasePath,
		"nfsServerPath", a.nfsServerPath,
		"provisionerName", a.provisionerName,
		"processAllNFS", a.processAllNFS,
		"fsType", a.fsType,
	)

	// Check if quota is available
	if err := a.checkQuotaAvailable(); err != nil {
		return fmt.Errorf("quota not available: %w", err)
	}

	// Load existing projects
	if err := a.loadProjects(); err != nil {
		slog.Warn("Failed to load existing projects", "error", err)
	}

	// Initial sync
	if err := a.syncAllQuotas(ctx); err != nil {
		slog.Error("Initial quota sync failed", "error", err)
	}

	// Start watching PVs
	go a.watchPVs(ctx)

	// Periodic sync
	ticker := time.NewTicker(a.syncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("Quota agent shutting down")
			return nil
		case <-ticker.C:
			if err := a.syncAllQuotas(ctx); err != nil {
				slog.Error("Periodic quota sync failed", "error", err)
			}
		}
	}
}

// detectFilesystemType detects the filesystem type of the quota path
func (a *QuotaAgent) detectFilesystemType() error {
	// Use 'df -T' to get filesystem type
	cmd := exec.Command("df", "-T", a.quotaPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to detect filesystem type: %w, output: %s", err, string(output))
	}

	outputStr := string(output)
	lines := strings.Split(outputStr, "\n")
	if len(lines) < 2 {
		return fmt.Errorf("unexpected df output: %s", outputStr)
	}

	// Parse the second line (first line is header)
	fields := strings.Fields(lines[1])
	if len(fields) < 2 {
		return fmt.Errorf("unexpected df output format: %s", lines[1])
	}

	fsType := strings.ToLower(fields[1])
	switch fsType {
	case "xfs":
		a.fsType = fsTypeXFS
	case "ext4":
		a.fsType = fsTypeExt4
	default:
		return fmt.Errorf("unsupported filesystem type: %s (only xfs and ext4 are supported)", fsType)
	}

	slog.Info("Detected filesystem type", "fsType", a.fsType, "path", a.quotaPath)
	return nil
}

// checkQuotaAvailable checks if quota commands are available for the detected filesystem
func (a *QuotaAgent) checkQuotaAvailable() error {
	switch a.fsType {
	case fsTypeXFS:
		return a.checkXFSQuotaAvailable()
	case fsTypeExt4:
		return a.checkExt4QuotaAvailable()
	default:
		return fmt.Errorf("unsupported filesystem type: %s", a.fsType)
	}
}

// loadProjects loads existing project mappings
func (a *QuotaAgent) loadProjects() error {
	// Projects file format: projectID:path
	data, err := os.ReadFile(a.projectsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Parse existing quotas if needed
	}

	slog.Info("Loaded existing projects", "count", len(lines))
	return nil
}

// syncAllQuotas syncs quotas for all matching PVs
func (a *QuotaAgent) syncAllQuotas(ctx context.Context) error {
	pvList, err := a.client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list PVs: %w", err)
	}

	syncedCount := 0
	for _, pv := range pvList.Items {
		if a.shouldProcessPV(&pv) {
			if err := a.ensureQuota(ctx, &pv); err != nil {
				slog.Error("Failed to ensure quota for PV", "pv", pv.Name, "error", err)
			} else {
				syncedCount++
			}
		}
	}

	slog.Debug("Quota sync completed", "synced", syncedCount, "total", len(pvList.Items))
	return nil
}

// shouldProcessPV checks if this PV should be processed by the agent
func (a *QuotaAgent) shouldProcessPV(pv *v1.PersistentVolume) bool {
	// Must be an NFS PV (never touch non-NFS PVs)
	if pv.Spec.NFS == nil {
		return false
	}

	// Must be in Bound state
	if pv.Status.Phase != v1.VolumeBound {
		return false
	}

	// If process-all-nfs is enabled, process all NFS PVs
	if a.processAllNFS {
		return true
	}

	// Otherwise, must be provisioned by our provisioner
	if pv.Annotations == nil {
		return false
	}

	provisioner := pv.Annotations["pv.kubernetes.io/provisioned-by"]
	if provisioner != a.provisionerName {
		return false
	}

	return true
}

// ensureQuota ensures the quota is applied for a PV
func (a *QuotaAgent) ensureQuota(ctx context.Context, pv *v1.PersistentVolume) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Get capacity from PV
	capacity, ok := pv.Spec.Capacity[v1.ResourceStorage]
	if !ok {
		return fmt.Errorf("PV %s has no storage capacity", pv.Name)
	}
	capacityBytes := capacity.Value()

	// Get NFS path and convert to local path
	nfsPath := pv.Spec.NFS.Path
	localPath := a.nfsPathToLocal(nfsPath)

	// Check if directory exists
	if _, err := os.Stat(localPath); os.IsNotExist(err) {
		slog.Warn("Directory does not exist, skipping quota", "path", localPath, "pv", pv.Name)
		return nil
	}

	// Check if quota already applied with same size
	if existingQuota, exists := a.appliedQuotas[localPath]; exists && existingQuota == capacityBytes {
		return nil // Already applied
	}

	// Generate project name from PV name
	projectName := a.getProjectName(pv)
	projectID := a.generateProjectID(projectName)

	// Apply quota based on filesystem type
	if err := a.applyQuota(localPath, projectName, projectID, capacityBytes); err != nil {
		// Update PV annotation to mark as failed
		a.updateQuotaStatus(ctx, pv, quotaStatusFailed)
		return err
	}

	// Track applied quota
	a.appliedQuotas[localPath] = capacityBytes

	// Update PV annotation to mark as applied
	a.updateQuotaStatus(ctx, pv, quotaStatusApplied)

	slog.Info("Quota applied successfully",
		"pv", pv.Name,
		"path", localPath,
		"capacity", formatBytes(capacityBytes),
	)

	return nil
}

// nfsPathToLocal converts NFS server path to local mount path
func (a *QuotaAgent) nfsPathToLocal(nfsPath string) string {
	// Replace NFS server path prefix with local mount path
	// e.g., /data/ns-pvc-xxx -> /export/ns-pvc-xxx
	if strings.HasPrefix(nfsPath, a.nfsServerPath) {
		return filepath.Join(a.nfsBasePath, strings.TrimPrefix(nfsPath, a.nfsServerPath))
	}
	return filepath.Join(a.nfsBasePath, filepath.Base(nfsPath))
}

// getProjectName gets or generates project name for a PV
func (a *QuotaAgent) getProjectName(pv *v1.PersistentVolume) string {
	if pv.Annotations != nil {
		if name, ok := pv.Annotations[annotationProjectName]; ok && name != "" {
			return name
		}
	}
	// Generate from PV name
	name := strings.ReplaceAll(pv.Name, "-", "_")
	if len(name) > 32 {
		name = name[:32]
	}
	return "pv_" + name
}

// generateProjectID generates a numeric project ID from project name
func (a *QuotaAgent) generateProjectID(projectName string) uint32 {
	// Simple hash function to generate project ID
	var hash uint32 = 2166136261
	for _, c := range projectName {
		hash ^= uint32(c)
		hash *= 16777619
	}
	// Ensure ID is in valid range (1-4294967294)
	return (hash % 4294967293) + 1
}

// applyQuota applies project quota based on filesystem type
func (a *QuotaAgent) applyQuota(path, projectName string, projectID uint32, sizeBytes int64) error {
	switch a.fsType {
	case fsTypeXFS:
		return a.applyXFSQuota(path, projectName, projectID, sizeBytes)
	case fsTypeExt4:
		return a.applyExt4Quota(path, projectName, projectID, sizeBytes)
	default:
		return fmt.Errorf("unsupported filesystem type: %s", a.fsType)
	}
}

// addProject adds a project to the projects and projid files
func (a *QuotaAgent) addProject(path, projectName string, projectID uint32) error {
	// Add to projid file: projectName:projectID
	projidEntry := fmt.Sprintf("%s:%d\n", projectName, projectID)
	if err := a.appendToFile(a.projidFile, projidEntry, projectName); err != nil {
		return err
	}

	// Add to projects file: projectID:path
	projectsEntry := fmt.Sprintf("%d:%s\n", projectID, path)
	if err := a.appendToFile(a.projectsFile, projectsEntry, strconv.FormatUint(uint64(projectID), 10)); err != nil {
		return err
	}

	return nil
}

// appendToFile appends an entry to a file if it doesn't already exist
func (a *QuotaAgent) appendToFile(filename, entry, searchKey string) error {
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

// updateQuotaStatus updates the quota status annotation on the PV
func (a *QuotaAgent) updateQuotaStatus(ctx context.Context, pv *v1.PersistentVolume, status string) {
	// Get fresh PV
	freshPV, err := a.client.CoreV1().PersistentVolumes().Get(ctx, pv.Name, metav1.GetOptions{})
	if err != nil {
		slog.Error("Failed to get PV for status update", "pv", pv.Name, "error", err)
		return
	}

	if freshPV.Annotations == nil {
		freshPV.Annotations = make(map[string]string)
	}
	freshPV.Annotations[annotationQuotaStatus] = status

	_, err = a.client.CoreV1().PersistentVolumes().Update(ctx, freshPV, metav1.UpdateOptions{})
	if err != nil {
		slog.Error("Failed to update PV quota status", "pv", pv.Name, "error", err)
	}
}

// watchPVs watches for PV changes
func (a *QuotaAgent) watchPVs(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		watcher, err := a.client.CoreV1().PersistentVolumes().Watch(ctx, metav1.ListOptions{})
		if err != nil {
			slog.Error("Failed to start PV watch", "error", err)
			time.Sleep(5 * time.Second)
			continue
		}

		for event := range watcher.ResultChan() {
			pv, ok := event.Object.(*v1.PersistentVolume)
			if !ok {
				continue
			}

			switch event.Type {
			case watch.Added, watch.Modified:
				if a.shouldProcessPV(pv) {
					if err := a.ensureQuota(ctx, pv); err != nil {
						slog.Error("Failed to ensure quota", "pv", pv.Name, "error", err)
					}
				}
			case watch.Deleted:
				// Quota will be automatically removed when directory is deleted
				a.mu.Lock()
				localPath := a.nfsPathToLocal(pv.Spec.NFS.Path)
				delete(a.appliedQuotas, localPath)
				a.mu.Unlock()
				slog.Debug("PV deleted, quota tracking removed", "pv", pv.Name)
			}
		}

		slog.Warn("PV watch ended, restarting...")
		time.Sleep(1 * time.Second)
	}
}

// removeQuota removes quota for a path
func (a *QuotaAgent) removeQuota(projectID uint32) error {
	var cmd *exec.Cmd

	switch a.fsType {
	case fsTypeXFS:
		// Set quota to 0 (unlimited) for XFS
		cmd = exec.Command("xfs_quota", "-x", "-c",
			fmt.Sprintf("limit -p bhard=0 %d", projectID),
			a.quotaPath)
	case fsTypeExt4:
		// Set quota to 0 (unlimited) for ext4
		cmd = exec.Command("setquota", "-P",
			fmt.Sprintf("%d", projectID),
			"0", "0", "0", "0",
			a.quotaPath)
	default:
		return fmt.Errorf("unsupported filesystem type: %s", a.fsType)
	}

	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to remove quota: %w, output: %s", err, string(output))
	}
	return nil
}
