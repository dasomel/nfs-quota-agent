#!/usr/bin/env bash
# Self-test for check-third-party-licenses.sh. No network, no real `go`: a
# fake `go` executable placed first on PATH emulates
# `go tool go-licenses report ./... --template hack/third-party-licenses.tmpl`
# under a handful of scenarios (see FAKE_MODE below). See #95.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
CHECK_SCRIPT_SRC="$REPO_ROOT/scripts/ci/check-third-party-licenses.sh"

WORKDIR="$(mktemp -d)"
cleanup() { rm -rf "$WORKDIR"; }
trap cleanup EXIT

PASS=0
FAIL=0

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1"; FAIL=$((FAIL + 1)); }

# Sets up a scratch repo dir at $1 with the real fixture files plus a fake
# `go` on its own PATH-only bin dir. Returns nothing; sets DIR/FAKEBIN.
new_case() {
  DIR="$WORKDIR/$1"
  FAKEBIN="$DIR/fakebin"
  mkdir -p "$DIR/hack" "$FAKEBIN"
  cp "$REPO_ROOT/THIRD_PARTY_LICENSES.md" "$DIR/THIRD_PARTY_LICENSES.md"
  cp "$REPO_ROOT/hack/third-party-licenses.tmpl" "$DIR/hack/third-party-licenses.tmpl"
  cp "$CHECK_SCRIPT_SRC" "$DIR/check-third-party-licenses.sh"
  chmod +x "$DIR/check-third-party-licenses.sh"

  cat >"$FAKEBIN/go" <<'FAKEGO'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == "tool" && "${2:-}" == "go-licenses" && "${3:-}" == "report" ]]; then
  # Guard the exact argv the real Makefile/CI invocation relies on, so a
  # drifted flag in the check script fails here instead of only in CI.
  if [[ "${4:-}" != "./..." || "${5:-}" != "--template" || "${6:-}" != "hack/third-party-licenses.tmpl" ]]; then
    echo "fake go: unexpected go-licenses argv: $*" >&2
    exit 99
  fi
  CONTENT="$FAKE_CONTENT_FILE"
  case "${FAKE_MODE:-clean}" in
    clean)
      cat "$CONTENT"
      exit 0
      ;;
    discovery-failure)
      awk '!seen && /^\| `/ { sub(/\[https:\/\/[^]]*\]\(https:\/\/[^)]*\)/, "[Unknown](Unknown)"); seen=1 } { print }' "$CONTENT"
      echo "W0901 12:00:00.000000       1 report.go:128] Error discovering license URL: getting file URL in library golang.org/x/text: dial tcp: i/o timeout" >&2
      exit 0
      ;;
    fail-n-then-succeed)
      n=0
      [[ -f "$FAKE_COUNT_FILE" ]] && n=$(cat "$FAKE_COUNT_FILE")
      n=$((n + 1))
      echo "$n" >"$FAKE_COUNT_FILE"
      if [[ "$n" -le "$FAKE_FAIL_COUNT" ]]; then
        awk '!seen && /^\| `/ { sub(/\[https:\/\/[^]]*\]\(https:\/\/[^)]*\)/, "[Unknown](Unknown)"); seen=1 } { print }' "$CONTENT"
        echo "W0901 12:00:00.000000       1 report.go:128] Error discovering license URL: getting file URL in library golang.org/x/text: dial tcp: i/o timeout" >&2
        exit 0
      fi
      cat "$CONTENT"
      exit 0
      ;;
    discovery-failure-stderr-only)
      cat "$CONTENT"
      echo "W0901 12:00:00.000000       1 report.go:128] Error discovering license URL: getting file URL in library golang.org/x/text: dial tcp: i/o timeout" >&2
      exit 0
      ;;
    discovery-failure-output-only)
      awk '!seen && /^\| `/ { sub(/\[https:\/\/[^]]*\]\(https:\/\/[^)]*\)/, "[Unknown](Unknown)"); seen=1 } { print }' "$CONTENT"
      exit 0
      ;;
    stale)
      cat "$CONTENT"
      echo '| `example.com/totally-new-module` | v9.9.9 | MIT | [https://example.com/LICENSE](https://example.com/LICENSE) |'
      exit 0
      ;;
    unrelated-error)
      echo "F0901 12:00:00.000000       1 main.go:42] some unrelated go-licenses fatal error" >&2
      exit 3
      ;;
    *)
      echo "fake go: unknown FAKE_MODE '$FAKE_MODE'" >&2
      exit 98
      ;;
  esac
fi
echo "fake go: unhandled args: $*" >&2
exit 99
FAKEGO
  chmod +x "$FAKEBIN/go"
}

# Runs the check script under test with $1 = check|write, capturing combined
# stdout+stderr into OUT and exit code into RC. Relies on FAKE_MODE and any
# GO_LICENSES_* / FAKE_* env vars already exported by the caller.
run_script() {
  set +e
  OUT="$(cd "$DIR" && PATH="$FAKEBIN:$PATH" GO_LICENSES_RETRY_DELAY="${GO_LICENSES_RETRY_DELAY:-0}" \
    FAKE_CONTENT_FILE="$DIR/THIRD_PARTY_LICENSES.md" \
    ./check-third-party-licenses.sh "$1" 2>&1)"
  RC=$?
  set -e
}

# --- Case: clean run -> exit 0 ---
new_case clean
FAKE_MODE=clean run_script check
if [[ "$RC" -eq 0 ]]; then pass "clean: exit 0"; else fail "clean: expected exit 0, got $RC ($OUT)"; fi

