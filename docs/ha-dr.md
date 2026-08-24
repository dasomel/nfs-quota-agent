# NFS Server HA/DR — Active/Standby Quota Mutation Gate

Status: `--ha-active-file` implements the acceptance item every other #11
item depends on ("standby agent는 ownership이 확인되기 전 quota mutation을
수행하지 않는다") plus a failover reconciliation trigger independent of
`--sync-interval`'s cadence. Implementation is
[`internal/agent/ha.go`](../internal/agent/ha.go); the gate call sites are
`ensureQuota` and `RemoveOrphan` (`internal/agent/agent.go`,
`internal/agent/orphan.go`), plus the web UI's orphan-delete handlers
(`internal/ui/server.go`). Everything else #11 asked for — fencing
enforcement, read-back verification, RPO/RTO measurement, a replication
backend compatibility matrix, split-brain testing — is **not**
implemented; see §6.

This design was reviewed independently before merge (per this repo's
practice for concurrency-sensitive/safety-critical changes — see
`CLAUDE.md`), which found and led to fixing three real defects: a
QuotaPolicy status "applied lie" on standby (§3), the failover trigger
racing `syncAllQuotas` onto a second goroutine (§3), and the same trigger
silently reconciling nothing because of a stale cache (§3). What follows
describes the reviewed, fixed design — see the PR history for the
pre-review version if you want the contrast.

## 1. What this agent does and does not own

`nfs-quota-agent` reconciles Kubernetes PV state with local filesystem
project quotas on one NFS server node. It has never implemented, and this
change does not add, NFS data replication, cluster membership, leader
election, or fencing. Those are the job of an external HA/DR layer —
DRBD, storage-array replication, Pacemaker, a custom failover script —
and always will be: see #11's own "범위 제외" (out of scope) list.

What this agent *can* own is narrower and mechanical: given an external
signal saying "you are (not) the active node," stop mutating shared quota
metadata (`/etc/projects`, `/etc/projid`, actual filesystem quota limits)
when that signal says standby. That's the whole of what's implemented
here.

## 2. The signal: a file, not a protocol

`--ha-active-file <path>` (env/flag, default empty = disabled) points at
a path this agent only ever *reads*, never writes. Its existence means
"this instance is active"; its absence means "standby, refuse mutation."
`HAActive()` (`internal/agent/ha.go`) is the read; nothing in this
package creates, removes, or touches the file.

Why a file and not a Kubernetes `Lease` or similar API object: this
problem is about NFS *server*-level active/standby (see #11's title), a
concept that already has mature, external tooling (Pacemaker resource
agents, DRBD `promote`/`demote` hooks) built around exactly this
primitive — a promote/demote script that creates or removes a marker
file is the natural integration point, requires no new RBAC, and works
whether or not the fencing layer is Kubernetes-aware at all. A `Lease`
would tie this agent's HA behavior to the Kubernetes API being reachable,
which is an availability dependency the NFS-server-level failover this
issue is about shouldn't need. See #11's design principles: "split-brain에서
두 agent가 동시에 quota mutation을 수행하지 않도록 fencing/ownership gate를
둔다" — provide the gate, not the fencing decision.

Point `--ha-active-file` (`ha.activeFile` in the Helm chart) at a path
under `--state-dir` (`/var/lib/nfs-quota-agent` by default), which is
already a host-backed volume in the chart (see the PR #25 comment in
`internal/quota/project.go` for why that volume exists and is mounted the
way it is) — no new volume mount is needed as long as the promote/demote
script and the agent container can both reach that host path.

## 3. What "gate" actually means here

- `ensureQuota` (`internal/agent/agent.go`) checks `HAActive()` before
  taking `a.mu`, before doing anything else. Standby: log at Debug, return
  `ErrHAStandby` — **not** `nil`. An earlier version of this returned `nil`
  (the same "skip, not an error" convention `ensureQuota` uses for a PV
  whose local directory doesn't exist yet), which an independent review
  caught as a real bug: `syncAllQuotas`'s QuotaPolicy accounting
  (`recordEnforcement`, `policy.go`) treats a `nil` `ensureQuota` error as
  a successfully applied claim, so a standby node was publishing
  `Applied=True` on QuotaPolicy status while enforcing nothing — the exact
  "applied lie" class `docs/quotapolicy-design.md` §11 and
  `zz_f7_applied_lie_test.go` already exist to prevent for the
  missing-local-directory case. `ErrHAStandby` gets its own
  `v1alpha1.ReasonHAStandby` in `classifyEnforcementError` (`policy.go`) so
  a standby claim now correctly shows up as a `failingClaims` entry with an
  honest reason instead of a lie. No filesystem command runs,
  `appliedQuotas` isn't touched, the PV's status annotation isn't written.
- `syncAllQuotas` and `pvReconcileQueue.process` (the watch/reconcile-queue
  path, `internal/agent/reconcile_queue.go`) both special-case
  `ErrHAStandby`: excluded from `syncedCount`/`nfs_quota_agent_reconcile_total`
  (it isn't a successful reconcile) and from
  `nfs_quota_agent_reconcile_errors_total` (nothing went wrong either) --
  and `syncAllQuotas` logs one `Warn`-level summary per cycle
  (`"Skipped quota mutation for PVs: this instance is HA standby"`,
  with a count) instead of a per-PV line, so a misconfigured
  `--ha-active-file` (see §5) is visible without spamming a per-PV Debug
  line nobody's watching at that log level.
- `RemoveOrphan` (`internal/agent/orphan.go`) checks the same thing and
  also returns `ErrHAStandby` (the same sentinel, shared from
  `internal/agent/ha.go`) so `cleanupOrphans` can log and skip accurately
  instead of either claiming a false success (auditing a cleanup that
  never happened) or logging a standby refusal as if it were a real
  failure. This also covers the web UI's manual "delete this orphan"
  action (`internal/ui/server.go`): `ui.AgentInterface` gained its own
  `HAActive()` method (mirroring `metrics.AgentInfo`'s) so the UI handlers
  check it directly and return `409 Conflict` with an honest message
  before ever calling `RemoveOrphan` -- `agent` importing `ui` (not the
  reverse) means the UI package can't type-check `ErrHAStandby` itself, so
  checking the same underlying state directly is what makes the UI's
  refusal message accurate rather than `RemoveOrphan`'s generic-error
  path turning it into an opaque `500`.
- `runHAActivePolling` (`internal/agent/ha.go`) polls `HAActive()` every
  2s (independent of, and much shorter than, `--sync-interval`'s default
  30s). It does **not** call `syncAllQuotas` itself -- an earlier version
  did, and an independent review found that made `syncAllQuotas`
  concurrent for the first time (this goroutine's call racing `Run()`'s
  own ticker-driven call), which corrupts `knownProjectIDs` (replaced
  wholesale per cycle, not merged: a project ID one cycle allocates can
  read as free to a concurrently-running cycle and get handed to a second
  project) and can race two `finishQuotaPolicyCycle`/`WriteStatus` calls
  against the same object. Instead it sends a non-blocking signal on a
  buffered channel that `Run()`'s own single sync-loop goroutine consumes
  alongside its regular ticker -- reconciliation still only ever runs from
  that one goroutine, the invariant `syncAllQuotas`'s own doc comment
  already asserted before this feature existed. On the reverse
  (active→standby) transition it clears `appliedQuotas` under `a.mu`:
  without that, `ensureQuota`'s cache-hit shortcut would make the *next*
  active-triggered sync silently re-apply nothing for any PV whose
  capacity hadn't changed, since the cache's entire validity assumption
  ("this process is the only writer of this filesystem") is exactly what
  HA existing at all means isn't always true. It only runs when
  `--ha-active-file` is set -- the common no-HA deployment pays nothing
  for it.
- Readiness (`ReadinessOK`, used by the `/ready` probe) is **not** gated
  on `HAActive()`. A standby node is a legitimate, intentionally
  non-enforcing state, not an unhealthy one — folding it into readiness
  would make Kubernetes treat a correctly-behaving standby as a probe
  failure. `HAActive()` is exposed separately, as its own thing: the
  `nfs_quota_agent_ha_active` Prometheus gauge (1 active / 0 standby,
  always 1 when HA gating is off). **This means the three metrics above
  stay green on a standby node by design** (nothing failed) -- any
  enforcement-health alert built on them must also check
  `nfs_quota_agent_ha_active`, or "last full sync is recent" can read as
  healthy on a node enforcing nothing.

## 4. Fail-safe direction

`HAActive()` treats *any* `os.Stat` error on the active-file path — not
just "does not exist" — as standby: a permission error or a transient
stat failure also reads as "refuse to mutate." A false negative here
(wrongly treating an active node as standby, and so wrongly skipping
enforcement for one poll/sync cycle) is cheap: the next successful
`HAActive()` read or the next full resync corrects it. A false positive
(wrongly treating a standby node as active) risks two nodes mutating the
same shared quota metadata — the actual split-brain #11 is about. Given
that asymmetry, failing toward standby is the only defensible default.

## 5. Applicability boundary: the standby node must still have the export mounted

`Run()` calls `detectFilesystemType`/`checkQuotaAvailable` and exits with
an error (CrashLoopBackOff under the DaemonSet) if either fails --
*before* `runHAActivePolling` ever starts, let alone reaches a point where
`HAActive()` matters. On a DRBD-style pair where the standby node's export
is genuinely unmounted (the primary integration model §2 describes), the
agent never reaches the gate at all; it dies at startup detection instead.
This gate only actually functions in a topology where both nodes keep the
filesystem mounted (shared storage, or a replication mode that exposes a
read-only mount on standby) and rely on this gate — not on the mount
itself being absent — to prevent standby mutation. Making startup
detection degrade to not-ready instead of fatal when `--ha-active-file` is
set is a real improvement but a larger, separate change; not attempted
here.

## 6. Deliberately not implemented

Everything below is real #11 scope this change does not attempt, because
each needs either a decision only an operator/deployment can make, or a
real multi-node DR test environment this repository doesn't have access
to (same limitation this repo's `CLAUDE.md` already notes for filesystem
read-back verification generally):

- **Fencing enforcement.** This agent trusts the file. It cannot detect
  or prevent two nodes from both seeing "active" simultaneously (e.g. a
  split-brain in the external HA layer itself) — that guarantee has to
  come from whatever creates/removes the file (STONITH, quorum, etc.).
- **Filesystem quota read-back verification.** Comparing actual
  `xfs_quota report`/`repquota`/`btrfs qgroup show` output against desired
  state after a failover needs a real prjquota-mounted host to implement
  against reliably; not attempted here, same reasoning as #10's and
  QuotaPolicy's `Drifted` condition.
- **Replication lag / quota metadata RPO tracking**, **quota enforcement
  RPO/RTO measurement**, and the **HA topology compatibility matrix** by
  backend (DRBD / storage-array replication / rsync-based DR) — these
  need a real multi-node deployment against a specific replication
  backend to produce honest numbers or a real "supported/partial/
  unavailable" matrix; a matrix asserted from this codebase alone without
  ever running against real replication would just be guessing.
- **`/etc/projects`/`/etc/projid` project ID/name/path consistency
  validation specifically on failover.** `loadProjects`
  (`internal/agent/agent.go`, `Run()`) already runs
  `CheckProjectFileConsistency` at startup, but that's pre-existing,
  runs once at process start, and isn't re-triggered by a standby→active
  transition the way `syncAllQuotas` now is (§3) — whether that's
  sufficient for a real failover (as opposed to a process restart) is
  unverified without a real two-node test.
- **Automatic post-failover reconciliation beyond re-applying quotas that
  actually changed.** §3 now covers the specific bug an independent
  review found here (a stale `appliedQuotas` cache making the failover
  trigger apply nothing) — `syncAllQuotas`, once triggered, does
  genuinely re-list every PV and re-apply any quota whose cached state no
  longer matches. What's still unverified: whether the *actual filesystem
  state* it's comparing against is trustworthy right after a real
  failover (as opposed to `appliedQuotas` merely being empty in-memory) —
  that's the read-back verification gap two bullets above, not something
  this bullet claims to have closed.
- **Failback consistency gate.** Symmetric to activation: this
  implementation reacts identically to any standby→active transition,
  whether it's an initial failover or a failback, and doesn't add extra
  gating specific to "this was previously active, demoted, and is being
  promoted back."
- **Offline DR/failover test evidence.** The acceptance criteria's own
  검증 steps (§ in #11) describe a real two-NFS-server test harness; none
  of that exists in this repository or CI.

If any of the above becomes the next priority, it needs its own scoped
issue and, for the ones above that depend on a specific replication
backend or a real multi-node cluster, a decision about which environment
to validate against before code gets written against assumptions no one
has confirmed.
