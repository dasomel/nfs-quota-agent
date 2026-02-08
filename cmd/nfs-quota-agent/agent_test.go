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
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNewQuotaAgent(t *testing.T) {
	agent := NewQuotaAgent(nil, "/export", "/data", "test-provisioner")

	if agent.nfsBasePath != "/export" {
		t.Errorf("nfsBasePath = %s, expected /export", agent.nfsBasePath)
	}
	if agent.nfsServerPath != "/data" {
		t.Errorf("nfsServerPath = %s, expected /data", agent.nfsServerPath)
	}
	if agent.provisionerName != "test-provisioner" {
		t.Errorf("provisionerName = %s, expected test-provisioner", agent.provisionerName)
	}
	if agent.quotaPath != "/export" {
		t.Errorf("quotaPath = %s, expected /export", agent.quotaPath)
	}
	if agent.projectsFile != "/etc/projects" {
		t.Errorf("projectsFile = %s, expected /etc/projects", agent.projectsFile)
	}
	if agent.projidFile != "/etc/projid" {
		t.Errorf("projidFile = %s, expected /etc/projid", agent.projidFile)
	}
	if agent.appliedQuotas == nil {
		t.Error("appliedQuotas should be initialized")
	}
}

func TestNfsPathToLocal(t *testing.T) {
	agent := NewQuotaAgent(nil, "/export", "/data", "test")

	tests := []struct {
		name     string
		nfsPath  string
		expected string
	}{
		{
			name:     "standard path conversion",
			nfsPath:  "/data/namespace-pvc-xxx",
			expected: "/export/namespace-pvc-xxx",
		},
		{
			name:     "nested path conversion",
			nfsPath:  "/data/subdir/pvc-123",
			expected: "/export/subdir/pvc-123",
		},
		{
			name:     "path without server prefix",
			nfsPath:  "/other/path/volume",
			expected: "/export/volume",
		},
		{
			name:     "root data path",
			nfsPath:  "/data",
			expected: "/export",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := agent.nfsPathToLocal(tt.nfsPath)
			if result != tt.expected {
				t.Errorf("nfsPathToLocal(%s) = %s, expected %s", tt.nfsPath, result, tt.expected)
			}
		})
	}
}

func TestGetProjectName(t *testing.T) {
	agent := NewQuotaAgent(nil, "/export", "/data", "test")

	tests := []struct {
		name     string
		pv       *v1.PersistentVolume
		expected string
	}{
		{
			name: "with custom annotation",
			pv: &v1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: "pvc-12345",
					Annotations: map[string]string{
						"nfs.io/project-name": "custom-project",
					},
				},
			},
			expected: "custom-project",
		},
		{
			name: "without annotation - short name",
			pv: &v1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: "pvc-12345",
				},
			},
			expected: "pv_pvc_12345",
		},
		{
			name: "without annotation - long name truncated",
			pv: &v1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: "pvc-very-long-name-that-exceeds-thirty-two-characters",
				},
			},
			expected: "pv_pvc_very_long_name_that_exceeds_",
		},
		{
			name: "with empty annotation",
			pv: &v1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: "pvc-test",
					Annotations: map[string]string{
						"nfs.io/project-name": "",
					},
				},
			},
			expected: "pv_pvc_test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := agent.getProjectName(tt.pv)
			if result != tt.expected {
				t.Errorf("getProjectName() = %s, expected %s", result, tt.expected)
			}
		})
	}
}

func TestGenerateProjectID(t *testing.T) {
	agent := NewQuotaAgent(nil, "/export", "/data", "test")

	// Test deterministic generation
	id1 := agent.generateProjectID("test-project")
	id2 := agent.generateProjectID("test-project")
	if id1 != id2 {
		t.Errorf("generateProjectID should be deterministic: %d != %d", id1, id2)
	}

	// Test different inputs produce different IDs
	id3 := agent.generateProjectID("another-project")
	if id1 == id3 {
		t.Errorf("different inputs should produce different IDs: %d == %d", id1, id3)
	}

	// Test ID is in valid range (1-4294967294)
	if id1 < 1 || id1 > 4294967294 {
		t.Errorf("project ID out of range: %d", id1)
	}
}

