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

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dasomel/nfs-quota-agent/internal/audit"
	"github.com/dasomel/nfs-quota-agent/internal/history"
	"github.com/dasomel/nfs-quota-agent/internal/quota"
)

func TestSettersAndGetters(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())

	a.SetProcessAllNFS(true)
	a.SetQuotaPath("/quota")
	a.SetSyncInterval(5 * time.Second)
	logger, err := audit.NewLogger(audit.Config{Enabled: false})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	a.SetAuditLogger(logger)
	a.SetEnableAutoCleanup(true)
	a.SetCleanupIntervalDuration(2 * time.Minute)
	a.SetOrphanGracePeriodDuration(3 * time.Hour)
	a.SetCleanupDryRunFlag(false)
	a.SetEnablePolicy(true)
	a.SetDefaultQuota(1024)
	a.SetEnforceMaxQuota(true)

	if !a.processAllNFS || a.quotaPath != "/quota" || a.syncInterval != 5*time.Second {
		t.Fatalf("basic setters did not apply: %+v", a)
	}
	if a.EnableAutoCleanup() != true || a.CleanupDryRun() != false {
		t.Fatalf("cleanup getters mismatch")
	}
	if a.OrphanGracePeriod() != 3*time.Hour || a.CleanupInterval() != 2*time.Minute {
		t.Fatalf("duration getters mismatch")
	}
	if !a.EnablePolicy() || a.AuditLogger() != logger {
		t.Fatalf("policy/audit getters mismatch")
	}
	if a.BasePath() == "" {
		t.Fatalf("BasePath should return nfsBasePath")
	}
	if got := a.AppliedQuotaCount(); got != 0 {
		t.Fatalf("AppliedQuotaCount = %d, want 0", got)
	}
}

