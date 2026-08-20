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

package pvpath

import (
	"path/filepath"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNFSPath(t *testing.T) {
	cases := []struct {
		name string
		pv   *v1.PersistentVolume
		want string
	}{
		{
			name: "native NFS",
			pv: &v1.PersistentVolume{Spec: v1.PersistentVolumeSpec{PersistentVolumeSource: v1.PersistentVolumeSource{
				NFS: &v1.NFSVolumeSource{Path: "/exports/pvc-1"},
			}}},
			want: "/exports/pvc-1",
		},
		{
			name: "CSI share+subDir",
			pv: &v1.PersistentVolume{Spec: v1.PersistentVolumeSpec{PersistentVolumeSource: v1.PersistentVolumeSource{CSI: &v1.CSIPersistentVolumeSource{
				VolumeAttributes: map[string]string{"share": "/exports", "subDir": "pvc-2"},
			}}}},
			want: "/exports/pvc-2",
		},
		{
			name: "CSI share+subdir lowercase fallback",
			pv: &v1.PersistentVolume{Spec: v1.PersistentVolumeSpec{PersistentVolumeSource: v1.PersistentVolumeSource{CSI: &v1.CSIPersistentVolumeSource{
				VolumeAttributes: map[string]string{"share": "/exports", "subdir": "pvc-3"},
			}}}},
			want: "/exports/pvc-3",
		},
		{
			name: "CSI subDir takes precedence over subdir",
			pv: &v1.PersistentVolume{Spec: v1.PersistentVolumeSpec{PersistentVolumeSource: v1.PersistentVolumeSource{CSI: &v1.CSIPersistentVolumeSource{
				VolumeAttributes: map[string]string{"share": "/exports", "subDir": "upper", "subdir": "lower"},
			}}}},
			want: "/exports/upper",
		},
		{
			name: "CSI share only falls back to PV name",
			pv: &v1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "pvc-4"},
				Spec: v1.PersistentVolumeSpec{PersistentVolumeSource: v1.PersistentVolumeSource{CSI: &v1.CSIPersistentVolumeSource{
					VolumeAttributes: map[string]string{"share": "/exports"},
				}}},
			},
			want: "/exports/pvc-4",
		},
		{
			name: "CSI missing attributes",
			pv:   &v1.PersistentVolume{Spec: v1.PersistentVolumeSpec{PersistentVolumeSource: v1.PersistentVolumeSource{CSI: &v1.CSIPersistentVolumeSource{}}}},
			want: "",
		},
		{
			name: "no NFS or CSI source",
			pv:   &v1.PersistentVolume{},
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NFSPath(tc.pv); got != tc.want {
				t.Errorf("NFSPath() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestToLocal(t *testing.T) {
	if got := ToLocal("/exports/pvc-1", "/exports", "/data/nfs"); got.Path != filepath.Join("/data/nfs", "pvc-1") || got.Fallback {
		t.Errorf("ToLocal(matching prefix) = %+v, want Path=%q Fallback=false", got, filepath.Join("/data/nfs", "pvc-1"))
	}

	got := ToLocal("/other/deep/pvc-2", "/exports", "/data/nfs")
	if got.Path != filepath.Join("/data/nfs", "pvc-2") {
		t.Errorf("ToLocal(non-matching prefix).Path = %q, want %q", got.Path, filepath.Join("/data/nfs", "pvc-2"))
	}
	if !got.Fallback {
		t.Error("ToLocal(non-matching prefix).Fallback = false, want true")
	}
}

func TestContains(t *testing.T) {
	cases := []struct {
		name        string
		root        string
		target      string
		wantContain bool
	}{
		{"root itself", "/export", "/export", true},
		{"direct child", "/export", "/export/pvc-1", true},
		{"nested child", "/export", "/export/a/b/pvc-1", true},
		{"traversal escape", "/export", "/export/../etc", false},
		{"sibling with shared prefix", "/export", "/exportfoo", false},
		{"sibling with shared prefix and subdir", "/export", "/exportfoo/pvc-1", false},
		{"unrelated path", "/export", "/etc/passwd", false},
		{"uncleaned but valid child", "/export", "/export/./a/../pvc-1", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Contains(tc.root, tc.target); got != tc.wantContain {
				t.Errorf("Contains(%q, %q) = %v, want %v", tc.root, tc.target, got, tc.wantContain)
			}
		})
	}
}
