# Security Policy

English | [한국어](SECURITY-ko.md)

## Supported Versions

| Version | Supported          |
| ------- | ------------------ |
| v0.x    | :white_check_mark: |

## Security Model & Privileges

`nfs-quota-agent` operates as a node-level daemon with privileges required to query directory usage (`xfs_quota` / `quotactl` / directory walk) and enforce quota allocations on mounted NFS storage volumes.

- Daemon endpoints must run behind mTLS or Kubernetes RBAC authentication.
- Quota manipulation requests must validate volume boundary isolation to prevent path traversal.
- Never log plaintext credentials or volume authentication keys.

## Reporting a Vulnerability

Please report security issues responsibly. Do NOT create public GitHub issues for security vulnerabilities.
Use GitHub Private Vulnerability Reporting or contact the maintainers directly. Acknowledgement will be provided within 48 hours.

Reference: [OpenForge Security Standard](https://github.com/dasomel/openforge/blob/main/docs/security.md)
