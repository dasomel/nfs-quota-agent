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

package quotapolicy

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/dasomel/nfs-quota-agent/internal/apis/quota/v1alpha1"
)

func findCondition(conds []metav1.Condition, t string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == t {
			return &conds[i]
		}
	}
	return nil
}

func TestBuildStatus_ReadyReflectsSelectorValidity(t *testing.T) {
	valid := namedPolicy("ns", "valid", 100, v1alpha1.QuotaPolicySelector{})
	status := BuildStatus(&valid, nil, LimitRangeInfo{}, nil, metav1.Now())
	ready := findCondition(status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.Reason != v1alpha1.ReasonSelectorValid {
		t.Fatalf("expected Ready=True/SelectorValid, got %+v", ready)
	}

	invalid := namedPolicy("ns", "invalid", 100, v1alpha1.QuotaPolicySelector{
		LabelSelector: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "k", Operator: "NotReal"}},
		},
	})
	status = BuildStatus(&invalid, nil, LimitRangeInfo{}, nil, metav1.Now())
	ready = findCondition(status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != v1alpha1.ReasonSelectorInvalid {
		t.Fatalf("expected Ready=False/SelectorInvalid, got %+v", ready)
	}
}

func TestBuildStatus_AppliedVacuousWhenNoMatches(t *testing.T) {
	p := namedPolicy("ns", "p", 100, v1alpha1.QuotaPolicySelector{})
	status := BuildStatus(&p, nil, LimitRangeInfo{}, nil, metav1.Now())

	applied := findCondition(status.Conditions, v1alpha1.ConditionApplied)
	if applied == nil || applied.Status != metav1.ConditionTrue || applied.Reason != v1alpha1.ReasonNoMatchingClaims {
		t.Fatalf("expected Applied=True/NoMatchingClaims, got %+v", applied)
	}
	degraded := findCondition(status.Conditions, v1alpha1.ConditionDegraded)
	if degraded == nil || degraded.Status != metav1.ConditionFalse {
		t.Fatalf("expected Degraded=False, got %+v", degraded)
	}
	if status.MatchedClaims != 0 || status.AppliedClaims != 0 || status.ShadowedClaims != 0 {
		t.Fatalf("expected all-zero counts, got %+v", status)
	}
}

func TestBuildStatus_CountsAndConditions(t *testing.T) {
	p := namedPolicy("ns", "p", 100, v1alpha1.QuotaPolicySelector{})
	outcomes := []ClaimOutcome{
		{Claim: Claim{Namespace: "ns", Name: "a"}, MatchKind: v1alpha1.MatchKindNamespaceWide, Won: true},
		{Claim: Claim{Namespace: "ns", Name: "b"}, MatchKind: v1alpha1.MatchKindNamespaceWide, Won: true},
		{Claim: Claim{Namespace: "ns", Name: "c"}, MatchKind: v1alpha1.MatchKindPVCName, Won: false, ResolvedBy: "other"},
	}

	status := BuildStatus(&p, outcomes, LimitRangeInfo{}, nil, metav1.Now())

	if status.MatchedClaims != 3 {
		t.Fatalf("MatchedClaims = %d, want 3", status.MatchedClaims)
	}
	if status.AppliedClaims != 2 {
		t.Fatalf("AppliedClaims = %d, want 2", status.AppliedClaims)
	}
	if status.ShadowedClaims != 1 {
		t.Fatalf("ShadowedClaims = %d, want 1", status.ShadowedClaims)
	}
	applied := findCondition(status.Conditions, v1alpha1.ConditionApplied)
	if applied == nil || applied.Status != metav1.ConditionTrue || applied.Reason != v1alpha1.ReasonAllClaimsApplied {
		t.Fatalf("expected Applied=True/AllClaimsApplied, got %+v", applied)
	}
}

