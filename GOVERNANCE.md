# Project Governance

English | [한국어](GOVERNANCE-ko.md)

## Overview

`nfs-quota-agent` is an open-source project licensed under the [Apache License 2.0](LICENSE). The project provides a Kubernetes-native filesystem quota management agent for shared NFS PersistentVolumes.

This document outlines the governance structure for `nfs-quota-agent`. It reflects the project's current state as a young, single-maintainer project while establishing a transparent pathway toward community-driven governance as the contributor base grows.

## Decision-Making Model

### Current State (Maintainer-Led)

`nfs-quota-agent` is currently led by a single maintainer. Current maintainers are listed in [MAINTAINERS.md](MAINTAINERS.md). During this early phase, the maintainer holds final decision-making authority over project architecture, roadmap, security, releases, and contribution acceptance.

### Path to Maintainer Voting

The project intends to evolve toward vendor-neutral, maintainer-voted governance as its contributor base grows. Once `nfs-quota-agent` reaches 3 active maintainers from independent affiliations or backgrounds, governance is expected to transition to a majority-vote model:

* Technical decisions, policy updates, and maintainer additions would be decided by a majority vote of active maintainers.
* Major architectural decisions would be recorded via explicit proposals (GitHub Issues or RFCs) with a formal maintainer vote.

This is a stated intent, not a formal commitment on a fixed timeline — see [CNCF Sandbox Consideration](#future-cncf-sandbox-consideration) below.

## Day-to-Day Operations

Day-to-day operations follow standard open collaboration practices:

* **Lazy Consensus**: Routine decisions, bug fixes, documentation updates, and minor features proceed through Pull Requests (PRs). If a PR receives maintainer approval and no objections after a reasonable review window (typically 48–72 hours), consensus is assumed.
* **Escalation**: Architectural changes, breaking changes, or disputed PRs are escalated to GitHub Issues or Discussions for public review before reaching consensus or maintainer resolution.
* **Review Routing**: [`.github/CODEOWNERS`](.github/CODEOWNERS) mirrors the maintainer list in [MAINTAINERS.md](MAINTAINERS.md) so GitHub requests review from a maintainer automatically on every PR.

## Maintainership

### Becoming a Maintainer

We welcome new maintainers. Maintainers are contributors who demonstrate long-term commitment and responsibility toward the project.

**Criteria**:
* Sustained contributions (code, code reviews, issue triage, documentation, or operational testing) over several months.
* Demonstrated understanding of Kubernetes storage semantics, Linux filesystem quotas (XFS, ext4, btrfs), and daemon security constraints.
* Adherence to the project's [Code of Conduct](CODE_OF_CONDUCT.md) and security guidelines.

**Nomination & Selection**:
1. An existing maintainer nominates a contributor by opening a governance issue or PR.
2. Existing maintainers review the candidate's contributions and vote.
3. Upon unanimous agreement of current maintainers (or majority vote once 3+ maintainers exist) and candidate acceptance, the candidate is added to [MAINTAINERS.md](MAINTAINERS.md).

### Maintainer Responsibilities, Inactivity & Removal

Maintainers participate in PR reviews, issue triage, security advisories, and release preparation.

* **Inactivity**: Maintainers who are inactive for over 6 months may be transitioned to Emeritus status after maintainer outreach. Emeritus maintainers can reactivate their status upon returning to active contribution.
* **Removal**: A maintainer may also be removed for resignation, a serious or repeated breach of the [Code of Conduct](CODE_OF_CONDUCT.md), or a security policy violation. Removal is decided by the remaining active maintainer(s) (majority vote once 3+ maintainers exist), includes revoking repository/registry/CI credentials, and the affected maintainer may respond before the decision is finalized.

## Release Ownership

Releases are owned and managed by project Maintainers.

* **Versioning**: `nfs-quota-agent` follows [Semantic Versioning (SemVer 2.0.0)](https://semver.org/).
* **Automation & Verification**: Release tags trigger GitHub Actions workflows that publish container images, Helm charts, SBOMs, sha256 verification manifests, and compatibility matrices. Maintainers ensure all automated checks pass before finalizing a release.

## Amending Governance

This document may be amended through the standard Pull Request process:
1. Proposed changes are submitted via a PR against `GOVERNANCE.md`.
2. The proposal remains open for public community review for at least 7 days.
3. Approval requires agreement from active maintainers.

## Future CNCF Sandbox Consideration

As outlined in [#81](https://github.com/dasomel/nfs-quota-agent/issues/81), CNCF Sandbox is a long-term readiness goal, not a current application or a claim of present eligibility. Establishing transparent governance and a clear maintainer pathway now is preparatory work toward that goal. Any future decision to formally pursue Sandbox status would itself go through the decision-making model above — as a public proposal, with the maintainer(s) at the time deciding.
