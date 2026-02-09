# nfs-quota-agent - Development Guidelines

## Project Overview

Kubernetes agent that enforces filesystem project quotas (XFS/ext4) for NFS PersistentVolumes. Watches PV events, applies quotas via OS-level project quota commands, and provides monitoring through Prometheus metrics, web UI dashboard, and audit logging.

## Tech Stack

| Component | Version | Notes |
|-----------|---------|-------|
| Go | 1.24 | `go.mod` |
| client-go | v0.29.0 | Kubernetes API client |
| Alpine | 3.21 | Container runtime base |
| XFS tools | xfsprogs-extra | `xfs_quota` command |
| ext4 tools | quota-tools, e2fsprogs | `setquota`, `chattr` commands |

### Version Policy
- 항상 최신 안정 버전(stable)을 사용한다
- alpha, beta, rc 버전은 사용하지 않는다
- 보안 패치가 포함된 버전은 즉시 업데이트한다

### Version Check Commands
```bash
go version
go list -m -versions k8s.io/client-go | tr ' ' '\n' | grep -v alpha | grep -v beta | grep -v rc | tail -1
go get -u ./... && go mod tidy
```

### Update Checklist
1. `go.mod` - Go 버전, Kubernetes 의존성
2. `Dockerfile` - Go 이미지, Alpine 이미지
3. `charts/*/Chart.yaml` - appVersion
4. 업데이트 후 반드시 `make test` 실행

---

## Project Structure

```
nfs-quota-agent/
├── cmd/nfs-quota-agent/
│   └── main.go                    # CLI entry point: flag parsing + subcommand routing only
│
├── internal/
│   ├── agent/                     # Core agent logic
│   │   ├── agent.go               # QuotaAgent struct, Run(), syncAllQuotas, ensureQuota
│   │   ├── orphan.go              # Orphan detection/cleanup: findOrphans, RemoveOrphan, GetOrphans
│   │   └── watch.go               # PV watcher: watchPVs
│   │
│   ├── audit/                     # Audit logging
│   │   ├── entry.go               # Entry struct, Action constants (CREATE/UPDATE/DELETE/CLEANUP)
│   │   ├── logger.go              # Logger struct, Config, NewLogger, Log, LogQuotaCreate/Update
│   │   ├── filter.go              # Filter struct, QueryLog, PrintEntries
│   │   └── audit_test.go
│   │
│   ├── cleanup/                   # Standalone cleanup command
│   │   └── cleanup.go             # RunCleanup, OrphanedQuota, Result
│   │
│   ├── completion/                # Shell completions
│   │   └── completion.go          # BashCompletion, ZshCompletion, FishCompletion, RunCompletion
│   │
│   ├── history/                   # Usage history tracking
│   │   ├── store.go               # Store, UsageHistory, TrendData, NewStore, Record, Query
│   │   └── store_test.go
│   │
│   ├── metrics/                   # Prometheus metrics
│   │   └── metrics.go             # Collector, StartServer, AgentInfo interface
│   │
│   ├── policy/                    # Namespace quota policies
│   │   ├── policy.go              # NamespacePolicy, Violation, GetAllNamespacePolicies, GetViolations
│   │   ├── parse.go               # ParseQuotaSize
│   │   └── parse_test.go
│   │
│   ├── quota/                     # Filesystem quota operations
│   │   ├── detect.go              # DetectFSType (df -T), DetectFSTypeWithFindmnt
│   │   ├── xfs.go                 # CheckXFSQuotaAvailable, ApplyXFSQuota
│   │   ├── ext4.go                # CheckExt4QuotaAvailable, ApplyExt4Quota
│   │   ├── project.go             # AddProject, AppendToFile, RemoveLineFromFile, ReadProjectsFile
│   │   ├── report.go              # GetXFSQuotaReport, GetExt4QuotaReport
│   │   └── report_cmd.go          # OS command constructors for report
│   │
│   ├── status/                    # Status display & reporting
│   │   ├── types.go               # DiskUsage, DirUsage structs (shared across packages)
│   │   ├── disk.go                # GetDiskUsage (syscall.Statfs)
│   │   ├── dir.go                 # GetDirUsages, GetDirSize
│   │   ├── display.go             # ShowStatus, ShowTop, MakeProgressBar
│   │   └── report.go              # QuotaReport, GenerateReport (JSON/YAML/CSV/table)
│   │
│   ├── ui/                        # Web UI dashboard
│   │   ├── dashboard.go           # go:embed dashboard.html
│   │   ├── dashboard.html         # ~1500 lines HTML/CSS/JS (embedded at build time)
│   │   └── server.go              # Server, Options, AgentInterface, all /api/* handlers
│   │
│   └── util/                      # Shared utilities
│       ├── format.go              # FormatBytes, FormatDuration, ParseSize
│       └── format_test.go
│
├── charts/nfs-quota-agent/        # Helm chart
├── docs/                          # Documentation & screenshots
├── .github/workflows/             # CI/CD pipelines
├── Dockerfile
├── Makefile
└── go.mod
```

