# ADR-0002: Kubernetes Events and retry metrics

Status: Proposed — awaiting maintainer decision

Issue: #152 (split from #81, Phase 6)

## Context

Today the agent reports outcomes through three channels, none Kubernetes-native or RBAC-governed: structured `slog` lines, a host-local audit log file per node ([`internal/audit/logger.go:33-41`](../../internal/audit/logger.go)), and Prometheus counters/gauges from `/metrics` ([`internal/metrics/metrics.go:236-254`](../../internal/metrics/metrics.go)). No code path constructs a `corev1.Event` or uses `client-go`'s `tools/record` (`k8s.io/client-go v0.36.4`, [`go.mod:9`](../../go.mod)) today; `kubectl describe pv` shows nothing about quota outcomes. ADR-0001 deliberately keeps reconciliation, not admission, as the contract and relies on the PV status annotation, audit log, and structured logs for evidence ([`docs/adr/0001-quotapolicy-admission-webhook.md:186-196`](0001-quotapolicy-admission-webhook.md)). This ADR does not revisit that; it asks whether Events become a fourth, cluster-visible channel layered on top.

The watch path's reconcile queue retries failed `ensureQuota` calls with exponential backoff (5ms up to `defaultMaxRetryDelay` = 30s, below `workqueue`'s own 1000s default) via `AddRateLimited` ([`internal/agent/reconcile_queue.go:32-42`](../../internal/agent/reconcile_queue.go), [`internal/agent/reconcile_queue.go:129-135`](../../internal/agent/reconcile_queue.go)). `process` records every outcome through `recordReconcileResult` ([`internal/agent/reconcile_queue.go:301`](../../internal/agent/reconcile_queue.go), [`internal/agent/reconcile_queue.go:305`](../../internal/agent/reconcile_queue.go), [`internal/agent/agent.go:463-468`](../../internal/agent/agent.go)), which only distinguishes total vs. errored attempts ([`internal/agent/agent.go:214-217`](../../internal/agent/agent.go)), exported as two flat counters ([`internal/metrics/metrics.go:244-250`](../../internal/metrics/metrics.go)). There is no per-reason breakdown of *why* a reconcile failed and no signal for how many times a PV has been retried or how long it waited in backoff — the queue depth gauge ([`internal/metrics/metrics.go:238-240`](../../internal/metrics/metrics.go)) climbing doesn't say whether that's one PV looping or many distinct ones.

The current `ClusterRole` grants no `events` verb ([`charts/nfs-quota-agent/templates/clusterrole.yaml:8-11`](../../charts/nfs-quota-agent/templates/clusterrole.yaml)). Any Events option widens the privileged DaemonSet's RBAC surface, which `AGENTS.md`'s High-risk paths section treats the same as an argv or `/etc/projects` change: strongest available reasoning, review from a context that did not author it.

PersistentVolumes are cluster-scoped, so an `Event` whose `involvedObject` references a PV is itself retrievable cluster-wide (`kubectl get events --field-selector involvedObject.kind=PersistentVolume` from any namespace with `list` on `events`), unlike an Event on a namespaced PVC. Any option emitting PV-referencing Events must treat that visibility as a given, not an implementation detail to tune later.

A second, sharper consequence of that cluster-scoping: both `client-go` recorders default an Event's own `Namespace` to `metav1.NamespaceDefault` ("default") whenever the referenced object's namespace is empty — always true for a PV. Verified in the pinned `k8s.io/client-go v0.36.4` module cache: `tools/record/event.go`'s `recorderImpl.makeEvent` (namespace-defaulting at lines 489-491) and `tools/events/event_recorder.go`'s `recorderImpl.makeEvent` (lines 93-95) both run `namespace := ref.Namespace; if namespace == "" { namespace = metav1.NamespaceDefault }`. Every PV Event this design creates therefore lands in the `default` namespace regardless of where the agent is installed — this applies to B and D exactly as much as C, and it is `default`'s RBAC, not the agent's install namespace's, that actually governs who can `list`/`get` these Events without cluster-wide `events` read.

## Options

### A. No events: annotations + audit log stay the contract

- **RBAC diff / cardinality / failure semantics / opt-out flag:** not applicable — no `events` verb, nothing depends on Event API availability.
- **Tenant visibility:** unchanged — a tenant sees quota outcomes only via the web UI (`--enable-ui`) or the PV's own status annotation, never `kubectl get events`/`describe`.
- **Assessment:** matches ADR-0001's posture exactly, but leaves the motivating problem in #81/#152 unaddressed: nothing surfaces a quota failure to `kubectl describe pv` or a namespace-scoped viewer.

### B. `EventRecorder`, cluster-wide `events` create/patch

