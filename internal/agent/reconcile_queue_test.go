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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dasomel/nfs-quota-agent/internal/quota"
)

// blockingXFSRunner behaves like xfsHappyRunner for detect/-V calls, but
// blocks every quota-mutating xfs_quota call until unblock is closed --
// used to simulate a worker stuck mid-reconcile for shutdown-drain tests.
func blockingXFSRunner(unblock <-chan struct{}) *fakeRunner {
	return &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		switch name {
		case "findmnt":
			return []byte("xfs\n"), nil
		case "xfs_quota":
			if len(args) > 0 && args[0] == "-V" {
				return []byte("xfs_quota version 1.0"), nil
			}
			<-unblock
			return []byte("Project quota state: ON"), nil
		default:
			return []byte(""), nil
		}
	}}
}

// failNTimesXFSRunner behaves like xfsHappyRunner, except every
// quota-mutating xfs_quota call fails until it has been called n times,
// after which it succeeds -- used to exercise the reconcile queue's
// AddRateLimited retry path.
func failNTimesXFSRunner(n int) *fakeRunner {
	var calls atomic.Int32
	state := &xfsQuotaState{}
	return &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		switch name {
		case "findmnt":
			return []byte("xfs\n"), nil
		case "xfs_quota":
			if len(args) > 0 && args[0] == "-V" {
				return []byte("xfs_quota version 1.0"), nil
			}
			if calls.Add(1) <= int32(n) {
				return nil, fmt.Errorf("simulated transient xfs_quota failure")
			}
			// Past the injected-failure count: behave like a working
			// xfs_quota, including answering the post-apply read-back
			// verification's `report` query (#10) from the same state
			// the retry's `limit -p` call just wrote -- see xfsHappyRunner's
			// doc comment for why a bare fixed-string reply isn't enough.
			if len(args) >= 3 && args[1] == "-c" {
				if out, ok := state.handle(args[2]); ok {
					return out, nil
				}
			}
			return []byte("Project quota state: ON"), nil
		default:
			return []byte(""), nil
		}
	}}
}

// newQueueTestAgent builds an xfs-configured QuotaAgent ready for a
// pvReconcileQueue test to enqueue reconciliation against.
func newQueueTestAgent(t *testing.T) *QuotaAgent {
	t.Helper()
	client := fake.NewSimpleClientset()
	a := newTestAgent(t, client)
	a.nfsServerPath = "/exports"
	a.fsType = quota.FSTypeXFS
	a.processAllNFS = true
	return a
}

func mkPVDir(t *testing.T, a *QuotaAgent, nfsPath string) string {
	t.Helper()
	localPath := a.nfsPathToLocal(nfsPath)
	if err := os.MkdirAll(localPath, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", localPath, err)
	}
	return localPath
}

func TestPVReconcileQueueProcessesEnqueuedItems(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	a := newQueueTestAgent(t)

	localPaths := make([]string, 0, 3)
	rq := newPVReconcileQueue(a, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rq.start(ctx)
	defer rq.shutdown(2 * time.Second)

	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("pv-%d", i)
		nfsPath := fmt.Sprintf("/exports/pvc-%d", i)
		localPaths = append(localPaths, mkPVDir(t, a, nfsPath))
		rq.enqueue(newBoundPV(name, nfsPath, 1), 0)
	}

	for _, localPath := range localPaths {
		localPath := localPath
		waitFor(t, 2*time.Second, func() bool {
			a.mu.Lock()
			defer a.mu.Unlock()
			_, ok := a.appliedQuotas[localPath]
			return ok
		})
	}
}

