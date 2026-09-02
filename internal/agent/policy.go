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

// policy.go wires internal/quotapolicy (QuotaPolicy resolution, effective-
// quota bounding, and status write-back) into the agent's existing
// syncAllQuotas cadence. There is deliberately no second watch loop or work
// queue here — see docs/quotapolicy-design.md and CLAUDE.md: ensureQuota
// already serializes every PV through a.mu, and there is exactly one agent
// instance per node, so there is no concurrency for a queue to protect.

import (
	"context"
	"errors"
	"log/slog"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/dasomel/nfs-quota-agent/internal/apis/quota/v1alpha1"
	"github.com/dasomel/nfs-quota-agent/internal/policy"
	"github.com/dasomel/nfs-quota-agent/internal/quotapolicy"
)

// quotaPolicyCycle carries everything one syncAllQuotas pass needs to
// resolve QuotaPolicy objects and, at the end of the pass, write their
// status back. A nil *quotaPolicyCycle — returned by beginQuotaPolicyCycle
// whenever the feature is disabled, unconfigured, or there are simply no
// QuotaPolicy objects to resolve — makes every method below a documented
// no-op, so agent.go's call sites don't need a separate "is this enabled"
// branch.
type quotaPolicyCycle struct {
	client kubernetes.Interface

	byNamespace map[string][]v1alpha1.QuotaPolicy
	// pvcLabels is keyed by "namespace/name" -> the PVC's labels, needed for
	// labelSelector matching. Populated with one List per namespace that has
	// at least one QuotaPolicy, per docs/quotapolicy-design.md's "not one
	// Get per PV" instruction.
	pvcLabels map[string]map[string]string

	// outcomes is keyed by "namespace/policyName" -> every ClaimOutcome
	// recorded against that policy this cycle (as winner or shadowed
	// loser), the input to quotapolicy.BuildStatus.
	outcomes map[string][]quotapolicy.ClaimOutcome

	// matchKindFor is keyed by "namespace/pvcName" -> the MatchKind the
	// current winner earned, set by resolve and consumed by
	// recordEnforcement once the enforcement result (ensureQuota's error,
	// if any) is known. resolve and recordEnforcement are always called as
	// a pair for the same PV within one syncAllQuotas loop iteration, so
	// this simple map is enough — no risk of one PV's entry being read
	// before it's written or clobbered by another.
	matchKindFor map[string]v1alpha1.MatchKind

	// limitRange caches internal/policy.GetNamespacePolicy's LimitRange
	// info per namespace, computed lazily in finishQuotaPolicyCycle (not
	// every namespace-with-a-policy needs it: only ones where the winning
	// policy sets maxQuota end up asking).
	limitRange map[string]quotapolicy.LimitRangeInfo
}

// resolvedPolicySnapshot is the read-only subset of one syncAllQuotas
// cycle's QuotaPolicy resolution inputs that the watch path (watch.go, via
// resolveFromSnapshot) needs between sync cycles. It intentionally carries
// only byNamespace/pvcLabels — never quotaPolicyCycle's outcomes/
// matchKindFor/limitRange maps, which accumulate mutations over the cycle
// and are only ever touched by the syncAllQuotas goroutine. A given
// snapshot's maps are built once in beginQuotaPolicyCycle and never mutated
// afterward, so publishing the pointer under QuotaAgent.mu and letting
// watchPVs' goroutine read the maps lock-free after that is safe: there is
// no writer left to race with.
type resolvedPolicySnapshot struct {
	byNamespace map[string][]v1alpha1.QuotaPolicy
	pvcLabels   map[string]map[string]string
}

// setPolicySnapshot publishes s as the snapshot resolveFromSnapshot reads.
func (a *QuotaAgent) setPolicySnapshot(s *resolvedPolicySnapshot) {
	a.mu.Lock()
	a.policySnapshot = s
	a.mu.Unlock()
}

