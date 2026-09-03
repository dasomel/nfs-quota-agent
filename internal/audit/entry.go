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

package audit

import "time"

// Action represents the type of quota action
type Action string

const (
	ActionCreate   Action = "CREATE"
	ActionUpdate   Action = "UPDATE"
	ActionDelete   Action = "DELETE"
	ActionCleanup  Action = "CLEANUP"
	ActionAllocate Action = "ALLOCATE"
	// ActionVerifyFailed marks a distinct failure class from
	// ActionCreate/ActionUpdate's own Success=false: the quota apply
	// command itself exited 0, but reading the value back off the
	// filesystem afterward didn't match what was requested (#10).
	ActionVerifyFailed Action = "VERIFY_FAILED"
	// ActionBindingRejected marks a StorageClass-restricted policy rejection
	// caused by a path fallback (Fallback=true) (#14).
	ActionBindingRejected Action = "binding_rejected"
	// ActionDecisionUpdated marks an annotation-only QuotaPolicy decision refresh
	// on a cache hit where filesystem quota bytes are unchanged (#14).
	ActionDecisionUpdated Action = "decision_updated"
)

// Entry represents a single audit log entry
type Entry struct {
	Timestamp time.Time `json:"timestamp"`
	Action    Action    `json:"action"`
	// CorrelationID identifies one ensureQuota/ensureQuotaMutated reconcile
	// attempt for a PV (#14's admission<->enforcement correlation item):
	// every audit entry this same attempt produces (e.g. a CREATE/UPDATE
	// entry and, when the apply's read-back verification disagrees, its
	// paired VERIFY_FAILED entry) shares this value, and it is also emitted
	// on the structured slog lines for that same attempt -- so a log line
	// and an audit entry for the same attempt can be joined without relying
	// on timestamp proximity. Generated agent-side (crypto/rand), never
	// read from a Kubernetes object: an independent review of #14's design
	// flagged anything persisted on a tenant-editable object (a PV/PVC
	// annotation or label) as untrusted input the agent must not trust for
	// its own bookkeeping. Two different reconcile attempts for the same PV
	// (a retry, or the periodic resync re-touching it later) always get a
	// fresh ID -- this is per-attempt, not per-PV.
	CorrelationID string `json:"correlation_id,omitempty"`
	PVName        string `json:"pv_name,omitempty"`
	Namespace     string `json:"namespace,omitempty"`
	PVCName       string `json:"pvc_name,omitempty"`
	Path          string `json:"path"`
	ProjectID     uint32 `json:"project_id,omitempty"`
	ProjectName   string `json:"project_name,omitempty"`
	OldQuota      int64  `json:"old_quota_bytes,omitempty"`
	// NewQuota is the REQUESTED size in bytes (PV capacity, or the
	// QuotaPolicy-resolved effective size) -- kept exactly as before this
	// field existed, for backward compatibility with anything already
	// reading new_quota_bytes as "what was asked for". See EnforcedQuota
	// for what the filesystem actually enforces.
	NewQuota int64 `json:"new_quota_bytes,omitempty"`
	// EnforcedQuota is quota.ExpectedEnforcedBytes(FSType, requested size)
	// -- the value the apply INTENDED to enforce, the same one written to
	// the nfs.io/enforced-limit-bytes annotation on a successful CREATE/
	// UPDATE. XFS/ext4 floor to whole KB, so this can differ from NewQuota
	// whenever the requested size isn't already a 1024-byte multiple; for
	// btrfs it always equals NewQuota.
	//
	// On a failed CREATE/UPDATE (Success=false) this is omitted/zero:
	// nothing was enforced to record. On a VERIFY_FAILED entry, though, it
	// IS populated -- it is what the read-back verification DISAGREED
	// with, not proof the value is actually enforced on disk. Never treat
	// a non-zero EnforcedQuota as proof of enforcement on its own; check
	// Action and Success first (ActionVerifyFailed or Success=false means
	// the backend does not actually hold this value). An independent
	// review of #14's design flagged NewQuota alone as conflating
	// "requested" and "enforced", which made every audit entry for a
	// non-1024-multiple XFS/ext4 request look like it recorded the wrong
	// value.
	EnforcedQuota int64  `json:"enforced_quota_bytes,omitempty"`
	FSType        string `json:"fs_type,omitempty"`
	Success       bool   `json:"success"`
	Error         string `json:"error,omitempty"`
	NodeName      string `json:"node_name,omitempty"`
	AgentID       string `json:"agent_id,omitempty"`
	// Policy records which QuotaPolicy (if any) shaped this attempt's
	// requested size, and how -- see PolicyProvenance. nil whenever no
	// QuotaPolicy matched the claim (including every deployment with the
	// feature disabled), so a pre-QuotaPolicy audit consumer sees no shape
	// change.
	Policy *PolicyProvenance `json:"policy,omitempty"`
}

// PolicyProvenance identifies the QuotaPolicy that resolved this attempt's
// requested size and the outcome of that resolution (quotapolicy.
// BoundOutcome, e.g. "ClampedToMax", "RaisedToMin") -- #14's "policy
// provenance" acceptance item. UID and Generation are recorded (not just
// Name) because a policy can be deleted and recreated with the same name,
// or edited after the fact; an audit entry should point at the exact
// object version that made this decision, not just a name that might now
// refer to something else.
type PolicyProvenance struct {
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Generation int64  `json:"generation"`
	Outcome    string `json:"outcome"`
	// DecisionID is a deterministic short hash uniquely identifying this
	// enforcement decision per (PV, policy UID, policy generation, bound outcome,
	// effective bytes) -- #14's admission<->enforcement correlation item.
	// Additive; omitted when empty.
	DecisionID string `json:"decision_id,omitempty"`
}
