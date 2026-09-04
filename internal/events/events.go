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

// Package events emits events.k8s.io/v1 Kubernetes Events (via
// k8s.io/client-go/tools/events) about per-PV quota outcomes, per
// docs/adr/0002-kubernetes-events-and-retry-metrics.md (option D). It is a
// fourth, cluster-visible channel layered on top of the existing
// slog/audit-log/Prometheus-metrics reporting -- ADR-0001's status
// annotation, audit log, and structured logs remain the contract; this
// package is additive and, when disabled (the default), emits nothing and
// requires no RBAC grant at all.
package events

import (
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/events"
)

// ReportingController identifies this agent as the source of every Event it
// emits (events.k8s.io/v1's reportingController field, and the
// reportingInstance prefix client-go derives from it).
const ReportingController = "nfs-quota-agent"

// TypeNormal/TypeWarning mirror corev1.EventTypeNormal/EventTypeWarning
// (the only two eventType values Eventf accepts) so call sites in
// internal/agent don't need their own k8s.io/api/core/v1 import just for
// these two string constants.
const (
	TypeNormal  = v1.EventTypeNormal
	TypeWarning = v1.EventTypeWarning
)

// Reason enumerates the fixed, bounded set of Event reasons this agent may
// emit. Kept as a closed Go type (not a raw string built from an error
// message) precisely because the ADR's threat model treats an unbounded
// reason as the same kind of cardinality risk a raw error string would be
// for the retry metrics -- see docs/adr/0002-kubernetes-events-and-retry-metrics.md
// "Threat / abuse model". Only reasons with an obvious existing call site in
// internal/agent are actually wired; the others are declared here for the
// vocabulary the ADR's Scope names, matching how internal/apis/quota/v1alpha1's
// Reason* constants document some outcomes that may never fire in practice.
type Reason string

const (
	// QuotaApplied: a quota was newly applied or updated for a PV and its
	// read-back verification (if any) matched. Normal.
	QuotaApplied Reason = "QuotaApplied"
	// QuotaApplyFailed: the filesystem apply command itself failed (before
	// read-back verification ran). Warning.
	QuotaApplyFailed Reason = "QuotaApplyFailed"
	// QuotaVerificationFailed: the apply command succeeded but the
	// post-apply read-back (verifyQuotaOnDisk) found the on-disk state
	// didn't match what was requested (#10). Warning.
	QuotaVerificationFailed Reason = "QuotaVerificationFailed"
	// QuotaExceeded: current usage is at or above the enforced limit.
	// Warning.
	QuotaExceeded Reason = "QuotaExceeded"
	// QuotaNearLimit: current usage is at or above 90% of the enforced
	// limit but has not reached it. Normal.
	QuotaNearLimit Reason = "QuotaNearLimit"
	// PolicyClamped: a QuotaPolicy (quota.nfs.io/v1alpha1) clamped the
	// effective quota down to its maxQuota (quotapolicy.BoundClampedToMax).
	// Normal.
	PolicyClamped Reason = "PolicyClamped"
	// PolicyRejected: a claim a QuotaPolicy won was rejected at enforcement
	// time (the shrink guard or a StorageClass binding path fallback).
	// Warning.
	PolicyRejected Reason = "PolicyRejected"
	// QuotaDrifted: the independent read-back drift check (#13's Drifted
	// condition) found the on-disk enforced quota no longer matches what
	// this agent believes it applied. Warning.
	QuotaDrifted Reason = "QuotaDrifted"
)

// Recorder emits a bounded, deduplicated stream of Kubernetes Events about
// PV quota outcomes. Every method is safe to call on a nil *recorder
// obtained from a disabled configuration only through NewNoop, never
// through a nil Recorder interface value -- callers should always hold a
// non-nil Recorder (NewNoop when the feature is off), not a nil interface,
// so every call site can use it unconditionally.
type Recorder interface {
	// Event emits eventType/reason regarding pv, formatting messageFmt with
	// args the same way fmt.Sprintf does, unless an Event for the same
	// (pv.Name, reason) pair was already emitted within the configured
	// dedup window -- see recorder.Event's doc comment for why this exists
	// on top of EventBroadcaster's own client-side aggregation.
	Event(pv *v1.PersistentVolume, eventType string, reason Reason, messageFmt string, args ...interface{})
	// Shutdown stops the underlying broadcaster, if any. Safe to call more
	// than once and on a no-op recorder.
	Shutdown()
}

