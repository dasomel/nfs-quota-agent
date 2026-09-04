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

package events

import (
	"fmt"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
)

// Recorded is one Event captured by Fake, message already formatted (the
// same formatting Recorder.Event's real implementation applies via
// Eventf), so tests can assert on the final text rather than re-deriving
// fmt.Sprintf's output from messageFmt/args themselves.
type Recorded struct {
	PVName    string
	EventType string
	Reason    Reason
	Message   string
}

// Fake is an in-memory Recorder for internal/agent's tests -- no
// EventBroadcaster, no API server, no goroutine. It intentionally applies
// the identical per-(pv, reason) dedup window the real recorder does, so a
// test exercising the dedup behavior itself (not just "was an Event
// emitted at all") gets the real contract, not a bypassed one.
//
// Now defaults to time.Now but is overridable per-instance so a test can
// advance time deterministically instead of sleeping for a real window
// (window is typically the agent's syncInterval, tens of seconds).
type Fake struct {
	window time.Duration
	Now    func() time.Time

	mu       sync.Mutex
	Events   []Recorded
	lastSeen map[string]time.Time
}

// NewFake returns a Fake Recorder deduplicating repeat (pv, reason) pairs
// within window, matching NewRecorder's real contract.
func NewFake(window time.Duration) *Fake {
	return &Fake{
		window:   window,
		Now:      time.Now,
		lastSeen: make(map[string]time.Time),
	}
}

func (f *Fake) Event(pv *v1.PersistentVolume, eventType string, reason Reason, messageFmt string, args ...interface{}) {
	if pv == nil {
		return
	}
	key := pv.Name + "/" + string(reason)
	now := f.Now()

	f.mu.Lock()
	defer f.mu.Unlock()
	if last, ok := f.lastSeen[key]; ok && now.Sub(last) < f.window {
		return
	}
	f.lastSeen[key] = now
	f.Events = append(f.Events, Recorded{
		PVName:    pv.Name,
		EventType: eventType,
		Reason:    reason,
		Message:   fmt.Sprintf(messageFmt, args...),
	})
}

// Forget drops every dedup entry recorded for pvName, matching the real
// recorder's Forget contract -- note this does NOT remove pvName's past
// entries from f.Events (that history stays for assertions); it only
// clears lastSeen so a later Event call for pvName isn't deduped against
// pre-Forget timestamps.
func (f *Fake) Forget(pvName string) {
	prefix := pvName + "/"
	f.mu.Lock()
	defer f.mu.Unlock()
	for key := range f.lastSeen {
		if strings.HasPrefix(key, prefix) {
			delete(f.lastSeen, key)
		}
	}
}

func (f *Fake) Shutdown() {}

// Count returns how many Events Fake actually recorded (post-dedup) for
// pv/reason -- a convenience for assertions like "exactly one
// QuotaApplied fired despite three reconciles inside the window."
func (f *Fake) Count(pvName string, reason Reason) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.Events {
		if e.PVName == pvName && e.Reason == reason {
			n++
		}
	}
	return n
}
