//go:build !unix

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
)

// GetDirAllocatedSize falls back to apparent size (filepath.Walk summing
// info.Size(), identical to GetDirSize) on non-unix platforms, since
// syscall.Stat_t's Blocks field isn't available there. This build only
// exists so the package compiles on a non-unix GOOS; the agent itself only
// ever runs on Linux (privileged, host-node execution -- see CLAUDE.md).
// See dirsize_unix.go's doc comment for what this walk is for and why it
// has exactly one caller.
func GetDirAllocatedSize(path string) uint64 {
	var size uint64
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			size += uint64(info.Size())
		}
		return nil
	})
	return size
}
