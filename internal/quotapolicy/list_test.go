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
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/dasomel/nfs-quota-agent/internal/apis/quota/v1alpha1"
)

// newFakeDynamicClient builds a dynamic fake client that knows how to list
// QuotaPolicy/QuotaPolicyList, pre-loaded with objects. QuotaPolicy does not
// register a runtime.Scheme of its own yet (see the package doc comment in
// internal/apis/quota/v1alpha1/types.go), so tests build one locally, same
// as any other dynamic-client consumer of an unregistered CRD type.
func newFakeDynamicClient(t *testing.T, objects ...*v1alpha1.QuotaPolicy) *dynamicfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(GroupVersionResource.GroupVersion().WithKind("QuotaPolicy"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(GroupVersionResource.GroupVersion().WithKind("QuotaPolicyList"), &unstructured.UnstructuredList{})

	gvrToListKind := map[schema.GroupVersionResource]string{GroupVersionResource: "QuotaPolicyList"}

	var runtimeObjs []runtime.Object
	for _, o := range objects {
		runtimeObjs = append(runtimeObjs, toUnstructured(t, o))
	}

	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind, runtimeObjs...)
}

func toUnstructured(t *testing.T, p *v1alpha1.QuotaPolicy) *unstructured.Unstructured {
	t.Helper()
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(p)
	if err != nil {
		t.Fatalf("ToUnstructured: %v", err)
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetAPIVersion(v1alpha1.GroupName + "/" + v1alpha1.GroupVersion)
	u.SetKind(v1alpha1.QuotaPolicyKind)
	return u
}

func TestList_ReturnsConvertedPolicies(t *testing.T) {
	p1 := namedPolicy("ns-a", "one", 100, v1alpha1.QuotaPolicySelector{})
	p2 := namedPolicy("ns-b", "two", 50, pvcNameSelector("some-pvc"))
	client := newFakeDynamicClient(t, &p1, &p2)

	policies, err := List(context.Background(), client)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(policies) != 2 {
		t.Fatalf("expected 2 policies, got %d: %+v", len(policies), policies)
	}

	byName := map[string]v1alpha1.QuotaPolicy{}
	for _, p := range policies {
		byName[p.Name] = p
	}
	if byName["one"].Namespace != "ns-a" || byName["one"].Spec.Priority != 100 {
		t.Fatalf("policy 'one' round-tripped incorrectly: %+v", byName["one"])
	}
	if byName["two"].Namespace != "ns-b" || byName["two"].Spec.Selector.PVCName == nil || *byName["two"].Spec.Selector.PVCName != "some-pvc" {
		t.Fatalf("policy 'two' round-tripped incorrectly: %+v", byName["two"])
	}
}

// TestList_QuantityRoundTrip proves resource.Quantity values survive the
// path this package actually uses in production: Go struct -> unstructured
// (ToUnstructured, done by newFakeDynamicClient the same way the real
// dynamic client's List response would arrive) -> back to a typed
// v1alpha1.QuotaPolicy via List's FromUnstructured. A test built only from
// typed structs never exercises this conversion at all and would not catch
// a regression here — e.g. a naive implementation that round-tripped
// Quantity through its int64 millivalue would silently lose the original
// suffix/precision even when .Value() still compared equal.
func TestList_QuantityRoundTrip(t *testing.T) {
	// Same set verified directly against a live Kubernetes 1.35 cluster:
	// .Value() and .String() are both preserved for each of these.
	quantities := []string{"1Gi", "10Gi", "9Gi", "1000Mi", "500M", "1536Mi", "0"}

	for _, qs := range quantities {
		t.Run(qs, func(t *testing.T) {
			q := resource.MustParse(qs)
			p := namedPolicy("ns-a", "p", 100, v1alpha1.QuotaPolicySelector{})
			p.Spec.MaxQuota = &q

			client := newFakeDynamicClient(t, &p)
			policies, err := List(context.Background(), client)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(policies) != 1 || policies[0].Spec.MaxQuota == nil {
				t.Fatalf("expected exactly one policy with MaxQuota set, got %+v", policies)
			}

			got := policies[0].Spec.MaxQuota
			if got.Value() != q.Value() {
				t.Fatalf("Value() = %d, want %d", got.Value(), q.Value())
			}
			if got.String() != q.String() {
				t.Fatalf("String() = %q, want %q", got.String(), q.String())
			}
		})
	}
}

func TestList_EmptyWhenNoPolicies(t *testing.T) {
	client := newFakeDynamicClient(t)
	policies, err := List(context.Background(), client)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(policies) != 0 {
		t.Fatalf("expected no policies, got %+v", policies)
	}
}

// TestList_CRDNotInstalled_NotFound covers the degrade path: the API server
// has no route for quotapolicies (CRD never applied), which client-go
// surfaces as a NotFound error on the list call. List must return an empty
// slice and no error -- never fail the sync cycle over a missing CRD.
func TestList_CRDNotInstalled_NotFound(t *testing.T) {
	client := newFakeDynamicClient(t)
	client.PrependReactor("list", "quotapolicies", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: v1alpha1.GroupName, Resource: "quotapolicies"}, "")
	})

	policies, err := List(context.Background(), client)
	if err != nil {
		t.Fatalf("expected no error for a missing CRD, got %v", err)
	}
	if policies != nil {
		t.Fatalf("expected nil policies for a missing CRD, got %+v", policies)
	}
}