- **RBAC diff:** add to the base rule block (unconditional, not gated behind `quotaPolicy.enabled`, since it is not a QuotaPolicy-only feature):

  ```yaml
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["create", "patch"]
  ```

  `client-go`'s `record.EventBroadcaster` aggregates identical (`involvedObject`, `reason`, `message`) events client-side over a bounded LRU window before calling the API — `patch` lets it increment an existing Event's `count`/`lastTimestamp` instead of creating a new object per occurrence. `get`/`list`/`watch` are not required: aggregation is in-process, not read back from the API.
- **Cardinality / rate limiting:** `EventBroadcaster`'s aggregation caps *distinct* Event objects per (involvedObject, reason) pair, but not the number of distinct PVs an event can be raised against, nor the create/patch RPC rate beyond client-go's shared QPS/burst REST-config settings. Many PVs failing simultaneously produce one Event object per PV per reason, unbounded by PV count — a per-PV dedup window (skip re-emitting the same reason for the same PV inside e.g. the `syncInterval` default of 30s) is needed on top of the recorder's own aggregation to bound total object count under churn.
- **Tenant visibility:** because `involvedObject` is a cluster-scoped PV, any principal with `list`/`get` on core `events` in *any* namespace (a common default for `view`/`edit` ClusterRoles) sees every quota event for every PV in the cluster, not just their own PVC's — cluster-wide disclosure of quota outcomes (path prefix, project ID, failure reason) across tenants, gated only by whatever RBAC already grants `events` read.
- **Failure semantics:** `EventRecorder.Eventf` is fire-and-forget; the recorder logs and drops on API error, so an unreachable API server never blocks `ensureQuota`. Events are best-effort and can silently go missing during an outage — the same window ADR-0001 already accepts for the PV status annotation write.
- **Opt-out flag:** `--enable-events` (default `false`), mirroring `--enable-audit` ([`cmd/nfs-quota-agent/main.go:189`](../../cmd/nfs-quota-agent/main.go)), plus a chart `events.enabled` value gating the RBAC rule like `quotaPolicy.enabled` gates its own block ([`charts/nfs-quota-agent/templates/clusterrole.yaml:24`](../../charts/nfs-quota-agent/templates/clusterrole.yaml)).

### C. Events scoped to the agent's own namespace, referencing the PV by name

- **RBAC diff:** same verbs as B, but as a namespaced `Role`/`RoleBinding` in the agent's install namespace instead of a `ClusterRole` rule:

  ```yaml
  # Role, not ClusterRole
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["create", "patch"]
  ```

  This does not reduce the write surface: the API server stores an Event's `involvedObject` reference regardless of which namespace's `Role` created it, and a cluster-scoped `involvedObject.kind: PersistentVolume` stays queryable cluster-wide by anyone with cross-namespace `list` on `events` — the same exposure as B. **This does not achieve the "no cluster-wide verb" framing it appears to offer**: RBAC namespaces the create/patch grant, not the resulting object's discoverability. Confining visibility would require emitting Events against a namespaced proxy object (the consuming PVC) instead of the PV, which changes what the event is "about" and is not explored further here.

  Worse, this option is likely broken outright rather than merely equivalent to B: per the namespace-defaulting behavior above, every PV Event actually gets created in the `default` namespace, not the agent's install namespace. A `Role`/`RoleBinding` scoped to the install namespace grants `create`/`patch` there, not in `default` — so unless the agent happens to be installed into `default`, every `Eventf` call is denied with a 403, and this option silently fails to emit any Events at all.
- **Cardinality / rate limiting / failure semantics / opt-out flag:** identical to B (`Role` instead of `ClusterRole`), modulo the 403 above.
- **Tenant visibility:** identical to B — cluster-wide, since the referenced object is cluster-scoped independent of where the writer's RBAC grant lives (when it works at all).
- **Assessment:** listed because the issue asked for it, but doubly rejected: it neither narrows tenant visibility relative to B nor reliably works, since its own RBAC grant targets the wrong namespace for what the recorder actually writes to.

### D. `events.k8s.io/v1`, same verbs

- **RBAC diff:**

  ```yaml
  - apiGroups: ["events.k8s.io"]
    resources: ["events"]
    verbs: ["create", "patch"]
  ```

  Functionally equivalent to B/C at the RBAC-verb level, but `events.k8s.io/v1` is the schema Kubernetes has recommended since 1.19 (structured `regarding`/`related`, `series` for aggregation instead of ad hoc `count`/`firstTimestamp`/`lastTimestamp`). `client-go`'s `record.EventRecorder` targets the legacy `core/v1` API; `events.k8s.io/v1` natively means `k8s.io/client-go/tools/events.EventRecorder` instead — a different constructor and interface, not a drop-in RBAC-only change.
- **Cardinality / tenant visibility / failure semantics / opt-out flag:** identical to B; the per-PV dedup gap applies the same way, backed by `events.k8s.io/v1`'s own `series` field instead of ad hoc timestamps.
- **Assessment:** the more future-proof API surface for a *new* integration with no legacy caller to preserve, at the cost of a different recorder interface than `tools/record`'s widely-documented pattern.

