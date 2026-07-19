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
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckExt4QuotaAvailable(t *testing.T) {
	t.Run("setquota missing", func(t *testing.T) {
		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			return nil, errors.New("not found")
		}}
		withFakeRunner(t, r)

		err := CheckExt4QuotaAvailable("/data")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "setquota command not found") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("prjquota enabled", func(t *testing.T) {
		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			if name == "setquota" {
				return []byte("setquota version"), nil
			}
			return []byte("rw,relatime,prjquota"), nil
		}}
		withFakeRunner(t, r)

		if err := CheckExt4QuotaAvailable("/data"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("prjquota missing still succeeds with warning", func(t *testing.T) {
		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			if name == "setquota" {
				return []byte("setquota version"), nil
			}
			return []byte("rw,relatime"), nil
		}}
		withFakeRunner(t, r)

		if err := CheckExt4QuotaAvailable("/data"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("findmnt failure is only a warning, still succeeds", func(t *testing.T) {
		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			if name == "setquota" {
				return []byte("setquota version"), nil
			}
			return nil, errors.New("findmnt failed")
		}}
		withFakeRunner(t, r)

		if err := CheckExt4QuotaAvailable("/data"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestApplyExt4Quota(t *testing.T) {
	t.Run("success via chattr -R", func(t *testing.T) {
		dir := t.TempDir()
		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			return []byte("ok"), nil
		}}
		withFakeRunner(t, r)

		err := ApplyExt4Quota("/data", "/data/proj1", "proj1", 2001, 4*1024*1024,
			filepath.Join(dir, "projects"), filepath.Join(dir, "projid"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(r.calls) != 2 {
			t.Fatalf("expected chattr + setquota calls, got %d: %+v", len(r.calls), r.calls)
		}
		if r.calls[0].name != "chattr" {
			t.Errorf("expected chattr first, got %s", r.calls[0].name)
		}
		if r.calls[1].name != "setquota" {
			t.Errorf("expected setquota second, got %s", r.calls[1].name)
		}
		wantSizeKB := "4096"
		if !containsArg(r.calls[1].args, wantSizeKB) {
			t.Errorf("expected setquota args to contain hard limit %q, got %+v", wantSizeKB, r.calls[1].args)
		}
	})

	t.Run("chattr -R fails, falls back to per-directory walk", func(t *testing.T) {
		dir := t.TempDir()
		projPath := filepath.Join(dir, "projdir")
		if err := makeDirWithSubdir(projPath); err != nil {
			t.Fatalf("setup failed: %v", err)
		}

		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			if name == "chattr" && containsArg(args, "-R") {
				return []byte("chattr -R failed"), errors.New("boom")
			}
			return []byte("ok"), nil
		}}
		withFakeRunner(t, r)

		err := ApplyExt4Quota("/data", projPath, "proj2", 2002, 1024,
			filepath.Join(dir, "projects"), filepath.Join(dir, "projid"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Expect: failed chattr -R, then a chattr per directory entry (walk), then setquota.
		var chattrCalls, setquotaCalls int
		for _, c := range r.calls {
			switch c.name {
			case "chattr":
				chattrCalls++
			case "setquota":
				setquotaCalls++
			}
		}
		if chattrCalls < 2 {
			t.Errorf("expected at least 2 chattr calls (failed -R plus walk fallback), got %d", chattrCalls)
		}
		if setquotaCalls != 1 {
			t.Errorf("expected 1 setquota call, got %d", setquotaCalls)
		}
	})

	t.Run("setquota failure propagates", func(t *testing.T) {
		dir := t.TempDir()
		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			if name == "setquota" {
				return []byte("setquota failed output"), errors.New("boom")
			}
			return []byte("ok"), nil
		}}
		withFakeRunner(t, r)

		err := ApplyExt4Quota("/data", "/data/proj3", "proj3", 2003, 1024,
			filepath.Join(dir, "projects"), filepath.Join(dir, "projid"))
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

		badPath := t.TempDir()
		err := ApplyExt4Quota("/data", "/data/proj4", "proj4", 2004, 1024, badPath, filepath.Join(badPath, "projid"))
		if err == nil {
			t.Fatal("expected error from AddProject")
		}
		if len(r.calls) != 0 {
			t.Errorf("expected no exec calls when AddProject fails, got %d", len(r.calls))
		}
	})

	t.Run("invalid path returns error and zero exec calls", func(t *testing.T) {
		r := &fakeRunner{}
		withFakeRunner(t, r)

		dir := t.TempDir()
		err := ApplyExt4Quota("/data", "/data/proj ect5", "proj5", 2005, 1024, filepath.Join(dir, "projects"), filepath.Join(dir, "projid"))
		if err == nil {
			t.Fatal("expected error from path validation")
		}
		if !strings.Contains(err.Error(), "invalid path") {
			t.Errorf("unexpected error: %v", err)
		}
		if len(r.calls) != 0 {
			t.Errorf("expected zero exec calls, got %d", len(r.calls))
		}
	})

	t.Run("invalid name returns error and zero exec calls", func(t *testing.T) {
		r := &fakeRunner{}
		withFakeRunner(t, r)

		dir := t.TempDir()
		err := ApplyExt4Quota("/data", "/data/proj5", "proj\"5", 2005, 1024, filepath.Join(dir, "projects"), filepath.Join(dir, "projid"))
		if err == nil {
			t.Fatal("expected error from name validation")
		}
		if !strings.Contains(err.Error(), "invalid projectName") {
			t.Errorf("unexpected error: %v", err)
		}
		if len(r.calls) != 0 {
			t.Errorf("expected zero exec calls, got %d", len(r.calls))
		}
	})
}