func TestBuildStatus_FailingClaimsMarkDegraded(t *testing.T) {
	p := namedPolicy("ns", "p", 100, v1alpha1.QuotaPolicySelector{})
	outcomes := []ClaimOutcome{
		{Claim: Claim{Namespace: "ns", Name: "a"}, Won: true},
		{Claim: Claim{Namespace: "ns", Name: "b"}, Won: true, EnforcementErr: errors.New("disk full"), EnforcementReason: v1alpha1.ReasonEnforcementFailed},
	}

	status := BuildStatus(&p, outcomes, LimitRangeInfo{}, nil, metav1.Now())

	applied := findCondition(status.Conditions, v1alpha1.ConditionApplied)
	if applied == nil || applied.Status != metav1.ConditionFalse || applied.Reason != v1alpha1.ReasonPartiallyApplied {
		t.Fatalf("expected Applied=False/PartiallyApplied, got %+v", applied)
	}
	degraded := findCondition(status.Conditions, v1alpha1.ConditionDegraded)
	if degraded == nil || degraded.Status != metav1.ConditionTrue || degraded.Reason != v1alpha1.ReasonEnforcementFailed {
		t.Fatalf("expected Degraded=True/EnforcementFailed, got %+v", degraded)
	}
	if len(status.FailingClaims) != 1 || status.FailingClaims[0].Name != "b" {
		t.Fatalf("expected exactly one failing claim 'b', got %+v", status.FailingClaims)
	}
}

func TestBuildStatus_DriftedClaimsMarkDrifted(t *testing.T) {
	p := namedPolicy("ns", "p", 100, v1alpha1.QuotaPolicySelector{})
	outcomes := []ClaimOutcome{
		{Claim: Claim{Namespace: "ns", Name: "a"}, Won: true},
		{Claim: Claim{Namespace: "ns", Name: "b"}, Won: true, DriftErr: errors.New("on-disk quota 500 does not match expected enforced value 1000")},
	}

	status := BuildStatus(&p, outcomes, LimitRangeInfo{}, nil, metav1.Now())

	// Applied must stay True: enforcement itself reported no error for
	// either claim -- Drifted is a separate axis, not a replacement for
	// Applied (see ConditionDrifted's doc comment: "Applied=True and
	// Drifted=True describes a policy that is enforced but has since
	// drifted out of band").
	applied := findCondition(status.Conditions, v1alpha1.ConditionApplied)
	if applied == nil || applied.Status != metav1.ConditionTrue {
		t.Fatalf("expected Applied=True (enforcement itself succeeded for both claims), got %+v", applied)
	}
	degraded := findCondition(status.Conditions, v1alpha1.ConditionDegraded)
	if degraded == nil || degraded.Status != metav1.ConditionFalse {
		t.Fatalf("expected Degraded=False (no EnforcementErr on either claim), got %+v", degraded)
	}

	drifted := findCondition(status.Conditions, v1alpha1.ConditionDrifted)
	if drifted == nil || drifted.Status != metav1.ConditionTrue || drifted.Reason != v1alpha1.ReasonQuotaDriftDetected {
		t.Fatalf("expected Drifted=True/QuotaDriftDetected, got %+v", drifted)
	}
	if len(status.DriftedClaims) != 1 || status.DriftedClaims[0].Name != "b" {
		t.Fatalf("expected exactly one drifted claim 'b', got %+v", status.DriftedClaims)
	}
	if status.DriftedClaims[0].Reason != v1alpha1.ReasonQuotaDriftDetected {
		t.Fatalf("expected drifted claim reason QuotaDriftDetected, got %q", status.DriftedClaims[0].Reason)
	}
}

func TestBuildStatus_DriftedFalseWhenNoDrift(t *testing.T) {
	p := namedPolicy("ns", "p", 100, v1alpha1.QuotaPolicySelector{})
	outcomes := []ClaimOutcome{
		{Claim: Claim{Namespace: "ns", Name: "a"}, Won: true},
	}

	status := BuildStatus(&p, outcomes, LimitRangeInfo{}, nil, metav1.Now())

	drifted := findCondition(status.Conditions, v1alpha1.ConditionDrifted)
	if drifted == nil || drifted.Status != metav1.ConditionFalse || drifted.Reason != v1alpha1.ReasonNoDrift {
		t.Fatalf("expected Drifted=False/NoDrift, got %+v", drifted)
	}
	if len(status.DriftedClaims) != 0 {
		t.Fatalf("expected no drifted claims, got %+v", status.DriftedClaims)
	}
}

