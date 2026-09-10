# OpenForge adoption

Follow the canonical OpenForge standards:
- https://github.com/dasomel/openforge/blob/main/docs/model-agnostic-agent-instructions.md
- https://github.com/dasomel/openforge/blob/main/docs/agent-engineering.md
- https://github.com/dasomel/openforge/blob/main/docs/user-centric-validation.md

Keep filesystem/kernel/quota/security invariants local. Model/tool files are thin adapters. For quota enforcement, host mutation, RBAC, install/configuration, upgrade, and UI/API changes, use risk-proportional validation and distinguish stubbed CommandRunner evidence from real prjquota-mounted filesystem behavior. Confirmed user-visible defects become regression evidence.

Safe local/disposable work within scope may proceed autonomously. Production/shared mutation, destructive external actions, release/publish, credential/permission widening, or unrelated external mutation requires explicit authorization unless already granted.