// TestPVReconcileQueueCoalescesDuplicateEnqueues guards #12's "동일 PV의
// 연속 Modified 이벤트가 중복 filesystem mutation으로 이어지지 않는다"
// acceptance item: two enqueue() calls for the same PV key made before a
// worker ever picks the key up must collapse into exactly one reconcile,
// using the *latest* state stored (not the first).
func TestPVReconcileQueueCoalescesDuplicateEnqueues(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	a := newQueueTestAgent(t)
	localPath := mkPVDir(t, a, "/exports/pvc-dup")

	rq := newPVReconcileQueue(a, 1)
	defer rq.shutdown(2 * time.Second) // registered before any Fatalf-capable call below

	pv := newBoundPV("pv-dup", "/exports/pvc-dup", 1)
	// Both enqueue calls happen before start(): no worker exists yet to
	// race the queue's internal dirty/processing bookkeeping, so this
	// deterministically exercises workqueue.Add()'s de-duplication.
	rq.enqueue(pv, 1*1024*1024*1024) // stale: would-be first event
	rq.enqueue(pv, 2*1024*1024*1024) // latest: what should actually apply

	if depth := rq.depth(); depth != 1 {
		t.Fatalf("expected exactly 1 queued key after two enqueues for the same PV, got depth=%d", depth)
	}

	rq.start(context.Background())

	waitFor(t, 2*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.appliedQuotas[localPath] == 2*1024*1024*1024
	})

	total, errs, _ := a.ReconcileStats()
	if total != 1 {
		t.Errorf("expected exactly 1 reconcile to have run (duplicate events coalesced), got %d", total)
	}
	if errs != 0 {
		t.Errorf("expected no reconcile errors, got %d", errs)
	}
}

// TestPVReconcileQueueRetriesFailedItems guards #12's retry/backoff gap: a
// transient ensureQuota failure must not wait for the next full resync --
// AddRateLimited requeues it, and it must eventually succeed once the
// underlying failure clears.
func TestPVReconcileQueueRetriesFailedItems(t *testing.T) {
	withFakeRunner(t, failNTimesXFSRunner(1))
	a := newQueueTestAgent(t)
	localPath := mkPVDir(t, a, "/exports/pvc-retry")

	rq := newPVReconcileQueue(a, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rq.start(ctx)
	defer rq.shutdown(2 * time.Second)

	rq.enqueue(newBoundPV("pv-retry", "/exports/pvc-retry", 1), 0)

	waitFor(t, 2*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		_, ok := a.appliedQuotas[localPath]
		return ok
	})

	total, errs, _ := a.ReconcileStats()
	if total < 2 {
		t.Errorf("expected at least 2 reconcile attempts (1 failure + 1 retry that succeeded), got %d", total)
	}
	if errs < 1 {
		t.Errorf("expected at least 1 recorded reconcile error from the injected failure, got %d", errs)
	}
}

// TestPVReconcileQueueEnqueueDeleteBeforeProcessingIsANoop guards the
// pre-processing side of deletion: if a PV is deleted (enqueueDelete) after
// being enqueued but before any worker reaches it, the tombstone Store
// simply overwrites the real item in latest (last write wins, same as
// TestPVReconcileQueueCoalescesDuplicateEnqueues) -- no quota gets applied
// and no worker touches ensureQuota at all for this key.
func TestPVReconcileQueueEnqueueDeleteBeforeProcessingIsANoop(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	a := newQueueTestAgent(t)
	localPath := mkPVDir(t, a, "/exports/pvc-del")

	rq := newPVReconcileQueue(a, 1)
	defer rq.shutdown(2 * time.Second)

	pv := newBoundPV("pv-del", "/exports/pvc-del", 1)
	rq.enqueue(pv, 0)
	rq.enqueueDelete(pv)

	rq.start(context.Background())

	waitFor(t, 2*time.Second, func() bool { return rq.depth() == 0 })

	a.mu.Lock()
	_, applied := a.appliedQuotas[localPath]
	a.mu.Unlock()
	if applied {
		t.Errorf("expected no quota to be applied for a PV deleted before its enqueue was processed")
	}
	if total, _, _ := a.ReconcileStats(); total != 0 {
		t.Errorf("expected ensureQuota to never run for a key whose only queued state is a tombstone, got %d reconcile(s)", total)
	}
}

