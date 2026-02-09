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
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/dasomel/nfs-quota-agent/internal/status"
	"github.com/dasomel/nfs-quota-agent/internal/util"
)

// UsageHistory represents a single usage snapshot
type UsageHistory struct {
	Timestamp time.Time `json:"timestamp"`
	Path      string    `json:"path"`
	DirName   string    `json:"dirName"`
	Used      uint64    `json:"used"`
	Quota     uint64    `json:"quota"`
	UsedPct   float64   `json:"usedPct"`
}

// Data stores all history entries
type Data struct {
	Entries []UsageHistory `json:"entries"`
}

// TrendData represents usage trend for a path
type TrendData struct {
	Path       string         `json:"path"`
	DirName    string         `json:"dirName"`
	Current    uint64         `json:"current"`
	CurrentStr string         `json:"currentStr"`
	Quota      uint64         `json:"quota"`
	QuotaStr   string         `json:"quotaStr"`
	Change24h  int64          `json:"change24h"`
	Change7d   int64          `json:"change7d"`
	Change30d  int64          `json:"change30d"`
	Trend      string         `json:"trend"` // "up", "down", "stable"
	History    []UsageHistory `json:"history"`
}

// Store manages usage history storage
type Store struct {
	filePath   string
	interval   time.Duration
	retention  time.Duration
	maxEntries int
	data       Data
	mu         sync.RWMutex
}

// NewStore creates a new history store
func NewStore(filePath string, interval, retention time.Duration) (*Store, error) {
	// Ensure directory exists
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	store := &Store{
		filePath:   filePath,
		interval:   interval,
		retention:  retention,
		maxEntries: 100000, // Max entries to prevent unbounded growth
		data:       Data{Entries: []UsageHistory{}},
	}

	// Load existing data
	if err := store.load(); err != nil {
		slog.Warn("Failed to load existing history", "error", err)
	}

	return store, nil
}

// Interval returns the collection interval
func (h *Store) Interval() time.Duration {
	return h.interval
}

// load reads history from file
func (h *Store) load() error {
	data, err := os.ReadFile(h.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if err := json.Unmarshal(data, &h.data); err != nil {
		return err
	}

	slog.Info("Loaded history data", "entries", len(h.data.Entries))
	return nil
}

// save writes history to file
func (h *Store) save() error {
	h.mu.RLock()
	data, err := json.Marshal(h.data)
	h.mu.RUnlock()

	if err != nil {
		return err
	}

	// Write to temp file first
	tmpPath := h.filePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}

	// Atomic rename
	return os.Rename(tmpPath, h.filePath)
}

// Record records current usage snapshot
func (h *Store) Record(usages []status.DirUsage) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := time.Now()

	// Add new entries
	for _, u := range usages {
		entry := UsageHistory{
			Timestamp: now,
			Path:      u.Path,
			DirName:   filepath.Base(u.Path),
			Used:      u.Used,
			Quota:     u.Quota,
			UsedPct:   u.QuotaPct,
		}
		h.data.Entries = append(h.data.Entries, entry)
	}

	// Prune old entries
	h.prune()

	// Save to disk
	h.mu.Unlock()
	err := h.save()
	h.mu.Lock()

	return err
}

// prune removes old entries (must be called with lock held)
func (h *Store) prune() {
	cutoff := time.Now().Add(-h.retention)

	// Filter entries within retention period
	var kept []UsageHistory
	for _, e := range h.data.Entries {
		if e.Timestamp.After(cutoff) {
			kept = append(kept, e)
		}
	}

	// Also limit total entries
	if len(kept) > h.maxEntries {
		kept = kept[len(kept)-h.maxEntries:]
	}

	h.data.Entries = kept
}

