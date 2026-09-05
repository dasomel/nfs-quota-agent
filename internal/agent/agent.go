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

// Package agent implements the core daemon that watches Kubernetes PersistentVolumes
// and reconciles them with filesystem project quotas on NFS exports. It also coordinates
// periodic synchronization, namespace quota policies, and automatic cleanup of orphaned quotas.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/dasomel/nfs-quota-agent/internal/apis/quota/v1alpha1"
	"github.com/dasomel/nfs-quota-agent/internal/audit"
	"github.com/dasomel/nfs-quota-agent/internal/events"
	"github.com/dasomel/nfs-quota-agent/internal/history"
	"github.com/dasomel/nfs-quota-agent/internal/pvpath"
	"github.com/dasomel/nfs-quota-agent/internal/quota"
	"github.com/dasomel/nfs-quota-agent/internal/quotapolicy"
	"github.com/dasomel/nfs-quota-agent/internal/status"
	"github.com/dasomel/nfs-quota-agent/internal/util"
)

const (
	// AnnotationProjectName is the annotation key for the project name on PVs.
	AnnotationProjectName = "nfs.io/project-name"
	// AnnotationQuotaStatus is the annotation key representing the quota application status on PVs.
	AnnotationQuotaStatus = "nfs.io/quota-status"
	// AnnotationEnforcedLimitBytes records the filesystem project quota hard
	// limit actually enforced for this PV, in bytes. This can differ from
	// PV.Spec.Capacity (the requested capacity) when a QuotaPolicy (#13)
	// clamps the effective limit -- exposing it separately keeps
	// "what Kubernetes was asked for" and "what the filesystem enforces"
	// from being confused with each other (#14 acceptance).
	AnnotationEnforcedLimitBytes = "nfs.io/enforced-limit-bytes"
	// AnnotationPolicyDecision records the winning QuotaPolicy decision and
	// deterministic decision ID on the PV in format:
	// <policy-name>/<generation>/<outcome>/<id> (#14).
	// Removed when no policy applies.
	AnnotationPolicyDecision = "nfs.io/policy-decision"

	// policyDecisionPreserve is an internal sentinel instructing updateQuotaStatus
	// to preserve any existing AnnotationPolicyDecision annotation on the PV.
	policyDecisionPreserve = "__PRESERVE__"

	// QuotaStatusPending indicates the quota application is pending.
	QuotaStatusPending = "pending"
	// QuotaStatusApplied indicates the quota has been successfully applied.
	QuotaStatusApplied = "applied"
	// QuotaStatusFailed indicates the quota application has failed.
	QuotaStatusFailed = "failed"
)

// QuotaAgent manages filesystem quotas for NFS PVs
type QuotaAgent struct {
	client          kubernetes.Interface
	nfsBasePath     string
	nfsServerPath   string
	provisionerName string
	processAllNFS   bool
	quotaPath       string
	fsType          string
	projectsFile    string
	projidFile      string
	// stateDir is a host-backed directory (distinct from projectsFile's and
	// projidFile's own directory, which may be nothing but a bind-mounted
	// individual file inside the container) used to hold the crash-recovery
	// backup sidecars quota.RemoveLineFromFile/RecoverProjectFile keep. See
	// their doc comments in internal/quota/project.go.
	stateDir         string
	syncInterval     time.Duration
	mu               sync.Mutex
	appliedQuotas    map[string]int64
	appliedDecisions map[string]string
	// appliedQuotaPVNames maps localPath -> the PV name last applied there,
	// kept in lockstep with appliedQuotas (set alongside it in
	// ensureQuotaMutatedWith, deleted alongside it in
	// forgetAppliedQuotaForPV). pruneAppliedQuotas needs this because, at
	// prune time, the PV behind a stale localPath is by definition no
	// longer in the live pvList -- there is nowhere else left to read its
	// name from in order to call eventRecorder.Forget for it.
	appliedQuotaPVNames map[string]string
	knownProjectIDs     map[uint32]string // cache of projid file; refreshed once per sync cycle
	auditLogger         *audit.Logger

	// shrinkGuardRejectWarned tracks which localPaths are currently in a
	// "the shrink/brownfield guard rejected this apply and we already
	// logged it" state -- guarded by mu, like appliedQuotas. Entries are
	// added right before the guard's rate-limited slog.Warn (#92) and
	// removed the moment a later apply for that path succeeds, so a
	// repeated rejection (the common brownfield case: identical outcome
	// every syncInterval until an operator intervenes) logs once per
	// state transition instead of once per cycle forever. Pruned in
	// pruneAppliedQuotas alongside appliedQuotas so it stays bounded by
	// live PVs, not by every path ever seen.
	shrinkGuardRejectWarned map[string]struct{}
	// storageClassBindingRejectWarned has the same state-transition rate
	// limiting contract as shrinkGuardRejectWarned, but tracks fail-closed
	// path-mapping rejections for StorageClass-restricted policies (#14),
	// keyed by PV name plus local path so basename collisions stay distinct.
	storageClassBindingRejectWarned map[string]struct{}
	storageClassBindingRejections   map[string]int64

	// priorEnforcedFromDisk and primed back ensureQuotaMutated's shrink
	// guard's restart case: appliedQuotas is purely in-memory, so it reads
	// as empty on a fresh process even for a claim that already has a real
	// on-disk quota from before this process started. Run() calls
	// primeAppliedQuotasFromDiskOnce before the first sync, and
	// syncAllQuotas retries it at the top of every cycle while still
	// unprimed (#93), fetching the filesystem-wide quota report and
	// recording each path's real on-disk hard limit here. Once primed is
	// true the call is a no-op (a plain a.mu check, no subprocess) for the
	// rest of the process's life -- a single bulk read, not one read per
	// ambiguous claim, so a burst of many first-touches (many new PVs at
	// once, or a restart with many pre-existing ones) costs one report
	// fetch, not N. See primeAppliedQuotasFromDiskOnce.
	priorEnforcedFromDisk map[string]uint64

	// priorUsageFromDisk is priorEnforcedFromDisk's sibling snapshot, taken
	// in the same primeAppliedQuotasFromDiskOnce pass at zero extra
	// subprocess cost: GetDirUsagesStrict already returns each path's usage
	// (u.Used) alongside its quota, so recording it for every path -- not
	// only ones with u.Quota > 0 -- closes the brownfield gap
	// priorEnforcedFromDisk alone cannot: a directory that already holds
	// real data but has never had a quota applied gets currentEnforced == 0
	// (nothing in priorEnforcedFromDisk, nothing in appliedQuotas), so the
	// shrink guard's currentEnforced > 0 gate skips it entirely and a small
	// hard limit can land on a large, already-full directory with no
	// warning (#90). ensureQuota treats a path whose priorUsageFromDisk
	// exceeds the newly requested enforced limit as reason to suspect a
	// problem and pay for one authoritative live read (currentUsageBytes)
	// before deciding -- the same one it already pays for on a real shrink
	// -- rather than trusting this best-effort, possibly-stale snapshot
	// directly. A path never seen in the startup snapshot (created after
	// startup) is simply absent here, which compares as "not exceeding"
	// and applies as today: it has no on-disk history to be suspicious of.
	priorUsageFromDisk map[string]uint64

	// primed is true once primeAppliedQuotasFromDiskOnce has successfully
	// filled priorEnforcedFromDisk/priorUsageFromDisk from a nil-error
	// report read -- guarded by mu, not a sync.Once: a sync.Once fires
	// exactly once ever, so a report failure at the one moment Run() called
	// it used to leave the guard fail-open (suspectBrownfield permanently
	// unable to fire) for the rest of the process's life (#93). Replacing
	// it with a plain bool lets primeAppliedQuotasFromDiskOnce be called
	// again -- from every syncAllQuotas cycle -- and retry for real instead
	// of being a one-shot attempt. See primeAppliedQuotasFromDiskOnce's doc
	// comment for the full retry contract, and ShrinkGuardPrimed for the
	// external accessor (metrics).
	primed bool

	// primeFailures counts every failed primeAppliedQuotasFromDiskOnce
	// attempt (report fetch or directory read error) since process start --
	// a Prometheus counter, so it accumulates for the process's lifetime and
	// is never reset back to 0 on a later success, matching
	// reconcileTotal/reconcileErrors' own "never reset" convention below.
	// Read by the nfs_quota_agent_shrink_guard_prime_failures_total metric
	// and by primeAppliedQuotasFromDiskOnce's own rate-limited slog.Warn.
	primeFailures atomic.Int64

	// haActiveFile, when non-empty, gates every quota mutation (ensureQuota,
	// RemoveOrphan) on this file's existence: present means this instance is
	// the active/owning node for quota enforcement, absent means standby.
	// Empty (the default) disables HA gating entirely -- existing
	// single-node/no-HA deployments enforce unconditionally, unchanged. See
	// #11: this agent deliberately does not implement its own
	// election/fencing/replication -- an external cluster manager or HA
	// tool (Pacemaker resource agent, a DRBD promote/demote hook, a custom
	// script) owns deciding which node is active and communicates that
	// decision by creating/removing this file. HAActive() is the read;
	// nothing in this package ever creates or removes the file itself.
	haActiveFile string

	// policySnapshot is the QuotaPolicy set (and the PVC labels needed to
	// match it) as of the most recent syncAllQuotas cycle, published by
	// beginQuotaPolicyCycle and read by the watch path (watch.go via
	// resolveFromSnapshot) so an Added/Modified event can resolve policy
	// for itself instead of ignoring it. Guarded by mu; the maps inside a
	// given snapshot are never mutated after publish, so reading them after
	// releasing mu is safe — see beginQuotaPolicyCycle's doc comment in
	// policy.go for why this exists (ensureQuota's own status-annotation
	// write generates a Modified event for the very PV it just enforced a
	// quota on).
	policySnapshot *resolvedPolicySnapshot

	// reconcileQueue is the live pvReconcileQueue watchPVsWithBackoff (in
	// watch.go) creates for the lifetime of the watch loop -- nil before the
	// first watch attempt and after final shutdown. An atomic.Pointer rather
	// than a plain field because ReconcileQueueDepth (metrics.AgentInfo) can
	// be read from the metrics HTTP handler's goroutine at any time,
	// concurrently with watch.go setting/clearing it.
	reconcileQueue atomic.Pointer[pvReconcileQueue]
	// reconcileTotal/reconcileErrors/reconcileDurationNanos accumulate across
	// the process lifetime (never reset), matching how a Prometheus counter
	// is meant to be read -- as a rate via rate()/increase(), not a snapshot.
	reconcileTotal         atomic.Int64
	reconcileErrors        atomic.Int64
	reconcileDurationNanos atomic.Int64
	// verificationFailures counts ensureQuota applies that succeeded (the
	// quota binary exited 0) but whose read-back verification (see
	// verifyQuotaOnDisk) then found the on-disk state didn't actually
	// match what was requested -- a distinct failure class from an apply
	// command itself failing, tracked separately so operators can tell
	// "the tool refused" from "the tool lied" (#10). Also counted in
	// reconcileErrors when the failing call went through the watch path's
	// reconcile queue (recordReconcileResult) -- but NOT when it came from
	// syncAllQuotas' periodic full sync, which calls ensureQuota directly
	// and never touches recordReconcileResult. Don't assume this is always
	// a subset of reconcileErrors; it can exceed it.
	verificationFailures atomic.Int64

	// reconcileRetriesApplyError/VerifyFailed/PolicySnapshotPending count
	// AddRateLimited retry attempts by the fixed 3-reason enum
	// docs/adr/0002-kubernetes-events-and-retry-metrics.md defines for
	// nfs_quota_agent_reconcile_retries_total{reason}. Three explicit
	// atomic fields rather than a map: the reason vocabulary is
	// compile-time bounded to these three values regardless of what a
	// caller passes, matching storageClassBindingRejections' own reasoning
	// for capping cardinality, but without even a map to defend.
	reconcileRetriesApplyError            atomic.Int64
	reconcileRetriesVerifyFailed          atomic.Int64
	reconcileRetriesPolicySnapshotPending atomic.Int64
	// reconcileBackoff accumulates the rate limiter's computed delay for
	// each reconcile-queue retry, for
	// nfs_quota_agent_reconcile_backoff_seconds -- see
	// retryBackoffHistogram's doc comment (retry_metrics.go).
	reconcileBackoff retryBackoffHistogram

	// eventRecorder emits events.k8s.io/v1 Events about per-PV quota
	// outcomes (docs/adr/0002-kubernetes-events-and-retry-metrics.md).
	// Always non-nil: events.NewNoop() by default (SetEventRecorder is the
	// only way to install a real one), so every call site below can call
	// it unconditionally instead of nil-checking per call.
	eventRecorder events.Recorder

	// Auto-cleanup configuration
	enableAutoCleanup bool
	cleanupInterval   time.Duration
	orphanGracePeriod time.Duration
	cleanupDryRun     bool
	orphanLastSeen    map[string]time.Time
	orphanMu          sync.Mutex

	// History configuration
	historyStore *history.Store

	// Policy configuration. enablePolicy gates the web UI's advisory
	// namespace policy/violations views only (internal/policy) — it never
	// affects quota sizing.
	enablePolicy bool

	// QuotaPolicy (quota.nfs.io/v1alpha1) configuration. Distinct from the
	// enablePolicy block above, which backs the older LimitRange/Annotation
	// namespace-policy chain in internal/policy — this is the CRD-based,
	// per-claim policy added by docs/quotapolicy-design.md, reconciled from
	// internal/quotapolicy.
	// dynamicClient is nil unless the caller supplies one via
	// SetDynamicClient; quotaPolicyEnabled with a nil dynamicClient degrades
	// to "no policies" (logged once) rather than panicking, matching how a
	// missing CRD degrades in quotapolicy.List.
	quotaPolicyEnabled bool
	dynamicClient      dynamic.Interface

	// quotaPolicySingleWriter opts this agent into writing QuotaPolicy
	// status. Default false, independent of quotaPolicyEnabled: this chart
	// is a DaemonSet that explicitly supports several NFS server nodes at
	// once (see values.yaml's nodeSelector comment), and every one of them
	// resolving the same policy and calling UpdateStatus with its own
	// partial appliedClaims/failingClaims view would flap that status every
	// cycle rather than converge — see docs/quotapolicy-design.md §11
	// "Multi-writer status". Quota *enforcement* is unaffected by this
	// flag; it only gates finishQuotaPolicyCycle's status write-back.
	quotaPolicySingleWriter bool
	// quotaPolicyStatusSkipLogOnce fires the "status write-back disabled"
	// warning exactly once per process instead of once per sync cycle.
	quotaPolicyStatusSkipLogOnce sync.Once

	// Health/readiness state, read by the metrics server's /health and
	// /ready handlers. Kept on a dedicated mutex, separate from mu, so
	// probe reads never contend with quota-application work.
	healthMu                sync.RWMutex
	lastHeartbeat           time.Time // updated once per Run() loop iteration; liveness proof of life
	fsDetected              bool
	quotaAvailable          bool
	initialSyncDone         bool
	consecutiveSyncFailures int
	lastSyncErr             error
	// lastSuccessfulFullSync is when syncAllQuotas (the periodic full
	// reconciliation, independent of the watch path) last completed without
	// error -- exposed as a metric so an operator can see how stale the
	// full-reconcile backstop is, separate from whether the watch itself is
	// connected.
	lastSuccessfulFullSync time.Time
}

