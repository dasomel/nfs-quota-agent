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

package metrics

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Render outside the 30s filesystem-metrics cache so short bursts have fresh
// before/after snapshots without extra filesystem reports or a.mu contention.
func (c *Collector) phaseMetrics() string {
	var sb strings.Builder
	sb.WriteString(reconcilePhaseHeader)
	buckets, counts, sums, totals := c.agent.ReconcilePhaseHistogram()
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		var cumulative int64
		for i, le := range buckets {
			cumulative += counts[name][i]
			fmt.Fprintf(&sb, "nfs_quota_agent_reconcile_phase_duration_seconds_bucket{phase=%q,le=%q} %d\n", name, strconv.FormatFloat(le, 'g', -1, 64), cumulative)
		}
		fmt.Fprintf(&sb, "nfs_quota_agent_reconcile_phase_duration_seconds_bucket{phase=%q,le=\"+Inf\"} %d\n", name, totals[name])
		fmt.Fprintf(&sb, "nfs_quota_agent_reconcile_phase_duration_seconds_sum{phase=%q} %.9f\n", name, sums[name])
		fmt.Fprintf(&sb, "nfs_quota_agent_reconcile_phase_duration_seconds_count{phase=%q} %d\n", name, totals[name])
	}
	return sb.String()
}
