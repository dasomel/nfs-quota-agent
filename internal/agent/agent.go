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

// Package agent implements the core daemon that watches Kubernetes PersistentVolumes
// and reconciles them with filesystem project quotas on NFS exports. It also coordinates
// periodic synchronization, namespace quota policies, and automatic cleanup of orphaned quotas.
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/dasomel/nfs-quota-agent/internal/audit"
	"github.com/dasomel/nfs-quota-agent/internal/history"
	"github.com/dasomel/nfs-quota-agent/internal/pvpath"
	"github.com/dasomel/nfs-quota-agent/internal/quota"
	"github.com/dasomel/nfs-quota-agent/internal/status"
	"github.com/dasomel/nfs-quota-agent/internal/util"
)

const (
	// AnnotationProjectName is the annotation key for the project name on PVs.
	AnnotationProjectName = "nfs.io/project-name"
	// AnnotationQuotaStatus is the annotation key representing the quota application status on PVs.
	AnnotationQuotaStatus = "nfs.io/quota-status"

	// QuotaStatusPending indicates the quota application is pending.
	QuotaStatusPending = "pending"
	// QuotaStatusApplied indicates the quota has been successfully applied.
	QuotaStatusApplied = "applied"
	// QuotaStatusFailed indicates the quota application has failed.
	QuotaStatusFailed = "failed"
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
	// stateDir is a host-backed directory (distinct from projectsFile's and
	// projidFile's own directory, which may be nothing but a bind-mounted
	// individual file inside the container) used to hold the crash-recovery
	// backup sidecars quota.RemoveLineFromFile/RecoverProjectFile keep. See
	// their doc comments in internal/quota/project.go.
	stateDir        string
	syncInterval    time.Duration
	mu              sync.Mutex
	appliedQuotas   map[string]int64
	knownProjectIDs map[uint32]string // cache of projid file; refreshed once per sync cycle
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

	// Health/readiness state, read by the metrics server's /health and
	// /ready handlers. Kept on a dedicated mutex, separate from mu, so
	// probe reads never contend with quota-application work.
	healthMu                sync.RWMutex
	lastHeartbeat           time.Time // updated once per Run() loop iteration; liveness proof of life
	fsDetected              bool
	quotaAvailable          bool
	initialSyncDone         bool
	consecutiveSyncFailures int
	lastSyncErr             error
}

// livenessStallMultiplier controls how many syncInterval periods may pass
// without a heartbeat before liveness reports not-ok. It must be generous:
// too tight and a legitimately slow sync (many PVs) trips a restart loop.
const livenessStallMultiplier = 4

// readinessSyncFailureThreshold is the number of consecutive sync failures
// after which readiness flips to not-ready. A single transient failure
// (e.g. one missed API call) should not pull the instance out of service.
const readinessSyncFailureThreshold = 3

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
		stateDir:          "/var/lib/nfs-quota-agent",
		syncInterval:      30 * time.Second,
		appliedQuotas:     make(map[string]int64),
		knownProjectIDs:   make(map[uint32]string),
		cleanupInterval:   1 * time.Hour,
		orphanGracePeriod: 24 * time.Hour,
		cleanupDryRun:     true,
		orphanLastSeen:    make(map[string]time.Time),
	}
}

// Setters for configuration

// SetProcessAllNFS sets whether all NFS PVs should be processed.
func (a *QuotaAgent) SetProcessAllNFS(v bool) { a.processAllNFS = v }

// SetQuotaPath sets the local quota path.
func (a *QuotaAgent) SetQuotaPath(v string) { a.quotaPath = v }

// SetProjectsFile sets the projects file path.
func (a *QuotaAgent) SetProjectsFile(v string) { a.projectsFile = v }

// SetProjidFile sets the projid file path.
func (a *QuotaAgent) SetProjidFile(v string) { a.projidFile = v }

// SetStateDir sets the host-backed directory used for crash-recovery backup
// sidecars of projectsFile/projidFile. An empty value disables the sidecar
// (see quota.RemoveLineFromFile/RecoverProjectFile), degrading gracefully
// rather than failing quota operations.
func (a *QuotaAgent) SetStateDir(v string) { a.stateDir = v }

// SetSyncInterval sets the synchronization interval.
func (a *QuotaAgent) SetSyncInterval(v time.Duration) { a.syncInterval = v }

// SetAuditLogger sets the audit logger instance.
func (a *QuotaAgent) SetAuditLogger(v *audit.Logger) { a.auditLogger = v }

// SetEnableAutoCleanup enables or disables automatic orphan cleanup.
func (a *QuotaAgent) SetEnableAutoCleanup(v bool) { a.enableAutoCleanup = v }

