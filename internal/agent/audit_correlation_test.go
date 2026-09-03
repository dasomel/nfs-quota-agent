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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/dasomel/nfs-quota-agent/internal/apis/quota/v1alpha1"
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

	// Pins the split MEDIUM-1/MEDIUM-2 (independent opus review of PR #111)
	// asked for: VERIFY_FAILED legitimately carries EnforcedQuota (the
	// value the apply INTENDED to enforce, which the read-back then
	// disagreed with -- see audit.Entry.EnforcedQuota's doc comment), but
	// the folded CREATE entry for the SAME failed attempt must NOT --
	// "if err == nil" in ensureQuotaMutatedWith is the only thing enforcing
	// that, and without an assertion here, mutating it to unconditionally
	// populate EnforcedQuota left every other test in this file green.
	verifyFailed, folded := firstAttempt[0], firstAttempt[1]
	if verifyFailed.Action != audit.ActionVerifyFailed {
		t.Fatalf("firstAttempt[0].Action = %q, want %q", verifyFailed.Action, audit.ActionVerifyFailed)
	}
	if verifyFailed.EnforcedQuota == 0 {
		t.Fatalf("VERIFY_FAILED entry must record what the apply intended to enforce (non-zero), got 0")
	}
	if folded.Success {
		t.Fatalf("the folded CREATE/UPDATE entry for a failed attempt must have Success=false")
	}
	if folded.EnforcedQuota != 0 {
		t.Fatalf("a FAILED CREATE/UPDATE entry must have EnforcedQuota=0 (nothing was durably enforced), got %d", folded.EnforcedQuota)
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

// TestEnsureQuota_ProjectIDAllocationFailureAudit_CorrelationAndProvenance
// is LOW-3 from the independent opus review of PR #111: pins that
// LogProjectIDAllocationFailure's audit entry -- previously untested for
// this -- carries a correlation ID, and (called via the plain ensureQuota
// entry point, with no policyAttempt available) has no Policy provenance.
func TestEnsureQuota_ProjectIDAllocationFailureAudit_CorrelationAndProvenance(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	a, pv, _ := ensureQuotaFixture(t, 1)
	a.fsType = quota.FSTypeXFS

	auditPath := filepath.Join(t.TempDir(), "audit.log")
	logger, err := audit.NewLogger(audit.Config{Enabled: true, FilePath: auditPath, NodeName: "n", AgentID: "a"})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	a.SetAuditLogger(logger)

	// Exhaust every ID generateProjectID would probe for this PV's project
	// name (same technique as TestEnsureQuota_ProjectIDExhaustionIsAudited).
	projectName := a.getProjectName(pv)
	id := a.hashProjectName(projectName)
	a.knownProjectIDs = make(map[uint32]string)
	for i := 0; i <= maxProjectIDProbe; i++ {
		a.knownProjectIDs[id] = fmt.Sprintf("someone-else-%d", i)
		id++
		if id == 0 {
			id = 1
		}
	}

	if err := a.ensureQuota(context.Background(), pv, 0); err == nil {
		t.Fatal("expected ensureQuota to fail when the project ID range is exhausted")
	}
	logger.Close()

	entries := readAuditEntries(t, auditPath)
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(entries))
	}
	if entries[0].Action != audit.ActionAllocate {
		t.Fatalf("Action = %q, want %q", entries[0].Action, audit.ActionAllocate)
	}
	if entries[0].CorrelationID == "" {
		t.Fatalf("expected a correlation ID on a LogProjectIDAllocationFailure entry")
	}
	if entries[0].Policy != nil {
		t.Fatalf("expected no Policy provenance on a direct ensureQuota call, got %+v", entries[0].Policy)
	}
}

