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

// status.go builds a fresh QuotaPolicyStatus from one sync cycle's
// resolution/enforcement results. BuildStatus itself is pure — no I/O — so
// it's testable without a fake dynamic client; WriteStatus in list.go is
// what actually persists the result.

import (
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dasomel/nfs-quota-agent/internal/apis/quota/v1alpha1"
)

// maxStatusSampleEntries bounds FailingClaims and MatchedClaimSample to
// match the CRD's own +kubebuilder:validation:MaxItems=20
// (charts/nfs-quota-agent/crds/quota.nfs.io_quotapolicies.yaml) — exceeding
// it makes the status subresource update fail server-side, so the cap is
// enforced here too rather than relying on the API server to reject an
// oversized write after Go has already built it.
const maxStatusSampleEntries = 20

// ClaimOutcome is one claim's resolution (and, if it won, enforcement)
// result — the unit BuildStatus aggregates into counts and bounded samples
// for one policy. The caller (internal/agent) builds one of these per claim
// per policy it participated in (as winner or as a shadowed loser).
type ClaimOutcome struct {
	Claim      Claim
	MatchKind  v1alpha1.MatchKind
	Won        bool
	ResolvedBy string // name of the policy that won instead, when !Won

	// EnforcementErr, when non-nil, means Won was true but applying the
	// quota to the filesystem failed. Ignored when !Won.
	EnforcementErr error
	// EnforcementReason is a Reason* constant describing EnforcementErr
	// (e.g. ReasonEnforcementFailed, ReasonFilesystemUnavailable,
	// ReasonProjectIDExhausted). Ignored when EnforcementErr is nil.
	EnforcementReason string

	// DriftErr, when non-nil, means Won was true, the enforcement attempt
	// itself reported no error, but an independent read-back check found
	// the on-disk quota no longer matches what this policy currently
	// specifies (#13's Drifted condition, #10's read-back mechanism). The
	// caller only ever sets this when EnforcementErr is nil -- Degraded
	// already covers the case where enforcement itself failed, and
	// BuildStatus's setDrifted defensively ignores DriftErr on an outcome
	// that also has EnforcementErr set, regardless. Mutually exclusive
	// with DriftUnknown below (the caller sets at most one).
	DriftErr error
	// DriftUnknown is true when this claim's drift status genuinely
	// couldn't be determined this cycle -- the on-disk quota report
	// itself was unreadable (a transient xfs_quota/repquota/btrfs
	// failure), not that it was read and found to match. setDrifted
	// reports Drifted=Unknown rather than False when this fires, so a
	// report outage doesn't masquerade as a confirmed "no drift" signal.
	DriftUnknown bool
}

// LimitRangeInfo carries just what BuildStatus needs from
// internal/policy.GetNamespacePolicy to evaluate the LimitRangeConflict
// condition, so this package doesn't need to import internal/policy's full
// NamespacePolicy type for one field.
type LimitRangeInfo struct {
	// Present is true only when the namespace has a LimitRange PVC-max
	// entry to compare against. A LimitRange that sets only Min/Default
	// (no Max), or no LimitRange at all, both count as "not present" here
	// — there is nothing to conflict with either way, so both report
	// ReasonNoLimitRange.
	Present  bool
	MaxBytes int64
}

// BuildStatus computes a fresh QuotaPolicyStatus for policy from this
// cycle's outcomes and the namespace's LimitRange info. previous is the
// policy's current Status.Conditions (before this cycle), used only so
// meta.SetStatusCondition can preserve LastTransitionTime for conditions
// whose status hasn't changed — never read for its counts or samples,
// which are always recomputed fresh every cycle.
func BuildStatus(policy *v1alpha1.QuotaPolicy, outcomes []ClaimOutcome, lr LimitRangeInfo, previous []metav1.Condition, now metav1.Time) v1alpha1.QuotaPolicyStatus {
	status := v1alpha1.QuotaPolicyStatus{
		ObservedGeneration: policy.Generation,
	}

	conditions := append([]metav1.Condition(nil), previous...)

	setReady(&conditions, policy, now)
	appliedCount, wonCount, failing := setAppliedAndDegraded(&conditions, policy, outcomes, now)
	setLimitRangeConflict(&conditions, policy, lr, now)
	drifted := setDrifted(&conditions, policy, outcomes, now)

	status.Conditions = conditions
	status.MatchedClaims = int32(len(outcomes))
	status.AppliedClaims = int32(appliedCount)
	status.ShadowedClaims = int32(len(outcomes) - wonCount)
	status.FailingClaims = capFailingClaims(failing)
	status.DriftedClaims = capDriftedClaims(drifted)
	status.MatchedClaimSample = capMatchedClaims(outcomes)

	return status
}

