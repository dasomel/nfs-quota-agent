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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// TestSyncAllQuotas_DriftIndependentOfEnforcementCache is the end-to-end
// regression test for #13's Drifted condition: it proves the drift check
// actually catches what it exists to catch -- a claim ensureQuota's own
// cache short-circuit (appliedQuotas[localPath] == sizeBytes) would
// otherwise skip re-verifying, because nothing about the *enforcement
// path* changed between cycles. Cycle 1 applies and caches the policy's
// 1Gi max. Between cycles, the fake xfs_quota's on-disk state is mutated
// directly (simulating an out-of-band change -- someone running xfs_quota
// by hand, or a kernel-level anomaly) without touching the agent's cache
// or re-running any apply. Cycle 2 must still detect the mismatch via the
// independent read-back check and report Drifted=True, even though
// ensureQuota itself does no work that cycle (same cached value, same
// effectiveBytes -- its fast path returns nil without ever calling
// verifyQuotaOnDisk).
func TestSyncAllQuotas_DriftIndependentOfEnforcementCache(t *testing.T) {
	state := &xfsQuotaState{}
	var enforcementCalls atomic.Int32 // project -s / limit -p, i.e. actual mutation attempts
	runner := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		switch name {
		case "findmnt":
			return []byte("xfs\n"), nil
		case "xfs_quota":
			if len(args) > 0 && args[0] == "-V" {
				return []byte("xfs_quota version 1.0"), nil
			}
			if len(args) >= 3 && args[1] == "-c" {
				if strings.HasPrefix(args[2], "project -s") || strings.HasPrefix(args[2], "limit -p") {
					enforcementCalls.Add(1)
				}
				if out, ok := state.handle(args[2]); ok {
					return out, nil
				}
			}
			return []byte("Project quota state: ON"), nil
		default:
			return []byte(""), nil
		}
	}}
	withFakeRunner(t, runner)

	a, _ := quotaPolicyTestFixture(t)
	a.SetQuotaPolicyEnabled(true)
	a.SetQuotaPolicySingleWriter(true)
	p := gi1MaxPolicy("default", "cap-at-1gi")
	dyn := newFakeQuotaPolicyClient(t, p)
	a.SetDynamicClient(dyn)

	// Cycle 1: normal apply. Confirms the baseline (Applied=True,
	// Drifted=False) before introducing drift.
	if err := a.syncAllQuotas(context.Background()); err != nil {
		t.Fatalf("syncAllQuotas (cycle 1): %v", err)
	}
	localPath := a.nfsPathToLocal("/exports/pvc-1")
	if got := a.appliedQuotas[localPath]; got != oneGiBytes {
		t.Fatalf("applied quota after cycle 1 = %d, want %d", got, oneGiBytes)
	}

	got1, err := dyn.Resource(quotapolicy.GroupVersionResource).Namespace(p.Namespace).Get(context.Background(), p.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get policy after cycle 1: %v", err)
	}
	assertConditionStatus(t, got1, "Drifted", "False")

	// Introduce drift: directly overwrite the fake's on-disk state for
	// this project's ID to a different value, bypassing the agent
	// entirely -- exactly what an out-of-band xfs_quota invocation would
	// do. The agent's own appliedQuotas cache and QuotaPolicy resolution
	// are completely untouched.
	state.mu.Lock()
	for id := range state.applied {
		state.applied[id] = oneGiBytes / 2
	}
	state.mu.Unlock()

	// Cycle 2: same policy, same PV, same cache -- ensureQuota's fast path
	// (appliedQuotas[localPath] == sizeBytes) means it does no work at all
	// this cycle. Only the independent drift check can catch this.
	enforcementCalls.Store(0)
	if err := a.syncAllQuotas(context.Background()); err != nil {
		t.Fatalf("syncAllQuotas (cycle 2): %v", err)
	}
	if got := a.appliedQuotas[localPath]; got != oneGiBytes {
		t.Fatalf("appliedQuotas cache changed after cycle 2 = %d, want unchanged %d (drift detection must not itself mutate the cache)", got, oneGiBytes)
	}
	// Directly proves "independent of the enforcement cache": Drifted is
	// detected below without ensureQuota ever re-running project -s/limit
	// -p this cycle. Without this, a future refactor that re-verifies
	// (and incidentally re-applies) during enforcement whenever it
	// detects drift could make this test still pass for the wrong reason.
	if got := enforcementCalls.Load(); got != 0 {
		t.Fatalf("enforcement calls (project -s/limit -p) in cycle 2 = %d, want 0 -- drift must be detected without re-running the apply path", got)
	}

	got2, err := dyn.Resource(quotapolicy.GroupVersionResource).Namespace(p.Namespace).Get(context.Background(), p.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get policy after cycle 2: %v", err)
	}
	assertConditionStatus(t, got2, "Drifted", "True")
	assertConditionStatus(t, got2, "Applied", "True") // enforcement itself still reported no error this cycle
	driftedClaims, found, err := unstructured.NestedSlice(got2.Object, "status", "driftedClaims")
	if err != nil || !found || len(driftedClaims) != 1 {
		t.Fatalf("expected exactly one driftedClaims entry, found=%v err=%v got=%+v", found, err, driftedClaims)
	}
}