// livenessStallMultiplier controls how many syncInterval periods may pass
// without a heartbeat before liveness reports not-ok. It must be generous:
// too tight and a legitimately slow sync (many PVs) trips a restart loop.
const livenessStallMultiplier = 4

// readinessSyncFailureThreshold is the number of consecutive sync failures
// after which readiness flips to not-ready. A single transient failure
// (e.g. one missed API call) should not pull the instance out of service.
const readinessSyncFailureThreshold = 3

// NewQuotaAgent creates a new QuotaAgent
func NewQuotaAgent(client kubernetes.Interface, nfsBasePath, nfsServerPath, provisionerName string) *QuotaAgent {
	return &QuotaAgent{
		client:                          client,
		nfsBasePath:                     nfsBasePath,
		nfsServerPath:                   nfsServerPath,
		provisionerName:                 provisionerName,
		quotaPath:                       nfsBasePath,
		projectsFile:                    "/etc/projects",
		projidFile:                      "/etc/projid",
		stateDir:                        "/var/lib/nfs-quota-agent",
		syncInterval:                    30 * time.Second,
		appliedQuotas:                   make(map[string]int64),
		appliedDecisions:                make(map[string]string),
		appliedQuotaPVNames:             make(map[string]string),
		priorEnforcedFromDisk:           make(map[string]uint64),
		priorUsageFromDisk:              make(map[string]uint64),
		shrinkGuardRejectWarned:         make(map[string]struct{}),
		storageClassBindingRejectWarned: make(map[string]struct{}),
		storageClassBindingRejections:   make(map[string]int64),
		knownProjectIDs:                 make(map[uint32]string),
		cleanupInterval:                 1 * time.Hour,
		orphanGracePeriod:               24 * time.Hour,
		cleanupDryRun:                   true,
		orphanLastSeen:                  make(map[string]time.Time),
		eventRecorder:                   events.NewNoop(),
	}
}

// Setters for configuration

// SetProcessAllNFS sets whether all NFS PVs should be processed.
func (a *QuotaAgent) SetProcessAllNFS(v bool) { a.processAllNFS = v }

// SetEventRecorder installs r as the agent's events.k8s.io/v1 Event
// recorder (docs/adr/0002-kubernetes-events-and-retry-metrics.md). Not
// called at all by default -- NewQuotaAgent already wires events.NewNoop(),
// matching --enable-events' default-false posture -- so main.go only calls
// this when the flag is on and a real client is available.
func (a *QuotaAgent) SetEventRecorder(r events.Recorder) { a.eventRecorder = r }

// SetQuotaPath sets the local quota path.
func (a *QuotaAgent) SetQuotaPath(v string) { a.quotaPath = v }

// SetProjectsFile sets the projects file path.
func (a *QuotaAgent) SetProjectsFile(v string) { a.projectsFile = v }

// SetProjidFile sets the projid file path.
func (a *QuotaAgent) SetProjidFile(v string) { a.projidFile = v }

// ProjectsFile returns the configured projects file path, for callers
// (internal/metrics, internal/ui) that need to read the same real
// on-disk quota state ensureQuota's own verification does, rather than
// assume the standard /etc/projects -- see the CLAUDE.md gotcha on
// GetDirUsages' hardcoded defaults for why this matters under a
// non-default --projects-file.
func (a *QuotaAgent) ProjectsFile() string { return a.projectsFile }

// ProjidFile returns the configured projid file path. See ProjectsFile.
func (a *QuotaAgent) ProjidFile() string { return a.projidFile }

// SetStateDir sets the host-backed directory used for crash-recovery backup
// sidecars of projectsFile/projidFile. An empty value disables the sidecar
// (see quota.RemoveLineFromFile/RecoverProjectFile), degrading gracefully
// rather than failing quota operations.
func (a *QuotaAgent) SetStateDir(v string) { a.stateDir = v }

// SetSyncInterval sets the synchronization interval.
func (a *QuotaAgent) SetSyncInterval(v time.Duration) { a.syncInterval = v }

// SetAuditLogger sets the audit logger instance.
func (a *QuotaAgent) SetAuditLogger(v *audit.Logger) { a.auditLogger = v }

// SetEnableAutoCleanup enables or disables automatic orphan cleanup.
func (a *QuotaAgent) SetEnableAutoCleanup(v bool) { a.enableAutoCleanup = v }

// SetCleanupIntervalDuration sets the automatic cleanup interval.
func (a *QuotaAgent) SetCleanupIntervalDuration(v time.Duration) { a.cleanupInterval = v }

// SetOrphanGracePeriodDuration sets the grace period before orphaned directories are removed.
func (a *QuotaAgent) SetOrphanGracePeriodDuration(v time.Duration) { a.orphanGracePeriod = v }

// SetCleanupDryRunFlag sets the dry-run flag for the orphan cleanup.
func (a *QuotaAgent) SetCleanupDryRunFlag(v bool) { a.cleanupDryRun = v }

// SetHistoryStore sets the usage history store.
func (a *QuotaAgent) SetHistoryStore(v *history.Store) { a.historyStore = v }

// SetEnablePolicy enables or disables the web UI's advisory namespace
// policy/violations views (internal/policy). Quota sizing never consults
// this.
func (a *QuotaAgent) SetEnablePolicy(v bool) { a.enablePolicy = v }

// SetQuotaPolicyEnabled enables or disables QuotaPolicy (quota.nfs.io/v1alpha1)
// resolution and enforcement. Defaults to false: see
// docs/quotapolicy-design.md and cmd/nfs-quota-agent/main.go's
// --enable-quota-policy flag for why this stays opt-in.
func (a *QuotaAgent) SetQuotaPolicyEnabled(v bool) { a.quotaPolicyEnabled = v }

// SetDynamicClient sets the dynamic client used to list and update the
// status of QuotaPolicy objects (internal/quotapolicy). Kept as a setter
// rather than a NewQuotaAgent parameter, matching every other optional
// dependency on this type, and letting tests wire a
// k8s.io/client-go/dynamic/fake client without touching the constructor.
func (a *QuotaAgent) SetDynamicClient(v dynamic.Interface) { a.dynamicClient = v }

// SetQuotaPolicySingleWriter declares that this is the only agent instance
// that will ever resolve QuotaPolicy for the cluster (a single NFS server
// node, or a deployment that has verified out-of-band that only one node
// runs with --enable-quota-policy). Set it to true only when that's
// actually true: see quotaPolicySingleWriter's doc comment for why a
// multi-node deployment must leave this false.
func (a *QuotaAgent) SetQuotaPolicySingleWriter(v bool) { a.quotaPolicySingleWriter = v }

// QuotaPolicyEnabled returns whether QuotaPolicy resolution is enabled.
func (a *QuotaAgent) QuotaPolicyEnabled() bool { return a.quotaPolicyEnabled }

// Getters for UI/metrics interface

// BasePath returns the base mount path of the NFS export.
func (a *QuotaAgent) BasePath() string { return a.nfsBasePath }

// EnableAutoCleanup returns whether automatic cleanup is enabled.
func (a *QuotaAgent) EnableAutoCleanup() bool { return a.enableAutoCleanup }

// CleanupDryRun returns whether cleanup runs in dry-run mode.
func (a *QuotaAgent) CleanupDryRun() bool { return a.cleanupDryRun }

// OrphanGracePeriod returns the grace period for orphaned directories.
func (a *QuotaAgent) OrphanGracePeriod() time.Duration { return a.orphanGracePeriod }

// CleanupInterval returns the interval between cleanup runs.
func (a *QuotaAgent) CleanupInterval() time.Duration { return a.cleanupInterval }

// EnablePolicy returns whether namespace quota policy is enabled.
func (a *QuotaAgent) EnablePolicy() bool { return a.enablePolicy }

// AuditLogger returns the configured audit logger instance.
func (a *QuotaAgent) AuditLogger() *audit.Logger { return a.auditLogger }

// AppliedQuotaCount returns the number of active project quotas applied.
func (a *QuotaAgent) AppliedQuotaCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.appliedQuotas)
}

// ReconcileQueueDepth returns the number of PV keys currently queued or
// in flight in the watch path's reconcile queue (see reconcile_queue.go).
// 0 before the watch loop's first attempt or after final shutdown.
func (a *QuotaAgent) ReconcileQueueDepth() int {
	q := a.reconcileQueue.Load()
	if q == nil {
		return 0
	}
	return q.depth()
}

// ReconcileStats returns cumulative counts/duration for reconcile-queue
// work processed since process start: total items processed, how many
// ended in error, and the total wall-clock time spent in ensureQuota across
// all of them (seconds -- divide by total for a mean).
func (a *QuotaAgent) ReconcileStats() (total, errs int64, durationSeconds float64) {
	total = a.reconcileTotal.Load()
	errs = a.reconcileErrors.Load()
	durationSeconds = time.Duration(a.reconcileDurationNanos.Load()).Seconds()
	return total, errs, durationSeconds
}

// recordReconcileResult records one reconcile-queue item's outcome for
// ReconcileStats. Called by pvReconcileQueue.process (reconcile_queue.go).
func (a *QuotaAgent) recordReconcileResult(d time.Duration, err error) {
	a.reconcileTotal.Add(1)
	a.reconcileDurationNanos.Add(d.Nanoseconds())
	if err != nil {
		a.reconcileErrors.Add(1)
	}
}

// recordReconcileRetry records one reconcile-queue AddRateLimited retry
// attempt for nfs_quota_agent_reconcile_retries_total{reason} and its
// computed backoff delay for nfs_quota_agent_reconcile_backoff_seconds.
// reason must be one of the retryReason* constants (reconcile_queue.go);
// any other value is silently not counted in ReconcileRetryStats (but
// still observed in the backoff histogram) rather than panicking or
// growing an unbounded map -- see reconcileRetriesApplyError's doc comment
// for why the reason vocabulary is fixed at three explicit fields.
func (a *QuotaAgent) recordReconcileRetry(reason string, backoffSeconds float64) {
	switch reason {
	case retryReasonApplyError:
		a.reconcileRetriesApplyError.Add(1)
	case retryReasonVerifyFailed:
		a.reconcileRetriesVerifyFailed.Add(1)
	case retryReasonPolicySnapshotPending:
		a.reconcileRetriesPolicySnapshotPending.Add(1)
	}
	a.reconcileBackoff.observe(backoffSeconds)
}

// ReconcileRetryStats returns cumulative reconcile-queue retry-attempt
// counts by reason since process start (never reset), for
// nfs_quota_agent_reconcile_retries_total{reason} (metrics.AgentInfo).
func (a *QuotaAgent) ReconcileRetryStats() map[string]int64 {
	return map[string]int64{
		retryReasonApplyError:            a.reconcileRetriesApplyError.Load(),
		retryReasonVerifyFailed:          a.reconcileRetriesVerifyFailed.Load(),
		retryReasonPolicySnapshotPending: a.reconcileRetriesPolicySnapshotPending.Load(),
	}
}

// ReconcileBackoffHistogram returns a snapshot of
// nfs_quota_agent_reconcile_backoff_seconds: the fixed bucket upper bounds
// (seconds), their current per-bucket (not cumulative) observation counts
// in the same order, the running sum of observed seconds, and the total
// observation count (metrics.AgentInfo).
func (a *QuotaAgent) ReconcileBackoffHistogram() (buckets []float64, counts []int64, sum float64, count int64) {
	return a.reconcileBackoff.snapshot()
}

// forgetAppliedQuotaForPV drops pv's local path from the applied-quota
// cache. Called by pvReconcileQueue.process's tombstone branch (a Deleted
// event routed through the reconcile queue -- see enqueueDelete's doc
// comment for why Deleted goes through the queue at all rather than
// mutating this cache directly from watch.go's eventLoop) so that a
// worker still mid-flight on an older Added/Modified for the same key
// cannot re-populate this entry after the deletion is processed: workqueue
// guarantees the tombstone is delivered to a worker only after any
// already-in-flight reconcile for the same key has finished, never before
// or concurrently with it.
func (a *QuotaAgent) forgetAppliedQuotaForPV(pv *v1.PersistentVolume) {
	// Evicts events.Recorder's own per-(pv, reason) dedup window entries for
	// this PV -- without this, r.last (internal/events) keeps growing by
	// one entry per (ever-seen PV, reason) pair for the life of the
	// process. Called unconditionally, ahead of the nfsPath=="" early
	// return below: Forget only needs pv.Name, not a local path, so a PV
	// with no resolvable NFS path (which never had an appliedQuotas entry
	// to drop in the first place) must not also skip this.
	a.eventRecorder.Forget(pv.Name)

	nfsPath := a.getNFSPath(pv)
	if nfsPath == "" {
		return
	}
	localPath := a.nfsPathToLocal(nfsPath)

	a.mu.Lock()
	delete(a.appliedQuotas, localPath)
	delete(a.appliedDecisions, localPath)
	delete(a.appliedQuotaPVNames, localPath)
	a.mu.Unlock()
}

