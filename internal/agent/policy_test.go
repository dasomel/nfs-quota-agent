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
	"os"
	"path/filepath"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dasomel/nfs-quota-agent/internal/apis/quota/v1alpha1"
	"github.com/dasomel/nfs-quota-agent/internal/quota"
	"github.com/dasomel/nfs-quota-agent/internal/quotapolicy"
)

// newFakeQuotaPolicyClient builds a dynamic fake client pre-loaded with
// policies, usable as the agent's dynamicClient. QuotaPolicy has no
// registered runtime.Scheme yet (see internal/apis/quota/v1alpha1's package
// doc comment), so — same as internal/quotapolicy's own list_test.go — the
// test builds one locally.
func newFakeQuotaPolicyClient(t *testing.T, policies ...*v1alpha1.QuotaPolicy) *dynamicfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(quotapolicy.GroupVersionResource.GroupVersion().WithKind("QuotaPolicy"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(quotapolicy.GroupVersionResource.GroupVersion().WithKind("QuotaPolicyList"), &unstructured.UnstructuredList{})
	gvrToListKind := map[schema.GroupVersionResource]string{quotapolicy.GroupVersionResource: "QuotaPolicyList"}

	var objs []runtime.Object
	for _, p := range policies {
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(p)
		if err != nil {
			t.Fatalf("ToUnstructured: %v", err)
		}
		u := &unstructured.Unstructured{Object: m}
		u.SetAPIVersion(v1alpha1.GroupName + "/" + v1alpha1.GroupVersion)
		u.SetKind(v1alpha1.QuotaPolicyKind)
		objs = append(objs, u)
	}

	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind, objs...)
}

// gi1MaxPolicy returns a namespace-wide policy in "default" (matching
// newBoundPV's ClaimRef.Namespace) enforcing a hard 1Gi maximum.
func gi1MaxPolicy(namespace, name string) *v1alpha1.QuotaPolicy {
	return &v1alpha1.QuotaPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: v1alpha1.QuotaPolicySpec{
			MaxQuota:   resource.NewQuantity(1*1024*1024*1024, resource.BinarySI),
			EnforceMax: true,
		},
	}
}

