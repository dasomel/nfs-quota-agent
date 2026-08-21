# QuotaPolicy Custom Resource — Design

Status: design-and-types only (issue #13). No controller, no generated CRD
YAML, no deepcopy. Those land in a follow-up PR once this shape is agreed.
Types live at [`internal/apis/quota/v1alpha1/types.go`](../internal/apis/quota/v1alpha1/types.go).

## 1. Problem

Today, quota policy for a namespace comes from one of three sources,
resolved by [`internal/policy.GetNamespacePolicy`](../internal/policy/policy.go)
in strict priority order:

1. a `LimitRange` with a `PersistentVolumeClaim` limit entry (`Max`/`Min`/`Default`)
2. the `nfs.io/default-quota` / `nfs.io/max-quota` namespace annotations
3. a global default passed to the agent as a flag

This is namespace-wide only — there is no way to give one PVC, or one group
of PVCs selected by label, a different bound than the rest of the
namespace — and it is imperative rather than declarative: the annotations
are values on a `Namespace` object, not a reviewable, GitOps-able resource
with its own status. Issue #13 asks for a CRD that closes both gaps.

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
`FilesystemUnavailable`, `ProjectIDExhausted`, `QuotaDriftDetected`,
`NoDrift`, `ExceedsLimitRangeMax`, `WithinLimitRange`, `NoLimitRange`) so
that dashboards and scripts built against them don't rot as the controller
grows more call sites. `ProjectIDExhausted` anticipates the collision
fallback in `hashProjectName` running out of room, though the controller PR
may find it never needs to report that reason in practice — it's included
now so the vocabulary doesn't need a breaking addition for it later.

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

## 7. Relationship to what exists today

| Existing mechanism | Superseded? | Still works? |
|---|---|---|
| `nfs.io/default-quota` / `nfs.io/max-quota` namespace annotations | Outranked, not removed | Yes — still evaluated as source `"Annotation"` for namespaces with no matching `QuotaPolicy` |
| `LimitRange` PVC limits | Outranked for the filesystem-enforcement value; unaffected as an admission control | Yes — still gates PVC creation exactly as before |
| Global default flag | Outranked | Yes — unchanged as the bottom of the chain |
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

The package deliberately does **not** implement `runtime.Object` yet (see
the package doc comment in `types.go`) — `DeepCopyObject`/`DeepCopyInto` and
`GroupVersion`/`SchemeBuilder` registration are `controller-gen` output that
belongs with the CRD YAML in the controller PR, not hand-written ahead of
it.

## 10. Open questions

- **Cross-field validation of `minQuota <= maxQuota`.** CEL
  (`+kubebuilder:validation:XValidation`) can express this using the
  `quantity()` CEL library extension, but that extension's availability
  depends on the Kubernetes API server version, and this design doesn't
  confirm which server versions this chart is expected to support alpha
  CRDs against. Left as a marker to add in the controller PR once that's
  confirmed, rather than asserted here without verification. Until then,
  an inverted `minQuota`/`maxQuota` is only caught at reconcile time
  (candidate: a `Ready=False`/`SelectorInvalid`-adjacent reason — exact
  reason TBD).
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