// recordHeartbeat marks the periodic sync loop as having made progress.
// Called once at Run() start and once per loop iteration; liveness compares
// its age against syncInterval, never against Kubernetes API or NFS state.
func (a *QuotaAgent) recordHeartbeat() {
	a.healthMu.Lock()
	a.lastHeartbeat = time.Now()
	a.healthMu.Unlock()
}

// setFilesystemDetected records whether detectFilesystemType succeeded.
func (a *QuotaAgent) setFilesystemDetected(v bool) {
	a.healthMu.Lock()
	a.fsDetected = v
	a.healthMu.Unlock()
}

// setQuotaAvailable records whether checkQuotaAvailable succeeded.
func (a *QuotaAgent) setQuotaAvailable(v bool) {
	a.healthMu.Lock()
	a.quotaAvailable = v
	a.healthMu.Unlock()
}

// recordSyncResult records the outcome of a syncAllQuotas attempt (initial
// or periodic). Consecutive failures accumulate and flip readiness to
// not-ready once they cross readinessSyncFailureThreshold; a success resets
// the counter so a single transient failure doesn't pull the instance out
// of service.
func (a *QuotaAgent) recordSyncResult(err error) {
	a.healthMu.Lock()
	defer a.healthMu.Unlock()
	a.initialSyncDone = true
	if err != nil {
		a.consecutiveSyncFailures++
		a.lastSyncErr = err
		return
	}
	a.consecutiveSyncFailures = 0
	a.lastSyncErr = nil
	a.lastSuccessfulFullSync = time.Now()
}

// LastSuccessfulFullSync returns when the periodic full reconciliation
// (syncAllQuotas) last completed without error, or the zero Time if it
// never has.
func (a *QuotaAgent) LastSuccessfulFullSync() time.Time {
	a.healthMu.RLock()
	defer a.healthMu.RUnlock()
	return a.lastSuccessfulFullSync
}

// LivenessOK reports whether the agent's main loop is making progress. It
// deliberately does not consult the Kubernetes API or the NFS mount: a
// probe wired to it must not restart-loop on a transient outage that a
// restart cannot fix. Before the first loop iteration completes it reports
// ok, since startup work (filesystem detection, initial sync) legitimately
// takes time.
func (a *QuotaAgent) LivenessOK() (bool, string) {
	a.healthMu.RLock()
	hb := a.lastHeartbeat
	interval := a.syncInterval
	a.healthMu.RUnlock()

	if hb.IsZero() {
		return true, "starting"
	}
	if interval <= 0 {
		return true, "ok"
	}
	threshold := interval * livenessStallMultiplier
	if age := time.Since(hb); age > threshold {
		return false, fmt.Sprintf("sync loop stalled: last heartbeat %s ago (threshold %s)", age.Round(time.Second), threshold)
	}
	return true, "ok"
}

// ReadinessOK reports whether this instance can enforce quotas right now:
// filesystem type detected, quota subsystem available, base path present,
// and the initial sync completed without a run of consecutive failures. It
// reads live state, so it flips back to not-ready if sync starts failing
// repeatedly after having been ready. An agent with zero PVs to manage is
// legitimately ready, so this never consults AppliedQuotaCount.
func (a *QuotaAgent) ReadinessOK() (bool, string) {
	a.healthMu.RLock()
	fsDetected := a.fsDetected
	quotaAvailable := a.quotaAvailable
	initialSyncDone := a.initialSyncDone
	failures := a.consecutiveSyncFailures
	lastErr := a.lastSyncErr
	a.healthMu.RUnlock()

	if !fsDetected {
		return false, "filesystem type not detected"
	}
	if !quotaAvailable {
		return false, "quota subsystem not available"
	}
	if _, err := os.Stat(a.nfsBasePath); err != nil {
		return false, fmt.Sprintf("base path not accessible: %v", err)
	}
	if !initialSyncDone {
		return false, "initial quota sync not yet completed"
	}
	if failures >= readinessSyncFailureThreshold {
		return false, fmt.Sprintf("quota sync failing: %d consecutive failures (last error: %v)", failures, lastErr)
	}
	return true, "ok"
}

// Run starts the quota agent
func (a *QuotaAgent) Run(ctx context.Context) error {
	// Mark the process alive before any startup work, so liveness is
	// generous during (potentially slow) filesystem/quota detection rather
	// than reporting not-started as a stall.
	a.recordHeartbeat()

	// Detect filesystem type
	if err := a.detectFilesystemType(); err != nil {
		return fmt.Errorf("failed to detect filesystem type: %w", err)
	}
	a.setFilesystemDetected(true)

	slog.Info("Starting NFS Quota Agent",
		"nfsBasePath", a.nfsBasePath,
		"nfsServerPath", a.nfsServerPath,
		"provisionerName", a.provisionerName,
		"processAllNFS", a.processAllNFS,
		"fsType", a.fsType,
	)

	// Check if quota is available
	if err := a.checkQuotaAvailable(); err != nil {
		return fmt.Errorf("quota not available: %w", err)
	}
	a.setQuotaAvailable(true)

	// Load existing projects
	if err := a.loadProjects(); err != nil {
		slog.Warn("Failed to load existing projects", "error", err)
	}

	// Snapshot the real on-disk quota report once, before the first sync
	// touches anything -- see priorEnforcedFromDisk's doc comment and
	// ensureQuotaMutated's shrink guard for why: appliedQuotas starts
	// empty every restart, and without this snapshot the guard can't tell
	// a claim that's genuinely new from one whose on-disk quota was
	// already lower than current usage before this process even started.
	a.primeAppliedQuotasFromDiskOnce()

	// Initial sync
	if err := a.syncAllQuotas(ctx); err != nil {
		slog.Error("Initial quota sync failed", "error", err)
		a.recordSyncResult(err)
	} else {
		a.recordSyncResult(nil)
	}

	// Start watching PVs. watchWG lets shutdown below wait for watchPVs to
	// actually finish -- which includes draining its reconcile queue (see
	// watch.go/reconcile_queue.go) -- rather than returning from Run() while
	// queued quota mutations are still in flight on another goroutine.
	var watchWG sync.WaitGroup
	watchWG.Add(1)
	go func() {
		defer watchWG.Done()
		a.watchPVs(ctx)
	}()

	// Start auto-cleanup if enabled
	if a.enableAutoCleanup {
		go a.runAutoCleanup(ctx)
	}

	// Start HA active-marker polling if configured (#11): only when opted
	// in, since the common single-node/no-HA deployment has nothing to
	// poll for and shouldn't pay for an extra goroutine/ticker. haSyncNow
	// carries only a coalescing signal (buffered 1, non-blocking send in
	// runHAActivePolling) -- the actual syncAllQuotas call it triggers
	// still runs from this function's own goroutine below, via the select
	// loop's dedicated case, not from the polling goroutine itself. That
	// matters: syncAllQuotas is documented (its own doc comment) and
	// relied upon elsewhere (knownProjectIDs, policySnapshot) as having
	// exactly one caller goroutine: calling it from a second goroutine
	// concurrently was found, in review, to risk corrupting both.
	haSyncNow := make(chan struct{}, 1)
	if a.haActiveFile != "" {
		go a.runHAActivePolling(ctx, haActivePollInterval, haSyncNow)
	}

	// Start history collection if enabled
	if a.historyStore != nil {
		go a.collectHistory(ctx)
	}

	// Periodic sync
	ticker := time.NewTicker(a.syncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("Quota agent shutting down")
			watchWG.Wait()
			return nil
		case <-ticker.C:
			a.recordHeartbeat()
			if err := a.syncAllQuotas(ctx); err != nil {
				slog.Error("Periodic quota sync failed", "error", err)
				a.recordSyncResult(err)
			} else {
				a.recordSyncResult(nil)
			}
		case <-haSyncNow:
			a.recordHeartbeat()
			slog.Info("Running failover reconciliation triggered by an HA standby->active transition")
			if err := a.syncAllQuotas(ctx); err != nil {
				slog.Error("Failover reconciliation failed", "error", err)
				a.recordSyncResult(err)
			} else {
				a.recordSyncResult(nil)
			}
		}
	}
}

// detectFilesystemType detects the filesystem type of the quota path
func (a *QuotaAgent) detectFilesystemType() error {
	fsType, err := quota.DetectFSTypeWithFindmnt(a.quotaPath)
	if err != nil {
		return err
	}

	switch fsType {
	case "xfs":
		a.fsType = quota.FSTypeXFS
	case "ext4":
		a.fsType = quota.FSTypeExt4
	case "btrfs":
		a.fsType = quota.FSTypeBtrfs
	default:
		return fmt.Errorf("unsupported filesystem type: %s (only xfs, ext4, and btrfs are supported)", fsType)
	}

	slog.Info("Detected filesystem type", "fsType", a.fsType, "path", a.quotaPath)
	return nil
}

// checkQuotaAvailable checks if quota commands are available
func (a *QuotaAgent) checkQuotaAvailable() error {
	switch a.fsType {
	case quota.FSTypeXFS:
		return quota.CheckXFSQuotaAvailable(a.quotaPath)
	case quota.FSTypeExt4:
		return quota.CheckExt4QuotaAvailable(a.quotaPath)
	case quota.FSTypeBtrfs:
		return quota.CheckBtrfsQuotaAvailable(a.quotaPath)
	default:
		return fmt.Errorf("unsupported filesystem type: %s", a.fsType)
	}
}

// loadProjects loads existing project mappings
func (a *QuotaAgent) loadProjects() error {
	if a.fsType == quota.FSTypeBtrfs {
		return nil
	}

	// Recover the host's project metadata files from their backup sidecar
	// before anything reads them, in case an earlier in-place rewrite
	// crashed and left projects or projid truncated/corrupt.
	if err := quota.RecoverProjectFile(a.projectsFile, a.stateDir); err != nil {
		slog.Warn("Failed to recover projects file", "error", err)
	}
	if err := quota.RecoverProjectFile(a.projidFile, a.stateDir); err != nil {
		slog.Warn("Failed to recover projid file", "error", err)
	}

	// Report (never auto-repair) any project ID present in only one of
	// projects/projid: rewriting host quota metadata based on a guess is
	// worse than surfacing the mismatch for an operator to resolve.
	if mismatches, err := quota.CheckProjectFileConsistency(a.projectsFile, a.projidFile); err != nil {
		slog.Warn("Failed to check project file consistency", "error", err)
	} else {
		for _, mismatch := range mismatches {
			slog.Error("Inconsistent project identity files detected", "detail", mismatch)
		}
	}

	projects, err := quota.ReadProjectsFile(a.projectsFile)
	if err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	for _, path := range projects {
		if path != "" {
			if _, exists := a.appliedQuotas[path]; !exists {
				// Mark as known (size unknown until next sync updates it)
				a.appliedQuotas[path] = 0
			}
		}
	}

	slog.Info("Loaded existing projects", "count", len(projects))
	return nil
}