// quotaPolicyTestFixture wires an agent, a 10Gi bound PV (with its
// namespace/name-matching PVC, needed for QuotaPolicy label matching), and
// the local directory ensureQuota expects to already exist.
func quotaPolicyTestFixture(t *testing.T) (*QuotaAgent, *v1.PersistentVolume) {
	t.Helper()
	pv := newBoundPV("pv-1", "/exports/pvc-1", 10) // 10Gi requested
	pv.Annotations = map[string]string{"pv.kubernetes.io/provisioned-by": "example.com/nfs"}
	pvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: pv.Spec.ClaimRef.Namespace,
			Name:      pv.Spec.ClaimRef.Name,
		},
	}
	client := fake.NewSimpleClientset(pv, pvc)

	a := newTestAgent(t, client)
	a.nfsServerPath = "/exports"
	a.fsType = quota.FSTypeXFS
	a.provisionerName = "example.com/nfs"

	if err := os.MkdirAll(filepath.Join(a.nfsBasePath, "pvc-1"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return a, pv
}

const tenGiBytes = 10 * 1024 * 1024 * 1024
const oneGiBytes = 1 * 1024 * 1024 * 1024

// TestSyncAllQuotas_QuotaPolicyDisabled_AppliesCapacityUnchanged is the
// regression test that matters most for this feature: with
// quotaPolicyEnabled left at its default (false), a claim that would be
// clamped hard by a QuotaPolicy object sitting right there in the fake
// dynamic client must still get exactly its own PV capacity applied — the
// same behavior as every version of this agent before QuotaPolicy existed.
// If this ever starts failing, QuotaPolicy has leaked into the enforcement
// path for users who never opted in.
func TestSyncAllQuotas_QuotaPolicyDisabled_AppliesCapacityUnchanged(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	a, _ := quotaPolicyTestFixture(t)

	// Deliberately NOT calling a.SetQuotaPolicyEnabled(true), even though a
	// policy that would clamp this claim to 1Gi is right there.
	a.SetDynamicClient(newFakeQuotaPolicyClient(t, gi1MaxPolicy("default", "cap-at-1gi")))

	if err := a.syncAllQuotas(context.Background()); err != nil {
		t.Fatalf("syncAllQuotas: %v", err)
	}

	localPath := a.nfsPathToLocal("/exports/pvc-1")
	if got := a.appliedQuotas[localPath]; got != tenGiBytes {
		t.Fatalf("applied quota = %d, want %d (raw PV capacity, unaffected by the disabled feature)", got, tenGiBytes)
	}
}

// TestSyncAllQuotas_QuotaPolicyEnabledNoPolicies_AppliesCapacityUnchanged
// covers the other half of the same regression: the feature flag is ON but
// no QuotaPolicy object exists (empty dynamic client). This must behave
// identically to the flag being off — enabling the feature with nothing yet
// applied must never itself change enforcement.
func TestSyncAllQuotas_QuotaPolicyEnabledNoPolicies_AppliesCapacityUnchanged(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	a, _ := quotaPolicyTestFixture(t)

	a.SetQuotaPolicyEnabled(true)
	a.SetDynamicClient(newFakeQuotaPolicyClient(t)) // no policies at all

	if err := a.syncAllQuotas(context.Background()); err != nil {
		t.Fatalf("syncAllQuotas: %v", err)
	}

	localPath := a.nfsPathToLocal("/exports/pvc-1")
	if got := a.appliedQuotas[localPath]; got != tenGiBytes {
		t.Fatalf("applied quota = %d, want %d (raw PV capacity, no policies to apply)", got, tenGiBytes)
	}
}

// TestSyncAllQuotas_QuotaPolicyEnabledNilDynamicClient_AppliesCapacityUnchanged
// covers enabling the flag without ever wiring a dynamic client (an
// operator error, or a deployment that hasn't been given RBAC yet) — this
// must degrade the same way, not panic or fail the sync.
func TestSyncAllQuotas_QuotaPolicyEnabledNilDynamicClient_AppliesCapacityUnchanged(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	a, _ := quotaPolicyTestFixture(t)

	a.SetQuotaPolicyEnabled(true) // dynamicClient left nil

	if err := a.syncAllQuotas(context.Background()); err != nil {
		t.Fatalf("syncAllQuotas: %v", err)
	}

	localPath := a.nfsPathToLocal("/exports/pvc-1")
	if got := a.appliedQuotas[localPath]; got != tenGiBytes {
		t.Fatalf("applied quota = %d, want %d", got, tenGiBytes)
	}
}

// TestSyncAllQuotas_QuotaPolicyResolves_ClampsToMax proves the positive
// path: with the feature enabled and a matching, enforceMax=true policy
// present, the size actually handed to the filesystem is the
// QuotaPolicy-resolved bound, not the PV's raw capacity.
func TestSyncAllQuotas_QuotaPolicyResolves_ClampsToMax(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	a, _ := quotaPolicyTestFixture(t)

	a.SetQuotaPolicyEnabled(true)
	a.SetDynamicClient(newFakeQuotaPolicyClient(t, gi1MaxPolicy("default", "cap-at-1gi")))

	if err := a.syncAllQuotas(context.Background()); err != nil {
		t.Fatalf("syncAllQuotas: %v", err)
	}

	localPath := a.nfsPathToLocal("/exports/pvc-1")
	if got := a.appliedQuotas[localPath]; got != oneGiBytes {
		t.Fatalf("applied quota = %d, want %d (clamped to the policy's maxQuota)", got, oneGiBytes)
	}
}

// TestFinishQuotaPolicyCycle_WritesStatusWhenSingleWriter verifies the
// status write-back path: with quotaPolicySingleWriter explicitly opted
// into, after a sync cycle with a matching, successfully-enforced policy,
// the QuotaPolicy object's status in the dynamic client reflects the match.
func TestFinishQuotaPolicyCycle_WritesStatusWhenSingleWriter(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	a, _ := quotaPolicyTestFixture(t)

	a.SetQuotaPolicyEnabled(true)
	a.SetQuotaPolicySingleWriter(true)
	p := gi1MaxPolicy("default", "cap-at-1gi")
	dyn := newFakeQuotaPolicyClient(t, p)
	a.SetDynamicClient(dyn)

	if err := a.syncAllQuotas(context.Background()); err != nil {
		t.Fatalf("syncAllQuotas: %v", err)
	}

	got, err := dyn.Resource(quotapolicy.GroupVersionResource).Namespace(p.Namespace).Get(context.Background(), p.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get policy after sync: %v", err)
	}
	matched, found, err := unstructured.NestedInt64(got.Object, "status", "matchedClaims")
	if err != nil || !found || matched != 1 {
		t.Fatalf("matchedClaims: found=%v err=%v value=%d, want 1", found, err, matched)
	}
	applied, found, err := unstructured.NestedInt64(got.Object, "status", "appliedClaims")
	if err != nil || !found || applied != 1 {
		t.Fatalf("appliedClaims: found=%v err=%v value=%d, want 1", found, err, applied)
	}
	conditions, found, err := unstructured.NestedSlice(got.Object, "status", "conditions")
	if err != nil || !found || len(conditions) == 0 {
		t.Fatalf("expected non-empty conditions: found=%v err=%v", found, err)
	}
}

// TestFinishQuotaPolicyCycle_SkipsStatusByDefault guards the multi-writer
// fix: with QuotaPolicy enabled but quotaPolicySingleWriter left at its
// default (false) — the topology this DaemonSet supports out of the box,
// per values.yaml's nodeSelector comment allowing several NFS server nodes
// — a sync cycle must enforce the quota (that's unaffected) but must NOT
// write QuotaPolicy status at all. Publishing a partial per-node view from
// every replica would flap the object's status every cycle; see
// finishQuotaPolicyCycle's doc comment and docs/quotapolicy-design.md §11.
func TestFinishQuotaPolicyCycle_SkipsStatusByDefault(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	a, _ := quotaPolicyTestFixture(t)

	a.SetQuotaPolicyEnabled(true) // quotaPolicySingleWriter left false (default)
	p := gi1MaxPolicy("default", "cap-at-1gi")
	dyn := newFakeQuotaPolicyClient(t, p)
	a.SetDynamicClient(dyn)

	if err := a.syncAllQuotas(context.Background()); err != nil {
		t.Fatalf("syncAllQuotas: %v", err)
	}

	// Enforcement itself must still have happened (clamped to 1Gi).
	localPath := a.nfsPathToLocal("/exports/pvc-1")
	if got := a.appliedQuotas[localPath]; got != oneGiBytes {
		t.Fatalf("applied quota = %d, want %d — status-write gating must not affect enforcement", got, oneGiBytes)
	}

	got, err := dyn.Resource(quotapolicy.GroupVersionResource).Namespace(p.Namespace).Get(context.Background(), p.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get policy after sync: %v", err)
	}
	// The QuotaPolicy's Status field always round-trips as `status: {}`
	// (non-pointer structs aren't omitted by encoding/json's omitempty),
	// so asserting the whole "status" key is absent would be wrong — the
	// int32/slice fields *inside* it are what actually get omitted when
	// unset, so their absence is what proves WriteStatus never ran.
	if _, found, _ := unstructured.NestedInt64(got.Object, "status", "matchedClaims"); found {
		t.Fatalf("expected status.matchedClaims to be unset without quotaPolicySingleWriter, got %+v", got.Object["status"])
	}
	if _, found, _ := unstructured.NestedSlice(got.Object, "status", "conditions"); found {
		t.Fatalf("expected status.conditions to be unset without quotaPolicySingleWriter, got %+v", got.Object["status"])
	}
}
