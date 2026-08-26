# QuotaPolicy Custom Resource — Design

Status: types, generated deepcopy, the generated CRD manifest, and the
controller (resolution, effective-quota bounding, status write-back) are
all implemented, gated behind `--enable-quota-policy` /
`quotaPolicy.enabled` (default off). Types live at
[`internal/apis/quota/v1alpha1/types.go`](../internal/apis/quota/v1alpha1/types.go);
generated output is `zz_generated.deepcopy.go` in the same package and
[`charts/nfs-quota-agent/crds/`](../charts/nfs-quota-agent/crds/), both
produced by `make generate` (`go tool controller-gen`) and checked for
staleness in CI. The controller itself is
[`internal/quotapolicy`](../internal/quotapolicy) (pure resolution and
bounding logic, plus dynamic-client listing and status write-back), wired
into the agent's existing sync cadence from
[`internal/agent/policy.go`](../internal/agent/policy.go) — no separate
watch loop or work queue; see §11 below for what that means. `Drifted`
(#13) is implemented via an independent filesystem read-back, not the
enforcement cache — see "`Drifted`: independent read-back, not the
enforcement cache" below.

## 1. Problem

Today, quota policy for a namespace is *advertised* from up to two sources,
resolved by [`internal/policy.GetNamespacePolicy`](../internal/policy/policy.go)
in strict priority order:

1. a `LimitRange` with a `PersistentVolumeClaim` limit entry (`Max`/`Min`/`Default`)
2. the `nfs.io/default-quota` / `nfs.io/max-quota` namespace annotations

**Correction (post-controller-PR):** this section originally described a
third "global default flag" tier and framed this chain as *the* mechanism
governing quota size. Neither was accurate — `GetNamespacePolicy` never
implemented a global-default tier (the corresponding `--default-quota` /
`--enforce-max-quota` flags were removed as dead code once that was
discovered), and the whole chain only ever fed the web UI's advisory
Policies/Violations display (`internal/policy`'s `GetAllNamespacePolicies`
/ `GetViolations`), never actual filesystem quota sizing — `LimitRange`
remains real as an admission-time PVC size gate, but the two-tier
`GetNamespacePolicy` chain itself was informational only.

This is namespace-wide only — there is no way to give one PVC, or one group
of PVCs selected by label, a different bound than the rest of the
namespace — and it is imperative rather than declarative: the annotations
are values on a `Namespace` object, not a reviewable, GitOps-able resource
with its own status. Issue #13 asks for a CRD that closes both gaps, and
— since it turns out nothing before it actually enforced a size bound —
also closes the enforcement gap this section originally understated.

## 2. API group and scope

**Group: `quota.nfs.io`, version `v1alpha1`.**

The issue's own draft floats `storage.narwhal.io`, but the issue also states
the agent "must operate correctly without Narwhal" — coupling the API group
to another product's name contradicts that. Every annotation the agent
reads or writes today already lives under `nfs.io/`:
`nfs.io/project-name`, `nfs.io/quota-status`, `nfs.io/default-quota`,
`nfs.io/max-quota`. `quota.nfs.io` continues that namespace and keeps the
API self-contained.

**Namespace-scoped.**

| | Namespace-scoped (chosen) | Cluster-scoped |
|---|---|---|
| RBAC | Namespace owner manages their own policy with namespace-scoped RBAC (`Role`/`RoleBinding`); no cluster privilege needed | Requires `ClusterRole` — every user who can create one can affect every namespace |
| Matches the objects it governs | Yes — a policy governs PVCs in its own namespace, same relationship as `LimitRange` and `ResourceQuota` to their namespace | No — a cluster-scoped object governing namespaced objects is a weaker fit and harder to reason about ownership for |
| Platform-wide defaults | Not directly — a platform team applies the same `QuotaPolicy` object per namespace (identical spec, or templated via GitOps) | Would be a natural single object for "every namespace gets this floor/ceiling unless overridden" |

Cluster-scoped is deferred, not ruled out: the platform-wide-default use
case it would buy is already served today by the agent's global-default
flag (source `"Global"` in `NamespacePolicy`), so there's no functional gap
forcing a cluster-scoped kind now. If platform teams need policy templating
across many namespaces without hand-applying per namespace, that is better
solved later by a separate, deliberately-named cluster-scoped kind (e.g.
`ClusterQuotaPolicy`) than by adding a scope toggle to this one — namespace
admins should not have to reason about a resource that might not be
namespace-scoped depending on how it was created.

## 3. Precedence

QuotaPolicy is inserted **above** the existing three-source chain:

```
QuotaPolicy > LimitRange > Annotation > Global
```

It wins because it is the explicit, GitOps-managed declaration — the same
reasoning `LimitRange` already gets priority over the annotation for being
more structured and admission-integrated. `internal/policy.NamespacePolicy.Source`
already models "which source won" as a string; QuotaPolicy adds a fourth
value (`"QuotaPolicy"`) to that set. That change is not part of this PR — the
type is `internal/apis/quota/v1alpha1`, not `internal/policy` — but is noted
here since the doc is expected to state how the CRD relates to what exists.

### QuotaPolicy vs. LimitRange are different layers

This is the part most likely to be misread as "QuotaPolicy replaces
LimitRange." It doesn't, because they don't act at the same point:

- **`LimitRange`** is a Kubernetes-native **admission** control. It runs
  when a PVC is created and can reject or mutate the PVC's
  `spec.resources.requests.storage` before it ever reaches etcd. It has no
  opinion on what happens to the backing filesystem.
- **`QuotaPolicy`** (like the annotations before it) is **filesystem
  enforcement**, applied out-of-band by this agent after the PVC/PV exist,
  via `xfs_quota` / `setquota` / btrfs qgroups. It has no admission power —
  a PVC that already exceeds it is not rejected, only reported.

So "QuotaPolicy wins" means: when computing the *filesystem* quota bound to
enforce, QuotaPolicy's numbers are used instead of LimitRange's or the
annotations'. It does not mean QuotaPolicy can override what LimitRange
already admitted, and it does not make LimitRange irrelevant — LimitRange
still gates what PVC sizes are even accepted.

**The conflict that must not be silent:** if a QuotaPolicy's `maxQuota` is
larger than the namespace's `LimitRange` PVC max, the agent still enforces
the QuotaPolicy's (larger) filesystem quota — but that disagreement is a
misconfiguration signal (someone is asking the filesystem to allow more
than admission would ever let a new PVC request), so it is recorded as the
`LimitRangeConflict` condition on the `QuotaPolicy`, not silently resolved
by either capping QuotaPolicy to LimitRange's max or ignoring LimitRange.
Existing PVCs that were sized against a smaller LimitRange are unaffected
by this either way, since LimitRange never re-validates existing objects.

### Cross-field bounds are enforced by the API server

`minQuota`, `defaultQuota` and `maxQuota` are checked against each other at
apply time via `XValidation` rules on the spec, so an inverted policy is
rejected by `kubectl` rather than discovered at reconcile time:

| Rule | Message |
|---|---|
| `minQuota <= maxQuota` | `minQuota must not exceed maxQuota` |
| `defaultQuota >= minQuota` | `defaultQuota must not be smaller than minQuota` |
| `defaultQuota <= maxQuota` | `defaultQuota must not exceed maxQuota` |

The rules go through the CEL `quantity()` library rather than comparing the
raw strings, because these are `resource.Quantity` values and naive string
comparison gets same-magnitude pairs backwards: by value `10Gi > 9Gi`, but
lexically `"10Gi" < "9Gi"` (the leading `"1"` sorts before `"9"`) — a
string-based rule would reject that valid ordering and accept its inverse.
Verified directly against a live Kubernetes 1.36 cluster: applying
`minQuota: 10Gi, maxQuota: 9Gi` (invalid by value) is rejected with
`minQuota must not exceed maxQuota`, and applying `minQuota: 9Gi, maxQuota:
10Gi` (valid by value, but the pair a naive lexical rule would reject) is
accepted. `quantity()` has been available and GA since Kubernetes 1.29,
comfortably below the 1.35/1.36 range this project targets, so there is no
version gate to guard.

Each rule guards on both of its operands being present. All three fields are
optional, so a rule that assumed one were set would reject valid policies
that simply omit it — a policy specifying only `maxQuota` is legitimate.

Verified against a real API server (v1.36.1) rather than by inspection: a
valid policy is accepted, `minQuota: 2Gi` with `maxQuota: 1000Mi` is
rejected (the case string comparison would have let through), an oversized
`defaultQuota` is rejected, and a policy carrying only `maxQuota` is
accepted.

## 4. Multiple policies matching one PVC

A PVC can be matched by more than one `QuotaPolicy` in its namespace (e.g.
one namespace-wide policy and one narrower label-selector policy). Resolved
deterministically, no randomness, in this order:

1. **Most specific selector wins**: an explicit `spec.selector.pvcName`
   beats `spec.selector.labelSelector`, which beats a namespace-wide
   (both-unset) selector. Encoded as `MatchKind` in the types
   (`PVCName` > `LabelSelector` > `NamespaceWide`).
2. **Tie → lowest `spec.priority` wins.** Convention: `0` is the strongest
   priority, matching Kubernetes' own `PriorityClass` and
   scheduler-extender conventions — *not* "higher number wins," which also
   exists elsewhere in the ecosystem (e.g. Envoy/Istio route priority in
   some configurations) and would be the wrong convention to silently pick
   without stating it. Default is `100`, leaving room to insert both above
   and below the default without renumbering everything.
3. **Still tied → lexicographically smallest resource name wins.** This is
   a name comparison (`metadata.name`), not a value comparison of the
   quotas themselves — it's a deterministic tiebreak, not a "smallest quota
   wins" policy choice.

**Auditability**: `QuotaPolicyStatus.MatchedClaimSample` records, for a
bounded sample of matched claims, the `MatchKind` that matched and — when
this policy lost — the name of the policy that won instead
(`ResolvedBy`). Reading every `QuotaPolicy` in a namespace and looking at
each one's sample is how an operator reconstructs "why did claim X get
quota Y" without a separate audit object. See §6 for why this is a sample
rather than an exhaustive list.

## 5. Conditions

`QuotaPolicyStatus.Conditions` uses `metav1.Condition` (the standard
`type`/`status`/`reason`/`message`/`lastTransitionTime`/`observedGeneration`
shape), `+patchStrategy=merge` on `type` like every other Kubernetes
condition list, so controllers and clients can patch a single condition
without clobbering the others.

| Type | Meaning | True | False |
|---|---|---|---|
| `Ready` | The policy object itself is well-formed and its selector resolves | selector valid | selector invalid (e.g. would-be mutually-exclusive fields, malformed label selector) |
| `Applied` | Every claim this policy currently wins for has the quota enforced | all won claims enforced | at least one won claim not yet enforced or failing |
| `Degraded` | One or more won claims are failing enforcement | failures present (see `FailingClaims`) | none |
| `Drifted` | The enforced filesystem quota no longer matches spec for one or more won claims | drift detected | none / not checked |
| `LimitRangeConflict` | This policy's `maxQuota` exceeds the namespace `LimitRange` PVC max | conflict present | no LimitRange, or within it |

These are independent axes, not a single state machine — `Applied=True` and
`Drifted=True` together is a valid and meaningful combination (enforced now,
but something changed it since).

Reasons are a fixed vocabulary (`Reason*` constants in the types file:
`SelectorValid`, `SelectorInvalid`, `NoMatchingClaims`, `AllClaimsApplied`,
`PartiallyApplied`, `NotYetReconciled`, `EnforcementFailed`,
`FilesystemUnavailable`, `ProjectIDExhausted`, `UnsafeShrinkRejected`,
`QuotaDriftDetected`, `NoDrift`, `ExceedsLimitRangeMax`, `WithinLimitRange`,
`NoLimitRange`) so that dashboards and scripts built against them don't rot
as the controller grows more call sites. `ProjectIDExhausted` anticipates
the collision fallback in `hashProjectName` running out of room, though the
controller PR may find it never needs to report that reason in practice —
it's included now so the vocabulary doesn't need a breaking addition for it
later. `UnsafeShrinkRejected` surfaces through `Degraded`/`FailingClaims`
like any other enforcement failure — see §11's "Shrink guard" for the
`ensureQuota`-level check that produces it.

## 6. Status: no usage, no history, bounded samples only

Issue #13 explicitly excludes usage and history from `QuotaPolicyStatus`.
What's included instead:

- `matchedClaims` / `appliedClaims` / `shadowedClaims` — plain counts, cheap
  to keep accurate on every reconcile.
- `failingClaims` — a **bounded sample, capped at 20 entries**,
  most-recent-first, of currently-failing won claims. It is a triage aid,
  not a log: entries reflect current failures only, and there is no
  guarantee a given failing claim stays in the sample as others fail too.
- `matchedClaimSample` — a **bounded sample, capped at 20 entries** (same
  cap, independent list) used for precedence auditing (§4).

Both caps are enforced by `+kubebuilder:validation:MaxItems=20` so oversized
status can't be written even by a buggy controller — `etcd` object size and
`kubectl get -o yaml` readability both degrade with unbounded per-item
status lists, which is the standard reason Kubernetes API conventions
recommend bounding list-shaped status.

**For usage over time, don't look at `QuotaPolicy` status.** Use:
- the agent's Prometheus metrics (`internal/metrics`) for current gauges, or
- `internal/history.Store`'s persisted snapshots (when `history.enabled` in
  the Helm chart) for trend data.

