# Research Evidence

NFS Quota Agent follows the OpenForge Research Evidence Collection Standard:
https://github.com/dasomel/openforge/blob/main/docs/research-evidence.md

Collect machine-readable evidence during normal development when practical. Useful evidence includes build/test/E2E duration and results, filesystem/backend enforcement outcomes, install/deploy time, quota apply/verify latency, failure/recovery/retry behavior, runtime resource usage, and agent-assisted attempts/interventions/review corrections/CI retries/final verification. Preserve negative results and distinguish unit/stubbed evidence from real filesystem/kernel E2E.

## Legacy evidence on discovery

During every implementation, bug fix, verification, release, or documentation task, also inspect evidence encountered from earlier work. Existing QA/test results, compatibility records, benchmark outputs, CI results, issue evidence, and `.agents/evals/traces/*.json` are legacy research evidence and should be cataloged rather than rewritten or discarded.

Use `dasomel/openforge#89` as the portfolio-level legacy catalog source of truth. Record source/path, date when known, evidence class/strength, environment scope, metrics/facts, limitations, and likely paper use. Existing agent traces should be classified as agent-assisted engineering plus verification/reliability evidence where applicable. Do not infer missing historical duration, token use, interventions, or other values that were never recorded. Preserve failures and partial results.

## Public-data rule

This is a personal OSS/test project. Public test paths, RFC1918 addresses, local hostnames/domains, filesystem/backend names, Kubernetes object names, and reproducibility-relevant environment details may remain when intentionally part of the test setup.

Never publish credentials, tokens, private keys, kubeconfig credentials, password material, or accidental personal data. Review future third-party/non-public artifacts separately. Validate structured evidence against the OpenForge schema and run secret/pattern checks before publication.