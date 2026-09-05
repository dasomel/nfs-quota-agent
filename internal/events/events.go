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
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
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
	// Forget drops every dedup-window entry recorded for pvName, across all
	// reasons. Callers should invoke this exactly where a PV's other
	// per-path caches (e.g. internal/agent's appliedQuotas) are dropped for
	// the same PV -- otherwise r.last (see recorder.Event's doc comment)
	// accumulates one entry per (ever-seen PV, reason) pair for the life of
	// the process, including PVs that were deleted long ago.
	Forget(pvName string)
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
func (noopRecorder) Forget(string)                                                      {}
func (noopRecorder) Shutdown()                                                          {}

// recorder is Recorder's real implementation, backed directly by
// events.EventBroadcaster (events.k8s.io/v1 -- ADR-0002 option D) rather
// than events.EventBroadcasterAdapter: the adapter exists to bridge old
// (tools/record, core/v1) and new (tools/events, events.k8s.io/v1) callers
// during a migration, which this package has no need for -- it never had a
// core/v1 caller to preserve, and the adapter type itself is deprecated
// ("This interface will be removed once migration is completed").
// Using EventBroadcaster's own NewBroadcaster/EventSinkImpl directly is
// both the non-deprecated path and the more honest one: it only ever
// speaks events.k8s.io/v1.
type recorder struct {
	broadcaster events.EventBroadcaster
	inner       events.EventRecorderLogger
	window      time.Duration
	cancel      context.CancelFunc

	mu sync.Mutex
	// last is keyed by pv.Name + "/" + string(reason), one entry per pair
	// (not per message) so it stays bounded regardless of how many distinct
	// messages a (pv, reason) pair ever produces -- see Event's doc comment
	// for why the message is part of what gets compared, not part of the
	// key itself.
	last map[string]dedupEntry
}

// dedupEntry is recorder.last's value: the most recently emitted message
// for a (pv, reason) pair and when it was emitted. Comparing both fields
// lets Event distinguish "the same outcome repeating" (suppress) from "the
// same reason firing again with a materially different message, e.g. a
// resize changing the size/limit named in the text" (must not be
// suppressed) within one dedup window.
type dedupEntry struct {
	message string
	at      time.Time
}

// NewRecorder starts an events.k8s.io/v1 EventBroadcaster backed by client
// and returns a Recorder deduplicating repeat (pv, reason) pairs within
// window. window MUST exceed the agent's periodic sync tick period
// (--sync-interval), not merely equal it: the periodic path calls Event
// once per PV per sync tick, so if window == syncInterval, Event.Event's
// `now.Sub(last) < r.window` check compares a delta that is always
// slightly >= one tick period against a window of exactly one tick period
// -- it is essentially never strictly less, so dedup never actually
// suppresses anything on the periodic path and only ever helps the
// separate, faster watch-triggered retry-queue path. The caller
// (cmd/nfs-quota-agent/main.go) passes 2*syncInterval precisely for this
// headroom -- ADR-0002 says "reuse syncInterval" for the dedup
// requirement's intent (bound repeats to roughly one sync cycle), not
// "pass syncInterval's exact value here."
// The returned Recorder owns background goroutines (via
// StartRecordingToSinkWithContext) until Shutdown is called.
func NewRecorder(client kubernetes.Interface, window time.Duration) Recorder {
	ctx, cancel := context.WithCancel(context.Background())
	broadcaster := events.NewBroadcaster(&events.EventSinkImpl{Interface: client.EventsV1()})
	// StartRecordingToSinkWithContext's only failure mode is its internal
	// watch.Broadcaster.Watch() call, which only errors after Shutdown has
	// already been called on it -- unreachable here since this broadcaster
	// was just constructed above and nothing can have shut it down yet.
	_ = broadcaster.StartRecordingToSinkWithContext(ctx)
	return &recorder{
		broadcaster: broadcaster,
		inner:       broadcaster.NewRecorder(clientgoscheme.Scheme, ReportingController),
		window:      window,
		cancel:      cancel,
		last:        make(map[string]dedupEntry),
	}
}

