//go:build livecluster

package quotapolicy

import (
	"context"
	"os"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/dasomel/nfs-quota-agent/internal/apis/quota/v1alpha1"
)

// Runs against a REAL API server (build tag `livecluster`, KUBECONTEXT env).
// A fake dynamic client accepts any status object, so it cannot prove the
// capped status is actually writable: the CRD's MaxItems=20 rejects the
// ENTIRE status update when exceeded, taking counts and conditions down
// with the oversized sample. That failure mode only appears against a real
// API server, which is why this test exists alongside the unit tests.
func TestLive_CappedStatusIsAcceptedByAPIServer(t *testing.T) {
	ctxName := os.Getenv("KUBECONTEXT")
	if ctxName == "" {
		t.Skip("set KUBECONTEXT")
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{CurrentContext: ctxName}).ClientConfig()
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("dynamic client: %v", err)
	}
	ctx := context.Background()

	policies, err := List(ctx, dc)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	t.Logf("listed %d QuotaPolicy objects from the live cluster", len(policies))

	var target *v1alpha1.QuotaPolicy
	for i := range policies {
		if policies[i].Namespace == "qp-test" && policies[i].Name == "qp-namespace-wide" {
			target = &policies[i]
		}
	}
	if target == nil {
		t.Fatal("fixture qp-test/qp-namespace-wide not found; apply the e2e fixture first")
	}

	// 25 outcomes -> both sample lists must be capped to 20, or the whole
	// update is rejected.
	outcomes := make([]ClaimOutcome, 0, 25)
	for i := 0; i < 25; i++ {
		outcomes = append(outcomes, ClaimOutcome{
			Claim:     Claim{Namespace: "qp-test", Name: "pvc-" + string(rune('a'+i%26))},
			MatchKind: v1alpha1.MatchKindNamespaceWide,
			Won:       true,
		})
	}
	st := BuildStatus(target, outcomes, LimitRangeInfo{}, target.Status.Conditions, metav1.Now())

	if got := len(st.MatchedClaimSample); got > 20 {
		t.Fatalf("matchedClaimSample not capped: %d", got)
	}
	if got := st.MatchedClaims; got != 25 {
		t.Errorf("matchedClaims = %d, want 25 -- the COUNT must stay truthful even though the sample is capped", got)
	}

	if err := WriteStatus(ctx, dc, target, st); err != nil {
		t.Fatalf("WriteStatus rejected by the real API server: %v", err)
	}

	back, err := dc.Resource(GroupVersionResource).Namespace("qp-test").
		Get(ctx, "qp-namespace-wide", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	status, found, _ := unstructuredNestedMap(back.Object, "status")
	if !found {
		t.Fatal("status was not persisted")
	}
	t.Logf("persisted status keys: %v", keysOf(status))
	if mc, ok := status["matchedClaims"]; !ok {
		t.Error("matchedClaims missing from persisted status -- counts were lost")
	} else {
		t.Logf("persisted matchedClaims = %v", mc)
	}
	if conds, ok := status["conditions"].([]interface{}); !ok || len(conds) == 0 {
		t.Error("conditions missing from persisted status")
	} else {
		t.Logf("persisted %d conditions", len(conds))
	}
}

func unstructuredNestedMap(obj map[string]interface{}, key string) (map[string]interface{}, bool, error) {
	v, ok := obj[key]
	if !ok {
		return nil, false, nil
	}
	m, ok := v.(map[string]interface{})
	return m, ok, nil
}

func keysOf(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
