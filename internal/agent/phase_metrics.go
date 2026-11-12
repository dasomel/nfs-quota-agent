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
	"sync"
	"time"
)

const (
	phaseLockWait = iota
	phaseApply
	phaseVerify
	phaseStatusUpdate
	phaseOther
	phaseCount
)

var phaseNames = [phaseCount]string{"lock_wait", "apply", "verify", "status_update", "other"}
var phaseBuckets = [...]float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5}

type reconcilePhasesKey struct{}
type reconcilePhases struct{ durations [phaseCount]time.Duration }

func (p *reconcilePhases) start() time.Time {
	if p == nil {
		return time.Time{}
	}
	return time.Now()
}

func (p *reconcilePhases) end(phase int, start time.Time) {
	if p != nil {
		p.durations[phase] += time.Since(start)
	}
}

func (p *reconcilePhases) finishLocked(start time.Time) {
	if p == nil {
		return
	}
	other := time.Since(start)
	for _, phase := range []int{phaseApply, phaseVerify, phaseStatusUpdate} {
		other -= p.durations[phase]
	}
	p.durations[phaseOther] = other
}

// D1: A fixed-label histogram exposes distributions and additive phase time.
// Cost: a few time.Now calls per watch reconcile and bounded counters.
// Escape hatch: none needed; the metric is passive. Skipped phases observe
// zero once per attempt, including failures/cache hits; full sync is excluded.
// Queue wait is omitted: the coalescing/retrying queue stores no enqueue time.
type phaseHistogram struct {
	mu     sync.Mutex
	counts [phaseCount][len(phaseBuckets)]int64
	sums   [phaseCount]float64
	total  int64
}

func (h *phaseHistogram) observe(p *reconcilePhases) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.total++
	for phase, duration := range p.durations {
		seconds := duration.Seconds()
		h.sums[phase] += seconds
		for i, le := range phaseBuckets {
			if seconds <= le {
				h.counts[phase][i]++
				break
			}
		}
	}
}

// ReconcilePhaseHistogram returns independent copies with noncumulative buckets.
// Its separate mutex never acquires a.mu, including during live metric scrapes.
func (a *QuotaAgent) ReconcilePhaseHistogram() (buckets []float64, counts map[string][]int64, sums map[string]float64, totals map[string]int64) {
	h := &a.reconcilePhases
	h.mu.Lock()
	defer h.mu.Unlock()
	buckets = append([]float64(nil), phaseBuckets[:]...)
	counts, sums, totals = make(map[string][]int64), make(map[string]float64), make(map[string]int64)
	for phase, name := range phaseNames {
		counts[name] = append([]int64(nil), h.counts[phase][:]...)
		sums[name], totals[name] = h.sums[phase], h.total
	}
	return
}
