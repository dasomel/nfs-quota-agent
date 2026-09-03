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
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/dasomel/nfs-quota-agent/internal/audit"
	"github.com/dasomel/nfs-quota-agent/internal/quotapolicy"
)

// TestPolicyDecision_AnnotationWrittenOnSuccessAndRemovedWhenPolicyNoLongerApplies
// guards #14's deliverable: write the policy decision ID to nfs.io/policy-decision
// (format <policy-name>/<generation>/<outcome>/<id>) on successful apply, and remove
// it when no policy applies.
func TestPolicyDecision_AnnotationWrittenOnSuccessAndRemovedWhenPolicyNoLongerApplies(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	a, pv := quotaPolicyTestFixture(t)

	a.SetQuotaPolicyEnabled(true)
	p := gi1MaxPolicy("default", "cap-at-1gi")
	p.UID = types.UID("uid-policy-14")
	p.Generation = 2
	a.SetDynamicClient(newFakeQuotaPolicyClient(t, p))

	ctx := context.Background()
	if err := a.syncAllQuotas(ctx); err != nil {
		t.Fatalf("syncAllQuotas with policy: %v", err)
	}

	freshPV, err := a.client.CoreV1().PersistentVolumes().Get(ctx, pv.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get PV: %v", err)
	}

	expectedID := quotapolicy.ComputeDecisionID(pv.Name, "uid-policy-14", 2, string(quotapolicy.BoundClampedToMax), oneGiBytes)
	expectedAnnotation := quotapolicy.FormatPolicyDecision("cap-at-1gi", 2, string(quotapolicy.BoundClampedToMax), expectedID)

	gotAnnotation, ok := freshPV.Annotations[AnnotationPolicyDecision]
	if !ok {
		t.Fatalf("expected annotation %q to be present on PV, got annotations: %v", AnnotationPolicyDecision, freshPV.Annotations)
	}
	if gotAnnotation != expectedAnnotation {
		t.Errorf("annotation %s = %q, want %q", AnnotationPolicyDecision, gotAnnotation, expectedAnnotation)
	}

	// Now remove the policy: QuotaPolicy no longer applies.
	a.SetDynamicClient(newFakeQuotaPolicyClient(t)) // empty client, no policies match

	if err := a.syncAllQuotas(ctx); err != nil {
		t.Fatalf("syncAllQuotas without policy: %v", err)
	}

	freshPVAfter, err := a.client.CoreV1().PersistentVolumes().Get(ctx, pv.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get PV after policy removal: %v", err)
	}

	if val, exists := freshPVAfter.Annotations[AnnotationPolicyDecision]; exists {
		t.Errorf("expected annotation %q to be deleted when no policy applies, but found %q", AnnotationPolicyDecision, val)
	}
}