func TestShouldProcessPV(t *testing.T) {
	tests := []struct {
		name string
		pv   *v1.PersistentVolume
		mod  func(a *QuotaAgent)
		want bool
	}{
		{
			name: "not bound",
			pv:   &v1.PersistentVolume{Status: v1.PersistentVolumeStatus{Phase: v1.VolumePending}},
			want: false,
		},
		{
			name: "no NFS or CSI source",
			pv:   &v1.PersistentVolume{Status: v1.PersistentVolumeStatus{Phase: v1.VolumeBound}},
			want: false,
		},
		{
			name: "processAllNFS true takes native NFS regardless of annotation",
			pv: &v1.PersistentVolume{
				Status: v1.PersistentVolumeStatus{Phase: v1.VolumeBound},
				Spec:   v1.PersistentVolumeSpec{PersistentVolumeSource: v1.PersistentVolumeSource{NFS: &v1.NFSVolumeSource{}}},
			},
			mod:  func(a *QuotaAgent) { a.processAllNFS = true },
			want: true,
		},
		{
			name: "CSI matching driver always processed",
			pv: &v1.PersistentVolume{
				Status: v1.PersistentVolumeStatus{Phase: v1.VolumeBound},
				Spec: v1.PersistentVolumeSpec{PersistentVolumeSource: v1.PersistentVolumeSource{
					CSI: &v1.CSIPersistentVolumeSource{Driver: "example.com/nfs"},
				}},
			},
			want: true,
		},
		{
			name: "CSI non-matching driver and no NFS",
			pv: &v1.PersistentVolume{
				Status: v1.PersistentVolumeStatus{Phase: v1.VolumeBound},
				Spec: v1.PersistentVolumeSpec{PersistentVolumeSource: v1.PersistentVolumeSource{
					CSI: &v1.CSIPersistentVolumeSource{Driver: "other/driver"},
				}},
			},
			want: false,
		},
		{
			name: "native NFS without annotations",
			pv: &v1.PersistentVolume{
				Status: v1.PersistentVolumeStatus{Phase: v1.VolumeBound},
				Spec:   v1.PersistentVolumeSpec{PersistentVolumeSource: v1.PersistentVolumeSource{NFS: &v1.NFSVolumeSource{}}},
			},
			want: false,
		},
		{
			name: "native NFS with matching provisioner annotation",
			pv: &v1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"pv.kubernetes.io/provisioned-by": "example.com/nfs"}},
				Status:     v1.PersistentVolumeStatus{Phase: v1.VolumeBound},
				Spec:       v1.PersistentVolumeSpec{PersistentVolumeSource: v1.PersistentVolumeSource{NFS: &v1.NFSVolumeSource{}}},
			},
			want: true,
		},
		{
			name: "native NFS with mismatched provisioner annotation",
			pv: &v1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"pv.kubernetes.io/provisioned-by": "other/provisioner"}},
				Status:     v1.PersistentVolumeStatus{Phase: v1.VolumeBound},
				Spec:       v1.PersistentVolumeSpec{PersistentVolumeSource: v1.PersistentVolumeSource{NFS: &v1.NFSVolumeSource{}}},
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAgent(t, fake.NewSimpleClientset())
			a.provisionerName = "example.com/nfs"
			if tc.mod != nil {
				tc.mod(a)
			}
			if got := a.shouldProcessPV(tc.pv); got != tc.want {
				t.Errorf("shouldProcessPV() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGetNFSPath(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())

	tests := []struct {
		name string
		pv   *v1.PersistentVolume
		want string
	}{
		{
			name: "native NFS",
			pv:   &v1.PersistentVolume{Spec: v1.PersistentVolumeSpec{PersistentVolumeSource: v1.PersistentVolumeSource{NFS: &v1.NFSVolumeSource{Path: "/exports/pvc-1"}}}},
			want: "/exports/pvc-1",
		},
		{
			name: "CSI share+subDir",
			pv: &v1.PersistentVolume{Spec: v1.PersistentVolumeSpec{PersistentVolumeSource: v1.PersistentVolumeSource{CSI: &v1.CSIPersistentVolumeSource{
				VolumeAttributes: map[string]string{"share": "/exports", "subDir": "pvc-2"},
			}}}},
			want: "/exports/pvc-2",
		},
		{
			name: "CSI share+subdir lowercase fallback",
			pv: &v1.PersistentVolume{Spec: v1.PersistentVolumeSpec{PersistentVolumeSource: v1.PersistentVolumeSource{CSI: &v1.CSIPersistentVolumeSource{
				VolumeAttributes: map[string]string{"share": "/exports", "subdir": "pvc-3"},
			}}}},
			want: "/exports/pvc-3",
		},
		{
			name: "CSI share only falls back to PV name",
			pv: &v1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "pvc-4"},
				Spec: v1.PersistentVolumeSpec{PersistentVolumeSource: v1.PersistentVolumeSource{CSI: &v1.CSIPersistentVolumeSource{
					VolumeAttributes: map[string]string{"share": "/exports"},
				}}},
			},
			want: "/exports/pvc-4",
		},
		{
			name: "CSI missing attributes",
			pv:   &v1.PersistentVolume{Spec: v1.PersistentVolumeSpec{PersistentVolumeSource: v1.PersistentVolumeSource{CSI: &v1.CSIPersistentVolumeSource{}}}},
			want: "",
		},
		{
			name: "no source",
			pv:   &v1.PersistentVolume{},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := a.getNFSPath(tc.pv); got != tc.want {
				t.Errorf("getNFSPath() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNfsPathToLocal(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	a.nfsServerPath = "/exports"

	if got := a.nfsPathToLocal("/exports/pvc-1"); got != filepath.Join(a.nfsBasePath, "pvc-1") {
		t.Errorf("prefixed path = %q", got)
	}
	// No matching prefix falls back to basename join.
	if got := a.nfsPathToLocal("/other/deep/pvc-2"); got != filepath.Join(a.nfsBasePath, "pvc-2") {
		t.Errorf("fallback path = %q", got)
	}
}

func TestGetProjectName(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())

	withAnnotation := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1", Annotations: map[string]string{AnnotationProjectName: "custom-name"}},
	}
	if got := a.getProjectName(withAnnotation); got != "custom-name" {
		t.Errorf("getProjectName() = %q, want custom-name", got)
	}

	longName := &v1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a-", 40)}}
	got := a.getProjectName(longName)
	if !strings.HasPrefix(got, "pv_") || len(got) != len("pv_")+32 {
		t.Errorf("getProjectName() truncation = %q (len %d)", got, len(got))
	}

	derived := &v1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "my-pv"}}
	if got := a.getProjectName(derived); got != "pv_my_pv" {
		t.Errorf("getProjectName() = %q, want pv_my_pv", got)
	}
}

