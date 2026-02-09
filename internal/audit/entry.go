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

package audit

import "time"

// Action represents the type of quota action
type Action string

const (
	ActionCreate  Action = "CREATE"
	ActionUpdate  Action = "UPDATE"
	ActionDelete  Action = "DELETE"
	ActionCleanup Action = "CLEANUP"
)

// Entry represents a single audit log entry
type Entry struct {
	Timestamp   time.Time `json:"timestamp"`
	Action      Action    `json:"action"`
	PVName      string    `json:"pv_name,omitempty"`
	Namespace   string    `json:"namespace,omitempty"`
	PVCName     string    `json:"pvc_name,omitempty"`
	Path        string    `json:"path"`
	ProjectID   uint32    `json:"project_id,omitempty"`
	ProjectName string    `json:"project_name,omitempty"`
	OldQuota    int64     `json:"old_quota_bytes,omitempty"`
	NewQuota    int64     `json:"new_quota_bytes,omitempty"`
	FSType      string    `json:"fs_type,omitempty"`
	Success     bool      `json:"success"`
	Error       string    `json:"error,omitempty"`
	NodeName    string    `json:"node_name,omitempty"`
	AgentID     string    `json:"agent_id,omitempty"`
}
