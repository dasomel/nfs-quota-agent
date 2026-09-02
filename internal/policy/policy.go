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

// Package policy reads advisory quota policy for Kubernetes namespaces, for
// the web UI's Policies/Violations views. It retrieves policy rules from
// LimitRanges, ResourceQuotas, or namespace annotations, and reports
// whether existing PersistentVolumes exceed them — informationally only:
// nothing in this package gates or influences actual filesystem quota
// sizing, which lives in internal/agent and internal/quota.
package policy

import (
	"context"
	"fmt"
	"log/slog"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/dasomel/nfs-quota-agent/internal/util"
)

const (
	// Namespace annotations for quota policy (fallback when no LimitRange)
	AnnotationDefaultQuota = "nfs.io/default-quota"
	AnnotationMaxQuota     = "nfs.io/max-quota"
)

// NamespacePolicy represents quota policy for a namespace
type NamespacePolicy struct {
	Namespace string `json:"namespace"`

	// LimitRange values (primary source)
	LimitRangeName    string `json:"limitRangeName,omitempty"`
	LimitRangeMax     int64  `json:"limitRangeMax,omitempty"`
	LimitRangeMin     int64  `json:"limitRangeMin,omitempty"`
	LimitRangeDefault int64  `json:"limitRangeDefault,omitempty"`
	LimitRangeMaxStr  string `json:"limitRangeMaxStr,omitempty"`
	LimitRangeMinStr  string `json:"limitRangeMinStr,omitempty"`
	LimitRangeDefStr  string `json:"limitRangeDefStr,omitempty"`

	// ResourceQuota values (namespace total)
	ResourceQuotaName    string `json:"resourceQuotaName,omitempty"`
	ResourceQuotaHard    int64  `json:"resourceQuotaHard,omitempty"`
	ResourceQuotaUsed    int64  `json:"resourceQuotaUsed,omitempty"`
	ResourceQuotaHardStr string `json:"resourceQuotaHardStr,omitempty"`
	ResourceQuotaUsedStr string `json:"resourceQuotaUsedStr,omitempty"`

	// Effective values (computed from LimitRange > Annotation)
	DefaultQuota int64  `json:"defaultQuota"`
	MaxQuota     int64  `json:"maxQuota"`
	MinQuota     int64  `json:"minQuota"`
	DefaultStr   string `json:"defaultStr"`
	MaxStr       string `json:"maxStr"`
	MinStr       string `json:"minStr"`

	// Source of effective values. There is no "Global" tier: a
	// --default-quota-style fallback was planned (see the removed
	// ValidateQuota) but never implemented here, so this only ever reads
	// "LimitRange", "Annotation", or "None".
	Source string `json:"source"` // "LimitRange", "Annotation", "None"
}

// Violation represents a quota policy violation
type Violation struct {
	Namespace      string `json:"namespace"`
	PVCName        string `json:"pvcName"`
	PVName         string `json:"pvName"`
	RequestedBytes int64  `json:"requestedBytes"`
	RequestedStr   string `json:"requestedStr"`
	MaxQuotaBytes  int64  `json:"maxQuotaBytes"`
	MaxQuotaStr    string `json:"maxQuotaStr"`
	MinQuotaBytes  int64  `json:"minQuotaBytes,omitempty"`
	MinQuotaStr    string `json:"minQuotaStr,omitempty"`
	ViolationType  string `json:"violationType"` // "exceeds_max", "below_min"
}

