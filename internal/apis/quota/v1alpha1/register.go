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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// SchemeGroupVersion is the schema.GroupVersion for this package, built
// from the GroupName/GroupVersion constants in types.go rather than a
// second literal, so the two can never drift apart. This is what was
// missing per the "not registered" note on types.go and
// docs/quotapolicy-design.md §9: without it nothing can build a typed
// client-go clientset for QuotaPolicy, even though the type already
// satisfies runtime.Object (see the compile-time check in types.go).
// internal/quotapolicy deliberately keeps using the dynamic/unstructured
// client (see docs/quotapolicy-design.md §11) — this registration exists
// for future/external tooling (kubectl plugins, other controllers), not to
// change that package's own client.
var SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: GroupVersion}

// SchemeBuilder collects addKnownTypes so it can be added to a
// runtime.Scheme via AddToScheme. Kept as a package-level var, matching the
// standard k8s.io/apimachinery API-group registration pattern used by
// generated clientsets.
var (
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	// AddToScheme registers QuotaPolicy and QuotaPolicyList (and the
	// metav1 list-options types they need) with a runtime.Scheme, e.g. for
	// building a typed client-go clientset or codec against this type.
	AddToScheme = SchemeBuilder.AddToScheme
)

// addKnownTypes registers QuotaPolicy and QuotaPolicyList under
// SchemeGroupVersion, and the common metav1 meta types every API group
// needs (ListOptions, GetOptions, etc.) so codecs built from this scheme
// can encode/decode those too.
func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion,
		&QuotaPolicy{},
		&QuotaPolicyList{},
	)
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}
