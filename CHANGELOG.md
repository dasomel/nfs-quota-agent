# Changelog

All notable changes to this project will be documented in this file.

## [0.1.1] - 2026-02-05

### Bug Fixes

- Add util-linux package to container for findmnt command (fixes mount options check warning)

## [0.1.0] - 2026-02-04

### Bug Fixes

- Handle long LVM device names in filesystem detection (use findmnt with df -T fallback)
- Update Go version to 1.22 for CI compatibility by @dasomel
- Use stable Go 1.22 and k8s.io/client-go v0.29.0 by @dasomel
- Resolve golangci-lint errors by @dasomel
- Update Go version to 1.22 in CI/CD workflows and Dockerfile by @dasomel
- Upgrade to Go 1.23 and fix remaining lint issues by @dasomel
- Update govulncheck to Go 1.23 and increase lint timeout by @dasomel
- Upgrade to Go 1.24 and update dependencies for security fixes by @dasomel
- Update govulncheck to Go 1.24 by @dasomel
- Add ref tag to Docker image metadata for security scan by @dasomel