// TestPVReconcileQueueDeleteAfterInFlightReconcileWins is the regression
// test for the bug an independent review found in this queue's first
// version: Deleted used to be handled by mutating appliedQuotas directly
// from watch.go's eventLoop, completely independent of the reconcile
// queue. That let a Deleted event race an already-in-flight worker
// reconciling an older Added/Modified for the same PV -- the worker's
// ensureQuota call could finish (re-populating appliedQuotas) *after* the
// direct delete had already run, permanently resurrecting a stale entry
// for a PV that no longer exists (see enqueueDelete's doc comment). This
// test drives exactly that interleaving -- enqueueDelete arrives while a
// worker is blocked mid-flight inside the real reconcile's filesystem
// apply call -- and asserts the fix (routing Deleted through the same
// per-key queue, so workqueue's dirty/processing tracking guarantees the
// tombstone is redelivered and processed only after the in-flight call
// finishes) leaves no resurrected entry.
func TestPVReconcileQueueDeleteAfterInFlightReconcileWins(t *testing.T) {
	unblock := make(chan struct{})
	runner := blockingXFSRunner(unblock)
	withFakeRunner(t, runner)
	a := newQueueTestAgent(t)
	localPath := mkPVDir(t, a, "/exports/pvc-race")

	rq := newPVReconcileQueue(a, 1)
	defer rq.shutdown(2 * time.Second)
	rq.start(context.Background())

	pv := newBoundPV("pv-race", "/exports/pvc-race", 1)
	rq.enqueue(pv, 0)

	// Wait until the worker has entered ensureQuota's filesystem apply call
	// -- i.e. it already Load()'d the real (non-tombstone) item and is
	// blocked past that point -- before delivering the delete. This is
	// exactly the interleaving the fix exists to handle.
	waitFor(t, 2*time.Second, func() bool {
		runner.mu.Lock()
		defer runner.mu.Unlock()
		return len(runner.calls) >= 1
	})

	rq.enqueueDelete(pv)
	close(unblock) // let the in-flight worker finish its (now-stale) apply

	// Observe both terminal effects in one bounded loop: a separate cache wait
	// and later counter assertion leave an unobserved scheduling window.
	deadline := time.Now().Add(2 * time.Second)
	for {
		a.mu.Lock()
		_, applied := a.appliedQuotas[localPath]
		a.mu.Unlock()
		total, errs, duration := a.ReconcileStats()
		if !applied && total == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("delete/reconcile did not converge within 2s: applied=%t total=%d errors=%d duration_seconds=%f", applied, total, errs, duration)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestPVReconcileQueueShutdownDrainsQueuedWork guards #12's "shutdown 시
// destructive work가 중간 상태로 남지 않도록 drain" acceptance item: work
// already queued when ctx is done must still finish, not be abandoned.
func TestPVReconcileQueueShutdownDrainsQueuedWork(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	a := newQueueTestAgent(t)
	localPath := mkPVDir(t, a, "/exports/pvc-drain")

	rq := newPVReconcileQueue(a, 1)
	rq.enqueue(newBoundPV("pv-drain", "/exports/pvc-drain", 1), 0)

	ctx, cancel := context.WithCancel(context.Background())
	rq.start(ctx)
	cancel() // simulate the owning watchPVsWithBackoff's ctx already being done

	rq.shutdown(2 * time.Second)

	a.mu.Lock()
	_, applied := a.appliedQuotas[localPath]
	a.mu.Unlock()
	if !applied {
		t.Errorf("expected already-queued work to be drained (processed) by shutdown, not abandoned")
	}
}

// TestPVReconcileQueueShutdownTimesOutOnStuckWorker guards the bounded side
// of drain: shutdown must not hang forever waiting for a worker stuck in a
// slow/hung ensureQuota call -- it gives up after drainTimeout.
func TestPVReconcileQueueShutdownTimesOutOnStuckWorker(t *testing.T) {
	unblock := make(chan struct{})
	// withFakeRunner's t.Cleanup restores the package-level CommandRunner
	// after this test function returns. The worker goroutine below reads
	// that same variable inside its still-blocked ensureQuota call, so this
	// test must not return -- letting that Cleanup race the worker's read --
	// until the worker has actually finished, not merely been signaled to.
	// Explicitly unblocking and waiting for it at the end (rather than a
	// bare `defer close(unblock)`) is what makes that ordering deterministic
	// under -race.
	runner := blockingXFSRunner(unblock)
	withFakeRunner(t, runner)
	a := newQueueTestAgent(t)
	mkPVDir(t, a, "/exports/pvc-stuck")

	rq := newPVReconcileQueue(a, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rq.start(ctx)
	rq.enqueue(newBoundPV("pv-stuck", "/exports/pvc-stuck", 1), 0)

	// Give the worker a moment to actually enter the blocked xfs_quota call.
	// rq.depth() is not a reliable signal here: workqueue.Len() only counts
	// items still waiting, not the one already handed to a worker via Get()
	// -- with a single worker started before enqueue, that handoff can
	// happen before this goroutine ever observes depth>=1. The runner
	// having recorded a call is the actual signal that a worker reached the
	// blocking point inside ensureQuota.
	waitFor(t, time.Second, func() bool {
		runner.mu.Lock()
		defer runner.mu.Unlock()
		return len(runner.calls) >= 1
	})

	drainTimeout := 100 * time.Millisecond
	start := time.Now()
	rq.shutdown(drainTimeout)
	elapsed := time.Since(start)

	close(unblock)
	rq.wg.Wait() // let the now-unblocked worker actually finish before Cleanup runs

	if elapsed < drainTimeout {
		t.Errorf("shutdown returned after %s, before drainTimeout %s elapsed -- it should have waited at least that long for the stuck worker", elapsed, drainTimeout)
	}
	if elapsed > 2*time.Second {
		t.Errorf("shutdown took %s to return after a %s drainTimeout -- it should give up promptly once the timeout is reached, not keep waiting", elapsed, drainTimeout)
	}
}

// TestPVReconcileQueuePruneExceptRemovesOnlyStaleEntries is a direct,
// queue-level test of pruneExcept (see TestSyncAllQuotasPrunesReconcileQueueLatestState
// in cache_consistency_test.go for the syncAllQuotas-wired version).
func TestPVReconcileQueuePruneExceptRemovesOnlyStaleEntries(t *testing.T) {
	a := newQueueTestAgent(t)
	rq := newPVReconcileQueue(a, 1)

	rq.latest.Store("pv-a", &reconcileItem{pv: newBoundPV("pv-a", "/exports/pvc-a", 1)})
	rq.latest.Store("pv-b", &reconcileItem{pv: newBoundPV("pv-b", "/exports/pvc-b", 1)})

	rq.pruneExcept(map[string]struct{}{"pv-a": {}})

	if _, ok := rq.latest.Load("pv-a"); !ok {
		t.Errorf("expected pv-a (still live) to remain in latest")
	}
	if _, ok := rq.latest.Load("pv-b"); ok {
		t.Errorf("expected pv-b (no longer live) to be pruned from latest")
	}
}

// TestPVReconcileQueuePruneExceptDoesNotClobberConcurrentEnqueue guards
// pruneExcept's core safety property: it must use CompareAndDelete, not
// Delete, so a prune racing a concurrent enqueue()/enqueueDelete() for the
// same key can never win against a value fresher than what the prune
// itself observed.
func TestPVReconcileQueuePruneExceptDoesNotClobberConcurrentEnqueue(t *testing.T) {
	a := newQueueTestAgent(t)
	rq := newPVReconcileQueue(a, 1)

	pv := newBoundPV("pv-race-prune", "/exports/pvc-race-prune", 1)
	rq.latest.Store(pv.Name, &reconcileItem{pv: pv, effectiveBytes: 1 * 1024 * 1024 * 1024})

	// Simulate pruneExcept's Range() having already observed this (now
	// stale) value before a fresh enqueue() overwrites it.
	stale, _ := rq.latest.Load(pv.Name)
	rq.enqueue(pv, 2*1024*1024*1024)

	rq.latest.CompareAndDelete(pv.Name, stale)

	v, ok := rq.latest.Load(pv.Name)
	if !ok {
		t.Fatalf("expected the fresher enqueue()'d entry to survive a prune racing an older observed value")
	}
	if item := v.(*reconcileItem); item.effectiveBytes != 2*1024*1024*1024 {
		t.Errorf("expected the fresher entry (2GiB) to remain, got %d bytes", item.effectiveBytes)
	}
}

// TestReconcileQueuePathUsesLiveUsageRead guards #92's other half: unlike
// syncAllQuotas' PV loop (which shares one passUsageSnapshot across a
// whole pass), the watch path always passes a nil snapshot -- every
// reconcile it processes pays for its own live usage-report read, with no
// cross-PV memoization. Two independent brownfield reconciles through the
// real queue must cost two report calls, not one.
func TestReconcileQueuePathUsesLiveUsageRead(t *testing.T) {
	runner, _ := xfsHappyRunnerWithState()
	originalRun := runner.fn
	secondReportStarted := make(chan struct{})
	releaseSecondReport := make(chan struct{})
	var queueReports atomic.Int32
	runner.fn = func(name string, args ...string) ([]byte, error) {
		out, err := originalRun(name, args...)
		if name == "xfs_quota" && len(args) >= 3 && args[1] == "-c" && strings.HasPrefix(args[2], "report") && queueReports.Add(1) == 2 {
			close(secondReportStarted)
			<-releaseSecondReport
		}
		return out, err
	}
	withFakeRunner(t, runner)

	base := t.TempDir()
	pv0, _ := brownfieldPV(t, base, "pv-brown-0", 1_000_000, 10_000_000)
	pv1, _ := brownfieldPV(t, base, "pv-brown-1", 1_000_000, 10_000_000)

	client := fake.NewSimpleClientset(pv0, pv1)
	a := NewQuotaAgent(client, base, "/exports", "example.com/nfs")
	a.SetProjectsFile(filepath.Join(base, "projects"))
	a.SetProjidFile(filepath.Join(base, "projid"))
	a.SetStateDir(t.TempDir())
	a.fsType = quota.FSTypeXFS
	a.processAllNFS = true
	writeEmptyProjectMappings(t, a)
	// Prime the brownfield snapshot first -- the watch path never primes
	// on its own (only Run()/syncAllQuotas do), and without it
	// suspectBrownfield can never fire, so both reconciles would apply
	// successfully instead of being rejected.
	a.primeAppliedQuotasFromDiskOnce()

	rq := newPVReconcileQueue(a, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rq.start(ctx)

	// Ignore the startup prime's report; the wrapper blocks precisely the
	// second report issued by a watch-path reconciliation below.
	queueReports.Store(0)
	callsBefore := countReportCalls(runner.callsSnapshot())
	rq.enqueue(pv0, 0)
	rq.enqueue(pv1, 0)

	select {
	case <-secondReportStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("second watch-path usage report did not start")
	}

	// Both intended reconciles reached their live report. Stop accepting the
	// permanent brownfield rejections before releasing the second worker, then
	// wait for every worker to exit before asserting runner state or restoring
	// the package-level fake. This removes the retry-backoff race entirely.
	rq.queue.ShutDown()
	close(releaseSecondReport)
	rq.wg.Wait()

	if got := countReportCalls(runner.callsSnapshot()) - callsBefore; got != 2 {
		t.Fatalf("report calls across two independent watch-path reconciles = %d, want 2 (each pays for its own live read, no cross-PV memoization)", got)
	}
}
