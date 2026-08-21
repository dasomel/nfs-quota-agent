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

// Package quota implements commands to configure, update, and remove project quotas
// across various file systems like XFS, ext4, and Btrfs. It manages configurations
// in projects and projid files and invokes command line utilities to enforce limits.
package quota

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// AddProject adds a project to the projects and projid files.
//
// Order is load-bearing: projid is written first, then projects.
// loadExistingProjectIDs (internal/agent/agent.go) reads projid to decide
// which project IDs are already taken. If a crash happens after the projid
// write but before the projects write, the ID is already reserved and the
// next sync simply re-runs AddProject: the projid write no-ops (the key
// already exists) and projects gets written, so the sequence is
// self-healing. Writing projects first would leave the ID unreserved in
// projid after a crash, letting a different project claim the same ID and
// bleed quota across two paths. Do not reorder these two writes.
func AddProject(path, projectName string, projectID uint32, projectsFile, projidFile string) error {
	// Add to projid file: projectName:projectID
	projidEntry := fmt.Sprintf("%s:%d\n", projectName, projectID)
	if err := AppendToFile(projidFile, projidEntry, projectName); err != nil {
		return err
	}

	// Add to projects file: projectID:path
	projectsEntry := fmt.Sprintf("%d:%s\n", projectID, path)
	if err := AppendToFile(projectsFile, projectsEntry, strconv.FormatUint(uint64(projectID), 10)); err != nil {
		return err
	}

	return nil
}

// AppendToFile appends an entry to a file if it doesn't already exist. The
// write is fsynced (file and containing directory) before returning success,
// so a caller that gets a nil error knows the bytes reached disk rather than
// just the page cache.
func AppendToFile(filename, entry, searchKey string) (err error) {
	// Read existing content
	data, err := os.ReadFile(filename)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	// Check if entry already exists by looking for searchKey at the start of any line
	prefix := searchKey + ":"
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) || line == searchKey {
			return nil // Already exists
		}
	}

	// Append entry
	f, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()

	if _, err = f.WriteString(entry); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	err = syncDir(filepath.Dir(filename))
	return err
}

// RemoveLineFromFile removes lines starting with prefix from a file.
//
// The target file (/etc/projects or /etc/projid) is bind-mounted as an
// individual host file, so it cannot be replaced via the usual
// temp-file-plus-rename pattern — rename onto a mount point fails with
// EBUSY, and even a successful rename elsewhere would only replace the
// mount inside the container, decoupling the agent from the file the quota
// tools actually read. The rewrite therefore has to happen in place, which
// opens a real (if small) window where a crash mid-write leaves a truncated
// or partial file. To make that recoverable rather than eliminating it
// (which in-place rewriting cannot do), the pre-rewrite contents are first
// saved to a sidecar under stateDir — see sidecarPath — fsynced before the
// rewrite starts. RecoverProjectFile restores from it on the next startup
// if the rewrite didn't finish.
//
// stateDir must be a host-backed directory, not a directory that lives
// inside a bind-mounted individual file's own mount (e.g. the container's
// /etc when only /etc/projects and /etc/projid are bind-mounted into it) —
// otherwise the sidecar is written to the container's ephemeral layer and
// is gone on the next restart, exactly when recovery is needed. Losing the
// backup must never block the rewrite itself: if stateDir is empty, or the
// backup can't be created or written, this logs a warning and proceeds
// with the rewrite anyway.
func RemoveLineFromFile(filename, prefix, stateDir string) error {
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}

	if backupPath := sidecarPath(filename, stateDir); backupPath != "" {
		if mkErr := os.MkdirAll(stateDir, 0755); mkErr != nil {
			slog.Warn("Cannot create state directory; proceeding without a recovery backup",
				"stateDir", stateDir, "error", mkErr)
		} else if wErr := writeFileSynced(backupPath, data, 0644); wErr != nil {
			slog.Warn("Failed to write recovery backup; proceeding without one",
				"backup", backupPath, "error", wErr)
		}
	}

	var newLines []string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, prefix) {
			newLines = append(newLines, line)
		}
	}

	return writeFileSynced(filename, []byte(strings.Join(newLines, "\n")), 0644)
}

// backupSuffix names the sidecar RemoveLineFromFile keeps next to its
// target. It is a single fixed name that gets overwritten on every rewrite,
// so it never accumulates garbage across repeated operations.
const backupSuffix = ".bak"