// TestPolicyDecision_CacheHitRefreshesDecisionOnGenerationChange tests that when
// policy generation/outcome changes without changing enforcedBytes, the cache shortcut
// updates the PV annotation and emits an audit entry with action decision_updated,
// without invoking the quota runner (#14 review finding).
func TestPolicyDecision_CacheHitRefreshesDecisionOnGenerationChange(t *testing.T) {
	var runnerCalls int
	happy := xfsHappyRunner()
	r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		runnerCalls++
		return happy.fn(name, args...)
	}}
	withFakeRunner(t, r)

	a, pv := quotaPolicyTestFixture(t)
	a.SetQuotaPolicyEnabled(true)

	logPath := filepath.Join(t.TempDir(), "audit.log")
	auditLog, err := audit.NewLogger(audit.Config{Enabled: true, FilePath: logPath})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer auditLog.Close()
	a.SetAuditLogger(auditLog)

	p1 := gi1MaxPolicy("default", "cap-at-1gi")
	p1.UID = types.UID("uid-gen-test")
	p1.Generation = 1
	a.SetDynamicClient(newFakeQuotaPolicyClient(t, p1))

	ctx := context.Background()
	if err := a.syncAllQuotas(ctx); err != nil {
		t.Fatalf("syncAllQuotas (gen 1): %v", err)
	}

	freshPV1, err := a.client.CoreV1().PersistentVolumes().Get(ctx, pv.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get PV: %v", err)
	}
	expectedID1 := quotapolicy.ComputeDecisionID(pv.Name, "uid-gen-test", 1, string(quotapolicy.BoundClampedToMax), oneGiBytes)
	expectedAnnotation1 := quotapolicy.FormatPolicyDecision("cap-at-1gi", 1, string(quotapolicy.BoundClampedToMax), expectedID1)
	if got := freshPV1.Annotations[AnnotationPolicyDecision]; got != expectedAnnotation1 {
		t.Fatalf("annotation after gen 1 = %q, want %q", got, expectedAnnotation1)
	}

	callsAfterGen1 := runnerCalls

	// Update policy to generation 7 (same bytes: cap-at-1gi, outcome ClampedToMax, 1Gi)
	p7 := gi1MaxPolicy("default", "cap-at-1gi")
	p7.UID = types.UID("uid-gen-test")
	p7.Generation = 7
	a.SetDynamicClient(newFakeQuotaPolicyClient(t, p7))

	// Sync without clearing appliedQuotas cache!
	if err := a.syncAllQuotas(ctx); err != nil {
		t.Fatalf("syncAllQuotas (gen 7): %v", err)
	}

	// Quota runner must NOT be called on a cache hit
	if runnerCalls != callsAfterGen1 {
		t.Errorf("runner calls increased from %d to %d; expected 0 runner calls on cache hit", callsAfterGen1, runnerCalls)
	}

	// Annotation must reflect generation 7
	freshPV7, err := a.client.CoreV1().PersistentVolumes().Get(ctx, pv.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get PV: %v", err)
	}
	expectedID7 := quotapolicy.ComputeDecisionID(pv.Name, "uid-gen-test", 7, string(quotapolicy.BoundClampedToMax), oneGiBytes)
	expectedAnnotation7 := quotapolicy.FormatPolicyDecision("cap-at-1gi", 7, string(quotapolicy.BoundClampedToMax), expectedID7)
	if got := freshPV7.Annotations[AnnotationPolicyDecision]; got != expectedAnnotation7 {
		t.Errorf("annotation after gen 7 = %q, want %q", got, expectedAnnotation7)
	}

	// Audit log must carry a decision_updated entry for generation 7
	entries, err := audit.QueryLog(logPath, audit.Filter{Action: audit.ActionDecisionUpdated})
	if err != nil {
		t.Fatalf("QueryLog: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 %s audit entry, got %d", audit.ActionDecisionUpdated, len(entries))
	}
	if entries[0].Policy == nil || entries[0].Policy.Generation != 7 || entries[0].Policy.DecisionID != expectedID7 {
		t.Errorf("audit entry policy = %+v, want Generation=7 and DecisionID=%s", entries[0].Policy, expectedID7)
	}
}

// TestPolicyDecision_CacheHitUnchangedDecisionCausesNoPVUpdate verifies that when
// neither quota bytes nor policy decision changes on a cache hit, no PV update is made.
func TestPolicyDecision_CacheHitUnchangedDecisionCausesNoPVUpdate(t *testing.T) {
	var runnerCalls int
	withFakeRunner(t, &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		runnerCalls++
		return []byte(""), nil
	}})
	a, _ := quotaPolicyTestFixture(t)
	a.SetQuotaPolicyEnabled(true)
	p := gi1MaxPolicy("default", "cap-at-1gi")
	p.UID = types.UID("uid-unchanged-test")
	p.Generation = 1
	a.SetDynamicClient(newFakeQuotaPolicyClient(t, p))

	ctx := context.Background()
	if err := a.syncAllQuotas(ctx); err != nil {
		t.Fatalf("first syncAllQuotas: %v", err)
	}

	client := a.client.(*fake.Clientset)
	client.ClearActions()
	callsBefore := runnerCalls

	// Second sync with identical policy and quota state (cache hit with unchanged decision)
	if err := a.syncAllQuotas(ctx); err != nil {
		t.Fatalf("second syncAllQuotas: %v", err)
	}

	// Assert NO update or patch actions on persistentvolumes
	for _, act := range client.Actions() {
		if act.GetResource().Resource == "persistentvolumes" {
			verb := act.GetVerb()
			if verb == "update" || verb == "patch" {
				t.Errorf("unexpected mutating action on persistentvolumes for unchanged cache hit: verb=%s", verb)
			}
		}
	}
	if runnerCalls != callsBefore {
		t.Errorf("runner was called %d times on unchanged cache hit, want 0", runnerCalls-callsBefore)
	}
}

// TestPolicyDecision_NoPVCClientCalls asserts via the fake clientset's action list
// that no update/patch actions occur on persistentvolumeclaims (#14 review safeguard).
func TestPolicyDecision_NoPVCClientCalls(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	a, _ := quotaPolicyTestFixture(t)

	a.SetQuotaPolicyEnabled(true)
	p := gi1MaxPolicy("default", "cap-at-1gi")
	p.UID = types.UID("uid-no-pvc-writes")
	p.Generation = 1
	a.SetDynamicClient(newFakeQuotaPolicyClient(t, p))

	client := a.client.(*fake.Clientset)
	client.ClearActions()

	ctx := context.Background()
	if err := a.syncAllQuotas(ctx); err != nil {
		t.Fatalf("syncAllQuotas: %v", err)
	}

	for _, act := range client.Actions() {
		if act.GetResource().Resource == "persistentvolumeclaims" {
			verb := act.GetVerb()
			if verb == "update" || verb == "patch" || verb == "create" || verb == "delete" {
				t.Fatalf("detected forbidden mutating action on persistentvolumeclaims: verb=%s", verb)
			}
		}
	}
}

