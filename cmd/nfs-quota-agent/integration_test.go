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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSyncAllQuotasWithFakeClient(t *testing.T) {
	// Create fake client with test PVs
	fakeClient := fake.NewSimpleClientset(
		createTestPV("pv-test-001", "cluster.local/nfs-provisioner", "/data/pv-test-001", 10),
		createTestPV("pv-test-002", "cluster.local/nfs-provisioner", "/data/pv-test-002", 20),
		createTestPV("pv-other", "other-provisioner", "/data/pv-other", 5),
	)

	// Create temp directory for testing
	tmpDir, err := os.MkdirTemp("", "sync-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create directories for PVs
	_ = os.MkdirAll(filepath.Join(tmpDir, "pv-test-001"), 0755)
	_ = os.MkdirAll(filepath.Join(tmpDir, "pv-test-002"), 0755)
	_ = os.MkdirAll(filepath.Join(tmpDir, "pv-other"), 0755)

	agent := NewQuotaAgent(fakeClient, tmpDir, "/data", "cluster.local/nfs-provisioner")

	// List PVs and verify filtering
	ctx := context.Background()
	pvList, err := fakeClient.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to list PVs: %v", err)
	}

	if len(pvList.Items) != 3 {
		t.Errorf("Expected 3 PVs, got %d", len(pvList.Items))
	}

	// Count how many should be processed
	processCount := 0
	for _, pv := range pvList.Items {
		if agent.shouldProcessPV(&pv) {
			processCount++
		}
	}

	if processCount != 2 {
		t.Errorf("Expected 2 PVs to be processed, got %d", processCount)
	}
}

func TestSyncAllQuotasProcessAllNFS(t *testing.T) {
	// Create fake client with test PVs
	fakeClient := fake.NewSimpleClientset(
		createTestPV("pv-test-001", "cluster.local/nfs-provisioner", "/data/pv-test-001", 10),
		createTestPV("pv-test-002", "", "/data/pv-test-002", 20), // No provisioner annotation
		createTestPV("pv-test-003", "other-provisioner", "/data/pv-test-003", 5),
	)

	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "sync-all-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	agent := NewQuotaAgent(fakeClient, tmpDir, "/data", "cluster.local/nfs-provisioner")
	agent.processAllNFS = true

	ctx := context.Background()
	pvList, err := fakeClient.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to list PVs: %v", err)
	}

	// With processAllNFS enabled, all NFS PVs should be processed
	processCount := 0
	for _, pv := range pvList.Items {
		if agent.shouldProcessPV(&pv) {
			processCount++
		}
	}

	if processCount != 3 {
		t.Errorf("Expected all 3 NFS PVs to be processed with processAllNFS, got %d", processCount)
	}
}

