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
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/dasomel/nfs-quota-agent/internal/quota"
)

// TestWatchPVsEventStormAtScale exercises #12's "1k/10k PV 규모에서 event
// storm 테스트를 통과한다" acceptance item at reduced scale, not literally
// 1,000+: this test drives the FULL watch -> reconcile-queue -> ensureQuota
// pipeline, and every distinct new project ensureQuota creates costs a real
// fsync on both the projects and projid files (PR #25's crash-safety
// hardening -- see quota.writeFileSynced -- not bypassable from a test
// without exercising a different code path than production). Measured
// locally (macOS/APFS) at ~7.8ms per fsync round-trip, 1,500 PVs took ~23s;
// GitHub Actions' ubuntu-latest runners are typically faster but that was
// not verified before deciding to keep CI wall-clock predictable. numPVs
// below was chosen with the user to stay fast rather than to hit the
// acceptance item's literal 1k floor -- see reconcile_queue_test.go for
// dedicated, fast, sub-second unit-level coverage of duplicate-event
// coalescing and bounded-worker behavior; this test's job is proving the
// same mechanics hold when driven by many *distinct* PVs through the real
// watch event stream, which a small numPVs still demonstrates (the queue's
// per-item cost is O(1), so this doesn't qualitatively change at 1k/10k --
// it only takes proportionally longer, dominated by the same fsync cost).
//
// If CI wall-clock or a real cluster later prove fsync is cheaper than
// measured here, numPVs can be raised back toward 1k+ to satisfy the
// acceptance item's literal wording.
func TestWatchPVsEventStormAtScale(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())

	// At 1,500+ reconciles, each logging 1-2 lines through the default
	// slog handler, synchronous stdout writes (especially under `go test
	// -v`'s output capture) dominate this test's wall-clock far more than
	// the actual queue/reconcile work being measured. Discard logging for
	// the storm itself; every other test in this package still exercises
	// the log lines this would suppress.
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	client := fake.NewSimpleClientset()
	fw := watch.NewFake()
	client.PrependWatchReactor("persistentvolumes", func(action ktesting.Action) (bool, watch.Interface, error) {
		return true, fw, nil
	})

	a := newTestAgent(t, client)
	a.nfsServerPath = "/exports"
	a.fsType = quota.FSTypeXFS
	a.processAllNFS = true

	const numPVs = 300
	const oneGi = 1 * 1024 * 1024 * 1024
	const twoGi = 2 * 1024 * 1024 * 1024

	localPaths := make([]string, numPVs)
	pvs := make([]*v1.PersistentVolume, numPVs)
	for i := 0; i < numPVs; i++ {
		nfsPath := fmt.Sprintf("/exports/pvc-storm-%d", i)
		localPaths[i] = a.nfsPathToLocal(nfsPath)
		if err := os.MkdirAll(localPaths[i], 0755); err != nil {
			t.Fatalf("mkdir %s: %v", localPaths[i], err)
		}
		pvs[i] = newBoundPV(fmt.Sprintf("pv-storm-%d", i), nfsPath, 1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runWatchPVs(a, ctx)

	for _, pv := range pvs {
		fw.Add(pv)
	}
	// Concurrent capacity churn on half the PVs, simulating the 검증 step's
	// "동시에 capacity를 변경" -- these are Modified events for keys that
	// may already be queued or mid-flight from the Added wave above,
	// exercising the same coalescing path as
	// TestPVReconcileQueueCoalescesDuplicateEnqueues, but now under
	// real event-stream load instead of two hand-sequenced calls.
	for i := 0; i < numPVs; i += 2 {
		pv2 := pvs[i].DeepCopy()
		pv2.Spec.Capacity = v1.ResourceList{v1.ResourceStorage: *resource.NewQuantity(twoGi, resource.BinarySI)}
		fw.Modify(pv2)
	}

	waitFor(t, 10*time.Second, func() bool {
		a.mu.Lock()
		n := len(a.appliedQuotas)
		a.mu.Unlock()
		return n >= numPVs
	})
	waitFor(t, 5*time.Second, func() bool { return a.ReconcileQueueDepth() == 0 })

	a.mu.Lock()
	for i, localPath := range localPaths {
		want := int64(oneGi)
		if i%2 == 0 {
			want = twoGi
		}
		if got := a.appliedQuotas[localPath]; got != want {
			t.Errorf("pv-storm-%d: appliedQuotas[%s] = %d, want %d", i, localPath, got, want)
		}
	}
	appliedCount := len(a.appliedQuotas)
	a.mu.Unlock()
	if appliedCount != numPVs {
		t.Errorf("appliedQuotas has %d entries, want exactly %d (no leaked or missing keys)", appliedCount, numPVs)
	}

	total, errs, _ := a.ReconcileStats()
	totalEvents := int64(numPVs + numPVs/2)
	if total < numPVs {
		t.Errorf("reconcileTotal=%d is below numPVs=%d -- some PV never got reconciled at all", total, numPVs)
	}
	if total > totalEvents {
		t.Errorf("reconcileTotal=%d exceeds the raw event count sent (%d) -- possible unbounded requeue loop", total, totalEvents)
	}
	if errs != 0 {
		t.Errorf("expected zero reconcile errors in a happy-path event storm, got %d", errs)
	}

	cancel()
	fw.Stop()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("watchPVs did not stop after context cancellation following the event storm")
	}
}
