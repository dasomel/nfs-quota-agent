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
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

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
	a.appliedQuotas[localPath] = appliedQuota{enforcedBytes: oneGiBytes}

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
// forever. Simulated here by seeding appliedQuotas directly (as
// ensureQuotaMutatedWith would have, enforced bytes and PV name together in
// one entry) and then calling pruneAppliedQuotas with live/liveNames that no
// longer mention the PV, the same shape the periodic sync produces once a PV
// has actually vanished from the API list.
func TestPruneAppliedQuotas_ForgetsEventRecorderForDisappearedPV(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	fakeRec := events.NewFake(30 * time.Second)
	a.SetEventRecorder(fakeRec)

	pv := newBoundPV("pv-1", "/exports/pvc-1", 1)
	localPath := a.nfsPathToLocal(a.getNFSPath(pv))
	a.appliedQuotas[localPath] = appliedQuota{enforcedBytes: oneGiBytes, pvName: pv.Name}

	fakeRec.Event(pv, events.TypeNormal, events.QuotaApplied, "applied")
	if got := fakeRec.Count(pv.Name, events.QuotaApplied); got != 1 {
		t.Fatalf("setup: QuotaApplied events = %d, want 1", got)
	}

	// live/liveNames with no entry for pv-1's path/name: exactly what a
	// sync cycle sees once pv-1 has been deleted from the API and the
	// watch never delivered a Deleted event for it.
	a.pruneAppliedQuotas(map[string]struct{}{}, map[string]struct{}{})

	// One check now covers what used to be two: enforced bytes and PV name
	// live in the same entry, so this absence proves both are gone together
	// -- the entry can no longer drop one while keeping the other.
	if _, exists := a.appliedQuotas[localPath]; exists {
		t.Fatalf("appliedQuotas still has an entry for %s after pruneAppliedQuotas", localPath)
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

// TestEnsureQuota_EmitsQuotaShrinkRejectedEventWithoutPolicy covers the
// QuotaShrinkRejected Event at its real call site (the shrink guard inside
// ensureQuotaMutatedWith, agent.go) for a plain, non-policy caller: the
// guard rejects independently of any QuotaPolicy claim, so it must emit
// even when pa is nil -- and must not also fire PolicyRejected, which is
// reserved for the StorageClass binding path fallback (see
// TestEnsureQuotaMutatedWith_EmitsPolicyRejectedEventForBindingFallback
// below).
func TestEnsureQuota_EmitsQuotaShrinkRejectedEventWithoutPolicy(t *testing.T) {
	runner, state := xfsHappyRunnerWithState()
	withFakeRunner(t, runner)
	a, pv, _ := ensureQuotaFixture(t, 1)
	a.fsType = quota.FSTypeXFS
	fakeRec := events.NewFake(30 * time.Second)
	a.SetEventRecorder(fakeRec)
	ctx := context.Background()

	if err := a.ensureQuota(ctx, pv, 1_000_000); err != nil {
		t.Fatalf("initial ensureQuota: %v", err)
	}
	state.setUsedBytes(500_000)

	err := a.ensureQuota(ctx, pv, 100_000)
	if !errors.Is(err, errUnsafeShrink) {
		t.Fatalf("expected errUnsafeShrink, got %v", err)
	}

	if got := fakeRec.Count(pv.Name, events.QuotaShrinkRejected); got != 1 {
		t.Fatalf("QuotaShrinkRejected events = %d, want 1 (events=%+v)", got, fakeRec.Events)
	}
	if got := fakeRec.Count(pv.Name, events.PolicyRejected); got != 0 {
		t.Fatalf("PolicyRejected events = %d, want 0 for a non-policy caller (events=%+v)", got, fakeRec.Events)
	}
}

// TestEnsureQuota_QuotaShrinkRejectedEventOncePerTransition pins the Event
// to the shrink guard's #92 transition gate rather than to the recorder's
// dedup window: the message embeds the live usage figure, so a workload
// that keeps writing would change the text on every sync tick and slip a
// fresh Event past a message-compared window. A repeat rejection with
// different usage must stay silent; a successful apply in between re-arms
// the gate so the next rejection is a new transition and emits again. The
// re-apply uses a NEW size: re-applying the cached 1_000_000 would hit the
// cache short-circuit, never reach the mutation path that clears the gate,
// and (correctly, per #92) not count as a fresh transition.
func TestEnsureQuota_QuotaShrinkRejectedEventOncePerTransition(t *testing.T) {
	runner, state := xfsHappyRunnerWithState()
	withFakeRunner(t, runner)
	a, pv, _ := ensureQuotaFixture(t, 1)
	a.fsType = quota.FSTypeXFS
	fakeRec := events.NewFake(30 * time.Second)
	a.SetEventRecorder(fakeRec)
	ctx := context.Background()

	if err := a.ensureQuota(ctx, pv, 1_000_000); err != nil {
		t.Fatalf("initial ensureQuota: %v", err)
	}
	state.setUsedBytes(500_000)
	if err := a.ensureQuota(ctx, pv, 100_000); !errors.Is(err, errUnsafeShrink) {
		t.Fatalf("first rejection: expected errUnsafeShrink, got %v", err)
	}
	state.setUsedBytes(600_000)
	if err := a.ensureQuota(ctx, pv, 100_000); !errors.Is(err, errUnsafeShrink) {
		t.Fatalf("repeat rejection: expected errUnsafeShrink, got %v", err)
	}
	if got := fakeRec.Count(pv.Name, events.QuotaShrinkRejected); got != 1 {
		t.Fatalf("QuotaShrinkRejected events after a repeat rejection with changed usage = %d, want 1 (events=%+v)", got, fakeRec.Events)
	}

	if err := a.ensureQuota(ctx, pv, 2_000_000); err != nil {
		t.Fatalf("re-apply above usage: %v", err)
	}
	if err := a.ensureQuota(ctx, pv, 100_000); !errors.Is(err, errUnsafeShrink) {
		t.Fatalf("rejection after re-arm: expected errUnsafeShrink, got %v", err)
	}
	if got := fakeRec.Count(pv.Name, events.QuotaShrinkRejected); got != 2 {
		t.Fatalf("QuotaShrinkRejected events after a fresh transition = %d, want 2 (events=%+v)", got, fakeRec.Events)
	}
}

// TestEnsureQuotaMutatedWith_ShrinkRejectedWithPolicyDoesNotDoubleReport is
// the companion to the test above: a shrink rejection that happens to carry
// a winning QuotaPolicy (pa.winner != nil) must still emit exactly
// QuotaShrinkRejected, never PolicyRejected -- PolicyRejected is now
// reserved for the StorageClass binding path fallback exclusively (see
// events.go's narrowed doc comment), so this guards against the split
// silently reintroducing double-reporting for the policy-attached case.
func TestEnsureQuotaMutatedWith_ShrinkRejectedWithPolicyDoesNotDoubleReport(t *testing.T) {
	runner, state := xfsHappyRunnerWithState()
	withFakeRunner(t, runner)
	a, pv, _ := ensureQuotaFixture(t, 1)
	a.fsType = quota.FSTypeXFS
	fakeRec := events.NewFake(30 * time.Second)
	a.SetEventRecorder(fakeRec)
	ctx := context.Background()

	if _, err := a.ensureQuotaMutatedWith(ctx, pv, 1_000_000, nil, nil); err != nil {
		t.Fatalf("initial apply: %v", err)
	}
	state.setUsedBytes(500_000)

	policy := gi1MaxPolicy("default", "cap-at-1gi")
	pa := &policyAttempt{winner: policy}
	_, err := a.ensureQuotaMutatedWith(ctx, pv, 100_000, nil, pa)
	if !errors.Is(err, errUnsafeShrink) {
		t.Fatalf("expected errUnsafeShrink, got %v", err)
	}

	if got := fakeRec.Count(pv.Name, events.QuotaShrinkRejected); got != 1 {
		t.Fatalf("QuotaShrinkRejected events = %d, want 1 (events=%+v)", got, fakeRec.Events)
	}
	if got := fakeRec.Count(pv.Name, events.PolicyRejected); got != 0 {
		t.Fatalf("PolicyRejected events = %d, want 0 (double-reporting; events=%+v)", got, fakeRec.Events)
	}
}

// TestEnsureQuotaMutatedWith_EmitsPolicyRejectedEventForBindingFallback
// covers the PolicyRejected Event at its real call site (the StorageClass
// binding path fallback rejection inside ensureQuotaMutatedWith, agent.go),
// mirroring TestStorageClassBindingFallbackRejectsBeforeQuotaMutation's
// fixture (policy_test.go). It also pins the message text recordEnforcement
// used to emit, now emitted at the guard site instead.
func TestEnsureQuotaMutatedWith_EmitsPolicyRejectedEventForBindingFallback(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	a, pv := quotaPolicyTestFixture(t)
	pv.Spec.StorageClassName = "nfs-csi"
	pv.Spec.NFS.Path = "/crafted/pvc-1" // maps by basename, therefore ambiguous.
	fakeRec := events.NewFake(30 * time.Second)
	a.SetEventRecorder(fakeRec)

	policy := gi1MaxPolicy("default", "bound")
	policy.Spec.Selector.StorageClassNames = []string{"nfs-csi"}

	_, err := a.ensureQuotaMutatedWith(context.Background(), pv, oneGiBytes, nil, &policyAttempt{winner: policy})
	if !errors.Is(err, errStorageClassBindingPathFallback) {
		t.Fatalf("err = %v, want fallback rejection", err)
	}

	if got := fakeRec.Count(pv.Name, events.PolicyRejected); got != 1 {
		t.Fatalf("PolicyRejected events = %d, want 1 (events=%+v)", got, fakeRec.Events)
	}
	wantPrefix := "QuotaPolicy " + policy.Name + " claim for PV " + pv.Name + " was rejected at enforcement time"
	var found bool
	for _, e := range fakeRec.Events {
		if e.PVName == pv.Name && e.Reason == events.PolicyRejected {
			if !strings.HasPrefix(e.Message, wantPrefix) {
				t.Fatalf("PolicyRejected message = %q, want prefix %q", e.Message, wantPrefix)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("no PolicyRejected event recorded for %s (events=%+v)", pv.Name, fakeRec.Events)
	}
	if got := fakeRec.Count(pv.Name, events.QuotaShrinkRejected); got != 0 {
		t.Fatalf("QuotaShrinkRejected events = %d, want 0 for a binding fallback rejection", got)
	}
}

// TestEnsureQuota_NoRejectionEventForHAStandby guards the classification
// boundary from the caller's side now that both rejection Events are
// emitted from guard sites rather than classified centrally in
// recordEnforcement: a transient/resource condition like ErrHAStandby (see
// TestEnsureQuota_SkipsMutationWhenStandby, ha_test.go) returns before
// either guard ever runs, so neither rejection reason may fire for it.
func TestEnsureQuota_NoRejectionEventForHAStandby(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())

	a := newTestAgent(t, fake.NewSimpleClientset())
	a.fsType = quota.FSTypeXFS
	a.SetHAActiveFile(filepath.Join(t.TempDir(), "does-not-exist"))
	fakeRec := events.NewFake(30 * time.Second)
	a.SetEventRecorder(fakeRec)

	pv := newBoundPV("pv-standby", "/exports/pvc-standby", 1)
	localPath := a.nfsPathToLocal("/exports/pvc-standby")
	if err := os.MkdirAll(localPath, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	err := a.ensureQuota(context.Background(), pv, 0)
	if !errors.Is(err, ErrHAStandby) {
		t.Fatalf("expected ErrHAStandby, got: %v", err)
	}

	if got := fakeRec.Count(pv.Name, events.QuotaShrinkRejected); got != 0 {
		t.Fatalf("QuotaShrinkRejected events = %d, want 0 for ErrHAStandby", got)
	}
	if got := fakeRec.Count(pv.Name, events.PolicyRejected); got != 0 {
		t.Fatalf("PolicyRejected events = %d, want 0 for ErrHAStandby", got)
	}
}