func TestHashProjectNameDeterministic(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	h1 := a.hashProjectName("same-name")
	h2 := a.hashProjectName("same-name")
	if h1 != h2 || h1 == 0 {
		t.Fatalf("hashProjectName not deterministic/non-zero: %d vs %d", h1, h2)
	}
	if a.hashProjectName("different-name") == h1 {
		t.Fatalf("different names unexpectedly hashed the same")
	}
}

func TestGenerateProjectIDCollision(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	a.knownProjectIDs = make(map[uint32]string)

	id := a.hashProjectName("proj-a")
	// Force a collision: id already claimed by a different project name.
	a.knownProjectIDs[id] = "someone-else"

	got := a.generateProjectID("proj-a")
	if got == id {
		t.Fatalf("expected generateProjectID to skip the colliding id")
	}
	if a.knownProjectIDs[got] != "proj-a" {
		t.Fatalf("cache not updated for resolved id")
	}

	// Same name again should now return the same cached id without collision handling.
	if got2 := a.generateProjectID("proj-a"); got2 != got {
		t.Fatalf("generateProjectID not stable for same project name: %d vs %d", got2, got)
	}
}

func TestLoadExistingProjectIDs(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())

	content := "# comment\n\nproj_one:5\nproj_two:7\nmalformed-line\n"
	if err := os.WriteFile(a.projidFile, []byte(content), 0644); err != nil {
		t.Fatalf("write projid: %v", err)
	}

	ids := a.loadExistingProjectIDs()
	if ids[5] != "proj_one" || ids[7] != "proj_two" {
		t.Fatalf("unexpected ids map: %+v", ids)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 entries, got %d: %+v", len(ids), ids)
	}

	// Missing file should return an empty map, not an error.
	missing := newTestAgent(t, fake.NewSimpleClientset())
	missing.projidFile = filepath.Join(t.TempDir(), "does-not-exist")
	if ids := missing.loadExistingProjectIDs(); len(ids) != 0 {
		t.Fatalf("expected empty map for missing file, got %+v", ids)
	}
}

func TestLoadProjects(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	content := "1:/data/pvc-1\n2:/data/pvc-2\n"
	if err := os.WriteFile(a.projectsFile, []byte(content), 0644); err != nil {
		t.Fatalf("write projects: %v", err)
	}

	if err := a.loadProjects(); err != nil {
		t.Fatalf("loadProjects: %v", err)
	}
	if _, ok := a.appliedQuotas["/data/pvc-1"]; !ok {
		t.Fatalf("expected /data/pvc-1 to be tracked")
	}
	if _, ok := a.appliedQuotas["/data/pvc-2"]; !ok {
		t.Fatalf("expected /data/pvc-2 to be tracked")
	}

	// Existing entries should not be clobbered.
	a.appliedQuotas["/data/pvc-1"] = 999
	if err := a.loadProjects(); err != nil {
		t.Fatalf("loadProjects (second call): %v", err)
	}
	if a.appliedQuotas["/data/pvc-1"] != 999 {
		t.Fatalf("loadProjects overwrote an already-known path")
	}

	// Error path: projectsFile pointing at a directory causes a real read error.
	dirAgent := newTestAgent(t, fake.NewSimpleClientset())
	dirAgent.projectsFile = t.TempDir()
	if err := dirAgent.loadProjects(); err == nil {
		t.Fatalf("expected error when projectsFile is a directory")
	}
}

func TestDetectFilesystemType(t *testing.T) {
	tests := []struct {
		name      string
		findmnt   string
		wantErr   bool
		wantFSVal string
	}{
		{name: "xfs supported", findmnt: "xfs\n", wantFSVal: quota.FSTypeXFS},
		{name: "ext4 supported", findmnt: "ext4\n", wantFSVal: quota.FSTypeExt4},
		{name: "btrfs supported", findmnt: "btrfs\n", wantFSVal: quota.FSTypeBtrfs},
		{name: "unsupported type", findmnt: "ntfs\n", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
				if name == "findmnt" {
					return []byte(tc.findmnt), nil
				}
				return nil, nil
			}}
			withFakeRunner(t, r)

			a := newTestAgent(t, fake.NewSimpleClientset())
			err := a.detectFilesystemType()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("detectFilesystemType: %v", err)
			}
			if a.fsType != tc.wantFSVal {
				t.Fatalf("fsType = %q, want %q", a.fsType, tc.wantFSVal)
			}
		})
	}
}

