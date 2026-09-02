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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dasomel/nfs-quota-agent/internal/audit"
	"github.com/dasomel/nfs-quota-agent/internal/quota"
	"github.com/dasomel/nfs-quota-agent/internal/quotapolicy"
)

// readAuditEntries parses every newline-terminated JSON line in path as an
// audit.Entry, in file order.
func readAuditEntries(t *testing.T, path string) []audit.Entry {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	var entries []audit.Entry
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var e audit.Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("unmarshal audit entry %q: %v", line, err)
		}
		entries = append(entries, e)
	}
	return entries
}

// TestEnsureQuota_AuditEntriesShareCorrelationIDPerAttempt guards #14's
// admission<->enforcement correlation item: every audit entry produced by
// ONE ensureQuota call (here, a successful apply followed by a failed
// read-back verification -- #10's VERIFY_FAILED plus the folded CREATE
// entry with the same error) must carry the same correlation_id, and a
// SEPARATE ensureQuota call (a second reconcile attempt for the same PV)
// must get a fresh one, not reuse the first attempt's.
func TestEnsureQuota_AuditEntriesShareCorrelationIDPerAttempt(t *testing.T) {
	// The report never shows the project the preceding `limit -p` call just
	// "applied" -- every attempt's read-back verification fails, so every
	// ensureQuota call here produces exactly two audit entries (VERIFY_FAILED
	// + CREATE-with-error), the same pairing
	// TestEnsureQuota_VerificationFailureNotReportedApplied exercises.
	r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		if name == "xfs_quota" && len(args) >= 3 && strings.HasPrefix(args[2], "report") {
			return []byte("Project ID   Used   Soft   Hard   Warn/Grace\n"), nil
		}
		return xfsHappyRunner().fn(name, args...)
	}}
	withFakeRunner(t, r)

	a, pv, _ := ensureQuotaFixture(t, 1)
	a.fsType = quota.FSTypeXFS

	auditPath := filepath.Join(t.TempDir(), "audit.log")
	logger, err := audit.NewLogger(audit.Config{Enabled: true, FilePath: auditPath, NodeName: "n", AgentID: "a"})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	a.SetAuditLogger(logger)

	ctx := context.Background()
	if err := a.ensureQuota(ctx, pv, 0); err == nil {
		t.Fatalf("expected the first attempt's read-back verification to fail")
	}
	if err := a.ensureQuota(ctx, pv, 0); err == nil {
		t.Fatalf("expected the second attempt's read-back verification to fail")
	}
	logger.Close()

	entries := readAuditEntries(t, auditPath)
	if len(entries) != 4 {
		t.Fatalf("expected 4 audit entries (2 attempts x 2 entries each), got %d", len(entries))
	}

	firstAttempt := []audit.Entry{entries[0], entries[1]}
	secondAttempt := []audit.Entry{entries[2], entries[3]}

	for i, e := range firstAttempt {
		if e.CorrelationID == "" {
			t.Fatalf("firstAttempt[%d].CorrelationID is empty, want a generated ID", i)
		}
	}
	if firstAttempt[0].CorrelationID != firstAttempt[1].CorrelationID {
		t.Fatalf("first attempt's two entries have different correlation IDs: %q vs %q", firstAttempt[0].CorrelationID, firstAttempt[1].CorrelationID)
	}

	for i, e := range secondAttempt {
		if e.CorrelationID == "" {
			t.Fatalf("secondAttempt[%d].CorrelationID is empty, want a generated ID", i)
		}
	}
	if secondAttempt[0].CorrelationID != secondAttempt[1].CorrelationID {
		t.Fatalf("second attempt's two entries have different correlation IDs: %q vs %q", secondAttempt[0].CorrelationID, secondAttempt[1].CorrelationID)
	}

	if firstAttempt[0].CorrelationID == secondAttempt[0].CorrelationID {
		t.Fatalf("two separate ensureQuota attempts must not share a correlation ID, both got %q", firstAttempt[0].CorrelationID)
	}
}