# --- Case: discovery failure every attempt, attempts=2 -> exit 2 ---
new_case discovery-failure-always
FAKE_MODE=discovery-failure GO_LICENSES_ATTEMPTS=2 run_script check
if [[ "$RC" -eq 2 ]]; then pass "discovery-failure: exit 2"; else fail "discovery-failure: expected exit 2, got $RC ($OUT)"; fi
if echo "$OUT" | grep -q "not a stale"; then pass "discovery-failure: mentions 'not a stale'"; else fail "discovery-failure: missing 'not a stale' ($OUT)"; fi
if echo "$OUT" | grep -q "is stale"; then fail "discovery-failure: unexpectedly mentions 'is stale' ($OUT)"; else pass "discovery-failure: does not mention 'is stale'"; fi

# --- Case: each discovery signal alone is sufficient (guards the OR in discovery_failed) ---
new_case discovery-failure-stderr-only
FAKE_MODE=discovery-failure-stderr-only GO_LICENSES_ATTEMPTS=1 run_script check
if [[ "$RC" -eq 2 ]]; then pass "discovery-failure-stderr-only: exit 2"; else fail "discovery-failure-stderr-only: expected exit 2, got $RC ($OUT)"; fi

new_case discovery-failure-output-only
FAKE_MODE=discovery-failure-output-only GO_LICENSES_ATTEMPTS=1 run_script check
if [[ "$RC" -eq 2 ]]; then pass "discovery-failure-output-only: exit 2"; else fail "discovery-failure-output-only: expected exit 2, got $RC ($OUT)"; fi

# --- Case: GO_LICENSES_ATTEMPTS must be a positive integer -> exit 64, no go-licenses run ---
new_case invalid-attempts
FAKE_MODE=unrelated-error GO_LICENSES_ATTEMPTS=0 run_script check
if [[ "$RC" -eq 64 ]]; then pass "invalid-attempts: exit 64"; else fail "invalid-attempts: expected exit 64, got $RC ($OUT)"; fi
if echo "$OUT" | grep -q "positive integer"; then pass "invalid-attempts: names the misconfiguration"; else fail "invalid-attempts: missing 'positive integer' ($OUT)"; fi

# --- Case: fails twice then succeeds, attempts=3 -> exit 0, exactly two warnings ---
new_case fail-twice-then-succeed
COUNT_FILE="$DIR/attempt-count"
FAKE_MODE=fail-n-then-succeed FAKE_FAIL_COUNT=2 FAKE_COUNT_FILE="$COUNT_FILE" GO_LICENSES_ATTEMPTS=3 run_script check
if [[ "$RC" -eq 0 ]]; then pass "fail-twice-then-succeed: exit 0"; else fail "fail-twice-then-succeed: expected exit 0, got $RC ($OUT)"; fi
WARN_COUNT="$(echo "$OUT" | grep -c "::warning::" || true)"
if [[ "$WARN_COUNT" -eq 2 ]]; then pass "fail-twice-then-succeed: exactly two ::warning:: lines"; else fail "fail-twice-then-succeed: expected 2 warnings, got $WARN_COUNT ($OUT)"; fi

# --- Case: stale (genuinely different) -> exit 1, "is stale" ---
new_case stale
FAKE_MODE=stale run_script check
if [[ "$RC" -eq 1 ]]; then pass "stale: exit 1"; else fail "stale: expected exit 1, got $RC ($OUT)"; fi
if echo "$OUT" | grep -q "is stale"; then pass "stale: mentions 'is stale'"; else fail "stale: missing 'is stale' ($OUT)"; fi

# --- Case: unrelated go-licenses error -> exit 3 ---
new_case unrelated-error
FAKE_MODE=unrelated-error run_script check
if [[ "$RC" -eq 3 ]]; then pass "unrelated-error: exit 3"; else fail "unrelated-error: expected exit 3, got $RC ($OUT)"; fi

# --- Case: write mode on clean run rewrites file identically ---
new_case write-clean
BEFORE_SUM="$(shasum -a 256 "$DIR/THIRD_PARTY_LICENSES.md" | awk '{print $1}')"
FAKE_MODE=clean run_script write
AFTER_SUM="$(shasum -a 256 "$DIR/THIRD_PARTY_LICENSES.md" | awk '{print $1}')"
if [[ "$RC" -eq 0 && "$BEFORE_SUM" == "$AFTER_SUM" ]]; then
  pass "write-clean: exit 0 and file unchanged (identical rewrite)"
else
  fail "write-clean: expected exit 0 and identical content, got rc=$RC before=$BEFORE_SUM after=$AFTER_SUM ($OUT)"
fi

# --- Case: write mode on discovery failure leaves file untouched ---
new_case write-discovery-failure
BEFORE_SUM="$(shasum -a 256 "$DIR/THIRD_PARTY_LICENSES.md" | awk '{print $1}')"
FAKE_MODE=discovery-failure GO_LICENSES_ATTEMPTS=1 run_script write
AFTER_SUM="$(shasum -a 256 "$DIR/THIRD_PARTY_LICENSES.md" | awk '{print $1}')"
if [[ "$RC" -eq 2 && "$BEFORE_SUM" == "$AFTER_SUM" ]]; then
  pass "write-discovery-failure: exit 2 and file untouched"
else
  fail "write-discovery-failure: expected exit 2 and untouched file, got rc=$RC before=$BEFORE_SUM after=$AFTER_SUM ($OUT)"
fi

echo
echo "== $PASS passed, $FAIL failed =="
[[ "$FAIL" -eq 0 ]]
