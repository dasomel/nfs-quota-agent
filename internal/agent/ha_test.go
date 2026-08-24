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
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dasomel/nfs-quota-agent/internal/apis/quota/v1alpha1"
	"github.com/dasomel/nfs-quota-agent/internal/quota"
	"github.com/dasomel/nfs-quota-agent/internal/quotapolicy"
	"github.com/dasomel/nfs-quota-agent/internal/ui"
)

func TestHAActive_DefaultsToActiveWhenUnconfigured(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	if !a.HAActive() {
		t.Error("expected HAActive to default to true when SetHAActiveFile was never called")
	}
}

func TestHAActive_StandbyWhenFileMissing(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	a.SetHAActiveFile(filepath.Join(t.TempDir(), "does-not-exist"))
	if a.HAActive() {
		t.Error("expected HAActive to report false when the configured active-file does not exist")
	}
}

func TestHAActive_ActiveWhenFileExists(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	dir := t.TempDir()
	activeFile := filepath.Join(dir, "active")
	if err := os.WriteFile(activeFile, nil, 0644); err != nil {
		t.Fatalf("write active file: %v", err)
	}
	a.SetHAActiveFile(activeFile)
	if !a.HAActive() {
		t.Error("expected HAActive to report true when the configured active-file exists")
	}
}

// TestEnsureQuota_SkipsMutationWhenStandby guards #11's core acceptance
// item: "standby agent는 ownership이 확인되기 전 quota mutation을 수행하지
// 않는다". No filesystem command may run and appliedQuotas must stay empty.
func TestEnsureQuota_SkipsMutationWhenStandby(t *testing.T) {
	runner := xfsHappyRunner()
	withFakeRunner(t, runner)

	a := newTestAgent(t, fake.NewSimpleClientset())
	a.fsType = quota.FSTypeXFS
	a.SetHAActiveFile(filepath.Join(t.TempDir(), "does-not-exist"))

	pv := newBoundPV("pv-standby", "/exports/pvc-standby", 1)
	localPath := a.nfsPathToLocal("/exports/pvc-standby")
	if err := os.MkdirAll(localPath, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	err := a.ensureQuota(context.Background(), pv, 0)
	if !errors.Is(err, ErrHAStandby) {
		t.Fatalf("expected ErrHAStandby, got: %v", err)
	}

	a.mu.Lock()
	_, applied := a.appliedQuotas[localPath]
	a.mu.Unlock()
	if applied {
		t.Error("expected no quota to be applied while standby")
	}

	runner.mu.Lock()
	calls := len(runner.calls)
	runner.mu.Unlock()
	if calls != 0 {
		t.Errorf("expected zero filesystem commands to run while standby, got %d", calls)
	}
}

func TestEnsureQuota_AppliesWhenActive(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())

	a := newTestAgent(t, fake.NewSimpleClientset())
	a.fsType = quota.FSTypeXFS
	activeFile := filepath.Join(t.TempDir(), "active")
	if err := os.WriteFile(activeFile, nil, 0644); err != nil {
		t.Fatalf("write active file: %v", err)
	}
	a.SetHAActiveFile(activeFile)

	pv := newBoundPV("pv-active", "/exports/pvc-active", 1)
	localPath := a.nfsPathToLocal("/exports/pvc-active")
	if err := os.MkdirAll(localPath, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := a.ensureQuota(context.Background(), pv, 0); err != nil {
		t.Fatalf("ensureQuota: %v", err)
	}

	a.mu.Lock()
	_, applied := a.appliedQuotas[localPath]
	a.mu.Unlock()
	if !applied {
		t.Error("expected quota to be applied while active")
	}
}

// TestRemoveOrphan_RefusesWhenStandby guards the same acceptance item for
// orphan cleanup: destructive removal is quota mutation too.
func TestRemoveOrphan_RefusesWhenStandby(t *testing.T) {
	runner := xfsHappyRunner()
	withFakeRunner(t, runner)

	a := newTestAgent(t, fake.NewSimpleClientset())
	a.fsType = quota.FSTypeXFS
	a.SetHAActiveFile(filepath.Join(t.TempDir(), "does-not-exist"))

	dir := filepath.Join(a.nfsBasePath, "to-remove")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	err := a.RemoveOrphan(ui.OrphanInfo{Path: dir, DirName: "to-remove"})
	if !errors.Is(err, ErrHAStandby) {
		t.Fatalf("expected ErrHAStandby, got %v", err)
	}
	if _, statErr := os.Stat(dir); statErr != nil {
		t.Errorf("expected the orphan directory to survive a standby RemoveOrphan call, got stat error: %v", statErr)
	}
	runner.mu.Lock()
	calls := len(runner.calls)
	runner.mu.Unlock()
	if calls != 0 {
		t.Errorf("expected zero filesystem commands to run while standby, got %d", calls)
	}
}

// TestCleanupOrphans_SkipsSilentlyWhenStandby guards cleanupOrphans's
// handling of ErrHAStandby specifically: it must not log a false "Removed
// orphan directory" success or audit-record one (see the errors.Is check
// in cleanupOrphans, orphan.go) -- observed here via the directory
// surviving and the orphan cache entry remaining tracked.
func TestCleanupOrphans_SkipsSilentlyWhenStandby(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())

	a := newTestAgent(t, fake.NewSimpleClientset())
	a.fsType = quota.FSTypeXFS
	a.SetHAActiveFile(filepath.Join(t.TempDir(), "does-not-exist"))
	a.orphanGracePeriod = 0 // eligible for deletion immediately
	a.cleanupDryRun = false // NewQuotaAgent defaults this true; standby must refuse for a real reason, not dry-run

	dir := filepath.Join(a.nfsBasePath, "orphan-dir", "sub")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	a.cleanupOrphans(context.Background())

	if _, statErr := os.Stat(dir); statErr != nil {
		t.Errorf("expected the orphan directory to survive cleanupOrphans while standby, got stat error: %v", statErr)
	}
}

