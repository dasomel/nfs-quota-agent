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

package quotapolicy

import (
	"fmt"

	"github.com/dasomel/nfs-quota-agent/internal/apis/quota/v1alpha1"
)

// BoundOutcome enumerates what EffectiveQuota did to reach its result.
type BoundOutcome string

const (
	// BoundUnchanged: the starting value (requested, or defaultQuota when
	// requested was substituted for it) already satisfied minQuota and
	// maxQuota, so the returned value equals the starting value.
	BoundUnchanged BoundOutcome = "Unchanged"
	// BoundUsedDefault: requested was <= 0, so defaultQuota was used as the
	// starting point instead of the PV's own capacity.
	BoundUsedDefault BoundOutcome = "UsedDefault"
	// BoundRaisedToMin: the starting value was below minQuota and was
	// raised to meet it.
	BoundRaisedToMin BoundOutcome = "RaisedToMin"
	// BoundClampedToMax: enforceMax is true and the value exceeded
	// maxQuota, so it was clamped down to maxQuota.
	BoundClampedToMax BoundOutcome = "ClampedToMax"
	// BoundAdvisoryOverage: enforceMax is false and the value exceeds
	// maxQuota. The value is left unchanged — maxQuota is advisory only in
	// this mode — but the overage is recorded here so the caller can
	// surface it (log line / status), rather than the excess passing
	// unremarked.
	BoundAdvisoryOverage BoundOutcome = "AdvisoryOverage"
)

// BoundDecision explains what EffectiveQuota did and why, for the caller to
// use in conditions and log messages.
type BoundDecision struct {
	Outcome BoundOutcome
	Detail  string
}

// EffectiveQuota returns the filesystem quota size (bytes) to enforce for a
// claim whose winning policy is spec, given the PV's requested capacity in
// bytes. Semantics, applied in exactly this order:
//
//  1. Start from requested. If requested <= 0 and spec.DefaultQuota is set,
//     start from spec.DefaultQuota instead. (A bound PV should always carry
//     a positive capacity in practice; this only matters for a
//     hand-constructed or degenerate claim.)
//  2. If spec.MinQuota is set and the value is below it, raise to
//     MinQuota.
//  3. If spec.MaxQuota is set and spec.EnforceMax is true and the value
//     exceeds it, clamp to MaxQuota.
//  4. If spec.MaxQuota is set and spec.EnforceMax is false and the value
//     exceeds it, leave the value unchanged and report the overage as
//     advisory via BoundDecision, rather than the caller silently allowing
//     it unremarked.
//
// minQuota is applied before maxQuota so an (admission-time-impossible, but
// defense-in-depth) minQuota > maxQuota spec still produces a deterministic
// result (clamped down to maxQuota) instead of depending on evaluation
// order. In practice the CRD's XValidation rules already reject
// minQuota > maxQuota, defaultQuota < minQuota, and defaultQuota > maxQuota
// at admission time, so raising to minQuota can never itself push the value
// back above maxQuota for a real, admitted QuotaPolicy — the two outcomes
// (RaisedToMin and ClampedToMax/AdvisoryOverage) are mutually exclusive in
// practice, and this function reports only the last one it applied.
func EffectiveQuota(requested int64, spec v1alpha1.QuotaPolicySpec) (int64, BoundDecision) {
	value := requested
	decision := BoundDecision{Outcome: BoundUnchanged, Detail: "requested capacity used as-is"}

	if value <= 0 && spec.DefaultQuota != nil {
		value = spec.DefaultQuota.Value()
		decision = BoundDecision{
			Outcome: BoundUsedDefault,
			Detail:  fmt.Sprintf("requested capacity was %d; used defaultQuota %s", requested, spec.DefaultQuota.String()),
		}
	}

	if spec.MinQuota != nil {
		if min := spec.MinQuota.Value(); value < min {
			value = min
			decision = BoundDecision{
				Outcome: BoundRaisedToMin,
				Detail:  fmt.Sprintf("raised to minQuota %s", spec.MinQuota.String()),
			}
		}
	}

	if spec.MaxQuota != nil {
		if max := spec.MaxQuota.Value(); value > max {
			if spec.EnforceMax {
				value = max
				decision = BoundDecision{
					Outcome: BoundClampedToMax,
					Detail:  fmt.Sprintf("clamped to maxQuota %s (enforceMax=true)", spec.MaxQuota.String()),
				}
			} else {
				decision = BoundDecision{
					Outcome: BoundAdvisoryOverage,
					Detail:  fmt.Sprintf("exceeds maxQuota %s but enforceMax=false; left unchanged (advisory only)", spec.MaxQuota.String()),
				}
			}
		}
	}

	return value, decision
}
