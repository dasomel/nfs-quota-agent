//go:build livecluster

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
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// Runs against a REAL API server (build tag `livecluster`, KUBECONTEXT env).
// The fake clientset's List/Watch resourceVersion handling is
// self-consistent by construction (it's the same in-memory tracker on both
// sides), which cannot prove a real etcd-backed API server actually accepts
// List(...).ResourceVersion fed straight back into Watch(ResourceVersion:
// ..., AllowWatchBookmarks: true) and delivers events from that point
// forward -- that failure mode (a real server rejecting the request shape,
// synchronously or via a watch.Error) only appears here.
//
// This does NOT force a reconnect or a 410 Gone against the live server --
// simulating either reliably needs control this test doesn't have over a
// real API server's connection lifecycle. Those specific behaviors (resume
// after reconnect, fresh List after Gone/Expired) are covered by the fake
// clientset-driven tests in watch_test.go, which control both precisely.
func TestLive_ListThenWatchResourceVersionIsAcceptedByAPIServer(t *testing.T) {
	ctxName := os.Getenv("KUBECONTEXT")
	if ctxName == "" {
		t.Skip("set KUBECONTEXT")
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{CurrentContext: ctxName}).ClientConfig()
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}

	a := NewQuotaAgent(client, t.TempDir(), "/exports", "example.com/nfs-livecluster-test")
	// processAllNFS=true so shouldProcessPV accepts the native-NFS test PV
	// below and the watch handler actually calls ensureQuota -- which then
	// takes the safe "Directory does not exist, skipping quota" early
	// return (nfsBasePath is a fresh t.TempDir() with nothing under it, and
	// no filesystem quota command runs on that path), giving this test a
	// real, PV-name-bearing log line to assert on without needing an actual
	// quota-capable filesystem.
	a.SetProcessAllNFS(true)

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	cfg2 := watchBackoffConfig{minBackoff: 200 * time.Millisecond, maxBackoff: 2 * time.Second, minHealthyDuration: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() { a.watchPVsWithBackoff(ctx, cfg2); close(done) }()

	// Give the initial List+Watch a moment to establish before creating a PV.
	time.Sleep(2 * time.Second)

	pvName := fmt.Sprintf("nfs-quota-agent-livecluster-watch-test-%d", time.Now().UnixNano())
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvName},
		Spec: v1.PersistentVolumeSpec{
			Capacity:                      v1.ResourceList{v1.ResourceStorage: *resource.NewQuantity(1024*1024*1024, resource.BinarySI)},
			AccessModes:                   []v1.PersistentVolumeAccessMode{v1.ReadWriteMany},
			PersistentVolumeSource:        v1.PersistentVolumeSource{NFS: &v1.NFSVolumeSource{Server: "nfs.example.invalid", Path: "/exports/livecluster-watch-test"}},
			PersistentVolumeReclaimPolicy: v1.PersistentVolumeReclaimRetain,
		},
	}
	created, err := client.CoreV1().PersistentVolumes().Create(context.Background(), pv, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create test PV: %v", err)
	}
	t.Cleanup(func() {
		_ = client.CoreV1().PersistentVolumes().Delete(context.Background(), pvName, metav1.DeleteOptions{})
	})
	t.Logf("created PV %s at resourceVersion=%s", pvName, created.ResourceVersion)

	// shouldProcessPV requires Phase == Bound; a PV with no PVC claiming it
	// never reaches that on its own, so patch it via the status subresource
	// to reach the ensureQuota codepath this test wants to observe in logs.
	// This cluster runs real controllers that keep touching the object
	// right after creation (a PV-protection finalizer, in practice), so a
	// single re-Get can still lose the race -- retry with a fresh Get each
	// time, same shape as quotapolicy.WriteStatus's own conflict retry.
	var statusErr error
	for attempt := 0; attempt < 5; attempt++ {
		latest, err := client.CoreV1().PersistentVolumes().Get(context.Background(), pvName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("re-get test PV before status update: %v", err)
		}
		latest.Status.Phase = v1.VolumeBound
		_, statusErr = client.CoreV1().PersistentVolumes().UpdateStatus(context.Background(), latest, metav1.UpdateOptions{})
		if statusErr == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if statusErr != nil {
		t.Fatalf("mark test PV Bound after retries: %v", statusErr)
	}

	// Let the watch observe the Added event (and update its tracked
	// resourceVersion from it), then let the context timeout end the loop.
	time.Sleep(3 * time.Second)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("watchPVsWithBackoff did not stop after context cancellation")
	}

	logs := logBuf.String()
	t.Logf("captured logs:\n%s", logs)
	if strings.Contains(logs, "Failed to list PVs before starting watch") || strings.Contains(logs, "Failed to start PV watch") {
		t.Errorf("the real API server rejected the List-then-Watch(resourceVersion=..., AllowWatchBookmarks=true) call the fake clientset accepts unconditionally -- see the captured logs above")
	}
	if !strings.Contains(logs, "pv="+pvName) {
		t.Errorf("expected the watch to have observed the created PV %s (ensureQuota's \"Directory does not exist\" log line), found no reference to it in the captured logs", pvName)
	}
}
