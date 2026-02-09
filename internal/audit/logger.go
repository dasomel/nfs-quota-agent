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

package audit

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Logger handles audit logging
type Logger struct {
	mu          sync.Mutex
	writer      io.Writer
	file        *os.File
	filePath    string
	nodeName    string
	agentID     string
	maxFileSize int64
	enabled     bool
}

// Config holds audit logger configuration
type Config struct {
	Enabled     bool
	FilePath    string
	MaxFileSize int64 // Max file size in bytes before rotation
	NodeName    string
	AgentID     string
}

// DefaultConfig returns default audit configuration
func DefaultConfig() Config {
	hostname, _ := os.Hostname()
	return Config{
		Enabled:     true,
		FilePath:    "/var/log/nfs-quota-agent/audit.log",
		MaxFileSize: 100 * 1024 * 1024, // 100MB
		NodeName:    hostname,
		AgentID:     fmt.Sprintf("agent-%d", os.Getpid()),
	}
}

// NewLogger creates a new audit logger
func NewLogger(config Config) (*Logger, error) {
	logger := &Logger{
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
func (l *Logger) Log(entry Entry) error {
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
func (l *Logger) LogQuotaCreate(pvName, namespace, pvcName, path, projectName string, projectID uint32, quotaBytes int64, fsType string, err error) {
	entry := Entry{
		Action:      ActionCreate,
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
func (l *Logger) LogQuotaUpdate(pvName, path, projectName string, projectID uint32, oldQuota, newQuota int64, fsType string, err error) {
	entry := Entry{
		Action:      ActionUpdate,
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
func (l *Logger) LogQuotaDelete(pvName, path, projectName string, projectID uint32, err error) {
	entry := Entry{
		Action:      ActionDelete,
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
func (l *Logger) LogCleanup(path, projectName string, projectID uint32, err error) {
	entry := Entry{
		Action:      ActionCleanup,
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
func (l *Logger) rotateIfNeeded() error {
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
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file != nil {
		return l.file.Close()
	}
	return nil
}
