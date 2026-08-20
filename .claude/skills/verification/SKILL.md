---
name: verification
description: Verify a change to nfs-quota-agent before claiming it is done, fixed, or passing. Use when finishing any code change in this repo, before committing or opening a PR, and whenever deciding what counts as evidence for quota, UI, chart, or dependency changes.
---

# Verifying a change to nfs-quota-agent

Run what the change can actually invalidate, paste the output, and name what you did not verify. `make help` lists every target; the notes below are about which ones prove what.

## Baseline for any Go change

```bash
make test && make vet && gofmt -l .
```

`gofmt -l .` printing nothing is the pass condition. `make lint` runs the same golangci-lint config CI does — run it when the diff is large enough that a style failure would be an annoying round trip.

## What each kind of change needs beyond the baseline

**`internal/quota/**`** — the OS binaries are stubbed through `quota.CommandRunner`, so tests can only prove argv shape and output parsing. Report that distinction explicitly. Real enforcement needs a host with the filesystem mounted `prjquota`; if you don't have one, say the change is unverified against a real filesystem rather than implying coverage.

Useful on a real host:

```bash
findmnt -o OPTIONS /path                     # must include prjquota
xfs_quota -x -c "report -p -b" /path
repquota -P /path
btrfs qgroup show -re --raw /path
```

**`internal/ui/dashboard.html`** — the file is `go:embed`-ed, so a build proves it compiled in, not that it renders. Launch it and look:

```bash
make build && ./bin/nfs-quota-agent ui --path=<dir> --addr=:8080
```

Then open it in a browser and exercise the part you changed. A passing build is not evidence for this file.

**Chart or manifest changes** — `make helm-lint`. If RBAC verbs or `privileged` changed, state the new privilege surface in your report; a lint pass says nothing about whether the widening was warranted.

**Dependency or Go version bumps** — `make build && make test && make docker-build`. The Docker build is the step that catches a `Dockerfile` builder-stage version left behind. Confirm all four version sites moved together (see `CLAUDE.md`).

## Reporting

State the commands you ran with their output, then the gap: which parts are covered by stubbed tests, which need a real NFS host, which were not exercised at all. A completion claim without that split is not verification.
