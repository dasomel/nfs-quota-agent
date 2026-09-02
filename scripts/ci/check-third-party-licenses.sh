#!/usr/bin/env bash
# Verifies (or regenerates) THIRD_PARTY_LICENSES.md via go-licenses, retrying
# when go-licenses fails to discover a module's license URL -- a symptom of
# its http:// fallback for golang.org/x/* being blocked by CI egress policy,
# not of a stale committed file. See #95.
set -euo pipefail

MODE="${1:-}"
if [[ "$MODE" != "check" && "$MODE" != "write" ]]; then
  echo "Usage: $0 check|write" >&2
  exit 64
fi

ATTEMPTS="${GO_LICENSES_ATTEMPTS:-3}"
RETRY_DELAY="${GO_LICENSES_RETRY_DELAY:-5}"

OUT_FILE="$(mktemp)"
ERR_FILE="$(mktemp)"
trap 'rm -f "$OUT_FILE" "$ERR_FILE"' EXIT

discovery_failed() {
  grep -q "Error discovering license URL" "$ERR_FILE" || grep -q '\[Unknown\](Unknown)' "$OUT_FILE"
}

success=0
attempt=1
while [[ "$attempt" -le "$ATTEMPTS" ]]; do
  set +e
  go tool go-licenses report ./... --template hack/third-party-licenses.tmpl >"$OUT_FILE" 2>"$ERR_FILE"
  rc=$?
  set -e
  cat "$ERR_FILE" >&2

  if discovery_failed; then
    offending="$(grep "Error discovering license URL" "$ERR_FILE" || true)"
    echo "::warning::go-licenses attempt $attempt/$ATTEMPTS failed to discover a license URL (likely transient egress, not a stale file): ${offending:-[Unknown](Unknown) present in generated output}"
    if [[ "$attempt" -lt "$ATTEMPTS" ]]; then
      sleep "$RETRY_DELAY"
    fi
    attempt=$((attempt + 1))
    continue
  fi

  if [[ "$rc" -ne 0 ]]; then
    echo "::error::go-licenses exited $rc for a reason other than license-URL discovery; see stderr above" >&2
    exit "$rc"
  fi

  success=1
  break
done

if [[ "$success" -ne 1 ]]; then
  echo "::error::go-licenses could not resolve a license URL after $ATTEMPTS attempts (see 'Error discovering license URL' above). Either the runner's egress blocked the lookup (a transient https failure falls back to http://:80, which the allowlist refuses) or the module named above has no resolvable source URL at all. Either way this is not a stale THIRD_PARTY_LICENSES.md — do NOT run 'make license' to fix it; check the named module first." >&2
  exit 2
fi

if [[ "$MODE" == "write" ]]; then
  cp "$OUT_FILE" THIRD_PARTY_LICENSES.md.tmp
  mv THIRD_PARTY_LICENSES.md.tmp THIRD_PARTY_LICENSES.md
  echo "THIRD_PARTY_LICENSES.md written."
  exit 0
fi

if diff -u THIRD_PARTY_LICENSES.md "$OUT_FILE"; then
  echo "THIRD_PARTY_LICENSES.md is up to date."
  exit 0
fi
echo "::error::THIRD_PARTY_LICENSES.md is stale — run 'make license' and commit the result" >&2
exit 1
