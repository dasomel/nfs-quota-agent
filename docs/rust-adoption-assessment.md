# Rust Adoption Assessment

The agent should remain Go-first. Do not rewrite the controller or Kubernetes client stack merely to use Rust.

## Candidate Rust boundaries

### Medium
- privileged filesystem/quota helper for path validation, project-id parsing and filesystem metadata handling
- offline artifact/signature/SBOM verifier used by release tooling

### Low / keep Go
- Kubernetes watch/reconcile loop
- Helm integration
- Prometheus HTTP exposure
- existing quota command orchestration unless benchmarks show a real bottleneck

## Rationale

The highest-value Rust boundary is the security-sensitive filesystem helper, because memory-safe path handling and explicit data types can reduce classes of bugs while keeping the existing Go controller architecture. A helper should first be exposed through a narrow JSON/CLI contract rather than replacing the agent.

## Exit criteria

- benchmark against current Go implementation
- no regression across XFS/ext4/Btrfs behavior
- offline reproducible build
- SBOM/license/provenance
- rollback to Go helper
