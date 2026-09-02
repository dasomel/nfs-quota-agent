#!/usr/bin/env bash
# Self-test for check-egress-block.sh. No real CI egress dependency: probe
# classification is driven with a fake `curl` on PATH (deterministic, no
# sockets), and the script's own `selftest` mode is additionally run for
# real (real curl, a real loopback python3 http.server) since that mode is
# exactly what proves the script's detection logic without needing a
# harden-runner-blocked environment. See #26.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$SCRIPT_DIR/check-egress-block.sh"

WORKDIR="$(mktemp -d)"
cleanup() { rm -rf "$WORKDIR"; }
trap cleanup EXIT

PASS=0
FAIL=0
pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1"; FAIL=$((FAIL + 1)); }

# Writes a fake `curl` to $1/curl that inspects the trailing URL argument
# and prints the http_code body plus exits with the rc for the resolved
# mode. Mirrors the real curl invocation this script makes: `curl -sS -o
# /dev/null -w '%{http_code}' --connect-timeout N --max-time N URL`.
#
# Mode resolution, per URL slug:
#   FAKE_CURL_URL_<slug>_SEQ set   -> comma-separated modes, one per call
#                                     to this URL in order (state kept in
#                                     $FAKE_CURL_STATE_DIR/<slug>.count),
#                                     clamped to the last entry once the
#                                     sequence is exhausted. Simulates a
#                                     flaky destination across retries.
#   FAKE_CURL_URL_<slug> set       -> that fixed mode, every call.
#   neither set                    -> FAKE_CURL_MODE (default "ok").
write_fake_curl() {
  local bin="$1"
  cat >"$bin/curl" <<'FAKECURL'
#!/usr/bin/env bash
set -euo pipefail
url="${@: -1}"
slug="$(printf '%s' "$url" | tr -c 'A-Za-z0-9' '_')"
seqvar="FAKE_CURL_URL_${slug}_SEQ"
var="FAKE_CURL_URL_${slug}"
if [[ -n "${!seqvar:-}" ]]; then
  statedir="${FAKE_CURL_STATE_DIR:?FAKE_CURL_STATE_DIR must be set to use a _SEQ mode}"
  countfile="$statedir/${slug}.count"
  n=0
  [[ -f "$countfile" ]] && n="$(cat "$countfile")"
  echo $((n + 1)) >"$countfile"
  IFS=',' read -r -a seq <<<"${!seqvar}"
  idx="$n"
  [[ "$idx" -ge "${#seq[@]}" ]] && idx=$((${#seq[@]} - 1))
  mode="${seq[$idx]}"
else
  mode="${!var:-${FAKE_CURL_MODE:-ok}}"
fi
case "$mode" in
  ok) echo -n "200"; exit 0 ;;
  dns) exit 6 ;;
  refused) exit 7 ;;
  timeout) exit 28 ;;
  tls) exit 35 ;;
  empty) exit 52 ;;
  weird) exit 99 ;;
  *) echo "fake curl: unknown FAKE_CURL mode '$mode'" >&2; exit 99 ;;
esac
FAKECURL
  chmod +x "$bin/curl"
}

# --- probe() classification, exhaustive over the fake curl's modes ---
FAKEBIN="$WORKDIR/fakebin"
mkdir -p "$FAKEBIN"
write_fake_curl "$FAKEBIN"

run_probe() {
  # $1 = FAKE_CURL_MODE, $2 = expected classification
  local got
  got="$(PATH="$FAKEBIN:$PATH" FAKE_CURL_MODE="$1" "$SCRIPT" probe "http://probe.example.invalid/" 1)"
  if [[ "$got" == "$2" ]]; then
    pass "probe() classifies curl exit for mode '$1' as $2"
  else
    fail "probe() classifies curl exit for mode '$1' as $2 (got: $got)"
  fi
}
run_probe ok "http:200"
run_probe dns "dns_failure"
run_probe refused "connection_refused"
run_probe timeout "timeout"
run_probe tls "tls_failure"
run_probe empty "empty_reply"
run_probe weird "curl_error_99"

