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

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/workqueue"
)

// defaultMaxRetryDelay caps how long a failed reconcile's exponential
// backoff can grow to before AddRateLimited hands it back to a worker.
// workqueue.DefaultTypedControllerRateLimiter's own default (1000s) lets a
// stuck retry sit on a resolved-stale item.pv/effectiveBytes for far longer
// than syncInterval's periodic full resync (30s default) needs to have
// already superseded it with a fresh List -- capping the retry ceiling
// near that scale means a retry can never carry a state staler than the
// backstop resync would already have corrected. This is a fixed constant,
// not threaded from the configurable --sync-interval flag: it only needs
// to be roughly the same order of magnitude, not exact.
const defaultMaxRetryDelay = 30 * time.Second

// pvReconcileQueue decouples the watch event stream (watch.go's eventLoop)
// from ensureQuota's filesystem work, and coalesces duplicate events for
// the same PV: workqueue.Add() on a key already queued or currently being
// processed is a no-op until that item's Done() call, at which point --
// only if it was Add()-ed again while in flight -- it is reprocessed
// exactly once more, not once per event received in between. AddRateLimited
// on failure gives ensureQuota errors their own short exponential backoff
// (5ms up to defaultMaxRetryDelay) instead of waiting for the next
// syncInterval (up to 30s) full resync to retry -- a gap #12's own tracking
// comments flagged.
//
// This exists for pipeline decoupling and observability (queue depth,
// latency, error counts, faster retry), NOT to protect against two
// ensureQuota calls racing each other: ensureQuota holds a.mu for its
// entire body, and the DaemonSet model (#23) means exactly one agent
// instance runs per node, so nothing here races another writer over a.mu --
// see syncAllQuotas's own doc comment (agent.go) making the identical point
// about QuotaPolicy resolution. Multiple workers therefore don't
// parallelize the actual filesystem mutation (a.mu still serializes it);
// what they buy is that a slow xfs_quota/exec call no longer blocks the
// watch event loop's resourceVersion tracking, Bookmark keepalive, or
// ctx.Done() responsiveness.
//
// Serialization is NOT the same as ordering, though, and that distinction
// is exactly why Deleted goes through this same queue (enqueueDelete)
// rather than being handled inline by watch.go's eventLoop the way it was
// before this queue existed: with a synchronous eventLoop, Added/Modified
// and Deleted for one PV could never interleave. Once ensureQuota moved
// onto worker goroutines, a Deleted arriving while a worker is still
// mid-flight on an older Added/Modified for the same key could otherwise
// race that worker's own appliedQuotas write and lose -- see enqueueDelete
// and process's tombstone branch for how routing both through the same
// per-key queue avoids that (found in review; see git history for the
// prior, racy design where Deleted mutated appliedQuotas directly from
// watch.go).
type pvReconcileQueue struct {
	queue      workqueue.TypedRateLimitingInterface[string]
	agent      *QuotaAgent
	numWorkers int
	wg         sync.WaitGroup

	// latest holds the most recently observed state per key (PV name): the
	// PV + resolved effective quota size for a live reconcile, or a
	// tombstone for a pending deletion (see reconcileItem.deleted) -- so a
	// worker always reconciles the newest known state rather than a
	// specific event that may have been superseded while it sat in the
	// queue. Only watch.go's single-goroutine eventLoop writes here (via
	// enqueue/enqueueDelete); workers only ever read or, once per full sync
	// cycle, compare-and-delete via pruneExcept. No worker/process path may
	// unconditionally delete an entry: a worker that just Get()'d key K has
	// no way to distinguish "the latest entry for K is the one I'm about to
	// process" from "enqueue()/enqueueDelete() already overwrote it with a
	// newer event for the same key while I was mid-flight" -- deleting
	// unconditionally there could erase a fresher update a concurrent
	// eventLoop call just stored, silently dropping a reconciliation. That
	// is why pruneExcept uses CompareAndDelete (only removes an entry if it
	// is still exactly the value this call observed) instead of Delete.
	latest sync.Map // key: PV name (string) -> *reconcileItem

	// inFlight counts items a worker has Get()'d but not yet Done()'d, so
	// depth() reflects "queued or in flight" as its doc comment claims --
	// workqueue.Len() alone only counts items still waiting (see depth's
	// doc comment).
	inFlight atomic.Int32
}

type reconcileItem struct {
	pv             *v1.PersistentVolume
	effectiveBytes int64
	policyAttempt  *policyAttempt
	// pendingPolicySnapshot prevents a StorageClass PV from using raw
	// capacity while policy selection inputs have not completed one listing.
	// It is resolved by the worker on each rate-limited retry.
	pendingPolicySnapshot bool
	// deleted marks this entry as a tombstone: pv no longer exists and
	// process should forget its applied-quota cache entry rather than call
	// ensureQuota. See enqueueDelete.
	deleted bool
}