---

## Architecture & Package Dependencies

```
                      cmd/nfs-quota-agent/main.go
                      (flag parsing + subcommand routing)
                                 │
          ┌──────────┬───────────┼───────────┬──────────┬─────────┐
          ▼          ▼           ▼           ▼          ▼         ▼
       agent      cleanup    status       audit     completion   ui
          │          │          │           │                     │
          ├──audit   ├──quota   ├──quota    ├──util               ├──audit
          ├──history ├──status  └──util     │                     ├──history
          ├──quota   └──util               │                     ├──policy
          ├──status                        │                     ├──quota
          ├──ui (OrphanInfo type)          │                     ├──status
          └──util                          │                     └──util
                                           │
       metrics                             │
          ├──quota                         │
          └──status                        │
```

### Inter-Package Contracts

| Interface | Defined In | Implemented By | Purpose |
|-----------|-----------|----------------|---------|
| `ui.AgentInterface` | `internal/ui` | `agent.QuotaAgent` | UI server queries agent state |
| `metrics.AgentInfo` | `internal/metrics` | `agent.QuotaAgent` | Metrics server queries agent |
| `ui.OrphanInfo` | `internal/ui` | used by `agent` | Shared orphan data type |

### Key Design Decisions
- **Agent fields are private** with getter/setter methods, allowing `main.go` to configure the agent without tight coupling
- **`ui.OrphanInfo`** lives in `ui` package (not `agent`) to avoid circular dependency: `agent` imports `ui` for the type, `ui` imports `agent` via interface
- **`status.DirUsage`** is in `status/types.go` so `history` can import it without pulling in `status` implementation
- **Quota functions are standalone** (not methods on QuotaAgent), accepting all parameters explicitly

---

## Subcommands

| Command | Entry Function | Packages Used |
|---------|---------------|---------------|
| `run` | `runAgent()` | agent, audit, history, metrics, policy, ui |
| `status` | `runStatus()` | status |
| `top` | `runTop()` | status |
| `report` | `runReport()` | status |
| `cleanup` | `runCleanup()` | cleanup |
| `ui` | `runUI()` | ui |
| `audit` | `runAudit()` | audit |
| `completion` | `completion.RunCompletion()` | completion |

---

## Adding New Filesystem Support

To add support for a new filesystem (e.g., btrfs):

1. **`internal/quota/btrfs.go`** - Create with:
   - `CheckBtrfsQuotaAvailable(quotaPath string) error`
   - `ApplyBtrfsQuota(quotaPath, path, projectName string, projectID uint32, sizeBytes int64, projectsFile, projidFile string) error`

2. **`internal/quota/detect.go`** - Add constant:
   ```go
   const FSTypeBtrfs = "btrfs"
   ```

3. **`internal/agent/agent.go`** - Add cases in:
   - `detectFilesystemType()` - recognize "btrfs"
   - `checkQuotaAvailable()` - call `quota.CheckBtrfsQuotaAvailable()`
   - `applyQuota()` - call `quota.ApplyBtrfsQuota()`

4. **`internal/quota/report.go`** - Add `GetBtrfsQuotaReport()` if applicable

5. **`Dockerfile`** - Add required packages (`apk add btrfs-progs`)

6. **`README.md`** - Document prerequisites and mount options

---

## Coding Conventions

### Naming
- **Packages**: singular, lowercase (`audit`, `quota`, `status`)
- **Types**: avoid stuttering with package prefix (`audit.Logger` not `audit.AuditLogger`)
- **Exported**: PascalCase (`FormatBytes`, `QuotaAgent`)
- **Private**: camelCase (`nfsBasePath`, `syncAllQuotas`)
- **Constants**: PascalCase for exported (`FSTypeXFS`), camelCase for unexported
- **Files**: lowercase with underscore (`report_cmd.go`)

### Structured Logging
```go
slog.Info("message", "key1", value1, "key2", value2)
slog.Error("failed to do x", "error", err, "context", ctx)
slog.Warn("recoverable issue", "detail", detail)
slog.Debug("verbose info", "data", data)
```

### Error Handling
```go
// Wrap with context
if err != nil {
    return fmt.Errorf("failed to apply quota for %s: %w", path, err)
}

// Log at point of handling, not at point of creation
```

### External Commands
```go
cmd := exec.Command("xfs_quota", "-x", "-c", quotaCmd, quotaPath)
output, err := cmd.CombinedOutput()
if err != nil {
    return fmt.Errorf("xfs_quota failed: %w, output: %s", err, string(output))
}
```

### Interface Patterns (for decoupling)
```go
// Define interface where it's consumed (not where it's implemented)
type AgentInfo interface {
    BasePath() string
    AppliedQuotaCount() int
}

// Accept interface, return struct
func StartServer(addr string, agent AgentInfo, version string) { ... }
```

