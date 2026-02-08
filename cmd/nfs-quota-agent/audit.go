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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// AuditAction represents the type of quota action
type AuditAction string

const (
	AuditActionCreate  AuditAction = "CREATE"
	AuditActionUpdate  AuditAction = "UPDATE"
	AuditActionDelete  AuditAction = "DELETE"
	AuditActionCleanup AuditAction = "CLEANUP"
)

// AuditEntry represents a single audit log entry
type AuditEntry struct {
	Timestamp   time.Time   `json:"timestamp"`
	Action      AuditAction `json:"action"`
	PVName      string      `json:"pv_name,omitempty"`
	Namespace   string      `json:"namespace,omitempty"`
	PVCName     string      `json:"pvc_name,omitempty"`
	Path        string      `json:"path"`
	ProjectID   uint32      `json:"project_id,omitempty"`
	ProjectName string      `json:"project_name,omitempty"`
	OldQuota    int64       `json:"old_quota_bytes,omitempty"`
	NewQuota    int64       `json:"new_quota_bytes,omitempty"`
	FSType      string      `json:"fs_type,omitempty"`
	Success  bool   `json:"success"`
	Error    string `json:"error,omitempty"`
	NodeName string `json:"node_name,omitempty"`
	AgentID  string `json:"agent_id,omitempty"`
}

// AuditLogger handles audit logging
type AuditLogger struct {
	mu          sync.Mutex
	writer      io.Writer
	file        *os.File
	filePath    string
	nodeName    string
	agentID     string
	maxFileSize int64
	enabled     bool
}

// AuditConfig holds audit logger configuration
type AuditConfig struct {
	Enabled     bool
	FilePath    string
	MaxFileSize int64 // Max file size in bytes before rotation
	NodeName    string
	AgentID     string
}

// DefaultAuditConfig returns default audit configuration
func DefaultAuditConfig() AuditConfig {
	hostname, _ := os.Hostname()
	return AuditConfig{
		Enabled:     true,
		FilePath:    "/var/log/nfs-quota-agent/audit.log",
		MaxFileSize: 100 * 1024 * 1024, // 100MB
		NodeName:    hostname,
		AgentID:     fmt.Sprintf("agent-%d", os.Getpid()),
	}
}

// NewAuditLogger creates a new audit logger
func NewAuditLogger(config AuditConfig) (*AuditLogger, error) {
	logger := &AuditLogger{
		filePath:    config.FilePath,
		nodeName:    config.NodeName,
		agentID:     config.AgentID,
		maxFileSize: config.MaxFileSize,
		enabled:     config.Enabled,
	}

	if !config.Enabled {
		logger.writer = io.Discard
		return logger, nil
	}

	// Create directory if not exists
	dir := filepath.Dir(config.FilePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create audit log directory: %w", err)
	}

	// Open or create audit log file
	file, err := os.OpenFile(config.FilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open audit log file: %w", err)
	}

	logger.file = file
	logger.writer = file

	return logger, nil
}

// Log writes an audit entry
func (l *AuditLogger) Log(entry AuditEntry) error {
	if !l.enabled {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// Set common fields
	entry.Timestamp = time.Now().UTC()
	entry.NodeName = l.nodeName
	entry.AgentID = l.agentID

	// Encode to JSON
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("failed to marshal audit entry: %w", err)
	}

	// Check if rotation is needed
	if l.file != nil {
		if err := l.rotateIfNeeded(); err != nil {
			// Log rotation error but continue
			fmt.Fprintf(os.Stderr, "Warning: audit log rotation failed: %v\n", err)
		}
	}

	// Write entry
	if _, err := l.writer.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("failed to write audit entry: %w", err)
	}

	return nil
}

// LogQuotaCreate logs quota creation
func (l *AuditLogger) LogQuotaCreate(pvName, namespace, pvcName, path, projectName string, projectID uint32, quotaBytes int64, fsType string, err error) {
	entry := AuditEntry{
		Action:      AuditActionCreate,
		PVName:      pvName,
		Namespace:   namespace,
		PVCName:     pvcName,
		Path:        path,
		ProjectID:   projectID,
		ProjectName: projectName,
		NewQuota:    quotaBytes,
		FSType:      fsType,
		Success:     err == nil,
	}
	if err != nil {
		entry.Error = err.Error()
	}
	_ = l.Log(entry)
}

// LogQuotaUpdate logs quota update
func (l *AuditLogger) LogQuotaUpdate(pvName, path, projectName string, projectID uint32, oldQuota, newQuota int64, fsType string, err error) {
	entry := AuditEntry{
		Action:      AuditActionUpdate,
		PVName:      pvName,
		Path:        path,
		ProjectID:   projectID,
		ProjectName: projectName,
		OldQuota:    oldQuota,
		NewQuota:    newQuota,
		FSType:      fsType,
		Success:     err == nil,
	}
	if err != nil {
		entry.Error = err.Error()
	}
	_ = l.Log(entry)
}