// syncAllQuotas syncs quotas for all matching PVs
func (a *QuotaAgent) syncAllQuotas(ctx context.Context) error {
	pvList, err := a.client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list PVs: %w", err)
	}

	// Retry the shrink guard's brownfield-snapshot prime every cycle while
	// still unprimed (#93) -- a no-op single a.mu check once it has
	// succeeded. See primeAppliedQuotasFromDiskOnce's doc comment: this is
	// what turns a single startup report failure from a permanent
	// fail-open window (the old sync.Once behavior) into a transient one
	// that self-heals on the next cycle the report is readable.
	a.primeAppliedQuotasFromDiskOnce()

	// Refresh project ID cache once per sync cycle so generateProjectID
	// doesn't read /etc/projid on every PV.
	ids := a.loadExistingProjectIDs()
	a.markOrphanedProjectIDsAsTaken(ids)
	a.mu.Lock()
	a.knownProjectIDs = ids
	a.mu.Unlock()

	// Resolve QuotaPolicy objects once per cycle (nil when the feature is
	// disabled, no dynamic client is configured, or no policies exist) —
	// see policy.go. This is the only place QuotaPolicy is reconciled: no
	// second watch loop or work queue, per docs/quotapolicy-design.md and
	// CLAUDE.md — ensureQuota already serializes every PV through a.mu, so
	// there is no concurrency for a queue to protect.
	cycle := a.beginQuotaPolicyCycle(ctx)

	// Lazily fetched at most once for this whole cycle, on the first PV
	// that actually needs a drift check (#13) -- the report is identical
	// for every PV on this node within one cycle (same
	// fsType/quotaPath/projectsFile/projidFile, none of which vary per
	// PV), so fetching it per-PV would repeat the same filesystem-wide
	// scan once per matched claim. See fetchQuotaReport's doc comment.
	//
	// Known, accepted residual gap: this snapshot can still be stale
	// relative to a mutation the *watch path's* reconcile queue makes
	// concurrently, in a separate goroutine, to a claim this same cycle
	// also drift-checks against it -- the mutatedThisCycle-equivalent
	// guard below (using ensureQuotaMutated's own return value) only
	// covers mutations *this syncAllQuotas call itself* makes, not ones
	// made by a concurrent caller. Fully closing that would mean either
	// re-fetching per claim (defeating the fetch-once optimization this
	// exists for) or locking a claim's whole check against concurrent
	// watch-path reconciliation of the same claim, which reconcile_queue.go
	// doesn't currently expose a hook for. The failure mode is a spurious
	// Drifted=True for one claim, self-correcting on the next cycle once
	// the concurrent write has landed -- not a missed real problem or an
	// enforcement error, which is why this is documented rather than
	// solved outright.
	var driftReport map[string]uint64
	var driftReportErr error
	var driftReportFetched bool

	// usageSnap memoizes, once per this whole cycle and lazily on the
	// first PV that needs it, which localPaths the current usage report
	// even knows about -- see passUsageSnapshot's doc comment (#92). A
	// brownfield claim that stays rejected every cycle until an operator
	// intervenes (the exact "N claims, N report subprocesses every 30s"
	// case #92 targets) is served from this after the first check instead
	// of paying for its own filesystem-wide report fetch; a path present
	// in the report always still gets ensureQuotaMutatedWith's own live
	// currentUsageBytes read, so no approving decision's freshness
	// changes.
	usageSnap := &passUsageSnapshot{}

	syncedCount := 0
	haSkippedCount := 0
	live := make(map[string]struct{}, len(pvList.Items))
	liveNames := make(map[string]struct{}, len(pvList.Items))
	for _, pv := range pvList.Items {
		if !a.shouldProcessPV(&pv) {
			continue
		}
		var localPath string
		var hasLocalDir bool
		if nfsPath := a.getNFSPath(&pv); nfsPath != "" {
			localPath = a.nfsPathToLocal(nfsPath)
			live[localPath] = struct{}{}
			liveNames[pv.Name] = struct{}{}
			if _, statErr := os.Stat(localPath); statErr == nil {
				hasLocalDir = true
			}
		}

		// Whether to resolve/record a QuotaPolicy outcome for this claim at
		// all depends on hasLocalDir and quotaPolicySingleWriter together —
		// see the two cases below. This chart runs as a DaemonSet across
		// possibly several NFS server nodes (values.yaml's nodeSelector
		// comment), each with its own disjoint slice of PV directories, and
		// syncAllQuotas lists PVs cluster-wide regardless of which node's
		// export backs them.
		var effectiveBytes int64
		var winner *v1alpha1.QuotaPolicy
		var boundDecision quotapolicy.BoundDecision
		switch {
		case hasLocalDir:
			// The normal case: this node backs the claim, resolve and
			// record its real outcome below once ensureQuota runs.
			effectiveBytes, winner, boundDecision = cycle.resolve(&pv)
		case a.quotaPolicySingleWriter:
			// No local directory, but this agent has declared itself the
			// only writer — so there is no "some other node owns this
			// claim" explanation available, and staying silent about it
			// would repeat the exact bug docs/quotapolicy-design.md §11 and
			// the regression test guarding this describe: a claim a policy
			// matches but never actually enforces must not be silently
			// excluded from status, or Applied can read True (or, worse,
			// vacuously True from zero recorded claims) while nothing was
			// enforced. Resolve to find out if a policy would have won,
			// so it can be recorded as a real failure below rather than
			// dropped.
			effectiveBytes, winner, boundDecision = cycle.resolve(&pv)
		default:
			// Multi-writer default: a claim with no local directory here
			// most likely belongs to a different NFS server node's export.
			// Excluding it entirely (not even counting it as matched) is
			// what makes each node's status view honest about only what it
			// can see — see finishQuotaPolicyCycle's doc comment for why
			// that alone still isn't sufficient to write status safely,
			// which is exactly why status write-back stays gated off here.
		}

		// pa carries winner/boundDecision into ensureQuotaMutatedWith so its
		// audit entry can attach policy provenance (#14) -- see
		// policyAttempt's doc comment for why this is the only call site
		// that has this information to give. A nil winner still produces a
		// non-nil pa here, but ensureQuotaMutatedWith only attaches
		// provenance when pa.winner is itself non-nil, so this is
		// equivalent to passing nil when nothing matched.
		pa := &policyAttempt{winner: winner, decision: boundDecision}
		mutated, err := a.ensureQuotaMutatedWith(ctx, &pv, effectiveBytes, usageSnap, pa)
		switch {
		case hasLocalDir:
			// Independent drift check (#13's Drifted condition), only
			// when enforcement itself reported no error: ensureQuota's
			// own cache short-circuit (appliedQuotas[localPath] ==
			// sizeBytes) skips re-verifying an already-cached value even
			// if the real on-disk state has since changed out of band --
			// this is what catches that case on every full sync cycle
			// regardless. A standby instance's err is ErrHAStandby
			// (non-nil), so this never runs there either, same as the
			// enforcement path it piggybacks on.
			//
			// Restricted to claims THIS call to ensureQuotaMutated did
			// NOT itself just mutate: the shared driftReport snapshot
			// below is fetched once, lazily, on the first claim that
			// needs it -- so for a PV that gets a fresh apply/update
			// *after* that snapshot was taken (a different PV earlier in
			// this same loop triggered the fetch), the snapshot predates
			// this PV's own mutation and comparing against it would
			// misreport a brand new, correctly-applied value as drift. A
			// freshly mutated claim doesn't need this check anyway:
			// ensureQuota's own apply-time read-back (#10's
			// verifyQuotaOnDisk) already confirmed it moments ago.
			//
			// mutated comes directly from ensureQuotaMutated's own return
			// value, not inferred from a before/after read of
			// appliedQuotas: an independent review pointed out that
			// inference is vulnerable to an ABA race against the watch
			// path's reconcile queue, which can concurrently call
			// ensureQuota for the same PV in a separate goroutine and
			// leave the net cache value looking unchanged even though a
			// mutation genuinely happened during this window.
			var drift driftCheck
			if err == nil && winner != nil && !mutated {
				if !driftReportFetched {
					driftReportFetched = true
					driftReport, driftReportErr = a.fetchQuotaReport()
					if driftReportErr != nil {
						// A transient report-command failure isn't
						// evidence of drift, only that this cycle
						// couldn't check -- see errQuotaReportUnavailable's
						// doc comment. Logged once per cycle, not once per
						// PV: every PV shares this same fetch attempt.
						slog.Warn("Could not check quota drift this cycle: on-disk quota report unavailable", "error", driftReportErr)
					}
				}
				if driftReportErr != nil {
					drift.unknown = true
				} else if sizeBytes, sizeErr := resolveSizeBytes(&pv, effectiveBytes); sizeErr == nil {
					drift.err = compareToReport(a.fsType, driftReport, localPath, sizeBytes)
					if drift.err != nil {
						a.eventRecorder.Event(&pv, events.TypeWarning, events.QuotaDrifted,
							"On-disk quota for PV %s no longer matches what this agent applied: %v", pv.Name, drift.err)
					}
					// sizeErr itself is deliberately not surfaced as
					// drift: a PV with no storage capacity is an
					// enforcement problem ensureQuota's own error path
					// above would already have caught (ensureQuota calls
					// the identical resolveSizeBytes first), not a "the
					// filesystem disagrees" one -- and err is nil here
					// precisely because ensureQuota already resolved this
					// pv without error this same cycle.
				}
			}
			cycle.recordEnforcement(winner, &pv, err, drift)
		case a.quotaPolicySingleWriter:
			// ensureQuota returned nil here too (it skips silently on a
			// missing directory), so without substituting a real error the
			// claim would still look like a clean success. Record the
			// actual, honest outcome: matched, but not enforced.
			cycle.recordEnforcement(winner, &pv, errLocalDirectoryMissing, driftCheck{})
		}
		switch {
		case errors.Is(err, ErrHAStandby):
			// Not counted as synced (nothing happened) and not logged as
			// a failure (nothing went wrong) -- see haSkippedCount's
			// summary log below instead of a per-PV line here, which
			// would otherwise repeat once per PV every syncInterval for
			// as long as this node stays standby.
			haSkippedCount++
		case err != nil:
			slog.Error("Failed to ensure quota for PV", "pv", pv.Name, "error", err)
		default:
			syncedCount++
		}
	}

	if haSkippedCount > 0 {
		slog.Warn("Skipped quota mutation for PVs: this instance is HA standby",
			"count", haSkippedCount, "activeFile", a.haActiveFile)
	}

	a.pruneAppliedQuotas(live, liveNames)
	if rq := a.reconcileQueue.Load(); rq != nil {
		// Bounds the reconcile queue's latest cache the same way
		// pruneAppliedQuotas bounds appliedQuotas: entries for PVs the
		// watch never got a Deleted event for (disconnected during the
		// deletion, or no longer matching shouldProcessPV) would otherwise
		// accumulate for the life of the process. See pruneExcept's doc
		// comment for why this is a compare-and-delete, not a delete.
		rq.pruneExcept(liveNames)
	}
	a.finishQuotaPolicyCycle(ctx, cycle)

	slog.Debug("Quota sync completed", "synced", syncedCount, "total", len(pvList.Items))
	return nil
}

// pruneAppliedQuotas drops cache entries for paths no longer backed by a PV.
//
// The watch removes an entry when it sees a Deleted event, but a watch that
// reconnects restarts from the current resourceVersion, so deletions that
// happened while it was down are never delivered. Nothing else purged those
// entries, which both inflated the applied-quota metric and — because
// ensureQuota returns early on a cache hit — could skip applying a quota to a
// path that a later PV reused. The full list this sync just walked is the
// authority on what still exists.
//
// This is the periodic sync's equivalent of forgetAppliedQuotaForPV's
// tombstone path, including its eventRecorder.Forget call: a PV deleted
// while the watch was disconnected is exactly the case forgetAppliedQuotaForPV
// never runs for (no Deleted event was ever delivered), so without Forget
// here too, events.Recorder's r.last dedup map would keep growing by one
// entry per (ever-seen PV, reason) pair for such PVs forever. appliedQuotaPVNames
// is what makes the PV name available here at all: the PV itself is by
// definition no longer in live/liveNames.
func (a *QuotaAgent) pruneAppliedQuotas(live map[string]struct{}, liveNames map[string]struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()

	for path := range a.appliedQuotas {
		if _, ok := live[path]; !ok {
			delete(a.appliedQuotas, path)
			delete(a.appliedDecisions, path)
			if pvName, ok := a.appliedQuotaPVNames[path]; ok {
				delete(a.appliedQuotaPVNames, path)
				a.eventRecorder.Forget(pvName)
			}
			slog.Debug("Dropped applied-quota cache entry with no matching PV", "path", path)
		}
	}
	for path := range a.appliedDecisions {
		if _, ok := live[path]; !ok {
			delete(a.appliedDecisions, path)
		}
	}
	// shrinkGuardRejectWarned (#92) bounds itself the same way: a path
	// whose PV is gone can't transition states again, so there's nothing
	// left for that entry to rate-limit.
	for path := range a.shrinkGuardRejectWarned {
		if _, ok := live[path]; !ok {
			delete(a.shrinkGuardRejectWarned, path)
		}
	}
	for key := range a.storageClassBindingRejectWarned {
		pvName, _, _ := strings.Cut(key, "\x00")
		if _, ok := liveNames[pvName]; !ok {
			delete(a.storageClassBindingRejectWarned, key)
		}
	}
}

// shouldProcessPV checks if this PV should be processed by the agent
func (a *QuotaAgent) shouldProcessPV(pv *v1.PersistentVolume) bool {
	if pv.Status.Phase != v1.VolumeBound {
		return false
	}

	isNativeNFS := pv.Spec.NFS != nil
	isCSINFS := pv.Spec.CSI != nil && pv.Spec.CSI.Driver == a.provisionerName

	if !isNativeNFS && !isCSINFS {
		return false
	}

	if a.processAllNFS {
		return true
	}

	if isCSINFS {
		return true
	}

	if pv.Annotations == nil {
		return false
	}

	provisioner := pv.Annotations["pv.kubernetes.io/provisioned-by"]
	return provisioner == a.provisionerName
}

// getNFSPath extracts the NFS path from a PV. Delegates to pvpath, the
// single source of truth shared with ui and cleanup.
func (a *QuotaAgent) getNFSPath(pv *v1.PersistentVolume) string {
	return pvpath.NFSPath(pv)
}

// resolveSizeBytes computes the actual byte count that should be enforced
// for pv, given effectiveBytes (a QuotaPolicy-resolved bound, or 0/negative
// to mean "use the PV's own capacity unchanged" -- see ensureQuota's doc
// comment on effectiveBytes for the full contract). Pulled out of
// ensureQuota so the independent drift check in syncAllQuotas (#13's
// Drifted condition) resolves the exact same expected value ensureQuota
// itself would -- duplicating this arithmetic inline at both call sites
// would let them silently diverge if either one changed later, which is
// exactly the class of bug #10's CRITICAL finding was.
func resolveSizeBytes(pv *v1.PersistentVolume, effectiveBytes int64) (int64, error) {
	capacity, ok := pv.Spec.Capacity[v1.ResourceStorage]
	if !ok {
		return 0, fmt.Errorf("PV %s has no storage capacity", pv.Name)
	}
	if effectiveBytes > 0 {
		return effectiveBytes, nil
	}
	return capacity.Value(), nil
}

