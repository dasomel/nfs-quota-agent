# Tasks: <change title>

Link this checklist to the related Change Package. Every implementation task SHOULD name the requirement or acceptance scenario it advances.

## Inspect and establish evidence

- [ ] `T-001` (`REQ-___`) Confirm the source of truth and current behavior.
- [ ] `T-002` (`AC-___`) Reproduce the problem or capture the pre-change baseline.
- [ ] `T-003` Review dependencies, affected workflows and downstream consumers.

## Implement

- [ ] `T-010` (`REQ-___`) ...
- [ ] `T-011` (`REQ-___`) ...

## Verify

- [ ] `T-020` (`AC-___`) Run the planned unit/static checks.
- [ ] `T-021` (`AC-___`) Run the planned integration/runtime/user-journey checks.
- [ ] `T-022` Capture failures, successes, environment and commands as evidence.
- [ ] `T-023` Convert discovered regression risks into durable checks where practical.

## Synchronize durable truth

- [ ] `T-030` Update normative documentation and operational guidance.
- [ ] `T-031` Update ADR or design references when required.
- [ ] `T-032` Update release, migration, rollback and compatibility notes.
- [ ] `T-033` Review portfolio/downstream impact and publish verified status where required.

## Completion review

- [ ] Every requirement maps to an acceptance scenario and verification result.
- [ ] Material scope changes were reflected in the Change Package and re-reviewed.
- [ ] Expected evidence is attached or linked.
- [ ] Known incomplete work has an owner and tracking issue.
- [ ] The PR states the checks actually run and any important unverified path.
