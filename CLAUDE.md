# nfs-quota-agent — Agent Guidelines

Kubernetes agent that enforces filesystem project quotas (XFS / ext4 / btrfs) for NFS PersistentVolumes. Watches PV events, applies quotas via privileged OS commands, and exposes Prometheus metrics, a web UI dashboard, and audit logging.

`CLAUDE.md` is the real file — there is no `AGENTS.md` here, and adding one standalone would be invisible to Claude Code. If a tool that reads project-scope `AGENTS.md` (Codex CLI, Cursor, Copilot) ever joins this project, `git mv` this file and leave a `CLAUDE.md` containing the single line `@AGENTS.md` in the same commit.

---

## 1. Agent Operating Rules (project-scoped)

Model routing and worker tooling are personal settings — they belong in your own `~/.claude/CLAUDE.md` or an uncommitted `CLAUDE.local.md`, not here. This section states only what is true about **this repository**, whichever agent or model is doing the work.

### Risk tiers

| Change touches | Requirement |
|---|---|
| `exec` argv construction, `validateQuotaArg`, `/etc/projects` · `/etc/projid` mutation, `privileged` / RBAC in the Helm chart | **Highest tier reasoning + a review pass by a different context than the author.** A defect here is a host-level or command-injection issue, not a failing test. |
| `internal/quota/**` (any file) | Author and reviewer must be separate passes. Never self-approve. |
| `internal/agent/**` — reconcile loop, project-ID allocation | Standard worker lane; re-read §9 on hash-derived project IDs before touching allocation. |
| `internal/ui/dashboard.html` | Browser verification required (below). |
| Version bumps, `go.mod` tidy, test-table extensions | Cheapest lane that can do it; §2 lists every file a Go bump must touch. |

### Evidence rules

**Never claim done without** `make test` + `make vet` + `gofmt -l .` output shown. Quota behavior cannot be verified by reading code — the OS commands are stubbed via `CommandRunner`, so a green suite proves argv shape and parsing, not real quota enforcement. Say so explicitly when that is all you have.

**UI changes require a running binary.** `./bin/nfs-quota-agent ui --path=<dir> --addr=:8080`, then observe in a browser. `go build` passing is not evidence for `dashboard.html`.

### Delegating to workers

A worker in another process (external CLI, detached agent) does **not** inherit this file — project-scope discovery is not something to assume for any tool but Claude Code. Whenever a lane is delegated:

- Put the hard constraints for that lane **inline in the prompt**. Injecting or telling the worker to read `CLAUDE.md` is a floor, not a substitute.
- Always inline these three, because they are the ones a worker silently gets wrong: (1) external commands go through `defaultRunner`, never `exec.Command`; (2) operator-controlled strings pass `validateQuotaArg` first; (3) a new `internal/` file without a `_test.go` sibling is an incomplete change.
- Require the evidence rules above in the worker's completion criteria. A worker reporting "done" without command output has not verified anything.

---

## 2. Tech Stack

| Component | Version | Source of truth |
|---|---|---|
| Go | 1.26.0 | `go.mod`, `Dockerfile`, `.github/workflows/ci.yaml` |
| k8s.io/{api,apimachinery,client-go} | v0.36.2 | `go.mod` |
| Alpine (runtime base) | 3.21 | `Dockerfile` |
| XFS | `xfsprogs-extra` → `xfs_quota` | `Dockerfile` |
| ext4 | `quota-tools`, `e2fsprogs` → `setquota`, `chattr` | `Dockerfile` |
| btrfs | `btrfs-progs` → `btrfs qgroup` | **not installed in the image — see §9** |

### Version policy
- 항상 최신 안정 버전(stable). alpha / beta / rc 금지.
- 보안 패치 포함 버전은 즉시 반영.
- Go 버전을 올릴 때는 **네 곳 모두**: `go.mod`, `Dockerfile` (builder stage), `ci.yaml` (`go-version` ×3 + `go-version-input`), `charts/nfs-quota-agent/Chart.yaml` appVersion.

```bash
go list -m -versions k8s.io/client-go | tr ' ' '\n' | grep -vE 'alpha|beta|rc' | tail -1
go get -u ./... && go mod tidy && make test
```

---

## 3. Architecture

```
                      cmd/nfs-quota-agent/main.go
                      (flag parsing + subcommand routing only)
                                 │
          ┌──────────┬───────────┼───────────┬──────────┬─────────┐
          ▼          ▼           ▼           ▼          ▼         ▼
       agent      cleanup     status       audit    completion    ui
          │          │           │           │                     │
          ├──audit   ├──quota    ├──quota    └──util               ├──audit
          ├──history ├──status   └──util                           ├──history
          ├──quota   └──util                                       ├──policy
          ├──status                                                ├──quota
          ├──ui (OrphanInfo type)          metrics                 ├──status
          └──util                            ├──quota              └──util
                                             └──status
```

