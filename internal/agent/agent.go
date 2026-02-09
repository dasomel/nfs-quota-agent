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
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/dasomel/nfs-quota-agent/internal/audit"
	"github.com/dasomel/nfs-quota-agent/internal/history"
	"github.com/dasomel/nfs-quota-agent/internal/quota"
	"github.com/dasomel/nfs-quota-agent/internal/status"
	"github.com/dasomel/nfs-quota-agent/internal/util"
)

const (
	// Annotation keys
	AnnotationProjectName = "nfs.io/project-name"
	AnnotationQuotaStatus = "nfs.io/quota-status"

	// Quota status values
	QuotaStatusPending = "pending"
	QuotaStatusApplied = "applied"
	QuotaStatusFailed  = "failed"
)

// QuotaAgent manages filesystem quotas for NFS PVs
type QuotaAgent struct {
	client          kubernetes.Interface
	nfsBasePath     string
	nfsServerPath   string
	provisionerName string
	processAllNFS   bool
	quotaPath       string
	fsType          string
	projectsFile    string
	projidFile      string
	syncInterval    time.Duration
	mu              sync.Mutex
	appliedQuotas   map[string]int64
	auditLogger     *audit.Logger

	// Auto-cleanup configuration
	enableAutoCleanup bool
	cleanupInterval   time.Duration
	orphanGracePeriod time.Duration
	cleanupDryRun     bool
	orphanLastSeen    map[string]time.Time
	orphanMu          sync.Mutex

	// History configuration
	historyStore *history.Store

	// Policy configuration
	enablePolicy    bool
	defaultQuota    int64
	enforceMaxQuota bool
}

// NewQuotaAgent creates a new QuotaAgent
func NewQuotaAgent(client kubernetes.Interface, nfsBasePath, nfsServerPath, provisionerName string) *QuotaAgent {
	return &QuotaAgent{
		client:            client,
		nfsBasePath:       nfsBasePath,
		nfsServerPath:     nfsServerPath,
		provisionerName:   provisionerName,
		quotaPath:         nfsBasePath,
		projectsFile:      "/etc/projects",
		projidFile:        "/etc/projid",
		syncInterval:      30 * time.Second,
		appliedQuotas:     make(map[string]int64),
		cleanupInterval:   1 * time.Hour,
		orphanGracePeriod: 24 * time.Hour,
		cleanupDryRun:     true,
		orphanLastSeen:    make(map[string]time.Time),
	}
}

// Setters for configuration

func (a *QuotaAgent) SetProcessAllNFS(v bool)                      { a.processAllNFS = v }
func (a *QuotaAgent) SetQuotaPath(v string)                        { a.quotaPath = v }
func (a *QuotaAgent) SetProjectsFile(v string)                     { a.projectsFile = v }
func (a *QuotaAgent) SetProjidFile(v string)                       { a.projidFile = v }
func (a *QuotaAgent) SetSyncInterval(v time.Duration)              { a.syncInterval = v }
func (a *QuotaAgent) SetAuditLogger(v *audit.Logger)               { a.auditLogger = v }
func (a *QuotaAgent) SetEnableAutoCleanup(v bool)                  { a.enableAutoCleanup = v }
func (a *QuotaAgent) SetCleanupIntervalDuration(v time.Duration)   { a.cleanupInterval = v }
func (a *QuotaAgent) SetOrphanGracePeriodDuration(v time.Duration) { a.orphanGracePeriod = v }
func (a *QuotaAgent) SetCleanupDryRunFlag(v bool)                  { a.cleanupDryRun = v }
func (a *QuotaAgent) SetHistoryStore(v *history.Store)             { a.historyStore = v }
func (a *QuotaAgent) SetEnablePolicy(v bool)                       { a.enablePolicy = v }
func (a *QuotaAgent) SetDefaultQuota(v int64)                      { a.defaultQuota = v }
func (a *QuotaAgent) SetEnforceMaxQuota(v bool)                    { a.enforceMaxQuota = v }

// Getters for UI/metrics interface

func (a *QuotaAgent) BasePath() string                 { return a.nfsBasePath }
func (a *QuotaAgent) EnableAutoCleanup() bool          { return a.enableAutoCleanup }
func (a *QuotaAgent) CleanupDryRun() bool              { return a.cleanupDryRun }
func (a *QuotaAgent) OrphanGracePeriod() time.Duration { return a.orphanGracePeriod }
func (a *QuotaAgent) CleanupInterval() time.Duration   { return a.cleanupInterval }
func (a *QuotaAgent) EnablePolicy() bool               { return a.enablePolicy }
func (a *QuotaAgent) AuditLogger() *audit.Logger       { return a.auditLogger }

