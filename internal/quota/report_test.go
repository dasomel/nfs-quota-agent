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
	"os"
	"path/filepath"
	"testing"
)

func TestGetXFSQuotaReport_InvalidArgument(t *testing.T) {
	r := &fakeRunner{}
	withFakeRunner(t, r)

	_, _, err := GetXFSQuotaReport("/data/proj ect", "/etc/projects", "/etc/projid")
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

	_, _, err := GetExt4QuotaReport("/data/proj ect", "/etc/projects", "/etc/projid")
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

	_, _, err := GetXFSQuotaReport("/data", "/etc/projects", "/etc/projid")
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

	// Nonexistent projectsFile/projidFile: os.ReadFile fails, both maps
	// stay pre-populated-empty rather than panicking.
	quotaMap, usageMap, err := GetXFSQuotaReport("/data", "/does/not/exist/projects", "/does/not/exist/projid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(quotaMap) != 0 || len(usageMap) != 0 {
		t.Errorf("expected empty maps when no project mapping found, got quota=%v usage=%v", quotaMap, usageMap)
	}
}

func TestStrictQuotaReports_MappingFileReadError(t *testing.T) {
	r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		return []byte("Project ID   Used   Soft   Hard   Warn/Grace\n"), nil
	}}
	withFakeRunner(t, r)

	dir := t.TempDir()
	projectsFile := filepath.Join(dir, "projects")
	projidFile := filepath.Join(dir, "projid")
	if err := os.WriteFile(projectsFile, nil, 0o644); err != nil {
		t.Fatalf("write projects: %v", err)
	}
	if err := os.WriteFile(projidFile, nil, 0o644); err != nil {
		t.Fatalf("write projid: %v", err)
	}

	if _, _, err := GetXFSQuotaReportStrict("/data", projectsFile, dir); err == nil {
		t.Fatal("expected strict XFS report to reject unreadable projid mapping")
	}
	if _, _, err := GetExt4QuotaReportStrict("/data", dir, projidFile); err == nil {
		t.Fatal("expected strict ext4 report to reject unreadable projects mapping")
	}
	if _, _, err := GetExt4QuotaReportStrict("/data", projectsFile, dir); err == nil {
		t.Fatal("expected strict ext4 report to reject unreadable projid mapping")
	}

	// The established best-effort API intentionally remains tolerant for
	// reporting callers that can still provide a useful directory walk.
	if _, _, err := GetXFSQuotaReport("/data", projectsFile, dir); err != nil {
		t.Fatalf("non-strict XFS report unexpectedly failed: %v", err)
	}
	if _, _, err := GetExt4QuotaReport("/data", dir, projidFile); err != nil {
		t.Fatalf("non-strict ext4 report unexpectedly failed: %v", err)
	}
	if _, _, err := GetExt4QuotaReport("/data", projectsFile, dir); err != nil {
		t.Fatalf("non-strict ext4 report unexpectedly failed: %v", err)
	}
}

func TestGetXFSQuotaReport_ResolvesPathFromConfiguredFiles(t *testing.T) {
	dir := t.TempDir()
	projectsFile := filepath.Join(dir, "projects")
	projidFile := filepath.Join(dir, "projid")
	if err := os.WriteFile(projectsFile, []byte("100:/data/pvc-1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile projects: %v", err)
	}
	if err := os.WriteFile(projidFile, []byte("pvc-1:100\n"), 0o644); err != nil {
		t.Fatalf("WriteFile projid: %v", err)
	}

	r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		return []byte("Project ID   Used   Soft   Hard   Warn/Grace\n#pvc-1     100    0      2097152    00 [------]\n"), nil
	}}
	withFakeRunner(t, r)

	quotaMap, usageMap, err := GetXFSQuotaReport("/data", projectsFile, projidFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usageMap["/data/pvc-1"] != 100*1024 {
		t.Errorf("usageMap[/data/pvc-1] = %d, want %d", usageMap["/data/pvc-1"], 100*1024)
	}
	if quotaMap["/data/pvc-1"] != 2097152*1024 {
		t.Errorf("quotaMap[/data/pvc-1] = %d, want %d", quotaMap["/data/pvc-1"], 2097152*1024)
	}

	// A second projectsFile/projidFile pair pointing at different content
	// must resolve independently -- this is the behavior the previous
	// hardcoded /etc/projects and /etc/projid could never demonstrate.
	dir2 := t.TempDir()
	projectsFile2 := filepath.Join(dir2, "projects")
	projidFile2 := filepath.Join(dir2, "projid")
	if err := os.WriteFile(projectsFile2, []byte("100:/other/pvc-1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile projects2: %v", err)
	}
	if err := os.WriteFile(projidFile2, []byte("pvc-1:100\n"), 0o644); err != nil {
		t.Fatalf("WriteFile projid2: %v", err)
	}

	quotaMap2, _, err := GetXFSQuotaReport("/data", projectsFile2, projidFile2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := quotaMap2["/data/pvc-1"]; ok {
		t.Errorf("quotaMap2 resolved /data/pvc-1 using the first pair's file content — projectsFile/projidFile parameters are not actually isolating reads")
	}
	if quotaMap2["/other/pvc-1"] != 2097152*1024 {
		t.Errorf("quotaMap2[/other/pvc-1] = %d, want %d", quotaMap2["/other/pvc-1"], 2097152*1024)
	}
}

func TestGetExt4QuotaReport_CommandError(t *testing.T) {
	r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		return []byte("some output"), errors.New("boom")
	}}
	withFakeRunner(t, r)

	_, _, err := GetExt4QuotaReport("/data", "/etc/projects", "/etc/projid")
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

	quotaMap, usageMap, err := GetExt4QuotaReport("/data", "/does/not/exist/projects", "/does/not/exist/projid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(quotaMap) != 0 || len(usageMap) != 0 {
		t.Errorf("expected empty maps when no project mapping found, got quota=%v usage=%v", quotaMap, usageMap)
	}
}