// TestEnsureQuota_ShrinkGuardRejectionAudit_CorrelationAndProvenance is
// LOW-3's other half: pins that the shrink guard's LogQuotaUpdate
// rejection entry (agent.go's errUnsafeShrink branch) also carries a
// correlation ID, and -- called via plain ensureQuota, no policyAttempt --
// has no Policy provenance.
func TestEnsureQuota_ShrinkGuardRejectionAudit_CorrelationAndProvenance(t *testing.T) {
	runner, state := xfsHappyRunnerWithState()
	withFakeRunner(t, runner)
	a, pv, _ := ensureQuotaFixture(t, 1)
	a.fsType = quota.FSTypeXFS
	ctx := context.Background()

	auditPath := filepath.Join(t.TempDir(), "audit.log")
	logger, err := audit.NewLogger(audit.Config{Enabled: true, FilePath: auditPath, NodeName: "n", AgentID: "a"})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	a.SetAuditLogger(logger)

	if err := a.ensureQuota(ctx, pv, 1_000_000); err != nil {
		t.Fatalf("initial ensureQuota: %v", err)
	}
	state.setUsedBytes(500_000)

	if err := a.ensureQuota(ctx, pv, 100_000); err == nil {
		t.Fatalf("expected a shrink below current usage to be refused")
	}
	logger.Close()

	entries := readAuditEntries(t, auditPath)
	if len(entries) != 2 {
		t.Fatalf("expected 2 audit entries (initial apply + rejected shrink), got %d", len(entries))
	}
	rejected := entries[1]
	if rejected.Action != audit.ActionUpdate {
		t.Fatalf("Action = %q, want %q", rejected.Action, audit.ActionUpdate)
	}
	if rejected.Success {
		t.Fatalf("expected the rejected shrink entry to have Success=false")
	}
	if rejected.CorrelationID == "" {
		t.Fatalf("expected a correlation ID on the shrink-guard rejection entry")
	}
	if rejected.CorrelationID == entries[0].CorrelationID {
		t.Fatalf("the rejected shrink is a separate reconcile attempt from the initial apply and must not share its correlation ID")
	}
	if rejected.Policy != nil {
		t.Fatalf("expected no Policy provenance on a direct ensureQuota call, got %+v", rejected.Policy)
	}
}

// TestSyncAllQuotas_ShrinkGuardRejectionAudit_HasPolicyProvenance closes
// the gap TestEnsureQuota_ShrinkGuardRejectionAudit_CorrelationAndProvenance
// leaves open: when a QuotaPolicy (not a direct ensureQuota caller) is what
// resolved the shrunk size, the shrink-guard's LogQuotaUpdate rejection
// entry must still carry that policy's provenance, the same way a
// successful clamp does (TestSyncAllQuotas_PolicyProvenanceRecordedWhenPolicyApplies).
// Reuses TestSyncAllQuotas_PolicyShrinkBelowUsageSurfacesAsFailingClaim's
// two-cycle shrink scenario (policy_test.go), adding an audit logger.
func TestSyncAllQuotas_ShrinkGuardRejectionAudit_HasPolicyProvenance(t *testing.T) {
	runner, state := xfsHappyRunnerWithState()
	withFakeRunner(t, runner)
	a, _ := quotaPolicyTestFixture(t)
	a.SetQuotaPolicyEnabled(true)
	a.SetQuotaPolicySingleWriter(true)

	auditPath := filepath.Join(t.TempDir(), "audit.log")
	logger, err := audit.NewLogger(audit.Config{Enabled: true, FilePath: auditPath, NodeName: "n", AgentID: "a"})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	a.SetAuditLogger(logger)

	p := &v1alpha1.QuotaPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "shrinking-policy"},
		Spec: v1alpha1.QuotaPolicySpec{
			MaxQuota:   resource.NewQuantity(1_000_000, resource.BinarySI),
			EnforceMax: true,
		},
	}
	p.Generation = 2
	dyn := newFakeQuotaPolicyClient(t, p)
	a.SetDynamicClient(dyn)

	if err := a.syncAllQuotas(context.Background()); err != nil {
		t.Fatalf("syncAllQuotas (cycle 1): %v", err)
	}

	state.setUsedBytes(500_000)
	live, err := dyn.Resource(quotapolicy.GroupVersionResource).Namespace(p.Namespace).Get(context.Background(), p.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get policy before update: %v", err)
	}
	if err := unstructured.SetNestedField(live.Object, "100000", "spec", "maxQuota"); err != nil {
		t.Fatalf("set maxQuota: %v", err)
	}
	if err := unstructured.SetNestedField(live.Object, int64(3), "metadata", "generation"); err != nil {
		t.Fatalf("set generation: %v", err)
	}
	if _, err := dyn.Resource(quotapolicy.GroupVersionResource).Namespace(p.Namespace).Update(context.Background(), live, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update policy: %v", err)
	}

	if err := a.syncAllQuotas(context.Background()); err != nil {
		t.Fatalf("syncAllQuotas (cycle 2): %v", err)
	}
	logger.Close()

	entries := readAuditEntries(t, auditPath)
	if len(entries) != 2 {
		t.Fatalf("expected 2 audit entries (cycle 1 CREATE + cycle 2 rejected shrink UPDATE), got %d", len(entries))
	}
	rejected := entries[1]
	if rejected.Success {
		t.Fatalf("expected the cycle-2 shrink rejection entry to have Success=false")
	}
	if rejected.CorrelationID == "" {
		t.Fatalf("expected a correlation ID on the policy-driven shrink-guard rejection entry")
	}
	if rejected.Policy == nil {
		t.Fatalf("expected Policy provenance on a shrink-guard rejection resolved by a QuotaPolicy")
	}
	if rejected.Policy.Name != "shrinking-policy" {
		t.Errorf("Policy.Name = %q, want %q", rejected.Policy.Name, "shrinking-policy")
	}
	if rejected.Policy.Generation != 3 {
		t.Errorf("Policy.Generation = %d, want 3", rejected.Policy.Generation)
	}
	if rejected.Policy.Outcome != string(quotapolicy.BoundClampedToMax) {
		t.Errorf("Policy.Outcome = %q, want %q", rejected.Policy.Outcome, quotapolicy.BoundClampedToMax)
	}
}

