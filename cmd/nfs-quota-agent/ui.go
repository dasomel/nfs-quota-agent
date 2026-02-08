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
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// UIServer serves the web UI
type UIServer struct {
	basePath      string
	nfsServerPath string
	addr          string
	auditLogPath  string
	client        kubernetes.Interface
	agent         *QuotaAgent
	historyStore  *HistoryStore
}

// StartUIServer starts the web UI server
func StartUIServer(addr, basePath string) error {
	return StartUIServerWithK8s(addr, basePath, "", nil)
}

// StartUIServerWithK8s starts the web UI server with Kubernetes client support
func StartUIServerWithK8s(addr, basePath, nfsServerPath string, client kubernetes.Interface) error {
	return StartUIServerFull(addr, basePath, nfsServerPath, "/var/log/nfs-quota-agent/audit.log", client)
}

// StartUIServerFull starts the web UI server with all options
func StartUIServerFull(addr, basePath, nfsServerPath, auditLogPath string, client kubernetes.Interface) error {
	return StartUIServerWithAgent(addr, basePath, nfsServerPath, auditLogPath, client, nil, nil)
}

// StartUIServerWithAgent starts the web UI server with agent reference
func StartUIServerWithAgent(addr, basePath, nfsServerPath, auditLogPath string, client kubernetes.Interface, agent *QuotaAgent, historyStore *HistoryStore) error {
	ui := &UIServer{
		basePath:      basePath,
		nfsServerPath: nfsServerPath,
		addr:          addr,
		auditLogPath:  auditLogPath,
		client:        client,
		agent:         agent,
		historyStore:  historyStore,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", ui.handleIndex)
	mux.HandleFunc("/api/status", ui.handleAPIStatus)
	mux.HandleFunc("/api/quotas", ui.handleAPIQuotas)
	mux.HandleFunc("/api/audit", ui.handleAPIAudit)
	mux.HandleFunc("/api/config", ui.handleAPIConfig)
	mux.HandleFunc("/api/orphans", ui.handleAPIOrphans)
	mux.HandleFunc("/api/orphans/delete", ui.handleAPIOrphansDelete)
	mux.HandleFunc("/api/history", ui.handleAPIHistory)
	mux.HandleFunc("/api/trends", ui.handleAPITrends)
	mux.HandleFunc("/api/policies", ui.handleAPIPolicies)
	mux.HandleFunc("/api/violations", ui.handleAPIViolations)
	mux.HandleFunc("/api/files", ui.handleAPIFiles)

	slog.Info("Starting Web UI", "addr", addr, "url", fmt.Sprintf("http://localhost%s", addr))
	return http.ListenAndServe(addr, mux)
}

func (ui *UIServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, dashboardHTML)
}

func (ui *UIServer) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	fsType, _ := detectFSType(ui.basePath)
	diskUsage, err := getDiskUsage(ui.basePath)
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	dirUsages, _ := getDirUsages(ui.basePath, fsType)

	var totalUsed, totalQuota uint64
	var warningCount, exceededCount, okCount int

	for _, du := range dirUsages {
		totalUsed += du.Used
		totalQuota += du.Quota
		if du.Quota > 0 {
			if du.QuotaPct >= 100 {
				exceededCount++
			} else if du.QuotaPct >= 90 {
				warningCount++
			} else {
				okCount++
			}
		}
	}

	response := map[string]interface{}{
		"timestamp":  time.Now().Format(time.RFC3339),
		"path":       ui.basePath,
		"filesystem": fsType,
		"disk": map[string]interface{}{
			"total":        diskUsage.Total,
			"used":         diskUsage.Used,
			"available":    diskUsage.Available,
			"usedPct":      diskUsage.UsedPct,
			"totalStr":     formatBytes(int64(diskUsage.Total)),
			"usedStr":      formatBytes(int64(diskUsage.Used)),
			"availableStr": formatBytes(int64(diskUsage.Available)),
		},
		"summary": map[string]interface{}{
			"totalDirectories": len(dirUsages),
			"totalUsed":        totalUsed,
			"totalQuota":       totalQuota,
			"totalUsedStr":     formatBytes(int64(totalUsed)),
			"totalQuotaStr":    formatBytes(int64(totalQuota)),
			"okCount":          okCount,
			"warningCount":     warningCount,
			"exceededCount":    exceededCount,
		},
	}

	_ = json.NewEncoder(w).Encode(response)
}

// PVInfo contains PV and PVC binding information
type PVInfo struct {
	PVName      string
	PVCName     string
	Namespace   string
	Phase       string
	NfsPath     string
	Capacity    string
	IsBound     bool
}

// getPVInfoMap returns a map of directory path to PV info
func (ui *UIServer) getPVInfoMap(ctx context.Context) map[string]*PVInfo {
	pvMap := make(map[string]*PVInfo)

	if ui.client == nil {
		return pvMap
	}

	pvList, err := ui.client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		slog.Warn("Failed to list PVs for UI", "error", err)
		return pvMap
	}

	for _, pv := range pvList.Items {
		nfsPath := ""

		// Get NFS path from native NFS or CSI
		if pv.Spec.NFS != nil {
			nfsPath = pv.Spec.NFS.Path
		} else if pv.Spec.CSI != nil && pv.Spec.CSI.Driver == "nfs.csi.k8s.io" {
			if share, ok := pv.Spec.CSI.VolumeAttributes["share"]; ok {
				nfsPath = share
				// Check both "subDir" (NFS CSI driver) and "subdir" (lowercase)
				subdir := pv.Spec.CSI.VolumeAttributes["subDir"]
				if subdir == "" {
					subdir = pv.Spec.CSI.VolumeAttributes["subdir"]
				}
				if subdir != "" {
					nfsPath = filepath.Join(share, subdir)
				}
			}
		}

		if nfsPath == "" {
			continue
		}

		// Convert NFS path to local path
		localPath := ui.nfsPathToLocal(nfsPath)

		info := &PVInfo{
			PVName:  pv.Name,
			NfsPath: nfsPath,
			Phase:   string(pv.Status.Phase),
			IsBound: pv.Status.Phase == v1.VolumeBound,
		}

		if capacity, ok := pv.Spec.Capacity[v1.ResourceStorage]; ok {
			info.Capacity = capacity.String()
		}

		if pv.Spec.ClaimRef != nil {
			info.PVCName = pv.Spec.ClaimRef.Name
			info.Namespace = pv.Spec.ClaimRef.Namespace
		}

		pvMap[localPath] = info
	}

	return pvMap
}

// nfsPathToLocal converts NFS server path to local mount path
func (ui *UIServer) nfsPathToLocal(nfsPath string) string {
	if ui.nfsServerPath != "" && strings.HasPrefix(nfsPath, ui.nfsServerPath) {
		return filepath.Join(ui.basePath, strings.TrimPrefix(nfsPath, ui.nfsServerPath))
	}
	return filepath.Join(ui.basePath, filepath.Base(nfsPath))
}

func (ui *UIServer) handleAPIQuotas(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	fsType, _ := detectFSType(ui.basePath)
	dirUsages, err := getDirUsages(ui.basePath, fsType)
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Get PV info map
	ctx := r.Context()
	pvMap := ui.getPVInfoMap(ctx)

	// Sort by usage percentage (descending)
	sort.Slice(dirUsages, func(i, j int) bool {
		if dirUsages[i].Quota > 0 && dirUsages[j].Quota > 0 {
			return dirUsages[i].QuotaPct > dirUsages[j].QuotaPct
		}
		return dirUsages[i].Used > dirUsages[j].Used
	})

	var quotas []map[string]interface{}
	for _, du := range dirUsages {
		status := "no_quota"
		if du.Quota > 0 {
			if du.QuotaPct >= 100 {
				status = "exceeded"
			} else if du.QuotaPct >= 90 {
				status = "warning"
			} else {
				status = "ok"
			}
		}

		entry := map[string]interface{}{
			"directory": filepath.Base(du.Path),
			"path":      du.Path,
			"used":      du.Used,
			"usedStr":   formatBytes(int64(du.Used)),
			"quota":     du.Quota,
			"quotaStr":  formatBytes(int64(du.Quota)),
			"usedPct":   du.QuotaPct,
			"status":    status,
		}

		// Add PV/PVC info if available
		if pvInfo, ok := pvMap[du.Path]; ok {
			entry["pvName"] = pvInfo.PVName
			entry["pvPhase"] = pvInfo.Phase
			entry["pvcName"] = pvInfo.PVCName
			entry["namespace"] = pvInfo.Namespace
			entry["isBound"] = pvInfo.IsBound
			entry["pvStatus"] = "bound"
		} else {
			entry["pvName"] = ""
			entry["pvPhase"] = ""
			entry["pvcName"] = ""
			entry["namespace"] = ""
			entry["isBound"] = false
			entry["pvStatus"] = "orphaned"
		}

		quotas = append(quotas, entry)
	}

	_ = json.NewEncoder(w).Encode(quotas)
}