// assertConditionStatus fails the test unless got's status.conditions
// contains one entry of the given type with the given status.
func assertConditionStatus(t *testing.T, got *unstructured.Unstructured, condType, wantStatus string) {
	t.Helper()
	conditions, found, err := unstructured.NestedSlice(got.Object, "status", "conditions")
	if err != nil || !found {
		t.Fatalf("status.conditions: found=%v err=%v", found, err)
	}
	for _, c := range conditions {
		m, ok := c.(map[string]interface{})
		if !ok || m["type"] != condType {
			continue
		}
		if m["status"] != wantStatus {
			t.Fatalf("condition %s status = %v, want %v (full: %+v)", condType, m["status"], wantStatus, m)
		}
		return
	}
	t.Fatalf("condition %s not found in %+v", condType, conditions)
}

// assertMatchedAndApplied fails the test unless got's status.matchedClaims
// and status.appliedClaims equal the given values exactly.
func assertMatchedAndApplied(t *testing.T, got *unstructured.Unstructured, wantMatched, wantApplied int64) {
	t.Helper()
	matched, found, err := unstructured.NestedInt64(got.Object, "status", "matchedClaims")
	if err != nil || !found || matched != wantMatched {
		t.Fatalf("status.matchedClaims: found=%v err=%v value=%d, want %d", found, err, matched, wantMatched)
	}
	applied, found, err := unstructured.NestedInt64(got.Object, "status", "appliedClaims")
	if err != nil || !found || applied != wantApplied {
		t.Fatalf("status.appliedClaims: found=%v err=%v value=%d, want %d", found, err, applied, wantApplied)
	}
}

