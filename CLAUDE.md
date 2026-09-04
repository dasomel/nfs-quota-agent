# nfs-quota-agent

Kubernetes agent that applies XFS / ext4 / btrfs project quotas to NFS PersistentVolumes. Runs privileged on the NFS server node, watches PV events, and exposes metrics, a web UI, and audit logs.

Layout, commands, testing conventions, and contribution flow are already documented — read `README.md`, `CONTRIBUTING.md`, `DESIGN.md`, and `make help` instead of restating them here. This file carries only what those can't tell you.

`CLAUDE.md` is the real context file. A standalone `AGENTS.md` is invisible to Claude Code; if a tool that reads project-scope `AGENTS.md` ever joins, `git mv` this file and leave a `CLAUDE.md` containing the single line `@AGENTS.md` in the same commit.

## Gotchas

**A green test suite does not mean quotas work.** Every external binary goes through `quota.CommandRunner`, and tests stub it. Passing tests prove argv shape and output parsing; enforcement needs a real `prjquota`-mounted host. Say which one you have when you report. The post-apply read-back verification (`ensureQuota` → `verifyQuotaOnDisk`) doesn't change this: it's exercised in tests against the same stubbed runner, so it proves the comparison logic is self-consistent, not that a real kernel would agree with either side of it.

**XFS and ext4 floor the requested size to whole KB before enforcing it** (`bhard=%dk`, `setquota`'s KB hard-limit column) — the value actually enforced is `sizeBytes` rounded down to the nearest 1024-byte multiple (minimum 1KB for any nonzero request), not `sizeBytes` itself. Any code comparing a requested byte count against an on-disk/reported value for these two backends must go through `quota.ExpectedEnforcedBytes(fsType, sizeBytes)`, not compare `sizeBytes` directly — an independent review caught exactly this omission as a CRITICAL bug in the read-back verification (PR #68): every PV capacity that wasn't already a 1024-byte multiple looked like a permanent failure despite being applied correctly, invisible in tests because every fixture used a Gi-multiple capacity. btrfs has no such rounding.

**`GetXFSQuotaReport`/`GetExt4QuotaReport`'s `projectsFile`/`projidFile` parameters aren't threaded everywhere they could be.** Three of their seven call sites (`internal/agent`'s `recordHistory`, `internal/metrics`, `internal/ui`) have an agent-configured path in scope but still call through `internal/status.GetDirUsages`, which hardcodes `/etc/projects`/`/etc/projid`. A non-default `--projects-file`/`--projid-file` deployment sees the web UI and history/metrics usage views silently show empty or wrong data while `ensureQuota`'s own read-back verification (which does use the configured paths) reports quotas as correctly applied. Even where `projidFile` *is* threaded correctly (PR #155's ext4 fix), `GetExt4QuotaReport`'s name-keyed row resolution still assumes the agent's configured `projidFile` matches what the `repquota` binary itself reads as `/etc/projid` on the host it runs on — under a non-default `--projid-file` those can disagree, silently dropping a row or matching it to the wrong path.

**btrfs needs a subvolume with `btrfs quota enable` already run.** `btrfs.go` shells out to `btrfs`; the image's `apk add` line does install `btrfs-progs` (verified 2026-08-31 against a built image — `/sbin/btrfs` present, `btrfs-progs-6.17.1-r1` recorded in `/licenses/os-packages-manifest.txt`; an earlier version of this note claiming `btrfs-progs` was missing was stale). The real remaining precondition is the target path being a subvolume with `btrfs quota enable` already run — confirmed on a real kernel (colima VM, aarch64 Ubuntu 24.04) at the raw filesystem level: `mkfs.btrfs` → mount → `btrfs quota enable` → `btrfs subvolume create` → `btrfs qgroup limit` → a write past the limit failed with "Disk quota exceeded" at the exact byte boundary. Only the raw filesystem path was exercised this way, not yet the agent's own `btrfs.go` code path end-to-end.

**ext4 project quota can fail for a host kernel-module reason that looks like filesystem corruption.** ext4's project quota tracking depends on the `quota_tree` kernel module; on a minimal kernel package (confirmed on a colima VM's default Ubuntu 24.04 cloud image, kernel 6.8.0-117-generic) that module lives in `linux-modules-extra`, not the base kernel install. Without it, mounting an ext4 fs formatted with `mkfs.ext4 -O project,quota` using `-o prjquota` fails at `mount(2)` with a generic error, and the kernel log reads `ext4_enable_quotas: Failed to enable quota tracking (type=0, err=-3, ino=...)` — easy to mistake for corruption, but `e2fsck -f` reports the filesystem clean either way, because nothing is actually broken; the quota kernel format just isn't loadable. Confirmed fix: `apt-get install linux-modules-extra-$(uname -r) && modprobe quota_tree quota_v2` before mounting. This is a host/kernel packaging gap, not something `internal/quota` can detect or work around — worth checking first on any NFS server node that's running a minimal/cloud kernel image.

**Project IDs are hash-derived** (`hashProjectName`, with collision fallback against existing IDs). Changing the hash silently re-maps every existing quota — no migration path, no error.

**A Go version bump touches four places**, and missing one fails late: `go.mod`, the `Dockerfile` builder stage, `.github/workflows/ci.yaml` (three `go-version` plus `go-version-input`), and the chart's `appVersion`. Chart `version` and `appVersion` drift apart easily since nothing enforces they move together — reconcile both during any release change, and check the current values in `Chart.yaml` rather than trusting a number written here, since this file isn't the source of truth for it.

**Three placements look like mistakes and aren't.** `ui.OrphanInfo` lives in `ui` so `agent` can import the type while `ui` reaches back only through `AgentInterface`; moving it re-creates the cycle. `status.DirUsage` sits in `status/types.go` so `history` gets the type without the implementation. `quota.defaultRunner` is package-level rather than a parameter so the test seam could be added without churning every exported signature.

**Privileged, host-node execution is the design, not debt.** Dropping privileges or running off the NFS server node means redesigning the quota path, not tightening the manifest.

**Filesystem backends deliberately don't share a signature.** XFS and ext4 take the full project tuple because they use `/etc/projects`; btrfs takes `(path, sizeBytes)` because it uses subvolume qgroups. A new backend should match the model its filesystem actually uses, and needs the three switch sites in `agent.go` plus the `Dockerfile` package list updated with it.

**Inferring "did this mutate anything" from a before/after cache read is ABA-vulnerable here.** `syncAllQuotas` (the periodic full sync) and the watch path's reconcile queue are separate goroutines that can both call `ensureQuota` for the same PV; a concurrent watch-triggered write can leave a before/after snapshot of `appliedQuotas[localPath]` looking unchanged even though a real mutation happened in between. `ensureQuotaMutated` exists so callers that need to know can get the actual signal from the call itself instead of inferring it — an independent review caught the inferred version as the root cause of a real false-positive in #13's `Drifted` condition.

## High-risk paths

Changes to argv construction, `validateQuotaArg`, `/etc/projects` · `/etc/projid` writes, or `privileged` / RBAC in the chart fail as host-level or command-injection problems rather than as failing tests. Give them the strongest reasoning available and a review pass from a context that didn't author them. Widening an RBAC verb widens cluster privilege — same tier.

In `internal/quota`, a bare `exec.Command`, or an operator-controlled string reaching argv without passing `validateQuotaArg`, is a defect regardless of what the tests say.

This isn't only about injection. Unit conversion and comparison logic in the apply/verify path (the KB-flooring gotcha above is the concrete example) is just as capable of silently misreporting enforcement as a false failure or a false success, and just as capable of passing every stubbed test while doing it. Treat any change to what a quota apply is compared against — not just what argv it builds — as the same tier.

## Delegated workers

A worker in a separate process does not inherit this file. Carry the constraints that matter for the lane inline in its prompt, and require command output as evidence — a worker that reports "done" without it has verified nothing.

## Verification

Use the `verification` skill before claiming a change is complete.
