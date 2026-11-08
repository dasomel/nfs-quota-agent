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
	"path/filepath"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/dasomel/nfs-quota-agent/internal/quota"
)

// stormResult is what runPVEventStorm measured for one storm.
type stormResult struct {
	elapsed        time.Duration // first event sent -> every PV applied and queue drained
	reconcileTotal int64
	errors         int64
}

// stormProgress combines every observable signal that proves the event-storm
// pipeline is advancing, so a single waitForProgress call neither mistakes
// steady queue depth during in-flight Modified reconciles for a stall nor
// returns before every Modified 2Gi update has landed.
type stormProgress struct {
	applied        int
	evenModified   int
	reconcileTotal int64
	queueDepth     int
}

// timerController is implemented by *testing.B; runPVEventStorm uses it to
// keep per-iteration setup and teardown out of the measured time.
type timerController interface {
	StopTimer()
	StartTimer()
}

// setupStormEnv swaps process-global state (quota.CommandRunner and
// slog.Default) once for tb and restores both via tb.Cleanup. The returned
// closure resets the fake runner's call log and simulated XFS table between
// benchmark iterations without accumulating tb.Cleanup callbacks across b.N.
func setupStormEnv(tb testing.TB) func() {
	tb.Helper()
	runner, state := xfsHappyRunnerWithState()
	withFakeRunner(tb, runner)

	// Synchronous log writes (especially under `go test -v`'s output
	// capture) would dominate the measured work; discard logging for the
	// storm itself.
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	tb.Cleanup(func() { slog.SetDefault(prevLogger) })

	return func() {
		runner.mu.Lock()
		runner.calls = nil
		runner.mu.Unlock()
		state.mu.Lock()
		clear(state.applied)
		state.mu.Unlock()
	}
}