// Query returns history for a specific path
func (h *Store) Query(path string, start, end time.Time) []UsageHistory {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var result []UsageHistory
	for _, e := range h.data.Entries {
		if e.Path == path {
			if !start.IsZero() && e.Timestamp.Before(start) {
				continue
			}
			if !end.IsZero() && e.Timestamp.After(end) {
				continue
			}
			result = append(result, e)
		}
	}

	// Sort by timestamp
	sort.Slice(result, func(i, j int) bool {
		return result[i].Timestamp.Before(result[j].Timestamp)
	})

	return result
}

// GetTrend calculates usage trend for a path
func (h *Store) GetTrend(path string) *TrendData {
	h.mu.RLock()
	defer h.mu.RUnlock()

	now := time.Now()
	history := h.Query(path, now.Add(-30*24*time.Hour), now)

	if len(history) == 0 {
		return nil
	}

	current := history[len(history)-1]

	trend := &TrendData{
		Path:       path,
		DirName:    current.DirName,
		Current:    current.Used,
		CurrentStr: util.FormatBytes(int64(current.Used)),
		Quota:      current.Quota,
		QuotaStr:   util.FormatBytes(int64(current.Quota)),
		History:    history,
	}

	// Calculate changes
	trend.Change24h = h.calculateChange(history, now.Add(-24*time.Hour))
	trend.Change7d = h.calculateChange(history, now.Add(-7*24*time.Hour))
	trend.Change30d = h.calculateChange(history, now.Add(-30*24*time.Hour))

	// Determine trend direction
	if trend.Change24h > 0 {
		trend.Trend = "up"
	} else if trend.Change24h < 0 {
		trend.Trend = "down"
	} else {
		trend.Trend = "stable"
	}

	return trend
}

// calculateChange calculates usage change since a point in time
func (h *Store) calculateChange(history []UsageHistory, since time.Time) int64 {
	if len(history) == 0 {
		return 0
	}

	current := history[len(history)-1].Used

	// Find entry closest to 'since'
	var oldEntry *UsageHistory
	for i := range history {
		if history[i].Timestamp.After(since) {
			if i > 0 {
				oldEntry = &history[i-1]
			} else {
				oldEntry = &history[i]
			}
			break
		}
	}

	if oldEntry == nil {
		// No data that old, use oldest available
		oldEntry = &history[0]
	}

	return int64(current) - int64(oldEntry.Used)
}

// GetAllTrends returns trends for all tracked paths
func (h *Store) GetAllTrends() []TrendData {
	h.mu.RLock()
	defer h.mu.RUnlock()

	// Get unique paths
	pathSet := make(map[string]bool)
	for _, e := range h.data.Entries {
		pathSet[e.Path] = true
	}

	var trends []TrendData
	for path := range pathSet {
		if trend := h.GetTrend(path); trend != nil {
			trends = append(trends, *trend)
		}
	}

	// Sort by current usage descending
	sort.Slice(trends, func(i, j int) bool {
		return trends[i].Current > trends[j].Current
	})

	return trends
}

// GetHistoryStats returns statistics about stored history
func (h *Store) GetHistoryStats() map[string]interface{} {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if len(h.data.Entries) == 0 {
		return map[string]interface{}{
			"entries":   0,
			"paths":     0,
			"oldestStr": "-",
			"newestStr": "-",
		}
	}

	// Get unique paths
	pathSet := make(map[string]bool)
	oldest := h.data.Entries[0].Timestamp
	newest := h.data.Entries[0].Timestamp

	for _, e := range h.data.Entries {
		pathSet[e.Path] = true
		if e.Timestamp.Before(oldest) {
			oldest = e.Timestamp
		}
		if e.Timestamp.After(newest) {
			newest = e.Timestamp
		}
	}

	return map[string]interface{}{
		"entries":   len(h.data.Entries),
		"paths":     len(pathSet),
		"oldest":    oldest,
		"newest":    newest,
		"oldestStr": oldest.Format(time.RFC3339),
		"newestStr": newest.Format(time.RFC3339),
		"retention": h.retention.String(),
		"interval":  h.interval.String(),
	}
}