// ensureQuota ensures the quota is applied for a PV.
//
// effectiveBytes, when positive, overrides the PV's own capacity — this is
// the QuotaPolicy-resolved bound the caller computed via
// quotapolicy.Resolve/EffectiveQuota. Pass 0 (or a negative value) to apply
// the PV's capacity unchanged. syncAllQuotas passes 0 whenever the feature
// is disabled or no policy matches a claim, so the applied value is exactly
// capacityBytes, unchanged from before QuotaPolicy existed. watch.go does
// NOT hardcode 0: see resolveFromSnapshot in policy.go, which resolves
// against the most recent sync cycle's cached policy set so an
// Added/Modified event doesn't ignore QuotaPolicy (or, worse, undo a clamp
// the last sync just applied — see beginQuotaPolicyCycle's doc comment for
// why that would otherwise oscillate forever).
// ensureQuota's public signature is unchanged for its existing callers
// (reconcile_queue.go, every existing test): it just discards the mutated
// signal ensureQuotaMutated now also returns. syncAllQuotas' drift check
// (#13) uses ensureQuotaMutated directly instead of inferring "did this
// call actually change anything" from a before/after cache-value
// comparison -- an independent review caught that inference as
// ABA-vulnerable against a concurrent watch-path reconcile for the same
// claim (a genuinely reachable race: the periodic full sync and the watch
// reconcile queue are separate goroutines that can both call ensureQuota
// for the same PV).
// ensureQuotaWith is ensureQuota's implementation for callers with a
// policyAttempt to attach (the watch path via reconcile_queue.go, carrying
// the provenance resolveFromSnapshot derived at event-enqueue time).
func (a *QuotaAgent) ensureQuotaWith(ctx context.Context, pv *v1.PersistentVolume, effectiveBytes int64, pa *policyAttempt) error {
	_, err := a.ensureQuotaMutatedWith(ctx, pv, effectiveBytes, nil, pa)
	return err
}

// ensureQuota is a convenience wrapper for ensureQuotaWith with pa == nil.
// Non-policy callers pass pa == nil; this preserves any existing live
// QuotaPolicy decision annotation on the PV rather than stripping it.
func (a *QuotaAgent) ensureQuota(ctx context.Context, pv *v1.PersistentVolume, effectiveBytes int64) error {
	return a.ensureQuotaWith(ctx, pv, effectiveBytes, nil)
}

// ensureQuotaMutated is ensureQuota's real implementation, and
// ensureQuotaMutatedWith's wrapper for every caller that has no
// passUsageSnapshot to share -- reconcile_queue.go's watch path (via
// ensureQuotaWith) included, so every watch-triggered apply always pays for
// its own live currentUsageBytes read rather than reusing another PV's
// memoized report (#92). mutated is true only when this specific call
// actually wrote a new value into a.appliedQuotas (the fresh-apply success
// path) -- false for every other return, including the cache-hit no-op
// (already correct, nothing to do) and every error path (nothing was
// durably changed). Callers passing pa == nil preserve existing policy
// decision annotations.
func (a *QuotaAgent) ensureQuotaMutated(ctx context.Context, pv *v1.PersistentVolume, effectiveBytes int64) (mutated bool, err error) {
	return a.ensureQuotaMutatedWith(ctx, pv, effectiveBytes, nil, nil)
}

// policyAttempt carries QuotaPolicy provenance (#14) into
// ensureQuotaMutatedWith for one reconcile attempt: the winning policy
// object and the BoundDecision that produced effectiveBytes for it. nil
// for every attempt with no provenance to attach (no QuotaPolicy matched,
// winner is nil). Both syncAllQuotas (via cycle.resolve) and the watch
// path (via resolveFromSnapshot, reconcileItem, and ensureQuotaWith)
// populate this when a policy shaped the request, so both paths produce
// audit entries with policy provenance.
type policyAttempt struct {
	winner   *v1alpha1.QuotaPolicy
	decision quotapolicy.BoundDecision
}

// newCorrelationID generates a fresh opaque per-attempt identifier (#14):
// 16 random bytes (crypto/rand, stdlib only -- no new dependency), hex
// encoded. Agent-side and ephemeral -- never derived from or persisted to
// any Kubernetes object, so a tenant editing their own PVC has no way to
// influence or predict it (an independent review of #14's design flagged
// exactly that as the risk a correlation ID sourced from a Kubernetes
// object would carry). No error handling here: on Go >=1.24 (go.mod pins
// 1.26) crypto/rand.Read is documented to never return an error -- it
// crashes the program instead if the OS RNG ever fails -- so a fallback
// branch here would be dead code, and worse, an independent review
// pointed out that a reachable fallback returning a fixed value would
// make every attempt in that (impossible) failure window share one ID,
// exactly what the correlation ID's per-attempt uniqueness guarantee
// forbids.
func newCorrelationID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// passUsageSnapshot memoizes, for at most one syncAllQuotas pass, which
// localPaths the fsType-specific usage report knows about -- never a usage
// value itself. #92's targeted cost: a brownfield claim's live
// currentUsageBytes read always comes back !ok (no project ID has ever
// been associated with it, so it can never appear in the report), and the
// guard rejects unconditionally on !ok regardless of *why* -- so N such
// claims repeating the identical rejection every cycle until an operator
// intervenes previously cost N full filesystem-wide report subprocess
// calls per cycle, serialized under a.mu, for no new information. Once
// `fetched` is true, an absent localPath answers (0, false) immediately
// with zero further I/O; a present one still falls through to a live
// read -- every approving decision's freshness is therefore completely
// unchanged, and a path that gains a project ID mid-pass is rejected once
// more (self-correcting, same as before this memo existed) rather than
// silently trusted. See usageForGuard.
type passUsageSnapshot struct {
	fetched bool
	known   map[string]struct{}
	err     error
}