// SetCleanupIntervalDuration sets the automatic cleanup interval.
func (a *QuotaAgent) SetCleanupIntervalDuration(v time.Duration) { a.cleanupInterval = v }

// SetOrphanGracePeriodDuration sets the grace period before orphaned directories are removed.
func (a *QuotaAgent) SetOrphanGracePeriodDuration(v time.Duration) { a.orphanGracePeriod = v }

// SetCleanupDryRunFlag sets the dry-run flag for the orphan cleanup.
func (a *QuotaAgent) SetCleanupDryRunFlag(v bool) { a.cleanupDryRun = v }

// SetHistoryStore sets the usage history store.
func (a *QuotaAgent) SetHistoryStore(v *history.Store) { a.historyStore = v }

// SetEnablePolicy enables or disables namespace quota policy enforcement.
func (a *QuotaAgent) SetEnablePolicy(v bool) { a.enablePolicy = v }

// SetDefaultQuota sets the default namespace quota limit in bytes.
func (a *QuotaAgent) SetDefaultQuota(v int64) { a.defaultQuota = v }

// SetEnforceMaxQuota sets whether the maximum namespace quota should be enforced.
func (a *QuotaAgent) SetEnforceMaxQuota(v bool) { a.enforceMaxQuota = v }

// Getters for UI/metrics interface

// BasePath returns the base mount path of the NFS export.
func (a *QuotaAgent) BasePath() string { return a.nfsBasePath }

// EnableAutoCleanup returns whether automatic cleanup is enabled.
func (a *QuotaAgent) EnableAutoCleanup() bool { return a.enableAutoCleanup }

// CleanupDryRun returns whether cleanup runs in dry-run mode.
func (a *QuotaAgent) CleanupDryRun() bool { return a.cleanupDryRun }

// OrphanGracePeriod returns the grace period for orphaned directories.
func (a *QuotaAgent) OrphanGracePeriod() time.Duration { return a.orphanGracePeriod }

// CleanupInterval returns the interval between cleanup runs.
func (a *QuotaAgent) CleanupInterval() time.Duration { return a.cleanupInterval }

// EnablePolicy returns whether namespace quota policy is enabled.
func (a *QuotaAgent) EnablePolicy() bool { return a.enablePolicy }

// AuditLogger returns the configured audit logger instance.
func (a *QuotaAgent) AuditLogger() *audit.Logger { return a.auditLogger }

// AppliedQuotaCount returns the number of active project quotas applied.
func (a *QuotaAgent) AppliedQuotaCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.appliedQuotas)
}

// recordHeartbeat marks the periodic sync loop as having made progress.
// Called once at Run() start and once per loop iteration; liveness compares
// its age against syncInterval, never against Kubernetes API or NFS state.
func (a *QuotaAgent) recordHeartbeat() {
	a.healthMu.Lock()
	a.lastHeartbeat = time.Now()
	a.healthMu.Unlock()
}

// setFilesystemDetected records whether detectFilesystemType succeeded.
func (a *QuotaAgent) setFilesystemDetected(v bool) {
	a.healthMu.Lock()
	a.fsDetected = v
	a.healthMu.Unlock()
}

// setQuotaAvailable records whether checkQuotaAvailable succeeded.
func (a *QuotaAgent) setQuotaAvailable(v bool) {
	a.healthMu.Lock()
	a.quotaAvailable = v
	a.healthMu.Unlock()
}

// recordSyncResult records the outcome of a syncAllQuotas attempt (initial
// or periodic). Consecutive failures accumulate and flip readiness to
// not-ready once they cross readinessSyncFailureThreshold; a success resets
// the counter so a single transient failure doesn't pull the instance out
// of service.
func (a *QuotaAgent) recordSyncResult(err error) {
	a.healthMu.Lock()
	defer a.healthMu.Unlock()
	a.initialSyncDone = true
	if err != nil {
		a.consecutiveSyncFailures++
		a.lastSyncErr = err
		return
	}
	a.consecutiveSyncFailures = 0
	a.lastSyncErr = nil
}

// LivenessOK reports whether the agent's main loop is making progress. It
// deliberately does not consult the Kubernetes API or the NFS mount: a
// probe wired to it must not restart-loop on a transient outage that a
// restart cannot fix. Before the first loop iteration completes it reports
// ok, since startup work (filesystem detection, initial sync) legitimately
// takes time.
func (a *QuotaAgent) LivenessOK() (bool, string) {
	a.healthMu.RLock()
	hb := a.lastHeartbeat
	interval := a.syncInterval
	a.healthMu.RUnlock()

	if hb.IsZero() {
		return true, "starting"
	}
	if interval <= 0 {
		return true, "ok"
	}
	threshold := interval * livenessStallMultiplier
	if age := time.Since(hb); age > threshold {
		return false, fmt.Sprintf("sync loop stalled: last heartbeat %s ago (threshold %s)", age.Round(time.Second), threshold)
	}
	return true, "ok"
}

