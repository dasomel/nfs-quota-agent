# nfs-quota-agent

@AGENTS.md

> The legacy `.claude/skills/verification` compatibility route makes this adapter intentionally retained for now; remove it only when that Claude-specific compatibility path is retired.

## Project skill routing

Load `.agents/skills/nfs-quota-verification/SKILL.md` when the task changes quota application/verification, filesystem behavior, privileged/RBAC behavior, packaging/install paths, or other user/runtime behavior that requires evidence beyond ordinary local checks. Do not activate it for unrelated documentation or trivial edits.

The old `.claude/skills/verification` entry is a compatibility adapter only; do not add workflow rules there.
