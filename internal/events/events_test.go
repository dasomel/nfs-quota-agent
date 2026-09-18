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

package events

import (
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func testPV(name string) *v1.PersistentVolume {
	return &v1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name)}}
}

func TestFake_DedupWithinWindow(t *testing.T) {
	f := NewFake(30 * time.Second)
	now := time.Unix(1000, 0)
	f.Now = func() time.Time { return now }

	pv := testPV("pv-a")
	f.Event(pv, TypeNormal, QuotaApplied, "applied %s", "10Gi")
	f.Event(pv, TypeNormal, QuotaApplied, "applied %s", "10Gi")

	if got := f.Count("pv-a", QuotaApplied); got != 1 {
		t.Fatalf("expected 1 event within dedup window, got %d", got)
	}
}

// TestFake_DedupBoundary pins the exact boundary condition
// dedupWindow.allow's `now.Sub(prev.at) < d.window` check implements, which
// is what makes window == syncInterval useless for the periodic sync path
// (see NewRecorder's doc comment): two Events window/2 apart must dedup to
// one, and two Events a full window apart must not dedup at all, because
// the check is a strict "<", not "<=".
func TestFake_DedupBoundary(t *testing.T) {
	const window = 30 * time.Second
	pv := testPV("pv-a")

	t.Run("window/2 apart dedups", func(t *testing.T) {
		f := NewFake(window)
		now := time.Unix(1000, 0)
		f.Now = func() time.Time { return now }
		f.Event(pv, TypeNormal, QuotaApplied, "applied")
		now = now.Add(window / 2)
		f.Event(pv, TypeNormal, QuotaApplied, "applied")

		if got := f.Count("pv-a", QuotaApplied); got != 1 {
			t.Fatalf("expected 1 event window/2 apart, got %d", got)
		}
	})

	t.Run("a full window apart does not dedup", func(t *testing.T) {
		f := NewFake(window)
		now := time.Unix(1000, 0)
		f.Now = func() time.Time { return now }
		f.Event(pv, TypeNormal, QuotaApplied, "applied")
		now = now.Add(window)
		f.Event(pv, TypeNormal, QuotaApplied, "applied")

		if got := f.Count("pv-a", QuotaApplied); got != 2 {
			t.Fatalf("expected 2 events a full window apart, got %d", got)
		}
	})
}

func TestFake_ReemitsAfterWindow(t *testing.T) {
	f := NewFake(30 * time.Second)
	now := time.Unix(1000, 0)
	f.Now = func() time.Time { return now }

	pv := testPV("pv-a")
	f.Event(pv, TypeNormal, QuotaApplied, "applied")
	now = now.Add(31 * time.Second)
	f.Event(pv, TypeNormal, QuotaApplied, "applied")

	if got := f.Count("pv-a", QuotaApplied); got != 2 {
		t.Fatalf("expected 2 events once the window elapsed, got %d", got)
	}
}

func TestFake_DifferentReasonsNotDeduped(t *testing.T) {
	f := NewFake(30 * time.Second)
	now := time.Unix(1000, 0)
	f.Now = func() time.Time { return now }

	pv := testPV("pv-a")
	f.Event(pv, TypeWarning, QuotaApplyFailed, "failed")
	f.Event(pv, TypeWarning, QuotaVerificationFailed, "verify failed")

	if len(f.Events) != 2 {
		t.Fatalf("expected 2 distinct-reason events, got %d", len(f.Events))
	}
}

func TestFake_DifferentPVsNotDeduped(t *testing.T) {
	f := NewFake(30 * time.Second)
	now := time.Unix(1000, 0)
	f.Now = func() time.Time { return now }

	f.Event(testPV("pv-a"), TypeNormal, QuotaApplied, "applied")
	f.Event(testPV("pv-b"), TypeNormal, QuotaApplied, "applied")

	if len(f.Events) != 2 {
		t.Fatalf("expected 2 distinct-PV events, got %d", len(f.Events))
	}
}

func TestFake_NilPVIsNoop(t *testing.T) {
	f := NewFake(30 * time.Second)
	f.Event(nil, TypeNormal, QuotaApplied, "applied")
	if len(f.Events) != 0 {
		t.Fatalf("expected nil PV to be a no-op, got %d events", len(f.Events))
	}
}

// TestFake_ForgetClearsDedupWindow guards Forget's contract: after
// forgetting a PV, a subsequent Event for the same (pv, reason) must not be
// deduped against the pre-Forget timestamp, even though it's well within
// what would otherwise be the dedup window.
func TestFake_ForgetClearsDedupWindow(t *testing.T) {
	f := NewFake(30 * time.Second)
	now := time.Unix(1000, 0)
	f.Now = func() time.Time { return now }

	pv := testPV("pv-a")
	f.Event(pv, TypeNormal, QuotaApplied, "applied")
	f.Forget("pv-a")
	// Still well inside the 30s window -- without Forget this would dedup.
	now = now.Add(1 * time.Second)
	f.Event(pv, TypeNormal, QuotaApplied, "applied")

	if got := f.Count("pv-a", QuotaApplied); got != 2 {
		t.Fatalf("expected 2 events after Forget clears the dedup window, got %d", got)
	}
}

