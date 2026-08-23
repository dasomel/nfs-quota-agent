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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dasomel/nfs-quota-agent/internal/apis/quota/v1alpha1"
)

// namedPolicy requires priority as an explicit positional argument rather
// than leaving it to a QuotaPolicySpec struct literal's default, on
// purpose: the CRD's +kubebuilder:default=100 is an API-server admission
// time default (confirmed live: `kubectl apply` with priority omitted comes
// back as priority: 100), not a Go zero-value default -- a bare Go struct
// literal that "omits" Priority actually gets 0, the *strongest* priority,
// which is a shape the API server never returns for an object that really
// omitted it. Forcing every call site to pass a real number here makes that
// distinction impossible to get wrong by accident: a test that means
// "matches the CRD default" must write 100, not leave the field unset.
func namedPolicy(ns, name string, priority int32, sel v1alpha1.QuotaPolicySelector) v1alpha1.QuotaPolicy {
	return v1alpha1.QuotaPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1alpha1.QuotaPolicySpec{
			Selector: sel,
			Priority: priority,
		},
	}
}

func pvcNameSelector(name string) v1alpha1.QuotaPolicySelector {
	return v1alpha1.QuotaPolicySelector{PVCName: &name}
}

func labelSelector(labels map[string]string) v1alpha1.QuotaPolicySelector {
	return v1alpha1.QuotaPolicySelector{LabelSelector: &metav1.LabelSelector{MatchLabels: labels}}
}

func TestResolve_NoPolicies(t *testing.T) {
	res := Resolve(Claim{Namespace: "ns", Name: "pvc"}, nil)
	if res.Winner != nil {
		t.Fatalf("expected no winner, got %+v", res.Winner)
	}
	if len(res.Losers) != 0 || len(res.Invalid) != 0 {
		t.Fatalf("expected empty losers/invalid, got %+v", res)
	}
}

func TestResolve_SpecificityRanking(t *testing.T) {
	claim := Claim{Namespace: "ns", Name: "my-pvc", Labels: map[string]string{"tier": "gold"}}

	tests := []struct {
		name       string
		policies   []v1alpha1.QuotaPolicy
		wantWinner string
		wantKind   v1alpha1.MatchKind
	}{
		{
			name: "pvcName beats labelSelector and namespace-wide",
			policies: []v1alpha1.QuotaPolicy{
				namedPolicy("ns", "nswide", 100, v1alpha1.QuotaPolicySelector{}),
				namedPolicy("ns", "bylabel", 100, labelSelector(map[string]string{"tier": "gold"})),
				namedPolicy("ns", "byname", 100, pvcNameSelector("my-pvc")),
			},
			wantWinner: "byname",
			wantKind:   v1alpha1.MatchKindPVCName,
		},
		{
			name: "labelSelector beats namespace-wide when pvcName absent",
			policies: []v1alpha1.QuotaPolicy{
				namedPolicy("ns", "nswide", 100, v1alpha1.QuotaPolicySelector{}),
				namedPolicy("ns", "bylabel", 100, labelSelector(map[string]string{"tier": "gold"})),
			},
			wantWinner: "bylabel",
			wantKind:   v1alpha1.MatchKindLabelSelector,
		},
		{
			name: "namespace-wide wins when it's the only match",
			policies: []v1alpha1.QuotaPolicy{
				namedPolicy("ns", "nswide", 100, v1alpha1.QuotaPolicySelector{}),
			},
			wantWinner: "nswide",
			wantKind:   v1alpha1.MatchKindNamespaceWide,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Resolve(claim, tt.policies)
			if res.Winner == nil {
				t.Fatalf("expected a winner, got none")
			}
			if res.Winner.Name != tt.wantWinner {
				t.Fatalf("winner = %q, want %q", res.Winner.Name, tt.wantWinner)
			}
			if res.MatchKind != tt.wantKind {
				t.Fatalf("matchKind = %s, want %s", res.MatchKind, tt.wantKind)
			}
			if len(res.Losers) != len(tt.policies)-1 {
				t.Fatalf("losers = %d, want %d", len(res.Losers), len(tt.policies)-1)
			}
		})
	}
}