`QuotaPolicy` status answers "is this policy currently working," not "how
has usage under it changed."

## 6.5. REST facade (`/api/quota-policies`)

Issue #13 asks for a REST compatibility facade over the CRD, not a
separate API surface with its own state. `internal/ui/server.go`'s
`GET /api/quota-policies` is exactly that: it calls the same
`quotapolicy.List` the agent's own sync cycle uses, via the same
`dynamic.Interface` (`main.go` passes the one already constructed for
`--enable-quota-policy` into `ui.Options.DynamicClient` -- not a second
client), and returns the typed `v1alpha1.QuotaPolicy` objects as JSON
directly. There is no write path: the CRD (`kubectl apply` / GitOps) is
the only way to create or modify a `QuotaPolicy`, matching this design's
"CRD is the source of truth" principle (§1) -- a REST-side write path
would just be a second place `.spec` could be set from, which is exactly
what this design avoids. `enabled` in the response reflects whether a
dynamic client was configured at all (i.e. `--enable-quota-policy`), not
a separate flag that could drift from it. Returns `enabled: false` and an
empty list when unconfigured, including for the standalone `ui`
subcommand (`nfs-quota-agent ui`), which has no Kubernetes client of any
kind.

## 7. Relationship to what exists today

| Existing mechanism | Superseded? | Still works? |
|---|---|---|
| `nfs.io/default-quota` / `nfs.io/max-quota` namespace annotations | Unrelated to actual enforcement — these never fed filesystem quota sizing in the first place, only the web UI's advisory Policies/Violations display (`internal/policy`) | Yes, for that advisory display only — `GetNamespacePolicy` still reads them as source `"Annotation"`. `QuotaPolicy` is the only mechanism of the two that actually enforces a size bound. |
| `LimitRange` PVC limits | Outranked for the filesystem-enforcement value; unaffected as an admission control | Yes — still gates PVC creation exactly as before |
| `--default-quota` / `--enforce-max-quota` flags | Removed (not superseded — they were dead code before `QuotaPolicy` existed: `internal/policy.GetNamespacePolicy` never gained the "Global Default" tier its own doc comment described, so nothing ever read them) | No — removed. `QuotaPolicy`'s `defaultQuota`/`maxQuota`/`enforceMax` fields are the real replacement. |
| `nfs.io/project-name` on the PV | Unrelated | Yes — QuotaPolicy governs the size bound, not project naming/ID allocation, which is untouched |
| `nfs.io/quota-status` on the PV | Unrelated | Yes — this remains the per-PV pending/applied/failed status; `QuotaPolicy` status is a policy-level rollup, not a replacement for the per-PV annotation |

