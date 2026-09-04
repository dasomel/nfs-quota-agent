# ADR-0001: QuotaPolicy admission control

Status: Accepted — option A (no webhook), maintainer decision 2026-09-04

Issue: #132

## Context

QuotaPolicy currently acts after admission. The agent selects a winning policy
for a bound PV and computes an effective filesystem limit from the PV's observed
capacity; it raises values below `minQuota`, clamps values above `maxQuota` when
`enforceMax` is true, and records advisory overages without clamping when it is
false ([`internal/quotapolicy/bound.go:57-122`](../../internal/quotapolicy/bound.go)).
Both periodic sync and PV watch reconciliation use that resolver
([`internal/agent/policy.go:100-147`](../../internal/agent/policy.go),
[`internal/agent/policy.go:211-275`](../../internal/agent/policy.go)). This makes
the enforced filesystem limit converge after a PVC is bound; it does not reject
the PVC request.

Every policy-shaped reconciliation computes a deterministic decision ID from
the PV, policy UID and generation, outcome, and effective bytes
([`internal/quotapolicy/decision.go:25-44`](../../internal/quotapolicy/decision.go)).
On successful apply, the agent records that ID in the existing PV status update,
audit provenance, and structured logs
([`internal/agent/agent.go:1272-1302`](../../internal/agent/agent.go),
[`internal/agent/agent.go:1593-1606`](../../internal/agent/agent.go),
[`internal/agent/agent.go:1620-1649`](../../internal/agent/agent.go)). Maintainers
can therefore correlate the eventual enforcement decision without adding an
admission component.

The current chart runs a privileged DaemonSet on selected NFS server nodes
([`charts/nfs-quota-agent/values.yaml:174-184`](../../charts/nfs-quota-agent/values.yaml),
[`charts/nfs-quota-agent/values.yaml:258-263`](../../charts/nfs-quota-agent/values.yaml)).
Its ClusterRole already reads PVCs and QuotaPolicies, and updates QuotaPolicy
status, only when the feature is enabled
([`charts/nfs-quota-agent/templates/clusterrole.yaml:24-56`](../../charts/nfs-quota-agent/templates/clusterrole.yaml)).
There is no admission-serving workload or certificate lifecycle today.

The chart has no `kubeVersion` constraint
([`charts/nfs-quota-agent/Chart.yaml:1-18`](../../charts/nfs-quota-agent/Chart.yaml)),
and the documented minimum is Kubernetes 1.20
([`README.md:15-20`](../../README.md)). The compatibility matrix records real
cluster evidence only for 1.35 and 1.36; it is evidence of observations, not a
support-range declaration
([`hack/compatibility-matrix.json:2-3`](../../hack/compatibility-matrix.json),
[`hack/compatibility-matrix.json:41-53`](../../hack/compatibility-matrix.json)).

### Threat model: do not trust a PVC-carried decision ID

An earlier design persisted the admission decision ID on the PVC. It was
rejected because the agent would need cluster-wide PVC write authority and
because a PVC annotation is tenant-writable input, not trustworthy evidence.
An actor able to create or patch its PVC could copy, forge, or retain a stale ID
and make it look correlated with a different policy generation
([`internal/audit/entry.go:47-60`](../../internal/audit/entry.go)). The current
PVC grant is read-only
([`charts/nfs-quota-agent/templates/clusterrole.yaml:24-41`](../../charts/nfs-quota-agent/templates/clusterrole.yaml));
persisting an ID there would widen it cluster-wide. The accepted
design derives the ID from controller-observed inputs and writes it to the PV,
whose existing ClusterRole already permits status annotations
([`internal/quotapolicy/decision.go:25-38`](../../internal/quotapolicy/decision.go),
[`charts/nfs-quota-agent/templates/clusterrole.yaml:8-11`](../../charts/nfs-quota-agent/templates/clusterrole.yaml)).
Admission must not make PVC annotations authoritative or add PVC write verbs.

## Options

### A. No webhook: reconciliation is the contract

- **Surface and placement:** no new objects, Service, certificate, workload, or
  RBAC verbs. The existing DaemonSet remains the only decision engine.
- **Agent outage:** PVC admission continues. Existing on-disk quotas remain in
  force; applying changes for new or modified PVs and recording their decision
  IDs waits until the DaemonSet returns, then reconciliation converges.
- **Cost:** no new control-plane dependency or certificate operations. Users do
  not receive an admission-time rejection and may observe requested capacity
  differing from the later filesystem limit.
- **Does not prevent:** violating PVC create/resize, PVCs admitted before a
  policy exists, policy changes after admission, or writes to new/unreconciled
  paths while the agent is unavailable. Existing shrink safeguards still apply
  during reconciliation.
- **Does not prevent (healthy agent):** a window between PV bind and the
  watch-triggered `ensureQuota` completing, during which the volume carries no
  enforced filesystem limit. This exists independently of agent availability;
  the periodic sync (`syncInterval`, default 30s in
  [`values.yaml`](../../charts/nfs-quota-agent/values.yaml)) is only the
  fallback if the watch event is missed. Reconcile-time clamping bounds the
  window, it does not close it.

### B. Validating webhook, fail-open

- **Surface and placement:** add a cluster-scoped
  `ValidatingWebhookConfiguration`, a TLS `Service`, and preferably a separate,
  non-privileged Deployment and ServiceAccount. Reusing the privileged
  DaemonSet couples API admission to NFS-node scheduling and agent rollouts; a
  separate Deployment avoids that coupling but adds another workload.
