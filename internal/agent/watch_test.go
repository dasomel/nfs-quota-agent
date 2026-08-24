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
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/dasomel/nfs-quota-agent/internal/quota"
)

func runWatchPVs(a *QuotaAgent, ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		a.watchPVs(ctx)
		close(done)
	}()
	return done
}

func TestWatchPVsReturnsImmediatelyWhenContextAlreadyDone(t *testing.T) {
	client := fake.NewSimpleClientset()
	var watchCalled atomic.Bool
	client.PrependWatchReactor("persistentvolumes", func(action ktesting.Action) (bool, watch.Interface, error) {
		watchCalled.Store(true)
		return true, watch.NewFake(), nil
	})

	a := newTestAgent(t, client)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done before watchPVs ever runs

	done := runWatchPVs(a, ctx)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("watchPVs did not return promptly for an already-canceled context")
	}
	if watchCalled.Load() {
		t.Fatalf("watchPVs should not attempt to start a watch when ctx is already done")
	}
}

func TestWatchPVsRetriesOnWatchStartError(t *testing.T) {
	client := fake.NewSimpleClientset()
	var attempts atomic.Int32
	client.PrependWatchReactor("persistentvolumes", func(action ktesting.Action) (bool, watch.Interface, error) {
		attempts.Add(1)
		return true, nil, errors.New("simulated watch start failure")
	})

	a := newTestAgent(t, client)
	// Short-lived context: the retry backoff starts at 1s, so this proves
	// watchPVs honors ctx.Done() during the backoff wait instead of blocking
	// for the full backoff duration.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := runWatchPVs(a, ctx)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("watchPVs did not stop after context deadline during backoff wait")
	}
	if attempts.Load() < 1 {
		t.Fatalf("expected at least one watch attempt")
	}
}

func TestWatchPVsDispatchesAddModifyDelete(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())

	client := fake.NewSimpleClientset()
	fw := watch.NewFake()
	client.PrependWatchReactor("persistentvolumes", func(action ktesting.Action) (bool, watch.Interface, error) {
		return true, fw, nil
	})

	a := newTestAgent(t, client)
	a.nfsServerPath = "/exports"
	a.fsType = quota.FSTypeXFS
	// newBoundPV builds a native NFS PV without the provisioned-by annotation,
	// so processAllNFS must be on for shouldProcessPV to accept it.
	a.processAllNFS = true

	localPath := a.nfsPathToLocal("/exports/pvc-w1")
	if err := os.MkdirAll(localPath, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runWatchPVs(a, ctx)

	pv := newBoundPV("pv-w1", "/exports/pvc-w1", 1)
	fw.Add(pv)

	waitFor(t, 2*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		_, ok := a.appliedQuotas[localPath]
		return ok
	})

	// Modify with a larger capacity should update the tracked quota.
	pv2 := pv.DeepCopy()
	pv2.Spec.Capacity = v1.ResourceList{
		v1.ResourceStorage: *resource.NewQuantity(2*1024*1024*1024, resource.BinarySI),
	}
	fw.Modify(pv2)

	waitFor(t, 2*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.appliedQuotas[localPath] == 2*1024*1024*1024
	})

	// Delete should drop quota tracking for the path entirely.
	fw.Delete(pv2)
	waitFor(t, 2*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		_, ok := a.appliedQuotas[localPath]
		return !ok
	})

	cancel()
	fw.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("watchPVs did not stop after context cancellation")
	}
}

func TestWatchPVsRestartsAfterChannelCloses(t *testing.T) {
	client := fake.NewSimpleClientset()
	fw := watch.NewFake()
	var attempts atomic.Int32
	client.PrependWatchReactor("persistentvolumes", func(action ktesting.Action) (bool, watch.Interface, error) {
		attempts.Add(1)
		return true, fw, nil
	})

	a := newTestAgent(t, client)
	ctx, cancel := context.WithCancel(context.Background())
	done := runWatchPVs(a, ctx)

	waitFor(t, time.Second, func() bool { return attempts.Load() >= 1 })

	// Closing the watch channel triggers the "watch ended, restarting" path.
	// Cancel concurrently so the post-close select picks ctx.Done() instead of
	// waiting out the real (1s) reconnect backoff.
	cancel()
	fw.Stop()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("watchPVs did not stop after channel close + context cancellation")
	}
}

