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

package policy

import (
	"context"
	"errors"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func newNamespace(name string, annotations map[string]string) *v1.Namespace {
	return &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: annotations,
		},
	}
}

func newLimitRangeForPVC(name, namespace string, max, min, def, defReq string) *v1.LimitRange {
	limit := v1.LimitRangeItem{
		Type: v1.LimitTypePersistentVolumeClaim,
	}
	if max != "" {
		limit.Max = v1.ResourceList{v1.ResourceStorage: resource.MustParse(max)}
	}
	if min != "" {
		limit.Min = v1.ResourceList{v1.ResourceStorage: resource.MustParse(min)}
	}
	if def != "" {
		limit.Default = v1.ResourceList{v1.ResourceStorage: resource.MustParse(def)}
	}
	if defReq != "" {
		limit.DefaultRequest = v1.ResourceList{v1.ResourceStorage: resource.MustParse(defReq)}
	}
	return &v1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       v1.LimitRangeSpec{Limits: []v1.LimitRangeItem{limit}},
	}
}

func newResourceQuota(name, namespace, hard, used string) *v1.ResourceQuota {
	rq := &v1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1.ResourceQuotaSpec{
			Hard: v1.ResourceList{v1.ResourceRequestsStorage: resource.MustParse(hard)},
		},
	}
	if used != "" {
		rq.Status.Used = v1.ResourceList{v1.ResourceRequestsStorage: resource.MustParse(used)}
	}
	return rq
}

func TestGetNamespacePolicy_NilClient(t *testing.T) {
	p, err := GetNamespacePolicy(context.Background(), nil, "default")
	if err == nil {
		t.Fatal("expected error for nil client, got nil")
	}
	if p != nil {
		t.Errorf("expected nil policy, got %+v", p)
	}
}

func TestGetNamespacePolicy_NoPolicy(t *testing.T) {
	client := fake.NewSimpleClientset(newNamespace("default", nil))

	p, err := GetNamespacePolicy(context.Background(), client, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Source != "None" {
		t.Errorf("expected source None, got %s", p.Source)
	}
	if p.DefaultQuota != 0 || p.MaxQuota != 0 || p.MinQuota != 0 {
		t.Errorf("expected zero quotas, got %+v", p)
	}
}

func TestGetNamespacePolicy_MissingNamespace(t *testing.T) {
	client := fake.NewSimpleClientset()

	p, err := GetNamespacePolicy(context.Background(), client, "does-not-exist")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Source != "None" {
		t.Errorf("expected source None for missing namespace, got %s", p.Source)
	}
}

func TestGetNamespacePolicy_LimitRangePriority(t *testing.T) {
	// LimitRange should take priority over annotations even when both present.
	ns := newNamespace("ns1", map[string]string{
		AnnotationDefaultQuota: "1Gi",
		AnnotationMaxQuota:     "2Gi",
	})
	lr := newLimitRangeForPVC("lr1", "ns1", "10Gi", "1Gi", "5Gi", "")

	client := fake.NewSimpleClientset(ns, lr)

	p, err := GetNamespacePolicy(context.Background(), client, "ns1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Source != "LimitRange" {
		t.Errorf("expected source LimitRange, got %s", p.Source)
	}
	if p.MaxQuota != 10*1024*1024*1024 {
		t.Errorf("expected max 10Gi, got %d", p.MaxQuota)
	}
	if p.MinQuota != 1024*1024*1024 {
		t.Errorf("expected min 1Gi, got %d", p.MinQuota)
	}
	if p.DefaultQuota != 5*1024*1024*1024 {
		t.Errorf("expected default 5Gi, got %d", p.DefaultQuota)
	}
	if p.LimitRangeName != "lr1" {
		t.Errorf("expected LimitRangeName lr1, got %s", p.LimitRangeName)
	}
}

func TestGetNamespacePolicy_LimitRangeDefaultRequestFallback(t *testing.T) {
	// DefaultRequest should only be used when Default is not set (DefaultQuota == 0).
	lr := newLimitRangeForPVC("lr1", "ns1", "10Gi", "", "", "3Gi")
	client := fake.NewSimpleClientset(lr)

	p, err := GetNamespacePolicy(context.Background(), client, "ns1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.DefaultQuota != 3*1024*1024*1024 {
		t.Errorf("expected DefaultQuota from DefaultRequest fallback (3Gi), got %d", p.DefaultQuota)
	}
}

func TestGetNamespacePolicy_LimitRangeDefaultTakesPriorityOverDefaultRequest(t *testing.T) {
	lr := newLimitRangeForPVC("lr1", "ns1", "10Gi", "", "5Gi", "3Gi")
	client := fake.NewSimpleClientset(lr)

	p, err := GetNamespacePolicy(context.Background(), client, "ns1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.DefaultQuota != 5*1024*1024*1024 {
		t.Errorf("expected DefaultQuota to remain from Default (5Gi), got %d", p.DefaultQuota)
	}
}

func TestGetNamespacePolicy_LimitRangeIgnoresNonPVCType(t *testing.T) {
	lr := &v1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Name: "lr1", Namespace: "ns1"},
		Spec: v1.LimitRangeSpec{
			Limits: []v1.LimitRangeItem{
				{
					Type: v1.LimitTypeContainer,
					Max:  v1.ResourceList{v1.ResourceCPU: resource.MustParse("2")},
				},
			},
		},
	}
	client := fake.NewSimpleClientset(lr)

	p, err := GetNamespacePolicy(context.Background(), client, "ns1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Source != "None" {
		t.Errorf("expected source None when LimitRange has no PVC-type limit, got %s", p.Source)
	}
}

