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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dasomel/nfs-quota-agent/internal/apis/quota/v1alpha1"
	"github.com/dasomel/nfs-quota-agent/internal/audit"
	"github.com/dasomel/nfs-quota-agent/internal/history"
	"github.com/dasomel/nfs-quota-agent/internal/quotapolicy"
	"github.com/dasomel/nfs-quota-agent/internal/status"
)

// historyDirUsage returns a single-entry usage snapshot rooted at
// /base/pvc-a, used to seed a history.Store in tests.
func historyDirUsage(t *testing.T) []status.DirUsage {
	t.Helper()
	return []status.DirUsage{
		{Path: "/base/pvc-a", Used: 1024, Quota: 4096, QuotaPct: 25},
	}
}

// fakeAgent implements AgentInterface for tests.
type fakeAgent struct {
	enableAutoCleanup bool
	cleanupDryRun     bool
	orphanGrace       time.Duration
	cleanupInterval   time.Duration
	enablePolicy      bool
	orphans           []OrphanInfo
	removeErr         error
	removedPaths      []string
	logger            *audit.Logger

	// haActive is a *bool (not bool) so its zero value ("unset") defaults
	// to active, matching agent.QuotaAgent.HAActive's own default when HA
	// gating isn't configured -- see the identical pattern in
	// internal/metrics/metrics_test.go's fakeAgent.
	haActive *bool
}

func (f *fakeAgent) EnableAutoCleanup() bool                     { return f.enableAutoCleanup }
func (f *fakeAgent) CleanupDryRun() bool                         { return f.cleanupDryRun }
func (f *fakeAgent) OrphanGracePeriod() time.Duration            { return f.orphanGrace }
func (f *fakeAgent) CleanupInterval() time.Duration              { return f.cleanupInterval }
func (f *fakeAgent) EnablePolicy() bool                          { return f.enablePolicy }
func (f *fakeAgent) GetOrphans(ctx context.Context) []OrphanInfo { return f.orphans }
func (f *fakeAgent) RemoveOrphan(o OrphanInfo) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removedPaths = append(f.removedPaths, o.Path)
	return nil
}
func (f *fakeAgent) AuditLogger() *audit.Logger { return f.logger }
func (f *fakeAgent) ProjectsFile() string       { return "/etc/projects" }
func (f *fakeAgent) ProjidFile() string         { return "/etc/projid" }
func (f *fakeAgent) HAActive() bool {
	if f.haActive == nil {
		return true
	}
	return *f.haActive
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, size), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func decodeJSON(t *testing.T, body *bytes.Buffer, v interface{}) {
	t.Helper()
	if err := json.Unmarshal(body.Bytes(), v); err != nil {
		t.Fatalf("decode JSON: %v\nbody: %s", err, body.String())
	}
}

func TestHandleIndex(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	srv.handleIndex(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("content-type = %q", ct)
	}
	if w.Body.String() != dashboardHTML {
		t.Fatalf("body does not match embedded dashboard HTML (len=%d vs %d)", w.Body.Len(), len(dashboardHTML))
	}
}

func TestHandleAPIStatus(t *testing.T) {
	base := t.TempDir()
	mustMkdir(t, filepath.Join(base, "pvc-a"))
	mustWriteFile(t, filepath.Join(base, "pvc-a", "f1"), 100)
	mustMkdir(t, filepath.Join(base, "pvc-b"))
	mustWriteFile(t, filepath.Join(base, "pvc-b", "f1"), 200)

	srv := &Server{basePath: base}
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()

	srv.handleAPIStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp map[string]interface{}
	decodeJSON(t, w.Body, &resp)

	if resp["path"] != base {
		t.Errorf("path = %v, want %v", resp["path"], base)
	}
	if resp["fsType"] == nil {
		t.Errorf("fsType field is missing from status response")
	}
	summary, ok := resp["summary"].(map[string]interface{})
	if !ok {
		t.Fatalf("summary field missing or wrong type: %#v", resp["summary"])
	}
	if got := summary["totalDirectories"]; got != float64(2) {
		t.Errorf("totalDirectories = %v, want 2", got)
	}
	disk, ok := resp["disk"].(map[string]interface{})
	if !ok || disk["total"] == nil {
		t.Fatalf("disk field missing or malformed: %#v", resp["disk"])
	}
}

func TestHandleAPIStatus_DiskUsageError(t *testing.T) {
	srv := &Server{basePath: filepath.Join(t.TempDir(), "does-not-exist")}
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()

	srv.handleAPIStatus(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}

	var resp map[string]interface{}
	decodeJSON(t, w.Body, &resp)
	if resp["error"] == nil {
		t.Fatalf("expected error field for invalid base path, got %#v", resp)
	}
}

