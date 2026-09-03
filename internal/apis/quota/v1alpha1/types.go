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

// Package v1alpha1 contains the QuotaPolicy API, the first alpha version of
// the quota.nfs.io API group. QuotaPolicy lets operators declare filesystem
// quota bounds for PersistentVolumeClaims as a Kubernetes object instead of
// (or on top of) the nfs.io/default-quota and nfs.io/max-quota namespace
// annotations consumed today by internal/policy.GetNamespacePolicy. See
// docs/quotapolicy-design.md for the full design rationale.
//
// NOTE: QuotaPolicy and QuotaPolicyList implement runtime.Object via the
// DeepCopyObject/DeepCopyInto methods in zz_generated.deepcopy.go (`make
// generate`, backed by `controller-gen object`; see the compile-time check
// below). register.go in this package registers both types (and
// SchemeGroupVersion/AddToScheme) with a runtime.Scheme, so a typed
// client-go clientset can be built against this API group. Note that
// internal/quotapolicy deliberately keeps using the dynamic/unstructured
// client rather than this typed registration — see
// docs/quotapolicy-design.md §11; the registration here targets
// future/external tooling (kubectl plugins, other controllers).
//
// +groupName=quota.nfs.io
// +k8s:deepcopy-gen=package
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Compile-time assertions that generation actually produced DeepCopyObject
// for both root types, rather than trusting that `make generate` ran and
// succeeded. If zz_generated.deepcopy.go is ever deleted or regenerated
// against a stale controller-gen version that drops these methods, this
// fails the build instead of failing silently at first runtime use.
var (
	_ runtime.Object = &QuotaPolicy{}
	_ runtime.Object = &QuotaPolicyList{}
)

const (
	// GroupName is the API group for QuotaPolicy and any future types added
	// to this group. It deliberately lives under nfs.io, matching every
	// annotation the agent already reads/writes (nfs.io/project-name,
	// nfs.io/quota-status, nfs.io/default-quota, nfs.io/max-quota), rather
	// than a product-specific group: the agent must keep working standalone.
	GroupName = "quota.nfs.io"

	// GroupVersion is the version of this alpha API. Additive changes only
	// are permitted within v1alpha1; see "Conversion strategy" in
	// docs/quotapolicy-design.md for what would force a v1alpha2/v1.
	GroupVersion = "v1alpha1"

	// QuotaPolicyKind is the Kind string for QuotaPolicy, as registered
	// with SchemeGroupVersion in register.go.
	QuotaPolicyKind = "QuotaPolicy"
)

// Condition types reported in QuotaPolicyStatus.Conditions. Each is a
// distinct axis of policy health rather than a state machine — more than
// one may be True at once (e.g. Applied=True and Drifted=True describes a
// policy that is enforced but has since drifted out of band).
const (
	// ConditionReady summarizes whether the policy object itself is well
	// formed and its selector resolvable (independent of whether any quota
	// enforcement has happened yet).
	ConditionReady = "Ready"

	// ConditionApplied reports whether the resolved quota bounds have been
	// successfully enforced on every PVC this policy currently wins for.
	ConditionApplied = "Applied"

	// ConditionDegraded reports enforcement failures on one or more won
	// claims (see Status.FailingClaims for the sample).
	ConditionDegraded = "Degraded"

	// ConditionDrifted reports that the on-disk/on-filesystem quota for one
	// or more won claims no longer matches what this policy currently
	// specifies (e.g. edited out of band, or the policy spec changed and
	// reconciliation hasn't caught up yet).
	ConditionDrifted = "Drifted"

	// ConditionLimitRangeConflict reports that this policy grants a quota
	// larger than the matching namespace's LimitRange PVC max. The policy
	// still wins (QuotaPolicy outranks LimitRange in the precedence chain —
	// see docs/quotapolicy-design.md "Precedence"), but the disagreement
	// between the admission-time PVC size cap and the filesystem-enforcement
	// bound is surfaced here rather than silently resolved.
	ConditionLimitRangeConflict = "LimitRangeConflict"

	// ConditionStorageClassBinding reports whether a policy with StorageClass
	// restrictions matched any PVs or encountered a path fallback rejection.
	// Omitted for policies with no StorageClass restrictions.
	ConditionStorageClassBinding = "StorageClassBinding"
)

