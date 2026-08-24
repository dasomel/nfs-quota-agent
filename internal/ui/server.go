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

// Package ui implements the web-based dashboard and API handlers for the NFS quota agent.
// It serves an HTML5 interface displaying disk usage, quota statistics, namespace policies,
// audit logs, historical trends, and orphaned directory management.
package ui

import (
	"context"
	"crypto/subtle"
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
	"github.com/dasomel/nfs-quota-agent/internal/pvpath"
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
	// HAActive reports whether this instance currently owns quota
	// enforcement (see agent.QuotaAgent.HAActive, #11). Always true when
	// HA gating is disabled. Checked before a destructive orphan-delete
	// action so the UI can refuse with an honest 409 instead of letting
	// RemoveOrphan's own gate return agent.ErrHAStandby as a generic 500 --
	// agent -> ui is the only allowed import direction, so that sentinel
	// can't be checked by type from this package.
	HAActive() bool
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
	PVName      string
	PVCName     string
	Namespace   string
	Phase       string
	NfsPath     string
	Capacity    string
	IsBound     bool
	QuotaStatus string
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
	AuthToken     string
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
	authToken     string
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
		authToken:     opts.AuthToken,
		client:        opts.Client,
		agent:         opts.Agent,
		historyStore:  opts.HistoryStore,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", ui.handleIndex)
	mux.HandleFunc("/api/status", ui.authMiddleware(ui.handleAPIStatus))
	mux.HandleFunc("/api/quotas", ui.authMiddleware(ui.handleAPIQuotas))
	mux.HandleFunc("/api/audit", ui.authMiddleware(ui.handleAPIAudit))
	mux.HandleFunc("/api/config", ui.authMiddleware(ui.handleAPIConfig))
	mux.HandleFunc("/api/orphans", ui.authMiddleware(ui.handleAPIOrphans))
	mux.HandleFunc("/api/orphans/delete", ui.authMiddleware(ui.handleAPIOrphansDelete))
	mux.HandleFunc("/api/orphans/scan", ui.authMiddleware(ui.handleAPIOrphansScan))
	mux.HandleFunc("/api/orphans/cleanup", ui.authMiddleware(ui.handleAPIOrphansCleanup))
	mux.HandleFunc("/api/history", ui.authMiddleware(ui.handleAPIHistory))
	mux.HandleFunc("/api/trends", ui.authMiddleware(ui.handleAPITrends))
	mux.HandleFunc("/api/policies", ui.authMiddleware(ui.handleAPIPolicies))
	mux.HandleFunc("/api/violations", ui.authMiddleware(ui.handleAPIViolations))
	mux.HandleFunc("/api/files", ui.authMiddleware(ui.handleAPIFiles))

	slog.Info("Starting Web UI", "addr", opts.Addr, "url", fmt.Sprintf("http://localhost%s", opts.Addr))
	server := &http.Server{
		Addr:              opts.Addr,
		Handler:           mux,
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return server.ListenAndServe()
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
		w.WriteHeader(http.StatusInternalServerError)
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
		"fsType":     fsType,
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
		nfsPath := pvpath.NFSPath(&pv)
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

		if pv.Annotations != nil {
			info.QuotaStatus = pv.Annotations["nfs.io/quota-status"]
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

// nfsPathToLocal converts NFS server path to local mount path. Delegates to
// pvpath.ToLocal, the same mapping agent and cleanup use.
func (ui *Server) nfsPathToLocal(nfsPath string) string {
	return pvpath.ToLocal(nfsPath, ui.nfsServerPath, ui.basePath).Path
}

func (ui *Server) handleAPIQuotas(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	fsType, _ := quota.DetectFSType(ui.basePath)
	dirUsages, err := status.GetDirUsages(ui.basePath, fsType)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
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

	quotas := []map[string]interface{}{}
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
			entry["quotaStatus"] = pvInfo.QuotaStatus
		} else {
			entry["pvName"] = ""
			entry["pvPhase"] = ""
			entry["pvcName"] = ""
			entry["namespace"] = ""
			entry["isBound"] = false
			entry["pvStatus"] = "orphaned"
			entry["quotaStatus"] = ""
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
			if limit > 1000 {
				limit = 1000
			}
		}
	}

	pv := r.URL.Query().Get("pv")
	namespace := r.URL.Query().Get("namespace")
	path := r.URL.Query().Get("path")
	startTimeStr := r.URL.Query().Get("start_time")
	endTimeStr := r.URL.Query().Get("end_time")

	var startTime, endTime time.Time
	var err error
	if startTimeStr != "" {
		startTime, err = time.Parse(time.RFC3339, startTimeStr)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid start_time, must be RFC3339"})
			return
		}
	}
	if endTimeStr != "" {
		endTime, err = time.Parse(time.RFC3339, endTimeStr)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid end_time, must be RFC3339"})
			return
		}
	}

	filter := audit.Filter{
		Action:    audit.Action(action),
		OnlyFails: failsOnly,
		PVName:    pv,
		Namespace: namespace,
		Path:      path,
		StartTime: startTime,
		EndTime:   endTime,
	}

	entries, err := audit.QueryLog(ui.auditLogPath, filter)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
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

	// RemoveOrphan itself also refuses (returning agent.ErrHAStandby) when
	// standby, but agent isn't importable here (agent -> ui is the only
	// allowed direction; see CLAUDE.md's "three placements" note) so that
	// sentinel can't be checked by type from this package. Checking
	// HAActive() directly gives the same refusal with an honest status
	// code (409, not RemoveOrphan's generic-error 500) and message instead
	// of round-tripping into RemoveOrphan just to get the same answer.
	if !ui.agent.HAActive() {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "this instance is HA standby; quota mutation refused"})
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
		w.WriteHeader(http.StatusInternalServerError)
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
		w.WriteHeader(http.StatusInternalServerError)
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
	cleanPath := filepath.Clean(path)
	cleanBase := filepath.Clean(ui.basePath)

	rel, err := filepath.Rel(cleanBase, cleanPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "access denied"})
		return
	}

	// Symlink escape check
	if evalPath, err := filepath.EvalSymlinks(cleanPath); err == nil {
		if evalBase, err := filepath.EvalSymlinks(cleanBase); err == nil {
			evalPath = filepath.Clean(evalPath)
			evalBase = filepath.Clean(evalBase)
			relEval, err := filepath.Rel(evalBase, evalPath)
			if err != nil || relEval == ".." || strings.HasPrefix(relEval, ".."+string(filepath.Separator)) || filepath.IsAbs(relEval) {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "access denied"})
				return
			}
		}
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