# --- run_ci(), driven per-URL so positive/negative controls differ ---
# The loopback sanity probe hits a real 127.0.0.1:<port> URL that the fake
# curl does not special-case, so it falls back to FAKE_CURL_MODE (kept
# "ok" in every case below) -- only the allowed/blocked URLs are steered
# per-slug via FAKE_CURL_URL_<slug> (fixed mode) or FAKE_CURL_URL_<slug>_SEQ
# (one mode per call, for exercising the positive control's retry).
STATEDIR="$WORKDIR/state"
run_ci_case() {
  local name="$1" allowed_mode_var="$2" allowed_mode_val="$3" blocked_mode="$4" expect_rc="$5" expect_grep="$6" extra_grep="${7:-}"
  local out rc
  rm -rf "$STATEDIR"
  mkdir -p "$STATEDIR"
  # env, not a shell assignment prefix: the variable NAME here is itself a
  # runtime value ($allowed_mode_var), and bash only recognizes "NAME=val"
  # as an assignment word when NAME is a literal identifier in the source,
  # not the result of an expansion -- env parses its argument as NAME=val
  # regardless.
  out="$(PATH="$FAKEBIN:$PATH" \
    FAKE_CURL_MODE=ok \
    FAKE_CURL_STATE_DIR="$STATEDIR" \
    EGRESS_ALLOWED_URL="https://github.com" \
    EGRESS_BLOCKED_URL="https://example.com" \
    FAKE_CURL_URL_https___example_com="$blocked_mode" \
    env "${allowed_mode_var}=${allowed_mode_val}" \
    "$SCRIPT" ci 2>&1)" && rc=0 || rc=$?
  if [[ "$rc" -ne "$expect_rc" ]]; then
    fail "$name: expected exit $expect_rc, got $rc. Output:\n$out"
    return
  fi
  if ! grep -q "$expect_grep" <<<"$out"; then
    fail "$name: expected output to contain '$expect_grep'. Output:\n$out"
    return
  fi
  if [[ -n "$extra_grep" ]] && ! grep -q "$extra_grep" <<<"$out"; then
    fail "$name: expected output to also contain '$extra_grep' (retry attempts were logged). Output:\n$out"
    return
  fi
  pass "$name"
}

run_ci_case "both controls correct" FAKE_CURL_URL_https___github_com ok dns 0 "egress-check RESULT: PASS"
run_ci_case "negative control reachable (block not in effect)" FAKE_CURL_URL_https___github_com ok ok 1 "NEGATIVE CONTROL FAILED"
run_ci_case "positive control unreachable after all retry attempts" FAKE_CURL_URL_https___github_com dns dns 1 "POSITIVE CONTROL FAILED" "positive-control: attempt 3/3"
run_ci_case "negative control gives an ambiguous result" FAKE_CURL_URL_https___github_com ok weird 1 "unrecognized result"
run_ci_case "positive control fails twice then succeeds -> overall PASS" FAKE_CURL_URL_https___github_com_SEQ "dns,dns,ok" dns 0 "egress-check RESULT: PASS" "positive-control: attempt 3/3 target=https://github.com result=http:200"

# --- the script's own selftest mode, run for real: no faked curl, real
# loopback listener. This is the mode CLAUDE.md's design calls for --
# runnable anywhere, egress-blocked or not, verifying the same
# probe()/is_reachable()/is_blocked() logic exercised above end to end. ---
if out="$("$SCRIPT" selftest 2>&1)"; then
  if grep -q "^egress-check selftest: 6 passed, 0 failed$" <<<"$out"; then
    pass "real selftest mode passes all its internal assertions"
  else
    fail "real selftest mode exited 0 but assertion count line was unexpected. Output:\n$out"
  fi
else
  fail "real selftest mode exited non-zero. Output:\n$out"
fi

echo ""
echo "$PASS passed, $FAIL failed"
[[ "$FAIL" -eq 0 ]]