func TestCheckQuotaAvailable(t *testing.T) {
	t.Run("xfs success", func(t *testing.T) {
		withFakeRunner(t, xfsHappyRunner())
		a := newTestAgent(t, fake.NewSimpleClientset())
		a.fsType = quota.FSTypeXFS
		if err := a.checkQuotaAvailable(); err != nil {
			t.Fatalf("checkQuotaAvailable: %v", err)
		}
	})

	t.Run("ext4 success", func(t *testing.T) {
		withFakeRunner(t, &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			return []byte(""), nil
		}})
		a := newTestAgent(t, fake.NewSimpleClientset())
		a.fsType = quota.FSTypeExt4
		if err := a.checkQuotaAvailable(); err != nil {
			t.Fatalf("checkQuotaAvailable: %v", err)
		}
	})

	t.Run("btrfs success", func(t *testing.T) {
		withFakeRunner(t, &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			if name == "btrfs" && len(args) > 0 && args[0] == "--version" {
				return []byte("btrfs-progs v6.1"), nil
			}
			if name == "btrfs" && len(args) > 0 && args[0] == "qgroup" && args[1] == "show" {
				return []byte("qgroupid rfer excl max_rfer max_excl path\n"), nil
			}
			return nil, errors.New("unexpected command")
		}})
		a := newTestAgent(t, fake.NewSimpleClientset())
		a.fsType = quota.FSTypeBtrfs
		if err := a.checkQuotaAvailable(); err != nil {
			t.Fatalf("checkQuotaAvailable: %v", err)
		}
	})

	t.Run("unsupported fsType", func(t *testing.T) {
		a := newTestAgent(t, fake.NewSimpleClientset())
		a.fsType = "ntfs"
		if err := a.checkQuotaAvailable(); err == nil {
			t.Fatalf("expected error for unsupported fsType")
		}
	})
}

// ensureQuotaFixture wires an agent + PV + fake client ready for ensureQuota tests.
func ensureQuotaFixture(t *testing.T, capacityGi int64) (*QuotaAgent, *v1.PersistentVolume, *fake.Clientset) {
	t.Helper()
	pv := newBoundPV("pv-1", "/exports/pvc-1", capacityGi)
	client := fake.NewSimpleClientset(pv)
	a := newTestAgent(t, client)
	a.nfsServerPath = "/exports"
	localPath := a.nfsPathToLocal("/exports/pvc-1")
	if err := os.MkdirAll(localPath, 0755); err != nil {
		t.Fatalf("mkdir localPath: %v", err)
	}
	return a, pv, client
}

func TestEnsureQuotaSuccess(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	a, pv, client := ensureQuotaFixture(t, 1)
	a.fsType = quota.FSTypeXFS

	ctx := context.Background()
	if err := a.ensureQuota(ctx, pv); err != nil {
		t.Fatalf("ensureQuota: %v", err)
	}

	localPath := a.nfsPathToLocal("/exports/pvc-1")
	if a.appliedQuotas[localPath] == 0 {
		t.Fatalf("expected appliedQuotas to be recorded")
	}

	updated, err := client.CoreV1().PersistentVolumes().Get(ctx, pv.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pv: %v", err)
	}
	if updated.Annotations[AnnotationQuotaStatus] != QuotaStatusApplied {
		t.Fatalf("expected quota status applied, got %q", updated.Annotations[AnnotationQuotaStatus])
	}

	// Re-running with the same capacity should be a no-op (skip branch).
	callsBefore := 0
	// nothing else to assert directly on calls without exposing the runner; verifying idempotence via error/state is enough.
	if err := a.ensureQuota(ctx, pv); err != nil {
		t.Fatalf("second ensureQuota: %v", err)
	}
	_ = callsBefore
}