// sidecarPath returns the backup sidecar path for filename under stateDir,
// or "" if stateDir is empty — the signal used throughout this file to mean
// "no recovery backup available," so the sidecar degrades gracefully
// instead of failing the caller's real operation.
func sidecarPath(filename, stateDir string) string {
	if stateDir == "" {
		return ""
	}
	return filepath.Join(stateDir, filepath.Base(filename)+backupSuffix)
}

// writeFileSynced truncates (or creates) filename, writes data, and fsyncs
// both the file and its containing directory before returning, so a nil
// error means the bytes are durable rather than merely cached.
func writeFileSynced(filename string, data []byte, perm os.FileMode) (err error) {
	f, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()

	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	err = syncDir(filepath.Dir(filename))
	return err
}

// syncDir fsyncs a directory so that entries created or replaced within it
// (a new file, a rewritten file's data) survive a crash rather than only
// existing in the page cache. Linux-only, which matches this agent's
// privileged, host-node-only design.
func syncDir(dir string) (err error) {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := d.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()

	err = d.Sync()
	return err
}

// isWellFormedProjectFile reports whether every non-blank, non-comment line
// in data contains a ":" separator, matching the projects/projid line
// format. A line that fails this is treated as evidence of a corrupt,
// partially-written file.
func isWellFormedProjectFile(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.Contains(line, ":") {
			return false
		}
	}
	return true
}

// RecoverProjectFile restores filename from its RemoveLineFromFile backup
// sidecar under stateDir when filename looks truncated or corrupt from a
// crash mid-rewrite: zero-length while a non-empty sidecar exists, or
// containing a line that fails to parse as "key:value". It is a no-op when
// stateDir is empty, there is no sidecar (nothing to recover from), or the
// sidecar is itself empty, so a file that is legitimately empty on a fresh
// install — or a host where the state directory isn't available at all —
// is never clobbered. Callers should invoke it before anything else reads
// these files.
func RecoverProjectFile(filename, stateDir string) error {
	backupPath := sidecarPath(filename, stateDir)
	if backupPath == "" {
		return nil
	}

	backup, err := os.ReadFile(backupPath)
	if err != nil || len(backup) == 0 {
		// No usable sidecar to recover from.
		return nil
	}

	data, err := os.ReadFile(filename)
	if err != nil {
		if os.IsNotExist(err) {
			// No target file yet: a fresh install, not corruption.
			return nil
		}
		return err
	}

	if len(data) != 0 && isWellFormedProjectFile(data) {
		return nil // target looks fine, nothing to do
	}

	slog.Error("Detected truncated/corrupt project file, restoring from backup",
		"file", filename, "backup", backupPath)
	return writeFileSynced(filename, backup, 0644)
}

// ReadProjectsFile reads the projects file and returns projectID -> path mapping
func ReadProjectsFile(filename string) (map[string]string, error) {
	result := make(map[string]string)

	data, err := os.ReadFile(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return nil, err
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			result[parts[0]] = parts[1]
		}
	}

	return result, nil
}

// ReadProjidFile reads the projid file and returns projectID -> projectName mapping
func ReadProjidFile(filename string) (map[string]string, error) {
	result := make(map[string]string)

	data, err := os.ReadFile(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return nil, err
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			// projectName:projectID -> projectID:projectName
			result[parts[1]] = parts[0]
		}
	}

	return result, nil
}

// RemoveQuotaByID removes quota for a project ID
func RemoveQuotaByID(basePath, fsType, projectID string) error {
	if err := validateQuotaArg("basePath", basePath); err != nil {
		return err
	}

	switch fsType {
	case FSTypeXFS:
		// Set hard block limit to 0 (unlimited), effectively removing the quota
		if output, err := defaultRunner.Run("xfs_quota", "-x", "-c",
			fmt.Sprintf("limit -p bhard=0 %s", projectID),
			basePath); err != nil {
			return fmt.Errorf("failed to remove XFS quota for project %s: %w, output: %s", projectID, err, string(output))
		}
		return nil
	case FSTypeExt4:
		// Remove ext4 project quota by setting all limits to 0
		if output, err := defaultRunner.Run("setquota", "-P", projectID, "0", "0", "0", "0", basePath); err != nil {
			return fmt.Errorf("failed to remove ext4 quota for project %s: %w, output: %s", projectID, err, string(output))
		}
		return nil
	default:
		return fmt.Errorf("unsupported filesystem: %s", fsType)
	}
}
