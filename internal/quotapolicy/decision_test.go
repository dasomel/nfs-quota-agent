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

package quotapolicy

import (
	"fmt"
	"testing"
)

// TestComputeDecisionID_DeterministicSameInputs guards #14's requirement that
// identical decision inputs yield the identical decision ID across retries and restarts.
func TestComputeDecisionID_DeterministicSameInputs(t *testing.T) {
	id1 := ComputeDecisionID("pv-test", "uid-1234", 1, string(BoundClampedToMax), 1073741824)
	id2 := ComputeDecisionID("pv-test", "uid-1234", 1, string(BoundClampedToMax), 1073741824)
	if id1 == "" {
		t.Fatalf("ComputeDecisionID returned empty string")
	}
	if id1 != id2 {
		t.Fatalf("expected identical decision ID for same inputs, got %q vs %q", id1, id2)
	}
	if len(id1) != 16 {
		t.Errorf("expected 16-hex-char short hash, got len=%d (%q)", len(id1), id1)
	}
}

// TestComputeDecisionID_ChangesOnFieldChanges guards #14's requirement that
// a decision ID changes when generation, outcome, bytes, PV, or policy UID changes.
func TestComputeDecisionID_ChangesOnFieldChanges(t *testing.T) {
	baseID := ComputeDecisionID("pv-test", "uid-1234", 1, string(BoundClampedToMax), 1073741824)

	cases := []struct {
		name       string
		pv         string
		uid        string
		gen        int64
		outcome    string
		bytes      int64
		shouldDiff bool
	}{
		{
			name:       "same inputs",
			pv:         "pv-test",
			uid:        "uid-1234",
			gen:        1,
			outcome:    string(BoundClampedToMax),
			bytes:      1073741824,
			shouldDiff: false,
		},
		{
			name:       "generation changed",
			pv:         "pv-test",
			uid:        "uid-1234",
			gen:        2,
			outcome:    string(BoundClampedToMax),
			bytes:      1073741824,
			shouldDiff: true,
		},
		{
			name:       "outcome changed",
			pv:         "pv-test",
			uid:        "uid-1234",
			gen:        1,
			outcome:    string(BoundUnchanged),
			bytes:      1073741824,
			shouldDiff: true,
		},
		{
			name:       "effective bytes changed",
			pv:         "pv-test",
			uid:        "uid-1234",
			gen:        1,
			outcome:    string(BoundClampedToMax),
			bytes:      2147483648,
			shouldDiff: true,
		},
		{
			name:       "pv name changed",
			pv:         "pv-other",
			uid:        "uid-1234",
			gen:        1,
			outcome:    string(BoundClampedToMax),
			bytes:      1073741824,
			shouldDiff: true,
		},
		{
			name:       "policy UID changed",
			pv:         "pv-test",
			uid:        "uid-5678",
			gen:        1,
			outcome:    string(BoundClampedToMax),
			bytes:      1073741824,
			shouldDiff: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeDecisionID(tc.pv, tc.uid, tc.gen, tc.outcome, tc.bytes)
			if tc.shouldDiff && got == baseID {
				t.Fatalf("expected different decision ID from base %q, but got identical", baseID)
			}
			if !tc.shouldDiff && got != baseID {
				t.Fatalf("expected same decision ID as base %q, got %q", baseID, got)
			}
		})
	}
}

// TestFormatPolicyDecision tests formatting the nfs.io/policy-decision annotation value.
func TestFormatPolicyDecision(t *testing.T) {
	got := FormatPolicyDecision("gold-policy", 3, string(BoundClampedToMax), "0123456789abcdef")
	want := fmt.Sprintf("gold-policy/3/%s/0123456789abcdef", BoundClampedToMax)
	if got != want {
		t.Errorf("FormatPolicyDecision = %q, want %q", got, want)
	}
}