// resolveFromSnapshot resolves QuotaPolicy for pv against the most recent
// syncAllQuotas cycle's published snapshot, for callers with no cycle of
// their own — specifically watch.go's Added/Modified handler. It returns 0
// ("apply the PV's own capacity") whenever there's nothing to resolve:
// the feature disabled, no snapshot published yet (no sync has completed
// since startup, or since the feature was turned on — raw capacity is the
// correct thing to apply until the first sync has a chance to resolve
// policy), the claim's namespace has no policies, or none of them match.
// It never mutates agent or cycle state and takes no action beyond
// computing a number, so it's safe to call from the watch goroutine
// concurrently with a syncAllQuotas cycle running in the main loop.
func (a *QuotaAgent) resolveFromSnapshot(pv *v1.PersistentVolume) int64 {
	if !a.quotaPolicyEnabled || pv.Spec.ClaimRef == nil {
		return 0
	}
	a.mu.Lock()
	snap := a.policySnapshot
	a.mu.Unlock()
	if snap == nil {
		return 0
	}

	ns, name := pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name
	policies, ok := snap.byNamespace[ns]
	if !ok {
		return 0
	}

	claim := quotapolicy.Claim{Namespace: ns, Name: name, Labels: snap.pvcLabels[ns+"/"+name]}
	res := quotapolicy.Resolve(claim, policies)
	if res.Winner == nil {
		return 0
	}

	requested := int64(0)
	if capacity, ok := pv.Spec.Capacity[v1.ResourceStorage]; ok {
		requested = capacity.Value()
	}
	effective, _ := quotapolicy.EffectiveQuota(requested, res.Winner.Spec)
	return effective
}

// beginQuotaPolicyCycle lists QuotaPolicy objects and the PVCs needed to
// match them, once per syncAllQuotas call, and publishes a
// resolvedPolicySnapshot from that same listing for the watch path (see
// resolveFromSnapshot) — including when there are zero policies, so the
// watch path can tell "policies were listed this cycle; there simply
// aren't any" apart from "no sync has completed yet" (nil snapshot).
//
// It returns nil — meaning "act as if QuotaPolicy doesn't exist" for the
// rest of *this* cycle's sync-path bookkeeping — whenever the feature
// isn't usable right now: disabled, no dynamic client configured, a list
// error, or simply no policies found. None of those are treated as a sync
// failure: a namespace quota policy problem must never block the base
// quota-enforcement loop that predates this feature.
func (a *QuotaAgent) beginQuotaPolicyCycle(ctx context.Context) *quotaPolicyCycle {
	if !a.quotaPolicyEnabled {
		return nil
	}
	if a.dynamicClient == nil {
		slog.Warn("QuotaPolicy enabled but no dynamic client configured (SetDynamicClient); skipping policy resolution this cycle")
		return nil
	}

	policies, err := quotapolicy.List(ctx, a.dynamicClient)
	if err != nil {
		slog.Error("Failed to list QuotaPolicy objects; skipping policy resolution this cycle", "error", err)
		return nil
	}

	byNamespace := make(map[string][]v1alpha1.QuotaPolicy)
	for _, p := range policies {
		byNamespace[p.Namespace] = append(byNamespace[p.Namespace], p)
	}

	pvcLabels := make(map[string]map[string]string)
	for ns := range byNamespace {
		list, err := a.client.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			slog.Warn("Failed to list PersistentVolumeClaims for QuotaPolicy label matching", "namespace", ns, "error", err)
			continue
		}
		for _, pvc := range list.Items {
			pvcLabels[ns+"/"+pvc.Name] = pvc.Labels
		}
	}

	a.setPolicySnapshot(&resolvedPolicySnapshot{byNamespace: byNamespace, pvcLabels: pvcLabels})

	if len(policies) == 0 {
		return nil
	}

	return &quotaPolicyCycle{
		client:       a.client,
		byNamespace:  byNamespace,
		pvcLabels:    pvcLabels,
		outcomes:     make(map[string][]quotapolicy.ClaimOutcome),
		matchKindFor: make(map[string]v1alpha1.MatchKind),
		limitRange:   make(map[string]quotapolicy.LimitRangeInfo),
	}
}