// TestPolicyDecision_AuditEntryCarriesDecisionID_SyncAndWatchPaths guards #14's
// requirement that audit entries on both sync and watch paths carry the deterministic
// decision_id when a QuotaPolicy applies.
func TestPolicyDecision_AuditEntryCarriesDecisionID_SyncAndWatchPaths(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())

	// 1. Sync path
	t.Run("sync_path", func(t *testing.T) {
		a, pv := quotaPolicyTestFixture(t)
		auditPath := filepath.Join(t.TempDir(), "audit.log")
		logger, err := audit.NewLogger(audit.Config{Enabled: true, FilePath: auditPath, NodeName: "n", AgentID: "a"})
		if err != nil {
			t.Fatalf("NewLogger: %v", err)
		}
		a.SetAuditLogger(logger)

		a.SetQuotaPolicyEnabled(true)
		p := gi1MaxPolicy("default", "sync-policy")
		p.UID = types.UID("uid-sync-1")
		p.Generation = 4
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
			t.Fatalf("expected Policy block on audit entry")
		}

		expectedDecisionID := quotapolicy.ComputeDecisionID(pv.Name, "uid-sync-1", 4, string(quotapolicy.BoundClampedToMax), oneGiBytes)
		if entry.Policy.DecisionID != expectedDecisionID {
			t.Errorf("entry.Policy.DecisionID = %q, want %q", entry.Policy.DecisionID, expectedDecisionID)
		}
	})

	// 2. Watch path
	t.Run("watch_path", func(t *testing.T) {
		a, pv := quotaPolicyTestFixture(t)
		a.SetProcessAllNFS(true)

		auditPath := filepath.Join(t.TempDir(), "audit.log")
		logger, err := audit.NewLogger(audit.Config{Enabled: true, FilePath: auditPath, NodeName: "n", AgentID: "a"})
		if err != nil {
			t.Fatalf("NewLogger: %v", err)
		}
		a.SetAuditLogger(logger)

		a.SetQuotaPolicyEnabled(true)
		p := gi1MaxPolicy("default", "watch-policy")
		p.UID = types.UID("uid-watch-1")
		p.Generation = 5
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
		if entry.Policy == nil {
			t.Fatalf("expected Policy block on audit entry")
		}

		expectedDecisionID := quotapolicy.ComputeDecisionID(pv.Name, "uid-watch-1", 5, string(quotapolicy.BoundClampedToMax), oneGiBytes)
		if entry.Policy.DecisionID != expectedDecisionID {
			t.Errorf("entry.Policy.DecisionID = %q, want %q", entry.Policy.DecisionID, expectedDecisionID)
		}
	})
}

// TestPolicyDecision_NotWrittenOnFailedApply guards #14's requirement that
// a failed quota apply preserves any existing policy-decision annotation rather
// than clearing, overwriting, or corrupting it.
func TestPolicyDecision_NotWrittenOnFailedApply(t *testing.T) {
	// Fake runner that always fails apply
	r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		return nil, errors.New("simulated apply failure")
	}}
	withFakeRunner(t, r)

	a, pv := quotaPolicyTestFixture(t)
	a.SetQuotaPolicyEnabled(true)

	// Seed the annotation first to verify that a failed apply preserves the
	// existing policy decision annotation (#14 review finding).
	seedDecision := "seed-policy/1/ClampedToMax/abcdef1234567890"
	if pv.Annotations == nil {
		pv.Annotations = make(map[string]string)
	}
	pv.Annotations[AnnotationPolicyDecision] = seedDecision
	if _, err := a.client.CoreV1().PersistentVolumes().Update(context.Background(), pv, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update PV with seed annotation: %v", err)
	}

	p := gi1MaxPolicy("default", "fail-policy")
	p.UID = types.UID("uid-fail-1")
	p.Generation = 1
	a.SetDynamicClient(newFakeQuotaPolicyClient(t, p))

	ctx := context.Background()
	_ = a.syncAllQuotas(ctx) // expected to fail

	freshPV, err := a.client.CoreV1().PersistentVolumes().Get(ctx, pv.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get PV: %v", err)
	}

	if freshPV.Annotations[AnnotationQuotaStatus] != QuotaStatusFailed {
		t.Errorf("quota status = %q, want %q", freshPV.Annotations[AnnotationQuotaStatus], QuotaStatusFailed)
	}
	if got := freshPV.Annotations[AnnotationPolicyDecision]; got != seedDecision {
		t.Errorf("expected %s annotation to be preserved on failure, got %q, want %q", AnnotationPolicyDecision, got, seedDecision)
	}
}