// newPVReconcileQueue constructs a queue bound to agent a, with numWorkers
// worker goroutines started by start(). numWorkers <= 0 is the caller's
// bug, not this function's to silently paper over -- watchBackoffConfig.
// withDefaults() is the single place that applies the production default.
func newPVReconcileQueue(a *QuotaAgent, numWorkers int) *pvReconcileQueue {
	limiter := workqueue.NewTypedMaxOfRateLimiter(
		workqueue.NewTypedItemExponentialFailureRateLimiter[string](5*time.Millisecond, defaultMaxRetryDelay),
		// Overall (not per-item) token-bucket factor, matching
		// DefaultTypedControllerRateLimiter's own -- only the per-item
		// exponential ceiling above is what this constructor customizes.
		&workqueue.TypedBucketRateLimiter[string]{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
	)
	return &pvReconcileQueue{
		queue:      workqueue.NewTypedRateLimitingQueue(limiter),
		agent:      a,
		numWorkers: numWorkers,
	}
}

// enqueue records pv as the latest known state for its key and schedules
// reconciliation. Only ever called from watch.go's eventLoop, which is a
// single goroutine per watch connection (and connections are sequential,
// never concurrent), so the Store-then-Add pair here needs no additional
// locking beyond what sync.Map already provides against concurrent worker
// reads. Optional pa carries QuotaPolicy provenance (#14) derived at
// event-enqueue time.
func (q *pvReconcileQueue) enqueue(pv *v1.PersistentVolume, effectiveBytes int64, pa ...*policyAttempt) {
	var attempt *policyAttempt
	if len(pa) > 0 {
		attempt = pa[0]
	}
	q.latest.Store(pv.Name, &reconcileItem{pv: pv, effectiveBytes: effectiveBytes, policyAttempt: attempt})
	q.queue.Add(pv.Name)
}

// enqueuePendingPolicySnapshot defers a StorageClass PV until the first
// QuotaPolicy snapshot has been published. It deliberately does not affect
// StorageClass-less PVs, whose existing raw-capacity behavior remains valid.
func (q *pvReconcileQueue) enqueuePendingPolicySnapshot(pv *v1.PersistentVolume) {
	q.latest.Store(pv.Name, &reconcileItem{pv: pv, pendingPolicySnapshot: true})
	q.queue.Add(pv.Name)
}

// enqueueDelete records pv's key as deleted and schedules it for
// processing through the SAME per-key queue Added/Modified use, rather
// than mutating agent state directly from watch.go's eventLoop the way an
// earlier version of this code did. Routing it through the queue matters:
// workqueue tracks each key's dirty/processing state, so if a worker is
// still mid-flight on an older Added/Modified for this key when this call
// happens, the key is marked dirty and guaranteed to be redelivered to a
// worker -- carrying this tombstone -- immediately after that in-flight
// call's Done(). That ordering guarantee is what process's tombstone
// branch relies on to always run after, not possibly before or racing,
// any reconcile it is meant to undo.
func (q *pvReconcileQueue) enqueueDelete(pv *v1.PersistentVolume) {
	q.latest.Store(pv.Name, &reconcileItem{pv: pv, deleted: true})
	q.queue.Add(pv.Name)
}

// pruneExcept removes cached latest state for any key not present in live
// (PV names still known to exist, as of syncAllQuotas's most recent full
// List), bounding latest's growth for long-running processes with PV
// churn that happened while the watch was disconnected -- the same gap
// pruneAppliedQuotas (agent.go) exists to close for appliedQuotas itself.
// Uses CompareAndDelete rather than Delete: a key can legitimately be
// absent from live (a fresh PV created after this cycle's List ran) while
// still having a just-Store()'d entry a concurrent enqueue/enqueueDelete
// call put there after this prune started iterating -- CompareAndDelete
// only removes the entry if it is still exactly the value this call
// observed, so a racing fresher Store is never lost.
func (q *pvReconcileQueue) pruneExcept(live map[string]struct{}) {
	q.latest.Range(func(k, v any) bool {
		name, _ := k.(string)
		if _, ok := live[name]; !ok {
			q.latest.CompareAndDelete(k, v)
		}
		return true
	})
}

// start launches the worker pool. Workers run until the queue is shut down
// (see shutdown), which happens once, from watch.go's deferred cleanup when
// watchPVsWithBackoff's ctx is done.
func (q *pvReconcileQueue) start(ctx context.Context) {
	for i := 0; i < q.numWorkers; i++ {
		q.wg.Add(1)
		go q.worker(ctx)
	}
}

func (q *pvReconcileQueue) worker(ctx context.Context) {
	defer q.wg.Done()
	for {
		key, shutdown := q.queue.Get()
		if shutdown {
			return
		}
		q.inFlight.Add(1)
		q.process(ctx, key)
		q.inFlight.Add(-1)
	}
}

// process reconciles one PV key against its latest cached state. ctx is the
// same (possibly already-canceled, during shutdown drain) context
// watchPVsWithBackoff was given: that's deliberate, not an oversight --
// ensureQuota's actual filesystem quota mutation goes through
// quota.CommandRunner, which takes no context and always runs to
// completion regardless of ctx's state. Only the trailing Kubernetes
// status-annotation write (updateQuotaStatus) can be cut short by a
// canceled ctx, which is the same already-tolerated, logged-not-fatal
// failure mode that call has always had (a transient API error there
// leaves the quota applied but the annotation stale until the next
// successful write).
//
// A drainTimeout shutdown that gives up while a worker is still here
// abandons whatever retry AddRateLimited would otherwise have scheduled --
// the periodic full resync (syncAllQuotas) is the backstop for that, same
// as any other reconcile this queue never got to.
func (q *pvReconcileQueue) process(ctx context.Context, key string) {
	defer q.queue.Done(key)

	v, ok := q.latest.Load(key)
	if !ok {
		// Nothing cached for this key -- can't happen via enqueue/
		// enqueueDelete (both Store before Add), but guard rather than
		// panic on a nil type assertion if it ever does.
		q.queue.Forget(key)
		return
	}
	item, _ := v.(*reconcileItem)

	if item.deleted {
		q.agent.forgetAppliedQuotaForPV(item.pv)
		slog.Debug("PV deleted, quota tracking removed", "pv", item.pv.Name)
		q.queue.Forget(key)
		return
	}

	if item.pendingPolicySnapshot {
		effectiveBytes, winner, decision, snapshotReady := q.agent.resolveFromSnapshot(item.pv)
		if !snapshotReady {
			q.queue.AddRateLimited(key)
			return
		}
		var pa *policyAttempt
		if winner != nil {
			pa = &policyAttempt{winner: winner, decision: decision}
		}
		item = &reconcileItem{pv: item.pv, effectiveBytes: effectiveBytes, policyAttempt: pa}
	}

	start := time.Now()
	// ensureQuotaWith (via ensureQuotaMutatedWith) always passes a nil
	// passUsageSnapshot to the shrink/brownfield guard -- unlike
	// syncAllQuotas' PV loop, which shares one snapshot across an entire
	// pass (#92), every watch-triggered reconcile here pays for its own
	// live usage-report read. That's deliberate: this queue processes one
	// PV at a time, arbitrarily spaced out by real Kubernetes events, so
	// there is no "whole pass" to amortize a report fetch across the way
	// syncAllQuotas' PV loop has. item.policyAttempt carries QuotaPolicy
	// provenance (#14) captured at resolve time.
	err := q.agent.ensureQuotaWith(ctx, item.pv, item.effectiveBytes, item.policyAttempt)

	switch {
	case errors.Is(err, ErrHAStandby):
		// Not fed into recordReconcileResult at all -- neither a success
		// (nothing was reconciled) nor an error (nothing went wrong) for
		// the nfs_quota_agent_reconcile_total/_errors_total metrics to
		// reflect. Not retried via AddRateLimited either: retrying while
		// still standby just churns the backoff for no benefit, and
		// runHAActivePolling's failover trigger (ha.go) already
		// re-reconciles every PV via syncAllQuotas the moment this node
		// actually becomes active.
		slog.Debug("Skipping reconcile: this instance is HA standby", "pv", item.pv.Name)
		q.queue.Forget(key)
	case err != nil:
		q.agent.recordReconcileResult(time.Since(start), err)
		slog.Error("Failed to ensure quota", "pv", item.pv.Name, "error", err)
		q.queue.AddRateLimited(key)
	default:
		q.agent.recordReconcileResult(time.Since(start), nil)
		q.queue.Forget(key)
	}
}

// depth returns the current number of keys queued or in flight, for the
// nfs_quota_agent_reconcile_queue_depth metric. It does not count items
// currently waiting out AddRateLimited's backoff delay before becoming
// ready again -- workqueue's TypedRateLimitingInterface exposes no way to
// read that count -- so a queue with only failed-and-backing-off items can
// still under-report relative to "everything not yet successfully
// processed". Queued-or-in-flight is still the signal that answers the
// original concern this metric exists for (a stuck worker reading as
// idle): see the in-flight test coverage in reconcile_queue_test.go.
func (q *pvReconcileQueue) depth() int {
	return q.queue.Len() + int(q.inFlight.Load())
}

// shutdown stops the queue from accepting further work and waits for
// already-queued and in-flight items to finish processing (see process's
// doc comment for why letting them finish, rather than abandoning them, is
// the point), up to drainTimeout. A timeout is logged, not treated as
// fatal: the periodic full resync (syncAllQuotas) remains the backstop for
// whatever didn't finish draining.
func (q *pvReconcileQueue) shutdown(drainTimeout time.Duration) {
	q.queue.ShutDown()

	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(drainTimeout):
		slog.Warn("Reconcile queue did not fully drain before shutdown timeout",
			"timeout", drainTimeout, "remainingDepth", q.depth())
	}
}
