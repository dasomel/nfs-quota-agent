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

package metrics

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeAgent implements AgentInfo for tests.
type fakeAgent struct {
	basePath     string
	appliedCount int
}

func (f *fakeAgent) BasePath() string       { return f.basePath }
func (f *fakeAgent) AppliedQuotaCount() int { return f.appliedCount }

func TestHandleMetrics_Basic(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "pvc-1"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	c := &Collector{agent: &fakeAgent{basePath: dir, appliedCount: 5}, version: "v1.2.3"}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	c.handleMetrics(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q, want text/plain prefix", ct)
	}

	body := w.Body.String()
	for _, want := range []string{
		`nfs_quota_agent_info{version="v1.2.3"} 1`,
		"nfs_disk_total_bytes",
		"nfs_disk_used_bytes",
		"nfs_disk_available_bytes",
		"nfs_disk_used_percent",
		`nfs_quota_used_bytes{directory="pvc-1"}`,
		"nfs_quota_directories_total 1",
		"nfs_quota_applied_total 5",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\nfull body:\n%s", want, body)
		}
	}
}

func TestHandleMetrics_NoDirectories(t *testing.T) {
	dir := t.TempDir()
	c := &Collector{agent: &fakeAgent{basePath: dir, appliedCount: 0}}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	c.handleMetrics(w, req)

	body := w.Body.String()
	if strings.Contains(body, "nfs_quota_used_bytes") {
		t.Errorf("expected no per-directory metrics for an empty base path, got:\n%s", body)
	}
	if !strings.Contains(body, "nfs_quota_applied_total 0") {
		t.Errorf("expected applied total 0, got:\n%s", body)
	}
}

func TestHandleMetrics_DiskUsageError(t *testing.T) {
	c := &Collector{agent: &fakeAgent{basePath: "/nonexistent/path/for/nfs-quota-agent-test", appliedCount: 2}}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	c.handleMetrics(w, req)

	body := w.Body.String()
	if strings.Contains(body, "nfs_disk_total_bytes") {
		t.Errorf("expected no disk metrics for an invalid path, got:\n%s", body)
	}
	if !strings.Contains(body, "nfs_quota_applied_total 2") {
		t.Errorf("expected applied total metric to still be present, got:\n%s", body)
	}
}

func TestHandleMetrics_CachesWithinInterval(t *testing.T) {
	dir := t.TempDir()
	c := &Collector{agent: &fakeAgent{basePath: dir, appliedCount: 1}}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w1 := httptest.NewRecorder()
	c.handleMetrics(w1, req)

	// Stamp a sentinel body with a recent lastUpdate: a call within the
	// 30s freshness window must return it unchanged instead of recomputing.
	c.mu.Lock()
	c.metrics = "sentinel-content"
	c.lastUpdate = time.Now()
	c.mu.Unlock()

	w2 := httptest.NewRecorder()
	c.handleMetrics(w2, req)
	if w2.Body.String() != "sentinel-content" {
		t.Fatalf("expected cached sentinel content, got %q", w2.Body.String())
	}
}

func TestHandleMetrics_RecomputesWhenStale(t *testing.T) {
	dir := t.TempDir()
	c := &Collector{agent: &fakeAgent{basePath: dir, appliedCount: 9}}

	c.mu.Lock()
	c.metrics = "stale-content"
	c.lastUpdate = time.Now().Add(-time.Minute)
	c.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	c.handleMetrics(w, req)

	if w.Body.String() == "stale-content" {
		t.Fatal("expected stale metrics to be recomputed")
	}
	if !strings.Contains(w.Body.String(), "nfs_quota_applied_total 9") {
		t.Fatalf("recomputed metrics missing applied total: %s", w.Body.String())
	}
}

func TestHandleHealth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != "ok" {
		t.Fatalf("body = %q, want ok", w.Body.String())
	}
}

func TestHandleReady(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	w := httptest.NewRecorder()
	handleReady(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != "ok" {
		t.Fatalf("body = %q, want ok", w.Body.String())
	}
}