// TestFake_ForgetOnlyAffectsNamedPV confirms Forget scopes to the given
// PV's own entries and does not disturb another PV's dedup state.
func TestFake_ForgetOnlyAffectsNamedPV(t *testing.T) {
	f := NewFake(30 * time.Second)
	now := time.Unix(1000, 0)
	f.Now = func() time.Time { return now }

	f.Event(testPV("pv-a"), TypeNormal, QuotaApplied, "applied")
	f.Event(testPV("pv-b"), TypeNormal, QuotaApplied, "applied")
	f.Forget("pv-a")

	// pv-a re-emits immediately (forgotten); pv-b stays deduped (untouched).
	f.Event(testPV("pv-a"), TypeNormal, QuotaApplied, "applied")
	f.Event(testPV("pv-b"), TypeNormal, QuotaApplied, "applied")

	if got := f.Count("pv-a", QuotaApplied); got != 2 {
		t.Fatalf("pv-a events = %d, want 2 (forgotten)", got)
	}
	if got := f.Count("pv-b", QuotaApplied); got != 1 {
		t.Fatalf("pv-b events = %d, want 1 (dedup window untouched)", got)
	}
}

// TestFake_ForgetClearsAllReasonsForOnePV guards dedupWindow.forget's
// contract now that entries are nested by reason under each PV: forgetting
// pvName must drop every reason recorded for it in one call (not just
// whichever reason a naive partial clear happened to hit first), while a
// different PV's entries -- including one sharing a reason with the
// forgotten PV -- must stay untouched.
func TestFake_ForgetClearsAllReasonsForOnePV(t *testing.T) {
	f := NewFake(30 * time.Second)
	now := time.Unix(1000, 0)
	f.Now = func() time.Time { return now }

	pvA, pvB := testPV("pv-a"), testPV("pv-b")
	f.Event(pvA, TypeNormal, QuotaApplied, "applied")
	f.Event(pvA, TypeWarning, QuotaExceeded, "exceeded")
	f.Event(pvB, TypeNormal, QuotaApplied, "applied")

	f.Forget("pv-a")

	// Still well inside the window -- pv-a re-emits on both reasons
	// (forgotten); pv-b stays deduped on the reason it shares with pv-a
	// (untouched).
	now = now.Add(1 * time.Second)
	f.Event(pvA, TypeNormal, QuotaApplied, "applied")
	f.Event(pvA, TypeWarning, QuotaExceeded, "exceeded")
	f.Event(pvB, TypeNormal, QuotaApplied, "applied")

	if got := f.Count("pv-a", QuotaApplied); got != 2 {
		t.Fatalf("pv-a QuotaApplied events = %d, want 2 (forgotten)", got)
	}
	if got := f.Count("pv-a", QuotaExceeded); got != 2 {
		t.Fatalf("pv-a QuotaExceeded events = %d, want 2 (forgotten)", got)
	}
	if got := f.Count("pv-b", QuotaApplied); got != 1 {
		t.Fatalf("pv-b QuotaApplied events = %d, want 1 (dedup window untouched)", got)
	}
}

// TestFake_SameMessageTwiceWithinWindowDedupes is the "same outcome
// repeating" half of the message-aware dedup contract: an identical
// (pv, reason, message) triple inside the window still collapses to one
// event, exactly as the old (pv, reason)-only key did.
func TestFake_SameMessageTwiceWithinWindowDedupes(t *testing.T) {
	f := NewFake(30 * time.Second)
	now := time.Unix(1000, 0)
	f.Now = func() time.Time { return now }

	pv := testPV("pv-a")
	f.Event(pv, TypeNormal, QuotaApplied, "Quota set to %s for PV %s", "1Gi", "pv-a")
	f.Event(pv, TypeNormal, QuotaApplied, "Quota set to %s for PV %s", "1Gi", "pv-a")

	if got := f.Count("pv-a", QuotaApplied); got != 1 {
		t.Fatalf("identical message twice within window: got %d events, want 1", got)
	}
}