**A `QuotaPolicy` that matches no claims, or that is deleted, does not
silently ignore the annotations people already set.** Because QuotaPolicy
only outranks the annotation chain *when it wins for a given claim*,
removing the `QuotaPolicy` (or narrowing its selector) causes
`internal/policy.GetNamespacePolicy`'s existing LimitRange/Annotation/Global
chain to take back over for the now-unmatched claims — there is no state
where a claim has no effective policy because a QuotaPolicy briefly existed
and is now gone.

**Migration path**: adopting QuotaPolicy is additive and reversible.
Existing namespaces with only annotations keep working unchanged (source
stays `"Annotation"`). To migrate one namespace, create a namespace-wide
`QuotaPolicy` (empty `selector`) with the same `defaultQuota`/`maxQuota`
values as the existing annotations, verify `status.appliedClaims` matches
expectations, and then remove the annotations at leisure — removing them
before or after creating the QuotaPolicy makes no functional difference,
since QuotaPolicy already outranks them while both exist.

## 8. Conversion strategy

Single version, `v1alpha1`, while in alpha. Only additive changes are
permitted within it (new optional fields, new enum values, new condition
types/reasons) — no field removal, no semantic change to an existing
field's meaning, no renaming. There is no conversion webhook, because there
is no webhook infrastructure anywhere in this repo today (confirmed: no
`admission`, `webhook`, or `cert-manager` references in `charts/` or
`internal/`) and adding one is a materially larger scope than this design.

