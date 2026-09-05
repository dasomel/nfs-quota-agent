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
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dasomel/nfs-quota-agent/internal/apis/quota/v1alpha1"
	"github.com/dasomel/nfs-quota-agent/internal/events"
	"github.com/dasomel/nfs-quota-agent/internal/quota"
	"github.com/dasomel/nfs-quota-agent/internal/quotapolicy"
)

// TestEnsureQuota_EmitsQuotaAppliedEvent covers the QuotaApplied Event
// (docs/adr/0002-kubernetes-events-and-retry-metrics.md): a plain,
// non-policy successful apply must emit exactly one Normal QuotaApplied
// event regarding the PV.
func TestEnsureQuota_EmitsQuotaAppliedEvent(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	a, pv, _ := ensureQuotaFixture(t, 1)
	a.fsType = quota.FSTypeXFS
	fakeRec := events.NewFake(30 * time.Second)
	a.SetEventRecorder(fakeRec)

	if err := a.ensureQuota(context.Background(), pv, 0); err != nil {
		t.Fatalf("ensureQuota: %v", err)
	}

	if got := fakeRec.Count(pv.Name, events.QuotaApplied); got != 1 {
		t.Fatalf("QuotaApplied events = %d, want 1 (events=%+v)", got, fakeRec.Events)
	}
}

// TestEnsureQuota_AppliedEventDedupedWithinWindow guards the per-(pv,
// reason) dedup window itself, at the agent's real call site rather than
// only inside internal/events' own unit tests: re-running ensureQuota
// against an unchanged, already-applied PV must not emit a second
// QuotaApplied event within the window (the cache-hit branch returns
// before ever reaching the event call, so this also confirms the dedup
// isn't merely masking a second emission).
func TestEnsureQuota_AppliedEventDedupedWithinWindow(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	a, pv, _ := ensureQuotaFixture(t, 1)
	a.fsType = quota.FSTypeXFS
	fakeRec := events.NewFake(30 * time.Second)
	a.SetEventRecorder(fakeRec)

	ctx := context.Background()
	if err := a.ensureQuota(ctx, pv, 0); err != nil {
		t.Fatalf("first ensureQuota: %v", err)
	}
	if err := a.ensureQuota(ctx, pv, 0); err != nil {
		t.Fatalf("second ensureQuota (cache hit): %v", err)
	}

	if got := fakeRec.Count(pv.Name, events.QuotaApplied); got != 1 {
		t.Fatalf("QuotaApplied events after 2 calls (1 real apply + 1 cache hit) = %d, want 1", got)
	}
}

// TestEnsureQuota_EmitsQuotaApplyFailedEvent covers the QuotaApplyFailed
// Event: the filesystem apply command itself failing (before read-back
// verification ever runs) must emit a Warning QuotaApplyFailed event, not
// QuotaVerificationFailed.
func TestEnsureQuota_EmitsQuotaApplyFailedEvent(t *testing.T) {
	r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		if name == "xfs_quota" && len(args) >= 3 && strings.HasPrefix(args[2], "project -s") {
			return nil, errors.New("simulated project -s failure")
		}
		return xfsHappyRunner().fn(name, args...)
	}}
	withFakeRunner(t, r)
	a, pv, _ := ensureQuotaFixture(t, 1)
	a.fsType = quota.FSTypeXFS
	fakeRec := events.NewFake(30 * time.Second)
	a.SetEventRecorder(fakeRec)

	if err := a.ensureQuota(context.Background(), pv, 0); err == nil {
		t.Fatalf("expected ensureQuota to fail")
	}

	if got := fakeRec.Count(pv.Name, events.QuotaApplyFailed); got != 1 {
		t.Fatalf("QuotaApplyFailed events = %d, want 1 (events=%+v)", got, fakeRec.Events)
	}
	if got := fakeRec.Count(pv.Name, events.QuotaVerificationFailed); got != 0 {
		t.Fatalf("QuotaVerificationFailed events = %d, want 0 for an applyQuota-only failure", got)
	}
}

