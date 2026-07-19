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

// SetCommandRunnerForTesting overrides the package-level CommandRunner used by
// every exported function in this package. It exists so callers outside this
// package (e.g. internal/agent tests) can stub command execution without
// invoking real system binaries. It is not safe for concurrent use across
// parallel tests; callers must always invoke the returned restore func
// (typically via t.Cleanup) to put the previous runner back.
func SetCommandRunnerForTesting(r CommandRunner) (restore func()) {
	prev := defaultRunner
	defaultRunner = r
	return func() {
		defaultRunner = prev
	}
}
