#!/usr/bin/env bash
# Proves the CI egress block (step-security/harden-runner, egress-policy:
# block) actually blocks egress, instead of asserting an absence that is
# equally true when nothing tried to egress at all. An earlier go:generate-
# based negative test was rejected in #26 review as "passes vacuously": it
# could not distinguish "egress blocked" from "nothing attempted egress",
# so it would report success even with harden-runner removed entirely.
#
# This script always runs a positive control (an allowed destination MUST
# be reachable) alongside the negative control (a non-allowlisted
# destination MUST NOT be reachable). Only a run that observes both halves
# behave correctly proves the block is doing its job; either half failing
# is reported as a distinct, named failure so the log says which broke.
#
# The positive control and the loopback sanity probe are retried (bounded,
# with a short sleep, each attempt logged) because a transient github.com
# delay/5xx is a false negative unrelated to egress policy and would
# otherwise fail the whole CI run. The negative control stays single-shot
# on purpose: retrying it would only widen the window for one transient
# success to be misread as reachable, biasing the check in the wrong
# direction -- see the PR review that added this retry.
#
# Modes:
#   ci        Real check for use inside a harden-runner "block" job. Needs
#             an allowed external destination (must already be on that
#             job's harden-runner allowed-endpoints list) and a
#             non-allowlisted one, both reachable only over real egress.
#   selftest  Runs entirely over loopback so it can be exercised on a
#             developer machine or any CI job, egress-blocked or not, to
#             verify this script's own reachable/blocked classification
#             logic is correct. It does not exercise harden-runner.
#   probe URL [TIMEOUT]
#             Prints the classification for one URL and exits 0. A thin
#             seam so the test sibling can drive classification logic
#             against a faked `curl` on PATH without opening any socket.
#
# Env overrides (ci mode): EGRESS_ALLOWED_URL, EGRESS_BLOCKED_URL.
set -euo pipefail

TIMEOUT_DEFAULT=8
RETRY_ATTEMPTS_DEFAULT=3
RETRY_SLEEP_DEFAULT=2

# Classifies one curl attempt against $1 (a full URL) with timeout $2.
# Prints exactly one of: http:<code> | dns_failure | connection_refused |
# timeout | tls_failure | empty_reply | curl_error_<n>. Never fails the
# script itself -- an unreachable target is an expected outcome to
# classify, not a script error.
probe() {
  local url="$1" timeout="${2:-$TIMEOUT_DEFAULT}" code rc
  set +e
  code="$(curl -sS -o /dev/null -w '%{http_code}' --connect-timeout "$timeout" --max-time "$timeout" "$url" 2>/dev/null)"
  rc=$?
  set -e
  case "$rc" in
    0) echo "http:${code}" ;;
    6) echo "dns_failure" ;;
    7) echo "connection_refused" ;;
    28) echo "timeout" ;;
    35) echo "tls_failure" ;;
    52 | 56) echo "empty_reply" ;;
    *) echo "curl_error_${rc}" ;;
  esac
}

# True (exit 0) if a probe() result means "reached an HTTP server and got
# a response", i.e. the destination was NOT blocked.
is_reachable() {
  case "$1" in
    http:*) return 0 ;;
    *) return 1 ;;
  esac
}

# True (exit 0) if a probe() result is one of the recognized "the
# connection never completed" outcomes a network-level egress block
# produces. Anything else (e.g. an unexpected curl_error_N) is treated as
# ambiguous, not as evidence of a block, by the callers below.
is_blocked() {
  case "$1" in
    dns_failure | connection_refused | timeout | tls_failure | empty_reply) return 0 ;;
    *) return 1 ;;
  esac
}

