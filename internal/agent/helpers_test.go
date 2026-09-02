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
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// callsSnapshot returns a stable copy for assertions while queue workers may
// still be invoking Run. Tests must not read f.calls directly outside f.mu.
func (f *fakeRunner) callsSnapshot() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.calls...)
}

// withFakeRunner installs r as the quota package's CommandRunner for the
// duration of the test and restores the previous runner on cleanup.
func withFakeRunner(t *testing.T, r *fakeRunner) {
	t.Helper()
	restore := quota.SetCommandRunnerForTesting(r)
	t.Cleanup(restore)
}

// xfsQuotaState is a minimal in-memory stand-in for the kernel's XFS
// project quota table: it tracks each `limit -p bhard=...` call's project
// ID -> byte limit and answers a later `report -p -b` query from that
// state. Any fake xfs_quota runner that lets ensureQuota reach a successful
// apply needs this -- without it, the post-apply read-back verification
// (verifyQuotaOnDisk, #10) finds no matching project in the report and
// fails, since the report command must actually reflect what was applied
// rather than return a fixed string.
type xfsQuotaState struct {
	mu      sync.Mutex
	applied map[string]int64 // projectID string -> hard limit bytes
	// usedBytes overrides the report's "Used" column for every project this
	// state knows about -- 0 (the zero value) preserves the original
	// behavior. Tests exercising real usage (the shrink guard, #14) need
	// this: once a project has been through `limit -p`, the report always
	// has a matching entry, so GetDirUsages no longer falls back to a real
	// filepath.Walk of the test directory to determine usage -- the same
	// thing a real xfs_quota report's own Used column would supply in
	// production. All PVs/projects in one test share a single override
	// since no test here tracks more than one at a time.
	usedBytes int64
}

// setUsedBytes sets the value the report handler below reports as "Used"
// for every project it knows about.
func (s *xfsQuotaState) setUsedBytes(bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usedBytes = bytes
}

// handle answers a "-x -c <cmd> [path]" xfs_quota invocation if cmd is a
// `limit -p` or `report` call; ok is false for anything else (state/
// version checks), leaving the caller to answer those itself.
func (s *xfsQuotaState) handle(cmd string) (out []byte, ok bool) {
	switch {
	case strings.HasPrefix(cmd, "limit -p "):
		fields := strings.Fields(cmd)
		var projectID string
		var bhardBytes int64
		for _, f := range fields[2:] {
			if strings.HasPrefix(f, "bhard=") {
				if kb, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(f, "bhard="), "k"), 10, 64); err == nil {
					bhardBytes = kb * 1024
				}
			} else {
				projectID = f
			}
		}
		s.mu.Lock()
		if s.applied == nil {
			s.applied = map[string]int64{}
		}
		s.applied[projectID] = bhardBytes
		s.mu.Unlock()
		return []byte(""), true
	case strings.HasPrefix(cmd, "report"):
		s.mu.Lock()
		defer s.mu.Unlock()
		var sb strings.Builder
		sb.WriteString("Project ID   Used   Soft   Hard   Warn/Grace\n")
		for id, bytes := range s.applied {
			fmt.Fprintf(&sb, "#%s     %d      0      %d    00 [------]\n", id, s.usedBytes/1024, bytes/1024)
		}
		return []byte(sb.String()), true
	default:
		return nil, false
	}
}