func (ui *UIServer) handleAPIAudit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Parse query parameters
	action := r.URL.Query().Get("action")
	failsOnly := r.URL.Query().Get("fails_only") == "true"
	limitStr := r.URL.Query().Get("limit")
	limit := 100
	if limitStr != "" {
		if l, err := parseInt(limitStr); err == nil && l > 0 {
			limit = l
		}
	}

	filter := AuditFilter{
		Action:    AuditAction(action),
		OnlyFails: failsOnly,
	}

	entries, err := QueryAuditLog(ui.auditLogPath, filter)
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error":   err.Error(),
			"entries": []AuditEntry{},
		})
		return
	}

	// Apply limit (get last N entries)
	if len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}

	// Reverse to show newest first
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"total":   len(entries),
		"entries": entries,
	})
}

func parseInt(s string) (int, error) {
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid number")
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

func (ui *UIServer) handleAPIConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	config := map[string]interface{}{
		"auditEnabled":   ui.auditLogPath != "",
		"cleanupEnabled": ui.agent != nil && ui.agent.enableAutoCleanup,
		"historyEnabled": ui.historyStore != nil,
		"policyEnabled":  ui.agent != nil && ui.agent.enablePolicy,
	}
	_ = json.NewEncoder(w).Encode(config)
}

func (ui *UIServer) handleAPIOrphans(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if ui.agent == nil || ui.client == nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"orphans": []OrphanInfo{},
			"config": map[string]interface{}{
				"enabled":     false,
				"dryRun":      true,
				"gracePeriod": "24h",
			},
		})
		return
	}

	ctx := r.Context()
	orphans := ui.agent.GetOrphans(ctx)

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"orphans": orphans,
		"count":   len(orphans),
		"config": map[string]interface{}{
			"enabled":     ui.agent.enableAutoCleanup,
			"dryRun":      ui.agent.cleanupDryRun,
			"gracePeriod": ui.agent.orphanGracePeriod.String(),
			"interval":    ui.agent.cleanupInterval.String(),
		},
	})
}

func (ui *UIServer) handleAPIOrphansDelete(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "method not allowed"})
		return
	}

	if ui.agent == nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "agent not available"})
		return
	}

	// Check if cleanup is enabled - deletion not allowed if cleanup is disabled
	if !ui.agent.enableAutoCleanup {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "cleanup is not enabled"})
		return
	}

	// Check if dry-run mode - deletion not allowed in dry-run
	if ui.agent.cleanupDryRun {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "deletion not allowed in dry-run mode"})
		return
	}

	// Parse request body
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid request body"})
		return
	}

	if req.Path == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "path is required"})
		return
	}

	// Get orphans and find the matching one
	ctx := r.Context()
	orphans := ui.agent.GetOrphans(ctx)

	var targetOrphan *OrphanInfo
	for _, o := range orphans {
		if o.Path == req.Path {
			targetOrphan = &o
			break
		}
	}

	if targetOrphan == nil {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "orphan not found"})
		return
	}

	// Delete the orphan
	if err := ui.agent.removeOrphan(*targetOrphan); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Audit log
	if ui.agent.auditLogger != nil {
		ui.agent.auditLogger.LogCleanup(targetOrphan.Path, targetOrphan.DirName, 0, nil)
	}

	slog.Info("Orphan deleted via UI", "path", req.Path, "size", targetOrphan.SizeStr)

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"deleted": targetOrphan,
	})
}

func (ui *UIServer) handleAPIHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if ui.historyStore == nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"enabled": false,
			"history": []UsageHistory{},
		})
		return
	}

	path := r.URL.Query().Get("path")
	periodStr := r.URL.Query().Get("period")

	// Default to 24h
	period := 24 * time.Hour
	switch periodStr {
	case "7d":
		period = 7 * 24 * time.Hour
	case "30d":
		period = 30 * 24 * time.Hour
	case "24h", "":
		period = 24 * time.Hour
	}

	end := time.Now()
	start := end.Add(-period)

	history := ui.historyStore.Query(path, start, end)

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"enabled": true,
		"path":    path,
		"period":  periodStr,
		"history": history,
		"stats":   ui.historyStore.GetHistoryStats(),
	})
}

func (ui *UIServer) handleAPITrends(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if ui.historyStore == nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"enabled": false,
			"trends":  []TrendData{},
		})
		return
	}

	path := r.URL.Query().Get("path")

	if path != "" {
		// Single path trend
		trend := ui.historyStore.GetTrend(path)
		if trend == nil {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"enabled": true,
				"trend":   nil,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"enabled": true,
			"trend":   trend,
		})
		return
	}

	// All trends
	trends := ui.historyStore.GetAllTrends()
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"enabled": true,
		"trends":  trends,
		"count":   len(trends),
	})
}

func (ui *UIServer) handleAPIPolicies(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if ui.client == nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"enabled":  false,
			"policies": []NamespacePolicy{},
		})
		return
	}

	ctx := r.Context()
	policies, err := GetAllNamespacePolicies(ctx, ui.client)
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error":    err.Error(),
			"policies": []NamespacePolicy{},
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"enabled":  ui.agent != nil && ui.agent.enablePolicy,
		"policies": policies,
		"count":    len(policies),
	})
}

// FileInfo represents a file or directory entry
type FileInfo struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	SizeStr string `json:"sizeStr"`
	IsDir   bool   `json:"isDir"`
}

func (ui *UIServer) handleAPIFiles(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	path := r.URL.Query().Get("path")
	if path == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "path is required"})
		return
	}

	// Security check: ensure path is under basePath
	if !strings.HasPrefix(path, ui.basePath) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "access denied"})
		return
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	var files []FileInfo
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}

		size := info.Size()
		if entry.IsDir() {
			size = int64(getDirSize(filepath.Join(path, entry.Name())))
		}

		files = append(files, FileInfo{
			Name:    entry.Name(),
			Size:    size,
			SizeStr: formatBytes(size),
			IsDir:   entry.IsDir(),
		})
	}

	// Sort: directories first, then by name
	sort.Slice(files, func(i, j int) bool {
		if files[i].IsDir != files[j].IsDir {
			return files[i].IsDir
		}
		return files[i].Name < files[j].Name
	})

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"path":  path,
		"files": files,
		"count": len(files),
	})
}