// TestList_CRDNotInstalled_NoMatch covers the other shape a missing CRD can
// take: the RESTMapper has no route at all (meta.IsNoMatchError), which
// must degrade identically to the NotFound case.
func TestList_CRDNotInstalled_NoMatch(t *testing.T) {
	client := newFakeDynamicClient(t)
	client.PrependReactor("list", "quotapolicies", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, &meta.NoResourceMatchError{PartialResource: GroupVersionResource}
	})

	policies, err := List(context.Background(), client)
	if err != nil {
		t.Fatalf("expected no error for a NoMatch error, got %v", err)
	}
	if policies != nil {
		t.Fatalf("expected nil policies, got %+v", policies)
	}
}

func TestList_OtherErrorPropagates(t *testing.T) {
	client := newFakeDynamicClient(t)
	client.PrependReactor("list", "quotapolicies", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errString("boom"))
	})

	_, err := List(context.Background(), client)
	if err == nil {
		t.Fatalf("expected a non-nil error to propagate")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestWriteStatus_RoundTrips(t *testing.T) {
	p := namedPolicy("ns-a", "one", 100, v1alpha1.QuotaPolicySelector{})
	p.Generation = 3
	client := newFakeDynamicClient(t, &p)

	status := v1alpha1.QuotaPolicyStatus{
		ObservedGeneration: 3,
		MatchedClaims:      2,
		AppliedClaims:      1,
		ShadowedClaims:     1,
		Conditions: []metav1.Condition{
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonSelectorValid, Message: "ok", LastTransitionTime: metav1.Now()},
		},
	}

	if err := WriteStatus(context.Background(), client, &p, status); err != nil {
		t.Fatalf("WriteStatus: %v", err)
	}

	got, err := client.Resource(GroupVersionResource).Namespace("ns-a").Get(context.Background(), "one", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after WriteStatus: %v", err)
	}
	matched, found, err := unstructured.NestedInt64(got.Object, "status", "matchedClaims")
	if err != nil || !found {
		t.Fatalf("matchedClaims not found in written status: found=%v err=%v obj=%+v", found, err, got.Object)
	}
	if matched != 2 {
		t.Fatalf("matchedClaims = %d, want 2", matched)
	}
}

// TestWriteStatus_RetriesOnConflict proves WriteStatus survives a single
// resourceVersion conflict (e.g. the CR's owner edited spec between our Get
// and UpdateStatus) by re-Getting and retrying once, rather than failing
// the whole sync cycle over a benign, common race.
func TestWriteStatus_RetriesOnConflict(t *testing.T) {
	p := namedPolicy("ns-a", "one", 100, v1alpha1.QuotaPolicySelector{})
	client := newFakeDynamicClient(t, &p)

	var updateStatusCalls int
	client.PrependReactor("update", "quotapolicies", func(action ktesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "status" {
			return false, nil, nil // not our concern; let the default reactor handle it
		}
		updateStatusCalls++
		if updateStatusCalls == 1 {
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Group: v1alpha1.GroupName, Resource: "quotapolicies"}, "one", errString("simulated conflicting write"))
		}
		return false, nil, nil // second attempt: let the default reactor actually apply it
	})

	status := v1alpha1.QuotaPolicyStatus{MatchedClaims: 5}
	if err := WriteStatus(context.Background(), client, &p, status); err != nil {
		t.Fatalf("WriteStatus: %v", err)
	}
	if updateStatusCalls != 2 {
		t.Fatalf("expected exactly 2 UpdateStatus attempts (1 conflict + 1 retry), got %d", updateStatusCalls)
	}

	got, err := client.Resource(GroupVersionResource).Namespace("ns-a").Get(context.Background(), "one", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after WriteStatus: %v", err)
	}
	matched, found, err := unstructured.NestedInt64(got.Object, "status", "matchedClaims")
	if err != nil || !found || matched != 5 {
		t.Fatalf("matchedClaims after retry: found=%v err=%v value=%d, want 5", found, err, matched)
	}
}

// TestWriteStatus_ConflictExhaustsRetriesReturnsError proves a persistent
// conflict (every attempt collides) surfaces as an error rather than
// silently dropping the status update.
func TestWriteStatus_ConflictExhaustsRetriesReturnsError(t *testing.T) {
	p := namedPolicy("ns-a", "one", 100, v1alpha1.QuotaPolicySelector{})
	client := newFakeDynamicClient(t, &p)

	client.PrependReactor("update", "quotapolicies", func(action ktesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "status" {
			return false, nil, nil
		}
		return true, nil, apierrors.NewConflict(
			schema.GroupResource{Group: v1alpha1.GroupName, Resource: "quotapolicies"}, "one", errString("simulated conflicting write"))
	})

	err := WriteStatus(context.Background(), client, &p, v1alpha1.QuotaPolicyStatus{MatchedClaims: 5})
	if err == nil {
		t.Fatalf("expected an error when every UpdateStatus attempt conflicts")
	}
}
