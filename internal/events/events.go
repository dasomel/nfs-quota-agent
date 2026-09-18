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
	// time by the StorageClass binding path fallback (the shrink guard
	// reports QuotaShrinkRejected instead). Warning.
	PolicyRejected Reason = "PolicyRejected"
	// QuotaShrinkRejected: the shrink guard refused to apply a quota below the
	// path's current (or unknown) usage (errUnsafeShrink), with or without a
	// QuotaPolicy involved. Emitted once per transition into rejection (the
	// guard's own #92 gate), since its message carries the live usage figure
	// and would otherwise slip past the message-compared dedup window on
	// every tick. Warning.
	QuotaShrinkRejected Reason = "QuotaShrinkRejected"
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
	// the same PV -- not because skipping it would leave those entries
	// there forever (dedupWindow's own expiry sweep bounds that on its
	// own; see dedupWindow's doc comment), but because without Forget a PV
	// deleted and recreated with the same name inside one window would
	// inherit suppression from its predecessor's still-live entries.
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
	cancel      context.CancelFunc

	// dedup bounds how often this recorder re-emits the same (pv, reason)
	// outcome; the same type backs Fake's dedup behavior (see dedupWindow's
	// doc comment) so the real and test implementations can't drift apart.
	dedup *dedupWindow
}

// dedupEntry is dedupWindow.last's innermost value: the most recently
// emitted message for a (pv, reason) pair and when it was emitted.
// Comparing both fields lets allow distinguish "the same outcome
// repeating" (suppress) from "the same reason firing again with a
// materially different message, e.g. a resize changing the size/limit
// named in the text" (must not be suppressed) within one dedup window.
type dedupEntry struct {
	message string
	at      time.Time
}

// dedupWindow bounds how often the same (pv, reason) outcome re-emits.
// Both recorder and Fake compose one instead of each keeping their own
// copy of this logic, so the fake's dedup behavior can never silently
// drift from the real recorder's -- see allow for the suppression rule
// itself.
//
// last stays bounded by (PVs that emitted within the last window) x
// (reasons that have fired for each): allow calls sweep at most once per
// window to drop every entry whose age has reached window, so a PV that
// stops emitting -- including one deleted while this process wasn't
// watching -- ages out on its own within one window instead of persisting
// for the life of the process. There is no periodic goroutine; sweeping
// only ever happens inline on the allow path. See forget for the one case
// sweep can't handle: a PV deleted and recreated with the same name inside
// a single window.
type dedupWindow struct {
	window time.Duration

	mu sync.Mutex
	// last is keyed by pv.Name, then by reason, one entry per pair (not per
	// message) so it stays bounded regardless of how many distinct messages
	// a (pv, reason) pair ever produces -- see allow's doc comment for why
	// the message is compared, not part of the key itself.
	last map[string]map[Reason]dedupEntry
	// nextSweep is the earliest time at which allow will next call sweep,
	// advanced to now.Add(window) each time sweep runs -- so the O(len(last))
	// scan happens at most once per window no matter how often allow itself
	// is called within it. Zero value is the zero Time, which is always in
	// the past, so the very first allow call sweeps unconditionally;
	// harmless, since last starts out empty.
	nextSweep time.Time
}

// newDedupWindow returns a dedupWindow suppressing repeat (pv, reason,
// message) triples within window.
func newDedupWindow(window time.Duration) *dedupWindow {
	return &dedupWindow{
		window: window,
		last:   make(map[string]map[Reason]dedupEntry),
	}
}