### Inter-package contracts
| Interface | Defined in | Implemented by | Purpose |
|---|---|---|---|
| `ui.AgentInterface` | `internal/ui` | `agent.QuotaAgent` | UI server queries agent state |
| `metrics.AgentInfo` | `internal/metrics` | `agent.QuotaAgent` | Metrics server queries agent |
| `ui.OrphanInfo` | `internal/ui` | produced by `agent` | Shared orphan data type |
| `quota.CommandRunner` | `internal/quota` | `execCommandRunner`, test fakes | Seam for every external binary call |

### Design decisions (do not "clean these up")
- **`QuotaAgent` fields are private** with getters/setters so `main.go` configures it without coupling to the struct layout.
- **`ui.OrphanInfo` lives in `ui`, not `agent`** — breaks the cycle: `agent` imports `ui` for the type, `ui` reaches `agent` only through `AgentInterface`.
- **`status.DirUsage` lives in `status/types.go`** so `history` can import the type without the implementation.
- **Quota functions are standalone**, not methods on `QuotaAgent`; every parameter is explicit.
- **`quota.defaultRunner` is package-level, not a parameter** — deliberate, so exported signatures stayed stable when the test seam was introduced. Do not thread a runner through every signature.

---

## 4. Package Map

```
cmd/nfs-quota-agent/main.go   CLI entry: flags + subcommand routing only

internal/
  agent/     agent.go    QuotaAgent, Run, syncAllQuotas, ensureQuota, generateProjectID
             orphan.go   findOrphans, RemoveOrphan, GetOrphans
             watch.go    watchPVs
  quota/     runner.go   CommandRunner interface + execCommandRunner (the exec seam)
             testing.go  SetCommandRunnerForTesting(r) (restore func()) — exported for other pkgs
             validate.go validateQuotaArg — rejects empty / whitespace / quote / control chars in argv
             detect.go   FSTypeXFS|Ext4|Btrfs, DetectFSType (df -T), DetectFSTypeWithFindmnt
             xfs.go      CheckXFSQuotaAvailable, ApplyXFSQuota
             ext4.go     CheckExt4QuotaAvailable, ApplyExt4Quota
             btrfs.go    CheckBtrfsQuotaAvailable, ApplyBtrfsQuota, GetBtrfsQuotaReport
             project.go  AddProject, Append/RemoveLine, ReadProjectsFile, ReadProjidFile, RemoveQuotaByID
             report.go   GetXFSQuotaReport, GetExt4QuotaReport
  audit/     entry.go (Action: CREATE/UPDATE/DELETE/CLEANUP), logger.go, filter.go
  history/   store.go    Store, UsageHistory, TrendData, Record, Query, GetTrend
  policy/    policy.go   NamespacePolicy, Violation, GetAllNamespacePolicies, GetViolations
             parse.go    ParseQuotaSize
  status/    types.go (DiskUsage, DirUsage — shared), disk.go (syscall.Statfs), dir.go,
             display.go (ShowStatus, ShowTop), report.go (JSON/YAML/CSV/table)
  metrics/   metrics.go  Collector, StartServer, AgentInfo
  cleanup/   cleanup.go  RunCleanup, OrphanedQuota, Result
  completion/completion.go  Bash/Zsh/Fish completions
  ui/        server.go   Server, Options, AgentInterface, /api/* handlers
             dashboard.go    go:embed dashboard.html
             dashboard.html  ~1500 lines HTML/CSS/JS, embedded at build time
  util/      format.go   FormatBytes, FormatDuration, ParseSize
```

Every `internal/` package carries `_test.go` siblings. A new file in `internal/` without tests is an incomplete change.

---

## 5. Conventions

**Naming** — packages singular lowercase; no stuttering (`audit.Logger`, not `audit.AuditLogger`); files lowercase with underscores.

**Logging** — `slog` only, key/value pairs: `slog.Error("failed to apply quota", "path", path, "error", err)`.

**Errors** — wrap with context (`fmt.Errorf("...%s: %w", path, err)`); log at the point of handling, never at creation.

**External commands** — always through the runner, never `exec.Command` directly, and always validate operator-controlled strings first:

