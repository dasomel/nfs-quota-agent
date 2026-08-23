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
	"log/slog"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

const (
	defaultMinBackoff = 1 * time.Second
	defaultMaxBackoff = 60 * time.Second
	// defaultMinHealthyDuration is how long a watch must stay up before a
	// disconnect is treated as "was working" rather than "never really
	// connected". See the comment in watchPVsWithBackoff for why this,
	// rather than "received at least one event", is the reset signal.
	defaultMinHealthyDuration = 30 * time.Second
)

// watchBackoffConfig holds the reconnect-backoff timings for watchPVs. Zero
// fields default to the package constants above (see withDefaults) — this
// gives production callers a zero-value config while letting tests inject
// short timings. It is a parameter rather than fields on QuotaAgent because
// QuotaAgent (agent.go) is off limits for this change; a defaulted-zero
// config parameter gets the same test seam without touching that struct.
type watchBackoffConfig struct {
	minBackoff         time.Duration
	maxBackoff         time.Duration
	minHealthyDuration time.Duration
}

func (c watchBackoffConfig) withDefaults() watchBackoffConfig {
	if c.minBackoff <= 0 {
		c.minBackoff = defaultMinBackoff
	}
	if c.maxBackoff <= 0 {
		c.maxBackoff = defaultMaxBackoff
	}
	if c.minHealthyDuration <= 0 {
		c.minHealthyDuration = defaultMinHealthyDuration
	}
	return c
}

// watchPVs watches for PV changes with exponential backoff on reconnect.
func (a *QuotaAgent) watchPVs(ctx context.Context) {
	a.watchPVsWithBackoff(ctx, watchBackoffConfig{})
}

// watchPVsWithBackoff is watchPVs with injectable backoff timings, so tests
// can exercise backoff growth and reset without waiting out real,
// minute-scale delays.
func (a *QuotaAgent) watchPVsWithBackoff(ctx context.Context, cfg watchBackoffConfig) {
	cfg = cfg.withDefaults()
	backoff := cfg.minBackoff

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		watcher, err := a.client.CoreV1().PersistentVolumes().Watch(ctx, metav1.ListOptions{})
		if err != nil {
			slog.Error("Failed to start PV watch", "error", err, "retryIn", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, cfg.maxBackoff)
			continue
		}

		// Only trust a connection, and reset the backoff, once it has stayed
		// up for minHealthyDuration. The old behavior reset on Watch()
		// merely returning without error, which treats "connects, then dies
		// instantly" as success — turning a persistent mid-stream failure
		// (auth revoked, an API server that keeps rejecting the stream) into
		// an unthrottled reconnect once per minBackoff, forever.
		//
		// "Reset after at least one event" was considered and rejected: a
		// healthy watch against a quiescent cluster (no PV churn) can
		// legitimately see zero events for a long time, and that watch is
		// still healthy — it must not be penalized with growing backoff for
		// having nothing to report.
		connectedAt := time.Now()
		healthy := false
		healthyTimer := time.NewTimer(cfg.minHealthyDuration)

	eventLoop:
		for {
			select {
			case <-ctx.Done():
				healthyTimer.Stop()
				watcher.Stop()
				return
			case <-healthyTimer.C:
				healthy = true
				backoff = cfg.minBackoff
			case event, ok := <-watcher.ResultChan():
				if !ok {
					break eventLoop
				}

				if event.Type == watch.Error {
					// watch.Error events carry a *metav1.Status describing
					// why the stream is ending, not a PV — the type
					// assertion below would just drop it silently. Log it
					// so a persistently rejected stream is diagnosable
					// instead of showing up only as a 1/s "restarting" log.
					if status, ok := event.Object.(*metav1.Status); ok {
						slog.Error("PV watch received error event", "message", status.Message, "reason", status.Reason)
					} else {
						slog.Error("PV watch received error event", "object", event.Object)
					}
					continue
				}

				pv, ok := event.Object.(*v1.PersistentVolume)
				if !ok {
					continue
				}

				switch event.Type {
				case watch.Added, watch.Modified:
					if a.shouldProcessPV(pv) {
						// Resolve QuotaPolicy against the last sync cycle's
						// cached policy set (see resolveFromSnapshot in
						// policy.go) rather than passing 0 unconditionally:
						// ensureQuota's own quota-status annotation write
						// generates a Modified event for the very PV it just
						// enforced a quota on, and blindly reapplying raw
						// capacity here would immediately undo a QuotaPolicy
						// clamp the last sync applied.
						effectiveBytes := a.resolveFromSnapshot(pv)
						if err := a.ensureQuota(ctx, pv, effectiveBytes); err != nil {
							slog.Error("Failed to ensure quota", "pv", pv.Name, "error", err)
						}
					}
				case watch.Deleted:
					a.mu.Lock()
					nfsPath := a.getNFSPath(pv)
					if nfsPath != "" {
						localPath := a.nfsPathToLocal(nfsPath)
						delete(a.appliedQuotas, localPath)
					}
					a.mu.Unlock()
					slog.Debug("PV deleted, quota tracking removed", "pv", pv.Name)
				}
			}
		}
		healthyTimer.Stop()

		if !healthy {
			backoff = min(backoff*2, cfg.maxBackoff)
		}

		slog.Warn("PV watch ended, restarting...", "retryIn", backoff, "connectedFor", time.Since(connectedAt))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}
