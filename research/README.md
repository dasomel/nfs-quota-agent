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

## Prospective record format

Durable longitudinal records belong in `research/evidence/YYYY-MM.jsonl` and are
append-only. Each line is one observed event using schema version `1.0`:

Illustrative shape only; do not treat these values as a measurement.

```json
{"schema_version":"1.0","timestamp":"2026-09-15T00:00:00Z","repository":"dasomel/nfs-quota-agent","revision":"<immutable git SHA>","event_type":"test","task_or_test":"make test","result":"pass","duration_ms":0,"environment":"<runner or host profile>","attempt":1,"human_interventions":0,"review_corrections":0,"ci_retries":0,"metadata":{"evidence_class":"unit/stub"}}
```

Replace placeholders only with observed values; `duration_ms` must be measured,
never estimated. Preserve failed, partial, cancelled, skipped, and superseded
records alongside successful ones. Limit metadata to documented, safe fields.

Before committing a canonical record, validate its JSON/schema and inspect
free-form fields for credentials or private data. Keep historical artifacts in
their original locations and register them in the portfolio legacy catalog with
source, date, evidence class/strength, environment scope, facts, limitations,
paper use, and public-review state; do not rewrite or backfill missing values.

## First prospective batch (2026-09)

`research/evidence/2026-09.jsonl` seeds the format with real, measured records
from PRs #176–#184: `test`/`build` job durations from `check-runs` on each
merge commit (`stub/unit` evidence class), the real-kernel Air-Gap E2E
`deploy` durations for all three filesystem backends across two of those PRs
(`effective-enforcement` evidence class, per
[`docs/agent-execution-security.md`](../docs/agent-execution-security.md)'s
evidence-class definitions), and `agent_task` records covering PR-open-to-merge
wall time with observed `human_interventions`/`review_corrections`/`ci_retries`
counts. No value in that file was estimated or backfilled from anything other
than `gh api`/`gh run` output.