// ReadinessOK reports whether this instance can enforce quotas right now:
// filesystem type detected, quota subsystem available, base path present,
// and the initial sync completed without a run of consecutive failures. It
// reads live state, so it flips back to not-ready if sync starts failing
// repeatedly after having been ready. An agent with zero PVs to manage is
// legitimately ready, so this never consults AppliedQuotaCount.
func (a *QuotaAgent) ReadinessOK() (bool, string) {
	a.healthMu.RLock()
	fsDetected := a.fsDetected
	quotaAvailable := a.quotaAvailable
	initialSyncDone := a.initialSyncDone
	failures := a.consecutiveSyncFailures
	lastErr := a.lastSyncErr
	a.healthMu.RUnlock()

	if !fsDetected {
		return false, "filesystem type not detected"
	}
	if !quotaAvailable {
		return false, "quota subsystem not available"
	}
	if _, err := os.Stat(a.nfsBasePath); err != nil {
		return false, fmt.Sprintf("base path not accessible: %v", err)
	}
	if !initialSyncDone {
		return false, "initial quota sync not yet completed"
	}
	if failures >= readinessSyncFailureThreshold {
		return false, fmt.Sprintf("quota sync failing: %d consecutive failures (last error: %v)", failures, lastErr)
	}
	return true, "ok"
}

// Run starts the quota agent
func (a *QuotaAgent) Run(ctx context.Context) error {
	// Mark the process alive before any startup work, so liveness is
	// generous during (potentially slow) filesystem/quota detection rather
	// than reporting not-started as a stall.
	a.recordHeartbeat()

	// Detect filesystem type
	if err := a.detectFilesystemType(); err != nil {
		return fmt.Errorf("failed to detect filesystem type: %w", err)
	}
	a.setFilesystemDetected(true)

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
	a.setQuotaAvailable(true)

	// Load existing projects
	if err := a.loadProjects(); err != nil {
		slog.Warn("Failed to load existing projects", "error", err)
	}

	// Initial sync
	if err := a.syncAllQuotas(ctx); err != nil {
		slog.Error("Initial quota sync failed", "error", err)
		a.recordSyncResult(err)
	} else {
		a.recordSyncResult(nil)
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
			a.recordHeartbeat()
			if err := a.syncAllQuotas(ctx); err != nil {
				slog.Error("Periodic quota sync failed", "error", err)
				a.recordSyncResult(err)
			} else {
				a.recordSyncResult(nil)
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
	case "btrfs":
		a.fsType = quota.FSTypeBtrfs
	default:
		return fmt.Errorf("unsupported filesystem type: %s (only xfs, ext4, and btrfs are supported)", fsType)
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
	case quota.FSTypeBtrfs:
		return quota.CheckBtrfsQuotaAvailable(a.quotaPath)
	default:
		return fmt.Errorf("unsupported filesystem type: %s", a.fsType)
	}
}

// loadProjects loads existing project mappings
func (a *QuotaAgent) loadProjects() error {
	if a.fsType == quota.FSTypeBtrfs {
		return nil
	}

	// Recover the host's project metadata files from their backup sidecar
	// before anything reads them, in case an earlier in-place rewrite
	// crashed and left projects or projid truncated/corrupt.
	if err := quota.RecoverProjectFile(a.projectsFile, a.stateDir); err != nil {
		slog.Warn("Failed to recover projects file", "error", err)
	}
	if err := quota.RecoverProjectFile(a.projidFile, a.stateDir); err != nil {
		slog.Warn("Failed to recover projid file", "error", err)
	}

	projects, err := quota.ReadProjectsFile(a.projectsFile)
	if err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	for _, path := range projects {
		if path != "" {
			if _, exists := a.appliedQuotas[path]; !exists {
				// Mark as known (size unknown until next sync updates it)
				a.appliedQuotas[path] = 0
			}
		}
	}

	slog.Info("Loaded existing projects", "count", len(projects))
	return nil
}

// syncAllQuotas syncs quotas for all matching PVs
func (a *QuotaAgent) syncAllQuotas(ctx context.Context) error {
	pvList, err := a.client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list PVs: %w", err)
	}

	// Refresh project ID cache once per sync cycle so generateProjectID
	// doesn't read /etc/projid on every PV.
	ids := a.loadExistingProjectIDs()
	a.mu.Lock()
	a.knownProjectIDs = ids
	a.mu.Unlock()

	syncedCount := 0
	live := make(map[string]struct{}, len(pvList.Items))
	for _, pv := range pvList.Items {
		if !a.shouldProcessPV(&pv) {
			continue
		}
		if nfsPath := a.getNFSPath(&pv); nfsPath != "" {
			live[a.nfsPathToLocal(nfsPath)] = struct{}{}
		}
		if err := a.ensureQuota(ctx, &pv); err != nil {
			slog.Error("Failed to ensure quota for PV", "pv", pv.Name, "error", err)
		} else {
			syncedCount++
		}
	}

	a.pruneAppliedQuotas(live)

	slog.Debug("Quota sync completed", "synced", syncedCount, "total", len(pvList.Items))
	return nil
}