---

## Build & Test

```bash
make build              # Build binary (CGO_ENABLED=0)
make build-linux        # Build for linux/amd64, arm64, armv7
make test               # go test -v ./...
make test-coverage      # Tests with race detection + coverage HTML
make fmt                # go fmt ./...
make vet                # go vet ./...
make lint               # golangci-lint run
make docker-build       # Build Docker image
make docker-buildx      # Multi-arch build & push
make helm-lint          # Lint Helm chart
make helm-install       # Install using Helm
make helm-uninstall     # Uninstall Helm release
```

### Test Files
```
internal/util/format_test.go     # FormatBytes, ParseSize
internal/audit/audit_test.go     # Logger, LogQuotaCreate, filter
internal/history/store_test.go   # Store, Record, Query, GetTrend
internal/policy/parse_test.go    # ParseQuotaSize
```

### Running Tests
```bash
go test ./...                              # All tests
go test -v -run TestFormatBytes ./...      # Specific test
go test -race ./...                        # Race detection
go test -coverprofile=c.out ./internal/... # Internal packages only
```

### Testing Patterns
```go
// Table-driven tests
func TestFormatBytes(t *testing.T) {
    tests := []struct {
        name     string
        input    int64
        expected string
    }{
        {"zero", 0, "0 B"},
        {"bytes", 100, "100 B"},
        {"kilobytes", 1024, "1.00 KB"},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            result := util.FormatBytes(tt.input)
            if result != tt.expected {
                t.Errorf("FormatBytes(%d) = %q, want %q", tt.input, result, tt.expected)
            }
        })
    }
}

// Using fake Kubernetes client
import "k8s.io/client-go/kubernetes/fake"

func TestWithFakeClient(t *testing.T) {
    fakeClient := fake.NewSimpleClientset(pvObjects...)
    ag := agent.NewQuotaAgent(fakeClient, "/export", "/data", "provisioner")
    // test agent behavior
}
```

---

## Kubernetes Resources

### PV Annotations
| Annotation | Purpose |
|-----------|---------|
| `nfs.io/project-name` | Custom project name (optional) |
| `nfs.io/quota-status` | Quota status: `pending`, `applied`, `failed` |
| `pv.kubernetes.io/provisioned-by` | Filter by provisioner |

### Namespace Annotations (Policy)
| Annotation | Purpose |
|-----------|---------|
| `nfs.io/default-quota` | Default quota size (e.g., `10Gi`) |
| `nfs.io/max-quota` | Maximum allowed quota |

### RBAC Requirements
- `persistentvolumes`: get, list, watch, update, patch
- `persistentvolumeclaims`: get, list, watch
- `storageclasses`: get, list, watch
- `namespaces`: get, list, watch (for policy)
- `limitranges`: get, list (for policy)
- `resourcequotas`: get, list (for policy)

---

## Running Locally

```bash
# Agent mode (requires Kubernetes cluster)
./bin/nfs-quota-agent run \
  --kubeconfig=$HOME/.kube/config \
  --nfs-base-path=/mnt/nfs \
  --nfs-server-path=/data \
  --sync-interval=10s \
  --enable-ui \
  --enable-audit

# Status check (no Kubernetes needed)
./bin/nfs-quota-agent status --path=/mnt/nfs

# Web UI standalone
./bin/nfs-quota-agent ui --path=/mnt/nfs --addr=:8080

# Report generation
./bin/nfs-quota-agent report --path=/mnt/nfs --format=json --output=report.json

# Audit log query
./bin/nfs-quota-agent audit --file=/var/log/nfs-quota-agent/audit.log --action=CREATE
```

---

## Security Practices

### Container Security
- Agent must run on NFS server node (requires host filesystem access)
- Container needs `privileged: true` for quota commands
- Filesystem must be mounted with `prjquota` option

### Supply Chain
- Container images include SBOM attestation (via `docker buildx`)
- Binary releases include SPDX format SBOM (`sbom.spdx.json`)
- Container images include provenance attestation (`provenance: mode=max`)
- All release artifacts include SHA256 checksums

### Vulnerability Scanning
- CI: Trivy filesystem scan + govulncheck for Go dependencies
- Release: Trivy container image scan with SARIF upload to GitHub Security

---

## Verification Checklist

```bash
go build ./...          # Build passes
go test ./...           # All tests pass
go vet ./...            # No issues
gofmt -l ./...          # No formatting issues
make build              # Makefile build passes
make docker-build       # Docker build passes
```

---

## Debugging Quota Issues

```bash
# Check filesystem type
df -T /path

# Verify mount options (must include prjquota)
findmnt -o OPTIONS /path

# Test quota commands manually
# XFS:
xfs_quota -x -c "report -p -b" /path
xfs_quota -x -c "limit -p bhard=10g projectname" /path

# ext4:
repquota -P /path
setquota -P projectid 0 10485760 0 0 /path
```