// TestBuildStatus_EnforcementErrTakesPrecedenceOverDrift guards the
// invariant recordEnforcement (internal/agent/policy.go) is supposed to
// maintain -- DriftErr is only ever meaningful when EnforcementErr is nil
// -- defensively at the BuildStatus layer too, so a caller bug that sets
// both on the same outcome can't double-report the same underlying
// failure as both Degraded and Drifted.
func TestBuildStatus_EnforcementErrTakesPrecedenceOverDrift(t *testing.T) {
	p := namedPolicy("ns", "p", 100, v1alpha1.QuotaPolicySelector{})
	outcomes := []ClaimOutcome{
		{
			Claim:             Claim{Namespace: "ns", Name: "a"},
			Won:               true,
			EnforcementErr:    errors.New("disk full"),
			EnforcementReason: v1alpha1.ReasonEnforcementFailed,
			DriftErr:          errors.New("should be ignored"),
		},
	}

	status := BuildStatus(&p, outcomes, LimitRangeInfo{}, nil, metav1.Now())

	degraded := findCondition(status.Conditions, v1alpha1.ConditionDegraded)
	if degraded == nil || degraded.Status != metav1.ConditionTrue {
		t.Fatalf("expected Degraded=True, got %+v", degraded)
	}
	drifted := findCondition(status.Conditions, v1alpha1.ConditionDrifted)
	if drifted == nil || drifted.Status != metav1.ConditionFalse {
		t.Fatalf("expected Drifted=False when the same outcome already has an EnforcementErr, got %+v", drifted)
	}
	if len(status.DriftedClaims) != 0 {
		t.Fatalf("expected no drifted claims when EnforcementErr is set, got %+v", status.DriftedClaims)
	}
}

// TestBuildStatus_DriftedClaimsSampleCappedAt20 mirrors
// TestBuildStatus_FailingClaimsSampleCappedAt20's live-round-trip
// verification (see that test's doc comment for why: a real Kubernetes API
// server rejects the whole status write, not just the extra entry, if any
// bounded sample exceeds 20) for the new DriftedClaims sample.
func TestBuildStatus_DriftedClaimsSampleCappedAt20(t *testing.T) {
	p := namedPolicy("ns-b", "p", 100, v1alpha1.QuotaPolicySelector{})
	var outcomes []ClaimOutcome
	for i := 0; i < 25; i++ {
		outcomes = append(outcomes, ClaimOutcome{
			Claim:    Claim{Namespace: "ns-b", Name: "pvc"},
			Won:      true,
			DriftErr: errors.New("drift"),
		})
	}

	status := BuildStatus(&p, outcomes, LimitRangeInfo{}, nil, metav1.Now())
	if len(status.DriftedClaims) != maxStatusSampleEntries {
		t.Fatalf("DriftedClaims len = %d, want %d", len(status.DriftedClaims), maxStatusSampleEntries)
	}

	client := newFakeDynamicClient(t, &p)
	if err := WriteStatus(context.Background(), client, &p, status); err != nil {
		t.Fatalf("WriteStatus with a 20-entry-capped drifted sample must succeed, got: %v", err)
	}
	got, err := client.Resource(GroupVersionResource).Namespace("ns-b").Get(context.Background(), "p", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after WriteStatus: %v", err)
	}
	drifted, found, _ := unstructured.NestedSlice(got.Object, "status", "driftedClaims")
	if !found || len(drifted) != maxStatusSampleEntries {
		t.Fatalf("driftedClaims after write: found=%v len=%d, want %d", found, len(drifted), maxStatusSampleEntries)
	}
}