// watchResourceVersionOf extracts the ResourceVersion a fake Watch() call was
// made with, for reactors that want to assert on it.
func watchResourceVersionOf(action ktesting.Action) (string, bool) {
	wa, ok := action.(ktesting.WatchAction)
	if !ok {
		return "", false
	}
	return wa.GetWatchRestrictions().ResourceVersion, true
}

// TestWatchPVsListsBeforeFirstWatchToObtainResourceVersion guards #12's
// "initial List 이후 얻은 resourceVersion을 기준으로 Watch를 시작한다" acceptance
// item: a bare Watch() with no ResourceVersion starts a brand-new watch from
// "now", silently skipping any change between "server computes current
// state" and "watch stream opens" -- the standard List-then-Watch pattern
// closes that gap.
func TestWatchPVsListsBeforeFirstWatchToObtainResourceVersion(t *testing.T) {
	client := fake.NewSimpleClientset()

	var listCalls atomic.Int32
	client.PrependReactor("list", "persistentvolumes", func(action ktesting.Action) (bool, runtime.Object, error) {
		listCalls.Add(1)
		return true, &v1.PersistentVolumeList{ListMeta: metav1.ListMeta{ResourceVersion: "1000"}}, nil
	})

	var watchRV atomic.Value // string
	fw := watch.NewFake()
	client.PrependWatchReactor("persistentvolumes", func(action ktesting.Action) (bool, watch.Interface, error) {
		if rv, ok := watchResourceVersionOf(action); ok {
			watchRV.Store(rv)
		}
		return true, fw, nil
	})

	a := newTestAgent(t, client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runWatchPVs(a, ctx)

	waitFor(t, time.Second, func() bool { return listCalls.Load() >= 1 })
	waitFor(t, time.Second, func() bool {
		v, ok := watchRV.Load().(string)
		return ok && v == "1000"
	})

	cancel()
	fw.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("watchPVs did not stop")
	}
}