// ensureQuotaMutatedWith is ensureQuotaMutated's real implementation. snap,
// when non-nil, is syncAllQuotas' per-pass passUsageSnapshot (#92); nil
// (every other caller, including the watch path) means "always consult a
// live report," identical to this function's behavior before #92. pa carries
// optional QuotaPolicy provenance (#14) for the audit entry(ies) this call
// produces -- populated by syncAllQuotas' PV loop and watch.go via
// reconcile_queue.go's ensureQuotaWith.
//
// Every call to this function is one reconcile attempt for pv (#14): a
// fresh correlationID is generated here, once, at entry, and threaded into
// every audit entry and structured slog line this specific call emits, so
// they can be joined later without relying on timestamp proximity. This is
// the single choke point both the watch path (via ensureQuota/
// ensureQuotaWith, called from reconcile_queue.go) and the periodic
// syncAllQuotas path go through, which is why generating it here -- rather
// than in either caller -- covers both without changing either caller's
// signature.
func (a *QuotaAgent) ensureQuotaMutatedWith(ctx context.Context, pv *v1.PersistentVolume, effectiveBytes int64, snap *passUsageSnapshot, pa *policyAttempt) (mutated bool, err error) {
	correlationID := newCorrelationID()

	// policyProv is nil whenever pa is nil or matched no policy (pa.winner
	// == nil, e.g. quotaPolicySingleWriter's "matched but no local
	// directory" case in syncAllQuotas passes a policyAttempt through even
	// when resolve found nothing to record) -- see PolicyProvenance's doc
	// comment. Built once, up front, so every audit call below that wants
	// to attach it can just reference the same value.
	//
	// D14: decisionID is a deterministic short hash per (PV, policy UID,
	// policy generation, bound outcome, effective bytes) -- unlike
	// correlationID which is per-attempt. Stored on the PV in
	// AnnotationPolicyDecision on successful apply, and recorded on
	// audit.PolicyProvenance.DecisionID and the structured log line.
	// decisionID hashes raw effectiveBytes as a correlation key (joining
	// admission intent, audit logs, and status), not the filesystem-floored
	// value; nfs.io/enforced-limit-bytes remains the floored value actually
	// enforced on disk.
	// Cost: SHA-256 calculation for policy-bound claims.
	// Escape hatch: if no policy applies, decisionID is empty and
	// AnnotationPolicyDecision is deleted from the PV.
	var policyProv *audit.PolicyProvenance
	var decisionID string
	var policyDecision string
	if pa != nil && pa.winner != nil {
		effective := effectiveBytes
		if effective <= 0 {
			if storageCap, ok := pv.Spec.Capacity[v1.ResourceStorage]; ok {
				effective = storageCap.Value()
			}
		}
		decisionID = quotapolicy.ComputeDecisionID(pv.Name, string(pa.winner.UID), pa.winner.Generation, string(pa.decision.Outcome), effective)
		policyDecision = quotapolicy.FormatPolicyDecision(pa.winner.Name, pa.winner.Generation, string(pa.decision.Outcome), decisionID)
		// PolicyClamped is NOT emitted here: this block runs on every
		// reconcile attempt, including the appliedQuotas cache short-circuit
		// below that returns before any real mutation happens. Emitting here
		// made every no-op resync of an already-clamped PV re-emit
		// PolicyClamped forever (N clamped PVs -> N Normal Events per sync
		// tick, indefinitely). It's emitted instead further down, alongside
		// QuotaApplied, only on the path that actually mutates.
		policyProv = &audit.PolicyProvenance{
			Name:       pa.winner.Name,
			UID:        string(pa.winner.UID),
			Generation: pa.winner.Generation,
			Outcome:    string(pa.decision.Outcome),
			DecisionID: decisionID,
		}
	} else if pa == nil {
		// Non-policy callers pass pa == nil; preserve any existing live
		// QuotaPolicy decision annotation on the PV rather than clearing it.
		policyDecision = policyDecisionPreserve
	}

	// Checked before taking a.mu: HAActive() is just a stat call, and a
	// standby instance should never even contend for the lock over work
	// it's about to skip. See haActiveFile's doc comment (#11) -- this is
	// the actual mutation gate the acceptance criterion "standby agent는
	// ownership이 확인되기 전 quota mutation을 수행하지 않는다" asks for;
	// everywhere else (RemoveOrphan) has the identical check for the same
	// reason. Returns ErrHAStandby, not nil -- see its doc comment
	// (ha.go) for why a silent nil here would be a QuotaPolicy accounting
	// lie, and every caller of ensureQuota for how each one must treat
	// this specific error as "correctly skipped," not a failure.
	if !a.HAActive() {
		slog.Debug("Skipping quota mutation: this instance is HA standby", "pv", pv.Name, "correlation_id", correlationID)
		return false, ErrHAStandby
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	sizeBytes, err := resolveSizeBytes(pv, effectiveBytes)
	if err != nil {
		return false, err
	}

	// enforcedBytes is what the backend will actually be asked to enforce
	// for sizeBytes -- computed up front, once, so every comparison against
	// appliedQuotas below (the cache short-circuit, isUpdate, the shrink
	// guard) and every write to appliedQuotas/the enforced-limit annotation
	// stays in the same unit. Comparing/storing raw sizeBytes anywhere in
	// this mix was #90(c): XFS/ext4 floor to whole KB, so a sub-KB *expansion*
	// request could floor to a value below the last stored (also-floored)
	// enforced value and get misclassified as a shrink.
	enforcedBytes := quota.ExpectedEnforcedBytes(a.fsType, sizeBytes)

	nfsPath := a.getNFSPath(pv)
	if nfsPath == "" {
		return false, fmt.Errorf("PV %s has no NFS path", pv.Name)
	}
	var namespace, pvcName string
	if pv.Spec.ClaimRef != nil {
		namespace = pv.Spec.ClaimRef.Namespace
		pvcName = pv.Spec.ClaimRef.Name
	}
	pathResult := a.nfsPathToLocalResult(nfsPath)
	localPath := pathResult.Path
	if pa != nil && pa.winner != nil && len(pa.winner.Spec.Selector.StorageClassNames) > 0 && pathResult.Fallback {
		bindingErr := fmt.Errorf("%w: PV %s NFS path does not match configured server path", errStorageClassBindingPathFallback, pv.Name)
		rejectionKey := storageClassBindingRejectionKey(pv.Name, localPath)
		if _, alreadyWarned := a.storageClassBindingRejectWarned[rejectionKey]; !alreadyWarned {
			a.storageClassBindingRejectWarned[rejectionKey] = struct{}{}
			// The audit event and metric both describe entry into the rejected
			// state. Repeated queue retries are intentionally silent here.
			if a.auditLogger != nil {
				a.auditLogger.LogBindingRejected(pv.Name, namespace, pvcName, nfsPath, bindingErr,
					audit.AttemptContext{CorrelationID: correlationID, Policy: policyProv})
			}
			a.storageClassBindingRejections[v1alpha1.ReasonStorageClassBindingPathFallbackRejected]++
			slog.Warn("Refusing StorageClass-bound quota apply: NFS path mapping used basename fallback",
				"pv", pv.Name, "nfsPath", nfsPath, "correlation_id", correlationID)
		}
		_ = a.updateQuotaStatus(ctx, pv, QuotaStatusFailed, 0, correlationID, "")
		return false, bindingErr
	}

	if _, statErr := os.Stat(localPath); os.IsNotExist(statErr) {
		slog.Warn("Directory does not exist, skipping quota", "path", localPath, "pv", pv.Name, "correlation_id", correlationID)
		return false, nil
	}

	if existingQuota, exists := a.appliedQuotas[localPath]; exists && existingQuota == enforcedBytes {
		if policyDecision != policyDecisionPreserve && policyDecision != a.appliedDecisions[localPath] {
			if err := a.updateQuotaStatus(ctx, pv, QuotaStatusApplied, enforcedBytes, correlationID, policyDecision); err != nil {
				// updateQuotaStatus logs errors; leave appliedDecisions unchanged
				// so the next sync retries, and skip the audit entry (audit package
				// does not model failed decision_updated entries).
				slog.Error("Failed to update PV quota status on policy decision refresh; will retry next sync",
					"pv", pv.Name, "path", localPath, "error", err, "correlation_id", correlationID)
			} else {
				if policyDecision != "" {
					a.appliedDecisions[localPath] = policyDecision
				} else {
					delete(a.appliedDecisions, localPath)
				}
				if a.auditLogger != nil {
					a.auditLogger.LogDecisionUpdated(pv.Name, namespace, pvcName, localPath, sizeBytes, a.fsType,
						audit.AttemptContext{CorrelationID: correlationID, EnforcedQuota: enforcedBytes, Policy: policyProv})
				}
				if decisionID != "" {
					slog.Info("Quota policy decision refreshed on cache hit",
						"pv", pv.Name,
						"path", localPath,
						"capacity", util.FormatBytes(sizeBytes),
						"correlation_id", correlationID,
						"decision_id", decisionID,
					)
				}
			}
		}
		return false, nil
	}

	projectName := a.getProjectName(pv)
	projectID, err := a.generateProjectID(projectName)
	if err != nil {
		if a.auditLogger != nil {
			a.auditLogger.LogProjectIDAllocationFailure(pv.Name, namespace, pvcName, localPath, projectName, err,
				audit.AttemptContext{CorrelationID: correlationID, Policy: policyProv})
		}
		_ = a.updateQuotaStatus(ctx, pv, QuotaStatusFailed, 0, correlationID, "")
		return false, fmt.Errorf("failed to allocate project ID for PV %s: %w", pv.Name, err)
	}

	oldQuota := a.appliedQuotas[localPath]
	isUpdate := oldQuota > 0 && oldQuota != enforcedBytes

	// oldQuota (enforced-valued, see above) and sizeBytes (raw, unfloored)
	// are passed side by side into every LogQuotaUpdate call below and
	// further down -- that's an intentional, not accidental, unit
	// difference: oldQuota is "what was actually enforced by the previous
	// apply" (there is no raw record of that request left anywhere to log
	// instead), while sizeBytes/NewQuota is "what was requested this time,"
	// which for a rejected apply is also the only value worth showing a
	// human deciding whether to retry with a larger request. Flooring
	// sizeBytes here to match oldQuota would hide exactly the number an
	// operator needs; storing the raw pre-flooring request in appliedQuotas
	// instead would resurrect #90(c). The audit log's OldQuota/NewQuota
	// pair is therefore deliberately "last enforced" vs "this request," not
	// "enforced" vs "enforced" or "raw" vs "raw" -- a reader of audit.Entry
	// comparing them byte-for-byte across a KB-flooring backend should
	// expect NewQuota to already reflect the true delta approximately, not
	// exactly.

	// currentEnforced is what the shrink guard below treats as "the real
	// quota already in force for this claim." Prefer appliedQuotas
	// (oldQuota): it's this process's own record of its last successful
	// apply, now stored as the enforced (KB-floored, for XFS/ext4) value --
	// see enforcedBytes' doc comment above for why raw sizeBytes was wrong
	// here. Only fall back to priorEnforcedFromDisk -- a one-time snapshot of
	// the real on-disk report taken during Run()'s startup sequence, before the first sync
	// -- when appliedQuotas has no entry at all. That fallback closes a
	// real gap an independent review caught: appliedQuotas is purely
	// in-memory, so oldQuota reads as 0 for the first touch of every claim
	// in a fresh process, including one whose on-disk quota was already
	// lower than current usage before this process ever started -- gating
	// the shrink guard on oldQuota alone let that exact case bypass it
	// entirely.
	//
	// The snapshot is taken once in Run(), not lazily here on first use:
	// an earlier version called primeAppliedQuotasFromDiskOnce from this
	// function instead, which works for a real restart but silently
	// injects an extra quota-report subprocess call the first time any
	// test drives ensureQuota/syncAllQuotas directly without going through
	// Run() (most of this package's tests do exactly that) -- breaking
	// fake-runner tests that count or order-inject failures on specific
	// calls (TestPVReconcileQueueRetriesFailedItems) and, at production
	// scale, turning a burst of many first-touches (many new PVs created
	// at once) into one report fetch each instead of the one total a
	// single Run()-time snapshot costs (TestWatchPVsEventStormAtScale
	// caught that as a 10s timeout). A test that wants this fallback
	// populated calls primeAppliedQuotasFromDiskOnce itself, the same way
	// Run() does, rather than relying on it firing implicitly.
	currentEnforced := uint64(oldQuota)
	if currentEnforced == 0 {
		currentEnforced = a.priorEnforcedFromDisk[localPath]
	}

	// isShrink is the original, unambiguous case: a real enforced quota
	// already exists for this claim and the new request is lower than it.
	//
	// suspectBrownfield is #90(a): a claim can hold real, already-written
	// data while currentEnforced is still 0 -- nothing was ever recorded in
	// appliedQuotas or priorEnforcedFromDisk because no quota was ever
	// applied to it (a pre-existing NFS export brought under agent
	// management, or the agent's first deployment against a server that
	// already has data). isShrink alone would skip the guard entirely and
	// let a small hard limit land on a large directory with no warning.
	// priorUsageFromDisk -- populated for free alongside priorEnforcedFromDisk,
	// see its doc comment -- gives a reason to suspect that without an extra
	// subprocess call: if the startup snapshot already saw more usage at
	// this path than the new request would enforce, that's suspicious enough
	// to justify paying for the same authoritative live read isShrink pays
	// for below.
	//
	// Gated on currentEnforced == 0, not !isShrink: a review caught that
	// !isShrink also covers a *grow* on a path that already has an enforced
	// quota (currentEnforced > 0, enforcedBytes >= currentEnforced) -- e.g.
	// TestEnsureQuota_AllowsGrowThatDoesNotFullyClearOverQuota's scenario, a
	// legitimate increase on a directory that's already over its old quota.
	// priorUsageFromDisk is a startup-time snapshot that's never refreshed,
	// so gating on !isShrink there would permanently reject every future
	// grow (and every same-value re-apply) on any claim that was ever over
	// quota at startup, even after the new, larger quota would clear it --
	// currentEnforced == 0 is the actual brownfield condition: no quota has
	// ever been recorded for this claim at all, in this process or on disk.
	isShrink := currentEnforced > 0 && uint64(enforcedBytes) < currentEnforced
	suspectBrownfield := currentEnforced == 0 && a.priorUsageFromDisk[localPath] > uint64(enforcedBytes)

	if isShrink || suspectBrownfield {
		// #90(b): a report failure (or, for suspectBrownfield, a path the
		// report has no entry for at all -- e.g. no project ID has ever been
		// associated with it) must REJECT, not pass through. currentUsageBytes
		// used to return ok=true with a value silently substituted by
		// GetDirUsages' apparent-size fallback on a report failure, which this
		// guard then trusted as if it were authoritative; "unknown" is not a
		// safe "no" for a check that exists specifically to answer "would
		// this immediately put the volume over its new limit." usageForGuard
		// (#92) consults snap first when one was given -- see its doc
		// comment for why that's safe here.
		used, ok := a.usageForGuard(localPath, snap)
		if !ok || used > uint64(enforcedBytes) {
			priorUsage := a.priorUsageFromDisk[localPath]
			var shrinkErr error
			switch {
			case !ok && suspectBrownfield:
				// The headline #90(a) case: there's no project ID associated
				// with this path yet, so a live report read can never find
				// it -- !ok here does NOT mean "we know nothing," it means
				// "we can't get a fresher number than the one that already
				// triggered our suspicion." Naming priorUsage explicitly
				// (instead of the generic !ok message below) tells an
				// operator why this was refused instead of implying total
				// ignorance.
				shrinkErr = fmt.Errorf("%w: PV %s has no project quota associated with it yet, so its current usage can't be confirmed via a live read -- refusing because the startup snapshot recorded %s already used at %s, which the requested %s (enforced as %s) would not cover",
					errUnsafeShrink, pv.Name, util.FormatBytes(int64(priorUsage)), localPath, util.FormatBytes(sizeBytes), util.FormatBytes(enforcedBytes))
			case !ok:
				shrinkErr = fmt.Errorf("%w: current usage for PV %s could not be determined (usage report failed or has no entry for this path)",
					errUnsafeShrink, pv.Name)
			default:
				shrinkErr = fmt.Errorf("%w: new quota %s (enforced as %s) is below current usage %s for PV %s",
					errUnsafeShrink, util.FormatBytes(sizeBytes), util.FormatBytes(enforcedBytes), util.FormatBytes(int64(used)), pv.Name)
			}
			if a.auditLogger != nil {
				a.auditLogger.LogQuotaUpdate(pv.Name, localPath, projectName, projectID, oldQuota, sizeBytes, a.fsType, shrinkErr,
					audit.AttemptContext{CorrelationID: correlationID, Policy: policyProv})
			}
			_ = a.updateQuotaStatus(ctx, pv, QuotaStatusFailed, 0, correlationID, "")
			// Rate-limited to once per path per state transition (#92): a
			// brownfield claim that stays rejected repeats this exact
			// outcome every syncInterval forever until an operator
			// intervenes, which used to mean one identical warning per PV
			// every cycle for as long as it remains unresolved. Only the
			// transition INTO rejection logs; a successful apply below
			// clears the path's entry so a later rejection (a fresh state
			// transition) logs again.
			if _, alreadyWarned := a.shrinkGuardRejectWarned[localPath]; !alreadyWarned {
				a.shrinkGuardRejectWarned[localPath] = struct{}{}
				slog.Warn("Refusing quota apply: current usage is unsafe or unknown",
					"pv", pv.Name, "path", localPath,
					"currentEnforced", util.FormatBytes(int64(currentEnforced)), "requestedQuota", util.FormatBytes(sizeBytes),
					"usageKnown", ok, "currentUsage", util.FormatBytes(int64(used)),
					"suspectBrownfield", suspectBrownfield, "priorUsageFromDiskSnapshot", util.FormatBytes(int64(priorUsage)),
					"correlation_id", correlationID)
			}
			return false, shrinkErr
		}
	}

	err = a.applyQuota(localPath, projectName, projectID, sizeBytes)
	if err != nil {
		a.eventRecorder.Event(pv, events.TypeWarning, events.QuotaApplyFailed,
			"Failed to apply %s quota for PV %s: %v", util.FormatBytes(sizeBytes), pv.Name, err)
	}

	// Read-back verification (#10) runs before the CREATE/UPDATE audit
	// entry, not after: the quota binary exiting 0 means the command
	// succeeded, not that the kernel actually holds the limit we asked
	// for. Auditing "success" here and a VERIFY_FAILED entry right behind
	// it on a verification failure would leave two contradictory entries
	// for the same apply -- anything reading the audit log for "was this
	// quota applied" (the web UI's audit view) would trust the first one
	// and be wrong. err is folded into a single final outcome instead, so
	// exactly one CREATE/UPDATE entry reflects what actually happened.
	if err == nil {
		if verifyErr := a.verifyQuotaOnDisk(localPath, sizeBytes); verifyErr != nil {
			a.verificationFailures.Add(1)
			err = fmt.Errorf("quota apply succeeded but read-back verification failed for PV %s: %w: %w", pv.Name, errQuotaVerificationFailed, verifyErr)
			a.eventRecorder.Event(pv, events.TypeWarning, events.QuotaVerificationFailed,
				"Quota apply for PV %s succeeded but read-back verification failed: %v", pv.Name, verifyErr)
			if a.auditLogger != nil {
				// enforcedBytes is included here even though the overall
				// attempt is about to be recorded as failed below: the
				// backend DID accept enforcedBytes at the apply step
				// (applyQuota returned nil) -- it's the read-back that
				// disagreed -- so this is exactly the number the mismatch
				// is against, not a claim that the attempt succeeded.
				a.auditLogger.LogQuotaVerificationFailure(pv.Name, localPath, projectName, projectID, sizeBytes, a.fsType, verifyErr,
					audit.AttemptContext{CorrelationID: correlationID, EnforcedQuota: enforcedBytes, Policy: policyProv})
			}
		}
	}

	// EnforcedQuota is only ever attached to the audit entry on the success
	// path (err == nil here, after the read-back verification above) -- see
	// audit.Entry.EnforcedQuota's doc comment. A failed apply never had
	// anything enforced to record.
	attempt := audit.AttemptContext{CorrelationID: correlationID, Policy: policyProv}
	if err == nil {
		attempt.EnforcedQuota = enforcedBytes
	}
	if a.auditLogger != nil {
		if isUpdate {
			a.auditLogger.LogQuotaUpdate(pv.Name, localPath, projectName, projectID, oldQuota, sizeBytes, a.fsType, err, attempt)
		} else {
			a.auditLogger.LogQuotaCreate(pv.Name, namespace, pvcName, localPath, projectName, projectID, sizeBytes, a.fsType, err, attempt)
		}
	}

	if err != nil {
		_ = a.updateQuotaStatus(ctx, pv, QuotaStatusFailed, 0, correlationID, "")
		return false, err
	}

	// Stored/reported as enforcedBytes, not raw sizeBytes -- see
	// enforcedBytes' doc comment above (#90(c)): appliedQuotas backs the
	// shrink guard's currentEnforced/isUpdate comparisons, and
	// AnnotationEnforcedLimitBytes is documented as "what the filesystem
	// enforces," not what was requested.
	a.appliedQuotas[localPath] = enforcedBytes
	a.appliedQuotaPVNames[localPath] = pv.Name
	if policyDecision != policyDecisionPreserve {
		if policyDecision != "" {
			a.appliedDecisions[localPath] = policyDecision
		} else {
			delete(a.appliedDecisions, localPath)
		}
	}
	// A successful apply ends any rejected streak the guard's rate-limited
	// warning above was tracking, so a future rejection for this path (a
	// genuinely new state transition) logs again instead of staying
	// silent forever.
	delete(a.shrinkGuardRejectWarned, localPath)
	delete(a.storageClassBindingRejectWarned, storageClassBindingRejectionKey(pv.Name, localPath))
	_ = a.updateQuotaStatus(ctx, pv, QuotaStatusApplied, enforcedBytes, correlationID, policyDecision)

	a.eventRecorder.Event(pv, events.TypeNormal, events.QuotaApplied,
		"Quota set to %s for PV %s", util.FormatBytes(sizeBytes), pv.Name)
	// PolicyClamped, moved here from the top of this function (see the
	// comment there): this line only runs on the path that just mutated
	// appliedQuotas above, so a resync that hits the cache short-circuit
	// never re-emits it.
	if pa != nil && pa.winner != nil && pa.decision.Outcome == quotapolicy.BoundClampedToMax {
		a.eventRecorder.Event(pv, events.TypeNormal, events.PolicyClamped,
			"QuotaPolicy %s clamped PV %s to its maxQuota: %s", pa.winner.Name, pv.Name, pa.decision.Detail)
	}
	// QuotaExceeded/QuotaNearLimit (events.Reason) are declared for the
	// ADR's vocabulary but deliberately NOT wired here: the only usage
	// figure available at this point is usageForGuard/currentUsageBytes,
	// which always pays for a live quota-report subprocess call regardless
	// of whether snap is shared -- see usageForGuard's doc comment. Calling
	// it unconditionally on every successful apply reintroduces exactly
	// the extra-subprocess-per-apply cost #92 removed and
	// TestEnsureQuota_EmptyBrownfieldDirectoryAppliesWithoutExtraUsageRead
	// guards against; there is no cheap existing call site for a per-PV
	// usage-vs-limit transition today. Left for a follow-up that threads a
	// real per-cycle usage cache through this path.

	if decisionID != "" {
		slog.Info("Quota applied successfully",
			"pv", pv.Name,
			"path", localPath,
			"capacity", util.FormatBytes(sizeBytes),
			"correlation_id", correlationID,
			"decision_id", decisionID,
		)
	} else {
		slog.Info("Quota applied successfully",
			"pv", pv.Name,
			"path", localPath,
			"capacity", util.FormatBytes(sizeBytes),
			"correlation_id", correlationID,
		)
	}

	return true, nil
}

// currentUsageBytes returns the fsType-specific quota report's authoritative
// usage for localPath, so ensureQuota can refuse a quota apply that would
// immediately put the volume over its new limit. It deliberately uses
// status.GetReportedUsage, not status.GetDirUsages: GetDirUsages swallows a
// report failure into an empty map and falls back to an apparent-size
// filepath.Walk for any path the report has no entry for, which made a
// report hiccup indistinguishable from "usage is genuinely zero" -- #90(b)
// caught this guard trusting that silently-substituted value as if it were
// authoritative. ok is false both when the report command itself failed and
// when it succeeded but has no entry for localPath (e.g. no project ID has
// ever been associated with it); ensureQuota's caller treats either as
// "unknown" and fails closed (rejects), not "zero usage, safe to proceed" --
// see ensureQuota's shrink guard for why unknown must not be treated as
// safe.
//
// Two accepted, undocumented-elsewhere limitations: (1) called from
// ensureQuota, this runs while a.mu is held and invokes a real quota-report
// subprocess -- read-only, but unbounded and without a timeout, so a slow
// report blocks every other serialized reconcile for as long as it takes.
// Closing this would need a context-aware, cancellable usage read that
// status.GetReportedUsage doesn't support today -- a reasonable follow-up,
// not required for this guard to be a net safety improvement over not
// checking at all. (2) it fetches the WHOLE filesystem-wide report to read
// one path's usage, once per PV that needs it, with no per-cycle caching --
// fetchQuotaReport below already has the memo pattern this could reuse (one
// fetch per syncAllQuotas cycle, shared across every PV that needs it), but
// this function doesn't use it. With #90's suspectBrownfield fix in place
// the blast radius is limited to genuinely ambiguous claims (a real shrink,
// or a brownfield claim the startup snapshot flagged), but those stay
// ambiguous until an operator acts, so N such claims still cost N
// subprocess calls, serialized under a.mu, every single sync cycle for as
// long as they remain unresolved. Flagged for a follow-up, not fixed here:
// reusing fetchQuotaReport's memo would need it to also expose the usage
// map (today it discards everything but quotaMap), and to be threaded
// through ensureQuota's per-PV call site the way syncAllQuotas' drift check
// already threads its own report fetch.
func (a *QuotaAgent) currentUsageBytes(localPath string) (used uint64, ok bool) {
	usageMap, err := status.GetReportedUsage(a.nfsBasePath, a.fsType, a.projectsFile, a.projidFile)
	if err != nil {
		return 0, false
	}
	used, ok = usageMap[localPath]
	return used, ok
}

// usageForGuard answers the shrink guard's "what is localPath's current
// usage" question (#92). snap == nil (every caller other than
// syncAllQuotas' own PV loop) always falls straight through to a live
// currentUsageBytes read, identical to this guard's behavior before #92.
//
// With a non-nil snap, the report is fetched at most once for the whole
// pass (lazily, on the first PV that reaches this guard at all) and
// memoizes only which localPaths it contains -- never a usage value. A
// path ABSENT from that membership set answers (0, false) immediately,
// with no further I/O: absence is exactly what the guard already treats
// as "reject" regardless of freshness (no project ID has ever been
// associated with the path, so a live re-read could never find it
// either), which is what makes reusing this answer across every
// brownfield PV in the pass safe. A path PRESENT in the report still
// falls through to its own live currentUsageBytes call -- a real shrink
// candidate's approval depends on a value that can change from one PV to
// the next, so presence is never served from cache. A memoized fetch
// error is treated the same as "everything is absent" (0, false) for
// every subsequent caller this pass, logged once here instead of once per
// PV.
func (a *QuotaAgent) usageForGuard(localPath string, snap *passUsageSnapshot) (used uint64, ok bool) {
	if snap == nil {
		return a.currentUsageBytes(localPath)
	}
	if !snap.fetched {
		snap.fetched = true
		usageMap, err := status.GetReportedUsage(a.nfsBasePath, a.fsType, a.projectsFile, a.projidFile)
		if err != nil {
			snap.err = err
			slog.Warn("Could not check current usage this cycle: on-disk usage report unavailable; every shrink/brownfield guard check this cycle will reject", "error", err)
		} else {
			known := make(map[string]struct{}, len(usageMap))
			for path := range usageMap {
				known[path] = struct{}{}
			}
			snap.known = known
		}
	}
	if snap.err != nil {
		return 0, false
	}
	if _, present := snap.known[localPath]; !present {
		return 0, false
	}
	return a.currentUsageBytes(localPath)
}

// primeAppliedQuotasFromDiskOnceLogEvery rate-limits
// primeAppliedQuotasFromDiskOnce's failure slog.Warn to once every N
// consecutive-since-start failures, instead of once per syncAllQuotas cycle
// forever -- at the default 30s sync interval that's roughly once every 5
// minutes while the report stays unreadable.
const primeAppliedQuotasFromDiskOnceLogEvery = 10

// primeAppliedQuotasFromDiskOnce populates priorEnforcedFromDisk and
// priorUsageFromDisk from one filesystem-wide *strict* quota report read
// (status.GetDirUsagesStrict) -- see priorEnforcedFromDisk's doc comment on
// the QuotaAgent struct for why ensureQuotaMutated's shrink guard needs
// this. Called from Run() before the first sync, and again at the top of
// every syncAllQuotas cycle (#93): unlike the sync.Once-guarded version
// this replaces, it is safe -- and expected -- to call repeatedly. Once
// a.primed is true (set only after a nil-error read has filled both maps
// under a.mu) every later call is a single a.mu check and an immediate
// return: no subprocess, no re-fetch, matching the "one report fetch, not
// N, for a burst of first-touches" cost guarantee
// TestWatchPVsEventStormAtScale and
// TestEnsureQuota_EmptyBrownfieldDirectoryAppliesWithoutExtraUsageRead rely
// on.
//
// GetDirUsagesStrict, not GetDirUsages, is deliberate: GetDirUsages
// swallows a report failure into an empty result, which would make this
// function believe an unreadable report means "no quotas exist yet" and
// mark itself primed off that false negative -- exactly the failure mode
// #93 exists to close. A read that errors here leaves a.primed false,
// increments primeFailures, and is retried on the very next syncAllQuotas
// cycle instead of being a permanent, process-lifetime miss the way the
// old sync.Once version was.
//
// Unprimed stays fail-open by design, not fail-closed: while a.primed is
// false, priorUsageFromDisk is empty, so suspectBrownfield (in
// ensureQuotaMutated) can never fire and a data-holding directory that's
// never had a quota applied gets one silently accepted at whatever size is
// requested -- see ShrinkGuardPrimed's doc comment for why fail-closed
// would be worse here: it would turn a transient report fault into an
// enforcement outage for every first-touch apply on an otherwise healthy,
// greenfield node. isShrink's currentEnforced > 0 branch is unaffected
// either way; only the brownfield heuristic is disarmed while unprimed.
// nfs_quota_agent_shrink_guard_primed and
// nfs_quota_agent_shrink_guard_prime_failures_total (internal/metrics) make
// this window observable instead of silent.
//
// Not called from the watch path (reconcile_queue.go): a late prime only
// changes the outcome for a claim whose currentEnforced is still 0 (no
// quota has ever been recorded for it, in this process or on disk) --
// nothing this agent applies between "unprimed" and "primed" changes what
// that comparison means, so retrying once per syncAllQuotas cycle (rather
// than once per watch-triggered reconcile too) is sufficient.
func (a *QuotaAgent) primeAppliedQuotasFromDiskOnce() {
	a.mu.Lock()
	primed := a.primed
	a.mu.Unlock()
	if primed {
		return
	}

	usages, err := status.GetDirUsagesStrict(a.nfsBasePath, a.fsType, a.projectsFile, a.projidFile)
	if err != nil {
		n := a.primeFailures.Add(1)
		if n%primeAppliedQuotasFromDiskOnceLogEvery == 1 {
			slog.Warn("Could not prime the shrink guard's on-disk quota snapshot; will retry next sync cycle",
				"error", err, "consecutiveFailures", n)
		}
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	for _, u := range usages {
		if u.Quota > 0 {
			a.priorEnforcedFromDisk[u.Path] = u.Quota
		}
		// Recorded for every path, not just quota>0: see
		// priorUsageFromDisk's doc comment on the QuotaAgent struct for
		// why this closes the brownfield shrink-guard gap (#90) at zero
		// extra subprocess cost -- GetDirUsagesStrict already returned
		// u.Used.
		a.priorUsageFromDisk[u.Path] = u.Used
	}
	a.primed = true
}

// ShrinkGuardPrimed reports whether the shrink guard's brownfield-suspicion
// snapshot (priorUsageFromDisk/priorEnforcedFromDisk) has been successfully
// populated from an on-disk report at least once since process start.
// False means suspectBrownfield can never fire yet -- the guard is
// fail-open, not fail-closed, for that specific check -- see
// primeAppliedQuotasFromDiskOnce's doc comment for why. Read by the
// nfs_quota_agent_shrink_guard_primed metric, the alertable signal for this
// (internal/metrics.AgentInfo).
func (a *QuotaAgent) ShrinkGuardPrimed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.primed
}

// ShrinkGuardPrimeFailures returns the cumulative count of failed
// primeAppliedQuotasFromDiskOnce attempts (report fetch or directory read
// error) since process start. Never reset, including after a later
// success -- a Prometheus counter, matching ReconcileStats' totals. Read by
// the nfs_quota_agent_shrink_guard_prime_failures_total metric
// (internal/metrics.AgentInfo).
func (a *QuotaAgent) ShrinkGuardPrimeFailures() int64 {
	return a.primeFailures.Load()
}

// StorageClassBindingRejections returns a copy so metrics rendering cannot
// race with reconciliation updates.
func (a *QuotaAgent) StorageClassBindingRejections() map[string]int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]int64, len(a.storageClassBindingRejections))
	for reason, count := range a.storageClassBindingRejections {
		out[reason] = count
	}
	return out
}

