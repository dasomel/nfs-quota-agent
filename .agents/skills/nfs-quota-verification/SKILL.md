---
name: nfs-quota-verification
description: Verify nfs-quota-agent changes with evidence appropriate to Go logic, real quota filesystems, embedded UI, Helm/RBAC, and dependency bumps. Use before claiming a repository change is complete, fixed, or safe to merge.
license: Apache-2.0
compatibility: Requires the nfs-quota-agent checkout, Go toolchain, Make targets, and optional real XFS/ext4/Btrfs quota host for runtime evidence.
metadata:
  openforge-scope: project
  openforge-owner: dasomel/nfs-quota-agent
  openforge-maturity: draft
  openforge-version: "1"
---

# nfs-quota-agent Verification

## Use When

- Finishing any code, chart, UI, dependency, or quota-behavior change in this repository.
- Deciding whether current evidence proves the requested behavior.
- Preparing a commit or PR that claims a bug is fixed or a feature is complete.

## Do Not Use When

- You only need generic Go formatting/lint advice unrelated to this repository.
- A task has not changed behavior and no completion claim is being made.

## Inputs

- The changed paths and requested behavior.
- Current repository state and relevant issue/spec.
- Whether a real quota-enabled host is available.

## Workflow

1. Identify what the change can invalidate instead of blindly running every check.
2. For ordinary Go changes, run:

```bash
make test && make vet && gofmt -l .
```

`gofmt -l .` must print nothing. Run `make lint` for a material code diff or before merge when CI lint failures would be costly.

3. Apply the path-specific evidence below.

### Quota implementation (`internal/quota/**`)

Tests stub OS quota binaries through `quota.CommandRunner`; they prove argv shape and parsing, not real enforcement. When a real host is available, confirm the mounted filesystem and actual quota state:

```bash
findmnt -o OPTIONS /path
xfs_quota -x -c "report -p -b" /path
repquota -P /path
btrfs qgroup show -re --raw /path
```

Only run the command appropriate to the tested filesystem. If no real quota host is available, report that runtime enforcement remains unverified.

### Embedded dashboard (`internal/ui/dashboard.html`)

A Go build only proves the embedded file compiled. For visual/interaction changes:

```bash
make build && ./bin/nfs-quota-agent ui --path=<dir> --addr=:8080
```

Open the UI and exercise the changed behavior.

### Helm / manifest / RBAC

Run `make helm-lint`. If RBAC verbs, host access, or `privileged` changed, report the new privilege surface separately; lint does not prove the widening is justified.

### Dependency / Go version bump

Run:

```bash
make build && make test && make docker-build
```

Confirm all version sites documented by `AGENTS.md` move together, including the Docker builder stage.

## Verification

Report:

- exact commands run and pass/fail result;
- whether evidence is unit/stub, build/static, UI/manual, or real filesystem/runtime;
- important paths not exercised;
- any privilege/API behavior that changed.

Do not present stubbed quota tests as proof of a real filesystem property.

## Stop / Escalate When

- Real filesystem behavior is required but no suitable quota-enabled host is available.
- The fix requires permission/RBAC widening or destructive filesystem operations beyond the approved scope.
- Current evidence contradicts the intended behavior or repeated patches stop converging.

## References

- `AGENTS.md`
- `Makefile` / `make help`
- quota implementation under `internal/quota/`
- Helm/chart manifests and repository CI configuration