// TestWatchPath_PolicyProvenanceRecordedWhenPolicyApplies pins that a
// watch-triggered apply (Added event processed via watch.go and the
// reconcile queue) attaches QuotaPolicy provenance to the resulting audit
// entry when a policy matched in the snapshot (#14).
func TestWatchPath_PolicyProvenanceRecordedWhenPolicyApplies(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	a, pv := quotaPolicyTestFixture(t)
	a.SetProcessAllNFS(true)

	auditPath := filepath.Join(t.TempDir(), "audit.log")
	logger, err := audit.NewLogger(audit.Config{Enabled: true, FilePath: auditPath, NodeName: "n", AgentID: "a"})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	a.SetAuditLogger(logger)

	a.SetQuotaPolicyEnabled(true)
	p := gi1MaxPolicy("default", "watch-cap-at-1gi")
	p.UID = types.UID("uid-watch-1")
	p.Generation = 3
	a.SetDynamicClient(newFakeQuotaPolicyClient(t, p))

	cycle := a.beginQuotaPolicyCycle(context.Background())
	if cycle == nil {
		t.Fatalf("beginQuotaPolicyCycle returned nil")
	}

	client := a.client.(*fake.Clientset)
	fw := watch.NewFake()
	client.PrependWatchReactor("persistentvolumes", func(action ktesting.Action) (bool, watch.Interface, error) {
		return true, fw, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := runWatchPVs(a, ctx)

	fw.Add(pv)

	localPath := filepath.Join(a.nfsBasePath, "pvc-1")
	waitFor(t, 2*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.appliedQuotas[localPath] == oneGiBytes
	})

	cancel()
	fw.Stop()
	<-done
	logger.Close()

	entries := readAuditEntries(t, auditPath)
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(entries))
	}
	entry := entries[0]
	if entry.CorrelationID == "" {
		t.Fatalf("expected a correlation ID on watch-triggered audit entry")
	}
	if entry.Policy == nil {
		t.Fatalf("expected Policy provenance to be recorded when a QuotaPolicy clamped the claim on the watch path")
	}
	if entry.Policy.Name != "watch-cap-at-1gi" {
		t.Errorf("Policy.Name = %q, want %q", entry.Policy.Name, "watch-cap-at-1gi")
	}
	if entry.Policy.UID != "uid-watch-1" {
		t.Errorf("Policy.UID = %q, want %q", entry.Policy.UID, "uid-watch-1")
	}
	if entry.Policy.Generation != 3 {
		t.Errorf("Policy.Generation = %d, want 3", entry.Policy.Generation)
	}
	if entry.Policy.Outcome != string(quotapolicy.BoundClampedToMax) {
		t.Errorf("Policy.Outcome = %q, want %q", entry.Policy.Outcome, quotapolicy.BoundClampedToMax)
	}
	if entry.NewQuota != oneGiBytes {
		t.Errorf("NewQuota = %d, want %d", entry.NewQuota, oneGiBytes)
	}
	if entry.EnforcedQuota != oneGiBytes {
		t.Errorf("EnforcedQuota = %d, want %d", entry.EnforcedQuota, oneGiBytes)
	}
}