// TestRunHAActivePolling_SignalsOnBecomingActive guards ha.go's fix for
// the concurrency finding an independent review surfaced: runHAActivePolling
// no longer calls syncAllQuotas itself (which raced it against Run()'s own
// ticker-driven call, on a second goroutine, corrupting knownProjectIDs/
// policySnapshot) -- it only ever sends a non-blocking signal on syncNow,
// which Run()'s single sync-loop goroutine is responsible for consuming.
// This test asserts exactly that contract: a signal arrives, nothing more.
//
// wasActive seeds false unconditionally (see runHAActivePolling's doc
// comment for why), so the active file already existing *before* the
// goroutine starts still produces a signal on the first tick -- no
// goroutine-scheduling race to work around here, unlike the old version of
// this test.
func TestRunHAActivePolling_SignalsOnBecomingActive(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	activeFile := filepath.Join(t.TempDir(), "active")
	if err := os.WriteFile(activeFile, nil, 0644); err != nil {
		t.Fatalf("write active file: %v", err)
	}
	a.SetHAActiveFile(activeFile)

	syncNow := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		a.runHAActivePolling(ctx, 5*time.Millisecond, syncNow)
		close(done)
	}()

	select {
	case <-syncNow:
	case <-time.After(2 * time.Second):
		t.Fatalf("expected a signal on syncNow for the standby(false)->active transition")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("runHAActivePolling did not stop after context cancellation")
	}
}

// TestRunHAActivePolling_ClearsAppliedQuotasOnBecomingStandby guards the
// fix for the finding that the failover trigger was a near no-op: without
// clearing appliedQuotas on the active->standby edge, ensureQuota's
// cache-hit shortcut would make the *next* active-triggered sync silently
// re-apply nothing for any PV whose capacity didn't change -- see
// runHAActivePolling's doc comment (ha.go).
func TestRunHAActivePolling_ClearsAppliedQuotasOnBecomingStandby(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	activeFile := filepath.Join(t.TempDir(), "active")
	if err := os.WriteFile(activeFile, nil, 0644); err != nil {
		t.Fatalf("write active file: %v", err)
	}
	a.SetHAActiveFile(activeFile)

	a.mu.Lock()
	a.appliedQuotas["/some/path"] = 123
	a.mu.Unlock()

	syncNow := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		a.runHAActivePolling(ctx, 5*time.Millisecond, syncNow)
		close(done)
	}()

	// Drain the initial become-active signal (wasActive seeds false, so
	// the file already being present produces one) before flipping to
	// standby, so the two transitions aren't confused with each other.
	select {
	case <-syncNow:
	case <-time.After(2 * time.Second):
		t.Fatalf("expected the initial become-active signal")
	}

	if err := os.Remove(activeFile); err != nil {
		t.Fatalf("remove active file: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return len(a.appliedQuotas) == 0
	})

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("runHAActivePolling did not stop after context cancellation")
	}
}