```go
if err := validateQuotaArg("project name", projectName); err != nil {
    return err
}
output, err := defaultRunner.Run("xfs_quota", "-x", "-c", quotaCmd, quotaPath)
if err != nil {
    return fmt.Errorf("xfs_quota failed: %w, output: %s", err, string(output))
}
```

A raw `exec.Command` in `internal/quota` is a defect: it is both untestable and unvalidated.

**Interfaces** — define where consumed, not where implemented; accept interfaces, return structs.

---

## 6. Testing

`make test` · `make test-coverage` (race + HTML) · `go test -run TestX ./internal/quota/`.

The project-specific pattern is the command seam — no test may invoke a real quota binary:

```go
restore := quota.SetCommandRunnerForTesting(fakeRunner{out: []byte(fixture)})
defer restore()
```

Kubernetes paths use `k8s.io/client-go/kubernetes/fake`. Table-driven tests with `t.Run(tt.name, ...)` are the house style.

Because the binaries are stubbed, tests assert **argv shape and output parsing**, not enforcement. Real quota behavior needs a `prjquota`-mounted host — call that out rather than implying coverage.

---

## 7. Adding a Filesystem Backend

XFS, ext4, and btrfs are implemented; use them as the reference. For a new type:

1. `internal/quota/<fs>.go` — `Check<FS>QuotaAvailable(quotaPath string) error`, `Apply<FS>Quota(...)`, optional `Get<FS>QuotaReport(basePath) (quota, usage map[string]uint64, error)`. All calls via `defaultRunner`.
2. `internal/quota/detect.go` — add `FSType<FS>`.
3. `internal/agent/agent.go` — add cases to `detectFilesystemType()`, `checkQuotaAvailable()`, `applyQuota()`.
4. `internal/quota/<fs>_test.go` — fake-runner fixtures for success, missing binary, quota-disabled.
5. `Dockerfile` — add the tool package to the `apk add` line.
6. `README.md` / `README_ko.md` — prerequisites and required mount options.

The signatures are **not uniform**: `ApplyXFSQuota` / `ApplyExt4Quota` take the full project tuple (`quotaPath, path, projectName, projectID, sizeBytes, projectsFile, projidFile`), while `ApplyBtrfsQuota(path, sizeBytes)` does not — btrfs uses subvolume qgroups, not `/etc/projects`. Match the model your filesystem actually uses instead of forcing the XFS shape.

---

## 8. Kubernetes Contract

**PV annotations** — `nfs.io/project-name` (optional custom name), `nfs.io/quota-status` (`pending` | `applied` | `failed`), `pv.kubernetes.io/provisioned-by` (provisioner filter).

**Namespace annotations (policy)** — `nfs.io/default-quota`, `nfs.io/max-quota`.

**RBAC** — `persistentvolumes` (get/list/watch/update/patch); `persistentvolumeclaims`, `storageclasses`, `namespaces` (get/list/watch); `limitranges`, `resourcequotas` (get/list). Adding a verb widens cluster privilege — treat as an opus-reviewed change.

---

## 9. Known Constraints & Gotchas

- **btrfs tooling is not in the container image.** `Dockerfile` installs `xfsprogs-extra quota-tools e2fsprogs util-linux`; `btrfs.go` shells out to `btrfs`. In-container btrfs quotas will fail until `btrfs-progs` is added.
- **btrfs requires the target path to be a subvolume** and `btrfs quota enable <path>` to have been run; `ApplyBtrfsQuota` fails with a specific error otherwise.
- **Chart version drift.** `charts/nfs-quota-agent/Chart.yaml` has `version: 0.3.0` / `appVersion: "0.2.2"`. Reconcile appVersion with the released binary version on any release change.
- **The agent must run on the NFS server node**, privileged, with the filesystem mounted `prjquota`. By design — do not "fix" it by dropping privileges without redesigning the quota path.
- **Project IDs are hash-derived** (`hashProjectName`) with collision fallback against `loadExistingProjectIDs()`. Changing the hash silently re-maps existing quotas.

---

## 10. Verification Checklist

```bash
make build              # CGO_ENABLED=0 build
make test               # go test ./...
make vet                # go vet ./...
gofmt -l .              # must print nothing
make lint               # golangci-lint (same as CI)
make helm-lint          # chart still valid
make docker-build       # image builds
```

CI additionally runs Trivy (SARIF → GitHub Security) and `govulncheck`. `make help` lists every target — not duplicated here.

Debugging a live host:

```bash
df -T /path                                  # filesystem type
findmnt -o OPTIONS /path                     # must include prjquota
xfs_quota -x -c "report -p -b" /path         # XFS
repquota -P /path                            # ext4
btrfs qgroup show -re --raw /path            # btrfs
```