// TestWatchPath_PolicyProvenanceAbsentWithoutMatchingPolicy pins that a
// watch-triggered apply with QuotaPolicy enabled but no policy matching in the
// snapshot leaves audit.Entry.Policy nil (#14).
func TestWatchPath_PolicyProvenanceAbsentWithoutMatchingPolicy(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	a, pv := quotaPolicyTestFixture(t)
	a.SetProcessAllNFS(true)

	auditPath := filepath.Join(t.TempDir(), "audit.log")
	logger, err := audit.NewLogger(audit.Config{Enabled: true, FilePath: auditPath, NodeName: "n", AgentID: "a"})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	a.SetAuditLogger(logger)

	a.SetQuotaPolicyEnabled(true)
	a.SetDynamicClient(newFakeQuotaPolicyClient(t)) // empty

	a.beginQuotaPolicyCycle(context.Background())

	client := a.client.(*fake.Clientset)
	fw := watch.NewFake()
	client.PrependWatchReactor("persistentvolumes", func(action ktesting.Action) (bool, watch.Interface, error) {
		return true, fw, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := runWatchPVs(a, ctx)

	fw.Add(pv)

	localPath := filepath.Join(a.nfsBasePath, "pvc-1")
	waitFor(t, 2*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.appliedQuotas[localPath] == tenGiBytes
	})

	cancel()
	fw.Stop()
	<-done
	logger.Close()

	entries := readAuditEntries(t, auditPath)
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(entries))
	}
	entry := entries[0]
	if entry.CorrelationID == "" {
		t.Fatalf("expected a correlation ID on watch-triggered audit entry")
	}
	if entry.Policy != nil {
		t.Fatalf("expected no Policy provenance without a matching QuotaPolicy on watch path, got %+v", entry.Policy)
	}
	if entry.NewQuota != tenGiBytes {
		t.Errorf("NewQuota = %d, want %d", entry.NewQuota, tenGiBytes)
	}
}

// TestWatchPath_SnapshotRefreshBetweenResolveAndApply_RecordsResolvedPolicy
// pins that the snapshot being refreshed between resolveFromSnapshot (at
// watch/enqueue time) and apply (at reconcile-worker time) does not alter what
// is recorded for that attempt: the audit entry carries the policy that was
// actually used to determine effectiveBytes (#14).
func TestWatchPath_SnapshotRefreshBetweenResolveAndApply_RecordsResolvedPolicy(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	a, pv := quotaPolicyTestFixture(t)
	a.SetProcessAllNFS(true)

	auditPath := filepath.Join(t.TempDir(), "audit.log")
	logger, err := audit.NewLogger(audit.Config{Enabled: true, FilePath: auditPath, NodeName: "n", AgentID: "a"})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	a.SetAuditLogger(logger)

	a.SetQuotaPolicyEnabled(true)

	// Snapshot 1: 1Gi clamp, generation 1
	p1 := gi1MaxPolicy("default", "policy-v1")
	p1.UID = types.UID("uid-p1")
	p1.Generation = 1
	a.setPolicySnapshot(&resolvedPolicySnapshot{
		byNamespace: map[string][]v1alpha1.QuotaPolicy{"default": {*p1}},
		pvcLabels:   map[string]map[string]string{"default/" + pv.Spec.ClaimRef.Name: {}},
	})

	// Queue is constructed but workers are NOT started yet.
	rq := newPVReconcileQueue(a, 1)

	// Simulate event resolution at enqueue time
	effectiveBytes, winner, decision, _ := a.resolveFromSnapshot(pv)
	var pa *policyAttempt
	if winner != nil {
		pa = &policyAttempt{winner: winner, decision: decision}
	}
	rq.enqueue(pv, effectiveBytes, pa)

	// Refresh the snapshot to Snapshot 2: 2Gi clamp, generation 2
	max2Gi := resource.NewQuantity(2*1024*1024*1024, resource.BinarySI)
	p2 := &v1alpha1.QuotaPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "policy-v2", UID: "uid-p2", Generation: 2},
		Spec: v1alpha1.QuotaPolicySpec{
			MaxQuota:   max2Gi,
			EnforceMax: true,
		},
	}
	a.setPolicySnapshot(&resolvedPolicySnapshot{
		byNamespace: map[string][]v1alpha1.QuotaPolicy{"default": {*p2}},
		pvcLabels:   map[string]map[string]string{"default/" + pv.Spec.ClaimRef.Name: {}},
	})

	// Confirm that the snapshot has indeed refreshed and a fresh resolve would produce policy-v2
	freshBytes, freshWinner, _, _ := a.resolveFromSnapshot(pv)
	if freshWinner == nil || freshWinner.Name != "policy-v2" || freshBytes != 2*1024*1024*1024 {
		t.Fatalf("expected snapshot to resolve to policy-v2 now, got %v (%d)", freshWinner, freshBytes)
	}

	// Now start the worker to process the enqueued item
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rq.start(ctx)
	defer rq.shutdown(2 * time.Second)

	localPath := filepath.Join(a.nfsBasePath, "pvc-1")
	waitFor(t, 2*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.appliedQuotas[localPath] == oneGiBytes
	})
	logger.Close()

	entries := readAuditEntries(t, auditPath)
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(entries))
	}
	entry := entries[0]
	if entry.Policy == nil {
		t.Fatalf("expected Policy provenance to be recorded")
	}
	if entry.Policy.Name != "policy-v1" {
		t.Errorf("Policy.Name = %q, want %q (must record the policy that shaped this attempt)", entry.Policy.Name, "policy-v1")
	}
	if entry.Policy.Generation != 1 {
		t.Errorf("Policy.Generation = %d, want 1", entry.Policy.Generation)
	}
	if entry.Policy.Outcome != string(quotapolicy.BoundClampedToMax) {
		t.Errorf("Policy.Outcome = %q, want %q", entry.Policy.Outcome, quotapolicy.BoundClampedToMax)
	}
}