// allow reports whether (pvName, reason, message) should be emitted at
// now, recording it if so. Before the lookup, allow gives d.last a chance
// to shrink: at most once per window, it calls sweep to drop every entry
// that has aged out (see sweep and dedupWindow's doc comment for why this
// is enough to keep d.last bounded without a live-PV set or a periodic
// goroutine). See forget for the one case sweep can't handle: a PV deleted
// and recreated with the same name inside a single window.
//
// The message is part of what's compared (not just part of the key) so
// that a changed message inside an otherwise-open window still gets
// through: a PV resized 1Gi->2Gi that re-applies while the previous
// QuotaApplied for it is still within the window must not have its second,
// materially different event silently swallowed just because the reason
// didn't change. d.last still holds at most one entry per (pv, reason) --
// a new message for the same pair replaces the previous entry rather than
// adding one, so this stays as bounded as a flat (pv.Name, reason)-keyed
// map would be.
func (d *dedupWindow) allow(pvName string, reason Reason, message string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !now.Before(d.nextSweep) {
		d.sweep(now)
		d.nextSweep = now.Add(d.window)
	}

	byReason := d.last[pvName]
	if prev, ok := byReason[reason]; ok && prev.message == message && now.Sub(prev.at) < d.window {
		return false
	}
	if byReason == nil {
		byReason = make(map[Reason]dedupEntry)
		d.last[pvName] = byReason
	}
	byReason[reason] = dedupEntry{message: message, at: now}
	return true
}

// forget drops every entry recorded for pvName (one per reason that has
// ever fired for it), so a PV that is later re-created with the same name
// starts with a clean dedup window instead of inheriting timestamps from
// before it was deleted.
func (d *dedupWindow) forget(pvName string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.last, pvName)
}

// sweep deletes every entry whose age has reached d.window: allow's
// suppression check (now.Sub(prev.at) < d.window) can never match such an
// entry again, so dropping it is invisible to every caller. An inner
// per-reason map is deleted too once sweeping empties it, so a PV that
// stops emitting -- for any reason, including one deleted while this
// process wasn't watching -- ages out of d.last within one window on its
// own, with no live-PV set needed. Called from allow, at most once per
// window; caller holds d.mu.
func (d *dedupWindow) sweep(now time.Time) {
	for pvName, byReason := range d.last {
		for reason, entry := range byReason {
			if now.Sub(entry.at) >= d.window {
				delete(byReason, reason)
			}
		}
		if len(byReason) == 0 {
			delete(d.last, pvName)
		}
	}
}

// NewRecorder starts an events.k8s.io/v1 EventBroadcaster backed by client
// and returns a Recorder deduplicating repeat (pv, reason) pairs within
// window. window MUST exceed the agent's periodic sync tick period
// (--sync-interval), not merely equal it: the periodic path calls Event
// once per PV per sync tick, so if window == syncInterval, dedupWindow.allow's
// `now.Sub(prev.at) < d.window` check compares a delta that is always
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
		cancel:      cancel,
		dedup:       newDedupWindow(window),
	}
}

// Event emits eventType/reason regarding pv, unless the dedup window
// suppresses it (see dedupWindow.allow for the exact suppression rule).
// This dedup sits on top of, not instead of, EventBroadcaster's own
// client-side aggregation (identical (regarding, reason) events collapse
// into one Event object's growing series/count): the broadcaster's
// aggregation bounds *repeated identical* Event objects, but does nothing
// to bound how often this process calls Eventf in the first place for a PV
// stuck flapping a condition every reconcile -- see
// docs/adr/0002-kubernetes-events-and-retry-metrics.md's "Cardinality /
// rate limiting" discussion of option D. See Forget, which callers use to
// evict a deleted PV's entries the same way internal/agent's
// forgetAppliedQuotaForPV drops appliedQuotas for it.
func (r *recorder) Event(pv *v1.PersistentVolume, eventType string, reason Reason, messageFmt string, args ...interface{}) {
	if pv == nil {
		return
	}
	message := fmt.Sprintf(messageFmt, args...)
	if !r.dedup.allow(pv.Name, reason, message, time.Now()) {
		return
	}

	// action mirrors reason: this agent has no finer-grained "what action
	// was taken" vocabulary than the outcome itself, which is the same
	// choice many simple EventRecorder callers in client-go's own tree
	// make when they have no separate action taxonomy. message is already
	// fully formatted above, so it's passed through Eventf as a literal
	// (via "%s") rather than re-formatted a second time.
	r.inner.Eventf(pv, nil, eventType, string(reason), string(reason), "%s", message)
}

// Forget drops pvName's dedup-window entries; see dedupWindow.forget for
// why this still matters even though dedupWindow.sweep bounds d.last on
// its own (a PV recreated with the same name inside one window must not
// inherit suppression from its predecessor).
func (r *recorder) Forget(pvName string) {
	r.dedup.forget(pvName)
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
