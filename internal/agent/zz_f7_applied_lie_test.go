package agent

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dasomel/nfs-quota-agent/internal/apis/quota/v1alpha1"
	"github.com/dasomel/nfs-quota-agent/internal/quotapolicy"
)

// ensureQuota returns nil (not an error) when the PV's local directory does
// not exist -- it logs "Directory does not exist, skipping quota" and gives
// up. recordEnforcement treats a nil error as success, so the claim lands in
// appliedClaims and Applied reports True.
//
// That makes Applied a false statement: its documented meaning is "every
// claim this policy currently wins for has the quota enforced", and nothing
// was enforced. This is not only a multi-node concern -- on a single node it
// fires for any PV whose directory is absent, including every PV backed by a
// different NFS server's export, which syncAllQuotas still lists because it
// lists PVs cluster-wide.
//
// NOTE: deliberately no directory is created for this PV.
func TestAppliedMustNotCountClaimsWhoseDirectoryIsMissing(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())

	pv := newBoundPV("pv-elsewhere", "/exports/pvc-elsewhere", 10)
	client := fake.NewSimpleClientset(pv)
	a := newTestAgent(t, client)
	// No os.MkdirAll here: this PV belongs to another node's export.

	max := *resourceQuantity(5 * gib)
	policy := &v1alpha1.QuotaPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "quota.nfs.io/v1alpha1", Kind: "QuotaPolicy"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "p", Generation: 1},
		Spec: v1alpha1.QuotaPolicySpec{
			Selector: v1alpha1.QuotaPolicySelector{}, Priority: 100,
			MaxQuota: &max, EnforceMax: true,
		},
	}
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(policy)
	if err != nil {
		t.Fatal(err)
	}
	dc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schemaGVR]string{quotapolicy.GroupVersionResource: "QuotaPolicyList"},
		&unstructured.Unstructured{Object: u})

	a.SetDynamicClient(dc)
	a.SetQuotaPolicyEnabled(true)
	a.SetProcessAllNFS(true)
	// Without this, finishQuotaPolicyCycle no-ops (see its doc comment) and
	// WriteStatus never runs, so the assertions below would pass vacuously
	// on the zero-value status this test never wrote -- not because the
	// fix works. This test is the single-writer node reporting a claim it
	// resolved a winner for but could not enforce, which is exactly the
	// case errLocalDirectoryMissing exists to cover.
	a.SetQuotaPolicySingleWriter(true)
	a.fsType = "xfs"

	ctx := context.Background()
	if err := a.syncAllQuotas(ctx); err != nil {
		t.Fatalf("syncAllQuotas: %v", err)
	}

	got, err := dc.Resource(quotapolicy.GroupVersionResource).Namespace("default").
		Get(ctx, "p", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	status, _ := got.Object["status"].(map[string]interface{})
	applied, _ := status["appliedClaims"]
	var appliedCond string
	if conds, ok := status["conditions"].([]interface{}); ok {
		for _, c := range conds {
			m := c.(map[string]interface{})
			if m["type"] == "Applied" {
				appliedCond = m["status"].(string)
			}
		}
	}

	a.mu.Lock()
	_, everApplied := a.appliedQuotas[a.nfsPathToLocal("/exports/pvc-elsewhere")]
	a.mu.Unlock()

	t.Logf("quota actually applied to the filesystem: %v", everApplied)
	t.Logf("status.appliedClaims=%v  Applied=%s", applied, appliedCond)

	if everApplied {
		t.Fatal("test setup wrong: the quota was applied, so there is nothing to catch")
	}
	if fmtInt(applied) != 0 {
		t.Errorf("appliedClaims=%v but no quota was applied to any filesystem; "+
			"ensureQuota skipped this PV because its directory is absent and returned nil, "+
			"which recordEnforcement counted as success", applied)
	}
	if appliedCond == "True" {
		t.Errorf("Applied=True while nothing was enforced -- the condition's documented " +
			"meaning is that every won claim has the quota enforced")
	}
}

func fmtInt(v interface{}) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}
