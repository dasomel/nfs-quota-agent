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
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/dasomel/nfs-quota-agent/internal/quota"
)

// TestWatchPVsBackoffGrowsWhenWatchDiesImmediately proves the fix for the
// hot-loop defect: a watch that establishes and then ends at once (the
// mid-stream-failure shape — authorization revoked, an API server
// persistently rejecting the stream) must not reset to minBackoff on every
// reconnect. Timings are injected via watchPVsWithBackoff so this asserts on
// the actual gap between Watch() calls, not just a call count, and finishes
// in well under a second.
func TestWatchPVsBackoffGrowsWhenWatchDiesImmediately(t *testing.T) {
	client := fake.NewSimpleClientset()

	var mu sync.Mutex
	var callTimes []time.Time
	client.PrependWatchReactor("persistentvolumes", func(action ktesting.Action) (bool, watch.Interface, error) {
		mu.Lock()
		callTimes = append(callTimes, time.Now())
		mu.Unlock()
		w := watch.NewFake()
		go w.Stop() // establishes fine, then ends immediately
		return true, w, nil
	})

	a := newTestAgent(t, client)
	cfg := watchBackoffConfig{
		minBackoff:         15 * time.Millisecond,
		maxBackoff:         200 * time.Millisecond,
		minHealthyDuration: time.Hour, // never reached: every connection dies instantly
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() { a.watchPVsWithBackoff(ctx, cfg); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("watchPVsWithBackoff did not stop after context deadline")
	}

	mu.Lock()
	times := append([]time.Time(nil), callTimes...)
	mu.Unlock()

	if len(times) < 3 {
		t.Fatalf("expected at least 3 reconnect attempts to observe growth, got %d", len(times))
	}
	// A hot loop reconnecting every minBackoff (15ms) would produce ~30
	// calls in 500ms. A backing-off loop (15, 30, 60, 120, 200, 200...)
	// produces far fewer.
	if len(times) > 12 {
		t.Errorf("reconnect did not back off: %d Watch() calls in 500ms of continuous immediate failure", len(times))
	}

	firstGap := times[1].Sub(times[0])
	lastGap := times[len(times)-1].Sub(times[len(times)-2])
	if lastGap < firstGap {
		t.Errorf("expected reconnect delay to grow, first gap=%s last gap=%s", firstGap, lastGap)
	}
}

// TestWatchPVsBackoffResetsAfterHealthyConnection proves the other half of
// the fix: a connection that stays up past minHealthyDuration is treated as
// healthy, so the *next* reconnect (after it eventually drops) uses
// minBackoff again rather than continuing to escalate.
func TestWatchPVsBackoffResetsAfterHealthyConnection(t *testing.T) {
	client := fake.NewSimpleClientset()

	var attempt int
	var mu sync.Mutex
	var callTimes []time.Time
	client.PrependWatchReactor("persistentvolumes", func(action ktesting.Action) (bool, watch.Interface, error) {
		mu.Lock()
		attempt++
		n := attempt
		callTimes = append(callTimes, time.Now())
		mu.Unlock()

		w := watch.NewFake()
		if n == 1 {
			// First connection dies immediately, forcing backoff to grow.
			go w.Stop()
		} else {
			// Second connection stays up past minHealthyDuration, then ends.
			go func() {
				time.Sleep(60 * time.Millisecond)
				w.Stop()
			}()
		}
		return true, w, nil
	})

	a := newTestAgent(t, client)
	cfg := watchBackoffConfig{
		minBackoff:         10 * time.Millisecond,
		maxBackoff:         500 * time.Millisecond,
		minHealthyDuration: 30 * time.Millisecond,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() { a.watchPVsWithBackoff(ctx, cfg); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("watchPVsWithBackoff did not stop after context deadline")
	}

	mu.Lock()
	times := append([]time.Time(nil), callTimes...)
	mu.Unlock()

	if len(times) < 3 {
		t.Fatalf("expected at least 3 connection attempts, got %d", len(times))
	}
	// Gap 1->2: after the immediate-death connection, backoff doubled to 20ms.
	gapAfterFailure := times[1].Sub(times[0])
	// Gap 2->3: connection 2 stayed healthy for 30ms+ before dropping, so
	// backoff should have been reset to minBackoff (10ms) rather than
	// continuing to double toward maxBackoff.
	gapAfterHealthy := times[2].Sub(times[1])

	// gapAfterHealthy includes the ~30ms the connection stayed up plus the
	// reset 10ms sleep; gapAfterFailure is just the ~20ms backoff sleep with
	// no connected time. Compare against the reconnect sleep only by
	// checking gapAfterHealthy is not escalating past what a reset implies:
	// it should be well under what continued doubling (40ms+ sleep on top
	// of the same connected time) would produce.
	if gapAfterHealthy > gapAfterFailure+cfg.minHealthyDuration+50*time.Millisecond {
		t.Errorf("backoff did not appear to reset after a healthy connection: gapAfterFailure=%s gapAfterHealthy=%s", gapAfterFailure, gapAfterHealthy)
	}
}

// TestWatchPVsLogsWatchErrorEvent proves watch.Error events are surfaced via
// slog instead of being silently dropped by the *v1.PersistentVolume type
// assertion (the second defect in the report).
func TestWatchPVsLogsWatchErrorEvent(t *testing.T) {
	client := fake.NewSimpleClientset()
	fw := watch.NewFake()
	client.PrependWatchReactor("persistentvolumes", func(action ktesting.Action) (bool, watch.Interface, error) {
		return true, fw, nil
	})

	a := newTestAgent(t, client)
	cfg := watchBackoffConfig{
		minBackoff:         10 * time.Millisecond,
		maxBackoff:         50 * time.Millisecond,
		minHealthyDuration: time.Hour,
	}

	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.watchPVsWithBackoff(ctx, cfg); close(done) }()

	fw.Error(&metav1.Status{Message: "watch closed by API server", Reason: metav1.StatusReasonExpired})

	waitFor(t, 2*time.Second, func() bool {
		return strings.Contains(buf.String(), "PV watch received error event")
	})

	out := buf.String()
	if !strings.Contains(out, "watch closed by API server") {
		t.Errorf("expected the Status message in the log, got: %s", out)
	}
	if !strings.Contains(out, "level=ERROR") {
		t.Errorf("expected the error event to be logged at Error level, got: %s", out)
	}

	cancel()
	fw.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("watchPVsWithBackoff did not stop after context cancellation")
	}
}