func TestGetExt4QuotaReport_ResolvesPathFromConfiguredFile(t *testing.T) {
	dir := t.TempDir()
	projectsFile := filepath.Join(dir, "projects")
	if err := os.WriteFile(projectsFile, []byte("100:/data/pvc-1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile projects: %v", err)
	}

	r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		return []byte("Project        used    soft    hard  grace   used  soft  hard  grace\n#100      --   100      0    2097152                5     0     0\n"), nil
	}}
	withFakeRunner(t, r)

	// No projid file, so this exercises the "#<id>" resolution path only.
	quotaMap, usageMap, err := GetExt4QuotaReport("/data", projectsFile, "/does/not/exist/projid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usageMap["/data/pvc-1"] != 100*1024 {
		t.Errorf("usageMap[/data/pvc-1] = %d, want %d", usageMap["/data/pvc-1"], 100*1024)
	}
	if quotaMap["/data/pvc-1"] != 2097152*1024 {
		t.Errorf("quotaMap[/data/pvc-1] = %d, want %d", quotaMap["/data/pvc-1"], 2097152*1024)
	}
}

// TestGetExt4QuotaReport_ResolvesNameKeyedRow reproduces the real-kernel
// finding behind PR #155's ext4 Air-Gap E2E failure: `repquota -P` does
// not print "#<id>" for a project that has an /etc/projid name -- it
// prints the name -- and AddProject always registers one. This fixture is
// not hand-written: it is the literal stdout of `repquota -P` run against
// a real `mkfs.ext4 -O project,quota` / `mount -o prjquota` filesystem on
// a real kernel (colima VM, aarch64 Ubuntu 24.04), after applying a
// project quota named "pv-e2e" the same way ApplyExt4Quota does (chattr -R
// +P -p <id>, then setquota -P <id> 0 <kb> 0 0). Before PR #155's fix,
// parseExt4RepquotaOutput only recognized "#<id>" rows and silently
// dropped this one, which is exactly what made ensureQuota's read-back
// verification (agent.go's verifyQuotaOnDisk) fail unconditionally for
// every ext4 PV in CI.
func TestGetExt4QuotaReport_ResolvesNameKeyedRow(t *testing.T) {
	dir := t.TempDir()
	projectsFile := filepath.Join(dir, "projects")
	projidFile := filepath.Join(dir, "projid")
	if err := os.WriteFile(projectsFile, []byte("12345:/mnt/ext4test/pvc-e2e\n"), 0o644); err != nil {
		t.Fatalf("WriteFile projects: %v", err)
	}
	if err := os.WriteFile(projidFile, []byte("pv-e2e:12345\n"), 0o644); err != nil {
		t.Fatalf("WriteFile projid: %v", err)
	}

	const realRepquotaOutput = "*** Report for project quotas on device /dev/loop3\n" +
		"Block grace time: 7days; Inode grace time: 7days\n" +
		"                        Block limits                File limits\n" +
		"Project         used    soft    hard  grace    used  soft  hard  grace\n" +
		"----------------------------------------------------------------------\n" +
		"pv-e2e    --       4       0  102400              1     0     0       \n" +
		"#0        --      20       0       0              2     0     0       \n"

	r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
		return []byte(realRepquotaOutput), nil
	}}
	withFakeRunner(t, r)

	quotaMap, usageMap, err := GetExt4QuotaReport("/mnt/ext4test", projectsFile, projidFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	const path = "/mnt/ext4test/pvc-e2e"
	if usageMap[path] != 4*1024 {
		t.Errorf("usageMap[%s] = %d, want %d", path, usageMap[path], 4*1024)
	}
	if quotaMap[path] != 102400*1024 {
		t.Errorf("quotaMap[%s] = %d, want %d", path, quotaMap[path], 102400*1024)
	}
	// #0 has no projects-file entry and must not resolve to anything.
	if len(quotaMap) != 1 || len(usageMap) != 1 {
		t.Errorf("expected exactly one resolved path, got quota=%v usage=%v", quotaMap, usageMap)
	}
}

