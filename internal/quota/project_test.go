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

package quota

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddProject(t *testing.T) {
	dir := t.TempDir()
	projectsFile := filepath.Join(dir, "projects")
	projidFile := filepath.Join(dir, "projid")

	if err := AddProject("/data/proj1", "proj1", 100, projectsFile, projidFile); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !fileContains(t, projidFile, "proj1:100") {
		t.Errorf("projid file missing entry")
	}
	if !fileContains(t, projectsFile, "100:/data/proj1") {
		t.Errorf("projects file missing entry")
	}

	// Calling again with the same project should be idempotent (no duplicate).
	if err := AddProject("/data/proj1", "proj1", 100, projectsFile, projidFile); err != nil {
		t.Fatalf("unexpected error on second call: %v", err)
	}
	data, err := os.ReadFile(projidFile)
	if err != nil {
		t.Fatalf("failed to read projid file: %v", err)
	}
	if strings.Count(string(data), "proj1:100") != 1 {
		t.Errorf("expected exactly one entry, got content: %q", string(data))
	}
}

// TestAddProject_NameRemappedToDifferentID reproduces the reported defect:
// projid already maps "pvc_a" -> 3 and projects already maps 3 -> /export/pvc-a.
// Calling AddProject for the same name with a *different* ID (as happens
// when generateProjectID hands out the base hash to a second project after
// the first, bumped one, is removed) must be rejected as a unit, leaving
// both files exactly as they were — not silently drop the projid write
// while still writing projects, which is what let two PVs end up sharing
// project ID 3 with only one of them named in projid.
func TestAddProject_NameRemappedToDifferentID(t *testing.T) {
	dir := t.TempDir()
	projectsFile := filepath.Join(dir, "projects")
	projidFile := filepath.Join(dir, "projid")

	if err := os.WriteFile(projidFile, []byte("pvc_a:3\n"), 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if err := os.WriteFile(projectsFile, []byte("3:/export/pvc-a\n"), 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	err := AddProject("/export/pvc-a-new", "pvc_a", 5, projectsFile, projidFile)
	if err == nil {
		t.Fatal("expected an error when a name is remapped to a different id")
	}
	if !errors.Is(err, ErrProjectConflict) {
		t.Errorf("expected ErrProjectConflict, got: %v", err)
	}

	projidData, _ := os.ReadFile(projidFile)
	if string(projidData) != "pvc_a:3\n" {
		t.Errorf("projid file must be left unmodified, got %q", string(projidData))
	}
	projectsData, _ := os.ReadFile(projectsFile)
	if string(projectsData) != "3:/export/pvc-a\n" {
		t.Errorf("projects file must be left unmodified (no orphaned id 5 entry), got %q", string(projectsData))
	}
}

// TestAddProject_IDRemappedToDifferentPath covers the third leg of the
// identity triple: the ID already resolves to a different path than the
// one requested. This can only be caught by validating against both files
// before writing (AppendToFile's own per-file check on the projid write
// alone would not see it, since projid does not carry path information).
func TestAddProject_IDRemappedToDifferentPath(t *testing.T) {
	dir := t.TempDir()
	projectsFile := filepath.Join(dir, "projects")
	projidFile := filepath.Join(dir, "projid")

	if err := AddProject("/export/pvc-a", "pvc_a", 3, projectsFile, projidFile); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	err := AddProject("/export/pvc-b", "pvc_b", 3, projectsFile, projidFile)
	if err == nil {
		t.Fatal("expected an error when an id is remapped to a different path")
	}
	if !errors.Is(err, ErrProjectConflict) {
		t.Errorf("expected ErrProjectConflict, got: %v", err)
	}

	projidData, _ := os.ReadFile(projidFile)
	if strings.Contains(string(projidData), "pvc_b") {
		t.Errorf("projid file must not have been written for the rejected conflict, got %q", string(projidData))
	}
}

// TestAddProject_PathRemappedToDifferentID covers the fourth direction the
// other three checks don't: the same path claimed under a second, different
// id. This is #15's own "path -> ID 방향 검증" gap, reachable without any
// file corruption -- an nfs.io/project-name annotation rename on a PV keeps
// the same NFS path but generates a new name, and therefore (via
// generateProjectID) a new id, for the exact same directory. A directory
// can only carry one XFS/ext4 project id at a time, so without this check
// /etc/projects would end up listing the same path under two ids, one of
// which goes stale as soon as either quota is actually applied.
func TestAddProject_PathRemappedToDifferentID(t *testing.T) {
	dir := t.TempDir()
	projectsFile := filepath.Join(dir, "projects")
	projidFile := filepath.Join(dir, "projid")

	if err := AddProject("/export/pvc-a", "pvc_a", 3, projectsFile, projidFile); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	// Same path, different name+id -- simulates an annotation rename.
	err := AddProject("/export/pvc-a", "pvc_a_renamed", 7, projectsFile, projidFile)
	if err == nil {
		t.Fatal("expected an error when a path is remapped to a different id")
	}
	if !errors.Is(err, ErrProjectConflict) {
		t.Errorf("expected ErrProjectConflict, got: %v", err)
	}

	projectsData, _ := os.ReadFile(projectsFile)
	if string(projectsData) != "3:/export/pvc-a\n" {
		t.Errorf("projects file must be left unmodified (no second id claiming the same path), got %q", string(projectsData))
	}
	projidData, _ := os.ReadFile(projidFile)
	if strings.Contains(string(projidData), "pvc_a_renamed") {
		t.Errorf("projid file must not have been written for the rejected conflict, got %q", string(projidData))
	}
}

// TestAddProject_SameIDSamePathIsIdempotent guards against the new path->id
// check in TestAddProject_PathRemappedToDifferentID above rejecting the
// ordinary case it must not touch: re-adding the exact same (path, name,
// id) tuple -- e.g. a resync re-processing a PV whose quota is already
// applied -- has to stay a no-op, not start erroring because the path is
// "already mapped" to the very id being requested.
func TestAddProject_SameIDSamePathIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	projectsFile := filepath.Join(dir, "projects")
	projidFile := filepath.Join(dir, "projid")

	if err := AddProject("/export/pvc-a", "pvc_a", 3, projectsFile, projidFile); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if err := AddProject("/export/pvc-a", "pvc_a", 3, projectsFile, projidFile); err != nil {
		t.Fatalf("expected the identical re-add to be a no-op, got: %v", err)
	}

	projectsData, _ := os.ReadFile(projectsFile)
	if string(projectsData) != "3:/export/pvc-a\n" {
		t.Errorf("projects file should be unchanged after an idempotent re-add, got %q", string(projectsData))
	}
}

// Colon/newline-in-name rejection at the AddProject level (neither file
// touched) is covered by TestAddProjectRejectsDelimiterInName, and the
// ordinary-name-still-works case by TestAddProjectAcceptsOrdinaryName, both
// in projectname_test.go.

func TestAddProject_WriteFailure(t *testing.T) {
	// Using a directory path as the projid "file" forces a write failure.
	badDir := t.TempDir()
	err := AddProject("/data/proj1", "proj1", 100, filepath.Join(badDir, "projects"), badDir)
	if err == nil {
		t.Fatal("expected error when projidFile is a directory")
	}
}

func TestAppendToFile(t *testing.T) {
	t.Run("creates file when missing", func(t *testing.T) {
		dir := t.TempDir()
		f := filepath.Join(dir, "newfile")
		if err := AppendToFile(f, "100:/data\n", "100"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !fileContains(t, f, "100:/data") {
			t.Errorf("expected entry to be written")
		}
	})

	t.Run("skips identical duplicate entries", func(t *testing.T) {
		dir := t.TempDir()
		f := filepath.Join(dir, "file")
		if err := os.WriteFile(f, []byte("100:/data\n"), 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}
		if err := AppendToFile(f, "100:/data\n", "100"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		data, _ := os.ReadFile(f)
		if strings.Count(string(data), "100:/data") != 1 {
			t.Errorf("expected exactly one entry, got %q", string(data))
		}
	})

	t.Run("rejects a conflicting value for an existing key", func(t *testing.T) {
		// This is the bug this behavior replaces: silently discarding a
		// value that disagrees with what's already on disk under the same
		// key (e.g. the same project ID reassigned to a different path)
		// used to look like success. It must now be a reported conflict,
		// and the file must be left untouched.
		dir := t.TempDir()
		f := filepath.Join(dir, "file")
		if err := os.WriteFile(f, []byte("100:/data\n"), 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}
		err := AppendToFile(f, "100:/other\n", "100")
		if err == nil {
			t.Fatal("expected an error for a conflicting value")
		}
		if !errors.Is(err, ErrProjectConflict) {
			t.Errorf("expected ErrProjectConflict, got: %v", err)
		}
		data, _ := os.ReadFile(f)
		if strings.Contains(string(data), "/other") {
			t.Errorf("expected file to be left unchanged, got %q", string(data))
		}
		if !strings.Contains(string(data), "100:/data") {
			t.Errorf("expected original entry to remain, got %q", string(data))
		}
	})

	t.Run("appends when no matching prefix", func(t *testing.T) {
		dir := t.TempDir()
		f := filepath.Join(dir, "file")
		if err := os.WriteFile(f, []byte("100:/data\n"), 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}
		if err := AppendToFile(f, "200:/other\n", "200"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !fileContains(t, f, "200:/other") {
			t.Errorf("expected new entry to be appended")
		}
	})

	t.Run("bare searchKey line with no value is a conflict, not a silent skip", func(t *testing.T) {
		// A bare "marker" line with no ":value" is what a write truncated
		// mid-flush looks like (cut off right after the key). Treating it
		// as "already exists" would silently accept corrupt data instead
		// of surfacing it — the same class of bug this change fixes for
		// same-key-different-value entries.
		dir := t.TempDir()
		f := filepath.Join(dir, "file")
		if err := os.WriteFile(f, []byte("marker\n"), 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}
		err := AppendToFile(f, "marker:extra\n", "marker")
		if err == nil {
			t.Fatal("expected an error for a bare marker line with no value")
		}
		if !errors.Is(err, ErrProjectConflict) {
			t.Errorf("expected ErrProjectConflict, got: %v", err)
		}
		data, _ := os.ReadFile(f)
		if strings.Contains(string(data), "extra") {
			t.Errorf("expected file to be left unchanged, got %q", string(data))
		}
	})

	t.Run("propagates read error for non-not-exist errors", func(t *testing.T) {
		// A directory path causes os.ReadFile to fail with a non-NotExist error.
		dir := t.TempDir()
		if err := AppendToFile(dir, "entry\n", "key"); err == nil {
			t.Fatal("expected error when filename is a directory")
		}
	})
}

func TestRemoveLineFromFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "file")
	content := "100:/data\n200:/other\n100:/dup\n"
	if err := os.WriteFile(f, []byte(content), 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	if err := RemoveLineFromFile(f, "100:", t.TempDir()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(f)
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}
	if strings.Contains(string(data), "100:") {
		t.Errorf("expected all 100: lines removed, got %q", string(data))
	}
	if !strings.Contains(string(data), "200:/other") {
		t.Errorf("expected unrelated line to remain, got %q", string(data))
	}
}

func TestRemoveLineFromFile_MissingFile(t *testing.T) {
	dir := t.TempDir()
	if err := RemoveLineFromFile(filepath.Join(dir, "missing"), "100:", t.TempDir()); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestRemoveLineFromFile_LeavesRecoverableBackupInStateDir(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(t.TempDir(), "state") // does not exist yet: MkdirAll must create it
	f := filepath.Join(dir, "file")
	original := "100:/data\n200:/other\n"
	if err := os.WriteFile(f, []byte(original), 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	if err := RemoveLineFromFile(f, "100:", stateDir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	backupPath := filepath.Join(stateDir, "file.bak")
	backup, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("expected backup sidecar to exist under stateDir: %v", err)
	}
	if string(backup) != original {
		t.Errorf("expected backup to hold pre-rewrite contents %q, got %q", original, string(backup))
	}

	// Confirm it did NOT land next to the target (the bug this replaced:
	// the target's own directory can be a bind-mounted individual file's
	// mount, which does not survive a container restart).
	if _, err := os.Stat(f + ".bak"); !os.IsNotExist(err) {
		t.Errorf("backup should not be written next to the target file, got err=%v", err)
	}
}

func TestRemoveLineFromFile_BackupDoesNotAccumulate(t *testing.T) {
	dir := t.TempDir()
	stateDir := t.TempDir()
	f := filepath.Join(dir, "file")
	backupPath := filepath.Join(stateDir, "file.bak")
	if err := os.WriteFile(f, []byte("100:/data\n200:/other\n"), 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	if err := RemoveLineFromFile(f, "100:", stateDir); err != nil {
		t.Fatalf("unexpected error on first rewrite: %v", err)
	}
	firstBackup, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("failed to read backup after first rewrite: %v", err)
	}

	if err := RemoveLineFromFile(f, "200:", stateDir); err != nil {
		t.Fatalf("unexpected error on second rewrite: %v", err)
	}
	secondBackup, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("failed to read backup after second rewrite: %v", err)
	}

	if string(secondBackup) == string(firstBackup)+string(firstBackup) {
		t.Errorf("backup appears to have accumulated content across rewrites: %q", string(secondBackup))
	}
	if strings.Count(string(secondBackup), "100:/data") != 0 {
		t.Errorf("second backup should reflect post-first-rewrite state, got %q", string(secondBackup))
	}
	if !strings.Contains(string(secondBackup), "200:/other") {
		t.Errorf("second backup should hold the pre-second-rewrite contents, got %q", string(secondBackup))
	}
}

func TestRemoveLineFromFile_DegradesWhenStateDirUnusable(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "file")
	if err := os.WriteFile(f, []byte("100:/data\n200:/other\n"), 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	t.Run("empty stateDir disables the sidecar but the rewrite still succeeds", func(t *testing.T) {
		if err := RemoveLineFromFile(f, "100:", ""); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if fileContains(t, f, "100:") {
			t.Errorf("expected rewrite to still happen with sidecar disabled")
		}
	})

	t.Run("stateDir that cannot be created still lets the rewrite succeed", func(t *testing.T) {
		f2 := filepath.Join(dir, "file2")
		if err := os.WriteFile(f2, []byte("300:/data\n400:/other\n"), 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}
		// A regular file in the way of stateDir makes os.MkdirAll fail.
		blocker := filepath.Join(t.TempDir(), "blocked")
		if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}
		unusableStateDir := filepath.Join(blocker, "state") // MkdirAll under a file: fails

		if err := RemoveLineFromFile(f2, "300:", unusableStateDir); err != nil {
			t.Fatalf("expected RemoveLineFromFile to degrade gracefully, got error: %v", err)
		}
		data, err := os.ReadFile(f2)
		if err != nil {
			t.Fatalf("failed to read: %v", err)
		}
		if strings.Contains(string(data), "300:") {
			t.Errorf("expected rewrite to still happen despite unusable state dir, got %q", string(data))
		}
	})
}

func TestRecoverProjectFile(t *testing.T) {
	t.Run("no sidecar is a no-op", func(t *testing.T) {
		dir := t.TempDir()
		f := filepath.Join(dir, "file")
		if err := os.WriteFile(f, []byte("100:/data\n"), 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}
		if err := RecoverProjectFile(f, t.TempDir()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !fileContains(t, f, "100:/data") {
			t.Errorf("file should be untouched")
		}
	})

	t.Run("empty stateDir is a no-op even for a corrupt target", func(t *testing.T) {
		dir := t.TempDir()
		f := filepath.Join(dir, "file")
		if err := os.WriteFile(f, []byte(""), 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}
		if err := RecoverProjectFile(f, ""); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, err := os.ReadFile(f); err != nil {
			t.Fatalf("failed to read: %v", err)
		}
	})

	t.Run("missing state directory is a no-op, not an error", func(t *testing.T) {
		dir := t.TempDir()
		f := filepath.Join(dir, "file")
		if err := os.WriteFile(f, []byte("100:/data\n"), 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}
		if err := RecoverProjectFile(f, filepath.Join(dir, "does-not-exist")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !fileContains(t, f, "100:/data") {
			t.Errorf("file should be untouched")
		}
	})

	t.Run("legitimately empty file on fresh install is not clobbered", func(t *testing.T) {
		dir := t.TempDir()
		f := filepath.Join(dir, "file")
		if err := os.WriteFile(f, []byte(""), 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}
		// No sidecar at all.
		if err := RecoverProjectFile(f, t.TempDir()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("failed to read: %v", err)
		}
		if len(data) != 0 {
			t.Errorf("expected file to remain empty, got %q", string(data))
		}
	})

	t.Run("empty file with empty sidecar is not clobbered", func(t *testing.T) {
		dir := t.TempDir()
		stateDir := t.TempDir()
		f := filepath.Join(dir, "file")
		if err := os.WriteFile(f, []byte(""), 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, "file.bak"), []byte(""), 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}
		if err := RecoverProjectFile(f, stateDir); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("failed to read: %v", err)
		}
		if len(data) != 0 {
			t.Errorf("expected file to remain empty, got %q", string(data))
		}
	})

	t.Run("truncated target with good sidecar is restored", func(t *testing.T) {
		dir := t.TempDir()
		stateDir := t.TempDir()
		f := filepath.Join(dir, "file")
		good := "100:/data\n200:/other\n"
		if err := os.WriteFile(filepath.Join(stateDir, "file.bak"), []byte(good), 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}
		if err := os.WriteFile(f, []byte(""), 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}

		if err := RecoverProjectFile(f, stateDir); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("failed to read: %v", err)
		}
		if string(data) != good {
			t.Errorf("expected restored content %q, got %q", good, string(data))
		}
	})

	t.Run("malformed line with good sidecar is restored", func(t *testing.T) {
		dir := t.TempDir()
		stateDir := t.TempDir()
		f := filepath.Join(dir, "file")
		good := "100:/data\n200:/other\n"
		if err := os.WriteFile(filepath.Join(stateDir, "file.bak"), []byte(good), 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}
		// Simulates a crash mid-write: the second line's "key:" got cut off
		// before the colon ever landed on disk.
		if err := os.WriteFile(f, []byte("100:/data\n20"), 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}

		if err := RecoverProjectFile(f, stateDir); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !fileContains(t, f, good) {
			t.Errorf("expected file to be restored from backup")
		}
	})

	t.Run("well-formed target is left alone even with a sidecar present", func(t *testing.T) {
		dir := t.TempDir()
		stateDir := t.TempDir()
		f := filepath.Join(dir, "file")
		current := "200:/other\n"
		if err := os.WriteFile(filepath.Join(stateDir, "file.bak"), []byte("100:/data\n"), 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}
		if err := os.WriteFile(f, []byte(current), 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}

		if err := RecoverProjectFile(f, stateDir); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("failed to read: %v", err)
		}
		if string(data) != current {
			t.Errorf("expected file to be left as-is, got %q", string(data))
		}
	})

	t.Run("missing target with sidecar is not created out of thin air", func(t *testing.T) {
		dir := t.TempDir()
		stateDir := t.TempDir()
		f := filepath.Join(dir, "file")
		if err := os.WriteFile(filepath.Join(stateDir, "file.bak"), []byte("100:/data\n"), 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}
		if err := RecoverProjectFile(f, stateDir); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("expected target to remain absent, got err=%v", err)
		}
	})
}

