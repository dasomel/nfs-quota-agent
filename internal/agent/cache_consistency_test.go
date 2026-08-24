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
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dasomel/nfs-quota-agent/internal/ui"
)

// RemoveOrphan deletes the path's quota, so leaving the path in appliedQuotas
// would make ensureQuota's cache-hit shortcut skip a PV that later reuses it,
// silently leaving that volume unenforced.
func TestRemoveOrphanClearsAppliedQuotaCache(t *testing.T) {
	client := fake.NewSimpleClientset()
	a := newTestAgent(t, client)

	orphanPath := filepath.Join(a.nfsBasePath, "pvc-recycled")
	if err := os.MkdirAll(orphanPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	const capacity int64 = 5 * 1024 * 1024 * 1024
	a.mu.Lock()
	a.appliedQuotas[orphanPath] = capacity
	a.mu.Unlock()

	// fsType empty keeps RemoveOrphan away from removeQuotaForPath, which is
	// exercised elsewhere; the cache clearing is what matters here.
	if err := a.RemoveOrphan(ui.OrphanInfo{Path: orphanPath, DirName: "pvc-recycled"}); err != nil {
		t.Fatalf("RemoveOrphan: %v", err)
	}

	a.mu.Lock()
	got, still := a.appliedQuotas[orphanPath]
	a.mu.Unlock()
	if still {
		t.Fatalf("appliedQuotas still records %s = %d after RemoveOrphan; a PV reusing this path would be skipped", orphanPath, got)
	}
}

// A watch that reconnects restarts from the current resourceVersion, so
// deletions during the outage never arrive. The periodic full list is the only
// thing that can notice, so it has to prune.
func TestSyncAllQuotasPrunesEntriesWithoutPV(t *testing.T) {
	pv := newBoundPV("pv-live", "/exports/pv-live", 1)
	client := fake.NewSimpleClientset(pv)
	a := newTestAgent(t, client)
	// newBoundPV builds a native NFS PV with no provisioned-by annotation,
	// so shouldProcessPV only accepts it with processAllNFS on.
	a.SetProcessAllNFS(true)

	withFakeRunner(t, &fakeRunner{})

	livePath := a.nfsPathToLocal("/exports/pv-live")
	stalePath := filepath.Join(a.nfsBasePath, "pv-deleted-while-watch-was-down")

	a.mu.Lock()
	a.appliedQuotas[livePath] = 1
	a.appliedQuotas[stalePath] = 42
	a.mu.Unlock()

	if err := a.syncAllQuotas(context.Background()); err != nil {
		t.Fatalf("syncAllQuotas: %v", err)
	}

	a.mu.Lock()
	_, staleKept := a.appliedQuotas[stalePath]
	_, liveKept := a.appliedQuotas[livePath]
	a.mu.Unlock()

	if staleKept {
		t.Errorf("entry for %s survived the sync although no PV backs it", stalePath)
	}
	if !liveKept {
		t.Errorf("entry for %s was pruned although its PV is still listed", livePath)
	}
}

// Pruning must key off the PV list, not off whether the quota was applied
// successfully, or a transient apply failure would evict a live path.
func TestSyncAllQuotasKeepsLivePathWhenApplyFails(t *testing.T) {
	pv := newBoundPV("pv-failing", "/exports/pv-failing", 1)
	client := fake.NewSimpleClientset(pv)
	a := newTestAgent(t, client)
	a.SetProcessAllNFS(true)

	livePath := a.nfsPathToLocal("/exports/pv-failing")
	if err := os.MkdirAll(livePath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	withFakeRunner(t, &fakeRunner{
		fn: func(name string, args ...string) ([]byte, error) {
			return nil, os.ErrPermission
		},
	})

	a.mu.Lock()
	a.appliedQuotas[livePath] = 1
	a.mu.Unlock()

	if err := a.syncAllQuotas(context.Background()); err != nil {
		t.Fatalf("syncAllQuotas: %v", err)
	}

	a.mu.Lock()
	_, kept := a.appliedQuotas[livePath]
	a.mu.Unlock()
	if !kept {
		t.Error("a live PV's cache entry was pruned because its quota apply failed")
	}
}

// TestSyncAllQuotasPrunesReconcileQueueLatestState guards the reconcile
// queue's own version of the exact leak class the two tests above guard
// for appliedQuotas: pvReconcileQueue.latest (reconcile_queue.go) only ever
// gets cleaned up by a live watch delivering a Deleted event for that PV
// (enqueueDelete) -- a PV deleted while the watch was disconnected leaves
// its entry cached for the life of the process otherwise. syncAllQuotas
// must prune it (pruneExcept) using the same live-PV-list authority
// pruneAppliedQuotas already uses, the same way it does for appliedQuotas.
func TestSyncAllQuotasPrunesReconcileQueueLatestState(t *testing.T) {
	pv := newBoundPV("pv-live", "/exports/pv-live", 1)
	client := fake.NewSimpleClientset(pv)
	a := newTestAgent(t, client)
	a.SetProcessAllNFS(true)
	withFakeRunner(t, &fakeRunner{})

	rq := newPVReconcileQueue(a, 1) // not started: this test only checks latest's contents
	a.reconcileQueue.Store(rq)
	t.Cleanup(func() { a.reconcileQueue.Store(nil) })

	rq.latest.Store("pv-live", &reconcileItem{pv: pv})
	rq.latest.Store("pv-deleted-while-watch-was-down", &reconcileItem{pv: newBoundPV("pv-deleted-while-watch-was-down", "/exports/gone", 1)})

	if err := a.syncAllQuotas(context.Background()); err != nil {
		t.Fatalf("syncAllQuotas: %v", err)
	}

	if _, ok := rq.latest.Load("pv-deleted-while-watch-was-down"); ok {
		t.Error("latest entry for a PV no longer in the cluster survived syncAllQuotas")
	}
	if _, ok := rq.latest.Load("pv-live"); !ok {
		t.Error("latest entry for a still-live PV was pruned by syncAllQuotas")
	}
}