// TestWatchPVsResumesFromLastResourceVersionOnReconnect guards #12's "watch
// reconnect 시 resourceVersion 기반 resume/re-list" acceptance item: after an
// event has been seen, a reconnect must resume from that event's
// resourceVersion (no gap, no re-List), not restart from "now" or re-run
// the initial List.
func TestWatchPVsResumesFromLastResourceVersionOnReconnect(t *testing.T) {
	client := fake.NewSimpleClientset()

	var listCalls atomic.Int32
	client.PrependReactor("list", "persistentvolumes", func(action ktesting.Action) (bool, runtime.Object, error) {
		listCalls.Add(1)
		return true, &v1.PersistentVolumeList{ListMeta: metav1.ListMeta{ResourceVersion: "1000"}}, nil
	})

	var mu sync.Mutex
	var watchRVs []string
	var attempt atomic.Int32
	fw1 := watch.NewFake()
	client.PrependWatchReactor("persistentvolumes", func(action ktesting.Action) (bool, watch.Interface, error) {
		n := attempt.Add(1)
		if rv, ok := watchResourceVersionOf(action); ok {
			mu.Lock()
			watchRVs = append(watchRVs, rv)
			mu.Unlock()
		}
		if n == 1 {
			return true, fw1, nil
		}
		return true, watch.NewFake(), nil
	})

	a := newTestAgent(t, client)
	cfg := watchBackoffConfig{minBackoff: 5 * time.Millisecond, maxBackoff: 20 * time.Millisecond, minHealthyDuration: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { a.watchPVsWithBackoff(ctx, cfg); close(done) }()

	waitFor(t, time.Second, func() bool { return attempt.Load() >= 1 })

	// newBoundPV's PV lacks the provisioned-by annotation and processAllNFS
	// is off, so shouldProcessPV rejects it -- irrelevant here since
	// resourceVersion tracking happens unconditionally, before that check.
	pv := newBoundPV("pv-rv", "/exports/pvc-rv", 1)
	pv.ResourceVersion = "1042"
	fw1.Add(pv)
	fw1.Stop()

	waitFor(t, time.Second, func() bool { return attempt.Load() >= 2 })

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("watchPVsWithBackoff did not stop")
	}

	if listCalls.Load() != 1 {
		t.Fatalf("expected exactly 1 List call (only before the first watch), got %d", listCalls.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(watchRVs) < 2 {
		t.Fatalf("expected at least 2 watch attempts, got %d: %v", len(watchRVs), watchRVs)
	}
	if watchRVs[0] != "1000" {
		t.Errorf("first watch resourceVersion = %q, want %q (from the initial List)", watchRVs[0], "1000")
	}
	if watchRVs[1] != "1042" {
		t.Errorf("second watch resourceVersion = %q, want %q (resumed from the last event, not re-Listed)", watchRVs[1], "1042")
	}
}

// TestWatchPVsResourceVersionGoneTriggersFreshList guards #12's "Watch `410
// Gone`/expired resourceVersion 발생 시 안전하게 List→Watch를 재시작한다"
// acceptance item.
func TestWatchPVsResourceVersionGoneTriggersFreshList(t *testing.T) {
	client := fake.NewSimpleClientset()

	var listCalls atomic.Int32
	client.PrependReactor("list", "persistentvolumes", func(action ktesting.Action) (bool, runtime.Object, error) {
		n := listCalls.Add(1)
		rv := "1000"
		if n > 1 {
			rv = "2000"
		}
		return true, &v1.PersistentVolumeList{ListMeta: metav1.ListMeta{ResourceVersion: rv}}, nil
	})

	var mu sync.Mutex
	var watchRVs []string
	var attempt atomic.Int32
	fw1 := watch.NewFake()
	client.PrependWatchReactor("persistentvolumes", func(action ktesting.Action) (bool, watch.Interface, error) {
		n := attempt.Add(1)
		if rv, ok := watchResourceVersionOf(action); ok {
			mu.Lock()
			watchRVs = append(watchRVs, rv)
			mu.Unlock()
		}
		if n == 1 {
			return true, fw1, nil
		}
		return true, watch.NewFake(), nil
	})

	a := newTestAgent(t, client)
	cfg := watchBackoffConfig{minBackoff: 5 * time.Millisecond, maxBackoff: 20 * time.Millisecond, minHealthyDuration: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { a.watchPVsWithBackoff(ctx, cfg); close(done) }()

	waitFor(t, time.Second, func() bool { return attempt.Load() >= 1 })

	fw1.Error(&metav1.Status{
		Reason:  metav1.StatusReasonExpired,
		Code:    410,
		Message: "too old resource version",
	})
	fw1.Stop()

	waitFor(t, time.Second, func() bool { return attempt.Load() >= 2 })

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("watchPVsWithBackoff did not stop")
	}

	if listCalls.Load() != 2 {
		t.Fatalf("expected 2 List calls (initial + one forced by the Gone/Expired error), got %d", listCalls.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(watchRVs) < 2 {
		t.Fatalf("expected at least 2 watch attempts, got %d", len(watchRVs))
	}
	if watchRVs[0] != "1000" || watchRVs[1] != "2000" {
		t.Errorf("watch resourceVersions = %v, want [1000 2000] -- the Gone/Expired error should have forced a fresh List rather than reusing the stale resourceVersion", watchRVs)
	}
}

// TestWatchPVsNonExpiredErrorEventPreservesResourceVersion is the negative
// counterpart to TestWatchPVsResourceVersionGoneTriggersFreshList: a
// watch.Error that is NOT Gone/Expired (an ordinary transient failure, here
// StatusReasonInternalError) must be logged and the connection kept alive
// -- it must not clear the tracked resourceVersion or force a re-List, and
// a real event delivered afterward on the same connection must still be
// tracked normally.
func TestWatchPVsNonExpiredErrorEventPreservesResourceVersion(t *testing.T) {
	client := fake.NewSimpleClientset()

	var listCalls atomic.Int32
	client.PrependReactor("list", "persistentvolumes", func(action ktesting.Action) (bool, runtime.Object, error) {
		listCalls.Add(1)
		return true, &v1.PersistentVolumeList{ListMeta: metav1.ListMeta{ResourceVersion: "1000"}}, nil
	})

	var mu sync.Mutex
	var watchRVs []string
	var attempt atomic.Int32
	fw1 := watch.NewFake()
	client.PrependWatchReactor("persistentvolumes", func(action ktesting.Action) (bool, watch.Interface, error) {
		n := attempt.Add(1)
		if rv, ok := watchResourceVersionOf(action); ok {
			mu.Lock()
			watchRVs = append(watchRVs, rv)
			mu.Unlock()
		}
		if n == 1 {
			return true, fw1, nil
		}
		return true, watch.NewFake(), nil
	})

	a := newTestAgent(t, client)
	cfg := watchBackoffConfig{minBackoff: 5 * time.Millisecond, maxBackoff: 20 * time.Millisecond, minHealthyDuration: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { a.watchPVsWithBackoff(ctx, cfg); close(done) }()

	waitFor(t, time.Second, func() bool { return attempt.Load() >= 1 })

	// A transient, non-expiry error: must not clear lastResourceVersion or
	// end the connection.
	fw1.Error(&metav1.Status{
		Reason:  metav1.StatusReasonInternalError,
		Code:    500,
		Message: "transient failure",
	})

	// The connection must still be alive: deliver a real event afterward
	// and confirm it's still tracked normally.
	pv := newBoundPV("pv-transient", "/exports/pvc-transient", 1)
	pv.ResourceVersion = "1077"
	fw1.Add(pv)
	fw1.Stop()

	waitFor(t, time.Second, func() bool { return attempt.Load() >= 2 })

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("watchPVsWithBackoff did not stop")
	}

	if listCalls.Load() != 1 {
		t.Fatalf("expected exactly 1 List call -- a non-Gone/Expired error must not force a re-List, got %d", listCalls.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(watchRVs) < 2 {
		t.Fatalf("expected at least 2 watch attempts, got %d: %v", len(watchRVs), watchRVs)
	}
	if watchRVs[1] != "1077" {
		t.Errorf("second watch resourceVersion = %q, want %q -- the transient error should not have cleared the position tracked from the event delivered after it", watchRVs[1], "1077")
	}
}

// TestWatchPVsBookmarkUpdatesResourceVersionWithoutQuotaMutation guards
// #12's `BOOKMARK`/`ERROR` event handling: a Bookmark carries a minimal
// object of the watched type (only resourceVersion populated) and exists
// purely to advance the client's resume position -- it must update
// resourceVersion tracking without going anywhere near quota mutation.
func TestWatchPVsBookmarkUpdatesResourceVersionWithoutQuotaMutation(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())
	client := fake.NewSimpleClientset()

	var mu sync.Mutex
	var watchRVs []string
	var attempt atomic.Int32
	fw1 := watch.NewFake()
	client.PrependWatchReactor("persistentvolumes", func(action ktesting.Action) (bool, watch.Interface, error) {
		n := attempt.Add(1)
		if rv, ok := watchResourceVersionOf(action); ok {
			mu.Lock()
			watchRVs = append(watchRVs, rv)
			mu.Unlock()
		}
		if n == 1 {
			return true, fw1, nil
		}
		return true, watch.NewFake(), nil
	})

	a := newTestAgent(t, client)
	a.nfsServerPath = "/exports"
	a.fsType = quota.FSTypeXFS
	a.processAllNFS = true

	cfg := watchBackoffConfig{minBackoff: 5 * time.Millisecond, maxBackoff: 20 * time.Millisecond, minHealthyDuration: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { a.watchPVsWithBackoff(ctx, cfg); close(done) }()

	waitFor(t, time.Second, func() bool { return attempt.Load() >= 1 })

	bookmark := &v1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "5000"}}
	fw1.Action(watch.Bookmark, bookmark)
	fw1.Stop()

	waitFor(t, time.Second, func() bool { return attempt.Load() >= 2 })

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("watchPVsWithBackoff did not stop")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(watchRVs) < 2 || watchRVs[1] != "5000" {
		t.Errorf("expected the reconnect to resume from the Bookmark's resourceVersion 5000, got %v", watchRVs)
	}

	a.mu.Lock()
	appliedCount := len(a.appliedQuotas)
	a.mu.Unlock()
	if appliedCount != 0 {
		t.Errorf("Bookmark event must not trigger quota application, but appliedQuotas has %d entries", appliedCount)
	}
}