A `v1` would be warranted once the shape has run in the field long enough
to be confident selector semantics, precedence rules, and the condition
vocabulary won't need a breaking change — realistically after the
controller PR has been deployed and iterated on for at least one release
cycle. At that point, conversion would need:

1. A `Hub`/`Spoke` conversion implementation (controller-runtime's
   `conversion.Convertible`), with `v1alpha1` (or whichever version proves
   most stable) as the hub.
2. A `ConversionReviewVersions` webhook registered on the CRD, which
   requires standing up webhook serving (TLS via cert-manager or a
   self-managed cert, a `Service`/`Deployment` for the webhook endpoint) —
   none of which exists in this repo yet, so it is new infrastructure, not
   a small addition.

## 9. Types summary

| Type | Purpose |
|---|---|
| `QuotaPolicySelector` | `pvcName` (most specific) XOR `labelSelector`, or neither for namespace-wide |
| `QuotaPolicySpec` | `selector`, `priority`, `defaultQuota`/`maxQuota`/`minQuota` (`resource.Quantity`), `enforceMax` |
| `FailingClaim` | One entry in the bounded failing-claims sample |
| `MatchedClaim` | One entry in the bounded precedence-audit sample |
| `QuotaPolicyStatus` | `observedGeneration`, `conditions`, counts, the two bounded samples |
| `QuotaPolicy` / `QuotaPolicyList` | The CRD root types; `kubebuilder:object:root` + `subresource:status` markers present for the future `controller-gen` run |