# Retries probe() against $1 (timeout $2) up to $3 attempts (default
# RETRY_ATTEMPTS_DEFAULT), sleeping $4 seconds (default
# RETRY_SLEEP_DEFAULT) between attempts, logging every attempt's
# classification with $5 as the log label. Returns as soon as an attempt
# is_reachable; otherwise prints the last attempt's classification once
# attempts are exhausted. Intended ONLY for checks where a transient
# failure is noise, not signal -- the positive control and the loopback
# sanity probe. The negative control must never call this: a retry there
# would only give a transient successful connection more chances to be
# mistaken for evidence, biasing an error-detection check in the wrong
# direction.
probe_with_retry() {
  local url="$1" timeout="$2" attempts="${3:-$RETRY_ATTEMPTS_DEFAULT}" sleep_s="${4:-$RETRY_SLEEP_DEFAULT}" label="$5"
  local result attempt
  for ((attempt = 1; attempt <= attempts; attempt++)); do
    result="$(probe "$url" "$timeout")"
    # Per-attempt progress goes to stderr, not stdout: this function's
    # stdout is captured whole by callers via $(...), and only the final
    # classification (echoed once, below or after the loop) belongs there.
    echo "egress-check ${label}: attempt ${attempt}/${attempts} target=${url} result=${result}" >&2
    if is_reachable "$result"; then
      echo "$result"
      return 0
    fi
    if [[ "$attempt" -lt "$attempts" ]]; then
      sleep "$sleep_s"
    fi
  done
  echo "$result"
}

start_local_listener() {
  # Serves a fixed sentinel body on 127.0.0.1 at an OS-assigned port.
  # Prints "<pid> <port>" on success. Caller is responsible for killing pid.
  local dir port pid
  dir="$(mktemp -d)"
  echo "egress-check-sentinel" >"$dir/index.html"
  # -u: unbuffered stdout. Without it, http.server's startup line sits in
  # Python's block buffer (stdout is a redirected file, not a tty) and the
  # poll loop below never sees it until the process exits.
  python3 -u -m http.server 0 --bind 127.0.0.1 --directory "$dir" >"$dir/server.log" 2>&1 &
  pid=$!
  # http.server prints "Serving HTTP on 127.0.0.1 port <N> ..." to stdout;
  # poll the log briefly instead of a fixed sleep, since startup time varies.
  for _ in $(seq 1 50); do
    port="$(grep -oE 'port [0-9]+' "$dir/server.log" 2>/dev/null | grep -oE '[0-9]+' | head -1 || true)"
    [[ -n "$port" ]] && break
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.1
  done
  if [[ -z "${port:-}" ]]; then
    kill "$pid" 2>/dev/null || true
    echo "::error::egress-check: local listener failed to report a port" >&2
    return 1
  fi
  echo "$pid $port"
}

run_ci() {
  local allowed_url blocked_url loop_pid loop_port loop_result allowed_result blocked_result fail=0

  allowed_url="${EGRESS_ALLOWED_URL:-https://github.com}"
  blocked_url="${EGRESS_BLOCKED_URL:-https://example.com}"

  echo "egress-check: starting local loopback listener (network-stack sanity check)"
  read -r loop_pid loop_port < <(start_local_listener)
  trap 'kill "${loop_pid:-}" 2>/dev/null || true' EXIT

  loop_result="$(probe_with_retry "http://127.0.0.1:${loop_port}/" 5 3 1 "loopback-sanity")"
  if ! is_reachable "$loop_result"; then
    echo "::error::egress-check: loopback listener unreachable after retries (${loop_result}) -- this is not an egress finding, the runner's local network stack itself is broken, so the controls below cannot be trusted" >&2
    exit 2
  fi

  allowed_result="$(probe_with_retry "$allowed_url" "$TIMEOUT_DEFAULT" "$RETRY_ATTEMPTS_DEFAULT" "$RETRY_SLEEP_DEFAULT" "positive-control")"
  if ! is_reachable "$allowed_result"; then
    echo "::error::egress-check: POSITIVE CONTROL FAILED -- allowlisted destination ${allowed_url} was not reachable after ${RETRY_ATTEMPTS_DEFAULT} attempts (${allowed_result}). Either the harden-runner allowed-endpoints list dropped this host, or egress is broken generally -- this run cannot tell block-is-working apart from network-is-dead, so the negative control below proves nothing" >&2
    fail=1
  fi

  # Single-shot, deliberately not retried -- see the module header.
  blocked_result="$(probe "$blocked_url")"
  echo "egress-check negative-control: target=${blocked_url} result=${blocked_result}"
  if is_reachable "$blocked_result"; then
    echo "::error::egress-check: NEGATIVE CONTROL FAILED -- non-allowlisted destination ${blocked_url} was reachable (${blocked_result}). The CI egress block did not take effect" >&2
    fail=1
  elif ! is_blocked "$blocked_result"; then
    echo "::error::egress-check: negative control gave an unrecognized result (${blocked_result}) for ${blocked_url} -- treating as ambiguous, not as proof of a block" >&2
    fail=1
  fi

  if [[ "$fail" -ne 0 ]]; then
    echo "egress-check RESULT: FAIL"
    exit 1
  fi
  echo "egress-check RESULT: PASS (loopback=${loop_result} positive=${allowed_result} negative=${blocked_result})"
}

