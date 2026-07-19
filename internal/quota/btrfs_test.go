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
	"strings"
	"testing"
)

func TestCheckBtrfsQuotaAvailable(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			if name == "btrfs" && len(args) > 0 && args[0] == "--version" {
				return []byte("btrfs-progs v6.1"), nil
			}
			if name == "btrfs" && len(args) > 0 && args[0] == "qgroup" && args[1] == "show" {
				return []byte("qgroupid rfer excl max_rfer max_excl path\n0/5 16384 16384 none none <toplevel>"), nil
			}
			return nil, errors.New("unexpected command")
		}}
		withFakeRunner(t, r)

		if err := CheckBtrfsQuotaAvailable("/data"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("missing binary", func(t *testing.T) {
		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			if name == "btrfs" && len(args) > 0 && args[0] == "--version" {
				return nil, errors.New("executable file not found")
			}
			return nil, errors.New("unexpected command")
		}}
		withFakeRunner(t, r)

		err := CheckBtrfsQuotaAvailable("/data")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "btrfs command not found") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("quotas disabled", func(t *testing.T) {
		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			if name == "btrfs" && len(args) > 0 && args[0] == "--version" {
				return []byte("btrfs-progs v6.1"), nil
			}
			if name == "btrfs" && len(args) > 0 && args[0] == "qgroup" && args[1] == "show" {
				return []byte("ERROR: can't list qgroups: quotas not enabled"), errors.New("exit status 1")
			}
			return nil, errors.New("unexpected command")
		}}
		withFakeRunner(t, r)

		err := CheckBtrfsQuotaAvailable("/data")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "btrfs quotas not enabled; please run 'btrfs quota enable") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestApplyBtrfsQuota(t *testing.T) {
	t.Run("success builds expected commands", func(t *testing.T) {
		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			if name == "btrfs" && args[0] == "subvolume" && args[1] == "show" {
				return []byte("Name: my-subvolume\nUUID: 1234"), nil
			}
			if name == "btrfs" && args[0] == "qgroup" && args[1] == "limit" {
				return []byte(""), nil
			}
			return nil, errors.New("unexpected command")
		}}
		withFakeRunner(t, r)

		err := ApplyBtrfsQuota("/data/subvol1", 10737418240)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(r.calls) != 2 {
			t.Fatalf("expected 2 btrfs calls, got %d", len(r.calls))
		}

		showCall := r.calls[0]
		if showCall.name != "btrfs" || showCall.args[0] != "subvolume" || showCall.args[1] != "show" || showCall.args[2] != "/data/subvol1" {
			t.Errorf("unexpected show call: %+v", showCall)
		}

		limitCall := r.calls[1]
		if limitCall.name != "btrfs" || limitCall.args[0] != "qgroup" || limitCall.args[1] != "limit" || limitCall.args[2] != "10737418240" || limitCall.args[3] != "/data/subvol1" {
			t.Errorf("unexpected limit call: %+v", limitCall)
		}
	})

	t.Run("not a subvolume error", func(t *testing.T) {
		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			if name == "btrfs" && args[0] == "subvolume" && args[1] == "show" {
				return []byte("ERROR: Not a Btrfs subvolume: /data/dir1"), errors.New("exit status 1")
			}
			return nil, errors.New("unexpected command")
		}}
		withFakeRunner(t, r)

		err := ApplyBtrfsQuota("/data/dir1", 10737418240)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "is not a btrfs subvolume") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("limit failure", func(t *testing.T) {
		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			if name == "btrfs" && args[0] == "subvolume" && args[1] == "show" {
				return []byte("Name: my-subvolume"), nil
			}
			if name == "btrfs" && args[0] == "qgroup" && args[1] == "limit" {
				return []byte("ERROR: limit failed"), errors.New("exit status 1")
			}
			return nil, errors.New("unexpected command")
		}}
		withFakeRunner(t, r)

		err := ApplyBtrfsQuota("/data/subvol1", 10737418240)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "failed to set btrfs quota limit") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestGetBtrfsQuotaReport(t *testing.T) {
	t.Run("well-formed output", func(t *testing.T) {
		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			output := `qgroupid rfer excl max_rfer max_excl path
-------- ---- ---- -------- -------- ----
0/5 16384 16384 none none <toplevel>
0/256 1048576 1048576 10737418240 none subvol1
0/257 2097152 2097152 none none subvol2
`
			return []byte(output), nil
		}}
		withFakeRunner(t, r)

		quotaMap, usageMap, err := GetBtrfsQuotaReport("/data")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(usageMap) != 2 {
			t.Errorf("expected 2 usage entries, got %d: %+v", len(usageMap), usageMap)
		}
		if usageMap["/data/subvol1"] != 1048576 {
			t.Errorf("expected subvol1 usage 1048576, got %d", usageMap["/data/subvol1"])
		}
		if usageMap["/data/subvol2"] != 2097152 {
			t.Errorf("expected subvol2 usage 2097152, got %d", usageMap["/data/subvol2"])
		}

		if len(quotaMap) != 1 {
			t.Errorf("expected 1 quota entry, got %d: %+v", len(quotaMap), quotaMap)
		}
		if quotaMap["/data/subvol1"] != 10737418240 {
			t.Errorf("expected subvol1 quota 10737418240, got %d", quotaMap["/data/subvol1"])
		}
	})

	t.Run("empty output", func(t *testing.T) {
		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			return []byte(""), nil
		}}
		withFakeRunner(t, r)

		quotaMap, usageMap, err := GetBtrfsQuotaReport("/data")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(quotaMap) != 0 || len(usageMap) != 0 {
			t.Errorf("expected empty maps, got quotas: %+v, usages: %+v", quotaMap, usageMap)
		}
	})

	t.Run("garbage output", func(t *testing.T) {
		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			return []byte("some random garbage\noutput here"), nil
		}}
		withFakeRunner(t, r)

		quotaMap, usageMap, err := GetBtrfsQuotaReport("/data")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(quotaMap) != 0 || len(usageMap) != 0 {
			t.Errorf("expected empty maps, got quotas: %+v, usages: %+v", quotaMap, usageMap)
		}
	})
}
