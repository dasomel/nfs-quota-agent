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

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAuditLogger(t *testing.T) {
	// Create temp directory for audit log
	tmpDir, err := os.MkdirTemp("", "audit-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logPath := filepath.Join(tmpDir, "audit.log")

	config := AuditConfig{
		Enabled:     true,
		FilePath:    logPath,
		MaxFileSize: 10 * 1024 * 1024, // 10MB
		NodeName:    "test-node",
		AgentID:     "test-agent",
	}

	logger, err := NewAuditLogger(config)
	if err != nil {
		t.Fatalf("Failed to create audit logger: %v", err)
	}
	defer logger.Close()

	// Log some entries
	logger.LogQuotaCreate("pv-test-1", "default", "pvc-test-1", "/data/test-1", "project_test_1", 1001, 1024*1024*1024, "xfs", "admin", "nfs.csi.k8s.io", nil)
	logger.LogQuotaUpdate("pv-test-2", "/data/test-2", "project_test_2", 1002, 512*1024*1024, 1024*1024*1024, "xfs", nil)
	logger.LogQuotaDelete("pv-test-3", "/data/test-3", "project_test_3", 1003, nil)

	// Close and verify
	logger.Close()

	// Read and verify log entries
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("Failed to read audit log: %v", err)
	}

	if len(data) == 0 {
		t.Fatal("Audit log is empty")
	}

	// Verify we can parse entries
	lines := 0
	for _, line := range splitLines(data) {
		if len(line) == 0 {
			continue
		}
		var entry AuditEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Errorf("Failed to parse audit entry: %v", err)
			continue
		}
		lines++

		// Verify common fields are set
		if entry.NodeName != "test-node" {
			t.Errorf("Expected NodeName 'test-node', got %s", entry.NodeName)
		}
		if entry.AgentID != "test-agent" {
			t.Errorf("Expected AgentID 'test-agent', got %s", entry.AgentID)
		}
		if entry.Timestamp.IsZero() {
			t.Error("Timestamp should not be zero")
		}
	}

	if lines != 3 {
		t.Errorf("Expected 3 log entries, got %d", lines)
	}
}

func TestAuditLoggerDisabled(t *testing.T) {
	config := AuditConfig{
		Enabled: false,
	}

	logger, err := NewAuditLogger(config)
	if err != nil {
		t.Fatalf("Failed to create disabled audit logger: %v", err)
	}
	defer logger.Close()

	// Should not error when logging to disabled logger
	logger.LogQuotaCreate("pv-test", "ns", "pvc", "/path", "proj", 1001, 1024, "xfs", "system", "", nil)
}

func TestAuditFilter(t *testing.T) {
	tests := []struct {
		name    string
		filter  AuditFilter
		entry   AuditEntry
		matches bool
	}{
		{
			name:    "empty filter matches all",
			filter:  AuditFilter{},
			entry:   AuditEntry{Action: AuditActionCreate},
			matches: true,
		},
		{
			name:    "action filter matches",
			filter:  AuditFilter{Action: AuditActionCreate},
			entry:   AuditEntry{Action: AuditActionCreate},
			matches: true,
		},
		{
			name:    "action filter does not match",
			filter:  AuditFilter{Action: AuditActionCreate},
			entry:   AuditEntry{Action: AuditActionDelete},
			matches: false,
		},
		{
			name:    "pv name filter matches",
			filter:  AuditFilter{PVName: "pv-test"},
			entry:   AuditEntry{PVName: "pv-test"},
			matches: true,
		},
		{
			name:    "pv name filter does not match",
			filter:  AuditFilter{PVName: "pv-test"},
			entry:   AuditEntry{PVName: "pv-other"},
			matches: false,
		},
		{
			name:    "fails only filter - success entry",
			filter:  AuditFilter{OnlyFails: true},
			entry:   AuditEntry{Success: true},
			matches: false,
		},
		{
			name:    "fails only filter - failed entry",
			filter:  AuditFilter{OnlyFails: true},
			entry:   AuditEntry{Success: false},
			matches: true,
		},
		{
			name:    "time range filter - within range",
			filter:  AuditFilter{StartTime: time.Now().Add(-1 * time.Hour), EndTime: time.Now().Add(1 * time.Hour)},
			entry:   AuditEntry{Timestamp: time.Now()},
			matches: true,
		},
		{
			name:    "time range filter - before start",
			filter:  AuditFilter{StartTime: time.Now()},
			entry:   AuditEntry{Timestamp: time.Now().Add(-1 * time.Hour)},
			matches: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.filter.Matches(tt.entry); got != tt.matches {
				t.Errorf("AuditFilter.Matches() = %v, want %v", got, tt.matches)
			}
		})
	}
}

func TestQueryAuditLog(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "audit-query-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logPath := filepath.Join(tmpDir, "audit.log")

	// Create logger and write entries
	config := AuditConfig{
		Enabled:  true,
		FilePath: logPath,
		NodeName: "test-node",
		AgentID:  "test-agent",
	}

	logger, err := NewAuditLogger(config)
	if err != nil {
		t.Fatalf("Failed to create audit logger: %v", err)
	}

	logger.LogQuotaCreate("pv-1", "ns-1", "pvc-1", "/data/1", "proj_1", 1001, 1024, "xfs", "user1", "nfs.csi.k8s.io", nil)
	logger.LogQuotaCreate("pv-2", "ns-2", "pvc-2", "/data/2", "proj_2", 1002, 2048, "xfs", "user2", "nfs.csi.k8s.io", nil)
	logger.LogQuotaDelete("pv-3", "/data/3", "proj_3", 1003, nil)
	logger.Close()

	// Query all entries
	entries, err := QueryAuditLog(logPath, AuditFilter{})
	if err != nil {
		t.Fatalf("Failed to query audit log: %v", err)
	}

	if len(entries) != 3 {
		t.Errorf("Expected 3 entries, got %d", len(entries))
	}

	// Query with action filter
	entries, err = QueryAuditLog(logPath, AuditFilter{Action: AuditActionCreate})
	if err != nil {
		t.Fatalf("Failed to query audit log: %v", err)
	}

	if len(entries) != 2 {
		t.Errorf("Expected 2 CREATE entries, got %d", len(entries))
	}

	// Query with namespace filter
	entries, err = QueryAuditLog(logPath, AuditFilter{Namespace: "ns-1"})
	if err != nil {
		t.Fatalf("Failed to query audit log: %v", err)
	}

	if len(entries) != 1 {
		t.Errorf("Expected 1 entry for ns-1, got %d", len(entries))
	}
}

func splitLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			lines = append(lines, data[start:i])
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}