All quantities are `resource.Quantity` (`k8s.io/apimachinery/pkg/api/resource`,
already an indirect dependency via `k8s.io/api`), never `int64` or `string`,
so they parse/print with the same `Ki`/`Mi`/`Gi` suffix conventions as every
other Kubernetes quantity field and so `kubectl` and generated clients
handle them natively.

`QuotaPolicy` and `QuotaPolicyList` implement `runtime.Object` via the
generated `DeepCopyObject`/`DeepCopyInto` in `zz_generated.deepcopy.go` (see
the package doc comment in `types.go`, and the compile-time
`var _ runtime.Object = &QuotaPolicy{}` check next to it). `GroupVersion`/
`SchemeBuilder` registration is still not done — that wiring, and the
client/lister that would use it, belongs with the controller PR.

## 10. Open questions

- **Cluster-scoped follow-on (`ClusterQuotaPolicy` or similar).** Discussed
  in §2 as deferred, not designed here.
- **What happens to `status` when a `QuotaPolicy` is created but its
  namespace doesn't exist yet, or is being deleted.** Kubernetes namespace
  lifecycle should make this rare (you can't normally create a namespaced
  object in a Terminating namespace), but the exact status shape for that
  edge isn't designed.
- **Whether `ProjectIDExhausted` is reachable in practice** given the
  existing collision-fallback behavior in `hashProjectName`, or whether the
  controller PR finds it never needs that reason — see §5.
- **Rate/order of reconciliation when many `QuotaPolicy` objects in a
  namespace change at once** (e.g. a bulk priority renumbering) is a
  controller-implementation concern, not a types/API concern, and is left
  to the follow-up PR.

