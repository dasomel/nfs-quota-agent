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
	"path/filepath"
	"testing"
)

func TestGetDiskUsage(t *testing.T) {
	du, err := GetDiskUsage(t.TempDir())
	if err != nil {
		t.Fatalf("GetDiskUsage: %v", err)
	}
	if du.Total == 0 {
		t.Fatal("expected nonzero Total on a real filesystem")
	}
	if du.Used > du.Total {
		t.Fatalf("Used (%d) > Total (%d)", du.Used, du.Total)
	}
	if du.UsedPct < 0 || du.UsedPct > 100 {
		t.Fatalf("UsedPct = %f, want within [0,100]", du.UsedPct)
	}
}

func TestGetDiskUsage_NonexistentPath(t *testing.T) {
	_, err := GetDiskUsage(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("expected error for nonexistent path")
	}
}
