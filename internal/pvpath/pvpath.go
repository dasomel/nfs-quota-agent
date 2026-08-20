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

// Package pvpath is the single source of truth for mapping a Kubernetes
// PersistentVolume to an NFS server-side path and, from there, to the local
// path where the agent applies filesystem quotas. Prior to this package the
// same logic was hand-rolled three times (agent, ui, cleanup) and one copy
// — cleanup — was missing CSI NFS support entirely, letting cleanup treat
// live CSI-backed volumes as orphaned. Callers that need path identity
// (e.g. deciding whether it is safe to delete something) must use Contains
// rather than string prefix checks — see its doc comment for why.
package pvpath

import (
	"path/filepath"
	"strings"

	v1 "k8s.io/api/core/v1"
)

// NFSPath extracts the NFS server-side export path from a PersistentVolume.
// It supports native NFS volumes (Spec.NFS.Path) and CSI NFS volumes
// (Spec.CSI.VolumeAttributes "share" + "subDir"/"subdir"). "subDir" takes
// precedence over the lowercase "subdir" when both are set. When only
// "share" is present, it falls back to joining the share with the PV name,
// matching how most CSI NFS provisioners lay out subdirectories. Returns ""
// when the PV has neither a recognizable native nor CSI NFS source.
func NFSPath(pv *v1.PersistentVolume) string {
	if pv.Spec.NFS != nil {
		return pv.Spec.NFS.Path
	}

	if pv.Spec.CSI != nil && pv.Spec.CSI.VolumeAttributes != nil {
		share := pv.Spec.CSI.VolumeAttributes["share"]
		subdir := pv.Spec.CSI.VolumeAttributes["subDir"]
		if subdir == "" {
			subdir = pv.Spec.CSI.VolumeAttributes["subdir"]
		}
		if share != "" && subdir != "" {
			return filepath.Join(share, subdir)
		}
		if share != "" {
			return filepath.Join(share, pv.Name)
		}
	}

	return ""
}

// LocalPath is the result of mapping an NFS server-side path to a local
// mount path.
type LocalPath struct {
	// Path is the resolved local path.
	Path string
	// Fallback is true when nfsPath did not start with nfsServerPath, so the
	// mapping fell back to joining nfsPath's basename onto nfsBasePath. A
	// fallback-derived path is ambiguous — two different nfsPaths that only
	// share a basename resolve to the same local path — so callers that
	// need certainty (e.g. cleanup deciding what to delete) must treat a
	// Fallback result as untrustworthy rather than authoritative.
	Fallback bool
}

// ToLocal converts an NFS server-side path to its local mount path by
// stripping the nfsServerPath prefix and joining the remainder onto
// nfsBasePath. When nfsPath does not start with nfsServerPath, it falls
// back to joining just the basename onto nfsBasePath and reports
// Fallback=true so the caller can decide how much to trust the result.
func ToLocal(nfsPath, nfsServerPath, nfsBasePath string) LocalPath {
	if strings.HasPrefix(nfsPath, nfsServerPath) {
		return LocalPath{
			Path: filepath.Join(nfsBasePath, strings.TrimPrefix(nfsPath, nfsServerPath)),
		}
	}
	return LocalPath{
		Path:     filepath.Join(nfsBasePath, filepath.Base(nfsPath)),
		Fallback: true,
	}
}

// Contains reports whether target is root itself or a descendant of root,
// comparing cleaned paths via filepath.Rel rather than a string prefix
// check. A naive strings.HasPrefix(target, root) is unsafe for this
// decision in two ways: it treats "/export/../etc" (which escapes root) as
// contained because ".." has not been resolved, and it treats "/exportfoo"
// (a sibling, not a child) as contained because it merely starts with the
// same characters. filepath.Rel compares cleaned path components instead,
// so both cases are correctly rejected.
func Contains(root, target string) bool {
	cleanRoot := filepath.Clean(root)
	cleanTarget := filepath.Clean(target)

	rel, err := filepath.Rel(cleanRoot, cleanTarget)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
