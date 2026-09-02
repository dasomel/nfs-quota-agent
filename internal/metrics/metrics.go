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

// Package metrics provides a Prometheus metrics exporter for the NFS quota agent.
// It collects system-level disk usage, directory quotas, and application status,
// exposing them via an HTTP API endpoint.
package metrics

import (
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dasomel/nfs-quota-agent/internal/quota"
	"github.com/dasomel/nfs-quota-agent/internal/status"
)

// AgentInfo provides the interface for metrics to query agent state
type AgentInfo interface {
	BasePath() string
	AppliedQuotaCount() int
	// ProjectsFile/ProjidFile return the agent's configured
	// /etc/projects, /etc/projid paths, so the per-directory usage/quota
	// metrics resolve project name/ID to path the same way ensureQuota's
	// own read-back verification does, rather than assume the standard
	// locations under a non-default --projects-file/--projid-file.
	ProjectsFile() string
	ProjidFile() string
	// LivenessOK reports whether the agent process is alive and making
	// progress. It must be independent of the Kubernetes API and the NFS
	// mount so /health never restart-loops on a transient outage a restart
	// wouldn't fix. reason is a short, human-readable explanation.
	LivenessOK() (ok bool, reason string)
	// ReadinessOK reports whether this instance can enforce quotas right
	// now (filesystem detected, quota subsystem available, base path
	// present, initial sync completed and not repeatedly failing). It
	// reflects live state, so it can flip back to not-ready after startup.
	ReadinessOK() (ok bool, reason string)
	// ReconcileQueueDepth returns how many PV keys are currently queued or
	// in flight in the watch path's reconcile queue. The queue persists
	// across watch reconnects (it's created once per watchPVsWithBackoff
	// call, not per connection), so this can be nonzero even mid-reconnect;
	// it's 0 only before the watch loop's first attempt or after final
	// shutdown.
	ReconcileQueueDepth() int
	// ReconcileStats returns cumulative counts/duration for reconcile-queue
	// work processed since process start (never reset): total items
	// processed, how many ended in error, and total wall-clock time spent
	// in ensureQuota across all of them, in seconds.
	ReconcileStats() (total, errors int64, durationSeconds float64)
	// VerificationFailures returns the cumulative count of quota applies
	// whose post-apply read-back verification found the on-disk state
	// didn't match what was requested, since process start (#10) -- a
	// narrower breakdown of ReconcileStats' errors, distinguishing "the
	// apply command itself failed" from "it exited 0 but the filesystem
	// disagreed."
	VerificationFailures() int64
	// LastSuccessfulFullSync returns when the periodic full reconciliation
	// last completed without error, or the zero Time if it never has.
	LastSuccessfulFullSync() time.Time
	// HAActive reports whether this instance currently owns quota
	// enforcement (see agent.QuotaAgent.HAActive). Always true when HA
	// gating is disabled -- most deployments, which have no HA setup at
	// all, will always read active.
	HAActive() bool
	// ShrinkGuardPrimed reports whether the shrink guard's brownfield
	// snapshot (agent.QuotaAgent.priorUsageFromDisk/priorEnforcedFromDisk)
	// has been successfully populated from an on-disk report at least once
	// since process start. False means the guard is fail-open for its
	// brownfield-suspicion check specifically (#93) -- see
	// agent.QuotaAgent.ShrinkGuardPrimed's doc comment.
	ShrinkGuardPrimed() bool
	// ShrinkGuardPrimeFailures returns the cumulative count of failed
	// shrink-guard prime attempts since process start (never reset,
	// including after a later success).
	ShrinkGuardPrimeFailures() int64
}

// Collector collects quota metrics for Prometheus
type Collector struct {
	agent      AgentInfo
	version    string
	mu         sync.RWMutex
	lastUpdate time.Time
	metrics    string
}