func TestShouldProcessPV(t *testing.T) {
	agent := NewQuotaAgent(nil, "/export", "/data", "cluster.local/nfs-provisioner")

	tests := []struct {
		name     string
		pv       *v1.PersistentVolume
		expected bool
	}{
		{
			name: "non-NFS PV",
			pv: &v1.PersistentVolume{
				Spec: v1.PersistentVolumeSpec{
					// No NFS field
				},
				Status: v1.PersistentVolumeStatus{
					Phase: v1.VolumeBound,
				},
			},
			expected: false,
		},
		{
			name: "NFS PV not bound",
			pv: &v1.PersistentVolume{
				Spec: v1.PersistentVolumeSpec{
					PersistentVolumeSource: v1.PersistentVolumeSource{
						NFS: &v1.NFSVolumeSource{
							Server: "nfs-server",
							Path:   "/data/pvc",
						},
					},
				},
				Status: v1.PersistentVolumeStatus{
					Phase: v1.VolumePending,
				},
			},
			expected: false,
		},
		{
			name: "NFS PV bound with matching provisioner",
			pv: &v1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"pv.kubernetes.io/provisioned-by": "cluster.local/nfs-provisioner",
					},
				},
				Spec: v1.PersistentVolumeSpec{
					PersistentVolumeSource: v1.PersistentVolumeSource{
						NFS: &v1.NFSVolumeSource{
							Server: "nfs-server",
							Path:   "/data/pvc",
						},
					},
				},
				Status: v1.PersistentVolumeStatus{
					Phase: v1.VolumeBound,
				},
			},
			expected: true,
		},
		{
			name: "NFS PV bound with different provisioner",
			pv: &v1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"pv.kubernetes.io/provisioned-by": "other-provisioner",
					},
				},
				Spec: v1.PersistentVolumeSpec{
					PersistentVolumeSource: v1.PersistentVolumeSource{
						NFS: &v1.NFSVolumeSource{
							Server: "nfs-server",
							Path:   "/data/pvc",
						},
					},
				},
				Status: v1.PersistentVolumeStatus{
					Phase: v1.VolumeBound,
				},
			},
			expected: false,
		},
		{
			name: "NFS PV bound without annotations",
			pv: &v1.PersistentVolume{
				Spec: v1.PersistentVolumeSpec{
					PersistentVolumeSource: v1.PersistentVolumeSource{
						NFS: &v1.NFSVolumeSource{
							Server: "nfs-server",
							Path:   "/data/pvc",
						},
					},
				},
				Status: v1.PersistentVolumeStatus{
					Phase: v1.VolumeBound,
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := agent.shouldProcessPV(tt.pv)
			if result != tt.expected {
				t.Errorf("shouldProcessPV() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

func TestShouldProcessPV_ProcessAllNFS(t *testing.T) {
	agent := NewQuotaAgent(nil, "/export", "/data", "cluster.local/nfs-provisioner")
	agent.processAllNFS = true

	// NFS PV without provisioner annotation should be processed when processAllNFS is true
	pv := &v1.PersistentVolume{
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeSource: v1.PersistentVolumeSource{
				NFS: &v1.NFSVolumeSource{
					Server: "nfs-server",
					Path:   "/data/pvc",
				},
			},
		},
		Status: v1.PersistentVolumeStatus{
			Phase: v1.VolumeBound,
		},
	}

	if !agent.shouldProcessPV(pv) {
		t.Error("shouldProcessPV() should return true when processAllNFS is enabled")
	}
}

func TestAppendToFile(t *testing.T) {
	agent := NewQuotaAgent(nil, "/export", "/data", "test")

	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "quota-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	testFile := filepath.Join(tmpDir, "test-file")

	// Test creating new file
	err = agent.appendToFile(testFile, "entry1:value1\n", "entry1")
	if err != nil {
		t.Fatalf("appendToFile failed: %v", err)
	}

	// Verify content
	content, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}
	if string(content) != "entry1:value1\n" {
		t.Errorf("File content = %s, expected entry1:value1\\n", string(content))
	}

	// Test appending new entry
	err = agent.appendToFile(testFile, "entry2:value2\n", "entry2")
	if err != nil {
		t.Fatalf("appendToFile failed: %v", err)
	}

	content, err = os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}
	if string(content) != "entry1:value1\nentry2:value2\n" {
		t.Errorf("File content = %s, expected entry1:value1\\nentry2:value2\\n", string(content))
	}

	// Test skipping duplicate entry
	err = agent.appendToFile(testFile, "entry1:newvalue\n", "entry1")
	if err != nil {
		t.Fatalf("appendToFile failed: %v", err)
	}

	content, err = os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}
	// Should not have duplicate
	if string(content) != "entry1:value1\nentry2:value2\n" {
		t.Errorf("File content = %s, should not have duplicate entry1", string(content))
	}
}

func TestAddProject(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "quota-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	agent := NewQuotaAgent(nil, tmpDir, "/data", "test")
	// Override to use temp directory for testing
	agent.projectsFile = filepath.Join(tmpDir, "projects")
	agent.projidFile = filepath.Join(tmpDir, "projid")

	err = agent.addProject("/export/pvc-123", "test_project", 12345)
	if err != nil {
		t.Fatalf("addProject failed: %v", err)
	}

	// Verify projid file
	projidContent, err := os.ReadFile(agent.projidFile)
	if err != nil {
		t.Fatalf("Failed to read projid file: %v", err)
	}
	expectedProjid := "test_project:12345\n"
	if string(projidContent) != expectedProjid {
		t.Errorf("projid content = %s, expected %s", string(projidContent), expectedProjid)
	}

	// Verify projects file
	projectsContent, err := os.ReadFile(agent.projectsFile)
	if err != nil {
		t.Fatalf("Failed to read projects file: %v", err)
	}
	expectedProjects := "12345:/export/pvc-123\n"
	if string(projectsContent) != expectedProjects {
		t.Errorf("projects content = %s, expected %s", string(projectsContent), expectedProjects)
	}
}

func TestLoadProjects(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "quota-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	agent := NewQuotaAgent(nil, tmpDir, "/data", "test")
	// Override to use temp directory for testing
	agent.projectsFile = filepath.Join(tmpDir, "projects")
	agent.projidFile = filepath.Join(tmpDir, "projid")

	// Test loading non-existent file (should not error)
	err = agent.loadProjects()
	if err != nil {
		t.Errorf("loadProjects should not error on non-existent file: %v", err)
	}

	// Create projects file
	projectsContent := "12345:/export/pvc-123\n# comment line\n67890:/export/pvc-456\n"
	err = os.WriteFile(agent.projectsFile, []byte(projectsContent), 0644)
	if err != nil {
		t.Fatalf("Failed to write projects file: %v", err)
	}

	// Test loading existing file
	err = agent.loadProjects()
	if err != nil {
		t.Errorf("loadProjects failed: %v", err)
	}
}

func TestAppliedQuotasTracking(t *testing.T) {
	agent := NewQuotaAgent(nil, "/export", "/data", "test-provisioner")

	// Test initial state
	if len(agent.appliedQuotas) != 0 {
		t.Errorf("appliedQuotas should be empty initially, got %d entries", len(agent.appliedQuotas))
	}

	// Simulate tracking applied quota
	agent.appliedQuotas["/export/pvc-123"] = 1024 * 1024 * 1024     // 1 GiB
	agent.appliedQuotas["/export/pvc-456"] = 5 * 1024 * 1024 * 1024 // 5 GiB

	if len(agent.appliedQuotas) != 2 {
		t.Errorf("appliedQuotas should have 2 entries, got %d", len(agent.appliedQuotas))
	}

	// Test quota lookup
	if quota, exists := agent.appliedQuotas["/export/pvc-123"]; !exists || quota != 1024*1024*1024 {
		t.Errorf("quota for pvc-123 = %d, expected %d", quota, 1024*1024*1024)
	}

	// Test removing quota tracking
	delete(agent.appliedQuotas, "/export/pvc-123")
	if _, exists := agent.appliedQuotas["/export/pvc-123"]; exists {
		t.Error("pvc-123 should be removed from appliedQuotas")
	}
}

func TestSyncIntervalConfiguration(t *testing.T) {
	agent := NewQuotaAgent(nil, "/export", "/data", "test-provisioner")

	// Default sync interval should be 30 seconds
	expectedInterval := 30 * time.Second
	if agent.syncInterval != expectedInterval {
		t.Errorf("syncInterval = %v, expected %v", agent.syncInterval, expectedInterval)
	}

	// Test modifying sync interval
	agent.syncInterval = 60 * time.Second
	if agent.syncInterval != 60*time.Second {
		t.Errorf("syncInterval = %v, expected 60s", agent.syncInterval)
	}
}

func TestProcessAllNFSFlag(t *testing.T) {
	agent := NewQuotaAgent(nil, "/export", "/data", "test-provisioner")

	// Default should be false
	if agent.processAllNFS {
		t.Error("processAllNFS should be false by default")
	}

	// When enabled, should process NFS PVs without provisioner annotation
	agent.processAllNFS = true

	pv := &v1.PersistentVolume{
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeSource: v1.PersistentVolumeSource{
				NFS: &v1.NFSVolumeSource{
					Server: "nfs-server",
					Path:   "/data/pvc",
				},
			},
		},
		Status: v1.PersistentVolumeStatus{
			Phase: v1.VolumeBound,
		},
	}

	if !agent.shouldProcessPV(pv) {
		t.Error("shouldProcessPV should return true when processAllNFS is enabled")
	}
}

