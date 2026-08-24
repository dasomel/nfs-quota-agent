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
	"log/slog"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

const (
	defaultMinBackoff = 1 * time.Second
	defaultMaxBackoff = 60 * time.Second
	// defaultMinHealthyDuration is how long a watch must stay up before a
	// disconnect is treated as "was working" rather than "never really
	// connected". See the comment in watchPVsWithBackoff for why this,
	// rather than "received at least one event", is the reset signal.
	defaultMinHealthyDuration = 30 * time.Second
	// defaultReconcileWorkers is the default bounded worker-pool size for
	// the reconcile queue (reconcile_queue.go). See pvReconcileQueue's doc
	// comment for why more than 1 worker doesn't parallelize the actual
	// quota mutation (a.mu still serializes it) -- this is sized for
	// pipeline decoupling, not throughput.
	defaultReconcileWorkers = 4
	// defaultDrainTimeout bounds how long shutdown waits for the reconcile
	// queue to finish already-queued/in-flight work before giving up and
	// logging a warning (see pvReconcileQueue.shutdown). Kept comfortably
	// under Kubernetes' terminationGracePeriodSeconds default (30s, and the
	// chart does not override it) with margin for the rest of Run()'s own
	// shutdown path: a drainTimeout at or above the grace period would make
	// SIGKILL race (and likely win over) the "did not fully drain" warning
	// this is meant to at least get logged before the process dies.
	defaultDrainTimeout = 10 * time.Second
)

// watchBackoffConfig holds the reconnect-backoff timings for watchPVs. Zero
// fields default to the package constants above (see withDefaults) — this
// gives production callers a zero-value config while letting tests inject
// short timings. It is a parameter rather than fields on QuotaAgent because
// QuotaAgent (agent.go) is off limits for this change; a defaulted-zero
// config parameter gets the same test seam without touching that struct.
type watchBackoffConfig struct {
	minBackoff         time.Duration
	maxBackoff         time.Duration
	minHealthyDuration time.Duration
	// reconcileWorkers sizes the reconcile queue's worker pool (see
	// reconcile_queue.go). Tests set this low/high to make queue behavior
	// deterministic to observe; production always takes the default.
	reconcileWorkers int
	// drainTimeout bounds how long shutdown waits for the reconcile queue
	// to drain (see pvReconcileQueue.shutdown). Tests set this short so a
	// deliberately-stuck-worker test doesn't wait out the real 30s default.
	drainTimeout time.Duration
}

func (c watchBackoffConfig) withDefaults() watchBackoffConfig {
	if c.minBackoff <= 0 {
		c.minBackoff = defaultMinBackoff
	}
	if c.maxBackoff <= 0 {
		c.maxBackoff = defaultMaxBackoff
	}
	if c.minHealthyDuration <= 0 {
		c.minHealthyDuration = defaultMinHealthyDuration
	}
	if c.reconcileWorkers <= 0 {
		c.reconcileWorkers = defaultReconcileWorkers
	}
	if c.drainTimeout <= 0 {
		c.drainTimeout = defaultDrainTimeout
	}
	return c
}

// watchPVs watches for PV changes with exponential backoff on reconnect.
func (a *QuotaAgent) watchPVs(ctx context.Context) {
	a.watchPVsWithBackoff(ctx, watchBackoffConfig{})
}