func TestHandleAPIQuotas_NoClient(t *testing.T) {
	base := t.TempDir()
	mustMkdir(t, filepath.Join(base, "pvc-a"))
	mustWriteFile(t, filepath.Join(base, "pvc-a", "f1"), 50)

	srv := &Server{basePath: base}
	req := httptest.NewRequest(http.MethodGet, "/api/quotas", nil)
	w := httptest.NewRecorder()

	srv.handleAPIQuotas(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var quotas []map[string]interface{}
	decodeJSON(t, w.Body, &quotas)
	if len(quotas) != 1 {
		t.Fatalf("len(quotas) = %d, want 1: %#v", len(quotas), quotas)
	}
	if quotas[0]["pvStatus"] != "orphaned" {
		t.Errorf("pvStatus = %v, want orphaned (no client configured)", quotas[0]["pvStatus"])
	}
	if quotas[0]["status"] != "no_quota" {
		t.Errorf("status = %v, want no_quota", quotas[0]["status"])
	}
	if quotas[0]["quotaStatus"] != "" {
		t.Errorf("quotaStatus = %v, want empty string", quotas[0]["quotaStatus"])
	}
}

func TestHandleAPIQuotas_WithBoundPV(t *testing.T) {
	base := t.TempDir()
	mustMkdir(t, filepath.Join(base, "pvc-a"))
	mustWriteFile(t, filepath.Join(base, "pvc-a", "f1"), 50)

	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pv-1",
			Annotations: map[string]string{
				"nfs.io/quota-status": "applied",
			},
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeSource: v1.PersistentVolumeSource{
				NFS: &v1.NFSVolumeSource{Server: "nfs.example.com", Path: "/export/pvc-a"},
			},
			Capacity: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Gi")},
			ClaimRef: &v1.ObjectReference{Name: "my-pvc", Namespace: "team-a"},
		},
		Status: v1.PersistentVolumeStatus{Phase: v1.VolumeBound},
	}

	srv := &Server{basePath: base, nfsServerPath: "/export", client: fake.NewSimpleClientset(pv)}
	req := httptest.NewRequest(http.MethodGet, "/api/quotas", nil)
	w := httptest.NewRecorder()

	srv.handleAPIQuotas(w, req)

	var quotas []map[string]interface{}
	decodeJSON(t, w.Body, &quotas)
	if len(quotas) != 1 {
		t.Fatalf("len(quotas) = %d, want 1: %#v", len(quotas), quotas)
	}
	if quotas[0]["pvStatus"] != "bound" {
		t.Fatalf("pvStatus = %v, want bound: %#v", quotas[0]["pvStatus"], quotas[0])
	}
	if quotas[0]["pvName"] != "pv-1" || quotas[0]["pvcName"] != "my-pvc" || quotas[0]["namespace"] != "team-a" {
		t.Errorf("PV binding fields not populated: %#v", quotas[0])
	}
	if quotas[0]["isBound"] != true {
		t.Errorf("isBound = %v, want true", quotas[0]["isBound"])
	}
	if quotas[0]["quotaStatus"] != "applied" {
		t.Errorf("quotaStatus = %v, want applied", quotas[0]["quotaStatus"])
	}
}