func TestFSTypeConstants(t *testing.T) {
	if fsTypeXFS != "xfs" {
		t.Errorf("fsTypeXFS = %s, expected xfs", fsTypeXFS)
	}
	if fsTypeExt4 != "ext4" {
		t.Errorf("fsTypeExt4 = %s, expected ext4", fsTypeExt4)
	}
}

func TestQuotaStatusConstants(t *testing.T) {
	if quotaStatusPending != "pending" {
		t.Errorf("quotaStatusPending = %s, expected pending", quotaStatusPending)
	}
	if quotaStatusApplied != "applied" {
		t.Errorf("quotaStatusApplied = %s, expected applied", quotaStatusApplied)
	}
	if quotaStatusFailed != "failed" {
		t.Errorf("quotaStatusFailed = %s, expected failed", quotaStatusFailed)
	}
}

func TestAnnotationConstants(t *testing.T) {
	if annotationProjectName != "nfs.io/project-name" {
		t.Errorf("annotationProjectName = %s, expected nfs.io/project-name", annotationProjectName)
	}
	if annotationQuotaStatus != "nfs.io/quota-status" {
		t.Errorf("annotationQuotaStatus = %s, expected nfs.io/quota-status", annotationQuotaStatus)
	}
}

func TestProjectIDRange(t *testing.T) {
	agent := NewQuotaAgent(nil, "/export", "/data", "test")

	// Test various project names to ensure IDs are in valid range
	testNames := []string{
		"pv_test",
		"pv_very_long_project_name_that_is_quite_extensive",
		"a",
		"",
		"pv_special_chars_123_456",
		"pv_namespace_pvc_abcdef123456",
	}

	for _, name := range testNames {
		id := agent.generateProjectID(name)
		if id < 1 || id > 4294967294 {
			t.Errorf("generateProjectID(%q) = %d, out of valid range [1, 4294967294]", name, id)
		}
	}
}