// noopRecorder is Recorder's disabled implementation: --enable-events=false
// (the default) wires this in, so every Event call in internal/agent is
// unconditional and no EventBroadcaster goroutine, API client call, or RBAC
// grant is ever needed.
type noopRecorder struct{}

// NewNoop returns a Recorder that discards every Event. Used whenever the
// events feature is disabled -- see cmd/nfs-quota-agent/main.go's
// --enable-events flag and the chart's events.enabled value, which also
// gates the events.k8s.io RBAC rule this Recorder would otherwise need.
func NewNoop() Recorder { return noopRecorder{} }

func (noopRecorder) Event(*v1.PersistentVolume, string, Reason, string, ...interface{}) {}
func (noopRecorder) Shutdown()                                                          {}

// recorder is Recorder's real implementation, backed by
// events.EventBroadcasterAdapter (events.k8s.io/v1 -- ADR-0002 option D).
type recorder struct {
	broadcaster events.EventBroadcasterAdapter
	inner       events.EventRecorderLogger
	window      time.Duration
	stopCh      chan struct{}

	mu   sync.Mutex
	last map[string]time.Time // key: pv.Name + "/" + string(reason)
}

// NewRecorder starts an events.k8s.io/v1 EventBroadcaster backed by client
// and returns a Recorder deduplicating repeat (pv, reason) pairs within
// window. window is expected to be the agent's --sync-interval (ADR-0002:
// "reuse syncInterval") -- not hardcoded here, so a caller that changes its
// sync cadence doesn't also have to remember to update this separately.
// The returned Recorder owns a background goroutine (via
// StartRecordingToSink) until Shutdown is called.
func NewRecorder(client kubernetes.Interface, window time.Duration) Recorder {
	stopCh := make(chan struct{})
	broadcaster := events.NewEventBroadcasterAdapter(client)
	broadcaster.StartRecordingToSink(stopCh)
	return &recorder{
		broadcaster: broadcaster,
		inner:       broadcaster.NewRecorder(ReportingController),
		window:      window,
		stopCh:      stopCh,
		last:        make(map[string]time.Time),
	}
}

// Event emits eventType/reason regarding pv, unless the same (pv.Name,
// reason) pair was already emitted within r.window. This dedup sits on top
// of, not instead of, EventBroadcaster's own client-side aggregation
// (identical (regarding, reason) events collapse into one Event object's
// growing series/count): the broadcaster's aggregation bounds *repeated
// identical* Event objects, but does nothing to bound how often this
// process calls Eventf in the first place for a PV stuck flapping a
// condition every reconcile -- see
// docs/adr/0002-kubernetes-events-and-retry-metrics.md's "Cardinality /
// rate limiting" discussion of option D. r.last is unbounded by PV count
// for the lifetime of the process; that is an accepted, bounded-in-practice
// tradeoff (one map entry per (live PV, reason-ever-fired) pair, not per
// occurrence) rather than a cache needing its own eviction, matching how
// appliedQuotas and the other per-path caches in internal/agent are never
// pruned except by pruneExcept's explicit sync against live PVs -- a future
// caller wanting that same pruning for r.last would wire it the same way.
func (r *recorder) Event(pv *v1.PersistentVolume, eventType string, reason Reason, messageFmt string, args ...interface{}) {
	if pv == nil {
		return
	}
	key := pv.Name + "/" + string(reason)
	now := time.Now()

	r.mu.Lock()
	if last, ok := r.last[key]; ok && now.Sub(last) < r.window {
		r.mu.Unlock()
		return
	}
	r.last[key] = now
	r.mu.Unlock()

	// action mirrors reason: this agent has no finer-grained "what action
	// was taken" vocabulary than the outcome itself, which is the same
	// choice many simple EventRecorder callers in client-go's own tree
	// make when they have no separate action taxonomy.
	r.inner.Eventf(pv, nil, eventType, string(reason), string(reason), messageFmt, args...)
}

// Shutdown stops the broadcaster's background goroutine. Called once, from
// the same place the agent's context is torn down (main.go), mirroring
// pvReconcileQueue.shutdown's pattern in internal/agent.
func (r *recorder) Shutdown() {
	select {
	case <-r.stopCh:
		// Already closed; avoid a double-close panic if Shutdown is ever
		// called twice (defensive -- current callers call it exactly once).
	default:
		close(r.stopCh)
	}
	r.broadcaster.Shutdown()
}