// TestWatchPVsAppliesQuotaOnAddedEventAfterRefactor guards against the
// backoff/error-handling refactor breaking the happy path: a normal Added
// event must still reach ensureQuota and get tracked.
func TestWatchPVsAppliesQuotaOnAddedEventAfterRefactor(t *testing.T) {
	withFakeRunner(t, xfsHappyRunner())

	client := fake.NewSimpleClientset()
	fw := watch.NewFake()
	client.PrependWatchReactor("persistentvolumes", func(action ktesting.Action) (bool, watch.Interface, error) {
		return true, fw, nil
	})

	a := newTestAgent(t, client)
	a.nfsServerPath = "/exports"
	a.fsType = quota.FSTypeXFS
	a.processAllNFS = true

	localPath := a.nfsPathToLocal("/exports/pvc-refactor")
	if err := os.MkdirAll(localPath, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cfg := watchBackoffConfig{minBackoff: 10 * time.Millisecond, maxBackoff: 50 * time.Millisecond, minHealthyDuration: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.watchPVsWithBackoff(ctx, cfg); close(done) }()

	fw.Add(newBoundPV("pv-refactor", "/exports/pvc-refactor", 1))

	waitFor(t, 2*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		_, ok := a.appliedQuotas[localPath]
		return ok
	})

	cancel()
	fw.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("watchPVsWithBackoff did not stop after context cancellation")
	}
}

// TestWatchPVsReconnectBackoffSleepRespectsContextCancellation proves ctx
// cancellation short-circuits the post-disconnect backoff sleep (as
// opposed to the connect-failure backoff sleep, already covered by
// TestWatchPVsRetriesOnWatchStartError in watch_test.go). minBackoff is set
// far longer than the test timeout, so a promptly-returning test proves the
// sleep was interrupted rather than waited out.
func TestWatchPVsReconnectBackoffSleepRespectsContextCancellation(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependWatchReactor("persistentvolumes", func(action ktesting.Action) (bool, watch.Interface, error) {
		w := watch.NewFake()
		go w.Stop() // dies immediately, forcing the post-disconnect backoff sleep
		return true, w, nil
	})

	a := newTestAgent(t, client)
	cfg := watchBackoffConfig{
		minBackoff:         5 * time.Second, // much longer than the ctx timeout below
		maxBackoff:         10 * time.Second,
		minHealthyDuration: time.Hour,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() { a.watchPVsWithBackoff(ctx, cfg); close(done) }()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatalf("watchPVsWithBackoff did not return promptly on context cancellation during the reconnect backoff sleep")
	}
}

// syncBuffer is a mutex-guarded bytes.Buffer. slog writes from the watch
// goroutine and String() reads from the test goroutine race on a bare
// bytes.Buffer under `go test -race` even though the actual log content is
// never wrong -- this just makes both sides use the same lock.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
