# nfs-quota-agent

Kubernetes agent that applies XFS / ext4 / btrfs project quotas to NFS PersistentVolumes. Runs privileged on the NFS server node, watches PV events, and exposes metrics, a web UI, and audit logs.

Layout, commands, testing conventions, and contribution flow are already documented — read `README.md`, `CONTRIBUTING.md`, `DESIGN.md`, and `make help` instead of restating them here. This file carries only what those can't tell you.

`CLAUDE.md` is the real context file. A standalone `AGENTS.md` is invisible to Claude Code; if a tool that reads project-scope `AGENTS.md` ever joins, `git mv` this file and leave a `CLAUDE.md` containing the single line `@AGENTS.md` in the same commit.

## Gotchas

**A green test suite does not mean quotas work.** Every external binary goes through `quota.CommandRunner`, and tests stub it. Passing tests prove argv shape and output parsing; enforcement needs a real `prjquota`-mounted host. Say which one you have when you report.

**btrfs passes tests and fails in the container.** `btrfs.go` shells out to `btrfs`, but the image's `apk add` line never installs `btrfs-progs`. btrfs also needs the target path to be a subvolume with `btrfs quota enable` already run.

**Project IDs are hash-derived** (`hashProjectName`, with collision fallback against existing IDs). Changing the hash silently re-maps every existing quota — no migration path, no error.

**A Go version bump touches four places**, and missing one fails late: `go.mod`, the `Dockerfile` builder stage, `.github/workflows/ci.yaml` (three `go-version` plus `go-version-input`), and the chart's `appVersion`. `Chart.yaml` currently carries `version: 0.3.0` against `appVersion: "0.2.2"` — reconcile that during any release change.

**Three placements look like mistakes and aren't.** `ui.OrphanInfo` lives in `ui` so `agent` can import the type while `ui` reaches back only through `AgentInterface`; moving it re-creates the cycle. `status.DirUsage` sits in `status/types.go` so `history` gets the type without the implementation. `quota.defaultRunner` is package-level rather than a parameter so the test seam could be added without churning every exported signature.

**Privileged, host-node execution is the design, not debt.** Dropping privileges or running off the NFS server node means redesigning the quota path, not tightening the manifest.

**Filesystem backends deliberately don't share a signature.** XFS and ext4 take the full project tuple because they use `/etc/projects`; btrfs takes `(path, sizeBytes)` because it uses subvolume qgroups. A new backend should match the model its filesystem actually uses, and needs the three switch sites in `agent.go` plus the `Dockerfile` package list updated with it.

## High-risk paths

Changes to argv construction, `validateQuotaArg`, `/etc/projects` · `/etc/projid` writes, or `privileged` / RBAC in the chart fail as host-level or command-injection problems rather than as failing tests. Give them the strongest reasoning available and a review pass from a context that didn't author them. Widening an RBAC verb widens cluster privilege — same tier.

In `internal/quota`, a bare `exec.Command`, or an operator-controlled string reaching argv without passing `validateQuotaArg`, is a defect regardless of what the tests say.

## Delegated workers

A worker in a separate process does not inherit this file. Carry the constraints that matter for the lane inline in its prompt, and require command output as evidence — a worker that reports "done" without it has verified nothing.

## Verification

Use the `verification` skill before claiming a change is complete.