// nfsPathToLocal converts NFS server path to local mount path. Delegates to
// pvpath.ToLocal for the mapping itself and keeps the fallback warning here,
// since logging is agent-specific behavior other callers of pvpath don't want.
func (a *QuotaAgent) nfsPathToLocal(nfsPath string) string {
	return a.nfsPathToLocalResult(nfsPath).Path
}

// nfsPathToLocalResult preserves pvpath.ToLocal's fallback signal for
// callers that must fail closed instead of accepting an ambiguous basename.
func (a *QuotaAgent) nfsPathToLocalResult(nfsPath string) pvpath.LocalPath {
	result := pvpath.ToLocal(nfsPath, a.nfsServerPath, a.nfsBasePath)
	if result.Fallback {
		// Using filepath.Base risks collision if multiple NFS paths share the same basename.
		// Log a warning so operators can fix nfs-server-path configuration.
		slog.Warn("NFS path does not match server path prefix, using basename as fallback — check --nfs-server-path configuration",
			"nfsPath", nfsPath,
			"nfsServerPath", a.nfsServerPath,
			"fallbackLocalPath", result.Path,
		)
	}
	return result
}

// getProjectName gets or generates project name for a PV
func (a *QuotaAgent) getProjectName(pv *v1.PersistentVolume) string {
	if pv.Annotations != nil {
		if name, ok := pv.Annotations[AnnotationProjectName]; ok && name != "" {
			return name
		}
	}
	name := strings.ReplaceAll(pv.Name, "-", "_")
	if len(name) > 32 {
		name = name[:32]
	}
	return "pv_" + name
}

// maxProjectIDProbe bounds how many consecutive IDs generateProjectID will
// try past the initial hash before giving up. The hash spreads names
// roughly uniformly across ~2^32 values, so a chain of collisions this long
// is astronomically unlikely for any realistic PV count; hitting the bound
// means knownProjectIDs is corrupted or pathological (e.g. duplicate/crafted
// entries), not that the ID space is genuinely exhausted. Bounding the probe
// turns that case into a reported error instead of an infinite loop.
const maxProjectIDProbe = 4096

// errProjectIDExhausted is returned by generateProjectID when no free ID
// was found within maxProjectIDProbe attempts.
var errProjectIDExhausted = fmt.Errorf("no available project ID found within %d attempts", maxProjectIDProbe)