// setReady evaluates the Ready condition, independent of any claim
// resolution — see ValidateSelector's doc comment for why this must not be
// derived from Resolve's per-claim Invalid list alone (a policy matching
// zero claims is never passed to Resolve at all).
func setReady(conditions *[]metav1.Condition, policy *v1alpha1.QuotaPolicy, now metav1.Time) {
	cond := metav1.Condition{
		Type:               v1alpha1.ConditionReady,
		ObservedGeneration: policy.Generation,
		LastTransitionTime: now,
	}
	if err := ValidateSelector(policy.Spec.Selector); err != nil {
		cond.Status = metav1.ConditionFalse
		cond.Reason = v1alpha1.ReasonSelectorInvalid
		cond.Message = err.Error()
	} else {
		cond.Status = metav1.ConditionTrue
		cond.Reason = v1alpha1.ReasonSelectorValid
		cond.Message = "selector is well-formed"
	}
	meta.SetStatusCondition(conditions, cond)
}

// setAppliedAndDegraded evaluates the Applied and Degraded conditions
// together, since both are derived from the same won/failing split, and
// returns the raw counts and failing sample the caller needs for the rest
// of the status. "Applied" is defined vacuously true when this policy
// currently wins for no claims at all (ReasonNoMatchingClaims) — the same
// convention docs/quotapolicy-design.md §5 states for the type: "every claim
// this policy currently wins for has the quota enforced" is trivially
// satisfied when there are no such claims.
func setAppliedAndDegraded(conditions *[]metav1.Condition, policy *v1alpha1.QuotaPolicy, outcomes []ClaimOutcome, now metav1.Time) (appliedCount, wonCount int, failing []ClaimOutcome) {
	for _, o := range outcomes {
		if !o.Won {
			continue
		}
		wonCount++
		if o.EnforcementErr != nil {
			failing = append(failing, o)
			continue
		}
		appliedCount++
	}

	applied := metav1.Condition{
		Type:               v1alpha1.ConditionApplied,
		ObservedGeneration: policy.Generation,
		LastTransitionTime: now,
	}
	degraded := metav1.Condition{
		Type:               v1alpha1.ConditionDegraded,
		ObservedGeneration: policy.Generation,
		LastTransitionTime: now,
	}

	switch {
	case wonCount == 0:
		applied.Status = metav1.ConditionTrue
		applied.Reason = v1alpha1.ReasonNoMatchingClaims
		applied.Message = "this policy currently wins for no claims"
		degraded.Status = metav1.ConditionFalse
		degraded.Reason = v1alpha1.ReasonNoMatchingClaims
		degraded.Message = "no won claims to fail"
	case len(failing) == 0:
		applied.Status = metav1.ConditionTrue
		applied.Reason = v1alpha1.ReasonAllClaimsApplied
		applied.Message = "every claim this policy won for is enforced"
		degraded.Status = metav1.ConditionFalse
		degraded.Reason = v1alpha1.ReasonAllClaimsApplied
		degraded.Message = "no enforcement failures"
	default:
		applied.Status = metav1.ConditionFalse
		applied.Reason = v1alpha1.ReasonPartiallyApplied
		applied.Message = "one or more won claims are not currently enforced"
		degraded.Status = metav1.ConditionTrue
		degraded.Reason = failingReason(failing)
		degraded.Message = "one or more won claims are failing enforcement; see status.failingClaims"
	}

	meta.SetStatusCondition(conditions, applied)
	meta.SetStatusCondition(conditions, degraded)
	return appliedCount, wonCount, failing
}