func (ui *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ui.authToken == "" {
			next(w, r)
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), []byte(ui.authToken)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func (ui *Server) handleAPIOrphansScan(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "method not allowed"})
		return
	}

	if ui.agent == nil || ui.client == nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"orphans": []OrphanInfo{},
			"count":   0,
		})
		return
	}

	ctx := r.Context()
	orphans := ui.agent.GetOrphans(ctx)

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"orphans": orphans,
		"count":   len(orphans),
	})
}

func (ui *Server) handleAPIOrphansCleanup(w http.ResponseWriter, r *http.Request) {
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

	var req struct {
		DryRun  *bool `json:"dryRun"`
		DryRun2 *bool `json:"dry_run"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid request body"})
		return
	}

	dryRun := true
	if req.DryRun != nil {
		dryRun = *req.DryRun
	} else if req.DryRun2 != nil {
		dryRun = *req.DryRun2
	}

	// Same check and reasoning as handleAPIOrphansDelete's HAActive() gate
	// above: refuse the whole request up front for a real (non-dry-run)
	// cleanup attempt rather than let it loop over every orphan calling
	// RemoveOrphan only to have each one individually refuse and get
	// logged as if it were a real per-orphan failure.
	if !dryRun && !ui.agent.HAActive() {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "this instance is HA standby; quota mutation refused"})
		return
	}

	ctx := r.Context()
	orphans := ui.agent.GetOrphans(ctx)
	scanned := ui.countScannedDirectories()

	cleaned := 0
	for _, orphan := range orphans {
		if !dryRun {
			if err := ui.agent.RemoveOrphan(orphan); err != nil {
				slog.Error("Failed to remove orphan in bulk cleanup", "path", orphan.Path, "error", err)
			} else {
				cleaned++
				if logger := ui.agent.AuditLogger(); logger != nil {
					logger.LogCleanup(orphan.Path, orphan.DirName, 0, nil)
				}
			}
		}
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"scanned":  scanned,
		"orphaned": len(orphans),
		"cleaned":  cleaned,
	})
}

func (ui *Server) countScannedDirectories() int {
	entries, err := os.ReadDir(ui.basePath)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || name == "projects" || name == "projid" {
			continue
		}
		count++

		subEntries, err := os.ReadDir(filepath.Join(ui.basePath, name))
		if err != nil {
			continue
		}
		for _, subEntry := range subEntries {
			if subEntry.IsDir() && !strings.HasPrefix(subEntry.Name(), ".") {
				count++
			}
		}
	}
	return count
}