// runPVEventStorm sends numPVs Added + numPVs/2 Modified PV events through
// the real watch -> reconcile-queue -> ensureQuota path and asserts the
// correctness invariants (exact enforcedBytes per PV, no leaked keys,
// reconcileTotal within [numPVs, raw event count], zero errors, clean
// shutdown). Shared by TestWatchPVsEventStormAtScale and
// BenchmarkWatchPVsEventStorm.
//
// It uses the stubbed quota.CommandRunner (no real xfs_quota), so it
// measures the controller pipeline -- watch, queue coalescing, ensureQuota
// bookkeeping and the real projects/projid fsync -- NOT kernel enforcement.
//
// Completion is awaited with a single waitForProgress (stall detection), not
// a fixed deadline: every new project costs a real fsync
// (quota.writeFileSynced), so on a loaded host a fixed 10s total deadline
// flaked (load avg ~30, 2026-11-08) even though the pipeline was advancing.
// The test fails only if progress stalls, with a single overall absolute cap.
func runPVEventStorm(tb testing.TB, numPVs int, resetEnv func()) stormResult {
	tb.Helper()
	const (
		oneGi = 1 * 1024 * 1024 * 1024
		twoGi = 2 * 1024 * 1024 * 1024
		stall = 10 * time.Second
		cap   = 120 * time.Second
	)
	tc, _ := tb.(timerController)
	if tc != nil {
		tc.StopTimer()
		defer tc.StartTimer()
	}
	if resetEnv != nil {
		resetEnv()
	}

	rootDir, err := os.MkdirTemp("", "nfs-quota-storm-*")
	if err != nil {
		tb.Fatalf("mkdirtemp: %v", err)
	}
	defer func() { _ = os.RemoveAll(rootDir) }()

	baseDir := filepath.Join(rootDir, "exports")
	stateDir := filepath.Join(rootDir, "state")
	for _, dir := range []string{baseDir, stateDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			tb.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fw := watch.NewFake()
	defer fw.Stop()

	client := fake.NewSimpleClientset()
	client.PrependWatchReactor("persistentvolumes", func(action ktesting.Action) (bool, watch.Interface, error) {
		return true, fw, nil
	})

	a := newTestAgentWithDirs(client, baseDir, stateDir)
	a.nfsServerPath = "/exports"
	a.fsType = quota.FSTypeXFS
	a.processAllNFS = true

	localPaths := make([]string, numPVs)
	pvs := make([]*v1.PersistentVolume, numPVs)
	for i := 0; i < numPVs; i++ {
		nfsPath := fmt.Sprintf("/exports/pvc-storm-%d", i)
		localPaths[i] = a.nfsPathToLocal(nfsPath)
		if err := os.MkdirAll(localPaths[i], 0755); err != nil {
			tb.Fatalf("mkdir %s: %v", localPaths[i], err)
		}
		pvs[i] = newBoundPV(fmt.Sprintf("pv-storm-%d", i), nfsPath, 1)
	}

	done := runWatchPVs(a, ctx)
	waitedForWatch := false
	defer func() {
		if tc != nil {
			tc.StopTimer()
		}
		cancel()
		fw.Stop()
		if !waitedForWatch {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				// Surface the leak instead of returning silently; this path
				// only runs after an earlier failure already aborted the storm.
				tb.Errorf("watchPVs goroutine still running 10s after cancel on the failure path (leaked)")
			}
		}
	}()

	if tc != nil {
		tc.StartTimer()
	}
	start := time.Now()

	for _, pv := range pvs {
		fw.Add(pv)
	}
	// Concurrent capacity churn on half the PVs: Modified events for keys
	// that may already be queued or mid-flight from the Added wave,
	// exercising the coalescing path under real event-stream load.
	for i := 0; i < numPVs; i += 2 {
		pv2 := pvs[i].DeepCopy()
		pv2.Spec.Capacity = v1.ResourceList{v1.ResourceStorage: *resource.NewQuantity(twoGi, resource.BinarySI)}
		fw.Modify(pv2)
	}

	evenCount := (numPVs + 1) / 2
	waitForProgress(tb, stall, cap, func() stormProgress {
		depth := a.ReconcileQueueDepth()
		total, _, _ := a.ReconcileStats()
		a.mu.Lock()
		applied := len(a.appliedQuotas)
		evenModified := 0
		for i := 0; i < numPVs; i += 2 {
			if a.appliedQuotas[localPaths[i]].enforcedBytes == twoGi {
				evenModified++
			}
		}
		a.mu.Unlock()
		return stormProgress{
			applied:        applied,
			evenModified:   evenModified,
			reconcileTotal: total,
			queueDepth:     depth,
		}
	}, func(p stormProgress) bool {
		return p.applied >= numPVs && p.queueDepth == 0 && p.evenModified == evenCount
	})
	elapsed := time.Since(start)
	if tc != nil {
		tc.StopTimer()
	}

	a.mu.Lock()
	for i, localPath := range localPaths {
		want := int64(oneGi)
		if i%2 == 0 {
			want = twoGi
		}
		if got := a.appliedQuotas[localPath].enforcedBytes; got != want {
			tb.Errorf("pv-storm-%d: appliedQuotas[%s] = %d, want %d", i, localPath, got, want)
		}
	}
	appliedCount := len(a.appliedQuotas)
	a.mu.Unlock()
	if appliedCount != numPVs {
		tb.Errorf("appliedQuotas has %d entries, want exactly %d (no leaked or missing keys)", appliedCount, numPVs)
	}

	total, errs, _ := a.ReconcileStats()
	totalEvents := int64(numPVs + (numPVs+1)/2)
	if total < int64(numPVs) {
		tb.Errorf("reconcileTotal=%d is below numPVs=%d -- some PV never got reconciled at all", total, numPVs)
	}
	if total > totalEvents {
		tb.Errorf("reconcileTotal=%d exceeds the raw event count sent (%d) -- possible unbounded requeue loop", total, totalEvents)
	}
	if errs != 0 {
		tb.Errorf("expected zero reconcile errors in a happy-path event storm, got %d", errs)
	}

	waitedForWatch = true
	cancel()
	fw.Stop()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		tb.Fatalf("watchPVs did not stop after context cancellation following the event storm")
	}
	return stormResult{elapsed: elapsed, reconcileTotal: total, errors: errs}
}

// TestWatchPVsEventStormAtScale exercises #12's event-storm acceptance item
// at reduced scale (300 PVs) to keep `make test` wall-clock predictable; every
// distinct new project costs a real fsync (PR #25, quota.writeFileSynced).
// Larger scales (100-2000 PVs) run in BenchmarkWatchPVsEventStorm
// (`make bench`). See reconcile_queue_test.go for fast unit-level coverage
// of duplicate-event coalescing and bounded-worker behavior.
func TestWatchPVsEventStormAtScale(t *testing.T) {
	resetEnv := setupStormEnv(t)
	runPVEventStorm(t, 300, resetEnv)
}

// BenchmarkWatchPVsEventStorm is the synthetic scalability harness for
// 100-2000 PVs. Stubbed CommandRunner: it measures the controller pipeline,
// not kernel quota enforcement (see runPVEventStorm).
func BenchmarkWatchPVsEventStorm(b *testing.B) {
	for _, n := range []int{100, 500, 1000, 2000} {
		b.Run(fmt.Sprintf("pvs=%d", n), func(b *testing.B) {
			resetEnv := setupStormEnv(b)
			b.ResetTimer()
			var elapsed time.Duration
			var reconciles int64
			for i := 0; i < b.N; i++ {
				r := runPVEventStorm(b, n, resetEnv)
				elapsed += r.elapsed
				reconciles += r.reconcileTotal
			}
			b.ReportMetric(elapsed.Seconds()*1000/float64(b.N)/float64(n), "ms/pv")
			b.ReportMetric(float64(n)*float64(b.N)/elapsed.Seconds(), "pvs/s")
			b.ReportMetric(float64(reconciles)/float64(b.N), "reconciles/op")
		})
	}
}
