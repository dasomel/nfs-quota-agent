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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dasomel/nfs-quota-agent/internal/quota"
)

// fakeReportRunner is a minimal quota.CommandRunner stand-in for exercising
// GetReportedUsage's per-fsType dispatch without invoking real xfs_quota/
// repquota/btrfs binaries. run answers every call; a nil run always returns
// empty output with no error.
type fakeReportRunner struct {
	run func(name string, args ...string) ([]byte, error)
}

func (f fakeReportRunner) Run(name string, args ...string) ([]byte, error) {
	if f.run == nil {
		return []byte(""), nil
	}
	return f.run(name, args...)
}

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

// TestGetReportedUsage_PropagatesReportFailure guards #90(b): unlike
// GetDirUsages, which swallows a report command failure into an empty map
// and falls back to a filepath.Walk apparent-size scan, GetReportedUsage
// must return the error to the caller so a caller needing to fail closed
// (ensureQuota's shrink guard) can actually tell "report failed" apart from
// "usage is zero."
func TestGetReportedUsage_PropagatesReportFailure(t *testing.T) {
	restore := quota.SetCommandRunnerForTesting(fakeReportRunner{run: func(name string, args ...string) ([]byte, error) {
		return nil, errors.New("simulated xfs_quota failure")
	}})
	defer restore()

	base := t.TempDir()
	_, err := GetReportedUsage(base, "xfs", filepath.Join(base, "projects"), filepath.Join(base, "projid"))
	if err == nil {
		t.Fatal("expected GetReportedUsage to propagate the report command's error")
	}
}

// TestGetReportedUsage_XFS_NoApparentSizeFallback guards the other half of
// #90(b): a path the report has no entry for must be absent from the
// returned map entirely -- no filepath.Walk substitute the way GetDirUsages
// provides one -- even though the path exists on disk with real data.
func TestGetReportedUsage_XFS_NoApparentSizeFallback(t *testing.T) {
	base := t.TempDir()
	projectsFile := filepath.Join(base, "projects")
	projidFile := filepath.Join(base, "projid")
	if err := os.WriteFile(projidFile, []byte("proj1:1\n"), 0o644); err != nil {
		t.Fatalf("write projid: %v", err)
	}
	reportedPath := filepath.Join(base, "pvc-reported")
	unreportedPath := filepath.Join(base, "pvc-unreported")
	if err := os.WriteFile(projectsFile, []byte("1:"+reportedPath+"\n"), 0o644); err != nil {
		t.Fatalf("write projects: %v", err)
	}
	// Real data on disk for the path the report never mentions -- a
	// GetDirUsages caller would see this via the apparent-size fallback;
	// GetReportedUsage must not.
	writeSized(t, filepath.Join(unreportedPath, "f"), 12345)

	restore := quota.SetCommandRunnerForTesting(fakeReportRunner{run: func(name string, args ...string) ([]byte, error) {
		return []byte("Project ID   Used   Soft   Hard   Warn/Grace\n" +
			"#proj1        500      0      1000    00 [------]\n"), nil
	}})
	defer restore()

	usageMap, err := GetReportedUsage(base, "xfs", projectsFile, projidFile)
	if err != nil {
		t.Fatalf("GetReportedUsage: %v", err)
	}
	if got, ok := usageMap[reportedPath]; !ok || got != 500*1024 {
		t.Errorf("usageMap[%s] = (%d, %v), want (512000, true)", reportedPath, got, ok)
	}
	if _, ok := usageMap[unreportedPath]; ok {
		t.Errorf("usageMap should have no entry for a path the report never mentioned, got one")
	}
}

func TestGetReportedUsage_UnsupportedFSType(t *testing.T) {
	if _, err := GetReportedUsage(t.TempDir(), "zfs", "/etc/projects", "/etc/projid"); err == nil {
		t.Fatal("expected an error for an unsupported filesystem type")
	}
}

// TestGetDirAllocatedSize_SparseFileBelowApparentSize guards #94: a sparse
// file (truncated to a size with no data ever written) allocates far fewer
// disk blocks than its apparent size claims. Asserting the relation
// (allocated < apparent), not a byte count, since the exact block size
// backing t.TempDir() varies by filesystem (APFS locally, ext4/xfs/tmpfs in
// CI). Deliberate breakage: summing info.Size() instead of st.Blocks*512
// makes allocated == apparent here, failing this assertion.
func TestGetDirAllocatedSize_SparseFileBelowApparentSize(t *testing.T) {
	base := t.TempDir()
	f, err := os.Create(filepath.Join(base, "sparse.bin"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.Truncate(1 << 30); err != nil { // 1 GiB, no bytes ever written
		f.Close()
		t.Fatalf("truncate: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	apparent := GetDirSize(base)
	allocated := GetDirAllocatedSize(base)
	if allocated >= apparent {
		t.Fatalf("GetDirAllocatedSize = %d, want < apparent size %d for a sparse file", allocated, apparent)
	}
}

// TestGetDirAllocatedSize_ManySmallFilesAtOrAboveApparentSize guards #94's
// other direction: per-file block rounding means many tiny files allocate
// at least as many bytes as their apparent size sums to (a 1-byte file
// still consumes a whole block), never fewer.
func TestGetDirAllocatedSize_ManySmallFilesAtOrAboveApparentSize(t *testing.T) {
	base := t.TempDir()
	for i := 0; i < 100; i++ {
		writeSized(t, filepath.Join(base, fmt.Sprintf("f%d", i)), 1)
	}

	apparent := GetDirSize(base)
	allocated := GetDirAllocatedSize(base)
	if allocated < apparent {
		t.Fatalf("GetDirAllocatedSize = %d, want >= apparent size %d for 100 one-byte files (block rounding)", allocated, apparent)
	}
}

// TestGetDirAllocatedSize_HardlinkPairCountedOnce guards the (Dev, Ino)
// dedupe: a directory holding a file plus a hardlink to it must allocate
// the same total as a directory holding just one copy of that file --
// summing st.Blocks per directory entry without dedup would double-count
// the hardlinked file's shared blocks.
func TestGetDirAllocatedSize_HardlinkPairCountedOnce(t *testing.T) {
	linkedBase := t.TempDir()
	original := filepath.Join(linkedBase, "original")
	writeSized(t, original, 100_000)
	if err := os.Link(original, filepath.Join(linkedBase, "hardlink")); err != nil {
		t.Skipf("hardlinks not supported on this filesystem: %v", err)
	}

	soloBase := t.TempDir()
	writeSized(t, filepath.Join(soloBase, "solo"), 100_000)

	linkedAllocated := GetDirAllocatedSize(linkedBase)
	soloAllocated := GetDirAllocatedSize(soloBase)
	if linkedAllocated != soloAllocated {
		t.Fatalf("GetDirAllocatedSize(dir with hardlink pair) = %d, want equal to single-file allocation %d (hardlink must count once)", linkedAllocated, soloAllocated)
	}
}
