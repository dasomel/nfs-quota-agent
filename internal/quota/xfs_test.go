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
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckXFSQuotaAvailable(t *testing.T) {
	t.Run("xfs_quota missing", func(t *testing.T) {
		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			return nil, errors.New("not found")
		}}
		withFakeRunner(t, r)

		if err := CheckXFSQuotaAvailable("/data"); err == nil {
			t.Fatal("expected error when xfs_quota -V fails")
		}
	})

	t.Run("state command fails", func(t *testing.T) {
		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			if len(args) > 0 && args[0] == "-V" {
				return []byte("xfs_quota version 1.0"), nil
			}
			return []byte("some error output"), errors.New("state failed")
		}}
		withFakeRunner(t, r)

		err := CheckXFSQuotaAvailable("/data")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "failed to check quota state") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("project quota enabled", func(t *testing.T) {
		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			if len(args) > 0 && args[0] == "-V" {
				return []byte("xfs_quota version 1.0"), nil
			}
			return []byte("Project quota state on /data (/dev/sda1)\n  Accounting: ON\n"), nil
		}}
		withFakeRunner(t, r)

		if err := CheckXFSQuotaAvailable("/data"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("project quota state not mentioned still succeeds with warning", func(t *testing.T) {
		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			if len(args) > 0 && args[0] == "-V" {
				return []byte("xfs_quota version 1.0"), nil
			}
			return []byte("unexpected output"), nil
		}}
		withFakeRunner(t, r)

		if err := CheckXFSQuotaAvailable("/data"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestApplyXFSQuota(t *testing.T) {
	t.Run("success builds expected commands", func(t *testing.T) {
		dir := t.TempDir()
		projectsFile := filepath.Join(dir, "projects")
		projidFile := filepath.Join(dir, "projid")

		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			return []byte("ok"), nil
		}}
		withFakeRunner(t, r)

		err := ApplyXFSQuota("/data", "/data/proj1", "proj1", 1001, 2*1024*1024, projectsFile, projidFile)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(r.calls) != 2 {
			t.Fatalf("expected 2 xfs_quota calls, got %d: %+v", len(r.calls), r.calls)
		}

		projectCall := r.calls[0]
		if projectCall.name != "xfs_quota" {
			t.Errorf("expected xfs_quota, got %s", projectCall.name)
		}
		wantProjectArg := fmt.Sprintf("project -s -p %s %d", "/data/proj1", 1001)
		if !containsArg(projectCall.args, wantProjectArg) {
			t.Errorf("expected args to contain %q, got %+v", wantProjectArg, projectCall.args)
		}

		limitCall := r.calls[1]
		wantLimitArg := fmt.Sprintf("limit -p bhard=%dk %d", 2*1024, 1001)
		if !containsArg(limitCall.args, wantLimitArg) {
			t.Errorf("expected args to contain %q, got %+v", wantLimitArg, limitCall.args)
		}

		// AddProject side effects verified via project.go tests; sanity check files were written.
		if !fileContains(t, projectsFile, "1001:/data/proj1") {
			t.Errorf("projects file missing expected entry")
		}
		if !fileContains(t, projidFile, "proj1:1001") {
			t.Errorf("projid file missing expected entry")
		}
	})

	t.Run("zero byte size rounds up to 1 KB", func(t *testing.T) {
		dir := t.TempDir()
		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			return []byte("ok"), nil
		}}
		withFakeRunner(t, r)

		err := ApplyXFSQuota("/data", "/data/proj2", "proj2", 1002, 0, filepath.Join(dir, "projects"), filepath.Join(dir, "projid"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		limitCall := r.calls[1]
		wantLimitArg := "limit -p bhard=1k 1002"
		if !containsArg(limitCall.args, wantLimitArg) {
			t.Errorf("expected args to contain %q, got %+v", wantLimitArg, limitCall.args)
		}
	})

	t.Run("project init command failure propagates", func(t *testing.T) {
		dir := t.TempDir()
		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			return []byte("init failed output"), errors.New("boom")
		}}
		withFakeRunner(t, r)

		err := ApplyXFSQuota("/data", "/data/proj3", "proj3", 1003, 1024, filepath.Join(dir, "projects"), filepath.Join(dir, "projid"))
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "failed to initialize project") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("quota limit command failure propagates", func(t *testing.T) {
		dir := t.TempDir()
		call := 0
		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			call++
			if call == 1 {
				return []byte("ok"), nil
			}
			return []byte("limit failed output"), errors.New("boom")
		}}
		withFakeRunner(t, r)

		err := ApplyXFSQuota("/data", "/data/proj4", "proj4", 1004, 1024, filepath.Join(dir, "projects"), filepath.Join(dir, "projid"))
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "failed to set quota limit") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("AddProject failure prevents any exec call", func(t *testing.T) {
		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			return []byte("ok"), nil
		}}
		withFakeRunner(t, r)

		// A directory path for projectsFile is not writable as a file, forcing AddProject to fail.
		badPath := t.TempDir()
		err := ApplyXFSQuota("/data", "/data/proj5", "proj5", 1005, 1024, badPath, filepath.Join(badPath, "projid"))
		if err == nil {
			t.Fatal("expected error from AddProject")
		}
		if !strings.Contains(err.Error(), "failed to add project") {
			t.Errorf("unexpected error: %v", err)
		}
		if len(r.calls) != 0 {
			t.Errorf("expected no exec calls when AddProject fails, got %d", len(r.calls))
		}
	})
}
