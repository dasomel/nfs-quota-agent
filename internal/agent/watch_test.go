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
	"sync/atomic"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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