func TestEnsureQuotaBtrfsSuccess(t *testing.T) {
	withFakeRunner(t, &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		if name == "btrfs" && args[0] == "subvolume" && args[1] == "show" {
			return []byte("Name: my-subvolume\nUUID: 1234"), nil
		}
		if name == "btrfs" && args[0] == "qgroup" && args[1] == "limit" {
			return []byte(""), nil
		}
		return nil, errors.New("unexpected command")
	}})
	a, pv, client := ensureQuotaFixture(t, 1)
	a.fsType = quota.FSTypeBtrfs

	ctx := context.Background()
	if err := a.ensureQuota(ctx, pv); err != nil {
		t.Fatalf("ensureQuota: %v", err)
	}

	localPath := a.nfsPathToLocal("/exports/pvc-1")
	if a.appliedQuotas[localPath] == 0 {
		t.Fatalf("expected appliedQuotas to be recorded")
	}

	updated, err := client.CoreV1().PersistentVolumes().Get(ctx, pv.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pv: %v", err)
	}
	if updated.Annotations[AnnotationQuotaStatus] != QuotaStatusApplied {
		t.Fatalf("expected quota status applied, got %q", updated.Annotations[AnnotationQuotaStatus])
	}
}

func TestEnsureQuotaNoCapacity(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-nocap"},
		Spec:       v1.PersistentVolumeSpec{PersistentVolumeSource: v1.PersistentVolumeSource{NFS: &v1.NFSVolumeSource{Path: "/exports/x"}}},
	}
	if err := a.ensureQuota(context.Background(), pv); err == nil || !strings.Contains(err.Error(), "no storage capacity") {
		t.Fatalf("expected no-capacity error, got %v", err)
	}
}

func TestEnsureQuotaNoNFSPath(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	pv := newBoundPV("pv-nopath", "", 1)
	pv.Spec.NFS = nil // remove NFS source, leaving no way to compute a path
	if err := a.ensureQuota(context.Background(), pv); err == nil || !strings.Contains(err.Error(), "no NFS path") {
		t.Fatalf("expected no-NFS-path error, got %v", err)
	}
}

func TestEnsureQuotaDirectoryMissing(t *testing.T) {
	pv := newBoundPV("pv-missing-dir", "/exports/pvc-missing", 1)
	client := fake.NewSimpleClientset(pv)
	a := newTestAgent(t, client)
	a.nfsServerPath = "/exports"
	// Note: local directory intentionally not created.

	if err := a.ensureQuota(context.Background(), pv); err != nil {
		t.Fatalf("expected nil error (skip) when directory missing, got %v", err)
	}
	localPath := a.nfsPathToLocal("/exports/pvc-missing")
	if _, ok := a.appliedQuotas[localPath]; ok {
		t.Fatalf("appliedQuotas should not be set when directory is missing")
	}
}

func TestEnsureQuotaUnsupportedFilesystem(t *testing.T) {
	a, pv, client := ensureQuotaFixture(t, 1)
	// a.fsType left at zero value ("") -> applyQuota hits the default/unsupported branch.

	ctx := context.Background()
	if err := a.ensureQuota(ctx, pv); err == nil || !strings.Contains(err.Error(), "unsupported filesystem type") {
		t.Fatalf("expected unsupported filesystem error, got %v", err)
	}

	updated, err := client.CoreV1().PersistentVolumes().Get(ctx, pv.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pv: %v", err)
	}
	if updated.Annotations[AnnotationQuotaStatus] != QuotaStatusFailed {
		t.Fatalf("expected quota status failed, got %q", updated.Annotations[AnnotationQuotaStatus])
	}
}

func TestEnsureQuotaCommandFailure(t *testing.T) {
	r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		if name == "xfs_quota" && len(args) >= 3 && strings.Contains(args[2], "project -s -p") {
			return []byte("boom"), errors.New("xfs_quota project init failed")
		}
		return xfsHappyRunner().fn(name, args...)
	}}
	withFakeRunner(t, r)

	a, pv, client := ensureQuotaFixture(t, 1)
	a.fsType = quota.FSTypeXFS

	ctx := context.Background()
	if err := a.ensureQuota(ctx, pv); err == nil {
		t.Fatalf("expected command failure error")
	}

	localPath := a.nfsPathToLocal("/exports/pvc-1")
	if _, ok := a.appliedQuotas[localPath]; ok {
		t.Fatalf("appliedQuotas should not be set on failure")
	}

	updated, err := client.CoreV1().PersistentVolumes().Get(ctx, pv.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pv: %v", err)
	}
	if updated.Annotations[AnnotationQuotaStatus] != QuotaStatusFailed {
		t.Fatalf("expected quota status failed, got %q", updated.Annotations[AnnotationQuotaStatus])
	}
}

