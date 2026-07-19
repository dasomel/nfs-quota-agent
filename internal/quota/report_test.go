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
	"errors"
	"testing"
)

// Note: GetXFSQuotaReport/GetExt4QuotaReport read the fixed system paths
// /etc/projid and /etc/projects directly, so full path-mapping behavior is
// not hermetically testable here. These tests cover command construction
// and error propagation through the CommandRunner seam (best-effort).

func TestGetXFSQuotaReport_InvalidArgument(t *testing.T) {
	r := &fakeRunner{}
	withFakeRunner(t, r)

	_, _, err := GetXFSQuotaReport("/data/proj ect")
	if err == nil {
		t.Fatal("expected error")
	}
	if len(r.calls) != 0 {
		t.Errorf("expected zero calls, got %d", len(r.calls))
	}
}

func TestGetExt4QuotaReport_InvalidArgument(t *testing.T) {
	r := &fakeRunner{}
	withFakeRunner(t, r)

	_, _, err := GetExt4QuotaReport("/data/proj ect")
	if err == nil {
		t.Fatal("expected error")
	}
	if len(r.calls) != 0 {
		t.Errorf("expected zero calls, got %d", len(r.calls))
	}
}

func TestGetXFSQuotaReport_CommandError(t *testing.T) {
	r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		return []byte("some output"), errors.New("boom")
	}}
	withFakeRunner(t, r)

	_, _, err := GetXFSQuotaReport("/data")
	if err == nil {
		t.Fatal("expected error")
	}
	if len(r.calls) != 1 || r.calls[0].name != "xfs_quota" {
		t.Errorf("expected single xfs_quota call, got %+v", r.calls)
	}
}

func TestGetXFSQuotaReport_NoMatchingProjects(t *testing.T) {
	r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		return []byte("Project ID   Used   Soft   Hard   Warn/Grace\n#proj1     100    0      200    00 [------]\n"), nil
	}}
	withFakeRunner(t, r)

	quotaMap, usageMap, err := GetXFSQuotaReport("/data")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Without /etc/projid or /etc/projects entries for "proj1", no path can
	// be resolved, so both maps should stay empty rather than panicking.
	if len(quotaMap) != 0 || len(usageMap) != 0 {
		t.Errorf("expected empty maps when no project mapping found, got quota=%v usage=%v", quotaMap, usageMap)
	}
}

func TestGetExt4QuotaReport_CommandError(t *testing.T) {
	r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		return []byte("some output"), errors.New("boom")
	}}
	withFakeRunner(t, r)

	_, _, err := GetExt4QuotaReport("/data")
	if err == nil {
		t.Fatal("expected error")
	}
	if len(r.calls) != 1 || r.calls[0].name != "repquota" {
		t.Errorf("expected single repquota call, got %+v", r.calls)
	}
}

func TestGetExt4QuotaReport_NoMatchingProjects(t *testing.T) {
	r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		return []byte("Project        used    soft    hard  grace   used  soft  hard  grace\n#100      --   100      0    200                5     0     0\n"), nil
	}}
	withFakeRunner(t, r)

	quotaMap, usageMap, err := GetExt4QuotaReport("/data")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(quotaMap) != 0 || len(usageMap) != 0 {
		t.Errorf("expected empty maps when no project mapping found, got quota=%v usage=%v", quotaMap, usageMap)
	}
}
