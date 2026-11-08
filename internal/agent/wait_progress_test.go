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
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTB captures Fatalf/Fatal calls from helpers under test and terminates
// the calling goroutine via runtime.Goexit, mirroring testing.common.FailNow.
type fakeTB struct {
	testing.TB
	mu     sync.Mutex
	failed bool
	msg    string
}

func (f *fakeTB) Helper() {}

func (f *fakeTB) Fatalf(format string, args ...any) {
	f.mu.Lock()
	f.failed = true
	f.msg = fmt.Sprintf(format, args...)
	f.mu.Unlock()
	runtime.Goexit()
}

func (f *fakeTB) Fatal(args ...any) {
	f.mu.Lock()
	f.failed = true
	f.msg = fmt.Sprint(args...)
	f.mu.Unlock()
	runtime.Goexit()
}

func runWithFakeTB(fn func(tb testing.TB)) (failed bool, msg string) {
	ftb := &fakeTB{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(ftb)
	}()
	<-done
	ftb.mu.Lock()
	defer ftb.mu.Unlock()
	return ftb.failed, ftb.msg
}

func TestWaitForProgress(t *testing.T) {
	t.Run("completes_when_done", func(t *testing.T) {
		var cur atomic.Int32
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			for i := int32(1); i <= 3; i++ {
				select {
				case <-stop:
					return
				case <-time.After(3 * time.Millisecond):
					cur.Store(i)
				}
			}
		}()

		failed, msg := runWithFakeTB(func(tb testing.TB) {
			waitForProgress(tb, 200*time.Millisecond, time.Second, func() int {
				return int(cur.Load())
			}, func(n int) bool {
				return n >= 3
			})
		})
		if failed {
			t.Fatalf("unexpected failure: %s", msg)
		}
		if got := cur.Load(); got < 3 {
			t.Fatalf("returned before done condition: got %d, want >= 3", got)
		}
	})

	t.Run("tolerates_slow_progress_past_stall_window", func(t *testing.T) {
		const (
			stall = 40 * time.Millisecond
			step  = 10 * time.Millisecond
			steps = 10 // ~100ms total > 2x stall window
		)
		begin := time.Now()
		failed, msg := runWithFakeTB(func(tb testing.TB) {
			waitForProgress(tb, stall, 2*time.Second, func() int {
				return int(time.Since(begin) / step)
			}, func(n int) bool {
				return n >= steps
			})
		})
		elapsed := time.Since(begin)
		if failed {
			t.Fatalf("unexpected failure on slow-but-progressing work: %s", msg)
		}
		if elapsed <= stall {
			t.Fatalf("expected work duration (%s) to exceed stall window (%s)", elapsed, stall)
		}
	})

	t.Run("fails_on_stall", func(t *testing.T) {
		const stall = 40 * time.Millisecond
		failed, msg := runWithFakeTB(func(tb testing.TB) {
			waitForProgress(tb, stall, 2*time.Second, func() int {
				return 5
			}, func(n int) bool {
				return n >= 10
			})
		})
		if !failed {
			t.Fatalf("expected waitForProgress to fail on stall, but it succeeded")
		}
		if !strings.Contains(msg, "no progress") {
			t.Fatalf("expected stall failure message containing %q, got %q", "no progress", msg)
		}
	})

	t.Run("fails_at_absolute_cap_while_progressing", func(t *testing.T) {
		const (
			stall = 500 * time.Millisecond
			cap   = 50 * time.Millisecond
		)
		var ticks atomic.Int32
		failed, msg := runWithFakeTB(func(tb testing.TB) {
			waitForProgress(tb, stall, cap, func() int {
				return int(ticks.Add(1))
			}, func(int) bool {
				return false
			})
		})
		if !failed {
			t.Fatalf("expected waitForProgress to fail at absolute cap, but it succeeded")
		}
		if !strings.Contains(msg, "absolute cap") {
			t.Fatalf("expected cap failure message containing %q, got %q", "absolute cap", msg)
		}
	})
}