// errUnsafeShrink is returned by ensureQuota when it refuses to apply a
// quota decrease that its own currentUsageBytes check found would put the
// volume over its new limit immediately -- see ensureQuota's shrink guard
// (#14: "shrink는 unsupported/unsafe 조건에서... 명확히 거부한다").
var errUnsafeShrink = errors.New("quota decrease rejected: below current on-disk usage")

var errStorageClassBindingPathFallback = errors.New("StorageClass binding rejected: ambiguous NFS path fallback")

// errQuotaVerificationFailed marks an ensureQuota error that originated
// from verifyQuotaOnDisk's post-apply read-back check (the apply command
// itself succeeded) rather than from applyQuota failing outright. Wrapped
// into the error ensureQuotaMutatedWith returns solely so
// reconcile_queue.go's retry-reason classification
// (nfs_quota_agent_reconcile_retries_total{reason}) can tell
// "verify_failed" apart from "apply_error" via errors.Is, without
// reconcile_queue.go needing any knowledge of verifyQuotaOnDisk's own
// error types.
var errQuotaVerificationFailed = errors.New("quota verification failed")

// storageClassBindingRejectionKey cannot use localPath alone: two PVs can
// legitimately map to one basename fallback path, yet each has its own state.
func storageClassBindingRejectionKey(pvName, localPath string) string {
	return pvName + "\x00" + localPath
}

// generateProjectID generates a unique numeric project ID from project name.
// Uses the in-memory knownProjectIDs cache (refreshed once per sync cycle).
// Must be called with a.mu held.
func (a *QuotaAgent) generateProjectID(projectName string) (uint32, error) {
	id := a.hashProjectName(projectName)

	for attempt := 0; attempt < maxProjectIDProbe; attempt++ {
		if existingName, taken := a.knownProjectIDs[id]; !taken || existingName == projectName {
			a.knownProjectIDs[id] = projectName // update cache for subsequent calls this cycle
			return id, nil
		}
		// Collision: different project already owns this ID, try next
		slog.Warn("Project ID collision detected, trying next ID",
			"projectName", projectName,
			"conflictingName", a.knownProjectIDs[id],
			"id", id,
		)
		id++
		if id == 0 {
			id = 1 // avoid ID 0
		}
	}

	return 0, fmt.Errorf("%w for project %q", errProjectIDExhausted, projectName)
}

// hashProjectName computes the initial FNV-1 hash for a project name
func (a *QuotaAgent) hashProjectName(projectName string) uint32 {
	var hash uint32 = 2166136261
	for _, c := range projectName {
		hash ^= uint32(c)
		hash *= 16777619
	}
	return (hash % 4294967293) + 1
}

// loadExistingProjectIDs reads /etc/projid and returns a map of projectID -> projectName
func (a *QuotaAgent) loadExistingProjectIDs() map[uint32]string {
	existing := make(map[uint32]string)
	data, err := os.ReadFile(a.projidFile)
	if err != nil {
		return existing
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			if id, err := strconv.ParseUint(parts[1], 10, 32); err == nil {
				existing[uint32(id)] = parts[0]
			}
		}
	}
	return existing
}

// orphanedProjectIDSentinel marks a knownProjectIDs entry for an ID that
// CheckProjectFileConsistency would report: present in the projects file
// but with no matching name in projid. It contains a control character,
// which validateQuotaArg rejects from every real project name before it
// can reach a quota command, so this value can never equal — and can
// never be handed out as — a legitimate project name.
const orphanedProjectIDSentinel = "\x00orphaned-project-id"

// markOrphanedProjectIDsAsTaken folds every ID present in the projects file
// but absent from projid into ids as taken.
//
// loadExistingProjectIDs only reads projid, so an orphaned ID (a
// pre-existing half-applied state — see CheckProjectFileConsistency) looks
// free to generateProjectID. Hashing a new project onto it would let
// AddProject's identity validation correctly reject the mismatch every
// single cycle, but the hash is deterministic, so without this the same
// project would retry the same doomed ID forever instead of the probe
// moving on to one that's actually free. Errors reading the projects file
// are logged and otherwise ignored: it's an availability hardening pass
// over an already-computed cache, not a correctness requirement for this
// sync cycle to proceed.
func (a *QuotaAgent) markOrphanedProjectIDsAsTaken(ids map[uint32]string) {
	projects, err := quota.ReadProjectsFile(a.projectsFile)
	if err != nil {
		slog.Warn("Failed to read projects file while checking for orphaned project IDs", "error", err)
		return
	}
	for idStr := range projects {
		id, err := strconv.ParseUint(idStr, 10, 32)
		if err != nil {
			continue
		}
		if _, known := ids[uint32(id)]; !known {
			ids[uint32(id)] = orphanedProjectIDSentinel
		}
	}
}

// applyQuota applies project quota based on filesystem type
func (a *QuotaAgent) applyQuota(path, projectName string, projectID uint32, sizeBytes int64) error {
	switch a.fsType {
	case quota.FSTypeXFS:
		return quota.ApplyXFSQuota(a.quotaPath, path, projectName, projectID, sizeBytes, a.projectsFile, a.projidFile)
	case quota.FSTypeExt4:
		return quota.ApplyExt4Quota(a.quotaPath, path, projectName, projectID, sizeBytes, a.projectsFile, a.projidFile)
	case quota.FSTypeBtrfs:
		return quota.ApplyBtrfsQuota(path, sizeBytes)
	default:
		return fmt.Errorf("unsupported filesystem type: %s", a.fsType)
	}
}

// verifyQuotaOnDisk reads back the actual filesystem-enforced quota for
// localPath and confirms it equals sizeBytes -- the "did the apply command
// exiting 0 actually mean the kernel holds this limit" check #10 asks for.
// Uses the agent's own configured a.projectsFile/a.projidFile (not the
// hardcoded /etc/projects, /etc/projid the status/UI reporting path uses),
// so this stays hermetically testable and correct under a non-default
// --projects-file/--projid-file.
//
// Called from within ensureQuota, which already holds a.mu for its whole
// body -- this only reads config fields set at startup (a.fsType,
// a.nfsBasePath, a.projectsFile, a.projidFile), so no additional locking
// is needed here.
func (a *QuotaAgent) verifyQuotaOnDisk(localPath string, sizeBytes int64) error {
	quotaMap, err := a.fetchQuotaReport()
	if err != nil {
		return err
	}
	return compareToReport(a.fsType, quotaMap, localPath, sizeBytes)
}

// errQuotaReportUnavailable wraps a failure to read the on-disk quota
// report itself (the xfs_quota/repquota/btrfs command failing) -- distinct
// from a report that was read successfully but disagrees with what's
// expected. A caller deciding whether to treat a failure as confirmed
// drift (#13's Drifted condition) must check for this and exclude it: an
// independent review pointed out that a transient report-command failure
// isn't evidence the filesystem's actual state disagrees with anything,
// only that this attempt to observe it failed -- reporting it as
// Drifted=True would be a false positive indistinguishable from a real
// mismatch to anything reading the condition.
var errQuotaReportUnavailable = errors.New("failed to read back on-disk quota report")

// fetchQuotaReport is the read half of verifyQuotaOnDisk, pulled out so a
// caller checking many PVs in one cycle (syncAllQuotas' drift check, #13)
// can fetch the filesystem-wide report once and reuse it, rather than
// repeating a full scan once per PV -- an independent review flagged the
// naive per-PV cost as unnecessarily expensive for installations with many
// QuotaPolicy-matched claims, since the report is identical for every PV
// on this node within one cycle (same fsType/quotaPath/projectsFile/
// projidFile, none of which vary per PV).
func (a *QuotaAgent) fetchQuotaReport() (map[string]uint64, error) {
	var quotaMap map[string]uint64
	var err error

	// a.quotaPath, matching what applyQuota itself targets (ApplyXFSQuota/
	// ApplyExt4Quota's quotaPath, ApplyBtrfsQuota's own path argument) --
	// not a.nfsBasePath, which happens to equal it by default but is a
	// separate field once --quota-path diverges from the base export path.
	switch a.fsType {
	case quota.FSTypeXFS:
		quotaMap, _, err = quota.GetXFSQuotaReport(a.quotaPath, a.projectsFile, a.projidFile)
	case quota.FSTypeExt4:
		quotaMap, _, err = quota.GetExt4QuotaReport(a.quotaPath, a.projectsFile, a.projidFile)
	case quota.FSTypeBtrfs:
		quotaMap, _, err = quota.GetBtrfsQuotaReport(a.quotaPath)
	default:
		return nil, fmt.Errorf("unsupported filesystem type: %s", a.fsType)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errQuotaReportUnavailable, err)
	}
	return quotaMap, nil
}

// compareToReport checks localPath's expected sizeBytes against a
// previously fetched quotaMap (see fetchQuotaReport) -- pure comparison,
// no I/O, so it's free to call once per PV even when the report itself
// was fetched once for the whole cycle.
func compareToReport(fsType string, quotaMap map[string]uint64, localPath string, sizeBytes int64) error {
	got, ok := quotaMap[localPath]
	if !ok {
		return fmt.Errorf("path %s not found in on-disk quota report after apply", localPath)
	}
	// Compare against what the backend was actually asked to enforce, not
	// the raw request: XFS/ext4 both floor to whole KB (see
	// ExpectedEnforcedBytes), so comparing against sizeBytes directly
	// would reject a correct apply for any capacity that isn't already a
	// 1024-byte multiple -- e.g. any decimal-SI `storage: 1G` PV.
	want := uint64(quota.ExpectedEnforcedBytes(fsType, sizeBytes))
	if got != want {
		return fmt.Errorf("on-disk quota %d does not match expected enforced value %d (requested %d)", got, want, sizeBytes)
	}
	return nil
}

// VerificationFailures returns the cumulative count of ensureQuota applies
// whose read-back verification (verifyQuotaOnDisk) failed after the apply
// command itself succeeded, since process start.
func (a *QuotaAgent) VerificationFailures() int64 {
	return a.verificationFailures.Load()
}

// updateQuotaStatus updates the quota status annotation on the PV, and --
// when st is QuotaStatusApplied and enforcedBytes is known (> 0) -- the
// enforced-limit annotation alongside it. When st is QuotaStatusApplied,
// policyDecision (if non-empty and not policyDecisionPreserve) is written
// to nfs.io/policy-decision; if empty (no policy applies), nfs.io/policy-decision
// is removed from the PV; if policyDecisionPreserve (callers with no policy
// attempt), existing annotations are preserved. A failed/pending write leaves
// any existing enforced-limit and policy-decision annotations untouched: they
// still reflect the last value actually enforced on the filesystem, which remains
// true regardless of this attempt's outcome (#14).
// updateQuotaStatus's correlationID parameter is "" for every caller
// outside ensureQuotaMutatedWith (e.g. the direct-call tests in
// agent_test.go) -- slog still renders the key as correlation_id="" for
// them, so those lines carry an explicitly empty ID rather than a
// fabricated one; only ensureQuotaMutatedWith attempts are joinable.
func (a *QuotaAgent) updateQuotaStatus(ctx context.Context, pv *v1.PersistentVolume, st string, enforcedBytes int64, correlationID string, policyDecision string) error {
	if ctx.Err() != nil {
		// Expected during the reconcile queue's shutdown drain (see
		// pvReconcileQueue.process): the filesystem quota mutation this
		// follows always completes regardless of ctx, but this trailing
		// annotation write legitimately can't -- an ERROR log per drained
		// item would just be noise for an already-documented, already
		// self-healing (next successful write) limitation.
		slog.Debug("Skipping quota status annotation write: context already done", "pv", pv.Name, "error", ctx.Err(), "correlation_id", correlationID)
		return ctx.Err()
	}
	freshPV, err := a.client.CoreV1().PersistentVolumes().Get(ctx, pv.Name, metav1.GetOptions{})
	if err != nil {
		slog.Error("Failed to get PV for status update", "pv", pv.Name, "error", err, "correlation_id", correlationID)
		return err
	}

	if freshPV.Annotations == nil {
		freshPV.Annotations = make(map[string]string)
	}
	freshPV.Annotations[AnnotationQuotaStatus] = st
	if st == QuotaStatusApplied {
		if enforcedBytes > 0 {
			freshPV.Annotations[AnnotationEnforcedLimitBytes] = strconv.FormatInt(enforcedBytes, 10)
		}
		if policyDecision == policyDecisionPreserve {
			// Preserve existing AnnotationPolicyDecision on freshPV; do not overwrite or delete.
		} else if policyDecision != "" {
			freshPV.Annotations[AnnotationPolicyDecision] = policyDecision
		} else {
			delete(freshPV.Annotations, AnnotationPolicyDecision)
		}
	}

	_, err = a.client.CoreV1().PersistentVolumes().Update(ctx, freshPV, metav1.UpdateOptions{})
	if err != nil {
		slog.Error("Failed to update PV quota status", "pv", pv.Name, "error", err, "correlation_id", correlationID)
		return err
	}
	return nil
}

// collectHistory collects usage history periodically
func (a *QuotaAgent) collectHistory(ctx context.Context) {
	slog.Info("Starting history collection", "interval", a.historyStore.Interval())

	ticker := time.NewTicker(a.historyStore.Interval())
	defer ticker.Stop()

	a.recordHistory()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.recordHistory()
		}
	}
}

// recordHistory records current usage to history
func (a *QuotaAgent) recordHistory() {
	if a.historyStore == nil {
		return
	}

	fsType, _ := quota.DetectFSType(a.nfsBasePath)
	usages, err := status.GetDirUsages(a.nfsBasePath, fsType, a.projectsFile, a.projidFile)
	if err != nil {
		slog.Error("Failed to get usages for history", "error", err)
		return
	}

	if err := a.historyStore.Record(usages); err != nil {
		slog.Error("Failed to record history", "error", err)
	}
}
