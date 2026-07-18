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
	"strings"
	"testing"
)

// containsArg reports whether args contains want as an exact element.
func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// fileContains reads filename and reports whether it contains substr.
func fileContains(t *testing.T, filename, substr string) bool {
	t.Helper()
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("failed to read %s: %v", filename, err)
	}
	return strings.Contains(string(data), substr)
}

// makeDirWithSubdir creates root with one nested subdirectory, used to
// exercise filepath.WalkDir fallback paths.
func makeDirWithSubdir(root string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	return os.MkdirAll(root+"/sub", 0o755)
}
