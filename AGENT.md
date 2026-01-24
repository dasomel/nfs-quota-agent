# nfs quota agent - development guidelines

## project overview

kubernetes agent that enforces filesystem project quotas (xfs/ext4) for nfs persistentvolumes.

## tech stack

- language: go 1.25+
- kubernetes client-go v0.35+
- container base: alpine linux 3.21+
- xfs/ext4 project quota commands

## version management guidelines

### policy
- **항상 최신 안정 버전(stable)을 사용한다**
- alpha, beta, rc 버전은 사용하지 않는다
- 보안 패치가 포함된 버전은 즉시 업데이트한다

### version check commands

```bash
# go 최신 버전 확인
go version

# kubernetes client-go 최신 버전 확인
go list -m -versions k8s.io/client-go | tr ' ' '\n' | grep -v alpha | grep -v beta | grep -v rc | tail -1

# 의존성 업데이트
go get -u ./...
go mod tidy

# alpine 최신 버전 확인
# https://alpinelinux.org/releases/
```

### current versions (2026-01)

| component | version | check command |
|-----------|---------|---------------|
| go | 1.25.x | `go version` |
| client-go | v0.35.x | `go list -m k8s.io/client-go` |
| alpine | 3.21 | dockerfile |

### update checklist

1. `go.mod` - go 버전, kubernetes 의존성
2. `dockerfile` - go 이미지, alpine 이미지
3. `charts/*/chart.yaml` - appversion
4. 업데이트 후 반드시 `make test` 실행

## project structure

```
nfs-quota-agent/
├── cmd/nfs-quota-agent/
│   ├── main.go              # entry point, subcommands, cli flags
│   ├── agent.go             # quotaagent struct, core logic, pv watching
│   ├── agent_test.go        # agent unit tests
│   ├── integration_test.go  # integration tests with fake k8s client
│   ├── quota_xfs.go         # xfs-specific quota functions
│   ├── quota_xfs_test.go    # xfs quota unit tests
│   ├── quota_ext4.go        # ext4-specific quota functions
│   ├── quota_ext4_test.go   # ext4 quota unit tests
│   ├── status.go            # status command - disk/quota status display
│   ├── report.go            # report command - json/yaml/csv export
│   ├── metrics.go           # prometheus metrics endpoint
│   ├── utils.go             # utility functions
│   └── utils_test.go        # utils unit tests
├── charts/nfs-quota-agent/  # helm chart
│   ├── chart.yaml
│   ├── values.yaml
│   └── templates/
├── deploy/              # kustomize manifests
│   ├── deployment.yaml
│   ├── rbac.yaml
│   └── kustomization.yaml
├── .github/workflows/   # ci/cd pipelines
│   ├── ci.yaml
│   └── release.yaml
├── dockerfile
├── makefile
├── readme.md
└── go.mod
```

## code organization rules

1. **main.go**: only contains `main()` function, flag parsing, and client initialization
2. **agent.go**: contains `quotaagent` struct and all core business logic
3. **quota_*.go**: filesystem-specific implementations (one file per filesystem)
4. **utils.go**: helper functions shared across the codebase

## adding new filesystem support

to add support for a new filesystem (e.g., btrfs):

1. create `quota_btrfs.go` with:
   - `checkbtrfsquotaavailable()` - verify quota tools exist
   - `applybtrfsquota()` - apply quota to directory

2. update `agent.go`:
   - add constant `fstypebtrfs = "btrfs"`
   - add case in `detectfilesystemtype()`
   - add case in `checkquotaavailable()`
   - add case in `applyquota()`
   - add case in `removequota()`

3. update `dockerfile`:
   - add required packages for btrfs quota tools

4. update `readme.md`:
   - document prerequisites and mount options

## coding conventions

### go style
- use `slog` for structured logging
- error wrapping with `fmt.errorf("context: %w", err)`
- mutex for concurrent access to shared state (`a.mu`)

### naming
- constants: camelcase with prefix (`fstypexfs`, `quotastatusapplied`)
- functions: descriptive verbs (`ensurequota`, `shouldprocesspv`)
- files: lowercase with underscore (`quota_xfs.go`)

### error handling
- return errors up the call stack
- log errors at the point of handling
- use `slog.warn` for recoverable issues
- use `slog.error` for failures that affect functionality

## build & test

```bash
# build
make build

# build for linux
make build-linux

# run tests
make test

# run tests with coverage
make test-coverage

# format code
make fmt

# run go vet
make vet

# run linter (requires golangci-lint)
make lint

# build docker image
make docker-build VERSION=v1.0.0

# deploy to k8s
make deploy

# undeploy
make undeploy
```

### test files
- `agent_test.go` - core agent functionality
- `integration_test.go` - k8s fake client integration tests
- `quota_xfs_test.go` - xfs quota logic
- `quota_ext4_test.go` - ext4 quota logic
- `utils_test.go` - utility functions

## key kubernetes resources

### pv annotations used
- `nfs.io/project-name`: custom project name (optional)
- `nfs.io/quota-status`: status tracking (pending/applied/failed)
- `pv.kubernetes.io/provisioned-by`: filter by provisioner