// TestEnsureQuota_EmitsQuotaVerificationFailedEvent covers the
// QuotaVerificationFailed Event, mirroring
// TestEnsureQuota_VerificationFailureNotReportedApplied's fixture (#10):
// the apply command exits 0 but the read-back report doesn't show the
// project, and the resulting event must be QuotaVerificationFailed, not
// QuotaApplyFailed.
func TestEnsureQuota_EmitsQuotaVerificationFailedEvent(t *testing.T) {
	r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		if name == "xfs_quota" && len(args) >= 3 && strings.HasPrefix(args[2], "report") {
			return []byte("Project ID   Used   Soft   Hard   Warn/Grace\n"), nil
		}
		return xfsHappyRunner().fn(name, args...)
	}}
	withFakeRunner(t, r)
	a, pv, _ := ensureQuotaFixture(t, 1)
	a.fsType = quota.FSTypeXFS
	fakeRec := events.NewFake(30 * time.Second)
	a.SetEventRecorder(fakeRec)

	if err := a.ensureQuota(context.Background(), pv, 0); err == nil {
		t.Fatalf("expected ensureQuota to fail")
	}

	if got := fakeRec.Count(pv.Name, events.QuotaVerificationFailed); got != 1 {
		t.Fatalf("QuotaVerificationFailed events = %d, want 1 (events=%+v)", got, fakeRec.Events)
	}
	if got := fakeRec.Count(pv.Name, events.QuotaApplyFailed); got != 0 {
		t.Fatalf("QuotaApplyFailed events = %d, want 0 for a verification-only failure", got)
	}
}

// TestEnsureQuotaMutatedWith_EmitsPolicyClampedEvent covers the
// PolicyClamped Event: a QuotaPolicy bound decision of BoundClampedToMax
// must emit a Normal PolicyClamped event, independent of whether the apply
// itself succeeds.
func TestEnsureQuotaMutatedWith_EmitsPolicyClampedEvent(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	a, pv := quotaPolicyTestFixture(t) // 10Gi requested PV
	fakeRec := events.NewFake(30 * time.Second)
	a.SetEventRecorder(fakeRec)

	policy := gi1MaxPolicy("default", "cap-at-1gi") // EnforceMax: true, MaxQuota: 1Gi
	pa := &policyAttempt{winner: policy, decision: quotapolicy.BoundDecision{Outcome: quotapolicy.BoundClampedToMax, Detail: "clamped to 1Gi"}}

	if _, err := a.ensureQuotaMutatedWith(context.Background(), pv, oneGiBytes, nil, pa); err != nil {
		t.Fatalf("ensureQuotaMutatedWith: %v", err)
	}

	if got := fakeRec.Count(pv.Name, events.PolicyClamped); got != 1 {
		t.Fatalf("PolicyClamped events = %d, want 1 (events=%+v)", got, fakeRec.Events)
	}
}

// TestEnsureQuotaMutatedWith_PolicyClampedNotReemittedOnNoOpResync guards
// the bug the PolicyClamped emission's relocation (see
// ensureQuotaMutatedWith's comment at the top of the function) fixed: the
// event used to be emitted before the appliedQuotas cache short-circuit,
// so every no-op resync of an already-clamped PV re-emitted it forever (N
// clamped PVs -> N Normal Events per sync tick, indefinitely). A real
// mutation on iteration 0 must emit exactly one PolicyClamped event, and
// two further resyncs of the same (PV, effective bytes, decision) that
// hit the cache short-circuit must not add any more. The Fake recorder is
// built with a zero dedup window here (unlike the other tests in this
// file) specifically so this assertion exercises ensureQuotaMutatedWith's
// own short-circuit, not the recorder's separate window-based dedup --
// with a real window, a re-emitted-but-deduped call would pass this test
// for the wrong reason.
func TestEnsureQuotaMutatedWith_PolicyClampedNotReemittedOnNoOpResync(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	a, pv := quotaPolicyTestFixture(t) // 10Gi requested PV
	fakeRec := events.NewFake(0)
	a.SetEventRecorder(fakeRec)

	policy := gi1MaxPolicy("default", "cap-at-1gi") // EnforceMax: true, MaxQuota: 1Gi
	pa := &policyAttempt{winner: policy, decision: quotapolicy.BoundDecision{Outcome: quotapolicy.BoundClampedToMax, Detail: "clamped to 1Gi"}}

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := a.ensureQuotaMutatedWith(ctx, pv, oneGiBytes, nil, pa); err != nil {
			t.Fatalf("ensureQuotaMutatedWith iter %d: %v", i, err)
		}
		if got := fakeRec.Count(pv.Name, events.PolicyClamped); got != 1 {
			t.Fatalf("PolicyClamped events after iter %d = %d, want 1 (events=%+v)", i, got, fakeRec.Events)
		}
	}
}

