#!/usr/bin/env bash
# Self-test for check-provenance-meta.sh (#26, PR #120 review). Exercises
# the exact failure modes an independent review found reachable without a
# real release: an empty array, an empty object, a good fixture, and a
# fixture missing one required field each.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$SCRIPT_DIR/check-provenance-meta.sh"

WORKDIR="$(mktemp -d)"
cleanup() { rm -rf "$WORKDIR"; }
trap cleanup EXIT

PASS=0
FAIL=0
pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1"; FAIL=$((FAIL + 1)); }

good_provenance() {
  cat <<'JSON'
{
  "binaryGoVersion": "go1.26.5",
  "goSum": { "file": "go.sum", "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" },
  "goMod": { "file": "go.mod", "sha256": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" },
  "sourceCommit": "cccccccccccccccccccccccccccccccccccccc",
  "sourceTreeHash": "dddddddddddddddddddddddddddddddddddddd",
  "builderImage": { "repository": "golang:1.26-alpine", "digest": "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee" },
  "runtimeImage": { "repository": "alpine:3.24", "digest": "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" }
}
JSON
}

# 1. A good fixture passes.
good_file="$WORKDIR/good.json"
good_provenance > "$good_file"
if "$SCRIPT" "$good_file" >/tmp/out.$$ 2>&1; then
  pass "good provenance-meta.json passes"
else
  fail "good provenance-meta.json unexpectedly rejected: $(cat /tmp/out.$$)"
fi
rm -f /tmp/out.$$

# 2. An empty array is rejected (the reviewer-reported "[]" case).
empty_array_file="$WORKDIR/empty-array.json"
echo '[]' > "$empty_array_file"
if "$SCRIPT" "$empty_array_file" >/tmp/out.$$ 2>&1; then
  fail "empty array [] was accepted (should FAIL: not an object)"
else
  if grep -q "must be a JSON object" /tmp/out.$$; then
    pass "empty array [] rejected with a clear message"
  else
    fail "empty array [] rejected but without a clear message: $(cat /tmp/out.$$)"
  fi
fi
rm -f /tmp/out.$$

# 3. An empty object is rejected (the reviewer-reported "{}" case).
empty_object_file="$WORKDIR/empty-object.json"
echo '{}' > "$empty_object_file"
if "$SCRIPT" "$empty_object_file" >/tmp/out.$$ 2>&1; then
  fail "empty object {} was accepted (should FAIL: missing required fields)"
else
  if grep -q "binaryGoVersion missing/empty" /tmp/out.$$; then
    pass "empty object {} rejected with a clear message"
  else
    fail "empty object {} rejected but without a clear message: $(cat /tmp/out.$$)"
  fi
fi
rm -f /tmp/out.$$

# 4. Each required field, removed one at a time, is caught individually.
for field in binaryGoVersion sourceCommit sourceTreeHash; do
  partial_file="$WORKDIR/missing-$field.json"
  good_provenance | jq "del(.$field)" > "$partial_file"
  if "$SCRIPT" "$partial_file" >/tmp/out.$$ 2>&1; then
    fail "missing $field was accepted"
  else
    if grep -q "$field missing/empty" /tmp/out.$$; then
      pass "missing $field caught with a clear message"
    else
      fail "missing $field rejected but without naming the field: $(cat /tmp/out.$$)"
    fi
  fi
  rm -f /tmp/out.$$
done

for field in file sha256; do
  partial_file="$WORKDIR/missing-goSum-$field.json"
  good_provenance | jq "del(.goSum.$field)" > "$partial_file"
  if "$SCRIPT" "$partial_file" >/tmp/out.$$ 2>&1; then
    fail "missing goSum.$field was accepted"
  else
    if grep -q "goSum.$field missing/empty" /tmp/out.$$; then
      pass "missing goSum.$field caught with a clear message"
    else
      fail "missing goSum.$field rejected but without naming the field: $(cat /tmp/out.$$)"
    fi
  fi
  rm -f /tmp/out.$$
done

# 5. Invalid JSON is rejected without a shell error.
invalid_file="$WORKDIR/invalid.json"
printf 'not json' > "$invalid_file"
if "$SCRIPT" "$invalid_file" >/tmp/out.$$ 2>&1; then
  fail "invalid JSON was accepted"
else
  if grep -q "not valid JSON" /tmp/out.$$; then
    pass "invalid JSON rejected with a clear message"
  else
    fail "invalid JSON rejected but without a clear message: $(cat /tmp/out.$$)"
  fi
fi
rm -f /tmp/out.$$

echo
echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
