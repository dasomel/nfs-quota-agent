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

package completion

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// captureStdout redirects os.Stdout for the duration of fn and returns what
// was written. RunCompletion prints directly to os.Stdout rather than
// accepting a writer.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w

	outCh := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		outCh <- buf.String()
	}()

	fn()

	_ = w.Close()
	os.Stdout = old
	return <-outCh
}

func TestRunCompletion_NoArgs(t *testing.T) {
	out := captureStdout(t, func() { RunCompletion(nil) })
	for _, want := range []string{
		"Usage: nfs-quota-agent completion <shell>",
		"bash", "zsh", "fish",
		"source <(nfs-quota-agent completion bash)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("usage output missing %q:\n%s", want, out)
		}
	}
}

func TestRunCompletion_Bash(t *testing.T) {
	out := captureStdout(t, func() { RunCompletion([]string{"bash"}) })
	if out != BashCompletion {
		t.Error("bash completion output does not match the BashCompletion constant")
	}
	if !strings.Contains(out, "_nfs_quota_agent_completions") {
		t.Error("bash completion missing expected function name")
	}
}

func TestRunCompletion_Zsh(t *testing.T) {
	out := captureStdout(t, func() { RunCompletion([]string{"zsh"}) })
	if out != ZshCompletion {
		t.Error("zsh completion output does not match the ZshCompletion var")
	}
	if !strings.HasPrefix(out, "#compdef nfs-quota-agent") {
		t.Error("zsh completion missing #compdef header")
	}
}

func TestRunCompletion_Fish(t *testing.T) {
	out := captureStdout(t, func() { RunCompletion([]string{"fish"}) })
	if out != FishCompletion {
		t.Error("fish completion output does not match the FishCompletion constant")
	}
	if !strings.Contains(out, "complete -c nfs-quota-agent") {
		t.Error("fish completion missing expected complete directives")
	}
}

// TestRunCompletion_UnknownShell exercises the os.Exit(1) branch via a
// subprocess re-exec, since calling it in-process would kill the test
// binary.
func TestRunCompletion_UnknownShell(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") == "1" {
		RunCompletion([]string{"powershell"})
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestRunCompletion_UnknownShell")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected *exec.ExitError, got %v (stderr=%s)", err, stderr.String())
	}
	if exitErr.ExitCode() != 1 {
		t.Fatalf("exit code = %d, want 1", exitErr.ExitCode())
	}
	if !strings.Contains(stderr.String(), "Unknown shell: powershell") {
		t.Fatalf("stderr = %q, want mention of the unknown shell", stderr.String())
	}
}