// GetNamespacePolicy retrieves quota policy for a namespace, for the web
// UI's advisory namespace policy/violations display (internal/ui) — it does
// not gate or influence actual quota sizing, which lives entirely in
// internal/agent/internal/quota.
// Priority: LimitRange > Namespace Annotation
func GetNamespacePolicy(ctx context.Context, client kubernetes.Interface, namespace string) (*NamespacePolicy, error) {
	if client == nil {
		return nil, fmt.Errorf("kubernetes client not available")
	}

	p := &NamespacePolicy{
		Namespace: namespace,
		Source:    "None",
	}

	// 1. Try to get LimitRange for PVC. Admission applies all LimitRanges in
	// the namespace, so aggregate across every LimitRange and every PVC item:
	// effective min is the largest min, effective max is the smallest max.
	limitRanges, err := client.CoreV1().LimitRanges(namespace).List(ctx, metav1.ListOptions{})
	if err == nil && len(limitRanges.Items) > 0 {
		var hasMax, hasMin bool
		for _, lr := range limitRanges.Items {
			for _, limit := range lr.Spec.Limits {
				if limit.Type != v1.LimitTypePersistentVolumeClaim {
					continue
				}
				p.Source = "LimitRange"
				if p.LimitRangeName == "" {
					p.LimitRangeName = lr.Name
				}

				// Max storage: smallest max across all LimitRanges and items
				if max, ok := limit.Max[v1.ResourceStorage]; ok {
					maxVal := max.Value()
					if !hasMax || maxVal < p.LimitRangeMax {
						p.LimitRangeMax = maxVal
						p.LimitRangeMaxStr = max.String()
						p.MaxQuota = maxVal
						p.MaxStr = max.String()
						hasMax = true
					}
				}

				// Min storage: largest min across all LimitRanges and items
				if min, ok := limit.Min[v1.ResourceStorage]; ok {
					minVal := min.Value()
					if !hasMin || minVal > p.LimitRangeMin {
						p.LimitRangeMin = minVal
						p.LimitRangeMinStr = min.String()
						p.MinQuota = minVal
						p.MinStr = min.String()
						hasMin = true
					}
				}

				// Default storage (first encountered)
				if def, ok := limit.Default[v1.ResourceStorage]; ok {
					if p.LimitRangeDefault == 0 {
						p.LimitRangeDefault = def.Value()
						p.LimitRangeDefStr = def.String()
						p.DefaultQuota = def.Value()
						p.DefaultStr = def.String()
					}
				}

				// DefaultRequest (used when no request specified)
				if defReq, ok := limit.DefaultRequest[v1.ResourceStorage]; ok {
					if p.DefaultQuota == 0 {
						p.DefaultQuota = defReq.Value()
						p.DefaultStr = defReq.String()
					}
				}
			}
		}
	}

	// 2. Get ResourceQuota for namespace total storage
	resourceQuotas, err := client.CoreV1().ResourceQuotas(namespace).List(ctx, metav1.ListOptions{})
	if err == nil && len(resourceQuotas.Items) > 0 {
		for _, rq := range resourceQuotas.Items {
			// Check for storage quota
			if hard, ok := rq.Spec.Hard[v1.ResourceRequestsStorage]; ok {
				p.ResourceQuotaName = rq.Name
				p.ResourceQuotaHard = hard.Value()
				p.ResourceQuotaHardStr = hard.String()

				// Get used amount
				if used, ok := rq.Status.Used[v1.ResourceRequestsStorage]; ok {
					p.ResourceQuotaUsed = used.Value()
					p.ResourceQuotaUsedStr = used.String()
				}
				break
			}
		}
	}

	// 3. Fallback to namespace annotations if no LimitRange
	if p.Source == "None" {
		ns, err := client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
		if err == nil && ns.Annotations != nil {
			// Parse default quota annotation
			if defaultStr, ok := ns.Annotations[AnnotationDefaultQuota]; ok {
				if bytes, err := ParseQuotaSize(defaultStr); err == nil {
					p.DefaultQuota = bytes
					p.DefaultStr = defaultStr
					p.Source = "Annotation"
				} else {
					slog.Warn("Invalid default quota annotation",
						"namespace", namespace,
						"value", defaultStr,
						"error", err,
					)
				}
			}

			// Parse max quota annotation
			if maxStr, ok := ns.Annotations[AnnotationMaxQuota]; ok {
				if bytes, err := ParseQuotaSize(maxStr); err == nil {
					p.MaxQuota = bytes
					p.MaxStr = maxStr
					p.Source = "Annotation"
				} else {
					slog.Warn("Invalid max quota annotation",
						"namespace", namespace,
						"value", maxStr,
						"error", err,
					)
				}
			}
		}
	}

	return p, nil
}

