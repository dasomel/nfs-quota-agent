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

// Package quotapolicy resolves which QuotaPolicy object, if any, governs a
// given PersistentVolumeClaim, and computes the filesystem quota bound to
// enforce for it. resolve.go and bound.go are pure — no I/O, no Kubernetes
// client — so the precedence and bounding rules are testable with plain
// structs; list.go and status.go are the only files in this package that
// talk to the cluster. See docs/quotapolicy-design.md for the full design.
package quotapolicy

import (
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/dasomel/nfs-quota-agent/internal/apis/quota/v1alpha1"
)

// Claim is the minimal shape of a PersistentVolumeClaim needed to resolve
// policy — decoupled from corev1.PersistentVolumeClaim so this package
// never needs a Kubernetes client type to be tested.
type Claim struct {
	Namespace        string
	Name             string
	Labels           map[string]string
	StorageClassName string
}

// Shadowed records a QuotaPolicy that matched a claim but lost precedence
// to the Resolution's Winner.
type Shadowed struct {
	Policy    *v1alpha1.QuotaPolicy
	MatchKind v1alpha1.MatchKind
	// Reason is a human-readable note on which rule decided this policy
	// lost: specificity, priority, or name (see docs/quotapolicy-design.md
	// §4). It is prose for logs/status messages, not one of the fixed
	// Reason* condition constants.
	Reason string
}

// InvalidSelector records a candidate QuotaPolicy (from the namespace being
// resolved) whose labelSelector could not be converted to a usable
// selector. An invalid selector never matches any claim — it is excluded
// from both Winner and Losers entirely, not merely deprioritized — but the
// caller still needs to know it was skipped so it can set that policy's own
// Ready=False/SelectorInvalid condition rather than silently treating it as
// "matches nothing".
type InvalidSelector struct {
	Policy *v1alpha1.QuotaPolicy
	Err    error
}

// Resolution is the outcome of resolving one claim against a set of
// QuotaPolicy objects.
type Resolution struct {
	// Winner is the QuotaPolicy this claim resolves to, or nil when none of
	// the supplied policies match.
	Winner *v1alpha1.QuotaPolicy
	// MatchKind is meaningful only when Winner != nil.
	MatchKind v1alpha1.MatchKind
	// Losers is every other matching policy, ordered most-specific-first,
	// with why each one lost to Winner.
	Losers []Shadowed
	// Invalid lists every policy considered (i.e. in claim.Namespace)
	// whose selector failed to evaluate. Populated the same way regardless
	// of claim, since selector validity is a property of the policy, not
	// of the claim being resolved against it — see ValidateSelector for
	// the standalone, claim-independent check the status writer uses to
	// build a policy's Ready condition even when it matches zero claims.
	Invalid []InvalidSelector
}

// candidate pairs a matching policy with the MatchKind it earned against
// the claim being resolved, used internally to sort by precedence.
type candidate struct {
	policy    *v1alpha1.QuotaPolicy
	matchKind v1alpha1.MatchKind
}

// Resolve determines which of policies (if any) governs claim, applying the
// precedence rules from docs/quotapolicy-design.md §4 in order:
//
//  1. Specificity: PVCName > LabelSelector > NamespaceWide.
//  2. Tie -> lowest spec.priority wins. 0 is the strongest priority. This
//     compares Priority exactly as it is on the struct — the CRD's
//     +kubebuilder:default=100 is an API-server admission-time default, not
//     a Go zero-value default, so a QuotaPolicy built as a bare struct
//     literal (as every test here does) legitimately carries Priority == 0.
//     That value is not re-defaulted to 100 here: 0 already sorts as the
//     strongest priority, and re-applying the CRD default in Go would
//     silently disagree with an admitted object that explicitly set
//     priority: 0.
//  3. Still tied -> lexicographically smallest metadata.name wins.
//
// Only policies whose Namespace equals claim.Namespace are eligible, since
// QuotaPolicy is namespace-scoped (docs/quotapolicy-design.md §2). Resolve
// never calls the Kubernetes API; policies must already be fetched (see
// List in list.go).
func Resolve(claim Claim, policies []v1alpha1.QuotaPolicy) Resolution {
	var res Resolution
	var candidates []candidate

	for i := range policies {
		p := &policies[i]
		if p.Namespace != claim.Namespace {
			continue
		}

		kind, matches, err := matchSelector(p.Spec.Selector, claim)
		if err != nil {
			res.Invalid = append(res.Invalid, InvalidSelector{Policy: p, Err: err})
			continue
		}
		if !matches {
			continue
		}
		candidates = append(candidates, candidate{policy: p, matchKind: kind})
	}

	if len(candidates) == 0 {
		return res
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return wins(candidates[i], candidates[j])
	})

	res.Winner = candidates[0].policy
	res.MatchKind = candidates[0].matchKind

	for _, c := range candidates[1:] {
		res.Losers = append(res.Losers, Shadowed{
			Policy:    c.policy,
			MatchKind: c.matchKind,
			Reason:    reasonLostTo(candidates[0], c),
		})
	}

	return res
}