## 11. Controller implementation

This section documents the controller PR (`internal/quotapolicy`,
`internal/agent/policy.go`) — the parts of §4–§6 above that needed a
concrete choice once actually implemented.

### Effective-quota semantics

`quotapolicy.EffectiveQuota(requested, spec)` computes the size to enforce
for a claim once `quotapolicy.Resolve` has picked a winning policy, applied
in this order:

1. Start from `requested` (the PV's own capacity). If `requested <= 0` and
   `spec.defaultQuota` is set, start from `defaultQuota` instead.
2. If `spec.minQuota` is set and the value is below it, raise to
   `minQuota`.
3. If `spec.maxQuota` is set and `spec.enforceMax` is `true` and the value
   exceeds it, clamp to `maxQuota`.
4. If `spec.maxQuota` is set and `spec.enforceMax` is `false` and the value
   exceeds it, the value is left unchanged — `enforceMax: false` means
   `maxQuota` is advisory, not a hard cap — but the overage is recorded
   (`quotapolicy.BoundDecision`) so the caller can log/report it rather than
   it passing unremarked.

minQuota is applied before maxQuota; since the CRD's `XValidation` rules
already reject `minQuota > maxQuota`, `defaultQuota < minQuota`, and
`defaultQuota > maxQuota` at admission time, raising to `minQuota` can never
itself push a value from a real, admitted `QuotaPolicy` back above
`maxQuota` — the two outcomes are mutually exclusive in practice for a valid
object.

### Shrink guard: refusing a decrease below current usage

#3/#14 both ask that a `QuotaPolicy` change reducing `maxQuota` below a
claim's *current* usage not be applied silently. Project quota is
inherently non-destructive here — none of the xfs/ext4/btrfs backends ever
delete or truncate existing files when a hard limit drops, they only
reject future writes once usage already exceeds the new limit (`EDQUOT`) —
so the risk isn't data loss, it's an operator changing a policy and
unknowingly cutting off a tenant's writes with no warning.

`ensureQuota` checks this immediately before calling into the filesystem
backend, whenever the new size is both a genuine decrease
(`sizeBytes < oldQuota`, not the initial apply) and would actually be
unsafe: `currentUsageBytes` reads the same `status.GetDirUsages` report the
web UI and `/metrics` already use, and the decrease is refused only when
current usage exceeds the *enforced* new limit — not the raw requested
value, and not some percentage margin below the old one.
`expectedEnforcedBytes` floors to the same KB boundary
`ApplyXFSQuota`/`ApplyExt4Quota` do before ever calling `xfs_quota`/
`setquota` (btrfs applies the exact byte value), so a request within one KB
of the boundary can't look safe against the raw value while the limit that
actually reaches the filesystem is already below current usage — the same
class of mismatch the #10 CRITICAL rounding bug was. Beyond that flooring,
this bound needs no policy judgment call: usage already above the enforced
new limit means the change would immediately start rejecting writes, true
regardless of how conservative or aggressive an operator wants shrinks to
be in general.

A refusal doesn't touch `appliedQuotas` or the filesystem at all — the
previous quota stays in force exactly as before — and surfaces through the
same `EnforcementErr`/`FailingClaims` machinery every other enforcement
failure uses, with `Reason: UnsafeShrinkRejected`, rather than a new status
field. When the usage report itself can't be read, the shrink proceeds:
`currentUsageBytes` returns `ok=false`, which this check treats as "no
evidence this is unsafe," not "assume the worst" — the alternative (block
every shrink whenever the report has a hiccup) would make legitimate
policy decreases permanently stuck behind an unrelated report failure.

