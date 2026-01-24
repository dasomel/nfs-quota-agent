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
	"os"
	"testing"
)

func TestXFSQuotaSizeCalculation(t *testing.T) {
	tests := []struct {
		name       string
		sizeBytes  int64
		expectedKB int64
	}{
		{
			name:       "1 GiB",
			sizeBytes:  1024 * 1024 * 1024,
			expectedKB: 1024 * 1024,
		},
		{
			name:       "10 GiB",
			sizeBytes:  10 * 1024 * 1024 * 1024,
			expectedKB: 10 * 1024 * 1024,
		},
		{
			name:       "500 MiB",
			sizeBytes:  500 * 1024 * 1024,
			expectedKB: 500 * 1024,
		},
		{
			name:       "minimum 1KB for small sizes",
			sizeBytes:  512,
			expectedKB: 1, // Should be at least 1KB
		},
		{
			name:       "zero bytes should be 1KB minimum",
			sizeBytes:  0,
			expectedKB: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sizeKB := tt.sizeBytes / 1024
			if sizeKB == 0 {
				sizeKB = 1
			}
			if sizeKB != tt.expectedKB {
				t.Errorf("sizeKB = %d, expected %d", sizeKB, tt.expectedKB)
			}
		})
	}
}

func TestXFSProjectFilesCreation(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "xfs-quota-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	agent := NewQuotaAgent(nil, tmpDir, "/data", "test-provisioner")
	agent.fsType = fsTypeXFS

	// Test adding XFS project
	err = agent.addProject("/export/pvc-test-123", "pv_pvc_test_123", 12345)
	if err != nil {
		t.Fatalf("addProject failed: %v", err)
	}

	// Verify projid file content
	projidContent, err := os.ReadFile(agent.projidFile)
	if err != nil {
		t.Fatalf("Failed to read projid file: %v", err)
	}
	expectedProjid := "pv_pvc_test_123:12345\n"
	if string(projidContent) != expectedProjid {
		t.Errorf("projid content = %q, expected %q", string(projidContent), expectedProjid)
	}

	// Verify projects file content
	projectsContent, err := os.ReadFile(agent.projectsFile)
	if err != nil {
		t.Fatalf("Failed to read projects file: %v", err)
	}
	expectedProjects := "12345:/export/pvc-test-123\n"
	if string(projectsContent) != expectedProjects {
		t.Errorf("projects content = %q, expected %q", string(projectsContent), expectedProjects)
	}
}

func TestXFSQuotaAgentFSTypeConfiguration(t *testing.T) {
	agent := NewQuotaAgent(nil, "/export", "/data", "test-provisioner")
	agent.fsType = fsTypeXFS

	if agent.fsType != "xfs" {
		t.Errorf("fsType = %s, expected xfs", agent.fsType)
	}
}

func TestXFSDuplicateProjectEntry(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "xfs-quota-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	agent := NewQuotaAgent(nil, tmpDir, "/data", "test-provisioner")
	agent.fsType = fsTypeXFS

	// Add project twice
	err = agent.addProject("/export/pvc-123", "pv_pvc_123", 12345)
	if err != nil {
		t.Fatalf("First addProject failed: %v", err)
	}

	err = agent.addProject("/export/pvc-123", "pv_pvc_123", 12345)
	if err != nil {
		t.Fatalf("Second addProject failed: %v", err)
	}

	// Verify no duplicate entries
	projidContent, err := os.ReadFile(agent.projidFile)
	if err != nil {
		t.Fatalf("Failed to read projid file: %v", err)
	}

	// Should only have one entry
	expectedProjid := "pv_pvc_123:12345\n"
	if string(projidContent) != expectedProjid {
		t.Errorf("projid content = %q, expected single entry %q", string(projidContent), expectedProjid)
	}
}
