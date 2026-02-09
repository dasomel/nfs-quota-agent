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

package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/dasomel/nfs-quota-agent/internal/audit"
	"github.com/dasomel/nfs-quota-agent/internal/history"
	"github.com/dasomel/nfs-quota-agent/internal/policy"
	"github.com/dasomel/nfs-quota-agent/internal/quota"
	"github.com/dasomel/nfs-quota-agent/internal/status"
	"github.com/dasomel/nfs-quota-agent/internal/util"
)

// AgentInterface provides the interface UI needs from the agent
type AgentInterface interface {
	EnableAutoCleanup() bool
	CleanupDryRun() bool
	OrphanGracePeriod() time.Duration
	CleanupInterval() time.Duration
	EnablePolicy() bool
	GetOrphans(ctx context.Context) []OrphanInfo
	RemoveOrphan(orphan OrphanInfo) error
	AuditLogger() *audit.Logger
}

// OrphanInfo represents an orphaned directory
type OrphanInfo struct {
	Path      string    `json:"path"`
	DirName   string    `json:"dirName"`
	Size      uint64    `json:"size"`
	SizeStr   string    `json:"sizeStr"`
	FirstSeen time.Time `json:"firstSeen"`
	Age       string    `json:"age"`
	CanDelete bool      `json:"canDelete"`
}

// PVInfo contains PV and PVC binding information
type PVInfo struct {
	PVName    string
	PVCName   string
	Namespace string
	Phase     string
	NfsPath   string
	Capacity  string
	IsBound   bool
}

// FileInfo represents a file or directory entry
type FileInfo struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	SizeStr string `json:"sizeStr"`
	IsDir   bool   `json:"isDir"`
}

// Options configures the UI server
type Options struct {
	Addr          string
	BasePath      string
	NfsServerPath string
	AuditLogPath  string
	Client        kubernetes.Interface
	Agent         AgentInterface
	HistoryStore  *history.Store
}

// Server serves the web UI
type Server struct {
	basePath      string
	nfsServerPath string
	addr          string
	auditLogPath  string
	client        kubernetes.Interface
	agent         AgentInterface
	historyStore  *history.Store
}

// StartServer starts the web UI server with the given options
func StartServer(opts Options) error {
	ui := &Server{
		basePath:      opts.BasePath,
		nfsServerPath: opts.NfsServerPath,
		addr:          opts.Addr,
		auditLogPath:  opts.AuditLogPath,
		client:        opts.Client,
		agent:         opts.Agent,
		historyStore:  opts.HistoryStore,
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

	slog.Info("Starting Web UI", "addr", opts.Addr, "url", fmt.Sprintf("http://localhost%s", opts.Addr))
	return http.ListenAndServe(opts.Addr, mux)
}

func (ui *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, dashboardHTML)
}

func (ui *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	fsType, _ := quota.DetectFSType(ui.basePath)
	diskUsage, err := status.GetDiskUsage(ui.basePath)
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	dirUsages, _ := status.GetDirUsages(ui.basePath, fsType)

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
			"totalStr":     util.FormatBytes(int64(diskUsage.Total)),
			"usedStr":      util.FormatBytes(int64(diskUsage.Used)),
			"availableStr": util.FormatBytes(int64(diskUsage.Available)),
		},
		"summary": map[string]interface{}{
			"totalDirectories": len(dirUsages),
			"totalUsed":        totalUsed,
			"totalQuota":       totalQuota,
			"totalUsedStr":     util.FormatBytes(int64(totalUsed)),
			"totalQuotaStr":    util.FormatBytes(int64(totalQuota)),
			"okCount":          okCount,
			"warningCount":     warningCount,
			"exceededCount":    exceededCount,
		},
	}

	_ = json.NewEncoder(w).Encode(response)
}