**Known, accepted gaps** (an independent review pass raised both; neither
is fixed here): usage is sampled once, synchronously, inside the same
`a.mu`-held critical section that then calls `applyQuota` — an NFS client
can still write between the sample and the apply, so this narrows the
unsafe-shrink window without closing it completely (closing it fully would
need transactional filesystem semantics this agent doesn't have). And when
the quota report has no entry for a path, `GetDirUsages` falls back to
summing apparent file sizes (`filepath.Walk`), which can undercount true
usage for sparse or preallocated files — a shrink this guard treats as
safe could still turn out unsafe in that specific case. Both are pre-existing
properties of `GetDirUsages`/`GetDirSize` (already relied on by `/metrics`
and the web UI), not something this guard introduces; fixing either is a
reasonable follow-up, not a blocker for this guard being a net improvement
over not checking at all.

### Reconcile cadence, and why the watch path resolves against a cache

`QuotaPolicy` is fully *resolved* only on the agent's existing
`syncAllQuotas` cadence — there is no second watch loop or work queue for
that, since `ensureQuota` already serializes every PV through the agent's
single mutex and there is exactly one agent instance per node. But the
watch path (`internal/agent/watch.go`) still needs to *use* that
resolution, not ignore it, for a subtle reason: `ensureQuota` writes the
`nfs.io/quota-status` annotation onto the PV it just enforced a quota on,
which generates a `Modified` watch event for that same PV. If the watch
handler responded to that event with the PV's raw capacity (no policy
context at all), it would immediately re-apply the unclamped size and undo
whatever the sync just enforced — the agent fighting itself, oscillating
between clamped and unclamped every cycle, spending most of the interval
*unclamped*.

The fix: `beginQuotaPolicyCycle` publishes a `resolvedPolicySnapshot` (the
namespace→policies map and the PVC labels needed to match them) each sync
cycle, guarded by `QuotaAgent.mu`. `watch.go`'s Added/Modified handler calls
`resolveFromSnapshot(pv)`, which runs the same `quotapolicy.Resolve` /
`EffectiveQuota` the sync path uses, against that cached snapshot, before
calling `ensureQuota`. This means:

- A watch event between sync cycles resolves policy correctly (lagging the
  policy set itself by at most one sync interval — acceptable, and honest,
  since nothing claims tighter freshness).
- The specific oscillation above cannot happen: the watch handler computes
  the *same* effective size the last sync did for an unchanged claim, so
  `ensureQuota`'s cache-hit early return fires and nothing is reapplied.
- Before the first sync completes (no snapshot published yet), or with the
  feature off, `resolveFromSnapshot` returns 0 — apply the PV's own
  capacity, the correct and only sane behavior with nothing resolved yet.

The published snapshot's maps are never mutated after construction, so the
watch goroutine reading them concurrently with a sync cycle running in the
main loop is safe without extra locking beyond the pointer swap itself.

### Multi-writer status: this chart's DaemonSet can run on several nodes

`charts/nfs-quota-agent/values.yaml`'s `nodeSelector` comment explicitly
supports several NFS server nodes ("add each node's label here"), meaning
several `QuotaAgent` instances can be `--enable-quota-policy` at once. Left
unaddressed, this breaks `QuotaPolicy` status two ways:

1. **Honesty**: `syncAllQuotas` lists PVs cluster-wide, but a given node
   only has a local directory for the claims its own export backs. Without
   filtering, every node would compute the *same* `matchedClaims` (it lists
   the same policies) but a *different* `appliedClaims` (only the claims it
   can actually enforce) — and, worse, `ensureQuota` returning `nil` on a
   missing directory (a deliberate, correct "not mine to enforce" skip, not
   a failure) would get counted as a successful application if nothing
   distinguished it, making `Applied=True` a false statement. Fixed with a
   `hasLocalDir` check in the `syncAllQuotas` loop: a claim without a local
   directory is (a) excluded entirely from this node's resolution when
   `quotaPolicySingleWriter` is false — some other node presumably owns it
   — or (b) resolved and recorded as a real failure
   (`errLocalDirectoryMissing` → `ReasonFilesystemUnavailable`) when
   `quotaPolicySingleWriter` is true, since then there *is* no other node to
   blame it on.
2. **Convergence**: even with (1)'s honest, per-node counts, N nodes each
   calling `UpdateStatus` with their own correct-but-partial view would
   still make the object's status flap between N different snapshots every
   cycle, never settling — which reads as an intermittent bug, not a
   deliberate degradation, and is worse than not reporting.

