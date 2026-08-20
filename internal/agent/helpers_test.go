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
	"path/filepath"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dasomel/nfs-quota-agent/internal/quota"
)

// fakeCall records one invocation made through fakeRunner.
type fakeCall struct {
	name string
	args []string
}

// fakeRunner is a scriptable quota.CommandRunner used to avoid invoking real
// system binaries from agent tests. fn decides the response for each call;
// calls records every invocation for assertions on command construction.
type fakeRunner struct {
	mu    sync.Mutex
	fn    func(name string, args ...string) ([]byte, error)
	calls []fakeCall
}

func (f *fakeRunner) Run(name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{name: name, args: args})
	fn := f.fn
	f.mu.Unlock()
	if fn == nil {
		return nil, nil
	}
	return fn(name, args...)
}

// withFakeRunner installs r as the quota package's CommandRunner for the
// duration of the test and restores the previous runner on cleanup.
func withFakeRunner(t *testing.T, r *fakeRunner) {
	t.Helper()
	restore := quota.SetCommandRunnerForTesting(r)
	t.Cleanup(restore)
}

// xfsHappyRunner returns a fakeRunner that answers every findmnt/xfs_quota
// call needed for a successful xfs detect+check+apply flow.
func xfsHappyRunner() *fakeRunner {
	return &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		switch name {
		case "findmnt":
			return []byte("xfs\n"), nil
		case "xfs_quota":
			if len(args) > 0 && args[0] == "-V" {
				return []byte("xfs_quota version 1.0"), nil
			}
			return []byte("Project quota state: ON"), nil
		default:
			return []byte(""), nil
		}
	}}
}

// newTestAgent builds a QuotaAgent wired to a fake clientset and temp-file
// backed projects/projid files, ready for direct manipulation by tests
// (package-internal test, so unexported fields are reachable).
func newTestAgent(t *testing.T, client *fake.Clientset) *QuotaAgent {
	t.Helper()
	base := t.TempDir()
	a := NewQuotaAgent(client, base, "/exports", "example.com/nfs")
	a.SetProjectsFile(filepath.Join(base, "projects"))
	a.SetProjidFile(filepath.Join(base, "projid"))
	return a
}

func newBoundPV(name, nfsPath string, capacityGi int64) *v1.PersistentVolume {
	return &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1.PersistentVolumeSpec{
			Capacity: v1.ResourceList{
				v1.ResourceStorage: *resource.NewQuantity(capacityGi*1024*1024*1024, resource.BinarySI),
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				NFS: &v1.NFSVolumeSource{Server: "nfs.example.com", Path: nfsPath},
			},
			ClaimRef: &v1.ObjectReference{Namespace: "default", Name: name + "-claim"},
		},
		Status: v1.PersistentVolumeStatus{Phase: v1.VolumeBound},
	}
}

// waitFor polls cond until it returns true or timeout elapses, failing the
// test if the timeout is reached. Poll interval is capped well under 100ms
// per the no-flaky-sleep constraint.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s", timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
