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

func TestDetectFSType(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		err        error
		wantFSType string
		wantErr    bool
	}{
		{
			name:       "xfs filesystem",
			output:     "Filesystem     Type  1K-blocks     Used Available Use% Mounted on\n/dev/sda1      xfs   103081248 60000000  43000000  59% /data\n",
			wantFSType: "xfs",
		},
		{
			name:       "ext4 filesystem",
			output:     "Filesystem     Type  1K-blocks     Used Available Use% Mounted on\n/dev/sda1      ext4  103081248 60000000  43000000  59% /data\n",
			wantFSType: "ext4",
		},
		{
			name:       "unknown filesystem type passed through lowercased",
			output:     "Filesystem     Type  1K-blocks     Used Available Use% Mounted on\n/dev/sda1      ZFS   103081248 60000000  43000000  59% /data\n",
			wantFSType: "zfs",
		},
		{
			name:       "long device name wraps to two lines",
			output:     "Filesystem                                       Type 1K-blocks    Used Available Use% Mounted on\n/dev/mapper/very-long-volume-group-name-vol\n                                                 xfs   103081248 60000000  43000000  59% /data\n",
			wantFSType: "xfs",
		},
		{
			name:    "command error propagates",
			err:     errors.New("df: command not found"),
			wantErr: true,
		},
		{
			name:    "unexpected output too short",
			output:  "no data here",
			wantErr: true,
		},
		{
			name:    "single field line",
			output:  "Filesystem\nonlyonefield\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
				return []byte(tt.output), tt.err
			}}
			withFakeRunner(t, r)

			got, err := DetectFSType("/data")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (fsType=%q)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.wantFSType {
				t.Errorf("DetectFSType() = %q, want %q", got, tt.wantFSType)
			}

			if len(r.calls) != 1 {
				t.Fatalf("expected 1 call, got %d", len(r.calls))
			}
			if r.calls[0].name != "df" {
				t.Errorf("expected command df, got %s", r.calls[0].name)
			}
		})
	}
}

func TestDetectFSTypeWithFindmnt(t *testing.T) {
	t.Run("findmnt succeeds", func(t *testing.T) {
		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			if name != "findmnt" {
				t.Fatalf("expected findmnt, got %s", name)
			}
			return []byte("xfs\n"), nil
		}}
		withFakeRunner(t, r)

		got, err := DetectFSTypeWithFindmnt("/data")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "xfs" {
			t.Errorf("got %q, want xfs", got)
		}
	})

	t.Run("findmnt fails falls back to df -T", func(t *testing.T) {
		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			if name == "findmnt" {
				return nil, errors.New("findmnt: not found")
			}
			// fallback df -T
			return []byte("Filesystem Type 1K-blocks Used Available Use% Mounted\n/dev/sda1 ext4 1 1 1 1% /data\n"), nil
		}}
		withFakeRunner(t, r)

		got, err := DetectFSTypeWithFindmnt("/data")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "ext4" {
			t.Errorf("got %q, want ext4", got)
		}
		if len(r.calls) != 2 {
			t.Fatalf("expected 2 calls (findmnt then df fallback), got %d", len(r.calls))
		}
	})

	t.Run("findmnt returns empty output falls back to df -T", func(t *testing.T) {
		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			if name == "findmnt" {
				return []byte("\n"), nil
			}
			return []byte("Filesystem Type 1K-blocks Used Available Use% Mounted\n/dev/sda1 xfs 1 1 1 1% /data\n"), nil
		}}
		withFakeRunner(t, r)

		got, err := DetectFSTypeWithFindmnt("/data")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "xfs" {
			t.Errorf("got %q, want xfs", got)
		}
	})

	t.Run("findmnt fails and df fallback also fails", func(t *testing.T) {
		r := &fakeRunner{fn: func(name string, args ...string) ([]byte, error) {
			return nil, errors.New("boom")
		}}
		withFakeRunner(t, r)

		if _, err := DetectFSTypeWithFindmnt("/data"); err == nil {
			t.Fatal("expected error")
		}
	})
}