func TestBuildStatus_LimitRangeConflict(t *testing.T) {
	tests := []struct {
		name       string
		policy     *v1alpha1.QuotaPolicy
		lr         LimitRangeInfo
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name: "no LimitRange",
			policy: func() *v1alpha1.QuotaPolicy {
				p := namedPolicy("ns", "p", 100, v1alpha1.QuotaPolicySelector{})
				p.Spec.MaxQuota = gi(20)
				return &p
			}(),
			lr:         LimitRangeInfo{Present: false},
			wantStatus: metav1.ConditionFalse,
			wantReason: v1alpha1.ReasonNoLimitRange,
		},
		{
			name: "max conflict only",
			policy: func() *v1alpha1.QuotaPolicy {
				p := namedPolicy("ns", "p", 100, v1alpha1.QuotaPolicySelector{})
				p.Spec.MaxQuota = gi(20)
				return &p
			}(),
			lr: LimitRangeInfo{
				Present:  true,
				MaxBytes: 10 * 1024 * 1024 * 1024,
				MinBytes: 5 * 1024 * 1024 * 1024,
			},
			wantStatus: metav1.ConditionTrue,
			wantReason: v1alpha1.ReasonExceedsLimitRangeMax,
		},
		{
			name: "min>policy max",
			policy: func() *v1alpha1.QuotaPolicy {
				p := namedPolicy("ns", "p", 100, v1alpha1.QuotaPolicySelector{})
				p.Spec.MaxQuota = gi(2)
				return &p
			}(),
			lr: LimitRangeInfo{
				Present:  true,
				MaxBytes: 100 * 1024 * 1024 * 1024,
				MinBytes: 5 * 1024 * 1024 * 1024,
			},
			wantStatus: metav1.ConditionTrue,
			wantReason: v1alpha1.ReasonBelowLimitRangeMin,
		},
		{
			name: "policy min<LimitRange min",
			policy: func() *v1alpha1.QuotaPolicy {
				p := namedPolicy("ns", "p", 100, v1alpha1.QuotaPolicySelector{})
				p.Spec.MaxQuota = gi(20)
				p.Spec.MinQuota = gi(1)
				return &p
			}(),
			lr: LimitRangeInfo{
				Present:  true,
				MaxBytes: 30 * 1024 * 1024 * 1024,
				MinBytes: 5 * 1024 * 1024 * 1024,
			},
			wantStatus: metav1.ConditionTrue,
			wantReason: v1alpha1.ReasonMinQuotaBelowLimitRangeMin,
		},
		{
			name: "both max and min conflicts (precedence: max-conflict first over minQuota<min)",
			policy: func() *v1alpha1.QuotaPolicy {
				p := namedPolicy("ns", "p", 100, v1alpha1.QuotaPolicySelector{})
				p.Spec.MaxQuota = gi(40)
				p.Spec.MinQuota = gi(1)
				return &p
			}(),
			lr: LimitRangeInfo{
				Present:  true,
				MaxBytes: 30 * 1024 * 1024 * 1024,
				MinBytes: 5 * 1024 * 1024 * 1024,
			},
			wantStatus: metav1.ConditionTrue,
			wantReason: v1alpha1.ReasonExceedsLimitRangeMax,
		},
		{
			name: "both max and min conflicts (precedence: min>max over minQuota<min)",
			policy: func() *v1alpha1.QuotaPolicy {
				p := namedPolicy("ns", "p", 100, v1alpha1.QuotaPolicySelector{})
				p.Spec.MaxQuota = gi(2)
				p.Spec.MinQuota = gi(1)
				return &p
			}(),
			lr: LimitRangeInfo{
				Present:  true,
				MaxBytes: 100 * 1024 * 1024 * 1024,
				MinBytes: 5 * 1024 * 1024 * 1024,
			},
			wantStatus: metav1.ConditionTrue,
			wantReason: v1alpha1.ReasonBelowLimitRangeMin,
		},
		{
			name: "values exactly equal (no conflict — boundary)",
			policy: func() *v1alpha1.QuotaPolicy {
				p := namedPolicy("ns", "p", 100, v1alpha1.QuotaPolicySelector{})
				p.Spec.MaxQuota = gi(20)
				p.Spec.MinQuota = gi(5)
				return &p
			}(),
			lr: LimitRangeInfo{
				Present:  true,
				MaxBytes: 20 * 1024 * 1024 * 1024,
				MinBytes: 5 * 1024 * 1024 * 1024,
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: v1alpha1.ReasonWithinLimitRange,
		},
		{
			name: "LimitRange with no PVC type entry",
			policy: func() *v1alpha1.QuotaPolicy {
				p := namedPolicy("ns", "p", 100, v1alpha1.QuotaPolicySelector{})
				p.Spec.MaxQuota = gi(20)
				return &p
			}(),
			lr: LimitRangeInfo{
				Present:  true,
				MaxBytes: 0,
				MinBytes: 0,
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: v1alpha1.ReasonNoLimitRange,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := BuildStatus(tt.policy, nil, tt.lr, nil, metav1.Now())
			cond := findCondition(status.Conditions, v1alpha1.ConditionLimitRangeConflict)
			if cond == nil {
				t.Fatalf("condition %s not found", v1alpha1.ConditionLimitRangeConflict)
			}
			if cond.Status != tt.wantStatus || cond.Reason != tt.wantReason {
				t.Fatalf("expected Status=%s Reason=%s, got Status=%s Reason=%s (message: %s)",
					tt.wantStatus, tt.wantReason, cond.Status, cond.Reason, cond.Message)
			}
		})
	}
}

