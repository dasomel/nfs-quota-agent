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

package status

import (
	"os"
	"path/filepath"
	"testing"
)

// writeSized creates the parent directories of path (if needed) and writes a
// file of the given size at path.
func writeSized(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestGetDirSize(t *testing.T) {
	base := t.TempDir()
	writeSized(t, filepath.Join(base, "a.txt"), 100)
	writeSized(t, filepath.Join(base, "sub", "b.txt"), 250)

	if got := GetDirSize(base); got != 350 {
		t.Fatalf("GetDirSize = %d, want 350", got)
	}
}

func TestGetDirSize_NonexistentPath(t *testing.T) {
	if got := GetDirSize(filepath.Join(t.TempDir(), "missing")); got != 0 {
		t.Fatalf("GetDirSize(missing) = %d, want 0", got)
	}
}

// Unknown fsType ("" here) makes GetDirUsages skip both the XFS and ext4
// quota-report lookups entirely, so these tests exercise the pure directory
// scanning logic without needing real xfs_quota/repquota tooling or a real
// XFS/ext4 mount.
func TestGetDirUsages_FlatDirectories(t *testing.T) {
	base := t.TempDir()
	writeSized(t, filepath.Join(base, "pvc-a", "f"), 100)
	writeSized(t, filepath.Join(base, "pvc-b", "f"), 50)

	usages, err := GetDirUsages(base, "", "/etc/projects", "/etc/projid")
	if err != nil {
		t.Fatalf("GetDirUsages: %v", err)
	}
	if len(usages) != 2 {
		t.Fatalf("len(usages) = %d, want 2: %#v", len(usages), usages)
	}

	byPath := map[string]DirUsage{}
	for _, u := range usages {
		byPath[u.Path] = u
	}
	if u, ok := byPath[filepath.Join(base, "pvc-a")]; !ok || u.Used != 100 {
		t.Errorf("pvc-a usage = %#v, want Used=100", u)
	}
	if u, ok := byPath[filepath.Join(base, "pvc-b")]; !ok || u.Used != 50 {
		t.Errorf("pvc-b usage = %#v, want Used=50", u)
	}
}

func TestGetDirUsages_NestedNamespaceDirectories(t *testing.T) {
	base := t.TempDir()
	writeSized(t, filepath.Join(base, "team-a", "pvc-1", "f"), 200)
	writeSized(t, filepath.Join(base, "team-a", "pvc-2", "f"), 300)

	usages, err := GetDirUsages(base, "", "/etc/projects", "/etc/projid")
	if err != nil {
		t.Fatalf("GetDirUsages: %v", err)
	}
	// A namespace directory containing subdirectories expands into nested
	// entries rather than being reported as a single flat entry itself.
	if len(usages) != 2 {
		t.Fatalf("len(usages) = %d, want 2: %#v", len(usages), usages)
	}
	for _, u := range usages {
		if filepath.Base(filepath.Dir(u.Path)) != "team-a" {
			t.Errorf("unexpected path %q, want nested under team-a", u.Path)
		}
	}
}

func TestGetDirUsages_SkipsHiddenAndProjectFiles(t *testing.T) {
	base := t.TempDir()
	writeSized(t, filepath.Join(base, ".hidden", "f"), 10)
	if err := os.MkdirAll(filepath.Join(base, "projects"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(base, "projid"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeSized(t, filepath.Join(base, "pvc-a", "f"), 5)

	usages, err := GetDirUsages(base, "", "/etc/projects", "/etc/projid")
	if err != nil {
		t.Fatalf("GetDirUsages: %v", err)
	}
	if len(usages) != 1 || filepath.Base(usages[0].Path) != "pvc-a" {
		t.Fatalf("expected only pvc-a to be scanned, got %#v", usages)
	}
}

func TestGetDirUsages_NonexistentBasePath(t *testing.T) {
	_, err := GetDirUsages(filepath.Join(t.TempDir(), "missing"), "", "/etc/projects", "/etc/projid")
	if err == nil {
		t.Fatal("expected error for nonexistent base path")
	}
}
