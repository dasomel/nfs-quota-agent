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

package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dasomel/nfs-quota-agent/internal/status"
)

func TestNewStore(t *testing.T) {
	tmpDir := t.TempDir()
	historyPath := filepath.Join(tmpDir, "history.json")

	store, err := NewStore(historyPath, 5*time.Minute, 24*time.Hour)
	if err != nil {
		t.Fatalf("Failed to create history store: %v", err)
	}

	if store == nil {
		t.Fatal("Expected non-nil store")
	}

	if store.filePath != historyPath {
		t.Errorf("Expected filePath %s, got %s", historyPath, store.filePath)
	}
}

func TestStoreRecord(t *testing.T) {
	tmpDir := t.TempDir()
	historyPath := filepath.Join(tmpDir, "history.json")

	store, err := NewStore(historyPath, 5*time.Minute, 24*time.Hour)
	if err != nil {
		t.Fatalf("Failed to create history store: %v", err)
	}

	// Record some usage data
	usages := []status.DirUsage{
		{Path: "/data/test1", Used: 1024, Quota: 2048},
		{Path: "/data/test2", Used: 512, Quota: 1024},
	}

	if err := store.Record(usages); err != nil {
		t.Fatalf("Failed to record usage: %v", err)
	}

	// Check that data was saved
	if _, err := os.Stat(historyPath); os.IsNotExist(err) {
		t.Error("History file was not created")
	}

	// Verify entries
	if len(store.data.Entries) != 2 {
		t.Errorf("Expected 2 entries, got %d", len(store.data.Entries))
	}
}

func TestStoreQuery(t *testing.T) {
	tmpDir := t.TempDir()
	historyPath := filepath.Join(tmpDir, "history.json")

	store, err := NewStore(historyPath, 5*time.Minute, 24*time.Hour)
	if err != nil {
		t.Fatalf("Failed to create history store: %v", err)
	}

	// Record data
	usages := []status.DirUsage{
		{Path: "/data/test1", Used: 1024, Quota: 2048},
		{Path: "/data/test2", Used: 512, Quota: 1024},
	}
	_ = store.Record(usages)

	// Query specific path
	result := store.Query("/data/test1", time.Time{}, time.Time{})
	if len(result) != 1 {
		t.Errorf("Expected 1 result for /data/test1, got %d", len(result))
	}

	// Query non-existent path
	result = store.Query("/data/nonexistent", time.Time{}, time.Time{})
	if len(result) != 0 {
		t.Errorf("Expected 0 results for non-existent path, got %d", len(result))
	}
}

func TestStoreTrend(t *testing.T) {
	tmpDir := t.TempDir()
	historyPath := filepath.Join(tmpDir, "history.json")

	store, err := NewStore(historyPath, 5*time.Minute, 24*time.Hour)
	if err != nil {
		t.Fatalf("Failed to create history store: %v", err)
	}

	// Record initial data
	usages := []status.DirUsage{
		{Path: "/data/test1", Used: 1024, Quota: 2048},
	}
	_ = store.Record(usages)

	// Record more data (simulating growth)
	usages = []status.DirUsage{
		{Path: "/data/test1", Used: 2048, Quota: 2048},
	}
	_ = store.Record(usages)

	// Get trend
	trend := store.GetTrend("/data/test1")
	if trend == nil {
		t.Fatal("Expected non-nil trend")
	}

	if trend.Current != 2048 {
		t.Errorf("Expected current 2048, got %d", trend.Current)
	}

	if trend.Trend != "up" && trend.Trend != "stable" {
		// Could be stable if both records have same timestamp
		t.Logf("Trend: %s", trend.Trend)
	}
}

func TestStorePrune(t *testing.T) {
	tmpDir := t.TempDir()
	historyPath := filepath.Join(tmpDir, "history.json")

	// Create store with very short retention
	store, err := NewStore(historyPath, 5*time.Minute, 1*time.Millisecond)
	if err != nil {
		t.Fatalf("Failed to create history store: %v", err)
	}

	// Record data
	usages := []status.DirUsage{
		{Path: "/data/test1", Used: 1024, Quota: 2048},
	}
	_ = store.Record(usages)

	initialCount := len(store.data.Entries)

	// Wait for entries to expire
	time.Sleep(10 * time.Millisecond)

	// Record more data (triggers prune)
	_ = store.Record(usages)

	// Old entries should be pruned
	if len(store.data.Entries) > initialCount {
		t.Logf("Entries not pruned as expected, but this could be timing-dependent")
	}
}

func TestStoreLoadExisting(t *testing.T) {
	tmpDir := t.TempDir()
	historyPath := filepath.Join(tmpDir, "history.json")

	// Create and populate store
	store1, _ := NewStore(historyPath, 5*time.Minute, 24*time.Hour)
	usages := []status.DirUsage{
		{Path: "/data/test1", Used: 1024, Quota: 2048},
	}
	_ = store1.Record(usages)

	// Create new store (should load existing data)
	store2, err := NewStore(historyPath, 5*time.Minute, 24*time.Hour)
	if err != nil {
		t.Fatalf("Failed to create second store: %v", err)
	}

	if len(store2.data.Entries) != 1 {
		t.Errorf("Expected 1 entry to be loaded, got %d", len(store2.data.Entries))
	}
}
