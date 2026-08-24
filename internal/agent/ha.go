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
	"errors"
	"log/slog"
	"os"
	"time"
)

// haActivePollInterval is how often runHAActivePolling re-checks
// haActiveFile. Deliberately much shorter than syncInterval's default
// (30s): the periodic full resync already exists as a watch-gap backstop
// on that cadence, but a standby->active failover transition should
// trigger reconciliation promptly, not wait up to 30s for the next
// unrelated tick to notice it.
const haActivePollInterval = 2 * time.Second

// ErrHAStandby is returned by ensureQuota (agent.go) and RemoveOrphan
// (orphan.go) when this instance is HA standby (see haActiveFile's doc
// comment, #11) and refuses to perform quota mutation. Deliberately a real,
// non-nil error rather than a silent nil "skip" (the convention ensureQuota
// otherwise uses, e.g. for a PV whose local directory doesn't exist yet):
// nil would make syncAllQuotas's QuotaPolicy accounting
// (policy.go/recordEnforcement) treat a standby no-op as a successfully
// applied claim -- the exact "applied lie" class docs/quotapolicy-design.md
// §11 and zz_f7_applied_lie_test.go already exist to prevent for the
// missing-local-directory case (see errLocalDirectoryMissing, policy.go).
// Every caller that receives this must treat it as "correctly skipped, not
// a failure" for logging/retry/metrics purposes: see syncAllQuotas
// (agent.go), pvReconcileQueue.process (reconcile_queue.go), and
// cleanupOrphans (orphan.go) for the three places that do.
var ErrHAStandby = errors.New("this instance is HA standby; quota mutation refused")

// SetHAActiveFile sets the path HAActive checks to decide whether this
// instance is the active HA owner. Empty (the default) disables HA gating.
// See haActiveFile's doc comment (agent.go) for the external-ownership
// design this implements (#11).
//
// Must be called before Run(): haActiveFile itself is read (HAActive) from
// several goroutines Run() starts (the sync loop, watch/reconcile-queue
// workers, the metrics HTTP handler, runHAActivePolling) with no lock
// protecting it, which is safe only because the write here happens-before
// all of them start -- the same established pattern as every other
// SetXxx config method on QuotaAgent (see NewQuotaAgent's callers), not
// something specific to this field.
func (a *QuotaAgent) SetHAActiveFile(v string) { a.haActiveFile = v }

// HAActive reports whether this instance currently owns quota enforcement.
// Always true when HA gating is disabled (haActiveFile unset). When
// enabled, true iff haActiveFile exists -- any stat error (not just
// os.IsNotExist, so a permission error or a transient stat failure also
// fails safe rather than defaulting to "active") is treated as standby,
// since a false negative here (wrongly skipping enforcement) is far
// cheaper to recover from than a false positive (two nodes mutating the
// same shared quota metadata split-brain).
func (a *QuotaAgent) HAActive() bool {
	if a.haActiveFile == "" {
		return true
	}
	_, err := os.Stat(a.haActiveFile)
	return err == nil
}

// runHAActivePolling watches haActiveFile for active/standby transitions.
//
// On standby->active, it does not call syncAllQuotas itself -- an earlier
// version of this code did, and an independent review found that made
// syncAllQuotas concurrent for the first time (this goroutine's call
// racing the main sync loop's own ticker-driven call), which every
// existing doc comment in this package assumes cannot happen (see
// syncAllQuotas's own comment about "no second watch loop or work queue...
// there is no concurrency for a queue to protect" -- true only as long as
// nothing calls it from a second goroutine). Concurrent cycles can corrupt
// knownProjectIDs (agent.go: replaced wholesale, not merged, so a project
// ID one cycle allocated can appear "free" to the other and get handed to
// a second project) and race policySnapshot/QuotaPolicy status writes.
// Instead this sends a non-blocking signal on syncNow, which Run()'s own
// single sync-loop goroutine consumes alongside its regular ticker --
// reconciliation still only ever runs from that one goroutine.
//
// On active->standby, it clears appliedQuotas: that cache's entire
// validity assumption is "this process is the only writer of this
// filesystem," which HA existing at all means is not always true. Without
// clearing it, ensureQuota's cache-hit shortcut
// (`existingQuota == sizeBytes` -> return nil) makes the *next*
// active-triggered syncAllQuotas silently re-apply nothing on every PV
// whose capacity didn't change since this node was last active -- turning
// "failover reconciliation" into a re-list with no re-apply, which is not
// what it's advertised as doing (see docs/ha-dr.md §3).
//
// wasActive starts false unconditionally, even if this instance is
// already active when polling starts: seeding it from a real HAActive()
// read has a startup race (a transition landing between Run()'s initial
// sync and this goroutine's first tick would then never be seen as an
// edge, and the failover trigger would never fire for it). Starting false
// means an already-active instance sends one spurious, harmless,
// idempotent signal on its first tick instead -- see the trade-off this
// makes explicit in code, not just in review notes.
func (a *QuotaAgent) runHAActivePolling(ctx context.Context, pollInterval time.Duration, syncNow chan<- struct{}) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	wasActive := false
	slog.Info("HA active-marker polling started", "activeFile", a.haActiveFile, "pollInterval", pollInterval)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			active := a.HAActive()
			switch {
			case active && !wasActive:
				slog.Info("HA active marker present: became active, triggering failover reconciliation", "activeFile", a.haActiveFile)
				select {
				case syncNow <- struct{}{}:
				default:
					// A signal is already pending and hasn't been consumed
					// yet; the sync loop will pick it up on its own, no
					// need to queue a second one.
				}
			case !active && wasActive:
				slog.Warn("HA active marker absent: became standby, quota mutation will be skipped until it returns", "activeFile", a.haActiveFile)
				a.mu.Lock()
				a.appliedQuotas = make(map[string]int64)
				a.mu.Unlock()
			}
			wasActive = active
		}
	}
}