// watchPVsWithBackoff is watchPVs with injectable backoff timings, so tests
// can exercise backoff growth and reset without waiting out real,
// minute-scale delays.
//
// resourceVersion tracking: lastResourceVersion persists across reconnects
// within one call to this function (not just within one connection's inner
// eventLoop), so a dropped connection resumes the watch from where it left
// off — a bare Watch() with no ResourceVersion instead starts a brand-new
// watch from "now", silently skipping any Added/Modified/Deleted that
// happened during the gap. The periodic full resync (syncAllQuotas's
// ticker, independent of this loop — see its own doc comment) already
// covers that gap today, so resuming here is about tightening event-driven
// latency back to "as soon as reconnected", not a correctness requirement;
// full reconciliation is still the backstop, not this resume logic.
//
// When lastResourceVersion is empty (first call, or cleared after a Gone/
// Expired watch error below), a List precedes the Watch to establish a
// starting resourceVersion — the standard List-then-Watch pattern, closing
// the same gap for a first connection that resuming closes for a
// reconnect.
func (a *QuotaAgent) watchPVsWithBackoff(ctx context.Context, cfg watchBackoffConfig) {
	cfg = cfg.withDefaults()
	backoff := cfg.minBackoff
	var lastResourceVersion string

	// The reconcile queue is created once for the lifetime of this call,
	// not per connection: it decouples event ingestion from ensureQuota's
	// filesystem work (see reconcile_queue.go), and that decoupling should
	// survive a reconnect, not reset with it. Shutdown -- which drains
	// already-queued/in-flight work rather than abandoning it -- runs via
	// defer so every return path below (ctx.Done() at the top of the loop,
	// inside the eventLoop select, or a future added exit) goes through it
	// exactly once.
	rq := newPVReconcileQueue(a, cfg.reconcileWorkers)
	a.reconcileQueue.Store(rq)
	rq.start(ctx)
	defer func() {
		rq.shutdown(cfg.drainTimeout)
		// CompareAndSwap, not an unconditional Store(nil): watchPVsWithBackoff
		// only ever runs once in production, but an unconditional clear here
		// would let this call's cleanup blow away a DIFFERENT, still-live
		// queue if this function were ever called concurrently with itself
		// again (e.g. from a test sharing one *QuotaAgent). Only clear the
		// pointer if it still holds the queue this call created.
		a.reconcileQueue.CompareAndSwap(rq, nil)
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if lastResourceVersion == "" {
			list, err := a.client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
			if err != nil {
				slog.Error("Failed to list PVs before starting watch", "error", err, "retryIn", backoff)
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				backoff = min(backoff*2, cfg.maxBackoff)
				continue
			}
			lastResourceVersion = list.ResourceVersion
		}

		watcher, err := a.client.CoreV1().PersistentVolumes().Watch(ctx, metav1.ListOptions{
			ResourceVersion:     lastResourceVersion,
			AllowWatchBookmarks: true,
		})
		if err != nil {
			slog.Error("Failed to start PV watch", "error", err, "retryIn", backoff, "resourceVersion", lastResourceVersion)
			if apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
				// The resourceVersion itself was rejected before a stream
				// even opened (some API servers validate it synchronously
				// rather than only via a watch.Error event). Clearing it
				// forces a fresh List on the next iteration instead of
				// retrying the same now-invalid value forever.
				lastResourceVersion = ""
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, cfg.maxBackoff)
			continue
		}

		// Only trust a connection, and reset the backoff, once it has stayed
		// up for minHealthyDuration. The old behavior reset on Watch()
		// merely returning without error, which treats "connects, then dies
		// instantly" as success — turning a persistent mid-stream failure
		// (auth revoked, an API server that keeps rejecting the stream) into
		// an unthrottled reconnect once per minBackoff, forever.
		//
		// "Reset after at least one event" was considered and rejected: a
		// healthy watch against a quiescent cluster (no PV churn) can
		// legitimately see zero events for a long time, and that watch is
		// still healthy — it must not be penalized with growing backoff for
		// having nothing to report.
		connectedAt := time.Now()
		healthy := false
		healthyTimer := time.NewTimer(cfg.minHealthyDuration)

	eventLoop:
		for {
			select {
			case <-ctx.Done():
				healthyTimer.Stop()
				watcher.Stop()
				return
			case <-healthyTimer.C:
				healthy = true
				backoff = cfg.minBackoff
			case event, ok := <-watcher.ResultChan():
				if !ok {
					break eventLoop
				}

				if event.Type == watch.Error {
					// watch.Error events carry a *metav1.Status describing
					// why the stream is ending, not a PV — the type
					// assertion below would just drop it silently. Log it
					// so a persistently rejected stream is diagnosable
					// instead of showing up only as a 1/s "restarting" log.
					if status, ok := event.Object.(*metav1.Status); ok {
						slog.Error("PV watch received error event", "message", status.Message, "reason", status.Reason)
						if err := apierrors.FromObject(status); apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
							// The tracked resourceVersion fell out of the API
							// server's watch cache (etcd compaction, or the
							// connection was down longer than the server
							// retains history for). Clear it so the next
							// reconnect does a fresh List instead of
							// resuming from a position the server will keep
							// rejecting — and break out of this connection
							// now rather than continue: a server that emits
							// Gone/Expired without also closing the stream
							// would otherwise let the very next Bookmark or
							// PV event re-populate lastResourceVersion from
							// this same connection before the loop ever
							// gets back around to reconnecting, silently
							// undoing the clear above. The channel itself
							// hasn't closed (unlike the normal !ok exit
							// below), so explicitly Stop() the watcher
							// rather than leaking its underlying connection.
							lastResourceVersion = ""
							watcher.Stop()
							break eventLoop
						}
					} else {
						slog.Error("PV watch received error event", "object", event.Object)
					}
					continue
				}

				pv, ok := event.Object.(*v1.PersistentVolume)
				if !ok {
					continue
				}
				// Bookmark events carry a minimal object of the watched
				// type with only resourceVersion populated -- update the
				// resume position from every event uniformly rather than
				// special-casing Bookmark, then let the switch below
				// handle Added/Modified/Deleted; Bookmark itself needs no
				// further action.
				if pv.ResourceVersion != "" {
					lastResourceVersion = pv.ResourceVersion
				}

				switch event.Type {
				case watch.Added, watch.Modified:
					if a.shouldProcessPV(pv) {
						// Resolve QuotaPolicy against the last sync cycle's
						// cached policy set (see resolveFromSnapshot in
						// policy.go) rather than passing 0 unconditionally:
						// ensureQuota's own quota-status annotation write
						// generates a Modified event for the very PV it just
						// enforced a quota on, and blindly reapplying raw
						// capacity here would immediately undo a QuotaPolicy
						// clamp the last sync applied. Enqueueing (rather
						// than calling ensureQuota directly) hands the slow
						// filesystem work to the reconcile queue's workers so
						// this loop can keep draining watcher.ResultChan()
						// -- see reconcile_queue.go.
						effectiveBytes := a.resolveFromSnapshot(pv)
						rq.enqueue(pv, effectiveBytes)
					}
				case watch.Deleted:
					// Routed through the same per-key reconcile queue as
					// Added/Modified (enqueueDelete), not mutated here
					// directly: a worker can be mid-flight on an older
					// Added/Modified for this exact key when Deleted
					// arrives, and workqueue's per-key dirty/processing
					// tracking is what guarantees this tombstone is
					// processed strictly after that in-flight call
					// finishes -- see enqueueDelete's doc comment for why
					// deleting appliedQuotas straight from this goroutine
					// (the pre-review-fix design) could otherwise race a
					// worker's write and lose.
					rq.enqueueDelete(pv)
				}
			}
		}
		healthyTimer.Stop()

		if !healthy {
			backoff = min(backoff*2, cfg.maxBackoff)
		}

		slog.Warn("PV watch ended, restarting...", "retryIn", backoff, "connectedFor", time.Since(connectedAt))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}