// TestBuildStatus_FailingClaimsSampleCappedAt20 also proves the *rest* of
// the status still lands when the sample is capped: verified live against a
// Kubernetes 1.35 API server, writing 21 status.matchedClaimSample entries
// doesn't just get the extra entry rejected — it fails the whole
// UpdateStatus ("Too many: 21: must have at most 20 items" plus "some
// validation rules were not checked because the object was invalid"),
// taking counts and conditions down with it. Capping in Go before the call
// (maxStatusSampleEntries) is what avoids ever sending an oversized list in
// the first place, so this asserts the call actually succeeds end-to-end
// through WriteStatus, not just that BuildStatus's return value is capped
// in memory.
func TestBuildStatus_FailingClaimsSampleCappedAt20(t *testing.T) {
	p := namedPolicy("ns-a", "p", 100, v1alpha1.QuotaPolicySelector{})
	var outcomes []ClaimOutcome
	for i := 0; i < 25; i++ {
		outcomes = append(outcomes, ClaimOutcome{
			Claim:             Claim{Namespace: "ns-a", Name: "pvc"},
			Won:               true,
			EnforcementErr:    errors.New("fail"),
			EnforcementReason: v1alpha1.ReasonEnforcementFailed,
		})
	}

	status := BuildStatus(&p, outcomes, LimitRangeInfo{}, nil, metav1.Now())
	if len(status.FailingClaims) != maxStatusSampleEntries {
		t.Fatalf("FailingClaims len = %d, want %d", len(status.FailingClaims), maxStatusSampleEntries)
	}
	if status.MatchedClaims != 25 || status.AppliedClaims != 0 {
		t.Fatalf("unbounded counts wrong despite the cap: matched=%d applied=%d, want 25/0", status.MatchedClaims, status.AppliedClaims)
	}
	if len(status.Conditions) == 0 {
		t.Fatalf("expected conditions to still be populated alongside the capped sample")
	}

	client := newFakeDynamicClient(t, &p)
	if err := WriteStatus(context.Background(), client, &p, status); err != nil {
		t.Fatalf("WriteStatus with a 20-entry-capped sample must succeed, got: %v", err)
	}
	got, err := client.Resource(GroupVersionResource).Namespace("ns-a").Get(context.Background(), "p", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after WriteStatus: %v", err)
	}
	matched, found, _ := unstructured.NestedInt64(got.Object, "status", "matchedClaims")
	if !found || matched != 25 {
		t.Fatalf("matchedClaims after write = %v (found=%v), want 25 — the rest of status must land alongside the capped sample", matched, found)
	}
	failing, found, _ := unstructured.NestedSlice(got.Object, "status", "failingClaims")
	if !found || len(failing) != maxStatusSampleEntries {
		t.Fatalf("failingClaims after write: found=%v len=%d, want %d", found, len(failing), maxStatusSampleEntries)
	}
}