// failingReason picks the Degraded condition's Reason from the failing
// outcomes: their own EnforcementReason when every failure agrees, or the
// generic ReasonEnforcementFailed when they don't (or none was set) — the
// Reason field is a single value, so a mixed batch can't report more than
// one specific cause.
func failingReason(failing []ClaimOutcome) string {
	if len(failing) == 0 {
		return v1alpha1.ReasonEnforcementFailed
	}
	reason := failing[0].EnforcementReason
	if reason == "" {
		return v1alpha1.ReasonEnforcementFailed
	}
	for _, o := range failing[1:] {
		if o.EnforcementReason != reason {
			return v1alpha1.ReasonEnforcementFailed
		}
	}
	return reason
}

// setDrifted evaluates the Drifted condition (#13): whether the on-disk
// quota for any won claim no longer matches what this policy currently
// specifies, independent of whether the enforcement attempt itself
// reported an error. Applied/Degraded (setAppliedAndDegraded) already
// cover "did the last enforcement attempt succeed"; Drifted covers "does
// reality agree with that right now" -- these can and do diverge, since
// ensureQuota's own cache short-circuit skips re-verifying an
// already-cached value even if the filesystem state has since changed out
// of band (see #10's verifyQuotaOnDisk and internal/agent's independent
// per-cycle drift check that calls it here). Vacuously false when this
// policy wins for no claims, same convention as Applied/Degraded. Returns
// the drifted outcomes for the caller to build status.driftedClaims from.
func setDrifted(conditions *[]metav1.Condition, policy *v1alpha1.QuotaPolicy, outcomes []ClaimOutcome, now metav1.Time) []ClaimOutcome {
	var drifted []ClaimOutcome
	var unknownCount int
	for _, o := range outcomes {
		if !o.Won || o.EnforcementErr != nil {
			continue
		}
		// DriftErr checked first: the caller (recordEnforcement) only
		// ever sets one of these, never both, but a confirmed mismatch
		// must win over "unknown" if that invariant is ever violated --
		// hiding a known drift behind Unknown would be strictly worse
		// than the reverse.
		switch {
		case o.DriftErr != nil:
			drifted = append(drifted, o)
		case o.DriftUnknown:
			unknownCount++
		}
	}

	cond := metav1.Condition{
		Type:               v1alpha1.ConditionDrifted,
		ObservedGeneration: policy.Generation,
		LastTransitionTime: now,
	}
	switch {
	case len(drifted) > 0:
		cond.Status = metav1.ConditionTrue
		cond.Reason = v1alpha1.ReasonQuotaDriftDetected
		cond.Message = "one or more won claims' on-disk quota no longer matches this policy; see status.driftedClaims"
	case unknownCount > 0:
		// Not False/NoDrift: the report itself was unreadable for these
		// claims, so nothing was actually checked -- reporting a healthy
		// "no drift" during exactly the outage an operator most needs to
		// know about would defeat the point of this condition.
		cond.Status = metav1.ConditionUnknown
		cond.Reason = v1alpha1.ReasonDriftCheckUnavailable
		cond.Message = fmt.Sprintf("could not check %d won claim(s) for drift this cycle; the on-disk quota report was unavailable", unknownCount)
	default:
		cond.Status = metav1.ConditionFalse
		cond.Reason = v1alpha1.ReasonNoDrift
		cond.Message = "on-disk quota matches this policy for every won claim checked this cycle"
	}
	meta.SetStatusCondition(conditions, cond)
	return drifted
}