// TestForgetAppliedQuotaForPV_ForgetsEventRecorder covers the eviction path
// added alongside events.Recorder.Forget: dropping a PV's appliedQuotas
// entry must also drop its events.Recorder dedup-window entries, or those
// accumulate forever for PVs long since deleted (see Forget's doc
// comment in internal/events).
func TestForgetAppliedQuotaForPV_ForgetsEventRecorder(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	fakeRec := events.NewFake(30 * time.Second)
	a.SetEventRecorder(fakeRec)

	pv := newBoundPV("pv-1", "/exports/pvc-1", 1)
	localPath := a.nfsPathToLocal(a.getNFSPath(pv))
	a.appliedQuotas[localPath] = oneGiBytes

	fakeRec.Event(pv, events.TypeNormal, events.QuotaApplied, "applied")
	if got := fakeRec.Count(pv.Name, events.QuotaApplied); got != 1 {
		t.Fatalf("setup: QuotaApplied events = %d, want 1", got)
	}

	a.forgetAppliedQuotaForPV(pv)

	if _, exists := a.appliedQuotas[localPath]; exists {
		t.Fatalf("appliedQuotas still has an entry for %s after forgetAppliedQuotaForPV", localPath)
	}
	// Re-emitting immediately (well within the 30s window) must not be
	// deduped: Forget must have cleared the recorder's own window state.
	fakeRec.Event(pv, events.TypeNormal, events.QuotaApplied, "applied")
	if got := fakeRec.Count(pv.Name, events.QuotaApplied); got != 2 {
		t.Fatalf("QuotaApplied events after Forget+re-emit = %d, want 2 (forget did not clear the dedup window)", got)
	}
}

// TestPruneAppliedQuotas_ForgetsEventRecorderForDisappearedPV covers #160
// review finding F1: a PV deleted while the watch was disconnected never
// runs forgetAppliedQuotaForPV (no Deleted event was ever delivered for
// it), so pruneAppliedQuotas -- the periodic sync's own detector for
// exactly that case, per its doc comment -- must call eventRecorder.Forget
// itself, or a deleted PV's dedup-window entries live in the recorder
// forever. Simulated here by seeding appliedQuotas/appliedQuotaPVNames
// directly (as ensureQuotaMutatedWith would have) and then calling
// pruneAppliedQuotas with live/liveNames that no longer mention the PV, the
// same shape the periodic sync produces once a PV has actually vanished
// from the API list.
func TestPruneAppliedQuotas_ForgetsEventRecorderForDisappearedPV(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	fakeRec := events.NewFake(30 * time.Second)
	a.SetEventRecorder(fakeRec)

	pv := newBoundPV("pv-1", "/exports/pvc-1", 1)
	localPath := a.nfsPathToLocal(a.getNFSPath(pv))
	a.appliedQuotas[localPath] = oneGiBytes
	a.appliedQuotaPVNames[localPath] = pv.Name

	fakeRec.Event(pv, events.TypeNormal, events.QuotaApplied, "applied")
	if got := fakeRec.Count(pv.Name, events.QuotaApplied); got != 1 {
		t.Fatalf("setup: QuotaApplied events = %d, want 1", got)
	}

	// live/liveNames with no entry for pv-1's path/name: exactly what a
	// sync cycle sees once pv-1 has been deleted from the API and the
	// watch never delivered a Deleted event for it.
	a.pruneAppliedQuotas(map[string]struct{}{}, map[string]struct{}{})

	if _, exists := a.appliedQuotas[localPath]; exists {
		t.Fatalf("appliedQuotas still has an entry for %s after pruneAppliedQuotas", localPath)
	}
	if _, exists := a.appliedQuotaPVNames[localPath]; exists {
		t.Fatalf("appliedQuotaPVNames still has an entry for %s after pruneAppliedQuotas", localPath)
	}
	if !slices.Contains(fakeRec.Forgotten, pv.Name) {
		t.Fatalf("pruneAppliedQuotas did not Forget %s (Forgotten=%v)", pv.Name, fakeRec.Forgotten)
	}
	// Re-emitting immediately (well within the 30s window) must not be
	// deduped: Forget must have cleared the recorder's own window state,
	// the same assertion TestForgetAppliedQuotaForPV_ForgetsEventRecorder
	// makes for the Deleted-event path.
	fakeRec.Event(pv, events.TypeNormal, events.QuotaApplied, "applied")
	if got := fakeRec.Count(pv.Name, events.QuotaApplied); got != 2 {
		t.Fatalf("QuotaApplied events after prune+re-emit = %d, want 2 (prune did not forget the dedup window)", got)
	}
}

