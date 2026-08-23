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
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/dasomel/nfs-quota-agent/internal/apis/quota/v1alpha1"
)

func gi(n int64) *resource.Quantity {
	q := resource.NewQuantity(n*1024*1024*1024, resource.BinarySI)
	return q
}

// TestEffectiveQuota: every case that sets MaxQuota also sets EnforceMax
// explicitly (true or false), never relying on the struct literal's Go zero
// value. This matters because the CRD's +kubebuilder:default=true for
// enforceMax is an API-server admission-time default (confirmed live: a
// QuotaPolicy applied with enforceMax omitted comes back as
// "enforceMax":true) -- a bare Go struct literal that "omits" EnforceMax
// actually gets false (advisory), the shape the API server never returns
// for an object that really omitted it. A case meant to exercise clamping
// that forgot EnforceMax: true would silently test the advisory branch
// instead while still passing, so every case states its intent explicitly.
func TestEffectiveQuota(t *testing.T) {
	tests := []struct {
		name      string
		requested int64
		spec      v1alpha1.QuotaPolicySpec
		want      int64
		wantOut   BoundOutcome
	}{
		{
			name:      "unchanged when within bounds",
			requested: 5 * 1024 * 1024 * 1024,
			spec:      v1alpha1.QuotaPolicySpec{MinQuota: gi(1), MaxQuota: gi(10), EnforceMax: true},
			want:      5 * 1024 * 1024 * 1024,
			wantOut:   BoundUnchanged,
		},
		{
			name:      "no bounds set at all is unchanged",
			requested: 5 * 1024 * 1024 * 1024,
			spec:      v1alpha1.QuotaPolicySpec{},
			want:      5 * 1024 * 1024 * 1024,
			wantOut:   BoundUnchanged,
		},
		{
			name:      "non-positive requested falls back to defaultQuota",
			requested: 0,
			spec:      v1alpha1.QuotaPolicySpec{DefaultQuota: gi(2)},
			want:      2 * 1024 * 1024 * 1024,
			wantOut:   BoundUsedDefault,
		},
		{
			name:      "negative requested also falls back to defaultQuota",
			requested: -1,
			spec:      v1alpha1.QuotaPolicySpec{DefaultQuota: gi(3)},
			want:      3 * 1024 * 1024 * 1024,
			wantOut:   BoundUsedDefault,
		},
		{
			name:      "non-positive requested with no defaultQuota stays as-is",
			requested: 0,
			spec:      v1alpha1.QuotaPolicySpec{},
			want:      0,
			wantOut:   BoundUnchanged,
		},
		{
			name:      "below minQuota is raised",
			requested: 1 * 1024 * 1024 * 1024,
			spec:      v1alpha1.QuotaPolicySpec{MinQuota: gi(5)},
			want:      5 * 1024 * 1024 * 1024,
			wantOut:   BoundRaisedToMin,
		},
		{
			name:      "above maxQuota with enforceMax=true is clamped",
			requested: 20 * 1024 * 1024 * 1024,
			spec:      v1alpha1.QuotaPolicySpec{MaxQuota: gi(10), EnforceMax: true},
			want:      10 * 1024 * 1024 * 1024,
			wantOut:   BoundClampedToMax,
		},
		{
			name:      "above maxQuota with enforceMax=false is left alone but reported",
			requested: 20 * 1024 * 1024 * 1024,
			spec:      v1alpha1.QuotaPolicySpec{MaxQuota: gi(10), EnforceMax: false},
			want:      20 * 1024 * 1024 * 1024,
			wantOut:   BoundAdvisoryOverage,
		},
		{
			name:      "default then raised to min",
			requested: 0,
			spec:      v1alpha1.QuotaPolicySpec{DefaultQuota: gi(1), MinQuota: gi(4)},
			want:      4 * 1024 * 1024 * 1024,
			wantOut:   BoundRaisedToMin,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, decision := EffectiveQuota(tt.requested, tt.spec)
			if got != tt.want {
				t.Fatalf("EffectiveQuota() = %d, want %d", got, tt.want)
			}
			if decision.Outcome != tt.wantOut {
				t.Fatalf("Outcome = %s, want %s (detail: %s)", decision.Outcome, tt.wantOut, decision.Detail)
			}
			if decision.Detail == "" {
				t.Fatalf("expected a non-empty Detail message")
			}
		})
	}
}