func TestEnsureQuotaUpdateFlowWithAuditLogger(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	a, pv, _ := ensureQuotaFixture(t, 1)
	a.fsType = quota.FSTypeXFS

	auditPath := filepath.Join(t.TempDir(), "audit.log")
	logger, err := audit.NewLogger(audit.Config{Enabled: true, FilePath: auditPath, NodeName: "n", AgentID: "a"})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	a.SetAuditLogger(logger)

	ctx := context.Background()
	if err := a.ensureQuota(ctx, pv); err != nil {
		t.Fatalf("initial ensureQuota: %v", err)
	}

	// Bump capacity to trigger the "isUpdate" branch (LogQuotaUpdate).
	pv.Spec.Capacity = v1.ResourceList{
		v1.ResourceStorage: *resource.NewQuantity(2*1024*1024*1024, resource.BinarySI),
	}

	if err := a.ensureQuota(ctx, pv); err != nil {
		t.Fatalf("update ensureQuota: %v", err)
	}

	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("expected audit log entries to be written")
	}
}

func TestSyncAllQuotas(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())

	matching := newBoundPV("pv-match", "/exports/pvc-match", 1)
	matching.Annotations = map[string]string{"pv.kubernetes.io/provisioned-by": "example.com/nfs"}
	nonBound := newBoundPV("pv-pending", "/exports/pvc-pending", 1)
	nonBound.Status.Phase = v1.VolumePending
	wrongProvisioner := newBoundPV("pv-other", "/exports/pvc-other", 1)
	wrongProvisioner.Annotations = map[string]string{"pv.kubernetes.io/provisioned-by": "someone/else"}

	client := fake.NewSimpleClientset(matching, nonBound, wrongProvisioner)
	a := newTestAgent(t, client)
	a.nfsServerPath = "/exports"
	a.fsType = quota.FSTypeXFS
	a.provisionerName = "example.com/nfs"

	for _, name := range []string{"pvc-match", "pvc-pending", "pvc-other"} {
		if err := os.MkdirAll(filepath.Join(a.nfsBasePath, name), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	if err := a.syncAllQuotas(context.Background()); err != nil {
		t.Fatalf("syncAllQuotas: %v", err)
	}

	matchPath := a.nfsPathToLocal("/exports/pvc-match")
	if _, ok := a.appliedQuotas[matchPath]; !ok {
		t.Fatalf("expected matching PV to have quota applied")
	}
	pendingPath := a.nfsPathToLocal("/exports/pvc-pending")
	if _, ok := a.appliedQuotas[pendingPath]; ok {
		t.Fatalf("non-bound PV should have been skipped")
	}
	otherPath := a.nfsPathToLocal("/exports/pvc-other")
	if _, ok := a.appliedQuotas[otherPath]; ok {
		t.Fatalf("PV with mismatched provisioner annotation should have been skipped")
	}
}

func TestUpdateQuotaStatusMissingPV(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	pv := &v1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "does-not-exist"}}
	// Should log and return without panicking when the Get fails.
	a.updateQuotaStatus(context.Background(), pv, QuotaStatusFailed)
}

func TestRecordHistoryNilStore(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	a.historyStore = nil
	a.recordHistory() // must be a no-op, not panic
}

