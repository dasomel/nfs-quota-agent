# Changelog

All notable changes to this project will be documented in this file.

## [0.2.0] - 2026-02-09

### Features

- Add automatic orphan directory cleanup with configurable grace period by @dasomel
  - Immediate orphan deletion with confirmation dialog
  - Delete button shows only in live mode (not dry-run)
- Add usage history and trend tracking with local JSON storage by @dasomel
- Add namespace quota policy with Kubernetes LimitRange/ResourceQuota integration by @dasomel
  - Priority: LimitRange > Namespace Annotation > Global Default
  - Support min/max/default from LimitRange for PersistentVolumeClaim
  - Display ResourceQuota usage in Policies tab
- Add Orphans, Trends, and Policies tabs to Web UI by @dasomel
- Add expandable file browser in Web UI by @dasomel
  - Click on quota or orphan rows to view directory contents
  - Shows file/folder names with sizes
- Add new API endpoints: /api/orphans, /api/history, /api/trends, /api/policies, /api/violations, /api/files by @dasomel

### Improvements

- Remove actor and provisioner fields from audit log by @dasomel

### Helm Chart

- Add cleanup configuration (interval, gracePeriod, dryRun) by @dasomel
- Add history configuration (path, interval, retention) by @dasomel
- Add policy configuration (defaultQuota, enforceMaxQuota) by @dasomel

## [0.1.3] - 2026-02-08

This release adds Web UI and audit logging support.

### Features

- Add Web UI with optional --enable-ui flag by @dasomel
  - Quota status dashboard with usage visualization
  - PV/PVC binding status display
  - Table sorting and nested directory support
  - Refresh button for real-time updates
- Add audit logging support with Helm chart option by @dasomel
  - Conditional audit tab in Web UI

### Bug Fixes

- Support subDir parameter with capital D in CSI volumeAttributes by @dasomel
- Handle long LVM device names in filesystem detection by @dasomel
- Fix Web UI quota display by parsing /etc/projid and /etc/projects by @dasomel
- Add CSI NFS PV support for quota sync by @dasomel
- Add required volume mounts for xfs_quota operation by @dasomel
- Use xfsprogs-extra for xfs_quota command by @dasomel

### Documentation

- Add namespace/PVC name directory pattern example by @dasomel
- Add recommended NFS mount options by @dasomel
- Update README with CSI NFS PV support by @dasomel
- Add required volume mounts section to README by @dasomel

## [0.1.2] - 2026-02-05

### Features

- Add metrics service configuration to Helm chart by @dasomel

## [0.1.1] - 2026-02-05

### Bug Fixes

- Add util-linux for findmnt command in container by @dasomel

## [0.1.0] - 2026-02-04

Initial release of NFS Quota Agent.

### Features

- XFS project quota management for NFS storage
- Kubernetes PV/PVC integration
- Prometheus metrics endpoint
- CLI commands: run, sync, list, cleanup

### Bug Fixes

- Update Go version to 1.24 for CI compatibility by @dasomel
- Resolve golangci-lint errors by @dasomel
- Add ref tag to Docker image metadata for security scan by @dasomel