func TestRunHAActivePolling_StopsOnContextCancel(t *testing.T) {
	a := newTestAgent(t, fake.NewSimpleClientset())
	a.SetHAActiveFile(filepath.Join(t.TempDir(), "does-not-exist"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	syncNow := make(chan struct{}, 1)
	go func() {
		a.runHAActivePolling(ctx, 5*time.Millisecond, syncNow)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("runHAActivePolling did not stop after context cancellation")
	}
}

// TestAppliedMustNotCountClaimsWhenHAStandby is the HA-standby sibling of
// zz_f7_applied_lie_test.go's TestAppliedMustNotCountClaimsWhoseDirectoryIsMissing:
// an independent review found ensureQuota's original HA gate returned nil
// (not an error) on standby, which recordEnforcement (policy.go) treats
// exactly like a real, successful apply -- reporting a QuotaPolicy claim as
// Applied=True on a node that enforced nothing. ensureQuota now returns
// ErrHAStandby, a real error, specifically so this can't happen. Unlike the
// missing-directory sibling test, the PV's local directory DOES exist here
// (hasLocalDir is true, exercising syncAllQuotas's normal
// `case hasLocalDir:` branch, not the singleWriter/no-local-dir one).
func TestAppliedMustNotCountClaimsWhenHAStandby(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())

	pv := newBoundPV("pv-standby-policy", "/exports/pvc-standby-policy", 10)
	client := fake.NewSimpleClientset(pv)
	a := newTestAgent(t, client)
	a.fsType = quota.FSTypeXFS
	a.SetProcessAllNFS(true)
	a.SetHAActiveFile(filepath.Join(t.TempDir(), "does-not-exist")) // standby

	localPath := a.nfsPathToLocal("/exports/pvc-standby-policy")
	if err := os.MkdirAll(localPath, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	max := *resourceQuantity(5 * gib)
	policy := &v1alpha1.QuotaPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "quota.nfs.io/v1alpha1", Kind: "QuotaPolicy"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "p", Generation: 1},
		Spec: v1alpha1.QuotaPolicySpec{
			Selector: v1alpha1.QuotaPolicySelector{}, Priority: 100,
			MaxQuota: &max, EnforceMax: true,
		},
	}
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(policy)
	if err != nil {
		t.Fatal(err)
	}
	dc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schemaGVR]string{quotapolicy.GroupVersionResource: "QuotaPolicyList"},
		&unstructured.Unstructured{Object: u})

	a.SetDynamicClient(dc)
	a.SetQuotaPolicyEnabled(true)
	// Without this, finishQuotaPolicyCycle no-ops and WriteStatus never
	// runs -- same note as the sibling test in zz_f7_applied_lie_test.go.
	a.SetQuotaPolicySingleWriter(true)

	ctx := context.Background()
	if err := a.syncAllQuotas(ctx); err != nil {
		t.Fatalf("syncAllQuotas: %v", err)
	}

	got, err := dc.Resource(quotapolicy.GroupVersionResource).Namespace("default").
		Get(ctx, "p", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	status, _ := got.Object["status"].(map[string]interface{})
	applied := status["appliedClaims"]
	var appliedCond, failReason string
	if conds, ok := status["conditions"].([]interface{}); ok {
		for _, c := range conds {
			m := c.(map[string]interface{})
			if m["type"] == "Applied" {
				appliedCond = m["status"].(string)
			}
		}
	}
	if failing, ok := status["failingClaims"].([]interface{}); ok && len(failing) > 0 {
		if m, ok := failing[0].(map[string]interface{}); ok {
			failReason, _ = m["reason"].(string)
		}
	}

	a.mu.Lock()
	_, everApplied := a.appliedQuotas[localPath]
	a.mu.Unlock()

	t.Logf("quota actually applied to the filesystem: %v", everApplied)
	t.Logf("status.appliedClaims=%v  Applied=%s  failingClaims[0].reason=%s", applied, appliedCond, failReason)

	if everApplied {
		t.Fatal("test setup wrong: the quota was applied, so there is nothing to catch")
	}
	if fmtInt(applied) != 0 {
		t.Errorf("appliedClaims=%v but no quota was applied to any filesystem (this node is HA standby)", applied)
	}
	if appliedCond == "True" {
		t.Errorf("Applied=True while nothing was enforced -- this node is HA standby and refused all mutation")
	}
	if failReason != v1alpha1.ReasonHAStandby {
		t.Errorf("failingClaims[0].reason = %q, want %q", failReason, v1alpha1.ReasonHAStandby)
	}
}