func (ui *UIServer) handleAPIViolations(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if ui.client == nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"violations": []PolicyViolation{},
		})
		return
	}

	ctx := r.Context()
	violations, err := GetPolicyViolations(ctx, ui.client)
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error":      err.Error(),
			"violations": []PolicyViolation{},
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"violations": violations,
		"count":      len(violations),
	})
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>NFS Quota Agent - Dashboard</title>
    <style>
        * {
            margin: 0;
            padding: 0;
            box-sizing: border-box;
        }
        body {
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Oxygen, Ubuntu, sans-serif;
            background: #f8fafc;
            color: #1e293b;
            min-height: 100vh;
        }
        body.dark {
            background: #0f172a;
            color: #e2e8f0;
        }
        .container {
            max-width: 1400px;
            margin: 0 auto;
            padding: 20px;
        }
        header {
            display: flex;
            justify-content: space-between;
            align-items: center;
            margin-bottom: 20px;
            padding-bottom: 20px;
            border-bottom: 1px solid #e2e8f0;
        }
        body.dark header { border-bottom-color: #334155; }
        h1 {
            font-size: 1.5rem;
            font-weight: 600;
            color: #1e293b;
        }
        body.dark h1 { color: #f8fafc; }
        .version {
            color: #64748b;
            font-size: 0.875rem;
        }
        .refresh-info {
            color: #64748b;
            font-size: 0.875rem;
        }
        .theme-toggle {
            background: #e2e8f0;
            border: none;
            padding: 8px 12px;
            border-radius: 8px;
            cursor: pointer;
            font-size: 1rem;
        }
        body.dark .theme-toggle { background: #334155; }
        .tabs {
            display: flex;
            gap: 8px;
            margin-bottom: 24px;
            border-bottom: 1px solid #e2e8f0;
            padding-bottom: 8px;
        }
        body.dark .tabs { border-bottom-color: #334155; }
        .tab {
            padding: 10px 20px;
            background: transparent;
            border: none;
            color: #64748b;
            cursor: pointer;
            font-size: 0.875rem;
            font-weight: 500;
            border-radius: 8px 8px 0 0;
            transition: all 0.2s;
        }
        body.dark .tab { color: #94a3b8; }
        .tab:hover {
            color: #1e293b;
            background: #e2e8f0;
        }
        body.dark .tab:hover { color: #e2e8f0; background: #1e293b; }
        .tab.active {
            color: #3b82f6;
            background: #e2e8f0;
            border-bottom: 2px solid #3b82f6;
        }
        body.dark .tab.active { background: #1e293b; }
        .tab-content {
            display: none;
        }
        .tab-content.active {
            display: block;
        }
        .cards {
            display: grid;
            grid-template-columns: repeat(auto-fit, minmax(280px, 1fr));
            gap: 20px;
            margin-bottom: 30px;
        }
        .card {
            background: #ffffff;
            border-radius: 12px;
            padding: 24px;
            border: 1px solid #e2e8f0;
            box-shadow: 0 1px 3px rgba(0,0,0,0.1);
        }
        body.dark .card { background: #1e293b; border-color: #334155; box-shadow: none; }
        .card-title {
            font-size: 0.875rem;
            color: #64748b;
            margin-bottom: 8px;
            text-transform: uppercase;
            letter-spacing: 0.05em;
        }
        body.dark .card-title { color: #94a3b8; }
        .card-value {
            font-size: 2rem;
            font-weight: 700;
            color: #1e293b;
        }
        body.dark .card-value { color: #f8fafc; }
        .card-subtitle {
            font-size: 0.875rem;
            color: #64748b;
            margin-top: 4px;
        }
        .progress-bar {
            height: 8px;
            background: #e2e8f0;
            border-radius: 4px;
            margin-top: 16px;
            overflow: hidden;
        }
        body.dark .progress-bar { background: #334155; }
        .progress-fill {
            height: 100%;
            border-radius: 4px;
            transition: width 0.3s ease;
        }
        .progress-fill.ok { background: linear-gradient(90deg, #22c55e, #16a34a); }
        .progress-fill.warning { background: linear-gradient(90deg, #eab308, #ca8a04); }
        .progress-fill.exceeded { background: linear-gradient(90deg, #ef4444, #dc2626); }

        .status-cards {
            display: grid;
            grid-template-columns: repeat(3, 1fr);
            gap: 16px;
            margin-bottom: 30px;
        }
        .status-card {
            background: #ffffff;
            border-radius: 12px;
            padding: 20px;
            text-align: center;
            border: 1px solid #e2e8f0;
            box-shadow: 0 1px 3px rgba(0,0,0,0.1);
        }
        body.dark .status-card { background: #1e293b; border-color: #334155; box-shadow: none; }
        .status-card.ok { border-left: 4px solid #22c55e; }
        .status-card.warning { border-left: 4px solid #eab308; }
        .status-card.exceeded { border-left: 4px solid #ef4444; }
        .status-count {
            font-size: 2.5rem;
            font-weight: 700;
        }
        .status-card.ok .status-count { color: #22c55e; }
        .status-card.warning .status-count { color: #eab308; }
        .status-card.exceeded .status-count { color: #ef4444; }
        .status-label {
            color: #94a3b8;
            font-size: 0.875rem;
            margin-top: 4px;
        }

        .table-container {
            background: #ffffff;
            border-radius: 12px;
            border: 1px solid #e2e8f0;
            overflow: hidden;
            box-shadow: 0 1px 3px rgba(0,0,0,0.1);
        }
        body.dark .table-container { background: #1e293b; border-color: #334155; box-shadow: none; }
        .table-header {
            display: flex;
            justify-content: space-between;
            align-items: center;
            padding: 16px 24px;
            border-bottom: 1px solid #e2e8f0;
        }
        body.dark .table-header { border-bottom-color: #334155; }
        .table-title {
            font-size: 1.125rem;
            font-weight: 600;
        }
        .search-input {
            background: #f8fafc;
            border: 1px solid #e2e8f0;
            border-radius: 8px;
            padding: 8px 16px;
            color: #1e293b;
            font-size: 0.875rem;
            width: 250px;
        }
        body.dark .search-input { background: #0f172a; border-color: #334155; color: #e2e8f0; }
        .search-input:focus {
            outline: none;
            border-color: #3b82f6;
        }
        table {
            width: 100%;
            border-collapse: collapse;
        }
        th {
            text-align: left;
            padding: 12px 24px;
            background: #f1f5f9;
            color: #64748b;
            font-weight: 500;
            font-size: 0.75rem;
            text-transform: uppercase;
            letter-spacing: 0.05em;
        }
        body.dark th { background: #0f172a; color: #94a3b8; }
        td {
            padding: 16px 24px;
            border-top: 1px solid #e2e8f0;
            font-size: 0.875rem;
        }
        body.dark td { border-top-color: #334155; }
        tr:hover {
            background: #f1f5f9;
        }
        body.dark tr:hover { background: #334155; }
        .dir-name {
            font-weight: 500;
            color: #1e293b;
            max-width: 300px;
            overflow: hidden;
            text-overflow: ellipsis;
            white-space: nowrap;
        }
        body.dark .dir-name { color: #f8fafc; }
        .usage-bar {
            width: 120px;
            height: 6px;
            background: #e2e8f0;
            border-radius: 3px;
            overflow: hidden;
        }
        body.dark .usage-bar { background: #334155; }
        .usage-fill {
            height: 100%;
            border-radius: 3px;
        }
        .badge {
            display: inline-block;
            padding: 4px 12px;
            border-radius: 9999px;
            font-size: 0.75rem;
            font-weight: 500;
        }
        .badge.ok { background: rgba(34, 197, 94, 0.2); color: #22c55e; }
        .badge.warning { background: rgba(234, 179, 8, 0.2); color: #eab308; }
        .badge.exceeded { background: rgba(239, 68, 68, 0.2); color: #ef4444; }
        .badge.no_quota { background: rgba(100, 116, 139, 0.2); color: #64748b; }
        .badge.bound { background: rgba(59, 130, 246, 0.2); color: #3b82f6; }
        .badge.orphaned { background: rgba(249, 115, 22, 0.2); color: #f97316; }
        .pv-info {
            font-size: 0.75rem;
            color: #64748b;
            max-width: 150px;
            overflow: hidden;
            text-overflow: ellipsis;
            white-space: nowrap;
        }
        body.dark .pv-info { color: #94a3b8; }
        .pvc-info {
            font-size: 0.75rem;
        }
        .pvc-ns {
            color: #94a3b8;
            font-size: 0.65rem;
        }
        .sortable {
            cursor: pointer;
            user-select: none;
        }
        .sortable:hover {
            background: #e2e8f0;
        }
        body.dark .sortable:hover { background: #1e293b; }
        .sort-icon {
            opacity: 0.3;
            margin-left: 4px;
        }
        .sortable.asc .sort-icon::after { content: '↑'; }
        .sortable.desc .sort-icon::after { content: '↓'; }
        .sortable.asc .sort-icon, .sortable.desc .sort-icon { opacity: 1; }

        .loading {
            text-align: center;
            padding: 40px;
            color: #64748b;
        }
        .error {
            background: rgba(239, 68, 68, 0.1);
            border: 1px solid #ef4444;
            color: #ef4444;
            padding: 16px;
            border-radius: 8px;
            margin-bottom: 20px;
        }
        .audit-filters {
            display: flex;
            gap: 12px;
            margin-bottom: 16px;
            flex-wrap: wrap;
        }
        .filter-select {
            background: #f8fafc;
            border: 1px solid #e2e8f0;
            border-radius: 8px;
            padding: 8px 12px;
            color: #1e293b;
            font-size: 0.875rem;
        }
        body.dark .filter-select { background: #0f172a; border-color: #334155; color: #e2e8f0; }
        .filter-select:focus {
            outline: none;
            border-color: #3b82f6;
        }
        .filter-checkbox {
            display: flex;
            align-items: center;
            gap: 8px;
            color: #64748b;
            font-size: 0.875rem;
        }
        body.dark .filter-checkbox { color: #94a3b8; }
        .audit-action {
            display: inline-block;
            padding: 2px 8px;
            border-radius: 4px;
            font-size: 0.75rem;
            font-weight: 600;
        }
        .audit-action.CREATE { background: rgba(34, 197, 94, 0.2); color: #22c55e; }
        .audit-action.UPDATE { background: rgba(59, 130, 246, 0.2); color: #3b82f6; }
        .audit-action.DELETE { background: rgba(239, 68, 68, 0.2); color: #ef4444; }
        .audit-action.CLEANUP { background: rgba(168, 85, 247, 0.2); color: #a855f7; }
        .audit-success { color: #22c55e; }
        .audit-fail { color: #ef4444; }
        .audit-error {
            font-size: 0.75rem;
        }
        /* Expandable rows for file browser */
        .expandable-row {
            cursor: pointer;
        }
        .expandable-row:hover {
            background: #f1f5f9;
        }
        body.dark .expandable-row:hover { background: #334155; }
        .expand-icon {
            display: inline-block;
            width: 16px;
            transition: transform 0.2s;
            margin-right: 4px;
        }
        .expand-icon.expanded {
            transform: rotate(90deg);
        }
        .file-list-row {
            background: #f8fafc;
        }
        body.dark .file-list-row { background: #0f172a; }
        .file-list-row td {
            padding: 0 !important;
        }
        .file-list {
            padding: 8px 24px 8px 48px;
            max-height: 300px;
            overflow-y: auto;
        }
        .file-item {
            display: flex;
            justify-content: space-between;
            padding: 6px 12px;
            border-radius: 4px;
            font-size: 0.813rem;
        }
        .file-item:hover {
            background: #e2e8f0;
        }
        body.dark .file-item:hover { background: #334155; }
        .file-item-name {
            display: flex;
            align-items: center;
            gap: 8px;
        }
        .file-icon {
            font-size: 1rem;
        }
        .file-size {
            color: #64748b;
            font-size: 0.75rem;
            color: #ef4444;
            margin-top: 4px;
        }
        .empty-state {
            text-align: center;
            padding: 60px 20px;
            color: #64748b;
        }
        .empty-state-icon {
            font-size: 3rem;
            margin-bottom: 16px;
        }

        @media (max-width: 768px) {
            .status-cards {
                grid-template-columns: 1fr;
            }
            .cards {
                grid-template-columns: 1fr;
            }
            .search-input {
                width: 100%;
                margin-top: 12px;
            }
            .table-header {
                flex-direction: column;
                align-items: flex-start;
            }
        }
    </style>
</head>
<body>
    <div class="container">
        <header>
            <div>
                <h1>📊 NFS Quota Dashboard</h1>
                <span class="version">nfs-quota-agent</span>
            </div>
            <div style="display: flex; align-items: center; gap: 16px;">
                <button class="theme-toggle" onclick="refreshData()" title="Refresh now">🔄</button>
                <button class="theme-toggle" onclick="toggleTheme()" title="Toggle theme">🌙</button>
                <div class="refresh-info">
                    Last updated: <span id="lastUpdate">-</span>
                    <br>Auto-refresh: 10s
                </div>
            </div>
        </header>

        <div id="error" class="error" style="display: none;"></div>

        <div class="tabs">
            <button class="tab active" onclick="switchTab('quotas')">📊 Quotas</button>
            <button class="tab" onclick="switchTab('orphans')" id="tab-btn-orphans" style="display:none;">🗑️ Orphans</button>
            <button class="tab" onclick="switchTab('trends')" id="tab-btn-trends" style="display:none;">📈 Trends</button>
            <button class="tab" onclick="switchTab('policies')" id="tab-btn-policies" style="display:none;">📋 Policies</button>
            <button class="tab" onclick="switchTab('audit')">📋 Audit Logs</button>
        </div>

        <div id="tab-quotas" class="tab-content active">
        <div class="cards">
            <div class="card">
                <div class="card-title">Disk Total</div>
                <div class="card-value" id="diskTotal">-</div>
                <div class="card-subtitle" id="diskPath">-</div>
            </div>
            <div class="card">
                <div class="card-title">Disk Used</div>
                <div class="card-value" id="diskUsed">-</div>
                <div class="card-subtitle" id="diskUsedPct">-</div>
                <div class="progress-bar">
                    <div class="progress-fill ok" id="diskProgress" style="width: 0%"></div>
                </div>
            </div>
            <div class="card">
                <div class="card-title">Disk Available</div>
                <div class="card-value" id="diskAvailable">-</div>
                <div class="card-subtitle" id="filesystem">-</div>
            </div>
            <div class="card">
                <div class="card-title">Total Directories</div>
                <div class="card-value" id="totalDirs">-</div>
                <div class="card-subtitle">with quotas configured</div>
            </div>
        </div>

        <div class="status-cards">
            <div class="status-card ok">
                <div class="status-count" id="okCount">0</div>
                <div class="status-label">OK (&lt;90%)</div>
            </div>
            <div class="status-card warning">
                <div class="status-count" id="warningCount">0</div>
                <div class="status-label">Warning (≥90%)</div>
            </div>
            <div class="status-card exceeded">
                <div class="status-count" id="exceededCount">0</div>
                <div class="status-label">Exceeded (≥100%)</div>
            </div>
        </div>

        <div class="table-container">
            <div class="table-header">
                <span class="table-title">Directory Quotas</span>
                <input type="text" class="search-input" id="searchInput" placeholder="Search directories...">
            </div>
            <table>
                <thead>
                    <tr>
                        <th class="sortable" onclick="sortTable('directory')">Directory <span class="sort-icon">↕</span></th>
                        <th class="sortable" onclick="sortTable('pvName')">PV <span class="sort-icon">↕</span></th>
                        <th class="sortable" onclick="sortTable('pvcName')">PVC <span class="sort-icon">↕</span></th>
                        <th class="sortable" onclick="sortTable('used')">Used <span class="sort-icon">↕</span></th>
                        <th class="sortable" onclick="sortTable('quota')">Quota <span class="sort-icon">↕</span></th>
                        <th class="sortable" onclick="sortTable('usedPct')">Usage <span class="sort-icon">↕</span></th>
                        <th class="sortable" onclick="sortTable('status')">Status <span class="sort-icon">↕</span></th>
                    </tr>
                </thead>
                <tbody id="quotaTable">
                    <tr><td colspan="7" class="loading">Loading...</td></tr>
                </tbody>
            </table>
        </div>
        </div>

        <div id="tab-orphans" class="tab-content">
            <div class="cards" style="grid-template-columns: repeat(4, 1fr);">
                <div class="card">
                    <div class="card-title">Auto-Cleanup</div>
                    <div class="card-value" id="cleanupEnabled">-</div>
                </div>
                <div class="card">
                    <div class="card-title">Mode</div>
                    <div class="card-value" id="cleanupMode">-</div>
                </div>
                <div class="card">
                    <div class="card-title">Grace Period</div>
                    <div class="card-value" id="cleanupGrace">-</div>
                </div>
                <div class="card">
                    <div class="card-title">Orphaned</div>
                    <div class="card-value" id="orphanCount">0</div>
                </div>
            </div>
            <div class="table-container">
                <div class="table-header">
                    <span class="table-title">Orphaned Directories</span>
                    <div style="display: flex; align-items: center; gap: 12px;">
                        <span id="orphanInfo" style="color: #64748b; font-size: 0.875rem;"></span>
                        <button id="deleteSelectedBtn" onclick="deleteSelectedOrphans()" style="display:none; background:#ef4444; color:white; border:none; padding:8px 16px; border-radius:8px; cursor:pointer; font-size:0.875rem;">🗑️ Delete Selected (<span id="selectedCount">0</span>)</button>
                    </div>
                </div>
                <table>
                    <thead>
                        <tr>
                            <th id="orphanSelectHeader" style="display:none; width:40px;"><input type="checkbox" id="selectAllOrphans" onchange="toggleSelectAll(this)"></th>
                            <th class="sortable" onclick="sortOrphans('dirName')">Directory <span class="sort-icon">↕</span></th>
                            <th class="sortable" onclick="sortOrphans('path')">Path <span class="sort-icon">↕</span></th>
                            <th class="sortable" onclick="sortOrphans('size')">Size <span class="sort-icon">↕</span></th>
                            <th class="sortable" onclick="sortOrphans('firstSeen')">First Seen <span class="sort-icon">↕</span></th>
                            <th class="sortable" onclick="sortOrphans('age')">Age <span class="sort-icon">↕</span></th>
                            <th class="sortable" onclick="sortOrphans('canDelete')">Status <span class="sort-icon">↕</span></th>
                        </tr>
                    </thead>
                    <tbody id="orphanTable">
                        <tr><td colspan="7" class="loading">Loading...</td></tr>
                    </tbody>
                </table>
            </div>
        </div>

        <div id="tab-trends" class="tab-content">
            <div class="cards" style="grid-template-columns: repeat(3, 1fr);">
                <div class="card">
                    <div class="card-title">History Entries</div>
                    <div class="card-value" id="historyEntries">-</div>
                </div>
                <div class="card">
                    <div class="card-title">Tracked Paths</div>
                    <div class="card-value" id="historyPaths">-</div>
                </div>
                <div class="card">
                    <div class="card-title">Retention</div>
                    <div class="card-value" id="historyRetention">-</div>
                </div>
            </div>
            <div class="table-container">
                <div class="table-header">
                    <span class="table-title">Usage Trends</span>
                    <span id="trendInfo" style="color: #64748b; font-size: 0.875rem;"></span>
                </div>
                <table>
                    <thead>
                        <tr>
                            <th class="sortable" onclick="sortTrends('dirName')">Directory <span class="sort-icon">↕</span></th>
                            <th class="sortable" onclick="sortTrends('current')">Current <span class="sort-icon">↕</span></th>
                            <th class="sortable" onclick="sortTrends('quota')">Quota <span class="sort-icon">↕</span></th>
                            <th class="sortable" onclick="sortTrends('change24h')">24h Change <span class="sort-icon">↕</span></th>
                            <th class="sortable" onclick="sortTrends('change7d')">7d Change <span class="sort-icon">↕</span></th>
                            <th class="sortable" onclick="sortTrends('change30d')">30d Change <span class="sort-icon">↕</span></th>
                            <th class="sortable" onclick="sortTrends('trend')">Trend <span class="sort-icon">↕</span></th>
                        </tr>
                    </thead>
                    <tbody id="trendTable">
                        <tr><td colspan="7" class="loading">Loading...</td></tr>
                    </tbody>
                </table>
            </div>
        </div>

        <div id="tab-policies" class="tab-content">
            <div class="table-container" style="margin-bottom: 24px;">
                <div class="table-header">
                    <span class="table-title">Namespace Policies</span>
                    <span id="policyCount" style="color: #64748b; font-size: 0.875rem;"></span>
                </div>
                <table>
                    <thead>
                        <tr>
                            <th class="sortable" onclick="sortPolicies('namespace')">Namespace <span class="sort-icon">↕</span></th>
                            <th class="sortable" onclick="sortPolicies('source')">Source <span class="sort-icon">↕</span></th>
                            <th class="sortable" onclick="sortPolicies('min')">Min <span class="sort-icon">↕</span></th>
                            <th class="sortable" onclick="sortPolicies('default')">Default <span class="sort-icon">↕</span></th>
                            <th class="sortable" onclick="sortPolicies('max')">Max <span class="sort-icon">↕</span></th>
                            <th class="sortable" onclick="sortPolicies('resourceQuotaHard')">ResourceQuota <span class="sort-icon">↕</span></th>
                        </tr>
                    </thead>
                    <tbody id="policyTable">
                        <tr><td colspan="6" class="loading">Loading...</td></tr>
                    </tbody>
                </table>
            </div>
            <div class="table-container">
                <div class="table-header">
                    <span class="table-title">Policy Violations</span>
                    <span id="violationCount" style="color: #64748b; font-size: 0.875rem;"></span>
                </div>
                <table>
                    <thead>
                        <tr>
                            <th class="sortable" onclick="sortViolations('namespace')">Namespace <span class="sort-icon">↕</span></th>
                            <th class="sortable" onclick="sortViolations('pvcName')">PVC Name <span class="sort-icon">↕</span></th>
                            <th class="sortable" onclick="sortViolations('pvName')">PV Name <span class="sort-icon">↕</span></th>
                            <th class="sortable" onclick="sortViolations('requested')">Requested <span class="sort-icon">↕</span></th>
                            <th class="sortable" onclick="sortViolations('maxQuota')">Max Allowed <span class="sort-icon">↕</span></th>
                            <th class="sortable" onclick="sortViolations('violationType')">Violation <span class="sort-icon">↕</span></th>
                        </tr>
                    </thead>
                    <tbody id="violationTable">
                        <tr><td colspan="6" class="loading">Loading...</td></tr>
                    </tbody>
                </table>
            </div>
        </div>

        <div id="tab-audit" class="tab-content">
            <div class="audit-filters">
                <select class="filter-select" id="auditActionFilter" onchange="fetchAuditLogs()">
                    <option value="">All Actions</option>
                    <option value="CREATE">CREATE</option>
                    <option value="UPDATE">UPDATE</option>
                    <option value="DELETE">DELETE</option>
                    <option value="CLEANUP">CLEANUP</option>
                </select>
                <select class="filter-select" id="auditLimitFilter" onchange="fetchAuditLogs()">
                    <option value="50">Last 50</option>
                    <option value="100" selected>Last 100</option>
                    <option value="200">Last 200</option>
                    <option value="500">Last 500</option>
                </select>
                <label class="filter-checkbox">
                    <input type="checkbox" id="auditFailsOnly" onchange="fetchAuditLogs()">
                    Show failures only
                </label>
            </div>
            <div class="table-container">
                <div class="table-header">
                    <span class="table-title">Audit Logs</span>
                    <span id="auditCount" style="color: #64748b; font-size: 0.875rem;">-</span>
                </div>
                <table>
                    <thead>
                        <tr>
                            <th>Timestamp</th>
                            <th>Action</th>
                            <th>PV Name</th>
                            <th>Namespace</th>
                            <th>Path</th>
                            <th>Quota</th>
                            <th>Status</th>
                        </tr>
                    </thead>
                    <tbody id="auditTable">
                        <tr><td colspan="7" class="loading">Loading...</td></tr>
                    </tbody>
                </table>
            </div>
        </div>
    </div>

    <script>
        let allQuotas = [];
        let currentSort = { field: 'usedPct', order: 'desc' };

        // Sorting function
        function sortTable(field) {
            // Toggle order if same field
            if (currentSort.field === field) {
                currentSort.order = currentSort.order === 'asc' ? 'desc' : 'asc';
            } else {
                currentSort.field = field;
                currentSort.order = 'asc';
            }

            // Update header icons
            document.querySelectorAll('.sortable').forEach(th => {
                th.classList.remove('asc', 'desc');
            });
            const activeHeader = document.querySelector(` + "`" + `th[onclick="sortTable('${field}')"]` + "`" + `);
            if (activeHeader) {
                activeHeader.classList.add(currentSort.order);
            }

            // Sort data
            const sorted = [...allQuotas].sort((a, b) => {
                let valA = a[field] || '';
                let valB = b[field] || '';

                // Handle numeric fields
                if (typeof valA === 'number' && typeof valB === 'number') {
                    return currentSort.order === 'asc' ? valA - valB : valB - valA;
                }

                // Handle string fields
                valA = String(valA).toLowerCase();
                valB = String(valB).toLowerCase();
                if (currentSort.order === 'asc') {
                    return valA.localeCompare(valB);
                }
                return valB.localeCompare(valA);
            });

            renderQuotas(sorted);
        }

        // Theme toggle
        function toggleTheme() {
            document.body.classList.toggle('dark');
            const isDark = document.body.classList.contains('dark');
            localStorage.setItem('theme', isDark ? 'dark' : 'light');
            document.querySelector('.theme-toggle').textContent = isDark ? '☀️' : '🌙';
        }

        // Load saved theme
        (function() {
            const savedTheme = localStorage.getItem('theme');
            if (savedTheme === 'dark') {
                document.body.classList.add('dark');
                document.querySelector('.theme-toggle').textContent = '☀️';
            }
        })();

        async function fetchStatus() {
            try {
                const response = await fetch('/api/status');
                const data = await response.json();

                if (data.error) {
                    showError(data.error);
                    return;
                }

                hideError();

                document.getElementById('diskTotal').textContent = data.disk.totalStr;
                document.getElementById('diskUsed').textContent = data.disk.usedStr;
                document.getElementById('diskAvailable').textContent = data.disk.availableStr;
                document.getElementById('diskUsedPct').textContent = data.disk.usedPct.toFixed(1) + '% used';
                document.getElementById('diskPath').textContent = data.path;
                document.getElementById('filesystem').textContent = data.filesystem.toUpperCase();
                document.getElementById('totalDirs').textContent = data.summary.totalDirectories;
                document.getElementById('okCount').textContent = data.summary.okCount;
                document.getElementById('warningCount').textContent = data.summary.warningCount;
                document.getElementById('exceededCount').textContent = data.summary.exceededCount;
                document.getElementById('lastUpdate').textContent = new Date().toLocaleTimeString();

                const progress = document.getElementById('diskProgress');
                progress.style.width = data.disk.usedPct + '%';
                progress.className = 'progress-fill ' + getStatusClass(data.disk.usedPct);
            } catch (err) {
                showError('Failed to fetch status: ' + err.message);
            }
        }

        async function fetchQuotas() {
            try {
                const response = await fetch('/api/quotas');
                const data = await response.json();

                if (data.error) {
                    showError(data.error);
                    return;
                }

                allQuotas = data;
                renderQuotas(data);
            } catch (err) {
                showError('Failed to fetch quotas: ' + err.message);
            }
        }

        function renderQuotas(quotas) {
            const tbody = document.getElementById('quotaTable');

            if (!quotas || quotas.length === 0) {
                tbody.innerHTML = '<tr><td colspan="7" class="loading">No quotas found</td></tr>';
                return;
            }

            tbody.innerHTML = quotas.map((q, idx) => {
                const pctStr = q.quota > 0 ? q.usedPct.toFixed(1) + '%' : '-';
                const quotaStr = q.quota > 0 ? q.quotaStr : '-';
                const statusClass = q.status;
                const barWidth = q.quota > 0 ? Math.min(q.usedPct, 100) : 0;
                const barColor = getStatusColor(q.status);
                const rowId = 'quota-row-' + idx;

                // PV info
                const pvStatus = q.pvStatus || 'orphaned';
                const pvName = q.pvName || '-';
                const pvBadge = pvStatus === 'bound'
                    ? '<span class="badge bound">Bound</span>'
                    : '<span class="badge orphaned">Orphaned</span>';
                const pvDisplay = q.pvName
                    ? '<div class="pv-info" title="' + q.pvName + '">' + truncate(q.pvName, 20) + '</div>'
                    : '-';

                // PVC info
                let pvcDisplay = '-';
                if (q.pvcName) {
                    pvcDisplay = '<div class="pvc-info">' + truncate(q.pvcName, 18) +
                        '<div class="pvc-ns">' + (q.namespace || '') + '</div></div>';
                }

                return ` + "`" + `
                    <tr class="expandable-row" onclick="toggleFileList('${rowId}', '${q.path}')">
                        <td><span class="expand-icon" id="icon-${rowId}">▶</span><span class="dir-name" title="${q.path}">${q.directory}</span></td>
                        <td>${pvDisplay}<div style="margin-top:4px">${pvBadge}</div></td>
                        <td>${pvcDisplay}</td>
                        <td>${q.usedStr}</td>
                        <td>${quotaStr}</td>
                        <td>
                            <div style="display: flex; align-items: center; gap: 8px;">
                                <div class="usage-bar">
                                    <div class="usage-fill" style="width: ${barWidth}%; background: ${barColor};"></div>
                                </div>
                                <span>${pctStr}</span>
                            </div>
                        </td>
                        <td><span class="badge ${statusClass}">${formatStatus(q.status)}</span></td>
                    </tr>
                    <tr class="file-list-row" id="${rowId}" style="display:none;">
                        <td colspan="7">
                            <div class="file-list" id="files-${rowId}">
                                <div class="loading">Loading files...</div>
                            </div>
                        </td>
                    </tr>
                ` + "`" + `;
            }).join('');
        }

        // Toggle file list for expandable rows
        async function toggleFileList(rowId, path) {
            const row = document.getElementById(rowId);
            const icon = document.getElementById('icon-' + rowId);
            const filesDiv = document.getElementById('files-' + rowId);

            if (row.style.display === 'none') {
                row.style.display = '';
                icon.classList.add('expanded');

                // Fetch files
                try {
                    const response = await fetch('/api/files?path=' + encodeURIComponent(path));
                    const data = await response.json();

                    if (data.error) {
                        filesDiv.innerHTML = '<div class="empty-state">' + data.error + '</div>';
                        return;
                    }

                    if (!data.files || data.files.length === 0) {
                        filesDiv.innerHTML = '<div class="empty-state">Empty directory</div>';
                        return;
                    }

                    filesDiv.innerHTML = data.files.map(f => {
                        const icon = f.isDir ? '📁' : '📄';
                        return ` + "`" + `
                            <div class="file-item">
                                <span class="file-item-name">
                                    <span class="file-icon">${icon}</span>
                                    <span>${f.name}</span>
                                </span>
                                <span class="file-size">${f.sizeStr}</span>
                            </div>
                        ` + "`" + `;
                    }).join('');
                } catch (err) {
                    filesDiv.innerHTML = '<div class="empty-state">Error: ' + err.message + '</div>';
                }
            } else {
                row.style.display = 'none';
                icon.classList.remove('expanded');
            }
        }

        function truncate(str, len) {
            if (!str) return '';
            if (str.length <= len) return str;
            return str.substring(0, len - 3) + '...';
        }

        function getStatusClass(pct) {
            if (pct >= 100) return 'exceeded';
            if (pct >= 90) return 'warning';
            return 'ok';
        }

        function getStatusColor(status) {
            switch (status) {
                case 'exceeded': return '#ef4444';
                case 'warning': return '#eab308';
                case 'ok': return '#22c55e';
                default: return '#64748b';
            }
        }

        function formatStatus(status) {
            switch (status) {
                case 'exceeded': return 'Exceeded';
                case 'warning': return 'Warning';
                case 'ok': return 'OK';
                case 'no_quota': return 'No Quota';
                default: return status;
            }
        }

        function showError(msg) {
            const el = document.getElementById('error');
            el.textContent = msg;
            el.style.display = 'block';
        }

        function hideError() {
            document.getElementById('error').style.display = 'none';
        }

        document.getElementById('searchInput').addEventListener('input', (e) => {
            const query = e.target.value.toLowerCase();
            const filtered = allQuotas.filter(q =>
                q.directory.toLowerCase().includes(query) ||
                q.path.toLowerCase().includes(query)
            );
            renderQuotas(filtered);
        });

        // Tab switching
        function switchTab(tabName) {
            document.querySelectorAll('.tab').forEach(t => t.classList.remove('active'));
            document.querySelectorAll('.tab-content').forEach(c => c.classList.remove('active'));

            document.querySelector(` + "`" + `.tab[onclick="switchTab('${tabName}')"]` + "`" + `).classList.add('active');
            document.getElementById('tab-' + tabName).classList.add('active');

            switch (tabName) {
                case 'audit':
                    fetchAuditLogs();
                    break;
                case 'orphans':
                    fetchOrphans();
                    break;
                case 'trends':
                    fetchTrends();
                    break;
                case 'policies':
                    fetchPolicies();
                    break;
            }
        }

        // Audit log functions
        async function fetchAuditLogs() {
            const action = document.getElementById('auditActionFilter').value;
            const limit = document.getElementById('auditLimitFilter').value;
            const failsOnly = document.getElementById('auditFailsOnly').checked;

            let url = '/api/audit?limit=' + limit;
            if (action) url += '&action=' + action;
            if (failsOnly) url += '&fails_only=true';

            try {
                const response = await fetch(url);
                const data = await response.json();

                if (data.error) {
                    renderAuditEmpty(data.error);
                    return;
                }

                document.getElementById('auditCount').textContent = data.total + ' entries';
                renderAuditLogs(data.entries);
            } catch (err) {
                renderAuditEmpty('Failed to fetch audit logs: ' + err.message);
            }
        }

        function renderAuditLogs(entries) {
            const tbody = document.getElementById('auditTable');

            if (!entries || entries.length === 0) {
                renderAuditEmpty('No audit logs found');
                return;
            }

            tbody.innerHTML = entries.map(e => {
                const timestamp = new Date(e.timestamp).toLocaleString();
                const quotaStr = e.new_quota_bytes ? formatSize(e.new_quota_bytes) : '-';
                const statusIcon = e.success ? '✓' : '✗';
                const statusClass = e.success ? 'audit-success' : 'audit-fail';

                let pvName = e.pv_name || '-';
                if (pvName.length > 25) pvName = pvName.substring(0, 22) + '...';

                let path = e.path || '-';
                if (path.length > 30) path = '...' + path.substring(path.length - 27);

                return ` + "`" + `
                    <tr>
                        <td style="white-space: nowrap;">${timestamp}</td>
                        <td><span class="audit-action ${e.action}">${e.action}</span></td>
                        <td title="${e.pv_name || ''}">${pvName}</td>
                        <td>${e.namespace || '-'}</td>
                        <td title="${e.path || ''}">${path}</td>
                        <td>${quotaStr}</td>
                        <td>
                            <span class="${statusClass}">${statusIcon}</span>
                            ${e.error ? '<div class="audit-error">' + e.error + '</div>' : ''}
                        </td>
                    </tr>
                ` + "`" + `;
            }).join('');
        }

        function renderAuditEmpty(message) {
            const tbody = document.getElementById('auditTable');
            tbody.innerHTML = ` + "`" + `
                <tr>
                    <td colspan="7">
                        <div class="empty-state">
                            <div class="empty-state-icon">📋</div>
                            <div>${message}</div>
                        </div>
                    </td>
                </tr>
            ` + "`" + `;
        }

        function formatSize(bytes) {
            if (bytes === 0) return '0 B';
            const k = 1024;
            const sizes = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
            const i = Math.floor(Math.log(bytes) / Math.log(k));
            return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + ' ' + sizes[i];
        }

        // Manual refresh
        function refreshData() {
            fetchStatus();
            fetchQuotas();
            // Refresh audit logs if on audit tab
            if (document.getElementById('tab-audit').classList.contains('active')) {
                fetchAuditLogs();
            }
        }

        // Fetch config and show/hide tabs
        async function fetchConfig() {
            try {
                const response = await fetch('/api/config');
                const config = await response.json();

                // Show/hide tabs based on config
                if (!config.auditEnabled) {
                    const auditTab = document.querySelector('.tab[onclick="switchTab(\'audit\')"]');
                    if (auditTab) auditTab.style.display = 'none';
                }
                if (config.cleanupEnabled) {
                    document.getElementById('tab-btn-orphans').style.display = '';
                }
                if (config.historyEnabled) {
                    document.getElementById('tab-btn-trends').style.display = '';
                }
                if (config.policyEnabled) {
                    document.getElementById('tab-btn-policies').style.display = '';
                }
            } catch (err) {
                console.error('Failed to fetch config:', err);
            }
        }

        // Orphans state
        let orphanDeleteEnabled = false;
        let allOrphans = [];

        // Orphans functions
        async function fetchOrphans() {
            try {
                const response = await fetch('/api/orphans');
                const data = await response.json();

                // Update config cards
                document.getElementById('cleanupEnabled').textContent = data.config.enabled ? 'Enabled' : 'Disabled';
                document.getElementById('cleanupMode').textContent = data.config.dryRun ? 'Dry-Run' : 'Live';
                document.getElementById('cleanupGrace').textContent = data.config.gracePeriod;
                document.getElementById('orphanCount').textContent = data.count || 0;
                document.getElementById('orphanInfo').textContent = data.count + ' orphaned directories found';

                // Enable delete functionality only in Live mode (not dry-run)
                // Requires: cleanup enabled AND not in dry-run mode
                orphanDeleteEnabled = data.config.enabled && !data.config.dryRun;
                document.getElementById('orphanSelectHeader').style.display = orphanDeleteEnabled ? '' : 'none';
                document.getElementById('deleteSelectedBtn').style.display = 'none';

                renderOrphans(data.orphans || []);
            } catch (err) {
                console.error('Failed to fetch orphans:', err);
            }
        }

        function renderOrphans(orphans) {
            const tbody = document.getElementById('orphanTable');
            allOrphans = orphans || [];

            const colCount = orphanDeleteEnabled ? 7 : 6;

            if (!orphans || orphans.length === 0) {
                tbody.innerHTML = '<tr><td colspan="' + colCount + '"><div class="empty-state"><div class="empty-state-icon">✓</div><div>No orphaned directories found</div></div></td></tr>';
                return;
            }

            tbody.innerHTML = orphans.map((o, idx) => {
                const status = o.canDelete
                    ? '<span class="badge exceeded">Can Delete</span>'
                    : '<span class="badge warning">In Grace Period</span>';
                const firstSeen = new Date(o.firstSeen).toLocaleString();
                const rowId = 'orphan-row-' + idx;
                const checkbox = orphanDeleteEnabled
                    ? '<td onclick="event.stopPropagation()"><input type="checkbox" class="orphan-checkbox" data-path="' + o.path + '" onchange="updateSelectedCount()"></td>'
                    : '';

                return ` + "`" + `
                    <tr class="expandable-row" onclick="toggleFileList('${rowId}', '${o.path}')">
                        ${checkbox}
                        <td><span class="expand-icon" id="icon-${rowId}">▶</span><span class="dir-name">${o.dirName}</span></td>
                        <td title="${o.path}">${truncate(o.path, 40)}</td>
                        <td>${o.sizeStr}</td>
                        <td>${firstSeen}</td>
                        <td>${o.age}</td>
                        <td>${status}</td>
                    </tr>
                    <tr class="file-list-row" id="${rowId}" style="display:none;">
                        <td colspan="${colCount}">
                            <div class="file-list" id="files-${rowId}">
                                <div class="loading">Loading files...</div>
                            </div>
                        </td>
                    </tr>
                ` + "`" + `;
            }).join('');
        }

        function toggleSelectAll(checkbox) {
            document.querySelectorAll('.orphan-checkbox').forEach(cb => {
                cb.checked = checkbox.checked;
            });
            updateSelectedCount();
        }

        function updateSelectedCount() {
            const allCheckboxes = document.querySelectorAll('.orphan-checkbox');
            const selectedCheckboxes = document.querySelectorAll('.orphan-checkbox:checked');
            const selected = selectedCheckboxes.length;

            document.getElementById('selectedCount').textContent = selected;
            document.getElementById('deleteSelectedBtn').style.display = selected > 0 ? '' : 'none';

            // Update select all checkbox state
            const selectAllCheckbox = document.getElementById('selectAllOrphans');
            if (selectAllCheckbox) {
                selectAllCheckbox.checked = allCheckboxes.length > 0 && selected === allCheckboxes.length;
                selectAllCheckbox.indeterminate = selected > 0 && selected < allCheckboxes.length;
            }
        }

        async function deleteSelectedOrphans() {
            const selectedPaths = Array.from(document.querySelectorAll('.orphan-checkbox:checked'))
                .map(cb => cb.dataset.path);

            if (selectedPaths.length === 0) {
                alert('No orphans selected');
                return;
            }

            const confirmMsg = 'Are you sure you want to delete ' + selectedPaths.length + ' orphaned director' +
                (selectedPaths.length > 1 ? 'ies' : 'y') + '?\n\n' +
                'This action cannot be undone!\n\n' +
                selectedPaths.map(p => '• ' + p.split('/').pop()).join('\n');

            if (!confirm(confirmMsg)) {
                return;
            }

            let deleted = 0;
            let errors = [];

            for (const path of selectedPaths) {
                try {
                    const response = await fetch('/api/orphans/delete', {
                        method: 'POST',
                        headers: { 'Content-Type': 'application/json' },
                        body: JSON.stringify({ path: path })
                    });

                    const data = await response.json();
                    if (data.success) {
                        deleted++;
                    } else {
                        errors.push(path + ': ' + (data.error || 'Unknown error'));
                    }
                } catch (err) {
                    errors.push(path + ': ' + err.message);
                }
            }

            if (errors.length > 0) {
                alert('Deleted ' + deleted + ' director' + (deleted > 1 ? 'ies' : 'y') + '.\n\nErrors:\n' + errors.join('\n'));
            } else {
                alert('Successfully deleted ' + deleted + ' director' + (deleted > 1 ? 'ies' : 'y') + '.');
            }

            // Refresh orphans list
            fetchOrphans();
        }

        // Trends functions
        async function fetchTrends() {
            try {
                const response = await fetch('/api/trends');
                const data = await response.json();

                if (!data.enabled) {
                    document.getElementById('trendTable').innerHTML = '<tr><td colspan="7"><div class="empty-state"><div class="empty-state-icon">📈</div><div>History collection not enabled</div></div></td></tr>';
                    return;
                }

                document.getElementById('trendInfo').textContent = (data.count || 0) + ' paths tracked';

                // Fetch history stats
                const statsResponse = await fetch('/api/history');
                const statsData = await statsResponse.json();
                if (statsData.stats) {
                    document.getElementById('historyEntries').textContent = statsData.stats.entries || 0;
                    document.getElementById('historyPaths').textContent = statsData.stats.paths || 0;
                    document.getElementById('historyRetention').textContent = statsData.stats.retention || '-';
                }

                allTrends = data.trends || [];
                renderTrends(allTrends);
            } catch (err) {
                console.error('Failed to fetch trends:', err);
            }
        }

        function renderTrends(trends) {
            const tbody = document.getElementById('trendTable');

            if (!trends || trends.length === 0) {
                tbody.innerHTML = '<tr><td colspan="7"><div class="empty-state"><div class="empty-state-icon">📈</div><div>No trend data available yet</div></div></td></tr>';
                return;
            }

            tbody.innerHTML = trends.map(t => {
                const trendIcon = t.trend === 'up' ? '↑' : (t.trend === 'down' ? '↓' : '→');
                const trendClass = t.trend === 'up' ? 'audit-fail' : (t.trend === 'down' ? 'audit-success' : '');

                return ` + "`" + `
                    <tr>
                        <td class="dir-name" title="${t.path || t.dirName}">${t.dirName}</td>
                        <td>${t.currentStr}</td>
                        <td>${t.quotaStr || '-'}</td>
                        <td>${formatChange(t.change24h)}</td>
                        <td>${formatChange(t.change7d)}</td>
                        <td>${formatChange(t.change30d)}</td>
                        <td><span class="${trendClass}">${trendIcon}</span></td>
                    </tr>
                ` + "`" + `;
            }).join('');
        }

        function formatChange(bytes) {
            if (bytes === 0) return '-';
            const sign = bytes > 0 ? '+' : '';
            return sign + formatSize(Math.abs(bytes));
        }

        // Sorting functions for all tables
        let orphanSort = { field: 'age', order: 'desc' };
        let trendSort = { field: 'current', order: 'desc' };
        let policySort = { field: 'namespace', order: 'asc' };
        let violationSort = { field: 'namespace', order: 'asc' };
        let allTrends = [];
        let allPolicies = [];
        let allViolations = [];

        function sortOrphans(field) {
            if (orphanSort.field === field) {
                orphanSort.order = orphanSort.order === 'asc' ? 'desc' : 'asc';
            } else {
                orphanSort.field = field;
                orphanSort.order = 'asc';
            }
            updateSortIcons('orphanTable', field, orphanSort.order);
            const sorted = [...allOrphans].sort((a, b) => sortCompare(a, b, field, orphanSort.order));
            renderOrphans(sorted);
        }

        function sortTrends(field) {
            if (trendSort.field === field) {
                trendSort.order = trendSort.order === 'asc' ? 'desc' : 'asc';
            } else {
                trendSort.field = field;
                trendSort.order = 'asc';
            }
            updateSortIcons('trendTable', field, trendSort.order);
            const sorted = [...allTrends].sort((a, b) => sortCompare(a, b, field, trendSort.order));
            renderTrends(sorted);
        }

        function sortPolicies(field) {
            if (policySort.field === field) {
                policySort.order = policySort.order === 'asc' ? 'desc' : 'asc';
            } else {
                policySort.field = field;
                policySort.order = 'asc';
            }
            updateSortIcons('policyTable', field, policySort.order);
            const sorted = [...allPolicies].sort((a, b) => sortCompare(a, b, field, policySort.order));
            renderPolicies(sorted);
        }

        function sortViolations(field) {
            if (violationSort.field === field) {
                violationSort.order = violationSort.order === 'asc' ? 'desc' : 'asc';
            } else {
                violationSort.field = field;
                violationSort.order = 'asc';
            }
            updateSortIcons('violationTable', field, violationSort.order);
            const sorted = [...allViolations].sort((a, b) => sortCompare(a, b, field, violationSort.order));
            renderViolations(sorted);
        }

        function sortCompare(a, b, field, order) {
            let valA = a[field];
            let valB = b[field];

            // Handle undefined/null
            if (valA == null) valA = '';
            if (valB == null) valB = '';

            // Handle numeric values
            if (typeof valA === 'number' && typeof valB === 'number') {
                return order === 'asc' ? valA - valB : valB - valA;
            }

            // Handle string values
            valA = String(valA).toLowerCase();
            valB = String(valB).toLowerCase();
            if (order === 'asc') {
                return valA.localeCompare(valB);
            }
            return valB.localeCompare(valA);
        }

        function updateSortIcons(tableId, field, order) {
            const table = document.getElementById(tableId).closest('table');
            table.querySelectorAll('.sortable').forEach(th => {
                th.classList.remove('asc', 'desc');
            });
            const activeHeader = table.querySelector(` + "`" + `.sortable[onclick*="${field}"]` + "`" + `);
            if (activeHeader) {
                activeHeader.classList.add(order);
            }
        }

        // Policies functions
        async function fetchPolicies() {
            try {
                const [policiesRes, violationsRes] = await Promise.all([
                    fetch('/api/policies'),
                    fetch('/api/violations')
                ]);

                const policiesData = await policiesRes.json();
                const violationsData = await violationsRes.json();

                document.getElementById('policyCount').textContent = (policiesData.count || 0) + ' namespaces with policies';
                document.getElementById('violationCount').textContent = (violationsData.count || 0) + ' violations';

                allPolicies = policiesData.policies || [];
                allViolations = violationsData.violations || [];
                renderPolicies(allPolicies);
                renderViolations(allViolations);
            } catch (err) {
                console.error('Failed to fetch policies:', err);
            }
        }

        function renderPolicies(policies) {
            const tbody = document.getElementById('policyTable');

            if (!policies || policies.length === 0) {
                tbody.innerHTML = '<tr><td colspan="6"><div class="empty-state"><div class="empty-state-icon">📋</div><div>No namespace policies defined</div><div style="font-size:0.875rem;color:#64748b;margin-top:8px;">Create a LimitRange or ResourceQuota, or add nfs.io/default-quota annotation</div></div></td></tr>';
                return;
            }

            tbody.innerHTML = policies.map(p => {
                // Source badge
                let sourceBadge = '';
                switch (p.source) {
                    case 'LimitRange':
                        sourceBadge = '<span class="badge bound">LimitRange</span>';
                        break;
                    case 'Annotation':
                        sourceBadge = '<span class="badge warning">Annotation</span>';
                        break;
                    default:
                        sourceBadge = '<span class="badge no_quota">None</span>';
                }

                // ResourceQuota info
                let rqInfo = '-';
                if (p.resourceQuotaHard > 0) {
                    const usedPct = p.resourceQuotaHard > 0 ? Math.round(p.resourceQuotaUsed / p.resourceQuotaHard * 100) : 0;
                    rqInfo = p.resourceQuotaUsedStr + ' / ' + p.resourceQuotaHardStr + ' (' + usedPct + '%)';
                }

                return ` + "`" + `
                    <tr>
                        <td><strong>${p.namespace}</strong></td>
                        <td>${sourceBadge}</td>
                        <td>${p.minStr || '-'}</td>
                        <td>${p.defaultStr || '-'}</td>
                        <td>${p.maxStr || '-'}</td>
                        <td>${rqInfo}</td>
                    </tr>
                ` + "`" + `;
            }).join('');
        }

        function renderViolations(violations) {
            const tbody = document.getElementById('violationTable');

            if (!violations || violations.length === 0) {
                tbody.innerHTML = '<tr><td colspan="6"><div class="empty-state"><div class="empty-state-icon">✓</div><div>No policy violations</div></div></td></tr>';
                return;
            }

            tbody.innerHTML = violations.map(v => {
                let limitStr = '-';
                let violationBadge = '';

                if (v.violationType === 'exceeds_max') {
                    limitStr = v.maxQuotaStr;
                    violationBadge = '<span class="badge exceeded">Exceeds Max</span>';
                } else if (v.violationType === 'below_min') {
                    limitStr = v.minQuotaStr;
                    violationBadge = '<span class="badge warning">Below Min</span>';
                }

                return ` + "`" + `
                    <tr>
                        <td title="${v.namespace}">${v.namespace}</td>
                        <td title="${v.pvcName}">${truncate(v.pvcName, 25)}</td>
                        <td title="${v.pvName}">${truncate(v.pvName, 25)}</td>
                        <td><span class="audit-fail">${v.requestedStr}</span></td>
                        <td>${limitStr}</td>
                        <td>${violationBadge}</td>
                    </tr>
                ` + "`" + `;
            }).join('');
        }

        // Initial load
        fetchConfig();
        fetchStatus();
        fetchQuotas();

        // Auto-refresh
        setInterval(() => {
            fetchStatus();
            fetchQuotas();
        }, 10000);
    </script>
</body>
</html>`
