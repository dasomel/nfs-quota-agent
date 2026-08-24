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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dasomel/nfs-quota-agent/internal/audit"
	"github.com/dasomel/nfs-quota-agent/internal/quota"
	"github.com/dasomel/nfs-quota-agent/internal/ui"
)

func TestTrackOrphan(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	a.orphanGracePeriod = time.Hour
	dir := t.TempDir()

	now := time.Now()
	info := a.trackOrphan(dir, "dirname", now)
	if info.CanDelete {
		t.Fatalf("freshly seen orphan should not be deletable yet")
	}
	if _, ok := a.orphanLastSeen[dir]; !ok {
		t.Fatalf("expected orphan to be tracked in orphanLastSeen")
	}

	// Simulate it having been seen well beyond the grace period.
	later := now.Add(2 * time.Hour)
	info2 := a.trackOrphan(dir, "dirname", later)
	if !info2.CanDelete {
		t.Fatalf("orphan past grace period should be deletable")
	}
}

func TestFindOrphans(t *testing.T) {
	pv := newBoundPV("pv-known", "/exports/known-flat", 1)
	client := fake.NewSimpleClientset(pv)
	a := newTestAgent(t, client)
	a.nfsServerPath = "/exports"
	a.orphanGracePeriod = time.Hour

	base := a.nfsBasePath
	for _, dir := range []string{"known-flat", "orphan-flat", filepath.Join("ns1", "orphan-sub"), ".hidden", "projects", "projid"} {
		if err := os.MkdirAll(filepath.Join(base, dir), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	// projects/projid are real files in production, not directories, but the
	// orphan scanner should skip them by name regardless of type.
	if err := os.WriteFile(filepath.Join(base, "regularfile"), []byte("x"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	orphans := a.findOrphans(context.Background())

	var names []string
	for _, o := range orphans {
		names = append(names, o.DirName)
	}
	joined := strings.Join(names, ",")

	if strings.Contains(joined, "known-flat") {
		t.Fatalf("known-flat should not be reported as orphan, got %v", names)
	}
	if !strings.Contains(joined, "orphan-flat") {
		t.Fatalf("expected orphan-flat to be reported as orphan, got %v", names)
	}
	if !strings.Contains(joined, "orphan-sub") {
		t.Fatalf("expected nested orphan-sub to be reported as orphan, got %v", names)
	}
	if strings.Contains(joined, "projects") || strings.Contains(joined, "projid") || strings.Contains(joined, ".hidden") {
		t.Fatalf("reserved/hidden entries must never be reported as orphans, got %v", names)
	}
}

func TestGetOrphansWrapsFindOrphans(t *testing.T) {
	pv := newBoundPV("pv-known", "/exports/known", 1)
	client := fake.NewSimpleClientset(pv)
	a := newTestAgent(t, client)
	a.nfsServerPath = "/exports"

	if err := os.MkdirAll(filepath.Join(a.nfsBasePath, "orphan-x"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	orphans := a.GetOrphans(context.Background())
	if len(orphans) != 1 || orphans[0].DirName != "orphan-x" {
		t.Fatalf("unexpected orphans: %+v", orphans)
	}
}

func TestCleanupOrphansDryRun(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	a.cleanupDryRun = true
	a.orphanGracePeriod = 0 // everything is immediately eligible

	orphanDir := filepath.Join(a.nfsBasePath, "orphan-dry")
	if err := os.MkdirAll(orphanDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	a.cleanupOrphans(context.Background())

	if _, err := os.Stat(orphanDir); err != nil {
		t.Fatalf("dry-run must not remove the orphan directory: %v", err)
	}
}

func TestCleanupOrphansStillInGracePeriod(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	a.cleanupDryRun = false
	a.orphanGracePeriod = time.Hour // fresh orphans won't qualify yet

	orphanDir := filepath.Join(a.nfsBasePath, "orphan-fresh")
	if err := os.MkdirAll(orphanDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	a.cleanupOrphans(context.Background())

	if _, err := os.Stat(orphanDir); err != nil {
		t.Fatalf("orphan still in grace period must not be removed: %v", err)
	}
}

func TestCleanupOrphansRemovesEligible(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	a.cleanupDryRun = false
	a.orphanGracePeriod = 0

	orphanDir := filepath.Join(a.nfsBasePath, "orphan-real")
	if err := os.MkdirAll(orphanDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	auditPath := filepath.Join(t.TempDir(), "audit.log")
	logger, err := audit.NewLogger(audit.Config{Enabled: true, FilePath: auditPath, NodeName: "n", AgentID: "a"})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	a.SetAuditLogger(logger)

	a.cleanupOrphans(context.Background())

	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Fatalf("eligible orphan should have been removed, stat err = %v", err)
	}
	data, err := os.ReadFile(auditPath)
	if err != nil || len(data) == 0 {
		t.Fatalf("expected audit log entry for cleanup, err=%v data=%q", err, data)
	}
}

func TestRemoveOrphan(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	a.fsType = quota.FSTypeXFS

	dir := filepath.Join(a.nfsBasePath, "to-remove")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	a.orphanLastSeen[dir] = time.Now()

	if err := a.RemoveOrphan(ui.OrphanInfo{Path: dir, DirName: "to-remove"}); err != nil {
		t.Fatalf("RemoveOrphan: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("expected directory to be removed")
	}
	if _, ok := a.orphanLastSeen[dir]; ok {
		t.Fatalf("orphanLastSeen entry should have been cleared")
	}
}

func TestRemoveQuotaForPath(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())

	if err := os.WriteFile(a.projectsFile, []byte("5:/data/x\n6:/data/y\n"), 0644); err != nil {
		t.Fatalf("write projects: %v", err)
	}
	if err := os.WriteFile(a.projidFile, []byte("myproj:5\nother:6\n"), 0644); err != nil {
		t.Fatalf("write projid: %v", err)
	}

	a.removeQuotaForPath("/data/x")

	projects, err := os.ReadFile(a.projectsFile)
	if err != nil {
		t.Fatalf("read projects: %v", err)
	}
	if strings.Contains(string(projects), "5:/data/x") {
		t.Fatalf("expected project entry for /data/x to be removed, got %q", projects)
	}
	if !strings.Contains(string(projects), "6:/data/y") {
		t.Fatalf("unrelated project entry should be preserved, got %q", projects)
	}

	projid, err := os.ReadFile(a.projidFile)
	if err != nil {
		t.Fatalf("read projid: %v", err)
	}
	if strings.Contains(string(projid), "myproj:5") {
		t.Fatalf("expected projid entry for myproj to be removed, got %q", projid)
	}
	if !strings.Contains(string(projid), "other:6") {
		t.Fatalf("unrelated projid entry should be preserved, got %q", projid)
	}

	// Unknown path should be a no-op, not a panic.
	if err := a.removeQuotaForPath("/no/such/path"); err != nil {
		t.Fatalf("unknown path should be a no-op, got error: %v", err)
	}
}

// TestRemoveQuotaForPathSurfacesRemoveLineFromFileFailure guards the fix for
// the bug flagged on nfs-quota-agent#10: removeQuotaForPath used to discard
// both of its RemoveLineFromFile errors (`_ = quota.RemoveLineFromFile(...)`),
// so a partial metadata cleanup during orphan removal left a stale
// projects/projid entry with nothing in the logs to explain why -- and, since
// PR #56 added the path->id identity check, that stale entry now makes any
// new PV that later claims the same path get permanently rejected.
func TestRemoveQuotaForPathSurfacesRemoveLineFromFileFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: file permission bits are not enforced, so the injected failure would not occur")
	}
	a := newTestAgent(t, fake.NewSimpleClientset())

	if err := os.WriteFile(a.projectsFile, []byte("5:/data/x\n"), 0644); err != nil {
		t.Fatalf("write projects: %v", err)
	}
	if err := os.WriteFile(a.projidFile, []byte("myproj:5\n"), 0644); err != nil {
		t.Fatalf("write projid: %v", err)
	}

	// RemoveLineFromFile rewrites the file in place; making it read-only
	// makes that rewrite fail while the earlier os.ReadFile lookups (which
	// only need read access) still succeed.
	if err := os.Chmod(a.projectsFile, 0444); err != nil {
		t.Fatalf("chmod projects read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(a.projectsFile, 0644) })

	err := a.removeQuotaForPath("/data/x")
	if err == nil {
		t.Fatalf("expected removeQuotaForPath to surface the RemoveLineFromFile failure, got nil")
	}
	if !strings.Contains(err.Error(), a.projectsFile) {
		t.Fatalf("expected error to name the file that failed (%s), got %q", a.projectsFile, err.Error())
	}

	projects, readErr := os.ReadFile(a.projectsFile)
	if readErr != nil {
		t.Fatalf("read projects: %v", readErr)
	}
	if !strings.Contains(string(projects), "5:/data/x") {
		t.Fatalf("failed rewrite should have left the original entry in place, got %q", projects)
	}
}

func TestRemoveOrphanProceedsWhenMetadataCleanupFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: file permission bits are not enforced, so the injected failure would not occur")
	}
	a := newTestAgent(t, fake.NewSimpleClientset())
	a.fsType = quota.FSTypeXFS

	dir := filepath.Join(a.nfsBasePath, "to-remove")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	a.orphanLastSeen[dir] = time.Now()

	if err := os.WriteFile(a.projectsFile, []byte("5:"+dir+"\n"), 0644); err != nil {
		t.Fatalf("write projects: %v", err)
	}
	if err := os.Chmod(a.projectsFile, 0444); err != nil {
		t.Fatalf("chmod projects read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(a.projectsFile, 0644) })

	// A metadata-cleanup failure must not block directory removal: the
	// directory is real and orphaned regardless of whether its stale
	// projects/projid entry could be cleaned up.
	if err := a.RemoveOrphan(ui.OrphanInfo{Path: dir, DirName: "to-remove"}); err != nil {
		t.Fatalf("RemoveOrphan should still succeed when only metadata cleanup fails: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("expected directory to be removed despite the metadata cleanup failure")
	}
}

func TestRunAutoCleanupTicks(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	a.cleanupDryRun = true
	a.orphanGracePeriod = 0
	a.cleanupInterval = 10 * time.Millisecond

	orphanDir := filepath.Join(a.nfsBasePath, "orphan-tick")
	if err := os.MkdirAll(orphanDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.runAutoCleanup(ctx)
		close(done)
	}()

	// Let at least one ticker fire (dry-run, so nothing destructive happens).
	time.Sleep(60 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("runAutoCleanup did not stop after context cancellation")
	}

	if _, err := os.Stat(orphanDir); err != nil {
		t.Fatalf("dry-run auto-cleanup must not remove the directory: %v", err)
	}
}