func TestGetNamespacePolicy_AnnotationFallback(t *testing.T) {
	ns := newNamespace("ns1", map[string]string{
		AnnotationDefaultQuota: "1Gi",
		AnnotationMaxQuota:     "2Gi",
	})
	client := fake.NewSimpleClientset(ns)

	p, err := GetNamespacePolicy(context.Background(), client, "ns1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Source != "Annotation" {
		t.Errorf("expected source Annotation, got %s", p.Source)
	}
	if p.DefaultQuota != 1024*1024*1024 {
		t.Errorf("expected default 1Gi, got %d", p.DefaultQuota)
	}
	if p.MaxQuota != 2*1024*1024*1024 {
		t.Errorf("expected max 2Gi, got %d", p.MaxQuota)
	}
}

func TestGetNamespacePolicy_AnnotationOnlyDefault(t *testing.T) {
	ns := newNamespace("ns1", map[string]string{
		AnnotationDefaultQuota: "1Gi",
	})
	client := fake.NewSimpleClientset(ns)

	p, err := GetNamespacePolicy(context.Background(), client, "ns1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Source != "Annotation" {
		t.Errorf("expected source Annotation, got %s", p.Source)
	}
	if p.MaxQuota != 0 {
		t.Errorf("expected max 0 when annotation absent, got %d", p.MaxQuota)
	}
}

func TestGetNamespacePolicy_InvalidAnnotationValues(t *testing.T) {
	// Invalid annotation values should be skipped (logged as warning), not error out,
	// and should not flip Source to "Annotation" since nothing was parsed.
	ns := newNamespace("ns1", map[string]string{
		AnnotationDefaultQuota: "not-a-size",
		AnnotationMaxQuota:     "also-invalid",
	})
	client := fake.NewSimpleClientset(ns)

	p, err := GetNamespacePolicy(context.Background(), client, "ns1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Source != "None" {
		t.Errorf("expected source None when all annotations invalid, got %s", p.Source)
	}
	if p.DefaultQuota != 0 || p.MaxQuota != 0 {
		t.Errorf("expected zero quotas when annotations invalid, got default=%d max=%d", p.DefaultQuota, p.MaxQuota)
	}
}

func TestGetNamespacePolicy_PartiallyInvalidAnnotations(t *testing.T) {
	// One valid, one invalid: valid one should still be applied and set Source.
	ns := newNamespace("ns1", map[string]string{
		AnnotationDefaultQuota: "1Gi",
		AnnotationMaxQuota:     "bogus",
	})
	client := fake.NewSimpleClientset(ns)

	p, err := GetNamespacePolicy(context.Background(), client, "ns1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Source != "Annotation" {
		t.Errorf("expected source Annotation, got %s", p.Source)
	}
	if p.DefaultQuota != 1024*1024*1024 {
		t.Errorf("expected default 1Gi, got %d", p.DefaultQuota)
	}
	if p.MaxQuota != 0 {
		t.Errorf("expected max quota to remain 0 for invalid value, got %d", p.MaxQuota)
	}
}

