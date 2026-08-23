package quotapolicy

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dasomel/nfs-quota-agent/internal/apis/quota/v1alpha1"
)

func p(ns, name string, prio int32, sel v1alpha1.QuotaPolicySelector) v1alpha1.QuotaPolicy {
	return v1alpha1.QuotaPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1alpha1.QuotaPolicySpec{Selector: sel, Priority: prio},
	}
}
func namePtr(s string) *string { return &s }

// Mirrors the fixture applied to a live cluster: specificity and priority
// are deliberately set in OPPOSITE order, so an implementation that checks
// priority before specificity picks a different winner.
func TestIndependent_SpecificityBeatsPriority(t *testing.T) {
	policies := []v1alpha1.QuotaPolicy{
		p("qp-test", "qp-by-name", 100, v1alpha1.QuotaPolicySelector{PVCName: namePtr("target-pvc")}),
		p("qp-test", "qp-by-label", 0, v1alpha1.QuotaPolicySelector{
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "gold"}}}),
		p("qp-test", "qp-namespace-wide", 0, v1alpha1.QuotaPolicySelector{}),
		// decoy: identical name+selector in another namespace
		p("qp-other", "qp-by-name", 0, v1alpha1.QuotaPolicySelector{PVCName: namePtr("target-pvc")}),
	}

	got := Resolve(Claim{Namespace: "qp-test", Name: "target-pvc",
		Labels: map[string]string{"tier": "gold"}}, policies)

	if got.Winner == nil {
		t.Fatal("no winner")
	}
	if got.Winner.Name != "qp-by-name" || got.Winner.Namespace != "qp-test" {
		t.Errorf("winner = %s/%s, want qp-test/qp-by-name (priority-first impl picks qp-by-label or qp-namespace-wide)",
			got.Winner.Namespace, got.Winner.Name)
	}
	if got.MatchKind != v1alpha1.MatchKindPVCName {
		t.Errorf("matchKind = %q, want PVCName", got.MatchKind)
	}
	if len(got.Losers) != 2 {
		t.Errorf("losers = %d, want 2 (the decoy in qp-other must not appear)", len(got.Losers))
	}
	for _, l := range got.Losers {
		if l.Policy.Namespace != "qp-test" {
			t.Errorf("cross-namespace policy %s/%s leaked into losers", l.Policy.Namespace, l.Policy.Name)
		}
	}
	t.Logf("winner=%s kind=%s losers=%d", got.Winner.Name, got.MatchKind, len(got.Losers))
	for _, l := range got.Losers {
		t.Logf("  lost: %s -- %s", l.Policy.Name, l.Reason)
	}
}

// Equal specificity, name order deliberately OPPOSITE to priority order.
// zzz-strong has the lower (stronger) priority but sorts last by name.
func TestIndependent_LowerPriorityWinsNotNameOrder(t *testing.T) {
	policies := []v1alpha1.QuotaPolicy{
		p("qp-test", "aaa-weak", 100, v1alpha1.QuotaPolicySelector{}),
		p("qp-test", "zzz-strong", 0, v1alpha1.QuotaPolicySelector{}),
	}
	got := Resolve(Claim{Namespace: "qp-test", Name: "any"}, policies)
	if got.Winner == nil || got.Winner.Name != "zzz-strong" {
		t.Errorf("winner = %v, want zzz-strong (name-first or reversed priority picks aaa-weak)", got.Winner)
	}
}

// tier=silver must not match the gold label selector, so the namespace-wide
// policy wins even though it is the least specific.
func TestIndependent_NonMatchingLabelFallsThrough(t *testing.T) {
	policies := []v1alpha1.QuotaPolicy{
		p("qp-test", "qp-by-label", 0, v1alpha1.QuotaPolicySelector{
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "gold"}}}),
		p("qp-test", "qp-namespace-wide", 100, v1alpha1.QuotaPolicySelector{}),
	}
	got := Resolve(Claim{Namespace: "qp-test", Name: "other-pvc",
		Labels: map[string]string{"tier": "silver"}}, policies)
	if got.Winner == nil || got.Winner.Name != "qp-namespace-wide" {
		t.Errorf("winner = %v, want qp-namespace-wide", got.Winner)
	}
	if len(got.Losers) != 0 {
		t.Errorf("losers = %d, want 0 (the gold selector should not have matched)", len(got.Losers))
	}
}

// An unparseable selector must be reported, not silently treated as a
// non-match.
func TestIndependent_InvalidSelectorIsReported(t *testing.T) {
	bad := p("qp-test", "qp-bad", 0, v1alpha1.QuotaPolicySelector{
		LabelSelector: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "tier", Operator: "BogusOperator", Values: []string{"gold"}},
			}}})
	got := Resolve(Claim{Namespace: "qp-test", Name: "x"}, []v1alpha1.QuotaPolicy{bad})
	if len(got.Invalid) != 1 {
		t.Fatalf("invalid = %d, want 1 -- an unparseable selector was silently dropped", len(got.Invalid))
	}
	if got.Winner != nil {
		t.Errorf("winner = %v, want nil", got.Winner)
	}
	t.Logf("reported: %v", got.Invalid[0].Err)
}
