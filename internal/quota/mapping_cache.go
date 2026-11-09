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
	"os"
	"path/filepath"
	"sync"
)

// D1: stat-keyed parsed mapping cache
//
// Reason: PV reconciles re-read /etc/projects and /etc/projid fully ~6 times
// per PV (checkProjectIdentityConflict -> ReadProjidFile/ReadProjectsFile,
// report.go readColonMapping), causing disk reads and line-parsing overhead
// to scale quadratically O(N^2) with the number of PVs (e.g. 2.06 MB at 100 PVs,
// 203 MB at 1,000, 810 MB at 2,000 PVs). Caching parsed mappings keyed by
// absolute filename and validated against filesystem metadata (device, inode,
// size, mtime ns, ctime ns) via syscall.Stat_t avoids re-reading and re-parsing
// untouched files during event storms and steady reconciles.
//
// Cost: Residual risk that an external editor or process modifies a mapping file
// in-place without altering its device, inode, total byte size, or nanosecond-level
// mtime and ctime within filesystem timestamp granularity. In that edge case, the
// cache would serve stale mapping data until the next agent write, restart, or
// mtime change.
//
// Escape hatch: Set NFS_QUOTA_DISABLE_MAPPING_CACHE=1 in the environment to
// completely disable caching and read directly from disk on every invocation.
var mappingCacheDisabled bool

func init() {
	if os.Getenv("NFS_QUOTA_DISABLE_MAPPING_CACHE") == "1" {
		mappingCacheDisabled = true
	}
}

type statKey struct {
	dev      uint64
	ino      uint64
	size     int64
	mtimeSec int64
	mtimeNs  int64
	ctimeSec int64
	ctimeNs  int64
}

type cachedMapping struct {
	stat        statKey
	colonMap    map[string]string // key:value as written in file
	invertedMap map[string]string // value:key (for ReadProjidFile: id -> name)
}

var (
	mappingCacheMu sync.Mutex
	mappingCache   = make(map[string]cachedMapping)

	// Test hooks for observability and verification assertions.
	mappingCacheHits   uint64
	mappingCacheMisses uint64
	mappingCacheReads  uint64
)

// ResetMappingCacheForTesting resets the cache state and counters for tests.
func ResetMappingCacheForTesting() {
	mappingCacheMu.Lock()
	clear(mappingCache)
	mappingCacheHits = 0
	mappingCacheMisses = 0
	mappingCacheReads = 0
	mappingCacheMu.Unlock()
}

// GetMappingCacheStatsForTesting returns the current hit, miss, and disk read counts.
func GetMappingCacheStatsForTesting() (hits, misses, reads uint64) {
	mappingCacheMu.Lock()
	defer mappingCacheMu.Unlock()
	return mappingCacheHits, mappingCacheMisses, mappingCacheReads
}

// SetMappingCacheDisabledForTesting sets whether the cache is disabled during tests.
// It returns a restore function.
func SetMappingCacheDisabledForTesting(disabled bool) func() {
	prev := mappingCacheDisabled
	mappingCacheDisabled = disabled
	return func() {
		mappingCacheDisabled = prev
	}
}

// copyMappingMap returns a shallow copy of m so that callers cannot mutate
// cached mapping state.
func copyMappingMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

// invalidateMappingCache clears the cached entry for filename if present.
func invalidateMappingCache(filename string) {
	if filename == "" {
		return
	}
	absPath, err := filepath.Abs(filename)
	if err != nil {
		absPath = filename
	}
	mappingCacheMu.Lock()
	delete(mappingCache, absPath)
	mappingCacheMu.Unlock()
}

// readAndParseMapping reads filename from disk and parses it into colonMap
// (key:value) and invertedMap (value:key). Never caches an error.
func readAndParseMapping(absPath string) (map[string]string, map[string]string, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, nil, err
	}

	colonMap := make(map[string]string)
	invertedMap := make(map[string]string)
	ForEachMappingLine(data, func(key, value string) bool {
		colonMap[key] = value
		invertedMap[value] = key
		return true
	})

	mappingCacheMu.Lock()
	mappingCacheReads++
	mappingCacheMu.Unlock()

	return colonMap, invertedMap, nil
}

// loadMapping retrieves the parsed colonMap and invertedMap for filename.
// If the cache is disabled or Stat_t is unavailable on the platform, it reads
// from disk directly. Missing or unreadable files return an error and are never
// cached.
func loadMapping(filename string) (map[string]string, map[string]string, error) {
	if filename == "" {
		_, err := os.ReadFile("")
		return nil, nil, err
	}

	absPath, err := filepath.Abs(filename)
	if err != nil {
		absPath = filename
	}

	if mappingCacheDisabled {
		return readAndParseMapping(absPath)
	}

	fi, err := os.Stat(absPath)
	if err != nil {
		// Missing or inaccessible file: ensure any stale entry is removed,
		// and never cache the error.
		mappingCacheMu.Lock()
		delete(mappingCache, absPath)
		mappingCacheMu.Unlock()
		return nil, nil, err
	}

	sk, ok := getStatKey(fi)
	if !ok {
		// Stat_t unavailable on this platform or FileInfo: bypass cache.
		return readAndParseMapping(absPath)
	}

	mappingCacheMu.Lock()
	cached, hit := mappingCache[absPath]
	if hit && cached.stat == sk {
		mappingCacheHits++
		cMap := copyMappingMap(cached.colonMap)
		iMap := copyMappingMap(cached.invertedMap)
		mappingCacheMu.Unlock()
		return cMap, iMap, nil
	}
	mappingCacheMisses++
	mappingCacheMu.Unlock()

	// Cache miss: read from disk and parse.
	data, err := os.ReadFile(absPath)
	if err != nil {
		mappingCacheMu.Lock()
		delete(mappingCache, absPath)
		mappingCacheMu.Unlock()
		return nil, nil, err
	}

	colonMap := make(map[string]string)
	invertedMap := make(map[string]string)
	ForEachMappingLine(data, func(key, value string) bool {
		colonMap[key] = value
		invertedMap[value] = key
		return true
	})

	// Re-stat to ensure file was not modified during read before caching.
	fi2, err := os.Stat(absPath)
	if err == nil {
		if sk2, ok := getStatKey(fi2); ok && sk2 == sk {
			mappingCacheMu.Lock()
			mappingCache[absPath] = cachedMapping{
				stat:        sk2,
				colonMap:    colonMap,
				invertedMap: invertedMap,
			}
			mappingCacheReads++
			mappingCacheMu.Unlock()
		}
	}

	return copyMappingMap(colonMap), copyMappingMap(invertedMap), nil
}