The fix for (2): `finishQuotaPolicyCycle` writes status only when
`quotaPolicySingleWriter` is `true` (flag: `--quota-policy-single-writer`,
chart: `quotaPolicy.singleWriter`, both default `false`). Left `false`
(the default, since the chart's own multi-node support means this can never
be safely assumed), the agent still enforces quotas exactly as resolved —
only the `QuotaPolicy` *status* write-back is skipped, logged once, not
silently. Real leader election (a `coordination.k8s.io` Lease) would let
this work unattended on multiple nodes without an operator declaration, but
that needs its own RBAC grant and is a materially larger change than this
PR; left as a follow-up.

`quotapolicy.WriteStatus` also retries once on `apierrors.IsConflict`
(re-`Get`, re-apply, re-`UpdateStatus`) — a `Get`-then-`UpdateStatus` can
still lose a race against whoever manages the CR's spec even with a single
`QuotaPolicy` writer, and that's a benign, common race that shouldn't fail
the whole sync cycle.

### `Drifted`: independent read-back, not the enforcement cache

`Drifted` is now set (#13), once #10 added the read-back mechanism §5's
"if you cannot determine it cheaply and honestly, omit the condition
entirely" rule was blocking on: `syncAllQuotas` independently reads back
the filesystem's actual project quota (`xfs_quota report` / `repquota` /
`btrfs qgroup show`, fetched once per sync cycle and reused across every
matched claim, not once per claim) and compares it against what the
policy currently specifies — deliberately *not* the agent's own
`appliedQuotas` cache, which only reflects what the agent last believes it
applied, not an independent observation of on-disk state.

A claim `ensureQuota` actually mutated this same cycle is excluded from
the check: its own apply-time read-back (#10's `verifyQuotaOnDisk`)
already confirmed it moments ago, and comparing it against a
report snapshot that may predate that exact mutation would misreport a
brand-new, correctly-applied value as drift (an independent review caught
this as the first version's most serious bug before it shipped). Only
claims that were untouched this cycle — genuine cache hits, where nothing
about their on-disk state could have changed as a side effect of this same
sync — are compared against the shared snapshot.

`Drifted` has three states, not two: `True` (confirmed mismatch — see
`status.driftedClaims`), `False` (checked, and every won claim matched),
and `Unknown` (the report itself couldn't be read this cycle — a
transient `xfs_quota`/`repquota`/`btrfs` failure). `Unknown` exists
specifically so a report outage can't masquerade as a healthy `False`;
an operator relying on this condition needs to be able to tell "checked,
fine" apart from "couldn't check."

**Known, accepted gap**: the shared report snapshot can still be stale
relative to a mutation the *watch path's* reconcile queue makes
concurrently, in a separate goroutine, to a claim this same cycle also
drift-checks against it. The exclusion above only covers mutations this
same `syncAllQuotas` call itself makes (via `ensureQuotaMutated`'s own
return value — not an inferred before/after cache comparison, which an
earlier version used and which turned out to be vulnerable to the same
kind of race). Fully closing the concurrent-watch-path case would mean
either re-fetching the report per claim (defeating the fetch-once
optimization) or locking a claim's check against concurrent
reconciliation of that same claim, which the reconcile queue doesn't
currently expose a hook for. The failure mode is a spurious `Drifted=True`
for one claim, self-correcting on the next cycle — not a missed real
problem or an enforcement error.

### RBAC: two new grants beyond the CRD-only ClusterRole, both gated on `quotaPolicy.enabled`

Resolving `spec.selector.labelSelector` needs each claim's PVC labels, which
the agent did not previously read at all. The ClusterRole now also grants
`get`/`list`/`watch` on core `persistentvolumeclaims`, matching the verb set
already used for `namespaces`/`limitranges`/`resourcequotas`. The
`quotapolicies`/`quotapolicies/status` grants were already present from the
CRD-only PR and didn't need widening. All three QuotaPolicy-related rules
are wrapped in `{{- if .Values.quotaPolicy.enabled }}` in
`clusterrole.yaml`: this branch's own history already removed a
cluster-wide `PersistentVolumeClaim` read rule once because nothing called
it, and granting cluster-wide read on other tenants' PVC names/labels
unconditionally — to every existing deployment that has this feature off by
default — would repeat that defect for zero capability gained. Verified by
rendering both ways: `quotaPolicy.enabled=false` produces a ClusterRole with
none of the three rules; `=true` produces all three.