// TestWatchPath_PolicyProvenanceRecordedOnVerifyFailed pins that when read-back
// verification fails on a watch-path apply, the VERIFY_FAILED audit entry carries
// Policy provenance (#14).
func TestWatchPath_PolicyProvenanceRecordedOnVerifyFailed(t *testing.T) {
	r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		if name == "xfs_quota" && len(args) >= 3 && strings.HasPrefix(args[2], "report") {
			return []byte("Project ID   Used   Soft   Hard   Warn/Grace\n"), nil
		}
		return xfsHappyRunner().fn(name, args...)
	}}
	withFakeRunner(t, r)

	a, pv := quotaPolicyTestFixture(t)
	a.SetProcessAllNFS(true)

	auditPath := filepath.Join(t.TempDir(), "audit.log")
	logger, err := audit.NewLogger(audit.Config{Enabled: true, FilePath: auditPath, NodeName: "n", AgentID: "a"})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	a.SetAuditLogger(logger)

	a.SetQuotaPolicyEnabled(true)
	p := gi1MaxPolicy("default", "verify-fail-policy")
	p.UID = types.UID("uid-vf-1")
	p.Generation = 4
	a.SetDynamicClient(newFakeQuotaPolicyClient(t, p))

	a.beginQuotaPolicyCycle(context.Background())

	rq := newPVReconcileQueue(a, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rq.start(ctx)
	defer rq.shutdown(2 * time.Second)

	effectiveBytes, winner, decision, _ := a.resolveFromSnapshot(pv)
	rq.enqueue(pv, effectiveBytes, &policyAttempt{winner: winner, decision: decision})

	// Wait until audit entries appear
	waitFor(t, 2*time.Second, func() bool {
		entries := readAuditEntries(t, auditPath)
		return len(entries) >= 2 // VERIFY_FAILED + failed CREATE
	})
	logger.Close()

	entries := readAuditEntries(t, auditPath)
	if len(entries) < 2 {
		t.Fatalf("expected at least 2 audit entries, got %d", len(entries))
	}
	verifyEntry := entries[0]
	if verifyEntry.Action != audit.ActionVerifyFailed {
		t.Errorf("first entry Action = %s, want %s", verifyEntry.Action, audit.ActionVerifyFailed)
	}
	if verifyEntry.Policy == nil {
		t.Fatalf("expected Policy provenance on VERIFY_FAILED entry")
	}
	if verifyEntry.Policy.Name != "verify-fail-policy" {
		t.Errorf("Policy.Name = %q, want %q", verifyEntry.Policy.Name, "verify-fail-policy")
	}
	if verifyEntry.Policy.Generation != 4 {
		t.Errorf("Policy.Generation = %d, want 4", verifyEntry.Policy.Generation)
	}
	if verifyEntry.Policy.Outcome != string(quotapolicy.BoundClampedToMax) {
		t.Errorf("Policy.Outcome = %q, want %q", verifyEntry.Policy.Outcome, quotapolicy.BoundClampedToMax)
	}
}

