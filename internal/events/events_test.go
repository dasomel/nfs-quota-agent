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
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func testPV(name string) *v1.PersistentVolume {
	return &v1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name)}}
}

func TestFake_DedupWithinWindow(t *testing.T) {
	f := NewFake(30 * time.Second)
	now := time.Unix(1000, 0)
	f.Now = func() time.Time { return now }

	pv := testPV("pv-a")
	f.Event(pv, TypeNormal, QuotaApplied, "applied %s", "10Gi")
	f.Event(pv, TypeNormal, QuotaApplied, "applied %s", "10Gi")

	if got := f.Count("pv-a", QuotaApplied); got != 1 {
		t.Fatalf("expected 1 event within dedup window, got %d", got)
	}
}

func TestFake_ReemitsAfterWindow(t *testing.T) {
	f := NewFake(30 * time.Second)
	now := time.Unix(1000, 0)
	f.Now = func() time.Time { return now }

	pv := testPV("pv-a")
	f.Event(pv, TypeNormal, QuotaApplied, "applied")
	now = now.Add(31 * time.Second)
	f.Event(pv, TypeNormal, QuotaApplied, "applied")

	if got := f.Count("pv-a", QuotaApplied); got != 2 {
		t.Fatalf("expected 2 events once the window elapsed, got %d", got)
	}
}

func TestFake_DifferentReasonsNotDeduped(t *testing.T) {
	f := NewFake(30 * time.Second)
	now := time.Unix(1000, 0)
	f.Now = func() time.Time { return now }

	pv := testPV("pv-a")
	f.Event(pv, TypeWarning, QuotaApplyFailed, "failed")
	f.Event(pv, TypeWarning, QuotaVerificationFailed, "verify failed")

	if len(f.Events) != 2 {
		t.Fatalf("expected 2 distinct-reason events, got %d", len(f.Events))
	}
}

func TestFake_DifferentPVsNotDeduped(t *testing.T) {
	f := NewFake(30 * time.Second)
	now := time.Unix(1000, 0)
	f.Now = func() time.Time { return now }

	f.Event(testPV("pv-a"), TypeNormal, QuotaApplied, "applied")
	f.Event(testPV("pv-b"), TypeNormal, QuotaApplied, "applied")

	if len(f.Events) != 2 {
		t.Fatalf("expected 2 distinct-PV events, got %d", len(f.Events))
	}
}

func TestFake_NilPVIsNoop(t *testing.T) {
	f := NewFake(30 * time.Second)
	f.Event(nil, TypeNormal, QuotaApplied, "applied")
	if len(f.Events) != 0 {
		t.Fatalf("expected nil PV to be a no-op, got %d events", len(f.Events))
	}
}

func TestNewNoop_NeverRecords(t *testing.T) {
	r := NewNoop()
	r.Event(testPV("pv-a"), TypeNormal, QuotaApplied, "applied")
	r.Shutdown()
	// No observable state to assert beyond "did not panic" -- NewNoop's
	// entire contract is that it discards everything.
}