// resolve determines the QuotaPolicy-effective quota size to enforce for
// pv, and records every policy this claim matched (as winner or shadowed
// loser) for the eventual status write-back. It returns (0, nil,
// zero-value BoundDecision) whenever there is nothing to resolve — a nil
// cycle, a PV with no ClaimRef, a namespace with no QuotaPolicy objects, or
// a namespace with policies none of which match this claim — and 0 is
// exactly the sentinel ensureQuota treats as "use the PV's own capacity",
// so every one of those cases reproduces pre-QuotaPolicy behavior
// automatically.
//
// decision is returned (not just consumed internally for the
// BoundAdvisoryOverage log line, as before #14) so the caller can attach
// policy provenance -- winner's name/UID/generation plus decision.Outcome
// -- to the audit entry ensureQuotaMutatedWith produces for this same PV,
// without a second, parallel path re-deriving it. It's only meaningful
// when winner is non-nil.
func (c *quotaPolicyCycle) resolve(pv *v1.PersistentVolume) (effectiveBytes int64, winner *v1alpha1.QuotaPolicy, decision quotapolicy.BoundDecision) {
	if c == nil || pv.Spec.ClaimRef == nil {
		return 0, nil, quotapolicy.BoundDecision{}
	}
	ns, name := pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name
	candidates, ok := c.byNamespace[ns]
	if !ok {
		return 0, nil, quotapolicy.BoundDecision{}
	}

	claimKey := ns + "/" + name
	claim := quotapolicy.Claim{Namespace: ns, Name: name, Labels: c.pvcLabels[claimKey]}
	res := quotapolicy.Resolve(claim, candidates)

	for _, inv := range res.Invalid {
		slog.Warn("QuotaPolicy has an invalid selector; it is excluded from matching until fixed",
			"namespace", inv.Policy.Namespace, "name", inv.Policy.Name, "error", inv.Err)
	}

	for _, l := range res.Losers {
		c.record(l.Policy.Namespace, l.Policy.Name, quotapolicy.ClaimOutcome{
			Claim:      claim,
			MatchKind:  l.MatchKind,
			Won:        false,
			ResolvedBy: res.Winner.Name,
		})
	}

	if res.Winner == nil {
		return 0, nil, quotapolicy.BoundDecision{}
	}

	requested := int64(0)
	if capacity, ok := pv.Spec.Capacity[v1.ResourceStorage]; ok {
		requested = capacity.Value()
	}
	effective, decision := quotapolicy.EffectiveQuota(requested, res.Winner.Spec)
	if decision.Outcome == quotapolicy.BoundAdvisoryOverage {
		slog.Warn("Claim exceeds QuotaPolicy maxQuota but enforceMax is false; applying the requested capacity unchanged",
			"namespace", ns, "pvcName", name, "policy", res.Winner.Name, "detail", decision.Detail)
	}

	c.matchKindFor[claimKey] = res.MatchKind
	return effective, res.Winner, decision
}

