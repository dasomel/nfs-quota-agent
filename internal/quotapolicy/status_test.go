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

func TestBuildStatus_LimitRangeConflict(t *testing.T) {
	p := namedPolicy("ns", "p", 100, v1alpha1.QuotaPolicySelector{})
	p.Spec.MaxQuota = gi(20)

	// Exceeds the namespace LimitRange max.
	status := BuildStatus(&p, nil, LimitRangeInfo{Present: true, MaxBytes: 10 * 1024 * 1024 * 1024}, nil, metav1.Now())
	cond := findCondition(status.Conditions, v1alpha1.ConditionLimitRangeConflict)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != v1alpha1.ReasonExceedsLimitRangeMax {
		t.Fatalf("expected LimitRangeConflict=True/ExceedsLimitRangeMax, got %+v", cond)
	}

	// Within the namespace LimitRange max.
	status = BuildStatus(&p, nil, LimitRangeInfo{Present: true, MaxBytes: 30 * 1024 * 1024 * 1024}, nil, metav1.Now())
	cond = findCondition(status.Conditions, v1alpha1.ConditionLimitRangeConflict)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonWithinLimitRange {
		t.Fatalf("expected LimitRangeConflict=False/WithinLimitRange, got %+v", cond)
	}

	// No LimitRange at all.
	status = BuildStatus(&p, nil, LimitRangeInfo{Present: false}, nil, metav1.Now())
	cond = findCondition(status.Conditions, v1alpha1.ConditionLimitRangeConflict)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonNoLimitRange {
		t.Fatalf("expected LimitRangeConflict=False/NoLimitRange, got %+v", cond)
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
