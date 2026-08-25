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

// FuzzParseBtrfsQgroupShow fuzzes the pure-parsing half of GetBtrfsQuotaReport
// against arbitrary `btrfs qgroup show` stdout -- see #7: the same reasoning
// already applied to validateQuotaArg's fuzz coverage (this repo's other
// hard-to-audit-by-hand parser of externally-influenced input), extended to
// the parser this issue's earlier comment named as still uncovered.
func FuzzParseBtrfsQgroupShow(f *testing.F) {
	seeds := []string{
		"",
		"qgroupid rfer excl max_rfer max_excl path\n-------- ---- ---- -------- -------- ----\n0/5 16384 16384 none none <toplevel>",
		"0/256 1048576 1048576 10737418240 none subvol1",
		"0/257 2097152 2097152 none none subvol2",
		"0/258 not-a-number not-a-number none none subvol3",
		"0/259 -1 -1 -1 -1 " + strings.Repeat("../", 200) + "escape",
		"0/260 18446744073709551615 18446744073709551615 18446744073709551615 18446744073709551615 subvol4",
		"garbage\nmore garbage\n",
		"0/261 1 1 1 1 /already/absolute/path",
		"0/262 1 1 1 1 none",
		"0/262 1 1 1 1",
		"\x00\x01\x02 1 1 1 1 subvol\x00null",
		"0/263 1 1 1 1 프로젝트/서브볼륨",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, output string) {
		// Contract: never panic (no index-out-of-range, no unchecked
		// strconv result use) on any input, and every path key produced
		// must actually be rooted under basePath or be the absolute path
		// value taken verbatim from the line -- never empty and never
		// literally "none" (those are supposed to be filtered).
		const basePath = "/data"
		quotaMap, usageMap := parseBtrfsQgroupShow(output, basePath)

		for path := range usageMap {
			if path == "" || path == "none" {
				t.Fatalf("usageMap contains a filtered-out path value %q for input %q", path, output)
			}
		}
		for path, limit := range quotaMap {
			if path == "" || path == "none" {
				t.Fatalf("quotaMap contains a filtered-out path value %q for input %q", path, output)
			}
			if limit == 0 {
				t.Fatalf("quotaMap entry %q has zero limit, should have been omitted for input %q", path, output)
			}
			if _, ok := usageMap[path]; !ok {
				t.Fatalf("quotaMap has path %q with no corresponding usageMap entry for input %q", path, output)
			}
		}
	})
}
