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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// ComputeDecisionID returns a deterministic short hash (16 hex characters,
// 64-bit truncated SHA-256) uniquely identifying an enforcement decision
// per (PV, policy UID, policy generation, bound outcome, effective bytes)
// -- #14's admission<->enforcement correlation.
//
// Retries, reconcile loops, and process restarts yield the identical
// decision ID for the same decision inputs, allowing cross-system
// correlation between admission evidence (kubectl describe pv), audit logs,
// and agent structured logs.
func ComputeDecisionID(pvName string, policyUID string, policyGeneration int64, outcome string, effectiveBytes int64) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s/%s/%d/%s/%d", pvName, policyUID, policyGeneration, outcome, effectiveBytes)
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:8])
}

// FormatPolicyDecision returns the value for the nfs.io/policy-decision
// PV annotation in format: <policy-name>/<generation>/<outcome>/<id>.
func FormatPolicyDecision(policyName string, policyGeneration int64, outcome string, decisionID string) string {
	return fmt.Sprintf("%s/%d/%s/%s", policyName, policyGeneration, outcome, decisionID)
}