func TestRecordHistoryWithStore(t *testing.T) {
	withFakeRunner(t, &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		if name == "df" {
			return []byte("Filesystem Type 1K-blocks Used Available Use% Mounted on\n/dev/sda1 ext4 100 10 90 10% /mnt\n"), nil
		}
		return []byte(""), nil
	}})

	a := newTestAgent(t, fake.NewSimpleClientset())
	if err := os.MkdirAll(filepath.Join(a.nfsBasePath, "pvc-hist"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	store, err := history.NewStore(filepath.Join(t.TempDir(), "history.json"), time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	a.historyStore = store

	a.recordHistory() // exercises DetectFSType + GetDirUsages + Store.Record
}

func TestCollectHistoryStopsOnContextCancel(t *testing.T) {
	withFakeRunner(t, &fakeRunner{})
	a := newTestAgent(t, fake.NewSimpleClientset())
	store, err := history.NewStore(filepath.Join(t.TempDir(), "history.json"), time.Millisecond, time.Hour)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	a.historyStore = store

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.collectHistory(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("collectHistory did not stop after context cancellation")
	}
}

func TestLivenessOK_BeforeFirstHeartbeat(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	a.SetSyncInterval(time.Second)

	ok, reason := a.LivenessOK()
	if !ok {
		t.Fatalf("LivenessOK before any heartbeat should be ok, got reason %q", reason)
	}
}

func TestLivenessOK_FreshHeartbeat(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	a.SetSyncInterval(time.Minute)
	a.recordHeartbeat()

	ok, reason := a.LivenessOK()
	if !ok {
		t.Fatalf("LivenessOK with a fresh heartbeat should be ok, got reason %q", reason)
	}
}

func TestLivenessOK_StalledHeartbeat(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	a.SetSyncInterval(time.Millisecond)
	a.healthMu.Lock()
	a.lastHeartbeat = time.Now().Add(-time.Hour)
	a.healthMu.Unlock()

	ok, reason := a.LivenessOK()
	if ok {
		t.Fatalf("LivenessOK should not be ok when heartbeat is far older than the threshold")
	}
	if !strings.Contains(reason, "stalled") {
		t.Fatalf("reason = %q, want it to mention the stall", reason)
	}
}

func TestReadinessOK_ProgressesThroughStartup(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())

	if ok, reason := a.ReadinessOK(); ok || reason != "filesystem type not detected" {
		t.Fatalf("ReadinessOK before startup = (%v, %q), want (false, filesystem type not detected)", ok, reason)
	}

	a.setFilesystemDetected(true)
	if ok, reason := a.ReadinessOK(); ok || reason != "quota subsystem not available" {
		t.Fatalf("ReadinessOK after fs detection = (%v, %q), want (false, quota subsystem not available)", ok, reason)
	}

	a.setQuotaAvailable(true)
	if ok, reason := a.ReadinessOK(); ok || reason != "initial quota sync not yet completed" {
		t.Fatalf("ReadinessOK after quota available = (%v, %q), want (false, initial quota sync not yet completed)", ok, reason)
	}

	a.recordSyncResult(nil)
	if ok, reason := a.ReadinessOK(); !ok {
		t.Fatalf("ReadinessOK after a completed sync should be ready, got reason %q", reason)
	}
}

func TestReadinessOK_ZeroAppliedQuotasIsReady(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	a.setFilesystemDetected(true)
	a.setQuotaAvailable(true)
	a.recordSyncResult(nil)

	if got := a.AppliedQuotaCount(); got != 0 {
		t.Fatalf("expected zero applied quotas, got %d", got)
	}
	if ok, reason := a.ReadinessOK(); !ok {
		t.Fatalf("an agent with zero PVs to manage should be ready, got reason %q", reason)
	}
}

func TestReadinessOK_FlipsBackAfterRepeatedSyncFailures(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	a.setFilesystemDetected(true)
	a.setQuotaAvailable(true)
	a.recordSyncResult(nil)

	for i := 0; i < readinessSyncFailureThreshold; i++ {
		a.recordSyncResult(errors.New("list PVs failed"))
	}

	ok, reason := a.ReadinessOK()
	if ok {
		t.Fatalf("ReadinessOK should flip to not-ready after %d consecutive sync failures", readinessSyncFailureThreshold)
	}
	if !strings.Contains(reason, "consecutive failures") {
		t.Fatalf("reason = %q, want it to mention consecutive failures", reason)
	}

	// A subsequent success resets the streak.
	a.recordSyncResult(nil)
	if ok, reason := a.ReadinessOK(); !ok {
		t.Fatalf("ReadinessOK should recover after a successful sync, got reason %q", reason)
	}
}

func TestReadinessOK_BasePathNotAccessible(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	a.setFilesystemDetected(true)
	a.setQuotaAvailable(true)
	a.recordSyncResult(nil)
	a.nfsBasePath = filepath.Join(t.TempDir(), "does-not-exist")

	ok, reason := a.ReadinessOK()
	if ok {
		t.Fatalf("ReadinessOK should fail when the base path is not accessible")
	}
	if !strings.Contains(reason, "base path not accessible") {
		t.Fatalf("reason = %q, want it to mention the base path", reason)
	}
}
