# Agent Execution Security Profile

nfs-quota-agent adopts the OpenForge Agent Execution Security Contract as a **reduced privileged-controller profile**.

The project is not an interactive LLM/tool executor and does not mint agent capability grants. Its relevant security boundary is narrower: Kubernetes state selects a quota reconciliation, the controller resolves that request into a filesystem target and validated command arguments, privileged host-side tools may mutate quota state, and the result is read back and exposed through status/audit evidence.

Reference contract: https://github.com/dasomel/openforge/blob/main/docs/agent-execution-security.md

## Profile boundary

| OpenForge concept | nfs-quota-agent mapping |
|---|---|
| agent identity | Kubernetes workload identity / ServiceAccount plus node placement |
| tool contract | filesystem backend (`xfs_quota`, `setquota`, `btrfs`) and its fixed argv construction |
| resolved target | validated local export path + filesystem backend + project identity |
| resolved arguments | controller-derived quota size/project tuple after existing validation and backend normalization |
| request-side authorization | Kubernetes RBAC, PV/provisioner selection, path validation, filesystem/backend checks |
| sandbox/enforcement | privileged NFS-server-node container + host filesystem/kernel quota enforcement |
| post-state verification | existing `ensureQuota` read-back path and backend-specific expected enforced bytes |
| execution evidence | PV status/annotations, audit log when enabled, metrics/history, real-filesystem E2E evidence |

## Required invariants

1. **No natural-language authority.** AI-generated text, UI input, comments, annotations not already part of the documented controller contract, or an external agent request must never directly become host command argv or filesystem authority.
2. **Validate before privileged execution.** Operator-controlled values that reach quota command argv must pass the existing validation boundary. A new bare `exec.Command` in `internal/quota` is a security defect.
3. **Bind evidence to the concrete reconciliation.** Evidence should identify the PV/project, backend, resolved target, requested quota, effective/enforced quota, outcome, and stable reconciliation correlation where available. Do not claim a generic `applied` state when read-back verification failed.
4. **Do not confuse controller tests with kernel enforcement.** Stubbed `quota.CommandRunner` tests prove resolution/argv/parsing behavior. Only a real quota-enabled filesystem proves effective enforcement.
5. **Mutation remains controller-owned.** This reduced profile does not add human approval, session grants, or an agent-facing mutation API. Any future interactive/AI tool that can request quota mutation must adopt the full OpenForge resolution/grant/approval/evidence chain before reaching this controller's privileged boundary.
6. **Cleanup is a separate destructive class.** Orphan cleanup remains opt-in and dry-run by default. An external agent must not turn cleanup into an implicit side effect of a read/inspection request.
7. **Fail closed on ambiguity.** Invalid paths, project identifiers, backend state, quota arguments, or unverifiable post-state must not be promoted to successful execution evidence.

## Evidence classes

The project deliberately distinguishes evidence strength:

- **contract evidence** — unit tests with a stubbed command runner; proves deterministic resolution, validation, argv shape, parsing, and comparison logic.
- **controller evidence** — Kubernetes fake/integration tests; proves reconciliation and API-state behavior without proving host enforcement.
- **effective enforcement evidence** — real XFS/ext4/Btrfs quota-enabled host execution with kernel/filesystem read-back and over-limit write behavior.
- **release evidence** — release manifest, provenance, SBOM/license checks, compatibility matrix, and signed/offline verification artifacts.

A higher evidence class may support a stronger claim, but a lower class must never be relabeled as effective host enforcement.

## OpenForge adoption status

This profile satisfies the portfolio requirement to define the project's authority/evidence boundary without introducing an unnecessary generic agent runtime. Further runtime changes should be driven by a concrete consumer that needs cross-system correlation. If that happens, prefer adding a stable reconciliation/evidence identifier at the controller boundary rather than allowing an external caller to supply arbitrary host execution parameters.