// TestEnsureQuota_EnforcedQuotaBytes_XFS guards #14's "requested vs
// enforced" item for XFS: a non-1024-multiple requested size must produce
// an audit entry whose EnforcedQuota equals quota.ExpectedEnforcedBytes,
// not the raw requested size.
func TestEnsureQuota_EnforcedQuotaBytes_XFS(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	pv, client := nonAlignedCapacityPV(t, "pv-xfs-nonaligned", "/exports/pvc-xfs-nonaligned")
	a := newTestAgent(t, client)
	a.nfsServerPath = "/exports"
	a.fsType = quota.FSTypeXFS
	if err := os.MkdirAll(a.nfsPathToLocal(pv.Spec.NFS.Path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	auditPath := filepath.Join(t.TempDir(), "audit.log")
	logger, err := audit.NewLogger(audit.Config{Enabled: true, FilePath: auditPath, NodeName: "n", AgentID: "a"})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	a.SetAuditLogger(logger)

	ctx := context.Background()
	if err := a.ensureQuota(ctx, pv, 0); err != nil {
		t.Fatalf("ensureQuota: %v", err)
	}
	logger.Close()

	entries := readAuditEntries(t, auditPath)
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(entries))
	}
	wantEnforced := quota.ExpectedEnforcedBytes(quota.FSTypeXFS, nonAlignedSizeBytes)
	if entries[0].NewQuota != nonAlignedSizeBytes {
		t.Fatalf("NewQuota = %d, want the raw requested %d", entries[0].NewQuota, nonAlignedSizeBytes)
	}
	if entries[0].EnforcedQuota != wantEnforced {
		t.Fatalf("EnforcedQuota = %d, want %d (ExpectedEnforcedBytes)", entries[0].EnforcedQuota, wantEnforced)
	}
	if wantEnforced == nonAlignedSizeBytes {
		t.Fatalf("test fixture bug: %d is already 1024-aligned, this test proves nothing", nonAlignedSizeBytes)
	}
}

// TestEnsureQuota_EnforcedQuotaBytes_Ext4 is TestEnsureQuota_EnforcedQuotaBytes_XFS
// for ext4, using ext4HappyRunner (helpers_test.go).
func TestEnsureQuota_EnforcedQuotaBytes_Ext4(t *testing.T) {
	withFakeRunner(t, ext4HappyRunner())
	pv, client := nonAlignedCapacityPV(t, "pv-ext4-nonaligned", "/exports/pvc-ext4-nonaligned")
	a := newTestAgent(t, client)
	a.nfsServerPath = "/exports"
	a.fsType = quota.FSTypeExt4
	if err := os.MkdirAll(a.nfsPathToLocal(pv.Spec.NFS.Path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	auditPath := filepath.Join(t.TempDir(), "audit.log")
	logger, err := audit.NewLogger(audit.Config{Enabled: true, FilePath: auditPath, NodeName: "n", AgentID: "a"})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	a.SetAuditLogger(logger)

	ctx := context.Background()
	if err := a.ensureQuota(ctx, pv, 0); err != nil {
		t.Fatalf("ensureQuota: %v", err)
	}
	logger.Close()

	entries := readAuditEntries(t, auditPath)
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(entries))
	}
	wantEnforced := quota.ExpectedEnforcedBytes(quota.FSTypeExt4, nonAlignedSizeBytes)
	if entries[0].EnforcedQuota != wantEnforced {
		t.Fatalf("EnforcedQuota = %d, want %d (ExpectedEnforcedBytes)", entries[0].EnforcedQuota, wantEnforced)
	}
	if wantEnforced == nonAlignedSizeBytes {
		t.Fatalf("test fixture bug: %d is already 1024-aligned, this test proves nothing", nonAlignedSizeBytes)
	}
}

