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

import "testing"

// fakeCall records one invocation made through fakeRunner.
type fakeCall struct {
	name string
	args []string
}

// fakeRunner is a scriptable CommandRunner used by tests to avoid invoking
// real system binaries. fn decides the response for each call; calls records
// every invocation for assertions on command construction.
type fakeRunner struct {
	fn    func(name string, args ...string) ([]byte, error)
	calls []fakeCall
}

func (f *fakeRunner) Run(name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, fakeCall{name: name, args: args})
	if f.fn == nil {
		return nil, nil
	}
	return f.fn(name, args...)
}

// withFakeRunner installs r as the package defaultRunner for the duration of
// the test and restores the previous runner afterward.
func withFakeRunner(t *testing.T, r *fakeRunner) {
	t.Helper()
	prev := defaultRunner
	defaultRunner = r
	t.Cleanup(func() {
		defaultRunner = prev
	})
}
