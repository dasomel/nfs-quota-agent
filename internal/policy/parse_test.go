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
	"testing"
)

func TestParseQuotaSize(t *testing.T) {
	tests := []struct {
		input    string
		expected int64
		wantErr  bool
	}{
		{"1Ki", 1024, false},
		{"1Mi", 1024 * 1024, false},
		{"1Gi", 1024 * 1024 * 1024, false},
		{"1Ti", 1024 * 1024 * 1024 * 1024, false},
		{"10Gi", 10 * 1024 * 1024 * 1024, false},
		{"100Mi", 100 * 1024 * 1024, false},
		{"1K", 1000, false},
		{"1M", 1000 * 1000, false},
		{"1G", 1000 * 1000 * 1000, false},
		{"1T", 1000 * 1000 * 1000 * 1000, false},
		{"1024", 1024, false},
		{"0", 0, false},
		{"", 0, true},
		{"invalid", 0, true},
		{"5.5Gi", 5905580032, false}, // 5.5 * 1024^3
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := ParseQuotaSize(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ParseQuotaSize(%q) expected error, got nil", tt.input)
				}
				return
			}
			if err != nil {
				t.Errorf("ParseQuotaSize(%q) unexpected error: %v", tt.input, err)
				return
			}
			if result != tt.expected {
				t.Errorf("ParseQuotaSize(%q) = %d, want %d", tt.input, result, tt.expected)
			}
		})
	}
}

func TestParseQuotaSizeCaseInsensitive(t *testing.T) {
	tests := []struct {
		input    string
		expected int64
	}{
		{"1gi", 1024 * 1024 * 1024},
		{"1GI", 1024 * 1024 * 1024},
		{"1Gi", 1024 * 1024 * 1024},
		{"10mi", 10 * 1024 * 1024},
		{"10MI", 10 * 1024 * 1024},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := ParseQuotaSize(tt.input)
			if err != nil {
				t.Errorf("ParseQuotaSize(%q) unexpected error: %v", tt.input, err)
				return
			}
			if result != tt.expected {
				t.Errorf("ParseQuotaSize(%q) = %d, want %d", tt.input, result, tt.expected)
			}
		})
	}
}