// TestEnsureQuota_EnforcedQuotaBytes_Btrfs proves the other half of the
// same acceptance item: btrfs has no KB-flooring (CLAUDE.md), so
// EnforcedQuota must equal the raw requested size exactly, not something
// independently floored.
func TestEnsureQuota_EnforcedQuotaBytes_Btrfs(t *testing.T) {
	var mu sync.Mutex
	limits := map[string]string{}
	withFakeRunner(t, &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		if name == "btrfs" && args[0] == "subvolume" && args[1] == "show" {
			return []byte("Name: my-subvolume\nUUID: 1234"), nil
		}
		if name == "btrfs" && args[0] == "qgroup" && args[1] == "limit" {
			mu.Lock()
			limits[args[3]] = args[2]
			mu.Unlock()
			return []byte(""), nil
		}
		if name == "btrfs" && args[0] == "qgroup" && args[1] == "show" {
			mu.Lock()
			defer mu.Unlock()
			var sb strings.Builder
			sb.WriteString("qgroupid rfer excl max_rfer max_excl path\n-------- ---- ---- -------- -------- ----\n")
			for path, bytes := range limits {
				sb.WriteString("0/256 " + bytes + " " + bytes + " " + bytes + " none " + path + "\n")
			}
			return []byte(sb.String()), nil
		}
		return nil, os.ErrInvalid
	}})

	pv, client := nonAlignedCapacityPV(t, "pv-btrfs-nonaligned", "/exports/pvc-btrfs-nonaligned")
	a := newTestAgent(t, client)
	a.nfsServerPath = "/exports"
	a.fsType = quota.FSTypeBtrfs
	if err := os.MkdirAll(a.nfsPathToLocal(pv.Spec.NFS.Path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	auditPath := filepath.Join(t.TempDir(), "audit.log")
	logger, err := audit.NewLogger(audit.Config{Enabled: true, FilePath: auditPath, NodeName: "n", AgentID: "a"})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	a.SetAuditLogger(logger)

	ctx := context.Background()
	if err := a.ensureQuota(ctx, pv, 0); err != nil {
		t.Fatalf("ensureQuota: %v", err)
	}
	logger.Close()

	entries := readAuditEntries(t, auditPath)
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(entries))
	}
	wantEnforced := quota.ExpectedEnforcedBytes(quota.FSTypeBtrfs, nonAlignedSizeBytes)
	if wantEnforced != nonAlignedSizeBytes {
		t.Fatalf("test fixture bug: ExpectedEnforcedBytes floored a btrfs value (%d != %d); btrfs must not floor", wantEnforced, nonAlignedSizeBytes)
	}
	if entries[0].EnforcedQuota != nonAlignedSizeBytes {
		t.Fatalf("EnforcedQuota = %d, want the raw requested %d (btrfs does not floor)", entries[0].EnforcedQuota, nonAlignedSizeBytes)
	}
}

// nonAlignedSizeBytes is a byte count deliberately NOT a multiple of 1024,
// so quota.ExpectedEnforcedBytes' XFS/ext4 KB flooring actually changes the
// value (CLAUDE.md's KB-flooring gotcha) -- every other fixture in this
// package uses a Gi-multiple capacity, which would make this comparison
// vacuous.
const nonAlignedSizeBytes = 1_048_577 // 1 MiB + 1 byte

// nonAlignedCapacityPV returns a bound PV with a non-1024-aligned capacity
// (see nonAlignedSizeBytes) at nfsPath, plus a fake clientset containing it.
func nonAlignedCapacityPV(t *testing.T, name, nfsPath string) (*v1.PersistentVolume, *fake.Clientset) {
	t.Helper()
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1.PersistentVolumeSpec{
			Capacity: v1.ResourceList{v1.ResourceStorage: *resource.NewQuantity(nonAlignedSizeBytes, resource.DecimalSI)},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				NFS: &v1.NFSVolumeSource{Server: "nfs.example.com", Path: nfsPath},
			},
			ClaimRef: &v1.ObjectReference{Namespace: "default", Name: name + "-claim"},
		},
		Status: v1.PersistentVolumeStatus{Phase: v1.VolumeBound},
	}
	client := fake.NewSimpleClientset(pv)
	return pv, client
}