func TestExpectedEnforcedBytes(t *testing.T) {
	tests := []struct {
		name      string
		fsType    string
		sizeBytes int64
		want      int64
	}{
		{"xfs floors to whole KB", FSTypeXFS, 1000000000, 999999488}, // 1G decimal SI, not a 1024 multiple
		{"xfs exact KB multiple unchanged", FSTypeXFS, 1073741824, 1073741824},
		{"xfs sub-1KB request floors to 1KB minimum", FSTypeXFS, 500, 1024},
		{"ext4 floors to whole KB", FSTypeExt4, 1000000000, 999999488},
		{"ext4 sub-1KB request floors to 1KB minimum", FSTypeExt4, 1, 1024},
		{"btrfs uses raw bytes, no rounding", FSTypeBtrfs, 1000000000, 1000000000},
		{"unknown fsType uses raw bytes", "zfs", 1000000000, 1000000000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExpectedEnforcedBytes(tc.fsType, tc.sizeBytes); got != tc.want {
				t.Errorf("ExpectedEnforcedBytes(%q, %d) = %d, want %d", tc.fsType, tc.sizeBytes, got, tc.want)
			}
		})
	}
}

// FuzzParseXFSQuotaReportOutput fuzzes the pure-parsing half of
// GetXFSQuotaReport against arbitrary `xfs_quota -x -c "report -p -b"`
// stdout -- see #7: the same treatment PR #65 already gave
// parseBtrfsQgroupShow, extended to the two report parsers that issue's
// earlier comment named as still uncovered. Unlike the btrfs parser, a
// path here never comes from the report line itself -- only from the
// caller-supplied nameToPaths/projectPaths maps -- so there's no
// path-traversal-from-output risk to check; the contract fuzzed here is
// simply "never panic, and never emit a path this input couldn't
// legitimately resolve to."
func FuzzParseXFSQuotaReportOutput(f *testing.F) {
	seeds := []string{
		"",
		"Project ID   Used   Soft   Hard   Warn/Grace\n#pvc-1     100    0      2097152    00 [------]\n",
		"#100     100    0      2097152    00 [------]\n",
		"#pvc-1 not-a-number not-a-number not-a-number\n",
		"#pvc-1 -1 -1 -1\n",
		"#pvc-1 18446744073709551615 18446744073709551615 18446744073709551615\n",
		"garbage\nmore garbage\n",
		"#unknown-project 100 0 2097152\n",
		"#pvc-1 100 0\n",
		"\x00\x01\x02 1 1 1\n",
		"#pvc-1 100 0 2097152 extra fields here\n",
		"#프로젝트 100 0 2097152\n",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	nameToPaths := map[string]string{"pvc-1": "/data/pvc-1"}
	projectPaths := map[string]string{"100": "/data/pvc-1"}
	validPaths := map[string]bool{"/data/pvc-1": true}

	f.Fuzz(func(t *testing.T, output string) {
		quotaMap, usageMap := parseXFSQuotaReportOutput([]byte(output), nameToPaths, projectPaths)
		for path := range usageMap {
			if !validPaths[path] {
				t.Fatalf("usageMap contains an unexpected path %q for input %q", path, output)
			}
		}
		for path := range quotaMap {
			if !validPaths[path] {
				t.Fatalf("quotaMap contains an unexpected path %q for input %q", path, output)
			}
		}
	})
}

// FuzzParseExt4RepquotaOutput is FuzzParseXFSQuotaReportOutput's ext4
// counterpart, fuzzing parseExt4RepquotaOutput against arbitrary
// `repquota -P` stdout.
func FuzzParseExt4RepquotaOutput(f *testing.F) {
	seeds := []string{
		"",
		"Project        used    soft    hard  grace   used  soft  hard  grace\n#100      --   100      0    2097152                5     0     0\n",
		"#100-- 100 0 2097152\n",
		"#100++ 100 0 2097152\n",
		"#not-a-number -- 100 0 2097152\n",
		"#100 -1 -1 -1 -1\n",
		"#100 18446744073709551615 18446744073709551615 18446744073709551615 18446744073709551615\n",
		"garbage\nmore garbage\n",
		"#999 -- 100 0 2097152\n",
		"#100 -- 100 0\n",
		"\x00\x01\x02 1 1 1 1\n",
		"#100 -- 100 0 2097152 extra columns here\n",
		// Real repquota -P names the row instead of "#<id>" whenever the
		// project has an /etc/projid entry -- see
		// TestGetExt4QuotaReport_ResolvesNameKeyedRow.
		"pv-e2e    --       4       0  102400              1     0     0\n",
		"unknown-name -- 100 0 2097152\n",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	projectPaths := map[string]string{"100": "/data/pvc-1"}
	nameToPaths := map[string]string{"pv-e2e": "/data/pvc-1"}
	validPaths := map[string]bool{"/data/pvc-1": true}

	f.Fuzz(func(t *testing.T, output string) {
		quotaMap, usageMap := parseExt4RepquotaOutput([]byte(output), projectPaths, nameToPaths)
		for path := range usageMap {
			if !validPaths[path] {
				t.Fatalf("usageMap contains an unexpected path %q for input %q", path, output)
			}
		}
		for path := range quotaMap {
			if !validPaths[path] {
				t.Fatalf("quotaMap contains an unexpected path %q for input %q", path, output)
			}
		}
	})
}
