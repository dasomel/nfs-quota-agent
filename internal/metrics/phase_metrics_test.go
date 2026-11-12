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
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

type livePhaseAgent struct {
	fakeAgent
	count int64
}

func (f *livePhaseAgent) ReconcilePhaseHistogram() ([]float64, map[string][]int64, map[string]float64, map[string]int64) {
	return []float64{.001, 5}, map[string][]int64{"apply": {0, f.count}}, map[string]float64{"apply": float64(f.count)}, map[string]int64{"apply": f.count}
}

func TestPhaseHistogramBypassesMetricsCache(t *testing.T) {
	agent := &livePhaseAgent{}
	c := &Collector{agent: agent, lastUpdate: time.Now(), metrics: "# cached filesystem metrics\n"}
	for _, count := range []int64{0, 1} {
		agent.count = count
		w := httptest.NewRecorder()
		c.handleMetrics(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		want := `nfs_quota_agent_reconcile_phase_duration_seconds_count{phase="apply"} ` + strconv.FormatInt(count, 10)
		if !strings.Contains(w.Body.String(), want) {
			t.Fatal(w.Body.String())
		}
		if !strings.HasPrefix(w.Body.String(), c.metrics) {
			t.Fatal("lost cached metrics")
		}
		if strings.Count(w.Body.String(), "# TYPE nfs_quota_agent_reconcile_phase_duration_seconds histogram") != 1 {
			t.Fatal(w.Body.String())
		}
	}
}