func TestGetNamespacePolicy_ResourceQuotaAttachedRegardlessOfSource(t *testing.T) {
	ns := newNamespace("ns1", map[string]string{
		AnnotationDefaultQuota: "1Gi",
	})
	rq := newResourceQuota("rq1", "ns1", "100Gi", "40Gi")
	client := fake.NewSimpleClientset(ns, rq)

	p, err := GetNamespacePolicy(context.Background(), client, "ns1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.ResourceQuotaName != "rq1" {
		t.Errorf("expected ResourceQuotaName rq1, got %s", p.ResourceQuotaName)
	}
	if p.ResourceQuotaHard != 100*1024*1024*1024 {
		t.Errorf("expected hard 100Gi, got %d", p.ResourceQuotaHard)
	}
	if p.ResourceQuotaUsed != 40*1024*1024*1024 {
		t.Errorf("expected used 40Gi, got %d", p.ResourceQuotaUsed)
	}
	// Annotation-derived source should still be set independent of ResourceQuota presence.
	if p.Source != "Annotation" {
		t.Errorf("expected source Annotation, got %s", p.Source)
	}
}

func TestGetNamespacePolicy_ResourceQuotaWithoutStorageKey(t *testing.T) {
	rq := &v1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "rq1", Namespace: "ns1"},
		Spec: v1.ResourceQuotaSpec{
			Hard: v1.ResourceList{v1.ResourceRequestsCPU: resource.MustParse("4")},
		},
	}
	client := fake.NewSimpleClientset(rq)

	p, err := GetNamespacePolicy(context.Background(), client, "ns1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.ResourceQuotaName != "" {
		t.Errorf("expected no ResourceQuotaName without storage key, got %s", p.ResourceQuotaName)
	}
}

