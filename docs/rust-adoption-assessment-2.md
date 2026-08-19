# Rust Adoption Assessment

Keep Go as the primary controller language. Evaluate Rust only for isolated filesystem/security helpers and offline artifact verification.

Candidates: privileged filesystem/quota helper; path/project-id parsing; artifact checksum/signature/SBOM verifier.

Do not rewrite Kubernetes watch/reconcile, Helm, or Prometheus layers unless benchmark evidence shows a material benefit.

Required proof: XFS/ext4/Btrfs parity, benchmark, offline reproducible build, SBOM/provenance, rollback to existing Go implementation.