// TestFake_DifferentMessageWithinWindowNotDeduped guards #160 review finding
// F2: a PV resized 1Gi->2Gi that re-applies inside the dedup window must
// still emit a second QuotaApplied, because the message (and therefore the
// resize) actually changed -- suppressing it made the resize invisible to
// anything watching Events, even though the reason (QuotaApplied) alone
// never changes.
func TestFake_DifferentMessageWithinWindowNotDeduped(t *testing.T) {
	f := NewFake(30 * time.Second)
	now := time.Unix(1000, 0)
	f.Now = func() time.Time { return now }

	pv := testPV("pv-a")
	f.Event(pv, TypeNormal, QuotaApplied, "Quota set to %s for PV %s", "1Gi", "pv-a")
	now = now.Add(1 * time.Second)
	f.Event(pv, TypeNormal, QuotaApplied, "Quota set to %s for PV %s", "2Gi", "pv-a")

	if got := f.Count("pv-a", QuotaApplied); got != 2 {
		t.Fatalf("changed message within window: got %d events, want 2 (resize must not be suppressed)", got)
	}
}

// TestFake_MessageDedupKeyStaysBoundedPerPVReason guards the "replace, not
// accumulate" half of F2's fix: a third, different message for the same
// (pv, reason) pair must still only ever keep dedupWindow.last's size to
// one entry for that pair -- exercised indirectly here by confirming a
// fourth call with the *first* message, still within the window, is
// treated as new (not deduped against the long-evicted first entry), which
// would only be possible if the map holds exactly one entry per (pv,
// reason), not one per (pv, reason, message) ever seen.
func TestFake_MessageDedupKeyStaysBoundedPerPVReason(t *testing.T) {
	f := NewFake(30 * time.Second)
	now := time.Unix(1000, 0)
	f.Now = func() time.Time { return now }

	pv := testPV("pv-a")
	f.Event(pv, TypeNormal, QuotaApplied, "Quota set to %s for PV %s", "1Gi", "pv-a")
	f.Event(pv, TypeNormal, QuotaApplied, "Quota set to %s for PV %s", "2Gi", "pv-a")
	f.Event(pv, TypeNormal, QuotaApplied, "Quota set to %s for PV %s", "1Gi", "pv-a")

	if got := f.Count("pv-a", QuotaApplied); got != 3 {
		t.Fatalf("three distinct-in-sequence messages within window: got %d events, want 3", got)
	}
	if got := len(f.dedup.last[pv.Name]); got != 1 {
		t.Fatalf("dedup has %d entries for one (pv, reason) pair, want 1 (replace, not accumulate)", got)
	}
}

// TestFake_ForgetClearsMessageDedupState confirms Forget still clears the
// message-aware dedup entry, not just a bare timestamp.
func TestFake_ForgetClearsMessageDedupState(t *testing.T) {
	f := NewFake(30 * time.Second)
	now := time.Unix(1000, 0)
	f.Now = func() time.Time { return now }

	pv := testPV("pv-a")
	f.Event(pv, TypeNormal, QuotaApplied, "Quota set to %s for PV %s", "1Gi", "pv-a")
	f.Forget("pv-a")
	now = now.Add(1 * time.Second)
	f.Event(pv, TypeNormal, QuotaApplied, "Quota set to %s for PV %s", "1Gi", "pv-a")

	if got := f.Count("pv-a", QuotaApplied); got != 2 {
		t.Fatalf("identical message after Forget: got %d events, want 2 (forget did not clear dedup state)", got)
	}
}

