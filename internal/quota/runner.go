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

import "os/exec"

// CommandRunner abstracts external command execution so quota operations
// can be unit tested without invoking real system binaries (xfs_quota,
// setquota, chattr, findmnt, df, repquota, ...).
type CommandRunner interface {
	// Run executes name with args and returns the combined stdout+stderr
	// output, mirroring exec.Cmd.CombinedOutput semantics.
	Run(name string, args ...string) ([]byte, error)
}

// execCommandRunner is the default CommandRunner backed by os/exec.
type execCommandRunner struct{}

func (execCommandRunner) Run(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// defaultRunner is the CommandRunner used by every exported function in this
// package. It is package-level (rather than a parameter) so all existing
// exported signatures stay unchanged; tests override it to avoid invoking
// real binaries.
var defaultRunner CommandRunner = execCommandRunner{}