func TestGetAllNamespacePolicies_NilClient(t *testing.T) {
	_, err := GetAllNamespacePolicies(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestGetAllNamespacePolicies_AggregatesAllSources(t *testing.T) {
	lr := newLimitRangeForPVC("lr1", "ns-lr", "10Gi", "", "", "")
	rq := newResourceQuota("rq1", "ns-rq", "50Gi", "")
	nsAnnotated := newNamespace("ns-anno", map[string]string{
		AnnotationDefaultQuota: "2Gi",
	})
	nsPlain := newNamespace("ns-plain", nil)

	client := fake.NewSimpleClientset(lr, rq, nsAnnotated, nsPlain)

	policies, err := GetAllNamespacePolicies(context.Background(), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := map[string]bool{}
	for _, p := range policies {
		found[p.Namespace] = true
	}

	if !found["ns-lr"] {
		t.Error("expected ns-lr to be included (has LimitRange)")
	}
	if !found["ns-rq"] {
		t.Error("expected ns-rq to be included (has ResourceQuota)")
	}
	if !found["ns-anno"] {
		t.Error("expected ns-anno to be included (has annotation)")
	}
	if found["ns-plain"] {
		t.Error("expected ns-plain to be excluded (no policy)")
	}
}

func TestGetAllNamespacePolicies_EmptyClusterReturnsEmpty(t *testing.T) {
	client := fake.NewSimpleClientset()

	policies, err := GetAllNamespacePolicies(context.Background(), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(policies) != 0 {
		t.Errorf("expected no policies, got %d", len(policies))
	}
}

func TestGetViolations_NilClient(t *testing.T) {
	_, err := GetViolations(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for nil client")
	}
}

func newPV(name, namespace, claimName, capacity string) *v1.PersistentVolume {
	return &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1.PersistentVolumeSpec{
			Capacity: v1.ResourceList{v1.ResourceStorage: resource.MustParse(capacity)},
			ClaimRef: &v1.ObjectReference{
				Namespace: namespace,
				Name:      claimName,
			},
		},
	}
}

func TestGetViolations_ExceedsMax(t *testing.T) {
	lr := newLimitRangeForPVC("lr1", "ns1", "10Gi", "", "", "")
	pv := newPV("pv1", "ns1", "pvc1", "20Gi")
	client := fake.NewSimpleClientset(lr, pv)

	violations, err := GetViolations(context.Background(), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation, got %d", len(violations))
	}
	v := violations[0]
	if v.ViolationType != "exceeds_max" {
		t.Errorf("expected exceeds_max, got %s", v.ViolationType)
	}
	if v.Namespace != "ns1" || v.PVCName != "pvc1" || v.PVName != "pv1" {
		t.Errorf("unexpected violation identity: %+v", v)
	}
}

func TestGetViolations_BelowMin(t *testing.T) {
	lr := newLimitRangeForPVC("lr1", "ns1", "", "5Gi", "", "")
	pv := newPV("pv1", "ns1", "pvc1", "1Gi")
	client := fake.NewSimpleClientset(lr, pv)

	violations, err := GetViolations(context.Background(), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation, got %d", len(violations))
	}
	if violations[0].ViolationType != "below_min" {
		t.Errorf("expected below_min, got %s", violations[0].ViolationType)
	}
}

func TestGetViolations_NoViolationWithinBounds(t *testing.T) {
	lr := newLimitRangeForPVC("lr1", "ns1", "10Gi", "1Gi", "", "")
	pv := newPV("pv1", "ns1", "pvc1", "5Gi")
	client := fake.NewSimpleClientset(lr, pv)

	violations, err := GetViolations(context.Background(), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(violations) != 0 {
		t.Errorf("expected no violations, got %d", len(violations))
	}
}

func TestGetViolations_SkipsPVWithoutClaimRef(t *testing.T) {
	lr := newLimitRangeForPVC("lr1", "ns1", "1Gi", "", "", "")
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv1"},
		Spec: v1.PersistentVolumeSpec{
			Capacity: v1.ResourceList{v1.ResourceStorage: resource.MustParse("100Gi")},
			// No ClaimRef.
		},
	}
	client := fake.NewSimpleClientset(lr, pv)

	violations, err := GetViolations(context.Background(), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(violations) != 0 {
		t.Errorf("expected PV without ClaimRef to be skipped, got %d violations", len(violations))
	}
}

func TestGetViolations_SkipsPVWithoutCapacity(t *testing.T) {
	lr := newLimitRangeForPVC("lr1", "ns1", "1Gi", "", "", "")
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv1"},
		Spec: v1.PersistentVolumeSpec{
			ClaimRef: &v1.ObjectReference{Namespace: "ns1", Name: "pvc1"},
			// No Capacity.
		},
	}
	client := fake.NewSimpleClientset(lr, pv)

	violations, err := GetViolations(context.Background(), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(violations) != 0 {
		t.Errorf("expected PV without capacity to be skipped, got %d violations", len(violations))
	}
}

func TestGetViolations_CachesPolicyPerNamespace(t *testing.T) {
	// Two PVCs in the same namespace should reuse the cached policy (only one
	// GetNamespacePolicy resolution should be needed); verify via consistent results.
	lr := newLimitRangeForPVC("lr1", "ns1", "10Gi", "", "", "")
	pv1 := newPV("pv1", "ns1", "pvc1", "20Gi")
	pv2 := newPV("pv2", "ns1", "pvc2", "30Gi")
	client := fake.NewSimpleClientset(lr, pv1, pv2)

	violations, err := GetViolations(context.Background(), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(violations) != 2 {
		t.Fatalf("expected 2 violations, got %d", len(violations))
	}
	for _, v := range violations {
		if v.MaxQuotaBytes != 10*1024*1024*1024 {
			t.Errorf("expected consistent cached max quota, got %d", v.MaxQuotaBytes)
		}
	}
}

func TestGetViolations_MultipleNamespacesIndependentPolicies(t *testing.T) {
	lrA := newLimitRangeForPVC("lrA", "ns-a", "5Gi", "", "", "")
	lrB := newLimitRangeForPVC("lrB", "ns-b", "50Gi", "", "", "")
	pvA := newPV("pvA", "ns-a", "pvcA", "10Gi") // exceeds ns-a max
	pvB := newPV("pvB", "ns-b", "pvcB", "10Gi") // within ns-b max

	client := fake.NewSimpleClientset(lrA, lrB, pvA, pvB)

	violations, err := GetViolations(context.Background(), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation, got %d", len(violations))
	}
	if violations[0].Namespace != "ns-a" {
		t.Errorf("expected violation in ns-a, got %s", violations[0].Namespace)
	}
}

// errReactorClientset wraps the fake clientset behavior indirectly to assert
// error propagation paths that are otherwise unreachable through the public API
// alone. We use it only to confirm GetViolations propagates a hard PV-list failure.
func TestGetViolations_PropagatesPVListError(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("list", "persistentvolumes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("simulated list failure")
	})

	_, err := GetViolations(context.Background(), client)
	if err == nil {
		t.Fatal("expected error when PV listing fails")
	}
}

func TestGetNamespacePolicy_MultipleLimitRanges_MinsOrderIndependent(t *testing.T) {
	for _, order := range []string{"lr1-then-lr2", "lr2-then-lr1"} {
		t.Run(order, func(t *testing.T) {
			lr1 := newLimitRangeForPVC("lr1", "ns1", "20Gi", "1Gi", "", "")
			lr2 := newLimitRangeForPVC("lr2", "ns1", "20Gi", "5Gi", "", "")

			var client *fake.Clientset
			if order == "lr1-then-lr2" {
				client = fake.NewSimpleClientset(lr1, lr2)
			} else {
				client = fake.NewSimpleClientset(lr2, lr1)
			}

			p, err := GetNamespacePolicy(context.Background(), client, "ns1")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p.Source != "LimitRange" {
				t.Errorf("expected source LimitRange, got %s", p.Source)
			}
			const wantMin = 5 * 1024 * 1024 * 1024
			if p.MinQuota != wantMin {
				t.Errorf("expected MinQuota %d (5Gi), got %d", wantMin, p.MinQuota)
			}
			if p.LimitRangeMin != wantMin {
				t.Errorf("expected LimitRangeMin %d (5Gi), got %d", wantMin, p.LimitRangeMin)
			}
		})
	}
}

func TestGetNamespacePolicy_MultipleLimitRanges_MaxesOrderIndependent(t *testing.T) {
	for _, order := range []string{"lr1-then-lr2", "lr2-then-lr1"} {
		t.Run(order, func(t *testing.T) {
			lr1 := newLimitRangeForPVC("lr1", "ns1", "20Gi", "1Gi", "", "")
			lr2 := newLimitRangeForPVC("lr2", "ns1", "10Gi", "1Gi", "", "")

			var client *fake.Clientset
			if order == "lr1-then-lr2" {
				client = fake.NewSimpleClientset(lr1, lr2)
			} else {
				client = fake.NewSimpleClientset(lr2, lr1)
			}

			p, err := GetNamespacePolicy(context.Background(), client, "ns1")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p.Source != "LimitRange" {
				t.Errorf("expected source LimitRange, got %s", p.Source)
			}
			const wantMax = 10 * 1024 * 1024 * 1024
			if p.MaxQuota != wantMax {
				t.Errorf("expected MaxQuota %d (10Gi), got %d", wantMax, p.MaxQuota)
			}
			if p.LimitRangeMax != wantMax {
				t.Errorf("expected LimitRangeMax %d (10Gi), got %d", wantMax, p.LimitRangeMax)
			}
		})
	}
}

func TestGetNamespacePolicy_SingleLimitRange_MultiplePVCItems(t *testing.T) {
	lr := &v1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Name: "lr-multi", Namespace: "ns1"},
		Spec: v1.LimitRangeSpec{
			Limits: []v1.LimitRangeItem{
				{
					Type: v1.LimitTypePersistentVolumeClaim,
					Min:  v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Gi")},
					Max:  v1.ResourceList{v1.ResourceStorage: resource.MustParse("20Gi")},
				},
				{
					Type: v1.LimitTypePersistentVolumeClaim,
					Min:  v1.ResourceList{v1.ResourceStorage: resource.MustParse("5Gi")},
					Max:  v1.ResourceList{v1.ResourceStorage: resource.MustParse("10Gi")},
				},
			},
		},
	}
	client := fake.NewSimpleClientset(lr)

	p, err := GetNamespacePolicy(context.Background(), client, "ns1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Source != "LimitRange" {
		t.Errorf("expected source LimitRange, got %s", p.Source)
	}
	const wantMin = 5 * 1024 * 1024 * 1024
	const wantMax = 10 * 1024 * 1024 * 1024
	if p.MinQuota != wantMin {
		t.Errorf("expected MinQuota %d (5Gi), got %d", wantMin, p.MinQuota)
	}
	if p.LimitRangeMin != wantMin {
		t.Errorf("expected LimitRangeMin %d (5Gi), got %d", wantMin, p.LimitRangeMin)
	}
	if p.MaxQuota != wantMax {
		t.Errorf("expected MaxQuota %d (10Gi), got %d", wantMax, p.MaxQuota)
	}
	if p.LimitRangeMax != wantMax {
		t.Errorf("expected LimitRangeMax %d (10Gi), got %d", wantMax, p.LimitRangeMax)
	}
}

// Ensure kubernetes.Interface is satisfied by the fake clientset used throughout
// (compile-time check that our helpers build the right type).
var _ kubernetes.Interface = fake.NewSimpleClientset()
