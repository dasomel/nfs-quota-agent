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

package util

import (
	"net/http"
	"testing"
	"time"
)

func TestNewHTTPServer(t *testing.T) {
	mux := http.NewServeMux()
	addr := ":9999"

	server := NewHTTPServer(addr, mux)

	if server.Addr != addr {
		t.Errorf("Addr = %q, expected %q", server.Addr, addr)
	}
	if server.Handler != mux {
		t.Errorf("Handler not wired to the given handler")
	}
	if server.ReadTimeout != 10*time.Second {
		t.Errorf("ReadTimeout = %v, expected %v", server.ReadTimeout, 10*time.Second)
	}
	if server.ReadHeaderTimeout != 5*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, expected %v", server.ReadHeaderTimeout, 5*time.Second)
	}
	if server.WriteTimeout != 30*time.Second {
		t.Errorf("WriteTimeout = %v, expected %v", server.WriteTimeout, 30*time.Second)
	}
	if server.IdleTimeout != 120*time.Second {
		t.Errorf("IdleTimeout = %v, expected %v", server.IdleTimeout, 120*time.Second)
	}
}