// GetAllNamespacePolicies returns policies for all namespaces with LimitRange or ResourceQuota
func GetAllNamespacePolicies(ctx context.Context, client kubernetes.Interface) ([]NamespacePolicy, error) {
	if client == nil {
		return nil, fmt.Errorf("kubernetes client not available")
	}

	// Track namespaces with policies
	namespacesWithPolicy := make(map[string]bool)

	// Find namespaces with LimitRanges for PVC
	limitRanges, err := client.CoreV1().LimitRanges("").List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, lr := range limitRanges.Items {
			for _, limit := range lr.Spec.Limits {
				if limit.Type == v1.LimitTypePersistentVolumeClaim {
					namespacesWithPolicy[lr.Namespace] = true
					break
				}
			}
		}
	}

	// Find namespaces with ResourceQuotas for storage
	resourceQuotas, err := client.CoreV1().ResourceQuotas("").List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, rq := range resourceQuotas.Items {
			if _, ok := rq.Spec.Hard[v1.ResourceRequestsStorage]; ok {
				namespacesWithPolicy[rq.Namespace] = true
			}
		}
	}

	// Find namespaces with quota annotations
	nsList, err := client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, ns := range nsList.Items {
			if ns.Annotations != nil {
				_, hasDefault := ns.Annotations[AnnotationDefaultQuota]
				_, hasMax := ns.Annotations[AnnotationMaxQuota]
				if hasDefault || hasMax {
					namespacesWithPolicy[ns.Name] = true
				}
			}
		}
	}

	// Get full policy for each namespace
	var policies []NamespacePolicy
	for namespace := range namespacesWithPolicy {
		pol, err := GetNamespacePolicy(ctx, client, namespace)
		if err != nil {
			continue
		}
		policies = append(policies, *pol)
	}

	return policies, nil
}

// GetViolations finds PVCs that violate namespace policies
func GetViolations(ctx context.Context, client kubernetes.Interface) ([]Violation, error) {
	if client == nil {
		return nil, fmt.Errorf("kubernetes client not available")
	}

	var violations []Violation

	// Get all PVs
	pvList, err := client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list PVs: %w", err)
	}

	// Cache namespace policies
	policyCache := make(map[string]*NamespacePolicy)

	for _, pv := range pvList.Items {
		if pv.Spec.ClaimRef == nil {
			continue
		}

		namespace := pv.Spec.ClaimRef.Namespace
		pvcName := pv.Spec.ClaimRef.Name

		// Get or cache policy
		pol, ok := policyCache[namespace]
		if !ok {
			p, err := GetNamespacePolicy(ctx, client, namespace)
			if err != nil {
				continue
			}
			pol = p
			policyCache[namespace] = pol
		}

		// Get PV capacity
		capacity, ok := pv.Spec.Capacity[v1.ResourceStorage]
		if !ok {
			continue
		}

		capacityBytes := capacity.Value()

		// Check if exceeds max
		if pol.MaxQuota > 0 && capacityBytes > pol.MaxQuota {
			violations = append(violations, Violation{
				Namespace:      namespace,
				PVCName:        pvcName,
				PVName:         pv.Name,
				RequestedBytes: capacityBytes,
				RequestedStr:   util.FormatBytes(capacityBytes),
				MaxQuotaBytes:  pol.MaxQuota,
				MaxQuotaStr:    pol.MaxStr,
				ViolationType:  "exceeds_max",
			})
		}

		// Check if below min
		if pol.MinQuota > 0 && capacityBytes < pol.MinQuota {
			violations = append(violations, Violation{
				Namespace:      namespace,
				PVCName:        pvcName,
				PVName:         pv.Name,
				RequestedBytes: capacityBytes,
				RequestedStr:   util.FormatBytes(capacityBytes),
				MinQuotaBytes:  pol.MinQuota,
				MinQuotaStr:    pol.MinStr,
				ViolationType:  "below_min",
			})
		}
	}

	return violations, nil
}