func (a *QuotaAgent) AppliedQuotaCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.appliedQuotas)
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

	// Start auto-cleanup if enabled
	if a.enableAutoCleanup {
		go a.runAutoCleanup(ctx)
	}

	// Start history collection if enabled
	if a.historyStore != nil {
		go a.collectHistory(ctx)
	}

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
	fsType, err := quota.DetectFSTypeWithFindmnt(a.quotaPath)
	if err != nil {
		return err
	}

	switch fsType {
	case "xfs":
		a.fsType = quota.FSTypeXFS
	case "ext4":
		a.fsType = quota.FSTypeExt4
	default:
		return fmt.Errorf("unsupported filesystem type: %s (only xfs and ext4 are supported)", fsType)
	}

	slog.Info("Detected filesystem type", "fsType", a.fsType, "path", a.quotaPath)
	return nil
}

// checkQuotaAvailable checks if quota commands are available
func (a *QuotaAgent) checkQuotaAvailable() error {
	switch a.fsType {
	case quota.FSTypeXFS:
		return quota.CheckXFSQuotaAvailable(a.quotaPath)
	case quota.FSTypeExt4:
		return quota.CheckExt4QuotaAvailable(a.quotaPath)
	default:
		return fmt.Errorf("unsupported filesystem type: %s", a.fsType)
	}
}

// loadProjects loads existing project mappings
func (a *QuotaAgent) loadProjects() error {
	data, err := os.ReadFile(a.projectsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	lines := strings.Split(string(data), "\n")
	count := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			count++
		}
	}

	slog.Info("Loaded existing projects", "count", count)
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
	if pv.Status.Phase != v1.VolumeBound {
		return false
	}

	isNativeNFS := pv.Spec.NFS != nil
	isCSINFS := pv.Spec.CSI != nil && pv.Spec.CSI.Driver == a.provisionerName

	if !isNativeNFS && !isCSINFS {
		return false
	}

	if a.processAllNFS {
		return true
	}

	if isCSINFS {
		return true
	}

	if pv.Annotations == nil {
		return false
	}

	provisioner := pv.Annotations["pv.kubernetes.io/provisioned-by"]
	return provisioner == a.provisionerName
}

// getNFSPath extracts the NFS path from a PV
func (a *QuotaAgent) getNFSPath(pv *v1.PersistentVolume) string {
	if pv.Spec.NFS != nil {
		return pv.Spec.NFS.Path
	}

	if pv.Spec.CSI != nil && pv.Spec.CSI.VolumeAttributes != nil {
		share := pv.Spec.CSI.VolumeAttributes["share"]
		subdir := pv.Spec.CSI.VolumeAttributes["subDir"]
		if subdir == "" {
			subdir = pv.Spec.CSI.VolumeAttributes["subdir"]
		}
		if share != "" && subdir != "" {
			return filepath.Join(share, subdir)
		}
		if share != "" {
			return filepath.Join(share, pv.Name)
		}
	}

	return ""
}

// ensureQuota ensures the quota is applied for a PV
func (a *QuotaAgent) ensureQuota(ctx context.Context, pv *v1.PersistentVolume) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	capacity, ok := pv.Spec.Capacity[v1.ResourceStorage]
	if !ok {
		return fmt.Errorf("PV %s has no storage capacity", pv.Name)
	}
	capacityBytes := capacity.Value()

	nfsPath := a.getNFSPath(pv)
	if nfsPath == "" {
		return fmt.Errorf("PV %s has no NFS path", pv.Name)
	}
	localPath := a.nfsPathToLocal(nfsPath)

	if _, err := os.Stat(localPath); os.IsNotExist(err) {
		slog.Warn("Directory does not exist, skipping quota", "path", localPath, "pv", pv.Name)
		return nil
	}

	if existingQuota, exists := a.appliedQuotas[localPath]; exists && existingQuota == capacityBytes {
		return nil
	}

	projectName := a.getProjectName(pv)
	projectID := a.generateProjectID(projectName)

	oldQuota := a.appliedQuotas[localPath]
	isUpdate := oldQuota > 0 && oldQuota != capacityBytes

	err := a.applyQuota(localPath, projectName, projectID, capacityBytes)

	var namespace, pvcName string
	if pv.Spec.ClaimRef != nil {
		namespace = pv.Spec.ClaimRef.Namespace
		pvcName = pv.Spec.ClaimRef.Name
	}

	if a.auditLogger != nil {
		if isUpdate {
			a.auditLogger.LogQuotaUpdate(pv.Name, localPath, projectName, projectID, oldQuota, capacityBytes, a.fsType, err)
		} else {
			a.auditLogger.LogQuotaCreate(pv.Name, namespace, pvcName, localPath, projectName, projectID, capacityBytes, a.fsType, err)
		}
	}

	if err != nil {
		a.updateQuotaStatus(ctx, pv, QuotaStatusFailed)
		return err
	}

	a.appliedQuotas[localPath] = capacityBytes
	a.updateQuotaStatus(ctx, pv, QuotaStatusApplied)

	slog.Info("Quota applied successfully",
		"pv", pv.Name,
		"path", localPath,
		"capacity", util.FormatBytes(capacityBytes),
	)

	return nil
}

