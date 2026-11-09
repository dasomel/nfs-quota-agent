//go:build linux

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
	"os"
	"syscall"
)

func getStatKey(fi os.FileInfo) (statKey, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return statKey{}, false
	}
	return statKey{
		dev:      st.Dev,
		ino:      st.Ino,
		size:     st.Size,
		mtimeSec: int64(st.Mtim.Sec),
		mtimeNs:  int64(st.Mtim.Nsec),
		ctimeSec: int64(st.Ctim.Sec),
		ctimeNs:  int64(st.Ctim.Nsec),
	}, true
}
