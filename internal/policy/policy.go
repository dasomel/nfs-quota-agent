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

	// Effective values (computed from LimitRange > Annotation > Global)
	DefaultQuota int64  `json:"defaultQuota"`
	MaxQuota     int64  `json:"maxQuota"`
	MinQuota     int64  `json:"minQuota"`
	DefaultStr   string `json:"defaultStr"`
	MaxStr       string `json:"maxStr"`
	MinStr       string `json:"minStr"`

	// Source of effective values
	Source string `json:"source"` // "LimitRange", "Annotation", "Global", "None"
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

// GetNamespacePolicy retrieves quota policy for a namespace
// Priority: LimitRange > Namespace Annotation > Global Default
func GetNamespacePolicy(ctx context.Context, client kubernetes.Interface, namespace string) (*NamespacePolicy, error) {
	if client == nil {
		return nil, fmt.Errorf("kubernetes client not available")
	}

	p := &NamespacePolicy{
		Namespace: namespace,
		Source:    "None",
	}

	// 1. Try to get LimitRange for PVC
	limitRanges, err := client.CoreV1().LimitRanges(namespace).List(ctx, metav1.ListOptions{})
	if err == nil && len(limitRanges.Items) > 0 {
		for _, lr := range limitRanges.Items {
			for _, limit := range lr.Spec.Limits {
				if limit.Type == v1.LimitTypePersistentVolumeClaim {
					p.LimitRangeName = lr.Name
					p.Source = "LimitRange"

					// Max storage
					if max, ok := limit.Max[v1.ResourceStorage]; ok {
						p.LimitRangeMax = max.Value()
						p.LimitRangeMaxStr = max.String()
						p.MaxQuota = max.Value()
						p.MaxStr = max.String()
					}

					// Min storage
					if min, ok := limit.Min[v1.ResourceStorage]; ok {
						p.LimitRangeMin = min.Value()
						p.LimitRangeMinStr = min.String()
						p.MinQuota = min.Value()
						p.MinStr = min.String()
					}

					// Default storage
					if def, ok := limit.Default[v1.ResourceStorage]; ok {
						p.LimitRangeDefault = def.Value()
						p.LimitRangeDefStr = def.String()
						p.DefaultQuota = def.Value()
						p.DefaultStr = def.String()
					}

					// DefaultRequest (used when no request specified)
					if defReq, ok := limit.DefaultRequest[v1.ResourceStorage]; ok {
						if p.DefaultQuota == 0 {
							p.DefaultQuota = defReq.Value()
							p.DefaultStr = defReq.String()
						}
					}

					break // Use first matching LimitRange
				}
			}
			if p.Source == "LimitRange" {
				break
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

// ValidateQuota validates requested quota against namespace policy
func ValidateQuota(ctx context.Context, client kubernetes.Interface, namespace string, requestedBytes int64, enforceMax bool) error {
	p, err := GetNamespacePolicy(ctx, client, namespace)
	if err != nil {
		// If we can't get the policy, don't block
		slog.Debug("Could not get namespace policy", "namespace", namespace, "error", err)
		return nil
	}

	// Check max quota
	if p.MaxQuota > 0 && enforceMax && requestedBytes > p.MaxQuota {
		return fmt.Errorf("requested quota %s exceeds maximum allowed %s for namespace %s (source: %s)",
			util.FormatBytes(requestedBytes),
			p.MaxStr,
			namespace,
			p.Source,
		)
	}

	// Check min quota
	if p.MinQuota > 0 && requestedBytes < p.MinQuota {
		return fmt.Errorf("requested quota %s is below minimum required %s for namespace %s (source: %s)",
			util.FormatBytes(requestedBytes),
			p.MinStr,
			namespace,
			p.Source,
		)
	}

	return nil
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