// specificityRank orders MatchKind from most (0) to least (2) specific.
func specificityRank(k v1alpha1.MatchKind) int {
	switch k {
	case v1alpha1.MatchKindPVCName:
		return 0
	case v1alpha1.MatchKindLabelSelector:
		return 1
	default: // v1alpha1.MatchKindNamespaceWide
		return 2
	}
}

// wins reports whether a outranks (beats) b under the three-step tie-break.
func wins(a, b candidate) bool {
	if ra, rb := specificityRank(a.matchKind), specificityRank(b.matchKind); ra != rb {
		return ra < rb
	}
	if a.policy.Spec.Priority != b.policy.Spec.Priority {
		return a.policy.Spec.Priority < b.policy.Spec.Priority // lower wins
	}
	return a.policy.Name < b.policy.Name
}

// reasonLostTo names, for Shadowed.Reason, which rule decided winner beat
// loser.
func reasonLostTo(winner, loser candidate) string {
	if specificityRank(winner.matchKind) != specificityRank(loser.matchKind) {
		return fmt.Sprintf("specificity: %s outranks %s", winner.matchKind, loser.matchKind)
	}
	if winner.policy.Spec.Priority != loser.policy.Spec.Priority {
		return fmt.Sprintf("priority: %d beats %d (lower wins)", winner.policy.Spec.Priority, loser.policy.Spec.Priority)
	}
	return fmt.Sprintf("name: %q sorts before %q", winner.policy.Name, loser.policy.Name)
}

// ValidateSelector checks a QuotaPolicySelector's shape independent of any
// specific claim, so the status writer can set a policy's Ready condition
// even when it currently matches zero claims (Resolve is never invoked for
// a namespace with no bound PVs). Today the only failure mode is a
// LabelSelector that doesn't convert to a usable label selector; PVCName
// has no comparable failure (length is already bounded by the CRD's
// MinLength/MaxLength), and PVCName/LabelSelector mutual exclusivity is
// enforced by the CRD's own XValidation rule, so it isn't re-checked here.
func ValidateSelector(sel v1alpha1.QuotaPolicySelector) error {
	if sel.LabelSelector == nil {
		return nil
	}
	if _, err := metav1.LabelSelectorAsSelector(sel.LabelSelector); err != nil {
		return fmt.Errorf("invalid labelSelector: %w", err)
	}
	return nil
}

// matchSelector reports whether sel matches claim, and which MatchKind it
// earned. An error means the selector could not be evaluated at all —
// callers must treat that as "does not match" and separately report it
// (see InvalidSelector), not silently drop the policy as a non-match.
func matchSelector(sel v1alpha1.QuotaPolicySelector, claim Claim) (v1alpha1.MatchKind, bool, error) {
	if len(sel.StorageClassNames) > 0 {
		if claim.StorageClassName == "" {
			return "", false, nil
		}
		found := false
		for _, sc := range sel.StorageClassNames {
			if sc == claim.StorageClassName {
				found = true
				break
			}
		}
		if !found {
			return "", false, nil
		}
	}

	if sel.PVCName != nil {
		if *sel.PVCName == claim.Name {
			return v1alpha1.MatchKindPVCName, true, nil
		}
		return "", false, nil
	}
	if sel.LabelSelector != nil {
		selector, err := metav1.LabelSelectorAsSelector(sel.LabelSelector)
		if err != nil {
			return "", false, fmt.Errorf("invalid labelSelector: %w", err)
		}
		if selector.Matches(labels.Set(claim.Labels)) {
			return v1alpha1.MatchKindLabelSelector, true, nil
		}
		return "", false, nil
	}
	// Namespace-wide: both unset, matches every claim in the namespace —
	// the caller already filtered by namespace above.
	return v1alpha1.MatchKindNamespaceWide, true, nil
}