// TestWatchPath_PolicyProvenanceRecordedOnUpdate pins that when a watch-triggered
// apply modifies an already-enforced quota, the UPDATE audit entry carries
// Policy provenance (#14).
func TestWatchPath_PolicyProvenanceRecordedOnUpdate(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	a, pv := quotaPolicyTestFixture(t)
	a.SetProcessAllNFS(true)

	auditPath := filepath.Join(t.TempDir(), "audit.log")
	logger, err := audit.NewLogger(audit.Config{Enabled: true, FilePath: auditPath, NodeName: "n", AgentID: "a"})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	a.SetAuditLogger(logger)

	a.SetQuotaPolicyEnabled(true)
	p := gi1MaxPolicy("default", "update-policy")
	p.UID = types.UID("uid-up-1")
	p.Generation = 1
	a.SetDynamicClient(newFakeQuotaPolicyClient(t, p))
	a.beginQuotaPolicyCycle(context.Background())

	client := a.client.(*fake.Clientset)
	fw := watch.NewFake()
	client.PrependWatchReactor("persistentvolumes", func(action ktesting.Action) (bool, watch.Interface, error) {
		return true, fw, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := runWatchPVs(a, ctx)

	localPath := filepath.Join(a.nfsBasePath, "pvc-1")

	// First event: initial apply (CREATE) at 1Gi
	fw.Add(pv)
	waitFor(t, 2*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.appliedQuotas[localPath] == oneGiBytes
	})

	// Update policy to allow 2Gi max, generation 2
	max2Gi := resource.NewQuantity(2*1024*1024*1024, resource.BinarySI)
	p2 := &v1alpha1.QuotaPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "update-policy", UID: "uid-up-1", Generation: 2},
		Spec: v1alpha1.QuotaPolicySpec{
			MaxQuota:   max2Gi,
			EnforceMax: true,
		},
	}
	a.setPolicySnapshot(&resolvedPolicySnapshot{
		byNamespace: map[string][]v1alpha1.QuotaPolicy{"default": {*p2}},
		pvcLabels:   map[string]map[string]string{"default/" + pv.Spec.ClaimRef.Name: {}},
	})

	// Second event: Modified event triggers UPDATE to 2Gi
	fw.Modify(pv)
	waitFor(t, 2*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.appliedQuotas[localPath] == 2*1024*1024*1024
	})

	cancel()
	fw.Stop()
	<-done
	logger.Close()

	entries := readAuditEntries(t, auditPath)
	if len(entries) != 2 {
		t.Fatalf("expected 2 audit entries (CREATE + UPDATE), got %d", len(entries))
	}
	updateEntry := entries[1]
	if updateEntry.Action != audit.ActionUpdate {
		t.Errorf("second entry Action = %s, want %s", updateEntry.Action, audit.ActionUpdate)
	}
	if updateEntry.Policy == nil {
		t.Fatalf("expected Policy provenance on UPDATE entry")
	}
	if updateEntry.Policy.Name != "update-policy" {
		t.Errorf("Policy.Name = %q, want %q", updateEntry.Policy.Name, "update-policy")
	}
	if updateEntry.Policy.Generation != 2 {
		t.Errorf("Policy.Generation = %d, want 2", updateEntry.Policy.Generation)
	}
	if updateEntry.Policy.Outcome != string(quotapolicy.BoundClampedToMax) {
		t.Errorf("Policy.Outcome = %q, want %q", updateEntry.Policy.Outcome, quotapolicy.BoundClampedToMax)
	}
	if updateEntry.EnforcedQuota != 2*1024*1024*1024 {
		t.Errorf("EnforcedQuota = %d, want %d", updateEntry.EnforcedQuota, 2*1024*1024*1024)
	}
}