func TestAppendToFile_DurableAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "file")

	if err := AppendToFile(f, "100:/data\n", "100"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := AppendToFile(f, "100:/data\n", "100"); err != nil {
		t.Fatalf("unexpected error on repeat call: %v", err)
	}

	data, err := os.ReadFile(f)
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}
	if strings.Count(string(data), "100:/data") != 1 {
		t.Errorf("expected exactly one entry after repeated append, got %q", string(data))
	}
}

func TestReadProjectsFile(t *testing.T) {
	t.Run("missing file returns empty map, no error", func(t *testing.T) {
		dir := t.TempDir()
		got, err := ReadProjectsFile(filepath.Join(dir, "missing"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("expected empty map, got %v", got)
		}
	})

	t.Run("parses entries, skips comments and blanks", func(t *testing.T) {
		dir := t.TempDir()
		f := filepath.Join(dir, "projects")
		content := "# comment\n\n100:/data/one\n200:/data/two\nmalformed-line\n"
		if err := os.WriteFile(f, []byte(content), 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}

		got, err := ReadProjectsFile(f)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got["100"] != "/data/one" || got["200"] != "/data/two" {
			t.Errorf("unexpected result: %v", got)
		}
		if len(got) != 2 {
			t.Errorf("expected 2 entries, got %d: %v", len(got), got)
		}
	})
}

func TestReadProjidFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "projid")
	content := "# comment\nproj1:100\nproj2:200\n"
	if err := os.WriteFile(f, []byte(content), 0644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	got, err := ReadProjidFile(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Mapping is inverted: projectID -> projectName
	if got["100"] != "proj1" || got["200"] != "proj2" {
		t.Errorf("unexpected result: %v", got)
	}
}

func TestReadProjidFile_MissingFile(t *testing.T) {
	dir := t.TempDir()
	got, err := ReadProjidFile(filepath.Join(dir, "missing"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
}

func TestCheckProjectFileConsistency(t *testing.T) {
	t.Run("reports no mismatches for a fully consistent pair", func(t *testing.T) {
		dir := t.TempDir()
		projectsFile := filepath.Join(dir, "projects")
		projidFile := filepath.Join(dir, "projid")
		if err := os.WriteFile(projectsFile, []byte("3:/export/pvc-a\n"), 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}
		if err := os.WriteFile(projidFile, []byte("pvc_a:3\n"), 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}

		mismatches, err := CheckProjectFileConsistency(projectsFile, projidFile)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mismatches) != 0 {
			t.Errorf("expected no mismatches, got %v", mismatches)
		}
	})

	t.Run("reports a half-applied state in both directions", func(t *testing.T) {
		// id 3 has a path but no name (the reported defect's exact shape);
		// id 7 has a name but no path (the reverse half-applied case).
		dir := t.TempDir()
		projectsFile := filepath.Join(dir, "projects")
		projidFile := filepath.Join(dir, "projid")
		if err := os.WriteFile(projectsFile, []byte("3:/export/pvc-a-new\n"), 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}
		if err := os.WriteFile(projidFile, []byte("pvc_g:7\n"), 0644); err != nil {
			t.Fatalf("setup failed: %v", err)
		}

		mismatches, err := CheckProjectFileConsistency(projectsFile, projidFile)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mismatches) != 2 {
			t.Fatalf("expected 2 mismatches, got %v", mismatches)
		}
		joined := strings.Join(mismatches, " | ")
		if !strings.Contains(joined, "id 3") || !strings.Contains(joined, "/export/pvc-a-new") {
			t.Errorf("expected a mismatch describing id 3's orphaned path, got %v", mismatches)
		}
		if !strings.Contains(joined, "id 7") || !strings.Contains(joined, "pvc_g") {
			t.Errorf("expected a mismatch describing id 7's orphaned name, got %v", mismatches)
		}
	})
}

func TestRemoveQuotaByID(t *testing.T) {
	tests := []struct {
		name       string
		fsType     string
		runErr     error
		wantErr    bool
		wantCmd    string
		errContain string
	}{
		{name: "xfs success", fsType: FSTypeXFS, wantCmd: "xfs_quota"},
		{name: "ext4 success", fsType: FSTypeExt4, wantCmd: "setquota"},
		{name: "xfs failure propagates", fsType: FSTypeXFS, runErr: errors.New("boom"), wantErr: true, errContain: "failed to remove XFS quota"},
		{name: "ext4 failure propagates", fsType: FSTypeExt4, runErr: errors.New("boom"), wantErr: true, errContain: "failed to remove ext4 quota"},
		{name: "unsupported filesystem", fsType: "zfs", wantErr: true, errContain: "unsupported filesystem"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
				return []byte("output"), tt.runErr
			}}
			withFakeRunner(t, r)

			err := RemoveQuotaByID("/data", tt.fsType, "1001")
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				if tt.errContain != "" && !strings.Contains(err.Error(), tt.errContain) {
					t.Errorf("expected error to contain %q, got %v", tt.errContain, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantCmd != "" {
				if len(r.calls) != 1 || r.calls[0].name != tt.wantCmd {
					t.Errorf("expected single call to %s, got %+v", tt.wantCmd, r.calls)
				}
			}
		})
	}

	t.Run("unsupported filesystem makes no exec call", func(t *testing.T) {
		r := &fakeRunner{}
		withFakeRunner(t, r)
		_ = RemoveQuotaByID("/data", "zfs", "1001")
		if len(r.calls) != 0 {
			t.Errorf("expected no exec calls for unsupported filesystem, got %d", len(r.calls))
		}
	})

	t.Run("invalid basePath returns error and zero exec calls", func(t *testing.T) {
		r := &fakeRunner{}
		withFakeRunner(t, r)
		err := RemoveQuotaByID("/data/proj ect1", FSTypeXFS, "1001")
		if err == nil {
			t.Fatal("expected error from basePath validation")
		}
		if !strings.Contains(err.Error(), "invalid basePath") {
			t.Errorf("unexpected error: %v", err)
		}
		if len(r.calls) != 0 {
			t.Errorf("expected zero exec calls, got %d", len(r.calls))
		}
	})

	t.Run("invalid projectID returns error and zero exec calls", func(t *testing.T) {
		// projectID is a string here (unlike ApplyXFSQuota/ApplyExt4Quota's
		// numeric projectID), sourced from callers that may have parsed it
		// out of /etc/projid content -- an independent review found it
		// reaching xfs_quota/setquota argv unvalidated at both call sites.
		r := &fakeRunner{}
		withFakeRunner(t, r)
		err := RemoveQuotaByID("/data", FSTypeXFS, "1001 extra")
		if err == nil {
			t.Fatal("expected error from projectID validation")
		}
		if !strings.Contains(err.Error(), "invalid projectID") {
			t.Errorf("unexpected error: %v", err)
		}
		if len(r.calls) != 0 {
			t.Errorf("expected zero exec calls, got %d", len(r.calls))
		}
	})
}

