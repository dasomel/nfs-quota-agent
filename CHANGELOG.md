# Changelog

All notable changes to this project will be documented in this file.

## [0.2.2] - 2026-03-23

### Bug Fixes

- Fix `chattr` argument splitting to prevent incorrect project ID flag passing by @dasomel
- Fix `RemoveQuotaByID` stub — fully implement quota removal for XFS and ext4 by @dasomel
- Fix path traversal vulnerability in Web UI directory listing by @dasomel
- Fix silent error discard in audit logger helper methods by @dasomel
- Fix `Record()` deadlock risk caused by deferred unlock before file write by @dasomel

### Security

- Add Bearer token authentication middleware to all `/api/*` routes by @dasomel
- Replace `sh -c` shell injection surface in ext4 `chattr` with `filepath.WalkDir` by @dasomel
- Upgrade `golang.org/x/net` v0.25.0 → v0.45.0 (GO-2026-4441, GO-2026-4440, GO-2025-3595, GO-2025-3503, GO-2024-3333) by @dasomel
- Upgrade `golang.org/x/oauth2` v0.10.0 → v0.27.0 (GO-2025-3488) by @dasomel
- Upgrade `google.golang.org/protobuf` v1.31.0 → v1.33.0 (GO-2024-2611) by @dasomel

### Features

- Add `--nfs-server-path` flag to `ui` subcommand for correct path mapping by @dasomel
- Add project ID collision detection with linear probing in `generateProjectID` by @dasomel
- Add project ID cache (`knownProjectIDs`) to reduce `/etc/projid` reads from N to 1 per sync cycle by @dasomel

### Refactoring

- Replace fixed `time.Sleep` in PV watcher with exponential backoff (1s–60s) and context-aware cancellation by @dasomel
- Extract `queryUnlocked` / `getTrendUnlocked` / `pruneUnlocked` to prevent nested lock acquisition in history store by @dasomel
- Reuse `quota.ReadProjectsFile` in `loadProjects` to eliminate duplicate parsing logic by @dasomel

## [0.2.1] - 2026-02-09

### Documentation

- Add feature guide with screenshots (EN/KO) by @dasomel

### Refactoring

- Reorganize into internal packages and clean up project by @dasomel

## [0.2.0] - 2026-02-08

### Bug Fixes

- Add required volume mounts for xfs_quota operation by @dasomel
- Add CSI NFS PV support for quota sync by @dasomel
- Fix Web UI quota display by parsing /etc/projid and /etc/projects by @dasomel
- Handle long LVM device names in filesystem detection by @dasomel
- Support subDir parameter with capital D in CSI volumeAttributes by @dasomel

### Documentation

- Update CHANGELOG.md for v0.1.3 by @github-actions[bot]
- Update CHANGELOG.md for v0.1.4 by @github-actions[bot]
- Update CHANGELOG.md for v0.1.5 by @github-actions[bot]
- Add required volume mounts section to README by @dasomel
- Update CHANGELOG.md for v0.1.6 by @github-actions[bot]
- Update CHANGELOG.md for v0.1.8 by @github-actions[bot]
- Update README with CSI NFS PV support and bump chart to 0.1.8 by @dasomel
- Update CHANGELOG.md for v0.1.9 by @github-actions[bot]
- Update CHANGELOG.md for v0.1.10 by @github-actions[bot]
- Update CHANGELOG.md for v0.1.11 by @github-actions[bot]
- Update CHANGELOG.md for v0.1.12 by @github-actions[bot]
- Add namespace/PVC name directory pattern example by @dasomel
- Add recommended NFS mount options by @dasomel
- Update CHANGELOG.md for v0.1.12 by @dasomel
- Update CHANGELOG.md for v0.1.15 by @github-actions[bot]
- Update CHANGELOG.md for v0.1.16 by @github-actions[bot]

### Features

- Add web UI as optional flag in run command by @dasomel
- Add PV/PVC status display and refresh button to Web UI by @dasomel
- Add audit logging support with Helm chart option by @dasomel
- Improve Web UI with sorting, nested dirs, and conditional audit tab by @dasomel
- Add actor and provisioner to audit logs by @dasomel

### Miscellaneous

- Bump Helm chart version to 0.1.6 by @dasomel
- Bump version to 0.1.10 by @dasomel
- Bump version to 0.1.11 by @dasomel

### Release

- V0.2.0 by @dasomel

## [0.1.3] - 2026-02-08

### Bug Fixes

- Use xfsprogs-extra for xfs_quota command by @dasomel

### Documentation

- Update CHANGELOG.md for v0.1.2 by @github-actions[bot]

## [0.1.2] - 2026-02-05

### Documentation

- Update CHANGELOG.md for v0.1.1 by @github-actions[bot]

### Features

- Add metrics service configuration to Helm chart by @dasomel

## [0.1.1] - 2026-02-05

### Bug Fixes

- Add util-linux for findmnt command in container by @dasomel

### Documentation

- Update CHANGELOG.md for v0.1.0 by @github-actions[bot]

## [0.1.0] - 2026-02-04

### Bug Fixes

- Update Go version to 1.22 for CI compatibility by @dasomel
- Use stable Go 1.22 and k8s.io/client-go v0.29.0 by @dasomel
- Resolve golangci-lint errors by @dasomel
- Update Go version to 1.22 in CI/CD workflows and Dockerfile by @dasomel
- Upgrade to Go 1.23 and fix remaining lint issues by @dasomel
- Update govulncheck to Go 1.23 and increase lint timeout by @dasomel
- Upgrade to Go 1.24 and update dependencies for security fixes by @dasomel
- Update govulncheck to Go 1.24 by @dasomel
- Add ref tag to Docker image metadata for security scan by @dasomel
- Handle long LVM device names in filesystem detection by @dasomel

### Documentation

- Update CHANGELOG.md for v0.1.0 by @github-actions[bot]
- Update CHANGELOG.md for v0.1.0 by @github-actions[bot]
- Update CHANGELOG.md for v0.1.0 by @github-actions[bot]
- Update CHANGELOG.md for v0.1.0 by @github-actions[bot]
- Update CHANGELOG.md for v0.1.0 by @github-actions[bot]
- Update CHANGELOG.md for v0.1.0 by @github-actions[bot]
- Update CHANGELOG.md for v0.1.0 by @github-actions[bot]
- Update CHANGELOG.md for v0.1.0 by @github-actions[bot]
- Update CHANGELOG.md for v0.1.0 by @github-actions[bot]