// setLimitRangeConflict evaluates the LimitRangeConflict condition per
// docs/quotapolicy-design.md §3: the policy still wins and is still
// enforced even when it conflicts, so this only ever reports the
// disagreement — it never changes what quota gets applied.
func setLimitRangeConflict(conditions *[]metav1.Condition, policy *v1alpha1.QuotaPolicy, lr LimitRangeInfo, now metav1.Time) {
	cond := metav1.Condition{
		Type:               v1alpha1.ConditionLimitRangeConflict,
		ObservedGeneration: policy.Generation,
		LastTransitionTime: now,
	}

	switch {
	case !lr.Present:
		cond.Status = metav1.ConditionFalse
		cond.Reason = v1alpha1.ReasonNoLimitRange
		cond.Message = "namespace has no LimitRange PVC max to compare against"
	case policy.Spec.MaxQuota != nil && policy.Spec.MaxQuota.Value() > lr.MaxBytes:
		cond.Status = metav1.ConditionTrue
		cond.Reason = v1alpha1.ReasonExceedsLimitRangeMax
		cond.Message = "spec.maxQuota exceeds the namespace LimitRange PVC max; QuotaPolicy still wins and is still enforced (see docs/quotapolicy-design.md §3)"
	default:
		cond.Status = metav1.ConditionFalse
		cond.Reason = v1alpha1.ReasonWithinLimitRange
		cond.Message = "spec.maxQuota is within the namespace LimitRange PVC max"
	}

	meta.SetStatusCondition(conditions, cond)
}

// capFailingClaims converts up to maxStatusSampleEntries failing outcomes
// into the bounded, most-recent-first FailingClaim sample. "Most recent" is
// simply this cycle's encounter order — there is no history across cycles
// to sort by (docs/quotapolicy-design.md §6: this is a triage sample, not a
// log).
func capFailingClaims(failing []ClaimOutcome) []v1alpha1.FailingClaim {
	if len(failing) == 0 {
		return nil
	}
	n := len(failing)
	if n > maxStatusSampleEntries {
		n = maxStatusSampleEntries
	}
	now := metav1.Now()
	out := make([]v1alpha1.FailingClaim, 0, n)
	for _, o := range failing[:n] {
		reason := o.EnforcementReason
		if reason == "" {
			reason = v1alpha1.ReasonEnforcementFailed
		}
		message := ""
		if o.EnforcementErr != nil {
			message = o.EnforcementErr.Error()
		}
		out = append(out, v1alpha1.FailingClaim{
			Namespace:          o.Claim.Namespace,
			Name:               o.Claim.Name,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: &now,
		})
	}
	return out
}

// capDriftedClaims converts up to maxStatusSampleEntries drifted outcomes
// into the bounded DriftedClaims sample. Mirrors capFailingClaims, reusing
// the same v1alpha1.FailingClaim shape (see DriftedClaims' doc comment for
// why) with DriftErr in place of EnforcementErr and a fixed Reason, since
// every entry here is drift by construction.
func capDriftedClaims(drifted []ClaimOutcome) []v1alpha1.FailingClaim {
	if len(drifted) == 0 {
		return nil
	}
	n := len(drifted)
	if n > maxStatusSampleEntries {
		n = maxStatusSampleEntries
	}
	now := metav1.Now()
	out := make([]v1alpha1.FailingClaim, 0, n)
	for _, o := range drifted[:n] {
		message := ""
		if o.DriftErr != nil {
			message = o.DriftErr.Error()
		}
		out = append(out, v1alpha1.FailingClaim{
			Namespace:          o.Claim.Namespace,
			Name:               o.Claim.Name,
			Reason:             v1alpha1.ReasonQuotaDriftDetected,
			Message:            message,
			LastTransitionTime: &now,
		})
	}
	return out
}

// capMatchedClaims converts up to maxStatusSampleEntries outcomes into the
// bounded MatchedClaimSample, winner(s) first so the sample favors the
// claims this policy actually governs over ones it merely lost.
func capMatchedClaims(outcomes []ClaimOutcome) []v1alpha1.MatchedClaim {
	if len(outcomes) == 0 {
		return nil
	}
	ordered := append([]ClaimOutcome(nil), outcomes...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Won && !ordered[j].Won
	})

	n := len(ordered)
	if n > maxStatusSampleEntries {
		n = maxStatusSampleEntries
	}
	out := make([]v1alpha1.MatchedClaim, 0, n)
	for _, o := range ordered[:n] {
		out = append(out, v1alpha1.MatchedClaim{
			Namespace:  o.Claim.Namespace,
			Name:       o.Claim.Name,
			MatchKind:  o.MatchKind,
			Won:        o.Won,
			ResolvedBy: o.ResolvedBy,
		})
	}
	return out
}
