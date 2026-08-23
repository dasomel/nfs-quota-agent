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
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/dasomel/nfs-quota-agent/internal/apis/quota/v1alpha1"
	"github.com/dasomel/nfs-quota-agent/internal/quotapolicy"
)

const gib = int64(1024 * 1024 * 1024)

// Guards the interaction between the sync path and the watch path.
//
// ensureQuota writes an nfs.io/quota-status annotation onto the PV, which
// generates a Modified watch event for that same PV. If the watch path
// applies the PV's raw capacity instead of the QuotaPolicy-resolved bound,
// the agent overwrites its own clamp moments after making it -- and then
// the next sync clamps again, so the enforced quota oscillates forever and
// spends most of each interval UNCLAMPED. That is invisible to any test
// that exercises syncAllQuotas and the watch loop separately, which is why
// this one drives them in sequence.
//
// This drives the event through the real watchPVsWithBackoff loop (via a
// watch.FakeWatcher), not by calling resolveFromSnapshot/ensureQuota
// directly in the test body: an earlier version of this test called those
// two functions itself to mimic what watch.go's handler does, which meant
// it kept passing even when watch.go's actual call site was mutated back
// to the pre-fix hardcoded-0 call -- it was verifying its own inlined copy
// of the logic, not watch.go. Firing a Modified event at the fake watcher
// and reading the result back through a.appliedQuotas exercises the real
// code path.
func TestWatchEventMustNotUndoTheQuotaPolicyClamp(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())

	pv := newBoundPV("pv-clamped", "/exports/pvc-clamped", 100) // requests 100Gi
	client := fake.NewSimpleClientset(pv)
	a := newTestAgent(t, client)

	localPath := filepath.Join(a.nfsBasePath, "pvc-clamped")
	if err := os.MkdirAll(localPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// A namespace-wide policy clamping every claim in "default" to 5Gi.
	max := *resourceQuantity(5 * gib)
	policy := &v1alpha1.QuotaPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "quota.nfs.io/v1alpha1", Kind: "QuotaPolicy"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "clamp-all", Generation: 1},
		Spec: v1alpha1.QuotaPolicySpec{
			Selector:   v1alpha1.QuotaPolicySelector{},
			Priority:   100,
			MaxQuota:   &max,
			EnforceMax: true, // explicit: the Go zero value is false (advisory)
		},
	}
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(policy)
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	dc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schemaGVR]string{quotapolicy.GroupVersionResource: "QuotaPolicyList"},
		&unstructured.Unstructured{Object: u})

	a.SetDynamicClient(dc)
	a.SetQuotaPolicyEnabled(true)
	a.SetProcessAllNFS(true) // native-NFS PV with no provisioned-by annotation
	a.fsType = "xfs"

	ctx := context.Background()
	if err := a.syncAllQuotas(ctx); err != nil {
		t.Fatalf("syncAllQuotas: %v", err)
	}

	a.mu.Lock()
	afterSync := a.appliedQuotas[localPath]
	a.mu.Unlock()
	if afterSync != 5*gib {
		t.Fatalf("after sync, applied = %d, want %d (5Gi clamp); the policy was not applied at all",
			afterSync, 5*gib)
	}

	// Drive the watch path for real: a fake watcher wired to the same
	// client watchPVsWithBackoff calls Watch() on, so the Modified event
	// below goes through watch.go's actual Added/Modified case, including
	// its resolveFromSnapshot call -- exactly what ensureQuota's own
	// annotation write triggers in production.
	var reconnects int32
	fw := watch.NewFake()
	client.PrependWatchReactor("persistentvolumes", func(action ktesting.Action) (bool, watch.Interface, error) {
		atomic.AddInt32(&reconnects, 1)
		return true, fw, nil
	})

	watchCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := watchBackoffConfig{
		minBackoff:         5 * time.Millisecond,
		maxBackoff:         20 * time.Millisecond,
		minHealthyDuration: time.Hour, // irrelevant here; never reconnects on its own
	}
	done := make(chan struct{})
	go func() { a.watchPVsWithBackoff(watchCtx, cfg); close(done) }()

	waitFor(t, time.Second, func() bool { return atomic.LoadInt32(&reconnects) == 1 })

	livePV, err := client.CoreV1().PersistentVolumes().Get(ctx, "pv-clamped", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fw.Modify(livePV)
	// Stopping the watcher right after Modify closes its ResultChan, which
	// only unblocks watchPVsWithBackoff's select once it has returned to
	// the top of the loop -- i.e. once the Modified case above (resolve +
	// ensureQuota) has fully run. Waiting for the resulting reconnect is
	// therefore proof the event was completely processed, not a guess at
	// how long that takes.
	fw.Stop()
	waitFor(t, time.Second, func() bool { return atomic.LoadInt32(&reconnects) == 2 })

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("watchPVsWithBackoff did not stop after context cancellation")
	}

	a.mu.Lock()
	afterWatch := a.appliedQuotas[localPath]
	a.mu.Unlock()

	if afterWatch != 5*gib {
		t.Errorf("after a watch event the applied quota is %d, want %d (5Gi).\n"+
			"The watch path re-applied the PV's raw capacity and overwrote the "+
			"QuotaPolicy clamp. Every annotation write triggers this, so the "+
			"enforced quota oscillates and the policy is effectively unenforced.",
			afterWatch, 5*gib)
	}
}

type schemaGVR = schema.GroupVersionResource

func resourceQuantity(b int64) *resource.Quantity { return resource.NewQuantity(b, resource.BinarySI) }