func TestEnsureQuotaDirectoryNotExists(t *testing.T) {
	fakeClient := fake.NewSimpleClientset(
		createTestPV("pv-missing-dir", "cluster.local/nfs-provisioner", "/data/missing-dir", 10),
	)

	tmpDir, err := os.MkdirTemp("", "missing-dir-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	agent := NewQuotaAgent(fakeClient, tmpDir, "/data", "cluster.local/nfs-provisioner")

	ctx := context.Background()
	pv, err := fakeClient.CoreV1().PersistentVolumes().Get(ctx, "pv-missing-dir", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get PV: %v", err)
	}

	// Should not error when directory doesn't exist
	err = agent.ensureQuota(ctx, pv)
	if err != nil {
		t.Errorf("ensureQuota should not error for missing directory, got: %v", err)
	}
}

func TestQuotaAlreadyApplied(t *testing.T) {
	fakeClient := fake.NewSimpleClientset(
		createTestPV("pv-already-applied", "cluster.local/nfs-provisioner", "/data/pv-already-applied", 10),
	)

	tmpDir, err := os.MkdirTemp("", "already-applied-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create the directory
	pvDir := filepath.Join(tmpDir, "pv-already-applied")
	os.MkdirAll(pvDir, 0755)

	agent := NewQuotaAgent(fakeClient, tmpDir, "/data", "cluster.local/nfs-provisioner")

	// Pre-populate appliedQuotas
	capacityBytes := int64(10 * 1024 * 1024 * 1024) // 10Gi
	agent.appliedQuotas[pvDir] = capacityBytes

	ctx := context.Background()
	pv, err := fakeClient.CoreV1().PersistentVolumes().Get(ctx, "pv-already-applied", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get PV: %v", err)
	}

	// Should return early without error when quota is already applied
	err = agent.ensureQuota(ctx, pv)
	if err != nil {
		t.Errorf("ensureQuota should return early for already applied quota, got: %v", err)
	}
}

func TestPVCapacityExtraction(t *testing.T) {
	tests := []struct {
		name         string
		capacityGi   int64
		expectedByte int64
	}{
		{
			name:         "1Gi capacity",
			capacityGi:   1,
			expectedByte: 1 * 1024 * 1024 * 1024,
		},
		{
			name:         "10Gi capacity",
			capacityGi:   10,
			expectedByte: 10 * 1024 * 1024 * 1024,
		},
		{
			name:         "100Gi capacity",
			capacityGi:   100,
			expectedByte: 100 * 1024 * 1024 * 1024,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pv := createTestPV("test-pv", "provisioner", "/data/test", tt.capacityGi)
			capacity, ok := pv.Spec.Capacity[v1.ResourceStorage]
			if !ok {
				t.Fatal("PV has no storage capacity")
			}
			if capacity.Value() != tt.expectedByte {
				t.Errorf("Capacity = %d bytes, expected %d bytes", capacity.Value(), tt.expectedByte)
			}
		})
	}
}

func TestAgentContextCancellation(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	tmpDir, err := os.MkdirTemp("", "ctx-cancel-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	agent := NewQuotaAgent(fakeClient, tmpDir, "/data", "test-provisioner")
	agent.syncInterval = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error)
	go func() {
		// This will fail because we can't detect filesystem type in test
		// But we can verify context cancellation works
		done <- agent.Run(ctx)
	}()

	// Cancel context immediately
	cancel()

	select {
	case err := <-done:
		// Agent should stop due to filesystem detection failure or context cancellation
		if err != nil && err != context.Canceled {
			// Expected - can't detect fs type in test environment
			t.Logf("Agent stopped with expected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Agent did not stop within timeout after context cancellation")
	}
}

func TestMixedPVTypes(t *testing.T) {
	// Create various PV types
	nfsPV := createTestPV("nfs-pv", "cluster.local/nfs-provisioner", "/data/nfs", 10)

	hostPathPV := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "hostpath-pv",
		},
		Spec: v1.PersistentVolumeSpec{
			Capacity: v1.ResourceList{
				v1.ResourceStorage: resource.MustParse("10Gi"),
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: "/data/hostpath",
				},
			},
		},
		Status: v1.PersistentVolumeStatus{
			Phase: v1.VolumeBound,
		},
	}

	fakeClient := fake.NewSimpleClientset(nfsPV, hostPathPV)

	tmpDir, err := os.MkdirTemp("", "mixed-pv-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	agent := NewQuotaAgent(fakeClient, tmpDir, "/data", "cluster.local/nfs-provisioner")

	ctx := context.Background()
	pvList, err := fakeClient.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to list PVs: %v", err)
	}

	nfsCount := 0
	for _, pv := range pvList.Items {
		if agent.shouldProcessPV(&pv) {
			nfsCount++
		}
	}

	// Only NFS PV should be processed
	if nfsCount != 1 {
		t.Errorf("Expected only 1 NFS PV to be processed, got %d", nfsCount)
	}
}

func TestUnboundPVNotProcessed(t *testing.T) {
	unboundPV := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "unbound-pv",
			Annotations: map[string]string{
				"pv.kubernetes.io/provisioned-by": "cluster.local/nfs-provisioner",
			},
		},
		Spec: v1.PersistentVolumeSpec{
			Capacity: v1.ResourceList{
				v1.ResourceStorage: resource.MustParse("10Gi"),
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				NFS: &v1.NFSVolumeSource{
					Server: "nfs-server",
					Path:   "/data/unbound",
				},
			},
		},
		Status: v1.PersistentVolumeStatus{
			Phase: v1.VolumeAvailable, // Not bound
		},
	}

	fakeClient := fake.NewSimpleClientset(unboundPV)

	tmpDir, err := os.MkdirTemp("", "unbound-pv-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	agent := NewQuotaAgent(fakeClient, tmpDir, "/data", "cluster.local/nfs-provisioner")

	if agent.shouldProcessPV(unboundPV) {
		t.Error("Unbound PV should not be processed")
	}
}

// Helper function to create test PVs
func createTestPV(name, provisioner, path string, capacityGi int64) *v1.PersistentVolume {
	annotations := make(map[string]string)
	if provisioner != "" {
		annotations["pv.kubernetes.io/provisioned-by"] = provisioner
	}

	return &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: annotations,
		},
		Spec: v1.PersistentVolumeSpec{
			Capacity: v1.ResourceList{
				v1.ResourceStorage: resource.MustParse(fmt.Sprintf("%dGi", capacityGi)),
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				NFS: &v1.NFSVolumeSource{
					Server: "nfs-server",
					Path:   path,
				},
			},
		},
		Status: v1.PersistentVolumeStatus{
			Phase: v1.VolumeBound,
		},
	}
}