### rbac requirements
- `persistentvolumes`: get, list, watch, update, patch
- `persistentvolumeclaims`: get, list, watch
- `storageclasses`: get, list, watch

## testing locally

```bash
./bin/nfs-quota-agent \
  --kubeconfig=$HOME/.kube/config \
  --nfs-base-path=/mnt/nfs \
  --nfs-server-path=/data \
  --sync-interval=10s
```

## security practices

### sbom (software bill of materials)
- container images include sbom attestation (via docker buildx)
- binary releases include spdx format sbom (`sbom.spdx.json`)

### vulnerability scanning
- ci: trivy filesystem scan + govulncheck for go dependencies
- release: trivy container image scan with sarif upload to github security

### supply chain security
- container images include provenance attestation (`provenance: mode=max`)
- all release artifacts include sha256 checksums

## important notes

- agent must run on nfs server node (requires host filesystem access)
- container needs `privileged: true` for quota commands
- filesystem must be mounted with `prjquota` option
- project ids are generated via fnv hash (deterministic)

---

# required skills

## go development
- go 1.25+ syntax and idioms
- go modules (`go.mod`, `go.sum`)
- standard library: `context`, `sync`, `os/exec`, `log/slog`
- error handling patterns with `%w` wrapping
- goroutines and channels for concurrent operations
- table-driven testing with `testing` package

## kubernetes
- client-go library usage
- persistentvolume (pv) and persistentvolumeclaim (pvc) concepts
- watch api for real-time resource monitoring
- rbac (serviceaccount, clusterrole, clusterrolebinding)
- kustomize for deployment management

## linux filesystem
- xfs project quota (`xfs_quota`, `prjquota` mount option)
- ext4 project quota (`setquota`, `chattr`, `tune2fs`)
- filesystem detection (`df -t`, `findmnt`)
- mount options and `/etc/fstab`

## container & deployment
- multi-stage dockerfile builds
- alpine linux package management (`apk`)
- kubernetes deployment, daemonset patterns
- node selectors and tolerations

## common tasks

### adding features
1. understand existing code structure in `cmd/nfs-quota-agent/`
2. follow file organization rules (see readme.md)
3. run `make fmt` and `make vet` before committing
4. update readme.md if user-facing changes

### debugging quota issues
1. check filesystem type: `df -t /path`
2. verify mount options: `findmnt -o options /path`
3. test quota commands manually:
   - xfs: `xfs_quota -x -c "report -p" /path`
   - ext4: `repquota -p /path`

### adding new filesystem support
1. create `quota_<fstype>.go` file
2. implement `check<fstype>quotaavailable()` and `apply<fstype>quota()`
3. update switch cases in `agent.go`
4. add packages to dockerfile
5. document in readme.md

## code patterns

### structured logging
```go
slog.Info("message", "key1", value1, "key2", value2)
slog.Error("failed to do x", "error", err, "context", ctx)
slog.Warn("recoverable issue", "detail", detail)
slog.Debug("verbose info", "data", data)
```

### error handling
```go
if err != nil {
    return fmt.Errorf("failed to do x: %w", err)
}
```

### exec external commands
```go
cmd := exec.Command("command", "arg1", "arg2")
output, err := cmd.CombinedOutput()
if err != nil {
    return fmt.Errorf("command failed: %w, output: %s", err, string(output))
}
```

### kubernetes client operations
```go
// list resources
pvList, err := a.client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})

// watch resources
watcher, err := a.client.CoreV1().PersistentVolumes().Watch(ctx, metav1.ListOptions{})
for event := range watcher.ResultChan() {
    // handle event
}

// update resource
_, err = a.client.CoreV1().PersistentVolumes().Update(ctx, pv, metav1.UpdateOptions{})
```

## testing

### test structure
```
cmd/nfs-quota-agent/
├── agent_test.go        # core agent logic tests
├── integration_test.go  # fake k8s client integration tests
├── quota_xfs_test.go    # xfs quota tests
├── quota_ext4_test.go   # ext4 quota tests
└── utils_test.go        # utility function tests
```

### running tests
```bash
# run all tests
make test

# run with coverage
make test-coverage

# run specific test
go test -v -run TestShouldProcessPV ./...

# run with race detection
go test -race ./...
```

### testing patterns
```go
// table-driven tests
func TestFunction(t *testing.T) {
    tests := []struct {
        name     string
        input    string
        expected string
    }{
        {"case1", "input1", "expected1"},
        {"case2", "input2", "expected2"},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            result := Function(tt.input)
            if result != tt.expected {
                t.Errorf("got %v, want %v", result, tt.expected)
            }
        })
    }
}

// using fake k8s client
import "k8s.io/client-go/kubernetes/fake"

func TestWithFakeClient(t *testing.T) {
    fakeClient := fake.NewSimpleClientset(pvObjects...)
    agent := NewQuotaAgent(fakeClient, "/export", "/data", "provisioner")
    // test agent behavior
}
```

## testing checklist

- [ ] `go build ./...` succeeds
- [ ] `go test ./...` passes
- [ ] `go vet ./...` passes
- [ ] `go fmt ./...` applied
- [ ] `make docker-build` succeeds
- [ ] manual test with `--kubeconfig` flag
- [ ] readme.md updated if needed
