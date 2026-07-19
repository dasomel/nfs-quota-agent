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

package status

import (
	"strings"
	"testing"
)

func TestMakeProgressBar(t *testing.T) {
	cases := []struct {
		name     string
		pct      float64
		width    int
		wantFull int
		wantBang bool
	}{
		{"zero", 0, 10, 0, false},
		{"half", 50, 10, 5, false},
		{"full", 100, 10, 10, true},
		{"over 100 is clamped", 150, 10, 10, true},
		{"warning threshold", 90, 10, 9, true},
		{"just under warning threshold", 89, 10, 8, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bar := MakeProgressBar(tc.pct, tc.width)
			if filled := strings.Count(bar, "█"); filled != tc.wantFull {
				t.Errorf("filled = %d, want %d (bar=%q)", filled, tc.wantFull, bar)
			}
			if got := strings.HasSuffix(bar, "]!"); got != tc.wantBang {
				t.Errorf("bang suffix = %v, want %v (bar=%q)", got, tc.wantBang, bar)
			}
		})
	}
}

// ShowStatus and ShowTop are not covered here: both call quota.DetectFSType,
// which shells out to `df -T <path>`. On BSD/macOS df, "-T" takes a
// filesystem-type argument rather than displaying a Type column (unlike GNU
// df), so passing a real directory path always fails with "unexpected df
// output" in this dev environment. That command construction lives in
// internal/quota (owned by another lane in this refactor), so exercising the
// two functions end-to-end is left to Linux CI / integration testing rather
// than reimplemented here.