// Fixed reason vocabulary for the conditions above. Reasons are API surface
// consumed by scripts/dashboards, so call sites must pick from this list
// rather than inventing new reasons ad hoc.
const (
	ReasonSelectorValid    = "SelectorValid"
	ReasonSelectorInvalid  = "SelectorInvalid"
	ReasonNoMatchingClaims = "NoMatchingClaims"

	ReasonAllClaimsApplied = "AllClaimsApplied"
	ReasonPartiallyApplied = "PartiallyApplied"
	ReasonNotYetReconciled = "NotYetReconciled"

	ReasonEnforcementFailed     = "EnforcementFailed"
	ReasonFilesystemUnavailable = "FilesystemUnavailable"
	ReasonProjectIDExhausted    = "ProjectIDExhausted"
	ReasonHAStandby             = "HAStandby"
	ReasonUnsafeShrinkRejected  = "UnsafeShrinkRejected"

	ReasonQuotaDriftDetected = "QuotaDriftDetected"
	ReasonNoDrift            = "NoDrift"
	// ReasonDriftCheckUnavailable pairs with Drifted=Unknown: the on-disk
	// quota report itself couldn't be read this cycle (a transient
	// xfs_quota/repquota/btrfs failure), so no won claim was actually
	// checked -- distinct from ReasonNoDrift, which means claims were
	// checked and matched. Reporting False/NoDrift here would be a false
	// "healthy" signal during exactly the outage an operator most needs
	// to know about.
	ReasonDriftCheckUnavailable = "DriftCheckUnavailable"

	ReasonExceedsLimitRangeMax       = "ExceedsLimitRangeMax"
	ReasonBelowLimitRangeMin         = "BelowLimitRangeMin"
	ReasonMinQuotaBelowLimitRangeMin = "MinQuotaBelowLimitRangeMin"
	ReasonWithinLimitRange           = "WithinLimitRange"
	ReasonNoLimitRange               = "NoLimitRange"

	ReasonStorageClassBindingNoMatchingPV         = "StorageClassBindingNoMatchingPV"
	ReasonStorageClassBindingPathFallbackRejected = "StorageClassBindingPathFallbackRejected"
	ReasonStorageClassBindingBound                = "StorageClassBindingBound"
)

// MatchKind records which clause of a QuotaPolicySelector matched a given
// PersistentVolumeClaim, i.e. its specificity rank for precedence purposes.
// +kubebuilder:validation:Enum=PVCName;LabelSelector;NamespaceWide
type MatchKind string

const (
	// MatchKindPVCName is the most specific match: an explicit PVC name.
	MatchKindPVCName MatchKind = "PVCName"
	// MatchKindLabelSelector is a label-selector match.
	MatchKindLabelSelector MatchKind = "LabelSelector"
	// MatchKindNamespaceWide is the least specific match: an empty
	// selector, matching every PVC in the policy's namespace.
	MatchKindNamespaceWide MatchKind = "NamespaceWide"
)

// QuotaPolicySelector chooses which PersistentVolumeClaims, within the
// QuotaPolicy's own namespace, a policy governs. Exactly one of PVCName or
// LabelSelector may be set; leaving both unset selects every PVC in the
// namespace. This mirrors the specificity order used to break ties between
// multiple matching policies (PVCName > LabelSelector > namespace-wide) —
// see docs/quotapolicy-design.md "Multiple matching policies".
// +kubebuilder:validation:XValidation:rule="!(has(self.pvcName) && has(self.labelSelector))",message="pvcName and labelSelector are mutually exclusive"
type QuotaPolicySelector struct {
	// pvcName selects a single PersistentVolumeClaim by name in the
	// policy's namespace. This is the most specific selector kind and wins
	// precedence over labelSelector and namespace-wide policies.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +optional
	PVCName *string `json:"pvcName,omitempty"`

	// labelSelector selects PersistentVolumeClaims by label within the
	// policy's namespace. Ranks below pvcName and above a namespace-wide
	// (empty) selector.
	// +optional
	LabelSelector *metav1.LabelSelector `json:"labelSelector,omitempty"`

	// storageClassNames selects PersistentVolumeClaims bound to PersistentVolumes
	// with one of the specified StorageClass names. Empty or nil matches any StorageClass.
	// Evaluated as an AND with pvcName/labelSelector.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$
	StorageClassNames []string `json:"storageClassNames,omitempty"`
}