// TestResolve_PriorityTieBreak_LowerWins is the case that would pass if the
// priority comparison were backwards: two policies tie on specificity
// (both namespace-wide), and the one with the numerically lower priority
// must win, per docs/quotapolicy-design.md §4 ("0 is the strongest
// priority, not higher-number-wins").
func TestResolve_PriorityTieBreak_LowerWins(t *testing.T) {
	claim := Claim{Namespace: "ns", Name: "pvc"}
	policies := []v1alpha1.QuotaPolicy{
		namedPolicy("ns", "weak", 200, v1alpha1.QuotaPolicySelector{}),
		namedPolicy("ns", "strong", 10, v1alpha1.QuotaPolicySelector{}),
	}

	res := Resolve(claim, policies)
	if res.Winner == nil || res.Winner.Name != "strong" {
		t.Fatalf("expected 'strong' (priority 10) to win over 'weak' (priority 200), got %+v", res.Winner)
	}
	if len(res.Losers) != 1 || res.Losers[0].Policy.Name != "weak" {
		t.Fatalf("expected 'weak' to be the sole loser, got %+v", res.Losers)
	}
}

func TestResolve_NameTieBreak(t *testing.T) {
	claim := Claim{Namespace: "ns", Name: "pvc"}
	policies := []v1alpha1.QuotaPolicy{
		namedPolicy("ns", "zzz", 100, v1alpha1.QuotaPolicySelector{}),
		namedPolicy("ns", "aaa", 100, v1alpha1.QuotaPolicySelector{}),
		namedPolicy("ns", "mmm", 100, v1alpha1.QuotaPolicySelector{}),
	}

	res := Resolve(claim, policies)
	if res.Winner == nil || res.Winner.Name != "aaa" {
		t.Fatalf("expected lexicographically smallest name 'aaa' to win, got %+v", res.Winner)
	}
}

func TestResolve_InvalidLabelSelector(t *testing.T) {
	claim := Claim{Namespace: "ns", Name: "pvc", Labels: map[string]string{"tier": "gold"}}
	bad := namedPolicy("ns", "bad", 100, v1alpha1.QuotaPolicySelector{
		LabelSelector: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "tier", Operator: "NotARealOperator", Values: []string{"gold"}},
			},
		},
	})
	good := namedPolicy("ns", "good", 100, v1alpha1.QuotaPolicySelector{})

	res := Resolve(claim, []v1alpha1.QuotaPolicy{bad, good})

	if res.Winner == nil || res.Winner.Name != "good" {
		t.Fatalf("expected the valid namespace-wide policy to win despite the invalid one, got %+v", res.Winner)
	}
	if len(res.Invalid) != 1 || res.Invalid[0].Policy.Name != "bad" {
		t.Fatalf("expected 'bad' reported as invalid, got %+v", res.Invalid)
	}
	if res.Invalid[0].Err == nil {
		t.Fatalf("expected a non-nil error explaining the invalid selector")
	}
	for _, l := range res.Losers {
		if l.Policy.Name == "bad" {
			t.Fatalf("invalid-selector policy must not appear as a loser (it never matched at all): %+v", res.Losers)
		}
	}
}

func TestResolve_OtherNamespaceNotMatched(t *testing.T) {
	claim := Claim{Namespace: "ns-a", Name: "pvc"}
	policies := []v1alpha1.QuotaPolicy{
		namedPolicy("ns-b", "other-ns", 0, v1alpha1.QuotaPolicySelector{}),
	}

	res := Resolve(claim, policies)
	if res.Winner != nil {
		t.Fatalf("expected no winner across namespaces, got %+v", res.Winner)
	}
	if len(res.Invalid) != 0 {
		t.Fatalf("policy from another namespace should not even be selector-checked, got %+v", res.Invalid)
	}
}

func TestValidateSelector(t *testing.T) {
	if err := ValidateSelector(v1alpha1.QuotaPolicySelector{}); err != nil {
		t.Fatalf("namespace-wide selector should be valid: %v", err)
	}
	name := "pvc"
	if err := ValidateSelector(v1alpha1.QuotaPolicySelector{PVCName: &name}); err != nil {
		t.Fatalf("pvcName selector should be valid: %v", err)
	}
	if err := ValidateSelector(labelSelector(map[string]string{"tier": "gold"})); err != nil {
		t.Fatalf("well-formed labelSelector should be valid: %v", err)
	}

	bad := v1alpha1.QuotaPolicySelector{
		LabelSelector: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "tier", Operator: "NotARealOperator"},
			},
		},
	}
	if err := ValidateSelector(bad); err == nil {
		t.Fatalf("expected an error for a malformed labelSelector")
	}
}
