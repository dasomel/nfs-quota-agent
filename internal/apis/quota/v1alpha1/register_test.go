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

package v1alpha1

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

// TestAddToSchemeRegistersQuotaPolicy is the concrete proof that
// AddToScheme actually wires QuotaPolicy into a runtime.Scheme, not just
// that this package compiles. Building a clientset/codec against a type
// that ObjectKinds can't resolve fails at first use, not at compile time,
// so this is the check that matters.
func TestAddToSchemeRegistersQuotaPolicy(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v, want nil", err)
	}

	gvks, _, err := scheme.ObjectKinds(&QuotaPolicy{})
	if err != nil {
		t.Fatalf("scheme.ObjectKinds(&QuotaPolicy{}) error = %v, want nil", err)
	}
	if len(gvks) != 1 {
		t.Fatalf("scheme.ObjectKinds(&QuotaPolicy{}) returned %d GVKs, want 1: %v", len(gvks), gvks)
	}
	if got := gvks[0]; got != SchemeGroupVersion.WithKind(QuotaPolicyKind) {
		t.Errorf("scheme.ObjectKinds(&QuotaPolicy{}) = %v, want %v", got, SchemeGroupVersion.WithKind(QuotaPolicyKind))
	}

	listGVKs, _, err := scheme.ObjectKinds(&QuotaPolicyList{})
	if err != nil {
		t.Fatalf("scheme.ObjectKinds(&QuotaPolicyList{}) error = %v, want nil", err)
	}
	if len(listGVKs) != 1 {
		t.Fatalf("scheme.ObjectKinds(&QuotaPolicyList{}) returned %d GVKs, want 1: %v", len(listGVKs), listGVKs)
	}
	if got := listGVKs[0]; got != SchemeGroupVersion.WithKind("QuotaPolicyList") {
		t.Errorf("scheme.ObjectKinds(&QuotaPolicyList{}) = %v, want %v", got, SchemeGroupVersion.WithKind("QuotaPolicyList"))
	}

	if got, want := SchemeGroupVersion.Group, GroupName; got != want {
		t.Errorf("SchemeGroupVersion.Group = %q, want %q", got, want)
	}
	if got, want := SchemeGroupVersion.Version, GroupVersion; got != want {
		t.Errorf("SchemeGroupVersion.Version = %q, want %q", got, want)
	}
}