// QuotaPolicySpec declares the filesystem quota bounds to enforce for the
// PersistentVolumeClaims matched by Selector, and this policy's priority
// relative to other QuotaPolicy objects that might match the same claim.
//
// The ordering rules below are enforced by the API server rather than left
// to reconcile time, so an inverted spec is rejected at apply. They go
// through the CEL quantity() library because these are resource.Quantity
// values: comparing them as strings gets same-magnitude pairs backwards —
// by value 10Gi > 9Gi, but lexically "10Gi" < "9Gi" (the leading "1" sorts
// before "9") — verified against a live 1.36 cluster: applying
// minQuota=10Gi/maxQuota=9Gi is rejected, and applying the valid inverse
// (minQuota=9Gi/maxQuota=10Gi, the pair a naive string rule would reject)
// is accepted. quantity() has been available and GA since Kubernetes 1.29,
// well below this project's floor.
//
// Every field involved is optional, so each rule guards on both of its
// operands being present — a rule that assumed a field were set would reject
// otherwise valid objects that simply omit it.
//
// +kubebuilder:validation:XValidation:rule="!has(self.minQuota) || !has(self.maxQuota) || quantity(self.minQuota).compareTo(quantity(self.maxQuota)) <= 0",message="minQuota must not exceed maxQuota"
// +kubebuilder:validation:XValidation:rule="!has(self.defaultQuota) || !has(self.minQuota) || quantity(self.defaultQuota).compareTo(quantity(self.minQuota)) >= 0",message="defaultQuota must not be smaller than minQuota"
// +kubebuilder:validation:XValidation:rule="!has(self.defaultQuota) || !has(self.maxQuota) || quantity(self.defaultQuota).compareTo(quantity(self.maxQuota)) <= 0",message="defaultQuota must not exceed maxQuota"
type QuotaPolicySpec struct {
	// selector determines which PersistentVolumeClaims in this namespace
	// this policy applies to.
	// +kubebuilder:validation:Required
	Selector QuotaPolicySelector `json:"selector"`

	// priority breaks ties when multiple QuotaPolicy objects match the same
	// PVC with equal selector specificity (see MatchKind). Lower values win
	// — this follows the same convention as Kubernetes PriorityClass and
	// scheduler-extender priorities, where 0 is the strongest priority, not
	// the weakest. If priority also ties, the lexicographically smallest
	// QuotaPolicy name wins, so resolution is always deterministic.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=100
	// +optional
	Priority int32 `json:"priority,omitempty"`

	// defaultQuota is the filesystem project quota size applied when a
	// matched PVC does not otherwise pin its own size expectation.
	// Mirrors internal/policy.NamespacePolicy.DefaultQuota /
	// the nfs.io/default-quota annotation it supersedes.
	// +optional
	// +kubebuilder:validation:XValidation:rule="quantity(self).compareTo(quantity('0')) >= 0",message="defaultQuota must not be negative"
	DefaultQuota *resource.Quantity `json:"defaultQuota,omitempty"`

	// maxQuota is the largest filesystem project quota this policy permits
	// for a matched PVC. Mirrors internal/policy.NamespacePolicy.MaxQuota /
	// the nfs.io/max-quota annotation it supersedes.
	// +optional
	// +kubebuilder:validation:XValidation:rule="quantity(self).compareTo(quantity('0')) >= 0",message="maxQuota must not be negative"
	MaxQuota *resource.Quantity `json:"maxQuota,omitempty"`

	// minQuota is the smallest filesystem project quota this policy permits
	// for a matched PVC. There is no namespace-annotation equivalent today;
	// internal/policy.NamespacePolicy.MinQuota is currently populated only
	// from a LimitRange's PVC min.
	// +optional
	// +kubebuilder:validation:XValidation:rule="quantity(self).compareTo(quantity('0')) >= 0",message="minQuota must not be negative"
	MinQuota *resource.Quantity `json:"minQuota,omitempty"`

	// enforceMax controls whether maxQuota is a hard limit (a matched claim
	// requesting more is clamped down to it) or advisory (the overage is
	// recorded via quotapolicy.BoundDecision but not enforced). Defaults to
	// true; set false to observe a rollout before enforcing it.
	// +kubebuilder:default=true
	// +optional
	EnforceMax bool `json:"enforceMax,omitempty"`
}

// FailingClaim names a PersistentVolumeClaim this policy won precedence for
// but failed to enforce on, and why. This is a bounded, most-recent-first
// sample for triage — not an audit log or a substitute for time series. For
// quota-application history over time, consult the agent's Prometheus
// metrics or, if history.enabled in the Helm chart, internal/history.Store's
// persisted snapshots; this field only ever reflects current failures.
type FailingClaim struct {
	// namespace of the failing PersistentVolumeClaim. Always equal to the
	// QuotaPolicy's own namespace, since QuotaPolicy is namespace-scoped;
	// carried explicitly so this struct is self-describing at the call
	// site and stays shaped like MatchedClaim below.
	Namespace string `json:"namespace"`

	// name of the failing PersistentVolumeClaim.
	Name string `json:"name"`

	// reason is a short machine-readable cause, drawn from the same
	// vocabulary as Conditions[].Reason where applicable (e.g.
	// EnforcementFailed, FilesystemUnavailable, ProjectIDExhausted).
	Reason string `json:"reason"`

	// message is a human-readable detail, e.g. the underlying command
	// error surfaced from internal/quota.CommandRunner.
	// +optional
	Message string `json:"message,omitempty"`

	// lastTransitionTime is when this claim was last observed to fail.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
}