// A rejected name must leave pre-existing content alone, not merely avoid
// creating the files: the reachable case is a bad annotation arriving on a
// host that already has projects/projid populated.
func TestAddProject_RejectedNameLeavesExistingFilesIntact(t *testing.T) {
	dir := t.TempDir()
	projectsFile := filepath.Join(dir, "projects")
	projidFile := filepath.Join(dir, "projid")

	const seedProjects = "3:/export/pvc-a\n"
	const seedProjid = "pvc_a:3\n"
	if err := os.WriteFile(projectsFile, []byte(seedProjects), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projidFile, []byte(seedProjid), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := AddProject("/export/victim", "evil:999", 7, projectsFile, projidFile); err == nil {
		t.Fatal("expected an error for a project name containing ':'")
	}

	gotProjects, err := os.ReadFile(projectsFile)
	if err != nil {
		t.Fatal(err)
	}
	gotProjid, err := os.ReadFile(projidFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotProjects) != seedProjects {
		t.Errorf("projects changed: %q", string(gotProjects))
	}
	if string(gotProjid) != seedProjid {
		t.Errorf("projid changed: %q", string(gotProjid))
	}
}

// Positive control for the delimiter rejection: an ordinary name still writes
// through and parses back with its ID intact, which is what makes the ID
// visible to allocation.
func TestAddProject_OrdinaryNameStillParsesBack(t *testing.T) {
	dir := t.TempDir()
	projectsFile := filepath.Join(dir, "projects")
	projidFile := filepath.Join(dir, "projid")

	if err := AddProject("/export/pvc-b", "pvc_b", 7, projectsFile, projidFile); err != nil {
		t.Fatalf("AddProject: %v", err)
	}

	ids, err := ReadProjidFile(projidFile)
	if err != nil {
		t.Fatal(err)
	}
	if ids["7"] != "pvc_b" {
		t.Fatalf("projid parsed back as %v; ID 7 must map to pvc_b or it is invisible to allocation", ids)
	}
}