- **Certificates:** either depend on cert-manager (`Certificate` plus
  namespaced `Issuer` or cluster-scoped `ClusterIssuer`) or own a self-signed
  CA, serving `Secret`, rotation, and `caBundle` update. Self-management needs a
  controller or hook with `get/create/update/patch` on the serving Secret and
  `get/update/patch` on `validatingwebhookconfigurations`; cert-manager avoids
  granting those verbs to the agent but adds an external controller dependency.
- **Runtime RBAC:** a dedicated webhook needs `get/list/watch` on
  `quotapolicies.quota.nfs.io` and core `limitranges`. The incoming PVC supplies
  its own name, labels, StorageClass, and requested size, so PVC write access is
  neither required nor acceptable.
- **Agent outage:** if co-located in the DaemonSet, connection errors and
  timeouts are ignored and the PVC is admitted; a separate healthy Deployment
  can still reject while filesystem enforcement is down. In either placement,
  an admitted violation is eventually handled by reconciliation.
- **Cost:** code, cache consistency, TLS rotation, availability monitoring, and
  upgrade ordering. Fail-open preserves API availability but cannot promise
  admission enforcement during an outage.
- **Does not prevent:** pre-existing PVCs, policy changes after admission,
  direct PV/filesystem changes, or fail-open admissions during webhook errors.

### C. Validating webhook, fail-closed

- **Surface and placement:** the same objects, certificate choices, runtime
  reads, and separate-Deployment preference as option B.
- **Agent outage:** co-location means an unavailable DaemonSet can reject every
  matching PVC create or resize. With a healthy separate Deployment, admission
  continues to accept or reject while only filesystem enforcement is delayed.
  That Deployment reduces but does not remove the control-plane dependency: its
  own network, certificate, or policy-cache failure can still stop storage API
  writes.
- **Cost:** the highest operational burden: disruption budgets, replicas,
  short timeouts, monitoring, safe bootstrap/upgrade ordering, and a documented
  emergency bypass are required. It provides the strongest admission guarantee.
- **Does not prevent:** PVCs admitted before the webhook or policy existed,
  policy changes after admission, direct PV/filesystem changes, or enforcement
  drift after a valid admission.

### D. ValidatingAdmissionPolicy (CEL)

- **Surface and placement:** add cluster-scoped `ValidatingAdmissionPolicy` and
  `ValidatingAdmissionPolicyBinding` objects. Evaluation runs in the API server;
  there is no Service, serving certificate, rotation, workload, or runtime
  ServiceAccount RBAC. The installer must be allowed to manage those two
  `admissionregistration.k8s.io` resources.
- **Agent outage:** admission evaluation remains available independently of the
  DaemonSet. CEL evaluation/parameter failures can be configured fail-open or
  fail-closed, but filesystem enforcement is still unavailable until the agent
  returns.
- **Viability:** [VAP is stable only on Kubernetes
  1.30+](https://kubernetes.io/docs/reference/access-authn-authz/validating-admission-policy/).
  It works on the two
  versions observed in the compatibility matrix, but not across this chart's
  documented 1.20+ range. Moreover, a binding evaluates selected parameter
  resources independently; it does not directly reproduce the Go resolver's
  specificity, priority, and name tie-break across multiple QuotaPolicies
  ([`internal/quotapolicy/resolve.go:96-117`](../../internal/quotapolicy/resolve.go)).
  It is therefore not a compatible default without raising/gating the supported
  Kubernetes version and simplifying or generating the policy bindings.
- **Cost:** lower serving cost than a webhook, but duplicated CEL semantics,
  version-gated chart paths, parameter authorization for PVC creators, and drift
  tests against the Go resolver.
- **Does not prevent:** pre-existing PVCs, later policy changes, direct
  PV/filesystem changes, or enforcement drift. It also cannot produce the
  current PV-based decision ID before a PV exists.

## Recommendation

Choose **A: no webhook** for the current `v1alpha1` contract. Reconciliation
already owns filesystem truth, records a stable and reviewable decision ID, and
recovers after agent downtime. Admission rejection would add a control-plane
availability dependency and certificate lifecycle for a request that the agent
can already safely clamp. VAP avoids serving infrastructure but cannot cover the
declared Kubernetes range or exactly reproduce winner selection today.

The recommendation adds **no RBAC verbs**. In particular, do not add PVC write
verbs; do not add `admissionregistration.k8s.io` verbs; and do not add Secret
verbs. If maintainers later require hard admission rejection, revisit a
separate, non-privileged Deployment with fail-open as the initial rollout and
explicitly decide certificate ownership before implementation.

## Decision

**Accepted — option A (no webhook), decided by the maintainer on 2026-09-04.**

- [x] Admission-time rejection is not wanted for the `v1alpha1` contract:
  reconcile-time clamping plus visible decision IDs is the contract.
- [x] Not applicable: no webhook, so no fail-open/fail-closed default,
  certificate ownership, or placement decision. Reopen #132 with a new ADR if
  a tenant-facing admission requirement appears.

## Consequences

If A is accepted, the API remains available independently of this agent and the
chart's privilege surface does not grow. Operators must understand that a PVC's
requested size can be admitted before its backing filesystem is clamped and use
the PV decision annotation, audit log, and structured log for evidence.

Admission-time user feedback and rejection remain deliberately absent. A later
decision for B or C requires a separate implementation ADR or an update to this
record covering certificate rotation, upgrade ordering, availability targets,
and exact RBAC. A later decision for D requires an explicit Kubernetes support
floor/gate and semantic equivalence tests against the Go resolver.
