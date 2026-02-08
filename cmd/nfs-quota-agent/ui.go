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
	ui := &UIServer{
		basePath:      basePath,
		nfsServerPath: nfsServerPath,
		addr:          addr,
		auditLogPath:  auditLogPath,
		client:        client,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", ui.handleIndex)
	mux.HandleFunc("/api/status", ui.handleAPIStatus)
	mux.HandleFunc("/api/quotas", ui.handleAPIQuotas)
	mux.HandleFunc("/api/audit", ui.handleAPIAudit)

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
				if subdir, ok := pv.Spec.CSI.VolumeAttributes["subdir"]; ok && subdir != "" {
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
                        <th>Directory</th>
                        <th>PV</th>
                        <th>PVC</th>
                        <th>Used</th>
                        <th>Quota</th>
                        <th>Usage</th>
                        <th>Status</th>
                    </tr>
                </thead>
                <tbody id="quotaTable">
                    <tr><td colspan="7" class="loading">Loading...</td></tr>
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

            tbody.innerHTML = quotas.map(q => {
                const pctStr = q.quota > 0 ? q.usedPct.toFixed(1) + '%' : '-';
                const quotaStr = q.quota > 0 ? q.quotaStr : '-';
                const statusClass = q.status;
                const barWidth = q.quota > 0 ? Math.min(q.usedPct, 100) : 0;
                const barColor = getStatusColor(q.status);

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
                    <tr>
                        <td><div class="dir-name" title="${q.path}">${q.directory}</div></td>
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
                ` + "`" + `;
            }).join('');
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

            if (tabName === 'audit') {
                fetchAuditLogs();
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

        // Initial load
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
