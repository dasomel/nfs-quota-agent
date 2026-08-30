#!/usr/bin/env bash
set -euo pipefail

IMAGE="${IMAGE:-nfs-quota-agent:agent-eval}"

echo "=== Build runtime image ==="
docker build -t "$IMAGE" .

echo "=== Verify filesystem runtime commands ==="
docker run --rm --entrypoint /bin/sh "$IMAGE" -ec '
  for cmd in xfs_quota setquota chattr findmnt btrfs; do
    printf "%-12s" "$cmd"
    if command -v "$cmd" >/dev/null 2>&1; then
      echo "OK ($(command -v "$cmd"))"
    else
      echo "MISSING"
      exit 1
    fi
  done
'