func TestProjectIDCollisionResistance(t *testing.T) {
	agent := NewQuotaAgent(nil, "/export", "/data", "test")

	// Generate many project IDs and check for collisions
	ids := make(map[uint32]string)
	collisions := 0

	for i := 0; i < 1000; i++ {
		name := fmt.Sprintf("pv_project_%d", i)
		id := agent.generateProjectID(name)
		if existingName, exists := ids[id]; exists {
			collisions++
			t.Logf("Collision: %q and %q both have ID %d", name, existingName, id)
		}
		ids[id] = name
	}

	// Allow some collisions due to hash nature, but not too many
	if collisions > 10 {
		t.Errorf("Too many collisions: %d out of 1000", collisions)
	}
}

func TestNfsPathToLocalEdgeCases(t *testing.T) {
	agent := NewQuotaAgent(nil, "/export", "/data", "test")

	tests := []struct {
		name     string
		nfsPath  string
		expected string
	}{
		{
			name:     "trailing slash on server path",
			nfsPath:  "/data/",
			expected: "/export",
		},
		{
			name:     "deep nested path",
			nfsPath:  "/data/level1/level2/level3/pvc",
			expected: "/export/level1/level2/level3/pvc",
		},
		{
			name:     "path with special characters",
			nfsPath:  "/data/ns-default-pvc-abc123",
			expected: "/export/ns-default-pvc-abc123",
		},
		{
			name:     "completely different base",
			nfsPath:  "/mnt/nfs/volume",
			expected: "/export/volume",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := agent.nfsPathToLocal(tt.nfsPath)
			if result != tt.expected {
				t.Errorf("nfsPathToLocal(%q) = %q, expected %q", tt.nfsPath, result, tt.expected)
			}
		})
	}
}

func TestGetProjectNameEdgeCases(t *testing.T) {
	agent := NewQuotaAgent(nil, "/export", "/data", "test")

	tests := []struct {
		name     string
		pv       *v1.PersistentVolume
		expected string
	}{
		{
			name: "PV name with multiple dashes",
			pv: &v1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: "pvc-abc-def-ghi-123",
				},
			},
			expected: "pv_pvc_abc_def_ghi_123",
		},
		{
			name: "PV name exactly 32 characters after prefix",
			pv: &v1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: "12345678901234567890123456789012", // 32 chars
				},
			},
			expected: "pv_12345678901234567890123456789012",
		},
		{
			name: "nil annotations",
			pv: &v1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "pvc-test",
					Annotations: nil,
				},
			},
			expected: "pv_pvc_test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := agent.getProjectName(tt.pv)
			if result != tt.expected {
				t.Errorf("getProjectName() = %q, expected %q", result, tt.expected)
			}
		})
	}
}