// recordEnforcement records the enforcement outcome (ensureQuota's error,
// if any) for pv's winning QuotaPolicy, once it's known. winner is exactly
// what resolve returned for this same pv earlier in the same loop
// iteration; nil means resolve found no matching policy, in which case
// there is nothing to record and this is a no-op, same as a nil cycle.
//
// driftErr, when non-nil, is an independent read-back mismatch (#10's
// verifyQuotaOnDisk, checked by the caller regardless of whether
// ensureQuota actually mutated anything this cycle -- see agent.go's
// syncAllQuotas). Only meaningful when err is nil: an enforcement failure
// already explains why the filesystem might disagree, so driftErr is
// ignored (not recorded) whenever err is non-nil, keeping Degraded and
// Drifted from both firing off the same underlying cause.
func (c *quotaPolicyCycle) recordEnforcement(winner *v1alpha1.QuotaPolicy, pv *v1.PersistentVolume, err error, drift driftCheck) {
	if c == nil || winner == nil || pv.Spec.ClaimRef == nil {
		return
	}
	ns, name := pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name
	claimKey := ns + "/" + name

	outcome := quotapolicy.ClaimOutcome{
		Claim:     quotapolicy.Claim{Namespace: ns, Name: name, Labels: c.pvcLabels[claimKey]},
		MatchKind: c.matchKindFor[claimKey],
		Won:       true,
	}
	switch {
	case err != nil:
		outcome.EnforcementErr = err
		outcome.EnforcementReason = classifyEnforcementError(err)
	case drift.unknown:
		outcome.DriftUnknown = true
	case drift.err != nil:
		outcome.DriftErr = drift.err
	}
	c.record(winner.Namespace, winner.Name, outcome)
}

// driftCheck carries the independent read-back drift check's outcome for
// one claim (#13's Drifted condition) into recordEnforcement. The zero
// value means "not checked this cycle" -- enforcement itself failed, there
// was no local directory, or the claim was mutated by ensureQuota this
// same cycle (see syncAllQuotas' doc comment on why a freshly mutated
// claim isn't re-checked against a report snapshot that may predate its
// own mutation). err and unknown are mutually exclusive: unknown means the
// on-disk quota report itself couldn't be read this cycle, which is a
// different, weaker signal than err (the report was read successfully and
// disagreed) -- see setDrifted's Unknown-vs-False handling.
type driftCheck struct {
	err     error
	unknown bool
}

// record appends outcome to the running list for the policy identified by
// namespace/policyName.
func (c *quotaPolicyCycle) record(namespace, policyName string, outcome quotapolicy.ClaimOutcome) {
	key := namespace + "/" + policyName
	c.outcomes[key] = append(c.outcomes[key], outcome)
}

// errLocalDirectoryMissing is the synthetic error substituted for a claim
// this node resolved a winning policy for, but whose backing directory does
// not exist locally, when quotaPolicySingleWriter is true. ensureQuota
// itself returns nil (not an error) for that case — see its "Directory
// does not exist, skipping quota" log line — which is the right contract
// for enforcement (a missing directory isn't a retryable failure) but would
// otherwise make recordEnforcement treat "silently skipped" the same as
// "successfully applied". See the syncAllQuotas loop in agent.go for where
// this is substituted, and docs/quotapolicy-design.md §11.
var errLocalDirectoryMissing = errors.New("PV local directory does not exist on this node")

// classifyEnforcementError maps an ensureQuota error to one of the fixed
// Reason* constants for FailingClaim/Degraded reporting, falling back to
// the generic ReasonEnforcementFailed for anything not specifically
// recognized. errProjectIDExhausted, errLocalDirectoryMissing,
// ErrHAStandby (ha.go), and errUnsafeShrink (agent.go) are the only cases
// distinguished today; docs/quotapolicy-design.md §5 already anticipates
// ReasonProjectIDExhausted may turn out to be unreachable in practice given
// the existing collision-fallback in generateProjectID — the same
// "included for the vocabulary, may never fire" reasoning could apply to
// any of the four.
func classifyEnforcementError(err error) string {
	switch {
	case errors.Is(err, errProjectIDExhausted):
		return v1alpha1.ReasonProjectIDExhausted
	case errors.Is(err, errLocalDirectoryMissing):
		return v1alpha1.ReasonFilesystemUnavailable
	case errors.Is(err, ErrHAStandby):
		return v1alpha1.ReasonHAStandby
	case errors.Is(err, errUnsafeShrink):
		return v1alpha1.ReasonUnsafeShrinkRejected
	default:
		return v1alpha1.ReasonEnforcementFailed
	}
}