// Event emits eventType/reason regarding pv, unless the identical (pv.Name,
// reason, message) was already emitted within r.window. This dedup sits on
// top of, not instead of, EventBroadcaster's own client-side aggregation
// (identical (regarding, reason) events collapse into one Event object's
// growing series/count): the broadcaster's aggregation bounds *repeated
// identical* Event objects, but does nothing to bound how often this
// process calls Eventf in the first place for a PV stuck flapping a
// condition every reconcile -- see
// docs/adr/0002-kubernetes-events-and-retry-metrics.md's "Cardinality /
// rate limiting" discussion of option D. r.last would otherwise be
// unbounded by PV count for the lifetime of the process, growing by one
// map entry per (ever-seen PV, reason-ever-fired) pair and never shrinking
// on its own even after a PV is deleted -- see Forget, which callers use to
// evict a deleted PV's entries the same way internal/agent's
// forgetAppliedQuotaForPV drops appliedQuotas for it.
//
// The message is part of what's compared (not just part of the key) so
// that a changed message inside an otherwise-open window still gets
// through: a PV resized 1Gi->2Gi that re-applies while the previous
// QuotaApplied for it is still within the window must not have its second,
// materially different event silently swallowed just because the reason
// didn't change. r.last still holds at most one entry per (pv.Name,
// reason) -- a new message for the same pair replaces the previous entry
// rather than adding one, so this stays as bounded as the old
// (pv.Name, reason)-only key was.
func (r *recorder) Event(pv *v1.PersistentVolume, eventType string, reason Reason, messageFmt string, args ...interface{}) {
	if pv == nil {
		return
	}
	key := pv.Name + "/" + string(reason)
	message := fmt.Sprintf(messageFmt, args...)
	now := time.Now()

	r.mu.Lock()
	if prev, ok := r.last[key]; ok && prev.message == message && now.Sub(prev.at) < r.window {
		r.mu.Unlock()
		return
	}
	r.last[key] = dedupEntry{message: message, at: now}
	r.mu.Unlock()

	// action mirrors reason: this agent has no finer-grained "what action
	// was taken" vocabulary than the outcome itself, which is the same
	// choice many simple EventRecorder callers in client-go's own tree
	// make when they have no separate action taxonomy. message is already
	// fully formatted above, so it's passed through Eventf as a literal
	// (via "%s") rather than re-formatted a second time.
	r.inner.Eventf(pv, nil, eventType, string(reason), string(reason), "%s", message)
}

// Forget drops every r.last entry recorded for pvName (one per reason that
// has ever fired for it), so a PV that is later re-created with the same
// name starts with a clean dedup window instead of inheriting timestamps
// from before it was deleted.
func (r *recorder) Forget(pvName string) {
	prefix := pvName + "/"
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range r.last {
		if strings.HasPrefix(key, prefix) {
			delete(r.last, key)
		}
	}
}

// Shutdown stops the broadcaster's background goroutine. Called once, from
// the same place the agent's context is torn down (main.go), mirroring
// pvReconcileQueue.shutdown's pattern in internal/agent.
func (r *recorder) Shutdown() {
	// Order matters: broadcaster.Shutdown() first, so it can flush any
	// already-queued Events through the sink while the
	// StartRecordingToSinkWithContext goroutine (driven by r.cancel's
	// context) is still running to deliver them. Canceling first would tear
	// down that goroutine before the flush had anywhere to go, silently
	// dropping whatever was still queued.
	//
	// broadcaster.Shutdown() is documented safe to call more than once, and
	// context.CancelFunc is safe to call more than once (a no-op after the
	// first call), unlike closing a channel -- so unlike the old stopCh
	// design this needs no separate already-canceled guard for a Shutdown
	// called twice.
	r.broadcaster.Shutdown()
	r.cancel()
}
