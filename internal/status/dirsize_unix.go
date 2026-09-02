//go:build unix

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
	"syscall"
)

// GetDirAllocatedSize sums the actual disk blocks allocated to every
// regular file under path (st.Blocks * 512, the same unit filesystem quota
// accounting charges against), rather than GetDirSize's apparent size
// (info.Size()). It exists for exactly one caller -- GetDirUsagesStrict,
// which feeds the brownfield shrink-guard snapshot (#94) -- and must not be
// substituted for GetDirSize/GetDirUsages anywhere else: every existing
// consumer (metrics, the web UI, history, cleanup, orphan sizing) keeps
// reading apparent size unchanged, so no displayed number in this repo
// changes because of this function.
//
// Block allocation is closer to quota accounting than apparent size, but it
// is NOT equal to what any of the three backends actually charges: XFS
// charges extent/metadata overhead by inode-to-project assignment rather
// than by path walk, and btrfs qgroups charge shared extents from CoW/
// reflinks that a per-file walk can't see at all. This is a suspicion
// trigger for the brownfield guard, not a claim of matching real
// enforcement -- see docs/arch notes for #94.
//
// A hardlinked file's blocks belong to disk exactly once no matter how many
// directory entries reference it, so this walk dedupes by (Dev, Ino): the
// first path found for a given inode counts its blocks, and every
// subsequent hardlink to that same inode is skipped -- otherwise a tree
// full of hardlinks would overstate allocation by counting the same blocks
// once per link.
//
// info.Sys() is asserted to *syscall.Stat_t via the comma-ok form, not a
// bare assertion: if that ever fails for a given entry (an unexpected
// os.FileInfo implementation), this falls back to info.Size() for that one
// file instead of panicking the whole walk.
func GetDirAllocatedSize(path string) uint64 {
	var size uint64
	seen := make(map[[2]uint64]struct{})
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			size += uint64(info.Size())
			return nil
		}
		key := [2]uint64{uint64(st.Dev), uint64(st.Ino)}
		if _, dup := seen[key]; dup {
			return nil
		}
		seen[key] = struct{}{}
		size += uint64(st.Blocks) * 512
		return nil
	})
	return size
}
