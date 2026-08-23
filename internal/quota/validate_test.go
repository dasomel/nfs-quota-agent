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

package quota

import (
	"strings"
	"testing"
	"unicode"
)

func TestValidateQuotaArg(t *testing.T) {
	tests := []struct {
		name    string
		kind    string
		val     string
		wantErr bool
		errSub  string
	}{
		{
			name:    "valid absolute path",
			kind:    "path",
			val:     "/data/project1",
			wantErr: false,
		},
		{
			name:    "path with space",
			kind:    "path",
			val:     "/data/project 1",
			wantErr: true,
			errSub:  "contains whitespace",
		},
		{
			name:    "name with double quote",
			kind:    "projectName",
			val:     "proj\"ect1",
			wantErr: true,
			errSub:  "contains quotes",
		},
		{
			name:    "name with single quote",
			kind:    "projectName",
			val:     "proj'ect1",
			wantErr: true,
			errSub:  "contains quotes",
		},
		{
			name:    "name with newline/control char",
			kind:    "projectName",
			val:     "project\n1",
			wantErr: true,
			errSub:  "contains whitespace", // newline is classified as Space by unicode.IsSpace
		},
		{
			name:    "name with control char",
			kind:    "projectName",
			val:     "project\x001",
			wantErr: true,
			errSub:  "contains control characters",
		},
		{
			name:    "empty",
			kind:    "path",
			val:     "",
			wantErr: true,
			errSub:  "cannot be empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateQuotaArg(tt.kind, tt.val)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateQuotaArg() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.errSub)
				}
				if !strings.Contains(err.Error(), tt.kind) {
					t.Errorf("error %q should contain kind %q", err.Error(), tt.kind)
				}
				if !strings.Contains(err.Error(), tt.errSub) {
					t.Errorf("error %q should contain substring %q", err.Error(), tt.errSub)
				}
			}
		})
	}
}

// validateProjectName itself is covered by TestValidateProjectName in
// projectname_test.go (colon/newline rejection, ordinary names, empty/quote
// delegation to validateQuotaArg); AddProject-level and ensureQuota-level
// reachability are covered by TestAddProjectRejectsDelimiterInName and
// TestEnsureQuota_ProjectNameWithColonFromAnnotationRejected respectively.

// FuzzValidateQuotaArg targets the one function every operator-controlled
// string (project name, path) must pass before it can reach argv -- see
// CLAUDE.md's "High-risk paths". The table above only exercises the cases
// someone thought to write down; this explores the space around them
// (multi-byte whitespace/control runes, mixed valid/invalid runs, near-miss
// shell metacharacters that aren't quotes or whitespace) for a panic or an
// input that should be rejected but isn't.
func FuzzValidateQuotaArg(f *testing.F) {
	for _, seed := range []string{
		"/data/project1",
		"/data/project 1",
		"proj\"ect1",
		"proj'ect1",
		"project\n1",
		"project\x001",
		"",
		"project;rm -rf /",
		"project`whoami`",
		"project$(whoami)",
		"project|cat",
		"project&&echo",
		"project\t1",
		"프로젝트1",
		"project​1", // zero-width space: not unicode.IsSpace or IsControl
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, value string) {
		err := validateQuotaArg("fuzz", value)
		if err == nil {
			// Accepted: every rune must be non-space, non-quote, non-control,
			// and the value must be non-empty -- the exact contract
			// validateQuotaArg documents.
			if value == "" {
				t.Fatalf("accepted an empty value")
			}
			for _, r := range value {
				if unicode.IsSpace(r) || unicode.IsControl(r) || r == '\'' || r == '"' {
					t.Fatalf("accepted %q containing rejected rune %q", value, r)
				}
			}
		}
	})
}
