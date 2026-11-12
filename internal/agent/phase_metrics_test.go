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
	"testing"

	"github.com/dasomel/nfs-quota-agent/internal/quota"
)

func TestWatchReconcilePhases(t *testing.T) {
	for _, outcome := range []string{"success", "apply_failure", "verify_failure", "early_failure", "cache_hit"} {
		t.Run(outcome, func(t *testing.T) {
			runner := xfsHappyRunner()
			if outcome == "verify_failure" {
				runner = failVerificationNTimesXFSRunner(1)
			}
			withFakeRunner(t, runner)
			a, pv, _ := ensureQuotaFixture(t, 1)
			a.fsType = quota.FSTypeXFS
			if outcome == "apply_failure" {
				a.fsType = "unsupported"
			}
			if outcome == "early_failure" {
				pv.Spec.Capacity = nil
			}
			if outcome == "cache_hit" {
				if err := a.ensureQuota(context.Background(), pv, 0); err != nil {
					t.Fatal(err)
				}
			}
			// Direct/full-sync calls must not contribute to watch-only phases.
			_, _, _, totals := a.ReconcilePhaseHistogram()
			if totals["apply"] != 0 {
				t.Fatal(totals)
			}
			q := newPVReconcileQueue(a, 1)
			defer q.queue.ShutDown()
			q.enqueue(pv, 0)
			key, _ := q.queue.Get()
			q.process(context.Background(), key)
			buckets, counts, sums, totals := a.ReconcilePhaseHistogram()
			want := []string{"lock_wait", "apply", "verify", "status_update", "other"}
			if len(totals) != len(want) {
				t.Fatal(totals)
			}
			for _, phase := range want {
				if totals[phase] != 1 {
					t.Errorf("%s count = %d, want 1", phase, totals[phase])
				}
				if sums[phase] < 0 {
					t.Errorf("%s negative sum", phase)
				}
				var n int64
				for _, count := range counts[phase] {
					n += count
				}
				if n != 1 {
					t.Errorf("%s buckets = %v", phase, counts[phase])
				}
			}
			if buckets[0] != .001 || buckets[len(buckets)-1] != 5 {
				t.Fatal(buckets)
			}
			if outcome == "success" && (sums["apply"] <= 0 || sums["verify"] <= 0 || sums["status_update"] <= 0) {
				t.Fatal(sums)
			}
			if outcome == "apply_failure" && sums["verify"] != 0 {
				t.Fatal(sums)
			}
			if outcome == "early_failure" || outcome == "cache_hit" {
				if sums["apply"] != 0 || sums["verify"] != 0 || sums["status_update"] != 0 {
					t.Fatal(sums)
				}
			}
			total, errors, _ := a.ReconcileStats()
			wantErrors := int64(0)
			if outcome == "apply_failure" || outcome == "verify_failure" || outcome == "early_failure" {
				wantErrors = 1
			}
			if total != 1 || errors != wantErrors {
				t.Fatalf("outcome total=%d errors=%d want errors=%d", total, errors, wantErrors)
			}
		})
	}
}