// TestForgetAppliedQuotaForPV_ForgetsEventRecorderEvenWithoutNFSPath guards
// the LOW half of #160 review finding F1: forgetAppliedQuotaForPV's early
// return for a PV with no resolvable NFS path used to skip
// eventRecorder.Forget entirely, even though Forget only needs pv.Name
// (never a local path) to do its job.
func TestForgetAppliedQuotaForPV_ForgetsEventRecorderEvenWithoutNFSPath(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	fakeRec := events.NewFake(30 * time.Second)
	a.SetEventRecorder(fakeRec)

	// A PV with neither Spec.NFS nor Spec.CSI set: getNFSPath returns "".
	pv := newBoundPV("pv-1", "", 1)

	fakeRec.Event(pv, events.TypeNormal, events.QuotaApplied, "applied")
	if got := fakeRec.Count(pv.Name, events.QuotaApplied); got != 1 {
		t.Fatalf("setup: QuotaApplied events = %d, want 1", got)
	}

	a.forgetAppliedQuotaForPV(pv)

	if !slices.Contains(fakeRec.Forgotten, pv.Name) {
		t.Fatalf("forgetAppliedQuotaForPV with no NFS path did not Forget %s (Forgotten=%v)", pv.Name, fakeRec.Forgotten)
	}
	fakeRec.Event(pv, events.TypeNormal, events.QuotaApplied, "applied")
	if got := fakeRec.Count(pv.Name, events.QuotaApplied); got != 2 {
		t.Fatalf("QuotaApplied events after Forget+re-emit = %d, want 2 (forget did not clear the dedup window)", got)
	}
}

// TestRecordEnforcement_EmitsPolicyRejectedEvent covers the PolicyRejected
// Event at its call site (policy.go's recordEnforcement), exercised
// directly against a minimal quotaPolicyCycle rather than through a full
// syncAllQuotas pass -- recordEnforcement's own contract (documented on
// the function) is that it only needs winner, pv, err, and drift, so this
// is a faithful unit test of the classification logic without the
// overhead of a real QuotaPolicy sync cycle.
func TestRecordEnforcement_EmitsPolicyRejectedEvent(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	fakeRec := events.NewFake(30 * time.Second)
	a.SetEventRecorder(fakeRec)

	pv := newBoundPV("pv-1", "/exports/pvc-1", 1)
	policy := gi1MaxPolicy("default", "cap-at-1gi")

	cycle := &quotaPolicyCycle{
		agent:        a,
		outcomes:     make(map[string][]quotapolicy.ClaimOutcome),
		matchKindFor: make(map[string]v1alpha1.MatchKind),
	}
	cycle.recordEnforcement(policy, pv, errUnsafeShrink, driftCheck{})

	if got := fakeRec.Count(pv.Name, events.PolicyRejected); got != 1 {
		t.Fatalf("PolicyRejected events = %d, want 1 (events=%+v)", got, fakeRec.Events)
	}
}

// TestRecordEnforcement_DoesNotEmitPolicyRejectedForTransientFailures
// guards the classification boundary: not every EnforcementReason means
// "the policy rejected this claim" -- a transient/resource condition like
// ErrHAStandby must not fire PolicyRejected.
func TestRecordEnforcement_DoesNotEmitPolicyRejectedForTransientFailures(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	fakeRec := events.NewFake(30 * time.Second)
	a.SetEventRecorder(fakeRec)

	pv := newBoundPV("pv-1", "/exports/pvc-1", 1)
	policy := gi1MaxPolicy("default", "cap-at-1gi")

	cycle := &quotaPolicyCycle{
		agent:        a,
		outcomes:     make(map[string][]quotapolicy.ClaimOutcome),
		matchKindFor: make(map[string]v1alpha1.MatchKind),
	}
	cycle.recordEnforcement(policy, pv, ErrHAStandby, driftCheck{})

	if got := fakeRec.Count(pv.Name, events.PolicyRejected); got != 0 {
		t.Fatalf("PolicyRejected events = %d, want 0 for ErrHAStandby", got)
	}
}
