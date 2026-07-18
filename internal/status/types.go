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

// Package status checks and reports disk usage and directory project quota status
// on local paths and NFS mounts. It aggregates system information and exposes
// statistics for metrics collection and reporting.
package status

// DiskUsage represents disk usage information
type DiskUsage struct {
	Total     uint64
	Used      uint64
	Available uint64
	UsedPct   float64
}

// DirUsage represents directory usage information
type DirUsage struct {
	Path      string
	Used      uint64
	Quota     uint64 // 0 if no quota
	UsedPct   float64
	QuotaPct  float64 // percentage of quota used
	ProjectID uint32
}
