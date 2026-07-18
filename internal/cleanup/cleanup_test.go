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
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// captureStdout redirects os.Stdout for the duration of fn and returns what
// was written. RunCleanup reports its progress via fmt.Printf(os.Stdout, ...)
// rather than returning a Result, so tests need this to assert on behavior.
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
	err := RunCleanup(t.TempDir(), filepath.Join(t.TempDir(), "does-not-exist"), true, false)
	if err == nil {
		t.Fatal("expected error for a nonexistent kubeconfig path")
	}
	if !strings.Contains(err.Error(), "failed to create Kubernetes config") {
		t.Fatalf("error = %v, want it to mention Kubernetes config creation", err)
	}
}

func TestRunCleanup_PVListFailure(t *testing.T) {
	// Port 1 is reserved and nothing listens there, so the PV list call
	// should fail fast (connection refused) instead of hanging.
	kubeconfig := writeKubeconfig(t, "https://127.0.0.1:1")

	err := RunCleanup(t.TempDir(), kubeconfig, true, false)
	if err == nil {
		t.Fatal("expected error when the API server is unreachable")
	}
	if !strings.Contains(err.Error(), "failed to list PVs") {
		t.Fatalf("error = %v, want it to mention listing PVs", err)
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
	out := captureStdout(t, func() {
		if err := RunCleanup(t.TempDir(), kubeconfig, true, false); err != nil {
			t.Fatalf("RunCleanup: %v", err)
		}
	})

	if !strings.Contains(out, "No orphaned quotas found.") {
		t.Fatalf("output missing no-orphans message:\n%s", out)
	}
}

func TestRunCleanup_DryRunReportsOrphan(t *testing.T) {
	srv := fakeAPIServer(t) // no PVs, so any project entry is orphaned
	defer srv.Close()
	kubeconfig := writeKubeconfig(t, srv.URL)

	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "projects"), []byte("100:/base/pvc-orphan\n"), 0o644); err != nil {
		t.Fatalf("write projects file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, "projid"), []byte("pvc-orphan:100\n"), 0o644); err != nil {
		t.Fatalf("write projid file: %v", err)
	}

	out := captureStdout(t, func() {
		if err := RunCleanup(base, kubeconfig, true, false); err != nil {
			t.Fatalf("RunCleanup: %v", err)
		}
	})

	if !strings.Contains(out, "Found 1 orphaned quotas") {
		t.Fatalf("output missing orphan count:\n%s", out)
	}
	if !strings.Contains(out, "Dry-run mode: No changes made.") {
		t.Fatalf("dry-run should not attempt removal:\n%s", out)
	}
}

// The force/non-dry-run removal path is not covered here: it calls into
// quota.RemoveQuotaByID, which shells out to xfs_quota/repquota binaries
// (internal/quota, owned by another lane in this refactor) and isn't safely
// exercisable without real quota tooling or a mocked command runner.