// TestBuildStatus_MatchedClaimSampleCappedAt20 is the matchedClaimSample
// counterpart to the failingClaims test above — see its doc comment for why
// asserting an end-to-end WriteStatus matters, not just BuildStatus's
// in-memory return value.
func TestBuildStatus_MatchedClaimSampleCappedAt20(t *testing.T) {
	p := namedPolicy("ns-a", "p", 100, v1alpha1.QuotaPolicySelector{})
	var outcomes []ClaimOutcome
	for i := 0; i < 30; i++ {
		outcomes = append(outcomes, ClaimOutcome{
			Claim:     Claim{Namespace: "ns-a", Name: "pvc"},
			MatchKind: v1alpha1.MatchKindNamespaceWide,
			Won:       i%2 == 0,
		})
	}

	status := BuildStatus(&p, outcomes, LimitRangeInfo{}, nil, metav1.Now())
	if len(status.MatchedClaimSample) != maxStatusSampleEntries {
		t.Fatalf("MatchedClaimSample len = %d, want %d", len(status.MatchedClaimSample), maxStatusSampleEntries)
	}
	if status.MatchedClaims != 30 {
		t.Fatalf("MatchedClaims (unbounded count) = %d, want 30", status.MatchedClaims)
	}
	if status.AppliedClaims != 15 || status.ShadowedClaims != 15 {
		t.Fatalf("AppliedClaims/ShadowedClaims wrong despite the cap: got %d/%d, want 15/15", status.AppliedClaims, status.ShadowedClaims)
	}

	client := newFakeDynamicClient(t, &p)
	if err := WriteStatus(context.Background(), client, &p, status); err != nil {
		t.Fatalf("WriteStatus with a 20-entry-capped sample must succeed, got: %v", err)
	}
	got, err := client.Resource(GroupVersionResource).Namespace("ns-a").Get(context.Background(), "p", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after WriteStatus: %v", err)
	}
	matched, found, _ := unstructured.NestedInt64(got.Object, "status", "matchedClaims")
	if !found || matched != 30 {
		t.Fatalf("matchedClaims after write = %v (found=%v), want 30 — the rest of status must land alongside the capped sample", matched, found)
	}
	sample, found, _ := unstructured.NestedSlice(got.Object, "status", "matchedClaimSample")
	if !found || len(sample) != maxStatusSampleEntries {
		t.Fatalf("matchedClaimSample after write: found=%v len=%d, want %d", found, len(sample), maxStatusSampleEntries)
	}
}

func TestBuildStatus_PreservesLastTransitionTimeWhenUnchanged(t *testing.T) {
	p := namedPolicy("ns", "p", 100, v1alpha1.QuotaPolicySelector{})
	earlier := metav1.NewTime(metav1.Now().Add(-1 * 60 * 60 * 1e9)) // 1h before "now" below, in ns
	previous := []metav1.Condition{
		{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonSelectorValid, Message: "ok", LastTransitionTime: earlier},
	}

	status := BuildStatus(&p, nil, LimitRangeInfo{}, previous, metav1.Now())
	ready := findCondition(status.Conditions, v1alpha1.ConditionReady)
	if ready == nil {
		t.Fatalf("expected a Ready condition")
	}
	if !ready.LastTransitionTime.Equal(&earlier) {
		t.Fatalf("expected LastTransitionTime to be preserved at %v, got %v", earlier, ready.LastTransitionTime)
	}
}