// TestSyncAllQuotas_DriftReportUnavailableReportsUnknown guards against
// exactly what an independent review caught before this shipped, in two
// rounds: first, that a transient failure to read the on-disk quota
// report itself (the xfs_quota command failing) must not be reported as
// Drifted=True -- that would be a false positive indistinguishable from a
// real, confirmed mismatch. Second (the review's follow-up round), that it
// must not be reported as Drifted=False either -- a report outage isn't
// evidence of a healthy "no drift" state any more than it's evidence of
// drift; it's Unknown, and the condition needs to say so or an operator
// reading a falsely-healthy status would miss exactly the outage they most
// need to know about.
func TestSyncAllQuotas_DriftReportUnavailableReportsUnknown(t *testing.T) {
	// failReports flips on only after cycle 1: cycle 1 needs its report
	// call to succeed (ensureQuota's own apply-time verification, #10 --
	// unrelated to this test) so the PV is genuinely cached as applied
	// before cycle 2's report failure is introduced. Otherwise a failure
	// on the very first report call would fail the apply itself, not
	// exercise the drift-check's handling of a report failure at all.
	var failReports atomic.Bool
	state := &xfsQuotaState{}
	runner := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		if name == "xfs_quota" && len(args) >= 3 && args[1] == "-c" && strings.HasPrefix(args[2], "report") && failReports.Load() {
			return nil, errors.New("simulated transient xfs_quota report failure")
		}
		switch name {
		case "findmnt":
			return []byte("xfs\n"), nil
		case "xfs_quota":
			if len(args) > 0 && args[0] == "-V" {
				return []byte("xfs_quota version 1.0"), nil
			}
			if len(args) >= 3 && args[1] == "-c" {
				if out, ok := state.handle(args[2]); ok {
					return out, nil
				}
			}
			return []byte("Project quota state: ON"), nil
		default:
			return []byte(""), nil
		}
	}}
	withFakeRunner(t, runner)

	a, _ := quotaPolicyTestFixture(t)
	a.SetQuotaPolicyEnabled(true)
	a.SetQuotaPolicySingleWriter(true)
	p := gi1MaxPolicy("default", "cap-at-1gi")
	dyn := newFakeQuotaPolicyClient(t, p)
	a.SetDynamicClient(dyn)

	if err := a.syncAllQuotas(context.Background()); err != nil {
		t.Fatalf("syncAllQuotas (cycle 1): %v", err)
	}
	localPath := a.nfsPathToLocal("/exports/pvc-1")
	if got := a.appliedQuotas[localPath]; got != oneGiBytes {
		t.Fatalf("applied quota after cycle 1 = %d, want %d", got, oneGiBytes)
	}

	// Cycle 2: the PV is already cached correctly (no fresh apply needed,
	// ensureQuota's fast path returns nil without calling report at all),
	// so the only report call this cycle comes from the drift check --
	// and it fails.
	failReports.Store(true)
	if err := a.syncAllQuotas(context.Background()); err != nil {
		t.Fatalf("syncAllQuotas (cycle 2): %v", err)
	}

	got, err := dyn.Resource(quotapolicy.GroupVersionResource).Namespace(p.Namespace).Get(context.Background(), p.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get policy after cycle 2: %v", err)
	}
	assertConditionStatus(t, got, "Applied", "True")
	assertConditionStatus(t, got, "Drifted", "Unknown")
	drifted, err := getConditionReason(got, "Drifted")
	if err != nil {
		t.Fatal(err)
	}
	if drifted != v1alpha1.ReasonDriftCheckUnavailable {
		t.Fatalf("Drifted condition reason = %q, want %q", drifted, v1alpha1.ReasonDriftCheckUnavailable)
	}
	if claims, found, err := unstructured.NestedSlice(got.Object, "status", "driftedClaims"); err != nil || (found && len(claims) != 0) {
		t.Fatalf("expected no driftedClaims when the report itself was unreadable (that's Unknown, not confirmed drift), found=%v err=%v got=%+v", found, err, claims)
	}
}

// getConditionReason returns the Reason of got's status.conditions entry
// of the given type.
func getConditionReason(got *unstructured.Unstructured, condType string) (string, error) {
	conditions, found, err := unstructured.NestedSlice(got.Object, "status", "conditions")
	if err != nil || !found {
		return "", fmt.Errorf("status.conditions: found=%v err=%w", found, err)
	}
	for _, c := range conditions {
		m, ok := c.(map[string]interface{})
		if !ok || m["type"] != condType {
			continue
		}
		reason, _ := m["reason"].(string)
		return reason, nil
	}
	return "", fmt.Errorf("condition %s not found", condType)
}