// MatchedClaim records a PersistentVolumeClaim this policy's selector
// currently matches, and how the multi-policy precedence chain resolved for
// it. This is what makes precedence auditable: for the sampled claims below
// you can see not just "applied" but which selector rank matched and, when
// this policy lost, who won instead. Bounded and best-effort in the same
// way as FailingClaim — see that type's doc comment for the general
// status-sizing rationale.
type MatchedClaim struct {
	// namespace of the matched PersistentVolumeClaim.
	Namespace string `json:"namespace"`

	// name of the matched PersistentVolumeClaim.
	Name string `json:"name"`

	// matchKind is which selector clause matched: pvcName, labelSelector,
	// or the namespace-wide default.
	MatchKind MatchKind `json:"matchKind"`

	// won is true if this policy is the one actually enforced for this
	// claim after resolving precedence against every other QuotaPolicy
	// that also matched it.
	Won bool `json:"won"`

	// resolvedBy names the QuotaPolicy that won instead, when won is
	// false. Empty when won is true.
	// +optional
	ResolvedBy string `json:"resolvedBy,omitempty"`
}

// QuotaPolicyStatus reports what a QuotaPolicy is currently doing: whether
// it is well-formed, how many claims it matches and has successfully
// applied to, and bounded samples for triage and precedence auditing. It
// intentionally excludes usage figures and historical trends — see
// FailingClaim's doc comment for where those live instead.
type QuotaPolicyStatus struct {
	// observedGeneration is the .metadata.generation last reconciled by the
	// controller. Compare against .metadata.generation to tell whether the
	// rest of this status reflects the current spec.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions is the standard Kubernetes condition list. See the
	// Condition* and Reason* constants in this package for the fixed set
	// of types and reasons this controller reports.
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// matchedClaims is the count of PersistentVolumeClaims this policy's
	// selector currently matches, regardless of whether this policy won
	// precedence for them.
	// +optional
	MatchedClaims int32 `json:"matchedClaims,omitempty"`

	// appliedClaims is the count of matched claims this policy won
	// precedence for and successfully enforced a quota on.
	// +optional
	AppliedClaims int32 `json:"appliedClaims,omitempty"`

	// shadowedClaims is the count of matched claims where a different,
	// higher-precedence QuotaPolicy won instead.
	// +optional
	ShadowedClaims int32 `json:"shadowedClaims,omitempty"`

	// failingClaims is a bounded, most-recent-first sample (capped at 20
	// entries; oldest evicted first) of won claims where enforcement is
	// currently failing.
	// +kubebuilder:validation:MaxItems=20
	// +optional
	FailingClaims []FailingClaim `json:"failingClaims,omitempty"`

	// driftedClaims is a bounded sample (capped at 20 entries) of won
	// claims whose on-disk quota no longer matches this policy, checked
	// independently of the enforcement attempt itself -- a claim only
	// appears here when its most recent enforcement attempt reported no
	// error (see FailingClaims for that case instead). Reuses the
	// FailingClaim shape rather than a parallel type: same
	// namespace/name/reason/message/lastTransitionTime fields apply.
	// +kubebuilder:validation:MaxItems=20
	// +optional
	DriftedClaims []FailingClaim `json:"driftedClaims,omitempty"`

	// matchedClaimSample is a bounded sample (capped at 20 entries) of
	// MatchedClaim covering this policy's matched claims, used to audit
	// precedence decisions. Not exhaustive above the cap; for the full
	// picture on a specific PVC, list every QuotaPolicy in its namespace
	// and check which ones match it.
	// +kubebuilder:validation:MaxItems=20
	// +optional
	MatchedClaimSample []MatchedClaim `json:"matchedClaimSample,omitempty"`
}

// QuotaPolicy declares filesystem project quota bounds — default, max, and
// min size — for a set of PersistentVolumeClaims in its own namespace, as a
// GitOps-managed alternative (and, per the precedence chain in
// docs/quotapolicy-design.md, override) to the nfs.io/default-quota and
// nfs.io/max-quota namespace annotations and to LimitRange.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=qp
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Priority",type=integer,JSONPath=".spec.priority"
// +kubebuilder:printcolumn:name="Matched",type=integer,JSONPath=".status.matchedClaims"
// +kubebuilder:printcolumn:name="Applied",type=integer,JSONPath=".status.appliedClaims"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type QuotaPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the desired quota policy.
	// +optional
	Spec QuotaPolicySpec `json:"spec,omitempty"`

	// status is the most recently observed policy state, maintained by the
	// controller. Never set this directly.
	// +optional
	Status QuotaPolicyStatus `json:"status,omitempty"`
}

// QuotaPolicyList is a list of QuotaPolicy.
//
// +kubebuilder:object:root=true
type QuotaPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []QuotaPolicy `json:"items"`
}
