# Change Package Workflow

Use this portable workflow for Class C, Class D, and complex/cross-component or operationally risky Class B changes.

## Risk-scale the work

- Class A: documentation-only; Issue/PR is sufficient.
- Class B: internal behavior; acceptance criteria are required. Use a package when complexity, component boundaries, or operational risk justify it.
- Class C: dependency, runtime, toolchain, package-manager, CI action, generator, or build-contract change. A reviewed Change Package is required before broad implementation.
- Class D: release, deployment, produced-artifact, permission, credential, security-control, migration, or other high-blast-radius boundary change. A reviewed Change Package is required before broad implementation; use an ADR when the repository's decision threshold is crossed.

## Build the change-scoped contract

For work requiring a package, create or maintain the package in the Issue/PR or use `templates/change/CHANGE.md` and `templates/change/TASKS.md` on the working branch.

Before broad implementation, make these explicit:

1. problem and intended outcome;
2. scope and non-goals;
3. stable requirement IDs such as `REQ-001`;
4. acceptance scenarios such as `AC-001`, preferably Given/When/Then;
5. architecture decisions and ADR threshold;
6. impact across source/API, dependencies, runtime/toolchain, CI/CD, release/packaging, generated output, security/supply chain, offline assets, docs/operations, and downstream consumers;
7. tasks mapped to requirements/acceptance criteria;
8. verification methods, environments, and expected evidence;
9. rollout, rollback, recovery, migration, and compatibility obligations where relevant.

Use `N/A — <reason>` instead of silently omitting an impact area.

## Review gate

The accountable maintainer must accept Class C/D scope, requirements, boundaries, and verification approach before broad implementation. Exploration, reproduction, and reversible prototypes may happen earlier, but must not silently establish the final contract or mutate production/shared environments.

If material scope or requirements change during implementation, update the package and re-review before continuing the affected work.

## Implement and verify

Implement the smallest coherent tasks against the accepted contract. Keep requirement → acceptance scenario → task → evidence traceability.

Verification must distinguish static/unit evidence from integration, runtime, live-system, and user-journey evidence. Do not claim a stronger evidence class than was actually observed.

Convert newly discovered regression risks into deterministic checks where practical.

## Complete without creating specification drift

At completion, synchronize durable truth to the artifact that owns it:

- behavior/invariants → code and tests;
- current operating contract → normative documentation;
- durable rationale → ADR;
- measured result → evidence record;
- cross-project state → portfolio/status metadata;
- change intent/history → Issue, PR, and Git history.

Do not create a parallel long-lived specification tree that duplicates code, tests, documentation, or ADRs. Temporary Change Package files should normally be removed before merge unless they remain useful operational documentation.
