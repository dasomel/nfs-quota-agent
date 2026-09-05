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

import "sync"

// reconcileBackoffBuckets are nfs_quota_agent_reconcile_backoff_seconds'
// fixed upper bounds (seconds), spanning the reconcile queue's 5ms floor to
// its defaultMaxRetryDelay (30s) ceiling (reconcile_queue.go) with roughly
// doubling resolution -- see
// docs/adr/0002-kubernetes-events-and-retry-metrics.md's retry-metrics
// section. Fixed and unlabeled by PV, so this can never become a
// cardinality problem regardless of PV count or churn.
var reconcileBackoffBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30,
}

// retryBackoffHistogram accumulates observations of the rate limiter's
// computed backoff delay for nfs_quota_agent_reconcile_backoff_seconds.
// Hand-rolled rather than a Prometheus client-library histogram because
// internal/metrics/metrics.go builds its whole exposition text by hand
// already (no client library dependency exists in this codebase) --
// see metrics.go's Collector.updateMetrics for the rendering side.
type retryBackoffHistogram struct {
	mu     sync.Mutex
	counts []int64 // per-bucket (NOT cumulative) counts, same order as reconcileBackoffBuckets
	sum    float64
	count  int64
}

// observe records one backoff delay (seconds) into the histogram.
func (h *retryBackoffHistogram) observe(seconds float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.counts == nil {
		h.counts = make([]int64, len(reconcileBackoffBuckets))
	}
	for i, le := range reconcileBackoffBuckets {
		if seconds <= le {
			h.counts[i]++
			break
		}
	}
	// A seconds value above the last bucket bound falls through without
	// incrementing any per-bucket counter -- it still counts toward the
	// +Inf bucket, which metrics.go renders as the running total (sum,
	// not cumulative-bucket, count) rather than a tracked bucket here.
	// The queue's own ceiling (defaultMaxRetryDelay) equals the last
	// bucket bound today, so this is defense against that constant
	// changing later without this bucket list following it, not an
	// expected path.
	h.sum += seconds
	h.count++
}

// snapshot returns the fixed bucket upper bounds, their current per-bucket
// (not cumulative) observation counts, the running sum of observed
// seconds, and the total observation count -- read by
// QuotaAgent.ReconcileBackoffHistogram (metrics.AgentInfo).
func (h *retryBackoffHistogram) snapshot() (buckets []float64, counts []int64, sum float64, count int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	buckets = append([]float64(nil), reconcileBackoffBuckets...)
	if h.counts == nil {
		counts = make([]int64, len(reconcileBackoffBuckets))
	} else {
		counts = append([]int64(nil), h.counts...)
	}
	return buckets, counts, h.sum, h.count
}