// LogQuotaDelete logs quota deletion
func (l *AuditLogger) LogQuotaDelete(pvName, path, projectName string, projectID uint32, err error) {
	entry := AuditEntry{
		Action:      AuditActionDelete,
		PVName:      pvName,
		Path:        path,
		ProjectID:   projectID,
		ProjectName: projectName,
		Success:     err == nil,
	}
	if err != nil {
		entry.Error = err.Error()
	}
	_ = l.Log(entry)
}

// LogCleanup logs cleanup operation
func (l *AuditLogger) LogCleanup(path, projectName string, projectID uint32, err error) {
	entry := AuditEntry{
		Action:      AuditActionCleanup,
		Path:        path,
		ProjectID:   projectID,
		ProjectName: projectName,
		Success:     err == nil,
	}
	if err != nil {
		entry.Error = err.Error()
	}
	_ = l.Log(entry)
}

// rotateIfNeeded rotates the log file if it exceeds max size
func (l *AuditLogger) rotateIfNeeded() error {
	if l.file == nil || l.maxFileSize <= 0 {
		return nil
	}

	info, err := l.file.Stat()
	if err != nil {
		return err
	}

	if info.Size() < l.maxFileSize {
		return nil
	}

	// Close current file
	l.file.Close()

	// Rotate file
	timestamp := time.Now().Format("20060102-150405")
	rotatedPath := fmt.Sprintf("%s.%s", l.filePath, timestamp)
	if err := os.Rename(l.filePath, rotatedPath); err != nil {
		return err
	}

	// Open new file
	file, err := os.OpenFile(l.filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	l.file = file
	l.writer = file

	return nil
}

// Close closes the audit logger
func (l *AuditLogger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file != nil {
		return l.file.Close()
	}
	return nil
}

// QueryAuditLog queries the audit log file
func QueryAuditLog(filePath string, filter AuditFilter) ([]AuditEntry, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var entries []AuditEntry
	decoder := json.NewDecoder(file)

	for {
		var entry AuditEntry
		if err := decoder.Decode(&entry); err != nil {
			if err == io.EOF {
				break
			}
			continue // Skip malformed entries
		}

		if filter.Matches(entry) {
			entries = append(entries, entry)
		}
	}

	return entries, nil
}

// AuditFilter filters audit entries
type AuditFilter struct {
	Action    AuditAction
	PVName    string
	Namespace string
	Path      string
	StartTime time.Time
	EndTime   time.Time
	OnlyFails bool
}

// Matches checks if an entry matches the filter
func (f AuditFilter) Matches(entry AuditEntry) bool {
	if f.Action != "" && entry.Action != f.Action {
		return false
	}
	if f.PVName != "" && entry.PVName != f.PVName {
		return false
	}
	if f.Namespace != "" && entry.Namespace != f.Namespace {
		return false
	}
	if f.Path != "" && entry.Path != f.Path {
		return false
	}
	if !f.StartTime.IsZero() && entry.Timestamp.Before(f.StartTime) {
		return false
	}
	if !f.EndTime.IsZero() && entry.Timestamp.After(f.EndTime) {
		return false
	}
	if f.OnlyFails && entry.Success {
		return false
	}
	return true
}

// PrintAuditEntries prints audit entries in a formatted table
func PrintAuditEntries(entries []AuditEntry, format string) {
	switch format {
	case "json":
		for _, entry := range entries {
			data, _ := json.Marshal(entry)
			fmt.Println(string(data))
		}
	case "table":
		fmt.Printf("%-20s %-8s %-30s %-40s %-10s %s\n",
			"TIMESTAMP", "ACTION", "PV_NAME", "PATH", "STATUS", "QUOTA")
		fmt.Println(strings.Repeat("-", 120))

		for _, entry := range entries {
			status := "OK"
			if !entry.Success {
				status = "FAIL"
			}
			quota := ""
			if entry.NewQuota > 0 {
				quota = formatBytes(entry.NewQuota)
			}

			pvName := entry.PVName
			if len(pvName) > 30 {
				pvName = pvName[:27] + "..."
			}
			path := entry.Path
			if len(path) > 40 {
				path = "..." + path[len(path)-37:]
			}

			fmt.Printf("%-20s %-8s %-30s %-40s %-10s %s\n",
				entry.Timestamp.Format("2006-01-02 15:04:05"),
				entry.Action,
				pvName,
				path,
				status,
				quota,
			)
		}
	default:
		for _, entry := range entries {
			fmt.Printf("[%s] %s: %s -> %s (success=%v)\n",
				entry.Timestamp.Format(time.RFC3339),
				entry.Action,
				entry.PVName,
				entry.Path,
				entry.Success,
			)
		}
	}
}
