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
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// MetricsCollector collects quota metrics for Prometheus
type MetricsCollector struct {
	agent      *QuotaAgent
	mu         sync.RWMutex
	lastUpdate time.Time
	metrics    string
}

// startMetricsServer starts the Prometheus metrics server
func startMetricsServer(addr string, agent *QuotaAgent) {
	collector := &MetricsCollector{
		agent: agent,
	}

	http.HandleFunc("/metrics", collector.handleMetrics)
	http.HandleFunc("/health", handleHealth)
	http.HandleFunc("/ready", handleReady)

	slog.Info("Starting metrics server", "addr", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		slog.Error("Metrics server failed", "error", err)
	}
}

func (c *MetricsCollector) handleMetrics(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Update metrics if stale (older than 30 seconds)
	if time.Since(c.lastUpdate) > 30*time.Second {
		c.updateMetrics()
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprint(w, c.metrics)
}

func (c *MetricsCollector) updateMetrics() {
	var sb strings.Builder

	// Metadata
	sb.WriteString("# HELP nfs_quota_agent_info Information about the NFS quota agent\n")
	sb.WriteString("# TYPE nfs_quota_agent_info gauge\n")
	sb.WriteString(fmt.Sprintf("nfs_quota_agent_info{version=\"%s\"} 1\n\n", version))

	// Get disk usage
	diskUsage, err := getDiskUsage(c.agent.nfsBasePath)
	if err == nil {
		sb.WriteString("# HELP nfs_disk_total_bytes Total disk space in bytes\n")
		sb.WriteString("# TYPE nfs_disk_total_bytes gauge\n")
		sb.WriteString(fmt.Sprintf("nfs_disk_total_bytes{path=\"%s\"} %d\n\n", c.agent.nfsBasePath, diskUsage.Total))

		sb.WriteString("# HELP nfs_disk_used_bytes Used disk space in bytes\n")
		sb.WriteString("# TYPE nfs_disk_used_bytes gauge\n")
		sb.WriteString(fmt.Sprintf("nfs_disk_used_bytes{path=\"%s\"} %d\n\n", c.agent.nfsBasePath, diskUsage.Used))

		sb.WriteString("# HELP nfs_disk_available_bytes Available disk space in bytes\n")
		sb.WriteString("# TYPE nfs_disk_available_bytes gauge\n")
		sb.WriteString(fmt.Sprintf("nfs_disk_available_bytes{path=\"%s\"} %d\n\n", c.agent.nfsBasePath, diskUsage.Available))

		sb.WriteString("# HELP nfs_disk_used_percent Disk usage percentage\n")
		sb.WriteString("# TYPE nfs_disk_used_percent gauge\n")
		sb.WriteString(fmt.Sprintf("nfs_disk_used_percent{path=\"%s\"} %.2f\n\n", c.agent.nfsBasePath, diskUsage.UsedPct))
	}

	// Get filesystem type
	fsType, _ := detectFSType(c.agent.nfsBasePath)

	// Get directory quotas
	dirUsages, err := getDirUsages(c.agent.nfsBasePath, fsType)
	if err == nil && len(dirUsages) > 0 {
		sb.WriteString("# HELP nfs_quota_used_bytes Used space by directory in bytes\n")
		sb.WriteString("# TYPE nfs_quota_used_bytes gauge\n")
		for _, du := range dirUsages {
			dirName := filepath.Base(du.Path)
			sb.WriteString(fmt.Sprintf("nfs_quota_used_bytes{directory=\"%s\"} %d\n", dirName, du.Used))
		}
		sb.WriteString("\n")

		sb.WriteString("# HELP nfs_quota_limit_bytes Quota limit by directory in bytes\n")
		sb.WriteString("# TYPE nfs_quota_limit_bytes gauge\n")
		for _, du := range dirUsages {
			if du.Quota > 0 {
				dirName := filepath.Base(du.Path)
				sb.WriteString(fmt.Sprintf("nfs_quota_limit_bytes{directory=\"%s\"} %d\n", dirName, du.Quota))
			}
		}
		sb.WriteString("\n")

		sb.WriteString("# HELP nfs_quota_used_percent Quota usage percentage by directory\n")
		sb.WriteString("# TYPE nfs_quota_used_percent gauge\n")
		for _, du := range dirUsages {
			if du.Quota > 0 {
				dirName := filepath.Base(du.Path)
				sb.WriteString(fmt.Sprintf("nfs_quota_used_percent{directory=\"%s\"} %.2f\n", dirName, du.QuotaPct))
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
		sb.WriteString(fmt.Sprintf("nfs_quota_directories_total %d\n\n", totalDirs))

		sb.WriteString("# HELP nfs_quota_warning_count Number of directories with >90%% usage\n")
		sb.WriteString("# TYPE nfs_quota_warning_count gauge\n")
		sb.WriteString(fmt.Sprintf("nfs_quota_warning_count %d\n\n", warningCount))

		sb.WriteString("# HELP nfs_quota_exceeded_count Number of directories with >100%% usage\n")
		sb.WriteString("# TYPE nfs_quota_exceeded_count gauge\n")
		sb.WriteString(fmt.Sprintf("nfs_quota_exceeded_count %d\n\n", exceededCount))
	}

	// Applied quotas count
	c.agent.mu.Lock()
	appliedCount := len(c.agent.appliedQuotas)
	c.agent.mu.Unlock()

	sb.WriteString("# HELP nfs_quota_applied_total Total number of applied quotas\n")
	sb.WriteString("# TYPE nfs_quota_applied_total gauge\n")
	sb.WriteString(fmt.Sprintf("nfs_quota_applied_total %d\n", appliedCount))

	c.metrics = sb.String()
	c.lastUpdate = time.Now()
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "ok")
}

func handleReady(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "ok")
}