// StartServer starts the Prometheus metrics server
func StartServer(addr string, agent AgentInfo, version string) {
	collector := &Collector{
		agent:   agent,
		version: version,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", collector.handleMetrics)
	mux.HandleFunc("/health", collector.handleHealth)
	mux.HandleFunc("/ready", collector.handleReady)

	slog.Info("Starting metrics server", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("Metrics server failed", "error", err)
	}
}

func (c *Collector) handleMetrics(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Update metrics if stale (older than 30 seconds)
	if time.Since(c.lastUpdate) > 30*time.Second {
		c.updateMetrics()
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprint(w, c.metrics)
}

func (c *Collector) updateMetrics() {
	var sb strings.Builder
	basePath := c.agent.BasePath()

	// Metadata
	sb.WriteString("# HELP nfs_quota_agent_info Information about the NFS quota agent\n")
	sb.WriteString("# TYPE nfs_quota_agent_info gauge\n")
	fmt.Fprintf(&sb, "nfs_quota_agent_info{version=\"%s\"} 1\n\n", c.version)

	// Get disk usage
	diskUsage, err := status.GetDiskUsage(basePath)
	if err == nil {
		sb.WriteString("# HELP nfs_disk_total_bytes Total disk space in bytes\n")
		sb.WriteString("# TYPE nfs_disk_total_bytes gauge\n")
		fmt.Fprintf(&sb, "nfs_disk_total_bytes{path=\"%s\"} %d\n\n", basePath, diskUsage.Total)

		sb.WriteString("# HELP nfs_disk_used_bytes Used disk space in bytes\n")
		sb.WriteString("# TYPE nfs_disk_used_bytes gauge\n")
		fmt.Fprintf(&sb, "nfs_disk_used_bytes{path=\"%s\"} %d\n\n", basePath, diskUsage.Used)

		sb.WriteString("# HELP nfs_disk_available_bytes Available disk space in bytes\n")
		sb.WriteString("# TYPE nfs_disk_available_bytes gauge\n")
		fmt.Fprintf(&sb, "nfs_disk_available_bytes{path=\"%s\"} %d\n\n", basePath, diskUsage.Available)

		sb.WriteString("# HELP nfs_disk_used_percent Disk usage percentage\n")
		sb.WriteString("# TYPE nfs_disk_used_percent gauge\n")
		fmt.Fprintf(&sb, "nfs_disk_used_percent{path=\"%s\"} %.2f\n\n", basePath, diskUsage.UsedPct)
	}

	// Get filesystem type
	fsType, _ := quota.DetectFSType(basePath)

	// Get directory quotas
	dirUsages, err := status.GetDirUsages(basePath, fsType, c.agent.ProjectsFile(), c.agent.ProjidFile())
	if err == nil && len(dirUsages) > 0 {
		sb.WriteString("# HELP nfs_quota_used_bytes Used space by directory in bytes\n")
		sb.WriteString("# TYPE nfs_quota_used_bytes gauge\n")
		for _, du := range dirUsages {
			dirName := filepath.Base(du.Path)
			fmt.Fprintf(&sb, "nfs_quota_used_bytes{directory=\"%s\"} %d\n", dirName, du.Used)
		}
		sb.WriteString("\n")

		sb.WriteString("# HELP nfs_quota_limit_bytes Quota limit by directory in bytes\n")
		sb.WriteString("# TYPE nfs_quota_limit_bytes gauge\n")
		for _, du := range dirUsages {
			if du.Quota > 0 {
				dirName := filepath.Base(du.Path)
				fmt.Fprintf(&sb, "nfs_quota_limit_bytes{directory=\"%s\"} %d\n", dirName, du.Quota)
			}
		}
		sb.WriteString("\n")

		sb.WriteString("# HELP nfs_quota_used_percent Quota usage percentage by directory\n")
		sb.WriteString("# TYPE nfs_quota_used_percent gauge\n")
		for _, du := range dirUsages {
			if du.Quota > 0 {
				dirName := filepath.Base(du.Path)
				fmt.Fprintf(&sb, "nfs_quota_used_percent{directory=\"%s\"} %.2f\n", dirName, du.QuotaPct)
			}
		}
		sb.WriteString("\n")

		// Summary metrics
		var totalDirs, warningCount, exceededCount int
		for _, du := range dirUsages {
			totalDirs++
			if du.Quota > 0 {
				if du.QuotaPct >= 100 {
					exceededCount++
				} else if du.QuotaPct >= 90 {
					warningCount++
				}
			}
		}

		sb.WriteString("# HELP nfs_quota_directories_total Total number of directories with quotas\n")
		sb.WriteString("# TYPE nfs_quota_directories_total gauge\n")
		fmt.Fprintf(&sb, "nfs_quota_directories_total %d\n\n", totalDirs)

		sb.WriteString("# HELP nfs_quota_warning_count Number of directories with >90%% usage\n")
		sb.WriteString("# TYPE nfs_quota_warning_count gauge\n")
		fmt.Fprintf(&sb, "nfs_quota_warning_count %d\n\n", warningCount)

		sb.WriteString("# HELP nfs_quota_exceeded_count Number of directories with >100%% usage\n")
		sb.WriteString("# TYPE nfs_quota_exceeded_count gauge\n")
		fmt.Fprintf(&sb, "nfs_quota_exceeded_count %d\n\n", exceededCount)
	}

	// Applied quotas count
	appliedCount := c.agent.AppliedQuotaCount()

	sb.WriteString("# HELP nfs_quota_applied_total Total number of applied quotas\n")
	sb.WriteString("# TYPE nfs_quota_applied_total gauge\n")
	fmt.Fprintf(&sb, "nfs_quota_applied_total %d\n\n", appliedCount)

	// Reconcile queue (watch path -> ensureQuota pipeline; see
	// internal/agent/reconcile_queue.go)
	sb.WriteString("# HELP nfs_quota_agent_reconcile_queue_depth Number of PV keys currently queued or in flight in the watch-path reconcile queue\n")
	sb.WriteString("# TYPE nfs_quota_agent_reconcile_queue_depth gauge\n")
	fmt.Fprintf(&sb, "nfs_quota_agent_reconcile_queue_depth %d\n\n", c.agent.ReconcileQueueDepth())

	reconcileTotal, reconcileErrors, reconcileDurationSeconds := c.agent.ReconcileStats()

	sb.WriteString("# HELP nfs_quota_agent_reconcile_total Total reconcile-queue items processed since process start\n")
	sb.WriteString("# TYPE nfs_quota_agent_reconcile_total counter\n")
	fmt.Fprintf(&sb, "nfs_quota_agent_reconcile_total %d\n\n", reconcileTotal)

	sb.WriteString("# HELP nfs_quota_agent_reconcile_errors_total Total reconcile-queue items that ended in error since process start\n")
	sb.WriteString("# TYPE nfs_quota_agent_reconcile_errors_total counter\n")
	fmt.Fprintf(&sb, "nfs_quota_agent_reconcile_errors_total %d\n\n", reconcileErrors)

	sb.WriteString("# HELP nfs_quota_agent_reconcile_duration_seconds_sum Cumulative wall-clock time spent in ensureQuota by reconcile-queue workers since process start\n")
	sb.WriteString("# TYPE nfs_quota_agent_reconcile_duration_seconds_sum counter\n")
	fmt.Fprintf(&sb, "nfs_quota_agent_reconcile_duration_seconds_sum %f\n\n", reconcileDurationSeconds)

	sb.WriteString("# HELP nfs_quota_agent_verification_failures_total Total applies where the quota command succeeded but a read-back of the quota tool's own reported project limit found a mismatch, since process start. Confirms the tool's bookkeeping, not that the target directory's inode is actually bound to that project.\n")
	sb.WriteString("# TYPE nfs_quota_agent_verification_failures_total counter\n")
	fmt.Fprintf(&sb, "nfs_quota_agent_verification_failures_total %d\n\n", c.agent.VerificationFailures())

	sb.WriteString("# HELP nfs_quota_agent_last_full_sync_timestamp_seconds Unix timestamp of the last successful periodic full reconciliation (syncAllQuotas); 0 if none has succeeded yet\n")
	sb.WriteString("# TYPE nfs_quota_agent_last_full_sync_timestamp_seconds gauge\n")
	var lastFullSyncUnix int64
	if lastSync := c.agent.LastSuccessfulFullSync(); !lastSync.IsZero() {
		lastFullSyncUnix = lastSync.Unix()
	}
	fmt.Fprintf(&sb, "nfs_quota_agent_last_full_sync_timestamp_seconds %d\n\n", lastFullSyncUnix)

	// HA active/standby state (#11). Always 1 when HA gating is disabled.
	sb.WriteString("# HELP nfs_quota_agent_ha_active Whether this instance currently owns quota enforcement (1) or is HA standby (0); always 1 when HA gating is not configured\n")
	sb.WriteString("# TYPE nfs_quota_agent_ha_active gauge\n")
	haActive := 0
	if c.agent.HAActive() {
		haActive = 1
	}
	fmt.Fprintf(&sb, "nfs_quota_agent_ha_active %d\n\n", haActive)

	// Shrink guard prime state (#93). primed=0 means the brownfield
	// suspicion check (suspectBrownfield in ensureQuotaMutated) is
	// currently fail-open -- an operator watching this metric can tell a
	// transient startup report failure apart from silent, permanent
	// exposure the way the old sync.Once-guarded prime would have left it.
	sb.WriteString("# HELP nfs_quota_agent_shrink_guard_primed Whether the shrink guard's brownfield-suspicion snapshot has been successfully populated from an on-disk report at least once (1) or not yet (0, fail-open for that check)\n")
	sb.WriteString("# TYPE nfs_quota_agent_shrink_guard_primed gauge\n")
	shrinkGuardPrimed := 0
	if c.agent.ShrinkGuardPrimed() {
		shrinkGuardPrimed = 1
	}
	fmt.Fprintf(&sb, "nfs_quota_agent_shrink_guard_primed %d\n\n", shrinkGuardPrimed)

	sb.WriteString("# HELP nfs_quota_agent_shrink_guard_prime_failures_total Cumulative count of failed shrink-guard prime attempts (on-disk report unreadable) since process start\n")
	sb.WriteString("# TYPE nfs_quota_agent_shrink_guard_prime_failures_total counter\n")
	fmt.Fprintf(&sb, "nfs_quota_agent_shrink_guard_prime_failures_total %d\n", c.agent.ShrinkGuardPrimeFailures())

	c.metrics = sb.String()
	c.lastUpdate = time.Now()
}

// handleHealth serves the liveness probe: is the process wedged? It never
// consults the Kubernetes API or the NFS mount (see AgentInfo.LivenessOK).
func (c *Collector) handleHealth(w http.ResponseWriter, r *http.Request) {
	ok, reason := c.agent.LivenessOK()
	writeProbeResult(w, ok, reason)
}

// handleReady serves the readiness probe: can this instance enforce quotas
// right now? Reflects live state, so it can go non-ready after startup.
func (c *Collector) handleReady(w http.ResponseWriter, r *http.Request) {
	ok, reason := c.agent.ReadinessOK()
	writeProbeResult(w, ok, reason)
}

// writeProbeResult writes a plain-text probe response: 200 with "ok" (or a
// short status) when healthy, 503 with the failing reason otherwise.
// Kubernetes probes only look at the status code; the body is for humans.
func writeProbeResult(w http.ResponseWriter, ok bool, reason string) {
	status := http.StatusOK
	if !ok {
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintln(w, reason)
}
