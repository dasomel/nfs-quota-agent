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

// list.go lists and writes back QuotaPolicy objects via the dynamic client,
// converting to/from the typed v1alpha1.QuotaPolicy with
// runtime.DefaultUnstructuredConverter. This repo deliberately uses raw
// client-go with no informers and no generated clientset for this type (see
// CLAUDE.md and docs/quotapolicy-design.md) — no controller-runtime, no
// kubebuilder scaffolding.

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/dasomel/nfs-quota-agent/internal/apis/quota/v1alpha1"
)

// GroupVersionResource identifies the QuotaPolicy CRD for the dynamic
// client. QuotaPolicy is namespace-scoped (docs/quotapolicy-design.md §2),
// so every call below goes through Namespace(ns) rather than the
// cluster-scoped resource interface.
var GroupVersionResource = schema.GroupVersionResource{
	Group:    v1alpha1.GroupName,
	Version:  v1alpha1.GroupVersion,
	Resource: "quotapolicies",
}

// crdMissingLogOnce makes the "CRD not installed" notice fire once per
// process rather than once per sync cycle — see List's doc comment.
var crdMissingLogOnce sync.Once

// List returns every QuotaPolicy across all namespaces.
//
// If the CRD is not installed in the cluster, List degrades to "no
// policies" instead of returning an error: apierrors.IsNotFound and
// meta.IsNoMatchError both mean the API server has no route for this
// GroupVersionResource, which is the expected steady state for anyone
// running with quotaPolicy.enabled=true before (or without ever) applying
// the CRD. That is logged at Info once per process — not once per sync
// cycle, which would spam a long-running agent's logs forever — and the
// caller proceeds exactly as if the list came back empty, never failing
// the sync cycle over it.
func List(ctx context.Context, client dynamic.Interface) ([]v1alpha1.QuotaPolicy, error) {
	list, err := client.Resource(GroupVersionResource).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			crdMissingLogOnce.Do(func() {
				slog.Info("QuotaPolicy CRD not installed; quota policy enforcement behaves as if no policies exist", "error", err)
			})
			return nil, nil
		}
		return nil, fmt.Errorf("failed to list QuotaPolicy objects: %w", err)
	}

	policies := make([]v1alpha1.QuotaPolicy, 0, len(list.Items))
	for i := range list.Items {
		var p v1alpha1.QuotaPolicy
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(list.Items[i].Object, &p); err != nil {
			slog.Error("Failed to convert QuotaPolicy from unstructured, skipping it this cycle",
				"namespace", list.Items[i].GetNamespace(), "name", list.Items[i].GetName(), "error", err)
			continue
		}
		policies = append(policies, p)
	}
	return policies, nil
}

// writeStatusMaxAttempts bounds WriteStatus's Get-then-UpdateStatus retry on
// a resourceVersion conflict. One retry (two attempts total) is enough to
// win against a single concurrent writer — the CR's own manager patching
// spec, or (if the caller has confirmed a single-writer topology; see
// docs/quotapolicy-design.md §11) nothing else, since only one agent
// writes status at all. It is not a substitute for actual single-writer
// enforcement: a genuine multi-writer race would just keep colliding, which
// is exactly why the agent gates WriteStatus behind
// quotaPolicySingleWriter rather than relying on retries to paper over it.
const writeStatusMaxAttempts = 2

// WriteStatus writes status back to the cluster for the QuotaPolicy named
// policy.Namespace/policy.Name, via the status subresource (UpdateStatus).
// It re-Gets the object immediately before writing so it carries a current
// resourceVersion — the value on policy may be from an earlier List() this
// same cycle — and only ever touches the status field, so it can't stomp a
// concurrent spec edit by whoever manages the CR. On an
// apierrors.IsConflict (the object changed between the Get and the
// UpdateStatus — e.g. its owner edited spec, or metadata.generation ticked
// up mid-write), it re-Gets once and retries rather than failing the whole
// sync cycle over a benign, common race.
func WriteStatus(ctx context.Context, client dynamic.Interface, policy *v1alpha1.QuotaPolicy, status v1alpha1.QuotaPolicyStatus) error {
	res := client.Resource(GroupVersionResource).Namespace(policy.Namespace)

	statusMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
	if err != nil {
		return fmt.Errorf("failed to convert QuotaPolicyStatus to unstructured: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < writeStatusMaxAttempts; attempt++ {
		current, err := res.Get(ctx, policy.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get QuotaPolicy %s/%s for status update: %w", policy.Namespace, policy.Name, err)
		}

		if err := unstructured.SetNestedMap(current.Object, statusMap, "status"); err != nil {
			return fmt.Errorf("failed to set status field on QuotaPolicy %s/%s: %w", policy.Namespace, policy.Name, err)
		}

		_, err = res.UpdateStatus(ctx, current, metav1.UpdateOptions{})
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return fmt.Errorf("failed to update status for QuotaPolicy %s/%s: %w", policy.Namespace, policy.Name, err)
		}
		lastErr = err
	}
	return fmt.Errorf("failed to update status for QuotaPolicy %s/%s after %d attempts (resourceVersion conflict): %w",
		policy.Namespace, policy.Name, writeStatusMaxAttempts, lastErr)
}