func TestHandleAPIAudit(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "audit.log")
	logger, err := audit.NewLogger(audit.Config{Enabled: true, FilePath: logPath, MaxFileSize: 1 << 20})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	for i, action := range []audit.Action{audit.ActionCreate, audit.ActionUpdate, audit.ActionDelete} {
		if err := logger.Log(audit.Entry{Action: action, Path: filepath.Join("/base", "pvc"), Success: i != 1}); err != nil {
			t.Fatalf("Log: %v", err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	srv := &Server{auditLogPath: logPath}
	req := httptest.NewRequest(http.MethodGet, "/api/audit?limit=2", nil)
	w := httptest.NewRecorder()
	srv.handleAPIAudit(w, req)

	var resp struct {
		Total   int           `json:"total"`
		Entries []audit.Entry `json:"entries"`
	}
	decodeJSON(t, w.Body, &resp)
	if resp.Total != 2 {
		t.Fatalf("total = %d, want 2", resp.Total)
	}
	// Newest first: last logged action was DELETE.
	if resp.Entries[0].Action != audit.ActionDelete {
		t.Errorf("entries[0].Action = %v, want DELETE (newest first)", resp.Entries[0].Action)
	}
	if resp.Entries[1].Action != audit.ActionUpdate {
		t.Errorf("entries[1].Action = %v, want UPDATE", resp.Entries[1].Action)
	}
}

func TestHandleAPIAudit_FailsOnlyFilter(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "audit.log")
	logger, err := audit.NewLogger(audit.Config{Enabled: true, FilePath: logPath, MaxFileSize: 1 << 20})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	_ = logger.Log(audit.Entry{Action: audit.ActionCreate, Path: "/a", Success: true})
	_ = logger.Log(audit.Entry{Action: audit.ActionCreate, Path: "/b", Success: false, Error: "boom"})

	srv := &Server{auditLogPath: logPath}
	req := httptest.NewRequest(http.MethodGet, "/api/audit?fails_only=true", nil)
	w := httptest.NewRecorder()
	srv.handleAPIAudit(w, req)

	var resp struct {
		Total   int           `json:"total"`
		Entries []audit.Entry `json:"entries"`
	}
	decodeJSON(t, w.Body, &resp)
	if resp.Total != 1 || resp.Entries[0].Path != "/b" {
		t.Fatalf("expected only the failed entry, got %#v", resp)
	}
}

func TestHandleAPIAudit_MissingFile(t *testing.T) {
	srv := &Server{auditLogPath: filepath.Join(t.TempDir(), "missing.log")}
	req := httptest.NewRequest(http.MethodGet, "/api/audit", nil)
	w := httptest.NewRecorder()
	srv.handleAPIAudit(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}

	var resp map[string]interface{}
	decodeJSON(t, w.Body, &resp)
	if resp["error"] == nil {
		t.Fatalf("expected error for missing audit log, got %#v", resp)
	}
	entries, ok := resp["entries"].([]interface{})
	if !ok || len(entries) != 0 {
		t.Fatalf("expected empty entries array, got %#v", resp["entries"])
	}
}

func TestHandleAPIConfig(t *testing.T) {
	cases := []struct {
		name string
		srv  *Server
		want map[string]bool
	}{
		{
			name: "all disabled",
			srv:  &Server{},
			want: map[string]bool{"auditEnabled": false, "cleanupEnabled": false, "historyEnabled": false, "policyEnabled": false},
		},
		{
			name: "all enabled",
			srv: &Server{
				auditLogPath: "/var/log/audit.log",
				agent:        &fakeAgent{enableAutoCleanup: true, enablePolicy: true},
				historyStore: &history.Store{},
			},
			want: map[string]bool{"auditEnabled": true, "cleanupEnabled": true, "historyEnabled": true, "policyEnabled": true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
			w := httptest.NewRecorder()
			tc.srv.handleAPIConfig(w, req)

			var resp map[string]bool
			decodeJSON(t, w.Body, &resp)
			for k, want := range tc.want {
				if resp[k] != want {
					t.Errorf("%s = %v, want %v", k, resp[k], want)
				}
			}
		})
	}
}

func TestHandleAPIOrphans_NoAgentOrClient(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/orphans", nil)
	w := httptest.NewRecorder()
	srv.handleAPIOrphans(w, req)

	var resp map[string]interface{}
	decodeJSON(t, w.Body, &resp)
	orphans, ok := resp["orphans"].([]interface{})
	if !ok || len(orphans) != 0 {
		t.Fatalf("expected empty orphans, got %#v", resp["orphans"])
	}
	cfg, ok := resp["config"].(map[string]interface{})
	if !ok || cfg["enabled"] != false || cfg["dryRun"] != true {
		t.Fatalf("expected default disabled config, got %#v", resp["config"])
	}
}

func TestHandleAPIOrphans_WithAgent(t *testing.T) {
	orphans := []OrphanInfo{{Path: "/base/orphan-1", DirName: "orphan-1", SizeStr: "1.0 KiB"}}
	agent := &fakeAgent{enableAutoCleanup: true, cleanupDryRun: true, orphanGrace: 24 * time.Hour, orphans: orphans}
	srv := &Server{agent: agent, client: fake.NewSimpleClientset()}

	req := httptest.NewRequest(http.MethodGet, "/api/orphans", nil)
	w := httptest.NewRecorder()
	srv.handleAPIOrphans(w, req)

	var resp map[string]interface{}
	decodeJSON(t, w.Body, &resp)
	if resp["count"] != float64(1) {
		t.Fatalf("count = %v, want 1", resp["count"])
	}
	cfg, _ := resp["config"].(map[string]interface{})
	if cfg["enabled"] != true || cfg["dryRun"] != true {
		t.Fatalf("config mismatch: %#v", cfg)
	}
}

func TestHandleAPIOrphansDelete(t *testing.T) {
	orphan := OrphanInfo{Path: "/base/orphan-1", DirName: "orphan-1"}

	t.Run("method not allowed", func(t *testing.T) {
		srv := &Server{agent: &fakeAgent{}}
		req := httptest.NewRequest(http.MethodGet, "/api/orphans/delete", nil)
		w := httptest.NewRecorder()
		srv.handleAPIOrphansDelete(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", w.Code)
		}
	})

	t.Run("no agent", func(t *testing.T) {
		srv := &Server{}
		req := httptest.NewRequest(http.MethodPost, "/api/orphans/delete", strings.NewReader(`{"path":"/x"}`))
		w := httptest.NewRecorder()
		srv.handleAPIOrphansDelete(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	})

	t.Run("cleanup disabled", func(t *testing.T) {
		srv := &Server{agent: &fakeAgent{enableAutoCleanup: false}}
		req := httptest.NewRequest(http.MethodPost, "/api/orphans/delete", strings.NewReader(`{"path":"/x"}`))
		w := httptest.NewRecorder()
		srv.handleAPIOrphansDelete(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
	})

	t.Run("dry run", func(t *testing.T) {
		srv := &Server{agent: &fakeAgent{enableAutoCleanup: true, cleanupDryRun: true}}
		req := httptest.NewRequest(http.MethodPost, "/api/orphans/delete", strings.NewReader(`{"path":"/x"}`))
		w := httptest.NewRecorder()
		srv.handleAPIOrphansDelete(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
	})

	t.Run("invalid body", func(t *testing.T) {
		srv := &Server{agent: &fakeAgent{enableAutoCleanup: true}}
		req := httptest.NewRequest(http.MethodPost, "/api/orphans/delete", strings.NewReader(`not-json`))
		w := httptest.NewRecorder()
		srv.handleAPIOrphansDelete(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	})

	t.Run("missing path", func(t *testing.T) {
		srv := &Server{agent: &fakeAgent{enableAutoCleanup: true}}
		req := httptest.NewRequest(http.MethodPost, "/api/orphans/delete", strings.NewReader(`{"path":""}`))
		w := httptest.NewRecorder()
		srv.handleAPIOrphansDelete(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	})

	t.Run("orphan not found", func(t *testing.T) {
		srv := &Server{agent: &fakeAgent{enableAutoCleanup: true, orphans: nil}}
		req := httptest.NewRequest(http.MethodPost, "/api/orphans/delete", strings.NewReader(`{"path":"/missing"}`))
		w := httptest.NewRecorder()
		srv.handleAPIOrphansDelete(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", w.Code)
		}
	})

	t.Run("remove error", func(t *testing.T) {
		agent := &fakeAgent{enableAutoCleanup: true, orphans: []OrphanInfo{orphan}, removeErr: os.ErrPermission}
		srv := &Server{agent: agent}
		req := httptest.NewRequest(http.MethodPost, "/api/orphans/delete", strings.NewReader(`{"path":"/base/orphan-1"}`))
		w := httptest.NewRecorder()
		srv.handleAPIOrphansDelete(w, req)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", w.Code)
		}
	})

	t.Run("success logs cleanup", func(t *testing.T) {
		logPath := filepath.Join(t.TempDir(), "audit.log")
		logger, err := audit.NewLogger(audit.Config{Enabled: true, FilePath: logPath, MaxFileSize: 1 << 20})
		if err != nil {
			t.Fatalf("NewLogger: %v", err)
		}
		defer logger.Close()

		agent := &fakeAgent{enableAutoCleanup: true, orphans: []OrphanInfo{orphan}, logger: logger}
		srv := &Server{agent: agent}
		req := httptest.NewRequest(http.MethodPost, "/api/orphans/delete", strings.NewReader(`{"path":"/base/orphan-1"}`))
		w := httptest.NewRecorder()
		srv.handleAPIOrphansDelete(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		var resp map[string]interface{}
		decodeJSON(t, w.Body, &resp)
		if resp["success"] != true {
			t.Fatalf("success = %v, want true", resp["success"])
		}
		if len(agent.removedPaths) != 1 || agent.removedPaths[0] != orphan.Path {
			t.Fatalf("RemoveOrphan not called with expected path: %#v", agent.removedPaths)
		}

		entries, err := audit.QueryLog(logPath, audit.Filter{})
		if err != nil {
			t.Fatalf("QueryLog: %v", err)
		}
		if len(entries) != 1 || entries[0].Action != audit.ActionCleanup {
			t.Fatalf("expected one CLEANUP audit entry, got %#v", entries)
		}
	})
}

func TestHandleAPIHistory_Disabled(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/history", nil)
	w := httptest.NewRecorder()
	srv.handleAPIHistory(w, req)

	var resp map[string]interface{}
	decodeJSON(t, w.Body, &resp)
	if resp["enabled"] != false {
		t.Fatalf("enabled = %v, want false", resp["enabled"])
	}
}

func TestHandleAPIHistory_Enabled(t *testing.T) {
	store, err := history.NewStore(filepath.Join(t.TempDir(), "history.json"), time.Minute, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Record(historyDirUsage(t)); err != nil {
		t.Fatalf("Record: %v", err)
	}

	srv := &Server{historyStore: store}
	for _, period := range []string{"", "24h", "7d", "30d"} {
		t.Run("period="+period, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/history?path=/base/pvc-a&period="+period, nil)
			w := httptest.NewRecorder()
			srv.handleAPIHistory(w, req)

			var resp map[string]interface{}
			decodeJSON(t, w.Body, &resp)
			if resp["enabled"] != true {
				t.Fatalf("enabled = %v, want true", resp["enabled"])
			}
			hist, ok := resp["history"].([]interface{})
			if !ok || len(hist) != 1 {
				t.Fatalf("expected one history entry, got %#v", resp["history"])
			}
		})
	}
}

func TestHandleAPITrends_Disabled(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/trends", nil)
	w := httptest.NewRecorder()
	srv.handleAPITrends(w, req)

	var resp map[string]interface{}
	decodeJSON(t, w.Body, &resp)
	if resp["enabled"] != false {
		t.Fatalf("enabled = %v, want false", resp["enabled"])
	}
}

func TestHandleAPITrends_UnknownPath(t *testing.T) {
	store, err := history.NewStore(filepath.Join(t.TempDir(), "history.json"), time.Minute, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	srv := &Server{historyStore: store}
	req := httptest.NewRequest(http.MethodGet, "/api/trends?path=/nowhere", nil)
	w := httptest.NewRecorder()
	srv.handleAPITrends(w, req)

	var resp map[string]interface{}
	decodeJSON(t, w.Body, &resp)
	if resp["enabled"] != true {
		t.Fatalf("enabled = %v, want true", resp["enabled"])
	}
	if resp["trend"] != nil {
		t.Fatalf("trend = %v, want nil for unknown path", resp["trend"])
	}
}

func TestHandleAPITrends_All(t *testing.T) {
	store, err := history.NewStore(filepath.Join(t.TempDir(), "history.json"), time.Minute, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Record(historyDirUsage(t)); err != nil {
		t.Fatalf("Record: %v", err)
	}

	srv := &Server{historyStore: store}
	req := httptest.NewRequest(http.MethodGet, "/api/trends", nil)
	w := httptest.NewRecorder()
	srv.handleAPITrends(w, req)

	var resp map[string]interface{}
	decodeJSON(t, w.Body, &resp)
	if resp["count"] != float64(1) {
		t.Fatalf("count = %v, want 1", resp["count"])
	}
}

func TestHandleAPIPolicies_NoClient(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/policies", nil)
	w := httptest.NewRecorder()
	srv.handleAPIPolicies(w, req)

	var resp map[string]interface{}
	decodeJSON(t, w.Body, &resp)
	if resp["enabled"] != false {
		t.Fatalf("enabled = %v, want false", resp["enabled"])
	}
}

func TestHandleAPIPolicies_WithClient(t *testing.T) {
	srv := &Server{client: fake.NewSimpleClientset(), agent: &fakeAgent{enablePolicy: true}}
	req := httptest.NewRequest(http.MethodGet, "/api/policies", nil)
	w := httptest.NewRecorder()
	srv.handleAPIPolicies(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	decodeJSON(t, w.Body, &resp)
	if resp["enabled"] != true {
		t.Fatalf("enabled = %v, want true", resp["enabled"])
	}
	if resp["count"] != float64(0) {
		t.Fatalf("count = %v, want 0 for empty cluster", resp["count"])
	}
}

func TestHandleAPIViolations(t *testing.T) {
	t.Run("no client", func(t *testing.T) {
		srv := &Server{}
		req := httptest.NewRequest(http.MethodGet, "/api/violations", nil)
		w := httptest.NewRecorder()
		srv.handleAPIViolations(w, req)

		var resp map[string]interface{}
		decodeJSON(t, w.Body, &resp)
		violations, ok := resp["violations"].([]interface{})
		if !ok || len(violations) != 0 {
			t.Fatalf("expected empty violations, got %#v", resp["violations"])
		}
	})

	t.Run("with client", func(t *testing.T) {
		srv := &Server{client: fake.NewSimpleClientset()}
		req := httptest.NewRequest(http.MethodGet, "/api/violations", nil)
		w := httptest.NewRecorder()
		srv.handleAPIViolations(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		var resp map[string]interface{}
		decodeJSON(t, w.Body, &resp)
		if resp["count"] != float64(0) {
			t.Fatalf("count = %v, want 0 for empty cluster", resp["count"])
		}
	})
}

// TestHandleAPIQuotaPolicies is the REST-facade test for #13's acceptance
// item: the endpoint must return the same state quotapolicy.List reads
// from the CRD, not a separately maintained copy.
func TestHandleAPIQuotaPolicies(t *testing.T) {
	t.Run("no dynamic client", func(t *testing.T) {
		srv := &Server{}
		req := httptest.NewRequest(http.MethodGet, "/api/quota-policies", nil)
		w := httptest.NewRecorder()
		srv.handleAPIQuotaPolicies(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		var resp map[string]interface{}
		decodeJSON(t, w.Body, &resp)
		if resp["enabled"] != false {
			t.Fatalf("enabled = %v, want false", resp["enabled"])
		}
		items, ok := resp["items"].([]interface{})
		if !ok || len(items) != 0 {
			t.Fatalf("expected empty items, got %#v", resp["items"])
		}
	})

	t.Run("with dynamic client", func(t *testing.T) {
		max := *resource.NewQuantity(5*1024*1024*1024, resource.BinarySI)
		qp := &v1alpha1.QuotaPolicy{
			TypeMeta:   metav1.TypeMeta{APIVersion: "quota.nfs.io/v1alpha1", Kind: "QuotaPolicy"},
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "p", Generation: 1},
			Spec: v1alpha1.QuotaPolicySpec{
				Selector: v1alpha1.QuotaPolicySelector{}, Priority: 100,
				MaxQuota: &max, EnforceMax: true,
			},
			Status: v1alpha1.QuotaPolicyStatus{ObservedGeneration: 1},
		}
		u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(qp)
		if err != nil {
			t.Fatal(err)
		}
		dc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
			map[schema.GroupVersionResource]string{quotapolicy.GroupVersionResource: "QuotaPolicyList"},
			&unstructured.Unstructured{Object: u})

		srv := &Server{dynamicClient: dc}
		req := httptest.NewRequest(http.MethodGet, "/api/quota-policies", nil)
		w := httptest.NewRecorder()
		srv.handleAPIQuotaPolicies(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		var resp struct {
			Enabled bool                   `json:"enabled"`
			Count   int                    `json:"count"`
			Items   []v1alpha1.QuotaPolicy `json:"items"`
		}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !resp.Enabled {
			t.Fatalf("enabled = %v, want true", resp.Enabled)
		}
		if resp.Count != 1 || len(resp.Items) != 1 {
			t.Fatalf("expected exactly 1 item, got count=%d items=%d", resp.Count, len(resp.Items))
		}
		got := resp.Items[0]
		if got.Namespace != "default" || got.Name != "p" {
			t.Errorf("got %s/%s, want default/p", got.Namespace, got.Name)
		}
		if got.Spec.MaxQuota == nil || got.Spec.MaxQuota.Value() != max.Value() {
			t.Errorf("spec.maxQuota = %v, want %v -- the endpoint must return the same spec the CRD has, not a summarized/lossy copy", got.Spec.MaxQuota, max.Value())
		}
		if got.Status.ObservedGeneration != 1 {
			t.Errorf("status.observedGeneration = %d, want 1 -- status must round-trip too, not just spec", got.Status.ObservedGeneration)
		}
	})
}

func TestHandleAPIFiles(t *testing.T) {
	base := t.TempDir()
	mustWriteFile(t, filepath.Join(base, "b-file.txt"), 10)
	mustMkdir(t, filepath.Join(base, "a-dir"))
	mustWriteFile(t, filepath.Join(base, "a-dir", "nested.txt"), 30)

	srv := &Server{basePath: base}

	t.Run("missing path param", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/files", nil)
		w := httptest.NewRecorder()
		srv.handleAPIFiles(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	})

	t.Run("path outside base is denied", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/files?path="+filepath.Dir(base), nil)
		w := httptest.NewRecorder()
		srv.handleAPIFiles(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
	})

	t.Run("symlink escaping base is denied", func(t *testing.T) {
		outsideDir := t.TempDir()
		mustMkdir(t, outsideDir)
		mustWriteFile(t, filepath.Join(outsideDir, "secret.txt"), 100)

		symlinkPath := filepath.Join(base, "escaped-link")
		if err := os.Symlink(outsideDir, symlinkPath); err != nil {
			t.Skipf("skipping symlink test: %v", err)
		}
		defer os.Remove(symlinkPath)

		req := httptest.NewRequest(http.MethodGet, "/api/files?path="+symlinkPath, nil)
		w := httptest.NewRecorder()
		srv.handleAPIFiles(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 for escaping symlink", w.Code)
		}
	})

	t.Run("symlink inside base is allowed", func(t *testing.T) {
		innerDir := filepath.Join(base, "a-dir")
		symlinkPath := filepath.Join(base, "inner-link")
		if err := os.Symlink(innerDir, symlinkPath); err != nil {
			t.Skipf("skipping symlink test: %v", err)
		}
		defer os.Remove(symlinkPath)

		req := httptest.NewRequest(http.MethodGet, "/api/files?path="+symlinkPath, nil)
		w := httptest.NewRecorder()
		srv.handleAPIFiles(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 for inner symlink", w.Code)
		}
	})

	t.Run("nonexistent path under base", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/files?path="+filepath.Join(base, "missing"), nil)
		w := httptest.NewRecorder()
		srv.handleAPIFiles(w, req)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", w.Code)
		}
	})

	t.Run("lists directories before files, alphabetically, with recursive dir size", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/files?path="+base, nil)
		w := httptest.NewRecorder()
		srv.handleAPIFiles(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		var resp struct {
			Files []FileInfo `json:"files"`
			Count int        `json:"count"`
		}
		decodeJSON(t, w.Body, &resp)
		if resp.Count != 2 {
			t.Fatalf("count = %d, want 2", resp.Count)
		}
		if !resp.Files[0].IsDir || resp.Files[0].Name != "a-dir" {
			t.Fatalf("files[0] = %#v, want directory a-dir first", resp.Files[0])
		}
		if resp.Files[0].Size != 30 {
			t.Errorf("a-dir size = %d, want 30 (recursive)", resp.Files[0].Size)
		}
		if resp.Files[1].IsDir || resp.Files[1].Name != "b-file.txt" {
			t.Fatalf("files[1] = %#v, want file b-file.txt", resp.Files[1])
		}
	})
}

func TestAuthMiddleware(t *testing.T) {
	called := false
	next := func(w http.ResponseWriter, r *http.Request) { called = true }

	t.Run("no token configured passes through", func(t *testing.T) {
		called = false
		srv := &Server{}
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		w := httptest.NewRecorder()
		srv.authMiddleware(next)(w, req)
		if !called {
			t.Fatal("expected next handler to be called")
		}
	})

	t.Run("correct bearer token passes through", func(t *testing.T) {
		called = false
		srv := &Server{authToken: "secret"}
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		req.Header.Set("Authorization", "Bearer secret")
		w := httptest.NewRecorder()
		srv.authMiddleware(next)(w, req)
		if !called {
			t.Fatal("expected next handler to be called")
		}
	})

	t.Run("missing or wrong token is rejected", func(t *testing.T) {
		called = false
		srv := &Server{authToken: "secret"}
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		w := httptest.NewRecorder()
		srv.authMiddleware(next)(w, req)
		if called {
			t.Fatal("next handler should not be called without valid token")
		}
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
	})
}

func TestNfsPathToLocal(t *testing.T) {
	srv := &Server{basePath: "/data/nfs", nfsServerPath: "/export"}

	if got := srv.nfsPathToLocal("/export/pvc-1"); got != "/data/nfs/pvc-1" {
		t.Errorf("nfsPathToLocal with matching prefix = %q, want /data/nfs/pvc-1", got)
	}
	if got := srv.nfsPathToLocal("/other/pvc-2"); got != "/data/nfs/pvc-2" {
		t.Errorf("nfsPathToLocal without matching prefix = %q, want /data/nfs/pvc-2 (falls back to basename)", got)
	}
}

func TestGetPVInfoMap_NilClient(t *testing.T) {
	srv := &Server{}
	m := srv.getPVInfoMap(context.Background())
	if len(m) != 0 {
		t.Fatalf("expected empty map for nil client, got %#v", m)
	}
}

func TestGetPVInfoMap_CSIVolume(t *testing.T) {
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-csi"},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: &v1.CSIPersistentVolumeSource{
					Driver: "nfs.csi.k8s.io",
					VolumeAttributes: map[string]string{
						"share":  "/export",
						"subDir": "pvc-csi",
					},
				},
			},
		},
		Status: v1.PersistentVolumeStatus{Phase: v1.VolumeBound},
	}
	srv := &Server{basePath: "/data/nfs", nfsServerPath: "/export", client: fake.NewSimpleClientset(pv)}
	m := srv.getPVInfoMap(context.Background())
	info, ok := m["/data/nfs/pvc-csi"]
	if !ok {
		t.Fatalf("expected PV info for /data/nfs/pvc-csi, got keys %v", mapKeys(m))
	}
	if info.PVName != "pv-csi" {
		t.Errorf("PVName = %q, want pv-csi", info.PVName)
	}
}

func mapKeys(m map[string]*PVInfo) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func TestHandleAPIAudit_FiltersAndClamp(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "audit.log")

	now := time.Now().UTC().Truncate(time.Second)
	entries := []audit.Entry{
		{Action: audit.ActionCreate, PVName: "pv-1", Namespace: "ns-1", Path: "/base/pvc-1", Success: true, Timestamp: now.Add(-10 * time.Minute)},
		{Action: audit.ActionUpdate, PVName: "pv-2", Namespace: "ns-2", Path: "/base/pvc-2", Success: true, Timestamp: now.Add(-5 * time.Minute)},
		{Action: audit.ActionDelete, PVName: "pv-3", Namespace: "ns-1", Path: "/base/pvc-3", Success: false, Timestamp: now},
	}

	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	for _, entry := range entries {
		data, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if _, err := file.Write(append(data, '\n')); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	file.Close()

	srv := &Server{auditLogPath: logPath}

	t.Run("filter by pv", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/audit?pv=pv-2", nil)
		w := httptest.NewRecorder()
		srv.handleAPIAudit(w, req)
		var resp struct {
			Entries []audit.Entry `json:"entries"`
		}
		decodeJSON(t, w.Body, &resp)
		if len(resp.Entries) != 1 || resp.Entries[0].PVName != "pv-2" {
			t.Fatalf("expected 1 entry with PV pv-2, got: %+v", resp.Entries)
		}
	})

	t.Run("filter by namespace", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/audit?namespace=ns-1", nil)
		w := httptest.NewRecorder()
		srv.handleAPIAudit(w, req)
		var resp struct {
			Entries []audit.Entry `json:"entries"`
		}
		decodeJSON(t, w.Body, &resp)
		if len(resp.Entries) != 2 {
			t.Fatalf("expected 2 entries for namespace ns-1, got: %d", len(resp.Entries))
		}
	})

	t.Run("filter by path", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/audit?path=/base/pvc-3", nil)
		w := httptest.NewRecorder()
		srv.handleAPIAudit(w, req)
		var resp struct {
			Entries []audit.Entry `json:"entries"`
		}
		decodeJSON(t, w.Body, &resp)
		if len(resp.Entries) != 1 || resp.Entries[0].Path != "/base/pvc-3" {
			t.Fatalf("expected 1 entry for path /base/pvc-3, got: %+v", resp.Entries)
		}
	})

	t.Run("filter by time range", func(t *testing.T) {
		start := now.Add(-7 * time.Minute).Format(time.RFC3339)
		end := now.Add(-2 * time.Minute).Format(time.RFC3339)
		req := httptest.NewRequest(http.MethodGet, "/api/audit?start_time="+start+"&end_time="+end, nil)
		w := httptest.NewRecorder()
		srv.handleAPIAudit(w, req)
		var resp struct {
			Entries []audit.Entry `json:"entries"`
		}
		decodeJSON(t, w.Body, &resp)
		if len(resp.Entries) != 1 || resp.Entries[0].PVName != "pv-2" {
			t.Fatalf("expected 1 entry (pv-2) in time range, got: %+v", resp.Entries)
		}
	})

	t.Run("invalid start_time", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/audit?start_time=invalid", nil)
		w := httptest.NewRecorder()
		srv.handleAPIAudit(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	})

	t.Run("limit clamp", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/audit?limit=2000", nil)
		w := httptest.NewRecorder()
		srv.handleAPIAudit(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	})
}

func TestHandleAPIOrphansScanAndCleanup(t *testing.T) {
	base := t.TempDir()
	mustMkdir(t, filepath.Join(base, "pvc-orphan1"))
	mustMkdir(t, filepath.Join(base, "pvc-orphan2"))

	orphans := []OrphanInfo{
		{Path: filepath.Join(base, "pvc-orphan1"), DirName: "pvc-orphan1", SizeStr: "100 B"},
		{Path: filepath.Join(base, "pvc-orphan2"), DirName: "pvc-orphan2", SizeStr: "200 B"},
	}
	agent := &fakeAgent{
		enableAutoCleanup: true,
		orphans:           orphans,
	}
	srv := &Server{
		basePath: base,
		agent:    agent,
		client:   fake.NewSimpleClientset(),
	}

	t.Run("scan endpoint", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/orphans/scan", nil)
		w := httptest.NewRecorder()
		srv.handleAPIOrphansScan(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}

		var resp map[string]interface{}
		decodeJSON(t, w.Body, &resp)
		count, ok := resp["count"].(float64)
		if !ok || count != 2 {
			t.Fatalf("count = %v, want 2", resp["count"])
		}
	})

	t.Run("cleanup endpoint dryRun=true", func(t *testing.T) {
		agent.removedPaths = nil
		body := `{"dryRun": true}`
		req := httptest.NewRequest(http.MethodPost, "/api/orphans/cleanup", strings.NewReader(body))
		w := httptest.NewRecorder()
		srv.handleAPIOrphansCleanup(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}

		var resp map[string]interface{}
		decodeJSON(t, w.Body, &resp)
		if resp["cleaned"].(float64) != 0 {
			t.Errorf("cleaned = %v, want 0 for dryRun=true", resp["cleaned"])
		}
		if resp["orphaned"].(float64) != 2 {
			t.Errorf("orphaned = %v, want 2", resp["orphaned"])
		}
		if resp["scanned"].(float64) != 2 {
			t.Errorf("scanned = %v, want 2, got %v", resp["scanned"], resp["scanned"])
		}
		if len(agent.removedPaths) != 0 {
			t.Errorf("expected no paths removed, but got: %v", agent.removedPaths)
		}
	})

	t.Run("cleanup endpoint dryRun=false", func(t *testing.T) {
		agent.removedPaths = nil
		body := `{"dryRun": false}`
		req := httptest.NewRequest(http.MethodPost, "/api/orphans/cleanup", strings.NewReader(body))
		w := httptest.NewRecorder()
		srv.handleAPIOrphansCleanup(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}

		var resp map[string]interface{}
		decodeJSON(t, w.Body, &resp)
		if resp["cleaned"].(float64) != 2 {
			t.Errorf("cleaned = %v, want 2 for dryRun=false", resp["cleaned"])
		}
		if len(agent.removedPaths) != 2 {
			t.Errorf("expected 2 paths removed, but got: %v", agent.removedPaths)
		}
	})

	t.Run("scan endpoint methods and errors", func(t *testing.T) {
		// GET is not allowed
		req := httptest.NewRequest(http.MethodGet, "/api/orphans/scan", nil)
		w := httptest.NewRecorder()
		srv.handleAPIOrphansScan(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", w.Code)
		}

		// Nil agent or client
		srvNil := &Server{}
		req2 := httptest.NewRequest(http.MethodPost, "/api/orphans/scan", nil)
		w2 := httptest.NewRecorder()
		srvNil.handleAPIOrphansScan(w2, req2)
		if w2.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w2.Code)
		}
	})

	t.Run("cleanup endpoint methods and errors", func(t *testing.T) {
		// GET is not allowed
		req := httptest.NewRequest(http.MethodGet, "/api/orphans/cleanup", nil)
		w := httptest.NewRecorder()
		srv.handleAPIOrphansCleanup(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", w.Code)
		}

		// Nil agent
		srvNil := &Server{}
		req2 := httptest.NewRequest(http.MethodPost, "/api/orphans/cleanup", strings.NewReader(`{}`))
		w2 := httptest.NewRecorder()
		srvNil.handleAPIOrphansCleanup(w2, req2)
		if w2.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", w2.Code)
		}

		// Invalid JSON body
		req3 := httptest.NewRequest(http.MethodPost, "/api/orphans/cleanup", strings.NewReader(`invalid-json`))
		w3 := httptest.NewRecorder()
		srv.handleAPIOrphansCleanup(w3, req3)
		if w3.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", w3.Code)
		}

		// dry_run in request body (snake_case)
		body := `{"dry_run": true}`
		req4 := httptest.NewRequest(http.MethodPost, "/api/orphans/cleanup", strings.NewReader(body))
		w4 := httptest.NewRecorder()
		srv.handleAPIOrphansCleanup(w4, req4)
		if w4.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w4.Code)
		}

		// countScannedDirectories error case (non-existent base path)
		srvErr := &Server{
			basePath: "/does-not-exist-dir-12345",
			agent:    agent,
			client:   fake.NewSimpleClientset(),
		}
		req5 := httptest.NewRequest(http.MethodPost, "/api/orphans/cleanup", strings.NewReader(`{"dryRun":true}`))
		w5 := httptest.NewRecorder()
		srvErr.handleAPIOrphansCleanup(w5, req5)
		if w5.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w5.Code)
		}
		var resp map[string]interface{}
		decodeJSON(t, w5.Body, &resp)
		if resp["scanned"].(float64) != 0 {
			t.Errorf("scanned = %v, want 0", resp["scanned"])
		}
	})
}
