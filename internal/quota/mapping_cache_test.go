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
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestMappingCache_HitAvoidsDiskRead(t *testing.T) {
	ResetMappingCacheForTesting()
	dir := t.TempDir()
	projectsFile := filepath.Join(dir, "projects")
	projidFile := filepath.Join(dir, "projid")

	if err := os.WriteFile(projectsFile, []byte("100:/export/pvc-1\n200:/export/pvc-2\n"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	if err := os.WriteFile(projidFile, []byte("pvc-1:100\npvc-2:200\n"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// First read: cache miss, reads from disk.
	m1, err := ReadProjectsFile(projectsFile)
	if err != nil {
		t.Fatalf("ReadProjectsFile error: %v", err)
	}
	if len(m1) != 2 || m1["100"] != "/export/pvc-1" {
		t.Fatalf("unexpected content: %v", m1)
	}

	hits, misses, reads := GetMappingCacheStatsForTesting()
	if hits != 0 || misses != 1 || reads != 1 {
		t.Fatalf("initial read stats: hits=%d, misses=%d, reads=%d (want 0, 1, 1)", hits, misses, reads)
	}

	// Second read: cache hit, no disk read!
	m2, err := ReadProjectsFile(projectsFile)
	if err != nil {
		t.Fatalf("ReadProjectsFile error: %v", err)
	}
	if !reflect.DeepEqual(m1, m2) {
		t.Fatalf("content mismatch on cache hit: got %v, want %v", m2, m1)
	}

	hits, misses, reads = GetMappingCacheStatsForTesting()
	if hits != 1 || misses != 1 || reads != 1 {
		t.Fatalf("second read stats: hits=%d, misses=%d, reads=%d (want 1, 1, 1)", hits, misses, reads)
	}

	// readColonMapping also hits the same cached entry!
	colonMap, err := readColonMapping(projectsFile)
	if err != nil {
		t.Fatalf("readColonMapping error: %v", err)
	}
	if !reflect.DeepEqual(colonMap, m1) {
		t.Fatalf("readColonMapping mismatch: got %v, want %v", colonMap, m1)
	}

	hits, misses, reads = GetMappingCacheStatsForTesting()
	if hits != 2 || misses != 1 || reads != 1 {
		t.Fatalf("readColonMapping read stats: hits=%d, misses=%d, reads=%d (want 2, 1, 1)", hits, misses, reads)
	}

	// ReadProjidFile on projidFile: initial read is a miss, second is a hit.
	p1, err := ReadProjidFile(projidFile)
	if err != nil {
		t.Fatalf("ReadProjidFile error: %v", err)
	}
	if p1["100"] != "pvc-1" || p1["200"] != "pvc-2" {
		t.Fatalf("unexpected projid content: %v", p1)
	}

	hits, misses, reads = GetMappingCacheStatsForTesting()
	if hits != 2 || misses != 2 || reads != 2 {
		t.Fatalf("projid first read stats: hits=%d, misses=%d, reads=%d (want 2, 2, 2)", hits, misses, reads)
	}

	p2, err := ReadProjidFile(projidFile)
	if err != nil {
		t.Fatalf("ReadProjidFile error: %v", err)
	}
	if !reflect.DeepEqual(p1, p2) {
		t.Fatalf("projid content mismatch on cache hit: got %v, want %v", p2, p1)
	}

	hits, misses, reads = GetMappingCacheStatsForTesting()
	if hits != 3 || misses != 2 || reads != 2 {
		t.Fatalf("projid second read stats: hits=%d, misses=%d, reads=%d (want 3, 2, 2)", hits, misses, reads)
	}
}

func TestMappingCache_InvalidationAfterPackageWrites(t *testing.T) {
	ResetMappingCacheForTesting()
	dir := t.TempDir()
	f := filepath.Join(dir, "projects")

	if err := os.WriteFile(f, []byte("100:/export/pvc-1\n"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// 1. Prime cache
	m, err := ReadProjectsFile(f)
	if err != nil || len(m) != 1 {
		t.Fatalf("prime failed: %v, %v", m, err)
	}

	// 2. AppendToFile invalidation
	if err := AppendToFile(f, "200:/export/pvc-2\n", "200"); err != nil {
		t.Fatalf("AppendToFile failed: %v", err)
	}

	// Cache was invalidated: next read must be a miss and contain the new entry
	m, err = ReadProjectsFile(f)
	if err != nil || len(m) != 2 || m["200"] != "/export/pvc-2" {
		t.Fatalf("AppendToFile invalidation check failed: %v, %v", m, err)
	}

	// Prime again
	_, _ = ReadProjectsFile(f)

	// 3. RemoveLineFromFile invalidation
	stateDir := filepath.Join(dir, "state")
	if err := RemoveLineFromFile(f, "200:", stateDir); err != nil {
		t.Fatalf("RemoveLineFromFile failed: %v", err)
	}

	m, err = ReadProjectsFile(f)
	if err != nil || len(m) != 1 || m["200"] != "" {
		t.Fatalf("RemoveLineFromFile invalidation check failed: %v, %v", m, err)
	}

	// 4. writeFileSynced invalidation
	if err := writeFileSynced(f, []byte("300:/export/pvc-3\n"), 0644); err != nil {
		t.Fatalf("writeFileSynced failed: %v", err)
	}

	m, err = ReadProjectsFile(f)
	if err != nil || len(m) != 1 || m["300"] != "/export/pvc-3" {
		t.Fatalf("writeFileSynced invalidation check failed: %v, %v", m, err)
	}

	// 5. RecoverProjectFile invalidation
	// Corrupt file and restore from backup sidecar
	backupPath := sidecarPath(f, stateDir)
	if err := os.WriteFile(backupPath, []byte("400:/export/pvc-4\n"), 0644); err != nil {
		t.Fatalf("WriteFile backup failed: %v", err)
	}
	// Truncate target file to simulate crash
	if err := os.WriteFile(f, []byte(""), 0644); err != nil {
		t.Fatalf("WriteFile truncate failed: %v", err)
	}

	if err := RecoverProjectFile(f, stateDir); err != nil {
		t.Fatalf("RecoverProjectFile failed: %v", err)
	}

	m, err = ReadProjectsFile(f)
	if err != nil || len(m) != 1 || m["400"] != "/export/pvc-4" {
		t.Fatalf("RecoverProjectFile invalidation check failed: %v, %v", m, err)
	}
}

func TestMappingCache_ExternalModificationDetected(t *testing.T) {
	ResetMappingCacheForTesting()
	dir := t.TempDir()
	f := filepath.Join(dir, "projects")

	if err := os.WriteFile(f, []byte("100:/export/pvc-1\n"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	m1, err := ReadProjectsFile(f)
	if err != nil || len(m1) != 1 {
		t.Fatalf("first read failed: %v, %v", m1, err)
	}

	// Sleep briefly so mtime changes on filesystems with low timestamp resolution
	time.Sleep(10 * time.Millisecond)

	// External rewrite
	if err := os.WriteFile(f, []byte("500:/export/pvc-5\n600:/export/pvc-6\n"), 0644); err != nil {
		t.Fatalf("external WriteFile failed: %v", err)
	}

	m2, err := ReadProjectsFile(f)
	if err != nil {
		t.Fatalf("second read failed: %v", err)
	}
	if len(m2) != 2 || m2["500"] != "/export/pvc-5" || m2["600"] != "/export/pvc-6" {
		t.Fatalf("external rewrite was not detected; got: %v", m2)
	}
}

func TestMappingCache_MissingAndStrictErrorSemantics(t *testing.T) {
	ResetMappingCacheForTesting()
	dir := t.TempDir()
	missing := filepath.Join(dir, "non-existent-file")

	// ReadProjectsFile on missing file: empty map, nil error
	pMap, err := ReadProjectsFile(missing)
	if err != nil {
		t.Fatalf("ReadProjectsFile on missing file returned error: %v", err)
	}
	if len(pMap) != 0 {
		t.Fatalf("ReadProjectsFile on missing file returned non-empty map: %v", pMap)
	}

	// ReadProjidFile on missing file: empty map, nil error
	idMap, err := ReadProjidFile(missing)
	if err != nil {
		t.Fatalf("ReadProjidFile on missing file returned error: %v", err)
	}
	if len(idMap) != 0 {
		t.Fatalf("ReadProjidFile on missing file returned non-empty map: %v", idMap)
	}

	// readColonMapping on missing file: empty map, error NOT nil
	colonMap, err := readColonMapping(missing)
	if err == nil {
		t.Fatalf("readColonMapping on missing file expected error, got nil")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("readColonMapping expected IsNotExist error, got: %v", err)
	}
	if len(colonMap) != 0 {
		t.Fatalf("readColonMapping returned non-empty map: %v", colonMap)
	}

	// Unreadable directory as file:
	unreadableDir := filepath.Join(dir, "a-directory")
	if err := os.Mkdir(unreadableDir, 0755); err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}
	_, pErr := ReadProjectsFile(unreadableDir)
	if pErr == nil {
		t.Fatalf("ReadProjectsFile on directory expected error, got nil")
	}
	_, cErr := readColonMapping(unreadableDir)
	if cErr == nil {
		t.Fatalf("readColonMapping on directory expected error, got nil")
	}

	// Errors are never cached: hits must be 0
	hits, _, _ := GetMappingCacheStatsForTesting()
	if hits != 0 {
		t.Fatalf("hits after error calls: got %d, want 0", hits)
	}
}

func TestMappingCache_ReturnedMapsNotSharedMutableState(t *testing.T) {
	ResetMappingCacheForTesting()
	dir := t.TempDir()
	f := filepath.Join(dir, "projects")

	if err := os.WriteFile(f, []byte("100:/export/pvc-1\n"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	m1, err := ReadProjectsFile(f)
	if err != nil {
		t.Fatalf("ReadProjectsFile failed: %v", err)
	}
	m1["100"] = "/mutated"
	m1["999"] = "/injected"

	m2, err := ReadProjectsFile(f)
	if err != nil {
		t.Fatalf("second ReadProjectsFile failed: %v", err)
	}
	if m2["100"] != "/export/pvc-1" {
		t.Fatalf("cached map was mutated: m2[100] = %q, want %q", m2["100"], "/export/pvc-1")
	}
	if _, ok := m2["999"]; ok {
		t.Fatalf("cached map was polluted with injected key")
	}

	// Test readColonMapping immutability
	c1, err := readColonMapping(f)
	if err != nil {
		t.Fatalf("readColonMapping failed: %v", err)
	}
	c1["100"] = "/mutated-colon"
	c2, err := readColonMapping(f)
	if err != nil {
		t.Fatalf("second readColonMapping failed: %v", err)
	}
	if c2["100"] != "/export/pvc-1" {
		t.Fatalf("cached colonMap was mutated: c2[100] = %q", c2["100"])
	}
}

func TestMappingCache_EscapeHatchDisabled(t *testing.T) {
	ResetMappingCacheForTesting()
	restore := SetMappingCacheDisabledForTesting(true)
	defer restore()

	dir := t.TempDir()
	f := filepath.Join(dir, "projects")
	if err := os.WriteFile(f, []byte("100:/export/pvc-1\n"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	_, _ = ReadProjectsFile(f)
	_, _ = ReadProjectsFile(f)

	hits, _, reads := GetMappingCacheStatsForTesting()
	if hits != 0 {
		t.Fatalf("expected 0 hits when cache disabled, got %d", hits)
	}
	if reads != 2 {
		t.Fatalf("expected 2 disk reads when cache disabled, got %d", reads)
	}
}

func TestMappingCache_ConcurrentRace(t *testing.T) {
	ResetMappingCacheForTesting()
	dir := t.TempDir()
	f := filepath.Join(dir, "projects")

	if err := os.WriteFile(f, []byte("100:/export/pvc-1\n"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	var wg sync.WaitGroup
	workers := 10
	iterations := 50

	// Concurrent readers
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				_, _ = ReadProjectsFile(f)
				_, _ = readColonMapping(f)
			}
		}()
	}

	// Concurrent writers
	for i := 0; i < workers/2; i++ {
		wg.Add(1)
		workerID := i
		go func() {
			defer wg.Done()
			for j := 0; j < iterations/5; j++ {
				entry := fmt.Sprintf("%d:/export/pvc-%d-%d\n", 1000+workerID*100+j, workerID, j)
				key := fmt.Sprintf("%d", 1000+workerID*100+j)
				_ = AppendToFile(f, entry, key)
			}
		}()
	}

	wg.Wait()
}