// TestSyncAllQuotas_PolicyProvenanceRecordedWhenPolicyApplies guards #14's
// "policy provenance" item: when a QuotaPolicy clamps a claim's requested
// size, the resulting audit entry must record the winning policy's
// name/generation and the BoundOutcome that produced the clamp.
func TestSyncAllQuotas_PolicyProvenanceRecordedWhenPolicyApplies(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	a, _ := quotaPolicyTestFixture(t)

	auditPath := filepath.Join(t.TempDir(), "audit.log")
	logger, err := audit.NewLogger(audit.Config{Enabled: true, FilePath: auditPath, NodeName: "n", AgentID: "a"})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	a.SetAuditLogger(logger)

	a.SetQuotaPolicyEnabled(true)
	p := gi1MaxPolicy("default", "cap-at-1gi")
	p.Generation = 3
	a.SetDynamicClient(newFakeQuotaPolicyClient(t, p))

	if err := a.syncAllQuotas(context.Background()); err != nil {
		t.Fatalf("syncAllQuotas: %v", err)
	}
	logger.Close()

	entries := readAuditEntries(t, auditPath)
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(entries))
	}
	entry := entries[0]
	if entry.Policy == nil {
		t.Fatalf("expected Policy provenance to be recorded when a QuotaPolicy clamped the claim")
	}
	if entry.Policy.Name != "cap-at-1gi" {
		t.Errorf("Policy.Name = %q, want %q", entry.Policy.Name, "cap-at-1gi")
	}
	if entry.Policy.Generation != 3 {
		t.Errorf("Policy.Generation = %d, want 3", entry.Policy.Generation)
	}
	if entry.Policy.Outcome != string(quotapolicy.BoundClampedToMax) {
		t.Errorf("Policy.Outcome = %q, want %q", entry.Policy.Outcome, quotapolicy.BoundClampedToMax)
	}
	if entry.NewQuota != oneGiBytes {
		t.Errorf("NewQuota = %d, want %d (the policy-resolved effective size)", entry.NewQuota, oneGiBytes)
	}
}

// TestSyncAllQuotas_PolicyProvenanceAbsentWithoutMatchingPolicy is the
// negative half of TestSyncAllQuotas_PolicyProvenanceRecordedWhenPolicyApplies:
// with QuotaPolicy enabled but no policy present at all, the resulting
// audit entry must have no Policy provenance (nil, not a zero-value
// struct) -- pre-QuotaPolicy audit consumers see no shape change.
func TestSyncAllQuotas_PolicyProvenanceAbsentWithoutMatchingPolicy(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	a, _ := quotaPolicyTestFixture(t)

	auditPath := filepath.Join(t.TempDir(), "audit.log")
	logger, err := audit.NewLogger(audit.Config{Enabled: true, FilePath: auditPath, NodeName: "n", AgentID: "a"})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	a.SetAuditLogger(logger)

	a.SetQuotaPolicyEnabled(true)
	a.SetDynamicClient(newFakeQuotaPolicyClient(t)) // no policies

	if err := a.syncAllQuotas(context.Background()); err != nil {
		t.Fatalf("syncAllQuotas: %v", err)
	}
	logger.Close()

	entries := readAuditEntries(t, auditPath)
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(entries))
	}
	if entries[0].Policy != nil {
		t.Fatalf("expected no Policy provenance without a matching QuotaPolicy, got %+v", entries[0].Policy)
	}
}

// TestEnsureQuota_NoPolicyProvenanceOnDirectCall covers the plain
// ensureQuota entry point (the watch/reconcile-queue path's shape, per
// CLAUDE.md's reconcile_queue.go constraint): with no policyAttempt
// available at all, the audit entry must have nil Policy, same as before
// #14.
func TestEnsureQuota_NoPolicyProvenanceOnDirectCall(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	a, pv, _ := ensureQuotaFixture(t, 1)
	a.fsType = quota.FSTypeXFS

	auditPath := filepath.Join(t.TempDir(), "audit.log")
	logger, err := audit.NewLogger(audit.Config{Enabled: true, FilePath: auditPath, NodeName: "n", AgentID: "a"})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	a.SetAuditLogger(logger)

	if err := a.ensureQuota(context.Background(), pv, 0); err != nil {
		t.Fatalf("ensureQuota: %v", err)
	}
	logger.Close()

	entries := readAuditEntries(t, auditPath)
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(entries))
	}
	if entries[0].Policy != nil {
		t.Fatalf("expected no Policy provenance on a direct ensureQuota call, got %+v", entries[0].Policy)
	}
	if entries[0].CorrelationID == "" {
		t.Fatalf("expected a correlation ID even without policy provenance")
	}
}
