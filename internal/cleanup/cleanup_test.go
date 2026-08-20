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

package cleanup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dasomel/nfs-quota-agent/internal/quota"
)

// fakeDFRunner stubs quota.CommandRunner just enough for
// quota.DetectFSType's "df -T" call to succeed, so RunCleanup's live/force
// path can be exercised without invoking a real df binary (whose "-T"
// output format differs across platforms) and without any test ever
// shelling out to a real quota binary.
type fakeDFRunner struct{}

func (fakeDFRunner) Run(name string, args ...string) ([]byte, error) {
	if name == "df" {
		return []byte("Filesystem     Type  1K-blocks    Used Available Use% Mounted on\n" +
			"/dev/fake      ext4   10000000 1000000   9000000  10% /data\n"), nil
	}
	return nil, fmt.Errorf("fakeDFRunner: unexpected command %q %v", name, args)
}

// captureStdout redirects os.Stdout for the duration of fn and returns what
// was written. RunCleanup reports its progress via fmt.Printf(os.Stdout, ...)
// in addition to returning a Result, so tests use this to assert on the
// human-readable output alongside the returned counts.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w

	outCh := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		outCh <- buf.String()
	}()

	fn()

	_ = w.Close()
	os.Stdout = old
	return <-outCh
}

// writeKubeconfig writes a minimal kubeconfig pointing at the given server.
func writeKubeconfig(t *testing.T, server string) string {
	t.Helper()
	content := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- cluster:
    server: %s
    insecure-skip-tls-verify: true
  name: test-cluster
contexts:
- context:
    cluster: test-cluster
    user: test-user
  name: test-context
current-context: test-context
users:
- name: test-user
  user: {}
`, server)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

func TestRunCleanup_InvalidKubeconfig(t *testing.T) {
	result, err := RunCleanup(t.TempDir(), "/exports", filepath.Join(t.TempDir(), "does-not-exist"), true, false)
	if err == nil {
		t.Fatal("expected error for a nonexistent kubeconfig path")
	}
	if !strings.Contains(err.Error(), "failed to create Kubernetes config") {
		t.Fatalf("error = %v, want it to mention Kubernetes config creation", err)
	}
	if result != nil {
		t.Fatalf("result = %+v, want nil on error", result)
	}
}

func TestRunCleanup_PVListFailure(t *testing.T) {
	// Port 1 is reserved and nothing listens there, so the PV list call
	// should fail fast (connection refused) instead of hanging.
	kubeconfig := writeKubeconfig(t, "https://127.0.0.1:1")

	result, err := RunCleanup(t.TempDir(), "/exports", kubeconfig, true, false)
	if err == nil {
		t.Fatal("expected error when the API server is unreachable")
	}
	if !strings.Contains(err.Error(), "failed to list PVs") {
		t.Fatalf("error = %v, want it to mention listing PVs", err)
	}
	if result != nil {
		t.Fatalf("result = %+v, want nil on error", result)
	}
}

// fakeAPIServer serves just enough of the Kubernetes API for RunCleanup to
// list PersistentVolumes successfully.
func fakeAPIServer(t *testing.T, pvs ...v1.PersistentVolume) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/persistentvolumes" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		list := v1.PersistentVolumeList{
			TypeMeta: metav1.TypeMeta{Kind: "PersistentVolumeList", APIVersion: "v1"},
			Items:    pvs,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)
	}))
}

func TestRunCleanup_NoOrphans(t *testing.T) {
	srv := fakeAPIServer(t)
	defer srv.Close()
	kubeconfig := writeKubeconfig(t, srv.URL)

	// basePath has no "projects"/"projid" files, so ReadProjectsFile /
	// ReadProjidFile return empty maps and no orphans are found.
	var result *Result
	out := captureStdout(t, func() {
		var err error
		result, err = RunCleanup(t.TempDir(), "/exports", kubeconfig, true, false)
		if err != nil {
			t.Fatalf("RunCleanup: %v", err)
		}
	})

	if !strings.Contains(out, "No orphaned quotas found.") {
		t.Fatalf("output missing no-orphans message:\n%s", out)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.ScannedCount != 0 || result.OrphanedCount != 0 || result.CleanedCount != 0 || len(result.Orphans) != 0 {
		t.Fatalf("result = %+v, want all-zero counts and no orphans", result)
	}
}

func TestRunCleanup_DryRunReportsOrphan(t *testing.T) {
	srv := fakeAPIServer(t) // no PVs, so any project entry is orphaned
	defer srv.Close()
	kubeconfig := writeKubeconfig(t, srv.URL)

	base := t.TempDir()
	// The orphan candidate path must live under base: RunCleanup now refuses
	// to act on any project path that resolves outside the cleanup root (see
	// TestRunCleanup_PathOutsideRootSkipped), which real /etc/projects
	// entries for this tool always do anyway.
	orphanPath := filepath.Join(base, "pvc-orphan")
	if err := os.WriteFile(filepath.Join(base, "projects"), []byte("100:"+orphanPath+"\n"), 0o644); err != nil {
		t.Fatalf("write projects file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, "projid"), []byte("pvc-orphan:100\n"), 0o644); err != nil {
		t.Fatalf("write projid file: %v", err)
	}

	var result *Result
	out := captureStdout(t, func() {
		var err error
		result, err = RunCleanup(base, "/exports", kubeconfig, true, false)
		if err != nil {
			t.Fatalf("RunCleanup: %v", err)
		}
	})

	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.ScannedCount != 1 || result.OrphanedCount != 1 || result.CleanedCount != 0 {
		t.Fatalf("result = %+v, want 1 scanned, 1 orphaned, 0 cleaned", result)
	}

	if !strings.Contains(out, "Found 1 orphaned quotas") {
		t.Fatalf("output missing orphan count:\n%s", out)
	}
	if !strings.Contains(out, "Dry-run mode: No changes made.") {
		t.Fatalf("dry-run should not attempt removal:\n%s", out)
	}
}

// The force/non-dry-run *removal* path (actually calling quota.RemoveQuotaByID)
// is not covered here: it shells out to xfs_quota/repquota binaries
// (internal/quota, owned by another lane in this refactor) and isn't safely
// exercisable without real quota tooling or a mocked command runner.
// TestRunCleanup_PreDeleteRevalidation_SkipsNewlyActivePath below does
// exercise the force/live code path up to (but never reaching) that call,
// because its one candidate turns out to still be active at delete time.

// fakeAPIServerSequence serves a different PersistentVolumeList on each
// successive request, so tests can simulate cluster state changing between
// RunCleanup's initial scan and its pre-delete revalidation. Once the
// sequence is exhausted, the last response is repeated.
func fakeAPIServerSequence(t *testing.T, responses ...[]v1.PersistentVolume) *httptest.Server {
	t.Helper()
	var calls int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/persistentvolumes" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		idx := int(atomic.AddInt32(&calls, 1)) - 1
		if idx >= len(responses) {
			idx = len(responses) - 1
		}
		list := v1.PersistentVolumeList{
			TypeMeta: metav1.TypeMeta{Kind: "PersistentVolumeList", APIVersion: "v1"},
			Items:    responses[idx],
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)
	}))
}

// writeProjectFiles writes minimal "projects" (projectID:path) and "projid"
// (projectName:projectID) files under base, mirroring the on-disk format
// quota.ReadProjectsFile / quota.ReadProjidFile parse.
func writeProjectFiles(t *testing.T, base string, projects map[string]string, projid map[string]string) {
	t.Helper()
	var pb, ib strings.Builder
	for id, path := range projects {
		pb.WriteString(id + ":" + path + "\n")
	}
	for name, id := range projid {
		ib.WriteString(name + ":" + id + "\n")
	}
	if err := os.WriteFile(filepath.Join(base, "projects"), []byte(pb.String()), 0o644); err != nil {
		t.Fatalf("write projects file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, "projid"), []byte(ib.String()), 0o644); err != nil {
		t.Fatalf("write projid file: %v", err)
	}
}

func TestRunCleanup_CSIActivePVNotOrphaned(t *testing.T) {
	base := t.TempDir()
	localPath := filepath.Join(base, "pvc-csi")

	pv := v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-csi"},
		Spec: v1.PersistentVolumeSpec{PersistentVolumeSource: v1.PersistentVolumeSource{
			CSI: &v1.CSIPersistentVolumeSource{
				Driver:           "nfs.csi.k8s.io",
				VolumeAttributes: map[string]string{"share": "/exports", "subDir": "pvc-csi"},
			},
		}},
	}
	srv := fakeAPIServer(t, pv)
	defer srv.Close()
	kubeconfig := writeKubeconfig(t, srv.URL)

	writeProjectFiles(t, base, map[string]string{"100": localPath}, map[string]string{"pvc-csi": "100"})

	var result *Result
	_ = captureStdout(t, func() {
		var err error
		result, err = RunCleanup(base, "/exports", kubeconfig, true, false)
		if err != nil {
			t.Fatalf("RunCleanup: %v", err)
		}
	})

	if result.ScannedCount != 1 || result.OrphanedCount != 0 || result.ActiveCount != 1 {
		t.Fatalf("result = %+v, want 1 scanned, 0 orphaned (CSI share+subDir PV must be recognized as active)", result)
	}
}

func TestRunCleanup_CSIShareOnlyActivePVNotOrphaned(t *testing.T) {
	base := t.TempDir()
	localPath := filepath.Join(base, "pv-share-only")

	pv := v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-share-only"},
		Spec: v1.PersistentVolumeSpec{PersistentVolumeSource: v1.PersistentVolumeSource{
			CSI: &v1.CSIPersistentVolumeSource{
				Driver:           "nfs.csi.k8s.io",
				VolumeAttributes: map[string]string{"share": "/exports"},
			},
		}},
	}
	srv := fakeAPIServer(t, pv)
	defer srv.Close()
	kubeconfig := writeKubeconfig(t, srv.URL)

	writeProjectFiles(t, base, map[string]string{"200": localPath}, map[string]string{"pv-share-only": "200"})

	var result *Result
	_ = captureStdout(t, func() {
		var err error
		result, err = RunCleanup(base, "/exports", kubeconfig, true, false)
		if err != nil {
			t.Fatalf("RunCleanup: %v", err)
		}
	})

	if result.OrphanedCount != 0 || result.ActiveCount != 1 {
		t.Fatalf("result = %+v, want 0 orphaned (CSI share-only falls back to share+PV-name, must still match)", result)
	}
}

func TestRunCleanup_NativeNFSActivePVNotOrphaned(t *testing.T) {
	base := t.TempDir()
	localPath := filepath.Join(base, "pvc-native")

	pv := v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-native"},
		Spec: v1.PersistentVolumeSpec{PersistentVolumeSource: v1.PersistentVolumeSource{
			NFS: &v1.NFSVolumeSource{Path: "/exports/pvc-native"},
		}},
	}
	srv := fakeAPIServer(t, pv)
	defer srv.Close()
	kubeconfig := writeKubeconfig(t, srv.URL)

	writeProjectFiles(t, base, map[string]string{"300": localPath}, map[string]string{"pvc-native": "300"})

	var result *Result
	_ = captureStdout(t, func() {
		var err error
		result, err = RunCleanup(base, "/exports", kubeconfig, true, false)
		if err != nil {
			t.Fatalf("RunCleanup: %v", err)
		}
	})

	if result.OrphanedCount != 0 || result.ActiveCount != 1 {
		t.Fatalf("result = %+v, want 0 orphaned (native NFS PV must be recognized as active)", result)
	}
}

// TestRunCleanup_SameBasenameFullPathIdentity is the regression case for the
// original bug: filepath.Base comparison would treat two different projects
// that merely share a basename as the same path, hiding a genuine orphan
// (or, as here, wrongly clearing it) behind an unrelated active PV.
func TestRunCleanup_SameBasenameFullPathIdentity(t *testing.T) {
	base := t.TempDir()
	activePath := filepath.Join(base, "team-a", "data")
	orphanPath := filepath.Join(base, "team-b", "data") // same basename "data" as activePath

	pv := v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-team-a"},
		Spec: v1.PersistentVolumeSpec{PersistentVolumeSource: v1.PersistentVolumeSource{
			NFS: &v1.NFSVolumeSource{Path: "/exports/team-a/data"},
		}},
	}
	srv := fakeAPIServer(t, pv)
	defer srv.Close()
	kubeconfig := writeKubeconfig(t, srv.URL)

	writeProjectFiles(t, base,
		map[string]string{"100": activePath, "200": orphanPath},
		map[string]string{"team-a-data": "100", "team-b-data": "200"},
	)

	var result *Result
	_ = captureStdout(t, func() {
		var err error
		result, err = RunCleanup(base, "/exports", kubeconfig, true, false)
		if err != nil {
			t.Fatalf("RunCleanup: %v", err)
		}
	})

	if result.ScannedCount != 2 {
		t.Fatalf("result = %+v, want 2 scanned", result)
	}
	if result.OrphanedCount != 1 || len(result.Orphans) != 1 {
		t.Fatalf("result = %+v, want exactly 1 orphan (the basename-matching active path must not hide it)", result)
	}
	if result.Orphans[0].Path != orphanPath {
		t.Fatalf("orphan path = %q, want %q (the genuinely orphaned one, not the active team-a path)", result.Orphans[0].Path, orphanPath)
	}
}

func TestRunCleanup_PathOutsideRootSkipped(t *testing.T) {
	base := t.TempDir()
	outsidePath := filepath.Join(t.TempDir(), "not-under-base", "pvc-x")

	srv := fakeAPIServer(t) // no PVs
	defer srv.Close()
	kubeconfig := writeKubeconfig(t, srv.URL)

	writeProjectFiles(t, base, map[string]string{"400": outsidePath}, map[string]string{"pvc-x": "400"})

	var result *Result
	out := captureStdout(t, func() {
		var err error
		result, err = RunCleanup(base, "/exports", kubeconfig, true, false)
		if err != nil {
			t.Fatalf("RunCleanup: %v", err)
		}
	})

	if result.OrphanedCount != 0 || len(result.Orphans) != 0 {
		t.Fatalf("result = %+v, want the outside-root path refused rather than classified orphaned", result)
	}
	if result.SkippedAmbiguousCount != 1 || len(result.Skipped) != 1 {
		t.Fatalf("result = %+v, want 1 skipped entry", result)
	}
	if result.Skipped[0].Path != outsidePath {
		t.Fatalf("skipped path = %q, want %q", result.Skipped[0].Path, outsidePath)
	}
	if !strings.Contains(out, "outside the cleanup root") {
		t.Fatalf("output missing outside-root skip reason:\n%s", out)
	}
}

// TestRunCleanup_PreDeleteRevalidation_SkipsNewlyActivePath covers the P0
// race: a project looked orphaned during the scan (no matching PV yet), but
// by the time the force/live removal loop runs, the PV now exists and is
// bound to that same path. RunCleanup must re-list PVs immediately before
// removal and refuse to delete it.
func TestRunCleanup_PreDeleteRevalidation_SkipsNewlyActivePath(t *testing.T) {
	base := t.TempDir()
	localPath := filepath.Join(base, "pvc-race")

	becomesActivePV := v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-race"},
		Spec: v1.PersistentVolumeSpec{PersistentVolumeSource: v1.PersistentVolumeSource{
			NFS: &v1.NFSVolumeSource{Path: "/exports/pvc-race"},
		}},
	}

	srv := fakeAPIServerSequence(t,
		[]v1.PersistentVolume{},                // initial scan: nothing active, candidate looks orphaned
		[]v1.PersistentVolume{becomesActivePV}, // pre-delete revalidation: now active
	)
	defer srv.Close()
	kubeconfig := writeKubeconfig(t, srv.URL)

	writeProjectFiles(t, base, map[string]string{"500": localPath}, map[string]string{"pvc-race": "500"})

	restore := quota.SetCommandRunnerForTesting(fakeDFRunner{})
	defer restore()

	var result *Result
	out := captureStdout(t, func() {
		var err error
		// force=true so the run doesn't block on the interactive confirmation prompt.
		result, err = RunCleanup(base, "/exports", kubeconfig, false, true)
		if err != nil {
			t.Fatalf("RunCleanup: %v", err)
		}
	})

	if result.OrphanedCount != 1 {
		t.Fatalf("result = %+v, want 1 orphan classified at scan time", result)
	}
	if result.CleanedCount != 0 {
		t.Fatalf("result = %+v, want 0 cleaned: the path became active before deletion", result)
	}
	if result.SkippedAmbiguousCount != 1 {
		t.Fatalf("result = %+v, want the newly-active path skipped instead of removed", result)
	}
	if !strings.Contains(out, "became active") {
		t.Fatalf("output missing pre-delete revalidation skip reason:\n%s", out)
	}
}
