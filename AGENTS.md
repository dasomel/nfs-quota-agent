# AGENTS.md

Read `CLAUDE.md` first. It contains the repository-specific quota, filesystem, concurrency, packaging, and high-risk-path gotchas that generic instructions cannot replace.

Also read `README.md`, `CONTRIBUTING.md`, `DESIGN.md`, and `make help` before editing.

## Work contract

- Make the smallest coherent change that solves the requested problem.
- Do not auto-fix unrelated findings; report them separately.
- Preserve package boundaries and existing access restrictions.
- Treat quota argv construction, validation, project-ID mapping, host-file writes, privileged/RBAC changes, filesystem semantics, unit conversion, and exported API changes as high-risk design changes.
- Let Go formatting/static-analysis rules own deterministic style.
- Comments explain why, invariants, hazards, filesystem semantics, or compatibility constraints; do not narrate obvious code.

## Bugs

Prefer: reproduce -> failing regression test/evidence -> minimal fix -> same test passes -> relevant regression suite.

A green stubbed test suite does not prove real quota enforcement. Distinguish mocked command-runner evidence from verification on a real quota-enabled filesystem and host.

## Verification

Do not claim completion without stating exactly which checks ran and which evidence class they provide.

## Convergence

End substantive work as A) complete/verified, B) meaningful verified progress with the next blocker isolated, or C) stop with evidence when further work requires unjustified scope, fragile patches, unsupported assumptions, or unacceptable risk.

Do not keep patching when the work is no longer converging.

Reference: https://github.com/dasomel/openforge/blob/main/docs/agent-engineering.md