// nfsPathToLocal converts NFS server path to local mount path
func (a *QuotaAgent) nfsPathToLocal(nfsPath string) string {
	if strings.HasPrefix(nfsPath, a.nfsServerPath) {
		return filepath.Join(a.nfsBasePath, strings.TrimPrefix(nfsPath, a.nfsServerPath))
	}
	return filepath.Join(a.nfsBasePath, filepath.Base(nfsPath))
}

// getProjectName gets or generates project name for a PV
func (a *QuotaAgent) getProjectName(pv *v1.PersistentVolume) string {
	if pv.Annotations != nil {
		if name, ok := pv.Annotations[AnnotationProjectName]; ok && name != "" {
			return name
		}
	}
	name := strings.ReplaceAll(pv.Name, "-", "_")
	if len(name) > 32 {
		name = name[:32]
	}
	return "pv_" + name
}

// generateProjectID generates a numeric project ID from project name
func (a *QuotaAgent) generateProjectID(projectName string) uint32 {
	var hash uint32 = 2166136261
	for _, c := range projectName {
		hash ^= uint32(c)
		hash *= 16777619
	}
	return (hash % 4294967293) + 1
}

// applyQuota applies project quota based on filesystem type
func (a *QuotaAgent) applyQuota(path, projectName string, projectID uint32, sizeBytes int64) error {
	switch a.fsType {
	case quota.FSTypeXFS:
		return quota.ApplyXFSQuota(a.quotaPath, path, projectName, projectID, sizeBytes, a.projectsFile, a.projidFile)
	case quota.FSTypeExt4:
		return quota.ApplyExt4Quota(a.quotaPath, path, projectName, projectID, sizeBytes, a.projectsFile, a.projidFile)
	default:
		return fmt.Errorf("unsupported filesystem type: %s", a.fsType)
	}
}

// updateQuotaStatus updates the quota status annotation on the PV
func (a *QuotaAgent) updateQuotaStatus(ctx context.Context, pv *v1.PersistentVolume, st string) {
	freshPV, err := a.client.CoreV1().PersistentVolumes().Get(ctx, pv.Name, metav1.GetOptions{})
	if err != nil {
		slog.Error("Failed to get PV for status update", "pv", pv.Name, "error", err)
		return
	}

	if freshPV.Annotations == nil {
		freshPV.Annotations = make(map[string]string)
	}
	freshPV.Annotations[AnnotationQuotaStatus] = st

	_, err = a.client.CoreV1().PersistentVolumes().Update(ctx, freshPV, metav1.UpdateOptions{})
	if err != nil {
		slog.Error("Failed to update PV quota status", "pv", pv.Name, "error", err)
	}
}

// collectHistory collects usage history periodically
func (a *QuotaAgent) collectHistory(ctx context.Context) {
	slog.Info("Starting history collection", "interval", a.historyStore.Interval())

	ticker := time.NewTicker(a.historyStore.Interval())
	defer ticker.Stop()

	a.recordHistory()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.recordHistory()
		}
	}
}

// recordHistory records current usage to history
func (a *QuotaAgent) recordHistory() {
	if a.historyStore == nil {
		return
	}

	fsType, _ := quota.DetectFSType(a.nfsBasePath)
	usages, err := status.GetDirUsages(a.nfsBasePath, fsType)
	if err != nil {
		slog.Error("Failed to get usages for history", "error", err)
		return
	}

	if err := a.historyStore.Record(usages); err != nil {
		slog.Error("Failed to record history", "error", err)
	}
}