## Retry metrics

Proposed additions to `internal/metrics/metrics.go`, alongside the existing reconcile counters ([`internal/metrics/metrics.go:244-254`](../../internal/metrics/metrics.go)):

- `nfs_quota_agent_reconcile_retries_total{reason}` — counter, incremented once per `AddRateLimited` call (not per terminal failure), so it counts retry attempts, not distinct PVs. `reason` is a closed 3-value enum (`apply_error`, `verify_failed`, `policy_snapshot_pending`), never a raw error string. Increment sites: the pending-policy-snapshot retry at [`internal/agent/reconcile_queue.go:266`](../../internal/agent/reconcile_queue.go) (`reason="policy_snapshot_pending"`) and the apply-error retry at [`internal/agent/reconcile_queue.go:303`](../../internal/agent/reconcile_queue.go) (`"apply_error"`, or `"verify_failed"` when `err` wraps the verification-mismatch error already distinguished for `nfs_quota_agent_verification_failures_total` at [`internal/agent/agent.go:463-468`](../../internal/agent/agent.go)).
- `nfs_quota_agent_reconcile_backoff_seconds` — histogram of the delay the rate limiter computed for each retry (its `When(key)` return value, not wall-clock time observed later), fixed buckets spanning the 5ms floor to the 30s `defaultMaxRetryDelay` ceiling ([`internal/agent/reconcile_queue.go:42`](../../internal/agent/reconcile_queue.go)). No PV-name label: the existing queue depth gauge already answers "how many distinct keys," not "how long did this one wait."

Neither metric adds a PV, namespace, or project-name label — both stay bounded by a fixed small enum or no label, unlike a hypothetical per-PV counter, which would grow unbounded with PV count and turn one hot-looping PV into a cardinality incident on top of the reconcile failure itself.

## Threat / abuse model

A tenant able to create PVCs rapidly (bound only by whatever `ResourceQuota`/`LimitRange` the cluster already enforces — this agent only reads those, never enforces at admission per ADR-0001) can drive the reconcile queue to process many PVs in a short window. Under options B–D this becomes Event create/patch calls proportional to distinct (PV, reason) pairs: `EventBroadcaster`'s in-process aggregation stops *repeated* identical events from each producing a new object, but a burst of *distinct* PVs each failing once still produces one Event per PV, unthrottled beyond the client's shared QPS/burst limit. That is Event-store pressure on the API server (etcd growth, watch fan-out), not a quota-bypass or filesystem risk — `ensureQuota` itself is unaffected. The retry metrics carry no equivalent risk: cardinality is capped by the closed reason enum regardless of PVC creation rate.

## Recommendation

Adopt **D (`events.k8s.io/v1`)** for the Events surface, gated behind `--enable-events` / chart value `events.enabled` (default `false`), with a per-PV, per-reason dedup window (reuse `syncInterval`, default 30s) implemented before calling the recorder — closing the gap `EventBroadcaster`'s own aggregation leaves open. Reject C: it does not narrow tenant visibility relative to B despite appearing to, adds a second RBAC object shape for no visibility gain, and — because every PV Event actually lands in the `default` namespace regardless of install namespace — its own `Role` grant would likely 403 rather than work at all. D over B mainly for the forward-looking schema; if the team prefers `tools/record`'s more established interface, B is an acceptable substitute with the identical RBAC diff, dedup requirement, and threat model — that does not need a new ADR revision.

Adopt the retry metrics regardless of which Events option (including A) is chosen: they need no RBAC change and address the observability gap independently of Event visibility questions.

## Decision

**Proposed — awaiting maintainer decision.**

- [ ] Confirm option (A / B / C / D) for the Events surface, or confirm no Events integration ships at all.
- [ ] Confirm the per-PV, per-reason dedup window (proposed: reuse `syncInterval`, default 30s) as a hard requirement before implementation, independent of which option is chosen.
- [ ] Confirm `--enable-events` / `events.enabled` default `false`.
- [ ] Confirm the retry-metrics names, labels, and cardinality bounds in this document, or propose changes.
- [ ] Confirm cluster-wide quota-outcome visibility to any principal with `events` read (options B–D alike) is an accepted trade-off, or require a design that avoids it before implementation proceeds.

## Consequences

If B, C, or D is accepted, implementation is gated on this ADR per #152's acceptance criteria and needs the dedup window, the `reason` enum wired through `ensureQuota`'s call sites, the chart RBAC rule and opt-out flag plumbed through `cmd/nfs-quota-agent/main.go`, and tests asserting no Event fires when the flag is off. If A is accepted, only the retry-metrics section proceeds, and #152 should be split so Events can be reopened later without re-litigating the metrics.