// TestSyncAllQuotas_DriftReportFetchedOnceForMultiplePVs is the direct
// regression test for the other half of the same review finding: with
// several PVs matched by the same policy, the on-disk quota report must
// be fetched once for the whole cycle and reused, not once per PV -- a
// naive per-PV fetch would turn one sync into N filesystem-wide report
// scans.
func TestSyncAllQuotas_DriftReportFetchedOnceForMultiplePVs(t *testing.T) {
	state := &xfsQuotaState{}
	var reportCalls atomic.Int32
	// corruptReport, once armed, makes the report response lie about every
	// project's limit -- proving the batch fetch is actually compared
	// per-PV (not just fetched once and blindly trusted): if only the
	// first matched PV were compared, or the comparison loop silently
	// skipped some claims, driftedClaims below would show fewer than
	// nPVs entries despite every one of them genuinely mismatching.
	var corruptReport atomic.Bool
	runner := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		switch name {
		case "findmnt":
			return []byte("xfs\n"), nil
		case "xfs_quota":
			if len(args) > 0 && args[0] == "-V" {
				return []byte("xfs_quota version 1.0"), nil
			}
			if len(args) >= 3 && args[1] == "-c" {
				if strings.HasPrefix(args[2], "report") {
					reportCalls.Add(1)
					if corruptReport.Load() {
						return []byte("Project ID   Used   Soft   Hard   Warn/Grace\n"), nil
					}
				}
				if out, ok := state.handle(args[2]); ok {
					return out, nil
				}
			}
			return []byte("Project quota state: ON"), nil
		default:
			return []byte(""), nil
		}
	}}
	withFakeRunner(t, runner)

	const nPVs = 3
	var pvs []*v1.PersistentVolume
	var objs []runtime.Object
	for i := 0; i < nPVs; i++ {
		name := "pv-" + string(rune('a'+i))
		nfsPath := "/exports/pvc-" + string(rune('a'+i))
		pv := newBoundPV(name, nfsPath, 10)
		pv.Annotations = map[string]string{"pv.kubernetes.io/provisioned-by": "example.com/nfs"}
		pvs = append(pvs, pv)
		objs = append(objs, pv, &v1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: pv.Spec.ClaimRef.Namespace, Name: pv.Spec.ClaimRef.Name},
		})
	}
	client := fake.NewSimpleClientset(objs...)

	a := newTestAgent(t, client)
	a.nfsServerPath = "/exports"
	a.fsType = quota.FSTypeXFS
	a.provisionerName = "example.com/nfs"
	for i := range pvs {
		if err := os.MkdirAll(filepath.Join(a.nfsBasePath, "pvc-"+string(rune('a'+i))), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	a.SetQuotaPolicyEnabled(true)
	a.SetQuotaPolicySingleWriter(true)
	p := gi1MaxPolicy("default", "cap-at-1gi")
	dyn := newFakeQuotaPolicyClient(t, p)
	a.SetDynamicClient(dyn)

	// Cycle 1 legitimately calls report once per freshly-applied PV (each
	// one goes through ensureQuota's own apply-time verification, #10 --
	// unrelated to this test's concern), so reportCalls isn't asserted
	// here.
	if err := a.syncAllQuotas(context.Background()); err != nil {
		t.Fatalf("syncAllQuotas (cycle 1): %v", err)
	}
	for i := range pvs {
		localPath := a.nfsPathToLocal("/exports/pvc-" + string(rune('a'+i)))
		if got := a.appliedQuotas[localPath]; got != oneGiBytes {
			t.Fatalf("pv %d: applied quota = %d, want %d", i, got, oneGiBytes)
		}
	}

	// Cycle 2: every PV is already cached correctly, so ensureQuota's fast
	// path skips all of them without ever calling report -- the only
	// report call this cycle can come from the shared drift-check fetch.
	// The report is also corrupted to show no matching entries at all, so
	// every one of the nPVs claims should independently come back as
	// drifted -- proving the single shared fetch is actually compared
	// per-claim, not just fetched and trusted for one PV.
	reportCalls.Store(0)
	corruptReport.Store(true)
	if err := a.syncAllQuotas(context.Background()); err != nil {
		t.Fatalf("syncAllQuotas (cycle 2): %v", err)
	}
	if got := reportCalls.Load(); got != 1 {
		t.Fatalf("report command called %d times in cycle 2 for %d matched PVs, want exactly 1 (fetched once for the cycle, not once per PV)", got, nPVs)
	}

	got, err := dyn.Resource(quotapolicy.GroupVersionResource).Namespace("default").Get(context.Background(), "cap-at-1gi", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get policy after cycle 2: %v", err)
	}
	assertConditionStatus(t, got, "Drifted", "True")
	matched, found, err := unstructured.NestedInt64(got.Object, "status", "matchedClaims")
	if err != nil || !found || matched != nPVs {
		t.Fatalf("matchedClaims after cycle 2: found=%v err=%v value=%d, want %d", found, err, matched, nPVs)
	}
	drifted, found, err := unstructured.NestedSlice(got.Object, "status", "driftedClaims")
	if err != nil || !found || len(drifted) != nPVs {
		t.Fatalf("driftedClaims after cycle 2: found=%v err=%v len=%d, want %d (every matched PV must be individually checked against the shared report, not just the first one)", found, err, len(drifted), nPVs)
	}
}

// TestSyncAllQuotas_FreshlyMutatedClaimNotFalselyDrifted is the direct
// regression test for the P1 finding an independent review raised against
// the first version of the drift check: the shared driftReport snapshot
// (fetched once, lazily, on the first claim that needs it) is fetched
// during THIS cycle, so a different claim that gets a fresh apply/update
// later in this same cycle -- after the snapshot was taken -- would be
// missing or stale in it. Comparing that freshly mutated claim against the
// stale snapshot would misreport a brand new, correctly-applied value as
// drift. The fix excludes any claim ensureQuota actually mutated this
// cycle from the drift check entirely (its own apply-time verification,
// #10, already confirmed it).
//
// Two separate pvcName-scoped policies target two separate PVs so their
// mutation timing can be controlled independently: pv-a's policy is
// unchanged between cycles (pv-a becomes a cache-hit in cycle 2, eligible
// for drift-check); pv-b's policy's maxQuota changes between cycles,
// forcing pv-b to be freshly re-applied in cycle 2.
func TestSyncAllQuotas_FreshlyMutatedClaimNotFalselyDrifted(t *testing.T) {
	state := &xfsQuotaState{}
	runner := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		switch name {
		case "findmnt":
			return []byte("xfs\n"), nil
		case "xfs_quota":
			if len(args) > 0 && args[0] == "-V" {
				return []byte("xfs_quota version 1.0"), nil
			}
			if len(args) >= 3 && args[1] == "-c" {
				if out, ok := state.handle(args[2]); ok {
					return out, nil
				}
			}
			return []byte("Project quota state: ON"), nil
		default:
			return []byte(""), nil
		}
	}}
	withFakeRunner(t, runner)

	pvA := newBoundPV("pv-a", "/exports/pvc-a", 10)
	pvA.Annotations = map[string]string{"pv.kubernetes.io/provisioned-by": "example.com/nfs"}
	pvB := newBoundPV("pv-b", "/exports/pvc-b", 10)
	pvB.Annotations = map[string]string{"pv.kubernetes.io/provisioned-by": "example.com/nfs"}
	client := fake.NewSimpleClientset(
		pvA, &v1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: pvA.Spec.ClaimRef.Namespace, Name: pvA.Spec.ClaimRef.Name}},
		pvB, &v1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: pvB.Spec.ClaimRef.Namespace, Name: pvB.Spec.ClaimRef.Name}},
	)

	a := newTestAgent(t, client)
	a.nfsServerPath = "/exports"
	a.fsType = quota.FSTypeXFS
	a.provisionerName = "example.com/nfs"
	for _, dir := range []string{"pvc-a", "pvc-b"} {
		if err := os.MkdirAll(filepath.Join(a.nfsBasePath, dir), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	pvcAName := pvA.Spec.ClaimRef.Name
	pvcBName := pvB.Spec.ClaimRef.Name
	pA := &v1alpha1.QuotaPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "policy-a"},
		Spec: v1alpha1.QuotaPolicySpec{
			Selector:   v1alpha1.QuotaPolicySelector{PVCName: &pvcAName},
			MaxQuota:   resource.NewQuantity(oneGiBytes, resource.BinarySI),
			EnforceMax: true,
		},
	}
	pB := &v1alpha1.QuotaPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "policy-b"},
		Spec: v1alpha1.QuotaPolicySpec{
			Selector:   v1alpha1.QuotaPolicySelector{PVCName: &pvcBName},
			MaxQuota:   resource.NewQuantity(oneGiBytes, resource.BinarySI),
			EnforceMax: true,
		},
	}
	dyn := newFakeQuotaPolicyClient(t, pA, pB)

	a.SetQuotaPolicyEnabled(true)
	a.SetQuotaPolicySingleWriter(true)
	a.SetDynamicClient(dyn)

	if err := a.syncAllQuotas(context.Background()); err != nil {
		t.Fatalf("syncAllQuotas (cycle 1): %v", err)
	}
	localPathA := a.nfsPathToLocal("/exports/pvc-a")
	localPathB := a.nfsPathToLocal("/exports/pvc-b")
	if got := a.appliedQuotas[localPathA]; got != oneGiBytes {
		t.Fatalf("pv-a applied quota after cycle 1 = %d, want %d", got, oneGiBytes)
	}
	if got := a.appliedQuotas[localPathB]; got != oneGiBytes {
		t.Fatalf("pv-b applied quota after cycle 1 = %d, want %d", got, oneGiBytes)
	}

	// Bump only policy-b's maxQuota: only pv-b needs a fresh apply in
	// cycle 2. pv-a's policy is untouched, so pv-a is a pure cache-hit,
	// eligible for (and expected to pass) the drift check.
	unstructuredB, err := dyn.Resource(quotapolicy.GroupVersionResource).Namespace("default").Get(context.Background(), "policy-b", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get policy-b: %v", err)
	}
	if err := unstructured.SetNestedField(unstructuredB.Object, "2Gi", "spec", "maxQuota"); err != nil {
		t.Fatalf("set maxQuota: %v", err)
	}
	if _, err := dyn.Resource(quotapolicy.GroupVersionResource).Namespace("default").Update(context.Background(), unstructuredB, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update policy-b: %v", err)
	}

	if err := a.syncAllQuotas(context.Background()); err != nil {
		t.Fatalf("syncAllQuotas (cycle 2): %v", err)
	}

	if got := a.appliedQuotas[localPathB]; got != 2*oneGiBytes {
		t.Fatalf("pv-b applied quota after cycle 2 = %d, want %d (should have been freshly re-applied to the new maxQuota)", got, 2*oneGiBytes)
	}

	gotA, err := dyn.Resource(quotapolicy.GroupVersionResource).Namespace("default").Get(context.Background(), "policy-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get policy-a after cycle 2: %v", err)
	}
	// matchedClaims/appliedClaims asserted explicitly, not just the
	// conditions: Applied=True and Drifted=False are both also true
	// vacuously for zero recorded outcomes, so without this a claim that
	// was silently skipped from status recording entirely (a different
	// bug than the one this test targets) could still pass.
	assertConditionStatus(t, gotA, "Applied", "True")
	assertConditionStatus(t, gotA, "Drifted", "False")
	assertMatchedAndApplied(t, gotA, 1, 1)

	gotB, err := dyn.Resource(quotapolicy.GroupVersionResource).Namespace("default").Get(context.Background(), "policy-b", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get policy-b after cycle 2: %v", err)
	}
	// The regression this test exists for: without the mutatedThisCycle
	// exclusion, pv-b's freshly-applied 2Gi could be compared against a
	// report snapshot taken before that mutation and misreported as
	// drift.
	assertConditionStatus(t, gotB, "Applied", "True")
	assertConditionStatus(t, gotB, "Drifted", "False")
	assertMatchedAndApplied(t, gotB, 1, 1)
	if claims, found, err := unstructured.NestedSlice(gotB.Object, "status", "driftedClaims"); err != nil || (found && len(claims) != 0) {
		t.Fatalf("policy-b: expected no driftedClaims for a claim freshly applied this same cycle, found=%v err=%v got=%+v", found, err, claims)
	}
}
