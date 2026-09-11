# Research Evidence

NFS Quota Agent follows the OpenForge Research Evidence Collection Standard:
https://github.com/dasomel/openforge/blob/main/docs/research-evidence.md

Collect sanitized machine-readable evidence during normal development when practical. Useful project evidence includes build/test/E2E duration and results, filesystem/backend enforcement outcomes, install/deploy time, quota apply/verify latency, failure/recovery/retry behavior, runtime resource usage where relevant, and agent-assisted attempts, human interventions, review corrections, CI retries, and final verification.

Preserve negative results and backend-specific failures. Record the evidence class (unit/stubbed vs real filesystem/kernel E2E) so later analysis does not treat them as equivalent.

## Public-data rule

Only sanitized records may be committed publicly. Never publish credentials, private URLs/IPs/hostnames, real server paths tied to users/customers, personal/customer/employer data, raw LDAP/Kubernetes/host dumps, confidential prompts/source, arbitrary environment dumps, or security-sensitive host details. Raw CI logs, mount output, kernel logs, security scans, and prompts are sensitive-by-default.

Before public storage: validate against the OpenForge schema, run secret/pattern checks, normalize environment labels, review free-form fields, and publish aggregate or categorized measurements when raw artifacts cannot be proven safe.