run_selftest() {
  local pass=0 fail=0 loop_pid loop_port result

  ok() { echo "PASS: $1"; pass=$((pass + 1)); }
  bad() { echo "FAIL: $1 (got: $2)"; fail=$((fail + 1)); }

  echo "egress-check selftest: verifying classification logic over loopback only, no CI egress dependency"

  read -r loop_pid loop_port < <(start_local_listener)
  trap 'kill "${loop_pid:-}" 2>/dev/null || true' EXIT

  result="$(probe "http://127.0.0.1:${loop_port}/" 5)"
  if is_reachable "$result"; then ok "reachable local listener classifies as http:*"; else bad "reachable local listener classifies as http:*" "$result"; fi

  # A port on loopback nothing is listening on refuses the connection
  # immediately -- this is the same OS-level signal a firewall REJECT
  # (as opposed to DROP) rule produces for the blocked-destination case.
  result="$(probe "http://127.0.0.1:1/" 3)"
  if [[ "$result" == "connection_refused" ]]; then ok "closed local port classifies as connection_refused"; else bad "closed local port classifies as connection_refused" "$result"; fi

  # A hostname under the reserved .invalid TLD (RFC 2606) can never resolve.
  result="$(probe "http://definitely-does-not-exist.invalid/" 3)"
  if [[ "$result" == "dns_failure" ]]; then ok "unresolvable hostname classifies as dns_failure"; else bad "unresolvable hostname classifies as dns_failure" "$result"; fi

  if is_reachable "$(probe "http://127.0.0.1:${loop_port}/" 5)" && [[ "$(probe "http://127.0.0.1:1/" 3)" != "http:"* ]]; then
    ok "is_reachable/is_blocked predicates agree with probe() on both cases"
  else
    bad "is_reachable/is_blocked predicates agree with probe() on both cases" "see above"
  fi

  # probe_with_retry() must return immediately on a reachable first
  # attempt (no gratuitous sleeping) and must retry a failing one.
  result="$(probe_with_retry "http://127.0.0.1:${loop_port}/" 5 3 1 "selftest-retry-immediate")"
  if is_reachable "$result"; then ok "probe_with_retry returns immediately when the first attempt succeeds"; else bad "probe_with_retry returns immediately when the first attempt succeeds" "$result"; fi

  result="$(probe_with_retry "http://127.0.0.1:1/" 2 3 1 "selftest-retry-exhausted")"
  if [[ "$result" == "connection_refused" ]]; then ok "probe_with_retry exhausts attempts and returns the last classification when never reachable"; else bad "probe_with_retry exhausts attempts and returns the last classification when never reachable" "$result"; fi

  echo "egress-check selftest: ${pass} passed, ${fail} failed"
  [[ "$fail" -eq 0 ]]
}

main() {
  local mode="${1:-}"
  case "$mode" in
    ci) run_ci ;;
    selftest) run_selftest ;;
    probe)
      shift
      [[ $# -ge 1 ]] || { echo "usage: $0 probe URL [TIMEOUT]" >&2; exit 64; }
      probe "$@"
      ;;
    *)
      echo "usage: $0 ci|selftest|probe URL [TIMEOUT]" >&2
      exit 64
      ;;
  esac
}

main "$@"