// xfsHappyRunner returns a fakeRunner that answers every findmnt/xfs_quota
// call needed for a successful xfs detect+check+apply+verify flow.
func xfsHappyRunner() *fakeRunner {
	state := &xfsQuotaState{}
	return &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		switch name {
		case "findmnt":
			return []byte("xfs\n"), nil
		case "xfs_quota":
			if len(args) > 0 && args[0] == "-V" {
				return []byte("xfs_quota version 1.0"), nil
			}
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

// xfsHappyRunnerWithState is xfsHappyRunner but also returns the
// xfsQuotaState backing it, for tests that need to reach into that state
// directly -- e.g. via setUsedBytes, to simulate real on-disk usage the
// report should reflect (the shrink guard, #14).
func xfsHappyRunnerWithState() (*fakeRunner, *xfsQuotaState) {
	state := &xfsQuotaState{}
	runner := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		switch name {
		case "findmnt":
			return []byte("xfs\n"), nil
		case "xfs_quota":
			if len(args) > 0 && args[0] == "-V" {
				return []byte("xfs_quota version 1.0"), nil
			}
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
	return runner, state
}

// ext4QuotaState is xfsQuotaState's ext4 counterpart: a minimal in-memory
// stand-in for the kernel's ext4 project quota table, tracking each
// `setquota -P` call's project ID -> byte hard limit and answering a later
// `repquota -P` query from that state. Needed for the same reason
// xfsQuotaState is: the post-apply read-back verification (verifyQuotaOnDisk,
// #10) requires the report to actually reflect what setquota just "applied,"
// not a fixed canned string.
type ext4QuotaState struct {
	mu      sync.Mutex
	applied map[string]int64 // projectID string -> hard limit bytes
}

// handleSetquota records the hard limit `setquota -P <id> <soft> <hard> <isoft> <ihard> <path>`
// sets for id -- see ApplyExt4Quota's call in internal/quota/ext4.go for the
// exact argument order this mirrors.
func (s *ext4QuotaState) handleSetquota(args []string) ([]byte, error) {
	// args: ["-P", projectID, blockSoft, blockHard, inodeSoft, inodeHard, quotaPath]
	if len(args) < 4 {
		return nil, fmt.Errorf("unexpected setquota args: %v", args)
	}
	projectID := args[1]
	hardKB, err := strconv.ParseInt(args[3], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("unexpected setquota hard limit %q: %w", args[3], err)
	}
	s.mu.Lock()
	if s.applied == nil {
		s.applied = map[string]int64{}
	}
	s.applied[projectID] = hardKB * 1024
	s.mu.Unlock()
	return []byte(""), nil
}

// handleRepquota renders `repquota -P`'s stdout format well enough for
// parseExt4RepquotaOutput (internal/quota/report.go) to resolve each known
// project ID's hard limit: "#<id> -- <used> <soft> <hard> <inodeUsed> <inodeHard>".
func (s *ext4QuotaState) handleRepquota() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	var sb strings.Builder
	sb.WriteString("Project ID      used    soft    hard  grace    used  soft  hard  grace\n")
	sb.WriteString("----------------------------------------------------------------------\n")
	for id, bytes := range s.applied {
		fmt.Fprintf(&sb, "#%s      --       0       0    %d       0     0     0\n", id, bytes/1024)
	}
	return []byte(sb.String())
}

// ext4HappyRunner returns a fakeRunner that answers every chattr/setquota/
// repquota call needed for a successful ext4 apply+verify flow -- the ext4
// analog of xfsHappyRunner.
func ext4HappyRunner() *fakeRunner {
	state := &ext4QuotaState{}
	return &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		switch name {
		case "chattr":
			return []byte(""), nil
		case "setquota":
			return state.handleSetquota(args)
		case "repquota":
			return state.handleRepquota(), nil
		case "findmnt":
			return []byte("ext4\n"), nil
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
	// Constructor defaults stateDir to the real host path
	// /var/lib/nfs-quota-agent; point it at a temp dir instead so tests
	// never touch that path.
	a.SetStateDir(t.TempDir())
	return a
}

// writeEmptyProjectMappings gives tests that deliberately prime an otherwise
// empty filesystem the same readable mapping files a configured agent has on
// a real host. A missing mapping is a strict-report failure, not proof that
// no project quotas exist.
func writeEmptyProjectMappings(t *testing.T, a *QuotaAgent) {
	t.Helper()
	for _, path := range []string{a.projectsFile, a.projidFile} {
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatalf("write empty project mapping %s: %v", path, err)
		}
	}
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

// countReportCalls counts how many of calls are an xfs_quota report
// invocation ("xfs_quota -x -c 'report ...'"), as opposed to a project/limit
// mutation or a detect/version probe -- used by #92's tests to isolate the
// shrink/brownfield guard's usage-report fetches from the rest of a normal
// apply flow's runner traffic.
func countReportCalls(calls []fakeCall) int {
	n := 0
	for _, c := range calls {
		if c.name == "xfs_quota" && len(c.args) >= 3 && c.args[1] == "-c" && strings.HasPrefix(c.args[2], "report") {
			n++
		}
	}
	return n
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