// TestNewRecorder_OnlyUsesEventsV1API covers #160 review finding F3: every
// prior test in this file exercises only Fake/Noop, so nothing actually
// proved the real recorder (backed by events.EventBroadcaster) confines
// itself to events.k8s.io/v1 -- exactly the resource/verb pair the chart's
// ClusterRole grants (see docs/adr/0002-kubernetes-events-and-retry-metrics.md).
// A regression that made it call the legacy core/v1 Events API instead
// (e.g. an accidental switch back to EventBroadcasterAdapter, see
// recorder's own doc comment on why that type is deliberately not used)
// would fail cluster-side with a forbidden error the unit tests could never
// catch on their own, since Fake never talks to a clientset at all.
func TestNewRecorder_OnlyUsesEventsV1API(t *testing.T) {
	client := fake.NewSimpleClientset()
	r := NewRecorder(client, 30*time.Second)
	// Shutdown stops the broadcaster's background goroutines once this test
	// is done asserting -- it is not itself the flush mechanism (Eventf's
	// delivery to the sink runs on its own goroutines with no synchronous
	// completion signal), so it's deferred rather than called before
	// polling: calling it immediately after Event races the shutdown
	// against the still-in-flight delivery and can drop the event before
	// it ever reaches the fake clientset.
	defer r.Shutdown()

	pv := testPV("pv-a")
	r.Event(pv, TypeNormal, QuotaApplied, "Quota set to %s for PV %s", "1Gi", pv.Name)

	// The actual API call happens asynchronously (recorderImpl.eventf's own
	// goroutine, then eventBroadcasterImpl.recordToSink's), so poll
	// Actions() with a deadline instead of sleeping a fixed, possibly-flaky
	// duration.
	deadline := time.Now().Add(5 * time.Second)
	var actions []k8stesting.Action
	for {
		actions = client.Actions()
		if len(actions) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if len(actions) == 0 {
		t.Fatalf("no client-go actions observed within the deadline; the recorder never reached the fake clientset")
	}

	for _, action := range actions {
		gvr := action.GetResource()
		if gvr.Group != "events.k8s.io" || gvr.Resource != "events" {
			t.Fatalf("action used group/resource %q/%q, want events.k8s.io/events: %#v", gvr.Group, gvr.Resource, action)
		}
		switch action.GetVerb() {
		case "create", "patch":
			// create for a new Event; patch only if a series/isomorphic
			// event forms, which a single Event call here shouldn't
			// trigger, but either is a legitimate events.k8s.io/v1 call.
		default:
			t.Fatalf("action used verb %q, want create or patch: %#v", action.GetVerb(), action)
		}
	}
}

// TestFake_SweepDropsExpiredEntries guards dedupWindow.sweep's contract: an
// entry whose age has reached the window can never suppress anything again
// (allow's rule is the strict "<" pinned by TestFake_DedupBoundary), so
// allow's lazy sweep is free to drop it from d.last once now reaches the
// next sweep due date -- and does.
func TestFake_SweepDropsExpiredEntries(t *testing.T) {
	const window = 30 * time.Second
	f := NewFake(window)
	t0 := time.Unix(1000, 0)
	now := t0
	f.Now = func() time.Time { return now }

	pvA, pvB, pvC := testPV("pv-a"), testPV("pv-b"), testPV("pv-c")
	f.Event(pvA, TypeNormal, QuotaApplied, "applied")
	f.Event(pvA, TypeWarning, QuotaExceeded, "exceeded")
	f.Event(pvB, TypeNormal, QuotaApplied, "applied")

	now = t0.Add(window)
	f.Event(pvC, TypeNormal, QuotaApplied, "applied")

	if got := len(f.dedup.last); got != 1 {
		t.Fatalf("dedup.last has %d PVs after sweep, want 1 (only pv-c)", got)
	}
	if _, ok := f.dedup.last["pv-c"]; !ok {
		t.Fatalf("dedup.last does not contain pv-c after sweep")
	}

	// pv-a's entries aged out and were swept, but that must be invisible to
	// callers: the same emit a full window later was already allowed under
	// the pre-sweep behavior too, since allow's check is a strict "<" (see
	// TestFake_DedupBoundary's "a full window apart" case) -- this proves
	// the sweep changed nothing observable.
	f.Event(pvA, TypeNormal, QuotaApplied, "applied")
	if got := f.Count("pv-a", QuotaApplied); got != 2 {
		t.Fatalf("pv-a QuotaApplied events = %d, want 2 (sweep must not change observable dedup behavior)", got)
	}
}

// TestFake_SweepRunsAtMostOncePerWindow guards allow's nextSweep throttle:
// two allow calls landing inside the same window (before nextSweep is
// reached) must not sweep at all, so an entry younger than the window --
// window/2 old here -- survives a later allow call within that same
// window, and nextSweep itself does not advance again until it actually
// elapses.
func TestFake_SweepRunsAtMostOncePerWindow(t *testing.T) {
	const window = 30 * time.Second
	f := NewFake(window)
	t0 := time.Unix(1000, 0)
	now := t0
	f.Now = func() time.Time { return now }

	f.Event(testPV("pv-a"), TypeNormal, QuotaApplied, "applied")
	nextSweep := f.dedup.nextSweep

	// A later allow call, still inside the same window: pv-a's entry is now
	// window/2 old, well short of what sweep needs to drop it.
	now = t0.Add(window / 2)
	f.Event(testPV("pv-b"), TypeNormal, QuotaApplied, "applied")

	if f.dedup.nextSweep != nextSweep {
		t.Fatalf("nextSweep advanced on an allow call still inside the window; sweep must run at most once per window")
	}
	if _, ok := f.dedup.last["pv-a"]; !ok {
		t.Fatalf("pv-a's entry (age window/2) was swept before its window elapsed")
	}
	if got := len(f.dedup.last); got != 2 {
		t.Fatalf("dedup.last has %d PVs, want 2 (pv-a survives, pv-b just added)", got)
	}
}

func TestNewNoop_NeverRecords(t *testing.T) {
	r := NewNoop()
	r.Event(testPV("pv-a"), TypeNormal, QuotaApplied, "applied")
	r.Shutdown()
	// No observable state to assert beyond "did not panic" -- NewNoop's
	// entire contract is that it discards everything.
}
