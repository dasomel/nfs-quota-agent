---
name: verification
description: Legacy Claude adapter for nfs-quota-agent verification. Use only when an existing Claude workflow invokes `verification`; load the canonical `nfs-quota-verification` skill instead for all verification work.
license: Apache-2.0
compatibility: Claude adapter; canonical workflow is repository-local under .agents/skills/nfs-quota-verification/.
metadata:
  openforge-scope: project
  openforge-owner: dasomel/nfs-quota-agent
  openforge-maturity: deprecated
  openforge-version: "2"
---

# Legacy verification adapter

The canonical workflow is:

`../../../.agents/skills/nfs-quota-verification/SKILL.md`

Read and follow that file. Do not maintain a second copy of the verification procedure here.