// getPVInfoMap returns a map of directory path to PV info
func (ui *Server) getPVInfoMap(ctx context.Context) map[string]*PVInfo {
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
func (ui *Server) nfsPathToLocal(nfsPath string) string {
	if ui.nfsServerPath != "" && strings.HasPrefix(nfsPath, ui.nfsServerPath) {
		return filepath.Join(ui.basePath, strings.TrimPrefix(nfsPath, ui.nfsServerPath))
	}
	return filepath.Join(ui.basePath, filepath.Base(nfsPath))
}

func (ui *Server) handleAPIQuotas(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	fsType, _ := quota.DetectFSType(ui.basePath)
	dirUsages, err := status.GetDirUsages(ui.basePath, fsType)
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	ctx := r.Context()
	pvMap := ui.getPVInfoMap(ctx)

	sort.Slice(dirUsages, func(i, j int) bool {
		if dirUsages[i].Quota > 0 && dirUsages[j].Quota > 0 {
			return dirUsages[i].QuotaPct > dirUsages[j].QuotaPct
		}
		return dirUsages[i].Used > dirUsages[j].Used
	})

	var quotas []map[string]interface{}
	for _, du := range dirUsages {
		st := "no_quota"
		if du.Quota > 0 {
			if du.QuotaPct >= 100 {
				st = "exceeded"
			} else if du.QuotaPct >= 90 {
				st = "warning"
			} else {
				st = "ok"
			}
		}

		entry := map[string]interface{}{
			"directory": filepath.Base(du.Path),
			"path":      du.Path,
			"used":      du.Used,
			"usedStr":   util.FormatBytes(int64(du.Used)),
			"quota":     du.Quota,
			"quotaStr":  util.FormatBytes(int64(du.Quota)),
			"usedPct":   du.QuotaPct,
			"status":    st,
		}

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

func (ui *Server) handleAPIAudit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	action := r.URL.Query().Get("action")
	failsOnly := r.URL.Query().Get("fails_only") == "true"
	limitStr := r.URL.Query().Get("limit")
	limit := 100
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			limit = l
		}
	}

	filter := audit.Filter{
		Action:    audit.Action(action),
		OnlyFails: failsOnly,
	}

	entries, err := audit.QueryLog(ui.auditLogPath, filter)
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error":   err.Error(),
			"entries": []audit.Entry{},
		})
		return
	}

	if len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}

	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"total":   len(entries),
		"entries": entries,
	})
}

func (ui *Server) handleAPIConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	config := map[string]interface{}{
		"auditEnabled":   ui.auditLogPath != "",
		"cleanupEnabled": ui.agent != nil && ui.agent.EnableAutoCleanup(),
		"historyEnabled": ui.historyStore != nil,
		"policyEnabled":  ui.agent != nil && ui.agent.EnablePolicy(),
	}
	_ = json.NewEncoder(w).Encode(config)
}

func (ui *Server) handleAPIOrphans(w http.ResponseWriter, r *http.Request) {
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
			"enabled":     ui.agent.EnableAutoCleanup(),
			"dryRun":      ui.agent.CleanupDryRun(),
			"gracePeriod": ui.agent.OrphanGracePeriod().String(),
			"interval":    ui.agent.CleanupInterval().String(),
		},
	})
}

func (ui *Server) handleAPIOrphansDelete(w http.ResponseWriter, r *http.Request) {
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

	if !ui.agent.EnableAutoCleanup() {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "cleanup is not enabled"})
		return
	}

	if ui.agent.CleanupDryRun() {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "deletion not allowed in dry-run mode"})
		return
	}

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

	if err := ui.agent.RemoveOrphan(*targetOrphan); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if logger := ui.agent.AuditLogger(); logger != nil {
		logger.LogCleanup(targetOrphan.Path, targetOrphan.DirName, 0, nil)
	}

	slog.Info("Orphan deleted via UI", "path", req.Path, "size", targetOrphan.SizeStr)

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"deleted": targetOrphan,
	})
}

func (ui *Server) handleAPIHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if ui.historyStore == nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"enabled": false,
			"history": []history.UsageHistory{},
		})
		return
	}

	path := r.URL.Query().Get("path")
	periodStr := r.URL.Query().Get("period")

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

	h := ui.historyStore.Query(path, start, end)

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"enabled": true,
		"path":    path,
		"period":  periodStr,
		"history": h,
		"stats":   ui.historyStore.GetHistoryStats(),
	})
}

func (ui *Server) handleAPITrends(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if ui.historyStore == nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"enabled": false,
			"trends":  []history.TrendData{},
		})
		return
	}

	path := r.URL.Query().Get("path")

	if path != "" {
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

	trends := ui.historyStore.GetAllTrends()
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"enabled": true,
		"trends":  trends,
		"count":   len(trends),
	})
}

func (ui *Server) handleAPIPolicies(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if ui.client == nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"enabled":  false,
			"policies": []policy.NamespacePolicy{},
		})
		return
	}

	ctx := r.Context()
	policies, err := policy.GetAllNamespacePolicies(ctx, ui.client)
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error":    err.Error(),
			"policies": []policy.NamespacePolicy{},
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"enabled":  ui.agent != nil && ui.agent.EnablePolicy(),
		"policies": policies,
		"count":    len(policies),
	})
}

func (ui *Server) handleAPIViolations(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if ui.client == nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"violations": []policy.Violation{},
		})
		return
	}

	ctx := r.Context()
	violations, err := policy.GetViolations(ctx, ui.client)
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error":      err.Error(),
			"violations": []policy.Violation{},
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"violations": violations,
		"count":      len(violations),
	})
}

func (ui *Server) handleAPIFiles(w http.ResponseWriter, r *http.Request) {
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
			size = int64(status.GetDirSize(filepath.Join(path, entry.Name())))
		}

		files = append(files, FileInfo{
			Name:    entry.Name(),
			Size:    size,
			SizeStr: util.FormatBytes(size),
			IsDir:   entry.IsDir(),
		})
	}

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