// finishQuotaPolicyCycle writes a fresh status to every QuotaPolicy object
// this cycle observed, whether or not it won any claims — a policy that
// currently matches nothing still needs its Ready condition reported (see
// quotapolicy.BuildStatus). No-op on a nil cycle.
//
// It is also a no-op — logged once, not once per cycle — unless
// quotaPolicySingleWriter is true. This chart is a DaemonSet that
// explicitly supports several NFS server nodes running at once (see
// values.yaml's nodeSelector comment): every one of them lists the exact
// same cluster-wide QuotaPolicy objects, but each only ever enforces (and,
// since syncAllQuotas now applies the has-local-directory predicate before
// recording an outcome, only ever *counts*) the claims whose backing
// directory lives on its own node. If every node still called
// WriteStatus, N agents would each overwrite the same policy's status with
// their own honest-but-partial appliedClaims/failingClaims/conditions every
// sync cycle, and the object would appear to flap between N different
// partial views rather than settle — worse than not reporting, because it
// reads as an intermittent bug rather than an unsupported topology. Rather
// than publish a number that might be one node's partial view, this skips
// the write and leaves status exactly as it was (stale, but honestly so —
// `status.observedGeneration` will fall behind `metadata.generation`,
// itself a signal something isn't reconciling). An operator who has
// verified they run exactly one --enable-quota-policy agent opts in via
// SetQuotaPolicySingleWriter / --quota-policy-single-writer /
// quotaPolicy.singleWriter. Real leader election (a coordination.k8s.io
// Lease) would let this work unattended on multiple nodes, but that needs
// its own RBAC grant and is a materially larger change than this PR; see
// docs/quotapolicy-design.md §11.
func (a *QuotaAgent) finishQuotaPolicyCycle(ctx context.Context, cycle *quotaPolicyCycle) {
	if cycle == nil {
		return
	}
	if !a.quotaPolicySingleWriter {
		a.quotaPolicyStatusSkipLogOnce.Do(func() {
			slog.Warn("QuotaPolicy status write-back is disabled: quotaPolicySingleWriter is not set. " +
				"This DaemonSet may run on several NFS server nodes; each would otherwise overwrite the same " +
				"QuotaPolicy's status with its own partial view. Set --quota-policy-single-writer (or " +
				"quotaPolicy.singleWriter in the chart) only if exactly one agent has --enable-quota-policy set. " +
				"Quota enforcement itself is unaffected.")
		})
		return
	}

	now := metav1.Now()
	for ns, policies := range cycle.byNamespace {
		lr := cycle.limitRangeInfo(ctx, ns)
		for i := range policies {
			p := &policies[i]
			outcomes := cycle.outcomes[ns+"/"+p.Name]
			status := quotapolicy.BuildStatus(p, outcomes, lr, p.Status.Conditions, now)
			if err := quotapolicy.WriteStatus(ctx, a.dynamicClient, p, status); err != nil {
				slog.Error("Failed to write QuotaPolicy status", "namespace", ns, "name", p.Name, "error", err)
			}
		}
	}
}

// limitRangeInfo returns (and caches) the LimitRangeInfo for ns, used to
// evaluate the LimitRangeConflict condition. Reuses
// internal/policy.GetNamespacePolicy — the same LimitRange lookup the
// web UI's advisory Policies view already performs — rather than re-reading
// LimitRange objects directly.
func (c *quotaPolicyCycle) limitRangeInfo(ctx context.Context, ns string) quotapolicy.LimitRangeInfo {
	if lr, ok := c.limitRange[ns]; ok {
		return lr
	}

	var lr quotapolicy.LimitRangeInfo
	if c.client != nil {
		pol, err := policy.GetNamespacePolicy(ctx, c.client, ns)
		if err != nil {
			slog.Warn("Failed to get namespace policy for LimitRangeConflict check", "namespace", ns, "error", err)
		} else if pol.LimitRangeMax > 0 {
			lr = quotapolicy.LimitRangeInfo{Present: true, MaxBytes: pol.LimitRangeMax}
		}
	}
	c.limitRange[ns] = lr
	return lr
}
