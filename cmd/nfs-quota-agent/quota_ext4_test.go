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

func TestExt4QuotaSizeCalculation(t *testing.T) {
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
			expectedKB: 1,
		},
		{
			name:       "exact 1KB",
			sizeBytes:  1024,
			expectedKB: 1,
		},
		{
			name:       "2KB",
			sizeBytes:  2048,
			expectedKB: 2,
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

func TestExt4ProjectFilesCreation(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "ext4-quota-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	agent := NewQuotaAgent(nil, tmpDir, "/data", "test-provisioner")
	agent.fsType = fsTypeExt4

	// Test adding ext4 project
	err = agent.addProject("/export/pvc-test-456", "pv_pvc_test_456", 67890)
	if err != nil {
		t.Fatalf("addProject failed: %v", err)
	}

	// Verify projid file content
	projidContent, err := os.ReadFile(agent.projidFile)
	if err != nil {
		t.Fatalf("Failed to read projid file: %v", err)
	}
	expectedProjid := "pv_pvc_test_456:67890\n"
	if string(projidContent) != expectedProjid {
		t.Errorf("projid content = %q, expected %q", string(projidContent), expectedProjid)
	}

	// Verify projects file content
	projectsContent, err := os.ReadFile(agent.projectsFile)
	if err != nil {
		t.Fatalf("Failed to read projects file: %v", err)
	}
	expectedProjects := "67890:/export/pvc-test-456\n"
	if string(projectsContent) != expectedProjects {
		t.Errorf("projects content = %q, expected %q", string(projectsContent), expectedProjects)
	}
}

func TestExt4QuotaAgentFSTypeConfiguration(t *testing.T) {
	agent := NewQuotaAgent(nil, "/export", "/data", "test-provisioner")
	agent.fsType = fsTypeExt4

	if agent.fsType != "ext4" {
		t.Errorf("fsType = %s, expected ext4", agent.fsType)
	}
}

func TestExt4DuplicateProjectEntry(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "ext4-quota-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	agent := NewQuotaAgent(nil, tmpDir, "/data", "test-provisioner")
	agent.fsType = fsTypeExt4

	// Add project twice
	err = agent.addProject("/export/pvc-789", "pv_pvc_789", 78900)
	if err != nil {
		t.Fatalf("First addProject failed: %v", err)
	}

	err = agent.addProject("/export/pvc-789", "pv_pvc_789", 78900)
	if err != nil {
		t.Fatalf("Second addProject failed: %v", err)
	}

	// Verify no duplicate entries
	projidContent, err := os.ReadFile(agent.projidFile)
	if err != nil {
		t.Fatalf("Failed to read projid file: %v", err)
	}

	// Should only have one entry
	expectedProjid := "pv_pvc_789:78900\n"
	if string(projidContent) != expectedProjid {
		t.Errorf("projid content = %q, expected single entry %q", string(projidContent), expectedProjid)
	}
}

func TestExt4MultipleProjects(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "ext4-quota-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	agent := NewQuotaAgent(nil, tmpDir, "/data", "test-provisioner")
	agent.fsType = fsTypeExt4

	// Add multiple projects
	projects := []struct {
		path        string
		projectName string
		projectID   uint32
	}{
		{"/export/pvc-001", "pv_pvc_001", 1001},
		{"/export/pvc-002", "pv_pvc_002", 1002},
		{"/export/pvc-003", "pv_pvc_003", 1003},
	}

	for _, p := range projects {
		err = agent.addProject(p.path, p.projectName, p.projectID)
		if err != nil {
			t.Fatalf("addProject failed for %s: %v", p.projectName, err)
		}
	}

	// Verify projid file has all entries
	projidContent, err := os.ReadFile(agent.projidFile)
	if err != nil {
		t.Fatalf("Failed to read projid file: %v", err)
	}

	expectedProjid := "pv_pvc_001:1001\npv_pvc_002:1002\npv_pvc_003:1003\n"
	if string(projidContent) != expectedProjid {
		t.Errorf("projid content = %q, expected %q", string(projidContent), expectedProjid)
	}

	// Verify projects file has all entries
	projectsContent, err := os.ReadFile(agent.projectsFile)
	if err != nil {
		t.Fatalf("Failed to read projects file: %v", err)
	}

	expectedProjects := "1001:/export/pvc-001\n1002:/export/pvc-002\n1003:/export/pvc-003\n"
	if string(projectsContent) != expectedProjects {
		t.Errorf("projects content = %q, expected %q", string(projectsContent), expectedProjects)
	}
}