// pruneAppliedQuotas drops cache entries for paths no longer backed by a PV.
//
// The watch removes an entry when it sees a Deleted event, but a watch that
// reconnects restarts from the current resourceVersion, so deletions that
// happened while it was down are never delivered. Nothing else purged those
// entries, which both inflated the applied-quota metric and — because
// ensureQuota returns early on a cache hit — could skip applying a quota to a
// path that a later PV reused. The full list this sync just walked is the
// authority on what still exists.
func (a *QuotaAgent) pruneAppliedQuotas(live map[string]struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()

	for path := range a.appliedQuotas {
		if _, ok := live[path]; !ok {
			delete(a.appliedQuotas, path)
			slog.Debug("Dropped applied-quota cache entry with no matching PV", "path", path)
		}
	}
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

// getNFSPath extracts the NFS path from a PV. Delegates to pvpath, the
// single source of truth shared with ui and cleanup.
func (a *QuotaAgent) getNFSPath(pv *v1.PersistentVolume) string {
	return pvpath.NFSPath(pv)
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

// nfsPathToLocal converts NFS server path to local mount path. Delegates to
// pvpath.ToLocal for the mapping itself and keeps the fallback warning here,
// since logging is agent-specific behavior other callers of pvpath don't want.
func (a *QuotaAgent) nfsPathToLocal(nfsPath string) string {
	result := pvpath.ToLocal(nfsPath, a.nfsServerPath, a.nfsBasePath)
	if result.Fallback {
		// Using filepath.Base risks collision if multiple NFS paths share the same basename.
		// Log a warning so operators can fix nfs-server-path configuration.
		slog.Warn("NFS path does not match server path prefix, using basename as fallback — check --nfs-server-path configuration",
			"nfsPath", nfsPath,
			"nfsServerPath", a.nfsServerPath,
			"fallbackLocalPath", result.Path,
		)
	}
	return result.Path
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

// generateProjectID generates a unique numeric project ID from project name.
// Uses the in-memory knownProjectIDs cache (refreshed once per sync cycle).
// Must be called with a.mu held.
func (a *QuotaAgent) generateProjectID(projectName string) uint32 {
	id := a.hashProjectName(projectName)

	for {
		if existingName, taken := a.knownProjectIDs[id]; !taken || existingName == projectName {
			a.knownProjectIDs[id] = projectName // update cache for subsequent calls this cycle
			return id
		}
		// Collision: different project already owns this ID, try next
		slog.Warn("Project ID collision detected, trying next ID",
			"projectName", projectName,
			"conflictingName", a.knownProjectIDs[id],
			"id", id,
		)
		id++
		if id == 0 {
			id = 1 // avoid ID 0
		}
	}
}

// hashProjectName computes the initial FNV-1 hash for a project name
func (a *QuotaAgent) hashProjectName(projectName string) uint32 {
	var hash uint32 = 2166136261
	for _, c := range projectName {
		hash ^= uint32(c)
		hash *= 16777619
	}
	return (hash % 4294967293) + 1
}

// loadExistingProjectIDs reads /etc/projid and returns a map of projectID -> projectName
func (a *QuotaAgent) loadExistingProjectIDs() map[uint32]string {
	existing := make(map[uint32]string)
	data, err := os.ReadFile(a.projidFile)
	if err != nil {
		return existing
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			if id, err := strconv.ParseUint(parts[1], 10, 32); err == nil {
				existing[uint32(id)] = parts[0]
			}
		}
	}
	return existing
}

// applyQuota applies project quota based on filesystem type
func (a *QuotaAgent) applyQuota(path, projectName string, projectID uint32, sizeBytes int64) error {
	switch a.fsType {
	case quota.FSTypeXFS:
		return quota.ApplyXFSQuota(a.quotaPath, path, projectName, projectID, sizeBytes, a.projectsFile, a.projidFile)
	case quota.FSTypeExt4:
		return quota.ApplyExt4Quota(a.quotaPath, path, projectName, projectID, sizeBytes, a.projectsFile, a.projidFile)
	case quota.FSTypeBtrfs:
		return quota.ApplyBtrfsQuota(path, sizeBytes)
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
